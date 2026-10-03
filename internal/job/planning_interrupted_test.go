// planning_interrupted_test.go tests design D5 (section 7.4) for the
// planning job and its own gate confirming turn, and the three section 7.5
// bugs this milestone fixes that touch planning: the first-turn resume
// (design section 7.4's "planning, first turn" row), every ordinary resume
// (the "planning, resumes" row), the gate's own confirming turn, the
// exhausted-cap bypass, and the invalid-retry stall (bug 3). It reuses
// planning_test.go's and gate_test.go's own shared fixtures (newJobTestStore,
// seedQueuedTicket, seedFeatureTicketInPlanning, seedCohort, claim, apply,
// getTicket, scriptedRuntime, byJobRuntime, claimWithRuntimes,
// recordingRuntime, questionResult, invalidResult, readyScriptedRuntime,
// readyStep, confirmedResult, seedGateQuestion, answerGateQuestion) and
// drives job.Registry()["planning"].Run directly, exactly as those files'
// own tests do.
package job_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// wantInterruptedText is job.go's own interruptedResumeText, unexported and
// so repeated here literally: package job_test cannot reference it
// directly.
const wantInterruptedText = "the previous run was interrupted; continue and return your document"

// planFirstInterruptedSess is the one external session id
// TestPlanningInterruptedFirstTurnResumes threads through its own first
// turn, interrupt, and free resume (goconst: three uses, one constant).
const planFirstInterruptedSess = "plan-first-interrupted"

// onStartThenCancel is a test-only runtime.Runtime that calls req.OnStart
// (design section 7.1's own start handshake) before returning
// runtime.ErrCanceled without ever producing a response -- a real agent
// process whose prompt pipe closed mid-handshake (section 7.1: "if serve
// dies between steps 2 and 4, the agent's stdin closes with no prompt, and
// the CLI exits without doing work"), but one whose OnStart already fired,
// so runJobWith's own RecordRunStart closure (runjob.go) has already
// written the session's external id before the cancel.
type onStartThenCancel struct {
	sessionID string
}

func (o onStartThenCancel) Run(_ context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	if req.OnStart != nil {
		req.OnStart(runtime.StartInfo{SessionID: o.sessionID})
	}
	return runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, runtime.ErrCanceled
}

// TestPlanningInterruptedFirstTurnResumes proves design section 7.4's
// "planning, first turn" row: a first turn cancelled after the start
// handshake (section 7.1) leaves the session's external id written (state
// SessionOpen, not idless), so the next tick resumes that same session
// free, carrying the interrupted input, rather than start over.
func TestPlanningInterruptedFirstTurnResumes(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)

	deps := claim(t, s, onStartThenCancel{sessionID: planFirstInterruptedSess}, ticketID)
	_, err := runPlanning(t, s, deps, ticketID) // first turn: interrupted after the start handshake
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	sess, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.ExternalID == nil || *sess.ExternalID != planFirstInterruptedSess {
		t.Fatalf("session external id = %v, want plan-first-interrupted (recorded at start, design section 7.1)", sess.ExternalID)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state = %v, want SessionOpen (external id already set, not idless)", state)
	}

	rt := readyScriptedRuntime(t, questionResult(response.JobPlanning, planFirstInterruptedSess))
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID) // resume: interrupted, free
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Errorf("commit.Session = %+v, want BumpResumes=false (an interrupted first turn resumes free)", commit.Session)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
	if rec.lastReq.SessionID != planFirstInterruptedSess {
		t.Errorf("resume RunRequest.SessionID = %q, want plan-first-interrupted (reused, not a new session)", rec.lastReq.SessionID)
	}
}

