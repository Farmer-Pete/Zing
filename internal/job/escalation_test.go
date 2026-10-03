// escalation_test.go tests task 13: section 6.7's escalation resolution --
// entry step 1(b)'s decode, the choice-by-origin routing table, choice c
// (abandon), and a response_invalid retry's fresh D14 chain. It reuses
// skeleton_test.go, planning_test.go, and gate_test.go's shared fixtures
// (newJobTestStore, seedQueuedTicket, seedFeatureTicketInPlanning, claim,
// apply, getTicket, fakeRuntime, advanceQueuedToPlanning, mustPlanning,
// runPlanning, seedCohort, seedPlanreviewArtifact, finding,
// answerGateQuestion, seedGateQuestion) and drives
// job.Registry()["planning"].Run directly, exactly as those files' own
// tests do.
//
// Most tests here construct their escalation directly through
// HandlerCommit.Escalation (escalateDirect), the store-level equivalent
// the task instructions call out as an accepted alternative to driving a
// real upstream failure: task 13's own scope is resolution, and every
// upstream trigger (an exec failure, an agent's own error, a cap) already
// has its own coverage from tasks 6 through 10. The seal retry test and the
// response_invalid retry test instead drive the real upstream flow (a real
// gate branch, a real D14 chain), because those two specifically need real
// state resolution reads back into.
package job_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// ---- shared construction helpers -------------------------------------

// testEscalationPayload builds a schema-valid EscalationPayload for code and
// origin, the same fixed What/Why/Tried/Options shape
// internal/store's own escalationTestPayload uses (unexported there, so
// this package keeps its own small copy).
func testEscalationPayload(code response.EscalationCode, origin response.EscalationOrigin) response.EscalationPayload {
	return response.EscalationPayload{
		Code: string(code), What: "what happened", Why: "why it happened", Tried: "what was tried",
		Options: []string{"retry", "planning", "abandon"}, Origin: string(origin),
	}
}

// escalateDirect inserts an escalation message plus its linked question
// directly through CommitHandlerResult (design D10, section 6.7): a real
// HandlerCommit.Escalation, exactly the shape runJob's own failure paths
// build, without reproducing whichever upstream failure would ordinarily
// produce it. Returns the linked question's id, the one this file's tests
// answer through answerGateQuestion (SaveDraft + SendBatch).
func escalateDirect(t *testing.T, s *store.Store, ticketID int64, runID, sessionID *int64, code response.EscalationCode, origin response.EscalationOrigin) int64 {
	t.Helper()
	owner := "escalate-direct-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("escalateDirect: claim: claimed=%v err=%v", claimed, err)
	}

	payload := testEscalationPayload(code, origin)
	payload.SessionID = sessionID
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Escalation: &store.EscalationCommit{
			RunID: runID, Body: string(code) + ": " + payload.What, Payload: payload,
		},
	})
	if err != nil || !applied {
		t.Fatalf("escalateDirect: CommitHandlerResult: applied=%v err=%v", applied, err)
	}

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) == 0 {
		t.Fatalf("escalateDirect: QuestionsByState(open) = %v, %v, want at least one", open, err)
	}
	return open[len(open)-1].ID
}

// legacyEscalationQuestion inserts an escalation message plus its linked
// question directly through store.InsertMessage, carrying the fixed
// three-option payload every escalation offered before #47 item 2
// (design section 6.7, pre-fix): "Retry", "Back to planning", and
// "Abandon", recommended "b". escalateDirect now goes through the fixed
// escalateTx, which never offers "b" once the ticket is past planning, so
// it cannot produce this shape any more -- this helper stands in for one of
// the escalations the database already carried before that fix shipped
// (the plan's own "Existing stored escalations: not touched"), so
// building.go's and postbuild.go's own choice == b row (D14, left
// unchanged by #47 item 2) still has a real stored row to resolve in
// tests. Returns the linked question's id.
func legacyEscalationQuestion(t *testing.T, s *store.Store, ticketID int64, code response.EscalationCode, origin response.EscalationOrigin) int64 {
	t.Helper()
	payload := testEscalationPayload(code, origin)
	body := string(code) + ": " + payload.What
	escPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("legacyEscalationQuestion: marshal escalation payload: %v", err)
	}
	escID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeEscalation, Author: testAuthorZing, Body: body, Payload: escPayload,
	})
	if err != nil {
		t.Fatalf("legacyEscalationQuestion: InsertMessage(escalation): %v", err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: "b",
		Options: []response.Option{
			{Key: "a", Text: "Retry"},
			{Key: "b", Text: "Back to planning"},
			{Key: "c", Text: "Abandon"},
		},
	})
	if err != nil {
		t.Fatalf("legacyEscalationQuestion: marshal question payload: %v", err)
	}
	qID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, ParentID: &escID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State: new("open"), Body: body + "\n\nHow should Zing proceed?", Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("legacyEscalationQuestion: InsertMessage(question): %v", err)
	}
	return qID
}

