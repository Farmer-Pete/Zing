package dispatch_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
	"zing/internal/tracker"
)

// The pipeline state names and the owner id this file's tests share, named
// once so goconst has nothing to flag.
const (
	testStateQueued    = "queued"
	testStatePlanning  = "planning"
	testStateBuilding  = "building"
	testStateReviewing = "reviewing"
	testStateJudging   = "judging"
	testStateShipping  = "shipping"
	testStateDone      = "done"

	testOwner       = "test-host-1"
	testFixtureRef  = "fake#1" // fixtures/tickets.toml's one ticket
	testTicketTitle = "a ticket"
	testBindingUser = "peter" // the Binding.User most tests here share (goconst)

	// testSeedReason and testSpyReason are the fixed Reason strings every
	// direct-to-state seed commit and spyHandler in this file shares
	// (goconst): neither is read back by anything a test asserts on.
	testSeedReason = "test setup"
	testSpyReason  = "test"

	// testSeedJudgingOwner is the claim owner every test that seeds a
	// ticket straight into "judging" (then substitutes a spyHandler for
	// it) shares (goconst): never read back by anything a test asserts
	// on.
	testSeedJudgingOwner = "seed-judging-owner"

	testWaitingQuestions = "questions"
	testWaitingGate      = "gate"
	testQuestionOpen     = "open"

	testReasonPlanReady = "plan ready"
	testRuntimeFake     = "fake"

	testMsgTypeEscalation = "escalation"
	testMsgTypeUpdate     = "update"
	testArtifactTypePlan  = "plan"
	testOutcomeError      = "error"
	// testQuestionLiteral is "question" itself, shared by every literal
	// that needs the exact string (a message Type or a run Outcome) so
	// goconst sees one definition, not three (no story behind the value).
	testQuestionLiteral = "question"
	testModelClaudeX    = "claude-x"
)

// testProject is the one project every test in this file seeds. LocalPath
// is filled in per test by seedProject (testProjectDir): the planning
// handler's ready entry point (design section 6.5) opens it for real
// through os.OpenRoot to check a ready response's code claims, so it must
// be a real directory, not a placeholder path.
var testProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", Tracker: "github",
}

// readyClaimEvidencePath is the file every seeded test project carries, the
// same path fixtures/scripts/planning/2.xml's one code claim cites
// ("cmd/zing/main.go:60"), so response.CheckCodeClaims resolves it for real
// against testProjectDir's own os.Root (design section 6.5, D19).
const readyClaimEvidencePath = "cmd/zing/main.go"

// testProjectDir returns a fresh temp directory carrying
// readyClaimEvidencePath, so a ready check's os.OpenRoot(project.LocalPath)
// plus response.CheckCodeClaims can resolve the fixture cohort's one code
// claim for real.
func testProjectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, readyClaimEvidencePath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("testProjectDir: mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("testProjectDir: write %s: %v", readyClaimEvidencePath, err)
	}
	return dir
}

// testModels and testBudget are the job.Deps.Models and job.Deps.Budget
// every handler that calls runJob (classify, planning, since this task)
// needs to resolve a model alias and pass the agent-time budget check:
// the same alias table and an ample budget cmd/zing/selftest.go's own
// e2eModels/e2eBudget wire the real dispatcher with. fakeRuntime never
// reads Model, so the exact ids do not matter beyond matching machine.toml's
// alias names.
var testModels = map[string]string{
	"sonnet": "claude-sonnet-5",
	"opus":   "claude-opus-5-5",
	"fable":  "claude-fable-5-1",
	"codex":  "gpt-5.5",
}

const testBudget = 240 * time.Minute

// --- shared fixtures -------------------------------------------------------

// newDispatchTestStore opens a fresh Store on a temp-file database, closed
// on test cleanup.
func newDispatchTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// loadMachine loads the real, checked-in machine.toml, the same process
// definition zing serve loads.
func loadMachine(t *testing.T) *machine.Machine {
	t.Helper()
	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	return m
}

// fakeRuntime returns a *runtime.Fake serving the real, checked-in
// fixtures/scripts tree, the same tree zing serve wires up.
func fakeRuntime(t *testing.T) *runtime.Fake {
	t.Helper()
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	return runtime.NewFake(scriptsFS)
}

// newFixtureTracker returns a *tracker.Fixture reading the real, checked-in
// fixtures/tickets.toml.
func newFixtureTracker(t *testing.T) *tracker.Fixture {
	t.Helper()
	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}
	return tr
}

// testRuntimeSet returns a runtime.Set mapping every machine.toml runtime
// name (claude, codex, fake) to rt (design section 4.1, D2: selftest and
// e2e map all three to one Fake), so a handler's
// d.Runtimes.For(d.Machine.Jobs[job].Runtime) lookup always resolves to rt
// regardless of which runtime name a job actually names.
func testRuntimeSet(t *testing.T, rt runtime.Runtime) runtime.Set {
	t.Helper()
	set, err := runtime.NewSet(map[string]runtime.Runtime{"claude": rt, "codex": rt, "fake": rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return set
}

// testDeps builds the job.Deps a handler test drives directly (bypassing
// the dispatcher), with rt resolvable under every machine.toml runtime name,
// the real, checked-in machine.toml as Deps.Machine, testModels/testBudget
// so classify and planning can resolve a model and pass the budget check,
// and Reserve wired to a real store.Reserve closure over (owner, expires),
// exactly as dispatch.Dispatcher.runAndCommit wires it for a real Tick
// (design D13). Sandbox is always off and RequireSandbox false (this suite
// drives the fake runtime, never a real sandboxed process, design D5);
// Commands is the real, unwrapped CommandRunner (PKG8-PLAN.md section 10);
// Projects covers every seeded project whose LocalPath happens to be a real
// git repository (buildTestProjects), which is every project a building
// test seeds through seedGitBackedProject and no other project in this
// file, since testProjectDir's own plain directory has no .git for
// GitCommonDir to resolve.
func testDeps(t *testing.T, s *store.Store, rt runtime.Runtime, owner string, expires time.Time) job.Deps {
	t.Helper()
	return job.Deps{
		Store: s, Runtimes: testRuntimeSet(t, rt), Machine: loadMachine(t),
		Models: testModels, Budget: testBudget, Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return s.Reserve(ctx, ticketID, owner, expires, su, seed)
		},
		Sandboxes: sandbox.OffSet(), RequireSandbox: false,
		Commands: job.NewCommandRunner(sandbox.Off(), false),
		Projects: buildTestProjects(t, s),
		DataDir:  t.TempDir(),
		// LensesParallel bounds ROUND's own semaphore (PKG9-PLAN.md section
		// 4.3, 6.2): zero would block every lens forever the moment a test
		// drives a ticket through "reviewing" for real.
		LensesParallel: 7,
	}
}

// dispatchTestGitHub is a never-called orchestrator.GitHub, enough to
// satisfy orchestrator.New's required parameter: this file's own building
// test never pushes or opens a pull request.
type dispatchTestGitHub struct{}

func (dispatchTestGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errors.New("dispatchTestGitHub: not implemented")
}

func (dispatchTestGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errors.New("dispatchTestGitHub: not implemented")
}

func (dispatchTestGitHub) CreateDraftPR(context.Context, string, string, string, string, string, string) (prURL string, number int, err error) {
	return "", 0, errors.New("dispatchTestGitHub: not implemented")
}

func (dispatchTestGitHub) FindPRByHead(context.Context, string, string, string, string) (prURL string, number int, ok bool, err error) {
	return "", 0, false, errors.New("dispatchTestGitHub: not implemented")
}

// buildTestProjects returns a job.Project for every store project whose
// LocalPath is a real git repository: orchestrator.New never fails on a
// plain directory, but GitCommonDir does, so a project seeded through the
// ordinary testProjectDir (no .git) is silently left out, and only a
// project seeded through seedGitBackedProject ever reaches the building
// handler with what it needs.
func buildTestProjects(t *testing.T, s *store.Store) map[int64]job.Project {
	t.Helper()
	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	out := make(map[int64]job.Project, len(projects))
	for _, p := range projects {
		orch, orchErr := orchestrator.New(
			orchestrator.Project{Owner: "fixture", Repo: "fixture", LocalPath: p.LocalPath, DefaultBranch: "main"},
			dispatchTestGitHub{}, orchestrator.NewRunner(), nil)
		if orchErr != nil {
			continue
		}
		repoGit, gitErr := orch.GitCommonDir(t.Context())
		if gitErr != nil {
			continue // not a git repository; never reached by a building test
		}
		out[p.ID] = job.Project{Orch: orch, RepoGit: repoGit, TestCmd: "test -f hello.txt", LintCmd: "true"}
	}
	return out
}

// seedGitBackedProject inserts a project whose LocalPath is a real, signed
// gitfixture repository carrying readyClaimEvidencePath at HEAD (PKG8-PLAN.md
// section 9.4, 10): the one shape the real building handler needs to run
// EnsureWorktree, CommitTask, and SignedStatus for real.
func seedGitBackedProject(t *testing.T, s *store.Store) int64 {
	t.Helper()
	dir := t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), dir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	if err := gitfixture.AddFile(t.Context(), dir, readyClaimEvidencePath, []byte("package main\n")); err != nil {
		t.Fatalf("gitfixture.AddFile: %v", err)
	}
	proj := testProject
	proj.LocalPath = dir
	id, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return id
}

// seedQueuedGitBackedTicket inserts one queued ticket under ref on a fresh
// git-backed project (seedGitBackedProject), for a test that drives it all
// the way through building.
func seedQueuedGitBackedTicket(t *testing.T, s *store.Store, ref string) int64 {
	t.Helper()
	projectID := seedGitBackedProject(t, s)
	id, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	return id
}

// seedProject inserts testProject and returns its id.
func seedProject(t *testing.T, s *store.Store) int64 {
	t.Helper()
	proj := testProject
	proj.LocalPath = testProjectDir(t)
	id, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return id
}

// seedQueuedTicket inserts one queued ticket under ref on testProject.
func seedQueuedTicket(t *testing.T, s *store.Store, ref string) int64 {
	t.Helper()
	projectID := seedProject(t, s)
	id, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	return id
}

func getTicket(t *testing.T, s *store.Store, id int64) store.Ticket {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTicket(%d): %v", id, err)
	}
	return ticket
}

// advanceTicket runs the real handler registered for each of states, in
// order, applying every commit directly against s (bypassing the
// dispatcher), so a test can arrange a ticket already sitting in the state
// right after the last one named. Mirrors
// internal/job/skeleton_test.go's advanceThroughStates.
func advanceTicket(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, states ...string) {
	t.Helper()
	for _, state := range states {
		ticket := getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("advanceTicket(%s): ticket state = %q, want %q", state, ticket.State, state)
		}
		if state == testStatePlanning {
			advancePlanning(t, s, rt, ticketID)
			continue
		}
		if state == testStateBuilding {
			advanceBuilding(t, s, rt, ticketID)
			continue
		}
		if state == testStateJudging {
			advanceJudging(t, s, rt, ticketID)
			continue
		}
		runHandlerOnce(t, s, rt, ticketID, state)
	}
}

// seedTicketDirectlyToState drives ticketID straight to state with one
// claim-then-commit, the same direct-commit shape TestTickUsesInjectedClock
// already seeds shipping with: no handler runs, so a test that only needs a
// ticket actually sitting in state (never caring how it got there) skips
// running the real handler chain for a state it is not itself testing.
func seedTicketDirectlyToState(t *testing.T, s *store.Store, ticketID int64, state string) {
	t.Helper()
	owner := "seed-direct-" + state
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedTicketDirectlyToState(%s): claim: claimed=%v err=%v", state, claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires, Next: state, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seedTicketDirectlyToState(%s): commit: applied=%v err=%v", state, applied, err)
	}
}

// judgeCheckFixtureCmd and dispatchJudgeCommands mirror cmd/zing/selftest.go's
// own e2eJudgeCheckCmd and selftestCommands: the fixture cohort's scenario
// s1 carries "curl -sf localhost:8080/hello" as its check_cmd, and judging's
// own CHECK step (design section 7.5) re-runs it for real, but this suite's
// fixture project never starts a real HTTP server on port 8080. Every other
// command (the building state's own "test -f hello.txt" and "true") still
// runs for real, at the same CommandRunner seam (job.Deps.Commands).
const judgeCheckFixtureCmd = "curl -sf localhost:8080/hello"

type dispatchJudgeCommands struct {
	real job.CommandRunner
}

func (c dispatchJudgeCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	if shellCmd == judgeCheckFixtureCmd {
		return 0, nil
	}
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// advanceJudgingMaxCalls bounds advanceJudging's (and
// advanceJudgingWithCommands') own handler-call loop: START, RUN, one CHECK
// (the fixture cohort's own single checked scenario, s1), and EVALUATE is
// four calls for a round that passes outright; a round that fails once,
// drives a fix to landing, and passes on a second round (PKG9-PLAN.md
// section 19.3 task 9) takes roughly twice that. The headroom above either
// catches a stuck handler instead of hanging the test.
const advanceJudgingMaxCalls = 16

// advanceJudging drives the real judging handler through as many calls as
// it now takes to land a passing round and transition to shipping (design
// section 7): unlike the skeleton's one-shot fake pass-through, each call
// only advances one step (START, RUN, one scenario's own CHECK, or
// EVALUATE), so this loops until the ticket leaves "judging". Its own
// Deps.Commands (dispatchJudgeCommands) keeps CHECK's re-run of the fixture
// cohort's one check command from ever dialing a real server, always
// reporting the pass a live one would have.
func advanceJudging(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	advanceJudgingWithCommands(t, s, rt, ticketID, dispatchJudgeCommands{real: job.NewCommandRunner(sandbox.Off(), false)})
}

// advanceJudgingWithCommands is advanceJudging's own loop, parameterized
// over cmds (PKG9-PLAN.md section 19.3 task 9): a caller whose own
// CommandRunner must see every one of judging's own CHECK calls in order
// -- a stateful one, failing round 1's own check and passing round 2's,
// the way dispatchJudgeFailThenPassCommands does -- builds it once, ahead
// of this loop, and shares that one instance across every call the loop
// makes; advanceJudging's own cmds is stateless, so a fresh one each call
// would behave identically, but sharing one here either way costs nothing.
func advanceJudgingWithCommands(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, cmds job.CommandRunner) {
	t.Helper()
	for range advanceJudgingMaxCalls {
		ticket := getTicket(t, s, ticketID)
		owner := fmt.Sprintf("advance-%d-judging", ticketID)
		expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
		claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
		if err != nil || !claimed {
			t.Fatalf("advanceJudging: claim: claimed=%v err=%v", claimed, err)
		}
		deps := testDeps(t, s, rt, owner, expires)
		deps.Commands = cmds
		commit, err := job.Registry()[testStateJudging].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("advanceJudging: Run: %v", err)
		}
		if err = job.ValidateCommit(ticket, commit); err != nil {
			t.Fatalf("advanceJudging: ValidateCommit: %v", err)
		}
		applied, err := s.CommitHandlerResult(t.Context(), commit)
		if err != nil || !applied {
			t.Fatalf("advanceJudging: CommitHandlerResult: applied=%v err=%v", applied, err)
		}
		if getTicket(t, s, ticketID).State != testStateJudging {
			return
		}
	}
	t.Fatalf("advanceJudging: still in judging after %d handler calls", advanceJudgingMaxCalls)
}

// dispatchJudgeFailThenPassCommands is dispatchJudgeCommands with a
// checkCalls counter shared across every call (PKG9-PLAN.md section 19.3
// task 9: "the e2e passes review, a judge failure, a fix, and a judge
// pass"): its first call to judgeCheckFixtureCmd reports the exit 1 a
// server not yet listening would give, failing judge round 1 and driving
// fixtures/scripts/build/fix/1.xml's own fix to landing; every later call
// -- round 2's own re-run, after the fix -- reports the exit 0 a live one
// would, matching fixtures/scripts/judge/2/1.xml's own scripted pass
// verdict for s1. checkCalls is a pointer, not a plain int, so every
// job.Deps copy this value is handed into still shares the one counter.
type dispatchJudgeFailThenPassCommands struct {
	real       job.CommandRunner
	checkCalls *int32
}

// newDispatchJudgeFailThenPassCommands returns a
// dispatchJudgeFailThenPassCommands wrapping real, its own
// judgeCheckFixtureCmd call counter freshly zeroed.
func newDispatchJudgeFailThenPassCommands(realRunner job.CommandRunner) dispatchJudgeFailThenPassCommands {
	return dispatchJudgeFailThenPassCommands{real: realRunner, checkCalls: new(int32)}
}

func (c dispatchJudgeFailThenPassCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	if shellCmd == judgeCheckFixtureCmd {
		if atomic.AddInt32(c.checkCalls, 1) == 1 {
			return 1, nil // round 1: the scenario's own check fails, forcing a fix
		}
		return 0, nil // round 2, after the fix lands: the check passes
	}
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// judgeRoundMarkerBodies returns ticketID's own "update" message bodies, in
// id order, for TestJudgeFailureFixThenPass' own marker assertions below.
func judgeRoundMarkerBodies(t *testing.T, s *store.Store, ticketID int64) []string {
	t.Helper()
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var bodies []string
	for i := range msgs {
		if msgs[i].Type == testMsgTypeUpdate {
			bodies = append(bodies, msgs[i].Body)
		}
	}
	return bodies
}

// TestJudgeFailureFixThenPass proves PKG9-PLAN.md section 19.3 task 9's own
// e2e, at the real dispatcher's own cut point (a handler driven through
// Registry() with a real store, the fake runtime, and real git, mirroring
// every other test in this file rather than the fake-runtime-only unit
// proof internal/job/judging_test.go's own TestJudgeFixThenPass already
// gives): review passes (every lens fixture returns zero findings), judge
// round 1 fails (dispatchJudgeFailThenPassCommands' own first call),
// building lands the fix request fixtures/scripts/build/fix/1.xml scripts,
// and judge round 2 -- fixtures/scripts/judge/2/1.xml's own first turn --
// passes, carrying the ticket on to shipping.
func TestJudgeFailureFixThenPass(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedGitBackedTicket(t, s, testFixtureRef)

	advanceTicket(t, s, rt, ticketID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing)

	cmds := newDispatchJudgeFailThenPassCommands(job.NewCommandRunner(sandbox.Off(), false))
	advanceJudgingWithCommands(t, s, rt, ticketID, cmds)

	ticket := getTicket(t, s, ticketID)
	if ticket.State != testStateShipping {
		t.Fatalf("ticket state = %q, want %q (judge round 2 must pass after the fix lands)", ticket.State, testStateShipping)
	}

	bodies := judgeRoundMarkerBodies(t, s, ticketID)
	var sawFailed, sawLanded, sawPassed bool
	for _, body := range bodies {
		switch {
		case strings.HasPrefix(body, "judge round 1 failed"):
			sawFailed = true
		case strings.HasPrefix(body, "fix landed ") && sawFailed && !sawPassed:
			sawLanded = true
		case body == "judge round 2 passed":
			sawPassed = true
		}
	}
	if !sawFailed {
		t.Error(`no "judge round 1 failed" marker, want judge round 1 to fail`)
	}
	if !sawLanded {
		t.Error(`no "fix landed" marker after judge round 1 failed, want the fix request to land`)
	}
	if !sawPassed {
		t.Error(`no "judge round 2 passed" marker, want round 2 to pass after the fix landed`)
	}
}

// testShipGitHubOwner is the placeholder owner and repo name
// dispatchShipGitHub's own test uses: it never calls the real GitHub API,
// so the exact value only has to be non-empty.
const testShipGitHubOwner = "zing-fixture"

// dispatchShipPR is one pull request dispatchShipGitHub has created.
type dispatchShipPR struct {
	url, head, base string
	number          int
}

// dispatchShipGitHub is a stateful, scripted orchestrator.GitHub double
// this file's own shipping e2e test needs (PKG9-PLAN.md section 19.4 task
// 8), unlike dispatchTestGitHub above (never called): CreateDraftPR,
// FindPRByHead, GetPR, and ListCheckRuns here are real enough to carry one
// pull request through a CI failure, a landed ci_log fix, the push that
// follows, and the merge GitHub reports once the pushed commit's checks
// have read green -- the same "the owner merged once CI went green" shape
// cmd/zing/selftest.go's own selftestShipGitHub scripts, since M3 builds
// neither the ready flip nor MERGE itself (shipping.go's own header
// comment). redSHA is the commit PUBLISH first pushed, read straight off
// the real bare origin remoteDir rather than a canned field this double
// would otherwise have to be told about, so a real git push is what
// actually moves what GetPR reports: every check run on redSHA always
// fails; every other sha (the one the landed fix later pushes) always
// succeeds, and GetPR reports the pull request merged starting on its own
// second read of that other sha, so one CI-green poll is actually observed
// before done.
type dispatchShipGitHub struct {
	mu          sync.Mutex
	remoteDir   string
	pr          *dispatchShipPR
	nextNum     int
	redSHA      string
	callsForSHA map[string]int
	threads     []orchestrator.Thread
	replies     map[string]string // raw thread id -> the body ReplyToThread posted
}

// dispatchShipReplyThreadID and dispatchShipFixThreadID are the two review
// threads newDispatchShipGitHub seeds (PKG9-PLAN.md section 19.5 task 10),
// cmd/zing/selftest.go's own selftestShipReplyThreadID and
// selftestShipFixThreadID mirrored at this package's own cut point: one a
// plain "reply" fixtures/scripts/respond/1/1.xml answers, the other a
// "fix" it collects into a consolidated fix request (design section 9.3
// step 3), so this test walks RESPOND, APPLY, the shared fix driver, and
// FIX-REPLIES (design section 9.4), not only PUBLISH and the ci_log fix
// M3 task 8 already proved here.
const (
	dispatchShipReplyThreadID = "RT_thread_1"
	dispatchShipFixThreadID   = "RT_thread_2"
	dispatchShipGHViewerLogin = "zing-dispatch-test-bot"
)

func newDispatchShipGitHub(remoteDir string) *dispatchShipGitHub {
	commentAt := time.Now().UTC()
	return &dispatchShipGitHub{
		remoteDir: remoteDir, callsForSHA: map[string]int{}, replies: make(map[string]string),
		threads: []orchestrator.Thread{
			{
				ID: dispatchShipReplyThreadID, Path: "cmd/zing/main.go", Line: 1,
				Comments: []orchestrator.ThreadComment{{
					ID: "c1", Author: "reviewer-bot", Body: "What does this line do?",
					CreatedAt: commentAt, UpdatedAt: commentAt,
				}},
			},
			{
				ID: dispatchShipFixThreadID, Path: "hello.txt", Line: 1,
				Comments: []orchestrator.ThreadComment{{
					ID: "c2", Author: "reviewer-bot", Body: "Validate this before using it.",
					CreatedAt: commentAt, UpdatedAt: commentAt,
				}},
			},
		},
	}
}

// headSHA reads branch's current commit straight off g's own real bare
// origin.
func (g *dispatchShipGitHub) headSHA(ctx context.Context, branch string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", g.remoteDir, "rev-parse", "refs/heads/"+branch)
	// Scrubbed, so a GIT_DIR a git hook exported cannot redirect "-C".
	cmd.Env = gitfixture.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("dispatchShipGitHub: rev-parse %s: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *dispatchShipGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errors.New("dispatchShipGitHub: not implemented")
}

func (g *dispatchShipGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errors.New("dispatchShipGitHub: not implemented")
}

func (g *dispatchShipGitHub) CreateDraftPR(ctx context.Context, _, _, head, base, _, _ string) (prURL string, number int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pr != nil {
		return "", 0, errors.New("dispatchShipGitHub: a pull request already exists for this head")
	}
	sha, err := g.headSHA(ctx, head)
	if err != nil {
		return "", 0, err
	}
	g.nextNum++
	g.pr = &dispatchShipPR{
		url:  fmt.Sprintf("https://github.com/%s/%s/pull/%d", testShipGitHubOwner, testShipGitHubOwner, g.nextNum),
		head: head, base: base, number: g.nextNum,
	}
	g.redSHA = sha
	return g.pr.url, g.pr.number, nil
}

func (g *dispatchShipGitHub) FindPRByHead(_ context.Context, _, _, head, base string) (prURL string, number int, ok bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pr == nil || g.pr.head != head || g.pr.base != base {
		return "", 0, false, nil
	}
	return g.pr.url, g.pr.number, true, nil
}

func (g *dispatchShipGitHub) GetPR(ctx context.Context, _, _ string, _ int) (orchestrator.PRState, error) {
	g.mu.Lock()
	pr := g.pr
	redSHA := g.redSHA
	g.mu.Unlock()
	if pr == nil {
		return orchestrator.PRState{}, errors.New("dispatchShipGitHub: GetPR before CreateDraftPR")
	}
	sha, err := g.headSHA(ctx, pr.head)
	if err != nil {
		return orchestrator.PRState{}, err
	}
	g.mu.Lock()
	g.callsForSHA[sha]++
	n := g.callsForSHA[sha]
	allResolved := true
	for _, th := range g.threads {
		if !th.IsResolved {
			allResolved = false
			break
		}
	}
	g.mu.Unlock()
	return orchestrator.PRState{
		Number: pr.number, State: "open", Draft: true, HeadSHA: sha, BaseRef: pr.base,
		// allResolved gates the same synthesized merge the package doc
		// comment on dispatchShipGitHub's own MarkReady/ConvertToDraft
		// already describes (M3's "the owner merged once CI went green"
		// shortcut): without it, a stray second GetPR call made while
		// RESPOND, APPLY, or FIX-REPLIES are still working through the
		// two seeded threads (M4 task 10) could push n to 2 and report
		// merged before either thread is actually resolved.
		Merged: sha != redSHA && n >= 2 && allResolved,
	}, nil
}

func (g *dispatchShipGitHub) Merge(context.Context, string, string, int, string, string, string) (string, error) {
	return "", errors.New("dispatchShipGitHub: Merge not implemented (M4)")
}

func (g *dispatchShipGitHub) ListCheckRuns(_ context.Context, _, _, sha string) ([]orchestrator.CheckRun, error) {
	g.mu.Lock()
	redSHA := g.redSHA
	g.mu.Unlock()
	conclusion := "success"
	if sha == redSHA {
		conclusion = "failure"
	}
	return []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: "completed", Conclusion: conclusion, AppSlug: "github-actions"}}, nil
}

