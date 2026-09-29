package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	zing "zing"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// This file tests runJob itself (unexported, design section 4.6), so it
// lives in package job rather than package job_test alongside job_test.go
// and skeleton_test.go.

// classifyBugXML is the checked-in classify-bug example (design section
// 22.1's own schema), reused here as the Fake's one scripted classify turn.
const classifyBugXML = `<zing job="classify" outcome="bug">
  <reason>The stack trace in the ticket shows a nil pointer dereference in the checkout handler, which only happens on a code path that already exists.</reason>
</zing>
`

// This file's repeated literals, named once so goconst has nothing to flag:
// testJobClassify is machine.toml's job name (== response.JobClassify, its
// model alias is "sonnet", its runtime "claude"); testRuntimeClaude and
// testRuntimeCodex are the other two runtime.Set keys every test registers
// alongside runtimeFake (skeleton.go); testModelAlias/testModelExact are one
// job.Deps.Models entry.
const (
	testJobClassify   = string(response.JobClassify)
	testRuntimeClaude = "claude"
	testRuntimeCodex  = "codex"
	testModelAlias    = "sonnet"
	testModelExact    = "claude-opus-x"
)

var runJobTestProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: "github",
}

// newRunJobTestStore opens a fresh Store on a temp-file database, closed on
// test cleanup.
func newRunJobTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedRunJobTicket inserts one queued ticket on runJobTestProject.
func seedRunJobTicket(t *testing.T, s *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	projectID, err := s.EnsureProject(ctx, runJobTestProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: "fake#1", Title: "t", State: stateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

func getRunJobTicket(t *testing.T, s *store.Store, ticketID int64) store.Ticket {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return ticket
}

// runJobTestMachine loads the real, checked-in machine.toml, so runJob's
// job/runtime/model/tools/timeout lookups run against the same job
// definitions zing serve loads (classify: model alias "sonnet", runtime
// "claude", tools read/grep/glob, timeout_minutes 5).
func runJobTestMachine(t *testing.T) *machine.Machine {
	t.Helper()
	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	return m
}

// claimRunJobTicket claims ticketID under a fresh owner and a
// second-precision lease, returning the exact (owner, expires) pair a
// realReserve closure must fence against (design section 4.5's exact-lease
// check), mirroring how dispatch.runAndCommit wires Deps.Reserve over the
// tick's own claim.
func claimRunJobTicket(t *testing.T, s *store.Store, ticketID int64) (owner string, expires time.Time) {
	t.Helper()
	owner = "runjob-test-owner"
	expires = time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}
	return owner, expires
}

// realReserve builds the same ReserveFunc closure dispatch.runAndCommit
// wires into Deps.Reserve: store.Reserve fenced on the caller's own
// (owner, expires).
func realReserve(s *store.Store, owner string, expires time.Time) ReserveFunc {
	return func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
		return s.Reserve(ctx, ticketID, owner, expires, su, seed)
	}
}

// seedAgentSeconds reserves and immediately terminalizes one run on
// ticketID, so AgentSecondsForTicket sums to seconds afterward, then leaves
// the ticket unclaimed again (CommitHandlerResult always clears the claim
// it fenced), ready for a test's own claimRunJobTicket call.
func seedAgentSeconds(t *testing.T, s *store.Store, ticketID int64, seconds int) {
	t.Helper()
	ctx := t.Context()
	owner, expires := claimRunJobTicket(t, s, ticketID)

	rsv, err := s.Reserve(ctx, ticketID, owner, expires, store.SessionUpsert{Job: testJobClassify, Runtime: runtimeFake}, store.RunSeed{Model: "claude-x"})
	if err != nil {
		t.Fatalf("seedAgentSeconds: Reserve: %v", err)
	}

	outcome := "bug"
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []store.Run{{ID: rsv.RunID, Outcome: &outcome, AgentSeconds: &seconds}},
	})
	if err != nil {
		t.Fatalf("seedAgentSeconds: CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("seedAgentSeconds: CommitHandlerResult: applied = false, want true")
	}
}

// recordingReserve wraps a ReserveFunc (nil is legal) and counts calls, so a
// test can assert runJob never reserved a run on an early failure path.
type recordingReserve struct {
	fn    ReserveFunc
	calls int
}

func (r *recordingReserve) Reserve(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
	r.calls++
	if r.fn == nil {
		return store.Reserved{}, errors.New("recordingReserve: unexpectedly called with no ReserveFunc wired")
	}
	return r.fn(ctx, ticketID, su, seed)
}