// reserveTerminalRun opens a fresh session for job (design section 4.5's
// own Reserve, Session insert on su.ID == nil) and terminalizes its first
// run as "error", the RunID a run-caused escalation's own Escalation.RunID
// points at. openSession leaves the session's external_id set (SessionOpen,
// the shape a failed resume carries) or unset (SessionIdless, the shape an
// ErrStart first turn carries -- LatestSession treats it the same as none).
func reserveTerminalRun(t *testing.T, s *store.Store, ticketID int64, jobName string, openSession bool) (runID, sessionID int64) {
	t.Helper()
	owner := "reserve-terminal-run-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("reserveTerminalRun: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: jobName, Runtime: testRuntimeClaude}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("reserveTerminalRun: reserve: %v", err)
	}

	outcome, exitCode, agentSeconds := "error", 1, 1
	commit := store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	}
	if openSession {
		extID := fmt.Sprintf("reserve-terminal-run-sess-%d", rsv.SessionID)
		commit.Session = &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID}
	}
	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil || !applied {
		t.Fatalf("reserveTerminalRun: CommitHandlerResult: applied=%v err=%v", applied, err)
	}
	return rsv.RunID, rsv.SessionID
}

// seedAnsweredPlanningRound opens a planning session directly, posts one
// open question attached to its first run, and answers it with option "a",
// leaving it answered but unresolved: the shape entry steps 1(c) and 3
// preserve on an exhausted session (design D17), and cap_resumes'
// resolution later folds into a fresh first turn's own prompt.Answers.
func seedAnsweredPlanningRound(t *testing.T, s *store.Store, ticketID int64) (sessionID, questionID int64) {
	t.Helper()
	owner := "seed-planning-round-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedAnsweredPlanningRound: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeClaude}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("seedAnsweredPlanningRound: reserve: %v", err)
	}

	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: "a", Options: []response.Option{{Key: "a", Text: testOptionAText}, {Key: "b", Text: testOptionBText}},
	})
	if err != nil {
		t.Fatalf("seedAnsweredPlanningRound: marshal question payload: %v", err)
	}
	extID := fmt.Sprintf("seed-planning-round-sess-%d", rsv.SessionID)
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID},
		Messages: []store.Message{{
			TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing,
			State: new("open"), Body: "Q1 body", Payload: payload,
		}},
		AttachRunToMsgs: true,
		Runs:            []store.Run{{ID: rsv.RunID}},
		Waiting:         new(testWaitingQuestions),
	})
	if err != nil || !applied {
		t.Fatalf("seedAnsweredPlanningRound: CommitHandlerResult: applied=%v err=%v", applied, err)
	}

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("seedAnsweredPlanningRound: QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	qID := open[0].ID
	if _, ansErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"}); ansErr != nil {
		t.Fatalf("seedAnsweredPlanningRound: AnswerQuestion: %v", ansErr)
	}
	return rsv.SessionID, qID
}

// assertFenced asserts prompt carries text's label as a fenced input (design
// section 4.2, D15): the "label:\n" header, fence.Wrap's own "<<<UNTRUSTED "
// marker, and text itself.
func assertFenced(t *testing.T, prompt, label, text string) {
	t.Helper()
	if !strings.Contains(prompt, label+":\n") {
		t.Errorf("prompt has no %q labeled input:\n%s", label, prompt)
	}
	if !strings.Contains(prompt, "<<<UNTRUSTED ") {
		t.Errorf("prompt has no fence marker, want a fenced %q input:\n%s", label, prompt)
	}
	if !strings.Contains(prompt, text) {
		t.Errorf("prompt does not contain %q's text %q:\n%s", label, text, prompt)
	}
}