func (g *dispatchShipGitHub) ListStatuses(context.Context, string, string, string) ([]orchestrator.CommitStatus, error) {
	return nil, nil
}

func (g *dispatchShipGitHub) RequiredCheckRules(context.Context, string, string, string) ([]orchestrator.RequiredCheck, error) {
	return []orchestrator.RequiredCheck{{Context: "ci"}}, nil
}

func (g *dispatchShipGitHub) JobLogTail(context.Context, string, string, int64, int) (string, error) {
	return "", nil
}

func (g *dispatchShipGitHub) RerunJob(context.Context, string, string, int64) error {
	return nil
}

// MarkReady and ConvertToDraft give dispatchShipGitHub job.DraftFlips too
// (M4 task 7): this fake always reports Draft: true (GetPR, above), so
// this file's own shipping e2e necessarily reaches row 8's ready flip on
// its first clean-sha poll before GetPR starts reporting merged; both
// calls just succeed, since this test cares about the ticket's own state
// and markers, not what a draft flip posts.
func (g *dispatchShipGitHub) MarkReady(context.Context, string) error {
	return nil
}

func (g *dispatchShipGitHub) ConvertToDraft(context.Context, string) error {
	return nil
}

// ListThreads, ThreadCommentsContain, ReplyToThread, and ResolveThread give
// dispatchShipGitHub job.ReviewThreads too (M4 tasks 4, 10): this test's
// own fixture ticket does open two real review threads
// (newDispatchShipGitHub), so these four track and mutate real state the
// same way GetPR's own callsForSHA bookkeeping does above, mirroring
// cmd/zing/selftest.go's own selftestShipGitHub at this package's own cut
// point.
func (g *dispatchShipGitHub) ListThreads(context.Context, string, string, int) ([]orchestrator.Thread, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]orchestrator.Thread, len(g.threads))
	copy(out, g.threads)
	return out, nil
}

func (g *dispatchShipGitHub) ThreadCommentsContain(_ context.Context, rawID, needle, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Contains(g.replies[rawID], needle), nil
}

func (g *dispatchShipGitHub) ReplyToThread(_ context.Context, rawID, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.IndexFunc(g.threads, func(th orchestrator.Thread) bool { return th.ID == rawID })
	if i < 0 {
		return fmt.Errorf("dispatchShipGitHub: ReplyToThread: unknown thread %s", rawID)
	}
	g.replies[rawID] = body
	now := time.Now().UTC()
	g.threads[i].Comments = append(g.threads[i].Comments, orchestrator.ThreadComment{
		ID: "zing-reply-" + rawID, Author: dispatchShipGHViewerLogin, Body: body, CreatedAt: now, UpdatedAt: now,
	})
	return nil
}

func (g *dispatchShipGitHub) ResolveThread(_ context.Context, rawID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.IndexFunc(g.threads, func(th orchestrator.Thread) bool { return th.ID == rawID })
	if i < 0 {
		return fmt.Errorf("dispatchShipGitHub: ResolveThread: unknown thread %s", rawID)
	}
	g.threads[i].IsResolved = true
	return nil
}

func (g *dispatchShipGitHub) ListReviews(context.Context, string, string, int) ([]orchestrator.Review, error) {
	return nil, nil
}

func (g *dispatchShipGitHub) RequestReviewers(context.Context, string, string, int, string) error {
	return errors.New("dispatchShipGitHub: not implemented")
}

func (g *dispatchShipGitHub) Viewer(context.Context) (string, error) {
	return dispatchShipGHViewerLogin, nil
}

func (g *dispatchShipGitHub) CommentOnPR(context.Context, string, string, int, string) error {
	return errors.New("dispatchShipGitHub: not implemented")
}

var (
	_ orchestrator.GitHub = (*dispatchShipGitHub)(nil)
	_ job.PullRequests    = (*dispatchShipGitHub)(nil)
	_ job.Checks          = (*dispatchShipGitHub)(nil)
	_ job.ReviewThreads   = (*dispatchShipGitHub)(nil)
	_ job.DraftFlips      = (*dispatchShipGitHub)(nil)
)

// dispatchShipTracker is a minimal job.ShipTracker double for this file's
// own direct-handler shipping e2e: it only counts calls, since this test
// cares about the ticket's own state and markers, not what the tracker
// posts (shipTrackerDouble, above, already covers PostPRLink/PostDone's
// own marker and ordering rules against the real Dispatcher).
type dispatchShipTracker struct {
	mu             sync.Mutex
	prLinks, dones int
}

func (tr *dispatchShipTracker) PostPRLink(context.Context, int64, string, string) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.prLinks++
	return nil
}

func (tr *dispatchShipTracker) PostDone(context.Context, int64, string, string, string) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.dones++
	return nil
}

var _ job.ShipTracker = (*dispatchShipTracker)(nil)

// advanceShippingMaxCalls bounds advanceShipping's own handler-call loop:
// PUBLISH, one failed POLL (ci_log fix request), RUN, CHECK-then-LAND, one
// POLL that pushes, one POLL that starts a respond batch once CI reads
// green (design section 19.5 task 10's own two seeded threads), RESPOND,
// APPLY (posts the plain reply and requests a fix for the other thread),
// RUN, CHECK-then-LAND for that fix, one POLL that pushes it, one POLL for
// FIX-REPLIES, one POLL for RE-REQUEST, one POLL that reads every check and
// thread clean (ready, idle), and one POLL that finds the pull request
// merged is sixteen calls; the headroom catches a stuck handler instead of
// hanging the test.
const advanceShippingMaxCalls = 24

// advanceShipping drives the real shipping handler through as many calls as
// it now takes to open a draft pull request, land a ci_log fix once GitHub
// reports its checks failed, push the fix, and reach "done" once GitHub
// reports the pull request merged (PKG9-PLAN.md section 19.4 task 8).
// Unlike advanceJudging's own fixed-shape round, POLL's own idle commit
// (design section 8.3) sets a real Poll{NextAt}, but this loop drives the
// handler directly, never through the dispatcher's own
// ListReadyCandidates, so that backoff is never actually waited out. Each
// call rebuilds the project's own *orchestrator.Orchestrator over gh, the
// same way shipClaim (internal/job/shipping_test.go) rebuilds Deps on every
// claim.
func advanceShipping(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, gh *dispatchShipGitHub, tr job.ShipTracker) {
	t.Helper()
	for range advanceShippingMaxCalls {
		ticket := getTicket(t, s, ticketID)
		owner := fmt.Sprintf("advance-%d-shipping", ticketID)
		expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
		claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
		if err != nil || !claimed {
			t.Fatalf("advanceShipping: claim: claimed=%v err=%v", claimed, err)
		}

		proj, err := s.ProjectForTicket(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("advanceShipping: ProjectForTicket: %v", err)
		}
		orch, err := orchestrator.New(
			orchestrator.Project{Owner: testShipGitHubOwner, Repo: testShipGitHubOwner, LocalPath: proj.LocalPath, DefaultBranch: "main"},
			gh, orchestrator.NewRunner(), nil)
		if err != nil {
			t.Fatalf("advanceShipping: orchestrator.New: %v", err)
		}
		repoGit, err := orch.GitCommonDir(t.Context())
		if err != nil {
			t.Fatalf("advanceShipping: GitCommonDir: %v", err)
		}

		deps := testDeps(t, s, rt, owner, expires)
		deps.Projects = map[int64]job.Project{
			ticket.ProjectID: {
				Orch: orch, RepoGit: repoGit, TestCmd: "test -f hello.txt", LintCmd: "true",
				Owner: testShipGitHubOwner, Repo: testShipGitHubOwner,
				PullRequests: gh, Checks: gh, Threads: gh, Flips: gh,
			},
		}
		deps.Tracker = tr

		commit, err := job.Registry()[testStateShipping].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("advanceShipping: Run: %v", err)
		}
		if err = job.ValidateCommit(ticket, commit); err != nil {
			t.Fatalf("advanceShipping: ValidateCommit: %v", err)
		}
		applied, err := s.CommitHandlerResult(t.Context(), commit)
		if err != nil || !applied {
			t.Fatalf("advanceShipping: CommitHandlerResult: applied=%v err=%v", applied, err)
		}
		if getTicket(t, s, ticketID).State != testStateShipping {
			return
		}
	}
	t.Fatalf("advanceShipping: still in shipping after %d handler calls", advanceShippingMaxCalls)
}

// TestShipCIFailThenFixThenMergeGoesDone proves the shipping state machine
// end to end, at this package's own cut point (PKG9-PLAN.md section 19.4
// task 8): PUBLISH opens a draft pull request, POLL finds its checks
// failed and requests a ci_log fix, the fix lands, POLL pushes it, POLL
// finds the pushed commit's checks green, and POLL finds the pull request
// merged -- the "the owner merged once CI went green" shape M3 scripts,
// since the ready flip and MERGE itself are M4's own (shipping.go's own
// header comment).
func TestShipCIFailThenFixThenMergeGoesDone(t *testing.T) {
	t.Parallel()
	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedGitBackedTicket(t, s, testFixtureRef)

	advanceTicket(t, s, rt, ticketID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing)
	advanceJudging(t, s, rt, ticketID)

	ticket := getTicket(t, s, ticketID)
	if ticket.State != testStateShipping {
		t.Fatalf("ticket state = %q, want %q (the judge round must pass before shipping)", ticket.State, testStateShipping)
	}

	proj, err := s.ProjectForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	remoteDir, err := gitfixture.WithBareOrigin(t.Context(), proj.LocalPath)
	if err != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", err)
	}

	gh := newDispatchShipGitHub(remoteDir)
	tr := &dispatchShipTracker{}
	advanceShipping(t, s, rt, ticketID, gh, tr)

	ticket = getTicket(t, s, ticketID)
	if ticket.State != testStateDone {
		t.Fatalf("ticket state = %q, want %q", ticket.State, testStateDone)
	}

	bodies := judgeRoundMarkerBodies(t, s, ticketID)
	var sawPROpened, sawFixRequested, sawFixLanded bool
	var sawBatchStarted, sawThreadsRequested, sawRepliesPosted bool
	landedCount := 0
	for _, body := range bodies {
		switch {
		case strings.HasPrefix(body, "pr opened "):
			sawPROpened = true
		case strings.HasPrefix(body, "fix requested ci_log after run ") && sawPROpened:
			sawFixRequested = true
		case strings.HasPrefix(body, "respond batch 1 started sha ") && sawFixRequested:
			sawBatchStarted = true
		case strings.HasPrefix(body, "fix requested threads after run ") && sawBatchStarted:
			sawThreadsRequested = true
		case strings.HasPrefix(body, "fix landed "):
			landedCount++
			sawFixLanded = true
		case strings.HasPrefix(body, "fix replies posted ") && sawThreadsRequested && landedCount >= 2:
			sawRepliesPosted = true
		}
	}
	if !sawPROpened {
		t.Error(`no "pr opened" marker, want PUBLISH to open a draft pull request`)
	}
	if !sawFixRequested {
		t.Error(`no "fix requested ci_log" marker after the pull request opened, want POLL to request a fix once CI failed`)
	}
	if !sawFixLanded {
		t.Error(`no "fix landed" marker after the ci_log fix request, want the fix to land`)
	}
	if !sawBatchStarted {
		t.Error(`no "respond batch 1 started" marker after the ci_log fix, want POLL to start a respond batch once an actionable thread exists (PKG9-PLAN.md section 19.5 task 10)`)
	}
	if !sawThreadsRequested {
		t.Error(`no "fix requested threads" marker after the respond batch started, want APPLY to collect the other thread into a fix request`)
	}
	if landedCount < 2 {
		t.Errorf(`%d "fix landed" markers, want at least 2 (the ci_log fix and the threads fix)`, landedCount)
	}
	if !sawRepliesPosted {
		t.Error(`no "fix replies posted" marker after the threads fix landed, want FIX-REPLIES to close the loop`)
	}
	reply, posted := gh.replies[dispatchShipReplyThreadID]
	if !posted {
		t.Error("no reply recorded for the review thread, want APPLY to post one")
	}
	wantPrefix := "Zing (an AI agent) replying on behalf of @" + dispatchShipGHViewerLogin + ":"
	if !strings.HasPrefix(reply, wantPrefix) {
		t.Errorf("posted reply = %q, want it to start with the disclosure prefix %q (design D10)", reply, wantPrefix)
	}
	for _, th := range gh.threads {
		if !th.IsResolved {
			t.Errorf("thread %s is still unresolved, want both seeded threads resolved by the time the ticket reaches done", th.ID)
		}
	}
	if tr.dones != 1 {
		t.Errorf("PostDone calls = %d, want 1", tr.dones)
	}
	if gh.pr == nil || gh.pr.number != 1 {
		t.Errorf("gh.pr = %+v, want exactly one pull request", gh.pr)
	}
}

// advanceBuildingMaxCalls bounds advanceBuilding's own handler-call loop
// (PKG8-PLAN.md task 9): each of the fixture plan's three tasks takes one
// RUN call and one CHECK-then-LAND call, so six calls lands every task; the
// headroom catches a stuck handler instead of hanging the test.
const advanceBuildingMaxCalls = 8

// advanceBuilding drives the real building handler through as many calls as
// it now takes to land every fixture task and transition to reviewing
// (design section 6): unlike the skeleton's one-shot fake build, each call
// only advances one step (RUN, or CHECK and LAND together), so this loops
// until the ticket leaves "building".
func advanceBuilding(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	for range advanceBuildingMaxCalls {
		runHandlerOnce(t, s, rt, ticketID, testStateBuilding)
		if getTicket(t, s, ticketID).State != testStateBuilding {
			return
		}
	}
	t.Fatalf("advanceBuilding: still in building after %d handler calls", advanceBuildingMaxCalls)
}

// advancePlanningMaxCalls bounds advancePlanning's own handler-call loop:
// classify (kind unset, stays planning), the first turn (posts questions,
// waits), the resume (stores the cohort, stays planning), the review tick
// (clean, posts the gate, design section 6.6), and the owner's approve
// (seals the cohort, transitions to building) is five calls; the headroom
// catches a stuck handler instead of hanging the test.
const advancePlanningMaxCalls = 6

// advancePlanning drives the real planning handler through as many calls as
// it now takes to reach building (design section 5.1): classify runs first
// on a kindless ticket and sets kind but carries no transition, so this
// loops the handler until either the ticket leaves planning or it waits on
// "questions" or "gate", in which case it answers the batch (answerOpenQuestion
// always takes the first offered option, "a" Approve for the gate) and keeps
// looping.
func advancePlanning(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	for range advancePlanningMaxCalls {
		runHandlerOnce(t, s, rt, ticketID, testStatePlanning)
		after := getTicket(t, s, ticketID)
		if after.State != testStatePlanning {
			return
		}
		if after.WaitingOn != nil && (*after.WaitingOn == testWaitingQuestions || *after.WaitingOn == testWaitingGate) {
			answerOpenQuestion(t, s, ticketID)
			continue
		}
		if after.WaitingOn != nil {
			t.Fatalf("advancePlanning: ticket waiting_on = %q, want questions, gate, or nil", *after.WaitingOn)
		}
	}
	t.Fatalf("advancePlanning: still in planning after %d handler calls", advancePlanningMaxCalls)
}

// runHandlerOnce claims the ticket, runs its state's handler once, and applies
// the resulting commit directly against s, bypassing the dispatcher.
func runHandlerOnce(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, state string) {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	owner := fmt.Sprintf("advance-%d-%s", ticketID, state)
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("advanceTicket(%s): claim: claimed=%v err=%v", state, claimed, err)
	}
	commit, err := job.Registry()[state].Run(t.Context(), ticket, testDeps(t, s, rt, owner, expires))
	if err != nil {
		t.Fatalf("advanceTicket(%s) Run: %v", state, err)
	}
	if err = job.ValidateCommit(ticket, commit); err != nil {
		t.Fatalf("advanceTicket(%s) ValidateCommit: %v", state, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil || !applied {
		t.Fatalf("advanceTicket(%s) CommitHandlerResult: applied=%v err=%v", state, applied, err)
	}
}

// answerOpenQuestion answers every open question on the ticket with its first
// offered option, so the planning batch is fully answered and the resume can
// run.
func answerOpenQuestion(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, testQuestionOpen)
	if err != nil {
		t.Fatalf("answerOpenQuestion: QuestionsByState: %v", err)
	}
	if len(open) == 0 {
		t.Fatalf("answerOpenQuestion: ticket %d has no open question", ticketID)
	}
	for i := range open {
		q := &open[i]
		var payload response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &payload); err != nil {
			t.Fatalf("answerOpenQuestion: unmarshal payload: %v", err)
		}
		if len(payload.Options) == 0 {
			t.Fatalf("answerOpenQuestion: question %d has no options", q.ID)
		}
		res, err := s.AnswerQuestion(t.Context(), store.AnswerInput{
			TicketID: ticketID, QuestionID: q.ID, Option: payload.Options[0].Key,
		})
		if err != nil || !res.Accepted {
			t.Fatalf("answerOpenQuestion: AnswerQuestion: accepted=%v conflict=%q err=%v", res.Accepted, res.Conflict, err)
		}
	}
}

