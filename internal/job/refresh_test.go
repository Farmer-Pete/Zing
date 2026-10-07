// refresh_test.go tests the planning refresh end to end (#98, design
// "Planning refreshes first"): refreshTicket (refresh.go) as the first
// statement of planningHandler.Run, driven through the real store exactly
// as skeleton_test.go's and planning_test.go's own handler tests do, with a
// fakeTicketSource standing in for the dispatcher's real tracker read.
package job_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// nullifyTrackerBody stands in for a row inserted before migration 0011: it
// clears tracker_body with a raw UPDATE against s's own database file, which
// InsertTicket cannot leave NULL since it always writes tracker_body equal
// to body.
func nullifyTrackerBody(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(s.Dir(), "zing.db"))
	if err != nil {
		t.Fatalf("nullifyTrackerBody: sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `UPDATE tickets SET tracker_body = NULL WHERE id = ?`, ticketID); err != nil {
		t.Fatalf("nullifyTrackerBody: update: %v", err)
	}
}

// claimForRefresh claims ticketID under the same owner literal
// claimWithRuntimes uses, for a test that applies a store.HandlerCommit
// directly rather than through a handler's Run.
func claimForRefresh(t *testing.T, s *store.Store, ticketID int64) (owner string, expires time.Time) {
	t.Helper()
	owner = "test-owner"
	expires = time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	return owner, expires
}

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