// assertNoLabel asserts prompt carries no label input at all: the negative
// half of section 6.7's per-cell "this input, not that one" rules (a's
// planreview retry carries no error; b's cap_loops back carries no
// findings; a response_invalid retry with n == 0 carries no invalid).
func assertNoLabel(t *testing.T, prompt, label string) {
	t.Helper()
	if strings.Contains(prompt, label+":\n") {
		t.Errorf("prompt carries a %q labeled input, want none:\n%s", label, prompt)
	}
}

// ---- classify origin: both choices classify fresh -------------------------

// TestEscalationResolve_Classify_EveryChoiceClassifiesFreshWithNotesAndError
// proves section 6.7's classify row: retry, back, and a reply with no
// option at all (routed as back) all classify fresh with the escalation's
// notes and error fenced, and SessionID empty (fresh, not a resume) --
// "classify + b classifies (does not plan)", and never resumes a session
// that never existed because kind is still unset.
func TestEscalationResolve_Classify_EveryChoiceClassifiesFreshWithNotesAndError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		option *string
	}{
		{"Retry", new("a")},
		{"Back", new("b")},
		{testCaseReplyOnly, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newJobTestStore(t)
			ticketID := seedQueuedTicket(t, s)
			advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)
			runID, sessID := reserveTerminalRun(t, s, ticketID, "classify", false)
			qID := escalateDirect(t, s, ticketID, &runID, &sessID, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginClassify)
			answerGateQuestion(t, s, ticketID, qID, tc.option, "please look again")

			rt := &scriptedRuntime{t: t, steps: []scriptedStep{classifyResult(response.OutcomeFeature, "cls-resolve-sess")}}
			rec := &recordingRuntime{rt: rt}
			commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
			if err != nil {
				t.Fatalf("escalation resolve (classify) Run: %v", err)
			}
			if rec.lastReq.SessionID != "" {
				t.Errorf("RunRequest.SessionID = %q, want empty (a fresh classify turn)", rec.lastReq.SessionID)
			}
			if commit.SetKind == nil {
				t.Fatal("commit.SetKind is nil, want a fresh classify turn's own outcome")
			}
			assertFenced(t, rec.lastReq.Prompt, "notes", "please look again")
			assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
			if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
				t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
			}
		})
	}
}

// ---- planning_first origin: both choices fresh -----------------------------

// TestEscalationResolve_PlanningFirst_EveryChoiceStartsFreshWithNotesAndError
// proves section 6.7's planning_first row ("retry | planning_first | resume
// or fresh with notes+error"; kind is set, so a session-idless ticket
// starts fresh): both retry and back carry the same notes and error and
// call the planning first turn with SessionID empty.
func TestEscalationResolve_PlanningFirst_EveryChoiceStartsFreshWithNotesAndError(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"a", "b"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			s := newJobTestStore(t)
			ticketID := seedFeatureTicketInPlanning(t, s)
			runID, sessID := reserveTerminalRun(t, s, ticketID, testStatePlanning, false) // idless: an ErrStart first turn
			qID := escalateDirect(t, s, ticketID, &runID, &sessID, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginPlanningFirst)
			answerGateQuestion(t, s, ticketID, qID, new(choice), "try again please")

			rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "planning-first-resolve-sess")}}
			rec := &recordingRuntime{rt: rt}
			commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
			if err != nil {
				t.Fatalf("escalation resolve (planning_first, %s) Run: %v", choice, err)
			}
			if rec.lastReq.SessionID != "" {
				t.Errorf("RunRequest.SessionID = %q, want empty (a fresh planning first turn)", rec.lastReq.SessionID)
			}
			if commit.Session == nil || commit.Session.BumpResumes {
				t.Fatalf("commit.Session = %+v, want a fresh session (BumpResumes false)", commit.Session)
			}
			assertFenced(t, rec.lastReq.Prompt, "notes", "try again please")
			assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
		})
	}
}

// ---- planning_resume origin: both choices resume ---------------------------

