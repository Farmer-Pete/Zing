package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/store"
)

// The full queued-to-done ring this file used to drive through a real serve()
// (answering its planning and gate questions over a live HTTP server) moved
// to cmd/zing/selftest.go's selftestResumeE2E once task 14 wired serve's own
// runtime.Set and tracker to the two real runtimes and the real GitHub
// tracker: that ring only ever worked against the scripted Fake runtime and
// the fixture tracker (design D2), which production must never build, and
// selftestResumeE2E already proves the identical ring against its own Fake
// and Fixture, independent of serve(). The tests below only need serve() to
// come up and shut down cleanly; the one exception,
// TestServe_ClearsStaleDrainingAndStoppedFlagsAtStartup, drives only the
// dispatcher's code-only queued-to-planning transition.

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

// loopback is the console.bind address the tests below that need a working
// listener use.
const loopback = "127.0.0.1"

// testServeProjectName and testServeTracker name writeZingTOML's own config
// project (goconst): both this file and seed_test.go build a matching
// store.Project row for it directly, rather than repeating the literals.
const (
	testServeProjectName = "zing"
	testServeTracker     = "github"
	testServeStateQueued = "queued"
	// testServeTicketRef is seedQueuedTicketForServe's own tracker_ref
	// (goconst): every file that later looks for that one ticket by ref
	// shares this constant rather than repeating the literal.
	testServeTicketRef = "manual#1"
)

// zingTOMLOpts parameterizes writeZingTOML's console.bind, dispatch, and
// port fields, the ones the tests below vary; every other key is a fixed,
// valid one project (name "zing", repo "x/zing").
type zingTOMLOpts struct {
	Port            int
	IntervalSeconds int
	MaxParallel     int
	Bind            []string // nil or empty writes bind = [], an invalid config
}

// newTestGitRepo git-inits a fresh temp directory and returns its path: task
// 8's serve wires one orchestrator.Orchestrator per configured project and
// resolves its git common dir at startup (GitCommonDir), so writeZingTOML's
// project path must be a real repository, not merely an existing directory.
// "git rev-parse --git-common-dir" succeeds against a freshly initialized
// repo with no commits, so init alone is enough.
func newTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", dir).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed argv, test-only
		t.Fatalf("git init %s: %v (%s)", dir, err, out)
	}
	return dir
}

// writeZingTOML writes a zing.toml built from opts to path, quoting each
// Bind entry into a TOML array (an empty or nil Bind writes bind = []). The
// one configured project's path is a fresh, real git repository
// (newTestGitRepo), since serve now resolves its git common dir at startup.
func writeZingTOML(t *testing.T, path string, opts zingTOMLOpts) {
	t.Helper()

	bindItems := make([]string, len(opts.Bind))
	for i, b := range opts.Bind {
		bindItems[i] = strconv.Quote(b)
	}

	doc := fmt.Sprintf(`
user = "test-user"
github_token = "test-github-token"

[console]
bind = [%s]
port = %d

[dispatch]
interval_seconds = %d
max_parallel = %d

[[projects]]
name = "zing"
repo = "x/zing"
path = %q
tracker = "github"
commands = { test = "go test ./...", lint = "golangci-lint run" }
`, strings.Join(bindItems, ", "), opts.Port, opts.IntervalSeconds, opts.MaxParallel, newTestGitRepo(t))

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
			if len(tickets) == 1 && tickets[0].State != testServeStateQueued {
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

	// Pre-migrate (avoids racing serve's own first store.Open against the
	// fresh file's WAL init), then mark the database draining and stopped,
	// simulating what a prior graceful stop leaves behind. It also seeds one
	// queued ticket directly (task 14: serve's own tracker is now the real
	// GitHub one, so this proof no longer waits on a real intake call
	// landing a ticket), under the project name writeZingTOML's config
	// names, so ensureBindings's own EnsureProject reconciles onto this row
	// at startup instead of inserting a second one.
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
	seedQueuedTicketForServe(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("pre-migrate store.Close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false) }()

	waitForTicketPastQueued(t, dbPath, serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// seedQueuedTicketForServe inserts one queued ticket directly into st, under
// a project named "zing" (writeZingTOML's own config project name), so a
// test can prove the dispatcher really claims and advances it without
// depending on a real tracker's Intake landing one first.
func seedQueuedTicketForServe(t *testing.T, st *store.Store) {
	t.Helper()

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: testServeProjectName, RepoURL: "https://github.com/x/zing", Tracker: testServeTracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testServeTicketRef, Title: "a ticket", State: testServeStateQueued,
	}); err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
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
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false) }()

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

	err := serve(t.Context(), cfgPath, dbPath, false)
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

// TestServe_BindsEveryLiteralAddressAndSkipsAnUnresolvedTailscaleEntry
// proves multi-bind (design section 6.14, Task 11): serve binds every
// literal console.bind entry (not only the first, unlike the single-bind
// skeleton TestConsoleBindAddr used to cover), and a "tailscale" entry that
// cannot resolve in this sandboxed test environment (no tailscale CLI, no
// matching interface) is skipped rather than failing serve.
func TestServe_BindsEveryLiteralAddressAndSkipsAnUnresolvedTailscaleEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback, bindTokenTailscale},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestResolveBindHosts_EmptyTokensResolvesToNoHosts and the config-level
// wildcard/allowed_hosts checks cover the rest of multi-bind resolution
// directly (bind_test.go, internal/config/config_test.go); serve's own
// "no address resolved" error path is covered by
// TestServe_ErrorsOnEmptyConsoleBind above.

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

// TestResolvePushToken_StableAcrossARestartUnlessExplicitlyConfigured
// proves the design section 6.13 precedence rule end to end against a real
// store: with no explicit console.push_token, the first call generates and
// persists a token that a second call (simulating a restart, with the same
// empty configured value) reuses unchanged; an explicit configured value on
// a later call always wins and overrides what was persisted, "rotating it
// in zing.toml" the way the design names.
func TestResolvePushToken_StableAcrossARestartUnlessExplicitlyConfigured(t *testing.T) {
	t.Parallel()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	first, err := resolvePushToken(t.Context(), st, "")
	if err != nil {
		t.Fatalf("resolvePushToken (first, generated): %v", err)
	}
	if first == "" {
		t.Fatal("resolvePushToken (first) = \"\", want a generated token")
	}

	// A second call with no explicit config, simulating a restart: must
	// reuse the persisted token, not generate a new one.
	second, err := resolvePushToken(t.Context(), st, "")
	if err != nil {
		t.Fatalf("resolvePushToken (second, after restart): %v", err)
	}
	if second != first {
		t.Errorf("resolvePushToken (after restart) = %q, want the same persisted token (%q)", second, first)
	}

	// An explicit config value always wins and rotates the effective token.
	explicit, err := resolvePushToken(t.Context(), st, "my-explicit-token")
	if err != nil {
		t.Fatalf("resolvePushToken (explicit): %v", err)
	}
	if explicit != "my-explicit-token" {
		t.Errorf("resolvePushToken (explicit) = %q, want my-explicit-token", explicit)
	}
}
