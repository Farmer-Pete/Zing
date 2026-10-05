// checkloop_test.go tests #55's CHECK loop: Zing runs the project's test
// and lint commands itself and resumes the same session with the output of
// any that fails, capped by jobs.build.check_loops instead of max_resumes.
package job_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

const (
	checkPendingPrefix   = "check failed pending run "
	checkDeliveredPrefix = "check failed delivered run "
	claimsPendingPrefix  = "claim errors pending run "
	// checkFailingTestCmd fails with a recognizable line every time.
	checkFailingTestCmd = "echo 'FAIL: TestPing want pong'; exit 1"
	checkFailLine       = "FAIL: TestPing want pong"
	checkLoopsWhat      = "the project's fix, lint or test command still fails after the builder's fix attempts"
)

// withCheckTestCommand returns deps with ticket's project rewired to run
// testCmd, and a lint command that always passes, at CHECK.
func withCheckTestCommand(deps job.Deps, ticket store.Ticket, testCmd string) job.Deps {
	proj := deps.Projects[ticket.ProjectID]
	proj.TestCmd = testCmd
	proj.LintCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	return deps
}

// checkLoopDriver ticks one building ticket whose CHECK test command is
// testCmd, against one scripted runtime that answers every turn with an ok
// build response claiming claims.
type checkLoopDriver struct {
	t        *testing.T
	s        *store.Store
	ticketID int64
	testCmd  string
	claims   []string
	script   *scriptedRuntime
	rec      *recordingRuntime
}

func newCheckLoopDriver(t *testing.T, testCmd string, claims []string) *checkLoopDriver {
	t.Helper()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t}
	return &checkLoopDriver{t: t, s: s, ticketID: ticketID, testCmd: testCmd, claims: claims, script: script, rec: &recordingRuntime{rt: script}}
}

// tick runs one building tick, scripting one more runtime turn first when
// expectRun is set, and applies the commit.
func (c *checkLoopDriver) tick(expectRun bool) store.HandlerCommit {
	c.t.Helper()
	if expectRun {
		c.script.steps = append(c.script.steps, buildStep(c.claims, nil, "check-loop-sess"))
	}
	ticket := getTicket(c.t, c.s, c.ticketID)
	deps := withCheckTestCommand(claimForBuild(c.t, c.s, c.rec, c.ticketID), ticket, c.testCmd)
	commit, err := job.Registry()[testStateBuilding].Run(c.t.Context(), ticket, deps)
	if err != nil {
		c.t.Fatalf("building tick: %v", err)
	}
	apply(c.t, c.s, ticket, commit)
	return commit
}

// latestBuildSession is the ticket's newest build session.
func latestBuildSession(t *testing.T, s *store.Store, ticketID int64) store.Session {
	t.Helper()
	sess, _, err := s.LatestSession(t.Context(), ticketID, "build", 3)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	return sess
}

// messageWithPrefix returns the first message whose body starts with prefix.
func messageWithPrefix(msgs []store.Message, prefix string) (store.Message, bool) {
	for i := range msgs {
		if strings.HasPrefix(msgs[i].Body, prefix) {
			return msgs[i], true
		}
	}
	return store.Message{}, false
}

// pendingRunID is the run id a pending marker's first line names.
func pendingRunID(t *testing.T, body, prefix string) string {
	t.Helper()
	head, _, _ := strings.Cut(body, "\n")
	rid := strings.TrimPrefix(head, prefix)
	if _, err := strconv.ParseInt(rid, 10, 64); err != nil {
		t.Fatalf("marker %q names no run id: %v", head, err)
	}
	return rid
}