// newDispatcher builds a Dispatcher over the real skeleton registry, unless
// reg is non-nil, in which case reg is used instead (a test's chance to
// substitute a spy handler for one state). cfg.Models and cfg.Budget default
// to testModels/testBudget when the caller leaves them unset, so a bare
// dispatch.Config{MaxParallel: N, Owner: testOwner} literal still lets
// classify and planning resolve a model and pass the budget check once a
// real Tick reaches them.
func newDispatcher(t *testing.T, s *store.Store, tr tracker.Tracker, b *bus.Broker, rt runtime.Runtime,
	reg map[string]job.Handler, bindings []dispatch.Binding, cfg dispatch.Config,
) *dispatch.Dispatcher {
	t.Helper()
	if reg == nil {
		reg = job.Registry()
	}
	if cfg.Models == nil {
		cfg.Models = testModels
	}
	if cfg.Budget == 0 {
		cfg.Budget = testBudget
	}
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	if cfg.LensesParallel == 0 {
		// Bounds ROUND's own semaphore (PKG9-PLAN.md section 4.3, 6.2): zero
		// would block every lens forever the moment a test drives a ticket
		// through "reviewing" for real.
		cfg.LensesParallel = 7
	}
	d, err := dispatch.New(s, tr, b, loadMachine(t), reg, bindings, cfg, testRuntimeSet(t, rt))
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}
	return d
}

// --- New ---------------------------------------------------------------------

// TestNew_MissingHandlerFailsAtNew proves a missing handler is caught at
// startup, never at a nil map read mid-tick (design section 6.8).
func TestNew_MissingHandlerFailsAtNew(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	m := loadMachine(t)
	reg := job.Registry()
	delete(reg, testStatePlanning)

	_, err := dispatch.New(s, newFixtureTracker(t), bus.New(), m, reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner}, testRuntimeSet(t, fakeRuntime(t)))
	if err == nil {
		t.Fatal("New with a missing handler: want an error, got nil")
	}
}

// --- Tick: reconcile, pick, claim, run, commit --------------------------------

// TestTick_ReconcilesAnExpiredClaimThenPicksAndRunsIt proves reconcile runs
// before pick (design section 6.8 step 1): a ticket whose claim already
// expired is freed and, in the same tick, claimed and advanced.
func TestTick_ReconcilesAnExpiredClaimThenPicksAndRunsIt(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	// Pre-claim with an expiry already in the past, simulating a claim a
	// prior tick left behind (or a crashed worker's lease).
	past := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, "stale-owner", past)
	if err != nil || !claimed {
		t.Fatalf("pre-claim: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want planning (reconciled then picked up)", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (cleared by the commit)", *final.ClaimOwner)
	}
}

// TestTick_IntakeInsertsAndDedupsOnASecondIntake proves intake (design
// section 6.8 step 3) inserts a new tracker ticket once, and a second tick's
// intake does not duplicate it. MaxParallel: 0 isolates intake's effect: the
// count guard (step 4) always blocks before pick, so the ticket is never
// claimed or advanced by either tick.
func TestTick_IntakeInsertsAndDedupsOnASecondIntake(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name}}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	first, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("after first Tick: %d tickets, want 1", len(first))
	}
	if first[0].TrackerRef != testFixtureRef || first[0].State != testStateQueued {
		t.Errorf("inserted ticket = %+v, want ref %s in state queued", first[0], testFixtureRef)
	}

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	second, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("after second Tick (dedup): %d tickets, want 1", len(second))
	}
}

// intakeCallTracker wraps a Tracker and records every project name Intake
// was called with, so a test can assert Intake was never called for a
// manual-mode binding (PKG9-PLAN.md D29) while still being called for an
// auto one. It embeds tracker.Tracker, the same pattern slowTracker below
// uses, so every other method just delegates.
type intakeCallTracker struct {
	tracker.Tracker
	mu       sync.Mutex
	projects []string
}

func (c *intakeCallTracker) Intake(ctx context.Context, project string, rule tracker.IntakeRule) ([]tracker.Ticket, error) {
	c.mu.Lock()
	c.projects = append(c.projects, project)
	c.mu.Unlock()
	return c.Tracker.Intake(ctx, project, rule)
}

func (c *intakeCallTracker) calledProjects() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.projects...)
}

// TestTick_IntakeSkipsManualModeProjectButRunsForAuto proves intake (design
// section 6.8 step 3, PKG9-PLAN.md D29) never calls Tracker.Intake for a
// binding whose Mode is "manual", while an "auto" binding alongside it still
// gets its ordinary automatic intake. MaxParallel: 0 isolates intake's own
// effect, as TestTick_IntakeInsertsAndDedupsOnASecondIntake above does.
func TestTick_IntakeSkipsManualModeProjectButRunsForAuto(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	autoProjectID := seedProject(t, s) // testProject.Name ("zing"), matching the fixture
	manualProjectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: "manual-proj", RepoURL: "https://github.com/x/manual", Tracker: "github",
	})
	if err != nil {
		t.Fatalf("EnsureProject(manual-proj): %v", err)
	}

	tr := &intakeCallTracker{Tracker: newFixtureTracker(t)}
	bindings := []dispatch.Binding{
		{StoreProjectID: autoProjectID, TrackerProject: testProject.Name, Mode: "auto"},
		{StoreProjectID: manualProjectID, TrackerProject: "manual-proj", Mode: "manual"},
	}

	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})
	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	called := tr.calledProjects()
	if !slices.Contains(called, testProject.Name) {
		t.Errorf("Intake calls = %v, want it to include the auto project %q", called, testProject.Name)
	}
	if slices.Contains(called, "manual-proj") {
		t.Errorf("Intake calls = %v, want no call for the manual-mode project", called)
	}

	tickets, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets after Tick = %d, want exactly 1 (only the auto project's fixture ticket)", len(tickets))
	}
	if tickets[0].ProjectID != autoProjectID {
		t.Errorf("the one inserted ticket's ProjectID = %d, want the auto project %d", tickets[0].ProjectID, autoProjectID)
	}
}

// failingIntakeTracker is a minimal Tracker test double for the intake
// resilience test below (PKG7-PLAN.md D6, task 14): Intake fails with
// errIntakeBoom for failProject and returns exactly one fixed ticket for any
// other project. Comment always succeeds, since a successful intake still
// posts a best-effort pickup comment for its new ticket; the rest are never
// called in this test.
type failingIntakeTracker struct {
	failProject string
	ticket      tracker.Ticket
}

var errIntakeBoom = errors.New("boom: intake unreachable")

func (f *failingIntakeTracker) Intake(_ context.Context, project string, _ tracker.IntakeRule) ([]tracker.Ticket, error) {
	if project == f.failProject {
		return nil, errIntakeBoom
	}
	return []tracker.Ticket{f.ticket}, nil
}

func (f *failingIntakeTracker) Fetch(context.Context, string, string) (tracker.Ticket, error) {
	panic("failingIntakeTracker: Fetch is unused by this test")
}

func (f *failingIntakeTracker) Issue(context.Context, string, string) (tracker.Ticket, error) {
	panic("failingIntakeTracker: Issue is unused by this test")
}

func (f *failingIntakeTracker) Comment(context.Context, string, string, string) error {
	return nil
}

func (f *failingIntakeTracker) FileTicket(context.Context, string, tracker.NewTicket) (string, error) {
	panic("failingIntakeTracker: FileTicket is unused by this test")
}

func (f *failingIntakeTracker) Collaborators(context.Context, string) ([]string, error) {
	panic("failingIntakeTracker: Collaborators is unused by this test")
}

func (f *failingIntakeTracker) Close(context.Context, string, string) error {
	panic("failingIntakeTracker: Close is unused by this test")
}

func (f *failingIntakeTracker) CommentContains(context.Context, string, string, string) (bool, error) {
	panic("failingIntakeTracker: CommentContains is unused by this test")
}

var _ tracker.Tracker = (*failingIntakeTracker)(nil)

// TestTick_IntakeErrorOnOneProjectLogsAndContinuesToTheNext proves intake
// resilience (PKG7-PLAN.md D6, section 9's "intake error (serve)", task 14):
// when the first of two bound projects' Tracker.Intake fails, the tick still
// returns nil, the second project's new ticket is still inserted, and a warn
// "intake error" naming the failing project and the error was logged. It
// swaps the process-wide slog default to capture that line (matching
// internal/tracker/fixture_test.go's TestFixture_CommentReturnsNil), so it
// does not run in parallel with another subtest that touches slog.
func TestTick_IntakeErrorOnOneProjectLogsAndContinuesToTheNext(t *testing.T) {
	s := newDispatchTestStore(t)

	failingProjectID := seedProject(t, s) // testProject.Name, "zing"

	okProject := testProject
	okProject.Name = "other"
	okProject.LocalPath = testProjectDir(t)
	okProjectID, ensureErr := s.EnsureProject(t.Context(), okProject)
	if ensureErr != nil {
		t.Fatalf("EnsureProject: %v", ensureErr)
	}

	bindings := []dispatch.Binding{
		{StoreProjectID: failingProjectID, TrackerProject: testProject.Name},
		{StoreProjectID: okProjectID, TrackerProject: okProject.Name},
	}
	tr := &failingIntakeTracker{
		failProject: testProject.Name,
		ticket:      tracker.Ticket{Ref: "fake#9", Title: "from the second project", Body: "body"},
	}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (one project's intake error must not fail the tick)", err)
	}

	tickets, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets after Tick = %d, want 1 (only the second project's ticket)", len(tickets))
	}
	if tickets[0].TrackerRef != "fake#9" || tickets[0].ProjectID != okProjectID {
		t.Errorf("inserted ticket = %+v, want ref fake#9 under project %d", tickets[0], okProjectID)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "intake error") {
		t.Errorf("log = %q, want it to contain \"intake error\"", logged)
	}
	if !strings.Contains(logged, testProject.Name) {
		t.Errorf("log = %q, want it to name the failing project %q", logged, testProject.Name)
	}
}

// TestTick_MaxParallelCountsOwnInflightOnly proves the max-parallel guard
// (design section 4.1 D1, 4.2 step 5) counts only tickets this process is
// itself running (d.inflight), never a ticket some other owner holds: with
// MaxParallel 1 and one ticket already claimed by a foreign owner, fill
// still has a free slot of its own and claims and launches the other, ready
// ticket. This replaces the pre-#45 TestTick_RespectsMaxParallel, whose own
// assertion (a foreign claim blocks picking) was the old CountActiveRuns
// guard's behavior -- store.CountActiveRuns counted every owner's claims,
// not only this process's own -- and is no longer true now that the guard
// is d.inflight's own length.
func TestTick_MaxParallelCountsOwnInflightOnly(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	foreignID := seedQueuedTicket(t, s, "fake#1")
	readyID := seedQueuedTicket(t, s, "fake#2")

	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), foreignID, "another-worker", expires)
	if err != nil || !claimed {
		t.Fatalf("claim foreignID: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	ready := getTicket(t, s, readyID)
	if ready.State != testStatePlanning {
		t.Errorf("ready ticket state = %q, want planning (the foreign claim on another ticket must not use this process's one slot)", ready.State)
	}

	foreign := getTicket(t, s, foreignID)
	if foreign.ClaimOwner == nil || *foreign.ClaimOwner != "another-worker" {
		t.Errorf("foreign ticket claim owner = %v, want unchanged another-worker", foreign.ClaimOwner)
	}
}

// TestTick_SecondWorkersClaimIsRefused simulates a second worker already
// holding a ticket's claim: the dispatcher must not touch it (design section
// 6.8 step 6, "another worker holds it").
func TestTick_SecondWorkersClaimIsRefused(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, "other-worker", expires)
	if err != nil || !claimed {
		t.Fatalf("pre-claim: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 5, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner == nil || *final.ClaimOwner != "other-worker" {
		t.Errorf("final ticket claim owner = %v, want other-worker (untouched)", final.ClaimOwner)
	}
}

// TestTick_PicksTheFurthestAlongTicketOverQueuedOnesAndExcludesTerminal
// arranges one done ticket and one ticket already at building alongside a
// plain queued ticket, then proves a single tick picks the furthest-along
// non-terminal candidate first and never touches the terminal one (design
// section 6.8 step 5).
func TestTick_PicksTheFurthestAlongTicketOverQueuedOnesAndExcludesTerminal(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)

	doneID := seedQueuedGitBackedTicket(t, s, "fake#1")
	advanceTicket(t, s, rt, doneID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging)
	// shipping's own real handler (M3 tasks 6, 7) takes a PUBLISH tick and a
	// POLL tick, neither of which this test cares about (it only needs a
	// terminal ticket to prove Tick excludes it); seedTicketDirectlyToState
	// mirrors TestTickUsesInjectedClock's own direct-commit seed, skipping
	// straight from judging to shipping to done.
	seedTicketDirectlyToState(t, s, doneID, testStateShipping)
	seedTicketDirectlyToState(t, s, doneID, testStateDone)

	buildingID := seedQueuedGitBackedTicket(t, s, "fake#2")
	advanceTicket(t, s, rt, buildingID, testStateQueued, testStatePlanning)

	queuedID := seedQueuedTicket(t, s, "fake#3")

	// MaxParallel: 1, not 2: with #45's fill filling every free slot, two
	// free slots would launch both the furthest-along ticket and the
	// merely-queued one this same Tick, leaving nothing to prove about
	// pick order (the "lower priority" assertion below needs the queued
	// ticket to still be untouched after this one Tick call).
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, nil, nil, dispatch.Config{
		MaxParallel: 1, Owner: testOwner,
		Sandboxes: sandbox.OffSet(), RequireSandbox: false, Commands: job.NewCommandRunner(sandbox.Off(), false),
		Projects: buildTestProjects(t, s),
	})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// One tick only runs the building handler's own RUN first turn for the
	// furthest-along ticket (task 9: a unit takes a RUN call and a separate
	// CHECK-then-LAND call, never both in one tick), so buildingID stays in
	// "building" -- but its first task's build_report now exists, proving
	// it really was picked and run this tick, unlike the untouched queued
	// ticket below.
	if got := getTicket(t, s, buildingID).State; got != testStateBuilding {
		t.Errorf("building ticket state = %q, want still building (one tick runs one build step)", got)
	}
	reports, err := s.BuildReports(t.Context(), buildingID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	if len(reports) == 0 {
		t.Error("building ticket has no build_report after being picked first, want one from this tick's RUN")
	}
	if got := getTicket(t, s, queuedID).State; got != testStateQueued {
		t.Errorf("queued ticket state = %q, want still queued (lower priority)", got)
	}
	if got := getTicket(t, s, doneID).State; got != testStateDone {
		t.Errorf("done ticket state = %q, want unchanged done (terminal, excluded)", got)
	}
}

// TestTick_NumericExternalIDTieBreak arranges two plain queued tickets, tied
// on pipeline position, whose tracker refs tie-break numerically, then
// proves one tick picks the lower numeric external id (design section 6.2,
// 6.8 step 5): "fake#3" before "fake#30", even though "30" sorts before "3"
// as text. A second tick is not driven here: once picked, "fake#3" moves to
// planning, which itself outranks any queued ticket by pipeline position
// (proved separately), so a further pick is no longer a tie-break case.
func TestTick_NumericExternalIDTieBreak(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	lowID := seedQueuedTicket(t, s, "fake#3")
	highID := seedQueuedTicket(t, s, "fake#30")

	// MaxParallel: 1, not 2: with #45's fill filling every free slot, two
	// free slots would launch both tied tickets this same Tick, leaving
	// nothing to prove about the tie-break itself.
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := getTicket(t, s, lowID).State; got != testStatePlanning {
		t.Errorf("fake#3 state = %q, want planning (picked first)", got)
	}
	if got := getTicket(t, s, highID).State; got != testStateQueued {
		t.Errorf("fake#30 state = %q, want still queued (lost the tie-break)", got)
	}
}

// TestTick_DrainsWithoutStartingWork proves the drain flag (design section
// 6.8 step 2) stops Tick before it picks or claims anything.
func TestTick_DrainsWithoutStartingWork(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want untouched while draining", final)
	}
}

// TestTick_StopsWithoutStartingWork proves the stopped flag (design section
// 6.8 step 2) stops Tick the same way draining does.
func TestTick_StopsWithoutStartingWork(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	if err := s.SetStopped(t.Context(), true); err != nil {
		t.Fatalf("SetStopped: %v", err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want untouched while stopped", final)
	}
}

// TestTick_ClaimUsesTheJobTimeoutAndRunsUnderThatDeadlineNotTheClaimGrace
// proves the run context's deadline is now+timeout, not the later
// now+timeout+5m claim expiry the lease carries as checkpoint grace (design
// section 6.8 step 6, 7). planning's machine.toml timeout_minutes is 60.
func TestTick_ClaimUsesTheJobTimeoutAndRunsUnderThatDeadlineNotTheClaimGrace(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	spy := &spyHandler{next: testStateBuilding, reason: testReasonPlanReady}
	reg := job.Registry()
	reg[testStatePlanning] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if !spy.HasDeadline() {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(59 * time.Minute)
	wantMax := after.Add(61 * time.Minute)
	if spy.Deadline().Before(wantMin) || spy.Deadline().After(wantMax) {
		t.Errorf("run deadline = %v, want within [%v, %v] (~60m, the planning job timeout, not +65m)", spy.Deadline(), wantMin, wantMax)
	}

	claimGraceMin := before.Add(64 * time.Minute)
	claimGraceMax := after.Add(66 * time.Minute)
	if spy.Expires().Before(claimGraceMin) || spy.Expires().After(claimGraceMax) {
		t.Errorf("claim expiry (Deps.Expires) = %v, want within [%v, %v] (~65m: 60m timeout + 5m grace)", spy.Expires(), claimGraceMin, claimGraceMax)
	}
}

// TestClaimTimeoutForReviewingCoversBuild proves the "reviewing" state's
// claim/run deadline is the largest of the review, build, and perimeter
// job timeouts (#55 plan D9): a review fix unit runs a 45-minute build run
// and a 45-minute CHECK inside "reviewing", so with build at 45 and review
// at 30 the deadline is 45m, not review's own 30m.
func TestClaimTimeoutForReviewingCoversBuild(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	// Jump the ticket straight to reviewing: this test only cares about
	// which job timeout the dispatcher looks up for that state, not how a
	// ticket really gets there, so it skips driving the real pipeline
	// (dispatch_test.go's own seed-direct shortcut, matching
	// postbuild_test.go's pbSeedTicketInState, package job).
	seedOwner := "seed-reviewing-owner"
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateReviewing, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateJudging, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateReviewing] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if !spy.HasDeadline() {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(44 * time.Minute)
	wantMax := after.Add(46 * time.Minute)
	if spy.Deadline().Before(wantMin) || spy.Deadline().After(wantMax) {
		t.Errorf("run deadline = %v, want within [%v, %v] (~45m, jobs.build.timeout_minutes, not review's 30m)", spy.Deadline(), wantMin, wantMax)
	}
}

// TestTickUsesInjectedClock proves step 5's own "now" (PKG9-PLAN.md section
// 17.1) comes from cfg.Now, not a bare time.Now() Tick reads itself: a
// ticket whose next_poll_at sits two hours past real wall-clock time is
// skipped by a Tick with no injected clock (cfg.Now defaults to time.Now in
// dispatch.New), but picked by a Tick whose cfg.Now reports a time already
// past that poll schedule -- the fake clock selftest's own 17.1 wiring
// needs to drive a babysit poll's backoff without a real wait.
func TestTickUsesInjectedClock(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	seedOwner := "seed-shipping-owner"
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateShipping, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	farFuture := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	pollOwner := "seed-poll-owner"
	pollExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err = s.Claim(t.Context(), ticketID, pollOwner, pollExpires)
	if err != nil || !claimed {
		t.Fatalf("seed poll claim: claimed=%v err=%v", claimed, err)
	}
	applied, err = s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: pollOwner, Expires: pollExpires,
		Poll: &store.PollUpdate{NextAt: farFuture, IntervalS: 300, Fingerprint: strings.Repeat("a", 64)},
	})
	if err != nil || !applied {
		t.Fatalf("seed poll commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateDone, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateShipping] = spy

	// The real clock: the poll is not due for another two hours, so this
	// tick must not pick the ticket.
	dReal := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := dReal.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (real clock): %v", err)
	}
	if spy.Calls() != 0 {
		t.Fatalf("spy.Calls() = %d after a real-clock tick, want 0 (the poll is not due yet)", spy.Calls())
	}

	// An injected clock past the poll time: this tick must pick it up.
	injected := farFuture.Add(time.Minute)
	dFake := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{
		MaxParallel: 2, Owner: testOwner, Now: func() time.Time { return injected },
	})
	if err := dFake.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (injected clock): %v", err)
	}
	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d after an injected-clock tick past the poll time, want 1", spy.Calls())
	}
}

