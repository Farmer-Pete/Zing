// refresh_test.go tests the planning refresh end to end (#98, design
// "Planning refreshes first"): refreshTicket (refresh.go) as the first
// statement of planningHandler.Run, driven through the real store exactly
// as skeleton_test.go's and planning_test.go's own handler tests do, with a
// fakeTicketSource standing in for the dispatcher's real tracker read.
package job_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"zing/internal/job"
	"zing/internal/store"
)

// fakeTicketSource is a job.TicketSource that always returns it and err,
// regardless of the projectID and ref it is called with.
type fakeTicketSource struct {
	it  job.IssueText
	err error
}

func (f fakeTicketSource) IssueText(context.Context, int64, string) (job.IssueText, error) {
	return f.it, f.err
}

// seedFeatureTicketInPlanningWithBody inserts a ticket with kind already
// set to "feature" and the given body, and advances it into planning,
// bypassing a real classify turn (seedFeatureTicketInPlanning,
// planning_test.go, does the same but always with the empty body).
func seedFeatureTicketInPlanningWithBody(t *testing.T, s *store.Store, body string) int64 {
	t.Helper()
	proj := testProject
	proj.LocalPath = testProjectDir(t)
	projectID, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	kind := testKindFeature
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle, Body: body,
		Kind: &kind, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)
	return ticketID
}