// deliveredCheckMarkers counts "check failed delivered run X" markers whose
// X is a run of sessionID.
func deliveredCheckMarkers(t *testing.T, s *store.Store, ticketID, sessionID int64) int {
	t.Helper()
	runIDs, err := s.SessionRunIDs(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("SessionRunIDs: %v", err)
	}
	rows, err := s.MarkersWithPrefix(t.Context(), ticketID, checkDeliveredPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	n := 0
	for i := range rows {
		rid, perr := strconv.ParseInt(strings.TrimPrefix(rows[i].Body, checkDeliveredPrefix), 10, 64)
		if perr == nil && slices.Contains(runIDs, rid) {
			n++
		}
	}
	return n
}

// TestCheckFailingTestResumesWithOutput proves a failing test command at
// CHECK writes one "check failed pending" marker carrying the command, its
// exit code, and its output, and that the next tick resumes the same
// session with that output fenced, free of max_resumes.
func TestCheckFailingTestResumesWithOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	c := newCheckLoopDriver(t, checkFailingTestCmd, []string{})
	c.tick(true) // RUN

	check := c.tick(false) // CHECK
	if len(check.Messages) != 1 || !strings.HasPrefix(check.Messages[0].Body, checkPendingPrefix) {
		t.Fatalf("CHECK commit.Messages = %+v, want exactly one check failed pending marker", check.Messages)
	}
	body := check.Messages[0].Body
	for _, want := range []string{"test command: echo", "exit code: 1", checkFailLine} {
		if !strings.Contains(body, want) {
			t.Errorf("marker body = %q, want it to contain %q", body, want)
		}
	}
	rid := pendingRunID(t, body, checkPendingPrefix)
	before := latestBuildSession(t, c.s, c.ticketID).Resumes

	resume := c.tick(true) // resume with the output
	prompt := c.rec.lastReq.Prompt
	label := strings.Index(prompt, "check:\n")
	fence := strings.Index(prompt, "<<<UNTRUSTED")
	fail := strings.Index(prompt, checkFailLine)
	if label < 0 || fence < label || fail < fence {
		t.Errorf("resume prompt = %q, want check:, then <<<UNTRUSTED, then the failing line", prompt)
	}
	if _, ok := messageWithPrefix(resume.Messages, checkDeliveredPrefix+rid); !ok {
		t.Errorf("resume commit.Messages = %+v, want %q", resume.Messages, checkDeliveredPrefix+rid)
	}
	if after := latestBuildSession(t, c.s, c.ticketID).Resumes; after != before {
		t.Errorf("sessions.resumes = %d after a check resume, want %d (unchanged)", after, before)
	}
}

// TestCheckPassingCommandsLandWithoutResume proves passing commands and
// matching claims land at the CHECK tick with no extra runtime turn.
func TestCheckPassingCommandsLandWithoutResume(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, nil, "land-sess")}}
	for range 2 { // RUN, then CHECK
		ticket := getTicket(t, s, ticketID)
		deps := withHelloAlwaysProject(claimForBuild(t, s, script, ticketID), ticket)
		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building tick: %v", err)
		}
		if _, ok := messageWithPrefix(commit.Messages, checkPendingPrefix); ok {
			t.Fatalf("commit.Messages = %+v, want no check failed marker", commit.Messages)
		}
		apply(t, s, ticket, commit)
	}
	reports, err := s.BuildReports(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	landed := false
	for _, r := range reports {
		if r.Report.TaskN == 1 && r.Report.CommitSHA != nil {
			landed = true
		}
	}
	if !landed {
		t.Errorf("no landed build_report for task 1 among %+v", reports)
	}
	if script.calls != 1 {
		t.Errorf("runtime calls = %d, want 1 (no resume)", script.calls)
	}
}

// driveCheckLoopToCap drives c through RUN, then check_loops (5) rounds of
// CHECK and resume, then a final CHECK, and returns the commit of the tick
// after that, which must be the loops_exhausted escalation.
func driveCheckLoopToCap(t *testing.T, c *checkLoopDriver) store.HandlerCommit {
	t.Helper()
	c.tick(true) // RUN
	for i := range 5 {
		check := c.tick(false)
		if _, ok := messageWithPrefix(check.Messages, checkPendingPrefix); !ok {
			t.Fatalf("CHECK %d commit.Messages = %+v, want a check failed pending marker", i+1, check.Messages)
		}
		c.tick(true) // resume
	}
	c.tick(false) // the sixth failing CHECK
	return c.tick(false)
}

