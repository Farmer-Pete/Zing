package job

// shared_internal_test.go tests shared.go's own unexported functions that no
// seam in planning_test.go (package job_test) can reach.

import (
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

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
