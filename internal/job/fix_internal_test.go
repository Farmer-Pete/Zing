package job

// fix_internal_test.go tests fix.go's own unexported functions --
// fixRequestMessage and openFixRequest -- that no seam in fix_test.go
// (package job_test) can reach, the same reason planning_internal_test.go
// and building_internal_test.go live in package job instead of alongside
// them.

import (
	"fmt"
	"path/filepath"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// newFixTestStore opens a fresh Store on a temp-file database, closed on
// test cleanup.
func newFixTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedFixTestTicket inserts one queued ticket, the minimum openFixRequest
// needs (only its own id).
func seedFixTestTicket(t *testing.T, s *store.Store) store.Ticket {
	t.Helper()
	ctx := t.Context()
	projectID, err := s.EnsureProject(ctx, store.Project{
		Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: testTrackerGithub,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(ctx, store.Ticket{ProjectID: projectID, TrackerRef: "fix#1", Title: "t", State: stateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	ticket, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return ticket
}

// TestFixRequestMessageRejectsUnknownKind proves fixRequestMessage's own
// validation (design section 5.2): a kind outside FixKind.Values returns
// the fixed error, before anything is written.
func TestFixRequestMessageRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	_, err := fixRequestMessage(store.Ticket{ID: 1}, FixKind("bogus"), "x", 0)
	if err == nil || err.Error() != "job: unknown fix kind bogus" {
		t.Fatalf("fixRequestMessage error = %v, want %q", err, "job: unknown fix kind bogus")
	}
}

// TestFixRequestMessageRejectsEmptyText proves fixRequestMessage's own
// validation (design section 5.2): text that is empty after
// strings.TrimSpace returns the fixed error.
func TestFixRequestMessageRejectsEmptyText(t *testing.T) {
	t.Parallel()
	_, err := fixRequestMessage(store.Ticket{ID: 1}, FixKindFindings, "   \n\t ", 0)
	if err == nil || err.Error() != "job: fix input is empty" {
		t.Fatalf("fixRequestMessage error = %v, want %q", err, "job: fix input is empty")
	}
}

// TestFixStageOptions proves fixStageOptions routes by the asking stage
// (design section 8, owner decision Q6, premise correction): only a
// failure-kind fix, whose asking stage is judging, offers the "judge
// again" option; findings, ci_log, and threads offer none.
func TestFixStageOptions(t *testing.T) {
	t.Parallel()
	want := []response.Option{{Key: fixRejudgeOptionKey, Text: fixRejudgeOptionText}}
	if got := fixStageOptions(FixKindFailure); len(got) != 1 || got[0] != want[0] {
		t.Errorf("fixStageOptions(failure) = %+v, want %+v", got, want)
	}
	for _, kind := range []FixKind{FixKindFindings, FixKindCILog, FixKindThreads} {
		if got := fixStageOptions(kind); got != nil {
			t.Errorf("fixStageOptions(%s) = %+v, want nil", kind, got)
		}
	}
}

// TestOpenFixRequestSkipsLanded proves openFixRequest's own D22 identity
// rule (design section 5.2): a request whose own "fix landed <id> sha
// ..." marker exists is not open; a second, later request with no landed
// marker of its own is.
func TestOpenFixRequestSkipsLanded(t *testing.T) {
	t.Parallel()
	s := newFixTestStore(t)
	ticket := seedFixTestTicket(t, s)
	d := Deps{Store: s}
	ctx := t.Context()

	msg, err := fixRequestMessage(ticket, FixKindCILog, "first", 0)
	if err != nil {
		t.Fatalf("fixRequestMessage: %v", err)
	}
	mid, err := s.InsertMessage(ctx, msg)
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	if _, insertErr := s.InsertMessage(ctx, store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("fix landed %d sha aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", mid),
	}); insertErr != nil {
		t.Fatalf("InsertMessage (landed): %v", insertErr)
	}

	if _, open, openErr := openFixRequest(ctx, d, ticket); openErr != nil {
		t.Fatalf("openFixRequest: %v", openErr)
	} else if open {
		t.Fatal("openFixRequest after landing: open = true, want false")
	}

	msg2, err := fixRequestMessage(ticket, FixKindFailure, "second", 10)
	if err != nil {
		t.Fatalf("fixRequestMessage: %v", err)
	}
	mid2, err := s.InsertMessage(ctx, msg2)
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	req, open, err := openFixRequest(ctx, d, ticket)
	if err != nil {
		t.Fatalf("openFixRequest: %v", err)
	}
	if !open {
		t.Fatal("openFixRequest: open = false, want true (the second, unlanded request)")
	}
	if req.MessageID != mid2 {
		t.Errorf("req.MessageID = %d, want %d", req.MessageID, mid2)
	}
	if req.Kind != FixKindFailure || req.Text != "second" || req.AfterRunID != 10 {
		t.Errorf("req = %+v, want {MessageID:%d Kind:failure Text:second AfterRunID:10}", req, mid2)
	}
}

// TestOpenFixRequestTwoOpenIsError proves openFixRequest's own loud-bug
// rule (design section 5.2): a producer never writes a request while one
// is open, so two unlanded requests on the same ticket is a bug, reported
// rather than silently resolved.
func TestOpenFixRequestTwoOpenIsError(t *testing.T) {
	t.Parallel()
	s := newFixTestStore(t)
	ticket := seedFixTestTicket(t, s)
	d := Deps{Store: s}
	ctx := t.Context()

	for _, text := range []string{"first", "second"} {
		msg, err := fixRequestMessage(ticket, FixKindCILog, text, 0)
		if err != nil {
			t.Fatalf("fixRequestMessage: %v", err)
		}
		if _, err := s.InsertMessage(ctx, msg); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
	}

	_, _, err := openFixRequest(ctx, d, ticket)
	wantErr := fmt.Sprintf("job: ticket %d has two open fix requests", ticket.ID)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("openFixRequest error = %v, want %q", err, wantErr)
	}
}
