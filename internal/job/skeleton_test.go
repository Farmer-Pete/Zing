package job_test

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"zing/fixtures"
	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// The pipeline state names and the one fixture tracker_ref this package's
// tests share, named once so goconst has nothing to flag across job_test.go
// and skeleton_test.go (both package job_test).
const (
	testStateQueued    = "queued"
	testStatePlanning  = "planning"
	testStateBuilding  = "building"
	testStateReviewing = "reviewing"
	testStateJudging   = "judging"
	testStateShipping  = "shipping"
	testStateDone      = "done"
	testRefFake1       = "fake#1"

	testMsgTypeQuestion  = "question"
	testWaitingQuestions = "questions"
	testAuthorZing       = "zing"

	testReasonPickedUp  = "picked up"
	testPlanningScript1 = "planning/1.xml"
)

// testProject is the one project every test in this file seeds.
var testProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: "github",
}

// newJobTestStore opens a fresh Store on a temp-file database, closed on
// test cleanup.
func newJobTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedQueuedTicket inserts a project and one queued ticket on it, under
// testRefFake1 (this file's one fixture tracker_ref), through the exported
// store API only (this file lives outside package store).
func seedQueuedTicket(t *testing.T, s *store.Store) int64 {
	t.Helper()
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: "Add a hello endpoint", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// claim claims ticketID for a fresh owner and a lease truncated to second
// precision (SQLite's TEXT timestamp round-trips at second precision), so
// the returned expires compares equal to what a later GetTicket reads
// back, and returns the Deps a handler test drives with.
func claim(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) job.Deps {
	t.Helper()
	owner := "test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}
	return job.Deps{Store: s, Runtime: rt, Owner: owner, Expires: expires}
}

// apply validates commit against t (the ticket's state before the commit)
// and applies it, failing the test on any error or a refused (applied =
// false) commit.
func apply(t *testing.T, s *store.Store, ticket store.Ticket, commit store.HandlerCommit) {
	t.Helper()
	if err := job.ValidateCommit(ticket, commit); err != nil {
		t.Fatalf("ValidateCommit: %v", err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
}

// fakeRuntime returns a *runtime.Fake serving the real, checked-in
// fixtures/scripts tree (fixtures/scripts/planning/1.xml,
// fixtures/scripts/build/1/1.xml), the same tree zing serve wires up.
func fakeRuntime(t *testing.T) *runtime.Fake {
	t.Helper()
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	return runtime.NewFake(scriptsFS)
}

// getTicket is a small GetTicket wrapper so call sites read as one line.
func getTicket(t *testing.T, s *store.Store, ticketID int64) store.Ticket {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return ticket
}

// TestRing_QueuedToDoneAnsweringOneQuestion drives every one of the six
// skeleton handlers, in pipeline order, against a real temp store and the
// real checked-in fixture scripts, applying each returned commit and
// reclaiming between states exactly as the dispatcher will (design section
// 6.8). Planning now takes two handler calls: first entry posts the one
// fixture question and waits, this test answers it through
// store.AnswerQuestion exactly as the console's POST /answer would, and only
// then does resume run. It asserts the ticket reaches done and that a state
// message was written on every one of the six transitions (design section
// 6.3, 7.1) -- planning's own first-entry call writes no state message,
// since it carries no Next.
func TestRing_QueuedToDoneAnsweringOneQuestion(t *testing.T) {
	s := newJobTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s)
	reg := job.Registry()

	// queued -> planning.
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, rt, ticketID)
	commit, err := reg[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued handler.Run: %v", err)
	}
	apply(t, s, ticket, commit)

	// planning first entry: posts the question and waits.
	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, rt, ticketID)
	commit, err = reg[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry handler.Run: %v", err)
	}
	apply(t, s, ticket, commit)

	waiting := getTicket(t, s, ticketID)
	if waiting.State != testStatePlanning {
		t.Fatalf("after first entry: ticket state = %q, want unchanged planning", waiting.State)
	}
	if waiting.WaitingOn == nil || *waiting.WaitingOn != testWaitingQuestions {
		t.Fatalf("after first entry: ticket waiting_on = %v, want questions", waiting.WaitingOn)
	}

	// Answer the one fixture question, exactly the console's POST /answer path.
	answerFixtureQuestion(t, s, ticketID, "b")

	answered := getTicket(t, s, ticketID)
	if answered.WaitingOn != nil {
		t.Fatalf("after answering: ticket waiting_on = %v, want nil (wait cleared)", answered.WaitingOn)
	}

	// planning resume -> building, then the remaining code-only states.
	order := []string{testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging, testStateShipping}
	for _, state := range order {
		ticket = getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("before handler %s: ticket state = %q, want %q", state, ticket.State, state)
		}
		deps = claim(t, s, rt, ticketID)

		handler, ok := reg[state]
		if !ok {
			t.Fatalf("Registry() has no handler for state %s", state)
		}
		commit, err = handler.Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("%s handler.Run: %v", state, err)
		}
		apply(t, s, ticket, commit)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateDone {
		t.Errorf("final ticket state = %q, want done", final.State)
	}
	if final.WaitingOn != nil {
		t.Errorf("final ticket waiting_on = %v, want nil", *final.WaitingOn)
	}
	if final.ClaimOwner != nil {
		t.Error("final ticket claim was not cleared")
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var stateMsgs int
	var questionMsgs int
	for _, m := range msgs {
		switch m.Type {
		case "state":
			stateMsgs++
		case testMsgTypeQuestion:
			questionMsgs++
		}
	}
	// queued->planning, planning->building, building->reviewing,
	// reviewing->judging, judging->shipping, shipping->done: six transitions,
	// six state messages. Planning's first-entry call carries no Next and
	// writes none.
	const wantStateMsgs = 6
	if stateMsgs != wantStateMsgs {
		t.Errorf("state messages = %d, want %d (one per transition)", stateMsgs, wantStateMsgs)
	}
	if questionMsgs != 1 {
		t.Errorf("question messages = %d, want 1 (the one fixture question)", questionMsgs)
	}
}

