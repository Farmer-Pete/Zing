// judging_host_test.go tests #49 task 3: host-kind scenario checks wired
// into judging (design section 5, 7): the host check tick
// (runPendingHostCheck, hostCheck), the host result read (judgeHostResults,
// judgeHostResultsAt, judgeHostResultFor, judgePendingHost), the host_checks
// prompt input (judgeHostChecksText, runFirst), CHECK's own host branch
// (hostVerdict, applyHostCheckExit), and the stale-command fresh-round path
// (retryFreshRound's own pending-host guard). It reuses judging_test.go's
// own harness (judgeAdvanceStart, judgeWorktreeDir, reviewMarker,
// judgeScriptsFS, judgeOkBothScript, recordingRuntime) and
// postbuild_test.go's own pbTicketInReviewingWith, package job (unreachable
// from job_test).
package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"zing/fixtures"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// ---- fixture: a ticket whose sealed s1 is kind host -----------------------

// judgeHostScriptsFS copies fixtures.FS's own scripts tree into an
// in-memory fs.FS and changes s1's kind in the planning "ready" turn
// (fixtures/scripts/planning/2.xml) from behavior to host: everything else
// -- s1's own check ("curl -sf localhost:8080/hello", judgeCheckScenarioCmd),
// s2 (negative, no check), and the three build turns -- is byte-identical
// to the real fixture tree pbFakeRuntime serves.
func judgeHostScriptsFS(t *testing.T) fstest.MapFS {
	t.Helper()
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("judgeHostScriptsFS: fs.Sub(scripts): %v", err)
	}
	m := make(fstest.MapFS)
	walkErr := fs.WalkDir(scriptsFS, ".", func(path string, d fs.DirEntry, walkDirErr error) error {
		if walkDirErr != nil {
			return walkDirErr
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(scriptsFS, path)
		if readErr != nil {
			return readErr
		}
		if path == "planning/2.xml" {
			patched := strings.Replace(string(data), `id="s1" kind="behavior"`, `id="s1" kind="host"`, 1)
			if patched == string(data) {
				t.Fatalf(`judgeHostScriptsFS: %s: id="s1" kind="behavior" not found`, path)
			}
			data = []byte(patched)
		}
		m[path] = &fstest.MapFile{Data: data}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("judgeHostScriptsFS: walk: %v", walkErr)
	}
	return m
}

// judgeHostTicketReady is judgeTicketReady (judging_test.go) with s1 of
// kind host instead of behavior: a fresh, git-backed ticket driven through
// planning (judgeHostScriptsFS's own patched fixture), a real three-task
// build, and one clean review round into "judging", with the sealed
// cohort s1 (host, check judgeCheckScenarioCmd) and s2 (negative, no
// check), and no judge round marker yet.
func judgeHostTicketReady(t *testing.T) (s *store.Store, ticket store.Ticket) {
	t.Helper()
	s, ticketID := pbTicketInReviewingWith(t, runtime.NewFake(judgeHostScriptsFS(t)))
	reviewTicket := pbGetTicket(t, s, ticketID)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), reviewTicket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), reviewTicket, deps)
	if err != nil {
		t.Fatalf("judgeHostTicketReady: review Run: %v", err)
	}
	if commit.Next != stateJudging {
		t.Fatalf("judgeHostTicketReady: review commit.Next = %q, want %q", commit.Next, stateJudging)
	}
	pbApply(t, s, reviewTicket, commit)
	return s, pbGetTicket(t, s, reviewTicket.ID)
}

// ---- a scripted Deps.HostCommands ------------------------------------------

// judgeHostCheckStep scripts one Deps.HostCommands.Run call's own outcome.
type judgeHostCheckStep struct {
	exit   int
	err    error
	output string
}

// judgeHostOutputRefused and judgeHostOutputPartial are the two output
// fixtures most tests below script a host check to write.
const (
	judgeHostOutputRefused = "connection refused\n"
	judgeHostOutputPartial = "partial\n"
)

// judgeHostCall records one call judgeScriptedHostCommands served: the dir
// hostCheck ran it in, and the exact command.
type judgeHostCall struct {
	dir string
	cmd string
}

// judgeScriptedHostCommands is a scripted job.CommandRunner for
// Deps.HostCommands: each call writes step.output, when set, to cio.Out and
// returns step.exit, step.err, replaying steps in order; a call past the
// scripted steps returns exit 0, nil.
type judgeScriptedHostCommands struct {
	mu    sync.Mutex
	steps []judgeHostCheckStep
	i     int
	calls []judgeHostCall
}

