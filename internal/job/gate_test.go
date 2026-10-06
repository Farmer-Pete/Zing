// gate_test.go tests task 7c: section 6.6's gate approve pre-check
// (branches 0 through 6), the cohort seal on approval, and section 6.7's
// "resume or fresh" on a gate rejection or a reply-only round. It reuses
// planning_test.go and skeleton_test.go's shared fixtures (newJobTestStore,
// seedFeatureTicketInPlanning, seedCohort, validPlan, validScenarios, claim,
// apply, getTicket, recordingRuntime, questionResult, fakeRuntime,
// advanceQueuedToPlanning) and drives job.Registry()["planning"].Run
// directly, exactly as planning_test.go's own tests do. The gate's own
// posting (a clean review's own commit) is pinned in planning_test.go's
// TestPlanningHandler_ReviewTick_CleanFloorPostsTheGate; this file starts
// from an already-posted, already-answered gate round, seeded directly
// through the store, so each branch is reachable without driving a whole
// review tick first.
package job_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// seedGateQuestion inserts one open gate question directly (design section
// 6.6): kind gate, options a/b, recommended "a", attached to runID when
// non-nil. Key is fixed ("Q1") since this bypasses CommitHandlerResult's own
// fillQuestionKeyTx allocation, which only the real gate-posting commit
// (planReviewOkCommit) goes through.
func seedGateQuestion(t *testing.T, s *store.Store, ticketID int64, runID *int64) int64 {
	t.Helper()
	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindGate, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: testApproveOptionText}, {Key: "b", Text: testRejectOptionText}},
	})
	if err != nil {
		t.Fatalf("marshal gate question payload: %v", err)
	}
	id, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, RunID: runID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State: new("open"), Body: "the plan objective", Payload: payload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(gate question): %v", err)
	}
	return id
}

// answerGateQuestion drives the real console draft/send path (SaveDraft,
// then SendBatch) against questionID: option, when non-nil, drafts a chip
// answer; notes, when non-empty, drafts a free reply on the same question.
// Both land in one batch, so AnsweredRounds sees one round carrying
// whichever of an answer, a reply, or both this call drafted.
func answerGateQuestion(t *testing.T, s *store.Store, ticketID, questionID int64, option *string, notes string) {
	t.Helper()
	if option != nil {
		if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Option: option}); err != nil {
			t.Fatalf("SaveDraft(option): %v", err)
		}
	}
	if notes != "" {
		if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Text: notes}); err != nil {
			t.Fatalf("SaveDraft(text): %v", err)
		}
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
}

// seedConfirmedGate seeds the confirming marker binding ticketID's own gate
// question qID, its newest sent approving answer (already written by
// answerGateQuestion), and planVersion (D32, design section 22.12.1,
// 22.12.3a). The branch tests below exercise gateApprove's own pre-check in
// isolation, so they seed this directly rather than drive a real confirming
// turn through the runtime; TestGateApproveRunsFreeConfirmingTurn (below)
// covers the real, unconfirmed path instead.
func seedConfirmedGate(t *testing.T, s *store.Store, ticketID, qID int64, planVersion int) {
	t.Helper()
	// ListMessages, not AnsweredRounds: by the time a retry seeds a new
	// cohort's confirmation, the original gate round may already be
	// resolved (ResolveQuestions, applied by an earlier commit in the same
	// test), and AnsweredRounds only ever returns an answered-but-unresolved
	// round.
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var aID int64
	found := false
	for i := range msgs {
		if msgs[i].Type == testMsgTypeAnswer && msgs[i].ParentID != nil && *msgs[i].ParentID == qID {
			aID = msgs[i].ID
			found = true
		}
	}
	if !found {
		t.Fatalf("seedConfirmedGate: no sent answer found on question %d", qID)
	}
	marker := fmt.Sprintf("gate confirmed run 1 plan v%d gate %d answer %d", planVersion, qID, aID)
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, ParentID: &qID, Type: testMsgTypeUpdate, Author: testAuthorSystem, Body: marker,
	}); err != nil {
		t.Fatalf("InsertMessage(confirming marker): %v", err)
	}
}