// TestPlanningInterruptedResumeIsFree proves design section 7.4's own
// "planning, resumes" row through maybeResumeValidationErrors: a validation
// errors resume normally charges (today's hardcoded true), but one cut
// short by InterruptRuns resumes free on the next tick, carrying the
// interrupted input next to the validation errors the pending marker still
// names.
func TestPlanningInterruptedResumeIsFree(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	badResp := readyResponse(validPlan("Store the ready cohort."), validClaims(), validScenarios(1, "bad"))
	firstRT := readyScriptedRuntime(t, readyStep(badResp, "interrupted-free-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID)
	if len(firstCommit.Messages) != 1 || !strings.HasPrefix(firstCommit.Messages[0].Body, "validation errors pending run ") {
		t.Fatalf("first commit.Messages = %+v, want one pending marker", firstCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps2 := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceledRT}}, ticketID)
	_, err := runPlanning(t, s, deps2, ticketID) // validation-errors resume: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps2.Owner, deps2.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	sess, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 1 {
		t.Fatalf("sessions.resumes after the canceled resume = %d, want 1 (charged at Reserve, design section 4.2)", sess.Resumes)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state = %v, want SessionOpen", state)
	}

	goodResp := &response.PlanningQuestionsResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeQuestion,
		Questions: []response.Question{{
			Key: "q1", Title: "Continue?", Body: testQuestionBody,
			Options: []response.Option{{Key: "a", Text: "Yes"}, {Key: "b", Text: "No"}}, Recommended: "a",
		}},
	}
	rec := &recordingRuntime{rt: readyScriptedRuntime(t, readyStep(goodResp, "interrupted-free-sess"))}
	resumeCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID) // resume: interrupted, free
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "need 2 to 30 scenarios") {
		t.Errorf("resume prompt does not also carry the validation errors:\n%s", rec.lastReq.Prompt)
	}
	if resumeCommit.Session == nil || resumeCommit.Session.BumpResumes {
		t.Errorf("resumeCommit.Session = %+v, want BumpResumes=false (the resume was not charged)", resumeCommit.Session)
	}
	apply(t, s, getTicket(t, s, ticketID), resumeCommit)

	sess, _, err = s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 1 {
		t.Errorf("sessions.resumes after the free interrupted resume = %d, want 1 (unchanged: the resume was not charged)", sess.Resumes)
	}
}

// bumpPlanningResumesBy issues n bare BumpResumes commits against
// sessionID, fast-forwarding its resumes counter without scripting n real
// planning turns (bumpResumesToCap's own shape, for fewer than maxResumes
// bumps).
func bumpPlanningResumesBy(t *testing.T, s *store.Store, ticketID, sessionID int64, n int, owner string, expires time.Time) {
	t.Helper()
	for range n {
		claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
		if err != nil || !claimed {
			t.Fatalf("bumpPlanningResumesBy: claim: claimed=%v err=%v", claimed, err)
		}
		applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Session: &store.SessionUpsert{ID: &sessionID, BumpResumes: true},
		})
		if err != nil || !applied {
			t.Fatalf("bumpPlanningResumesBy: CommitHandlerResult: applied=%v err=%v", applied, err)
		}
	}
}

// TestPlanningInterruptedResumeBypassesExhaustedCap proves design D5's own
// "ignores max_resumes" for planning's SessionExhausted branch
// (planningInterruptedFallback): a session driven to exactly max_resumes
// (12) by its own cancelled, interrupted final resume -- the cancelled
// resume's own charge at Reserve pushes sessions.resumes to the cap even
// though the run itself never finished -- still resumes on the next tick,
// free and with no resumes_exhausted escalation.
func TestPlanningInterruptedResumeBypassesExhaustedCap(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	firstRT := readyScriptedRuntime(t, invalidResult(reason, "cap-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: D14 invalid
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	sess, _, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	owner := "planning-cap-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpPlanningResumesBy(t, s, ticketID, sess.ID, 11, owner, expires) // resumes: 0 -> 11, one short of the cap

	_, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state before the final resume = %v, want SessionOpen", state)
	}

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceledRT}}, ticketID)
	_, err = runPlanning(t, s, deps, ticketID) // the D14 n==1 retry resume: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	sess, state, err = s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 12 {
		t.Fatalf("sessions.resumes after the canceled resume = %d, want 12 (charged at Reserve)", sess.Resumes)
	}
	if state != store.SessionExhausted {
		t.Fatalf("session state = %v, want SessionExhausted", state)
	}

	rec := &recordingRuntime{rt: readyScriptedRuntime(t, questionResult(response.JobPlanning, "cap-sess"))}
	resumeCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID) // next tick: resumes free, uncapped
	if err != nil {
		t.Fatalf("resume after the cap: %v", err)
	}
	if resumeCommit.Escalation != nil {
		t.Fatalf("resumeCommit.Escalation = %+v, want nil (no resumes_exhausted escalation)", resumeCommit.Escalation)
	}
	if resumeCommit.Session == nil || resumeCommit.Session.BumpResumes {
		t.Errorf("resumeCommit.Session = %+v, want BumpResumes=false (the resume bypasses the cap)", resumeCommit.Session)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
	// F011 / design section 7.5 bug 3: the interrupted run being resumed
	// here is itself the n==1 invalid-output retry (readyRT above returned
	// invalidResult(reason, ...) on the first turn), so the exhausted
	// fallback must also carry the invalid-retry text, not just the
	// interrupted input, or the model never learns why its last document
	// was rejected.
	if !strings.Contains(rec.lastReq.Prompt, reason) {
		t.Errorf("resume prompt does not carry the invalid-retry reason %q:\n%s", reason, rec.lastReq.Prompt)
	}
	apply(t, s, getTicket(t, s, ticketID), resumeCommit)

	sess, _, err = s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 12 {
		t.Errorf("sessions.resumes after the free resume = %d, want 12 (unchanged: the resume was not charged)", sess.Resumes)
	}
}

