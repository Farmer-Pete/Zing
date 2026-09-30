// seed_test.go is Task 12's test-first proof, at the cmd/zing level, of the
// "serve" subcommand's --seed-demo flag (design section 6.15, section 12
// row 12): parsing it, and the hard constraint that a normal serve --
// no flag passed -- never seeds the demo project. internal/console's own
// seed_test.go proves SeedDemo itself is idempotent and seeds every
// artifact and question kind; this file proves cmd/zing only calls it when
// asked.
package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"zing/internal/store"
)

// TestParseServeFlags_DefaultsToNoSeedDemo proves --seed-demo is off unless
// passed: parsing no arguments at all, and parsing an unrelated argument
// list, must both leave seedDemo false, so a normal `zing serve` never
// seeds.
func TestParseServeFlags_DefaultsToNoSeedDemo(t *testing.T) {
	t.Parallel()

	seedDemo, err := parseServeFlags(nil)
	if err != nil {
		t.Fatalf("parseServeFlags(nil): %v", err)
	}
	if seedDemo {
		t.Error("parseServeFlags(nil) = true, want false (no flag passed)")
	}
}

// TestParseServeFlags_SeedDemoFlagSetsTrue proves --seed-demo, when passed,
// parses to true, so an explicit operator request is actually honored.
func TestParseServeFlags_SeedDemoFlagSetsTrue(t *testing.T) {
	t.Parallel()

	seedDemo, err := parseServeFlags([]string{"--seed-demo"})
	if err != nil {
		t.Fatalf("parseServeFlags(--seed-demo): %v", err)
	}
	if !seedDemo {
		t.Error("parseServeFlags(--seed-demo) = false, want true")
	}
}

// TestParseServeFlags_RejectsUnknownFlag proves a typo or an unrelated flag
// fails parsing instead of being silently ignored.
func TestParseServeFlags_RejectsUnknownFlag(t *testing.T) {
	t.Parallel()

	if _, err := parseServeFlags([]string{"--not-a-real-flag"}); err == nil {
		t.Error("parseServeFlags(--not-a-real-flag) = nil error, want a parse error")
	}
}

// TestServe_WithoutSeedDemoFlag_DoesNotSeedTheDemoProject is the hard
// constraint's own proof against the real serve() entry point (design
// section 6.15: "It must NEVER run in a normal serve"): a real serve run,
// with seedDemo false exactly as every caller that never passes
// --seed-demo gets, is driven far enough to prove the store and dispatcher
// are live (a directly seeded ticket leaves "queued"; task 14's real
// GitHub tracker cannot be relied on to land one of its own inside the poll
// deadline), and at no point does a "demo" project exist.
func TestServe_WithoutSeedDemoFlag_DoesNotSeedTheDemoProject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback},
	})

	pre, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("pre-migrate store.Open: %v", err)
	}
	seedQueuedTicketForServe(t, pre)
	if err := pre.Close(); err != nil {
		t.Fatalf("pre-migrate store.Close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false) }()

	// Wait for the seeded ticket to leave "queued" (proving the store and
	// dispatcher are both live), asserting on every poll that no "demo"
	// project has appeared either.
	waitForTicketAssertingNoDemoProject(t, dbPath, serveDone)

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

// waitForTicketAssertingNoDemoProject polls dbPath until the seeded ticket
// has left state "queued", failing the test immediately if a "demo" project
// shows up on any poll along the way.
func waitForTicketAssertingNoDemoProject(t *testing.T, dbPath string, serveDone <-chan error) {
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
			t.Fatal("the seeded ticket never left state \"queued\" within the poll deadline")
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			projects, err := st.ListProjects(ctx)
			if err != nil {
				t.Fatalf("ListProjects: %v", err)
			}
			for _, p := range projects {
				if p.Name == "demo" {
					t.Fatalf("a %q project exists after serve() with seedDemo=false; SeedDemo must never run in a normal serve", "demo")
				}
			}

			tickets, err := st.ListAllTickets(ctx)
			if err != nil {
				t.Fatalf("ListAllTickets: %v", err)
			}
			// Wait on the directly-seeded ticket by its TrackerRef
			// (seedQueuedTicketForServe writes testServeTicketRef), ignoring
			// anything the real GitHub tracker's Intake may land during the
			// poll: a landed ticket would make len(tickets) > 1 and hang a
			// count-based wait forever.
			for i := range tickets {
				if tickets[i].TrackerRef == testServeTicketRef && tickets[i].State != testServeStateQueued {
					return
				}
			}
		}
	}
}
