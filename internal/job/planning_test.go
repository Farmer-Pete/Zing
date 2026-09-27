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
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: rt, testRuntimeCodex: rt, testRuntimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return job.Deps{
		Store: s, Runtimes: set, Machine: testMachine(t), Models: testModels, Budget: testBudget, Floor: testFloor,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, tid int64, su store.SessionUpsert, model string) (store.Reserved, error) {
			return s.Reserve(ctx, tid, owner, expires, su, model)
		},
	}
}

// claimWithFloor is claim, with Deps.Floor overridden to floor: the
// review-tick tests that must vary the configured floor across all four
// severities (design section 6.5, task 7b).
func claimWithFloor(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, floor response.Severity) job.Deps {
	t.Helper()
	deps := claim(t, s, rt, ticketID)
	deps.Floor = floor
	return deps
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
	if commit.SetKind == nil || (*commit.SetKind != "bug" && *commit.SetKind != testKindFeature) {
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

// TestPlanningHandler_Resume_ReadyOutcomeStoresCohortAndStaysInPlanning
// proves task 7b removed task 6's TEMPORARY shortcut: a ready outcome on
// resume stores the cohort and carries no transition at all, leaving the
// ticket in planning, not waiting, for the review tick (entry step 6) to
// pick up on the next tick, rather than jumping straight to building.
func TestPlanningHandler_Resume_ReadyOutcomeStoresCohortAndStaysInPlanning(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1

	answerFixtureQuestion(t, s, ticketID)

	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (task 7b removed the shortcut)", commit.Next)
	}
	if len(commit.Artifacts) == 0 {
		t.Fatal("commit.Artifacts is empty, want the stored cohort")
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning || final.WaitingOn != nil {
		t.Errorf("final ticket = (state=%q, waiting_on=%v), want (planning, nil): the review tick picks up the cohort next tick", final.State, final.WaitingOn)
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
	if secondCommit.SetKind == nil || *secondCommit.SetKind != testKindFeature {
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
	if len(firstCommit.Messages) != 1 || firstCommit.Messages[0].Type != testMsgTypeUpdate {
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

// ---- 6.5 ready: the cohort check and store (task 7a) -----------------------

// validPlan returns a plan that passes every response.CheckPlan rule (no
// placeholder or vague words, no performance claim without a measurement,
// no scenario-leak) for a feature ticket (task 7a never drives this through
// the bug-shape rules): every prose field distinct from
// validScenarios' own Given/When/Then text, so nothing here accidentally
// restates a scenario's acceptance.
func validPlan(objective string) response.Plan {
	return response.Plan{
		Overview: response.Overview{
			Objective: objective,
			Context:   "internal/job/planning.go stores the ready cohort once its checks pass.",
			Problem:   response.Problem{Text: "Without a stored cohort the build step has nothing to act on."},
			Goals:     []string{"the ready cohort round-trips through the store"},
			NonGoals:  []string{"the review tick, which a later task adds"},
		},
		Design: response.Design{
			Demo:  response.Demo{Cmd: "go test ./internal/job/...", Text: "the new test passes"},
			Shape: "readyCommit checks the claims, the scenario shape, and the plan, then stores three artifact types under one run id.",
		},
		Delivery: response.Delivery{
			Files: []response.FileChange{{Path: "internal/job/planning.go", Action: response.FileActionModify, Reason: "store the ready cohort"}},
			Tests: []response.TestCase{{Name: "TestReadyCohort", Seam: "readyCommit", Kind: response.TestKindIntegration, Asserts: "the cohort round-trips"}},
			Tasks: []response.Task{{N: 1, Test: "TestReadyCohort", Demo: true, Text: "Store the plan, claims, and scenarios under the reserved run."}},
		},
		Review: response.Review{
			TrustRoot:    "none",
			Alternatives: []string{"store the plan without a scenario cohort: rejected, the build step needs both"},
			Risks:        []string{"a later revision must not collide with an earlier plan version"},
		},
	}
}

// validClaims returns one true code claim citing readyClaimEvidencePath
// (skeleton_test.go's testProjectDir writes this file into every seeded
// test project), so response.CheckCodeClaims resolves it for real.
func validClaims() []response.Claim {
	return []response.Claim{
		{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictTrue, Evidence: readyClaimEvidencePath + ":1", Text: "the file exists"},
	}
}

// validScenarios returns n scenarios tagged with tag, each with distinct,
// unrelated Given/When/Then text so no plan built by validPlan ever
// restates one verbatim (response.CheckPlan's scenario-leak rule).
func validScenarios(n int, tag string) []response.Scenario {
	out := make([]response.Scenario, n)
	for i := range out {
		out[i] = response.Scenario{
			ID:    fmt.Sprintf("s%d", i+1),
			Kind:  response.ScenarioKindBehavior,
			Given: fmt.Sprintf("%s scenario %d starts from a fresh store", tag, i+1),
			When:  fmt.Sprintf("%s scenario %d runs the ready check", tag, i+1),
			Then:  fmt.Sprintf("%s scenario %d observes the stored artifact", tag, i+1),
		}
	}
	return out
}

// readyResponse builds a *response.ReadyResponse naming job planning and
// outcome ready, the shape a scriptedRuntime step hands back in place of a
// real agent's XML document.
func readyResponse(plan response.Plan, claims []response.Claim, scenarios []response.Scenario) *response.ReadyResponse {
	return &response.ReadyResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeReady,
		Claims:    claims,
		Scenarios: scenarios,
		Plan:      plan,
	}
}

// readyScriptedRuntime builds a byJobRuntime dispatching only JobPlanning to
// a scriptedRuntime carrying steps, the shape every ready-cohort test below
// drives planning turns through once classify (a real fixture run) has
// already set the ticket's kind.
func readyScriptedRuntime(t *testing.T, steps ...scriptedStep) byJobRuntime {
	t.Helper()
	return byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{
		response.JobPlanning: &scriptedRuntime{t: t, steps: steps},
	}}
}

// readyStep wraps resp as a scriptedStep whose Response is a valid or
// hand-broken ready document under sessionID.
func readyStep(resp response.Response, sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{Response: resp, SessionID: sessionID, ExitCode: 0, AgentTime: time.Second}}
}

// TestPlanningHandler_Ready_StoresPlanClaimsAndScenariosThenStaysInPlanning
// proves section 6.5's success path against the real fixture cohort
// (fixtures/scripts/planning/2.xml, two scenarios): exactly one plan
// artifact (version 1), one claims artifact (version 1), and one scenario
// artifact per Scenario, every one carrying the reserved run's id; the
// stored plan payload round-trips to the fixture's own plan; and the
// ticket stays in planning, not waiting (task 7b removed the shortcut
// pinned by
// TestPlanningHandler_Resume_ReadyOutcomeStoresCohortAndStaysInPlanning),
// so the review tick can pick up the new cohort on the next tick.
func TestPlanningHandler_Ready_StoresPlanClaimsAndScenariosThenStaysInPlanning(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1

	answerFixtureQuestion(t, s, ticketID)

	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume (ready) Run: %v", err)
	}
	if len(commit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one terminalized run", commit.Runs)
	}
	runID := commit.Runs[0].ID
	const wantArtifacts = 4 // plan + claims + 2 scenarios
	if len(commit.Artifacts) != wantArtifacts {
		t.Fatalf("commit.Artifacts = %d entries, want %d (plan, claims, 2 scenarios)", len(commit.Artifacts), wantArtifacts)
	}

	var plans, claims, scenarios int
	var planPayload json.RawMessage
	for _, a := range commit.Artifacts {
		if a.RunID == nil || *a.RunID != runID {
			t.Errorf("artifact %s carries run_id %v, want %d", a.Type, a.RunID, runID)
		}
		switch a.Type {
		case testArtifactTypePlan:
			plans++
			planPayload = a.Payload
		case testArtifactTypeClaims:
			claims++
		case testArtifactTypeScenario:
			scenarios++
		default:
			t.Errorf("unexpected artifact type %q", a.Type)
		}
	}
	if plans != 1 || claims != 1 || scenarios != 2 {
		t.Errorf("artifact counts = (plan=%d, claims=%d, scenario=%d), want (1, 1, 2)", plans, claims, scenarios)
	}

	var roundTripped response.Plan
	if unmarshalErr := json.Unmarshal(planPayload, &roundTripped); unmarshalErr != nil {
		t.Fatalf("unmarshal stored plan payload: %v", unmarshalErr)
	}
	const wantObjective = "Add a hello endpoint so a caller can get a plain-text greeting back over HTTP."
	if roundTripped.Overview.Objective != wantObjective {
		t.Errorf("stored plan objective = %q, want %q", roundTripped.Overview.Objective, wantObjective)
	}
	if len(roundTripped.Delivery.Tasks) != 1 || roundTripped.Review.TrustRoot != "none" {
		t.Errorf("stored plan = %+v, does not round-trip the fixture's own plan", roundTripped)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	stored, err := s.ListArtifacts(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(stored) != wantArtifacts {
		t.Fatalf("stored artifacts = %d, want %d", len(stored), wantArtifacts)
	}
	var scenarioVersions []int
	for _, a := range stored {
		switch a.Type {
		case testArtifactTypePlan, testArtifactTypeClaims:
			if a.Version != 1 {
				t.Errorf("artifact %s version = %d, want 1 (the only one on this ticket)", a.Type, a.Version)
			}
		case testArtifactTypeScenario:
			scenarioVersions = append(scenarioVersions, a.Version)
		}
		if a.RunID == nil || *a.RunID != runID {
			t.Errorf("stored artifact %s run_id = %v, want %d", a.Type, a.RunID, runID)
		}
	}
	slices.Sort(scenarioVersions)
	if !slices.Equal(scenarioVersions, []int{1, 2}) {
		t.Errorf("scenario artifact versions = %v, want [1 2] (one row per scenario, consecutively versioned)", scenarioVersions)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning || final.WaitingOn != nil {
		t.Errorf("final ticket = (state=%q, waiting_on=%v), want (planning, nil): task 7b removed the shortcut", final.State, final.WaitingOn)
	}
}

// TestPlanningHandler_Ready_SecondReadyStoresNewCohortLeavingOldRowsUntouched
// proves a revised plan on the same ticket: a second ready call stores plan
// version 2 and a new scenario cohort under a new run id, while the first
// cohort's rows (a different run id, plan version 1) sit untouched. The
// first ready's commit carries no transition (task 7b removed the
// shortcut), so it is applied unchanged and the ticket stays in planning
// between the two ready turns, exactly as a real review tick would leave
// it. The second ready arrives through the ordinary answered-round resume
// path (design section 5.1 step 1(c), 6.4): a question tied to the first
// run's session, answered exactly as AnsweredRounds expects, whether or not
// a real model turn asked it.
func TestPlanningHandler_Ready_SecondReadyStoresNewCohortLeavingOldRowsUntouched(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify: feature

	planA := readyResponse(validPlan("Store the ready cohort, version A."), validClaims(), validScenarios(2, "A"))
	planB := readyResponse(validPlan("Store the ready cohort, version B, a revision of A."), validClaims(), validScenarios(3, "B"))
	byJob := readyScriptedRuntime(t, readyStep(planA, "plan-sess-1"), readyStep(planB, "plan-sess-1"))

	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
	if len(firstCommit.Artifacts) != 4 {
		t.Fatalf("first commit.Artifacts = %d, want 4 (plan, claims, 2 scenarios)", len(firstCommit.Artifacts))
	}
	firstRunID := firstCommit.Runs[0].ID
	if firstCommit.Next != "" {
		t.Fatalf("firstCommit.Next = %q, want empty (task 7b removed the shortcut)", firstCommit.Next)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: "a", Options: []response.Option{{Key: "a", Text: "Revise the plan"}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	openState := "open"
	qID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, RunID: &firstRunID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State: &openState, Body: "Revise the plan?\n\nShould the plan be revised?", Payload: payload,
	})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	if _, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"}); answerErr != nil {
		t.Fatalf("AnswerQuestion: %v", answerErr)
	}

	secondCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second planning Run: %v", err)
	}
	if len(secondCommit.Artifacts) != 5 {
		t.Fatalf("second commit.Artifacts = %d, want 5 (plan, claims, 3 scenarios)", len(secondCommit.Artifacts))
	}
	secondRunID := secondCommit.Runs[0].ID
	if secondRunID == firstRunID {
		t.Fatalf("second ready reused run id %d, want a new run", secondRunID)
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	stored, err := s.ListArtifacts(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	var planVersions, claimsVersions []int
	var oldScenarios, newScenarios int
	for _, a := range stored {
		switch a.Type {
		case testArtifactTypePlan:
			planVersions = append(planVersions, a.Version)
		case testArtifactTypeClaims:
			claimsVersions = append(claimsVersions, a.Version)
		case testArtifactTypeScenario:
			switch {
			case a.RunID != nil && *a.RunID == firstRunID:
				oldScenarios++
			case a.RunID != nil && *a.RunID == secondRunID:
				newScenarios++
			}
		}
	}
	if !slices.Contains(planVersions, 1) || !slices.Contains(planVersions, 2) {
		t.Errorf("plan versions = %v, want both 1 and 2", planVersions)
	}
	if !slices.Contains(claimsVersions, 1) || !slices.Contains(claimsVersions, 2) {
		t.Errorf("claims versions = %v, want both 1 and 2", claimsVersions)
	}
	if oldScenarios != 2 {
		t.Errorf("old cohort (run %d) scenario rows = %d, want 2 (untouched)", firstRunID, oldScenarios)
	}
	if newScenarios != 3 {
		t.Errorf("new cohort (run %d) scenario rows = %d, want 3", secondRunID, newScenarios)
	}

	cohort, ok, err := s.CurrentCohort(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("CurrentCohort: %+v, %v", cohort, err)
	}
	if cohort.PlanVersion != 2 || cohort.RunID == nil || *cohort.RunID != secondRunID {
		t.Errorf("CurrentCohort = %+v, want (version 2, run %d)", cohort, secondRunID)
	}
}

// TestPlanningHandler_Ready_ClaimThroughOutwardSymlinkFailsAndPends proves
// D19: a code claim citing a path that resolves through a symlink pointing
// outside the checkout fails the claim check (os.Root cannot follow it),
// the pending marker names the claim's own element path, nothing is
// stored, and the ticket stays in planning, not waiting.
func TestPlanningHandler_Ready_ClaimThroughOutwardSymlinkFailsAndPends(t *testing.T) {
	s := newJobTestStore(t)

	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.go"), []byte("package secret\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	projectDir := t.TempDir()
	if err := os.Symlink(filepath.Join(outsideDir, "secret.go"), filepath.Join(projectDir, "escape.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	projectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: projectDir, Tracker: "github",
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	claims := []response.Claim{{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictTrue, Evidence: "escape.go:1", Text: "reads the outside file"}}
	resp := readyResponse(validPlan("Store the ready cohort."), claims, validScenarios(2, "sym"))
	byJob := readyScriptedRuntime(t, readyStep(resp, "sym-sess-1"))

	commit := mustPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
	if len(commit.Artifacts) != 0 {
		t.Fatalf("commit.Artifacts = %d, want 0 (the claim check must fail)", len(commit.Artifacts))
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one pending marker", commit.Messages)
	}
	body := commit.Messages[0].Body
	if !strings.HasPrefix(body, "validation errors pending run ") {
		t.Errorf("marker body = %q, want it to start with the pending prefix", body)
	}
	if !strings.Contains(body, "claims/claim[0]/evidence") {
		t.Errorf("marker body = %q, want the claim's own element path", body)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (the ticket stays in planning)", commit.Next)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning || final.WaitingOn != nil {
		t.Errorf("final ticket = (state=%q, waiting_on=%v), want (planning, nil)", final.State, final.WaitingOn)
	}
}

// TestPlanningHandler_Ready_ScenarioShapeFailuresNameTheElementPath covers
// section 6.5's scenario-cohort re-check: 1 scenario, 31 scenarios, and one
// empty then, each failing with the pending marker naming the specific
// element path and rule.
func TestPlanningHandler_Ready_ScenarioShapeFailuresNameTheElementPath(t *testing.T) {
	emptyThen := validScenarios(2, "empty")
	emptyThen[0].Then = ""

	cases := []struct {
		name      string
		scenarios []response.Scenario
		wantErr   string
	}{
		{name: "one scenario", scenarios: validScenarios(1, "one"), wantErr: "need 2 to 30 scenarios, have 1"},
		{name: "thirty-one scenarios", scenarios: validScenarios(31, "many"), wantErr: "need 2 to 30 scenarios, have 31"},
		{name: "empty then", scenarios: emptyThen, wantErr: "scenarios/scenario[0]/then: then must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newJobTestStore(t)
			ticketID := seedQueuedTicket(t, s)
			fake := fakeRuntime(t)
			advanceQueuedToPlanning(t, s, fake, ticketID)
			apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID))

			resp := readyResponse(validPlan("Store the ready cohort."), validClaims(), tc.scenarios)
			byJob := readyScriptedRuntime(t, readyStep(resp, "shape-sess"))

			commit := mustPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
			if len(commit.Artifacts) != 0 {
				t.Fatalf("commit.Artifacts = %d, want 0", len(commit.Artifacts))
			}
			if len(commit.Messages) != 1 || !strings.Contains(commit.Messages[0].Body, tc.wantErr) {
				t.Fatalf("commit.Messages = %+v, want the pending marker to carry %q", commit.Messages, tc.wantErr)
			}
		})
	}
}

// TestPlanningHandler_Ready_PlanCheckerFailureNamesElementPathAndRule proves
// a plan checker failure (a TODO placeholder) writes the pending marker
// with the checker's own element path and rule.
func TestPlanningHandler_Ready_PlanCheckerFailureNamesElementPathAndRule(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID))

	plan := validPlan("Store the ready cohort.")
	plan.Design.Shape = "TODO: describe the shape."
	resp := readyResponse(plan, validClaims(), validScenarios(2, "todo"))
	byJob := readyScriptedRuntime(t, readyStep(resp, "todo-sess"))

	commit := mustPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
	if len(commit.Artifacts) != 0 {
		t.Fatalf("commit.Artifacts = %d, want 0", len(commit.Artifacts))
	}
	const wantErr = `plan/design/shape: placeholder "TODO" not allowed`
	if len(commit.Messages) != 1 || !strings.Contains(commit.Messages[0].Body, wantErr) {
		t.Fatalf("commit.Messages = %+v, want the pending marker to carry %q", commit.Messages, wantErr)
	}
}

// TestPlanningHandler_Step5_LiveValidationMarkerResumesWithFencedErrorsThenDelivers
// proves entry-decision step 5: a live "validation errors pending" marker
// resumes the open session with those errors fenced behind
// prompt.Validation (captured through a recordingRuntime), writes the
// "validation errors delivered" marker in that same commit, and a later
// tick -- with the marker now delivered -- does not resume again.
func TestPlanningHandler_Step5_LiveValidationMarkerResumesWithFencedErrorsThenDelivers(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	badResp := readyResponse(validPlan("Store the ready cohort."), validClaims(), validScenarios(1, "bad"))
	goodResp := &response.QuestionResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q1", Title: "Continue?", Body: "Body.",
			Options: []response.Option{{Key: "a", Text: "Yes"}, {Key: "b", Text: "No"}}, Recommended: "a",
		}},
	}
	byJob := readyScriptedRuntime(t, readyStep(badResp, "step5-sess"), readyStep(goodResp, "step5-sess"))

	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
	if len(firstCommit.Messages) != 1 || !strings.HasPrefix(firstCommit.Messages[0].Body, "validation errors pending run ") {
		t.Fatalf("first commit.Messages = %+v, want one pending marker", firstCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	rec := &recordingRuntime{rt: byJob}
	secondCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second planning Run: %v", err)
	}
	if rec.lastReq.Prompt == "" {
		t.Fatal("recordingRuntime saw an empty resume prompt")
	}
	if !strings.Contains(rec.lastReq.Prompt, "<<<UNTRUSTED ") || !strings.Contains(rec.lastReq.Prompt, "<<<END ") {
		t.Errorf("resume prompt does not carry the fence markers:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "need 2 to 30 scenarios") {
		t.Errorf("resume prompt does not carry the validation errors:\n%s", rec.lastReq.Prompt)
	}

	var delivered int
	for _, m := range secondCommit.Messages {
		if strings.HasPrefix(m.Body, "validation errors delivered run ") {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("second commit.Messages = %+v, want exactly one delivered marker", secondCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	_, err = runPlanning(t, s, claimWithRuntimes(t, s, byJob, ticketID), ticketID)
	if !errors.Is(err, job.ErrNoAction) {
		t.Fatalf("third tick: err = %v, want job.ErrNoAction (no second resume)", err)
	}
}

// ---- 6.5 plan review tick and the floor loop (task 7b) ---------------------

// seedFeatureTicketInPlanning inserts a ticket with kind already set to
// "feature" and advances it into planning, bypassing a real classify turn:
// the review-tick tests below need a kinded ticket sitting in planning with
// a stored cohort (seedCohort), not another proof that classify sets kind
// (already TestPlanningHandler_Classify_StoresKindAndSessionExternalID's
// job).
func seedFeatureTicketInPlanning(t *testing.T, s *store.Store) int64 {
	t.Helper()
	proj := testProject
	proj.LocalPath = testProjectDir(t)
	projectID, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	kind := testKindFeature
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle,
		Kind: &kind, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)
	return ticketID
}

// seedCohort stores a plan version-1 cohort (one plan artifact, one
// scenario artifact per entry in scenarios) directly through the store,
// with the producing planning session already open (external_id set) so
// the entry decision's SessionOpen branch (steps 4-7) is reachable without
// driving a real ready turn: the review-tick tests below need a stored
// cohort, not another proof that readyCommit stores one (already
// TestPlanningHandler_Ready_StoresPlanClaimsAndScenariosThenStaysInPlanning's
// job).
func seedCohort(t *testing.T, s *store.Store, ticketID int64, plan response.Plan, scenarios []response.Scenario) (planVersion int, runID int64) {
	t.Helper()
	owner := "seed-cohort-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedCohort: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeClaude}, "claude-x")
	if err != nil {
		t.Fatalf("seedCohort: reserve: %v", err)
	}

	normalizePlanArraysForTest(&plan)
	planPayload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("seedCohort: marshal plan: %v", err)
	}
	artifacts := make([]store.Artifact, 0, 1+len(scenarios))
	artifacts = append(artifacts, store.Artifact{Type: testArtifactTypePlan, RunID: &rsv.RunID, Payload: planPayload})
	for _, sc := range scenarios {
		scPayload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			t.Fatalf("seedCohort: marshal scenario: %v", marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{Type: testArtifactTypeScenario, RunID: &rsv.RunID, Payload: scPayload})
	}

	extID := fmt.Sprintf("seed-sess-%d", rsv.SessionID)
	outcome, exitCode, agentSeconds := "ready", 0, 1
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session:   &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID},
		Runs:      []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Artifacts: artifacts,
	})
	if err != nil || !applied {
		t.Fatalf("seedCohort: CommitHandlerResult: applied=%v err=%v", applied, err)
	}

	cohort, ok, err := s.CurrentCohort(t.Context(), ticketID)
	if err != nil || !ok || cohort.RunID == nil {
		t.Fatalf("seedCohort: CurrentCohort: %+v, ok=%v, %v", cohort, ok, err)
	}
	return cohort.PlanVersion, *cohort.RunID
}