// countingRuntime wraps a runtime.Runtime, counts every Run call, and
// records the last call's context deadline and request, so a test can
// assert the runtime was driven exactly once (or not at all) with the
// fields runJob is responsible for filling in.
type countingRuntime struct {
	rt    runtime.Runtime
	calls int

	hasDeadline bool
	deadline    time.Time
	lastReq     runtime.RunRequest
}

func (c *countingRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	c.calls++
	c.lastReq = req
	if dl, ok := ctx.Deadline(); ok {
		c.hasDeadline = true
		c.deadline = dl
	}
	return c.rt.Run(ctx, req)
}

// --- runJob ------------------------------------------------------------

// TestRunJob_UnknownJobReturnsErrConfigNoReserve proves an unknown job name
// fails at step 1 (design section 4.6), before Deps.Reserve is ever called.
func TestRunJob_UnknownJobReturnsErrConfigNoReserve(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	d := Deps{
		Store: s, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
	}

	_, err := runJob(t.Context(), d, ticket, "no-such-job", store.SessionUpsert{}, runtime.RunRequest{}, nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
}

// TestRunJob_UnknownRuntimeReturnsErrConfigNoReserve proves an unknown
// runtime name fails at step 2, before Reserve, even though the job itself
// (classify) is well-formed (design section 4.6: the runtime is resolved
// before a run is ever reserved).
func TestRunJob_UnknownRuntimeReturnsErrConfigNoReserve(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeCodex: fake, runtimeFake: fake}) // "claude" missing on purpose
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
}

// TestRunJob_MissingModelAliasReturnsErrConfigNoReserve proves a Models map
// missing the job's model alias fails at step 3, before Reserve.
func TestRunJob_MissingModelAliasReturnsErrConfigNoReserve(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{}, // no "sonnet" alias
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
}

// TestRunJob_BudgetExhaustedReturnsErrBudgetNoReserveNoRun proves the budget
// check at step 4 trips before Reserve or the runtime ever run, once the
// ticket's spent agent-time already meets the cap (design section 4.6: >=,
// not >).
func TestRunJob_BudgetExhaustedReturnsErrBudgetNoReserveNoRun(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	seedAgentSeconds(t, s, ticketID, 10)

	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	counting := &countingRuntime{rt: runtime.NewFake(fstest.MapFS{})}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: 10 * time.Second, Owner: owner, Expires: expires, Reserve: rec.Reserve,
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want errors.Is(err, ErrBudget)", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
	if counting.calls != 0 {
		t.Errorf("runtime Run calls = %d, want 0", counting.calls)
	}
}

// TestRunJob_LostClaimWrapsErrClaimLost proves a lease that has already
// moved on by the time runJob reserves (design section 4.6 step 7) surfaces
// as an error that still satisfies errors.Is(err, store.ErrClaimLost),
// wrapped but never swallowed.
func TestRunJob_LostClaimWrapsErrClaimLost(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	// Simulate the lease moving on from under this call: expire every claim
	// as of a moment in the future, then let a different owner claim the
	// ticket, so (owner, expires) above no longer matches the live row.
	if _, err := s.ExpireClaims(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}
	otherExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, "someone-else", otherExpires)
	if err != nil || !claimed {
		t.Fatalf("re-claim by someone-else: claimed=%v err=%v", claimed, err)
	}

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: runtimeFake}, runtime.RunRequest{Job: response.JobClassify}, nil)
	if !errors.Is(err, store.ErrClaimLost) {
		t.Fatalf("err = %v, want errors.Is(err, store.ErrClaimLost)", err)
	}
}

