package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
	// testModelAliasOpus and testModelAliasFable name the other two
	// model-alias keys this file's own all-aliases Models maps repeat
	// (goconst); testModelAlias ("sonnet") and pbRuntimeCodex ("codex",
	// postbuild_test.go, also package job) already cover the other two.
	testModelAliasOpus  = "opus"
	testModelAliasFable = "fable"
	// testClassifyScriptKey is the Fake runtime's own script key for a
	// classify first turn (runtime/fake.go's scriptKey), shared by every
	// test in this file that scripts one (goconst).
	testClassifyScriptKey = "classify/1.xml"
)

var runJobTestProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: testTrackerGithub,
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
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	d := Deps{
		Store: s, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
	}

	_, err := runJob(t.Context(), d, ticket, "no-such-job", store.SessionUpsert{}, runtime.RunRequest{}, nil, nil, 0)
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
	t.Parallel()
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

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
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
	t.Parallel()
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

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
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
	t.Parallel()
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

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
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
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	// Simulate the lease moving on from under this call: expire every claim
	// as of a moment in the future, then let a different owner claim the
	// ticket, so (owner, expires) above no longer matches the live row.
	if _, err := s.ExpireClaims(t.Context(), time.Now().Add(time.Hour), ""); err != nil {
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
		DataDir: t.TempDir(),
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: runtimeFake}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
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
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testClassifyScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	before := time.Now()
	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
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

// TestRunJob_BuildRunCarriesDenyList proves runJobWith fills req.DenyBash
// from the ticket's project (TestCmd, LintCmd, then Deny, duplicates
// dropped) only for the build job, and that req.Timeout always comes from
// jobTimeout, the machine's own timeout_minutes for that job.
func TestRunJob_BuildRunCarriesDenyList(t *testing.T) {
	t.Parallel()
	const (
		denyTestCmd = "make test"
		denyLintCmd = "make lint"
		denyGoTest  = "go test ./..."
	)
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	counting := &countingRuntime{rt: stubRunResult{}}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(), Sandboxes: sandbox.OffSet(),
		Projects: map[int64]Project{
			ticket.ProjectID: {TestCmd: denyTestCmd, LintCmd: denyLintCmd, Deny: []string{denyGoTest, denyTestCmd}},
		},
	}

	_, err = runJob(t.Context(), d, ticket, jobBuildName, store.SessionUpsert{Job: jobBuildName, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: "1"}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob (build): %v", err)
	}
	wantDeny := []string{denyTestCmd, denyLintCmd, denyGoTest}
	if !slices.Equal(counting.lastReq.DenyBash, wantDeny) {
		t.Errorf("build DenyBash = %v, want %v", counting.lastReq.DenyBash, wantDeny)
	}
	const wantBuildTimeout = 45 * time.Minute // machine.toml jobs.build.timeout_minutes
	if counting.lastReq.Timeout != wantBuildTimeout {
		t.Errorf("build Timeout = %v, want %v", counting.lastReq.Timeout, wantBuildTimeout)
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob (classify): %v", err)
	}
	if len(counting.lastReq.DenyBash) != 0 {
		t.Errorf("classify DenyBash = %v, want empty", counting.lastReq.DenyBash)
	}
}

// TestProject_DenyCommands proves DenyCommands collapses each entry's
// internal and outer whitespace to single spaces, drops an empty LintCmd
// rather than keeping a blank entry, and drops a Deny entry that differs
// from TestCmd only in whitespace as the duplicate it is (r3f4: without
// the strings.Fields normalization, the padded TestCmd below would reach
// the deny hook with its internal double space intact, where it would
// never match a normally-spaced Bash call).
func TestProject_DenyCommands(t *testing.T) {
	t.Parallel()

	p := Project{
		TestCmd: "  make  test  ",
		LintCmd: "",
		Deny:    []string{"make test", "make ci"},
	}
	want := []string{"make test", "make ci"}
	if got := p.DenyCommands(); !slices.Equal(got, want) {
		t.Errorf("DenyCommands() = %v, want %v", got, want)
	}
}

// TestDeadlineInput_UsesBuildTimeout proves deadlineInput reads the
// machine's own build job.timeout_minutes through jobTimeout, the one
// function runJobWith's own req.Timeout also reads, so the two never
// drift.
func TestDeadlineInput_UsesBuildTimeout(t *testing.T) {
	t.Parallel()
	d := Deps{Machine: runJobTestMachine(t)}
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)

	in := deadlineInput(d, now)

	const want = "This run ends at 14:45 UTC, in 45 minutes." // machine.toml jobs.build.timeout_minutes
	if in.Text != want {
		t.Errorf("deadlineInput text = %q, want %q", in.Text, want)
	}
}