// TestPlanningRefresh_FirstTurnMarksDelivered proves runPlanningFirst
// delivers a live refresh in the very same commit that starts the first
// turn (#98, task 6): the refresh tick1 already wrote and applied means
// the planner has not yet seen the new text, so tick2's first-turn commit
// carries "ticket refresh delivered" alongside its own session/run fields.
func TestPlanningRefresh_FirstTurnMarksDelivered(t *testing.T) {
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
	apply(t, s, getTicket(t, s, ticketID), commit1)

	deps2 := claim(t, s, fakeRuntime(t), ticketID)
	deps2.Source = src
	commit2, err := job.Registry()[testStatePlanning].Run(t.Context(), getTicket(t, s, ticketID), deps2)
	if err != nil {
		t.Fatalf("tick 2 Run: %v", err)
	}
	delivered := false
	for _, m := range commit2.Messages {
		if m.Body == "ticket refresh delivered" {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("tick 2 commit.Messages = %+v, want one \"ticket refresh delivered\" message", commit2.Messages)
	}
	if len(commit2.Runs) == 0 {
		t.Errorf("tick 2 commit.Runs = %+v, want a reserved run (the first turn)", commit2.Runs)
	}
	apply(t, s, getTicket(t, s, ticketID), commit2)

	_, live, err := s.LiveMarker(t.Context(), ticketID, "ticket refreshed from tracker", "ticket refresh delivered")
	if err != nil {
		t.Fatalf("LiveMarker: %v", err)
	}
	if live {
		t.Error("LiveMarker reports live after the delivered commit was applied, want not live")
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
	nullifyTrackerBody(t, s, ticketID)

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
	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.Body != "A, edited" {
		t.Errorf("final ticket.Body = %q, want %q (the console edit must survive)", final.Body, "A, edited")
	}
}

// TestPlanningRefresh_ResumesIdleSessionBeforeReview proves the open-session
// branch of planningHandler.Run resumes a live refresh before maybeReviewTick
// ever runs (#98, task 6): a cohort with a ready, unreviewed plan sitting
// behind a live "ticket refreshed from tracker" marker gets the planner
// resumed, carrying the refreshed spec under "ticket:", instead of going
// straight to plan review.
func TestPlanningRefresh_ResumesIdleSessionBeforeReview(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("Resume before review, not after."), validScenarios(2, "idle"))

	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeUpdate, Author: testAuthorSystem,
		Body: "ticket refreshed from tracker\nowner comments changed, 1 now",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "resume-sess-1")}}
	deps := claimWithRuntimes(t, s, rt, ticketID)

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), getTicket(t, s, ticketID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(rt.reqs) != 1 {
		t.Fatalf("scriptedRuntime saw %d requests, want exactly 1 (no planreview request)", len(rt.reqs))
	}
	req := rt.reqs[0]
	if req.Job != response.JobPlanning || req.SessionID == "" {
		t.Errorf("request = (job=%s, sessionID=%q), want (planning, non-empty)", req.Job, req.SessionID)
	}
	if !strings.Contains(req.Prompt, "ticket:\n") {
		t.Errorf("resume prompt does not carry a \"ticket:\" input:\n%s", req.Prompt)
	}

	delivered := false
	for _, m := range commit.Messages {
		if m.Body == "ticket refresh delivered" {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("commit.Messages = %+v, want one \"ticket refresh delivered\" message", commit.Messages)
	}
}

// TestPlanningResume_DeliversRefreshedTicketOnce proves runPlanningResume's
// own live-refresh gate (#98, task 6), the exactly-once goal for a resume
// triggered by an undelivered owner answer (PlanningConversation.Undelivered,
// not the Run-level open-session branch TestPlanningRefresh_
// ResumesIdleSessionBeforeReview covers): the first such resume, with a
// live refresh marker already stored, carries the ticket spec under
// "ticket:" and marks the refresh delivered in the same commit; a second
// resume, after that commit is applied, carries no "ticket:" input.
func TestPlanningResume_DeliversRefreshedTicketOnce(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	options := []response.Option{{Key: "a", Text: testOptionAText}, {Key: "b", Text: testOptionBText}}
	firstResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q1", Title: "A question", Body: testQuestionBody, Options: options, Recommended: "a",
		}},
	}
	firstRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{Response: firstResp, SessionID: "resume-once-sess", ExitCode: 0, AgentTime: time.Second}},
	}}
	firstCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID)
	if err != nil {
		t.Fatalf("first turn Run: %v", err)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	const refreshedComments = "Comment by owner-login:\nREFRESHED-TEXT"
	refreshOwner, refreshExpires := claimForRefresh(t, s, ticketID)
	refreshApplied, refreshErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: refreshOwner, Expires: refreshExpires,
		SetTicketText: &store.TicketText{OwnerComments: refreshedComments},
	})
	if refreshErr != nil || !refreshApplied {
		t.Fatalf("CommitHandlerResult (seed refreshed text): applied=%v err=%v", refreshApplied, refreshErr)
	}

	_, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeUpdate, Author: testAuthorSystem,
		Body: "ticket refreshed from tracker\nowner comments changed, 1 now",
	})
	if insertErr != nil {
		t.Fatalf("InsertMessage: %v", insertErr)
	}

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	if _, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"}); answerErr != nil {
		t.Fatalf("AnswerQuestion: %v", answerErr)
	}

	secondResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q1", Title: "Another question", Body: testQuestionBody, Options: options, Recommended: "a",
		}},
		Replies: []response.Reply{
			{Question: "Q1", Settled: true, Decision: "Settled Q1.", Text: "Settling Q1."},
		},
	}
	rec := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{Response: secondResp, SessionID: "resume-once-sess", ExitCode: 0, AgentTime: time.Second}},
	}}}
	secondCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second turn (resume) Run: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, "ticket:\n") {
		t.Errorf("resume prompt does not carry a \"ticket:\" input:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "REFRESHED-TEXT") {
		t.Errorf("resume prompt does not carry the refreshed ticket text:\n%s", rec.lastReq.Prompt)
	}
	delivered := false
	for _, m := range secondCommit.Messages {
		if m.Body == "ticket refresh delivered" {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("second commit.Messages = %+v, want one \"ticket refresh delivered\" message", secondCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	open2, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open2) != 1 {
		t.Fatalf("QuestionsByState(open) after second turn = %v, %v, want exactly one", open2, err)
	}
	if _, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open2[0].ID, Option: "a"}); answerErr != nil {
		t.Fatalf("AnswerQuestion: %v", answerErr)
	}

	thirdResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q1", Title: "A third question", Body: testQuestionBody, Options: options, Recommended: "a",
		}},
		Replies: []response.Reply{
			{Question: "Q2", Settled: true, Decision: "Settled Q2.", Text: "Settling Q2."},
		},
	}
	rec2 := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{Response: thirdResp, SessionID: "resume-once-sess", ExitCode: 0, AgentTime: time.Second}},
	}}}
	if _, err := runPlanning(t, s, claimWithRuntimes(t, s, rec2, ticketID), ticketID); err != nil {
		t.Fatalf("third turn (second resume) Run: %v", err)
	}
	if strings.Contains(rec2.lastReq.Prompt, "ticket:\n") {
		t.Errorf("second resume prompt carries a \"ticket:\" input, want none (delivered once):\n%s", rec2.lastReq.Prompt)
	}
}