// TestEscalationResolve_PlanningResume_EveryChoiceResumesWithNotesAndError
// proves section 6.7's planning_resume row: the session survived the
// failed resume (still SessionOpen), so both retry and back resume it
// (SessionID set, not empty), carrying the escalation's notes and error.
func TestEscalationResolve_PlanningResume_EveryChoiceResumesWithNotesAndError(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"a", "b"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			s := newJobTestStore(t)
			ticketID := seedFeatureTicketInPlanning(t, s)
			runID, sessID := reserveTerminalRun(t, s, ticketID, testStatePlanning, true) // open: the failed resume's own session survives
			qID := escalateDirect(t, s, ticketID, &runID, &sessID, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginPlanningResume)
			answerGateQuestion(t, s, ticketID, qID, new(choice), "please resume")

			rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "planning-resume-resolve-sess")}}
			rec := &recordingRuntime{rt: rt}
			commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
			if err != nil {
				t.Fatalf("escalation resolve (planning_resume, %s) Run: %v", choice, err)
			}
			if rec.lastReq.SessionID == "" {
				t.Errorf("RunRequest.SessionID is empty, want the surviving session's external id (a resume)")
			}
			// BumpResumes is charged by Reserve now, not by the terminal
			// commit (design section 4.2), so a resume's own commit no
			// longer carries it; the resume is proved instead by the
			// commit's session id matching the surviving session sessID
			// names, not a freshly minted one.
			if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != sessID {
				t.Fatalf("commit.Session = %+v, want the resumed session %d", commit.Session, sessID)
			}
			assertFenced(t, rec.lastReq.Prompt, "notes", "please resume")
			assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
		})
	}
}

// ---- planreview origin: retry reviews, back goes to planning --------------

// TestEscalationResolve_Planreview_RetryReviewsFreshWithNotesOnly proves
// section 6.7's "a retry | planreview | review fresh with notes" row: no
// error input at all, unlike every other origin's retry.
func TestEscalationResolve_Planreview_RetryReviewsFreshWithNotesOnly(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("Planreview retry."), validScenarios(2, "prretry"))
	runID, sessID := reserveTerminalRun(t, s, ticketID, testArtifactTypePlanreview, false)
	qID := escalateDirect(t, s, ticketID, &runID, &sessID, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginPlanreview)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "please review again")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanreview, "planreview-retry-sess")}}
	rec := &recordingRuntime{rt: rt}
	_, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (planreview retry) Run: %v", err)
	}
	assertFenced(t, rec.lastReq.Prompt, "notes", "please review again")
	assertNoLabel(t, rec.lastReq.Prompt, "error")
}

// TestEscalationResolve_Planreview_BackResumesPlanningWithNotesAndError
// proves section 6.7's "b | any other except cap_budget | resume or fresh
// with notes + error" row applies to planreview too: back does not retry
// the review, it goes back to planning -- resuming the cohort's own still-
// open planning session (seedCohort's own producing session), since kind
// is set and resumeOrFresh finds it SessionOpen.
func TestEscalationResolve_Planreview_BackResumesPlanningWithNotesAndError(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("Planreview back."), validScenarios(2, "prback"))
	runID, sessID := reserveTerminalRun(t, s, ticketID, testArtifactTypePlanreview, false)
	qID := escalateDirect(t, s, ticketID, &runID, &sessID, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginPlanreview)
	answerGateQuestion(t, s, ticketID, qID, new("b"), "go back to planning")

	cohortSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "planreview-back-sess")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (planreview back) Run: %v", err)
	}
	// BumpResumes is charged by Reserve now, not by the terminal commit
	// (design section 4.2); the resume is proved instead by the commit's
	// session id matching the cohort's own still-open planning session.
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != cohortSess.ID {
		t.Fatalf("commit.Session = %+v, want the cohort's own planning session %d", commit.Session, cohortSess.ID)
	}
	if rec.lastReq.SessionID == "" {
		t.Error("RunRequest.SessionID is empty, want the cohort's own planning session's external id")
	}
	assertFenced(t, rec.lastReq.Prompt, "notes", "go back to planning")
	assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
}

// ---- seal/gate_approve origin ----------------------------------------------

