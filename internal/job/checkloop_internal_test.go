package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zing/internal/gitfixture"
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
	if shellCmd == fakeLintCmd && c.advance != nil {
		c.advance()
	}
	r := c.results[shellCmd]
	return r.exit, r.err
}

// TestCheckTestGetsOnlyRemainingBudget proves lint and test share one
// budget of jobs.build.timeout_minutes (plan D1): after a lint command that
// used 40 of 45 minutes, test gets the remaining 5, and a test timeout is
// reported against the whole budget. Not parallel: it swaps checkNow.
func TestCheckTestGetsOnlyRemainingBudget(t *testing.T) {
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
		}{
			fakeLintCmd: {exit: 0},
			fakeTestCmd: {exit: -1, err: ErrCommandTimeout},
		},
		advance: func() { now = now.Add(40 * time.Minute) },
	}
	d := Deps{Commands: fake, Machine: &machine.Machine{Jobs: map[string]machine.Job{jobBuildName: {TimeoutMinutes: 45}}}}
	proj := Project{TestCmd: fakeTestCmd, LintCmd: fakeLintCmd}
	results, err := runCheckCommands(t.Context(), d, store.Ticket{ID: 1}, orchestrator.Worktree{}, proj, nil)
	if err != nil {
		t.Fatalf("runCheckCommands: %v", err)
	}

	if got := fake.timeouts[fakeLintCmd]; got != 45*time.Minute {
		t.Errorf("lint timeout = %v, want 45m", got)
	}
	if got := fake.timeouts[fakeTestCmd]; got != 5*time.Minute {
		t.Errorf("test timeout = %v, want 5m (the budget left after lint)", got)
	}
	if len(results) != 2 || results[0].failed() || !results[1].TimedOut {
		t.Fatalf("results = %+v, want lint ok and test timed out", results)
	}
	if text := checkInputText(results); !strings.Contains(text, "test command: the-test\nexit code: none (killed when the 45m check budget ran out)") {
		t.Errorf("check input = %q, want the test timeout line", text)
	}
}

// TestCheckTestNotRunIsAFailure proves a passing lint command that uses the
// whole shared budget does not let CHECK land: test never ran, so the
// result is a failure with its own line, and the builder is resumed or the
// cap escalates. Not parallel: it swaps checkNow.
func TestCheckTestNotRunIsAFailure(t *testing.T) {
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
		}{fakeLintCmd: {exit: 0}},
		advance: func() { now = now.Add(45 * time.Minute) },
	}
	d := Deps{Commands: fake, Machine: &machine.Machine{Jobs: map[string]machine.Job{jobBuildName: {TimeoutMinutes: 45}}}}
	results, err := runCheckCommands(t.Context(), d, store.Ticket{ID: 1}, orchestrator.Worktree{}, Project{TestCmd: fakeTestCmd, LintCmd: fakeLintCmd}, nil)
	if err != nil {
		t.Fatalf("runCheckCommands: %v", err)
	}
	if _, ran := fake.timeouts[fakeTestCmd]; ran {
		t.Error("test ran with no budget left")
	}
	if got := failedKinds(results); len(got) != 1 || got[0] != checkKindTest {
		t.Fatalf("failed kinds = %v, want [test]", got)
	}
	want := "test command: the-test\ntest did not run: the CHECK budget ran out before it started"
	if text := checkInputText(results); text != want {
		t.Errorf("check input = %q, want %q", text, want)
	}
}

// snapshotClockRunner wraps a real orchestrator.Runner and advances a fake
// clock on its first call, so a test can prove the shared budget is read
// strictly after the pre-fix ChangedPaths snapshot, not merely after some
// fixed number of checkNow calls (review r1f2: a reordering that read the
// budget first would leave the clock at its start value and must not be
// mistaken for the snapshot having already run). ChangedPaths and
// RevertPaths both revalidate the worktree before their own git calls
// (perimeter.go), so even the first call this runner sees -- resolving the
// worktree's common git dir, or confirming its checked-out branch -- only
// happens once the snapshot is already under way.
type snapshotClockRunner struct {
	real    orchestrator.Runner
	advance func()
}