func (c *judgeScriptedHostCommands) Run(_ context.Context, dir, _, shellCmd string, _ time.Duration, cio CommandIO) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, judgeHostCall{dir: dir, cmd: shellCmd})
	if c.i >= len(c.steps) {
		return 0, nil
	}
	step := c.steps[c.i]
	c.i++
	if cio.Out != nil && step.output != "" {
		_, _ = cio.Out.Write([]byte(step.output)) //nolint:errcheck // a tailBuffer (job.go's own CommandIO.Out) never errors
	}
	return step.exit, step.err
}

func (c *judgeScriptedHostCommands) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *judgeScriptedHostCommands) lastCall() judgeHostCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1]
}

// ---- small test helpers -----------------------------------------------------

// judgeAdvanceHostCheck runs and applies one host-check tick (RUN's own
// enterAfterStart -> runPendingHostCheck branch): it fails the test unless
// the commit carries no run.
func judgeAdvanceHostCheck(t *testing.T, s *store.Store, rt runtime.Runtime, ticket store.Ticket, hostCommands CommandRunner) store.Ticket {
	t.Helper()
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judgeAdvanceHostCheck: Run: %v", err)
	}
	if len(commit.Runs) != 0 {
		t.Fatalf("judgeAdvanceHostCheck: commit.Runs = %+v, want none", commit.Runs)
	}
	pbApply(t, s, ticket, commit)
	return pbGetTicket(t, s, ticket.ID)
}

// judgeRoundSHA reads round n's own frozen sha back from ticketID's "judge
// round " markers (judgeStartedSHA).
func judgeRoundSHA(t *testing.T, s *store.Store, ticketID int64, n int) string { //nolint:unparam // every test below reads round 1's own sha, but the helper mirrors judgeStartedSHA's own general n parameter
	t.Helper()
	markers, err := s.MarkersWithPrefix(t.Context(), ticketID, judgeRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("judgeRoundSHA: MarkersWithPrefix: %v", err)
	}
	sha, err := judgeStartedSHA(markers, n)
	if err != nil {
		t.Fatalf("judgeRoundSHA: judgeStartedSHA: %v", err)
	}
	return sha
}

// judgeInsertRoundStartedMarker inserts one "judge round <n> started sha
// <sha> after run <afterRunID>" marker directly, the same way an owner
// retry's own commit would, so a test can put a ticket at round n's start
// without redriving round n-1's whole judge turn.
func judgeInsertRoundStartedMarker(t *testing.T, s *store.Store, ticket store.Ticket, n int, sha string, afterRunID int64) {
	t.Helper()
	msg := store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge round %d started sha %s after run %d", n, sha, afterRunID),
	}
	if _, err := s.InsertMessage(t.Context(), msg); err != nil {
		t.Fatalf("judgeInsertRoundStartedMarker: InsertMessage: %v", err)
	}
}

// hostMarkerRow and roundMarkerRow build a bare store.MessageRow around
// body, for judgeHostResults' own pure-function tests, which read only
// Body.
func hostMarkerRow(body string) store.MessageRow  { return store.MessageRow{Body: body} }
func roundMarkerRow(body string) store.MessageRow { return store.MessageRow{Body: body} }

// ---- TestJudgeHostCheckFlow -------------------------------------------------

// TestJudgeHostCheckFlow proves the host check tick and the host_checks
// prompt input (design section 5, 7.2): the first tick after START runs
// the host check, not the judge, and commits only its own marker; the
// second tick runs the judge, without running the host check again, with
// a prompt carrying the recorded exit code and output.
func TestJudgeHostCheckFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 3, output: judgeHostOutputRefused}}}
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("host check tick: %v", err)
	}
	if len(commit.Runs) != 0 {
		t.Fatalf("commit.Runs = %+v, want none", commit.Runs)
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one", commit.Messages)
	}
	wantHash := judgeHostCmdHash(judgeCheckScenarioCmd)
	wantBody := fmt.Sprintf("judge host 1 s1 exit 3 cmd %s\n%s", wantHash, judgeHostOutputRefused)
	if commit.Messages[0].Body != wantBody {
		t.Errorf("message body = %q, want %q", commit.Messages[0].Body, wantBody)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	if hostCommands.callCount() != 1 {
		t.Fatalf("host runner called %d times, want 1", hostCommands.callCount())
	}
	wantDir := judgeWorktreeDir(t, s, ticket.ID)
	gotCall := hostCommands.lastCall()
	if gotCall.cmd != judgeCheckScenarioCmd {
		t.Errorf("host runner cmd = %q, want %q", gotCall.cmd, judgeCheckScenarioCmd)
	}
	if gotCall.dir != wantDir {
		t.Errorf("host runner dir = %q, want the judge checkout %q", gotCall.dir, wantDir)
	}

	rec := &recordingRuntime{inner: rt}
	deps2 := pbClaim(t, s, rec, ticket.ID)
	deps2.HostCommands = hostCommands
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("judge run tick: %v", err)
	}
	if len(runCommit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one run", runCommit.Runs)
	}
	if hostCommands.callCount() != 1 {
		t.Errorf("host runner called %d times after the judge run, want still 1", hostCommands.callCount())
	}
	promptText := rec.lastRequest(t).Prompt
	if !strings.Contains(promptText, "host_checks:") {
		t.Errorf("prompt does not contain %q", "host_checks:")
	}
	if !strings.Contains(promptText, "scenario s1 exit 3\nconnection refused") {
		t.Errorf("prompt does not contain the host result block; prompt = %q", promptText)
	}
	pbApply(t, s, ticket, runCommit)
}