// normalizePlanArraysForTest is planning.go's own unexported
// normalizePlanArrays, mirrored here (job_test is an external test package,
// so it cannot call the unexported one job.readyCommit already applies):
// the artifacts/plan.json schema requires "changes", "types", "migrations",
// and "deletions" as arrays, never null, so seedCohort's direct marshal
// needs the same nil-to-empty-slice normalization a real ready turn's own
// readyArtifacts already gets.
func normalizePlanArraysForTest(p *response.Plan) {
	if p.Design.Changes == nil {
		p.Design.Changes = []response.Change{}
	}
	if p.Design.Types == nil {
		p.Design.Types = []response.TypeDef{}
	}
	for i := range p.Design.Types {
		if p.Design.Types[i].Transitions == nil {
			p.Design.Types[i].Transitions = []response.Transition{}
		}
	}
	if p.Design.Migrations.Items == nil {
		p.Design.Migrations.Items = []response.Migration{}
	}
	if p.Delivery.Deletions.Items == nil {
		p.Delivery.Deletions.Items = []response.Fence{}
	}
}

// finding builds one response.Finding, lens fixed at LensCorrectness (no
// test below varies it) and severity, location, text, and fix all named
// explicitly, the shape a planreview "ok" outcome carries.
func finding(sev response.Severity, location, text, fix string) response.Finding {
	return response.Finding{Lens: response.LensCorrectness, Severity: sev, Location: location, Text: text, Fix: fix}
}

