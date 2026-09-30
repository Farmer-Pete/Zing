package job_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// The pipeline state names and the one fixture tracker_ref this package's
// tests share, named once so goconst has nothing to flag across job_test.go
// and skeleton_test.go (both package job_test).
const (
	testStateQueued    = "queued"
	testStatePlanning  = "planning"
	testStateBuilding  = "building"
	testStateReviewing = "reviewing"
	testStateJudging   = "judging"
	testStateShipping  = "shipping"
	testStateDone      = "done"
	testRefFake1       = "fake#1"

	testMsgTypeQuestion  = "question"
	testWaitingQuestions = "questions"
	testWaitingGate      = "gate"
	testAuthorZing       = "zing"
	testRuntimeClaude    = "claude"
	testRuntimeFake      = "fake"
	testRuntimeCodex     = "codex"

	// testNoopShellCmd is the always-succeeds shell command several
	// building tests give a project's TestCmd or LintCmd when the test
	// only cares that the command exits 0, not what it does.
	testNoopShellCmd = "true"
	// testCodeResponseInvalid is D14's own escalation code (design section
	// 5.4), shared across classify, planning, and perimeter invalid-output
	// tests.
	testCodeResponseInvalid = "response_invalid"
	// testCodeResumesExhausted and testOriginCapResumes are the
	// resumes_exhausted escalation's own code and origin (design D17,
	// section 6.9), shared across planning's and building's own cap tests.
	testCodeResumesExhausted = "resumes_exhausted"
	testOriginCapResumes     = "cap_resumes"

	testReasonPickedUp  = "picked up"
	testPlanningScript1 = "planning/1.xml"

	testArtifactTypePlan       = "plan"
	testArtifactTypeClaims     = "claims"
	testArtifactTypeScenario   = "scenario"
	testArtifactTypePlanreview = "planreview"

	testMsgTypeUpdate = "update"
	testKindBug       = "bug"
	testKindFeature   = "feature"
	testTicketTitle   = "Add a hello endpoint"
	testModelClaudeX  = "claude-x"
)

// testProject is the one project every test in this file seeds. LocalPath
// is filled in per test by seedQueuedTicket (testProjectDir): the ready
// entry point (design section 6.5) now opens it for real through
// os.OpenRoot to check a ready response's code claims, so it must be a real
// directory, not the placeholder "/tmp/zing" this var carried through
// task 6.
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

