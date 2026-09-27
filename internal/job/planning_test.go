// planning_test.go tests the real planning handler (planning.go, task 6):
// classify, the planning first turn, an answered round's resume, D14's
// invalid-output retry and escalation, the resumes_exhausted cap, a
// canceled runtime, and the temporary "ready" shortcut. It reuses
// skeleton_test.go's shared fixtures (newJobTestStore, seedQueuedTicket,
// claim, apply, fakeRuntime, getTicket, testModels, testBudget) and drives
// job.Registry()["planning"].Run directly, exactly as skeleton_test.go's
// own handler tests do.
package job_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// ---- scripted and dispatching test runtimes --------------------------------

// scriptedStep is one canned (RunResult, error) pair a scriptedRuntime
// returns for one call, in order.
type scriptedStep struct {
	res runtime.RunResult
	err error
}

// scriptedRuntime returns steps[i] for its i'th call and records every
// request, so a test can script a runtime failure (ErrStart, ErrCanceled,
// *InvalidOutputError) or a hand-built Response no fixture script can
// express, and still inspect exactly what runJob sent it.
type scriptedRuntime struct {
	t     *testing.T
	steps []scriptedStep
	calls int
	reqs  []runtime.RunRequest
}

func (s *scriptedRuntime) Run(_ context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	s.t.Helper()
	s.reqs = append(s.reqs, req)
	if s.calls >= len(s.steps) {
		s.t.Fatalf("scriptedRuntime: call %d has no scripted step (only %d scripted)", s.calls+1, len(s.steps))
	}
	step := s.steps[s.calls]
	s.calls++
	return step.res, step.err
}

// byJobRuntime dispatches to a different runtime.Runtime per req.Job, so a
// test can give classify a real fixture-backed Fake while planning runs
// against a scriptedRuntime, even though machine.toml names the same
// runtime ("claude") for both.
type byJobRuntime struct {
	t     *testing.T
	byJob map[response.Job]runtime.Runtime
}

func (b byJobRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	b.t.Helper()
	rt, ok := b.byJob[req.Job]
	if !ok {
		b.t.Fatalf("byJobRuntime: no runtime registered for job %s", req.Job)
	}
	return rt.Run(ctx, req)
}

// claimWithRuntimes is claim (skeleton_test.go), but with rt registered
// under every machine.toml runtime name instead of one shared fakeRuntime,
// so a test's byJobRuntime or scriptedRuntime stands in for "claude"
// (classify and planning's own machine.toml runtime name), "codex", and
// "fake" alike.
func claimWithRuntimes(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) job.Deps {
	t.Helper()
	owner := "test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	set, err := runtime.NewSet(map[string]runtime.Runtime{"claude": rt, testRuntimeCodex: rt, testRuntimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return job.Deps{
		Store: s, Runtimes: set, Machine: testMachine(t), Models: testModels, Budget: testBudget,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, tid int64, su store.SessionUpsert, model string) (store.Reserved, error) {
			return s.Reserve(ctx, tid, owner, expires, su, model)
		},
	}
}

