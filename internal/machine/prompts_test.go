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
			sha256: "e01f7fbf0ec0a66987deb59533cd2f136f8fc86ebf732581cc721377f8d56ebe",
		},
		{
			name:   "planning-bug",
			path:   planningBugPromptPath,
			sha256: "3b435b3bc1f8f1ae26e49e169673e84bb79427c9cf851714f182bdd7495f6b63",
		},
		{
			name:   "planreview",
			path:   "prompts/planreview.md",
			sha256: "1dcc4f65fcb2e0c85c4e375c00caca934ca4d6d8f589db0e8ca5fe27e4d5495c",
		},
		{
			name:   testJobNameBuild,
			path:   "prompts/build.md",
			sha256: "c474c2de970139242c79d3ccdfd4411034c1c8c659a9259bd54818ef1e18607d",
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
			sha256: "0b3ace2fbe7bad963efb4ce744fca11ae2df70be0f5cb8daf777c25ca6547a96",
		},
		{
			name:   "judge",
			path:   "prompts/judge.md",
			sha256: "3ac9e3d378d60bd79e1fcd42cca366babf73f581b36cee604d0bb1062afb0b5e",
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

// TestBuildPromptStopsBeforeDeadline proves prompts/build.md tells the
// builder what the deadline input means and what to do with 10 minutes
// left: stop, commit nothing, and return the error outcome with code other
// (#53).
func TestBuildPromptStopsBeforeDeadline(t *testing.T) {
	t.Parallel()
	text := unwrapped(t, "prompts/build.md")
	for _, want := range []string{
		"The deadline input says when this run ends.",
		"Zing stops the run then and keeps nothing from it.",
		"Check the time with date between steps.",
		"When 10 minutes remain, stop: commit nothing, start no new command, and return the error outcome with code other.",
		"In what, list the parts of this task that remain; in tried, list what is done and the files you changed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompts/build.md lacks %q", want)
		}
	}
}

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

// taskSizeSentence is what both planning prompts say right after
// greenTasksSentence (#53): Zing stops each build run after
// {build_minutes} minutes, so a task expected to need more than half of
// that, or one that adds three or more new functions with their tests,
// needs splitting.
const taskSizeSentence = "Zing builds each task in one run that it stops after {build_minutes} minutes. " +
	"Split any task you expect to need more than half of that. " +
	"A task that adds three or more new functions with their tests needs splitting."

// TestPlanningPromptsSizeTasksToRun proves both planning prompts tell the
// planner the build run's time limit and ask it to split an oversized
// task, right after the green-tasks sentence, and that each prompt names
// {build_minutes} exactly once (#53).
func TestPlanningPromptsSizeTasksToRun(t *testing.T) {
	t.Parallel()
	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		text := unwrapped(t, path)
		if !strings.Contains(text, greenTasksSentence+" "+taskSizeSentence) {
			t.Errorf("%s lacks the task-size sentence right after the green-tasks sentence", path)
		}
		got, err := zing.Assets.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if n := strings.Count(string(got), "{build_minutes}"); n != 1 {
			t.Errorf("%s contains {build_minutes} %d times, want 1", path, n)
		}
	}
}

// simplificationLensTaskSplitLine is the line prompts/lenses/simplification.md
// adds to its "In a plan" section (#53): plan review proposes splitting an
// oversized task the same way the task-size sentence above asks the
// planner itself to.
const simplificationLensTaskSplitLine = "Find each task that adds three or more new functions with their tests, " +
	"and propose splitting it so each part fits one build run."

// TestSimplificationLensProposesSplittingOversizedTasks proves
// prompts/lenses/simplification.md carries the task-split line in its "In
// a plan" section, before "In code" (#53): deleting or rewording the line
// would otherwise pass every other test.
func TestSimplificationLensProposesSplittingOversizedTasks(t *testing.T) {
	t.Parallel()
	const path = "prompts/lenses/simplification.md"
	text := unwrapped(t, path)
	inPlanIdx := strings.Index(text, "In a plan")
	inCodeIdx := strings.Index(text, "In code")
	lineIdx := strings.Index(text, simplificationLensTaskSplitLine)
	anyMissing := inPlanIdx < 0 || inCodeIdx < 0 || lineIdx < 0
	if anyMissing {
		t.Fatalf("%s: In a plan at %d, In code at %d, task-split line at %d, want all present", path, inPlanIdx, inCodeIdx, lineIdx)
	}
	if inPlanIdx >= lineIdx || lineIdx >= inCodeIdx {
		t.Errorf("%s: want the task-split line between In a plan and In code, got In a plan=%d, line=%d, In code=%d", path, inPlanIdx, lineIdx, inCodeIdx)
	}
}

// testsLensPureFunctionLine is the bullet prompts/lenses/tests.md adds to
// its "In a plan" section (#111): plan review flags changed wiring whose
// decision has no test.
const testsLensPureFunctionLine = "If a changed handler, a callback, or another call site with no test harness " +
	"has no test of its decision, that is a major finding, even when the plan tests helpers or other cut points. " +
	"The fix names the pure function to extract and the test for it."