// findingsResponse builds a *response.FindingsResponse naming job planreview
// and outcome ok, the shape a scriptedRuntime step hands back in place of a
// real Codex turn.
func findingsResponse(findings ...response.Finding) *response.FindingsResponse {
	return &response.FindingsResponse{Job: response.JobPlanreview, Outcome: response.OutcomeOk, Findings: findings}
}

// insertUpdateMarker inserts an "update" message with body directly, for
// tests that seed a marker's state without driving the turn that would
// ordinarily write it (design section 5.3).
func insertUpdateMarker(t *testing.T, s *store.Store, ticketID int64, body string) {
	t.Helper()
	if _, err := s.InsertMessage(t.Context(), store.Message{TicketID: ticketID, Type: testMsgTypeUpdate, Author: "system", Body: body}); err != nil {
		t.Fatalf("insertUpdateMarker(%q): %v", body, err)
	}
}

// seedPlanreviewArtifact inserts a "planreview" artifact at exactly version,
// under runID, carrying findings, directly through the store: the shape a
// real review tick's own commit stores (design section 6.5).
func seedPlanreviewArtifact(t *testing.T, s *store.Store, ticketID int64, version int, runID int64, findings ...response.Finding) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Findings []response.Finding `json:"findings"`
	}{Findings: findings})
	if err != nil {
		t.Fatalf("seedPlanreviewArtifact: marshal payload: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: &runID, Type: testArtifactTypePlanreview, Version: version, Payload: payload,
	}); err != nil {
		t.Fatalf("seedPlanreviewArtifact: InsertArtifact: %v", err)
	}
}

