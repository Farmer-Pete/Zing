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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	zing "zing"
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
		Reserve: func(ctx context.Context, tid int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return s.Reserve(ctx, tid, owner, expires, su, seed)
		},
		DataDir: t.TempDir(),
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

// questionResult builds a scriptedStep whose Response is a minimal question
// document: enough for questionOutcomeCommit to post one message, the shape
// the handler itself never re-validates at runtime (design section 6.6,
// the same rule the skeleton always followed). Planning's own question
// outcome decodes to *response.PlanningQuestionsResponse, not the universal
// *response.QuestionResponse every other job uses (design section 22.2,
// D31), so forJob picks which concrete type this scripted step carries.
func questionResult(forJob response.Job, sessionID string) scriptedStep {
	questions := []response.Question{{
		Key: "q1", Title: "A question", Body: testQuestionBody,
		Options:     []response.Option{{Key: "a", Text: testOptionAText}, {Key: "b", Text: testOptionBText}},
		Recommended: "a",
	}}
	var resp response.Response
	if forJob == response.JobPlanning {
		resp = &response.PlanningQuestionsResponse{
			Job: forJob, Outcome: response.OutcomeQuestion,
			Questions: questions,
		}
	} else {
		resp = &response.QuestionResponse{
			Job: forJob, Outcome: response.OutcomeQuestion,
			Questions: questions,
		}
	}
	return scriptedStep{res: runtime.RunResult{
		Response:  resp,
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
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)

	deps := claim(t, s, rt, ticketID)
	commit, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("planning (classify) Run: %v", err)
	}
	if commit.SetKind == nil || (*commit.SetKind != testKindBug && *commit.SetKind != testKindFeature) {
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
	t.Parallel()
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
		if qp.Key != "" {
			t.Errorf("commit question payload.Key = %q, want empty (F030: CommitHandlerResult allocates it)", qp.Key)
		}
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if m.Type != testMsgTypeQuestion {
			continue
		}
		found = true
		if m.RunID == nil {
			t.Error("persisted question has no run_id, want the reserved run's id")
		}
		var qp response.QuestionPayload
		if unmarshalErr := json.Unmarshal(m.Payload, &qp); unmarshalErr != nil {
			t.Fatalf("unmarshal persisted question payload: %v", unmarshalErr)
		}
		if !strings.HasPrefix(qp.Key, "Q") {
			t.Errorf("persisted question payload.Key = %q, want an allocated Q<n> key", qp.Key)
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

// ---- 6.2 first turn: the prompt file follows the classified kind ----------

// TestPlanningHandler_FirstTurn_PromptFileFollowsKind proves section 6.2's
// own prompt selection (task 8, part 1): runPlanningFirst picks
// machine.Jobs["planning"].Prompt.Bug for a bug-kind ticket and .Feature for
// a feature-kind one, and prompt.Assemble puts JobPrompt first with nothing
// ahead of it (design section 4.2's assembly order), so the pinned prompt
// file's own text reaches the runtime as the assembled prompt's exact
// prefix.
func TestPlanningHandler_FirstTurn_PromptFileFollowsKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome response.Outcome
		asset   string
	}{
		{"bug", response.OutcomeBug, "prompts/planning-bug.md"},
		{"feature", response.OutcomeFeature, "prompts/planning-feature.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newJobTestStore(t)
			ticketID := seedQueuedTicket(t, s)
			advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

			rec := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "planning-sess")}}}
			rt := byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{
				response.JobClassify: &scriptedRuntime{t: t, steps: []scriptedStep{classifyResult(tc.outcome, "classify-sess")}},
				response.JobPlanning: rec,
			}}

			apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)) // classify
			if _, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID); err != nil {               // first turn
				t.Fatalf("planning first turn: %v", err)
			}

			want, err := fs.ReadFile(zing.Assets, tc.asset)
			if err != nil {
				t.Fatalf("read asset %s: %v", tc.asset, err)
			}
			if !strings.HasPrefix(rec.lastReq.Prompt, string(want)) {
				t.Fatalf("first-turn prompt for kind %s does not start with %s's own text:\n%s", tc.name, tc.asset, rec.lastReq.Prompt)
			}
		})
	}
}

// ---- resume: fenced answers, resumes bumped, round resolved ---------------