func (r *snapshotClockRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.advance()
	return r.real.Run(ctx, dir, name, args...)
}

func (r *snapshotClockRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.advance()
	return r.real.Output(ctx, dir, name, args...)
}

// TestCheckFixSnapshotUsesBudget proves the pre-fix ChangedPaths snapshot
// runs before remaining is computed: once the snapshot alone exhausts the
// shared budget, fix is reported as not run, exactly like lint or test. The
// clock only advances when the snapshot's own git calls run, not on a
// fixed call count, so a reordering that read the budget before the
// snapshot would instead see the clock still at its start value and let
// fix run. The worktree is set up through a separate, unwrapped
// orchestrator first, so that setup's own git calls do not advance the
// clock before the measured call. Not parallel: it swaps checkNow.
func TestCheckFixSnapshotUsesBudget(t *testing.T) {
	start := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	now := start
	orig := checkNow
	checkNow = func() time.Time { return now }
	t.Cleanup(func() { checkNow = orig })

	dir := t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), dir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	setupOrch, repoGit, ok := pbOrchestratorFor(t, dir, orchestrator.NewRunner())
	if !ok {
		t.Fatal("pbOrchestratorFor: not a git repository")
	}
	wt, _, err := setupOrch.EnsureWorktree(t.Context(), 1, "fix-snapshot-budget")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	runner := &snapshotClockRunner{real: orchestrator.NewRunner(), advance: func() { now = start.Add(45 * time.Minute) }}
	measureOrch, _, ok := pbOrchestratorFor(t, dir, runner)
	if !ok {
		t.Fatal("pbOrchestratorFor: not a git repository")
	}

	const theFix = "the-fix"
	fake := &budgetCommands{
		timeouts: map[string]time.Duration{},
		results: map[string]struct {
			exit int
			err  error
		}{},
	}
	d := Deps{Commands: fake, Machine: &machine.Machine{Jobs: map[string]machine.Job{jobBuildName: {TimeoutMinutes: 45}}}}
	proj := Project{Orch: measureOrch, RepoGit: repoGit, FixCmd: theFix, LintCmd: fakeLintCmd, TestCmd: fakeTestCmd}
	results, err := runCheckCommands(t.Context(), d, store.Ticket{ID: 1}, wt, proj, nil)
	if err != nil {
		t.Fatalf("runCheckCommands: %v", err)
	}

	if _, ran := fake.timeouts[theFix]; ran {
		t.Error("fix ran with no budget left")
	}
	if got := failedKinds(results); len(got) != 1 || got[0] != checkKindFix {
		t.Fatalf("failed kinds = %v, want [fix]", got)
	}
	want := "fix command: the-fix\nfix did not run: the CHECK budget ran out before it started"
	if text := checkInputText(results); text != want {
		t.Errorf("check input = %q, want %q", text, want)
	}
}

// plainShellCommands runs a shell command for real, with no sandbox and no
// OnStart callback: TestCheckFixRevertLogsStrayPaths needs fix's own shell
// command to actually write a file, but not the check_procs bookkeeping
// NewCommandRunner's OnStart path drives, which needs a real Deps.Store
// this test has no reason to open.
type plainShellCommands struct{}