// answerFixtureQuestion answers ticketID's one open question with option,
// through store.AnswerQuestion, and fails the test if the answer is not
// accepted.
func answerFixtureQuestion(t *testing.T, s *store.Store, ticketID int64, option string) store.AnswerResult {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) == 0 {
		t.Fatal("QuestionsByState(open) = no open questions, want at least one")
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{
		TicketID: ticketID, QuestionID: open[0].ID, Option: option,
	})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
	return result
}

// TestQueuedHandler_TransitionsToPlanning is a focused unit-level check of
// queuedHandler's commit shape (design section 6.5).
func TestQueuedHandler_TransitionsToPlanning(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)

	commit, err := job.Registry()[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStatePlanning || commit.Reason != testReasonPickedUp {
		t.Errorf("commit = (Next=%q, Reason=%q), want (planning, picked up)", commit.Next, commit.Reason)
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil", *commit.Waiting)
	}
}

// advanceQueuedToPlanning claims ticketID, runs the queued handler, applies
// its commit, and returns the ticket freshly reread from the store, now
// sitting in planning.
func advanceQueuedToPlanning(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) store.Ticket {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued Run: %v", err)
	}
	apply(t, s, ticket, commit)
	return getTicket(t, s, ticketID)
}

// TestPlanningHandler_FirstEntry_PostsQuestionAndWaits proves the first-entry
// branch (design section 6.6 diagram, left column, and the mapping table):
// no open session, one fake turn (fake turn 1, the real checked-in
// fixtures/scripts/planning/1.xml), outcome question, one question message
// per Questions[i] attached to the inserted turn-0 run through
// AttachRunToMsgs, and the ticket waits on "questions" with no transition.
func TestPlanningHandler_FirstEntry_PostsQuestionAndWaits(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)

	ticket := advanceQueuedToPlanning(t, s, rt, ticketID)
	deps := claim(t, s, rt, ticketID)

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry Run: %v", err)
	}

	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (no transition on first entry)", commit.Next)
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingQuestions {
		t.Fatalf("commit.Waiting = %v, want questions", commit.Waiting)
	}
	if commit.Session == nil || commit.Session.Job != testStatePlanning || commit.Session.Runtime != "fake" {
		t.Fatalf("commit.Session = %+v, want a fresh (planning, fake) session upsert", commit.Session)
	}
	if commit.Session.ExternalID == nil || *commit.Session.ExternalID == "" {
		t.Error("commit.Session.ExternalID is nil or empty, want the fake's minted session id")
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Turn != 0 || commit.Runs[0].Outcome == nil ||
		*commit.Runs[0].Outcome != string(response.OutcomeQuestion) {
		t.Errorf("commit.Runs = %+v, want exactly one turn-0 run with outcome question", commit.Runs)
	}
	if !commit.AttachRunToMsgs {
		t.Error("commit.AttachRunToMsgs = false, want true")
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %d, want 1 (the one fixture question)", len(commit.Messages))
	}

	msg := commit.Messages[0]
	if msg.Type != testMsgTypeQuestion || msg.Author != testAuthorZing {
		t.Errorf("commit.Messages[0] = (Type=%q, Author=%q), want (question, zing)", msg.Type, msg.Author)
	}
	if msg.State == nil || *msg.State != "open" {
		t.Errorf("commit.Messages[0].State = %v, want open", msg.State)
	}
	const wantBody = "How should the greeting read?\n\nPick the greeting style for GET /hello."
	if msg.Body != wantBody {
		t.Errorf("commit.Messages[0].Body = %q, want %q (title first, then body)", msg.Body, wantBody)
	}

	var payload response.QuestionPayload
	if err = json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if payload.Key != "Q1" {
		t.Errorf("payload.Key = %q, want Q1", payload.Key)
	}
	if payload.Kind != response.QuestionKindQuestion || payload.State != response.QuestionStateOpen {
		t.Errorf("payload = (Kind=%q, State=%q), want (question, open)", payload.Kind, payload.State)
	}
	if payload.Recommended != "b" {
		t.Errorf("payload.Recommended = %q, want b", payload.Recommended)
	}
	if len(payload.Options) != 2 || payload.Options[0].Key != "a" || payload.Options[1].Key != "b" {
		t.Errorf("payload.Options = %+v, want keys [a b]", payload.Options)
	}

	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want unchanged planning", final.State)
	}
	if final.WaitingOn == nil || *final.WaitingOn != testWaitingQuestions {
		t.Errorf("final ticket waiting_on = %v, want questions", final.WaitingOn)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var foundQuestion bool
	for _, m := range msgs {
		if m.Type == testMsgTypeQuestion {
			foundQuestion = true
			if m.RunID == nil {
				t.Error("persisted question message has no run_id, want the turn-0 run's id (AttachRunToMsgs)")
			}
		}
	}
	if !foundQuestion {
		t.Error("no question message persisted")
	}

	sess, ok, err := s.OpenSession(t.Context(), ticketID, testStatePlanning)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !ok {
		t.Fatal("OpenSession(planning): ok = false, want true")
	}
	if sess.ExternalID == nil || *sess.ExternalID != *commit.Session.ExternalID {
		t.Errorf("persisted session.ExternalID = %v, want %s", sess.ExternalID, *commit.Session.ExternalID)
	}
}