// TestClaimTimeoutForQueuedCoversClassifyRetry proves claimTimeoutFor's own
// "queued" row (PKG9-PLAN.md section 17.1, owner decision Q4): the real
// machine.toml gives jobs.classify a 5-minute timeout_minutes and a
// timeout_retries of 1, so max(defaultCodeTimeout, (1+1)*5) is 10 minutes,
// covering both of classify's attempts under one lease.
func TestClaimTimeoutForQueuedCoversClassifyRetry(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	seedQueuedTicket(t, s, testFixtureRef)

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if !spy.HasDeadline() {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(9 * time.Minute)
	wantMax := after.Add(11 * time.Minute)
	if spy.Deadline().Before(wantMin) || spy.Deadline().After(wantMax) {
		t.Errorf("run deadline = %v, want within [%v, %v] (~10m, max(defaultCodeTimeout, (1+timeout_retries)*classify timeout))", spy.Deadline(), wantMin, wantMax)
	}
}

// TestClaimTimeoutForJudging proves claimTimeoutFor's own "judging" row
// (PKG9-PLAN.md section 17.1): the real machine.toml gives jobs.judge and
// jobs.build both 45 minutes and jobs.perimeter 3, so
// max(judge, build, perimeter, 10) is 45 -- the build job's own timeout,
// not judge's alone, so this also proves the row reads every one of the
// three jobs rather than just "judge".
func TestClaimTimeoutForJudging(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	seedOwner := testSeedJudgingOwner
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateJudging, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateShipping, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateJudging] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if !spy.HasDeadline() {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(44 * time.Minute)
	wantMax := after.Add(46 * time.Minute)
	if spy.Deadline().Before(wantMin) || spy.Deadline().After(wantMax) {
		t.Errorf("run deadline = %v, want within [%v, %v] (~45m, max(judge, build, perimeter, 10))", spy.Deadline(), wantMin, wantMax)
	}
}

// TestClaimTimeoutForShipping proves claimTimeoutFor's own "shipping" row
// (PKG9-PLAN.md section 17.1): the real machine.toml gives jobs.build 45
// minutes, jobs.perimeter 3, and jobs.respond 15, so
// max(respond, build, perimeter) is 45 -- the build job's own timeout, not
// respond's alone, so this also proves the row reads every one of the
// three jobs rather than just "respond".
func TestClaimTimeoutForShipping(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	seedOwner := "seed-shipping-timeout-owner"
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateShipping, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateDone, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateShipping] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if !spy.HasDeadline() {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(44 * time.Minute)
	wantMax := after.Add(46 * time.Minute)
	if spy.Deadline().Before(wantMin) || spy.Deadline().After(wantMax) {
		t.Errorf("run deadline = %v, want within [%v, %v] (~45m, max(respond, build, perimeter))", spy.Deadline(), wantMin, wantMax)
	}
}

// TestRunAndCommitCopiesJudgeCodexHome proves runAndCommit copies
// dispatch.Config.JudgeCodexHome into every job.Deps it builds
// (PKG9-PLAN.md section 4.3, 7.3, D27), the same way it already threads
// DataDir and LensesParallel.
func TestRunAndCommitCopiesJudgeCodexHome(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	seedOwner := testSeedJudgingOwner
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateJudging, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateShipping, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateJudging] = spy

	const wantJudgeCodexHome = "/test/judge/codex/home"
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 2, Owner: testOwner, JudgeCodexHome: wantJudgeCodexHome})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if spy.JudgeCodexHome() != wantJudgeCodexHome {
		t.Errorf("Deps.JudgeCodexHome = %q, want %q", spy.JudgeCodexHome(), wantJudgeCodexHome)
	}
}

// TestRunAndCommitCopiesReviewBots proves runAndCommit copies
// dispatch.Config.ReviewBots into every job.Deps it builds, the same way
// TestRunAndCommitCopiesJudgeCodexHome already proves for JudgeCodexHome:
// without this copy, cmd/zing/serve.go could build an empty
// job.ReviewBotRule, or drop the field from the dispatch.Config literal
// entirely, and every pollIdle-level test would still pass (they all set
// deps.ReviewBots directly) while production never nudges a silent review
// bot.
func TestRunAndCommitCopiesReviewBots(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	seedOwner := testSeedJudgingOwner
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateJudging, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateShipping, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateJudging] = spy

	wantReviewBots := job.ReviewBotRule{
		Wait:   20 * time.Minute,
		Checks: []job.ReviewBotCheck{{Check: "CodeRabbit", Trigger: "@coderabbitai review"}},
	}
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 2, Owner: testOwner, ReviewBots: wantReviewBots})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	got := spy.ReviewBots()
	if got.Wait != wantReviewBots.Wait || !slices.Equal(got.Checks, wantReviewBots.Checks) {
		t.Errorf("Deps.ReviewBots = %+v, want %+v", got, wantReviewBots)
	}
}

// TestTick_HandlerDeadlineSurvivesSlowIntakeNotEatenByIt proves the run
// deadline is computed from a fresh time.Now() taken right before running
// the handler (after the claim), not the tick-start now (design section
// 6.8 step 6, fix 9): a slow intake step, which runs earlier in the same
// Tick, must not eat into the handler's own timeout budget. planning's
// machine.toml timeout_minutes is 60.
func TestTick_HandlerDeadlineSurvivesSlowIntakeNotEatenByIt(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	projectID := seedProject(t, s)
	const intakeDelay = 300 * time.Millisecond
	slow := &slowTracker{Tracker: newFixtureTracker(t), delay: intakeDelay}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name}}

	spy := &spyHandler{next: testStateBuilding, reason: testReasonPlanReady}
	reg := job.Registry()
	reg[testStatePlanning] = spy

	d := newDispatcher(t, s, slow, bus.New(), rt, reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	tickStart := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterTick := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if !spy.HasDeadline() {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}

	// Were the deadline still computed from the tick-start now (the bug),
	// it would sit at roughly tickStart+60m regardless of the slow intake.
	// The fix takes a fresh time.Now() after the claim, so the deadline must
	// land at least intakeDelay later than that, with a safety margin well
	// under intakeDelay so this assertion cannot pass by coincidence.
	const margin = 100 * time.Millisecond
	minDeadline := tickStart.Add(60*time.Minute + intakeDelay - margin)
	if spy.Deadline().Before(minDeadline) {
		t.Errorf("handler deadline = %v, want at least %v (computed after the %v slow intake, not at tick start)",
			spy.Deadline(), minDeadline, intakeDelay)
	}
	maxDeadline := afterTick.Add(61 * time.Minute)
	if spy.Deadline().After(maxDeadline) {
		t.Errorf("handler deadline = %v, want at most %v", spy.Deadline(), maxDeadline)
	}
}

// TestTick_ClaimExpirySurvivesSlowIntakeNotEatenByIt proves the claim expiry
// (Deps.Expires, the same value that fences the eventual commit) is computed
// from a fresh time.Now() taken after reconcile, intake, and count have
// already run (design section "dispatch" fix 5, cubic P2), not the
// tick-start now: a slow intake step must not shrink the lease's actual
// coverage, measured from the moment the ticket is really claimed, below
// timeout + claimGrace. planning's machine.toml timeout_minutes is 60;
// claimGrace is 5m, so the claim window is ~65m.
func TestTick_ClaimExpirySurvivesSlowIntakeNotEatenByIt(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	projectID := seedProject(t, s)
	const intakeDelay = 300 * time.Millisecond
	slow := &slowTracker{Tracker: newFixtureTracker(t), delay: intakeDelay}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name}}

	spy := &spyHandler{next: testStateBuilding, reason: testReasonPlanReady}
	reg := job.Registry()
	reg[testStatePlanning] = spy

	d := newDispatcher(t, s, slow, bus.New(), rt, reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	tickStart := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterTick := time.Now()

	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}

	// Were expires still computed from the tick-start now (the bug), it
	// would sit at roughly tickStart+65m regardless of the slow intake. The
	// fix takes a fresh time.Now() after intake, so expires must land at
	// least intakeDelay later than that, with a safety margin well under
	// intakeDelay so this assertion cannot pass by coincidence.
	const margin = 100 * time.Millisecond
	minExpires := tickStart.Add(65*time.Minute + intakeDelay - margin)
	if spy.Expires().Before(minExpires) {
		t.Errorf("claim expiry = %v, want at least %v (computed after the %v slow intake, not at tick start)",
			spy.Expires(), minExpires, intakeDelay)
	}
	maxExpires := afterTick.Add(66 * time.Minute)
	if spy.Expires().After(maxExpires) {
		t.Errorf("claim expiry = %v, want at most %v", spy.Expires(), maxExpires)
	}
}

// TestTick_HandlerErrorBeforeAStateChangeReleasesClaimAndLeavesState proves
// a handler error is recoverable: the dispatcher logs it, releases the
// claim with a fenced no-op commit, and leaves the ticket's state and wait
// untouched for a later retry (design section 6.8 step 7).
func TestTick_HandlerErrorBeforeAStateChangeReleasesClaimAndLeavesState(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = &spyHandler{err: errors.New("boom: handler blew up before any state change")}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a handler error is recoverable, not fail-closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.WaitingOn != nil {
		t.Errorf("final ticket waiting_on = %v, want nil", *final.WaitingOn)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}
}

// TestTick_StaleOwnerCommitFailsClosedWithNoRedrive proves the Q-runtime
// fail-closed rule (design section 6.8 step 7, section 13): when the fence
// CommitHandlerResult checks no longer matches (simulated here the way a
// concurrent reconcile stealing the lease mid-run would look), the
// dispatcher logs, stops the process (store.SetStopped), and returns the
// condition from Tick, having called the runtime exactly once. A second
// Tick call, now that the store is stopped, returns cleanly and never
// re-drives the runtime.
func TestTick_StaleOwnerCommitFailsClosedWithNoRedrive(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	counting := &countingRuntime{rt: rt}
	reg := job.Registry()
	reg[testStatePlanning] = &staleOwnerHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), counting, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	err := d.Tick(t.Context())
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Fatalf("first Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Fatalf("runtime calls after the fail-closed tick = %d, want exactly 1", got)
	}

	_, stopped, flagsErr := s.Flags(t.Context())
	if flagsErr != nil {
		t.Fatalf("Flags: %v", flagsErr)
	}
	if !stopped {
		t.Error("stopped flag = false, want true after fail-closed")
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Errorf("second Tick (stopped): err = %v, want nil", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Errorf("runtime calls after the second tick = %d, want still 1 (no re-drive)", got)
	}
}

// TestTick_ReleaseClaimFailsClosedWhenLeaseAlreadyLost proves the release
// path gets the same fail-closed treatment as the post-run commit path
// (design section 6.8 step 7, fix 8): a handler that drives the runtime once
// and then fails after a concurrent reconcile has already stolen its lease
// leaves releaseClaim's own fenced no-op commit unable to apply (applied =
// false, the lease already gone), and the dispatcher must stop the process
// and report ErrFailClosed rather than silently continuing as if the claim
// had been cleanly released, since the handler may already have advanced
// the runtime.
func TestTick_ReleaseClaimFailsClosedWhenLeaseAlreadyLost(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	counting := &countingRuntime{rt: rt}
	reg := job.Registry()
	reg[testStatePlanning] = &staleOwnerReleaseHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), counting, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	err := d.Tick(t.Context())
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Fatalf("Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Fatalf("runtime calls after the fail-closed tick = %d, want exactly 1", got)
	}

	_, stopped, flagsErr := s.Flags(t.Context())
	if flagsErr != nil {
		t.Fatalf("Flags: %v", flagsErr)
	}
	if !stopped {
		t.Error("stopped flag = false, want true after fail-closed on the release path")
	}
}

// --- post-handler writes survive a cancelled tick context (fix 4) --------

// TestTick_PostHandlerCommitSurvivesCancelledTickContext proves the
// post-handler commit runs under a detached, bounded context, not ctx
// itself (design section "dispatch" fix 4): a handler that cancels the tick
// context it was handed before returning its commit must still see that
// commit land, since a cancelled handler context (or the drain sequence's
// own force-cancel racing the same moment) must not be able to abort
// recording what the runtime already did.
// TestTick_HandlerCommitDiscardedAfterCancelledTickContext replaces the
// pre-#45 TestTick_PostHandlerCommitSurvivesCancelledTickContext, whose own
// name described the opposite of what #45 section 7.2 now requires: once
// ctx (the Dispatcher's own long-lived context, standing in here for
// serve's dispCtx) is cancelled by the time handler.Run returns, the run
// counts as a shutdown interrupt regardless of what the handler returned --
// a valid commit included -- so the commit here is discarded rather than
// applied, and the ticket's claim is cleared through InterruptRuns instead
// of CommitHandlerResult. The next tick's session resume (a later
// milestone) is what actually finishes the turn.
func TestTick_HandlerCommitDiscardedAfterCancelledTickContext(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	reg := job.Registry()
	reg[testStatePlanning] = &cancelingHandler{cancel: cancel, next: testStateDone, reason: testReasonPlanReady}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v, want nil (a shutdown interrupt is not a dispatcher error)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want unchanged planning (the commit must be discarded, not applied, once ctx is cancelled)", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (InterruptRuns clears it)", *final.ClaimOwner)
	}
}

// TestTick_ReleaseClaimSurvivesCancelledTickContext proves the release
// path's fenced no-op commit gets the same detached, bounded context (design
// section "dispatch" fix 4): a handler that cancels the tick context before
// returning a plain error must still see its claim released, rather than the
// release write itself failing because ctx was already cancelled.
func TestTick_ReleaseClaimSurvivesCancelledTickContext(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	reg := job.Registry()
	reg[testStateQueued] = &cancelingHandler{cancel: cancel, err: errors.New("boom: handler blew up after cancelling ctx")}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v, want nil (the claim release must still land despite the cancelled tick context)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released despite the cancelled tick context)", *final.ClaimOwner)
	}
}

// --- the minimal error path (design section 6.7) --------------------------

// errorScriptXML is a minimal, valid RunError document for the classify
// job: a universal error outcome with a code from the closed ErrorCode set.
// It is wired into an inline fstest.MapFS fake runtime, never added to the
// real fixtures/scripts tree, because the plan says the skeleton's real
// scripts never error (design section 6.7, section 12 task 8). classify,
// not planning, is the job this test's freshly-queued ticket actually runs
// first (design section 5.1 step 2: a nil Kind classifies before planning
// ever opens a session).
const errorScriptXML = `<zing job="classify" outcome="error">
  <error code="cannot_run">
    <what>The classify job's environment cannot run.</what>
    <why>The sandbox has no network access to reach the model.</why>
    <tried>Retried once; same failure.</tried>
  </error>
</zing>
`

// TestTick_ErrorOutcomeEscalates drives a ticket already claimed into
// planning against a fake runtime whose one scripted classify turn returns
// the universal error outcome, and proves the dispatcher applies the
// section 6.7 error-branch commit end to end: the ticket stays in its
// state, waiting on "error", with one escalation message authored "zing"
// whose EscalationPayload.Code is the script's RunError.Code (one of the
// four ErrorCode values) and whose Options are the fixed local
// retry/planning/abandon set.
func TestTick_ErrorOutcomeEscalates(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued) // queued -> planning, no session opened yet

	errFS := fstest.MapFS{"classify/1.xml": &fstest.MapFile{Data: []byte(errorScriptXML)}}
	errRT := runtime.NewFake(errFS)

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), errRT, nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want unchanged planning (no Next on the error branch)", final.State)
	}
	// Section 6.7's Write rule sets waiting_on to "questions" for every
	// escalation, cap or run-caused alike (the linked question offers
	// retry/planning/abandon): this superseded the old skeleton's "error"
	// flag once planning.go (task 6) became the real escalation writer.
	if final.WaitingOn == nil || *final.WaitingOn != testWaitingQuestions {
		t.Errorf("final ticket waiting_on = %v, want questions", final.WaitingOn)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (cleared by the commit)", *final.ClaimOwner)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var escalation *store.MessageRow
	for i := range msgs {
		if msgs[i].Type == testMsgTypeEscalation {
			escalation = &msgs[i]
		}
	}
	if escalation == nil {
		t.Fatal("no escalation message persisted")
	}
	if escalation.Author != "zing" {
		t.Errorf("escalation message author = %q, want zing", escalation.Author)
	}

	var payload response.EscalationPayload
	if err := json.Unmarshal(escalation.Payload, &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	validCodes := map[string]bool{
		string(response.ErrorCodePlanGap): true, string(response.ErrorCodeCannotRun): true,
		string(response.ErrorCodeEnvironment): true, string(response.ErrorCodeOther): true,
	}
	if !validCodes[payload.Code] {
		t.Errorf("escalation payload.Code = %q, want one of the four RunError.Code values", payload.Code)
	}
	if payload.Code != string(response.ErrorCodeCannotRun) {
		t.Errorf("escalation payload.Code = %q, want %q (the script's RunError.Code)", payload.Code, response.ErrorCodeCannotRun)
	}
	wantOptions := []string{"retry", "planning", "abandon"}
	if !slices.Equal(payload.Options, wantOptions) {
		t.Errorf("escalation payload.Options = %v, want %v", payload.Options, wantOptions)
	}
}

// --- Run -----------------------------------------------------------------

// TestRun_ReturnsWhenContextIsCancelled proves Run ticks on cfg.Interval and
// exits once ctx is done (design section 6.8).
func TestRun_ReturnsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, nil, bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{Interval: 5 * time.Millisecond, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := d.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run() = %v, want context.DeadlineExceeded", err)
	}
}

// TestRun_ReturnsAfterTheCurrentTickWhenDraining proves Run stops as soon as
// the drain flag is set, after its current tick finishes (design section
// 6.8).
func TestRun_ReturnsAfterTheCurrentTickWhenDraining(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	d := newDispatcher(t, s, nil, bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{Interval: 5 * time.Millisecond, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	if err := waitFor(t, done, "Run to return after the drain flag was set"); err != nil {
		t.Errorf("Run() = %v, want nil on drain", err)
	}
}

// TestRun_NotifyDrainReturnsPromptly proves NotifyDrain wakes Run well under
// a tick interval (design section 6.8, fix 5), rather than leaving it to
// notice draining only on the next ticker fire: with a long Interval, Run
// still returns almost immediately once SetDraining and NotifyDrain are
// called, because drainCh is buffered and Run's select observes it directly
// rather than waiting on the timer.
func TestRun_NotifyDrainReturnsPromptly(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, nil, bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{Interval: time.Hour, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	d.NotifyDrain()

	if err := waitFor(t, done, "Run to return promptly after NotifyDrain (want well under the 1h tick interval)"); err != nil {
		t.Errorf("Run() = %v, want nil on drain", err)
	}
}

// --- pickup comment wiring (plan section 6, 8) ------------------------------

// newTwoTicketFixture returns a *commentingFixture over an in-memory
// fixture with two tickets ("fake#1", "fake#2"), since the checked-in
// fixtures/tickets.toml carries only one and these tests need two new
// tickets in one intake.
func newTwoTicketFixture(t *testing.T) *commentingFixture {
	t.Helper()
	fsys := fstest.MapFS{"tickets.toml": &fstest.MapFile{Data: []byte(`project = "zing"

[[ticket]]
ref = "fake#1"
title = "one"
body = "body one"

[[ticket]]
ref = "fake#2"
title = "two"
body = "body two"
`)}}
	fx, err := tracker.NewFixture(fsys, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}
	return &commentingFixture{Fixture: fx}
}

// TestTick_IntakePostsPickupCommentForEachNewTicket proves intake posts the
// pickup comment for each newly inserted ticket, naming the binding's User
// (plan section 6, 8): one tick over two new tickets records exactly two
// pickup comments, and a second tick over the same, now-deduped, tickets
// leaves the recorded count at two (the earlier two remain; no new ones).
func TestTick_IntakePostsPickupCommentForEachNewTicket(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	rec := newTwoTicketFixture(t)
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 {
		t.Fatalf("after first Tick: %d pickup comments, want 2", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("after first Tick: %d Comment attempts, want 2", attempts)
	}
	want := tracker.PickupComment(testBindingUser)
	wantRefs := map[string]bool{"fake#1": true, "fake#2": true}
	gotRefs := make(map[string]bool, len(got))
	for _, c := range got {
		gotRefs[c.ref] = true
		if c.body != want {
			t.Errorf("pickup comment body = %q, want %q", c.body, want)
		}
		if c.project != testProject.Name {
			t.Errorf("pickup comment project = %q, want %q", c.project, testProject.Name)
		}
	}
	// Each of the two new tickets' refs must have received its own comment,
	// not both landing on the same ref (which the earlier count-only
	// assertion could not have caught).
	if len(gotRefs) != len(wantRefs) {
		t.Errorf("pickup comments covered refs %v, want exactly %v (one comment per distinct new ref)", gotRefs, wantRefs)
	}
	for ref := range wantRefs {
		if !gotRefs[ref] {
			t.Errorf("pickup comments never covered ref %q, want one for every new ticket", ref)
		}
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got = rec.recorded(); len(got) != 2 {
		t.Errorf("after second Tick (dedup): %d pickup comments, want still 2 (no new posts)", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("after second Tick (dedup): %d Comment attempts, want still 2 (no new attempts)", attempts)
	}
}

// TestTick_IntakePickupCommentFailureIsBestEffort proves a Comment failure
// is best-effort (plan section 6): Tick still returns nil, both ticket rows
// remain inserted, the later ticket's comment is still attempted, and a
// later tick over the same, now-deduped, tickets adds no further calls (no
// retry, no double-post).
func TestTick_IntakePickupCommentFailureIsBestEffort(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	rec := newTwoTicketFixture(t)
	rec.failFirst = true
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a pickup comment failure is best-effort)", err)
	}

	tickets, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("tickets inserted = %d, want 2 (both rows remain despite the comment failure)", len(tickets))
	}

	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded pickup comments = %d, want 1 (the first call failed, the second still attempted and recorded)", len(got))
	}
	if got[0].ref != "fake#2" {
		t.Errorf("recorded comment ref = %q, want fake#2 (the later ticket's comment, since the first failed)", got[0].ref)
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("Comment attempts = %d, want 2 (one failed attempt for fake#1, one successful for fake#2)", attempts)
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got = rec.recorded(); len(got) != 1 {
		t.Errorf("recorded pickup comments after second Tick = %d, want still 1 (no retry, no double-post)", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("Comment attempts after second Tick = %d, want still 2 (no retry attempt)", attempts)
	}
}

// --- test doubles ----------------------------------------------------------

// commentingFixture is a recording Tracker test double for the pickup
// comment wiring (plan section 6, 8): it embeds a *tracker.Fixture for
// Intake, Fetch, FileTicket, and Collaborators, and overrides Comment to
// record every call under a mutex. failFirst, when set, makes exactly the
// first Comment call return an error and every later call succeed and
// record normally, so a test can exercise the best-effort failure contract
// without losing coverage of the ticket after it.
type commentingFixture struct {
	*tracker.Fixture

	mu        sync.Mutex
	comments  []recordedComment
	attempts  int
	failFirst bool
	failed    bool
}

var _ tracker.Tracker = (*commentingFixture)(nil)

// recordedComment is one recorded commentingFixture.Comment call.
type recordedComment struct {
	project, ref, body string
}

func (c *commentingFixture) Comment(_ context.Context, project, ref, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.failFirst && !c.failed {
		c.failed = true
		return errors.New("boom: pickup comment failed")
	}
	c.comments = append(c.comments, recordedComment{project: project, ref: ref, body: body})
	return nil
}

// recorded returns a fresh copy of every Comment call recorded so far.
func (c *commentingFixture) recorded() []recordedComment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedComment(nil), c.comments...)
}

// attemptCount returns the total number of Comment calls made so far,
// successful or not, so a test can assert the exact attempt count alongside
// the (possibly smaller) number that were actually recorded.
func (c *commentingFixture) attemptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

// --- PostPRLink / PostDone (design section 8.2 step 5, 8.6 step 1, 11, 10.5) ---

// markedComment is one comment shipTrackerDouble tracks under a ref: author
// is who it was "posted" by, so a test can seed a comment under any author
// and prove CommentContains only ever counts the double's own login.
type markedComment struct {
	author, body string
}

// shipTrackerDouble is a Tracker test double for PostPRLink and PostDone:
// it embeds a *tracker.Fixture for every method they do not touch, and
// implements Comment, CommentContains, and Close itself so a test can seed
// a comment under an arbitrary author (seedMarked), inject an error from
// any of the three calls, and read back exactly what was posted, closed,
// and in what order.
type shipTrackerDouble struct {
	*tracker.Fixture

	mu            sync.Mutex
	ownLogin      string
	marked        map[string][]markedComment // ref -> comments, in arrival order
	posted        []recordedComment          // every successful Comment call
	closed        []string                   // every successful Close call's ref
	sequence      []string                   // "comment:<ref>" then "close:<ref>", call order
	closeAttempts int                        // every Close call, successful or not

	failComment, failContains, failClose error
}

func newShipTrackerDouble(t *testing.T, login string) *shipTrackerDouble {
	t.Helper()
	return &shipTrackerDouble{Fixture: newFixtureTracker(t), ownLogin: login, marked: map[string][]markedComment{}}
}

// seedMarked records a comment under testFixtureRef, the one ref every
// PostPRLink/PostDone test here uses, as if it were already posted before
// the call runs: author == the double's own login simulates a real earlier
// post surviving a crash; any other author simulates a spoofed marker
// CommentContains must ignore.
func (s *shipTrackerDouble) seedMarked(author, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked[testFixtureRef] = append(s.marked[testFixtureRef], markedComment{author: author, body: body})
}

func (s *shipTrackerDouble) Comment(_ context.Context, project, ref, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failComment != nil {
		return s.failComment
	}
	s.posted = append(s.posted, recordedComment{project: project, ref: ref, body: body})
	s.marked[ref] = append(s.marked[ref], markedComment{author: s.ownLogin, body: body})
	s.sequence = append(s.sequence, "comment:"+ref)
	return nil
}

func (s *shipTrackerDouble) CommentContains(_ context.Context, _, ref, needle string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failContains != nil {
		return false, s.failContains
	}
	for _, c := range s.marked[ref] {
		if c.author == s.ownLogin && strings.Contains(c.body, needle) {
			return true, nil
		}
	}
	return false, nil
}

func (s *shipTrackerDouble) Close(_ context.Context, _, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAttempts++
	if s.failClose != nil {
		return s.failClose
	}
	s.closed = append(s.closed, ref)
	s.sequence = append(s.sequence, "close:"+ref)
	return nil
}

func (s *shipTrackerDouble) postedComments() []recordedComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedComment(nil), s.posted...)
}