// TestRunJobWith_OnStartRecordsRunStart proves runJobWith's own OnStart
// closure (design section 7.1, #45): the fake runtime calls it with PID 0
// and the session id it minted, before running the fake's scripted turn,
// and the closure records that identity through store.RecordRunStart --
// started_at set, pgid left NULL for the fake runtime's PID 0, and the
// session's external_id filled with the fake's own minted id.
func TestRunJobWith_OnStartRecordsRunStart(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testClassifyScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)}}
	fake := runtime.NewFake(scripts)
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	before := time.Now()
	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	run, ok, err := s.FirstRun(t.Context(), rr.Reserved.SessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok {
		t.Fatal("FirstRun: ok = false, want true")
	}
	if run.StartedAt == nil || run.StartedAt.Before(before.Add(-time.Second)) {
		t.Errorf("run.StartedAt = %v, want set to roughly now", run.StartedAt)
	}
	if run.PGID != nil {
		t.Errorf("run.PGID = %v, want nil for the fake runtime's PID 0", run.PGID)
	}
	if run.ProcStart != nil {
		t.Errorf("run.ProcStart = %v, want nil for the fake runtime's PID 0", run.ProcStart)
	}

	sess, ok, err := s.OpenSession(t.Context(), ticketID, testJobClassify)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !ok {
		t.Fatal("OpenSession: ok = false, want true")
	}
	if sess.ExternalID == nil || *sess.ExternalID == "" {
		t.Fatal("session.ExternalID = nil, want the fake runtime's own minted session id")
	}
	if rr.Res.SessionID != *sess.ExternalID {
		t.Errorf("session.ExternalID = %q, want the fake's own RunResult.SessionID %q", *sess.ExternalID, rr.Res.SessionID)
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

// testSandboxExecArg is Sandbox.Prefix's own argv[0] (internal/sandbox),
// reused by every test below that just checks a request was wrapped at all
// (goconst: three or more call sites compared this literal).
const testSandboxExecArg = "sandbox-exec"

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
// RepoGit is a real, existing directory (not merely a plausible-looking
// path): ParamsFor resolves it with filepath.EvalSymlinks, so a path with
// nothing there would fail every test here that loads a real, available
// sandbox.
func buildSandboxDeps(t *testing.T, s *store.Store, rt runtime.Runtime, projectID int64, owner string, expires time.Time, sb sandbox.Sandbox, requireSandbox bool) Deps {
	t.Helper()
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: rt, testRuntimeCodex: rt, runtimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t),
		Models: map[string]string{testModelAlias: testModelExact, testModelAliasOpus: testModelExact, testModelAliasFable: testModelExact, testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		Projects:       map[int64]Project{projectID: {RepoGit: t.TempDir()}},
		Sandboxes:      sandbox.Set{Build: sb},
		RequireSandbox: requireSandbox,
		DataDir:        t.TempDir(),
	}
}

// TestRunJobWrapsWhenAvailable proves the sandbox step wraps a sandboxed
// job's request with the sandbox's own ExecPrefix and Env when the sandbox
// is available, regardless of RequireSandbox.
func TestRunJobWrapsWhenAvailable(t *testing.T) {
	t.Parallel()
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	// A real, existing directory: ParamsFor resolves WORKTREE with
	// filepath.EvalSymlinks, and building always passes a real worktree
	// directory here in production (runjob.go's own "Building passes the
	// worktree directory in req.WorkDir already").
	rr, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("runtime Run calls = %d, want 1", counting.calls)
	}
	if len(counting.lastReq.ExecPrefix) == 0 || counting.lastReq.ExecPrefix[0] != testSandboxExecArg {
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
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	counting := &countingRuntime{rt: runtime.NewFake(fstest.MapFS{})}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sandbox.Off(), true)
	rec := &recordingReserve{fn: deps.Reserve}
	deps.Reserve = rec.Reserve

	_, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel}, nil, nil, 0)
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
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sandbox.Off(), false)

	_, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel}, nil, nil, 0)
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
	t.Parallel()
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	// A real, existing directory: see TestRunJobWrapsWhenAvailable's own
	// comment on WorkDir.
	_, err := runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0)
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

// TestRunJobUsesResolvedWorkDir proves runJob's sandbox step hands the
// runtime the resolved worktree, not the one req.WorkDir was given, so the
// CLI's own working directory matches the WORKTREE rule the profile was
// built against and the TRANSCRIPTS folder it names (task 16a's own
// live-harness defect: a worktree path through macOS's /var ->
// /private/var symlink never matched a seatbelt subpath rule built from the
// unresolved path).
func TestRunJobUsesResolvedWorkDir(t *testing.T) {
	t.Parallel()
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", realDir, err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", realDir, err)
	}

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	_, err = runJob(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: link}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if counting.lastReq.WorkDir != resolved {
		t.Errorf("WorkDir = %q, want %q (resolved)", counting.lastReq.WorkDir, resolved)
	}
}