// questionResult builds a scriptedStep whose Response is a minimal
// QuestionResponse: enough for questionOutcomeCommit to post one message,
// the shape the handler itself never re-validates at runtime (design
// section 6.6, the same rule the skeleton always followed).
func questionResult(forJob response.Job, sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response: &response.QuestionResponse{
			Job: forJob, Outcome: response.OutcomeQuestion,
			Questions: []response.Question{{
				Key: "q1", Title: "A question", Body: "Body.",
				Options:     []response.Option{{Key: "a", Text: "Option A"}, {Key: "b", Text: "Option B"}},
				Recommended: "a",
			}},
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// classifyResult builds a scriptedStep whose Response is a minimal, valid
// ClassifyResponse.
func classifyResult(outcome response.Outcome, sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response: &response.ClassifyResponse{
			Job: response.JobClassify, Outcome: outcome, Reason: "one deciding fact",
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// invalidResult builds a scriptedStep that fails with *runtime.InvalidOutputError.
func invalidResult(reason, sessionID string) scriptedStep {
	return scriptedStep{
		res: runtime.RunResult{SessionID: sessionID, ExitCode: 1, AgentTime: time.Second},
		err: &runtime.InvalidOutputError{Reason: reason},
	}
}

// runPlanning runs job.Registry()["planning"] once against deps built for
// ticketID.
func runPlanning(t *testing.T, s *store.Store, deps job.Deps, ticketID int64) (store.HandlerCommit, error) {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	return job.Registry()[testStatePlanning].Run(t.Context(), ticket, deps)
}

// ---- classify -------------------------------------------------------------

// TestPlanningHandler_Classify_StoresKindAndSessionExternalID proves
// section 6.1's success path: SetKind, a terminalized run recording the
// Fake's own outcome, and the fresh session's external_id, with the ticket
// left in planning, not waiting (design section 6.8's classify row).
func TestPlanningHandler_Classify_StoresKindAndSessionExternalID(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)

	deps := claim(t, s, rt, ticketID)
	commit, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("planning (classify) Run: %v", err)
	}
	if commit.SetKind == nil || (*commit.SetKind != "bug" && *commit.SetKind != "feature") {
		t.Fatalf("commit.SetKind = %v, want bug or feature", commit.SetKind)
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != *commit.SetKind {
		t.Fatalf("commit.Runs = %+v, want one terminalized run with outcome %s", commit.Runs, *commit.SetKind)
	}
	if commit.Session == nil || commit.Session.ExternalID == nil || *commit.Session.ExternalID == "" {
		t.Fatalf("commit.Session = %+v, want a fresh session with a non-empty external id", commit.Session)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (classify carries no transition)", commit.Next)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning || final.WaitingOn != nil {
		t.Errorf("final ticket = (state=%q, waiting_on=%v), want (planning, nil)", final.State, final.WaitingOn)
	}
	if final.Kind == nil || *final.Kind != *commit.SetKind {
		t.Errorf("final ticket.Kind = %v, want %s", final.Kind, *commit.SetKind)
	}
}

// TestPlanningHandler_FirstTurn_PostsRealQuestionsBatchWithAllocatedKeys
// proves section 6.2's success path once classify has set a kind: a fresh
// session with its own external_id, one question message per
// Questions[i] attached to the reserved run, each carrying an uppercased
// Q<n> key, and the ticket waiting on "questions" (design section 6.8's
// planning "questions" row).
func TestPlanningHandler_FirstTurn_PostsRealQuestionsBatchWithAllocatedKeys(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)

	// classify: real fixture, sets kind.
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID))

	// planning first turn: real fixture (fixtures/scripts/planning/1.xml).
	deps := claim(t, s, rt, ticketID)
	commit, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("planning first-entry Run: %v", err)
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingQuestions {
		t.Fatalf("commit.Waiting = %v, want questions", commit.Waiting)
	}
	if !commit.AttachRunToMsgs || len(commit.Runs) != 1 {
		t.Fatalf("commit = (AttachRunToMsgs=%v, Runs=%+v), want (true, one run)", commit.AttachRunToMsgs, commit.Runs)
	}
	if commit.Session == nil || commit.Session.ExternalID == nil || *commit.Session.ExternalID == "" {
		t.Fatalf("commit.Session = %+v, want a fresh session with a non-empty external id", commit.Session)
	}
	if len(commit.Messages) == 0 {
		t.Fatal("commit.Messages is empty, want at least one question")
	}
	for _, m := range commit.Messages {
		var qp response.QuestionPayload
		if unmarshalErr := json.Unmarshal(m.Payload, &qp); unmarshalErr != nil {
			t.Fatalf("unmarshal question payload: %v", unmarshalErr)
		}
		if !strings.HasPrefix(qp.Key, "Q") {
			t.Errorf("question payload.Key = %q, want an uppercased Q<n> key", qp.Key)
		}
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if m.Type == testMsgTypeQuestion {
			found = true
			if m.RunID == nil {
				t.Error("persisted question has no run_id, want the reserved run's id")
			}
		}
	}
	if !found {
		t.Error("no question message persisted")
	}
}

// mustPlanning runs the planning handler once and fails the test on error,
// returning the commit for the caller to apply.
func mustPlanning(t *testing.T, s *store.Store, deps job.Deps, ticketID int64) store.HandlerCommit {
	t.Helper()
	commit, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("planning Run: %v", err)
	}
	return commit
}