// TestPlanningInterruptedValidationErrorsResumeAtExhaustedCap proves F011's
// other shape of design section 7.5 bug 3 / D5 (design section 7.4): an
// exhausted session whose newest run is interrupted, and whose prior turn
// left a live "validation errors pending" marker (one scenario, failing the
// 2-to-30 shape check), resumes with both the validation-errors text and
// the interrupted input, free and uncapped, and writes the "validation
// errors delivered" marker so the errors are not sent a second time.
func TestPlanningInterruptedValidationErrorsResumeAtExhaustedCap(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	badResp := readyResponse(validPlan("Store the ready cohort."), validClaims(), validScenarios(1, "val-cap"))
	firstRT := readyScriptedRuntime(t, readyStep(badResp, "val-cap-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: shape error
	if len(firstCommit.Messages) != 1 || !strings.HasPrefix(firstCommit.Messages[0].Body, "validation errors pending run ") {
		t.Fatalf("first commit.Messages = %+v, want one pending marker", firstCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	sess, _, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	owner := "planning-val-cap-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpPlanningResumesBy(t, s, ticketID, sess.ID, 11, owner, expires) // resumes: 0 -> 11, one short of the cap

	_, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state before the final resume = %v, want SessionOpen", state)
	}

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceledRT}}, ticketID)
	_, err = runPlanning(t, s, deps, ticketID) // the validation-errors resume: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	sess, state, err = s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 12 {
		t.Fatalf("sessions.resumes after the canceled resume = %d, want 12 (charged at Reserve)", sess.Resumes)
	}
	if state != store.SessionExhausted {
		t.Fatalf("session state = %v, want SessionExhausted", state)
	}

	rec := &recordingRuntime{rt: readyScriptedRuntime(t, questionResult(response.JobPlanning, "val-cap-sess"))}
	resumeCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID) // next tick: resumes free, uncapped
	if err != nil {
		t.Fatalf("resume after the cap: %v", err)
	}
	if resumeCommit.Escalation != nil {
		t.Fatalf("resumeCommit.Escalation = %+v, want nil (no resumes_exhausted escalation)", resumeCommit.Escalation)
	}
	if resumeCommit.Session == nil || resumeCommit.Session.BumpResumes {
		t.Errorf("resumeCommit.Session = %+v, want BumpResumes=false (the resume bypasses the cap)", resumeCommit.Session)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "need 2 to 30 scenarios") {
		t.Errorf("resume prompt does not carry the validation errors:\n%s", rec.lastReq.Prompt)
	}
	var delivered int
	for _, m := range resumeCommit.Messages {
		if strings.HasPrefix(m.Body, "validation errors delivered run ") {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("resumeCommit.Messages = %+v, want exactly one delivered marker", resumeCommit.Messages)
	}
	apply(t, s, getTicket(t, s, ticketID), resumeCommit)

	sess, _, err = s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 12 {
		t.Errorf("sessions.resumes after the free resume = %d, want 12 (unchanged: the resume was not charged)", sess.Resumes)
	}
}