// TestRunJobPicksProfileByName proves runJob resolves a job's sandbox
// profile by the name its own machine.toml entry gives (PKG9-PLAN.md
// section 4.7: d.Sandboxes.For(jobCfg.Sandbox)), not always "build": a job
// naming "readonly" runs wrapped in Sandboxes.ReadOnly even though
// Sandboxes.Build is off and required, which would refuse the run outright
// if runJob ever picked the wrong profile.
func TestRunJobPicksProfileByName(t *testing.T) {
	t.Parallel()
	readonlySB := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	m := runJobTestMachine(t)
	readonlyJob := m.Jobs[testJobBuild]
	readonlyJob.Sandbox = "readonly"
	const testJobReadonly = "readonly-test-job"
	m.Jobs[testJobReadonly] = readonlyJob

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	deps := Deps{
		Store: s, Runtimes: set, Machine: m,
		Models: map[string]string{testModelAlias: testModelExact, testModelAliasOpus: testModelExact, testModelAliasFable: testModelExact, testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		Projects:       map[int64]Project{ticket.ProjectID: {RepoGit: t.TempDir()}},
		Sandboxes:      sandbox.Set{Build: sandbox.Off(), ReadOnly: readonlySB},
		RequireSandbox: true,
	}

	rr, err := runJob(t.Context(), deps, ticket, testJobReadonly, store.SessionUpsert{Job: testJobReadonly, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v (want the readonly profile picked, not the off-and-required build one)", err)
	}
	if counting.calls != 1 {
		t.Fatalf("runtime Run calls = %d, want 1", counting.calls)
	}
	if len(counting.lastReq.ExecPrefix) == 0 || counting.lastReq.ExecPrefix[0] != testSandboxExecArg {
		t.Errorf("ExecPrefix = %v, want it to start with sandbox-exec", counting.lastReq.ExecPrefix)
	}
	if rr.Res.Response == nil || rr.Res.Response.Header().Outcome != response.OutcomeOk {
		t.Errorf("outcome = %v, want ok", rr.Res.Response)
	}
}

// testJudgeTestJob is the machine.toml job name TestRunJobJudgeParamsCodexHome
// and TestRunJobJudgeEmptyCodexHomeIsConfigError both register, a copy of
// the build job with its own Sandbox set to "judge" (goconst: shared by
// both).
const testJudgeTestJob = "judge-test-job"

// newJudgeProfileJob returns a machine.Machine whose own testJudgeTestJob
// entry is a copy of testJobBuild with Sandbox set to "judge", for a test
// that exercises runJob's own CODEX_HOME wiring (PKG9-PLAN.md section 7.3,
// D27) without needing the real judge.sb profile or a scenarios file: the
// Sandbox this test slots into Deps.Sandboxes.Judge is loaded under the
// name "build" (loadTestSandboxOrSkip), so sandbox.Prefix's own
// judge-name-keyed "SCENARIOS_FILE and CODEX_HOME together" rule never
// triggers, the same way TestRunJobPicksProfileByName reuses a sandbox
// loaded under one name for a different Deps.Sandboxes slot.
func newJudgeProfileJob(t *testing.T) *machine.Machine {
	t.Helper()
	m := runJobTestMachine(t)
	judgeJob := m.Jobs[testJobBuild]
	judgeJob.Sandbox = sandboxProfileJudge
	m.Jobs[testJudgeTestJob] = judgeJob
	return m
}

// TestRunJobJudgeParamsCodexHome proves runJob's applySandbox step fills
// Params.CodexHome from Deps.JudgeCodexHome for a job whose profile is
// "judge" (PKG9-PLAN.md section 7.3, D27): the sandbox-exec prefix carries
// "-D CODEX_HOME=<value>".
func TestRunJobJudgeParamsCodexHome(t *testing.T) {
	t.Parallel()
	judgeSB := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	m := newJudgeProfileJob(t)
	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	const wantJudgeCodexHome = "/test/judge/codex/home"
	deps := Deps{
		Store: s, Runtimes: set, Machine: m,
		Models: map[string]string{testModelAlias: testModelExact, testModelAliasOpus: testModelExact, testModelAliasFable: testModelExact, testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		Projects:       map[int64]Project{ticket.ProjectID: {RepoGit: t.TempDir()}},
		Sandboxes:      sandbox.Set{Build: sandbox.Off(), Judge: judgeSB},
		RequireSandbox: true,
		JudgeCodexHome: wantJudgeCodexHome,
	}

	_, err = runJob(t.Context(), deps, ticket, testJudgeTestJob, store.SessionUpsert{Job: testJudgeTestJob, Runtime: testRuntimeCodex},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	wantFlag := "CODEX_HOME=" + wantJudgeCodexHome
	if !slices.Contains(counting.lastReq.ExecPrefix, wantFlag) {
		t.Errorf("ExecPrefix = %v, want it to contain %q", counting.lastReq.ExecPrefix, wantFlag)
	}
}

// TestRunJobJudgeEmptyCodexHomeIsConfigError proves a judge-profile job
// with an empty Deps.JudgeCodexHome refuses to run at all, before any
// reserve, with the exact error text (PKG9-PLAN.md section 7.3, D27).
func TestRunJobJudgeEmptyCodexHomeIsConfigError(t *testing.T) {
	t.Parallel()
	judgeSB := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	m := newJudgeProfileJob(t)
	counting := &countingRuntime{rt: runtime.NewFake(fstest.MapFS{})}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	deps := Deps{
		Store: s, Runtimes: set, Machine: m,
		Models: map[string]string{testModelAlias: testModelExact, testModelAliasOpus: testModelExact, testModelAliasFable: testModelExact, testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
		Projects:       map[int64]Project{ticket.ProjectID: {RepoGit: t.TempDir()}},
		Sandboxes:      sandbox.Set{Build: sandbox.Off(), Judge: judgeSB},
		RequireSandbox: true,
		// JudgeCodexHome left empty.
	}

	_, err = runJob(t.Context(), deps, ticket, testJudgeTestJob, store.SessionUpsert{Job: testJudgeTestJob, Runtime: testRuntimeCodex},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	want := "job: configuration error: judge codex home is not configured"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
	if counting.calls != 0 {
		t.Errorf("runtime Run calls = %d, want 0", counting.calls)
	}
}

// TestRunJobUnknownProfileIsConfigError proves a job whose machine.toml
// sandbox name Set.For does not recognize is ErrConfig, with nothing
// reserved and the runtime never called (PKG9-PLAN.md section 4.7): this
// can never happen through a real, validated machine.toml (machine.go's
// own validateJob already refuses any other value), so this test builds
// its own machine.Machine to reach runJob's own defensive check directly.
func TestRunJobUnknownProfileIsConfigError(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	m := runJobTestMachine(t)
	mysteryJob := m.Jobs[testJobBuild]
	mysteryJob.Sandbox = "mystery"
	const testJobMystery = "mystery-test-job"
	m.Jobs[testJobMystery] = mysteryJob

	counting := &countingRuntime{rt: runtime.NewFake(fstest.MapFS{})}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	deps := Deps{
		Store: s, Runtimes: set, Machine: m,
		Models: map[string]string{testModelAlias: testModelExact, testModelAliasOpus: testModelExact, testModelAliasFable: testModelExact, testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
		Projects:       map[int64]Project{ticket.ProjectID: {RepoGit: t.TempDir()}},
		Sandboxes:      sandbox.OffSet(),
		RequireSandbox: true,
	}

	_, err = runJob(t.Context(), deps, ticket, testJobMystery, store.SessionUpsert{Job: testJobMystery, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
	if counting.calls != 0 {
		t.Errorf("runtime Run calls = %d, want 0", counting.calls)
	}
}

// ---- the private temp root of an unsandboxed run (design section 7.3) ----

// TestRunJobEmptyDataDirIsConfigError proves a job naming no sandbox
// (classify) refuses to run at all with an empty Deps.DataDir, before any
// reserve (PKG9-PLAN.md section 7.3): a private temp root needs somewhere
// to live.
func TestRunJobEmptyDataDirIsConfigError(t *testing.T) {
	t.Parallel()
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
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
		// DataDir deliberately left empty.
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
}

// TestUnsandboxedRunGetsPrivateTemp proves a job naming no sandbox
// (classify) carries TMPDIR and CLAUDE_CODE_TMPDIR pointing under
// <DataDir>/tmp/run/ in its request env, and that the private temp root is
// removed once runJob returns (PKG9-PLAN.md section 7.3).
func TestUnsandboxedRunGetsPrivateTemp(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	dataDir := t.TempDir()
	scripts := fstest.MapFS{testClassifyScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: counting, testRuntimeCodex: counting, runtimeFake: counting})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: dataDir,
	}

	_, err = runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	var tmpDir, claudeTmpDir string
	for _, kv := range counting.lastReq.Env {
		if after, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			tmpDir = after
		}
		if after, ok := strings.CutPrefix(kv, "CLAUDE_CODE_TMPDIR="); ok {
			claudeTmpDir = after
		}
	}
	wantRoot := filepath.Join(dataDir, "tmp", "run")
	if tmpDir == "" || !strings.HasPrefix(tmpDir, wantRoot) {
		t.Errorf("TMPDIR = %q, want it under %q", tmpDir, wantRoot)
	}
	if claudeTmpDir == "" || !strings.HasPrefix(claudeTmpDir, wantRoot) {
		t.Errorf("CLAUDE_CODE_TMPDIR = %q, want it under %q", claudeTmpDir, wantRoot)
	}

	privateRoot := filepath.Dir(tmpDir) // the run-id directory, parent of "tmp"
	if _, statErr := os.Stat(privateRoot); !os.IsNotExist(statErr) {
		t.Errorf("private temp root %s still exists after runJob returned (stat err = %v)", privateRoot, statErr)
	}
}

// TestRunJobSeedsTaskN proves runJob copies its own taskN parameter into
// RunSeed.TaskN (design section 6.3): a non-nil taskN lands on the reserved
// run's own task_n column, and a nil one (planning's own four callers)
// leaves it NULL.
func TestRunJobSeedsTaskN(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testClassifyScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)}}
	fake := runtime.NewFake(scripts)
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	n := 3
	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, &n, nil, 0)
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

// ---- runJobWith's afterReserve hook (PKG9-PLAN.md section 7.3, D19) ------

// recordingHook is an afterReserve (runjob.go) that records the exact
// store.Reserved it was called with and whether req already carried the
// sandbox's own ExecPrefix (it must not: runJobWith only builds the prefix
// after the hook returns, since the prefix may need to name a file the
// hook just wrote under the run's own id). It returns scenariosFile and a
// cleanup that counts its own calls and can be made to fail, plus hookErr.
type recordingHook struct {
	calls         int
	gotRunID      int64
	hadExecPrefix bool
	scenariosFile string
	cleanupCalls  int
	cleanupErr    error
	hookErr       error
}

func (h *recordingHook) hook(_ context.Context, rsv store.Reserved, req *runtime.RunRequest) (scenariosFile string, cleanup func() error, err error) {
	h.calls++
	h.gotRunID = rsv.RunID
	h.hadExecPrefix = len(req.ExecPrefix) != 0
	return h.scenariosFile, func() error {
		h.cleanupCalls++
		return h.cleanupErr
	}, h.hookErr
}

// stubRunResult is a fixed-result runtime.Runtime: every call returns res
// and err unconditionally, regardless of req or ctx. It stands in for the
// real Fake in the hook tests below that only care how runJobWith's own
// bookkeeping reacts to rt.Run's outcome (ok, a plain error, or a
// cancellation), not about a scripted turn.
type stubRunResult struct {
	res runtime.RunResult
	err error
}

func (s stubRunResult) Run(context.Context, runtime.RunRequest) (runtime.RunResult, error) {
	return s.res, s.err
}

// TestRunJobWithHookRunsAfterReserve proves runJobWith calls its
// afterReserve hook once, after Reserve has already fixed the run (the
// hook sees the same run id runResult.Reserved carries) and before the
// sandbox prefix is built (the hook's own req snapshot carries no
// ExecPrefix yet), and that the sandbox prefix is still built, from the
// params the hook had a chance to fill, before rt.Run (PKG9-PLAN.md section
// 7.3).
func TestRunJobWithHookRunsAfterReserve(t *testing.T) {
	t.Parallel()
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	h := &recordingHook{}
	rr, err := runJobWith(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0, h.hook)
	if err != nil {
		t.Fatalf("runJobWith: %v", err)
	}

	if h.calls != 1 {
		t.Fatalf("hook calls = %d, want 1", h.calls)
	}
	if h.gotRunID != rr.Reserved.RunID {
		t.Errorf("hook saw rsv.RunID = %d, want %d (the run Reserve fixed)", h.gotRunID, rr.Reserved.RunID)
	}
	if h.hadExecPrefix {
		t.Error("hook's own req already carried an ExecPrefix; want it called before the sandbox prefix is built")
	}
	if len(counting.lastReq.ExecPrefix) == 0 || counting.lastReq.ExecPrefix[0] != testSandboxExecArg {
		t.Errorf("ExecPrefix = %v, want it built (after the hook returned) before rt.Run", counting.lastReq.ExecPrefix)
	}
	if counting.calls != 1 {
		t.Errorf("runtime Run calls = %d, want 1", counting.calls)
	}
}

// TestRunJobWithHookCleanupAfterRun proves the hook's own cleanup always
// runs once rt.Run has returned, whatever it returned: ok, a plain error,
// or a cancellation (PKG9-PLAN.md section 7.3, matching runJob's own
// sandbox and private-temp-root cleanups, which already run on every
// path).
func TestRunJobWithHookCleanupAfterRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rt   runtime.Runtime
	}{
		{"ok", runtime.NewFake(fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}})},
		{"error", runtime.NewFake(fstest.MapFS{})}, // no script for the build label: rt.Run errors
		{"canceled", stubRunResult{err: runtime.ErrCanceled}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sb := loadTestSandboxOrSkip(t)

			s := newRunJobTestStore(t)
			ticketID := seedRunJobTicket(t, s)
			ticket := getRunJobTicket(t, s, ticketID)
			owner, expires := claimRunJobTicket(t, s, ticketID)

			deps := buildSandboxDeps(t, s, c.rt, ticket.ProjectID, owner, expires, sb, true)

			h := &recordingHook{}
			if _, runErr := runJobWith(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
				runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0, h.hook); runErr != nil {
				t.Logf("runJobWith: %v (expected for the %s case)", runErr, c.name)
			}

			if h.cleanupCalls != 1 {
				t.Errorf("hook cleanup calls = %d, want 1 (rt.Run outcome: %s)", h.cleanupCalls, c.name)
			}
		})
	}
}

