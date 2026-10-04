package job

import (
	"strings"
	"testing"

	"zing/internal/runtime"
)

// testReasonFailedValidation mirrors runtime's closed reasonFailedValidation
// string, used by several tests in this file (goconst).
const testReasonFailedValidation = "zing document failed validation"

// TestInvalidRetryCarriesFencedDetail is a regression test for a live
// planning ticket: a plan with the vague word "large" failed validation
// twice in a row, because the retry said only "zing document failed
// validation". The marker now keeps the validator's errors on its third
// line, and the retry prompt shows them inside a fence.
func TestInvalidRetryCarriesFencedDetail(t *testing.T) {
	t.Parallel()
	invErr := &runtime.InvalidOutputError{
		Reason: testReasonFailedValidation,
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
	open := strings.Index(got, "<<<UNTRUSTED ")
	detail := strings.Index(got, `vague word "large"`)
	end := strings.Index(got, "<<<END ")
	if open < 0 || detail < open || end < detail {
		t.Errorf("detail must sit between the fence markers, got %q", got)
	}
	if strings.Contains(invalidRetryText("no zing element in final message"), "<<<UNTRUSTED") {
		t.Error("a closed reason with no detail must not add a fence")
	}
}

// TestInvalidMarkerBody_KeepsErrorsOnePerLine proves invalidMarkerBody no
// longer flattens Detail's newlines into spaces (Q1: each validator error
// now lives on its own line, up to 64 KiB).
func TestInvalidMarkerBody_KeepsErrorsOnePerLine(t *testing.T) {
	t.Parallel()

	invErr := &runtime.InvalidOutputError{
		Reason: testReasonFailedValidation,
		Detail: "a: x\nb: y\nc: z",
	}
	got := invalidMarkerBody(5, invErr)
	want := "response invalid run 5\n" + testReasonFailedValidation + "\na: x\nb: y\nc: z"
	if got != want {
		t.Errorf("invalidMarkerBody = %q, want %q", got, want)
	}

	noDetail := &runtime.InvalidOutputError{Reason: testReasonFailedValidation}
	got = invalidMarkerBody(5, noDetail)
	want = "response invalid run 5\n" + testReasonFailedValidation
	if got != want {
		t.Errorf("invalidMarkerBody with no detail = %q, want %q", got, want)
	}
}

// TestInvalidRetryText_MultiLineDetail proves a marker reason with
// multiple error lines reaches the retry prompt's fence intact.
func TestInvalidRetryText_MultiLineDetail(t *testing.T) {
	t.Parallel()

	reason := testReasonFailedValidation + "\na: x\nb: y\nc: z"
	got := invalidRetryText(reason)
	if !strings.Contains(got, "a: x\nb: y\nc: z") {
		t.Errorf("invalidRetryText = %q, want it to contain the newline-joined sequence %q", got, "a: x\nb: y\nc: z")
	}
	open := strings.Index(got, "<<<UNTRUSTED ")
	end := strings.Index(got, "<<<END ")
	if open < 0 || end < open {
		t.Fatalf("invalidRetryText = %q, want a fenced block", got)
	}
	fenced := got[open:end]
	if !strings.Contains(fenced, "a: x\nb: y\nc: z") {
		t.Errorf("fenced block = %q, want it to contain the newline-joined sequence %q", fenced, "a: x\nb: y\nc: z")
	}
}