// TestRunJob_HappyPathReservesFillsRequestAndRuns proves the full order
// (design section 4.6): Reserve fires at turn 0 with RunToken set to that
// run id as decimal, WorkDir/Tools/Timeout come from the ticket's project
// and classify's own machine.toml job, the child context's deadline equals
// the job timeout, and the Fake's result comes back unchanged.
func TestRunJob_HappyPathReservesFillsRequestAndRuns(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{"classify/1.xml": &fstest.MapFile{Data: []byte(classifyBugXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
	}

	before := time.Now()
	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil)
	after := time.Now()
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	if counting.calls != 1 {
		t.Fatalf("runtime Run calls = %d, want 1", counting.calls)
	}
	if rr.Reserved.Turn != 0 {
		t.Errorf("Reserved.Turn = %d, want 0 (first run on a fresh session)", rr.Reserved.Turn)
	}
	wantToken := strconv.FormatInt(rr.Reserved.RunID, 10)
	if counting.lastReq.RunToken != wantToken {
		t.Errorf("RunToken = %q, want %q (the reserved run id as decimal)", counting.lastReq.RunToken, wantToken)
	}
	if counting.lastReq.WorkDir != runJobTestProject.LocalPath {
		t.Errorf("WorkDir = %q, want %q", counting.lastReq.WorkDir, runJobTestProject.LocalPath)
	}
	wantTools := []string{"read", "grep", "glob"} // classify's machine.toml tools
	if len(counting.lastReq.Tools) != len(wantTools) {
		t.Fatalf("Tools = %v, want %v", counting.lastReq.Tools, wantTools)
	}
	for i, tool := range wantTools {
		if counting.lastReq.Tools[i] != tool {
			t.Errorf("Tools[%d] = %q, want %q", i, counting.lastReq.Tools[i], tool)
		}
	}
	const wantTimeout = 5 * time.Minute // classify's machine.toml timeout_minutes
	if counting.lastReq.Timeout != wantTimeout {
		t.Errorf("Timeout = %v, want %v", counting.lastReq.Timeout, wantTimeout)
	}
	if !counting.hasDeadline {
		t.Fatal("the runtime's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(wantTimeout - time.Second)
	wantMax := after.Add(wantTimeout + time.Second)
	if counting.deadline.Before(wantMin) || counting.deadline.After(wantMax) {
		t.Errorf("child ctx deadline = %v, want within [%v, %v] (job timeout %v)", counting.deadline, wantMin, wantMax, wantTimeout)
	}

	if rr.Res.Response == nil || rr.Res.Response.Header().Outcome != response.OutcomeBug {
		t.Errorf("Res.Response outcome = %v, want %v (the Fake's scripted bug turn)", rr.Res.Response, response.OutcomeBug)
	}
}

// ---- the sandbox step (design section 5.5, task 8) -------------------

// buildOkXML is a build/ok document (the same shape as the checked-in
// fixtures/scripts/build/1/1.xml), reused here as the Fake's one scripted
// build turn.
const buildOkXML = `<zing job="build" outcome="ok">
  <claims>
    <files_changed>
      <path>cmd/zing/main.go</path>
    </files_changed>
    <test_exit>0</test_exit>
    <lint_exit>0</lint_exit>
  </claims>
  <report>ok</report>
  <notes></notes>
</zing>
`

const testJobBuild = "build"

// testBuildLabel is the label these sandbox tests reserve their one
// scripted build turn under (skeleton.go's own former buildLabel, now
// building.go's real per-task label).
const testBuildLabel = "1"

// testBuildScriptKey is the Fake runtime's own script key for job "build",
// label testBuildLabel, turn 1 (runtime/fake.go's scriptKey), reused across
// every sandbox test below that needs a scripted build turn.
const testBuildScriptKey = "build/1/1.xml"

// testSandboxProfile is a minimal, always-loadable seatbelt profile (no
// rule beyond the two placeholders every real profile carries): these tests
// are about runJob's own wrapping logic, not about proving the checked-in
// sandbox/build.sb profile loads (internal/sandbox's own suite does that).
var testSandboxProfile = []byte("(version 1)\n(allow default)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n")

// loadTestSandboxOrSkip loads testSandboxProfile for real, through
// sandbox.Load: every Sandbox field is unexported in internal/sandbox, so
// this package cannot fabricate an "available" Sandbox any other way. It
// skips, rather than fails, when this machine cannot load one at all
// (design section 5.4's own four reasons) -- non-macOS CI, most notably.
func loadTestSandboxOrSkip(t *testing.T) sandbox.Sandbox {
	t.Helper()
	sb := sandbox.Load(testSandboxProfile, t.TempDir(), nil, 7420)
	if !sb.Available() {
		t.Skipf("sandbox unavailable on this machine: %s", sb.Reason())
	}
	return sb
}

// buildSandboxDeps builds a Deps for the sandbox tests below: every
// machine.toml model alias resolves to testModelExact (the "build" job's
// model is "sonnet"; the exact id does not matter, since rt is a Fake or a
// counting wrapper around one), and Projects carries one entry, keyed by
// projectID, so applySandbox's own d.Projects[t.ProjectID] lookup resolves.
func buildSandboxDeps(t *testing.T, s *store.Store, rt runtime.Runtime, projectID int64, owner string, expires time.Time, sb sandbox.Sandbox, requireSandbox bool) Deps {
	t.Helper()
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: rt, testRuntimeCodex: rt, runtimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t),
		Models: map[string]string{"sonnet": testModelExact, "opus": testModelExact, "fable": testModelExact, "codex": testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		Projects:       map[int64]Project{projectID: {RepoGit: "/tmp/zing-git"}},
		Sandbox:        sb,
		RequireSandbox: requireSandbox,
	}
}

// TestRunJobWrapsWhenAvailable proves the sandbox step wraps a sandboxed
// job's request with the sandbox's own ExecPrefix and Env when the sandbox
// is available, regardless of RequireSandbox.
func TestRunJobWrapsWhenAvailable(t *testing.T) {
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	rr, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel}, nil)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("runtime Run calls = %d, want 1", counting.calls)
	}
	if len(counting.lastReq.ExecPrefix) == 0 || counting.lastReq.ExecPrefix[0] != "sandbox-exec" {
		t.Fatalf("ExecPrefix = %v, want it to start with sandbox-exec", counting.lastReq.ExecPrefix)
	}
	if !slices.ContainsFunc(counting.lastReq.Env, func(kv string) bool { return strings.HasPrefix(kv, "TMPDIR=") }) {
		t.Errorf("Env = %v, want a TMPDIR entry", counting.lastReq.Env)
	}
	if rr.Res.Response == nil || rr.Res.Response.Header().Outcome != response.OutcomeOk {
		t.Errorf("outcome = %v, want ok", rr.Res.Response)
	}
}