// TestRunJobWithHookErrorTerminalizes proves a hook error still returns the
// reserved run (Reserved.RunID set), the same shape a runtime failure
// returns, since Reserve has already fixed this call's turn by the time the
// hook runs: a caller like runAndRoute needs Reserved.RunID != 0 to route
// the error into postRunFailure rather than leaving the run's outcome NULL
// forever (PKG9-PLAN.md section 7.3). rt.Run itself is never called: the
// hook's own error comes before it.
func TestRunJobWithHookErrorTerminalizes(t *testing.T) {
	t.Parallel()
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	counting := &countingRuntime{rt: runtime.NewFake(fstest.MapFS{})}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	wantErr := errors.New("write scenarios file: boom")
	h := &recordingHook{hookErr: wantErr}
	rr, err := runJobWith(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0, h.hook)

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if rr.Reserved.RunID == 0 {
		t.Error("Reserved.RunID = 0, want the run Reserve already fixed")
	}
	if counting.calls != 0 {
		t.Errorf("runtime Run calls = %d, want 0 (the hook errored before rt.Run)", counting.calls)
	}
	if h.cleanupCalls != 1 {
		t.Errorf("hook cleanup calls = %d, want 1 (still run on a hook error)", h.cleanupCalls)
	}
}

// TestRunJobWithHookCleanupFailureLogged proves a cleanup error the hook
// returns is logged at WARN as "run cleanup failed" with ticket_id and
// run_id, and never replaces the run's own (successful) result (PKG9-PLAN.md
// section 7.3). Not parallel: it calls slog.SetDefault to capture a log
// line, which swaps the process-wide default logger.
func TestRunJobWithHookCleanupFailureLogged(t *testing.T) {
	sb := loadTestSandboxOrSkip(t)

	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testBuildScriptKey: &fstest.MapFile{Data: []byte(buildOkXML)}}
	counting := &countingRuntime{rt: runtime.NewFake(scripts)}
	deps := buildSandboxDeps(t, s, counting, ticket.ProjectID, owner, expires, sb, true)

	cleanupErr := errors.New("remove scenarios dir: boom")
	h := &recordingHook{cleanupErr: cleanupErr}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, err := runJobWith(t.Context(), deps, ticket, testJobBuild, store.SessionUpsert{Job: testJobBuild, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobBuild, Label: testBuildLabel, WorkDir: t.TempDir()}, nil, nil, 0, h.hook)
	if err != nil {
		t.Fatalf("runJobWith: %v", err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "run cleanup failed") {
		t.Fatalf("log missing \"run cleanup failed\"; got:\n%s", logged)
	}
	if !strings.Contains(logged, "ticket_id="+strconv.FormatInt(ticket.ID, 10)) {
		t.Errorf("log missing ticket_id=%d; got:\n%s", ticket.ID, logged)
	}
	if !strings.Contains(logged, "run_id="+strconv.FormatInt(rr.Reserved.RunID, 10)) {
		t.Errorf("log missing run_id=%d; got:\n%s", rr.Reserved.RunID, logged)
	}
	if !strings.Contains(logged, cleanupErr.Error()) {
		t.Errorf("log missing the cleanup error text; got:\n%s", logged)
	}
}

