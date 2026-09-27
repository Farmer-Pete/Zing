package tracker_test

import (
	"strings"
	"testing"

	"zing/internal/tracker"
)

const (
	testOwner  = "peter"
	testURL    = "https://example.com/console/t/42"
	disclosure = "Posted automatically by Zing, an agent working on peter's behalf."
)

// TestPickupComment_ExactBody proves PickupComment builds the exact body
// from PKG6-PLAN.md section 4.3: the pickup line, a blank line, then the
// disclosure naming owner.
func TestPickupComment_ExactBody(t *testing.T) {
	t.Parallel()

	got := tracker.PickupComment(testOwner)
	want := "Zing picked up this issue and started work.\n\n" + disclosure
	if got != want {
		t.Errorf("PickupComment(%q) =\n%q\nwant\n%q", testOwner, got, want)
	}
}

// TestGateComment_ExactBody proves GateComment interpolates both owner and
// consoleURL into the exact body from section 4.3.
func TestGateComment_ExactBody(t *testing.T) {
	t.Parallel()

	got := tracker.GateComment(testOwner, testURL)
	want := "Zing has a plan ready for review. Open the gate to approve or change it: " + testURL + "\n\n" + disclosure
	if got != want {
		t.Errorf("GateComment(%q, %q) =\n%q\nwant\n%q", testOwner, testURL, got, want)
	}
}

// TestPRComment_ExactBody proves PRComment interpolates both owner and
// prURL into the exact body from section 4.3.
func TestPRComment_ExactBody(t *testing.T) {
	t.Parallel()

	got := tracker.PRComment(testOwner, testURL)
	want := "Zing opened a draft pull request: " + testURL + "\n\n" + disclosure
	if got != want {
		t.Errorf("PRComment(%q, %q) =\n%q\nwant\n%q", testOwner, testURL, got, want)
	}
}

// TestDoneComment_ExactBody proves DoneComment interpolates both owner and
// prURL into the exact body from section 4.3.
func TestDoneComment_ExactBody(t *testing.T) {
	t.Parallel()

	got := tracker.DoneComment(testOwner, testURL)
	want := "Zing finished this ticket. The pull request is ready for review: " + testURL + "\n\n" + disclosure
	if got != want {
		t.Errorf("DoneComment(%q, %q) =\n%q\nwant\n%q", testOwner, testURL, got, want)
	}
}

// TestPickupComment_EmptyOwnerOmitsThePossessive proves an empty owner
// renders a disclosure with no name and no dangling possessive, rather than
// the grammatically broken "on 's behalf." the naive fmt.Sprintf produces.
func TestPickupComment_EmptyOwnerOmitsThePossessive(t *testing.T) {
	t.Parallel()

	got := tracker.PickupComment("")
	if !strings.HasSuffix(got, "Posted automatically by Zing.") {
		t.Errorf("PickupComment(\"\") = %q, want it to end with the no-name disclosure", got)
	}
	if strings.Contains(got, "'s behalf") {
		t.Errorf("PickupComment(\"\") = %q, want no dangling \"'s behalf\"", got)
	}
}

// TestComments_DisclosureNamesADifferentOwner proves the disclosure line
// is built from the owner parameter, not a hardcoded name, across all
// four builders.
func TestComments_DisclosureNamesADifferentOwner(t *testing.T) {
	t.Parallel()

	const other = "alice"
	wantSuffix := "Posted automatically by Zing, an agent working on alice's behalf."

	cases := map[string]string{
		"PickupComment": tracker.PickupComment(other),
		"GateComment":   tracker.GateComment(other, testURL),
		"PRComment":     tracker.PRComment(other, testURL),
		"DoneComment":   tracker.DoneComment(other, testURL),
	}
	for name, got := range cases {
		if !strings.HasSuffix(got, wantSuffix) {
			t.Errorf("%s(%q, ...) = %q, want it to end with %q", name, other, got, wantSuffix)
		}
	}
}