// TestJudgeHostCheckNotRerunByCheck proves CHECK's own host branch
// (hostVerdict, design section 7.5): it runs nothing, turns the recorded
// exit into the override row, and a failing host verdict fails the round.
func TestJudgeHostCheckNotRerunByCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 3, output: judgeHostOutputRefused}}}
	ticket = judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)

	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judge run: %v", err)
	}
	pbApply(t, s, ticket, runCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	checks := &judgeScriptedCheckCommands{}
	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.Commands = checks
	deps2.HostCommands = hostCommands
	checkCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one override row", checkCommit.Artifacts)
	}
	var override response.VerdictArtifact
	if unmarshalErr := json.Unmarshal(checkCommit.Artifacts[0].Payload, &override); unmarshalErr != nil {
		t.Fatalf("unmarshal override: %v", unmarshalErr)
	}
	if override.Scenario != "s1" {
		t.Errorf("override.Scenario = %q, want s1", override.Scenario)
	}
	if override.Result != response.ResultFail {
		t.Errorf("override.Result = %q, want fail", override.Result)
	}
	if override.CheckExit == nil || *override.CheckExit != 3 {
		t.Errorf("override.CheckExit = %v, want 3", override.CheckExit)
	}
	if !strings.HasSuffix(override.Evidence, "Zing ran the check command on the host before judging: exit 3.") {
		t.Errorf("override.Evidence = %q, want it to end with the host note", override.Evidence)
	}
	pbApply(t, s, ticket, checkCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	marker, ok := reviewMarker(t, s, ticket.ID, "judge check 1 s1 exit")
	if !ok {
		t.Fatal(`no "judge check 1 s1 exit" marker`)
	}
	if marker.Body != "judge check 1 s1 exit 3" {
		t.Errorf("marker body = %q, want %q", marker.Body, "judge check 1 s1 exit 3")
	}
	if checks.i != 0 {
		t.Errorf("judgeScriptedCheckCommands.i = %d, want 0 (CHECK must not re-run a host scenario)", checks.i)
	}
	if hostCommands.callCount() != 1 {
		t.Errorf("host runner called %d times, want still 1", hostCommands.callCount())
	}

	deps3 := pbClaim(t, s, rt, ticket.ID)
	deps3.HostCommands = hostCommands
	evalCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps3)
	if err != nil {
		t.Fatalf("EVALUATE: %v", err)
	}
	if !strings.HasPrefix(evalCommit.Messages[0].Body, "judge round 1 failed") {
		t.Fatalf("EVALUATE message = %q, want it to start with %q", evalCommit.Messages[0].Body, "judge round 1 failed")
	}
	if !strings.Contains(evalCommit.Messages[0].Body, "s1") {
		t.Errorf("EVALUATE message = %q, want it to name s1", evalCommit.Messages[0].Body)
	}
}

// TestJudgeHostCheckZeroExitPasses proves a passing host check counts like
// a passing behavior check (design section 7.4's own host row, JudgePasses).
func TestJudgeHostCheckZeroExitPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 0}}}
	ticket = judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)

	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judge run: %v", err)
	}
	pbApply(t, s, ticket, runCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.HostCommands = hostCommands
	checkCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	pbApply(t, s, ticket, checkCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	deps3 := pbClaim(t, s, rt, ticket.ID)
	deps3.HostCommands = hostCommands
	evalCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps3)
	if err != nil {
		t.Fatalf("EVALUATE: %v", err)
	}
	if evalCommit.Next != stateShipping {
		t.Errorf("commit.Next = %q, want %q", evalCommit.Next, stateShipping)
	}
}

