package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/config"
	"zing/internal/response"
	"zing/internal/store"
)

// testZingTOML is a minimal, valid zing.toml (every key config.Load
// requires, plus the console and dispatch keys serve reads): one project
// named "zing", matching fixtures/tickets.toml's project so intake serves
// its one fixture ticket; and the shortest dispatch interval
// config.Dispatch.IntervalSeconds's int type allows, so the ring below
// advances one state per second instead of stalling on the default 30s.
// %d takes a free loopback port: config.Load rejects an explicit
// console.port of 0 (checkValues requires 1-65535), so the test picks one
// itself instead of asking serve for an ephemeral one.
const testZingTOMLFormat = `
user = "test-user"

[console]
bind = ["127.0.0.1"]
port = %d

[dispatch]
interval_seconds = 1
max_parallel = 1

[[projects]]
name = "zing"
repo = "https://example.com/zing.git"
path = "/tmp/zing-project"
tracker = "github"
commands = { test = "go test ./...", lint = "golangci-lint run" }
`

// freeLoopbackPort asks the kernel for an unused loopback port by opening
// and immediately closing a listener on port 0, then reusing the port
// number it was assigned. This carries the ordinary test-only race of
// another process claiming the same port before serve binds it.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = ln.Close() }()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	return addr.Port
}

// stateSequenceWant is the ordered "state" message sequence design section
// 7.1's table drives the fixture ticket through, queued to done.
var stateSequenceWant = []string{"planning", "building", "reviewing", "judging", "shipping", "done"}

// TestServe_RingToDoneAnsweringOneQuestionThenCleanShutdown is the
// end-to-end integration test (PKG3-PLAN.md section 12 rows 5 and 7, section
// 6.10): zing serve, run against a temp config and a temp database, carries
// the one fixture ticket from queued into planning, where it waits on the
// one fixture question; this test answers it through a real POST /answer
// against the running server, exactly as the browser's chip click would,
// and the dispatcher resumes and carries the ticket the rest of the way to
// done, in the order design section 7.1's state table lists. ctx
// cancellation then drains the dispatcher and closes the store, and serve
// returns nil.
func TestServe_RingToDoneAnsweringOneQuestionThenCleanShutdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	doc := fmt.Sprintf(testZingTOMLFormat, port)
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}

	// Create and migrate the database once, up front, so serve's own
	// store.Open and this test's polling store.Open are never the two
	// connections racing to initialize a brand-new file's WAL mode: on a
	// fresh file that race can trip SQLITE_BUSY even under the driver's
	// busy_timeout pragma, since the very first WAL setup takes a lock
	// busy_timeout does not retry through.
	pre, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("pre-migrate store.Open: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatalf("pre-migrate store.Close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath) }()

	ticketID, questionID := waitForOpenQuestion(t, dbPath, serveDone)
	answerQuestion(t, baseURL, ticketID, questionID, "b")

	ticket := waitForDoneTicket(t, dbPath, serveDone)
	if ticket.ID != ticketID {
		t.Fatalf("done ticket id = %d, want the same ticket that asked the question (%d)", ticket.ID, ticketID)
	}
	assertStateSequence(t, dbPath, ticket.ID)
	assertExactlyOneQuestionAnswered(t, dbPath, ticket.ID)

	cancel()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within the drain window after ctx cancel")
	}
}

// waitForOpenQuestion polls a second store.Open on dbPath until the one
// fixture ticket is in planning, waiting on "questions", with at least one
// open question message, and returns the ticket id and that question's
// message id, so the test can answer it (design section 6.6, first entry).
func waitForOpenQuestion(t *testing.T, dbPath string, serveDone <-chan error) (ticketID, questionID int64) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	st := openStoreWithRetry(ctx, t, dbPath)
	defer func() { _ = st.Close() }()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("ticket did not reach an open question within the poll deadline")
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			tickets, err := st.ListAllTickets(ctx)
			if err != nil {
				t.Fatalf("ListAllTickets: %v", err)
			}
			if len(tickets) != 1 {
				continue
			}
			ticket := tickets[0]
			if ticket.State != "planning" || ticket.WaitingOn == nil || *ticket.WaitingOn != "questions" {
				continue
			}
			open, err := st.QuestionsByState(ctx, ticket.ID, "open")
			if err != nil {
				t.Fatalf("QuestionsByState(open): %v", err)
			}
			if len(open) == 0 {
				continue
			}
			return ticket.ID, open[0].ID
		}
	}
}

// answerQuestionTimeout bounds answerQuestion's HTTP round trip, so a wedged
// server (for example a handler deadlocked on the store) fails this test
// with a clear timeout instead of hanging it, or the whole test binary,
// forever: the request carries t.Context(), but a plain http.Client has no
// Timeout of its own and net/http does not derive one from the request
// context's absence of a deadline.
const answerQuestionTimeout = 10 * time.Second