// newJobTestStore opens a fresh Store on a temp-file database, closed on
// test cleanup.
func newJobTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedQueuedTicket inserts a project and one queued ticket on it, under
// testRefFake1 (this file's one fixture tracker_ref), through the exported
// store API only (this file lives outside package store).
func seedQueuedTicket(t *testing.T, s *store.Store) int64 {
	t.Helper()
	ctx := t.Context()

	proj := testProject
	proj.LocalPath = testProjectDir(t)
	projectID, err := s.EnsureProject(ctx, proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// jobTestGitHub is a never-called orchestrator.GitHub, enough to satisfy
// orchestrator.New's required parameter: this package's own building tests
// never push or open a pull request.
type jobTestGitHub struct{}

func (jobTestGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errFakeGitHub
}

func (jobTestGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errFakeGitHub
}

func (jobTestGitHub) CreateDraftPR(context.Context, string, string, string, string, string, string) (prURL string, number int, err error) {
	return "", 0, errFakeGitHub
}

func (jobTestGitHub) FindPRByHead(context.Context, string, string, string, string) (prURL string, number int, ok bool, err error) {
	return "", 0, false, errFakeGitHub
}

var errFakeGitHub = errors.New("jobTestGitHub: not implemented")

// buildJobTestProjects returns a job.Project for every store project whose
// LocalPath is a real git repository: orchestrator.New never fails on a
// plain directory, but GitCommonDir does, so a project seeded through the
// ordinary testProjectDir (no .git) is silently left out, and only a
// project seeded through seedQueuedGitBackedTicket ever reaches the real
// building handler with what it needs (PKG8-PLAN.md section 4.3).
func buildJobTestProjects(t *testing.T, s *store.Store) map[int64]job.Project {
	t.Helper()
	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	out := make(map[int64]job.Project, len(projects))
	for _, p := range projects {
		orch, orchErr := orchestrator.New(
			orchestrator.Project{Owner: "fixture", Repo: "fixture", LocalPath: p.LocalPath, DefaultBranch: "main"},
			jobTestGitHub{}, orchestrator.NewRunner(), nil)
		if orchErr != nil {
			continue
		}
		repoGit, gitErr := orch.GitCommonDir(t.Context())
		if gitErr != nil {
			continue // not a git repository; never reached by a building test
		}
		out[p.ID] = job.Project{Orch: orch, RepoGit: repoGit, TestCmd: "test -f hello.txt", LintCmd: testNoopShellCmd}
	}
	return out
}

// seedQueuedGitBackedTicket is seedQueuedTicket, but on a real, signed
// gitfixture repository carrying readyClaimEvidencePath at HEAD
// (PKG8-PLAN.md section 9.4, 10): the one shape the real building handler
// needs to run EnsureWorktree, CommitTask, and SignedStatus for real.
func seedQueuedGitBackedTicket(t *testing.T, s *store.Store) int64 {
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
	projectID, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// testMachine loads the real, checked-in machine.toml, the same process
// definition zing serve loads: skeleton.go's building and planning handlers
// look up their runtime by d.Machine.Jobs[job].Runtime, so every Deps this
// file builds needs the real job-name-to-runtime-name mapping ("build" and
// "planning" both name "claude").
func testMachine(t *testing.T) *machine.Machine {
	t.Helper()
	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	return m
}

// testModels and testBudget are the job.Deps.Models and job.Deps.Budget
// classify and planning need to resolve a model alias and pass the
// agent-time budget check (design section 4.4, 4.6), now that planning.go
// (task 6) routes both through runJob. fakeRuntime never reads Model, so
// the exact ids do not matter beyond matching machine.toml's alias names.
var testModels = map[string]string{
	"sonnet":         "claude-sonnet-5",
	"opus":           "claude-opus-4-8",
	"fable":          "claude-fable-5-1",
	testRuntimeCodex: "gpt-5.5",
}

const testBudget = 240 * time.Minute

// testFloor is job.Deps.Floor's value in every claim() this package builds:
// zing.toml's own default review.floor ("minor", internal/config's
// applyConfigDefaults), so the review-tick tests (planning_test.go, task
// 7b) exercise the same floor a real deployment would, unless a test
// overrides it (claimWithFloor).
const testFloor = response.SeverityMinor

// claim claims ticketID for a fresh owner and a lease truncated to second
// precision (SQLite's TEXT timestamp round-trips at second precision), so
// the returned expires compares equal to what a later GetTicket reads
// back, and returns the Deps a handler test drives with. rt serves every
// machine.toml runtime name (design section 4.1, D2: selftest and e2e map
// claude, codex, and fake to one Fake), so a handler's
// d.Runtimes.For(d.Machine.Jobs[job].Runtime) lookup always resolves to rt
// regardless of which runtime name the real machine.toml gives that job.
// Models, Budget, and Reserve are wired exactly as
// dispatch.Dispatcher.runAndCommit wires them for a real Tick (design D13),
// so a handler test that calls classify or planning directly needs no
// separate setup of its own.
func claim(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) job.Deps {
	t.Helper()
	owner := "test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: rt, testRuntimeCodex: rt, testRuntimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return job.Deps{
		Store: s, Runtimes: set, Machine: testMachine(t), Models: testModels, Budget: testBudget, Floor: testFloor,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return s.Reserve(ctx, ticketID, owner, expires, su, seed)
		},
		Sandbox: sandbox.Off(), RequireSandbox: false,
		Commands: job.NewCommandRunner(sandbox.Off(), false),
		Projects: buildJobTestProjects(t, s),
	}
}

// apply validates commit against t (the ticket's state before the commit)
// and applies it, failing the test on any error or a refused (applied =
// false) commit.
func apply(t *testing.T, s *store.Store, ticket store.Ticket, commit store.HandlerCommit) {
	t.Helper()
	if err := job.ValidateCommit(ticket, commit); err != nil {
		t.Fatalf("ValidateCommit: %v", err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
}

// fakeRuntime returns a *runtime.Fake serving the real, checked-in
// fixtures/scripts tree (fixtures/scripts/planning/1.xml,
// fixtures/scripts/build/1/1.xml), the same tree zing serve wires up.
func fakeRuntime(t *testing.T) *runtime.Fake {
	t.Helper()
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	return runtime.NewFake(scriptsFS)
}

// getTicket is a small GetTicket wrapper so call sites read as one line.
func getTicket(t *testing.T, s *store.Store, ticketID int64) store.Ticket {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return ticket
}

// TestRing_QueuedToDoneAnsweringOneQuestion drives every one of the six
// skeleton handlers, in pipeline order, against a real temp store and the
// real checked-in fixture scripts, applying each returned commit and
// reclaiming between states exactly as the dispatcher will (design section
// 6.8). Planning now takes four handler calls (design section 5.1, task
// 7b): a kindless ticket classifies first (fixtures/scripts/classify/1.xml,
// no transition, no state message), the first turn posts the one fixture
// question and waits, this test answers it through store.AnswerQuestion
// exactly as the console's POST /answer would, the resume stores the ready
// cohort (fixtures/scripts/planning/2.xml) and stays in planning (task 7b
// removed the old shortcut), and the review tick
// (fixtures/scripts/planreview/1.xml, zero findings) takes the TEMPORARY
// clean shortcut to building. It asserts the ticket reaches done and that a
// state message was written on every one of the six transitions (design
// section 6.3, 7.1) -- planning's own classify, first-entry, and ready
// calls write no state message, since none of them carries a Next; only the
// review tick's clean shortcut does.
func TestRing_QueuedToDoneAnsweringOneQuestion(t *testing.T) {
	s := newJobTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedGitBackedTicket(t, s)
	reg := job.Registry()

	// queued -> planning.
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, rt, ticketID)
	commit, err := reg[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued handler.Run: %v", err)
	}
	apply(t, s, ticket, commit)

	// planning: classify (kind unset, stays planning, no message).
	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, rt, ticketID)
	commit, err = reg[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning classify handler.Run: %v", err)
	}
	apply(t, s, ticket, commit)

	classified := getTicket(t, s, ticketID)
	if classified.State != testStatePlanning || classified.WaitingOn != nil {
		t.Fatalf("after classify: ticket = (state=%q, waiting_on=%v), want (planning, nil)", classified.State, classified.WaitingOn)
	}
	if classified.Kind == nil {
		t.Fatal("after classify: ticket.Kind is nil, want bug or feature")
	}

	// planning first entry: posts the question and waits.
	ticket = getTicket(t, s, ticketID)
	deps = claim(t, s, rt, ticketID)
	commit, err = reg[testStatePlanning].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("planning first-entry handler.Run: %v", err)
	}
	apply(t, s, ticket, commit)

	waiting := getTicket(t, s, ticketID)
	if waiting.State != testStatePlanning {
		t.Fatalf("after first entry: ticket state = %q, want unchanged planning", waiting.State)
	}
	if waiting.WaitingOn == nil || *waiting.WaitingOn != testWaitingQuestions {
		t.Fatalf("after first entry: ticket waiting_on = %v, want questions", waiting.WaitingOn)
	}

	// Answer the one fixture question, exactly the console's POST /answer path.
	answerFixtureQuestion(t, s, ticketID)

	answered := getTicket(t, s, ticketID)
	if answered.WaitingOn != nil {
		t.Fatalf("after answering: ticket waiting_on = %v, want nil (wait cleared)", answered.WaitingOn)
	}

	// planning resume (stores the cohort, stays in planning) and the review
	// tick (clean, the temporary shortcut to building) take two more handler
	// calls, reusing rt so the review tick's own runtime.For("codex") lookup
	// resolves to the same Fake, registered under all three runtime names
	// (claim's own doc comment).
	advancePlanningWithAnAnswer(t, s, rt, ticketID)

	// building: the real handler (design section 6) takes a RUN call and a
	// CHECK-then-LAND call per fixture task, not the skeleton's one-shot
	// fake build.
	ticket = getTicket(t, s, ticketID)
	if ticket.State != testStateBuilding {
		t.Fatalf("before building: ticket state = %q, want %q", ticket.State, testStateBuilding)
	}
	advanceBuilding(t, s, rt, ticketID)

	// the remaining code-only states.
	order := []string{testStateReviewing, testStateJudging, testStateShipping}
	for _, state := range order {
		ticket = getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("before handler %s: ticket state = %q, want %q", state, ticket.State, state)
		}
		deps = claim(t, s, rt, ticketID)

		handler, ok := reg[state]
		if !ok {
			t.Fatalf("Registry() has no handler for state %s", state)
		}
		commit, err = handler.Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("%s handler.Run: %v", state, err)
		}
		apply(t, s, ticket, commit)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateDone {
		t.Errorf("final ticket state = %q, want done", final.State)
	}
	if final.WaitingOn != nil {
		t.Errorf("final ticket waiting_on = %v, want nil", *final.WaitingOn)
	}
	if final.ClaimOwner != nil {
		t.Error("final ticket claim was not cleared")
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var stateMsgs int
	var questionMsgs int
	for _, m := range msgs {
		switch m.Type {
		case "state":
			stateMsgs++
		case testMsgTypeQuestion:
			questionMsgs++
		}
	}
	// queued->planning, planning->building, building->reviewing,
	// reviewing->judging, judging->shipping, shipping->done: six transitions,
	// six state messages. Planning's first-entry call carries no Next and
	// writes none.
	const wantStateMsgs = 6
	if stateMsgs != wantStateMsgs {
		t.Errorf("state messages = %d, want %d (one per transition)", stateMsgs, wantStateMsgs)
	}
	// The fixture Q1 plus the gate (design section 6.6, task 7c).
	const wantQuestionMsgs = 2
	if questionMsgs != wantQuestionMsgs {
		t.Errorf("question messages = %d, want %d", questionMsgs, wantQuestionMsgs)
	}
}