func (s *shipTrackerDouble) callSequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sequence...)
}

// closeAttemptCount is every Close call this double received, whether or
// not failClose made it fail, so a test can prove a failing Close was
// actually attempted and not merely absent from callSequence.
func (s *shipTrackerDouble) closeAttemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeAttempts
}

var _ tracker.Tracker = (*shipTrackerDouble)(nil)

// shipTestFixture builds a *store.Store, one ticket under testFixtureRef
// (state is irrelevant to PostPRLink/PostDone, which never read it), and a
// *shipTrackerDouble, all under one project id. It returns the project id,
// the ticket's own store id (the ticket id the marker is scoped to), and
// the double; each test builds its own binding.
func shipTestFixture(t *testing.T) (s *store.Store, projectID, ticketID int64, tr *shipTrackerDouble) {
	t.Helper()
	s = newDispatchTestStore(t)
	projectID = seedProject(t, s)
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	tr = newShipTrackerDouble(t, "zing-bot")
	return s, projectID, ticketID, tr
}

const testPRURL = "https://github.com/o/r/pull/1"

// TestPostPRLinkSkipsMarkedComment proves PostPRLink skips posting once its
// own marker is already on the issue, including when CommentContains found
// it on a later page (design section 8.2 step 5, 11): the real tracker's
// own pagination is proved by tracker.TestCommentContainsPagesAll;
// shipTrackerDouble stands in for "found it somewhere" here.
func TestPostPRLinkSkipsMarkedComment(t *testing.T) {
	t.Parallel()

	s, projectID, ticketID, tr := shipTestFixture(t)
	marker := fmt.Sprintf("<!-- zing:pr t%d -->", ticketID)
	tr.seedMarked(tr.ownLogin, "an earlier post\n\n"+marker)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostPRLink(t.Context(), projectID, testFixtureRef, testPRURL); err != nil {
		t.Fatalf("PostPRLink: %v", err)
	}
	if got := tr.postedComments(); len(got) != 0 {
		t.Errorf("posted comments = %+v, want none (the marker was already there)", got)
	}
}

// TestPostPRLinkIgnoresSpoofedMarker proves a marker from any login but the
// tracker's own never suppresses the real post (design section 10.5).
func TestPostPRLinkIgnoresSpoofedMarker(t *testing.T) {
	t.Parallel()

	s, projectID, ticketID, tr := shipTestFixture(t)
	marker := fmt.Sprintf("<!-- zing:pr t%d -->", ticketID)
	tr.seedMarked("impostor", marker)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostPRLink(t.Context(), projectID, testFixtureRef, testPRURL); err != nil {
		t.Fatalf("PostPRLink: %v", err)
	}
	got := tr.postedComments()
	if len(got) != 1 {
		t.Fatalf("posted comments = %+v, want 1 (a spoofed marker must not suppress the real post)", got)
	}
	if !strings.Contains(got[0].body, marker) {
		t.Errorf("posted comment body = %q, want it to carry %q", got[0].body, marker)
	}
}

// TestPostPRLinkErrorReturned proves a Comment failure propagates (design
// section 8.2 step 4, 11): the next tick must see the error and retry.
func TestPostPRLinkErrorReturned(t *testing.T) {
	t.Parallel()

	s, projectID, _, tr := shipTestFixture(t)
	tr.failComment = errors.New("boom: comment failed")

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostPRLink(t.Context(), projectID, testFixtureRef, testPRURL); err == nil {
		t.Error("PostPRLink err = nil, want an error")
	}
}

// TestPostDoneSkipsMarkedComment proves PostDone skips the comment once its
// own marker is already posted, but still calls Close every time (design
// section 8.6 step 1, 11): a crash between the post and the close must
// still converge on the next tick.
func TestPostDoneSkipsMarkedComment(t *testing.T) {
	t.Parallel()

	s, projectID, ticketID, tr := shipTestFixture(t)
	marker := fmt.Sprintf("<!-- zing:done t%d -->", ticketID)
	tr.seedMarked(tr.ownLogin, "an earlier post\n\n"+marker)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostDone(t.Context(), projectID, testFixtureRef, testPRURL, ""); err != nil {
		t.Fatalf("PostDone: %v", err)
	}
	if got := tr.postedComments(); len(got) != 0 {
		t.Errorf("posted comments = %+v, want none (the marker was already there)", got)
	}
	want := []string{"close:" + testFixtureRef}
	if got := tr.callSequence(); !slices.Equal(got, want) {
		t.Errorf("call sequence = %v, want %v (Close still runs when the comment is skipped)", got, want)
	}
}

// TestPostDoneIgnoresSpoofedMarker is PostPRLink's spoofed-marker proof,
// for PostDone (design section 10.5).
func TestPostDoneIgnoresSpoofedMarker(t *testing.T) {
	t.Parallel()

	s, projectID, ticketID, tr := shipTestFixture(t)
	marker := fmt.Sprintf("<!-- zing:done t%d -->", ticketID)
	tr.seedMarked("impostor", marker)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostDone(t.Context(), projectID, testFixtureRef, testPRURL, ""); err != nil {
		t.Fatalf("PostDone: %v", err)
	}
	got := tr.postedComments()
	if len(got) != 1 {
		t.Fatalf("posted comments = %+v, want 1 (a spoofed marker must not suppress the real post)", got)
	}
	if !strings.Contains(got[0].body, marker) {
		t.Errorf("posted comment body = %q, want it to carry %q", got[0].body, marker)
	}
}

// TestPostDoneClosesAfterComment proves PostDone posts the done comment
// before it closes the issue (design section 8.6 step 1).
func TestPostDoneClosesAfterComment(t *testing.T) {
	t.Parallel()

	s, projectID, _, tr := shipTestFixture(t)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostDone(t.Context(), projectID, testFixtureRef, testPRURL, ""); err != nil {
		t.Fatalf("PostDone: %v", err)
	}
	want := []string{"comment:" + testFixtureRef, "close:" + testFixtureRef}
	if got := tr.callSequence(); !slices.Equal(got, want) {
		t.Errorf("call sequence = %v, want %v", got, want)
	}
}

// TestPostDonePassesMergeSHA proves PostDone forwards its mergeSHA argument
// into tracker.DoneComment, so the posted comment names the merge commit
// instead of saying the pull request is ready for review.
func TestPostDonePassesMergeSHA(t *testing.T) {
	t.Parallel()

	const mergeSHA = "0123456789abcdef0123456789abcdef01234567"
	s, projectID, _, tr := shipTestFixture(t)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostDone(t.Context(), projectID, testFixtureRef, testPRURL, mergeSHA); err != nil {
		t.Fatalf("PostDone: %v", err)
	}
	got := tr.postedComments()
	if len(got) != 1 {
		t.Fatalf("posted comments = %+v, want 1", got)
	}
	if !strings.Contains(got[0].body, "was merged") || !strings.Contains(got[0].body, mergeSHA) {
		t.Errorf("posted comment body = %q, want it to say \"was merged\" and name %q", got[0].body, mergeSHA)
	}
	want := []string{"comment:" + testFixtureRef, "close:" + testFixtureRef}
	if gotSeq := tr.callSequence(); !slices.Equal(gotSeq, want) {
		t.Errorf("call sequence = %v, want %v", gotSeq, want)
	}
}

// TestPostDoneErrorReturned proves a Close failure propagates (design
// section 11: "the next tick sees it merged and runs DONE" only holds once
// Close actually succeeds).
func TestPostDoneErrorReturned(t *testing.T) {
	t.Parallel()

	s, projectID, _, tr := shipTestFixture(t)
	tr.failClose = errors.New("boom: close failed")

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}
	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.PostDone(t.Context(), projectID, testFixtureRef, testPRURL, ""); err == nil {
		t.Error("PostDone err = nil, want an error")
	}
}

// cancelingHandler is a job.Handler test double that cancels a captured
// context.CancelFunc from inside Run, simulating the tick context becoming
// cancelled (a drain force-cancel racing the exact moment the handler
// finishes) right before the post-handler store writes run, then returns
// either a fixed, valid commit (next/reason set) or a plain error (err set).
// It exercises dispatch fix 4's detached, bounded post-handler context on
// both the commit path (TestTick_PostHandlerCommitSurvivesCancelledTickContext)
// and the release path (TestTick_ReleaseClaimSurvivesCancelledTickContext).
type cancelingHandler struct {
	cancel context.CancelFunc

	next, reason string
	err          error
}

func (h *cancelingHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	h.cancel()
	if h.err != nil {
		return store.HandlerCommit{}, h.err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: h.next, Reason: h.reason}, nil
}

// spyHandler is a job.Handler test double: it records every call, its
// context's deadline, and the claim it was handed, and either returns err or
// a fixed, valid commit built from next/reason (or a fenced no-op when next
// is empty). #45 dispatchers can launch more than one ticket's handler at
// once (design section 4.1), so every recorded field is guarded by mu and
// read back only through the accessor methods below, never the bare field,
// so -race never sees a worker goroutine's write race a test goroutine's
// read (design section 10 item 1: "make spyHandler ... goroutine-safe").
type spyHandler struct {
	mu          sync.Mutex
	calls       int
	hasDeadline bool
	deadline    time.Time
	expires     time.Time
	// judgeCodexHome records d.JudgeCodexHome (PKG9-PLAN.md section 4.3,
	// 7.3, D27), so TestRunAndCommitCopiesJudgeCodexHome can assert
	// runAndCommit copied dispatch.Config.JudgeCodexHome into the Deps a
	// handler actually sees.
	judgeCodexHome string
	// reviewBots records d.ReviewBots, so
	// TestRunAndCommitCopiesReviewBots can assert runAndCommit copied
	// dispatch.Config.ReviewBots into the Deps a handler actually sees,
	// the same way judgeCodexHome already covers JudgeCodexHome.
	reviewBots job.ReviewBotRule
	// budget records d.Budget, so TestSetTuning_NextDepsCarriesNewBudget
	// can assert a SetTuning call reaches the very next job.Deps
	// runAndCommit builds (#81).
	budget time.Duration

	next, reason string
	err          error
}

func (h *spyHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	h.mu.Lock()
	h.calls++
	h.expires = d.Expires
	h.judgeCodexHome = d.JudgeCodexHome
	h.reviewBots = d.ReviewBots
	h.budget = d.Budget
	if dl, ok := ctx.Deadline(); ok {
		h.hasDeadline = true
		h.deadline = dl
	}
	err := h.err
	h.mu.Unlock()
	if err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: h.next, Reason: h.reason}, nil
}

// Calls returns the number of times Run has been called so far.
func (h *spyHandler) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// HasDeadline reports whether the most recent Run call's context carried a
// deadline.
func (h *spyHandler) HasDeadline() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hasDeadline
}

// Deadline returns the most recent Run call's context deadline.
func (h *spyHandler) Deadline() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deadline
}

// Expires returns the most recent Run call's d.Expires.
func (h *spyHandler) Expires() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.expires
}

// JudgeCodexHome returns the most recent Run call's d.JudgeCodexHome.
func (h *spyHandler) JudgeCodexHome() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.judgeCodexHome
}

// ReviewBots returns the most recent Run call's d.ReviewBots.
func (h *spyHandler) ReviewBots() job.ReviewBotRule {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reviewBots
}

// Budget returns the most recent Run call's d.Budget.
func (h *spyHandler) Budget() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.budget
}

// countingRuntime wraps a runtime.Runtime and counts every Run call, so a
// test can assert the fake was driven exactly once and never re-driven.
type countingRuntime struct {
	rt    runtime.Runtime
	calls atomic.Int64
}

func (c *countingRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	c.calls.Add(1)
	return c.rt.Run(ctx, req)
}

// staleOwnerHandler runs one real fake-runtime turn (job planning), then
// simulates a concurrent reconcile stealing this ticket's lease mid-run by
// expiring every claim as of just past its own Expires, and finally returns
// a commit that would otherwise be perfectly legal (planning -> done; not
// "building", which D32's own seal invariant, design section 22.12.3a, now
// gates on a GateApproval this test has no reason to carry).
// CommitHandlerResult's fence then no longer matches, so the dispatcher must
// fail closed rather than re-drive the fake session it already advanced.
type staleOwnerHandler struct{}

func (staleOwnerHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rt, err := d.Runtimes.For(d.Machine.Jobs[string(response.JobPlanning)].Runtime)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := rt.Run(ctx, runtime.RunRequest{Job: response.JobPlanning}); err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := d.Store.ExpireClaims(ctx, d.Expires.Add(time.Second), ""); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: testStateDone, Reason: testReasonPlanReady}, nil
}

// staleOwnerReleaseHandler runs one real fake-runtime turn (job planning),
// then simulates a concurrent reconcile stealing this ticket's lease
// mid-run the same way staleOwnerHandler does, but then returns a plain
// handler error instead of a commit. That drives the dispatcher's error
// path (runAndCommit -> releaseClaim), where the fenced no-op release commit
// now finds the lease already gone, exercising the release-path fail-closed
// behavior (fix 8) rather than the post-run-commit path staleOwnerHandler
// (above) exercises.
type staleOwnerReleaseHandler struct{}

func (staleOwnerReleaseHandler) Run(ctx context.Context, _ store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rt, err := d.Runtimes.For(d.Machine.Jobs[string(response.JobPlanning)].Runtime)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := rt.Run(ctx, runtime.RunRequest{Job: response.JobPlanning}); err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := d.Store.ExpireClaims(ctx, d.Expires.Add(time.Second), ""); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{}, errors.New("boom: handler blew up after the runtime already ran, lease now stolen")
}

// slowTracker wraps a Tracker and sleeps for delay before delegating Intake,
// so a test can simulate a slow intake step without touching the
// dispatcher's own timing code.
type slowTracker struct {
	tracker.Tracker
	delay time.Duration
}

func (s *slowTracker) Intake(ctx context.Context, project string, rule tracker.IntakeRule) ([]tracker.Ticket, error) {
	time.Sleep(s.delay)
	return s.Tracker.Intake(ctx, project, rule)
}

// --- the D16 seal-mismatch, ErrNoAction, and ErrCanceled dispatcher rules,
// and the D12 post-commit TrackerEffect (design section 4.5, 6.8) ----------

// testPlanPayload is examples/artifacts/plan.json verbatim: a real,
// schema-valid "plan" artifact, so the seal-mismatch test below fails at
// stage "count" (an empty scenario cohort) rather than at stage "plan"
// (design D16, section 4.5 check 1 then check 2).
const testPlanPayload = `{
  "overview": {
    "objective": "Stop checkout from crashing on an empty cart.",
    "context": "internal/cart handles cart state; internal/checkout reads it at payment time.",
    "problem": {
      "text": "checkout panics when cart.Items is nil instead of an empty slice.",
      "loop": {
        "cmd": "go test ./internal/cart/... -run TestEmptyCart",
        "text": "fails: nil pointer dereference in checkout.Total"
      },
      "repro": "create a cart, call Checkout without adding items",
      "hypotheses": [
        {
          "rank": 1,
          "cause": "NewCart never initializes Items",
          "prediction": "initializing Items to []Item{} makes the loop pass"
        }
      ]
    },
    "goals": ["checkout never panics on an empty cart"],
    "nongoals": ["changing the checkout API"]
  },
  "design": {
    "demo": {
      "cmd": "go run ./cmd/demo -empty-cart",
      "text": "an empty cart checks out for zero dollars instead of crashing"
    },
    "shape": "NewCart initializes Items to an empty slice; checkout reads it unchanged.",
    "changes": [
      {
        "path": "internal/cart/cart.go",
        "symbol": "NewCart",
        "kind": "modified",
        "callers": "checkout.New",
        "callees": "none",
        "before": "Items field left at its zero value (nil)",
        "after": "Items: make([]Item, 0)"
      }
    ],
    "types": [],
    "migrations": { "migrations": [] }
  },
  "delivery": {
    "files": [
      { "path": "internal/cart/cart.go", "action": "modify", "reason": "initialize Items to an empty slice" }
    ],
    "deletions": { "deletions": [] },
    "tests": [
      {
        "name": "TestEmptyCart_ReturnsEmptyOrder",
        "seam": "cart.NewCart",
        "kind": "regression",
        "mocks": "",
        "asserts": "checkout of a freshly created cart returns a zero-item order, no panic"
      }
    ],
    "tasks": [
      { "n": 1, "test": "TestEmptyCart_ReturnsEmptyOrder", "demo": true, "text": "Initialize cart.Items to an empty slice in NewCart." }
    ]
  },
  "review": {
    "trust_root": "none",
    "alternatives": ["guard checkout.Total with a nil check instead of fixing the source"],
    "risks": ["other constructors that build a Cart by struct literal still skip this initializer"]
  }
}
`

// sealMismatchHandler reserves a fresh run through Deps.Reserve, proposes a
// real "plan" artifact for it, and requests a Seal whose ExpectedCount (2)
// can never match the cohort's real scenario count (0, since this handler
// inserts none): CommitHandlerResult's own tx fails at stage "count" and
// rolls the whole commit back, including the plan artifact insert, so a
// second call sees the same, still-empty cohort and mismatches the same way
// (design D16, section 4.5, 6.6 branch 0).
type sealMismatchHandler struct{}

func (sealMismatchHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rsv, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		return store.HandlerCommit{}, err
	}

	// D32 (design section 22.12.3a): the seal invariant needs a confirmed
	// approval; this handler tests sealCohortTx's own mismatch, not the
	// gate flow, so it seeds a fresh, valid one on every call (including a
	// retry after a prior mismatch rolled its own fixture back).
	gateQID, approveAID, gaErr := gateApprovalFixture(ctx, d.Store, t.ID)
	if gaErr != nil {
		return store.HandlerCommit{}, gaErr
	}

	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Artifacts:    []store.Artifact{{RunID: &rsv.RunID, Type: testArtifactTypePlan, Version: 1, Payload: json.RawMessage(testPlanPayload)}},
		Seal:         &store.SealRequest{RunID: rsv.RunID, PlanVersion: 1, ExpectedCount: 2, At: time.Now()},
		GateApproval: &store.GateApproval{QuestionID: gateQID, AnswerID: approveAID, PlanVersion: 1},
	}, nil
}

// gateApprovalFixture seeds the minimal gate approval the seal invariant
// needs (D32, design section 22.12.1, 22.12.3a): a gate question, its
// approving answer, and the confirming marker binding both to plan version
// 1. Used by test handlers whose own point is sealCohortTx's or the
// dispatcher's behavior, not the gate flow itself.
func gateApprovalFixture(ctx context.Context, s *store.Store, ticketID int64) (gateQID, approveAID int64, err error) {
	const authorZing = "zing" // avoids a third bare "zing" literal (goconst)
	gatePayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindGate, State: response.QuestionStateAnswered,
		Recommended: "a", Options: []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		return 0, 0, err
	}
	gateQID, err = s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, Type: testQuestionLiteral, Author: authorZing, State: new("answered"), Body: "Q1", Payload: gatePayload,
	})
	if err != nil {
		return 0, 0, err
	}
	approveAID, err = s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &gateQID, Type: "answer", Author: "you", State: new("sent"), Payload: []byte(`{"option":"a"}`),
	})
	if err != nil {
		return 0, 0, err
	}
	_, err = s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &gateQID, Type: testMsgTypeUpdate, Author: "system",
		Body: fmt.Sprintf("gate confirmed run 1 plan v1 gate %d answer %d", gateQID, approveAID),
	})
	return gateQID, approveAID, err
}

