// planning_cap_test.go tests ticket 66: plan review's cap_loops
// loops_exhausted escalation adds option d, "Accept the plan and go to the
// gate" (maybeResumeFloorFindings's ExtraOptions/Recommended,
// enterFromEscalationRound's capLoops row, and acceptPlanAtCap). It reuses
// skeleton_test.go's and planning_test.go's shared fixtures
// (newJobTestStore, seedFeatureTicketInPlanning, seedCohort, validPlan,
// validScenarios, finding, seedPlanreviewArtifact, insertUpdateMarker,
// claim, apply, getTicket, runPlanning, scriptedRuntime, recordingRuntime,
// questionResult) and gate_test.go's answerGateQuestion, and drives
// job.Registry()["planning"].Run directly, exactly as those files' own
// tests do.
package job_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// seedCapLoopsEscalation seeds a feature ticket whose cohort has reached
// plan review's own loop cap (CountDeliveredReviews already at
// machine.toml's max_loops) with a stored planreview artifact carrying one
// at-or-below-floor minor finding ("still wrong") and one above-floor major
// finding ("worse"), drives planning once (producing the cap_loops
// loops_exhausted escalation), and returns the store, the ticket id, the
// escalation commit it already applied against s, and the escalation's own
// linked question row.
func seedCapLoopsEscalation(t *testing.T, objective string) (s *store.Store, ticketID int64, escCommit store.HandlerCommit, q store.MessageRow) {
	t.Helper()
	s = newJobTestStore(t)
	ticketID = seedFeatureTicketInPlanning(t, s)
	planVersion, runID := seedCohort(t, s, ticketID, validPlan(objective), validScenarios(2, "caploops"))

	insertUpdateMarker(t, s, ticketID, "planreview v1 delivered")
	insertUpdateMarker(t, s, ticketID, "planreview v2 delivered")

	minor := finding(response.SeverityMinor, "plan/design/shape", "still wrong", "fix it")
	major := finding(response.SeverityMajor, "plan/design/other", "worse", "fix that too")
	seedPlanreviewArtifact(t, s, ticketID, planVersion, runID, minor, major)
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("planreview v%d pending", planVersion))

	commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
	if err != nil {
		t.Fatalf("seedCapLoopsEscalation: planning Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatalf("seedCapLoopsEscalation: commit.Escalation = %+v, want loops_exhausted", commit.Escalation)
	}
	apply(t, s, getTicket(t, s, ticketID), commit)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one (the escalation's own linked question)", open, err)
	}
	return s, ticketID, commit, open[0]
}

// TestPlanCapLoops_EscalationOffersPlanGate proves maybeResumeFloorFindings'
// own ExtraOptions/Recommended addition: the cap_loops loops_exhausted
// question offers exactly a, b, d, c, option d's text is "Accept the plan
// and go to the gate", and it is the recommended option.
func TestPlanCapLoops_EscalationOffersPlanGate(t *testing.T) {
	t.Parallel()
	_, _, escCommit, q := seedCapLoopsEscalation(t, "Cap loops offers the gate option.")

	if escCommit.Escalation.Payload.Code != string(response.EscalationCodeLoopsExhausted) {
		t.Errorf("Escalation.Payload.Code = %q, want %q", escCommit.Escalation.Payload.Code, response.EscalationCodeLoopsExhausted)
	}
	if escCommit.Escalation.Payload.Origin != string(response.EscalationOriginCapLoops) {
		t.Errorf("Escalation.Payload.Origin = %q, want %q", escCommit.Escalation.Payload.Origin, response.EscalationOriginCapLoops)
	}

	var payload response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	wantKeys := []string{"a", "b", "d", "c"}
	gotKeys := make([]string, len(payload.Options))
	for i, o := range payload.Options {
		gotKeys[i] = o.Key
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("question option keys = %v, want %v", gotKeys, wantKeys)
	}
	for _, o := range payload.Options {
		if o.Key == "d" && o.Text != "Accept the plan and go to the gate" {
			t.Errorf("option d text = %q, want %q", o.Text, "Accept the plan and go to the gate")
		}
	}
	if payload.Recommended != "d" {
		t.Errorf("payload.Recommended = %q, want %q", payload.Recommended, "d")
	}
}