// TestCheckLoopEscalatesAtCap proves the sixth failing CHECK after five
// delivered loops escalates loops_exhausted with the last output, without
// ever charging max_resumes.
func TestCheckLoopEscalatesAtCap(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	c := newCheckLoopDriver(t, checkFailingTestCmd, []string{})
	esc := driveCheckLoopToCap(t, c)

	if esc.Escalation == nil {
		t.Fatalf("commit = %+v, want the loops_exhausted escalation", esc)
	}
	p := esc.Escalation.Payload
	if p.Code != string(response.EscalationCodeLoopsExhausted) || p.Origin != string(response.EscalationOriginBuild) {
		t.Errorf("payload = (%q, %q), want (loops_exhausted, build)", p.Code, p.Origin)
	}
	if esc.Escalation.RunID == nil {
		t.Error("escalation RunID = nil, want the failing run's id")
	}
	if p.What != checkLoopsWhat {
		t.Errorf("payload.What = %q, want %q", p.What, checkLoopsWhat)
	}
	if !strings.Contains(p.Tried, checkFailLine) {
		t.Errorf("payload.Tried = %q, want the last output", p.Tried)
	}
	if c.script.calls != 6 {
		t.Errorf("runtime calls = %d, want 6", c.script.calls)
	}
	sess := latestBuildSession(t, c.s, c.ticketID)
	if sess.Resumes != 0 {
		t.Errorf("sessions.resumes = %d, want 0 (check loops never charge max_resumes)", sess.Resumes)
	}
	runIDs, err := c.s.SessionRunIDs(t.Context(), sess.ID)
	if err != nil {
		t.Fatalf("SessionRunIDs: %v", err)
	}
	if len(runIDs) <= 3 {
		t.Errorf("session runs = %d, want more than max_resumes (3)", len(runIDs))
	}
}

// TestCheckClaimsAndCommandFailuresShareOneResume proves claim errors and a
// failing command at the same CHECK resume once, claims first, charged to
// max_resumes, and mark both delivered.
func TestCheckClaimsAndCommandFailuresShareOneResume(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	c := newCheckLoopDriver(t, "test -f hello.txt", []string{helloTxt, phantomTxt})
	c.tick(true) // RUN: writes nothing

	check := c.tick(false)
	if len(check.Messages) != 2 ||
		!strings.HasPrefix(check.Messages[0].Body, claimsPendingPrefix) ||
		!strings.HasPrefix(check.Messages[1].Body, checkPendingPrefix) {
		t.Fatalf("CHECK commit.Messages = %+v, want the claims marker, then the check marker", check.Messages)
	}
	rid := pendingRunID(t, check.Messages[1].Body, checkPendingPrefix)
	before := latestBuildSession(t, c.s, c.ticketID).Resumes

	resume := c.tick(true)
	prompt := c.rec.lastReq.Prompt
	claims, checkAt := strings.Index(prompt, "claims:\n"), strings.Index(prompt, "check:\n")
	if claims < 0 || checkAt < claims {
		t.Errorf("resume prompt = %q, want claims: before check:", prompt)
	}
	for _, want := range []string{"claim errors delivered run " + rid, checkDeliveredPrefix + rid} {
		if _, ok := messageWithPrefix(resume.Messages, want); !ok {
			t.Errorf("resume commit.Messages = %+v, want %q", resume.Messages, want)
		}
	}
	if after := latestBuildSession(t, c.s, c.ticketID).Resumes; after != before+1 {
		t.Errorf("sessions.resumes = %d, want %d (the claims resume is charged)", after, before+1)
	}
}

// recordingCommands is a job.CommandRunner that appends each shell command
// it is asked to run to a mutex-guarded slice, in call order, then runs it
// for real.
type recordingCommands struct {
	mu    sync.Mutex
	calls []string
	real  job.CommandRunner
}