// TestEscalationResolve_Seal_RetryReRunsApproveBranchesAndSealsAHealthyCohort
// proves section 6.7's "a retry | gate_approve, seal | re-run the approve
// branches" row end to end: a seal_failed escalation from a cohortless
// gate approval, retried once a healthy cohort exists, re-runs gateApprove
// and seals it, moving the ticket to building.
func TestEscalationResolve_Seal_RetryReRunsApproveBranchesAndSealsAHealthyCohort(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	gateQID := seedGateQuestion(t, s, ticketID, nil)
	answerGateQuestion(t, s, ticketID, gateQID, new("a"), "")
	firstCommit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve (branch 1, no cohort) Run: %v", err)
	}
	assertSealFailedEscalation(t, firstCommit, gateQID, "no current plan cohort")
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	newPlanVersion, newRunID := seedCohort(t, s, ticketID, validPlan("Now a healthy cohort."), validScenarios(2, "sealretry"))
	// The retry skips the confirming turn and calls gateApprove directly
	// (design section 22.12.3), but the seal invariant still requires a
	// confirmation for this new cohort's own plan version (design section
	// 22.12.3a): seed it against the original gate question, independent of
	// that question's own current (resolved) state.
	seedConfirmedGate(t, s, ticketID, gateQID, newPlanVersion)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one (the escalation's own linked question)", open, err)
	}
	answerGateQuestion(t, s, ticketID, open[0].ID, new("a"), "")

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (seal retry) Run: %v", err)
	}
	if commit.Next != testStateBuilding || commit.Seal == nil || commit.Seal.RunID != newRunID {
		t.Fatalf("commit = (Next=%q, Seal=%+v), want (building, sealing run %d)", commit.Next, commit.Seal, newRunID)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)
	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building", final.State)
	}
}

// TestEscalationResolve_Seal_BackResumesOrFreshWithNotesAndError proves
// section 6.7's "b | any other except cap_budget" row for seal: back does
// not retry the approve pre-check, it goes back to planning.
func TestEscalationResolve_Seal_BackResumesOrFreshWithNotesAndError(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeSealFailed, response.EscalationOriginSeal)
	answerGateQuestion(t, s, ticketID, qID, new("b"), "not ready, keep planning")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "seal-back-sess")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (seal back) Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (back to planning, not building)", commit.Next)
	}
	assertFenced(t, rec.lastReq.Prompt, "notes", "not ready, keep planning")
	assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
}

// TestEscalationResolve_GateApprove_RetryReRunsApproveBranches proves the
// gate_approve origin (design section 6.7 groups it with seal) shares seal's
// own retry code path, even though no upstream code escalates with this
// exact origin today.
func TestEscalationResolve_GateApprove_RetryReRunsApproveBranches(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeSealFailed, response.EscalationOriginGateApprove)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (gate_approve retry) Run: %v", err)
	}
	if commit.Escalation == nil || commit.Escalation.Payload.Code != string(response.EscalationCodeSealFailed) {
		t.Fatalf("commit.Escalation = %+v, want another seal_failed (still no cohort)", commit.Escalation)
	}
}

// ---- cap_resumes origin: both choices go fresh with preserved answers -----

// capResumesFreshQuestionResult builds a planning "question" outcome that
// also replies (but does not settle) the preserved planning thread Q1
// (D31, design section 22.2): a fresh session's own transcript
// (conversationFreshInput) still shows Q1 as an unsettled thread with an
// undelivered owner answer, so a response that ignores it fails
// checkConversation's "answer every owner message you receive" rule before
// ever reaching this outcome's own commit logic.
func capResumesFreshQuestionResult(sessionID string) scriptedStep {
	resp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q2", Title: "A question", Body: testQuestionBody,
			Options:     []response.Option{{Key: "a", Text: testOptionAText}, {Key: "b", Text: testOptionBText}},
			Recommended: "a",
		}},
		Replies: []response.Reply{
			{Question: "Q1", Text: "Noted, thanks."},
		},
	}
	return scriptedStep{res: runtime.RunResult{Response: resp, SessionID: sessionID, ExitCode: 0, AgentTime: time.Second}}
}