// TestJudgeHostCheckReusedAtSameSha proves a later round at the same sha
// reuses the newest recorded host result instead of running the check
// again (design section 5's own "once per frozen sha" rule).
func TestJudgeHostCheckReusedAtSameSha(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 3, output: judgeHostOutputRefused}}}
	ticket = judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)

	sha := judgeRoundSHA(t, s, ticket.ID, 1)
	maxRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	judgeInsertRoundStartedMarker(t, s, ticket, 2, sha, maxRunID)
	ticket = pbGetTicket(t, s, ticket.ID)

	rec := &recordingRuntime{inner: rt}
	deps := pbClaim(t, s, rec, ticket.ID)
	deps.HostCommands = hostCommands
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judge run: %v", err)
	}
	if len(runCommit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one run", runCommit.Runs)
	}
	if hostCommands.callCount() != 1 {
		t.Errorf("host runner called %d times, want still 1 (reused, not re-run)", hostCommands.callCount())
	}
	promptText := rec.lastRequest(t).Prompt
	if !strings.Contains(promptText, "scenario s1 exit 3") {
		t.Errorf("prompt does not contain the reused host result; prompt = %q", promptText)
	}
}

// TestJudgeHostCheckRerunAfterOwnerEdit proves an owner edit of a host
// scenario's check forces a fresh run at the same sha (design section 5):
// the edit changes the key a lookup hashes, so the old result no longer
// matches.
func TestJudgeHostCheckRerunAfterOwnerEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{
		{exit: 3, output: judgeHostOutputRefused},
		{exit: 0},
	}}
	ticket = judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)

	sha := judgeRoundSHA(t, s, ticket.ID, 1)
	newCheck := judgeCheckScenarioCmd + "2"
	if err := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticket.ID, Target: store.OwnerEditScenario, Ref: "s1", Action: store.OwnerEditActionEdit, Check: &newCheck,
	}); err != nil {
		t.Fatalf("OwnerEdit: %v", err)
	}

	maxRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	judgeInsertRoundStartedMarker(t, s, ticket, 2, sha, maxRunID)
	ticket = pbGetTicket(t, s, ticket.ID)

	commit := judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)
	_ = commit

	if hostCommands.callCount() != 2 {
		t.Fatalf("host runner called %d times, want 2", hostCommands.callCount())
	}
	gotCall := hostCommands.lastCall()
	if gotCall.cmd != newCheck {
		t.Errorf("host runner cmd = %q, want the edited check %q", gotCall.cmd, newCheck)
	}

	marker, ok := reviewMarker(t, s, ticket.ID, "judge host 2 s1 exit")
	if !ok {
		t.Fatal(`no "judge host 2 s1 exit" marker`)
	}
	wantPrefix := "judge host 2 s1 exit 0 cmd " + judgeHostCmdHash(newCheck)
	if !strings.HasPrefix(marker.Body, wantPrefix) {
		t.Errorf("marker body = %q, want it to start with %q", marker.Body, wantPrefix)
	}
}

// TestJudgeHostVerdictStaleCommandStartsFreshRound proves hostVerdict's own
// stale-command path (design section 5): an owner edit between the judge's
// own verdicts and CHECK leaves no result for the scenario's current key,
// so CHECK starts a fresh round at the same sha instead of judging stale
// output.
func TestJudgeHostVerdictStaleCommandStartsFreshRound(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{
		{exit: 3, output: judgeHostOutputRefused},
		{exit: 0},
	}}
	ticket = judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)

	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judge run: %v", err)
	}
	pbApply(t, s, ticket, runCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	sha := judgeRoundSHA(t, s, ticket.ID, 1)
	newCheck := judgeCheckScenarioCmd + "2"
	if editErr := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticket.ID, Target: store.OwnerEditScenario, Ref: "s1", Action: store.OwnerEditActionEdit, Check: &newCheck,
	}); editErr != nil {
		t.Fatalf("OwnerEdit: %v", editErr)
	}

	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.HostCommands = hostCommands
	checkCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Artifacts) != 0 {
		t.Fatalf("commit.Artifacts = %+v, want none", checkCommit.Artifacts)
	}
	if len(checkCommit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one", checkCommit.Messages)
	}
	wantPrefix := "judge round 2 started sha " + sha + " after run "
	if !strings.HasPrefix(checkCommit.Messages[0].Body, wantPrefix) {
		t.Errorf("message = %q, want it to start with %q", checkCommit.Messages[0].Body, wantPrefix)
	}
	pbApply(t, s, ticket, checkCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	nextTicket := judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)
	_ = nextTicket
	if hostCommands.callCount() != 2 {
		t.Fatalf("host runner called %d times, want 2", hostCommands.callCount())
	}
	gotCall := hostCommands.lastCall()
	if gotCall.cmd != newCheck {
		t.Errorf("host runner cmd = %q, want the edited check %q", gotCall.cmd, newCheck)
	}
}