// TestPlanningHandler_ReviewTick_StoresFindingsAtCohortVersionAndFencesInputs
// proves entry step 6 and section 6.5's prompt assembly: a stored cohort
// with no planreview artifact yet starts the review tick, the resulting
// "planreview" artifact lands at the exact cohort version carrying the
// reserved run's id, the session id is recorded, and the assembled prompt
// fences the ticket, the scenarios, and the plan (all three, captured
// through a recordingRuntime) -- with only the lens files' own "## In a
// plan" sections appended, never an "## In code" line.
func TestPlanningHandler_ReviewTick_StoresFindingsAtCohortVersionAndFencesInputs(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Review the plan on its own tick."), validScenarios(2, "review"))

	f := finding(response.SeverityMinor, "plan/design/shape", "the shape does not say who owns the demo", "name the owner")
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(f), "review-sess-1")}}
	rec := &recordingRuntime{rt: rt}

	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %d entries, want 1", len(commit.Artifacts))
	}
	a := commit.Artifacts[0]
	if a.Type != testArtifactTypePlanreview || a.Version != planVersion {
		t.Errorf("commit.Artifacts[0] = (type=%q, version=%d), want (planreview, %d)", a.Type, a.Version, planVersion)
	}
	if len(commit.Runs) != 1 || a.RunID == nil || *a.RunID != commit.Runs[0].ID {
		t.Errorf("artifact run_id = %v, want the terminalized run's own id %+v", a.RunID, commit.Runs)
	}
	if commit.Session == nil || commit.Session.ExternalID == nil || *commit.Session.ExternalID != "review-sess-1" {
		t.Fatalf("commit.Session = %+v, want a fresh session with external id review-sess-1", commit.Session)
	}

	if rec.lastReq.Prompt == "" {
		t.Fatal("recordingRuntime saw an empty review prompt")
	}
	prompt := rec.lastReq.Prompt
	if !strings.Contains(prompt, "<<<UNTRUSTED ") || !strings.Contains(prompt, "<<<END ") {
		t.Errorf("review prompt does not carry the fence markers:\n%s", prompt)
	}
	if !strings.Contains(prompt, testTicketTitle) {
		t.Errorf("review prompt does not carry the ticket:\n%s", prompt)
	}
	if !strings.Contains(prompt, "review scenario 1 starts from a fresh store") {
		t.Errorf("review prompt does not carry the cohort's scenarios:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<overview>") || !strings.Contains(prompt, "Review the plan on its own tick.") {
		t.Errorf("review prompt does not carry the plan rendered back to XML:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## In a plan") {
		t.Errorf("review prompt is missing a lens's \"## In a plan\" section:\n%s", prompt)
	}
	if strings.Contains(prompt, "## In code") {
		t.Errorf("review prompt carries an \"## In code\" section, want only \"## In a plan\":\n%s", prompt)
	}

	_ = runID
}

// TestPlanningHandler_ReviewTick_DropsUnresolvedLocationFindings proves
// section 6.5's drop rule: a finding whose Location does not resolve as an
// element path in the stored plan (response.ResolvesInPlan) is dropped from
// the stored artifact, while a finding at a real path survives.
func TestPlanningHandler_ReviewTick_DropsUnresolvedLocationFindings(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, _ := seedCohort(t, s, ticketID, validPlan("Drop unresolved findings."), validScenarios(2, "drop"))

	resolvable := finding(response.SeverityMajor, "plan/design/shape", "a real problem", "fix it")
	unresolved := finding(response.SeverityMajor, "plan/design/does-not-exist", "a hallucinated location", "fix it")
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(resolvable, unresolved), "drop-sess")}}

	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %d entries, want 1", len(commit.Artifacts))
	}
	if commit.Artifacts[0].Version != planVersion {
		t.Errorf("commit.Artifacts[0].Version = %d, want %d", commit.Artifacts[0].Version, planVersion)
	}

	var payload struct {
		Findings []response.Finding `json:"findings"`
	}
	if err := json.Unmarshal(commit.Artifacts[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal stored planreview payload: %v", err)
	}
	if len(payload.Findings) != 1 || payload.Findings[0].Location != "plan/design/shape" {
		t.Errorf("stored findings = %+v, want exactly the one resolvable finding", payload.Findings)
	}
}