// lowercaseKeyScript is a job-agnostic one-question QuestionResponse whose
// question key rides the wire lowercase ("q1"), the shape
// response.Question.Key's own pattern (^[qQ][0-9]+$) allows but the stored
// QuestionPayload.Key's tighter pattern (^Q[0-9]+$) does not.
const lowercaseKeyScript = `<zing job="planning" outcome="question">
  <question key="q1">
    <title>Question one</title>
    <body>Body one.</body>
    <option key="a">Option A</option>
    <option key="b">Option B</option>
    <recommended>a</recommended>
  </question>
  <progress>Asked one question before drafting the plan.</progress>
</zing>`

// TestPlanningHandler_FirstEntry_UppercasesALowercaseWireKey proves the
// stored QuestionPayload.Key is uppercased from the wire key (design section
// 6.6's mapping table, fix 6): a lowercase "q1" on the wire persists as the
// schema-valid "Q1", not the raw lowercase value that would fail
// QuestionPayload's ^Q[0-9]+$ pattern.
func TestPlanningHandler_FirstEntry_UppercasesALowercaseWireKey(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := runtime.NewFake(fstest.MapFS{testPlanningScript1: {Data: []byte(lowercaseKeyScript)}})

	ticket := advanceQueuedToPlanning(t, s, rt, ticketID)
	deps := claim(t, s, rt, ticketID)

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry Run: %v", err)
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %d, want 1", len(commit.Messages))
	}

	var payload response.QuestionPayload
	if unmarshalErr := json.Unmarshal(commit.Messages[0].Payload, &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal question payload: %v", unmarshalErr)
	}
	if payload.Key != "Q1" {
		t.Errorf("payload.Key = %q, want Q1 (uppercased from the wire's lowercase q1)", payload.Key)
	}

	// Persisting must succeed: a lowercase Key would fail the messages/question
	// schema's ^Q[0-9]+$ pattern and CommitHandlerResult would error.
	apply(t, s, ticket, commit)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %d, want 1", len(open))
	}
	var stored response.QuestionPayload
	if unmarshalErr := json.Unmarshal(open[0].Payload, &stored); unmarshalErr != nil {
		t.Fatalf("unmarshal persisted question payload: %v", unmarshalErr)
	}
	if stored.Key != "Q1" {
		t.Errorf("persisted payload.Key = %q, want Q1", stored.Key)
	}
}