// TestPlanningHandler_Resume_AnsweredRoundBumpsResumesAndFencesTheAnswer
// proves section 6.4: an answered round resumes the open session (its
// resumes total goes up by one, charged by Reserve at runJob time -- design
// section 4.2, ResolveQuestions the round's question ids), and the resumed
// prompt fences the owner's answer text behind the untrusted-input markers
// (design D15, section 4.2).
// TestPlanningHandler_Resume_ConversationBumpsResumesOnceAndFences is D31's
// rewrite of the pre-D31
// TestPlanningHandler_Resume_AnsweredRoundBumpsResumesAndFencesTheAnswer
// (design section 22.4): Q1 is a planning question, so the owner's answer
// never becomes an "answered round" any more -- it is delivered as
// undelivered conversation input, fenced exactly the way an answered
// round's own input used to be, and the resume settles the thread through
// commit.Conversation rather than resolving it through
// commit.ResolveQuestions. Deviation from the plan's own name: this is a
// plain owner delivery (Undelivered() non-empty, no D14/validation/floor
// reason, and the session's prior run did not end in error), so by design
// section 22.4's resume-charging table it is the one case that is never
// charged (BumpResumes=false) -- the "once" in this test's own name
// predates the owner's correction recorded in the build log ("owner-
// delivery resumes set BumpResumes=false"), which TestOwnerDeliveriesNever
// Exhaust and TestMixedResumeIsCharged exercise at the aggregate level.
// This test keeps the plan's own name but asserts the corrected amount: the
// session's resumes count stays unchanged.
func TestPlanningHandler_Resume_ConversationBumpsResumesOnceAndFences(t *testing.T) {
	t.Parallel()
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

	openSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	// A recording wrapper around the same Fake so the session it minted for
	// the first turn is the one the resume call reuses.
	rec := &recordingRuntime{rt: rt}
	resumeDeps := claim(t, s, rec, ticketID)
	commit, err := runPlanning(t, s, resumeDeps, ticketID)
	if err != nil {
		t.Fatalf("planning resume Run: %v", err)
	}
	// This resume is a plain owner delivery (Undelivered() non-empty, no
	// other agent-driven reason, and the session's one prior run ended
	// "question", not "error"), so design section 22.4's own table charges
	// it nothing: the resume is proved by the commit's session id matching
	// the already-open session, and by that session's resumes total having
	// stayed at zero (Reserve never bumped it).
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != openSess.ID {
		t.Fatalf("commit.Session = %+v, want the already-open session %d", commit.Session, openSess.ID)
	}
	sessions, err := s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	gotResumes := -1
	for _, sess := range sessions {
		if sess.ID == openSess.ID {
			gotResumes = sess.Resumes
		}
	}
	if gotResumes != 0 {
		t.Errorf("session %d resumes = %d, want 0 (an owner-delivery resume is never charged)", openSess.ID, gotResumes)
	}
	if commit.Conversation == nil || len(commit.Conversation.Settle) != 1 || commit.Conversation.Settle[0].QuestionID != open[0].ID || commit.Conversation.Settle[0].Decision == "" {
		t.Errorf("commit.Conversation = %+v, want one settled entry for question %d with a decision", commit.Conversation, open[0].ID)
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

// TestPlanningHandler_MixedResume_DeliversQueuedAndCharges proves design
// section 22.4's own worked case: "the owner sends while run 40 is in
// flight; run 40 returns invalid output; the next tick is the D14 retry,
// which delivers the queued messages and is charged once." Turn 1 posts
// Q1; the owner answers it (undelivered); turn 2 resumes to deliver that
// answer (entry step 4, free) but returns invalid output, so the answer is
// still undelivered and the session now carries one invalid-output marker;
// turn 3's entry decision is the D14 retry (entry step 2, charged), and its
// own resume still carries the same still-undelivered answer alongside the
// D14 invalid-reason input.
func TestPlanningHandler_MixedResume_DeliversQueuedAndCharges(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // turn 1: posts Q1

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	if _, ansErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"}); ansErr != nil {
		t.Fatalf("AnswerQuestion: %v", ansErr)
	}

	sessions, err := s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	var sess store.Session
	for _, sv := range sessions {
		if sv.Job == testStatePlanning {
			sess = sv
		}
	}
	if sess.ID == 0 || sess.ExternalID == nil {
		t.Fatalf("SessionsForTicket: no open planning session with an external id")
	}
	extID := *sess.ExternalID

	// Turn 2: entry step 4 (plain delivery, free) resumes, but the model's
	// output is invalid. invalidOutputCommit writes no delivered marker, so
	// Q1's answer stays undelivered.
	invalidRT := &scriptedRuntime{t: t, steps: []scriptedStep{invalidResult("not well-formed XML", extID)}}
	turn2 := mustPlanning(t, s, claim(t, s, invalidRT, ticketID), ticketID)
	apply(t, s, getTicket(t, s, ticketID), turn2)

	sessions, err = s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	for _, sv := range sessions {
		if sv.ID == sess.ID && sv.Resumes != 0 {
			t.Errorf("after turn 2, session %d resumes = %d, want 0 (the free delivery resume, even though it failed)", sess.ID, sv.Resumes)
		}
	}

	// Turn 3: the D14 retry (entry step 2, charged), carrying both the
	// invalid-reason input and the still-undelivered Q1 answer.
	settleResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q2", Title: "Next", Body: testQuestionBody,
			Options: []response.Option{{Key: "a", Text: "A"}}, Recommended: "a",
		}},
		Replies: []response.Reply{
			{Question: "Q1", Settled: true, Decision: "Use the owner's chosen option."},
		},
	}
	rec := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{Response: settleResp, SessionID: extID, ExitCode: 0, AgentTime: time.Second}},
	}}}
	turn3, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("turn 3 planning Run: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, "invalid") {
		t.Errorf("turn 3 prompt does not carry the D14 invalid-reason input:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "conversation:\n") {
		t.Errorf("turn 3 prompt does not carry the still-undelivered conversation input:\n%s", rec.lastReq.Prompt)
	}
	if turn3.Conversation == nil || len(turn3.Conversation.Settle) != 1 || turn3.Conversation.Settle[0].QuestionID != open[0].ID {
		t.Errorf("turn3.Conversation = %+v, want Q1 settled", turn3.Conversation)
	}
	apply(t, s, getTicket(t, s, ticketID), turn3)

	sessions, err = s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	for _, sv := range sessions {
		if sv.ID == sess.ID && sv.Resumes != 1 {
			t.Errorf("after turn 3, session %d resumes = %d, want 1 (the D14 retry is charged, even though it also delivered Q1)", sess.ID, sv.Resumes)
		}
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
	t.Parallel()
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

// ---- 6.8 nothing_to_do: accept only every code claim false ----------------

// nothingToDoResponse builds a *response.NothingToDoResponse naming job
// planning and outcome nothing_to_do, the shape a scriptedRuntime step hands
// back in place of a real agent's XML document (design section 6.8, task 8).
// Every caller resumes a session that answeredRoundReadyForResume left with
// one open planning thread, Q1 (D31, design section 22.2): nothing_to_do
// settles it, or checkConversation would reject the response outright
// ("nothing_to_do needs every question settled") before this outcome's own
// commit logic is ever reached.
func nothingToDoResponse(claims []response.Claim, notes string) *response.NothingToDoResponse {
	return &response.NothingToDoResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeNothingToDo,
		Claims: claims, Notes: notes,
		Replies: []response.Reply{
			{Question: "Q1", Settled: true, Decision: testQ1SettledDecision},
		},
	}
}

// answeredRoundReadyForResume advances a fresh ticket through classify (real
// fixture) and the first turn (real fixture, posts Q1), answers Q1, and
// returns rt, the same runtime.Fake every prior call used, so the caller's
// own resume call can still reuse the session it minted.
func answeredRoundReadyForResume(t *testing.T, s *store.Store, ticketID int64) *runtime.Fake {
	t.Helper()
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1
	answerFixtureQuestion(t, s, ticketID)
	return rt
}

// TestPlanningHandler_NothingToDo_AllCodeClaimsFalseGoesToDone proves
// design section 6.8's nothing_to_do accept row (task 8): a nothing_to_do
// outcome naming at least one code claim, every one of them false,
// terminalizes the run, transitions the ticket straight to done, and sets
// TrackerEffect so the dispatcher posts tracker.NothingToDoComment after the
// commit (design D12) -- proved end to end through dispatch.Tick by
// internal/dispatch's own TestTick_PlanningNothingToDoAllFalseClaimsPostsTrackerComment.
func TestPlanningHandler_NothingToDo_AllCodeClaimsFalseGoesToDone(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	answeredRoundReadyForResume(t, s, ticketID)

	claims := []response.Claim{
		{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":1", Text: "the endpoint already returns hello"},
		{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":2", Text: "a test already covers it"},
	}
	const notes = "the described behavior already exists and is already tested"
	resumeRT := readyScriptedRuntime(t, readyStep(nothingToDoResponse(claims, notes), "ntd-false-sess"))

	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume (nothing_to_do, all false) Run: %v", err)
	}
	if commit.Next != testStateDone {
		t.Errorf("commit.Next = %q, want done", commit.Next)
	}
	if commit.Reason != "nothing to do" {
		t.Errorf("commit.Reason = %q, want %q", commit.Reason, "nothing to do")
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != string(response.OutcomeNothingToDo) {
		t.Fatalf("commit.Runs = %+v, want one terminalized run with outcome nothing_to_do", commit.Runs)
	}
	if commit.Session == nil {
		t.Error("commit.Session is nil, want the resumed session's id recorded")
	}
	// D31 (design section 22.3, 22.4): Q1 is a planning question, so it is
	// no longer resolved via ResolveQuestions (that round mechanism never
	// sees a planning question any more) -- it is settled via
	// commit.Conversation instead.
	if commit.Conversation == nil || len(commit.Conversation.Settle) != 1 || commit.Conversation.Settle[0].Decision == "" {
		t.Errorf("commit.Conversation = %+v, want one settled question with a decision", commit.Conversation)
	}
	if commit.TrackerEffect == nil {
		t.Fatal("commit.TrackerEffect is nil, want {Kind: nothing_to_do, Ref: t.TrackerRef, Notes: resp.Notes}")
	}
	if commit.TrackerEffect.Kind != store.TrackerEffectKindNothingToDo || commit.TrackerEffect.Ref != testRefFake1 || commit.TrackerEffect.Notes != notes {
		t.Errorf("commit.TrackerEffect = %+v, want {Kind: %q, Ref: %q, Notes: %q}",
			commit.TrackerEffect, store.TrackerEffectKindNothingToDo, testRefFake1, notes)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	final := getTicket(t, s, ticketID)
	if final.State != testStateDone {
		t.Errorf("final ticket state = %q, want done", final.State)
	}
}

// TestPlanningHandler_NothingToDo_TrueCodeClaimErrorsRatherThanEscalates
// proves F022 (option B: the validator owns the nothing_to_do rule, not
// this handler): response.Validate's CheckNothingToDoClaims already rejects
// any code claim that is not verdict=false as an InvalidOutputError before a
// real runtime's response ever reaches nothingToDoCommit, so a true code
// claim reaching the handler here (only reachable through a scripted test
// runtime that bypasses validation, standing in for a compromised or buggy
// agent process) is a defensive error, not an escalation the handler
// silently accepted the model's word for.
func TestPlanningHandler_NothingToDo_TrueCodeClaimErrorsRatherThanEscalates(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	answeredRoundReadyForResume(t, s, ticketID)

	claims := []response.Claim{
		{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictTrue, Evidence: readyClaimEvidencePath + ":1", Text: "still returns the wrong status"},
		{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":2", Text: "the other half already works"},
	}
	resumeRT := readyScriptedRuntime(t, readyStep(nothingToDoResponse(claims, "one part still needs the fix"), "ntd-true-sess"))

	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
	if err == nil {
		t.Fatalf("commit = %+v, err = nil, want an error: a non-false code claim must never reach an escalation or accept", commit)
	}
	if !strings.Contains(err.Error(), "non-false code claim") {
		t.Errorf("err = %q, want it to name a non-false code claim", err)
	}
	if commit.TicketID != 0 || commit.Escalation != nil || commit.Next != "" || commit.TrackerEffect != nil {
		t.Errorf("commit = %+v, want the zero value (no commit on this defensive error)", commit)
	}
}

// TestPlanningHandler_NothingToDo_NoCodeClaimsEscalates proves the same
// escalation fires when the response names no code claim at all (only env
// claims, or none): with nothing to check, the run cannot prove there is
// nothing to build either, so What names the zero case by name rather than
// counting a true claim that does not exist.
func TestPlanningHandler_NothingToDo_NoCodeClaimsEscalates(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	answeredRoundReadyForResume(t, s, ticketID)

	claims := []response.Claim{
		{Kind: response.ClaimKindEnv, Verdict: response.ClaimVerdictFalse, Evidence: "", Text: "no environment change is needed"},
	}
	resumeRT := readyScriptedRuntime(t, readyStep(nothingToDoResponse(claims, "nothing needs doing"), "ntd-none-sess"))

	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume (nothing_to_do, no code claims) Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want nothing_to_do_with_true_claims")
	}
	var payload response.EscalationPayload
	if err := json.Unmarshal(mustEscalationPayload(t, commit), &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != string(response.EscalationCodeNothingToDoWithTrueClaims) {
		t.Errorf("payload.Code = %q, want nothing_to_do_with_true_claims", payload.Code)
	}
	const wantWhat = "no code claims to verify"
	if payload.What != wantWhat {
		t.Errorf("payload.What = %q, want %q", payload.What, wantWhat)
	}
	if commit.TrackerEffect != nil {
		t.Error("commit.TrackerEffect is set, want nil (no tracker comment on an escalation)")
	}
}

// ---- 6.8 children: escalate split_unsupported ------------------------------

// TestPlanningHandler_Children_EscalatesSplitUnsupported proves design D6:
// planning's children outcome is not yet built, so it always escalates
// split_unsupported naming the run that returned it, and leaves the ticket
// waiting on the owner rather than transitioning it.
func TestPlanningHandler_Children_EscalatesSplitUnsupported(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	answeredRoundReadyForResume(t, s, ticketID)

	children := &response.ChildrenResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeChildren,
		Children: []response.Child{
			{Key: "c1", Title: "Part one", Body: "build the read path"},
			{Key: "c2", Title: "Part two", Body: "build the write path"},
		},
		Notes: "the two halves share no code",
		// D31 (design section 22.2): children needs every planning question
		// settled, or checkConversation rejects the response before this
		// outcome's own escalation logic is ever reached. The resumed
		// session carries one open thread, Q1 (answeredRoundReadyForResume).
		Replies: []response.Reply{
			{Question: "Q1", Settled: true, Decision: testQ1SettledDecision},
		},
	}
	resumeRT := readyScriptedRuntime(t, readyStep(children, "children-sess"))

	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume (children) Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (stays in planning)", commit.Next)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want split_unsupported")
	}
	if commit.Escalation.RunID == nil {
		t.Error("commit.Escalation.RunID is nil, want the run that returned children")
	}
	var payload response.EscalationPayload
	if err := json.Unmarshal(mustEscalationPayload(t, commit), &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != string(response.EscalationCodeSplitUnsupported) || payload.Origin != string(response.EscalationOriginSplit) {
		t.Errorf("payload = (Code=%q, Origin=%q), want (split_unsupported, split)", payload.Code, payload.Origin)
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingQuestions {
		t.Errorf("commit.Waiting = %v, want questions", commit.Waiting)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want planning", final.State)
	}
}

// ---- an answered classify round re-runs classify fresh --------------------

// TestPlanningHandler_Classify_AnsweredQuestionRoundRerunsClassifyFresh
// proves entry decision step 1(d): classify's own universal question
// answered restarts classify fresh with the round's rendered answers, and
// resolves the round in that same commit.
func TestPlanningHandler_Classify_AnsweredQuestionRoundRerunsClassifyFresh(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(rt.reqs[1].Prompt, testOptionAText) {
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
	t.Parallel()
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
	if payload.Code != testCodeResponseInvalid {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if payload.Code != testCodeResumesExhausted || payload.Origin != testOriginCapResumes {
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

// TestPlanningHandler_SessionExhausted_ReadyCohortRunsReviewInsteadOfEscalating
// proves F004: a ready produced on the final permitted resume stores a
// valid cohort and leaves the session exhausted (readyCommit carries no
// Next), so entry step 3's SessionExhausted case must review that cohort
// (§6.5) before ever escalating resumes_exhausted -- otherwise the owner
// sees a misleading cap escalation while a reviewable cohort sits
// unreviewed underneath it. seedCohort stands in for the ready resume
// itself (already proved by TestPlanningHandler_Ready_..., task 7a/7b); this
// test only needs the cohort in place with its producing session exhausted.
func TestPlanningHandler_SessionExhausted_ReadyCohortRunsReviewInsteadOfEscalating(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("A ready cohort survives resume exhaustion."), validScenarios(2, "exhausted"))

	maxResumes := testMachine(t).Jobs[testStatePlanning].MaxResumes
	sess, state, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, maxResumes)
	if err != nil || state != store.SessionOpen {
		t.Fatalf("LatestSession after seedCohort = (state=%v, err=%v), want SessionOpen", state, err)
	}

	owner := "exhausted-with-cohort-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpResumesToCap(t, s, ticketID, sess.ID, maxResumes, owner, expires)

	_, state, err = s.LatestSession(t.Context(), ticketID, testStatePlanning, maxResumes)
	if err != nil || state != store.SessionExhausted {
		t.Fatalf("LatestSession after bump = (state=%v, err=%v), want SessionExhausted", state, err)
	}

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(), "post-exhaustion-review-sess")}}
	rec := &recordingRuntime{rt: rt}

	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning Run against an exhausted session with a cohort: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want nil: a ready cohort must be reviewed, not escalated as resumes_exhausted", commit.Escalation)
	}
	if len(commit.Artifacts) != 1 || commit.Artifacts[0].Type != testArtifactTypePlanreview {
		t.Fatalf("commit.Artifacts = %+v, want exactly one planreview artifact", commit.Artifacts)
	}
	if rec.lastReq.Job != response.JobPlanreview {
		t.Errorf("last runtime request job = %s, want planreview", rec.lastReq.Job)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	// The floor loop stays unreachable here (design F004: it needs a
	// planning resume, which an exhausted session cannot spend), so a
	// clean review posts the gate rather than pending or resuming.
	after := getTicket(t, s, ticketID)
	if after.WaitingOn == nil || *after.WaitingOn != testWaitingGate {
		t.Errorf("after the review: ticket.WaitingOn = %v, want gate", after.WaitingOn)
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
	t.Parallel()
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
	const wantObjective = "Add a greet package with a fixed hello message, delivered as three small build tasks, so a later ticket can wire it into the HTTP server."
	if roundTripped.Overview.Objective != wantObjective {
		t.Errorf("stored plan objective = %q, want %q", roundTripped.Overview.Objective, wantObjective)
	}
	if len(roundTripped.Delivery.Tasks) != 3 || roundTripped.Review.TrustRoot != "none" {
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

// TestReadyCommit_PostRunStoreFailureTerminalizesRun proves design F025:
// readyCommit's own os.OpenRoot failure (here, a project whose checkout
// directory does not exist) is a post-run infrastructure failure, not a
// runtime failure -- it surfaces only after runJob's Reserve has already
// written a run. Before the fix this escaped runPlanningResume as a bare
// error, which the dispatcher's release path could only release, not
// terminalize, orphaning the run with a NULL outcome forever. runAndRoute's
// postRunFailure seam now catches it: the reserved run terminalizes as an
// error, a post_run_failed escalation lands naming the same run, and the
// handler itself returns a nil error, so the commit reaches the dispatcher
// as an ordinary commit rather than a failure to release.
func TestReadyCommit_PostRunStoreFailureTerminalizesRun(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)

	proj := testProject
	proj.LocalPath = filepath.Join(t.TempDir(), "does-not-exist")
	projectID, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	answeredRoundReadyForResume(t, s, ticketID)

	plan := validPlan("Add a hello endpoint so a caller can get a plain-text greeting back over HTTP.")
	resp := readyResponse(plan, validClaims(), validScenarios(2, "post-run"))
	// D31 (design section 22.2): ready needs every planning question
	// settled, or checkConversation rejects the response before readyCommit
	// (and its own os.OpenRoot failure, this test's whole point) ever runs.
	// The resumed session carries one open thread, Q1
	// (answeredRoundReadyForResume).
	resp.Conversation = response.Conversation{Replies: []response.Reply{
		{Question: "Q1", Settled: true, Decision: testQ1SettledDecision},
	}}
	resumeRT := readyScriptedRuntime(t, readyStep(resp, "post-run-fail-sess"))

	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning resume (ready, missing checkout) Run: %v, want nil -- postRunFailure funnels the error into the commit", err)
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != string(response.OutcomeError) {
		t.Fatalf("commit.Runs = %+v, want exactly one terminalized run with outcome error", commit.Runs)
	}
	if commit.Session == nil {
		t.Error("commit.Session is nil, want the resumed session recorded")
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want a post_run_failed escalation")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodePostRunFailed) {
		t.Errorf("commit.Escalation.Payload.Code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodePostRunFailed)
	}
	if commit.Escalation.RunID == nil || *commit.Escalation.RunID != commit.Runs[0].ID {
		t.Errorf("commit.Escalation.RunID = %v, want %d (the same reserved run)", commit.Escalation.RunID, commit.Runs[0].ID)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginPlanningResume) {
		t.Errorf("commit.Escalation.Payload.Origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginPlanningResume)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	final := getTicket(t, s, ticketID)
	if final.WaitingOn == nil || *final.WaitingOn != testWaitingQuestions {
		t.Errorf("final ticket waiting_on = %v, want questions", final.WaitingOn)
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
	t.Parallel()
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
	// D31 (design section 22.2): ready needs every planning question
	// settled, or checkConversation rejects planB before readyCommit ever
	// runs. Q1, inserted above, is still open.
	planB.Conversation = response.Conversation{Replies: []response.Reply{
		{Question: "Q1", Settled: true, Decision: "Revise the plan with the owner's chosen option."},
	}}

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
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	badResp := readyResponse(validPlan("Store the ready cohort."), validClaims(), validScenarios(1, "bad"))
	goodResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q1", Title: "Continue?", Body: testQuestionBody,
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
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeClaude}, store.RunSeed{Model: testModelClaudeX})
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
	if _, err := s.InsertMessage(t.Context(), store.Message{TicketID: ticketID, Type: testMsgTypeUpdate, Author: testAuthorSystem, Body: body}); err != nil {
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
	t.Parallel()
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

// TestPlanReviewHasNoCodexHome proves a planreview run never carries
// CODEX_HOME in its environment (PKG9-PLAN.md section 7.3, D27): planreview
// names no sandbox in machine.toml, so it is one of runJob's unsandboxed
// jobs (applyPrivateTempRoot, not applySandbox's own judge-profile wiring),
// even when Deps.JudgeCodexHome is configured for the judge job elsewhere
// on the same process.
func TestPlanReviewHasNoCodexHome(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("No CODEX_HOME leaks into planreview."), validScenarios(2, "no-codex-home"))

	f := finding(response.SeverityMinor, "plan/design/shape", "no codex home check", "name it")
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(f), "no-codex-home-sess")}}
	rec := &recordingRuntime{rt: rt}

	deps := claim(t, s, rec, ticketID)
	deps.JudgeCodexHome = "/test/judge/codex/home"

	commit, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %d entries, want 1", len(commit.Artifacts))
	}

	if len(rec.lastReq.ExecPrefix) != 0 {
		t.Errorf("ExecPrefix = %v, want empty: planreview is unsandboxed", rec.lastReq.ExecPrefix)
	}
	for _, kv := range rec.lastReq.Env {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			t.Errorf("planreview's env carries %q, want no CODEX_HOME entry", kv)
		}
	}
}