// TestPlanCapLoops_AcceptPostsGate proves acceptPlanAtCap end to end: the
// owner's own d pick on the cap_loops escalation posts the gate (with the
// owner-chose explains text) plus gateCapMarker, resolves the escalation
// round, and makes no runtime call.
func TestPlanCapLoops_AcceptPostsGate(t *testing.T) {
	t.Parallel()
	const objective = "Cap loops, owner accepts and goes to the gate."
	s, ticketID, _, q := seedCapLoopsEscalation(t, objective)

	answerGateQuestion(t, s, ticketID, q.ID, new("d"), "")

	commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning Run (accept d): %v", err)
	}

	if commit.Escalation != nil {
		t.Errorf("commit.Escalation = %+v, want nil", commit.Escalation)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want %q", commit.Next, "")
	}
	if commit.Waiting == nil || *commit.Waiting != testWaitingGate {
		t.Fatalf("commit.Waiting = %v, want gate", commit.Waiting)
	}
	if want := []int64{q.ID}; !reflect.DeepEqual(commit.ResolveQuestions, want) {
		t.Errorf("commit.ResolveQuestions = %v, want %v", commit.ResolveQuestions, want)
	}
	if len(commit.Messages) != 2 {
		t.Fatalf("commit.Messages = %+v, want exactly 2 (the gate question, then the cap marker)", commit.Messages)
	}
	msg := commit.Messages[0]
	if !strings.HasPrefix(msg.Body, objective+"\n\n") {
		t.Errorf("gate message body = %q, want it to start with the objective %q", msg.Body, objective)
	}
	if !strings.Contains(msg.Body, "you chose to see this gate anyway") {
		t.Errorf("gate message body = %q, want it to say the owner chose to see this gate anyway", msg.Body)
	}
	var qp response.QuestionPayload
	if err = json.Unmarshal(msg.Payload, &qp); err != nil {
		t.Fatalf("unmarshal gate question payload: %v", err)
	}
	if qp.Kind != response.QuestionKindGate {
		t.Errorf("gate question Kind = %q, want gate", qp.Kind)
	}
	cohort, ok, err := s.CurrentCohort(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("CurrentCohort: %+v, ok=%v, %v", cohort, ok, err)
	}
	wantMarker := fmt.Sprintf("gate cap reached plan v%d", cohort.PlanVersion)
	if commit.Messages[1].Type != testMsgTypeUpdate || commit.Messages[1].Body != wantMarker {
		t.Errorf("commit.Messages[1] = %+v, want an update marker %q", commit.Messages[1], wantMarker)
	}

	apply(t, s, getTicket(t, s, ticketID), commit)
	after := getTicket(t, s, ticketID)
	if after.State != testStatePlanning || after.WaitingOn == nil || *after.WaitingOn != testWaitingGate {
		t.Fatalf("after accept: ticket = (state=%q, waiting_on=%v), want (planning, gate)", after.State, after.WaitingOn)
	}
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %+v, want exactly one (the gate)", open)
	}
	var openPayload response.QuestionPayload
	if err := json.Unmarshal(open[0].Payload, &openPayload); err != nil {
		t.Fatalf("unmarshal open question payload: %v", err)
	}
	if openPayload.Kind != response.QuestionKindGate {
		t.Errorf("the only open question's Kind = %q, want gate", openPayload.Kind)
	}
}

// TestPlanCapLoops_AcceptGateListsEveryFinding proves acceptPlanAtCap lists
// every stored planreview finding, above-floor included, on the owner-chose
// gate, in stored order after the owner-chose text and a blank line.
func TestPlanCapLoops_AcceptGateListsEveryFinding(t *testing.T) {
	t.Parallel()
	s, ticketID, _, q := seedCapLoopsEscalation(t, "Cap loops, gate lists every finding.")

	answerGateQuestion(t, s, ticketID, q.ID, new("d"), "")

	commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning Run (accept d): %v", err)
	}
	if len(commit.Messages) == 0 {
		t.Fatalf("commit.Messages = empty, want the gate question first")
	}
	body := commit.Messages[0].Body
	wantMinor := "- minor plan/design/shape still wrong"
	wantMajor := "- major plan/design/other worse"
	minorIdx := strings.Index(body, wantMinor)
	majorIdx := strings.Index(body, wantMajor)
	if minorIdx < 0 || majorIdx < 0 {
		t.Fatalf("gate message body = %q, want it to contain %q and %q", body, wantMinor, wantMajor)
	}
	if minorIdx > majorIdx {
		t.Errorf("gate message body lists the major finding before the minor one, want stored order:\n%s", body)
	}
	if !strings.Contains(body, "you chose to see this gate anyway.\n\n"+wantMinor) {
		t.Errorf("gate message body = %q, want the list after the owner-chose text and a blank line", body)
	}
}