func (c *recordingCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	c.mu.Lock()
	c.calls = append(c.calls, shellCmd)
	c.mu.Unlock()
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// TestCheckLintFailureSkipsTest proves CHECK runs lint before test and
// stops at the first failed result: a failing lint command is reported and
// the test command never runs (#79).
func TestCheckLintFailureSkipsTest(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{}, nil, "lint-fail-sess")}}
	const failingLint = "echo 'lint: goconst'; exit 1"
	cmds := &recordingCommands{real: job.NewCommandRunner(sandbox.Off(), false)}

	var check store.HandlerCommit
	for range 2 { // RUN, then CHECK
		ticket := getTicket(t, s, ticketID)
		deps := claimForBuild(t, s, script, ticketID)
		proj := deps.Projects[ticket.ProjectID]
		proj.LintCmd = failingLint
		proj.TestCmd = testNoopShellCmd
		deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
		deps.Commands = cmds
		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building tick: %v", err)
		}
		apply(t, s, ticket, commit)
		check = commit
	}
	m, ok := messageWithPrefix(check.Messages, checkPendingPrefix)
	if !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	for _, want := range []string{"lint command: echo", "lint: goconst"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("marker body = %q, want %q", m.Body, want)
		}
	}
	if strings.Contains(m.Body, "test command:") {
		t.Errorf("marker body = %q, want no test command section", m.Body)
	}
	if want := []string{failingLint}; !slices.Equal(cmds.calls, want) {
		t.Errorf("recorded commands = %v, want %v", cmds.calls, want)
	}
}

// TestCheckRunsLintBeforeTest proves a passing lint command runs before the
// test command, both recorded in that order, when test then fails.
func TestCheckRunsLintBeforeTest(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{}, nil, "lint-then-test-sess")}}
	cmds := &recordingCommands{real: job.NewCommandRunner(sandbox.Off(), false)}

	var check store.HandlerCommit
	for range 2 { // RUN, then CHECK
		ticket := getTicket(t, s, ticketID)
		deps := withCheckTestCommand(claimForBuild(t, s, script, ticketID), ticket, checkFailingTestCmd)
		deps.Commands = cmds
		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building tick: %v", err)
		}
		apply(t, s, ticket, commit)
		check = commit
	}
	m, ok := messageWithPrefix(check.Messages, checkPendingPrefix)
	if !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	if !strings.Contains(m.Body, checkFailLine) {
		t.Errorf("marker body = %q, want %q", m.Body, checkFailLine)
	}
	if strings.Contains(m.Body, "lint command:") {
		t.Errorf("marker body = %q, want no lint command section", m.Body)
	}
	if want := []string{testNoopShellCmd, checkFailingTestCmd}; !slices.Equal(cmds.calls, want) {
		t.Errorf("recorded commands = %v, want %v", cmds.calls, want)
	}
}

// timeoutTestCommands times out the test command after writing partial
// output, and runs every other command for real, recording whether it did.
type timeoutTestCommands struct {
	testCmd    string
	real       job.CommandRunner
	otherCalls int
}

func (c *timeoutTestCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	if shellCmd == c.testCmd {
		if cio.Out != nil {
			if _, err := io.WriteString(cio.Out, "partial"); err != nil {
				return -1, err
			}
		}
		return -1, job.ErrCommandTimeout
	}
	c.otherCalls++
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// TestCheckCommandTimeoutResumesWithPartialOutput proves lint runs once
// before a test command killed by the budget, which reports its partial
// output with the timeout line.
func TestCheckCommandTimeoutResumesWithPartialOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{}, nil, "timeout-sess")}}
	const slowTest = "sleep 3600"
	cmds := &timeoutTestCommands{testCmd: slowTest, real: job.NewCommandRunner(sandbox.Off(), false)}

	var check store.HandlerCommit
	for range 2 { // RUN, then CHECK
		ticket := getTicket(t, s, ticketID)
		deps := withCheckTestCommand(claimForBuild(t, s, script, ticketID), ticket, slowTest)
		deps.Commands = cmds
		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building tick: %v", err)
		}
		apply(t, s, ticket, commit)
		check = commit
	}
	m, ok := messageWithPrefix(check.Messages, checkPendingPrefix)
	if !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	for _, want := range []string{"exit code: none (killed when the 45m check budget ran out)", "partial"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("marker body = %q, want %q", m.Body, want)
		}
	}
	if cmds.otherCalls != 1 {
		t.Errorf("lint ran %d times before the test command timed out, want 1", cmds.otherCalls)
	}
}