// answerQuestion POSTs the console's $answer signal to /answer on the
// running server at baseURL, the same JSON shape and header a browser's chip
// click sends (design section 6.9): {"answer":{"ticket","question","option"}}
// with the Datastar-Request header. It fails the test on anything but 204.
func answerQuestion(t *testing.T, baseURL string, ticketID, questionID int64, option string) {
	t.Helper()

	body := fmt.Sprintf(`{"answer":{"ticket":%d,"question":%d,"option":%q}}`, ticketID, questionID, option)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/answer", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST /answer request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")

	client := &http.Client{Timeout: answerQuestionTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /answer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /answer: status = %d, want 204", resp.StatusCode)
	}
}

// openStoreWithRetry opens a second Store handle on dbPath, retrying on
// SQLITE_BUSY: store.Open runs its own migration check on every call, and
// that check can race serve's own first-ever Open (also migrating the same
// fresh file) closely enough that SQLite's busy_timeout pragma, which only
// applies inside a single connection's statements, does not cover it. A
// short retry loop is simpler than serializing test startup with serve's
// internals.
func openStoreWithRetry(ctx context.Context, t *testing.T, dbPath string) *store.Store {
	t.Helper()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("open store %s: %v (last error: %v)", dbPath, ctx.Err(), lastErr)
		default:
		}

		st, err := store.Open(ctx, dbPath)
		if err == nil {
			return st
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForDoneTicket polls a second store.Open on dbPath (SQLite WAL allows
// concurrent readers alongside serve's own writer) until the one fixture
// ticket reaches state "done", bounded so a stalled dispatcher fails the
// test instead of hanging it. It also watches serveDone, so an early serve
// failure (a bad config, a port already taken) fails with serve's own error
// instead of the opaque "did not reach done" timeout.
func waitForDoneTicket(t *testing.T, dbPath string, serveDone <-chan error) store.Ticket {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	st := openStoreWithRetry(ctx, t, dbPath)
	defer func() { _ = st.Close() }()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("ticket did not reach done within the poll deadline")
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			tickets, err := st.ListAllTickets(ctx)
			if err != nil {
				t.Fatalf("ListAllTickets: %v", err)
			}
			if len(tickets) == 1 && tickets[0].State == "done" {
				return tickets[0]
			}
		}
	}
}

// assertStateSequence reads ticketID's messages through a fresh store handle
// and asserts every "state" message's StatePayload.To appears, in order,
// exactly as stateSequenceWant lists (design section 7.1's state table).
func assertStateSequence(t *testing.T, dbPath string, ticketID int64) {
	t.Helper()

	ctx := t.Context()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("third store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	messages, err := st.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	var got []string
	for i := range messages {
		m := &messages[i]
		if m.Type != "state" {
			continue
		}
		var payload response.StatePayload
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			t.Fatalf("unmarshal state payload: %v", err)
		}
		got = append(got, string(payload.To))
	}

	if len(got) != len(stateSequenceWant) {
		t.Fatalf("state sequence = %v, want %v", got, stateSequenceWant)
	}
	for i, want := range stateSequenceWant {
		if got[i] != want {
			t.Fatalf("state sequence = %v, want %v", got, stateSequenceWant)
		}
	}
}

// assertExactlyOneQuestionAnswered reads ticketID's messages through a fresh
// store handle and asserts exactly one "answer" message was recorded, so the
// POST /answer this test drove is the only one that landed.
func assertExactlyOneQuestionAnswered(t *testing.T, dbPath string, ticketID int64) {
	t.Helper()

	ctx := t.Context()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("fourth store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	messages, err := st.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	var answers int
	for i := range messages {
		if messages[i].Type == "answer" {
			answers++
		}
	}
	if answers != 1 {
		t.Errorf("answer messages = %d, want 1", answers)
	}
}

// loopback is the console.bind address the tests below that need a working
// listener use.
const loopback = "127.0.0.1"

// zingTOMLOpts parameterizes writeZingTOML's console.bind, dispatch, and
// port fields, the ones the tests below vary; every other key mirrors
// testZingTOMLFormat's single fixture-matching project.
type zingTOMLOpts struct {
	Port            int
	IntervalSeconds int
	MaxParallel     int
	Bind            []string // nil or empty writes bind = [], an invalid config
}

// writeZingTOML writes a zing.toml built from opts to path, quoting each
// Bind entry into a TOML array (an empty or nil Bind writes bind = []).
func writeZingTOML(t *testing.T, path string, opts zingTOMLOpts) {
	t.Helper()

	bindItems := make([]string, len(opts.Bind))
	for i, b := range opts.Bind {
		bindItems[i] = strconv.Quote(b)
	}

	doc := fmt.Sprintf(`
user = "test-user"

[console]
bind = [%s]
port = %d

[dispatch]
interval_seconds = %d
max_parallel = %d

[[projects]]
name = "zing"
repo = "https://example.com/zing.git"
path = "/tmp/zing-project"
tracker = "github"
commands = { test = "go test ./...", lint = "golangci-lint run" }
`, strings.Join(bindItems, ", "), opts.Port, opts.IntervalSeconds, opts.MaxParallel)

	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
}

// waitForTicketPastQueued polls a second store.Open on dbPath until the one
// fixture ticket has left state "queued", bounded so a dispatcher that never
// claims anything fails the test instead of hanging it.
func waitForTicketPastQueued(t *testing.T, dbPath string, serveDone <-chan error) store.Ticket {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	st := openStoreWithRetry(ctx, t, dbPath)
	defer func() { _ = st.Close() }()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal(`ticket did not leave state "queued" within the poll deadline (stale draining/stopped flags left set?)`)
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			tickets, err := st.ListAllTickets(ctx)
			if err != nil {
				t.Fatalf("ListAllTickets: %v", err)
			}
			if len(tickets) == 1 && tickets[0].State != "queued" {
				return tickets[0]
			}
		}
	}
}