// answerFixtureQuestionOption is the option every caller of
// answerFixtureQuestion answers with: fixtures/scripts/planning/1.xml's own
// "b" ("hello, world"), the option every test in this file and
// planning_test.go exercises.
const answerFixtureQuestionOption = "b"

// answerFixtureQuestion answers ticketID's one open question with
// answerFixtureQuestionOption, through store.AnswerQuestion, and fails the
// test if the answer is not accepted.
func answerFixtureQuestion(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) == 0 {
		t.Fatal("QuestionsByState(open) = no open questions, want at least one")
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{
		TicketID: ticketID, QuestionID: open[0].ID, Option: answerFixtureQuestionOption,
	})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

// TestQueuedHandler_TransitionsToPlanning is a focused unit-level check of
// queuedHandler's commit shape (design section 6.5).
func TestQueuedHandler_TransitionsToPlanning(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)

	commit, err := job.Registry()[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStatePlanning || commit.Reason != testReasonPickedUp {
		t.Errorf("commit = (Next=%q, Reason=%q), want (planning, picked up)", commit.Next, commit.Reason)
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil", *commit.Waiting)
	}
}

// advanceQueuedToPlanning claims ticketID, runs the queued handler, and
// applies its commit, leaving the ticket sitting in planning.
func advanceQueuedToPlanning(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateQueued].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued Run: %v", err)
	}
	apply(t, s, ticket, commit)
}