func (plainShellCommands) Run(ctx context.Context, dir, _, shellCmd string, _ time.Duration, cio CommandIO) (int, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", shellCmd)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = cio.Out, cio.Out
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// TestCheckFixRevertLogsStrayPaths proves the WARN "fix change reverted"
// record is logged only once RevertPaths has actually removed the stray
// paths it names, not before: an operator who reads the record must be
// able to trust that the named paths are already gone (review r1f1). Not
// parallel: it swaps slog.Default.
func TestCheckFixRevertLogsStrayPaths(t *testing.T) {
	dir := t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), dir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	orch, repoGit, ok := pbOrchestratorFor(t, dir, orchestrator.NewRunner())
	if !ok {
		t.Fatal("pbOrchestratorFor: not a git repository")
	}
	wt, _, err := orch.EnsureWorktree(t.Context(), 1, "fix-revert-log")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	const fixCmd = "printf x > stray.txt"
	d := Deps{Commands: plainShellCommands{}, Machine: &machine.Machine{Jobs: map[string]machine.Job{jobBuildName: {TimeoutMinutes: 45}}}}
	proj := Project{Orch: orch, RepoGit: repoGit, FixCmd: fixCmd, LintCmd: "true", TestCmd: "true"}
	results, err := runCheckCommands(t.Context(), d, store.Ticket{ID: 1}, wt, proj, nil)
	if err != nil {
		t.Fatalf("runCheckCommands: %v", err)
	}
	if got := failedKinds(results); len(got) != 0 {
		t.Fatalf("failed kinds = %v, want none", got)
	}
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), "stray.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("stray.txt exists in the worktree, want it reverted before the log is read")
	}

	var records []map[string]any
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var rec map[string]any
		if decErr := dec.Decode(&rec); decErr != nil {
			t.Fatalf("decode log line: %v", decErr)
		}
		if rec["msg"] == "fix change reverted" {
			records = append(records, rec)
		}
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want exactly 1", records)
	}
	if records[0]["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", records[0]["level"])
	}
	paths, ok := records[0]["paths"].([]any)
	if !ok || len(paths) != 1 || paths[0] != "stray.txt" {
		t.Errorf("paths = %v, want [stray.txt]", records[0]["paths"])
	}
}

// TestRunCheckCommandLogsFixDuration proves runCheckCommand's existing
// "command re-run" INFO record (building.go) already covers the fix kind:
// one record per call, naming command "fix", with a whole-second duration
// and the right exit_code/timed_out pair. Not parallel: it swaps
// slog.Default.
func TestRunCheckCommandLogsFixDuration(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	const theFix = "the-fix"
	cmds := &budgetCommands{timeouts: map[string]time.Duration{}, results: map[string]struct {
		exit int
		err  error
	}{}}
	d := Deps{Commands: cmds}
	rid := int64(9)
	cmds.results[theFix] = struct {
		exit int
		err  error
	}{exit: 1}
	if _, err := runCheckCommand(t.Context(), d, store.Ticket{ID: 7}, orchestrator.Worktree{}, Project{}, &rid, checkKindFix, theFix, time.Minute, nil); err != nil {
		t.Fatalf("runCheckCommand: %v", err)
	}
	cmds.results[theFix] = struct {
		exit int
		err  error
	}{exit: -1, err: ErrCommandTimeout}
	if _, err := runCheckCommand(t.Context(), d, store.Ticket{ID: 7}, orchestrator.Worktree{}, Project{}, &rid, checkKindFix, theFix, time.Minute, nil); err != nil {
		t.Fatalf("runCheckCommand: %v", err)
	}

	var records []map[string]any
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		if rec["msg"] == "command re-run" && rec["command"] == checkKindFix {
			records = append(records, rec)
		}
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v, want exactly 2", records)
	}
	for _, rec := range records {
		if rec["level"] != "INFO" {
			t.Errorf("level = %v, want INFO", rec["level"])
		}
		if rec["ticket_id"] != float64(7) {
			t.Errorf("ticket_id = %v, want 7", rec["ticket_id"])
		}
		if rec["run_id"] != float64(9) {
			t.Errorf("run_id = %v, want 9", rec["run_id"])
		}
		v, ok := rec["seconds"].(float64)
		if !ok || v < 0 || v != math.Trunc(v) {
			t.Errorf("seconds = %v, want a non-negative whole number", rec["seconds"])
		}
	}
	if records[0]["exit_code"] != float64(1) || records[0]["timed_out"] != false {
		t.Errorf("first record = %+v, want exit_code 1, timed_out false", records[0])
	}
	if records[1]["exit_code"] != float64(-1) || records[1]["timed_out"] != true {
		t.Errorf("second record = %+v, want exit_code -1, timed_out true", records[1])
	}
}