// assertSealFailedEscalation asserts commit carries section 6.6's own
// failing-branch shape: Escalation.RunID nil, Origin seal, Code seal_failed,
// What exactly want, and ResolveQuestions exactly the gate round's question
// ids (design section 6.6, 6.8).
func assertSealFailedEscalation(t *testing.T, commit store.HandlerCommit, questionID int64, want string) {
	t.Helper()
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want a seal_failed escalation")
	}
	if commit.Escalation.RunID != nil {
		t.Errorf("commit.Escalation.RunID = %v, want nil", *commit.Escalation.RunID)
	}
	p := commit.Escalation.Payload
	if p.Code != string(response.EscalationCodeSealFailed) {
		t.Errorf("commit.Escalation.Payload.Code = %q, want seal_failed", p.Code)
	}
	if p.Origin != string(response.EscalationOriginSeal) {
		t.Errorf("commit.Escalation.Payload.Origin = %q, want seal", p.Origin)
	}
	if p.What != want {
		t.Errorf("commit.Escalation.Payload.What = %q, want %q", p.What, want)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != questionID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, questionID)
	}
}

// ---- D32: the gate's confirming turn (design section 22.12.3) ------------

// confirmedResult builds a scripted step whose response is a minimal,
// valid ConfirmedResponse.
func confirmedResult(sessionID, notes string) scriptedStep {
	resp := &response.ConfirmedResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeConfirmed,
		Notes: notes,
	}
	return scriptedStep{res: runtime.RunResult{Response: resp, SessionID: sessionID, ExitCode: 0, AgentTime: time.Second}}
}

// TestGateApproveRunsFreeConfirmingTurn proves D32's own entry change
// (design section 22.12.3): approving an unconfirmed gate no longer seals
// at once. It resumes the cohort's own producing session with
// prompt.ConfirmHeader in place of a job prompt, as a free resume
// (BumpResumes stays false: nothing agent-driven rode along), and a
// confirmed response writes the confirming marker while leaving the gate
// question "answered" and the ticket in planning, not building.
func TestGateApproveRunsFreeConfirmingTurn(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	_, runID := seedCohort(t, s, ticketID, validPlan("Confirming turn."), validScenarios(2, "confirm"))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{confirmedResult("confirm-sess-1", "Nothing is open.")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve (confirming turn) Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (the confirming turn never seals directly)", commit.Next)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Errorf("commit.Session = %+v, want BumpResumes=false (a free, owner-triggered resume)", commit.Session)
	}
	if !strings.HasPrefix(rec.lastReq.Prompt, "The owner wants to approve this plan and close the gate.") {
		t.Errorf("confirming turn prompt = %q, want it to lead with ConfirmHeader", rec.lastReq.Prompt)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	after := getTicket(t, s, ticketID)
	if after.State != testStatePlanning {
		t.Errorf("ticket state = %q, want still planning", after.State)
	}
	gate, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage(gate): %v", err)
	}
	if gate.State == nil || *gate.State != "answered" {
		t.Errorf("gate state = %v, want still answered (the seal resolves it, not the confirming turn)", gate.State)
	}
	if n := countMsgs(t, s, ticketID, testMsgTypeUpdate, "gate confirmed run "); n != 1 {
		t.Errorf("confirming markers = %d, want 1", n)
	}
}

// countMsgs counts ticketID's own messages of type with a body starting
// with prefix, through the public store API (ListMessages), not a direct
// SQL count: package job_test has no *sql.DB handle.
func countMsgs(t *testing.T, s *store.Store, ticketID int64, msgType, prefix string) int {
	t.Helper()
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	n := 0
	for i := range msgs {
		if msgs[i].Type == msgType && strings.HasPrefix(msgs[i].Body, prefix) {
			n++
		}
	}
	return n
}

// TestConfirmedWritesMarkerThenNextTickSeals proves the confirming turn's
// own two-tick shape end to end (design section 22.12.3's table): the
// first tick's "confirmed" commit writes the marker and nothing else
// transitions; applying it and running planning again finds the approval
// confirmed and seals, moving the ticket to building.
func TestConfirmedWritesMarkerThenNextTickSeals(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	_, runID := seedCohort(t, s, ticketID, validPlan("Two ticks."), validScenarios(2, "twotick"))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{confirmedResult("confirm-sess-2", "Nothing is open.")}}
	firstCommit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("first tick (confirming turn) Run: %v", err)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	secondCommit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("second tick (seal) Run: %v", err)
	}
	if secondCommit.Next != testStateBuilding || secondCommit.Seal == nil {
		t.Fatalf("second tick commit = (Next=%q, Seal=%+v), want (building, sealing run %d)", secondCommit.Next, secondCommit.Seal, runID)
	}
	apply(t, s, getTicket(t, s, ticketID), secondCommit)

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building", final.State)
	}
}

