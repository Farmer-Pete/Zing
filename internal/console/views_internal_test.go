// views_internal_test.go is a whitebox test for displayBody's
// msgTypeUpdate case (views.go), F012: "the owner sees raw internal marker
// text in the ticket thread". It lives in package console, not
// console_test, the same rail_internal_test.go precedent, because
// displayBody and the marker bodies it recognizes are cleanest proved
// directly against a synthetic store.MessageRow rather than through a real
// ticket and the job package's own commit-building machinery.
package console

import (
	"strings"
	"testing"

	"zing/internal/store"
)

// updateRow builds a sent (never draft) type="update" message row with the
// given body, the only two fields displayBody's update case reads.
func updateRow(body string) *store.MessageRow {
	return &store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Body: body}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
}

// TestDisplayBody_PlanreviewPendingMarkerIsHumanReadable proves a
// "planreview vN pending" marker (job.planreviewPendingMarker) no longer
// renders as-is, and instead reads as the owner-facing sentence explaining
// that planning is about to resume on its own.
func TestDisplayBody_PlanreviewPendingMarkerIsHumanReadable(t *testing.T) {
	t.Parallel()
	got := displayBody(updateRow("planreview v3 pending"))
	if got == "planreview v3 pending" {
		t.Fatalf("displayBody returned the raw marker unchanged: %q", got)
	}
	if !strings.Contains(got, "Planning resumes") {
		t.Errorf("displayBody(%q) = %q, want it to contain %q", "planreview v3 pending", got, "Planning resumes")
	}
}

// TestDisplayBody_PlanreviewDeliveredMarkerIsHumanReadable proves a
// "planreview vN delivered" marker renders as a plain sentence too.
func TestDisplayBody_PlanreviewDeliveredMarkerIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "Planning resumed with the review findings."
	if got := displayBody(updateRow("planreview v3 delivered")); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", "planreview v3 delivered", got, want)
	}
}

// TestDisplayBody_ValidationErrorsPendingRendersFieldLines proves a
// "validation errors pending run <id>" marker's own response.PathError
// lines (formatReadyErrors' "path: msg" shape) render as "Field <path>:
// <message>" lines under the explanatory sentence, rather than the raw
// path syntax the owner has no reason to parse.
func TestDisplayBody_ValidationErrorsPendingRendersFieldLines(t *testing.T) {
	t.Parallel()
	body := "validation errors pending run 7\n" +
		"scenarios/scenario[0]/then: then must not be empty\n" +
		"plan/overview/problem: problem is required"
	got := displayBody(updateRow(body))

	for _, want := range []string{
		"The plan did not pass its final checks.",
		"Field scenarios/scenario[0]/then: then must not be empty",
		"Field plan/overview/problem: problem is required",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("displayBody(%q) = %q, want it to contain %q", body, got, want)
		}
	}
}

// TestDisplayBody_ValidationErrorsDeliveredIsHumanReadable proves a
// "validation errors delivered run <id>" marker renders as a plain
// sentence.
func TestDisplayBody_ValidationErrorsDeliveredIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "The agent received the check results."
	if got := displayBody(updateRow("validation errors delivered run 7")); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", "validation errors delivered run 7", got, want)
	}
}

// TestDisplayBody_ResponseInvalidIsHumanReadable proves a "response invalid
// run <id>" marker (invalidOutputCommit) renders as a plain sentence, its
// invErr.Reason line dropped rather than shown raw.
func TestDisplayBody_ResponseInvalidIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "The agent's last response could not be used. Zing retries once."
	body := "response invalid run 9\nmissing required field \"plan\""
	if got := displayBody(updateRow(body)); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", body, got, want)
	}
}

// TestDisplayBody_SealMismatchIsHumanReadable proves a "seal mismatch
// cohort <runID>" marker (store.CountSealMismatches) renders as a plain
// sentence.
func TestDisplayBody_SealMismatchIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "The scenario set changed before approval. Zing re-reads it on the next tick."
	if got := displayBody(updateRow("seal mismatch cohort 4")); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", "seal mismatch cohort 4", got, want)
	}
}

// TestDisplayBody_UnknownUpdateBodyIsUnchanged proves an "update" body that
// matches none of the known markers falls through to the raw Body
// (updateLine's own default case), the same defensive fallback
// stateLine, escalationLine, and answerLine already use for a payload
// they cannot decode.
func TestDisplayBody_UnknownUpdateBodyIsUnchanged(t *testing.T) {
	t.Parallel()
	const body = "some future bookkeeping marker nobody recognizes yet"
	if got := displayBody(updateRow(body)); got != body {
		t.Errorf("displayBody(%q) = %q, want it unchanged", body, got)
	}
}

// TestUpdateLineBuildMarkers proves the six build markers design section
// 9.2 names each render as their own owner-facing sentence: "claims ok run
// <rid>", "claim errors pending run <rid>" (with its error lines kept
// below the header sentence), "claim errors delivered run <rid>",
// "perimeter resolved run <rid>", "retry requested" (no run id), and
// "perimeter question dropped run <rid>".
func TestUpdateLineBuildMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, want string
	}{
		{"claims ok", "claims ok run 12", "Claims checked for run 12."},
		{
			"claim errors pending",
			"claim errors pending run 12\nclaims/test_exit: observed 1, want 0",
			"Claim check failed for run 12:\nclaims/test_exit: observed 1, want 0",
		},
		{"claim errors delivered", "claim errors delivered run 12", "Claim errors sent back to run 12."},
		{"perimeter resolved", "perimeter resolved run 9", "Perimeter decided for run 9."},
		{"retry requested", "retry requested", "Retry requested."},
		{"perimeter question dropped", "perimeter question dropped run 4", "Perimeter question dropped for run 4."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := displayBody(updateRow(tc.body)); got != tc.want {
				t.Errorf("displayBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestUpdateLineReviewMarkers proves the six review markers reviewing.go
// writes (design section 5.1, 6.2, 6.2a, 6.5, 6.6) each render as their own
// owner-facing sentence: the four "review round <n> ..." round markers
// (done, with its own kept/dropped/merged line kept below the header;
// asked; failed; void), "review discussed <id>", and "review note <id>"
// (with an owner's note, and with none).
func TestUpdateLineReviewMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, body, want string }{
		{
			"round done",
			"review round 1 done sha abc123 lenses correctness,security\nkept 2 dropped 1 merged 1",
			"Review round 1 finished.\nkept 2 dropped 1 merged 1",
		},
		{"round asked", "review round 2 asked\nruns 5,6\ndone correctness", "Review round 2 is waiting on a lens question."},
		{"round failed", "review round 1 failed\nlens security: timed out", "Review round 1 failed. Zing retries the round."},
		{"round void", "review round 1 void\nhead moved from a to b", "Review round 1 restarted: the branch moved during the round."},
		{"discussed", "review discussed r1f2\nrun 12 batch r1f2 kept 1", "Finding r1f2 discussed with the lens."},
		{
			"note with text",
			"review note r1f2\nb.go:3 is generated, see the header",
			"Owner's note on r1f2:\nb.go:3 is generated, see the header",
		},
		{
			"note with none",
			"review note r1f2\n(the owner gave no note)",
			"Owner's note on r1f2:\n(the owner gave no note)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := displayBody(updateRow(tc.body)); got != tc.want {
				t.Errorf("displayBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