// ---- run evidence (#43 split: final message, stderr and transcript links) ----

// closeStoreThenResult is a runtime.Runtime stub that closes the store
// before returning a fixed result, so the caller's own recordRunEvidence
// write fails against an already-closed database (TestRunJob_EvidenceWriteFailureLogsWarn).
type closeStoreThenResult struct {
	store *store.Store
	res   runtime.RunResult
}

func (c closeStoreThenResult) Run(context.Context, runtime.RunRequest) (runtime.RunResult, error) {
	_ = c.store.Close()
	return c.res, nil
}

// jsonLogRecords parses buf as one JSON object per line (slog's own
// JSONHandler shape), so a logging test can assert on individual fields
// rather than substrings.
func jsonLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for line := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		recs = append(recs, m)
	}
	return recs
}

// findLogRecord returns the first record in recs whose "msg" field equals
// msg, failing the test if there is none.
func findLogRecord(t *testing.T, recs []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, r := range recs {
		if r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no log record with msg %q among %d records", msg, len(recs))
	return nil
}

// logRecordInt64 reads key from rec as the int64 a JSON number decodes to
// (encoding/json gives float64), failing the test if key is absent or not a
// number.
func logRecordInt64(t *testing.T, rec map[string]any, key string) int64 {
	t.Helper()
	v, ok := rec[key].(float64)
	if !ok {
		t.Fatalf("field %q = %v (%T), want a number", key, rec[key], rec[key])
	}
	return int64(v)
}

// logRecordString reads key from rec as a string, failing the test if key
// is absent or not a string.
func logRecordString(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	v, ok := rec[key].(string)
	if !ok {
		t.Fatalf("field %q = %v (%T), want a string", key, rec[key], rec[key])
	}
	return v
}

// TestRunJob_RecordsRunEvidence proves runJobWith stores a successful run's
// final message, stderr file path and transcript path through
// Store.RecordRunEvidence, right after the stderr file is written (design
// section: shape, recordRunEvidence).
func TestRunJob_RecordsRunEvidence(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	const wantTranscript = "/h/t.jsonl"
	stub := stubRunResult{res: runtime.RunResult{
		FinalMessage:   "doc",
		TranscriptPath: wantTranscript,
		Stderr:         []byte("boom"),
	}}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	dataDir := t.TempDir()
	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: dataDir,
	}

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	_, ev, err := s.RunEvidenceByID(t.Context(), rr.Reserved.RunID)
	if err != nil {
		t.Fatalf("RunEvidenceByID: %v", err)
	}
	if ev.FinalMessage == nil || *ev.FinalMessage != "doc" {
		t.Errorf("FinalMessage = %v, want \"doc\"", ev.FinalMessage)
	}
	wantStderrPath := filepath.Join(dataDir, "runs", fmt.Sprintf("run-%d-stderr.log", rr.Reserved.RunID))
	if ev.StderrPath == nil || *ev.StderrPath != wantStderrPath {
		t.Errorf("StderrPath = %v, want %q", ev.StderrPath, wantStderrPath)
	}
	if ev.TranscriptPath == nil || *ev.TranscriptPath != wantTranscript {
		t.Errorf("TranscriptPath = %v, want %q", ev.TranscriptPath, wantTranscript)
	}
}