// TestPlanningHandler_Resume_AfterBatchAnsweredTransitionsToBuilding proves
// the resume branch (design section 6.6 diagram, right column): an open
// session, the batch's one answered question read back and serialized into
// the prompt, one fake turn (fake turn 2, the session resumed), outcome
// ready, the answered question resolved, and the ticket transitions to
// building.
func TestPlanningHandler_Resume_AfterBatchAnsweredTransitionsToBuilding(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)

	ticket := advanceQueuedToPlanning(t, s, rt, ticketID)
	deps := claim(t, s, rt, ticketID)
	firstEntry, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry Run: %v", err)
	}
	apply(t, s, ticket, firstEntry)

	answerResult := answerFixtureQuestion(t, s, ticketID, "b")
	if !answerResult.WaitCleared {
		t.Fatal("AnswerQuestion: WaitCleared = false, want true (the one-question batch is done)")
	}

	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered): %v", err)
	}
	if len(answered) != 1 {
		t.Fatalf("QuestionsByState(answered) = %d, want 1", len(answered))
	}
	answeredID := answered[0].ID

	// A second, unrelated answered question on the same ticket, attached to
	// an unrelated (build) session's run rather than the planning session's
	// turn-0 run. The resume batch must be scoped to that turn-0 run
	// (design section 6.6: QuestionsByRun, not QuestionsByState across the
	// whole ticket), so this question must stay untouched by the resume
	// below.
	unrelatedID := postAndAnswerUnrelatedQuestion(t, s, rt, ticketID)

	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, rt, ticketID)
	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}

	if commit.Next != testStateBuilding || commit.Reason != "plan ready" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (building, plan ready)", commit.Next, commit.Reason)
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil", *commit.Waiting)
	}
	if commit.Session == nil || commit.Session.ID == nil {
		t.Fatalf("commit.Session = %+v, want an update of the existing session", commit.Session)
	}
	if !commit.Session.BumpResumes {
		t.Error("commit.Session.BumpResumes = false, want true")
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Turn != 1 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != "ready" {
		t.Errorf("commit.Runs = %+v, want exactly one turn-1 run with outcome ready", commit.Runs)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != answeredID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d] (only the planning run's batch, not the unrelated question %d)",
			commit.ResolveQuestions, answeredID, unrelatedID)
	}

	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building", final.State)
	}

	resolved, err := s.QuestionsByState(t.Context(), ticketID, "resolved")
	if err != nil {
		t.Fatalf("QuestionsByState(resolved): %v", err)
	}
	if len(resolved) != 1 || resolved[0].ID != answeredID {
		t.Errorf("QuestionsByState(resolved) = %v, want [question %d]", resolved, answeredID)
	}

	unrelated, err := s.GetMessage(t.Context(), unrelatedID)
	if err != nil {
		t.Fatalf("GetMessage(unrelated): %v", err)
	}
	if unrelated.State == nil || *unrelated.State != "answered" {
		t.Errorf("unrelated question state = %v, want unchanged answered (the resume must not touch it)", unrelated.State)
	}
}