// TestCheckFixCommandLandsLintFix proves a project's fix command repairs a
// lint finding before lint runs, and that fix's own edit to the builder's
// already-changed file lands in the unit's commit while a stray path fix
// also wrote is reverted and never lands (#79 plan task 2).
func TestCheckFixCommandLandsLintFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, nil, "fix-land-sess")}}
	cmds := &recordingCommands{real: job.NewCommandRunner(sandbox.Off(), false)}

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, script, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	_, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	if writeErr := os.WriteFile(filepath.Join(wt.Dir(), helloTxt), []byte("hello, WORLD\n"), 0o600); writeErr != nil {
		t.Fatalf("write hello.txt: %v", writeErr)
	}

	const (
		fixCmd  = "printf 'hello, world\\n' > hello.txt; printf x > stray.txt; exit 1"
		lintCmd = "grep -qx 'hello, world' hello.txt"
		testCmd = "true"
	)
	ticket = getTicket(t, s, ticketID)
	deps = claimForBuild(t, s, script, ticketID)
	proj := deps.Projects[ticket.ProjectID]
	proj.FixCmd, proj.LintCmd, proj.TestCmd = fixCmd, lintCmd, testCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	deps.Commands = cmds
	check, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // CHECK
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	apply(t, s, ticket, check)

	if _, ok := messageWithPrefix(check.Messages, checkPendingPrefix); ok {
		t.Fatalf("CHECK commit.Messages = %+v, want no check failed marker", check.Messages)
	}
	reports, err := s.BuildReports(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	landed := false
	for _, r := range reports {
		if r.Report.TaskN == 1 && r.Report.CommitSHA != nil {
			landed = true
		}
	}
	if !landed {
		t.Errorf("no landed build_report for task 1 among %+v", reports)
	}
	if script.calls != 1 {
		t.Errorf("runtime calls = %d, want 1 (no resume)", script.calls)
	}
	out, err := gitfixture.Git(t.Context(), wt.Dir(), "show", "HEAD:"+helloTxt)
	if err != nil {
		t.Fatalf("git show HEAD:%s: %v", helloTxt, err)
	}
	if string(out) != "hello, world\n" {
		t.Errorf("HEAD:%s = %q, want %q", helloTxt, out, "hello, world\n")
	}
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), "stray.txt")); !os.IsNotExist(statErr) {
		t.Errorf("stray.txt exists in the worktree, want it reverted before lint ran")
	}
	if want := []string{fixCmd, lintCmd, testCmd}; !slices.Equal(cmds.calls, want) {
		t.Errorf("recorded commands = %v, want %v", cmds.calls, want)
	}
}

// TestCheckFixTimeoutFailsCheck proves a fix command that times out fails
// CHECK on its own, exactly like lint or test, and that neither lint nor
// test ever ran.
func TestCheckFixTimeoutFailsCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{}, nil, "fix-timeout-sess")}}
	const slowFix = "sleep 3600"
	cmds := &timeoutTestCommands{testCmd: slowFix, real: job.NewCommandRunner(sandbox.Off(), false)}

	var check store.HandlerCommit
	for range 2 { // RUN, then CHECK
		ticket := getTicket(t, s, ticketID)
		deps := claimForBuild(t, s, script, ticketID)
		proj := deps.Projects[ticket.ProjectID]
		proj.FixCmd = slowFix
		proj.LintCmd = testNoopShellCmd
		proj.TestCmd = testNoopShellCmd
		deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
		deps.Commands = cmds
		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building tick: %v", err)
		}
		apply(t, s, ticket, commit)
		check = commit
	}
	m, ok := messageWithPrefix(check.Messages, checkPendingPrefix)
	if !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	for _, want := range []string{"fix command: sleep 3600", "exit code: none (killed when the 45m check budget ran out)"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("marker body = %q, want %q", m.Body, want)
		}
	}
	if cmds.otherCalls != 0 {
		t.Errorf("other commands ran %d times, want 0 (a fix timeout stops CHECK)", cmds.otherCalls)
	}
}