// TestRunJob_RecordsEvidenceForInvalidOutput proves evidence is recorded
// even when rt.Run returns an InvalidOutputError (every outcome stores
// evidence, design goal): the final message is kept, and a run with no
// stderr stores no stderr path.
func TestRunJob_RecordsEvidenceForInvalidOutput(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	wantErr := &runtime.InvalidOutputError{Reason: "no zing element in final message"}
	stub := stubRunResult{res: runtime.RunResult{FinalMessage: "bad text"}, err: wantErr}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	var invalidErr *runtime.InvalidOutputError
	// errors.As, not the modernize-suggested errors.AsType: AsType's (E, bool)
	// result has E discarded via _, and errcheck's check-blank (this repo's
	// config) flags that discard since E is itself error-shaped.
	if !errors.As(err, &invalidErr) { //nolint:modernize // see comment above
		t.Fatalf("runJob err = %v, want *runtime.InvalidOutputError", err)
	}

	_, ev, evErr := s.RunEvidenceByID(t.Context(), rr.Reserved.RunID)
	if evErr != nil {
		t.Fatalf("RunEvidenceByID: %v", evErr)
	}
	if ev.FinalMessage == nil || *ev.FinalMessage != "bad text" {
		t.Errorf("FinalMessage = %v, want \"bad text\"", ev.FinalMessage)
	}
	if ev.StderrPath != nil {
		t.Errorf("StderrPath = %v, want nil (no stderr)", ev.StderrPath)
	}
}

// TestRunJob_FakeRunStoresScriptAsFinalMessage proves a real, scripted
// planning-style fake run stores its script text as the run's
// final_message (runtime.Fake.Run already sets RunResult.FinalMessage to
// the script text; this proves runJobWith carries it through to the
// store).
func TestRunJob_FakeRunStoresScriptAsFinalMessage(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	scripts := fstest.MapFS{testClassifyScriptKey: &fstest.MapFile{Data: []byte(classifyBugXML)}}
	fake := runtime.NewFake(scripts)
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	_, ev, err := s.RunEvidenceByID(t.Context(), rr.Reserved.RunID)
	if err != nil {
		t.Fatalf("RunEvidenceByID: %v", err)
	}
	if ev.FinalMessage == nil || *ev.FinalMessage != classifyBugXML {
		t.Errorf("FinalMessage = %v, want the script text", ev.FinalMessage)
	}
}

// TestRunJob_EvidenceWriteFailureLogsWarn proves a failed evidence write
// logs a WARN "run evidence not recorded" with ticket_id, run_id and error,
// never fails the run itself, and never logs the final message text. Not
// t.Parallel: it swaps slog's process-wide default to capture the record.
func TestRunJob_EvidenceWriteFailureLogsWarn(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	wantRes := runtime.RunResult{
		FinalMessage: "hidden text",
	}
	stub := closeStoreThenResult{store: s, res: wantRes}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v, want nil (an evidence write failure must not fail the run)", err)
	}
	if rr.Res.FinalMessage != "hidden text" {
		t.Errorf("rr.Res.FinalMessage = %q, want %q", rr.Res.FinalMessage, "hidden text")
	}

	recs := jsonLogRecords(t, &logBuf)
	warn := findLogRecord(t, recs, "run evidence not recorded")
	if warn["level"] != slogLevelWarn {
		t.Errorf("level = %v, want %s", warn["level"], slogLevelWarn)
	}
	if got := logRecordInt64(t, warn, "ticket_id"); got != ticket.ID {
		t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, warn, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
	}
	if logRecordString(t, warn, "error") == "" {
		t.Error("error field is empty, want the store's own close-related error")
	}

	if strings.Contains(logBuf.String(), "hidden text") {
		t.Errorf("log buffer contains the final message text:\n%s", logBuf.String())
	}
}