// countSealMismatchMarkers counts msgs' "update" messages whose body starts
// with "seal mismatch cohort " (design section 5.3's marker convention,
// D16): every caller below only ever counts this one marker.
func countSealMismatchMarkers(msgs []store.MessageRow) int {
	const prefix = "seal mismatch cohort"
	n := 0
	for i := range msgs {
		if msgs[i].Type == testMsgTypeUpdate && strings.HasPrefix(msgs[i].Body, prefix) {
			n++
		}
	}
	return n
}

// TestTick_SealMismatchReleasesClaimWritesMarkerAndContinues proves the D16
// dispatcher rule (design section 4.5, 6.6 branch 0): a seal transaction
// mismatch releases the claim, writes a "seal mismatch cohort <runID>"
// marker, never stops the dispatcher, and Tick returns nil so the ticket is
// picked up again on a later tick, which mismatches (and marks) the same
// way a second time.
func TestTick_SealMismatchReleasesClaimWritesMarkerAndContinues(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = sealMismatchHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a seal mismatch must not fail closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false (a seal mismatch must not fail closed)")
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 1 {
		t.Fatalf("seal mismatch markers after first tick = %d, want 1", got)
	}

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v, want nil", err)
	}
	msgs, err = s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after second tick: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 2 {
		t.Errorf("seal mismatch markers after second tick = %d, want 2", got)
	}
}

// sealRefusedHandler proposes a Seal that sealCohortTx would otherwise
// accept (its ExpectedCount matches the one scenario it seeds), carrying no
// GateApproval at all: the seal invariant's own check 1 refuses it (design
// section 22.12.3a) before sealCohortTx ever runs, isolating that refusal
// from a sealCohortTx mismatch.
type sealRefusedHandler struct{}

func (sealRefusedHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rsv, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	scPayload, err := json.Marshal(response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Given: "the server is running", When: "a request arrives", Then: "it responds",
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Artifacts: []store.Artifact{
			{RunID: &rsv.RunID, Type: testArtifactTypePlan, Version: 1, Payload: json.RawMessage(testPlanPayload)},
			{RunID: &rsv.RunID, Type: "scenario", Payload: scPayload},
		},
		Seal: &store.SealRequest{RunID: rsv.RunID, PlanVersion: 1, ExpectedCount: 1, At: time.Now()},
	}, nil
}

// TestSealRefusedReleasesClaimAndMarks proves the D32 dispatcher rule
// (design section 22.12.3a): a seal invariant refusal (here, "no gate
// approval check") releases the claim, writes the "seal refused gate <QID>"
// marker (QID 0, since the commit carried no GateApproval at all) with the
// reason on its own line, never escalates, and never stops the dispatcher.
func TestSealRefusedReleasesClaimAndMarks(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = sealRefusedHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a seal refusal must not fail closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false (a seal refusal must not fail closed)")
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.Type == testMsgTypeUpdate && m.Body == "seal refused gate 0\nno gate approval check" {
			found = true
		}
	}
	if !found {
		t.Fatalf("messages = %+v, want a \"seal refused gate 0\\nno gate approval check\" marker", msgs)
	}
}

// seedGateReadyTicket seeds one project, one ticket already sitting in
// "planning" with kind "feature" (bypassing classify and the interview,
// which the real gate approve pre-check never touches), a plan cohort of n
// scenario artifacts (all unsealed) under a fresh reserved run, and one
// open, then answered ("a", approve), gate question attached to that run --
// the state design section 6.6's entry step 1(a) finds on its very first
// tick: an answered gate round ready to interpret. It returns the ticket id
// and the cohort's own run id.
func seedGateReadyTicket(t *testing.T, s *store.Store, n int) (ticketID, runID int64) {
	t.Helper()
	ctx := t.Context()

	projectID := seedProject(t, s)
	kind := "feature"
	ticketID, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: "gate-race#1", Title: testTicketTitle, Kind: &kind, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: InsertTicket: %v", err)
	}
	runHandlerOnce(t, s, fakeRuntime(t), ticketID, testStateQueued) // queued -> planning

	owner := "seed-gate-ready"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedGateReadyTicket: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(ctx, ticketID, owner, expires, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: reserve: %v", err)
	}

	extID := "seed-gate-ready-ext"
	artifacts := make([]store.Artifact, 0, 1+n)
	artifacts = append(artifacts, store.Artifact{RunID: &rsv.RunID, Type: testArtifactTypePlan, Version: 1, Payload: json.RawMessage(testPlanPayload)})
	for i := range n {
		sc := response.Scenario{
			ID: fmt.Sprintf("s%d", i+1), Kind: response.ScenarioKindBehavior,
			Given: "the server is running", When: "a client sends a request", Then: "the response is correct",
		}
		payload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			t.Fatalf("seedGateReadyTicket: marshal scenario: %v", marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{RunID: &rsv.RunID, Type: "scenario", Payload: payload})
	}

	outcome, exitCode, agentSeconds := "ready", 0, 1
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session:   &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID},
		Runs:      []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Artifacts: artifacts,
	})
	if err != nil || !applied {
		t.Fatalf("seedGateReadyTicket: store the cohort: applied=%v err=%v", applied, err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindGate, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: marshal gate question payload: %v", err)
	}
	const authorZing = "zing" // avoids a third bare "zing" literal (goconst)
	qID, err := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, RunID: &rsv.RunID, Type: testQuestionLiteral, Author: authorZing,
		State: new("open"), Body: "the plan objective", Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: insert gate question: %v", err)
	}
	if res, ansErr := s.AnswerQuestion(ctx, store.AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"}); ansErr != nil || !res.Accepted {
		t.Fatalf("seedGateReadyTicket: AnswerQuestion: %+v, %v", res, ansErr)
	}

	// D32 (design section 22.12.3a): the seal invariant needs a confirmed
	// approval, so this seeds the confirming marker directly -- the race
	// and mismatch behavior below is gateApprove's own pre-check, not the
	// confirming turn, which gets its own dedicated tests.
	msgs, listErr := s.ListMessages(ctx, ticketID)
	if listErr != nil {
		t.Fatalf("seedGateReadyTicket: ListMessages: %v", listErr)
	}
	var aID int64
	for i := range msgs {
		if msgs[i].Type == "answer" && msgs[i].ParentID != nil && *msgs[i].ParentID == qID {
			aID = msgs[i].ID
		}
	}
	if aID == 0 {
		t.Fatal("seedGateReadyTicket: no approving answer found")
	}
	if _, insErr := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &qID, Type: testMsgTypeUpdate, Author: "system",
		Body: fmt.Sprintf("gate confirmed run %d plan v1 gate %d answer %d", rsv.RunID, qID, aID),
	}); insErr != nil {
		t.Fatalf("seedGateReadyTicket: insert confirming marker: %v", insErr)
	}

	return ticketID, rsv.RunID
}

// TestTick_GateApproveTOCTOURace_MismatchReleasesThenPreCheckTakesBranch6
// proves the D16 TOCTOU race design section 6.6 branch 0's own commentary
// names as "unreachable in practice" under the single-owner claim, made
// reachable here through job.GateApproveSealRaceHook (a test-only seam):
// between the real gate approve pre-check's own read of CohortSealState
// (sealed == 0) and the commit it builds from that read (branch 4, sealing
// the whole cohort), a second store handle seals one scenario row through a
// raw SQL update, so the commit's own sealCohortTx sees an "update" stage
// mismatch (it sealed 2 of 3 rows, not all 3). The dispatcher's existing
// ErrSealMismatch rule (design D16, proved generically by
// TestTick_SealMismatchReleasesClaimWritesMarkerAndContinues above) releases
// the claim and writes one "seal mismatch cohort <runID>" marker without
// resolving the gate round (the whole mismatched tx rolled back); the next
// tick re-enters the same still-answered round, and this time
// CohortSealState reports 1 of 3 scenarios sealed -- branch 6 (design
// section 6.6), not branch 4 again -- so it escalates seal_failed and
// resolves the round, without writing a second marker.
func TestTick_GateApproveTOCTOURace_MismatchReleasesThenPreCheckTakesBranch6(t *testing.T) {
	// Not t.Parallel(): this test installs job.GateApproveSealRaceHook, a
	// package-level seam shared by every ticket's gate approve call, so it
	// must not run alongside another test whose own ticket might also reach
	// gateApprove concurrently. Go only runs t.Parallel() tests together
	// after every non-parallel test in the package has finished, so leaving
	// this one sequential is what keeps the hook's install/reset window
	// free of any other test's own gate approve call.
	dbPath := filepath.Join(t.TempDir(), "zing.db")
	s, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const scenarioCount = 3
	ticketID, runID := seedGateReadyTicket(t, s, scenarioCount)

	var raceOnce sync.Once
	job.GateApproveSealRaceHook = func(gotRunID int64) {
		raceOnce.Do(func() {
			if gotRunID != runID {
				t.Errorf("GateApproveSealRaceHook: runID = %d, want %d", gotRunID, runID)
			}
			db2, openErr := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
			if openErr != nil {
				t.Fatalf("race: open second db handle: %v", openErr)
			}
			defer func() { _ = db2.Close() }()
			if _, execErr := db2.ExecContext(t.Context(),
				`UPDATE artifacts SET sealed_at = ? WHERE id = (
					SELECT id FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ? ORDER BY id LIMIT 1
				)`,
				time.Now().UTC().Format(time.RFC3339), ticketID, gotRunID,
			); execErr != nil {
				t.Fatalf("race: seal one scenario row through the second handle: %v", execErr)
			}
		})
	}
	t.Cleanup(func() { job.GateApproveSealRaceHook = nil })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v, want nil (a seal mismatch must not fail closed)", err)
	}

	afterFirst := getTicket(t, s, ticketID)
	if afterFirst.State != testStatePlanning {
		t.Errorf("after the raced tick: ticket state = %q, want unchanged planning", afterFirst.State)
	}
	if afterFirst.ClaimOwner != nil {
		t.Errorf("after the raced tick: claim owner = %v, want nil (released)", *afterFirst.ClaimOwner)
	}
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after first tick: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 1 {
		t.Fatalf("seal mismatch markers after the raced tick = %d, want 1", got)
	}
	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered) after first tick: %v", err)
	}
	if len(answered) != 1 {
		t.Fatalf("answered questions after the raced tick = %d, want 1 (the mismatched tx rolled back the resolution)", len(answered))
	}

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v, want nil", err)
	}

	afterSecond := getTicket(t, s, ticketID)
	if afterSecond.State != testStatePlanning {
		t.Errorf("after the second tick: ticket state = %q, want unchanged planning (branch 6 only escalates)", afterSecond.State)
	}
	if afterSecond.WaitingOn == nil || *afterSecond.WaitingOn != testWaitingQuestions {
		t.Errorf("after the second tick: waiting_on = %v, want questions (the seal_failed escalation)", afterSecond.WaitingOn)
	}

	msgs, err = s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after second tick: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 1 {
		t.Errorf("seal mismatch markers after the second tick = %d, want still 1 (no new mismatch)", got)
	}
	var escalation *response.EscalationPayload
	for i := range msgs {
		if msgs[i].Type != testMsgTypeEscalation {
			continue
		}
		var p response.EscalationPayload
		if unmarshalErr := json.Unmarshal(msgs[i].Payload, &p); unmarshalErr != nil {
			t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
		}
		escalation = &p
	}
	if escalation == nil {
		t.Fatal("no escalation message found after the second tick")
	}
	if escalation.Code != string(response.EscalationCodeSealFailed) {
		t.Errorf("escalation.Code = %q, want seal_failed", escalation.Code)
	}
	wantWhat := "cohort is partially or inconsistently sealed (1 of 3)"
	if escalation.What != wantWhat {
		t.Errorf("escalation.What = %q, want %q", escalation.What, wantWhat)
	}

	resolved, err := s.QuestionsByState(t.Context(), ticketID, "resolved")
	if err != nil {
		t.Fatalf("QuestionsByState(resolved) after second tick: %v", err)
	}
	if len(resolved) != 1 {
		t.Errorf("resolved questions after the second tick = %d, want 1 (the gate round)", len(resolved))
	}
}

// noActionHandler always returns job.ErrNoAction: the entry decision found
// nothing to do this tick (design section 4.5, 5.1 step 8).
type noActionHandler struct{}

func (noActionHandler) Run(context.Context, store.Ticket, job.Deps) (store.HandlerCommit, error) {
	return store.HandlerCommit{}, job.ErrNoAction
}

// TestTick_ErrNoActionReleasesClaimWithoutStopping proves job.ErrNoAction
// releases the claim through the same no-op-commit path any other handler
// error uses, but never stops the dispatcher (design section 4.5).
func TestTick_ErrNoActionReleasesClaimWithoutStopping(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = noActionHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (ErrNoAction must not fail closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false (ErrNoAction must not fail closed)")
	}
}

// cancelingReserveHandler reserves a fresh run, then returns
// runtime.ErrCanceled with no commit, simulating a parent-context
// cancellation (dispatcher shutdown) the runtime itself reported mid-call
// (design D13, section 4.5).
type cancelingReserveHandler struct{}

func (cancelingReserveHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if _, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX}); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{}, runtime.ErrCanceled
}

// TestTick_ErrCanceledWithLiveContextLeavesClaimForExpireClaimsToReconcile
// (renamed from the pre-#45 TestTick_ErrCanceledLeavesClaimForExpireClaimsToReconcile
// to name its own now-narrower scope) proves runtime.ErrCanceled leaves the
// claim in place -- neither released nor fail-closed -- so the reserved run
// sits with a NULL outcome until the lease expires, at which point
// ExpireClaims (already exercised by task 4a) reconciles it to error/-1
// (design D13, section 4.5), but only while the Tick's own ctx is still
// live: #45 section 7.2 adds a second case, a run whose runAndCommit ctx
// (the Dispatcher's own, not the handler's) has itself been cancelled,
// where any error the handler returns -- runtime.ErrCanceled included --
// now means a shutdown interrupt instead (see
// TestTick_HandlerCommitDiscardedAfterCancelledTickContext and
// TestRun_ForceCancelInterruptsInflight). t.Context() here is never
// cancelled, so this test still exercises the original, narrower
// leave-the-claim-in-place path.
func TestTick_ErrCanceledWithLiveContextLeavesClaimForExpireClaimsToReconcile(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = cancelingReserveHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (ErrCanceled must not fail closed)", err)
	}

	claimed := getTicket(t, s, ticketID)
	if claimed.State != testStateQueued {
		t.Errorf("ticket state = %q, want unchanged queued", claimed.State)
	}
	if claimed.ClaimOwner == nil || claimed.ClaimExpiresAt == nil {
		t.Fatal("claim was released, want it left in place for ExpireClaims to reconcile")
	}

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 (the reserved run)", len(runs))
	}
	if runs[0].Outcome != nil {
		t.Errorf("run outcome = %q, want nil before the lease expires", *runs[0].Outcome)
	}

	if _, expireErr := s.ExpireClaims(t.Context(), claimed.ClaimExpiresAt.Add(time.Second), ""); expireErr != nil {
		t.Fatalf("ExpireClaims: %v", expireErr)
	}

	runs, err = s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket after ExpireClaims: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome == nil || *runs[0].Outcome != testOutcomeError {
		t.Fatalf("after ExpireClaims: run = %+v, want outcome error", runs[0])
	}
	if runs[0].ExitCode == nil || *runs[0].ExitCode != -1 {
		t.Errorf("after ExpireClaims: run exit_code = %v, want -1", runs[0].ExitCode)
	}
}

// postRunFailureHandler reserves a fresh run through Deps.Reserve, then
// returns the exact commit shape job.postRunFailure builds for design F025
// (a run terminalized as an error, plus a post_run_failed escalation), with
// no error at all -- since job.postRunFailure is unexported, this stands in
// for calling it directly, the fallback the F025 test plan itself allows.
// The point of this test is the dispatcher side of the fix: proving that
// once the job package has funneled a post-run failure into an ordinary
// commit, the dispatcher applies it through its normal CommitHandlerResult
// path (clearing the claim as part of that same fenced transaction) rather
// than through releaseClaim, which is what a plain error returned after
// Reserve used to force before the fix, orphaning the run with a NULL
// outcome forever.
type postRunFailureHandler struct{}

func (postRunFailureHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rsv, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	outcome := testOutcomeError
	exitCode := 0
	agentSeconds := 1
	waiting := testWaitingQuestions
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Runs: []store.Run{{ID: rsv.RunID, Turn: rsv.Turn, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Escalation: &store.EscalationCommit{
			RunID: &rsv.RunID,
			Body:  string(response.EscalationCodePostRunFailed) + ": storing or checking the plan",
			Payload: response.EscalationPayload{
				Code: string(response.EscalationCodePostRunFailed), What: "storing or checking the plan",
				Why:       "the agent's turn completed, but Zing could not store or check its result",
				Tried:     "boom: project for ticket: store lookup failed",
				Options:   []string{"retry", "planning", "abandon"},
				SessionID: &rsv.SessionID, Origin: string(response.EscalationOriginPlanningFirst),
			},
		},
		Waiting: &waiting,
	}, nil
}

// TestTick_HandlerErrorAfterReserve proves design F025's dispatcher-side
// half: a commit shaped like job.postRunFailure's own (a reserved run
// terminalized as an error, plus a post_run_failed escalation) commits clean
// through the dispatcher's ordinary path -- the claim clears as part of that
// same commit, never through releaseClaim -- Tick returns nil, and the
// dispatcher never stops.
func TestTick_HandlerErrorAfterReserve(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = postRunFailureHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (cleared by the commit, not releaseClaim)", *final.ClaimOwner)
	}

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome == nil || *runs[0].Outcome != testOutcomeError {
		t.Fatalf("runs = %+v, want exactly 1 with outcome error", runs)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var escalation *store.MessageRow
	for i := range msgs {
		if msgs[i].Type == testMsgTypeEscalation {
			escalation = &msgs[i]
		}
	}
	if escalation == nil {
		t.Fatal("no escalation message persisted")
	}
	var payload response.EscalationPayload
	if err = json.Unmarshal(escalation.Payload, &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	if payload.Code != string(response.EscalationCodePostRunFailed) {
		t.Errorf("escalation payload.Code = %q, want %q", payload.Code, response.EscalationCodePostRunFailed)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false")
	}
}

// claimLostBeforeReserveHandler steals its own ticket's lease (expiring
// every claim as of just past its own Expires, the same technique
// staleOwnerHandler uses) and then calls Deps.Reserve, which fences on the
// exact claim it was handed and finds it already gone: store.ErrClaimLost,
// with the runtime never called at all (design F021, section 4.6 step 7).
// It returns that error wrapped, exactly the shape a real handler's runJob
// call would produce.
type claimLostBeforeReserveHandler struct{}

func (claimLostBeforeReserveHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if _, err := d.Store.ExpireClaims(ctx, d.Expires.Add(time.Second), ""); err != nil {
		return store.HandlerCommit{}, err
	}
	_, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err == nil {
		return store.HandlerCommit{}, errors.New("Reserve unexpectedly succeeded after the lease was stolen")
	}
	return store.HandlerCommit{}, fmt.Errorf("job: claim lost before reserve: %w", err)
}

// TestTick_ClaimLostBeforeReserveDoesNotFailClosed proves design F021: a
// handler error wrapping store.ErrClaimLost from before runJob ever reserved
// a run (nothing was written, the runtime never ran) logs and moves on --
// Tick returns nil, the dispatcher never stops, and the runtime is never
// called -- unlike the genuine post-runtime lease-loss case
// (staleOwnerHandler, staleOwnerReleaseHandler), which still fails closed.
func TestTick_ClaimLostBeforeReserveDoesNotFailClosed(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = claimLostBeforeReserveHandler{}

	rt := &countingRuntime{rt: fakeRuntime(t)}
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (ErrClaimLost before Reserve must not fail closed)", err)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false (ErrClaimLost before Reserve must not fail closed)")
	}
	if got := rt.calls.Load(); got != 0 {
		t.Errorf("runtime calls = %d, want 0 (the runtime must never be called)", got)
	}
}

// trackerEffectHandler proposes a commit carrying only a TrackerEffect: no
// state transition, so ValidateCommit's non-empty rule is satisfied by
// TrackerEffect alone (design section 4.5).
type trackerEffectHandler struct{}

func (trackerEffectHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		TrackerEffect: &store.TrackerEffect{
			Kind: store.TrackerEffectKindNothingToDo, Ref: testFixtureRef, Notes: "already handled elsewhere",
		},
	}, nil
}

// trackerEffectUnknownKindHandler proposes a commit carrying a
// TrackerEffect whose Kind postCommitTrackerEffect does not recognize
// (design section 4.5): an unrecognized Kind must never guess which
// comment to send.
type trackerEffectUnknownKindHandler struct{}

func (trackerEffectUnknownKindHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		TrackerEffect: &store.TrackerEffect{Kind: "mystery", Ref: testFixtureRef, Notes: "should never post"},
	}, nil
}

// TestTrackerEffectUnknownKindPostsNothing proves postCommitTrackerEffect's
// fail-safe default (design section 4.5): a Kind it does not recognize
// posts no comment at all, rather than guessing one.
func TestTrackerEffectUnknownKindPostsNothing(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	if _, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	}); err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectUnknownKindHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	rec := &commentingFixture{Fixture: newFixtureTracker(t)}
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("tracker comments = %+v, want none (an unrecognized kind must post nothing)", got)
	}
	if rec.Closed(testFixtureRef) {
		t.Errorf("issue closed = true, want false (an unrecognized kind must close nothing)")
	}
}