// TestGateConfirmInterruptedResumes proves design section 7.4's "gate
// confirming turn" row: the confirming turn's own free resume carries the
// interrupted input when its own cohort session's newest run was cut
// short.
func TestGateConfirmInterruptedResumes(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	_, runID := seedCohort(t, s, ticketID, validPlan("Interrupted confirm."), validScenarios(2, "confirm-interrupt"))

	qID := seedGateQuestion(t, s, ticketID, &runID)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps := claim(t, s, canceledRT, ticketID)
	_, err := runPlanning(t, s, deps, ticketID) // confirming turn: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	rt := &scriptedRuntime{t: t, steps: []scriptedStep{confirmedResult("confirm-interrupt-sess-2", "All settled.")}}
	rec := &recordingRuntime{rt: rt}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID) // confirming turn again: free resume
	if err != nil {
		t.Fatalf("gate confirm resume: %v", err)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Errorf("commit.Session = %+v, want BumpResumes=false (already free, design section 22.12.3)", commit.Session)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("confirming turn prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
}

// ---- design section 7.5 bug 3: the planning invalid-retry stall -----------

// TestPlanningReconciledInvalidRetryStallResumes reproduces design section
// 7.5 bug 3: the D14 n==1 retry turn (the resume a first-turn invalid
// output triggers) is itself cut short and reconciled by ExpireClaims
// (interrupted=0, a plain lease expiry, not InterruptRuns). Before the fix,
// ConsecutiveInvalidOutputs' own walk (internal/store/planning_reads.go)
// stops on that response-less run uncounted, n comes back 0, and with
// nothing else pending the handler stalls on ErrNoAction forever. The fix
// (maybeResumeStalledInvalidRetry, planning.go) rebuilds the original
// invalid-retry text from the run that actually triggered the chain and
// resumes with it, charged and cap-gated exactly as today's ordinary D14
// retry, since this run was reconciled, not interrupted.
func TestPlanningReconciledInvalidRetryStallResumes(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	firstRT := readyScriptedRuntime(t, invalidResult(reason, "stall-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: D14 invalid
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps2 := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceledRT}}, ticketID)
	_, err := runPlanning(t, s, deps2, ticketID) // the D14 n==1 retry resume: cut short
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	// A plain lease-expiry reconcile (interrupted stays 0), not
	// InterruptRuns: section 7.5 bug 3's own "reconciled" row.
	if _, expireErr := s.ExpireClaims(t.Context(), time.Now().Add(20*time.Minute), ""); expireErr != nil {
		t.Fatalf("ExpireClaims: %v", expireErr)
	}

	_, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state = %v, want SessionOpen", state)
	}

	rec := &recordingRuntime{rt: readyScriptedRuntime(t, questionResult(response.JobPlanning, "stall-sess"))}
	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID) // must resume, not stall on ErrNoAction
	if err != nil {
		t.Fatalf("resume after the reconciled stall: %v (the bug: ErrNoAction forever)", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, reason) {
		t.Errorf("resume prompt does not carry the original invalid-retry reason:\n%s", rec.lastReq.Prompt)
	}
	if strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt carries the interrupted input, want none (this run was reconciled, not interrupted):\n%s", rec.lastReq.Prompt)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)

	// The terminal commit's own Session always carries BumpResumes=false
	// (Reserve already charged this resume when it reserved the run, same
	// comment as planning.go's own terminalizing commits); the resumes
	// count itself is where "charged" actually shows.
	sess, _, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.Resumes != 2 {
		t.Errorf("sessions.resumes after the reconciled retry's own resume = %d, want 2 (charged at Reserve for both the original n==1 retry and this one)", sess.Resumes)
	}
}