// ---- resume: fenced answers, resumes bumped, round resolved ---------------

// TestPlanningHandler_Resume_AnsweredRoundBumpsResumesAndFencesTheAnswer
// proves section 6.4: an answered round resumes the open session
// (BumpResumes true, ResolveQuestions the round's question ids), and the
// resumed prompt fences the owner's answer text behind the untrusted-input
// markers (design D15, section 4.2).
func TestPlanningHandler_Resume_AnsweredRoundBumpsResumesAndFencesTheAnswer(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify

	// planning first turn: real fixture posts Q1.
	firstTurnDeps := claim(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, firstTurnDeps, ticketID))

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	answerRes, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "b"})
	if err != nil || !answerRes.Accepted {
		t.Fatalf("AnswerQuestion: %+v, %v", answerRes, err)
	}

	// A recording wrapper around the same Fake so the session it minted for
	// the first turn is the one the resume call reuses.
	rec := &recordingRuntime{rt: rt}
	resumeDeps := claim(t, s, rec, ticketID)
	commit, err := runPlanning(t, s, resumeDeps, ticketID)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}
	if commit.Session == nil || commit.Session.ID == nil || !commit.Session.BumpResumes {
		t.Fatalf("commit.Session = %+v, want an existing session id with BumpResumes true", commit.Session)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != open[0].ID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, open[0].ID)
	}

	if rec.lastReq.Prompt == "" {
		t.Fatal("recordingRuntime saw an empty resume prompt")
	}
	if !strings.Contains(rec.lastReq.Prompt, "<<<UNTRUSTED ") || !strings.Contains(rec.lastReq.Prompt, "<<<END ") {
		t.Errorf("resume prompt does not carry the fence markers around the answer:\n%s", rec.lastReq.Prompt)
	}
	// fixtures/scripts/planning/1.xml's option "b" text (design section 12
	// task 8's fixture), the chosen option this test answers with below.
	if !strings.Contains(rec.lastReq.Prompt, "hello, world") {
		t.Errorf("resume prompt does not mention the chosen option text:\n%s", rec.lastReq.Prompt)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	resolved, err := s.QuestionsByState(t.Context(), ticketID, "resolved")
	if err != nil || len(resolved) != 1 || resolved[0].ID != open[0].ID {
		t.Errorf("QuestionsByState(resolved) = %v, %v, want [%d]", resolved, err, open[0].ID)
	}
}

// recordingRuntime wraps rt and records the last request it saw, so a test
// can inspect the assembled prompt runJob actually sent.
type recordingRuntime struct {
	rt      runtime.Runtime
	lastReq runtime.RunRequest
}

func (r *recordingRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	r.lastReq = req
	return r.rt.Run(ctx, req)
}

// TestPlanningHandler_Resume_ReadyOutcomeIsATemporaryShortcutToBuilding
// pins task 6's TEMPORARY shortcut by name: a ready outcome on resume goes
// straight to building, with a Reason naming itself a shortcut, rather
// than section 6.5's real cohort store and review tick (task 7).
func TestPlanningHandler_Resume_ReadyOutcomeIsATemporaryShortcutToBuilding(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1

	answerFixtureQuestion(t, s, ticketID, "b")

	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}
	if commit.Next != testStateBuilding {
		t.Errorf("commit.Next = %q, want building", commit.Next)
	}
	if !strings.Contains(commit.Reason, "shortcut") {
		t.Errorf("commit.Reason = %q, want it to name itself a temporary shortcut", commit.Reason)
	}
}

// ---- an answered classify round re-runs classify fresh --------------------

