package console_test

import (
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// TestCheckRerunEventsRenderInThread proves a check_rerun event row renders
// through eventLine's checkRerunLine rule in the real Thread view, keyed by
// event_kind rather than any body prefix (#34 first step).
func TestCheckRerunEventsRenderInThread(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "t#1", "Check rerun ticket")

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)

	for _, sha := range []string{shaA, shaA} {
		msg, err := store.NewEvent(ticketID, store.EventKindCheckRerun, response.CheckRerunEvent{
			Check: "test", SHA: sha, RunID: 1, CheckRunID: 1, Reason: response.RerunReasonFlaky,
		})
		if err != nil {
			t.Fatalf("NewEvent(test, %s): %v", sha, err)
		}
		if _, err := s.InsertMessage(t.Context(), msg); err != nil {
			t.Fatalf("InsertMessage(test, %s): %v", sha, err)
		}
	}
	lintMsg, err := store.NewEvent(ticketID, store.EventKindCheckRerun, response.CheckRerunEvent{
		Check: "lint", SHA: shaB, RunID: 2, CheckRunID: 2, Reason: response.RerunReasonFlaky,
	})
	if err != nil {
		t.Fatalf("NewEvent(lint, %s): %v", shaB, err)
	}
	if _, insertErr := s.InsertMessage(t.Context(), lintMsg); insertErr != nil {
		t.Fatalf("InsertMessage(lint, %s): %v", shaB, insertErr)
	}

	n, err := s.CountEvents(t.Context(), ticketID, store.EventKindCheckRerun, store.EventFilter{SHA: shaA})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountEvents(SHA shaA) = %d, want 2", n)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	main := mainFrame(t, srv.URL, "thread", ticketID, 0)

	if got := strings.Count(main, "Zing re-ran the test check on aaaaaaa (workflow run 1) because it failed."); got != 2 {
		t.Errorf(`thread frame has %d copies of "Zing re-ran the test check on aaaaaaa (workflow run 1) because it failed.", want 2; got:%s`, got, main)
	}
	if got := strings.Count(main, "Zing re-ran the lint check on bbbbbbb (workflow run 2) because it failed."); got != 1 {
		t.Errorf(`thread frame has %d copies of "Zing re-ran the lint check on bbbbbbb (workflow run 2) because it failed.", want 1; got:%s`, got, main)
	}
}