// TestPlanningRefresh_OwnerCommentReachesNextRound is the ticket's own
// done-when test (#98): a new owner comment reaches a planning round after
// the refresh that recorded it, inside the ticket input, under the same
// fence as the body.
func TestPlanningRefresh_OwnerCommentReachesNextRound(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	const body = "Add a CLI."
	ticketID := seedFeatureTicketInPlanningWithBody(t, s, body)

	const comment = "The CLI is a client of serve."
	rendered := "Comment by owner-login:\n" + comment
	src := fakeTicketSource{it: job.IssueText{Body: body, OwnerComments: rendered, CommentCount: 1}}

	deps1 := claim(t, s, fakeRuntime(t), ticketID)
	deps1.Source = src
	commit1, err := job.Registry()[testStatePlanning].Run(t.Context(), getTicket(t, s, ticketID), deps1)
	if err != nil {
		t.Fatalf("tick 1 Run: %v", err)
	}
	if commit1.SetTicketText == nil || commit1.SetTicketText.OwnerComments != rendered {
		t.Fatalf("tick 1 commit.SetTicketText = %+v, want OwnerComments %q", commit1.SetTicketText, rendered)
	}
	if len(commit1.Messages) != 1 ||
		!strings.HasPrefix(commit1.Messages[0].Body, "ticket refreshed from tracker") ||
		!strings.Contains(commit1.Messages[0].Body, "owner comments changed, 1 now") {
		t.Fatalf("tick 1 commit.Messages = %+v, want one refresh message naming the comment count", commit1.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), commit1)

	rec := &recordingRuntime{rt: fakeRuntime(t)}
	deps2 := claim(t, s, rec, ticketID)
	deps2.Source = src
	if _, err := job.Registry()[testStatePlanning].Run(t.Context(), getTicket(t, s, ticketID), deps2); err != nil {
		t.Fatalf("tick 2 Run: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, comment) {
		t.Errorf("tick 2 prompt does not carry the owner comment:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningRefresh_UnchangedWritesNothing proves a source that reports
// exactly what is already stored leaves the commit untouched and the tick
// runs as normal.
func TestPlanningRefresh_UnchangedWritesNothing(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)
	ticket := getTicket(t, s, ticketID)

	rec := &recordingRuntime{rt: fakeRuntime(t)}
	deps := claim(t, s, rec, ticketID)
	deps.Source = fakeTicketSource{it: job.IssueText{Body: ticket.Body}}

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.SetTicketText != nil {
		t.Errorf("commit.SetTicketText = %+v, want nil", commit.SetTicketText)
	}
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "ticket refreshed from tracker") {
			t.Errorf("commit.Messages = %+v, want no refresh message", commit.Messages)
		}
	}
	if rec.lastReq.Prompt == "" {
		t.Error("runtime received no request, want the tick to have run past the refresh")
	}
}

// TestPlanningRefresh_SourceErrorKeepsStoredText proves a tracker read
// error leaves the stored text alone and the tick still runs.
func TestPlanningRefresh_SourceErrorKeepsStoredText(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	const body = "Keep the stored text."
	ticketID := seedFeatureTicketInPlanningWithBody(t, s, body)
	ticket := getTicket(t, s, ticketID)

	rec := &recordingRuntime{rt: fakeRuntime(t)}
	deps := claim(t, s, rec, ticketID)
	deps.Source = fakeTicketSource{err: errors.New("tracker unavailable")}

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.SetTicketText != nil {
		t.Errorf("commit.SetTicketText = %+v, want nil", commit.SetTicketText)
	}
	if !strings.Contains(rec.lastReq.Prompt, body) {
		t.Errorf("prompt does not carry the stored ticket text:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningRefresh_DecidesOnClaimedRow proves refreshTicket re-reads the
// claimed row rather than trusting the ticket Run was handed (#98, design
// "the dispatcher read t before claiming it"): a console edit landing in
// that gap is the row refreshDecision actually compares against, so a
// changed issue body is reported as replacing the console edit, not the
// stale pre-claim body.
func TestPlanningRefresh_DecidesOnClaimedRow(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanningWithBody(t, s, "A")
	stale := getTicket(t, s, ticketID)

	if err := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticketID, Target: store.OwnerEditTicketBody, Action: store.OwnerEditActionEdit,
		Body: new("A, edited"),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	deps := claim(t, s, fakeRuntime(t), ticketID)
	deps.Source = fakeTicketSource{it: job.IssueText{Body: "B"}}

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), stale, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.SetTicketText == nil || commit.SetTicketText.Body == nil || *commit.SetTicketText.Body != "B" {
		t.Fatalf("commit.SetTicketText = %+v, want Body \"B\"", commit.SetTicketText)
	}
	if len(commit.Messages) != 1 || !strings.Contains(commit.Messages[0].Body, "the new issue body replaced the console edit of the ticket body") {
		t.Fatalf("commit.Messages = %+v, want the console-edit-replaced line", commit.Messages)
	}
}

// TestPlanningRefresh_ConsoleEditSurvivesUnchangedIssue proves a console
// body edit survives a refresh in which the issue body is unchanged (#98,
// owner decision Q2): the source reports the same body the console edit
// was made on top of (tracker_body, not the edited body), so refreshTicket
// finds nothing changed and the console edit stands.
func TestPlanningRefresh_ConsoleEditSurvivesUnchangedIssue(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanningWithBody(t, s, "A")

	if err := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticketID, Target: store.OwnerEditTicketBody, Action: store.OwnerEditActionEdit,
		Body: new("A, edited"),
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}
	ticket := getTicket(t, s, ticketID)
	if ticket.Body != "A, edited" || ticket.TrackerBody == nil || *ticket.TrackerBody != "A" {
		t.Fatalf("ticket after console edit = %+v, want Body %q, TrackerBody %q", ticket, "A, edited", "A")
	}

	deps := claim(t, s, fakeRuntime(t), ticketID)
	deps.Source = fakeTicketSource{it: job.IssueText{Body: "A"}}

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.SetTicketText != nil {
		t.Errorf("commit.SetTicketText = %+v, want nil", commit.SetTicketText)
	}
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "ticket refreshed from tracker") {
			t.Errorf("commit.Messages = %+v, want no refresh message", commit.Messages)
		}
	}

	final := getTicket(t, s, ticketID)
	if final.Body != "A, edited" {
		t.Errorf("final ticket.Body = %q, want %q (the console edit must survive)", final.Body, "A, edited")
	}
}
