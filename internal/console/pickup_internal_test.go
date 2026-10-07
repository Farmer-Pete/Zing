package console

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"zing/internal/store"
	"zing/internal/tracker"
)

// testQueuedState is the store's "queued" ticket state, and testGitHubTracker
// is the only tracker value the tickets.tracker and projects.tracker CHECK
// constraints accept, each named once here so this file's repeats are not
// more raw occurrences for goconst to flag (views_internal_test.go's
// navParkFixtureState is that file's own copy of the same fix, for the same
// reason).
const (
	testQueuedState   = "queued"
	testGitHubTracker = "github"
)

// TestInsertFresh_LiveTicketAtRefIs409 proves insertFresh turns a losing
// race against a live ticket into a 409 *actionRefusal, before any tracker
// call: RetireAbandonedRef refuses with store.ErrRefLive as soon as it sees
// a non-abandoned ticket at ref, and liveHolder maps that straight to the
// refusal naming the live ticket -- c.tracker stays nil throughout, so a
// tracker call here would panic.
func TestInsertFresh_LiveTicketAtRefIs409(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	projectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: "acme", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: testGitHubTracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	seededID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "5", Title: "live ticket", State: testQueuedState,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	c := &console{store: s}
	newID, retiredRef, err := c.insertFresh(t.Context(), projectID, "acme", tracker.Ticket{Ref: "5", Title: "new attempt"})
	if newID != 0 || retiredRef != "" {
		t.Errorf("insertFresh = (%d, %q), want (0, \"\")", newID, retiredRef)
	}

	var refusal *actionRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T (%v), want *actionRefusal", err, err)
	}
	if refusal.Status != 409 {
		t.Errorf("Status = %d, want 409", refusal.Status)
	}
	want := fmt.Sprintf("issue #5 is already ticket %d", seededID)
	if refusal.Reason != want {
		t.Errorf("Reason = %q, want %q", refusal.Reason, want)
	}

	tickets, err := s.TicketsByProject(t.Context(), projectID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(tickets))
	}
	if tickets[0].ID != seededID || tickets[0].TrackerRef != "5" || tickets[0].State != testQueuedState {
		t.Errorf("seeded ticket changed: %+v", tickets[0])
	}
}
