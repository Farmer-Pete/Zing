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
	"zing/internal/store"
)

// TestGateQuestionMessage_StatesWhatApproveDoes proves F013: the gate
// question's body carries a plain-language paragraph explaining what
// approving does, not just the bare plan objective, while leaving the two
// chip options unchanged.
func TestGateQuestionMessage_StatesWhatApproveDoes(t *testing.T) {
	t.Parallel()
	const objective = "Add a hello endpoint so a caller can get a plain-text greeting back over HTTP."
	msg, err := gateQuestionMessage(1, objective, false)
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

// TestGateQuestionMessage_LoopsExhaustedUsesDifferentExplainsText proves
// issue #48's gate text split: loopsExhausted false keeps today's
// clean-review explains text ("already fixed automatically"); loopsExhausted
// true swaps in the cap-reached explains text instead, and the two bodies
// never share that phrase.
func TestGateQuestionMessage_LoopsExhaustedUsesDifferentExplainsText(t *testing.T) {
	t.Parallel()
	const objective = "Loop exhausted on floor-only findings."

	tests := []struct {
		name           string
		loopsExhausted bool
		wantContains   string
		wantAbsent     string
	}{
		{"clean review", false, "already fixed automatically", "max_loops"},
		{"loops exhausted", true, "max_loops", "already fixed automatically"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg, err := gateQuestionMessage(1, objective, tt.loopsExhausted)
			if err != nil {
				t.Fatalf("gateQuestionMessage: %v", err)
			}
			if !strings.Contains(msg.Body, tt.wantContains) {
				t.Errorf("message body = %q, want it to contain %q", msg.Body, tt.wantContains)
			}
			if strings.Contains(msg.Body, tt.wantAbsent) {
				t.Errorf("message body = %q, want it to not contain %q", msg.Body, tt.wantAbsent)
			}
		})
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

// TestRenderAnswerText_MarksARevisedAnswer proves D30: when a question
// carries more than one sent answer (the console now lets an
// already-answered question take a revised draft while the ticket still
// waits), the first reads as an ordinary pick and every later one is
// marked "(revised)", so the resume prompt reads it as superseding the
// pick(s) before it rather than a second, unrelated choice.
func TestRenderAnswerText_MarksARevisedAnswer(t *testing.T) {
	t.Parallel()
	qp := response.QuestionPayload{
		Key: "Q1", Options: []response.Option{{Key: "a", Text: "Plain hello"}, {Key: "b", Text: "hello, world"}},
	}
	q := store.MessageRow{Message: store.Message{Body: "Q1\n\nHow should the greeting read?"}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped

	firstPayload, err := json.Marshal(response.AnswerPayload{Option: new("a")})
	if err != nil {
		t.Fatalf("marshal first answer payload: %v", err)
	}
	revisedPayload, err := json.Marshal(response.AnswerPayload{Option: new("b")})
	if err != nil {
		t.Fatalf("marshal revised answer payload: %v", err)
	}
	answers := []store.MessageRow{
		{Message: store.Message{Payload: firstPayload}},   //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{Message: store.Message{Payload: revisedPayload}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
	}

	got := renderAnswerText(q, qp, answers, nil)
	wantFirst := "chose a: Plain hello\n"
	wantRevised := "chose b: hello, world (revised)\n"
	if !strings.Contains(got, wantFirst) {
		t.Errorf("renderAnswerText missing the first, unmarked pick %q; got:\n%s", wantFirst, got)
	}
	if !strings.Contains(got, wantRevised) {
		t.Errorf("renderAnswerText missing the (revised) second pick %q; got:\n%s", wantRevised, got)
	}
}