// TestEscalationResolve_CapResumes_BackStartsFreshCarriesPreservedAnswersAndResolvesBothRounds
// proves section 6.7's cap_resumes row, D31-adjusted (design section 22.4):
// "b starts a fresh session" still holds, but the preserved planning
// question Q1 no longer resolves through commit.ResolveQuestions -- no
// planning question ever does any more (D31 dropped
// resolveCapResumesEscalation's own round-preserving loop) -- it instead
// arrives in the fresh session's own conversation transcript, and only
// resolves if and when the agent settles it through a reply.
func TestEscalationResolve_CapResumes_BackStartsFreshCarriesPreservedAnswersAndResolvesBothRounds(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	sessID, preservedQID := seedAnsweredPlanningRound(t, s, ticketID)
	escQID := escalateDirect(t, s, ticketID, nil, &sessID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	answerGateQuestion(t, s, ticketID, escQID, new("b"), "")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{capResumesFreshQuestionResult("cap-resumes-fresh-sess")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (cap_resumes back) Run: %v", err)
	}
	if rec.lastReq.SessionID != "" {
		t.Errorf("RunRequest.SessionID = %q, want empty (a fresh session, the exhausted one abandoned)", rec.lastReq.SessionID)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Fatalf("commit.Session = %+v, want a fresh session (BumpResumes false)", commit.Session)
	}
	assertFenced(t, rec.lastReq.Prompt, "conversation", "Q1 body")
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != escQID {
		t.Fatalf("commit.ResolveQuestions = %v, want [%d] (only the escalation round; Q1 is a planning question, never resolved this way any more)", commit.ResolveQuestions, escQID)
	}
	if commit.Conversation == nil || len(commit.Conversation.Settle) != 0 {
		t.Errorf("commit.Conversation = %+v, want a reply with no settle (Q1 stays open; the agent only acknowledged it)", commit.Conversation)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)
	stillOpen, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered): %v", err)
	}
	found := false
	for _, q := range stillOpen {
		if q.ID == preservedQID {
			found = true
		}
	}
	if !found {
		t.Errorf("preserved question %d is no longer answered, want it to stay open until the agent settles it", preservedQID)
	}
}

// TestEscalationResolve_CapResumes_RetryBehavesTheSameAsBack proves choice
// a on cap_resumes takes the identical fresh-with-preserved-conversation
// path as b (design section 6.7: the exhausted session guarantees
// resumeOrFresh's own SessionOpen branch is unreachable either way).
func TestEscalationResolve_CapResumes_RetryBehavesTheSameAsBack(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	sessID, _ := seedAnsweredPlanningRound(t, s, ticketID)
	escQID := escalateDirect(t, s, ticketID, nil, &sessID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	answerGateQuestion(t, s, ticketID, escQID, new("a"), "")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{capResumesFreshQuestionResult("cap-resumes-retry-sess")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (cap_resumes retry) Run: %v", err)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Fatalf("commit.Session = %+v, want a fresh session", commit.Session)
	}
	assertFenced(t, rec.lastReq.Prompt, "conversation", "Q1 body")
}

// ---- cap_loops origin: retry carries findings, back does not --------------

// TestEscalationResolve_CapLoops_RetryResumesWithOutstandingFindingsAndNotes
// proves section 6.7's "a retry | cap_loops | resume or fresh with the
// outstanding at-or-below-floor findings + notes" row.
func TestEscalationResolve_CapLoops_RetryResumesWithOutstandingFindingsAndNotes(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Cap loops retry."), validScenarios(2, "caploopsretry"))
	f := finding(response.SeverityMinor, "plan/design/shape", "outstanding finding text", "fix it")
	seedPlanreviewArtifact(t, s, ticketID, planVersion, runID, f)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeLoopsExhausted, response.EscalationOriginCapLoops)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "keep going")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "cap-loops-retry-sess")}}
	rec := &recordingRuntime{rt: rt}
	_, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (cap_loops retry) Run: %v", err)
	}
	assertFenced(t, rec.lastReq.Prompt, "findings", "outstanding finding text")
	assertFenced(t, rec.lastReq.Prompt, "notes", "keep going")
	assertNoLabel(t, rec.lastReq.Prompt, "error")
}

// TestEscalationResolve_CapLoops_BackResumesWithNotesAndErrorNoFindings
// proves section 6.7's "b | any other except cap_budget" row for
// cap_loops: back carries no findings at all, only notes and error.
func TestEscalationResolve_CapLoops_BackResumesWithNotesAndErrorNoFindings(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Cap loops back."), validScenarios(2, "caploopsback"))
	f := finding(response.SeverityMinor, "plan/design/shape", "outstanding finding text", "fix it")
	seedPlanreviewArtifact(t, s, ticketID, planVersion, runID, f)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeLoopsExhausted, response.EscalationOriginCapLoops)
	answerGateQuestion(t, s, ticketID, qID, new("b"), "give up on this loop")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "cap-loops-back-sess")}}
	rec := &recordingRuntime{rt: rt}
	_, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (cap_loops back) Run: %v", err)
	}
	assertFenced(t, rec.lastReq.Prompt, "notes", "give up on this loop")
	assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
	assertNoLabel(t, rec.lastReq.Prompt, "findings")
}

