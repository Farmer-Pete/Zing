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

// TestSilentRing_QueuedToDone drives every one of the six skeleton
// handlers, in pipeline order, against a real temp store and the real
// checked-in fixture scripts, applying each returned commit and reclaiming
// between states exactly as the dispatcher will (design section 6.8). It
// asserts the ticket reaches done and that a state message was written on
// every one of the six transitions (design section 6.3, 7.1).
func TestSilentRing_QueuedToDone(t *testing.T) {
	s := newJobTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s)

	reg := job.Registry()
	order := []string{testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging, testStateShipping}

	for _, state := range order {
		ticket := getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("before handler %s: ticket state = %q, want %q", state, ticket.State, state)
		}
		deps := claim(t, s, rt, ticketID)

		handler, ok := reg[state]
		if !ok {
			t.Fatalf("Registry() has no handler for state %s", state)
		}
		commit, err := handler.Run(t.Context(), ticket, deps)
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
	var states []string
	for _, m := range msgs {
		if m.Type == "state" {
			states = append(states, m.Type)
		}
	}
	if len(states) != len(order) {
		t.Errorf("state messages = %d, want %d (one per transition)", len(states), len(order))
	}
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
	if commit.Next != testStatePlanning || commit.Reason != "picked up" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (planning, picked up)", commit.Next, commit.Reason)
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil", *commit.Waiting)
	}
}

// TestPlanningHandler_ReadyOnTurnOneTransitionsToBuilding proves the task 4
// silent-ring planning handler: no open session, one fake turn, outcome
// ready, a fresh session and its turn-0 run persisted, and the ticket
// advances straight to building (design section 6.6, section 12 row 4).
func TestPlanningHandler_ReadyOnTurnOneTransitionsToBuilding(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)

	// Advance the ticket to planning the same way the ring does: through
	// the queued handler's own commit, not a private store setter.
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	queuedCommit, err := job.Registry()[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued Run: %v", err)
	}
	apply(t, s, ticket, queuedCommit)

	ticket = getTicket(t, s, ticketID)
	if ticket.State != testStatePlanning {
		t.Fatalf("ticket state = %q, want planning", ticket.State)
	}
	deps = claim(t, s, deps.Runtime, ticketID)

	commit, err := job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning Run: %v", err)
	}
	if commit.Next != testStateBuilding || commit.Reason != "plan ready" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (building, plan ready)", commit.Next, commit.Reason)
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil (no question in the task 4 handler)", *commit.Waiting)
	}
	if commit.Session == nil {
		t.Fatal("commit.Session = nil, want a fresh session upsert")
	}
	if commit.Session.Job != testStatePlanning || commit.Session.Runtime != "fake" {
		t.Errorf("commit.Session = (Job=%q, Runtime=%q), want (planning, fake)", commit.Session.Job, commit.Session.Runtime)
	}
	if commit.Session.ExternalID == nil || *commit.Session.ExternalID == "" {
		t.Error("commit.Session.ExternalID is nil or empty, want the fake's minted session id")
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Turn != 0 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != "ready" {
		t.Errorf("commit.Runs = %+v, want exactly one turn-0 run with outcome ready", commit.Runs)
	}

	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building", final.State)
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
// sitting in the state right after the last one named (each handler starts
// its own fresh runtime.Fake, since none of the skeleton handlers resumes a
// prior handler's session).
func advanceThroughStates(t *testing.T, s *store.Store, ticketID int64, states ...string) {
	t.Helper()
	reg := job.Registry()
	for _, state := range states {
		ticket := getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("advanceThroughStates(%s): ticket state = %q, want %q", state, ticket.State, state)
		}
		deps := claim(t, s, fakeRuntime(t), ticketID)
		commit, err := reg[state].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("%s handler.Run: %v", state, err)
		}
		apply(t, s, ticket, commit)
	}
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

	errRT := runtime.NewFake(fstest.MapFS{"planning/1.xml": {Data: []byte(errorScript)}})
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
	if len(commit.Messages) != 1 || commit.Messages[0].Type != "escalation" || commit.Messages[0].Author != "zing" {
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