// postAndAnswerUnrelatedQuestion posts one question on ticketID through a
// bare CommitHandlerResult (not the planning handler), attached to a fresh,
// unrelated "build" session's turn-0 run, answers it, and returns its
// message id. Used to prove a resume batch stays scoped to the planning
// session's own turn-0 run rather than sweeping in every answered question
// on the ticket (design section 6.6).
func postAndAnswerUnrelatedQuestion(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) int64 {
	t.Helper()
	ctx := t.Context()

	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q9", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: "a", Options: []response.Option{{Key: "a", Text: "A"}, {Key: "b", Text: "B"}},
	})
	if err != nil {
		t.Fatalf("marshal unrelated question payload: %v", err)
	}

	deps := claim(t, s, rt, ticketID)
	ticket := getTicket(t, s, ticketID)
	externalID := "ext-unrelated"
	openState := "open"
	apply(t, s, ticket, store.HandlerCommit{
		TicketID: ticketID, Owner: deps.Owner, Expires: deps.Expires,
		Session: &store.SessionUpsert{Job: "build", Runtime: "fake", ExternalID: &externalID},
		Runs:    []store.Run{{Turn: 0, Outcome: new("question")}},
		Messages: []store.Message{{
			TicketID: ticketID, Type: "question", Author: testAuthorZing,
			State: &openState, Body: "unrelated question", Payload: payload,
		}},
		AttachRunToMsgs: true,
	})

	open, err := s.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open) after posting the unrelated question: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) after posting the unrelated question = %d, want 1", len(open))
	}
	unrelatedID := open[0].ID

	res, err := s.AnswerQuestion(ctx, store.AnswerInput{TicketID: ticketID, QuestionID: unrelatedID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion(unrelated): %v", err)
	}
	if !res.Accepted {
		t.Fatalf("AnswerQuestion(unrelated): Accepted = false, Conflict = %q, want accepted", res.Conflict)
	}
	return unrelatedID
}

// multiQuestionScript is a job-agnostic two-question QuestionResponse,
// inline so TestPlanningHandler_Resume_MultiQuestionBatchGatesOnAllAnswered
// proves the batch gate without touching the real checked-in one-question
// fixture (design section 6.6: "at most one batch is open per ticket at a
// time").
const multiQuestionScript = `<zing job="planning" outcome="question">
  <question key="Q1">
    <title>Question one</title>
    <body>Body one.</body>
    <option key="a">Option A1</option>
    <option key="b">Option B1</option>
    <recommended>a</recommended>
  </question>
  <question key="Q2">
    <title>Question two</title>
    <body>Body two.</body>
    <option key="a">Option A2</option>
    <option key="b">Option B2</option>
    <recommended>b</recommended>
  </question>
  <progress>Asked two questions before drafting the plan.</progress>
</zing>`

// multiQuestionReadyScript is a minimal ready document this test's fake turn
// 2 serves once the two-question batch is fully answered. The handler reads
// only Header().Outcome at runtime (design section 6.6: "the skeleton does
// not validate the response at runtime"), so this need not be
// Validate-clean; it only needs to Parse.
const multiQuestionReadyScript = `<zing job="planning" outcome="ready"></zing>`

