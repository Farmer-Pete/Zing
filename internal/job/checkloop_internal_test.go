package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/store"
)

// TestCheckInputText pins the check input's exact text (#55 plan section
// 3): one section per failed command, test before lint, a blank line
// between sections, and the cut, timeout, and no-output variants.
func TestCheckInputText(t *testing.T) {
	t.Parallel()
	const (
		budget  = 45 * time.Minute
		goTest  = "go test ./..."
		makeLnt = "make lint"
	)
	testFail := commandResult{Kind: checkKindTest, Cmd: goTest, Exit: 1, Budget: budget, Output: "--- FAIL: TestPing\n\n", Total: 20}
	lintCut := commandResult{Kind: checkKindLint, Cmd: makeLnt, Exit: 2, Budget: budget, Output: "internal/x/y.go:12:2: ineffassign ...", Total: 70211}
	testTimeout := commandResult{Kind: checkKindTest, Cmd: goTest, Exit: -1, TimedOut: true, Budget: budget, Output: "partial\n", Total: 8}
	lintSilent := commandResult{Kind: checkKindLint, Cmd: makeLnt, Exit: 1, Budget: budget}
	testPass := commandResult{Kind: checkKindTest, Cmd: goTest, Exit: 0, Budget: budget, Output: "ok\n", Total: 3}
	lintPass := commandResult{Kind: checkKindLint, Cmd: makeLnt, Exit: 0, Budget: budget}

	cases := []struct {
		name    string
		results []commandResult
		want    string
	}{
		{
			"one failing test",
			[]commandResult{testFail, lintPass},
			"test command: go test ./...\nexit code: 1\noutput:\n--- FAIL: TestPing",
		},
		{
			"lint cut",
			[]commandResult{testPass, lintCut},
			"lint command: make lint\nexit code: 2\noutput (cut to the last 16384 of 70211 bytes):\ninternal/x/y.go:12:2: ineffassign ...",
		},
		{
			"timeout",
			[]commandResult{testTimeout},
			"test command: go test ./...\nexit code: none (killed when the 45m check budget ran out)\noutput:\npartial",
		},
		{
			"both failing",
			[]commandResult{testFail, lintSilent},
			"test command: go test ./...\nexit code: 1\noutput:\n--- FAIL: TestPing\n\nlint command: make lint\nexit code: 1\noutput: none",
		},
		{
			"no output",
			[]commandResult{testPass, lintSilent},
			"lint command: make lint\nexit code: 1\noutput: none",
		},
		{"none failing", []commandResult{testPass, lintPass}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := checkInputText(tc.results); got != tc.want {
				t.Errorf("checkInputText =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// budgetCommands is a CommandRunner that records each call's timeout and
// returns a scripted result per shell command. advance, when set, moves
// the fake check clock forward during the test command.
type budgetCommands struct {
	timeouts map[string]time.Duration
	results  map[string]struct {
		exit int
		err  error
	}
	advance func()
}

// The fake project's two commands.
const (
	fakeTestCmd = "the-test"
	fakeLintCmd = "the-lint"
)

func (c *budgetCommands) Run(_ context.Context, _, _, shellCmd string, timeout time.Duration, _ CommandIO) (int, error) {
	c.timeouts[shellCmd] = timeout
	if shellCmd == fakeTestCmd && c.advance != nil {
		c.advance()
	}
	r := c.results[shellCmd]
	return r.exit, r.err
}

// TestCheckLintGetsOnlyRemainingBudget proves test and lint share one
// budget of jobs.build.timeout_minutes (plan D1): after a test command that
// used 40 of 45 minutes, lint gets the remaining 5, and a lint timeout is
// reported against the whole budget. Not parallel: it swaps checkNow.
func TestCheckLintGetsOnlyRemainingBudget(t *testing.T) {
	start := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	now := start
	orig := checkNow
	checkNow = func() time.Time { return now }
	t.Cleanup(func() { checkNow = orig })

	d := Deps{Machine: &machine.Machine{Jobs: map[string]machine.Job{jobBuildName: {TimeoutMinutes: 45}}}}
	proj := Project{TestCmd: fakeTestCmd, LintCmd: fakeLintCmd}
	tk := store.Ticket{ID: 1}

	run := func(t *testing.T, lintErr error) ([]commandResult, *budgetCommands) {
		t.Helper()
		now = start
		fake := &budgetCommands{
			timeouts: map[string]time.Duration{},
			results: map[string]struct {
				exit int
				err  error
			}{
				fakeTestCmd: {exit: 1},
				fakeLintCmd: {exit: -1, err: lintErr},
			},
			advance: func() { now = now.Add(40 * time.Minute) },
		}
		d.Commands = fake
		results, err := runCheckCommands(t.Context(), d, tk, orchestrator.Worktree{}, proj, nil)
		if err != nil {
			t.Fatalf("runCheckCommands: %v", err)
		}
		return results, fake
	}

	_, fake := run(t, nil)
	if got := fake.timeouts[fakeTestCmd]; got != 45*time.Minute {
		t.Errorf("test timeout = %v, want 45m", got)
	}
	if got := fake.timeouts[fakeLintCmd]; got != 5*time.Minute {
		t.Errorf("lint timeout = %v, want 5m (the budget left after test)", got)
	}

	results, _ := run(t, ErrCommandTimeout)
	if len(results) != 2 || !results[0].failed() || results[0].TimedOut || !results[1].TimedOut {
		t.Fatalf("results = %+v, want test failed (not timed out) and lint timed out", results)
	}
	if text := checkInputText(results); !strings.Contains(text, "lint command: the-lint\nexit code: none (killed when the 45m check budget ran out)") {
		t.Errorf("check input = %q, want the lint timeout line", text)
	}
}

// TestCheckLintNotRunIsAFailure proves a passing test command that uses the
// whole shared budget does not let CHECK land: lint never ran, so the result is a failure with its own line, and the
// builder is resumed or the cap escalates. Not parallel: it swaps checkNow.
func TestCheckLintNotRunIsAFailure(t *testing.T) {
	start := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	now := start
	orig := checkNow
	checkNow = func() time.Time { return now }
	t.Cleanup(func() { checkNow = orig })

	fake := &budgetCommands{
		timeouts: map[string]time.Duration{},
		results: map[string]struct {
			exit int
			err  error
		}{fakeTestCmd: {exit: 0}},
		advance: func() { now = now.Add(45 * time.Minute) },
	}
	d := Deps{Commands: fake, Machine: &machine.Machine{Jobs: map[string]machine.Job{jobBuildName: {TimeoutMinutes: 45}}}}
	results, err := runCheckCommands(t.Context(), d, store.Ticket{ID: 1}, orchestrator.Worktree{}, Project{TestCmd: fakeTestCmd, LintCmd: fakeLintCmd}, nil)
	if err != nil {
		t.Fatalf("runCheckCommands: %v", err)
	}
	if _, ran := fake.timeouts[fakeLintCmd]; ran {
		t.Error("lint ran with no budget left")
	}
	if got := failedKinds(results); len(got) != 1 || got[0] != checkKindLint {
		t.Fatalf("failed kinds = %v, want [lint]", got)
	}
	want := "lint command: the-lint\nlint did not run: the CHECK budget ran out after the test command"
	if text := checkInputText(results); text != want {
		t.Errorf("check input = %q, want %q", text, want)
	}
}