// TestJudgeHostCheckTimeout proves hostCheck's own timeout row (design
// section 5): ErrCommandTimeout forces exit -1 regardless of the runner's
// own returned exit code, and the partial output written before the
// timeout is kept.
func TestJudgeHostCheckTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 0, err: ErrCommandTimeout, output: judgeHostOutputPartial}}}
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("host check tick: %v", err)
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one", commit.Messages)
	}
	body := commit.Messages[0].Body
	if !strings.HasPrefix(body, "judge host 1 s1 exit -1 cmd ") {
		t.Errorf("message = %q, want it to start with %q", body, "judge host 1 s1 exit -1 cmd ")
	}
	_, output, _ := strings.Cut(body, "\n")
	if output != judgeHostOutputPartial {
		t.Errorf("output = %q, want %q", output, judgeHostOutputPartial)
	}
}

// TestJudgeHostCheckOutputCapped proves hostCheck caps its own marker body
// at judgeHostOutputCap bytes (design section 5, judgeCapOutput).
func TestJudgeHostCheckOutputCapped(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	output := strings.Repeat("a", 69997) + "END"
	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 0, output: output}}}
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("host check tick: %v", err)
	}
	_, got, _ := strings.Cut(commit.Messages[0].Body, "\n")
	if len(got) != judgeHostOutputCap {
		t.Errorf("output length = %d, want %d", len(got), judgeHostOutputCap)
	}
	if !strings.HasSuffix(got, "END") {
		t.Errorf("output does not end with END: %q", got[max(0, len(got)-10):])
	}
}

// TestJudgeHostCheckNoRunnerIsConfigError proves hostCheck refuses to run a
// host check with no runner wired (design section 5, Deps.HostCommands).
func TestJudgeHostCheckNoRunnerIsConfigError(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	if len(commit.Messages) != 0 {
		t.Errorf("commit.Messages = %+v, want none", commit.Messages)
	}
}

// ---- TestJudgeHostCheckLogs -------------------------------------------------

// recordedLog is one slog record recordingLogHandler captured: the message
// and every attribute, flattened.
type recordedLog struct {
	msg   string
	attrs map[string]any
}

// recordingLogHandler is a minimal slog.Handler that records every record
// it handles, in call order, so TestJudgeHostCheckLogs can assert on the
// logging table's own messages and attributes directly, without a sink.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []recordedLog
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, recordedLog{msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

// byMessage returns the first recorded record whose Message is msg.
func (h *recordingLogHandler) byMessage(msg string) (recordedLog, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.msg == msg {
			return r, true
		}
	}
	return recordedLog{}, false
}