// TestPlanningHandler_Resume_MultiQuestionBatchGatesOnAllAnswered proves the
// batch is fully-answered-gated (design section 6.6): a two-question first
// entry posts both messages under the one turn-0 run; answering only one
// leaves the wait set; only once both are answered does the wait clear, and
// the resume commit resolves both question ids together.
func TestPlanningHandler_Resume_MultiQuestionBatchGatesOnAllAnswered(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := runtime.NewFake(fstest.MapFS{
		testPlanningScript1: {Data: []byte(multiQuestionScript)},
		"planning/2.xml":    {Data: []byte(multiQuestionReadyScript)},
	})

	ticket := advanceQueuedToPlanning(t, s, rt, ticketID)
	deps := claim(t, s, rt, ticketID)
	firstEntry, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry Run: %v", err)
	}
	if len(firstEntry.Messages) != 2 {
		t.Fatalf("commit.Messages = %d, want 2 (the two-question batch)", len(firstEntry.Messages))
	}
	apply(t, s, ticket, firstEntry)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("QuestionsByState(open) = %d, want 2", len(open))
	}

	// Answer the first question only: the batch is not done, so the wait
	// must stay set (this is the store-level gate that keeps the dispatcher
	// from ever calling the handler on a partial batch).
	first, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion(first): %v", err)
	}
	if !first.Accepted || first.WaitCleared {
		t.Fatalf("AnswerQuestion(first) = %+v, want Accepted=true, WaitCleared=false", first)
	}
	midway := getTicket(t, s, ticketID)
	if midway.WaitingOn == nil || *midway.WaitingOn != testWaitingQuestions {
		t.Fatalf("midway ticket waiting_on = %v, want still questions", midway.WaitingOn)
	}

	second, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[1].ID, Option: "b"})
	if err != nil {
		t.Fatalf("AnswerQuestion(second): %v", err)
	}
	if !second.Accepted || !second.WaitCleared {
		t.Fatalf("AnswerQuestion(second) = %+v, want Accepted=true, WaitCleared=true", second)
	}

	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered): %v", err)
	}
	if len(answered) != 2 {
		t.Fatalf("QuestionsByState(answered) = %d, want 2", len(answered))
	}

	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, rt, ticketID)
	resume, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}
	if resume.Next != testStateBuilding {
		t.Errorf("resume commit.Next = %q, want building", resume.Next)
	}

	wantIDs := []int64{open[0].ID, open[1].ID}
	if len(resume.ResolveQuestions) != len(wantIDs) {
		t.Fatalf("resume commit.ResolveQuestions = %v, want %v", resume.ResolveQuestions, wantIDs)
	}
	for i, id := range wantIDs {
		if resume.ResolveQuestions[i] != id {
			t.Errorf("resume commit.ResolveQuestions[%d] = %d, want %d", i, resume.ResolveQuestions[i], id)
		}
	}

	apply(t, s, ticket, resume)

	resolved, err := s.QuestionsByState(t.Context(), ticketID, "resolved")
	if err != nil {
		t.Fatalf("QuestionsByState(resolved): %v", err)
	}
	if len(resolved) != 2 {
		t.Errorf("QuestionsByState(resolved) = %d, want 2 (both resolved together)", len(resolved))
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building", final.State)
	}
}

// TestBuildingHandler_OkTransitionsToReviewing proves the building handler:
// a fresh session, one fake turn (job build, label 1), outcome ok, its
// turn-0 run persisted, and the ticket advances to reviewing.
func TestBuildingHandler_OkTransitionsToReviewing(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning)

	ticket := getTicket(t, s, ticketID)
	if ticket.State != testStateBuilding {
		t.Fatalf("ticket state = %q, want building", ticket.State)
	}
	deps := claim(t, s, fakeRuntime(t), ticketID)

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("building Run: %v", err)
	}
	if commit.Next != testStateReviewing || commit.Reason != "build done" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (reviewing, build done)", commit.Next, commit.Reason)
	}
	if commit.Session == nil || commit.Session.Job != "build" {
		t.Fatalf("commit.Session = %+v, want a fresh build session", commit.Session)
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != "ok" {
		t.Errorf("commit.Runs = %+v, want exactly one turn-0 run with outcome ok", commit.Runs)
	}

	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStateReviewing {
		t.Errorf("final ticket state = %q, want reviewing", final.State)
	}
}

// advanceThroughStates runs the handler registered for each of states, in
// order, applying every commit, so a test can arrange a ticket already
// sitting in the state right after the last one named (each named state
// gets its own fresh runtime.Fake, since none of the skeleton handlers
// resumes a prior handler's session). Planning is a special case: since task
// 7 one handler call only posts the question and waits, so this drives
// planning's first entry, answers the one fixture question, and drives its
// resume too, landing the ticket in building exactly as every other state's
// single silent transition does.
func advanceThroughStates(t *testing.T, s *store.Store, ticketID int64, states ...string) {
	t.Helper()
	reg := job.Registry()
	for _, state := range states {
		ticket := getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("advanceThroughStates(%s): ticket state = %q, want %q", state, ticket.State, state)
		}

		if state == testStatePlanning {
			advancePlanningWithAnAnswer(t, s, ticketID)
			continue
		}

		deps := claim(t, s, fakeRuntime(t), ticketID)
		commit, err := reg[state].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("%s handler.Run: %v", state, err)
		}
		apply(t, s, ticket, commit)
	}
}