// TestGateApproveNotesReachConfirmingTurn proves the approval notes row
// (design section 22.12.3): a reply sent alongside the approve pick
// reaches the confirming turn's own prompt as a fenced "notes" input.
func TestGateApproveNotesReachConfirmingTurn(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	_, runID := seedCohort(t, s, ticketID, validPlan("Notes reach confirm."), validScenarios(2, "notes"))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "the JSON must stay stable")

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{confirmedResult("confirm-sess-3", "Nothing is open.")}}
	rec := &recordingRuntime{rt: rt}
	if _, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID); err != nil {
		t.Fatalf("gate approve (confirming turn) Run: %v", err)
	}
	assertFenced(t, rec.lastReq.Prompt, "notes", "the JSON must stay stable")
}

// TestConfirmingTurnQuestionsCancelsApproval proves the confirming turn's
// own "questions" row (design section 22.12.3's table): the agent asking a
// new question instead of confirming resolves the gate question (with a
// cancellation marker naming the run, not a batch) and leaves the ticket
// waiting on the new thread -- the owner gets a fresh gate only once it
// settles and planning reaches ready again.
func TestConfirmingTurnQuestionsCancelsApproval(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	_, runID := seedCohort(t, s, ticketID, validPlan("Cancels on questions."), validScenarios(2, "cancels"))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	questionsResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestions,
		Questions: []response.Question{{
			Key: "q1", Title: "Should the JSON carry a schema version?", Body: testQuestionBody,
			Options:     []response.Option{{Key: "a", Text: testOptionAText}, {Key: "b", Text: testOptionBText}},
			Recommended: "a",
		}},
		Progress: "Checking the owner's note before confirming.",
	}
	rt := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{Response: questionsResp, SessionID: "confirm-sess-4", ExitCode: 0, AgentTime: time.Second}},
	}}
	commit, err := runPlanning(t, s, claim(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve (confirming turn, questions) Run: %v", err)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d] (the gate resolves on cancellation)", commit.ResolveQuestions, qID)
	}
	waiting := testWaitingQuestions
	if commit.Waiting == nil || *commit.Waiting != waiting {
		t.Errorf("commit.Waiting = %v, want %q", commit.Waiting, waiting)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	if n := countMsgs(t, s, ticketID, testMsgTypeUpdate, "gate approval cancelled gate "); n != 1 {
		t.Errorf("cancellation markers = %d, want 1", n)
	}
	after := getTicket(t, s, ticketID)
	if after.State != testStatePlanning {
		t.Errorf("ticket state = %q, want still planning", after.State)
	}
}

// ---- approve: branch 4 (seals) and branch 5 (already sealed) --------------

// TestPlanningHandler_Gate_Approve_SealsExactlyTheCohortAndTransitionsToBuilding
// proves branch 4: approving an unsealed, valid cohort seals exactly that
// cohort's scenario rows (an older cohort's rows stay unsealed), transitions
// to building, and resolves the gate round.
func TestPlanningHandler_Gate_Approve_SealsExactlyTheCohortAndTransitionsToBuilding(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	_, oldRunID := seedCohort(t, s, ticketID, validPlan("Older cohort."), validScenarios(2, "old"))
	newPlanVersion, newRunID := seedCohort(t, s, ticketID, validPlan("Newer cohort."), validScenarios(3, "new"))

	qID := seedGateQuestion(t, s, ticketID, &newRunID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")
	seedConfirmedGate(t, s, ticketID, qID, newPlanVersion)

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve Run: %v", err)
	}
	if commit.Next != testStateBuilding || commit.Reason == "" {
		t.Fatalf("commit = (Next=%q, Reason=%q), want (building, non-empty)", commit.Next, commit.Reason)
	}
	if commit.Seal == nil {
		t.Fatal("commit.Seal is nil, want the new cohort sealed")
	}
	if commit.Seal.RunID != newRunID || commit.Seal.PlanVersion != newPlanVersion || commit.Seal.ExpectedCount != 3 {
		t.Errorf("commit.Seal = %+v, want RunID=%d PlanVersion=%d ExpectedCount=3", commit.Seal, newRunID, newPlanVersion)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)

	newScenarios, err := s.ScenariosForRun(t.Context(), ticketID, newRunID, false)
	if err != nil {
		t.Fatalf("ScenariosForRun(new): %v", err)
	}
	if len(newScenarios) != 3 {
		t.Fatalf("new cohort scenarios = %d, want 3", len(newScenarios))
	}
	for i, sc := range newScenarios {
		if sc.SealedAt == nil {
			t.Errorf("new cohort scenario %d has no sealed_at, want sealed", i)
		}
	}

	oldScenarios, err := s.ScenariosForRun(t.Context(), ticketID, oldRunID, false)
	if err != nil {
		t.Fatalf("ScenariosForRun(old): %v", err)
	}
	if len(oldScenarios) != 2 {
		t.Fatalf("old cohort scenarios = %d, want 2", len(oldScenarios))
	}
	for i, sc := range oldScenarios {
		if sc.SealedAt != nil {
			t.Errorf("old cohort scenario %d has sealed_at %v, want unsealed", i, *sc.SealedAt)
		}
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building", final.State)
	}
}