// TestPlanningHandler_ReviewTick_DropsUnresolvedLocationFindings proves
// section 6.5's drop rule: a finding whose Location does not resolve as an
// element path in the stored plan (response.ResolvesInPlan) is dropped from
// the stored artifact, while a finding at a real path survives.
func TestPlanningHandler_ReviewTick_DropsUnresolvedLocationFindings(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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

// TestPlanningHandler_ReviewTick_CleanFloorPostsTheGate proves section 6.6's
// "Post" step (task 7c): a clean review (no at-or-below-floor findings)
// posts exactly one gate question in the same commit that stores the
// planreview artifact -- kind gate, options a/b, recommended "a", an
// allocated Q<n>, attached to the review run, body the plan's own objective
// -- and leaves the ticket in planning, waiting on "gate", rather than
// task 7b's removed shortcut straight to building.
func TestPlanningHandler_ReviewTick_CleanFloorPostsTheGate(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	const objective = "Clean review posts the gate."
	planVersion, runID := seedCohort(t, s, ticketID, validPlan(objective), validScenarios(2, "clean"))

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(), "clean-sess")}}
	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("review tick Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (the gate is a wait, not a transition)", commit.Next)
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingGate {
		t.Fatalf("commit.Waiting = %v, want gate", commit.Waiting)
	}
	if !commit.AttachRunToMsgs {
		t.Error("commit.AttachRunToMsgs = false, want true (the gate question attaches to the review run)")
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %d entries, want exactly 1 (the gate question)", len(commit.Messages))
	}
	msg := commit.Messages[0]
	if !strings.HasPrefix(msg.Body, objective+"\n\n") {
		t.Errorf("gate message body = %q, want it to start with the plan's objective %q, blank line, then what Approve does (F013)", msg.Body, objective)
	}
	var qp response.QuestionPayload
	if err = json.Unmarshal(msg.Payload, &qp); err != nil {
		t.Fatalf("unmarshal gate question payload: %v", err)
	}
	if qp.Kind != response.QuestionKindGate {
		t.Errorf("gate question Kind = %q, want gate", qp.Kind)
	}
	if qp.Key != "" {
		t.Errorf("gate question Key = %q, want empty (allocated by the commit)", qp.Key)
	}
	if qp.Recommended != "a" {
		t.Errorf("gate question Recommended = %q, want a", qp.Recommended)
	}
	wantOptions := []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}}
	if !slices.Equal(qp.Options, wantOptions) {
		t.Errorf("gate question Options = %+v, want %+v", qp.Options, wantOptions)
	}
	if qp.Items != nil {
		t.Errorf("gate question Items = %+v, want nil", qp.Items)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	posted, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(posted) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly 1", posted, err)
	}
	var postedPayload response.QuestionPayload
	if err := json.Unmarshal(posted[0].Payload, &postedPayload); err != nil {
		t.Fatalf("unmarshal posted gate question payload: %v", err)
	}
	if !strings.HasPrefix(postedPayload.Key, "Q") {
		t.Errorf("posted gate question Key = %q, want an allocated Q<n>", postedPayload.Key)
	}
	if posted[0].RunID == nil {
		t.Error("posted gate question RunID is nil, want it attached to the review run")
	}

	after := getTicket(t, s, ticketID)
	if after.State != testStatePlanning || after.WaitingOn == nil || *after.WaitingOn != testWaitingGate {
		t.Errorf("after posting the gate: ticket = (state=%q, waiting_on=%v), want (planning, gate)", after.State, after.WaitingOn)
	}

	_, _ = planVersion, runID
}