// allText renders every recorded message and attribute value as one
// string, so a test can assert a substring is absent from the whole log.
func (h *recordingLogHandler) allText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sb strings.Builder
	for _, r := range h.records {
		sb.WriteString(r.msg)
		for k, v := range r.attrs {
			fmt.Fprintf(&sb, " %s=%v", k, v)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// attrInt64 and attrString read one attribute back as the type slog gives
// a plain int or string argument (AnyValue special-cases int as Int64Kind,
// whose Any() returns int64), failing the test on a missing or
// differently-typed attribute.
func attrInt64(t *testing.T, r recordedLog, key string) int64 {
	t.Helper()
	v, ok := r.attrs[key]
	if !ok {
		t.Fatalf("%s: no attribute %q", r.msg, key)
	}
	i, ok := v.(int64)
	if !ok {
		t.Fatalf("%s: attribute %q = %v (%T), want int64", r.msg, key, v, v)
	}
	return i
}

func attrString(t *testing.T, r recordedLog, key string) string {
	t.Helper()
	v, ok := r.attrs[key]
	if !ok {
		t.Fatalf("%s: no attribute %q", r.msg, key)
	}
	str, ok := v.(string)
	if !ok {
		t.Fatalf("%s: attribute %q = %v (%T), want string", r.msg, key, v, v)
	}
	return str
}

// assertHostLogIdentity checks the five identity attributes every host-check
// log record in the design's logging table carries: ticket_id, round,
// scenario_id, sha, cmd_sha256.
func assertHostLogIdentity(t *testing.T, r recordedLog, ticketID int64, round int, scenarioID, sha, cmdHash string) {
	t.Helper()
	if got := attrInt64(t, r, "ticket_id"); got != ticketID {
		t.Errorf("%s: ticket_id = %d, want %d", r.msg, got, ticketID)
	}
	if got := attrInt64(t, r, "round"); got != int64(round) {
		t.Errorf("%s: round = %d, want %d", r.msg, got, round)
	}
	if got := attrString(t, r, "scenario_id"); got != scenarioID {
		t.Errorf("%s: scenario_id = %q, want %q", r.msg, got, scenarioID)
	}
	if got := attrString(t, r, "sha"); got != sha {
		t.Errorf("%s: sha = %q, want %q", r.msg, got, sha)
	}
	if got := attrString(t, r, "cmd_sha256"); got != cmdHash {
		t.Errorf("%s: cmd_sha256 = %q, want %q", r.msg, got, cmdHash)
	}
}

// TestJudgeHostCheckLogs proves the design's own logging table for a host
// check's start, finish, and verdict: every record carries ticket_id,
// round, scenario_id, sha, and cmd_sha256, and none carries the check's
// own output. It swaps the process-wide default slog logger, so it does
// not call t.Parallel.
func TestJudgeHostCheckLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	rec := &recordingLogHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })

	s, ticket := judgeHostTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	hostCommands := &judgeScriptedHostCommands{steps: []judgeHostCheckStep{{exit: 3, output: judgeHostOutputRefused}}}
	ticket = judgeAdvanceHostCheck(t, s, rt, ticket, hostCommands)

	deps := pbClaim(t, s, rt, ticket.ID)
	deps.HostCommands = hostCommands
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judge run: %v", err)
	}
	pbApply(t, s, ticket, runCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.HostCommands = hostCommands
	checkCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	pbApply(t, s, ticket, checkCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	sha := judgeRoundSHA(t, s, ticket.ID, 1)
	wantHash := judgeHostCmdHash(judgeCheckScenarioCmd)

	started, ok := rec.byMessage("judge host check started")
	if !ok {
		t.Fatal(`no "judge host check started" record`)
	}
	assertHostLogIdentity(t, started, ticket.ID, 1, "s1", sha, wantHash)

	finished, ok := rec.byMessage("judge host check finished")
	if !ok {
		t.Fatal(`no "judge host check finished" record`)
	}
	assertHostLogIdentity(t, finished, ticket.ID, 1, "s1", sha, wantHash)
	if got := attrInt64(t, finished, "exit"); got != 3 {
		t.Errorf("finished exit = %d, want 3", got)
	}

	verdict, ok := rec.byMessage("judge host verdict")
	if !ok {
		t.Fatal(`no "judge host verdict" record`)
	}
	assertHostLogIdentity(t, verdict, ticket.ID, 1, "s1", sha, wantHash)
	if got := attrString(t, verdict, "result"); got != string(response.ResultFail) {
		t.Errorf("verdict result = %q, want %q", got, response.ResultFail)
	}

	if strings.Contains(rec.allText(), "connection refused") {
		t.Error("a log record carries the check's own output")
	}
}

// ---- pure-function unit tests -----------------------------------------------

// TestJudgeCapOutput proves judgeCapOutput's own byte cap and UTF-8 repair
// (design section 5).
func TestJudgeCapOutput(t *testing.T) {
	t.Parallel()
	const limit = 65536

	t.Run("ascii_overflow_keeps_the_tail", func(t *testing.T) {
		t.Parallel()
		raw := bytes.Repeat([]byte("a"), 70000)
		got := judgeCapOutput(raw, limit)
		want := string(raw[len(raw)-limit:])
		if got != want {
			t.Errorf("got %d bytes, want the last %d bytes of raw", len(got), limit)
		}
	})

	t.Run("a_cut_landing_inside_a_multibyte_rune_keeps_it_whole", func(t *testing.T) {
		t.Parallel()
		raw := append(bytes.Repeat([]byte("a"), 10+65535), []byte("€")...)
		got := judgeCapOutput(raw, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("result is not valid UTF-8: %q", got)
		}
		if len(got) > limit {
			t.Errorf("len(got) = %d, want at most %d", len(got), limit)
		}
		if !strings.HasSuffix(got, "€") {
			t.Errorf("result does not end with the euro sign: %q", got)
		}
		if r, _ := utf8.DecodeRuneInString(got); r == utf8.RuneError {
			t.Errorf("result does not start on a rune boundary: %q", got)
		}
	})

	t.Run("a_cut_landing_inside_continuation_bytes_drops_them", func(t *testing.T) {
		t.Parallel()
		// 0x80 and 0x81 are both continuation bytes (not a rune start);
		// raw's own length equals limit, so no byte-slicing cut happens,
		// and judgeCapOutput's own leading-continuation-byte skip is what
		// drops exactly these two, one at a time.
		raw := append([]byte{0x80, 0x81}, bytes.Repeat([]byte("a"), limit-2)...)
		got := judgeCapOutput(raw, limit)
		want := strings.Repeat("a", limit-2)
		if got != want {
			t.Errorf("got %d bytes starting %q, want exactly %d a's", len(got), got[:min(4, len(got))], limit-2)
		}
	})

	t.Run("invalid_utf8_becomes_u_fffd_capped_at_the_limit", func(t *testing.T) {
		t.Parallel()
		raw := bytes.Repeat([]byte{0xff}, limit)
		got := judgeCapOutput(raw, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("result is not valid UTF-8: %q", got)
		}
		if got == "" || len(got) > limit {
			t.Fatalf("len(got) = %d, want (0, %d]", len(got), limit)
		}
		for _, r := range got {
			if r != utf8.RuneError {
				t.Errorf("result contains a rune other than U+FFFD: %q", r)
			}
		}
	})

	t.Run("a_smaller_limit_caps_the_same_way", func(t *testing.T) {
		t.Parallel()
		got := judgeCapOutput([]byte("hello world"), 5)
		if got != "world" {
			t.Errorf("got %q, want %q", got, "world")
		}
	})
}

// TestJudgeHostResults proves judgeHostResults' own dedup and sha scoping
// (design section 5): the newest marker per (scenario, command hash) wins,
// a different hash is a separate entry, a marker from a round started at a
// different sha is ignored, and an unparseable marker, or one whose round
// has no started marker, is an error.
func TestJudgeHostResults(t *testing.T) {
	t.Parallel()
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	hashA := judgeHostCmdHash("cmd a")
	hashB := judgeHostCmdHash("cmd b")
	roundMarkers := []store.MessageRow{
		roundMarkerRow("judge round 1 started sha " + shaA + " after run 1"),
		roundMarkerRow("judge round 2 started sha " + shaA + " after run 2"),
		roundMarkerRow("judge round 3 started sha " + shaB + " after run 3"),
	}

	t.Run("newest_marker_per_key_wins", func(t *testing.T) {
		t.Parallel()
		hostMarkers := []store.MessageRow{
			hostMarkerRow(fmt.Sprintf("judge host 1 s1 exit 3 cmd %s\nround1 output", hashA)),
			hostMarkerRow(fmt.Sprintf("judge host 2 s1 exit 0 cmd %s\nround2 output", hashA)),
		}
		got, err := judgeHostResults(hostMarkers, roundMarkers, shaA)
		if err != nil {
			t.Fatalf("judgeHostResults: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1", len(got))
		}
		r := got[judgeHostKey{ScenarioID: "s1", CmdSHA256: hashA}]
		if r.Exit != 0 || r.Output != "round2 output" {
			t.Errorf("got %+v, want exit 0, output %q", r, "round2 output")
		}
	})

	t.Run("different_hashes_give_separate_entries", func(t *testing.T) {
		t.Parallel()
		hostMarkers := []store.MessageRow{
			hostMarkerRow(fmt.Sprintf("judge host 1 s1 exit 3 cmd %s\nfirst", hashA)),
			hostMarkerRow(fmt.Sprintf("judge host 1 s1 exit 0 cmd %s\nsecond", hashB)),
		}
		got, err := judgeHostResults(hostMarkers, roundMarkers, shaA)
		if err != nil {
			t.Fatalf("judgeHostResults: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2", len(got))
		}
	})

	t.Run("a_marker_from_a_round_started_at_a_different_sha_is_ignored", func(t *testing.T) {
		t.Parallel()
		hostMarkers := []store.MessageRow{
			hostMarkerRow(fmt.Sprintf("judge host 3 s1 exit 0 cmd %s\nthird", hashA)),
		}
		got, err := judgeHostResults(hostMarkers, roundMarkers, shaA)
		if err != nil {
			t.Fatalf("judgeHostResults: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want empty (round 3 started at sha B, not A)", got)
		}
	})

	t.Run("an_unparseable_marker_is_an_error", func(t *testing.T) {
		t.Parallel()
		for _, body := range []string{
			"judge host 1 s1 exit x cmd " + hashA,
			"judge host 1 s1 exit 0",
		} {
			if _, err := judgeHostResults([]store.MessageRow{hostMarkerRow(body)}, roundMarkers, shaA); err == nil {
				t.Errorf("judgeHostResults(%q): want an error", body)
			}
		}
	})

	t.Run("a_marker_whose_round_has_no_started_marker_is_an_error", func(t *testing.T) {
		t.Parallel()
		hostMarkers := []store.MessageRow{hostMarkerRow("judge host 9 s1 exit 0 cmd " + hashA)}
		if _, err := judgeHostResults(hostMarkers, roundMarkers, shaA); err == nil {
			t.Error("want an error for a round with no started marker")
		}
	})
}

// TestJudgeHostChecksText proves judgeHostChecksText's own rendering
// (design section 5): one block per host scenario in cohort order, a
// timeout annotation on exit -1, non-host scenarios left out, and an error
// naming a host scenario whose result is stored under a different hash.
func TestJudgeHostChecksText(t *testing.T) {
	t.Parallel()
	s1 := response.Scenario{ID: "s1", Kind: response.ScenarioKindHost, Check: "cmd one"}
	s2 := response.Scenario{ID: "s2", Kind: response.ScenarioKindBehavior, Check: "cmd two"}
	s4 := response.Scenario{ID: "s4", Kind: response.ScenarioKindHost, Check: "cmd four"}
	scenarios := []response.Scenario{s1, s2, s4}

	results := map[judgeHostKey]judgeHostResult{
		{ScenarioID: "s1", CmdSHA256: judgeHostCmdHash(s1.Check)}: {Exit: 3, Output: judgeHostOutputRefused},
		{ScenarioID: "s4", CmdSHA256: judgeHostCmdHash(s4.Check)}: {Exit: -1, Output: "partial"},
	}

	got, err := judgeHostChecksText(scenarios, results)
	if err != nil {
		t.Fatalf("judgeHostChecksText: %v", err)
	}
	want := "scenario s1 exit 3\nconnection refused\n\nscenario s4 exit -1 (timed out after 10m)\npartial"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	t.Run("a_result_under_a_different_hash_is_an_error_naming_the_scenario", func(t *testing.T) {
		t.Parallel()
		stale := map[judgeHostKey]judgeHostResult{
			{ScenarioID: "s1", CmdSHA256: judgeHostCmdHash("edited")}: {Exit: 0},
			{ScenarioID: "s4", CmdSHA256: judgeHostCmdHash(s4.Check)}: {Exit: -1},
		}
		_, err := judgeHostChecksText(scenarios, stale)
		if err == nil || !strings.Contains(err.Error(), "s1") {
			t.Errorf("err = %v, want an error naming s1", err)
		}
	})
}

// TestApplyHostCheckExit proves applyHostCheckExit's own exit rule (design
// section 5, judgerules.go): pass only on exit 0, CheckExit set to exit,
// and the evidence note naming the host run Zing already made.
func TestApplyHostCheckExit(t *testing.T) {
	t.Parallel()
	judged := response.VerdictArtifact{Scenario: "s1", Result: response.ResultPass, Evidence: "the judge observed it pass"}

	t.Run("exit_0_passes", func(t *testing.T) {
		t.Parallel()
		row := applyHostCheckExit(judged, 0)
		if row.Result != response.ResultPass {
			t.Errorf("Result = %q, want pass", row.Result)
		}
		if !strings.HasSuffix(row.Evidence, "Zing ran the check command on the host before judging: exit 0.") {
			t.Errorf("Evidence = %q, want it to end with the host note", row.Evidence)
		}
		if row.CheckExit == nil || *row.CheckExit != 0 {
			t.Errorf("CheckExit = %v, want 0", row.CheckExit)
		}
	})

	t.Run("exit_2_fails", func(t *testing.T) {
		t.Parallel()
		row := applyHostCheckExit(judged, 2)
		if row.Result != response.ResultFail {
			t.Errorf("Result = %q, want fail", row.Result)
		}
		if row.CheckExit == nil || *row.CheckExit != 2 {
			t.Errorf("CheckExit = %v, want 2", row.CheckExit)
		}
	})

	t.Run("timeout_fails", func(t *testing.T) {
		t.Parallel()
		row := applyHostCheckExit(judged, -1)
		if row.Result != response.ResultFail {
			t.Errorf("Result = %q, want fail", row.Result)
		}
		if !strings.HasSuffix(row.Evidence, "Zing ran the check command on the host before judging: timed out after 10m.") {
			t.Errorf("Evidence = %q, want it to end with the host timeout note", row.Evidence)
		}
		if row.CheckExit == nil || *row.CheckExit != -1 {
			t.Errorf("CheckExit = %v, want -1", row.CheckExit)
		}
	})
}