// advancePlanningWithAnAnswer drives the planning handler's first entry
// (posts the one fixture question and waits), answers it through
// store.AnswerQuestion exactly as the console's POST /answer would, then
// drives the resume (resolves the batch and transitions to building). Both
// calls share one runtime.Fake, since a resume must reuse the fake session
// its first entry minted.
func advancePlanningWithAnAnswer(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	rt := fakeRuntime(t)
	reg := job.Registry()

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, rt, ticketID)
	firstEntry, err := reg[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry Run: %v", err)
	}
	apply(t, s, ticket, firstEntry)

	answerFixtureQuestion(t, s, ticketID, "b")

	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, rt, ticketID)
	resume, err := reg[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}
	apply(t, s, ticket, resume)
}

// TestReviewingHandler_TransitionsToJudging, TestJudgingHandler_TransitionsToShipping,
// and TestShippingHandler_TransitionsToDone cover the three remaining
// code-only handlers (design section 6.5).

func TestReviewingHandler_TransitionsToJudging(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning, testStateBuilding)

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateReviewing].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStateJudging || commit.Reason != "review clean" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (judging, review clean)", commit.Next, commit.Reason)
	}
	apply(t, s, ticket, commit)
	if final := getTicket(t, s, ticketID); final.State != testStateJudging {
		t.Errorf("final ticket state = %q, want judging", final.State)
	}
}

func TestJudgingHandler_TransitionsToShipping(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing)

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateJudging].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStateShipping || commit.Reason != "judge passed" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (shipping, judge passed)", commit.Next, commit.Reason)
	}
	apply(t, s, ticket, commit)
	if final := getTicket(t, s, ticketID); final.State != testStateShipping {
		t.Errorf("final ticket state = %q, want shipping", final.State)
	}
}

func TestShippingHandler_TransitionsToDone(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging)

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateShipping].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStateDone || commit.Reason != "shipped" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (done, shipped)", commit.Next, commit.Reason)
	}
	apply(t, s, ticket, commit)
	if final := getTicket(t, s, ticketID); final.State != testStateDone {
		t.Errorf("final ticket state = %q, want done", final.State)
	}
}

// errorScript is a job-agnostic ErrorResponse document, used only by
// TestPlanningHandler_ErrorOutcomeEscalates: task 4 never checks a real
// error script into fixtures/scripts (that fixture and its dispatcher-level
// exercise are task 8's, design section 12 row 8), but section 6.7's error
// branch is task 4's own code and needs a turn to drive it.
const errorScript = `<zing job="planning" outcome="error">
  <error code="plan_gap">
    <what>could not determine the goals</what>
    <why>the ticket body names no concrete behavior</why>
    <tried>read the ticket body twice</tried>
  </error>
</zing>`

// TestPlanningHandler_ErrorOutcomeEscalates proves the section 6.7 error
// branch: an escalation message carrying RunError's fields and the fixed
// local options, Waiting set to error, and no state transition.
func TestPlanningHandler_ErrorOutcomeEscalates(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	queuedCommit, err := job.Registry()[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued Run: %v", err)
	}
	apply(t, s, ticket, queuedCommit)

	errRT := runtime.NewFake(fstest.MapFS{testPlanningScript1: {Data: []byte(errorScript)}})
	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, errRT, ticketID)

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (no transition on error)", commit.Next)
	}
	if commit.Waiting == nil || *commit.Waiting != "error" {
		t.Fatalf("commit.Waiting = %v, want error", commit.Waiting)
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Type != "escalation" || commit.Messages[0].Author != testAuthorZing {
		t.Fatalf("commit.Messages = %+v, want exactly one zing escalation message", commit.Messages)
	}

	var payload response.EscalationPayload
	if err := json.Unmarshal(commit.Messages[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != "plan_gap" {
		t.Errorf("payload.Code = %q, want plan_gap", payload.Code)
	}
	if payload.What == "" || payload.Why == "" {
		t.Errorf("payload = %+v, want non-empty What and Why", payload)
	}
	wantOptions := []string{"retry", testStatePlanning, "abandon"}
	if len(payload.Options) != len(wantOptions) {
		t.Fatalf("payload.Options = %v, want %v", payload.Options, wantOptions)
	}
	for i, opt := range wantOptions {
		if payload.Options[i] != opt {
			t.Errorf("payload.Options[%d] = %q, want %q", i, payload.Options[i], opt)
		}
	}

	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want unchanged planning", final.State)
	}
	if final.WaitingOn == nil || *final.WaitingOn != "error" {
		t.Errorf("final ticket waiting_on = %v, want error", final.WaitingOn)
	}
}