// TestPlanningHandler_Classify_AnsweredQuestionRoundRerunsClassifyFresh
// proves entry decision step 1(d): classify's own universal question
// answered restarts classify fresh with the round's rendered answers, and
// resolves the round in that same commit.
func TestPlanningHandler_Classify_AnsweredQuestionRoundRerunsClassifyFresh(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		questionResult(response.JobClassify, "cls-sess-1"),
		classifyResult(response.OutcomeFeature, "cls-sess-2"),
	}}

	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if firstCommit.Waiting == nil || *firstCommit.Waiting != testWaitingQuestions {
		t.Fatalf("first commit.Waiting = %v, want questions", firstCommit.Waiting)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	if _, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"}); answerErr != nil {
		t.Fatalf("AnswerQuestion: %v", answerErr)
	}

	secondCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second planning Run: %v", err)
	}
	if secondCommit.SetKind == nil || *secondCommit.SetKind != "feature" {
		t.Fatalf("second commit.SetKind = %v, want feature", secondCommit.SetKind)
	}
	if len(secondCommit.ResolveQuestions) != 1 || secondCommit.ResolveQuestions[0] != open[0].ID {
		t.Errorf("second commit.ResolveQuestions = %v, want [%d]", secondCommit.ResolveQuestions, open[0].ID)
	}
	if len(rt.reqs) != 2 {
		t.Fatalf("scriptedRuntime saw %d calls, want 2", len(rt.reqs))
	}
	if !strings.Contains(rt.reqs[1].Prompt, "Option A") {
		t.Errorf("second classify prompt does not carry the answer's option text:\n%s", rt.reqs[1].Prompt)
	}

	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	resolved, err := s.QuestionsByState(t.Context(), ticketID, "resolved")
	if err != nil || len(resolved) != 1 {
		t.Fatalf("QuestionsByState(resolved) = %v, %v, want exactly one", resolved, err)
	}
}

// ---- D14: invalid output ----------------------------------------------------

// TestPlanningHandler_Classify_D14_SecondConsecutiveInvalidEscalates proves
// D14's two-strike rule for classify: the first invalid output only writes
// the marker and leaves the ticket not waiting, kind still unset; the
// second consecutive invalid output (classify re-runs fresh, since kind is
// still nil) escalates response_invalid in that same commit.
func TestPlanningHandler_Classify_D14_SecondConsecutiveInvalidEscalates(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

	const reason = "no zing element in final message"
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		invalidResult(reason, "cls-sess-1"),
		invalidResult(reason, "cls-sess-2"),
	}}

	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if firstCommit.Escalation != nil {
		t.Fatalf("first invalid commit.Escalation = %+v, want nil (first strike)", firstCommit.Escalation)
	}
	if firstCommit.Waiting != nil {
		t.Errorf("first invalid commit.Waiting = %v, want nil", *firstCommit.Waiting)
	}
	if len(firstCommit.Messages) != 1 || firstCommit.Messages[0].Type != "update" {
		t.Fatalf("first invalid commit.Messages = %+v, want one update marker", firstCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	after := getTicket(t, s, ticketID)
	if after.WaitingOn != nil || after.Kind != nil {
		t.Fatalf("after first invalid: ticket = (waiting_on=%v, kind=%v), want (nil, nil)", after.WaitingOn, after.Kind)
	}

	secondCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second planning Run: %v", err)
	}
	if secondCommit.Escalation == nil {
		t.Fatal("second invalid commit.Escalation is nil, want response_invalid")
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(mustEscalationPayload(t, secondCommit), &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
	}
	if payload.Code != "response_invalid" {
		t.Errorf("payload.Code = %q, want response_invalid", payload.Code)
	}
	if payload.Origin != "classify" {
		t.Errorf("payload.Origin = %q, want classify", payload.Origin)
	}
	if payload.SessionID == nil {
		t.Error("payload.SessionID is nil, want the second run's session id")
	}
	if secondCommit.Waiting == nil || *secondCommit.Waiting != testWaitingQuestions {
		t.Fatalf("second invalid commit.Waiting = %v, want questions", secondCommit.Waiting)
	}

	apply(t, s, getTicket(t, s, ticketID), secondCommit)
}

// mustEscalationPayload extracts commit.Escalation.Payload as raw JSON,
// failing the test if there is no escalation.
func mustEscalationPayload(t *testing.T, commit store.HandlerCommit) []byte {
	t.Helper()
	if commit.Escalation == nil {
		t.Fatal("commit carries no Escalation")
	}
	raw, err := json.Marshal(commit.Escalation.Payload)
	if err != nil {
		t.Fatalf("marshal escalation payload: %v", err)
	}
	return raw
}

// TestPlanningHandler_Classify_D14_InvalidThenValidThenInvalidDoesNotEscalate
// proves the chain resets on a valid terminalized run (design D14): an
// invalid output, then a valid (question) output, then, once that
// question is answered and classify re-runs fresh, a second invalid
// output -- which must NOT escalate, since the valid run in between reset
// the consecutive count to zero.
func TestPlanningHandler_Classify_D14_InvalidThenValidThenInvalidDoesNotEscalate(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

	const reason = "no zing element in final message"
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		invalidResult(reason, "cls-sess-1"),
		questionResult(response.JobClassify, "cls-sess-2"),
		invalidResult(reason, "cls-sess-3"),
	}}

	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)) // invalid, n=0

	questionCommit := mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID) // valid question, resets chain
	if questionCommit.Waiting == nil || *questionCommit.Waiting != testWaitingQuestions {
		t.Fatalf("question commit.Waiting = %v, want questions", questionCommit.Waiting)
	}
	apply(t, s, getTicket(t, s, ticketID), questionCommit)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	if _, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"}); answerErr != nil {
		t.Fatalf("AnswerQuestion: %v", answerErr)
	}

	thirdCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID) // fresh classify, then invalid
	if err != nil {
		t.Fatalf("third planning Run: %v", err)
	}
	if thirdCommit.Escalation != nil {
		t.Errorf("third invalid commit.Escalation = %+v, want nil (the valid run in between reset the chain)", thirdCommit.Escalation)
	}
	if thirdCommit.Waiting != nil {
		t.Errorf("third invalid commit.Waiting = %v, want nil", *thirdCommit.Waiting)
	}
	apply(t, s, getTicket(t, s, ticketID), thirdCommit)
}

