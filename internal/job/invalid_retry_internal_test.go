package job

import (
	"strings"
	"testing"

	"zing/internal/runtime"
)

// TestInvalidRetryCarriesFencedDetail is a regression test for a live
// planning ticket: a plan with the vague word "large" failed validation
// twice in a row, because the retry said only "zing document failed
// validation". The marker now keeps the validator's errors on its third
// line, and the retry prompt shows them inside a fence.
func TestInvalidRetryCarriesFencedDetail(t *testing.T) {
	t.Parallel()
	invErr := &runtime.InvalidOutputError{
		Reason: "zing document failed validation",
		Detail: `plan/review/risks/risk[3]: vague word "large"; give a concrete threshold`,
	}
	body := invalidMarkerBody(80, invErr)
	_, reason, _ := strings.Cut(strings.TrimPrefix(body, "response invalid run "), "\n")

	got := invalidRetryText(reason)
	for _, want := range []string{"zing document failed validation; return exactly one", `vague word "large"`, "<<<UNTRUSTED ", "<<<END "} {
		if !strings.Contains(got, want) {
			t.Errorf("invalidRetryText = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(invalidRetryText("no zing element in final message"), "<<<UNTRUSTED") {
		t.Error("a closed reason with no detail must not add a fence")
	}
}