// TestPlanningHandler_Gate_Approve_AlreadySealedTransitionsWithNoNewSeal
// proves branch 5: re-approving an already, consistently sealed cohort
// transitions to building with no new Seal request, and still resolves the
// gate round.
func TestPlanningHandler_Gate_Approve_AlreadySealedTransitionsWithNoNewSeal(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedSealedCohort(t, s, ticketID, validPlan("Already sealed."), validScenarios(2, "sealed"))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")
	seedConfirmedGate(t, s, ticketID, qID, planVersion)

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve Run: %v", err)
	}
	if commit.Next != testStateBuilding {
		t.Errorf("commit.Next = %q, want building", commit.Next)
	}
	if commit.Seal != nil {
		t.Errorf("commit.Seal = %+v, want nil (already sealed)", commit.Seal)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}

// ---- approve: the failing branches (0, 1, 2, 3, 6) -------------------------

// TestPlanningHandler_Gate_Approve_Branch1_NoCohortEscalates proves branch
// 1: no plan artifact at all escalates seal_failed with RunID nil and the
// exact "no current plan cohort" What.
func TestPlanningHandler_Gate_Approve_Branch1_NoCohortEscalates(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	qID := seedGateQuestion(t, s, ticketID, nil)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve Run: %v", err)
	}
	assertSealFailedEscalation(t, commit, qID, "no current plan cohort")
}

// TestPlanningHandler_Gate_Approve_Branch2_NoProducingRunEscalates proves
// branch 2: a plan artifact with a null run_id (a legacy cohort) escalates
// seal_failed with the exact "cohort has no producing run" What.
func TestPlanningHandler_Gate_Approve_Branch2_NoProducingRunEscalates(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	plan := validPlan("Legacy cohort.")
	normalizePlanArraysForTest(&plan)
	planPayload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err = s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypePlan, Version: 1, Payload: planPayload}); err != nil {
		t.Fatalf("InsertArtifact(plan, no run_id): %v", err)
	}

	qID := seedGateQuestion(t, s, ticketID, nil)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve Run: %v", err)
	}
	assertSealFailedEscalation(t, commit, qID, "cohort has no producing run")
}

// TestPlanningHandler_Gate_Approve_Branch3_ScenarioCountOutOfRangeEscalates
// proves branch 3 at both ends of the 2-30 range: a 1-scenario cohort and a
// 31-scenario cohort each escalate seal_failed with the exact "cohort has
// <n> scenarios, want 2 to 30" What.
func TestPlanningHandler_Gate_Approve_Branch3_ScenarioCountOutOfRangeEscalates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		n    int
	}{
		{"TooFew", 1},
		{"TooMany", 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newJobTestStore(t)
			ticketID := seedFeatureTicketInPlanning(t, s)
			planVersion, runID := seedCohort(t, s, ticketID, validPlan("Out of range."), validScenarios(tc.n, "range"))

			qID := seedGateQuestion(t, s, ticketID, &runID)
			answerGateQuestion(t, s, ticketID, qID, new("a"), "")
			seedConfirmedGate(t, s, ticketID, qID, planVersion)

			commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
			if err != nil {
				t.Fatalf("gate approve Run: %v", err)
			}
			want := "cohort has " + strconv.Itoa(tc.n) + " scenarios, want 2 to 30"
			assertSealFailedEscalation(t, commit, qID, want)
		})
	}
}