// ---- cap_budget origin: both choices re-escalate ---------------------------

// TestEscalationResolve_CapBudget_EveryChoiceReEscalatesWallClock proves
// section 6.7's cap_budget row: "a retry | cap_budget | re-escalate
// wall_clock in this commit ... resolve the round", and "b | cap_budget | as
// retry" -- identical either way, with no runtime call at all.
func TestEscalationResolve_CapBudget_EveryChoiceReEscalatesWallClock(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"a", "b"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			s := newJobTestStore(t)
			ticketID := seedFeatureTicketInPlanning(t, s)
			qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeWallClock, response.EscalationOriginCapBudget)
			answerGateQuestion(t, s, ticketID, qID, new(choice), "")

			// No scripted steps: a runtime call here is a bug (cap_budget's
			// retry/back never runs a job, design section 6.7).
			commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
			if err != nil {
				t.Fatalf("escalation resolve (cap_budget %s) Run: %v", choice, err)
			}
			if commit.Escalation == nil {
				t.Fatal("commit.Escalation is nil, want a re-escalated wall_clock")
			}
			if commit.Escalation.Payload.Code != string(response.EscalationCodeWallClock) ||
				commit.Escalation.Payload.Origin != string(response.EscalationOriginCapBudget) {
				t.Errorf("payload = %+v, want (wall_clock, cap_budget)", commit.Escalation.Payload)
			}
			const wantWhat = "raise budget.agent_minutes_per_ticket or abandon"
			if commit.Escalation.Payload.What != wantWhat {
				t.Errorf("payload.What = %q, want %q", commit.Escalation.Payload.What, wantWhat)
			}
			if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
				t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
			}
		})
	}
}

// ---- split and nothing_to_do_claims: both choices resume or fresh ---------

// TestEscalationResolve_SplitAndNothingToDoClaims_EveryChoiceResumesOrFresh
// proves section 6.7's "a | split, nothing_to_do_claims | same as b" row:
// both origins, every choice -- explicit retry, explicit back, and a
// text-only reply with no option at all -- all resume or fresh with notes
// and error. ReplyOnly proves roundRecommendedOption (planning.go, #47
// follow-up): split_unsupported and nothing_to_do_with_true_claims are the
// only two codes escalationOptionsFor (store/commit.go) still recommends
// "b" for in planning, so a plain reply on one of these still goes back to
// planning, exactly as an explicit "b" would.
func TestEscalationResolve_SplitAndNothingToDoClaims_EveryChoiceResumesOrFresh(t *testing.T) {
	t.Parallel()
	for _, origin := range []response.EscalationOrigin{response.EscalationOriginSplit, response.EscalationOriginNothingToDoClaims} {
		for _, tc := range []struct {
			name   string
			option *string
		}{
			{"a", new("a")},
			{"b", new("b")},
			{testCaseReplyOnly, nil},
		} {
			t.Run(string(origin)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				s := newJobTestStore(t)
				ticketID := seedFeatureTicketInPlanning(t, s)
				runID, sessID := reserveTerminalRun(t, s, ticketID, testStatePlanning, false)
				code := response.EscalationCodeSplitUnsupported
				if origin == response.EscalationOriginNothingToDoClaims {
					code = response.EscalationCodeNothingToDoWithTrueClaims
				}
				qID := escalateDirect(t, s, ticketID, &runID, &sessID, code, origin)
				answerGateQuestion(t, s, ticketID, qID, tc.option, "notes here")

				rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "split-ntd-sess")}}
				rec := &recordingRuntime{rt: rt}
				_, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
				if err != nil {
					t.Fatalf("escalation resolve (%s, %s) Run: %v", origin, tc.name, err)
				}
				assertFenced(t, rec.lastReq.Prompt, "notes", "notes here")
				assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
			})
		}
	}
}

// ---- choice c: abandon, any origin -----------------------------------------