// advanceThroughStates runs the handler registered for each of states, in
// order, applying every commit, so a test can arrange a ticket already
// sitting in the state right after the last one named (each named state
// gets its own fresh runtime.Fake, since none of the skeleton handlers
// resumes a prior handler's session). Planning is a special case (design
// section 5.1): a kindless ticket classifies before it ever opens a
// planning session, so this drives the planning handler in a loop -- see
// advancePlanningWithAnAnswer -- landing the ticket in building exactly as
// every other state's single silent transition does.
func advanceThroughStates(t *testing.T, s *store.Store, ticketID int64, states ...string) {
	t.Helper()
	reg := job.Registry()
	for _, state := range states {
		ticket := getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("advanceThroughStates(%s): ticket state = %q, want %q", state, ticket.State, state)
		}

		if state == testStatePlanning {
			advancePlanningWithAnAnswer(t, s, fakeRuntime(t), ticketID)
			continue
		}
		if state == testStateBuilding {
			advanceBuilding(t, s, fakeRuntime(t), ticketID)
			continue
		}

		deps := claim(t, s, fakeRuntime(t), ticketID)
		commit, err := reg[state].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("%s handler.Run: %v", state, err)
		}
		apply(t, s, ticket, commit)
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
	reg := job.Registry()
	for range advanceBuildingMaxCalls {
		ticket := getTicket(t, s, ticketID)
		deps := claim(t, s, rt, ticketID)
		commit, err := reg[testStateBuilding].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building Run: %v", err)
		}
		apply(t, s, ticket, commit)
		if getTicket(t, s, ticketID).State != testStateBuilding {
			return
		}
	}
	t.Fatalf("advanceBuilding: still in building after %d handler calls", advanceBuildingMaxCalls)
}