// TestPlanningHandler_Gate_Approve_Branch6_PartiallySealedEscalates proves
// branch 6: a cohort with one scenario pre-sealed and the rest unsealed
// escalates seal_failed with the exact "cohort is partially or
// inconsistently sealed (1 of 3)" What.
func TestPlanningHandler_Gate_Approve_Branch6_PartiallySealedEscalates(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	runID := seedPartiallySealedCohort(t, s, ticketID, validPlan("Partially sealed."), validScenarios(3, "partial"), 1)

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")
	seedConfirmedGate(t, s, ticketID, qID, 1)

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve Run: %v", err)
	}
	assertSealFailedEscalation(t, commit, qID, "cohort is partially or inconsistently sealed (1 of 3)")
}

// TestPlanningHandler_Gate_Approve_Branch0_TwoMismatchMarkersEscalates
// proves branch 0: two "seal mismatch cohort <runID>" markers for the
// current cohort escalate seal_failed with the exact "seal transaction
// mismatched twice" What, even though the cohort itself is otherwise a
// clean, sealable one (branch 0 wins over branches 3-6).
func TestPlanningHandler_Gate_Approve_Branch0_TwoMismatchMarkersEscalates(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Twice mismatched."), validScenarios(2, "mismatch"))

	insertUpdateMarker(t, s, ticketID, "seal mismatch cohort "+strconv.FormatInt(runID, 10))
	insertUpdateMarker(t, s, ticketID, "seal mismatch cohort "+strconv.FormatInt(runID, 10))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")
	seedConfirmedGate(t, s, ticketID, qID, planVersion)

	commit, err := runPlanning(t, s, claim(t, s, fakeRuntime(t), ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate approve Run: %v", err)
	}
	assertSealFailedEscalation(t, commit, qID, "seal transaction mismatched twice")
}

// ---- reject: option b, reply-only, and "resume or fresh" -------------------

// TestPlanningHandler_Gate_Reject_OptionBWithNotesResumesOpenSession proves
// section 6.6/6.7: rejecting with option "b" plus a reply resumes the open
// planning session with the reply's body fenced as notes, and resolves the
// gate round.
func TestPlanningHandler_Gate_Reject_OptionBWithNotesResumesOpenSession(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1
	answerFixtureQuestion(t, s, ticketID)                                                           // Q1: opens the planning session, still open after

	qID := seedGateQuestion(t, s, ticketID, nil)
	const notes = "not quite right, please revisit the greeting copy"
	answerGateQuestion(t, s, ticketID, qID, new("b"), notes)

	openSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "gate-reject-sess")}}
	rec := &recordingRuntime{rt: resumeRT}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate reject Run: %v", err)
	}
	// BumpResumes is charged by Reserve now, not by the terminal commit
	// (design section 4.2); the resume is proved instead by the commit's
	// session id matching the already-open session, not a freshly minted
	// one.
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != openSess.ID {
		t.Fatalf("commit.Session = %+v, want the already-open session %d (a resume, not fresh)", commit.Session, openSess.ID)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
	if rec.lastReq.Prompt == "" {
		t.Fatal("recordingRuntime saw an empty resume prompt")
	}
	if !strings.Contains(rec.lastReq.Prompt, "<<<UNTRUSTED ") || !strings.Contains(rec.lastReq.Prompt, notes) {
		t.Errorf("resume prompt does not carry the fenced rejection notes:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningHandler_Gate_Reject_ReplyOnlyResumesOpenSession proves section
// 6.6/6.7: a gate round carrying a reply and no option at all is a reject,
// same as option "b" -- it also resumes the open session with the reply's
// body fenced as notes.
func TestPlanningHandler_Gate_Reject_ReplyOnlyResumesOpenSession(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1
	answerFixtureQuestion(t, s, ticketID)

	qID := seedGateQuestion(t, s, ticketID, nil)
	const notes = "reply only, no chip clicked"
	answerGateQuestion(t, s, ticketID, qID, nil, notes)

	openSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "gate-reply-only-sess")}}
	rec := &recordingRuntime{rt: resumeRT}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate reply-only Run: %v", err)
	}
	// BumpResumes is charged by Reserve now, not by the terminal commit
	// (design section 4.2); the resume is proved instead by the commit's
	// session id matching the already-open session, not a freshly minted
	// one.
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != openSess.ID {
		t.Fatalf("commit.Session = %+v, want the already-open session %d (a resume, not fresh)", commit.Session, openSess.ID)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
	if !strings.Contains(rec.lastReq.Prompt, notes) {
		t.Errorf("resume prompt does not carry the reply-only notes:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningHandler_Gate_Reject_NoOpenSessionGoesToFreshFirstTurn proves
// section 6.7's "resume or fresh" fresh branch: with no open planning
// session (the ticket has never opened one; kind is already set), a gate
// rejection starts the planning first turn fresh with the notes, rather
// than resuming.
func TestPlanningHandler_Gate_Reject_NoOpenSessionGoesToFreshFirstTurn(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	qID := seedGateQuestion(t, s, ticketID, nil)
	const notes = "no session yet, start fresh"
	answerGateQuestion(t, s, ticketID, qID, new("b"), notes)

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "gate-fresh-sess")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate reject (fresh) Run: %v", err)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Fatalf("commit.Session = %+v, want a fresh session (BumpResumes false: Reserve mints a new session id for a fresh turn, distinct from a resume's own bumped one)", commit.Session)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
	if !strings.Contains(rec.lastReq.Prompt, notes) {
		t.Errorf("first-turn prompt does not carry the rejection notes:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningHandler_Gate_Reject_CapGateIncludesStoredFloorFindings proves
// issue #48 review P2: rejecting a gate posted at the loop cap
// (job/planning.go's own gateCapMarker, written in the same commit as the
// gate) carries the cohort's stored at-or-below-floor findings into the
// resume, fenced exactly as the floor-findings resume renders them,
// alongside the owner's notes -- a bare "fix these" note otherwise gives the
// planner nothing to act on.
func TestPlanningHandler_Gate_Reject_CapGateIncludesStoredFloorFindings(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1
	answerFixtureQuestion(t, s, ticketID)                                                           // Q1: opens the planning session, still open after

	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Cap gate reject."), validScenarios(2, "capreject"))
	f := finding(response.SeverityMinor, "plan/design/shape", "still a bit off", "tighten the copy")
	seedPlanreviewArtifact(t, s, ticketID, planVersion, runID, f)
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("gate cap reached plan v%d", planVersion))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	const notes = "fix these"
	answerGateQuestion(t, s, ticketID, qID, new("b"), notes)

	openSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "gate-reject-cap-sess")}}
	rec := &recordingRuntime{rt: resumeRT}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate reject (capped) Run: %v", err)
	}
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != openSess.ID {
		t.Fatalf("commit.Session = %+v, want the already-open session %d (a resume, not fresh)", commit.Session, openSess.ID)
	}
	if !strings.Contains(rec.lastReq.Prompt, "<<<UNTRUSTED ") || !strings.Contains(rec.lastReq.Prompt, "still a bit off") {
		t.Errorf("resume prompt does not carry the capped gate's stored floor finding, fenced:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, notes) {
		t.Errorf("resume prompt does not carry the owner's rejection notes:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningHandler_Gate_Reject_CapGateIncludesAboveFloorFindings proves
// owner decision Q2: rejecting a capped gate feeds back every finding still
// in the stored planreview artifact, above-floor findings included, not
// just the at-or-below-floor survivors outstandingFloorFindings would keep
// -- a capped gate can carry an above-floor finding when the owner picked
// option d on the cap_loops escalation (acceptPlanAtCap) to see the gate
// anyway.
func TestPlanningHandler_Gate_Reject_CapGateIncludesAboveFloorFindings(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1
	answerFixtureQuestion(t, s, ticketID)                                                           // Q1: opens the planning session, still open after

	planVersion, runID := seedCohort(t, s, ticketID, validPlan("Cap gate reject above floor."), validScenarios(2, "capreject2"))
	minor := finding(response.SeverityMinor, "plan/design/shape", "still a bit off", "tighten the copy")
	major := finding(response.SeverityMajor, "plan/design/other", "the plan skips migrations", "add a migration step")
	seedPlanreviewArtifact(t, s, ticketID, planVersion, runID, minor, major)
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("gate cap reached plan v%d", planVersion))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	const notes = "fix these"
	answerGateQuestion(t, s, ticketID, qID, new("b"), notes)

	openSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "gate-reject-cap-above-floor-sess")}}
	rec := &recordingRuntime{rt: resumeRT}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("gate reject (capped, above floor) Run: %v", err)
	}
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != openSess.ID {
		t.Fatalf("commit.Session = %+v, want the already-open session %d (a resume, not fresh)", commit.Session, openSess.ID)
	}
	if !strings.Contains(rec.lastReq.Prompt, "still a bit off") {
		t.Errorf("resume prompt does not carry the capped gate's at-or-below-floor finding:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "the plan skips migrations") {
		t.Errorf("resume prompt does not carry the capped gate's above-floor finding:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, notes) {
		t.Errorf("resume prompt does not carry the owner's rejection notes:\n%s", rec.lastReq.Prompt)
	}
}

// ---- seeding helpers this file adds on top of planning_test.go's own ------

// seedSealedCohort is seedCohort (planning_test.go), with every scenario
// artifact already sealed at the same instant: design section 6.6 branch
// 5's own precondition (a cohort consistently sealed before the gate is
// ever approved).
func seedSealedCohort(t *testing.T, s *store.Store, ticketID int64, plan response.Plan, scenarios []response.Scenario) (planVersion int, runID int64) {
	t.Helper()
	return seedCohortWithSeals(t, s, ticketID, plan, scenarios, len(scenarios))
}

// seedPartiallySealedCohort is seedCohort, with exactly sealedCount of
// scenarios' own artifacts sealed at the same instant and the rest left
// unsealed: design section 6.6 branch 6's own precondition.
func seedPartiallySealedCohort(t *testing.T, s *store.Store, ticketID int64, plan response.Plan, scenarios []response.Scenario, sealedCount int) int64 {
	t.Helper()
	_, runID := seedCohortWithSeals(t, s, ticketID, plan, scenarios, sealedCount)
	return runID
}

// seedCohortWithSeals is seedCohort (planning_test.go), sealing the first
// sealedCount scenario artifacts (in the order they are inserted, matching
// artifacts.id order) at one shared instant, and leaving the rest with a
// nil sealed_at.
func seedCohortWithSeals(t *testing.T, s *store.Store, ticketID int64, plan response.Plan, scenarios []response.Scenario, sealedCount int) (planVersion int, runID int64) {
	t.Helper()
	owner := "seed-sealed-cohort-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedCohortWithSeals: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeClaude}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("seedCohortWithSeals: reserve: %v", err)
	}

	normalizePlanArraysForTest(&plan)
	planPayload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("seedCohortWithSeals: marshal plan: %v", err)
	}
	sealedAt := time.Now().UTC().Truncate(time.Second)
	artifacts := make([]store.Artifact, 0, 1+len(scenarios))
	artifacts = append(artifacts, store.Artifact{Type: testArtifactTypePlan, RunID: &rsv.RunID, Payload: planPayload})
	for i, sc := range scenarios {
		scPayload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			t.Fatalf("seedCohortWithSeals: marshal scenario: %v", marshalErr)
		}
		a := store.Artifact{Type: testArtifactTypeScenario, RunID: &rsv.RunID, Payload: scPayload}
		if i < sealedCount {
			at := sealedAt
			a.SealedAt = &at
		}
		artifacts = append(artifacts, a)
	}

	extID := "seed-sealed-sess-" + strconv.FormatInt(rsv.SessionID, 10)
	outcome, exitCode, agentSeconds := "ready", 0, 1
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session:   &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID},
		Runs:      []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Artifacts: artifacts,
	})
	if err != nil || !applied {
		t.Fatalf("seedCohortWithSeals: CommitHandlerResult: applied=%v err=%v", applied, err)
	}

	cohort, ok, err := s.CurrentCohort(t.Context(), ticketID)
	if err != nil || !ok || cohort.RunID == nil {
		t.Fatalf("seedCohortWithSeals: CurrentCohort: %+v, ok=%v, %v", cohort, ok, err)
	}
	return cohort.PlanVersion, *cohort.RunID
}