// TestRunJob_EndLogNamesEvidenceNotContents proves the "runJob end" INFO
// line carries final_message_len and transcript_path, never the final
// message text itself. Not t.Parallel: it swaps slog's process-wide
// default to capture the record.
func TestRunJob_EndLogNamesEvidenceNotContents(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	const wantTranscript = "/h/t.jsonl"
	const wantFinalMessage = "secret words"
	stub := stubRunResult{res: runtime.RunResult{
		FinalMessage:   wantFinalMessage,
		TranscriptPath: wantTranscript,
	}}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	recs := jsonLogRecords(t, &logBuf)
	end := findLogRecord(t, recs, "runJob end")
	if got := logRecordInt64(t, end, "final_message_len"); got != int64(len(wantFinalMessage)) {
		t.Errorf("final_message_len = %d, want %d", got, len(wantFinalMessage))
	}
	if got := logRecordString(t, end, "transcript_path"); got != wantTranscript {
		t.Errorf("transcript_path = %q, want %q", got, wantTranscript)
	}
	if got := logRecordInt64(t, end, "ticket_id"); got != ticket.ID {
		t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, end, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
	}

	if strings.Contains(logBuf.String(), wantFinalMessage) {
		t.Errorf("log buffer contains the final message text:\n%s", logBuf.String())
	}
}

// TestRunJob_EndLogCountsStopHook proves the "runJob end" INFO line
// carries validate_denied, stop_hook_events, stop_hook_blocks and
// stop_hook_unread from the runtime's RunResult, next to ticket_id and
// run_id. Not t.Parallel: it swaps slog's process-wide default to
// capture the record, the same constraint
// TestRunJob_EndLogNamesEvidenceNotContents above has.
func TestRunJob_EndLogCountsStopHook(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	stub := stubRunResult{res: runtime.RunResult{
		ValidateDenied: 2,
		StopHookEvents: 3,
		StopHookBlocks: 1,
		StopHookUnread: 1,
	}}
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: stub, testRuntimeCodex: stub, runtimeFake: stub})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(),
	}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	rr, err := runJob(t.Context(), d, ticket, testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}

	recs := jsonLogRecords(t, &logBuf)
	end := findLogRecord(t, recs, "runJob end")
	if got := logRecordInt64(t, end, "validate_denied"); got != 2 {
		t.Errorf("validate_denied = %d, want 2", got)
	}
	if got := logRecordInt64(t, end, "stop_hook_events"); got != 3 {
		t.Errorf("stop_hook_events = %d, want 3", got)
	}
	if got := logRecordInt64(t, end, "stop_hook_blocks"); got != 1 {
		t.Errorf("stop_hook_blocks = %d, want 1", got)
	}
	if got := logRecordInt64(t, end, "stop_hook_unread"); got != 1 {
		t.Errorf("stop_hook_unread = %d, want 1", got)
	}
	if got := logRecordInt64(t, end, "ticket_id"); got != ticket.ID {
		t.Errorf("ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, end, "run_id"); got != rr.Reserved.RunID {
		t.Errorf("run_id = %d, want %d", got, rr.Reserved.RunID)
	}
}