// TestEscalationResolve_Abandon_AnyOriginTransitionsAndResolvesEverything
// proves section 6.7's choice c row: the ticket transitions straight to
// abandoned with no runtime call, every open or answered question resolves
// (ResolveAll), and job.ValidateCommit (apply's own check) accepts the new
// planning -> abandoned edge.
func TestEscalationResolve_Abandon_AnyOriginTransitionsAndResolvesEverything(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	sessID, _ := seedAnsweredPlanningRound(t, s, ticketID) // an older preserved round, also swept up by ResolveAll
	qID := escalateDirect(t, s, ticketID, nil, &sessID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	answerGateQuestion(t, s, ticketID, qID, new("c"), "I'm done with this one")

	commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (abandon) Run: %v", err)
	}
	if commit.Next != testStateAbandoned {
		t.Fatalf("commit.Next = %q, want abandoned", commit.Next)
	}
	if !commit.ResolveAll {
		t.Error("commit.ResolveAll = false, want true")
	}
	if !strings.Contains(commit.Reason, "resumes_exhausted") {
		t.Errorf("commit.Reason = %q, want it to name the escalation's own code", commit.Reason)
	}

	apply(t, s, getTicket(t, s, ticketID), commit) // apply calls job.ValidateCommit: proves planning -> abandoned is legal

	final := getTicket(t, s, ticketID)
	if final.State != testStateAbandoned {
		t.Errorf("final ticket state = %q, want abandoned", final.State)
	}
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 0 {
		t.Fatalf("QuestionsByState(open) after abandon = %v, %v, want none", open, err)
	}
	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil || len(answered) != 0 {
		t.Fatalf("QuestionsByState(answered) after abandon = %v, %v, want none", answered, err)
	}
}

// ---- response_invalid retry: a fresh D14 chain -----------------------------

// TestEscalationResolve_ResponseInvalidRetry_FreshChainThenOneAutomaticRetryThenEscalatesAgain
// proves design section 4.5's own D14 chain-boundary rule end to end: once
// the owner retries a response_invalid escalation, the step runs with n ==
// 0 (no "invalid" input at all -- the escalated run is a chain boundary), a
// following invalid output gets one automatic retry (no immediate second
// escalation), and only a second consecutive invalid output after that
// escalates again.
func TestEscalationResolve_ResponseInvalidRetry_FreshChainThenOneAutomaticRetryThenEscalatesAgain(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	advanceQueuedToPlanning(t, s, fakeRuntime(t), ticketID)

	const reason = "no zing element in final message"
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		invalidResult(reason, "cls-ri-1"),
		invalidResult(reason, "cls-ri-2"),
	}}
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)) // first strike

	secondCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second (escalating) planning Run: %v", err)
	}
	if secondCommit.Escalation == nil {
		t.Fatal("second commit.Escalation is nil, want response_invalid")
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one", open, err)
	}
	answerGateQuestion(t, s, ticketID, open[0].ID, new("a"), "")

	rt2 := &scriptedRuntime{t: t, steps: []scriptedStep{invalidResult(reason, "cls-ri-3")}}
	rec := &recordingRuntime{rt: rt2}
	retryCommit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (response_invalid retry) Run: %v", err)
	}
	assertNoLabel(t, rec.lastReq.Prompt, "invalid") // n == 0: the escalated run is a chain boundary
	if retryCommit.Escalation != nil {
		t.Fatalf("retryCommit.Escalation = %+v, want nil (one automatic retry, no immediate second escalation)", retryCommit.Escalation)
	}
	apply(t, s, getTicket(t, s, ticketID), retryCommit)

	thirdCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, &scriptedRuntime{t: t, steps: []scriptedStep{invalidResult(reason, "cls-ri-4")}}, ticketID), ticketID)
	if err != nil {
		t.Fatalf("third planning Run: %v", err)
	}
	if thirdCommit.Escalation == nil {
		t.Fatal("thirdCommit.Escalation is nil, want response_invalid (the second consecutive invalid output since the retry)")
	}
	if thirdCommit.Escalation.Payload.Code != string(response.EscalationCodeResponseInvalid) {
		t.Errorf("thirdCommit.Escalation.Payload.Code = %q, want response_invalid", thirdCommit.Escalation.Payload.Code)
	}
}

// testStateAbandoned is the one state name this file's own abandon test
// needs, mirroring skeleton_test.go's own testStateX constants (job.go's
// unexported stateAbandoned).
const testStateAbandoned = "abandoned"