// TestAdoptSkipsFixCommand proves adoption never runs a project's fix
// command: fix would edit the tree the "tree not clean" adoption check
// must find clean, so adopt always clears FixCmd before runCheckCommands.
// It records every command adoption actually runs (review r1f5): checking
// only that fix-ran.txt is absent cannot tell "fix never ran" apart from
// "fix ran and its stray file was reverted by the same cleanup that keeps
// fix's edits inside the unit's lane", since that cleanup would delete
// fix-ran.txt either way.
func TestAdoptSkipsFixCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID, _, _ := prepareUnrecordedCommit(t, []string{helloTxt}, nil, "", map[string]string{helloTxt: helloWorldContent}, true)

	// prepareUnrecordedCommit's own RUN already claimed and released the
	// ticket once (apply()); this tick needs its own fresh claim.
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, fakeRuntime(t), ticketID)
	proj := deps.Projects[ticket.ProjectID]
	proj.FixCmd = "printf x > fix-ran.txt"
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	cmds := &recordingCommands{real: job.NewCommandRunner(sandbox.Off(), false)}
	deps.Commands = cmds

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("adopt tick: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want none", commit.Escalation)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one landed build_report", commit.Artifacts)
	}
	if want := []string{proj.LintCmd, proj.TestCmd}; !slices.Equal(cmds.calls, want) {
		t.Errorf("recorded commands = %v, want %v (fix never ran)", cmds.calls, want)
	}
}

// cancelingFixCommands runs fixCmd by writing a stray file, canceling the
// test's own context, and returning a wrapped context.Canceled, exactly as
// a real sandboxed command looks when the tick that started it is
// canceled mid-run. Any other command goes to the real runner.
type cancelingFixCommands struct {
	fixCmd string
	cancel context.CancelFunc
	real   job.CommandRunner
}

func (c *cancelingFixCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	if shellCmd == c.fixCmd {
		if writeErr := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o600); writeErr != nil {
			return -1, writeErr
		}
		c.cancel()
		return -1, fmt.Errorf("job: command runner: %w", context.Canceled)
	}
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// TestCheckFixCleanupRunsAfterCancel proves fix's cleanup, which keeps its
// edits inside the unit's lane, runs even when the tick that started fix
// was canceled mid-run, because it runs under a context detached from the
// tick's own ctx.
func TestCheckFixCleanupRunsAfterCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{}, nil, "fix-cancel-sess")}}
	const theFix = "the-fix"

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, script, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	_, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))

	ctx, cancel := context.WithCancel(t.Context())
	cmds := &cancelingFixCommands{fixCmd: theFix, cancel: cancel, real: job.NewCommandRunner(sandbox.Off(), false)}

	ticket = getTicket(t, s, ticketID)
	deps = claimForBuild(t, s, script, ticketID)
	proj := deps.Projects[ticket.ProjectID]
	proj.FixCmd = theFix
	proj.LintCmd = testNoopShellCmd
	proj.TestCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	deps.Commands = cmds

	check, err := job.Registry()[testStateBuilding].Run(ctx, ticket, deps) // CHECK, canceled mid-fix
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want runtime.ErrCanceled", err)
	}
	if !reflect.DeepEqual(check, store.HandlerCommit{}) {
		t.Errorf("commit = %+v, want the zero value", check)
	}
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), "stray.txt")); !os.IsNotExist(statErr) {
		t.Errorf("stray.txt exists in the worktree, want the detached cleanup to have removed it")
	}
}