// TestTestsLensRequiresPureFunctionSeam proves prompts/lenses/tests.md
// carries the pure-function-seam bullet in its "In a plan" section, before
// "In code" (#111): deleting or rewording the line would otherwise pass
// every other test.
func TestTestsLensRequiresPureFunctionSeam(t *testing.T) {
	t.Parallel()
	const path = "prompts/lenses/tests.md"
	text := unwrapped(t, path)
	inPlanIdx := strings.Index(text, "In a plan")
	inCodeIdx := strings.Index(text, "In code")
	lineIdx := strings.Index(text, testsLensPureFunctionLine)
	anyMissing := inPlanIdx < 0 || inCodeIdx < 0 || lineIdx < 0
	if anyMissing {
		t.Fatalf("%s: In a plan at %d, In code at %d, pure-function line at %d, want all present", path, inPlanIdx, inCodeIdx, lineIdx)
	}
	if inPlanIdx >= lineIdx || lineIdx >= inCodeIdx {
		t.Errorf("%s: want the pure-function line between In a plan and In code, got In a plan=%d, line=%d, In code=%d", path, inPlanIdx, lineIdx, inCodeIdx)
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
// unobservable there. It also tells the planner to clear a check's state
// under a fresh mktemp -d directory rather than rm -f or rm -rf, which
// Codex's command policy refuses (#94).
const sandboxChecksSentence = "Zing runs every check inside the build sandbox, " +
	"which cannot start another sandbox. " +
	`Write temporary files under "$TMPDIR", never /tmp. ` +
	"For state a check must start without, write under a fresh directory " +
	"from mktemp -d, such as d=$(mktemp -d); Codex refuses rm -f and rm -rf. " +
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

// hostKindSentence is what both planning prompts say about kind host
// (#49): Zing runs a host scenario's check on the owner's machine at
// judging, outside any sandbox, once the owner approves it at the gate,
// for a live sandbox probe, a live zing serve, or wall-clock timing, and
// a host scenario needs a check.
const hostKindSentence = "The exception is kind host. " +
	"Zing runs a host scenario's check on the owner's machine at judging, " +
	"outside any sandbox, once the owner approves it at the gate. " +
	"Use it for a live sandbox probe, a live zing serve, or wall-clock timing. " +
	"A host scenario needs a check."

// TestPlanningPromptsTeachHostKind proves both planning prompts offer kind
// host right after the sandbox-checks sentence, and that planning-feature.md
// lists it as the fourth kind in its scenarios step (#49).
func TestPlanningPromptsTeachHostKind(t *testing.T) {
	t.Parallel()
	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		if !strings.Contains(unwrapped(t, path), hostKindSentence) {
			t.Errorf("%s lacks the host-kind sentence", path)
		}
	}
	if !strings.Contains(unwrapped(t, planningFeaturePromptPath), "behavior, negative, performance, or host") {
		t.Errorf("%s does not list host as the fourth kind", planningFeaturePromptPath)
	}
}

// governanceChecksSentence is what both planning prompts say about
// reading CLAUDE.md or AGENTS.md (#225, #92): the judge's checkout
// overwrites both with the default branch's copies, so a check must read
// the committed file with git show HEAD:FILE rather than the working
// copy.
const governanceChecksSentence = "A check reads CLAUDE.md or AGENTS.md with git show " +
	"HEAD:FILE, never from the working copy, such as git show HEAD:AGENTS.md | tr -s " +
	`'[:space:]' ' ' | grep -qF 'two words', because the judge's checkout holds the ` +
	"default branch's copies of both files."

// TestPlanningPromptsReadGovernanceFilesFromCommit proves both planning
// prompts tell the planner to read CLAUDE.md or AGENTS.md with
// git show HEAD:FILE rather than the working copy, since the judge's
// checkout holds the default branch's copies of both (#225, #92).
func TestPlanningPromptsReadGovernanceFilesFromCommit(t *testing.T) {
	t.Parallel()
	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		if !strings.Contains(unwrapped(t, path), governanceChecksSentence) {
			t.Errorf("%s lacks the governance-checks sentence", path)
		}
	}
}

// pureFunctionSeamSentence is what both planning prompts say in their
// Plan step (#111, #130): a decision inside a handler or callback moves
// into a pure function the plan tests.
const pureFunctionSeamSentence = "If behavior lives in an event handler, a UI callback, " +
	"or other code with no test harness, move the decision into a pure function " +
	"and test that function; the handler stays a shim of about one line that calls it."

// TestPlanningPromptsRequirePureFunctionSeam proves both planning prompts
// ask for a pure-function seam behind handler and callback logic (#111).
func TestPlanningPromptsRequirePureFunctionSeam(t *testing.T) {
	t.Parallel()
	for _, path := range []string{planningFeaturePromptPath, planningBugPromptPath} {
		if !strings.Contains(unwrapped(t, path), pureFunctionSeamSentence) {
			t.Errorf("%s lacks the pure-function seam sentence", path)
		}
	}
}

// TestJudgePromptTrustsHostChecks proves prompts/judge.md tells the judge
// that a host scenario arrives already run, with its exit code and
// output in host_checks, that the judge must not re-run it, and that it
// must never return cannot_run for one (#49).
func TestJudgePromptTrustsHostChecks(t *testing.T) {
	t.Parallel()
	text := unwrapped(t, "prompts/judge.md")
	for _, want := range []string{
		"Do not run a host scenario's check, and never return cannot_run for a host scenario.",
		"The host_checks input gives each one's exit code and the tail of its output.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompts/judge.md lacks %q", want)
		}
	}
}

// TestJudgePromptAmendsWrongChecks proves prompts/judge.md tells the
// judge to amend a sealed check that is wrong as written, naming the
// scenario id, the given, when, then and check it proposes, the kind
// only if it should change, and a reason, to amend at most one
// scenario, and to fail the scenario instead when the check is right
// and the code is wrong.
func TestJudgePromptAmendsWrongChecks(t *testing.T) {
	t.Parallel()
	text := unwrapped(t, "prompts/judge.md")
	for _, want := range []string{
		"add an amendment to that error",
		"the scenario id, the given, when, then, and check you propose, the kind only if it should change, and a reason",
		"Amend at most one scenario.",
		"When the check is right and the code is wrong, fail the scenario instead.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompts/judge.md lacks %q", want)
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