// TestPlanningHandler_FirstTurn_InvalidRecordsSessionAndStep4ResumesWithReason
// proves section 5.4's planning-specific rule: an invalid first turn still
// records its session id (D13: "every path except ErrStart"), which makes
// the session SessionOpen on the next tick, and step 4 resumes it with the
// closed reason wrapped in the fixed retry sentence, rather than starting a
// new session.
func TestPlanningHandler_FirstTurn_InvalidRecordsSessionAndStep4ResumesWithReason(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	planningRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		invalidResult(reason, "plan-sess-1"),
	}}
	rt := byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{
		response.JobClassify: fake, response.JobPlanning: planningRT,
	}}

	firstTurnCommit := mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if firstTurnCommit.Session == nil || firstTurnCommit.Session.ExternalID == nil || *firstTurnCommit.Session.ExternalID != "plan-sess-1" {
		t.Fatalf("first-turn commit.Session = %+v, want external id plan-sess-1", firstTurnCommit.Session)
	}
	apply(t, s, getTicket(t, s, ticketID), firstTurnCommit)

	sess, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state = %v, want SessionOpen", state)
	}

	planningRT.steps = append(planningRT.steps, questionResult(response.JobPlanning, *sess.ExternalID))
	resumeCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("step-4 resume Run: %v", err)
	}
	if len(planningRT.reqs) != 2 {
		t.Fatalf("planningRT saw %d calls, want 2 (the invalid first turn, then the step-4 resume)", len(planningRT.reqs))
	}
	resumeReq := planningRT.reqs[1]
	if resumeReq.SessionID != *sess.ExternalID {
		t.Errorf("resume RunRequest.SessionID = %q, want %q (reused, not a new session)", resumeReq.SessionID, *sess.ExternalID)
	}
	if !strings.Contains(resumeReq.Prompt, reason) {
		t.Errorf("resume prompt does not carry the invalid-output reason:\n%s", resumeReq.Prompt)
	}
	if resumeCommit.Waiting == nil || *resumeCommit.Waiting != testWaitingQuestions {
		t.Fatalf("step-4 resume commit.Waiting = %v, want questions", resumeCommit.Waiting)
	}
}

// ---- exec failure and runtime.ErrCanceled -----------------------------------