// TestRetryCapBudget_LogsBranchAtInfo proves retryCapBudget's (#25,
// planning.go) two INFO records -- "cap_budget retry resumes" when the
// budget has room, "cap_budget retry still over budget" when it does not --
// both carry ticket_id, agent_seconds, and cap_seconds, and both are
// dropped once settings.log_level drops below INFO. Not t.Parallel: it
// swaps slog's process-wide default to capture the records, the same
// constraint TestRunJob_EndLogNamesEvidenceNotContents above has.
func TestRetryCapBudget_LogsBranchAtInfo(t *testing.T) {
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	seedAgentSeconds(t, s, ticketID, 10)
	ticket := getRunJobTicket(t, s, ticketID)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	stillOverCommit, err := retryCapBudget(t.Context(), ticket, Deps{Store: s, Budget: 10 * time.Second}, nil, 0)
	if err != nil {
		t.Fatalf("retryCapBudget (still over budget): %v", err)
	}
	if stillOverCommit.Escalation == nil || stillOverCommit.Escalation.Payload.Code != string(response.EscalationCodeWallClock) {
		t.Errorf("still-over commit escalation = %+v, want wall_clock", stillOverCommit.Escalation)
	}

	resumesCommit, err := retryCapBudget(t.Context(), ticket, Deps{Store: s, Budget: 11 * time.Second}, nil, 0)
	if err != nil {
		t.Fatalf("retryCapBudget (budget has room): %v", err)
	}
	if resumesCommit.Escalation != nil {
		t.Errorf("resumes commit escalation = %+v, want nil", resumesCommit.Escalation)
	}

	recs := jsonLogRecords(t, &logBuf)
	resumes := findLogRecord(t, recs, "cap_budget retry resumes")
	if got := logRecordInt64(t, resumes, "ticket_id"); got != ticket.ID {
		t.Errorf("resumes ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, resumes, "agent_seconds"); got != 10 {
		t.Errorf("resumes agent_seconds = %d, want 10", got)
	}
	if got := logRecordInt64(t, resumes, "cap_seconds"); got != 11 {
		t.Errorf("resumes cap_seconds = %d, want 11", got)
	}

	stillOver := findLogRecord(t, recs, "cap_budget retry still over budget")
	if got := logRecordInt64(t, stillOver, "ticket_id"); got != ticket.ID {
		t.Errorf("still-over ticket_id = %d, want %d", got, ticket.ID)
	}
	if got := logRecordInt64(t, stillOver, "agent_seconds"); got != 10 {
		t.Errorf("still-over agent_seconds = %d, want 10", got)
	}
	if got := logRecordInt64(t, stillOver, "cap_seconds"); got != 10 {
		t.Errorf("still-over cap_seconds = %d, want 10", got)
	}

	logBuf.Reset()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if _, err := retryCapBudget(t.Context(), ticket, Deps{Store: s, Budget: 11 * time.Second}, nil, 0); err != nil {
		t.Fatalf("retryCapBudget (budget has room, warn level): %v", err)
	}
	if _, err := retryCapBudget(t.Context(), ticket, Deps{Store: s, Budget: 10 * time.Second}, nil, 0); err != nil {
		t.Fatalf("retryCapBudget (still over budget, warn level): %v", err)
	}
	if logged := logBuf.String(); strings.Contains(logged, "cap_budget retry") {
		t.Errorf("log buffer at WARN contains a cap_budget retry record, want neither:\n%s", logged)
	}
}

// TestRaiseCapBudget_LiftsOnlyThatTicket proves a budget_raised event on one
// ticket lifts only that ticket's own ticketBudget (runjob.go), not any
// other ticket's: raising ticket A by budgetRaiseMinutes resumes it, while
// ticket B, claimed with the same global budget and the same spent agent
// seconds, still returns ErrBudget (design section 6.7).
func TestRaiseCapBudget_LiftsOnlyThatTicket(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketA := seedRunJobTicket(t, s)
	projectID, err := s.EnsureProject(t.Context(), runJobTestProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketB, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "fake#2", Title: "t2", State: stateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	seedAgentSeconds(t, s, ticketA, 10)
	seedAgentSeconds(t, s, ticketB, 10)

	ownerA, expiresA := claimRunJobTicket(t, s, ticketA)
	raiseDeps := Deps{Store: s, Budget: 10 * time.Second, Owner: ownerA, Expires: expiresA}
	commit, err := retryCapBudget(t.Context(), getRunJobTicket(t, s, ticketA), raiseDeps, nil, budgetRaiseMinutes)
	if err != nil {
		t.Fatalf("retryCapBudget: %v", err)
	}
	if commit.Escalation != nil {
		t.Errorf("commit.Escalation = %+v, want nil", commit.Escalation)
	}
	if len(commit.Messages) != 2 {
		t.Fatalf("commit.Messages = %+v, want a marker plus one budget_raised event", commit.Messages)
	}
	if commit.Messages[0].Body != markerRetryRequested {
		t.Errorf("commit.Messages[0].Body = %q, want %q", commit.Messages[0].Body, markerRetryRequested)
	}
	ev := commit.Messages[1]
	if ev.EventKind == nil || *ev.EventKind != store.EventKindBudgetRaised {
		t.Fatalf("commit.Messages[1].EventKind = %v, want %q", ev.EventKind, store.EventKindBudgetRaised)
	}
	var payload response.BudgetRaisedEvent
	if err = json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal budget_raised payload: %v", err)
	}
	if payload.Minutes != 60 {
		t.Errorf("payload.Minutes = %d, want 60", payload.Minutes)
	}

	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	ownerA2, expiresA2 := claimRunJobTicket(t, s, ticketA)
	recA := &recordingReserve{fn: realReserve(s, ownerA2, expiresA2)}
	dA := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: 10 * time.Second, Owner: ownerA2, Expires: expiresA2, Reserve: recA.Reserve, DataDir: t.TempDir(),
	}
	_, err = runJob(t.Context(), dA, getRunJobTicket(t, s, ticketA), testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: runtimeFake}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if errors.Is(err, ErrBudget) {
		t.Fatalf("ticket A err = %v, want some error other than ErrBudget (the budget check passed)", err)
	}
	if recA.calls != 1 {
		t.Errorf("ticket A Reserve calls = %d, want 1", recA.calls)
	}

	ownerB, expiresB := claimRunJobTicket(t, s, ticketB)
	recB := &recordingReserve{fn: realReserve(s, ownerB, expiresB)}
	dB := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAlias: testModelExact},
		Budget: 10 * time.Second, Owner: ownerB, Expires: expiresB, Reserve: recB.Reserve, DataDir: t.TempDir(),
	}
	_, err = runJob(t.Context(), dB, getRunJobTicket(t, s, ticketB), testJobClassify, store.SessionUpsert{Job: testJobClassify, Runtime: runtimeFake}, runtime.RunRequest{Job: response.JobClassify}, nil, nil, 0)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("ticket B err = %v, want errors.Is(err, ErrBudget)", err)
	}
	if recB.calls != 0 {
		t.Errorf("ticket B Reserve calls = %d, want 0", recB.calls)
	}
}

// TestRetryCapBudget_RaiseStillOverBudget proves retryCapBudget's
// still-over-budget branch also carries the budget_raised event when
// raiseMinutes is above 0 (design section 6.7): a raise that is not enough
// re-escalates wall_clock, offering chip d again, and still records the
// event -- a second pick then compares against the sum of both.
func TestRetryCapBudget_RaiseStillOverBudget(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	seedAgentSeconds(t, s, ticketID, 4000)
	ticket := getRunJobTicket(t, s, ticketID)

	commit, err := retryCapBudget(t.Context(), ticket, Deps{Store: s, Budget: 0}, nil, budgetRaiseMinutes)
	if err != nil {
		t.Fatalf("retryCapBudget: %v", err)
	}
	if commit.Escalation == nil || commit.Escalation.Payload.Code != string(response.EscalationCodeWallClock) ||
		commit.Escalation.Payload.Origin != string(response.EscalationOriginCapBudget) {
		t.Fatalf("commit.Escalation = %+v, want wall_clock/cap_budget", commit.Escalation)
	}
	wantExtra := []response.Option{{Key: "d", Text: "raise this ticket's budget by 60 minutes"}}
	if !reflect.DeepEqual(commit.Escalation.ExtraOptions, wantExtra) {
		t.Errorf("commit.Escalation.ExtraOptions = %+v, want %+v", commit.Escalation.ExtraOptions, wantExtra)
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one budget_raised event", commit.Messages)
	}
	ev := commit.Messages[0]
	if ev.EventKind == nil || *ev.EventKind != store.EventKindBudgetRaised {
		t.Fatalf("commit.Messages[0].EventKind = %v, want %q", ev.EventKind, store.EventKindBudgetRaised)
	}
	var payload response.BudgetRaisedEvent
	if err = json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal budget_raised payload: %v", err)
	}
	if payload.Minutes != 60 {
		t.Errorf("payload.Minutes = %d, want 60", payload.Minutes)
	}
}