// TestCheckLoopCountResetsAfterFreshRetry proves an owner's retry of the
// loops_exhausted escalation starts a fresh session whose first turn
// carries the last output, and whose loop count starts again at 0.
func TestCheckLoopCountResetsAfterFreshRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	c := newCheckLoopDriver(t, checkFailingTestCmd, []string{})
	if esc := driveCheckLoopToCap(t, c); esc.Escalation == nil {
		t.Fatalf("commit = %+v, want the loops_exhausted escalation", esc)
	}
	oldSess := latestBuildSession(t, c.s, c.ticketID)

	open, err := c.s.QuestionsByState(t.Context(), c.ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly the escalation's question", open, err)
	}
	answerGateQuestion(t, c.s, c.ticketID, open[0].ID, new("a"), "try again")

	c.tick(true) // retry: a fresh session's first turn
	assertFenced(t, c.rec.lastReq.Prompt, "error", checkFailLine)
	newSess := latestBuildSession(t, c.s, c.ticketID)
	if newSess.ID == oldSess.ID {
		t.Fatalf("session after retry = %d, want a fresh one", newSess.ID)
	}

	check := c.tick(false)
	if _, ok := messageWithPrefix(check.Messages, checkPendingPrefix); !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	resume := c.tick(true)
	if resume.Escalation != nil {
		t.Fatalf("resume escalated %+v, want a resume of the fresh session", resume.Escalation.Payload)
	}
	if c.rec.lastReq.SessionID == "" {
		t.Error("resume RunRequest.SessionID is empty, want the fresh session resumed")
	}
	if n := deliveredCheckMarkers(t, c.s, c.ticketID, newSess.ID); n != 1 {
		t.Errorf("delivered check markers for the fresh session = %d, want 1", n)
	}
	if got := latestBuildSession(t, c.s, c.ticketID); got.ID != newSess.ID || got.Resumes != 0 {
		t.Errorf("session = (%d, resumes %d), want (%d, resumes 0)", got.ID, got.Resumes, newSess.ID)
	}
}

// TestCheckOutputCutToLastSixteenKiB proves long output is cut to its last
// 16384 bytes with the note naming the total.
func TestCheckOutputCutToLastSixteenKiB(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	c := newCheckLoopDriver(t, `head -c 40000 /dev/zero | tr '\0' x; echo END; exit 1`, []string{})
	c.tick(true)
	check := c.tick(false)
	m, ok := messageWithPrefix(check.Messages, checkPendingPrefix)
	if !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	if !strings.Contains(m.Body, "output (cut to the last 16384 of 40004 bytes):") {
		t.Errorf("marker body head = %q, want the cut note", m.Body[:min(len(m.Body), 200)])
	}
	if !strings.HasSuffix(m.Body, "END") {
		t.Errorf("marker body ends %q, want END", m.Body[max(0, len(m.Body)-20):])
	}
	if len(m.Body) > 16384+200 {
		t.Errorf("marker body is %d bytes, want at most %d", len(m.Body), 16384+200)
	}
}

// TestBuildInterruptedCheckResumeResendsOutput proves F009 for the check
// input: an interrupted check resume re-sends the output with the
// interrupted input, free, and the output is marked delivered once.
func TestBuildInterruptedCheckResumeResendsOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	c := newCheckLoopDriver(t, checkFailingTestCmd, []string{})
	c.tick(true) // RUN
	check := c.tick(false)
	if _, ok := messageWithPrefix(check.Messages, checkPendingPrefix); !ok {
		t.Fatalf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	ticket := getTicket(t, c.s, c.ticketID)
	deps := withCheckTestCommand(claimForBuild(t, c.s, canceledRT, c.ticketID), ticket, checkFailingTestCmd)
	if _, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps); !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want runtime.ErrCanceled", err)
	}
	if applied, err := c.s.InterruptRuns(t.Context(), c.ticketID, deps.Owner, deps.Expires); err != nil || !applied {
		t.Fatalf("InterruptRuns = (%v, %v), want (true, nil)", applied, err)
	}
	before := latestBuildSession(t, c.s, c.ticketID).Resumes

	resume := c.tick(true)
	prompt := c.rec.lastReq.Prompt
	assertFenced(t, prompt, "check", checkFailLine)
	if !strings.Contains(prompt, "the previous run was interrupted") {
		t.Errorf("resume prompt = %q, want the interrupted input too", prompt)
	}
	if resume.Session == nil || resume.Session.BumpResumes {
		t.Errorf("resume.Session = %+v, want BumpResumes false", resume.Session)
	}
	if after := latestBuildSession(t, c.s, c.ticketID).Resumes; after != before {
		t.Errorf("sessions.resumes = %d, want %d (the interrupted resume is free)", after, before)
	}
	rows, err := c.s.MarkersWithPrefix(t.Context(), c.ticketID, checkDeliveredPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("delivered check markers = %d, want exactly 1", len(rows))
	}
}