// TestPlanningHandler_FirstTurn_ErrStartLeavesAnIdlessSessionAndEscalates
// proves the exec-failure row for a first turn (design section 6.8): the
// reserved run terminalizes as error, escalates runtime_exec_failed with
// Origin planning_first, and -- since ErrStart is the one path that never
// learns a session id -- the session stays idless, which LatestSession
// treats the same as none.
func TestPlanningHandler_FirstTurn_ErrStartLeavesAnIdlessSessionAndEscalates(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	planningRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrStart},
	}}
	rt := byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{
		response.JobClassify: fake, response.JobPlanning: planningRT,
	}}

	commit := mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if commit.Session != nil {
		t.Errorf("commit.Session = %+v, want nil (ErrStart never learns a session id)", commit.Session)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want runtime_exec_failed")
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(mustEscalationPayload(t, commit), &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
	}
	if payload.Code != "runtime_exec_failed" || payload.Origin != "planning_first" {
		t.Errorf("payload = (Code=%q, Origin=%q), want (runtime_exec_failed, planning_first)", payload.Code, payload.Origin)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)

	_, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if state != store.SessionIdless {
		t.Fatalf("session state = %v, want SessionIdless", state)
	}
}

// TestPlanningHandler_Classify_ErrCanceledReturnsWithNoCommit proves D13's
// shutdown rule: runtime.ErrCanceled returns unchanged with a wholly empty
// commit, so the caller applies nothing and the dispatcher leaves the
// claim for ExpireClaims to reconcile.
func TestPlanningHandler_Classify_ErrCanceledReturnsWithNoCommit(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}

	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}
	if commit.TicketID != 0 || commit.Session != nil || len(commit.Runs) != 0 || commit.Escalation != nil || commit.Next != "" {
		t.Errorf("commit = %+v, want the zero value (no commit)", commit)
	}
}

// ---- resumes_exhausted (entry step 3) --------------------------------------

// bumpResumesToCap issues maxResumes bare BumpResumes commits against
// sessionID, fast-forwarding its resumes counter to the cap without
// scripting maxResumes real planning turns.
func bumpResumesToCap(t *testing.T, s *store.Store, ticketID, sessionID int64, maxResumes int, owner string, expires time.Time) {
	t.Helper()
	for range maxResumes {
		claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
		if err != nil || !claimed {
			t.Fatalf("bumpResumesToCap: claim: claimed=%v err=%v", claimed, err)
		}
		applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Session: &store.SessionUpsert{ID: &sessionID, BumpResumes: true},
		})
		if err != nil || !applied {
			t.Fatalf("bumpResumesToCap: CommitHandlerResult: applied=%v err=%v", applied, err)
		}
	}
}

// TestPlanningHandler_SessionExhausted_EscalatesResumesExhaustedExactlyOnce
// proves entry step 3 (design D17): once a session's resumes reaches
// machine.toml's max_resumes, the handler escalates resumes_exhausted with
// SessionID set and RunID nil, and a second tick, finding that escalation
// already recorded (HasEscalation), returns ErrNoAction rather than
// escalating again.
func TestPlanningHandler_SessionExhausted_EscalatesResumesExhaustedExactlyOnce(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify
	firstTurn := mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)                            // posts Q1
	apply(t, s, getTicket(t, s, ticketID), firstTurn)

	sess, _, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	owner := "resumes-exhausted-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpResumesToCap(t, s, ticketID, sess.ID, 12, owner, expires)

	_, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil || state != store.SessionExhausted {
		t.Fatalf("LatestSession after bump = (state=%v, err=%v), want SessionExhausted", state, err)
	}

	commit := mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want resumes_exhausted")
	}
	if commit.Escalation.RunID != nil {
		t.Errorf("commit.Escalation.RunID = %v, want nil (no run caused this, the cap did)", commit.Escalation.RunID)
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(mustEscalationPayload(t, commit), &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
	}
	if payload.Code != "resumes_exhausted" || payload.Origin != "cap_resumes" {
		t.Errorf("payload = (Code=%q, Origin=%q), want (resumes_exhausted, cap_resumes)", payload.Code, payload.Origin)
	}
	if payload.SessionID == nil || *payload.SessionID != sess.ID {
		t.Errorf("payload.SessionID = %v, want %d", payload.SessionID, sess.ID)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)

	_, err = runPlanning(t, s, claim(t, s, fake, ticketID), ticketID)
	if !errors.Is(err, job.ErrNoAction) {
		t.Fatalf("second tick after the cap escalation: err = %v, want job.ErrNoAction", err)
	}
}
