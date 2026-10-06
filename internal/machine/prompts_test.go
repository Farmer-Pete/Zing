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
			sha256: "5d267ec6b29c5bb14bb35a9ea593db2c43d06d1bda9660c3e5a4f095cd3855be",
		},
		{
			name:   "planning-bug",
			path:   planningBugPromptPath,
			sha256: "16904cfc1103b323df94d4140f8b9321b530a5403fdbd08be18a8fef682b5946",
		},
		{
			name:   "planreview",
			path:   "prompts/planreview.md",
			sha256: "7eceaab2061d7c45cba821969aec91b736d019491efa1bcce2e1d73d26d79221",
		},
		{
			name:   testJobNameBuild,
			path:   "prompts/build.md",
			sha256: "abd42e1ac7c2bd904dc54f8679c14cef7ee8d4f30b2b62c87f68bac95122348f",
		},
		{
			name:   "merge",
			path:   "prompts/merge.md",
			sha256: "dd65ef81eb5109231e84ee2c9ab008121f98a7e1fc4d9b9bc7708358dbb915f6",
		},
		{
			name:   "perimeter",
			path:   "prompts/perimeter.md",
			sha256: "78b4863acf309085bc37f8f33cfea8f430d08b57d44102023eea1f3c03ef00b6",
		},
		{
			name:   "review",
			path:   "prompts/review.md",
			sha256: "1c53cc1d16e14603ebc9e920dcdcbf4d949f66194c2bda56ab5502fbfa774d97",
		},
		{
			name:   "judge",
			path:   "prompts/judge.md",
			sha256: "d187df17a370261be5e527259e7f31725a5677a247a2c36c71477a077a51137f",
		},
		{
			name:   "respond",
			path:   "prompts/respond.md",
			sha256: "88af671ead5fc0ce2e8c78ac9adaf4fbb30c7b9106fe77e7cbe85d836b3b6701",
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
have this turn, return outcome replies. Outcome replies needs at least
one thread left open: if your replies settle every thread, return ready
(or children, or nothing_to_do) with the replies attached.

Settle a thread once the owner's messages give you its decision:
<reply question="Q7" settled="true" decision="...">...</reply>.
The decision is one sentence, at most 500 characters, saying what was
decided. Only you settle a thread. A settled thread takes no more
replies from you. Until the owner approves the gate, the owner can
reopen it by writing in it; you are then told "The owner reopened Q1."
with your earlier decision. Answer, and settle it again when the
owner's messages give you the decision.

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

// unwrapped reads path and joins its lines, so a test can look for a
// sentence the file wraps at its own column width.
func unwrapped(t *testing.T, path string) string {
	t.Helper()
	got, err := zing.Assets.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return strings.Join(strings.Fields(string(got)), " ")
}

// TestBuildPromptLeavesFullSuiteToZing proves the build prompt no longer
// asks the builder to run the full test and lint commands (#55): Zing runs
// them at CHECK and resumes the builder with any failing output, and a
// task that forbids its own fix is a plan_gap.
func TestBuildPromptLeavesFullSuiteToZing(t *testing.T) {
	t.Parallel()
	text := unwrapped(t, "prompts/build.md")
	if strings.Contains(text, "until both exit 0") {
		t.Error("prompts/build.md still tells the builder to run the commands until both exit 0")
	}
	if strings.Contains(text, "Every task ends green") {
		t.Error("prompts/build.md requires a regression test and fix of every task, including cleanup tasks and fix runs that reproduce nothing")
	}
	for _, want := range []string{
		"Do not run the project's full test or lint command",
		"return the error outcome with code plan_gap",
		"When this task adds a regression test for a failure you reproduced, its fix lands in this same task",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompts/build.md lacks %q", want)
		}
	}
}

// greenTasksSentence is what both planning prompts say about tests (#55):
// each task's named tests pass when it ends, and the full test and lint
// run is Zing's CHECK after the task, as build.md says.
const greenTasksSentence = "Every task ends with its named tests passing; " +
	"a test written in a task is made to pass in that same task, never left failing for a later one. " +
	"Zing runs the project's full test and lint commands after each task."

// TestPlanningPromptsRequireGreenTasks proves both planning prompts tell
// the planner that no task may end with a failing test (#55).
func TestPlanningPromptsRequireGreenTasks(t *testing.T) {
	t.Parallel()
	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		if !strings.Contains(unwrapped(t, path), greenTasksSentence) {
			t.Errorf("%s lacks the green-tasks sentence", path)
		}
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

// TestJudgePromptRunsChecksAsWritten proves prompts/judge.md tells the
// judge to run each sealed check through "zing check SID" rather than
// pasting it into its own shell or repairing it, to use "$TMPDIR" instead
// of /tmp, to run a long check in the background and poll it instead of
// waiting on one call, and to treat a skip the scenario's own then names
// as expected as an observed pass while a skip that hides the behavior
// under test stays unobserved (#78, #46, #38, #86).
func TestJudgePromptRunsChecksAsWritten(t *testing.T) {
	t.Parallel()
	text := unwrapped(t, "prompts/judge.md")
	for _, want := range []string{
		"Run each scenario's check with `zing check SID`",
		"Never paste a check into your own shell.",
		"do not repair it or run your own version",
		"Return the error outcome with code cannot_run, naming the scenario and the defect in its check",
		`Write temporary files under "$TMPDIR", never /tmp.`,
		"Run one like that in the background and poll it until it finishes",
		"A skip that the scenario's own then names as the expected result is an observed pass.",
		"A skip that hides the behavior under test is not observed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompts/judge.md lacks %q", want)
		}
	}
}

// sandboxChecksSentence is what both planning prompts say about where
// checks run (#78, #46): checks run inside the build sandbox, which
// cannot start another sandbox, so a given or check that needs a live
// serve, a machine outside the sandbox, or the owner's own config is
// unobservable there.
const sandboxChecksSentence = "Zing runs every check inside the build sandbox, " +
	"which cannot start another sandbox. " +
	`Write temporary files under "$TMPDIR", never /tmp. ` +
	"Write only givens and checks an agent inside that sandbox can observe: " +
	"no live zing serve, no machine outside the sandbox, and none of the " +
	"owner's own config such as ~/.codex, ~/.claude, or the console."

// TestPlanningPromptsTeachSandboxChecks proves both planning prompts tell
// the planner that checks run inside the build sandbox, so temp files go
// under "$TMPDIR" and scenarios must be observable from inside it (#78,
// #46).
func TestPlanningPromptsTeachSandboxChecks(t *testing.T) {
	t.Parallel()
	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		if !strings.Contains(unwrapped(t, path), sandboxChecksSentence) {
			t.Errorf("%s lacks the sandbox-checks sentence", path)
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