// groupLeaderCommands runs every test command as a real "sleep 30" process
// group leader, reports it through OnStart, snapshots the ticket's
// recorded CHECK command while it runs, then kills the group and returns
// exit 1. Lint passes without running anything.
type groupLeaderCommands struct {
	t        *testing.T
	s        *store.Store
	ticketID int64
	testCmd  string
	pid      int
	seen     *store.OpenCheck
}

func (c *groupLeaderCommands) Run(ctx context.Context, _, _, shellCmd string, _ time.Duration, cio job.CommandIO) (int, error) {
	if shellCmd != c.testCmd {
		return 0, nil
	}
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	c.pid = cmd.Process.Pid
	if cio.OnStart != nil {
		cio.OnStart(c.pid)
	}
	claims, err := c.s.ForeignClaims(ctx, "nobody")
	if err != nil {
		c.t.Errorf("ForeignClaims: %v", err)
	}
	for i := range claims {
		if claims[i].TicketID == c.ticketID {
			c.seen = claims[i].Check
		}
	}
	if err := syscall.Kill(-c.pid, syscall.SIGKILL); err != nil {
		c.t.Errorf("kill group %d: %v", c.pid, err)
	}
	_ = cmd.Wait() //nolint:errcheck // the group was just killed, so Wait reports the kill
	return 1, nil
}

// TestCheckRecordsAndClearsCommandGroup proves CHECK records the running
// command's process group in check_procs (plan D10) while it runs, with
// the budget start no later than the command start, and clears it once the
// command ends.
func TestCheckRecordsAndClearsCommandGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	script := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{}, nil, "group-sess")}}
	const groupTest = "the group test"
	cmds := &groupLeaderCommands{t: t, s: s, ticketID: ticketID, testCmd: groupTest}

	var check store.HandlerCommit
	for range 2 { // RUN, then CHECK
		ticket := getTicket(t, s, ticketID)
		deps := withCheckTestCommand(claimForBuild(t, s, script, ticketID), ticket, groupTest)
		deps.Commands = cmds
		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building tick: %v", err)
		}
		check = commit
		if cmds.pid != 0 { // CHECK ran; read the row before the claim is released
			claims, cerr := s.ForeignClaims(t.Context(), "nobody")
			if cerr != nil {
				t.Fatalf("ForeignClaims: %v", cerr)
			}
			for i := range claims {
				if claims[i].TicketID == ticketID && claims[i].Check != nil {
					t.Errorf("check_procs row %+v after the command ended, want none", claims[i].Check)
				}
			}
		}
		apply(t, s, ticket, commit)
	}
	if _, ok := messageWithPrefix(check.Messages, checkPendingPrefix); !ok {
		t.Errorf("CHECK commit.Messages = %+v, want a check failed pending marker", check.Messages)
	}
	c := cmds.seen
	if c == nil {
		t.Fatal("no check_procs row while the command ran")
	}
	if c.Kind != "test" || c.PGID != cmds.pid {
		t.Errorf("row = %+v, want kind test and pgid %d", c, cmds.pid)
	}
	if c.ProcStart == nil {
		t.Error("row proc_start is NULL, want the group leader's start token")
	}
	if c.BudgetStartedAt.After(c.StartedAt) {
		t.Errorf("budget_started_at %v is after started_at %v", c.BudgetStartedAt, c.StartedAt)
	}
}
