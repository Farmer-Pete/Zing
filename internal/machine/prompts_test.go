package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	zing "zing"
)

// TestPrompts_MatchThePinnedDesignText pins each Package 7 prompt file's
// SHA-256 to the section 22 design text. A stub regression, a partial
// paste, or an unreviewed edit changes the digest and fails this test.
func TestPrompts_MatchThePinnedDesignText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   string
		sha256 string
	}{
		{
			name:   "classify",
			path:   "prompts/classify.md",
			sha256: "5f20c69c8b34f89171ba2bcc5cbdc836f4865e5ad3039a310db14d6d77800462",
		},
		{
			name:   "planning-feature",
			path:   planningFeaturePromptPath,
			sha256: "7e2f9577094c21d75b8ae97e893817b04d4bdc20cda6ae8192d68712e89a6c5d",
		},
		{
			name:   "planning-bug",
			path:   planningBugPromptPath,
			sha256: "6a1b42de7e161c154d5856f76e79082777ad33cd15b20315a37a9dd16e82c139",
		},
		{
			name:   "planreview",
			path:   "prompts/planreview.md",
			sha256: "c0cc39368632ea2ffa0f4672e8c576d7d044b26a4cae9c43d7fc81c1c2320d7f",
		},
		{
			name:   testJobNameBuild,
			path:   "prompts/build.md",
			sha256: "3a88533d216936fba660e84fcf4dea0004f61442b9195b6ee00b3c2428b10fc8",
		},
		{
			name:   "perimeter",
			path:   "prompts/perimeter.md",
			sha256: "78b4863acf309085bc37f8f33cfea8f430d08b57d44102023eea1f3c03ef00b6",
		},
		{
			name:   "review",
			path:   "prompts/review.md",
			sha256: "d4c75ff8d43b49fd59a273c8b1badc7dec2f2d913851a1a0f7e6ccc0bb725c49",
		},
		{
			name:   "judge",
			path:   "prompts/judge.md",
			sha256: "40ba95666f7d24a842b22b061f2f8aeca7b0a13af3aeb48e7f59a0abaf6f9eb6",
		},
		{
			name:   "respond",
			path:   "prompts/respond.md",
			sha256: "341e4b2129076565acf85d1a3cbf5b9edaab3918593dbc3bfc9ec96843c9206d",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zing.Assets.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", tc.path, err)
			}

			if strings.Contains(string(got), "Stub prompt") {
				t.Errorf("%s still contains the stub placeholder text", tc.path)
			}

			sum := sha256.Sum256(got)
			if gotHex := hex.EncodeToString(sum[:]); gotHex != tc.sha256 {
				t.Errorf("%s sha256 = %s, want %s", tc.path, gotHex, tc.sha256)
			}
		})
	}
}

// conversationsSection is the exact text design section 22.6 requires
// verbatim in both planning prompts, the D31 teaching text for the
// conversation model: question keys, the replies element, settling a
// thread, and the rule that every thread must be settled before ready,
// children, or nothing_to_do.
const conversationsSection = `Conversations. Zing gives every question you ask a key, Q and a number,
such as Q7. It can differ from the key you wrote. Use only keys Zing
has shown you. Each question is a thread between you and the owner.
When the owner writes, you are resumed with a conversation input: for
each thread with something new, the owner's messages oldest first, each
a picked option or text. A later pick replaces an earlier one.

Answer every owner message you receive, in that same turn, with one
reply per thread inside replies:
<replies><reply question="Q7">your answer</reply></replies>.
Every outcome except error can carry replies. When replies are all you
have this turn, return outcome replies.

Settle a thread once the owner's messages give you its decision:
<reply question="Q7" settled="true" decision="...">...</reply>.
The decision is one sentence, at most 500 characters, saying what was
decided. Only you settle a thread. A settled thread takes no more
replies from you. Until the owner approves the gate, the owner can
reopen it by writing in it (D32, 22.12.2).

Settle every thread before you return ready, children, or
nothing_to_do, in that response or an earlier one. Zing rejects any of
the three while a thread is open, and resumes you with the error.`

// TestPlanningPromptsTeachConversations asserts both planning prompts
// carry design section 22.6's Conversations. section byte for byte
// (task D31-4): the agent's only teaching on question keys, replies, and
// settling a thread.
func TestPlanningPromptsTeachConversations(t *testing.T) {
	t.Parallel()

	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			got, err := zing.Assets.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			if !strings.Contains(string(got), conversationsSection) {
				t.Errorf("%s is missing the Conversations. section verbatim (design section 22.6)", path)
			}
		})
	}
}

// TestBuildPromptPlaceholders checks that prompts/build.md carries each of
// its five placeholders exactly once, so ForBuild's single replacement of
// each cannot silently miss or double up.
func TestBuildPromptPlaceholders(t *testing.T) {
	t.Parallel()

	got, err := zing.Assets.ReadFile("prompts/build.md")
	if err != nil {
		t.Fatalf("ReadFile(prompts/build.md): %v", err)
	}
	text := string(got)

	placeholders := []string{"{n}", "{total}", "{task title}", "{test_cmd}", "{lint_cmd}"}
	for _, p := range placeholders {
		if n := strings.Count(text, p); n != 1 {
			t.Errorf("prompts/build.md contains %s %d times, want 1", p, n)
		}
	}
}

// TestReviewPromptPlaceholders checks that prompts/review.md carries
// {lens} and {sha} exactly once each (plan section 12.1), so ForReview's
// single replacement of each cannot silently miss or double up.
func TestReviewPromptPlaceholders(t *testing.T) {
	t.Parallel()

	got, err := zing.Assets.ReadFile("prompts/review.md")
	if err != nil {
		t.Fatalf("ReadFile(prompts/review.md): %v", err)
	}
	text := string(got)

	placeholders := []string{"{lens}", "{sha}"}
	for _, p := range placeholders {
		if n := strings.Count(text, p); n != 1 {
			t.Errorf("prompts/review.md contains %s %d times, want 1", p, n)
		}
	}
}
