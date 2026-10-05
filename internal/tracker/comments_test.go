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

// TestDoneCommentWithoutMergeKeepsReadyForReview proves DoneComment keeps
// today's exact "ready for review" body when mergeSHA is "". Renames
// TestDoneComment_ExactBody.
func TestDoneCommentWithoutMergeKeepsReadyForReview(t *testing.T) {
	t.Parallel()

	got := tracker.DoneComment(testOwner, testURL, "")
	want := "Zing finished this ticket. The pull request is ready for review: " + testURL + "\n\n" + disclosure
	if got != want {
		t.Errorf("DoneComment(%q, %q, %q) =\n%q\nwant\n%q", testOwner, testURL, "", got, want)
	}
}

// TestDoneCommentNamesMergeCommit proves DoneComment, given a merge sha,
// says the pull request was merged and names that merge commit instead of
// saying it is ready for review.
func TestDoneCommentNamesMergeCommit(t *testing.T) {
	t.Parallel()

	const mergeSHA = "0123456789abcdef0123456789abcdef01234567"
	got := tracker.DoneComment(testOwner, testURL, mergeSHA)
	want := "Zing finished this ticket. The pull request was merged: " + testURL + " (merge commit " + mergeSHA + ")\n\n" + disclosure
	if got != want {
		t.Errorf("DoneComment(%q, %q, %q) =\n%q\nwant\n%q", testOwner, testURL, mergeSHA, got, want)
	}
}

// TestNothingToDoComment_ExactBody proves NothingToDoComment interpolates
// owner and notes into the exact body (design D12, plan section 6.8): the
// fixed sentence, the agent's notes, then the shared disclosure line.
func TestNothingToDoComment_ExactBody(t *testing.T) {
	t.Parallel()

	const notes = "The requested behavior already works as described."
	got := tracker.NothingToDoComment(testOwner, notes)
	want := "Zing found nothing to do for this issue. " + notes + "\n\n" + disclosure
	if got != want {
		t.Errorf("NothingToDoComment(%q, %q) =\n%q\nwant\n%q", testOwner, notes, got, want)
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
// five builders.
func TestComments_DisclosureNamesADifferentOwner(t *testing.T) {
	t.Parallel()

	const other = "alice"
	wantSuffix := "Posted automatically by Zing, an agent working on alice's behalf."

	cases := map[string]string{
		"PickupComment":      tracker.PickupComment(other),
		"GateComment":        tracker.GateComment(other, testURL),
		"PRComment":          tracker.PRComment(other, testURL),
		"DoneComment":        tracker.DoneComment(other, testURL, "0123456789abcdef0123456789abcdef01234567"),
		"NothingToDoComment": tracker.NothingToDoComment(other, "notes"),
	}
	for name, got := range cases {
		if !strings.HasSuffix(got, wantSuffix) {
			t.Errorf("%s(%q, ...) = %q, want it to end with %q", name, other, got, wantSuffix)
		}
	}
}

// TestReplyPrefix proves ReplyPrefix names the login (design D10, section
// 9.3) and falls back to the no-owner form, the same empty-owner rule
// disclosure follows, when login is "".
func TestReplyPrefix(t *testing.T) {
	t.Parallel()

	t.Run("login", func(t *testing.T) {
		t.Parallel()
		got := tracker.ReplyPrefix(testOwner)
		want := "Zing (an AI agent) replying on behalf of @peter:"
		if got != want {
			t.Errorf("ReplyPrefix(%q) = %q, want %q", testOwner, got, want)
		}
	})

	t.Run("empty login", func(t *testing.T) {
		t.Parallel()
		got := tracker.ReplyPrefix("")
		want := "Zing (an AI agent) replying:"
		if got != want {
			t.Errorf("ReplyPrefix(\"\") = %q, want %q", got, want)
		}
	})
}
