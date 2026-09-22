package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestServe_SilentRingToDoneThenCleanShutdown is the silent-ring integration
// test (PKG3-PLAN.md section 12 row 5, section 6.10): zing serve, run
// end-to-end against a temp config and a temp database, carries the one
// fixture ticket from queued to done with no question, in the order design
// section 7.1's state table lists; ctx cancellation then drains the
// dispatcher and closes the store, and serve returns nil.
func TestServe_SilentRingToDoneThenCleanShutdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	doc := fmt.Sprintf(testZingTOMLFormat, freeLoopbackPort(t))
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

	ticket := waitForDoneTicket(t, dbPath, serveDone)
	assertStateSequence(t, dbPath, ticket.ID)

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