// storedQuestionKeys returns every persisted "question" message's allocated
// Key on ticketID, in insertion (ListMessages) order.
func storedQuestionKeys(t *testing.T, s *store.Store, ticketID int64) []string {
	t.Helper()
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var keys []string
	for i := range msgs {
		if msgs[i].Type != testMsgTypeQuestion {
			continue
		}
		var qp response.QuestionPayload
		if unmarshalErr := json.Unmarshal(msgs[i].Payload, &qp); unmarshalErr != nil {
			t.Fatalf("unmarshal question payload: %v", unmarshalErr)
		}
		keys = append(keys, qp.Key)
	}
	return keys
}

// settleQ1AndQ2 settles ticketID's open planning questions directly through
// store.CommitHandlerResult's own Conversation field (D31, design section
// 22.3), the same mechanism a real planning reply uses: a test-only
// shortcut for tests whose own point is unrelated to conversation
// settling, so they need not script a turn that carries <replies>.
func settleQ1AndQ2(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	settle := make([]store.SettleQuestion, len(open))
	for i := range open {
		settle[i] = store.SettleQuestion{QuestionID: open[i].ID, Decision: "settled directly for this test"}
	}
	owner := "settle-q1-q2-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Conversation: &store.ConversationCommit{Settle: settle},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult (settle): %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult (settle): applied = false")
	}
}