// TestPlanningHandler_ReviewTick_FloorSplitsFindingsAcrossAllFourFloors
// proves section 6.5's floor split for every review.floor value: the same
// four findings (one per severity) split differently depending on the
// configured floor. Since the at-or-below survivors are what entry step 7
// fences into the next resume (prompt.Findings), this drives one more tick
// after the review lands and inspects that resumed prompt.
func TestPlanningHandler_ReviewTick_FloorSplitsFindingsAcrossAllFourFloors(t *testing.T) {
	const blockerText, majorText, minorText, nitText = "blocker text", "major text", "minor text", "nit text"
	findings := []response.Finding{
		finding(response.SeverityBlocker, "plan/design/shape", blockerText, "fix"),
		finding(response.SeverityMajor, "plan/design/shape", majorText, "fix"),
		finding(response.SeverityMinor, "plan/design/shape", minorText, "fix"),
		finding(response.SeverityNit, "plan/design/shape", nitText, "fix"),
	}

	cases := []struct {
		floor     response.Severity
		wantIn    []string
		wantNotIn []string
	}{
		{response.SeverityBlocker, []string{blockerText, majorText, minorText, nitText}, nil},
		{response.SeverityMajor, []string{majorText, minorText, nitText}, []string{blockerText}},
		{response.SeverityMinor, []string{minorText, nitText}, []string{blockerText, majorText}},
		{response.SeverityNit, []string{nitText}, []string{blockerText, majorText, minorText}},
	}

	for _, tc := range cases {
		t.Run(string(tc.floor), func(t *testing.T) {
			s := newJobTestStore(t)
			ticketID := seedFeatureTicketInPlanning(t, s)
			seedCohort(t, s, ticketID, validPlan("Floor split."), validScenarios(2, "floor"))

			reviewRT := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(findings...), "floor-sess")}}
			firstCommit, err := runPlanning(t, s, claimWithFloor(t, s, reviewRT, ticketID, tc.floor), ticketID)
			if err != nil {
				t.Fatalf("review tick Run: %v", err)
			}
			apply(t, s, getTicket(t, s, ticketID), firstCommit)

			resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "floor-resume-sess")}}
			rec := &recordingRuntime{rt: resumeRT}
			if _, err := runPlanning(t, s, claimWithFloor(t, s, rec, ticketID, tc.floor), ticketID); err != nil {
				t.Fatalf("floor resume Run: %v", err)
			}

			for _, want := range tc.wantIn {
				if !strings.Contains(rec.lastReq.Prompt, want) {
					t.Errorf("floor %s: resume prompt missing %q:\n%s", tc.floor, want, rec.lastReq.Prompt)
				}
			}
			for _, notWant := range tc.wantNotIn {
				if strings.Contains(rec.lastReq.Prompt, notWant) {
					t.Errorf("floor %s: resume prompt unexpectedly contains %q:\n%s", tc.floor, notWant, rec.lastReq.Prompt)
				}
			}
		})
	}
}