// TestTick_TrackerEffectPostsNothingToDoCommentAfterCommit proves the D12
// post-commit effect (design section 4.5, 6.8): once CommitHandlerResult has
// applied, the dispatcher resolves the ticket's project binding and posts
// tracker.NothingToDoComment for that binding's user.
func TestTick_TrackerEffectPostsNothingToDoCommentAfterCommit(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	const bindingUser = testBindingUser
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: bindingUser}}

	rec := &commentingFixture{Fixture: newFixtureTracker(t)}
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("tracker comments = %d, want 1", len(got))
	}
	wantBody := tracker.NothingToDoComment(bindingUser, "already handled elsewhere") + "\n\n" + fmt.Sprintf("<!-- zing:nothing t%d -->", ticketID)
	if got[0].body != wantBody {
		t.Errorf("comment body =\n%q\nwant\n%q", got[0].body, wantBody)
	}
	if got[0].ref != testFixtureRef {
		t.Errorf("comment ref = %q, want %q", got[0].ref, testFixtureRef)
	}
	if got[0].project != testProject.Name {
		t.Errorf("comment project = %q, want %q", got[0].project, testProject.Name)
	}
	if !rec.Closed(testFixtureRef) {
		t.Errorf("issue closed = false, want true (the nothing_to_do path closes after commenting)")
	}
}

// TestTick_NothingToDoSkipsMarkedCommentStillCloses proves postMarkedOnce's
// guard applies on the nothing_to_do path too (design section 10.5, owner
// decision Q1): when the zing:nothing marker is already on the issue from
// the tracker's own login, postCommitTrackerEffect posts no comment but
// still closes.
func TestTick_NothingToDoSkipsMarkedCommentStillCloses(t *testing.T) {
	t.Parallel()

	s, projectID, ticketID, tr := shipTestFixture(t)

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	marker := fmt.Sprintf("<!-- zing:nothing t%d -->", ticketID)
	tr.seedMarked(tr.ownLogin, "an earlier post\n\n"+marker)

	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := tr.postedComments(); len(got) != 0 {
		t.Errorf("posted comments = %+v, want none (the marker was already there)", got)
	}
	wantSeq := []string{"close:" + testFixtureRef}
	if seq := tr.callSequence(); !slices.Equal(seq, wantSeq) {
		t.Errorf("call sequence = %v, want %v", seq, wantSeq)
	}
}

// TestTick_NothingToDoCloseFailureIsBestEffort proves a failing Close is
// best-effort, just as a failing Comment is (design D12): Tick still
// returns nil and the ticket's own commit stays applied. It does not run in
// parallel with another subtest that touches slog (matching
// TestTick_IntakeErrorOnOneProjectLogsAndContinuesToTheNext above), since it
// swaps the process-wide slog default to capture "tracker close failed".
func TestTick_NothingToDoCloseFailureIsBestEffort(t *testing.T) {
	s, projectID, ticketID, tr := shipTestFixture(t)

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	tr.failClose = errors.New("boom: close failed")

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, tr, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a tracker close failure is best-effort)", err)
	}

	got := tr.postedComments()
	if len(got) != 1 {
		t.Fatalf("posted comments = %d, want 1", len(got))
	}
	marker := fmt.Sprintf("<!-- zing:nothing t%d -->", ticketID)
	if !strings.HasSuffix(got[0].body, marker) {
		t.Errorf("comment body = %q, want it to end with %q", got[0].body, marker)
	}

	wantSeq := []string{"comment:" + testFixtureRef}
	if seq := tr.callSequence(); !slices.Equal(seq, wantSeq) {
		t.Errorf("call sequence = %v, want %v (a failed close must not be recorded as one)", seq, wantSeq)
	}
	if n := tr.closeAttemptCount(); n != 1 {
		t.Errorf("close attempts = %d, want 1 (the close must actually be tried, not skipped)", n)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (the ticket's own commit still applied)", *final.ClaimOwner)
	}

	logged := logBuf.String()
	wantWarn := fmt.Sprintf("msg=\"tracker close failed\" ticket_id=%d ref=%s", ticketID, testFixtureRef)
	if !strings.Contains(logged, wantWarn) {
		t.Errorf("log output = %q, want it to contain %q", logged, wantWarn)
	}
	if strings.Contains(logged, "tracker issue closed") {
		t.Errorf("log output = %q, want no \"tracker issue closed\" (the close failed)", logged)
	}
}

// planningNothingToDoRuntime lets classify and the planning first turn run
// for real against the checked-in fixtures (so the ticket gets a real kind
// and a real Q1 to answer), then swaps planning's second call (the resume)
// for a hand-built nothing_to_do response, so a test can drive the real
// planning handler's own design section 6.8 nothing_to_do row (task 8)
// through a real dispatch.Tick.
type planningNothingToDoRuntime struct {
	t             *testing.T
	fake          *runtime.Fake
	resp          response.Response
	planningCalls int
}

func (r *planningNothingToDoRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	r.t.Helper()
	if req.Job != response.JobPlanning {
		return r.fake.Run(ctx, req)
	}
	r.planningCalls++
	if r.planningCalls == 1 {
		return r.fake.Run(ctx, req)
	}
	return runtime.RunResult{Response: r.resp, SessionID: "ntd-sess", ExitCode: 0, AgentTime: time.Second}, nil
}

// TestTick_PlanningNothingToDoAllFalseClaimsPostsTrackerComment proves task
// 8's nothing_to_do row end to end, through the real planning handler and a
// real dispatch.Tick (not the trackerEffectHandler stub the previous test
// uses): once classify and the first turn have run for real and the owner
// has answered Q1, a resume whose nothing_to_do response names two code
// claims, both false, terminalizes the run, moves the ticket to done, and
// the same Tick posts exactly one tracker comment whose body is
// tracker.NothingToDoComment(bindingUser, resp.Notes).
func TestTick_PlanningNothingToDoAllFalseClaimsPostsTrackerComment(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	const bindingUser = "nothing-to-do-owner"
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	const notes = "the described behavior already exists and is already tested"
	resp := &response.NothingToDoResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeNothingToDo,
		Claims: []response.Claim{
			{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":1", Text: "already returns hello"},
			{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":2", Text: "already tested"},
		},
		Notes: notes,
		// D31 (design section 22.2): nothing_to_do needs every planning
		// question settled, or checkConversation rejects the response
		// before this outcome's own commit logic (and the tracker comment
		// this test is about) ever runs. Q1 is answerOpenQuestion's own
		// open planning question.
		Replies: []response.Reply{
			{Question: "Q1", Settled: true, Decision: "The owner's answer to Q1 settles this thread."},
		},
	}
	rt := &planningNothingToDoRuntime{t: t, fake: fakeRuntime(t), resp: resp}

	advanceTicket(t, s, rt, ticketID, testStateQueued)    // queued -> planning
	runHandlerOnce(t, s, rt, ticketID, testStatePlanning) // classify: sets kind
	runHandlerOnce(t, s, rt, ticketID, testStatePlanning) // first turn: posts Q1, waits
	answerOpenQuestion(t, s, ticketID)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: bindingUser}}
	rec := &commentingFixture{Fixture: newFixtureTracker(t)}
	d := newDispatcher(t, s, rec, bus.New(), rt, nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateDone {
		t.Fatalf("final ticket state = %q, want done", final.State)
	}

	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("tracker comments = %d, want 1", len(got))
	}
	wantBody := tracker.NothingToDoComment(bindingUser, notes) + "\n\n" + fmt.Sprintf("<!-- zing:nothing t%d -->", ticketID)
	if got[0].body != wantBody {
		t.Errorf("comment body =\n%q\nwant\n%q", got[0].body, wantBody)
	}
	if got[0].ref != testFixtureRef {
		t.Errorf("comment ref = %q, want %q", got[0].ref, testFixtureRef)
	}
}

// TestTick_PlanningNothingToDoClosesIssueOnce proves task 1's close (design
// D12, owner decision Q1): after the real planning handler's nothing_to_do
// commit lands, the same Tick posts tracker.NothingToDoComment through the
// zing:nothing marker and then closes the issue, in that order, and a
// second Tick against the now-done ticket repeats neither call. It does not
// run in parallel with another subtest that touches slog (matching
// TestTick_IntakeErrorOnOneProjectLogsAndContinuesToTheNext above), since it
// swaps the process-wide slog default to capture "tracker issue closed".
func TestTick_PlanningNothingToDoClosesIssueOnce(t *testing.T) {
	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	const bindingUser = "nothing-to-do-owner"
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	const notes = "the described behavior already exists and is already tested"
	resp := &response.NothingToDoResponse{
		Job: response.JobPlanning, Outcome: response.OutcomeNothingToDo,
		Claims: []response.Claim{
			{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":1", Text: "already returns hello"},
			{Kind: response.ClaimKindCode, Verdict: response.ClaimVerdictFalse, Evidence: readyClaimEvidencePath + ":2", Text: "already tested"},
		},
		Notes: notes,
		Replies: []response.Reply{
			{Question: "Q1", Settled: true, Decision: "The owner's answer to Q1 settles this thread."},
		},
	}
	rt := &planningNothingToDoRuntime{t: t, fake: fakeRuntime(t), resp: resp}

	advanceTicket(t, s, rt, ticketID, testStateQueued)    // queued -> planning
	runHandlerOnce(t, s, rt, ticketID, testStatePlanning) // classify: sets kind
	runHandlerOnce(t, s, rt, ticketID, testStatePlanning) // first turn: posts Q1, waits
	answerOpenQuestion(t, s, ticketID)

	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: bindingUser}}
	tr := newShipTrackerDouble(t, "zing-bot")
	d := newDispatcher(t, s, tr, bus.New(), rt, nil, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateDone {
		t.Fatalf("final ticket state = %q, want done", final.State)
	}

	wantBody := tracker.NothingToDoComment(bindingUser, notes) + "\n\n" + fmt.Sprintf("<!-- zing:nothing t%d -->", ticketID)
	got := tr.postedComments()
	if len(got) != 1 {
		t.Fatalf("posted comments = %d, want 1", len(got))
	}
	if got[0].body != wantBody {
		t.Errorf("comment body =\n%q\nwant\n%q", got[0].body, wantBody)
	}
	if got[0].ref != testFixtureRef {
		t.Errorf("comment ref = %q, want %q", got[0].ref, testFixtureRef)
	}

	wantInfo := fmt.Sprintf("msg=\"tracker issue closed\" ticket_id=%d ref=%s", ticketID, testFixtureRef)
	if logged := logBuf.String(); !strings.Contains(logged, wantInfo) {
		t.Errorf("log output = %q, want it to contain %q", logged, wantInfo)
	}

	wantSeq := []string{"comment:" + testFixtureRef, "close:" + testFixtureRef}
	if seq := tr.callSequence(); !slices.Equal(seq, wantSeq) {
		t.Fatalf("call sequence = %v, want %v", seq, wantSeq)
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := tr.postedComments(); len(got) != 1 {
		t.Errorf("posted comments after second Tick = %d, want still 1 (no retry post)", len(got))
	}
	if seq := tr.callSequence(); !slices.Equal(seq, wantSeq) {
		t.Errorf("call sequence after second Tick = %v, want unchanged %v (no retry comment or close)", seq, wantSeq)
	}
}

// TestTick_TrackerEffectFailureIsBestEffort proves a failing tracker comment
// only warns: Tick still returns nil, and the ticket's own commit (already
// applied before the tracker call runs) is untouched (design D12).
func TestTick_TrackerEffectFailureIsBestEffort(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	rec := &commentingFixture{Fixture: newFixtureTracker(t)}
	rec.failFirst = true
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a tracker comment failure is best-effort)", err)
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("recorded comments = %d, want 0 (the one attempt failed)", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 1 {
		t.Errorf("Comment attempts = %d, want 1", attempts)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (the ticket's own commit still applied)", *final.ClaimOwner)
	}
	if rec.Closed(testFixtureRef) {
		t.Errorf("issue closed = true, want false (a failed comment means no close, owner decision Q2)")
	}
}

// TestDispatchShipGitHubHeadSHAIgnoresInheritedGitDir reproduces the
// pre-push failure of TestShipCIFailThenFixThenMergeGoesDone: lefthook's
// pre-push exports GIT_DIR, which overrides "git -C <origin>", so headSHA
// read the branch off the repository being pushed, GetPR failed every
// poll, and shipping never advanced. headSHA must read the fixture origin.
//
// Not parallel: it calls t.Setenv, which t.Parallel forbids.
func TestDispatchShipGitHubHeadSHAIgnoresInheritedGitDir(t *testing.T) {
	ctx := t.Context()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := gitfixture.NewSigningRepo(ctx, repo); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}
	remoteDir, err := gitfixture.WithBareOrigin(ctx, repo)
	if err != nil {
		t.Fatalf("WithBareOrigin: %v", err)
	}
	if out, pushErr := gitfixture.Git(ctx, repo, "push", "-q", "origin", "main"); pushErr != nil {
		t.Fatalf("push: %v: %s", pushErr, out)
	}
	want, err := gitfixture.Git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	// A decoy repository with no "main" branch, exported as a git hook would.
	decoy := t.TempDir()
	if out, initErr := gitfixture.Git(ctx, decoy, "init", "-q", "-b", "other"); initErr != nil {
		t.Fatalf("init decoy: %v: %s", initErr, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))

	got, err := newDispatchShipGitHub(remoteDir).headSHA(ctx, "main")
	if err != nil {
		t.Fatalf("headSHA: %v", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("headSHA = %q, want %q", got, strings.TrimSpace(string(want)))
	}
}

// --- #45 milestone 4: parallel fill/Tick/Run -------------------------------

// barrierHandler blocks until released, signaling its own arrival on
// started first, so a test can observe exactly when a worker's handler has
// started and control exactly when it finishes. ctx.Done() unblocks both
// selects too, so a force-cancelled dispatcher never leaves this handler
// stuck. Shared by every #45 test below that needs to hold a worker open
// to create or close a specific race window.
type barrierHandler struct {
	started chan int64
	release chan struct{}
	// next and reason, when next is non-empty, make Run return a real
	// transition commit once released, instead of the default fenced
	// no-op.
	next, reason string
	// err, when non-nil, makes Run return it (instead of a commit) once
	// released, modeling a worker whose own error becomes visible right as
	// it is released -- the precise moment a test wants to race against a
	// concurrent drain or stop.
	err error
}

func (h *barrierHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	select {
	case h.started <- t.ID:
	case <-ctx.Done():
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return store.HandlerCommit{}, ctx.Err()
	}
	if h.err != nil {
		return store.HandlerCommit{}, h.err
	}
	if h.next == "" {
		return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}, nil
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: h.next, Reason: h.reason}, nil
}

// reservingBarrierHandler is barrierHandler's twin for a test that needs a
// real open run recorded first (so a later interrupt has something to mark
// interrupted), through d.Reserve the same way a real job handler would.
type reservingBarrierHandler struct {
	started chan int64
	release chan struct{}
	job     string
}

func (h *reservingBarrierHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if _, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: h.job, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX}); err != nil {
		return store.HandlerCommit{}, err
	}
	select {
	case h.started <- t.ID:
	case <-ctx.Done():
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return store.HandlerCommit{}, ctx.Err()
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}, nil
}

// perTicketHandler dispatches to a different job.Handler per ticket id, so
// two tickets claimed in the same fill pass (same state, same registry
// entry) can behave completely differently -- one failing closed while the
// other stays in flight, for instance. byTicket is never written after
// construction, so concurrent Run calls reading it race nothing.
type perTicketHandler struct {
	byTicket map[int64]job.Handler
}

func (h *perTicketHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	return h.byTicket[t.ID].Run(ctx, t, d)
}

// selfStealingHandler simulates a concurrent release of this exact ticket's
// own claim -- and only this ticket's -- by committing the same fenced
// no-op releaseClaim itself uses, before returning what would otherwise be
// a perfectly legal transition commit. The dispatcher's own commit then
// finds its fence already gone and fails closed, isolated to this one
// ticket: safe to run alongside another, unrelated ticket in the same Run
// pass, unlike staleOwnerHandler's own ExpireClaims("") call, which would
// expire every claim, not just this one.
type selfStealingHandler struct{}

func (selfStealingHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if _, err := d.Store.CommitHandlerResult(ctx, store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: testStatePlanning, Reason: testSpyReason}, nil
}

// barrierSelfStealingHandler is selfStealingHandler's own twin that first
// blocks until released (signaling its own arrival on started), the same
// barrierHandler shape, so a test can hold the fail-closed trigger open
// until a specific moment -- most usefully, to fire it back to back with a
// concurrent NotifyDrain (design section 4.4, 4.6).
type barrierSelfStealingHandler struct {
	started chan int64
	release chan struct{}
}

func (h *barrierSelfStealingHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	select {
	case h.started <- t.ID:
	case <-ctx.Done():
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return store.HandlerCommit{}, ctx.Err()
	}
	return selfStealingHandler{}.Run(ctx, t, d)
}

// TestTick_FillsEveryFreeSlot proves fill claims and launches every ready
// candidate it has a free slot for, in one pass, not just the single
// highest-priority one (design section 4.2 step 5, D1): MaxParallel 3 with
// three ready tickets, one Tick call runs all three.
func TestTick_FillsEveryFreeSlot(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	idA := seedQueuedTicket(t, s, "fake#1")
	idB := seedQueuedTicket(t, s, "fake#2")
	idC := seedQueuedTicket(t, s, "fake#3")

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 3, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := spy.Calls(); got != 3 {
		t.Fatalf("spy.Calls() = %d, want 3 (one Tick fills every free slot)", got)
	}
	for _, id := range []int64{idA, idB, idC} {
		if got := getTicket(t, s, id).State; got != testStatePlanning {
			t.Errorf("ticket %d state = %q, want planning", id, got)
		}
	}
}

// TestTick_RefusedClaimTriesNext proves a refused claim on one candidate
// does not end the pass: the next candidate in order is claimed and
// launched within the same Tick call (design section 4.2 step 5).
func TestTick_RefusedClaimTriesNext(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	firstID := seedQueuedTicket(t, s, "fake#1")
	secondID := seedQueuedTicket(t, s, "fake#2")

	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), firstID, "another-worker", expires)
	if err != nil || !claimed {
		t.Fatalf("pre-claim firstID: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := getTicket(t, s, secondID).State; got != testStatePlanning {
		t.Errorf("second ticket state = %q, want planning (a refused claim must try the next candidate in the same pass)", got)
	}
}