// TestPlanningReconciledInvalidRetryStallEscalatesOnSecondInvalid proves PR
// review fix F1: maybeResumeStalledInvalidRetry's own resume now passes
// priorInvalid=1 (it found the preceding invalid marker, so this resume is
// itself the D14 retry turn), so a recovered retry that is invalid again
// still escalates response_invalid through D14's two-strike rule, instead
// of silently resetting the chain and retrying forever (the same stall
// TestPlanningReconciledInvalidRetryStallResumes above proves is otherwise
// fixed, but only for a retry that recovers into something valid).
func TestPlanningReconciledInvalidRetryStallEscalatesOnSecondInvalid(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	firstRT := readyScriptedRuntime(t, invalidResult(reason, "stall-escalate-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: D14 invalid
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps2 := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceledRT}}, ticketID)
	_, err := runPlanning(t, s, deps2, ticketID) // the D14 n==1 retry resume: cut short
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	// A plain lease-expiry reconcile (interrupted stays 0), same as
	// TestPlanningReconciledInvalidRetryStallResumes above.
	if _, expireErr := s.ExpireClaims(t.Context(), time.Now().Add(20*time.Minute), ""); expireErr != nil {
		t.Fatalf("ExpireClaims: %v", expireErr)
	}

	// The recovered retry is invalid again: the second consecutive invalid
	// output on this chain.
	again := readyScriptedRuntime(t, invalidResult(reason, "stall-escalate-sess"))
	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, again, ticketID), ticketID)
	if err != nil {
		t.Fatalf("resume after the reconciled stall: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want response_invalid (second consecutive invalid output)")
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(mustEscalationPayload(t, commit), &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
	}
	if payload.Code != testCodeResponseInvalid {
		t.Errorf("payload.Code = %q, want response_invalid", payload.Code)
	}
}

// TestPlanningTwiceInterruptedInvalidRetryKeepsReason proves PR review fix
// F2: priorInvalidReason now walks back past interrupted runs in the same
// resume chain (job.go's priorNonInterruptedRun), so a D14 retry that is
// itself interrupted a second time in a row still finds the original
// invalid-retry text on the run that actually carries its marker, rather
// than landing on its own interrupted predecessor (which carries no
// marker of its own) and losing it.
func TestPlanningTwiceInterruptedInvalidRetryKeepsReason(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	firstRT := readyScriptedRuntime(t, invalidResult(reason, "twice-interrupted-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: D14 invalid (R1, carries the marker)
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	canceled := func() *scriptedRuntime {
		return &scriptedRuntime{t: t, steps: []scriptedStep{
			{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
		}}
	}

	// D14's own n==1 retry (R2): interrupted mid-flight, before it ever
	// answers (no marker of its own).
	deps2 := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceled()}}, ticketID)
	if _, err := runPlanning(t, s, deps2, ticketID); !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("R2 err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}
	applied, err := s.InterruptRuns(t.Context(), ticketID, deps2.Owner, deps2.Expires)
	if err != nil || !applied {
		t.Fatalf("InterruptRuns (R2): applied=%v err=%v, want applied=true, err=nil", applied, err)
	}

	// R2's own free resume (R3): interrupted again, also before it ever
	// answers -- the twice-interrupted chain this fix is for.
	deps3 := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceled()}}, ticketID)
	if _, r3Err := runPlanning(t, s, deps3, ticketID); !errors.Is(r3Err, runtime.ErrCanceled) {
		t.Fatalf("R3 err = %v, want errors.Is(err, runtime.ErrCanceled)", r3Err)
	}
	applied, err = s.InterruptRuns(t.Context(), ticketID, deps3.Owner, deps3.Expires)
	if err != nil || !applied {
		t.Fatalf("InterruptRuns (R3): applied=%v err=%v, want applied=true, err=nil", applied, err)
	}

	// The next tick's own free resume (R4) must still carry the original
	// invalid-retry reason, found by walking back past both interrupted
	// runs (R3, then R2) to R1, the run that actually carries the marker.
	rec := &recordingRuntime{rt: readyScriptedRuntime(t, questionResult(response.JobPlanning, "twice-interrupted-sess"))}
	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("resume after the twice-interrupted retry: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, reason) {
		t.Errorf("resume prompt does not carry the original invalid-retry reason:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Errorf("commit.Session = %+v, want BumpResumes=false (genuinely interrupted, so free)", commit.Session)
	}
}