// TestPlanningHandler_ReviewTick_CleanFloorIsATemporaryShortcutToBuilding
// pins task 7b's TEMPORARY shortcut by name: a clean review (no
// at-or-below-floor findings) goes straight to building, with a Reason
// naming itself a shortcut, rather than section 6.6's real gate (task 7c).
func TestPlanningHandler_ReviewTick_CleanFloorIsATemporaryShortcutToBuilding(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("Clean review shortcut."), validScenarios(2, "clean"))

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(), "clean-sess")}}
	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	if commit.Next != testStateBuilding {
		t.Errorf("commit.Next = %q, want building", commit.Next)
	}
	if !strings.Contains(commit.Reason, "shortcut") {
		t.Errorf("commit.Reason = %q, want it to name itself a temporary shortcut", commit.Reason)
	}
}

// TestPlanningHandler_ReviewTick_FloorFindingsPendThenResumeThenDeliverThenStop
// proves entry steps 6 and 7 across ticks (design section 5.1, 5.3): floor
// findings write "planreview vN pending" and leave the ticket in planning,
// not waiting; the next tick resumes planning with the findings fenced and
// writes "planreview vN delivered" in that same commit; a following tick
// does not resume again (no live pending marker left).
func TestPlanningHandler_ReviewTick_FloorFindingsPendThenResumeThenDeliverThenStop(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, _ := seedCohort(t, s, ticketID, validPlan("Floor loop."), validScenarios(2, "loop"))

	f := finding(response.SeverityMinor, "plan/design/shape", "needs a name", "name it")
	reviewRT := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(f), "loop-sess-1")}}

	firstCommit, err := runPlanning(t, s, claim(t, s, reviewRT, ticketID), ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	wantPending := fmt.Sprintf("planreview v%d pending", planVersion)
	if len(firstCommit.Messages) != 1 || firstCommit.Messages[0].Body != wantPending {
		t.Fatalf("firstCommit.Messages = %+v, want one %q marker", firstCommit.Messages, wantPending)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	after := getTicket(t, s, ticketID)
	if after.State != testStatePlanning || after.WaitingOn != nil {
		t.Fatalf("after the review tick: ticket = (state=%q, waiting_on=%v), want (planning, nil)", after.State, after.WaitingOn)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "loop-resume-sess")}}
	rec := &recordingRuntime{rt: resumeRT}
	secondCommit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("floor resume Run: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, "needs a name") {
		t.Errorf("resume prompt does not carry the floor finding:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "<<<UNTRUSTED ") {
		t.Errorf("resume prompt does not fence the findings:\n%s", rec.lastReq.Prompt)
	}
	wantDelivered := fmt.Sprintf("planreview v%d delivered", planVersion)
	var delivered int
	for _, m := range secondCommit.Messages {
		if m.Body == wantDelivered {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("secondCommit.Messages = %+v, want exactly one %q marker", secondCommit.Messages, wantDelivered)
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	_, err = runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
	if !errors.Is(err, job.ErrNoAction) {
		t.Fatalf("third tick: err = %v, want job.ErrNoAction (no second resume)", err)
	}
}

// TestPlanningHandler_ReviewTick_MaxLoopsEscalatesLoopsExhausted proves the
// section 5.1 step 7 cap: CountDeliveredReviews already at machine.toml's
// planreview max_loops (2), with a live pending marker and at-or-below
// findings, escalates loops_exhausted with a nil RunID (design section 6.7)
// rather than resuming a third time.
func TestPlanningHandler_ReviewTick_MaxLoopsEscalatesLoopsExhausted(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Max loops."), validScenarios(2, "maxloops"))

	// Two earlier loops' own delivered markers (different versions: this
	// cohort's own pending marker below is version-scoped, so these do not
	// interfere with its liveness), bringing CountDeliveredReviews to
	// machine.toml's own max_loops (2) before this tick even runs.
	insertUpdateMarker(t, s, ticketID, "planreview v1 delivered")
	insertUpdateMarker(t, s, ticketID, "planreview v2 delivered")

	f := finding(response.SeverityMinor, "plan/design/shape", "still wrong", "fix it")
	seedPlanreviewArtifact(t, s, ticketID, planVersion, runID, f)
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("planreview v%d pending", planVersion))

	commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want loops_exhausted")
	}
	if commit.Escalation.RunID != nil {
		t.Errorf("commit.Escalation.RunID = %v, want nil (no run caused this, the cap did)", commit.Escalation.RunID)
	}
	var payload response.EscalationPayload
	if err := json.Unmarshal(mustEscalationPayload(t, commit), &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != "loops_exhausted" || payload.Origin != "cap_loops" {
		t.Errorf("payload = (Code=%q, Origin=%q), want (loops_exhausted, cap_loops)", payload.Code, payload.Origin)
	}
}

// TestPlanningHandler_ReviewTick_QuestionAndD14RetryDoNotConsumeLoopAllowance
// proves that a planreview universal question, once answered, re-runs the
// review fresh with the answers and resolves the round (entry step 1(e)),
// and that neither it nor a D14 invalid retry ever writes a "delivered"
// marker, so CountDeliveredReviews stays at 0 throughout -- the loop
// allowance (design section 5.1 step 7) is untouched by either.
func TestPlanningHandler_ReviewTick_QuestionAndD14RetryDoNotConsumeLoopAllowance(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("Question then D14 then ok."), validScenarios(2, "consume"))

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanreview, "pr-sess-1")}}
	rec := &recordingRuntime{rt: rt}

	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingQuestions {
		t.Fatalf("commit.Waiting = %v, want questions", commit.Waiting)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)

	n, err := s.CountDeliveredReviews(t.Context(), ticketID)
	if err != nil || n != 0 {
		t.Fatalf("CountDeliveredReviews after a planreview question = %d, %v, want 0", n, err)
	}

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	if _, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"}); answerErr != nil {
		t.Fatalf("AnswerQuestion: %v", answerErr)
	}

	rt.steps = append(rt.steps, invalidResult("no zing element in final message", "pr-sess-2"))
	invalidCommit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planreview re-run Run: %v", err)
	}
	if len(invalidCommit.ResolveQuestions) != 1 || invalidCommit.ResolveQuestions[0] != open[0].ID {
		t.Errorf("invalidCommit.ResolveQuestions = %v, want [%d]", invalidCommit.ResolveQuestions, open[0].ID)
	}
	if !strings.Contains(rec.lastReq.Prompt, "Option A") {
		t.Errorf("planreview re-run prompt does not carry the answer's option text:\n%s", rec.lastReq.Prompt)
	}
	apply(t, s, getTicket(t, s, ticketID), invalidCommit)

	n, err = s.CountDeliveredReviews(t.Context(), ticketID)
	if err != nil || n != 0 {
		t.Fatalf("CountDeliveredReviews after a planreview D14 retry = %d, %v, want 0", n, err)
	}
}