// waitForServing polls baseURL's GET / until it gets any HTTP response,
// bounded so a server that never comes up fails the test instead of hanging
// it. It also watches serveDone, so an early serve failure reports serve's
// own error instead of the opaque "never became reachable" timeout.
func waitForServing(t *testing.T, baseURL string, serveDone <-chan error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("server never became reachable within the poll deadline")
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", http.NoBody)
			if err != nil {
				t.Fatalf("build GET / request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				continue // not up yet
			}
			_ = resp.Body.Close()
			return
		}
	}
}

// cancelAndWaitForServe cancels ctx and asserts serve returns nil on
// serveDone within the drain window, the same shutdown assertion the
// end-to-end test above makes.
func cancelAndWaitForServe(t *testing.T, cancel context.CancelFunc, serveDone <-chan error) {
	t.Helper()

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within the drain window after ctx cancel")
	}
}

// TestServe_ClearsStaleDrainingAndStoppedFlagsAtStartup proves the fix for
// a stale "draining" flag surviving a prior graceful stop: serve, run
// against a database whose "draining" and "stopped" settings are both
// already true (exactly as a previous clean shutdown leaves them), must
// clear both before starting the dispatcher. Left unfixed, dispatch.Tick's
// very first flag read (design section 6.8 step 2) returns without ever
// claiming the ticket, and dispatch.Run exits on the same flag right after,
// so the HTTP listener comes up but nothing is ever dispatched; the ticket
// then never leaves "queued" and waitForTicketPastQueued below times out.
func TestServe_ClearsStaleDrainingAndStoppedFlagsAtStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback},
	})

	// Pre-migrate (see testZingTOMLFormat's doc comment for why this avoids
	// the fresh-WAL-init race with serve's own first store.Open), then mark
	// the database draining and stopped, simulating what a prior graceful
	// stop leaves behind.
	st, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("pre-migrate store.Open: %v", err)
	}
	if err := st.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining(true): %v", err)
	}
	if err := st.SetStopped(t.Context(), true); err != nil {
		t.Fatalf("SetStopped(true): %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("pre-migrate store.Close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath) }()

	waitForTicketPastQueued(t, dbPath, serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestServe_ClampsInvalidDispatchConfig proves that an explicit, invalid
// dispatch.interval_seconds and dispatch.max_parallel (both 0: present in
// the file, so internal/config's applyDefaults leaves them as the literal
// zeros instead of substituting its own defaults) do not reach
// dispatch.New unclamped. Left unfixed, time.Duration(0) passed to
// dispatch.Config.Interval panics time.NewTicker inside the dispatcher's
// goroutine the moment serve starts it, crashing the process; this test
// instead expects serve to come up and shut down cleanly.
func TestServe_ClampsInvalidDispatchConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 0, MaxParallel: 0, Bind: []string{loopback},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestServe_ErrorsOnEmptyConsoleBind proves that an explicit, empty
// console.bind ("bind = []", present in the file so applyDefaults does not
// substitute its own default) fails serve with a clear error instead of
// panicking net.JoinHostPort(cfg.Console.Bind[0], ...) on an out-of-range
// index. The error must surface before store.Open, so no database file is
// created.
func TestServe_ErrorsOnEmptyConsoleBind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: freeLoopbackPort(t), IntervalSeconds: 1, MaxParallel: 1, Bind: nil,
	})

	err := serve(t.Context(), cfgPath, dbPath)
	if err == nil {
		t.Fatal("serve returned nil, want an error for an empty console.bind")
	}
	if !strings.Contains(err.Error(), "console.bind") {
		t.Errorf("serve error = %q, want it to mention console.bind", err.Error())
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat dbPath after the error = %v, want os.ErrNotExist (serve must fail before store.Open)", statErr)
	}
}