// TestTick_ConcurrentDriveRefused proves Tick and Run enforce one driver at
// a time (design section 4.1): a second Tick call while one is still in
// flight, and a Tick call while Run is driving, both return
// ErrConcurrentDrive rather than racing fill's own claim-and-launch
// critical section.
func TestTick_ConcurrentDriveRefused(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	started := make(chan int64, 1)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &barrierHandler{started: started, release: release}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Interval: time.Hour, Owner: testOwner})

	tickErrCh := make(chan error, 1)
	go func() { tickErrCh <- d.Tick(t.Context()) }()

	waitFor(t, started, "handler to start")

	if err := d.Tick(t.Context()); !errors.Is(err, dispatch.ErrConcurrentDrive) {
		t.Fatalf("second Tick while the first is in flight: err = %v, want ErrConcurrentDrive", err)
	}

	close(release)
	if err := waitFor(t, tickErrCh, "first Tick to return"); err != nil {
		t.Fatalf("first Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}
}

// TestRun_RunsTwoTicketsAtOnce proves this process can have more than one
// ticket's handler running at the same time (design D1, section 4.4):
// MaxParallel 2 with two ready tickets, both handlers must be observed
// running (their own started signal received) before either is released.
func TestRun_RunsTwoTicketsAtOnce(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	seedQueuedTicket(t, s, "fake#1")
	seedQueuedTicket(t, s, "fake#2")

	started := make(chan int64, 2)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &barrierHandler{started: started, release: release}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	seen := make(map[int64]bool)
	for len(seen) < 2 {
		seen[waitFor(t, started, "both handlers to start")] = true
	}

	close(release)
	cancel()
	_ = waitFor(t, runErrCh, "Run to return") //nolint:errcheck // this test only proves Run returns after cancel
}

// TestRun_DrainWaitsForInflight proves a graceful drain does not return
// from Run until the in-flight worker has actually finished (design
// section 4.4, 4.5): NotifyDrain while a handler is still blocked must not
// make Run return early.
func TestRun_DrainWaitsForInflight(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	started := make(chan int64, 1)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &barrierHandler{started: started, release: release, next: testStatePlanning, reason: testSpyReason}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitFor(t, started, "handler to start")

	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	d.NotifyDrain()

	// Run must not have returned yet: the handler is still blocked.
	select {
	case err := <-runErrCh:
		t.Fatalf("Run returned early (err=%v) while its one worker was still blocked", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := waitFor(t, runErrCh, "Run to return after the worker was released"); err != nil {
		t.Fatalf("Run: %v, want nil (a graceful drain is not an error)", err)
	}

	if got := getTicket(t, s, ticketID).State; got != testStatePlanning {
		t.Errorf("final ticket state = %q, want planning (the commit must still apply after a graceful drain)", got)
	}
}

// TestRun_ForceCancelInterruptsInflight proves a force-cancelled dispatcher
// context interrupts every in-flight run (design section 4.5, 7.2): the run
// is recorded interrupted (outcome error, interrupted=1) and its claim is
// cleared, rather than left for ExpireClaims to reconcile later.
func TestRun_ForceCancelInterruptsInflight(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	started := make(chan int64, 1)
	release := make(chan struct{}) // never closed: only ctx cancellation frees the handler
	reg := job.Registry()
	reg[testStateQueued] = &reservingBarrierHandler{started: started, release: release, job: testStatePlanning}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitFor(t, started, "handler to start")

	cancel()
	if err := waitFor(t, runErrCh, "Run to return after the force-cancel"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: err = %v, want context.Canceled", err)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (InterruptRuns clears it)", *final.ClaimOwner)
	}
	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if !runs[0].Interrupted {
		t.Error("run.Interrupted = false, want true")
	}
	if runs[0].Outcome == nil || *runs[0].Outcome != testOutcomeError {
		t.Errorf("run.Outcome = %v, want error", runs[0].Outcome)
	}
	if runs[0].ExitCode == nil || *runs[0].ExitCode != -1 {
		t.Errorf("run.ExitCode = %v, want -1", runs[0].ExitCode)
	}
}

// TestRun_FillErrorRaisesAlerts proves a fill error (not a worker error)
// raises both D3 alerts under Run, naming it "in a dispatcher pass" since
// it carries no ticket id (design section 4.6, F001): this is the Run path
// serve actually drives, where three of the four select-loop exits used to
// call setStop and finish without ever calling reportFirstError, so alert 1
// never fired. It drives Run, not Tick (Tick already called
// reportFirstError correctly, which let the Tick-driven version of this
// test hide the Run gap). The store hook is a second, raw connection to
// the same on-disk database that drops the tickets table out from under
// fill's reconcile step (ExpireClaims), a real failure with no test-only
// fault-injection field needed; Run's own Flags(ctx) call (the settings
// table, left intact) still succeeds, so the pass reaches fill for real
// and fails there, at the ticker.C branch's fill-error exit.
func TestRun_FillErrorRaisesAlerts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "zing.db")
	s, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedQueuedTicket(t, s, testFixtureRef)

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.ExecContext(t.Context(), "DROP TABLE tickets"); err != nil {
		t.Fatalf("drop tickets table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after the flags-read error")
	cancel()

	runErr := waitFor(t, runErrCh, "Run to return")
	if runErr == nil {
		t.Fatal("Run against a dropped tickets table: want an error, got nil")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "in a dispatcher pass") {
		t.Errorf("log = %q, want alert 1 naming \"in a dispatcher pass\" (no ticket id)", logged)
	}
	if got := strings.Count(logged, "dispatcher stopped after"); got != 1 {
		t.Errorf("alert 2 (\"dispatcher stopped after ...\") appeared %d times, want exactly 1 (log: %s)", got, logged)
	}
	if i1, i2 := strings.Index(logged, "in a dispatcher pass"), strings.Index(logged, "dispatcher stopped after"); i1 < 0 || i2 < 0 || i2 < i1 {
		t.Errorf("alerts out of order (alert1 at %d, alert2 at %d); log: %s", i1, i2, logged)
	}
}

// TestRun_FailClosedLetsOthersFinish proves D3 end to end under Run: with
// two tickets launched in the same pass, one whose commit fails closed
// (selfStealingHandler) does not stop the other, still in-flight, ticket
// from finishing and committing normally; Run returns only after both are
// done, wrapping ErrFailClosed, and the two alerts appear in order.
func TestRun_FailClosedLetsOthersFinish(t *testing.T) {
	s := newDispatchTestStore(t)
	aID := seedQueuedTicket(t, s, "fake#1")
	bID := seedQueuedTicket(t, s, "fake#2")

	started := make(chan int64, 1)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &perTicketHandler{byTicket: map[int64]job.Handler{
		aID: selfStealingHandler{},
		bID: &barrierHandler{started: started, release: release, next: testStatePlanning, reason: testSpyReason},
	}}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	if got := waitFor(t, started, "B to start"); got != bID {
		t.Fatalf("started ticket = %d, want %d (B)", got, bID)
	}

	waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after A's fail-closed commit")

	close(release)

	waitUntil(t, func() bool { return getTicket(t, s, bID).State == testStatePlanning },
		"B's own commit to apply despite A's fail-closed")
	cancel()

	runErr := waitFor(t, runErrCh, "Run to return")
	if !errors.Is(runErr, dispatch.ErrFailClosed) {
		t.Fatalf("Run err = %v, want errors.Is(err, dispatch.ErrFailClosed)", runErr)
	}

	if got := getTicket(t, s, bID).State; got != testStatePlanning {
		t.Errorf("B's final state = %q, want planning (its own commit must still apply despite A's fail-closed)", got)
	}
	if final := getTicket(t, s, aID); final.ClaimOwner != nil {
		t.Errorf("A's final claim owner = %v, want nil", *final.ClaimOwner)
	}

	logged := logBuf.String()
	firstIdx := strings.Index(logged, fmt.Sprintf("fail-closed on ticket %d", aID))
	secondIdx := strings.Index(logged, "dispatcher stopped after fail-closed")
	if firstIdx < 0 {
		t.Errorf("log = %q, want alert 1 naming ticket %d", logged, aID)
	}
	if secondIdx < 0 {
		t.Errorf("log = %q, want alert 2", logged)
	}
	if firstIdx >= 0 && secondIdx >= 0 && secondIdx < firstIdx {
		t.Errorf("alert 2 appeared before alert 1 in the log")
	}
	if got := strings.Count(logged, "dispatcher stopped after fail-closed"); got != 1 {
		t.Errorf("alert 2 appeared %d times, want exactly 1 (log: %s)", got, logged)
	}
}

// TestFill_StopBetweenCheckAndLaunch proves fill's own launch
// linearization point (design section 4.2 step 5): a stop set between a
// successful Claim and the critical section that would otherwise launch
// the worker must prevent that launch, releasing the claim instead. The
// afterClaimForTest hook (export_test.go) opens this otherwise
// sub-microsecond window deterministically.
func TestFill_StopBetweenCheckAndLaunch(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})
	dispatch.SetAfterClaimForTest(d, func(int64) {
		dispatch.SetStopForTest(d, errors.New("boom: injected stop between claim and launch"))
	})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := spy.Calls(); got != 0 {
		t.Errorf("spy.Calls() = %d, want 0 (the handler must never run once stop was set before launch)", got)
	}
	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want unchanged queued, claim released", final)
	}
}

// TestRun_DrainBetweenClaimAndLaunchReleasesClaim proves the same launch
// linearization point (design section 4.5) specifically for a real drain,
// through NotifyDrain: a drain observed between a successful Claim and the
// launch critical section releases the claim rather than launching the
// worker. Driven through Tick for determinism (Run shares the identical
// fill/NotifyDrain/setStop mechanism, section 4.4).
func TestRun_DrainBetweenClaimAndLaunchReleasesClaim(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Interval: time.Hour, Owner: testOwner})
	dispatch.SetAfterClaimForTest(d, func(int64) {
		if err := s.SetDraining(t.Context(), true); err != nil {
			t.Errorf("SetDraining: %v", err)
		}
		d.NotifyDrain()
	})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := spy.Calls(); got != 0 {
		t.Errorf("spy.Calls() = %d, want 0 (a drain observed between claim and launch must release, not run)", got)
	}
	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want unchanged queued, claim released", final)
	}
}

// TestFill_ReleaseAfterCancelStillLands proves the claim-release write
// fill takes after observing a stop mid-claim still lands even when ctx is
// already cancelled by the time it runs (design section 4.2 step 5,
// "dispatch" fix 4): releaseClaimNoStop's own postHandlerContext detaches
// from ctx, so the cancellation set inside the afterClaimForTest hook must
// not prevent the release.
func TestFill_ReleaseAfterCancelStillLands(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = &spyHandler{next: testStatePlanning, reason: testSpyReason}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	dispatch.SetAfterClaimForTest(d, func(int64) {
		dispatch.SetStopForTest(d, errors.New("boom: injected stop before cancel"))
		cancel()
	})

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want claim released even though ctx was cancelled before the release write", final)
	}
}

// TestFill_CtxCanceledBetweenClaimAndLaunchReleasesClaim proves PR review
// fix D1: fill's own launch linearization point (design section 4.2 step
// 5) also treats a ctx cancellation that never went through setStop as a
// stop -- the force-cancel at the drain deadline can race this exact
// window too, same as an explicit setStop call, and launching a worker
// against an already-cancelled ctx would be no different from launching
// one after a stop. ctx is cancelled here with d.stop deliberately left
// unset, so only the new ctx.Err() check (not the existing d.stop check)
// can be what prevents the launch.
func TestFill_CtxCanceledBetweenClaimAndLaunchReleasesClaim(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	dispatch.SetAfterClaimForTest(d, func(int64) {
		cancel()
	})

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := spy.Calls(); got != 0 {
		t.Errorf("spy.Calls() = %d, want 0 (a ctx cancelled between claim and launch must release, not run)", got)
	}
	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want unchanged queued, claim released", final)
	}
}

// TestRun_AlertNamesTheErrorThatStopped proves reportFirstError always
// describes d.stopErr -- the error the first setStop(err) call with a
// non-nil error recorded -- never whichever error a caller's own select
// happens to observe first (design section 4.6). It drives this through
// Tick, not Run: Tick raises the same two alerts through the same
// reportFirstError/logStopAlert calls (section 4.3), and Tick's own
// synchronous fill-then-collect shape makes the race's two sides -- a
// worker's own setStop call, and fill's own later claim failure -- land in
// one deterministic call, with no ticker timing involved at all.
//
// The scenario: two ready tickets, A first in pick order, B second.
// A's handler (selfStealingHandler) fails closed almost immediately once
// claimed and launched; B's own claim attempt is deliberately blocked,
// through the beforeClaimForTest hook, until A's worker has actually
// called setStop -- so by the time fill reaches B, d.stopErr already names
// A. B's own claim is then made to fail for real (by closing the store
// from inside that same hook, once unblocked), so fill itself also returns
// a second, later error with no ticket id of its own. The first error
// setStop ever recorded must still be the one reportFirstError describes:
// alert 1 names ticket A and selfStealingHandler's own fail-closed cause,
// never "in a dispatcher pass" (what a B-authored alert would say).
func TestRun_AlertNamesTheErrorThatStopped(t *testing.T) {
	s := newDispatchTestStore(t)
	aID := seedQueuedTicket(t, s, "fake#1")
	bID := seedQueuedTicket(t, s, "fake#2")

	reg := job.Registry()
	reg[testStateQueued] = &perTicketHandler{byTicket: map[int64]job.Handler{
		aID: selfStealingHandler{},
		bID: &spyHandler{next: testStatePlanning, reason: testSpyReason},
	}}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	workerAStopped := make(chan struct{})
	dispatch.SetStopErrRecordedForTest(d, func(error) { close(workerAStopped) })
	dispatch.SetBeforeClaimForTest(d, func(ticketID int64) {
		if ticketID != bID {
			return
		}
		<-workerAStopped // B's claim must never be attempted before A's worker has recorded its own error
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close store ahead of B's claim: %v", closeErr)
		}
	})

	err := d.Tick(t.Context())
	if err == nil {
		t.Fatal("Tick: want a joined error (A's fail-closed plus B's claim failure), got nil")
	}
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Errorf("Tick err = %v, want it to wrap ErrFailClosed (A's own error)", err)
	}

	logged := logBuf.String()
	wantAlert1 := fmt.Sprintf("fail-closed on ticket %d:", aID)
	if !strings.Contains(logged, wantAlert1) {
		t.Errorf("log = %q, want alert 1 to start %q (A's own ticket and cause)", logged, wantAlert1)
	}
	if strings.Contains(logged, "in a dispatcher pass") {
		t.Errorf("log = %q, want alert 1 to never describe B's fill error (\"in a dispatcher pass\")", logged)
	}
	wantAlert2 := fmt.Sprintf("dispatcher stopped after fail-closed on ticket %d", aID)
	if !strings.Contains(logged, wantAlert2) {
		t.Errorf("log = %q, want alert 2 %q", logged, wantAlert2)
	}
}

// TestRun_DrainRacingWorkerErrorStillAlerts proves alert 1 and alert 2 each
// appear exactly once, in order, regardless of which side of a genuine
// race Run's own select resolves first (design section 4.4, 4.6): a
// worker's own error becoming visible on results at (as close as this
// process can arrange without a sleep) the same moment NotifyDrain is
// called. reportFirstError's one-shot guard (firstErrorReported) and
// setStop's own "first non-nil wins" rule together must make the outcome
// identical either way: whether Run's select picks the drainCh case (so
// finish's own results-draining loop is what first sees the worker's
// error) or the results case directly (handle(r)), d.stopErr ends up the
// worker's own error in both orderings, since NotifyDrain's own
// setStop(nil) can never claim that slot ahead of a worker's non-nil one
// (setStop's "first error wins" rule, not first *call*).
func TestRun_DrainRacingWorkerErrorStillAlerts(t *testing.T) {
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	started := make(chan int64, 1)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &barrierSelfStealingHandler{started: started, release: release}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitFor(t, started, "handler to start")

	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	// Release the failing handler and signal the drain back to back, with
	// no sleep between them: Run's own select must then race its drainCh
	// case against its results case for real. Whichever it picks, the
	// assertions below must still hold.
	close(release)
	d.NotifyDrain()

	runErr := waitFor(t, runErrCh, "Run to return")
	if !errors.Is(runErr, dispatch.ErrFailClosed) {
		t.Errorf("Run err = %v, want errors.Is(err, dispatch.ErrFailClosed)", runErr)
	}

	logged := logBuf.String()
	alert1 := fmt.Sprintf("fail-closed on ticket %d: %s: ticket %d: the lease was lost", ticketID, dispatch.ErrFailClosed.Error(), ticketID)
	alert2 := fmt.Sprintf("dispatcher stopped after fail-closed on ticket %d", ticketID)
	if got := strings.Count(logged, alert1); got != 1 {
		t.Errorf("alert 1 (%q) appeared %d times, want exactly 1 (log: %s)", alert1, got, logged)
	}
	if got := strings.Count(logged, alert2); got != 1 {
		t.Errorf("alert 2 (%q) appeared %d times, want exactly 1 (log: %s)", alert2, got, logged)
	}
	if i1, i2 := strings.Index(logged, alert1), strings.Index(logged, alert2); i1 < 0 || i2 < 0 || i2 < i1 {
		t.Errorf("alerts out of order (alert1 at %d, alert2 at %d); log: %s", i1, i2, logged)
	}
}

// schemaInvalidHandler reserves a run and returns a commit whose one
// question message has options null, the #213 shape: the question schema
// rejects null options, so CommitHandlerResult's own insertMessageTx fails
// the commit and the dispatcher's own schema-invalid branch is what
// TestTick_SchemaInvalidCommitEscalatesAndKeepsDispatching exercises.
type schemaInvalidHandler struct{}

func (schemaInvalidHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rsv, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	outcome := "ok"
	exitCode := 0
	agentSeconds := 1
	ext := "ext-session"
	state := testQuestionOpen
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Session: &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &ext},
		Runs:    []store.Run{{ID: rsv.RunID, Turn: rsv.Turn, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Messages: []store.Message{{
			TicketID: t.ID, Type: testQuestionLiteral, Author: "zing", State: &state, Body: "Q1",
			Payload: json.RawMessage(`{"key":"Q1","kind":"question","state":"open","recommended":"a","options":null}`),
		}},
	}, nil
}

// TestTick_SchemaInvalidCommitEscalatesAndKeepsDispatching proves a commit
// that fails schema validation escalates its own ticket instead of
// stopping the dispatcher (#213's original failure mode, this time for any
// ticket, not just the one builder #213's own fix patched): with
// MaxParallel 1 and two queued tickets, A's handler (schemaInvalidHandler)
// reserves a run and returns a commit whose question payload fails the
// question schema; B's handler (spyHandler) is an ordinary queued-to-planning
// transition. The first Tick claims A (lower numeric TrackerRef sorts
// first), the schema failure escalates A rather than setting the stopped
// flag, and the second Tick claims and advances B.
func TestTick_SchemaInvalidCommitEscalatesAndKeepsDispatching(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	aID := seedQueuedTicket(t, s, "fake#1")
	bID := seedQueuedTicket(t, s, "fake#2")

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = &perTicketHandler{byTicket: map[int64]job.Handler{
		aID: schemaInvalidHandler{},
		bID: spy,
	}}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: err = %v, want nil", err)
	}

	a := getTicket(t, s, aID)
	if a.ClaimOwner != nil {
		t.Errorf("A's claim owner = %v, want nil", *a.ClaimOwner)
	}
	if a.WaitingOn == nil || *a.WaitingOn != testWaitingQuestions {
		t.Errorf("A's WaitingOn = %v, want %q", a.WaitingOn, testWaitingQuestions)
	}

	msgs, err := s.ListMessages(t.Context(), aID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var escalation *store.MessageRow
	for i := range msgs {
		if msgs[i].Type == testMsgTypeEscalation {
			escalation = &msgs[i]
		}
	}
	if escalation == nil {
		t.Fatal("no escalation message persisted for A")
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(escalation.Payload, &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
	}
	if payload.Code != string(response.EscalationCodePostRunFailed) {
		t.Errorf("escalation payload.Code = %q, want %q", payload.Code, response.EscalationCodePostRunFailed)
	}
	if !strings.Contains(payload.Tried, "payload does not match schema question") {
		t.Errorf("escalation payload.Tried = %q, want it to name the schema validation error", payload.Tried)
	}

	runs, err := s.RunsForTicket(t.Context(), aID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome == nil || *runs[0].Outcome != testOutcomeError {
		t.Fatalf("A's runs = %+v, want exactly 1 with outcome error", runs)
	}
	wantRunIDs := fmt.Sprintf("run ids: %d", runs[0].ID)
	if !strings.Contains(payload.Tried, wantRunIDs) {
		t.Errorf("escalation payload.Tried = %q, want it to contain %q", payload.Tried, wantRunIDs)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false")
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: err = %v, want nil", err)
	}
	if got := getTicket(t, s, bID).State; got != testStatePlanning {
		t.Errorf("B's state after the second Tick = %q, want %q", got, testStatePlanning)
	}
}

// schemaInvalidSelfStealingHandler combines selfStealingHandler's own
// concurrent-release simulation with schemaInvalidHandler's own
// schema-failing commit: it reserves a run, releases this exact ticket's
// own claim out from under itself (the same fenced no-op releaseClaim
// uses), then returns the options-null commit. The question schema check
// inside insertMessageTx runs well before the fenced ticket UPDATE
// (internal/store/commit.go), so CommitHandlerResult reports the schema
// failure first; only the dispatcher's own escalation attempt, retrying
// CommitHandlerResult a second time, ever reaches that lost fence.
type schemaInvalidSelfStealingHandler struct{}

func (schemaInvalidSelfStealingHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rsv, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, store.RunSeed{Model: testModelClaudeX})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := d.Store.CommitHandlerResult(ctx, store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}); err != nil {
		return store.HandlerCommit{}, err
	}
	outcome := "ok"
	exitCode := 0
	agentSeconds := 1
	ext := "ext-session"
	state := testQuestionOpen
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Session: &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &ext},
		Runs:    []store.Run{{ID: rsv.RunID, Turn: rsv.Turn, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Messages: []store.Message{{
			TicketID: t.ID, Type: testQuestionLiteral, Author: "zing", State: &state, Body: "Q1",
			Payload: json.RawMessage(`{"key":"Q1","kind":"question","state":"open","recommended":"a","options":null}`),
		}},
	}, nil
}

// TestTick_SchemaInvalidCommitWithLostLeaseFailsClosed proves the
// schema-invalid branch still falls through to today's fail-closed path
// when its own escalation attempt cannot apply: a concurrent release of
// this exact ticket's own claim (schemaInvalidSelfStealingHandler) means
// the escalation commit's fenced ticket UPDATE finds the lease already
// gone, so CommitHandlerResult reports applied=false for it, and the
// dispatcher reports the original schema error, wrapped in ErrFailClosed,
// rather than silently losing it.
func TestTick_SchemaInvalidCommitWithLostLeaseFailsClosed(t *testing.T) {
	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	reg := job.Registry()
	reg[testStatePlanning] = &schemaInvalidSelfStealingHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	err := d.Tick(t.Context())
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Fatalf("Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", err)
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Errorf("Tick: err = %v, want errors.Is(err, store.ErrSchemaInvalid)", err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "schema escalation not applied") {
		t.Errorf("log = %q, want the \"schema escalation not applied\" line, proving the escalation branch was reached", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("ticket_id=%d", ticketID)) {
		t.Errorf("log = %q, want ticket_id=%d", logged, ticketID)
	}
	if !strings.Contains(logged, "applied=false") {
		t.Errorf("log = %q, want applied=false", logged)
	}

	_, stopped, flagsErr := s.Flags(t.Context())
	if flagsErr != nil {
		t.Fatalf("Flags: %v", flagsErr)
	}
	if !stopped {
		t.Error("stopped flag = false, want true after fail-closed")
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.Type == testMsgTypeEscalation {
			t.Errorf("found an escalation message %+v, want none: the escalation commit must not have applied", m)
		}
	}

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != nil {
		t.Fatalf("runs = %+v, want exactly 1 with outcome still nil", runs)
	}
}

// unownedRunMessageHandler returns a commit with one "update" message whose
// RunID names a run this ticket does not own, so CommitHandlerResult's own
// runOwnedByTicketTx check fails with a plain error unrelated to schema
// validation (internal/store/commit.go).
type unownedRunMessageHandler struct{}

func (unownedRunMessageHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	unownedRunID := int64(999999)
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Messages: []store.Message{{TicketID: t.ID, RunID: &unownedRunID, Type: testMsgTypeUpdate, Author: "zing", Body: "x"}},
	}, nil
}

// TestTick_NonSchemaCommitErrorStillFailsClosed proves a commit error that
// has nothing to do with schema validation still fails the dispatcher
// closed, exactly as before this ticket's change: a message whose RunID
// names a run the ticket does not own fails runOwnedByTicketTx inside
// CommitHandlerResult with a plain error that never wraps
// store.ErrSchemaInvalid, so the new branch in runAndCommit must leave it
// alone and fall straight through to the existing fail-closed path.
func TestTick_NonSchemaCommitErrorStillFailsClosed(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	reg := job.Registry()
	reg[testStatePlanning] = &unownedRunMessageHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	err := d.Tick(t.Context())
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Fatalf("Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", err)
	}
	if errors.Is(err, store.ErrSchemaInvalid) {
		t.Errorf("Tick: err = %v, want errors.Is(err, store.ErrSchemaInvalid) = false", err)
	}

	_, stopped, flagsErr := s.Flags(t.Context())
	if flagsErr != nil {
		t.Fatalf("Flags: %v", flagsErr)
	}
	if !stopped {
		t.Error("stopped flag = false, want true after fail-closed")
	}
}