// TestPlanCapLoops_RetryBackAbandonUnchanged proves options a, b, and c on
// the cap_loops loops_exhausted question behave exactly as they did before
// ticket 66 added option d: no subtest posts a gate.
func TestPlanCapLoops_RetryBackAbandonUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("a: retry resumes with floor findings and the note", func(t *testing.T) {
		t.Parallel()
		s, ticketID, _, q := seedCapLoopsEscalation(t, "Cap loops retry.")
		answerGateQuestion(t, s, ticketID, q.ID, new("a"), "keep going")

		rec := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "cap-retry-sess")}}}
		commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
		if err != nil {
			t.Fatalf("planning Run (retry): %v", err)
		}
		if commit.Waiting != nil && *commit.Waiting == testWaitingGate {
			t.Errorf("commit.Waiting = %q, want not gate", *commit.Waiting)
		}
		for _, m := range commit.Messages {
			var qp response.QuestionPayload
			if json.Unmarshal(m.Payload, &qp) == nil && qp.Kind == response.QuestionKindGate {
				t.Errorf("commit.Messages carries a gate-kind question, want none")
			}
		}
		if !strings.Contains(rec.lastReq.Prompt, "still wrong") {
			t.Errorf("resume prompt does not carry the floor finding:\n%s", rec.lastReq.Prompt)
		}
		if !strings.Contains(rec.lastReq.Prompt, "keep going") {
			t.Errorf("resume prompt does not carry the owner's note:\n%s", rec.lastReq.Prompt)
		}
		if strings.Contains(rec.lastReq.Prompt, "raise machine.toml") {
			t.Errorf("resume prompt carries the error label, want none on a retry:\n%s", rec.lastReq.Prompt)
		}
	})

	t.Run("b: back to planning resumes with notes and the error", func(t *testing.T) {
		t.Parallel()
		s, ticketID, _, q := seedCapLoopsEscalation(t, "Cap loops back to planning.")
		answerGateQuestion(t, s, ticketID, q.ID, new("b"), "back off")

		rec := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "cap-back-sess")}}}
		commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
		if err != nil {
			t.Fatalf("planning Run (back): %v", err)
		}
		if commit.Waiting != nil && *commit.Waiting == testWaitingGate {
			t.Errorf("commit.Waiting = %q, want not gate", *commit.Waiting)
		}
		if !strings.Contains(rec.lastReq.Prompt, "back off") {
			t.Errorf("resume prompt does not carry the owner's note:\n%s", rec.lastReq.Prompt)
		}
		if !strings.Contains(rec.lastReq.Prompt, "raise machine.toml") {
			t.Errorf("resume prompt does not carry the error:\n%s", rec.lastReq.Prompt)
		}
		if strings.Contains(rec.lastReq.Prompt, "still wrong") {
			t.Errorf("resume prompt carries the floor finding, want no findings label on back:\n%s", rec.lastReq.Prompt)
		}
	})

	t.Run("c: abandon", func(t *testing.T) {
		t.Parallel()
		s, ticketID, _, q := seedCapLoopsEscalation(t, "Cap loops abandon.")
		answerGateQuestion(t, s, ticketID, q.ID, new("c"), "")

		commit, err := runPlanning(t, s, claim(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
		if err != nil {
			t.Fatalf("planning Run (abandon): %v", err)
		}
		if commit.Next != testStateAbandoned {
			t.Errorf("commit.Next = %q, want %q", commit.Next, testStateAbandoned)
		}
	})
}

// TestPlanCapLoops_ReplyOnlyResolvesAsRetry proves owner decision Q1: a
// reply with no chip on the cap_loops loops_exhausted question resolves as
// Retry, not as the question's own recommended d, since a note is for the
// planner and the gate cannot act on it.
func TestPlanCapLoops_ReplyOnlyResolvesAsRetry(t *testing.T) {
	t.Parallel()
	s, ticketID, _, q := seedCapLoopsEscalation(t, "Cap loops reply-only resolves as retry.")
	answerGateQuestion(t, s, ticketID, q.ID, nil, "try splitting the task")

	rec := &recordingRuntime{rt: &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "cap-reply-only-sess")}}}
	commit, err := runPlanning(t, s, claim(t, s, rec, ticketID), ticketID)
	if err != nil {
		t.Fatalf("planning Run (reply-only): %v", err)
	}
	if !strings.Contains(rec.lastReq.Prompt, "still wrong") {
		t.Errorf("resume prompt does not carry the floor finding:\n%s", rec.lastReq.Prompt)
	}
	if !strings.Contains(rec.lastReq.Prompt, "try splitting the task") {
		t.Errorf("resume prompt does not carry the owner's note:\n%s", rec.lastReq.Prompt)
	}
	if commit.Waiting != nil && *commit.Waiting == testWaitingGate {
		t.Errorf("commit.Waiting = %q, want not gate", *commit.Waiting)
	}
	for _, m := range commit.Messages {
		var qp response.QuestionPayload
		if json.Unmarshal(m.Payload, &qp) == nil && qp.Kind == response.QuestionKindGate {
			t.Errorf("commit.Messages carries a gate-kind question, want none")
		}
	}
}