// TestServe_UsesFirstOfMultipleConsoleBinds proves that a console.bind with
// more than one entry does not error or panic: serve binds the first entry
// only (the second, "198.51.100.1", is TEST-NET-2, reserved and never
// dialed) and still serves normally.
func TestServe_UsesFirstOfMultipleConsoleBinds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback, "198.51.100.1"},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestConsoleBindAddr covers consoleBindAddr directly: an empty Bind errors,
// a single entry joins with the port, and more than one entry uses only the
// first (a warning is logged for the rest, not asserted here).
func TestConsoleBindAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bind    []string
		port    int
		want    string
		wantErr bool
	}{
		{name: "empty", bind: nil, port: 7420, wantErr: true},
		{name: "single", bind: []string{loopback}, port: 7420, want: "127.0.0.1:7420"},
		{name: "multiple uses first", bind: []string{loopback, "100.64.0.1"}, port: 7420, want: "127.0.0.1:7420"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := consoleBindAddr(config.Console{Bind: tc.bind, Port: tc.port})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("consoleBindAddr(%v) error = nil, want an error", tc.bind)
				}
				return
			}
			if err != nil {
				t.Fatalf("consoleBindAddr(%v): %v", tc.bind, err)
			}
			if got != tc.want {
				t.Errorf("consoleBindAddr(%v) = %q, want %q", tc.bind, got, tc.want)
			}
		})
	}
}

// TestDispatchInterval covers dispatchInterval directly: a positive
// interval_seconds converts straight to seconds, zero or negative clamps to
// defaultDispatchInterval, and a value large enough to overflow a
// time.Duration also clamps to defaultDispatchInterval instead of wrapping
// around and panicking time.NewTicker (cubic P1).
func TestDispatchInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "positive", seconds: 5, want: 5 * time.Second},
		{name: "zero clamps to default", seconds: 0, want: defaultDispatchInterval},
		{name: "negative clamps to default", seconds: -1, want: defaultDispatchInterval},
		{
			name:    "just under the overflow bound converts straight through",
			seconds: int(maxDispatchIntervalSeconds), want: time.Duration(maxDispatchIntervalSeconds) * time.Second,
		},
		{
			name:    "huge value overflowing a time.Duration clamps to default",
			seconds: math.MaxInt64, want: defaultDispatchInterval,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dispatchInterval(tc.seconds); got != tc.want {
				t.Errorf("dispatchInterval(%d) = %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}

// TestDispatchMaxParallel covers dispatchMaxParallel directly: a positive
// value passes through, and zero or negative clamps to
// defaultDispatchMaxParallel.
func TestDispatchMaxParallel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "positive", n: 3, want: 3},
		{name: "zero clamps to default", n: 0, want: defaultDispatchMaxParallel},
		{name: "negative clamps to default", n: -1, want: defaultDispatchMaxParallel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dispatchMaxParallel(tc.n); got != tc.want {
				t.Errorf("dispatchMaxParallel(%d) = %d, want %d", tc.n, got, tc.want)
			}
		})
	}
}

// TestDispatchFailure covers dispatchFailure directly (the decision
// shutdown makes about whether the dispatcher's own error becomes serve's
// return value): it fires only when dispTriggered is true and de is a real,
// non-context.Canceled error, in which case the returned error wraps de.
func TestDispatchFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	tests := []struct {
		name          string
		dispTriggered bool
		de            error
		wantNil       bool
	}{
		{name: "not triggered by the dispatcher", dispTriggered: false, de: boom, wantNil: true},
		{name: "triggered, no error", dispTriggered: true, de: nil, wantNil: true},
		{name: "triggered, context canceled", dispTriggered: true, de: context.Canceled, wantNil: true},
		{name: "triggered, real error", dispTriggered: true, de: boom, wantNil: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dispatchFailure(tc.dispTriggered, tc.de)
			if tc.wantNil {
				if got != nil {
					t.Errorf("dispatchFailure(%v, %v) = %v, want nil", tc.dispTriggered, tc.de, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("dispatchFailure(%v, %v) = nil, want a wrapped error", tc.dispTriggered, tc.de)
			}
			if !errors.Is(got, boom) {
				t.Errorf("dispatchFailure(%v, %v) = %v, want it to wrap %v", tc.dispTriggered, tc.de, got, boom)
			}
		})
	}
}
