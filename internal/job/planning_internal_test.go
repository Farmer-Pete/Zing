package job

// planning_internal_test.go tests planning.go's own unexported functions
// that no seam in planning_test.go (package job_test) can reach -- the same
// reason runjob_test.go lives in package job instead of alongside it (see
// that file's own comment).

import (
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/response"
)

// TestGateQuestionMessage_StatesWhatApproveDoes proves F013: the gate
// question's body carries a plain-language paragraph explaining what
// approving does, not just the bare plan objective, while leaving the two
// chip options unchanged.
func TestGateQuestionMessage_StatesWhatApproveDoes(t *testing.T) {
	const objective = "Add a hello endpoint so a caller can get a plain-text greeting back over HTTP."
	msg, err := gateQuestionMessage(1, objective)
	if err != nil {
		t.Fatalf("gateQuestionMessage: %v", err)
	}
	if !strings.HasPrefix(msg.Body, objective+"\n\n") {
		t.Fatalf("message body = %q, want it to start with the objective, a blank line, then what Approve does", msg.Body)
	}
	if !strings.Contains(msg.Body, "seals") {
		t.Errorf("message body = %q, want it to say approve seals the scenario set", msg.Body)
	}
	if !strings.Contains(msg.Body, "cannot be undone") {
		t.Errorf("message body = %q, want it to say this cannot be undone", msg.Body)
	}

	var payload response.QuestionPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if payload.Kind != response.QuestionKindGate {
		t.Errorf("payload.Kind = %q, want gate", payload.Kind)
	}
	wantOptions := []response.Option{{Key: gateOptionApprove, Text: "Approve"}, {Key: gateOptionReject, Text: "Reject"}}
	if len(payload.Options) != len(wantOptions) {
		t.Fatalf("payload.Options = %+v, want %+v", payload.Options, wantOptions)
	}
	for i, o := range wantOptions {
		if payload.Options[i] != o {
			t.Errorf("payload.Options[%d] = %+v, want %+v", i, payload.Options[i], o)
		}
	}
}

// TestPostRunFailedWhatFor covers postRunFailedWhatFor's own table (design
// section 4.1): the four origins runAndRoute already threads through
// postRunFailure, the three origins Package 8 adds ahead of their own
// callers, the three origins Package 9 adds the same way, and an unknown
// origin's fallback sentence.
func TestPostRunFailedWhatFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		origin response.EscalationOrigin
		want   string
	}{
		{response.EscalationOriginClassify, "classifying the ticket"},
		{response.EscalationOriginPlanningFirst, "storing or checking the plan"},
		{response.EscalationOriginPlanningResume, "storing or checking the plan"},
		{response.EscalationOriginPlanreview, "storing the plan review"},
		{response.EscalationOriginBuild, "storing or checking the build result"},
		{response.EscalationOriginFix, "storing or checking the build result"},
		{response.EscalationOriginPerimeter, "storing the perimeter description"},
		{response.EscalationOriginReview, "storing the review findings"},
		{response.EscalationOriginJudge, "storing or checking the verdicts"},
		{response.EscalationOriginRespond, "storing the thread actions"},
		{response.EscalationOriginSeal, "storing or checking the agent's result"},
	}
	for _, tt := range tests {
		t.Run(string(tt.origin), func(t *testing.T) {
			t.Parallel()
			if got := postRunFailedWhatFor(tt.origin); got != tt.want {
				t.Errorf("postRunFailedWhatFor(%s) = %q, want %q", tt.origin, got, tt.want)
			}
		})
	}
}