// advancePlanningMaxCalls bounds advancePlanningWithAnAnswer's own
// handler-call loop: classify (kind unset, stays planning), the first turn
// (posts questions, waits), the resume (stores the ready cohort, stays
// planning), the review tick (clean, posts the gate, task 7c), and the
// owner's approve (seals the cohort and moves to building) is five calls;
// the headroom catches a stuck handler instead of hanging the test.
const advancePlanningMaxCalls = 6

// advancePlanningWithAnAnswer drives the real planning handler through as
// many calls as it now takes to reach building (design section 5.1):
// classify runs first on a kindless ticket and sets kind but carries no
// transition, so this loops the handler -- sharing one runtime.Runtime
// throughout, since a resume (and, once the cohort is stored, the review
// tick) must reuse the session an earlier call minted -- until either the
// ticket leaves planning, waits on "questions" (answered through
// store.AnswerQuestion exactly as the console's POST /answer would), or
// waits on "gate" (approved the same way, design section 6.6, task 7c), and
// keeps looping either way. rt is the caller's own runtime, not a fresh one
// this helper mints, so a caller that already drove classify or the first
// turn against a particular runtime.Fake can keep using it here.
func advancePlanningWithAnAnswer(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	reg := job.Registry()

	for range advancePlanningMaxCalls {
		ticket := getTicket(t, s, ticketID)
		deps := claim(t, s, rt, ticketID)
		commit, err := reg[testStatePlanning].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("planning Run: %v", err)
		}
		apply(t, s, ticket, commit)

		after := getTicket(t, s, ticketID)
		if after.State != testStatePlanning {
			return
		}
		if after.WaitingOn != nil && *after.WaitingOn == testWaitingQuestions {
			answerFixtureQuestion(t, s, ticketID)
			continue
		}
		if after.WaitingOn != nil && *after.WaitingOn == testWaitingGate {
			answerGateApprove(t, s, ticketID)
			continue
		}
		if after.WaitingOn != nil {
			t.Fatalf("advancePlanningWithAnAnswer: ticket waiting_on = %q, want questions, gate, or nil", *after.WaitingOn)
		}
	}
	t.Fatalf("advancePlanningWithAnAnswer: still in planning after %d handler calls", advancePlanningMaxCalls)
}

// answerGateApprove answers ticketID's one open gate question with option
// "a" (Approve, design section 6.6, D8), through store.AnswerQuestion
// exactly as the console's POST /answer would.
func answerGateApprove(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %d questions, want exactly 1 (the gate)", len(open))
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{
		TicketID: ticketID, QuestionID: open[0].ID, Option: "a",
	})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

// TestReviewingHandler_TransitionsToJudging, TestJudgingHandler_TransitionsToShipping,
// and TestShippingHandler_TransitionsToDone cover the three remaining
// code-only handlers (design section 6.5).

func TestReviewingHandler_TransitionsToJudging(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedGitBackedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning, testStateBuilding)

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateReviewing].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStateJudging || commit.Reason != "review clean" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (judging, review clean)", commit.Next, commit.Reason)
	}
	apply(t, s, ticket, commit)
	if final := getTicket(t, s, ticketID); final.State != testStateJudging {
		t.Errorf("final ticket state = %q, want judging", final.State)
	}
}

func TestJudgingHandler_TransitionsToShipping(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedGitBackedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing)

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateJudging].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStateShipping || commit.Reason != "judge passed" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (shipping, judge passed)", commit.Next, commit.Reason)
	}
	apply(t, s, ticket, commit)
	if final := getTicket(t, s, ticketID); final.State != testStateShipping {
		t.Errorf("final ticket state = %q, want shipping", final.State)
	}
}

func TestShippingHandler_TransitionsToDone(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedGitBackedTicket(t, s)
	advanceThroughStates(t, s, ticketID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging)

	ticket := getTicket(t, s, ticketID)
	deps := claim(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateShipping].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != testStateDone || commit.Reason != "shipped" {
		t.Errorf("commit = (Next=%q, Reason=%q), want (done, shipped)", commit.Next, commit.Reason)
	}
	apply(t, s, ticket, commit)
	if final := getTicket(t, s, ticketID); final.State != testStateDone {
		t.Errorf("final ticket state = %q, want done", final.State)
	}
}