// TestPlanningHandler_QuestionKeys_GateSharesAllocationWithPlanningQuestions
// proves F030: a planning question batch never stores the model's own
// q.Key (design section 4.5, 6.7: "gate and planning questions use the same
// Q<n> allocation"). A batch whose model keys are Q2 and Q7 lands as Q1,
// Q2 (commit order, the model's own keys discarded), and a later gate
// question on the same ticket continues that same count as Q3, rather than
// colliding with either one.
func TestPlanningHandler_QuestionKeys_GateSharesAllocationWithPlanningQuestions(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	batch := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{
			{Key: "Q2", Title: "First", Body: "first body.", Options: []response.Option{{Key: "a", Text: "A"}}, Recommended: "a"},
			{Key: "Q7", Title: "Second", Body: "second body.", Options: []response.Option{{Key: "a", Text: "A"}}, Recommended: "a"},
		},
	}
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{Response: batch, SessionID: "keys-batch-sess", ExitCode: 0, AgentTime: time.Second}},
		readyStep(findingsResponse(), "keys-review-sess"),
	}}

	commit := mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID) // first turn: posts the Q2/Q7 batch
	for _, m := range commit.Messages {
		var qp response.QuestionPayload
		if unmarshalErr := json.Unmarshal(m.Payload, &qp); unmarshalErr != nil {
			t.Fatalf("unmarshal question payload: %v", unmarshalErr)
		}
		if qp.Key != "" {
			t.Errorf("commit question payload.Key = %q, want empty (store allocates in commit order)", qp.Key)
		}
	}
	apply(t, s, getTicket(t, s, ticketID), commit)

	if got := storedQuestionKeys(t, s, ticketID); !slices.Equal(got, []string{"Q1", "Q2"}) {
		t.Fatalf("stored question keys after the batch = %v, want [Q1 Q2] (the model's own Q2/Q7 discarded)", got)
	}

	// D31 (design section 22.4 entry step 6): the review tick refuses to
	// start while any planning thread is open, so Q1 and Q2 must settle
	// first -- this test is about the gate's own key allocation, not
	// conversation settling, so it settles them directly through the store
	// rather than scripting a third planning turn.
	settleQ1AndQ2(t, s, ticketID)

	seedCohort(t, s, ticketID, validPlan("The gate shares the Q<n> allocation."), validScenarios(2, "keys"))

	gateCommit := mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID) // review tick: clean floor posts the gate
	apply(t, s, getTicket(t, s, ticketID), gateCommit)

	if got := storedQuestionKeys(t, s, ticketID); !slices.Equal(got, []string{"Q1", "Q2", "Q3"}) {
		t.Fatalf("stored question keys after the gate = %v, want [Q1 Q2 Q3]", got)
	}
}