// TestRunJobErrSandboxWhenRequired proves an unavailable, required sandbox
// returns ErrSandbox with nothing reserved and the runtime never called
// (design section 5.5).
func TestRunJobErrSandboxWhenRequired(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	counting := &countingRuntime{rt: runtime.NewFake(fstest.MapFS{})}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sandbox.Off(), true)
	rec := &recordingReserve{fn: deps.Reserve}
	deps.Reserve = rec.Reserve

	_, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel}, nil)
	if !errors.Is(err, ErrSandbox) {
		t.Fatalf("err = %v, want ErrSandbox", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
	if counting.calls != 0 {
		t.Errorf("runtime Run calls = %d, want 0", counting.calls)
	}
}

// TestRunJobUnwrappedWhenNotRequired proves an unavailable sandbox with
// RequireSandbox false runs the job unwrapped (design D5: suites on the
// fake runtime), rather than failing.
func TestRunJobUnwrappedWhenNotRequired(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sandbox.Off(), false)

	_, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel}, nil)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("runtime Run calls = %d, want 1", counting.calls)
	}
	if len(counting.lastReq.ExecPrefix) != 0 {
		t.Errorf("ExecPrefix = %v, want empty (unwrapped)", counting.lastReq.ExecPrefix)
	}
}

// TestRunJobRemovesRunDir proves the sandbox run directory Reserve's own
// wrapping step creates is removed once runJob returns (its deferred
// cleanup, design section 5.5).
func TestRunJobRemovesRunDir(t *testing.T) {
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	_, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel}, nil)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	var runDir string
	for _, kv := range counting.lastReq.ExecPrefix {
		if after, ok := strings.CutPrefix(kv, "RUN_DIR="); ok {
			runDir = after
		}
	}
	if runDir == "" {
		t.Fatalf("no RUN_DIR=... entry found in ExecPrefix %v", counting.lastReq.ExecPrefix)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir %s still exists after runJob returned (stat err = %v)", runDir, err)
	}
}

// TestRunJobSeedsTaskN proves runJob copies its own taskN parameter into
// RunSeed.TaskN (design section 6.3): a non-nil taskN lands on the reserved
// run's own task_n column, and a nil one (planning's own four callers)
// leaves it NULL.
func TestRunJobSeedsTaskN(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{"classify/1.xml": &fstest.MapFile{Data: []byte(classifyBugXML)}}
	fake := runtime.NewFake(scripts)
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
	}

	n := 3
	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, &n)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	run, ok, err := s.FirstRun(t.Context(), rr.Reserved.SessionID)
	if err != nil || !ok {
		t.Fatalf("FirstRun: ok=%v err=%v", ok, err)
	}
	if run.TaskN == nil || *run.TaskN != 3 {
		t.Errorf("run.TaskN = %v, want 3", run.TaskN)
	}
}