// TestPlanningReconciledInvalidRetryStaysCapped proves section 7.5 bug 3's
// own exhausted-session guard: a session already at max_resumes, whose
// newest run is a reconciled (not interrupted) D14 retry, still escalates
// resumes_exhausted through today's cap path -- it does not resume, since
// resumeCharge's own gate stays true for a reconciled run.
func TestPlanningReconciledInvalidRetryStaysCapped(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	firstRT := readyScriptedRuntime(t, invalidResult(reason, "capped-invalid-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: D14 invalid
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	sess, _, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	owner := "planning-reconciled-cap-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpPlanningResumesBy(t, s, ticketID, sess.ID, 12, owner, expires) // resumes: 0 -> 12 (the cap)

	_, state, err := s.LatestSession(t.Context(), ticketID, "planning", 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if state != store.SessionExhausted {
		t.Fatalf("session state = %v, want SessionExhausted", state)
	}

	commit := mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID) // cap tick: must escalate, not resume
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want resumes_exhausted")
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(mustEscalationPayload(t, commit), &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
	}
	if payload.Code != testCodeResumesExhausted || payload.Origin != testOriginCapResumes {
		t.Errorf("payload = (Code=%q, Origin=%q), want (resumes_exhausted, cap_resumes)", payload.Code, payload.Origin)
	}
}

// TestPlanningInterruptedInvalidRetryResumesWithBothInputs proves section
// 7.5 bug 3's own "interrupted" row: when the D14 retry turn that stalls is
// genuinely interrupted (InterruptRuns, not a plain lease-expiry reconcile),
// the next tick's own free resume carries both the interrupted input and
// the original invalid-retry text, side by side.
func TestPlanningInterruptedInvalidRetryResumesWithBothInputs(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	fake := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, fake, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, fake, ticketID), ticketID)) // classify

	const reason = "zing document failed validation"
	firstRT := readyScriptedRuntime(t, invalidResult(reason, "both-sess"))
	firstCommit := mustPlanning(t, s, claimWithRuntimes(t, s, firstRT, ticketID), ticketID) // first turn: D14 invalid
	apply(t, s, getTicket(t, s, ticketID), firstCommit)

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps2 := claimWithRuntimes(t, s, byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{response.JobPlanning: canceledRT}}, ticketID)
	_, err := runPlanning(t, s, deps2, ticketID) // the D14 n==1 retry resume: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps2.Owner, deps2.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	rec := &recordingRuntime{rt: readyScriptedRuntime(t, questionResult(response.JobPlanning, "both-sess"))}
	commit, err := runPlanning(t, s, claimWithRuntimes(t, s, rec, ticketID), ticketID) // must resume, free, with both inputs
	if err != nil {
		t.Fatalf("resume after the interrupted retry: %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, reason) {
		t.Errorf("resume prompt does not carry the original invalid-retry reason:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, wantInterruptedText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", rec.lastReq.Prompt)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Errorf("commit.Session = %+v, want BumpResumes=false (genuinely interrupted, so free)", commit.Session)
	}
}

// TestPlanReviewInterruptedRerunsFresh proves design section 7.4's
// planreview row ("keep fresh re-runs"): runPlanReview's own SessionUpsert
// never carries a session id to resume (the D14 chain walks by job alone,
// unlike planning's own session-scoped check), so an interrupted review
// tick's next attempt runs an entirely fresh planreview turn rather than
// try to resume the interrupted session.
func TestPlanReviewInterruptedRerunsFresh(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedFeatureTicketInPlanning(t, s)
	seedCohort(t, s, ticketID, validPlan("Interrupted review tick."), validScenarios(2, "interrupted-review"))

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps := claim(t, s, canceledRT, ticketID)
	_, err := runPlanning(t, s, deps, ticketID) // review tick: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticketID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	freshRT := byJobRuntime{t: t, byJob: map[response.Job]runtime.Runtime{
		response.JobPlanreview: &scriptedRuntime{t: t, steps: []scriptedStep{readyStep(findingsResponse(), "planreview-fresh-sess")}},
	}}
	commit := mustPlanning(t, s, claimWithRuntimes(t, s, freshRT, ticketID), ticketID) // review tick again: must run fresh
	if commit.Session == nil || commit.Session.ExternalID == nil || *commit.Session.ExternalID != "planreview-fresh-sess" {
		t.Fatalf("commit.Session = %+v, want a fresh session with external id planreview-fresh-sess", commit.Session)
	}
}