// TestPlanningHandler_ReviewTick_FloorFindingsPendThenResumeThenDeliverThenStop
// proves entry steps 6 and 7 across ticks (design section 5.1, 5.3): floor
// findings write "planreview vN pending" and leave the ticket in planning,
// not waiting; the next tick resumes planning with the findings fenced and
// writes "planreview vN delivered" in that same commit; a following tick
// does not resume again (no live pending marker left).
func TestPlanningHandler_ReviewTick_FloorFindingsPendThenResumeThenDeliverThenStop(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if !strings.Contains(rec.lastReq.Prompt, testOptionAText) {
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
	t.Parallel()
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
	if payload.Code != testCodeResponseInvalid || payload.Origin != testArtifactTypePlanreview {
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
	t.Parallel()
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

// ---- caps: the agent budget, checked identically at every runJob call -----

// claimWithBudget is claimWithRuntimes with Budget zeroed out (already
// exhausted: agentSeconds*time.Second >= 0 on the very first call) and
// Reserve wrapped to fail the test if runJob's budget check (design section
// 4.6 step 4, which runs before step 7's Reserve) ever lets a call through
// to it: an exhausted budget must refuse the call with no run ever reserved
// (design section 6.8's "none" Run column for ErrBudget).
func claimWithBudget(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) job.Deps {
	t.Helper()
	deps := claimWithRuntimes(t, s, rt, ticketID)
	deps.Budget = 0
	deps.Reserve = func(context.Context, int64, store.SessionUpsert, store.RunSeed) (store.Reserved, error) {
		t.Fatal("Reserve was called, want the exhausted budget to refuse the call first")
		return store.Reserved{}, nil
	}
	return deps
}

// assertWallClockEscalation asserts commit is the one shape every runJob
// caller's ErrBudget maps to, identically, through routeFailure and
// budgetEscalationCommit (design section 6.8, task 8 part 4): wall_clock,
// cap_budget, a nil RunID (no run was ever reserved), the fixed What text,
// and waiting on questions.
func assertWallClockEscalation(t *testing.T, commit store.HandlerCommit) {
	t.Helper()
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want wall_clock")
	}
	if commit.Escalation.RunID != nil {
		t.Errorf("commit.Escalation.RunID = %v, want nil (no run was ever reserved)", commit.Escalation.RunID)
	}
	var payload response.EscalationPayload
	if err := json.Unmarshal(mustEscalationPayload(t, commit), &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != string(response.EscalationCodeWallClock) || payload.Origin != string(response.EscalationOriginCapBudget) {
		t.Errorf("payload = (Code=%q, Origin=%q), want (wall_clock, cap_budget)", payload.Code, payload.Origin)
	}
	const wantWhat = "raise budget.agent_minutes_per_ticket or abandon"
	if payload.What != wantWhat {
		t.Errorf("payload.What = %q, want %q", payload.What, wantWhat)
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingQuestions {
		t.Errorf("commit.Waiting = %v, want questions", commit.Waiting)
	}
}

// TestPlanningHandler_Budget_ExhaustedBeforeClassifyEscalatesWallClock,
// ...BeforeFirstTurn..., ...BeforeResume..., and ...BeforeReviewTick... prove
// every one of runJob's four callers (classify, the planning first turn, a
// resume, and the review tick) maps ErrBudget through the same helper
// (task 8 part 4): the escalation is identical no matter which step ran out
// of budget, and no run is ever reserved for it.

func TestPlanningHandler_Budget_ExhaustedBeforeClassifyEscalatesWallClock(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

	commit, err := runPlanning(t, s, claimWithBudget(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning (classify, budget exhausted) Run: %v", err)
	}
	assertWallClockEscalation(t, commit)
}

func TestPlanningHandler_Budget_ExhaustedBeforeFirstTurnEscalatesWallClock(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify sets kind

	commit, err := runPlanning(t, s, claimWithBudget(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning (first turn, budget exhausted) Run: %v", err)
	}
	assertWallClockEscalation(t, commit)
}

func TestPlanningHandler_Budget_ExhaustedBeforeResumeEscalatesWallClock(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := answeredRoundReadyForResume(t, s, ticketID)

	commit, err := runPlanning(t, s, claimWithBudget(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning (resume, budget exhausted) Run: %v", err)
	}
	assertWallClockEscalation(t, commit)
}

func TestPlanningHandler_Budget_ExhaustedBeforeReviewTickEscalatesWallClock(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := answeredRoundReadyForResume(t, s, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // resume: stores the ready cohort

	commit, err := runPlanning(t, s, claimWithBudget(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning (review tick, budget exhausted) Run: %v", err)
	}
	assertWallClockEscalation(t, commit)
}

// TestPlanningHandler_Budget_ExhaustedBeforeResumeResolvesTheAnsweredRound
// proved, pre-D31, that budget exhaustion does not strand the round that
// triggered the run: the wall_clock escalation resolved the same answered
// question ids the resume itself would have (design section 6.7, 6.8; PR
// #23 review). Q1 (answeredRoundReadyForResume) is now a planning
// question, which an answered round never carries any more (design
// section 22.3): the resume this test drives is reached through entry
// step 4 (plain conversation delivery, resolveIDs nil), so the budget
// escalation carries no ResolveQuestions at all -- Q1 stays open, its
// undelivered answer redelivered once the owner resolves the wall_clock
// escalation and planning resumes again.
func TestPlanningHandler_Budget_ExhaustedBeforeResumeResolvesTheAnsweredRound(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := answeredRoundReadyForResume(t, s, ticketID)

	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered): %v", err)
	}
	if len(answered) == 0 {
		t.Fatal("no answered question to resolve; the resume fixture changed")
	}

	commit, err := runPlanning(t, s, claimWithBudget(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning (resume, budget exhausted) Run: %v", err)
	}
	assertWallClockEscalation(t, commit)
	if len(commit.ResolveQuestions) != 0 {
		t.Errorf("commit.ResolveQuestions = %v, want none (Q1 is a planning question; it stays open, not resolved by this escalation)", commit.ResolveQuestions)
	}
}