// TestPlanningHandler_ReviewTick_D14_SecondConsecutiveInvalidEscalates
// proves D14's two-strike rule for the review tick: the first invalid
// output only writes the marker and leaves the ticket not waiting; the
// second consecutive invalid output (the review re-runs fresh, since every
// review tick opens a fresh session) escalates response_invalid in that
// same commit, with Origin planreview.
func TestPlanningHandler_ReviewTick_D14_SecondConsecutiveInvalidEscalates(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("D14 planreview."), validScenarios(2, "d14"))

	const reason = "no zing element in final message"
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		invalidResult(reason, "pr-d14-1"),
		invalidResult(reason, "pr-d14-2"),
	}}

	firstCommit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("first review tick Run: %v", err)
	}
	if firstCommit.Escalation != nil {
		t.Fatalf("first invalid commit.Escalation = %+v, want nil (first strike)", firstCommit.Escalation)
	}
	if len(firstCommit.Messages) != 1 || firstCommit.Messages[0].Type != testMsgTypeUpdate {
		t.Fatalf("first invalid commit.Messages = %+v, want one update marker", firstCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	after := getTicket(t, s, ticketID)
	if after.WaitingOn != nil {
		t.Fatalf("after first invalid: ticket.WaitingOn = %v, want nil", after.WaitingOn)
	}

	secondCommit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second review tick Run: %v", err)
	}
	if secondCommit.Escalation == nil {
		t.Fatal("second invalid commit.Escalation is nil, want response_invalid")
	}
	var payload response.EscalationPayload
	if err := json.Unmarshal(mustEscalationPayload(t, secondCommit), &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != "response_invalid" || payload.Origin != testArtifactTypePlanreview {
		t.Errorf("payload = (Code=%q, Origin=%q), want (response_invalid, planreview)", payload.Code, payload.Origin)
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)
}

// TestPlanningHandler_ReviewTick_D14_InvalidThenValidThenInvalidDoesNotEscalate
// proves the chain resets on a valid terminalized run (design D14): an
// invalid review output, then a valid (question) output, then, once that
// question is answered and the review re-runs fresh, a second invalid
// output -- which must NOT escalate, since the valid run in between reset
// the consecutive count to zero.
func TestPlanningHandler_ReviewTick_D14_InvalidThenValidThenInvalidDoesNotEscalate(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("D14 reset."), validScenarios(2, "d14reset"))

	const reason = "no zing element in final message"
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		invalidResult(reason, "pr-reset-1"),
		questionResult(response.JobPlanreview, "pr-reset-2"),
		invalidResult(reason, "pr-reset-3"),
	}}

	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // invalid, n=0

	questionCommit := mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID) // valid question, resets chain
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

	thirdCommit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID) // fresh review, then invalid
	if err != nil {
		t.Fatalf("third planning Run: %v", err)
	}
	if thirdCommit.Escalation != nil {
		t.Errorf("third invalid commit.Escalation = %+v, want nil (the valid run in between reset the chain)", thirdCommit.Escalation)
	}
	apply(t, s, getTicket(t, s, ticketID), thirdCommit)
}
