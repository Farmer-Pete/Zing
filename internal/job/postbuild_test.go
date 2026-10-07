// postbuild_test.go tests task 3: postBuildPrelude, unitInFlight, and the
// unitEscalation conversions of resumeBuildRound, retryFreshRun,
// retryCapResumesBuild, retryCapResumesPerimeter, and resolvePerimeterQuestion
// (design section 5.4 change 3, 5.5, 5.6, #28 gap 3). postBuildPrelude and
// unitInFlight are both unexported, so, like fix_internal_test.go and
// building_internal_test.go, this file lives in package job rather than
// alongside fix_test.go and building_test.go (package job_test): their own
// richer fixtures (newJobTestStore, claim, scriptedRuntime, buildTicketInBuilding)
// live in that other Go package and are not reachable from here, so this file
// builds its own small equivalents, reusing the real checked-in fixture
// scripts (fixtures/scripts/planning, fixtures/scripts/build) and real git
// (gitfixture) exactly as job_test's own do.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/gitfixture"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// ---- harness: store, projects, deps, apply (mirrors skeleton_test.go's own
// newJobTestStore/claim/apply/fakeRuntime/getTicket, package job_test,
// unreachable from here) ------------------------------------------------

const (
	pbRefFake1               = "fake#1"
	pbTicketTitle            = "Add a hello endpoint"
	pbWaitingQuestions       = "questions"
	pbWaitingGate            = "gate"
	pbReadyClaimEvidencePath = "cmd/zing/main.go"
	pbHelloTxt               = "hello.txt"
	pbNoopShellCmd           = "true"
	// pbFixTestCmd rewrites hello.txt for real (fix_test.go's own
	// fixTestCmd technique, package job_test): a scriptedRuntime's own
	// buildStep carries no fake-runtime tree effect (design section 9.3),
	// so CHECK's own re-run of the project's test command is what a fix
	// scenario's "hello.txt changed" claim needs satisfied for real.
	pbFixTestCmd = "printf 'hello, world\\n' > hello.txt && test -f hello.txt"
	// pbExtraPath and pbExtraReason are the single-extra perimeter scenario's
	// own undeclared path and builder reason (building_test.go's own
	// testExtraPath/testExtraReason, package job_test, unreachable from here).
	pbExtraPath   = "extra1.go"
	pbExtraReason = "needed a helper"

	// pbProjectName and pbRepoURL are this file's own store project fixture
	// fields, named once so goconst has nothing to flag: fix_internal_test.go
	// and runjob_test.go (also package job) already repeat "zing"/
	// "github.com/x/zing" for their own unrelated fixtures.
	pbProjectName = "postbuild-fixture"
	pbRepoURL     = "https://example.test/postbuild-fixture"

	// pbRuntimeClaude, pbRuntimeCodex, and pbModelClaudeX name this file's
	// own runtime and model fixtures, for the same reason.
	pbRuntimeClaude = "claude"
	pbRuntimeCodex  = "codex"
	pbModelClaudeX  = "claude-x"
)

var pbModels = map[string]string{
	testModelAlias: "claude-sonnet-5", testModelAliasOpus: "claude-opus-5-5", testModelAliasFable: "claude-fable-5-1", pbRuntimeCodex: "gpt-5.5",
}

const pbBudget = 240 * time.Minute

const pbFloor = response.SeverityMinor

// pbGitHub is a never-called orchestrator.GitHub, enough to satisfy
// orchestrator.New's required parameter (skeleton_test.go's jobTestGitHub,
// package job_test, unreachable from here).
type pbGitHub struct{}

func (pbGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errPbGitHub
}

func (pbGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errPbGitHub
}

func (pbGitHub) CreateDraftPR(context.Context, string, string, string, string, string, string) (prURL string, number int, err error) {
	return "", 0, errPbGitHub
}

func (pbGitHub) FindPRByHead(context.Context, string, string, string, string) (prURL string, number int, ok bool, err error) {
	return "", 0, false, errPbGitHub
}

var errPbGitHub = errors.New("pbGitHub: not implemented")

// testTrackerGithub is the store's only legal projects.tracker value
// (migrations/0001_init.sql's own CHECK constraint): fix_internal_test.go,
// runjob_test.go, and this file (all package job) each seed a store project,
// so it is named once here, shared by all three, rather than repeated as a
// literal (goconst).
const testTrackerGithub = "github"

// newPostbuildTestStore opens a fresh Store on a temp-file database, closed
// on test cleanup.
func newPostbuildTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// pbFixtureOwner and pbFixtureDefaultBranch are this file's own
// orchestrator.Project fixture fields (goconst, once rather than repeated
// at every orchestrator.New call site in this package, package job):
// building a fake GitHub owner/repo pair and a "main" default branch is
// never read back by anything a test asserts on.
const (
	pbFixtureOwner         = "fixture"
	pbFixtureDefaultBranch = "main"
)

// pbOrchestratorFor builds an *orchestrator.Orchestrator over localPath
// under run (judging_test.go's own TestJudgeWorktreeRemoveFailureLogged
// passes a Runner that selectively fails, in place of pbBuildProjects' own
// orchestrator.NewRunner()), and reads back its common git dir. ok is false
// when localPath is not a real git repository (orchestrator.New never fails
// on a plain directory, but GitCommonDir does), the same silent skip
// pbBuildProjects already gave a non-git-backed project.
func pbOrchestratorFor(t *testing.T, localPath string, run orchestrator.Runner) (orch *orchestrator.Orchestrator, repoGit string, ok bool) {
	t.Helper()
	orch, orchErr := orchestrator.New(
		orchestrator.Project{Owner: pbFixtureOwner, Repo: pbFixtureOwner, LocalPath: localPath, DefaultBranch: pbFixtureDefaultBranch},
		pbGitHub{}, run, nil)
	if orchErr != nil {
		return nil, "", false
	}
	repoGit, ok = pbGitCommonDir(t, orch, localPath)
	if !ok {
		return nil, "", false
	}
	return orch, repoGit, true
}

// pbCommonDirs caches each fixture repository's common git dir by its
// LocalPath. Every pbClaim and shipClaim rebuilds its Projects, and a
// shipping test claims dozens of times, so asking git each time cost
// thousands of process spawns per run, which the race detector makes
// slow. A fixture repository never moves, and every LocalPath is its own
// test's own temp dir, so the answer never changes for a given key.
var pbCommonDirs sync.Map

// pbGitCommonDir returns orch.GitCommonDir for localPath, cached in
// pbCommonDirs. ok is false, and nothing is cached, when localPath is not
// a git repository.
func pbGitCommonDir(t *testing.T, orch *orchestrator.Orchestrator, localPath string) (repoGit string, ok bool) {
	t.Helper()
	if cached, hit := pbCommonDirs.Load(localPath); hit {
		repoGit, ok = cached.(string)
		return repoGit, ok
	}
	repoGit, err := orch.GitCommonDir(t.Context())
	if err != nil {
		return "", false
	}
	pbCommonDirs.Store(localPath, repoGit)
	return repoGit, true
}

// pbBuildProjects returns a job.Project for every store project whose
// LocalPath is a real git repository (skeleton_test.go's own
// buildJobTestProjects, package job_test, unreachable from here).
func pbBuildProjects(t *testing.T, s *store.Store) map[int64]Project {
	t.Helper()
	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	out := make(map[int64]Project, len(projects))
	for _, p := range projects {
		orch, repoGit, ok := pbOrchestratorFor(t, p.LocalPath, orchestrator.NewRunner())
		if !ok {
			continue
		}
		out[p.ID] = Project{Orch: orch, RepoGit: repoGit, TestCmd: "test -f " + pbHelloTxt, LintCmd: pbNoopShellCmd}
	}
	return out
}

// pbSeedQueuedGitBackedTicket seeds a project and one queued ticket on a
// real, signed gitfixture repository (skeleton_test.go's own
// seedQueuedGitBackedTicket, package job_test, unreachable from here).
func pbSeedQueuedGitBackedTicket(t *testing.T, s *store.Store) int64 {
	t.Helper()
	dir := t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), dir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	if err := gitfixture.AddFile(t.Context(), dir, pbReadyClaimEvidencePath, []byte("package main\n")); err != nil {
		t.Fatalf("gitfixture.AddFile: %v", err)
	}

	projectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: pbProjectName, RepoURL: pbRepoURL, Tracker: testTrackerGithub, LocalPath: dir,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: pbRefFake1, Title: pbTicketTitle, State: stateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// pbSeedTicketInState inserts a queued ticket, then moves it straight to
// state through CommitHandlerResult directly (bypassing job.ValidateCommit's
// own legal-edge check, the same test-only shortcut escalateDirect and
// reserveTerminalRun take): every ticket starts queued (InsertTicket's own
// rule), and a pure escalation-resolution test needs no project, worktree,
// or plan to reach a post-build state.
func pbSeedTicketInState(t *testing.T, s *store.Store, state string) store.Ticket {
	t.Helper()
	projectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: pbProjectName, RepoURL: pbRepoURL, Tracker: testTrackerGithub, LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: pbRefFake1, Title: pbTicketTitle, State: stateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	owner := "seed-ticket-state-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, claimErr := s.Claim(t.Context(), ticketID, owner, expires)
	if claimErr != nil || !claimed {
		t.Fatalf("pbSeedTicketInState: claim: claimed=%v err=%v", claimed, claimErr)
	}
	applied, commitErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires, Next: state, Reason: "test setup",
	})
	if commitErr != nil || !applied {
		t.Fatalf("pbSeedTicketInState: CommitHandlerResult: applied=%v err=%v", applied, commitErr)
	}
	return pbGetTicket(t, s, ticketID)
}

// pbMachine loads the real, checked-in machine.toml (skeleton_test.go's own
// testMachine, package job_test, unreachable from here).
func pbMachine(t *testing.T) *machine.Machine {
	t.Helper()
	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	return m
}

// pbFakeRuntime returns a *runtime.Fake serving the real, checked-in
// fixtures/scripts tree (skeleton_test.go's own fakeRuntime, package
// job_test, unreachable from here).
func pbFakeRuntime(t *testing.T) *runtime.Fake {
	t.Helper()
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	return runtime.NewFake(scriptsFS)
}

// pbClaim claims ticketID for a fresh owner and returns the Deps a handler
// test drives with (skeleton_test.go's own claim, package job_test,
// unreachable from here).
func pbClaim(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) Deps {
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

	set, err := runtime.NewSet(map[string]runtime.Runtime{pbRuntimeClaude: rt, pbRuntimeCodex: rt, "fake": rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return Deps{
		Store: s, Runtimes: set, Machine: pbMachine(t), Models: pbModels, Budget: pbBudget, Floor: pbFloor,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return s.Reserve(ctx, ticketID, owner, expires, su, seed)
		},
		Sandboxes: sandbox.OffSet(), RequireSandbox: false,
		Commands: NewCommandRunner(sandbox.Off(), false),
		Projects: pbBuildProjects(t, s),
		DataDir:  t.TempDir(),
		// LensesParallel bounds ROUND's own semaphore (PKG9-PLAN.md section
		// 4.3, 6.2): the config default (7), so a review round test runs
		// every lens without a test needing to set this itself; a test that
		// exercises the bound overrides the returned Deps' own field.
		LensesParallel: 7,
	}
}

// pbGetTicket is a small GetTicket wrapper (skeleton_test.go's own
// getTicket, package job_test, unreachable from here).
func pbGetTicket(t *testing.T, s *store.Store, ticketID int64) store.Ticket {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return ticket
}

// pbApply validates commit against ticket and applies it (skeleton_test.go's
// own apply, package job_test, unreachable from here).
func pbApply(t *testing.T, s *store.Store, ticket store.Ticket, commit store.HandlerCommit) {
	t.Helper()
	if err := ValidateCommit(ticket, commit); err != nil {
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

// pbWithTestCmd returns deps with ticket's own project's TestCmd overridden,
// LintCmd a no-op (fix_test.go's own withFixTestCmd, package job_test,
// unreachable from here).
func pbWithTestCmd(deps Deps, ticket store.Ticket, testCmd string) Deps {
	proj := deps.Projects[ticket.ProjectID]
	proj.TestCmd = testCmd
	proj.LintCmd = pbNoopShellCmd
	deps.Projects = map[int64]Project{ticket.ProjectID: proj}
	return deps
}

// ---- harness: advancing a ticket to "reviewing" through the real planning
// and building handlers (skeleton_test.go's own advanceQueuedToPlanning,
// advancePlanningWithAnAnswer, advanceBuilding, package job_test,
// unreachable from here) --------------------------------------------------

func pbAnswerFixtureQuestion(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) == 0 {
		t.Fatal("QuestionsByState(open) = no open questions, want at least one")
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "b"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

func pbAnswerGateApprove(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %d questions, want exactly 1 (the gate)", len(open))
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

func pbAdvanceQueuedToPlanning(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	ticket := pbGetTicket(t, s, ticketID)
	deps := pbClaim(t, s, rt, ticketID)
	commit, err := (queuedHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("queued Run: %v", err)
	}
	pbApply(t, s, ticket, commit)
}

const pbAdvancePlanningMaxCalls = 6

func pbAdvancePlanningWithAnAnswer(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	for range pbAdvancePlanningMaxCalls {
		ticket := pbGetTicket(t, s, ticketID)
		deps := pbClaim(t, s, rt, ticketID)
		commit, err := (planningHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("planning Run: %v", err)
		}
		pbApply(t, s, ticket, commit)

		after := pbGetTicket(t, s, ticketID)
		if after.State != statePlanning {
			return
		}
		if after.WaitingOn != nil && *after.WaitingOn == pbWaitingQuestions {
			pbAnswerFixtureQuestion(t, s, ticketID)
			continue
		}
		if after.WaitingOn != nil && *after.WaitingOn == pbWaitingGate {
			pbAnswerGateApprove(t, s, ticketID)
			continue
		}
		if after.WaitingOn != nil {
			t.Fatalf("pbAdvancePlanningWithAnAnswer: waiting_on = %q, want questions, gate, or nil", *after.WaitingOn)
		}
	}
	t.Fatalf("pbAdvancePlanningWithAnAnswer: still in planning after %d calls", pbAdvancePlanningMaxCalls)
}

const pbAdvanceBuildingMaxCalls = 8

func pbAdvanceBuilding(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	for range pbAdvanceBuildingMaxCalls {
		ticket := pbGetTicket(t, s, ticketID)
		deps := pbClaim(t, s, rt, ticketID)
		commit, err := (buildingHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("building Run: %v", err)
		}
		pbApply(t, s, ticket, commit)
		if pbGetTicket(t, s, ticketID).State != stateBuilding {
			return
		}
	}
	t.Fatalf("pbAdvanceBuilding: still in building after %d calls", pbAdvanceBuildingMaxCalls)
}

// pbTicketInReviewing drives a fresh, git-backed ticket from queued through
// planning and a real three-task build into "reviewing", with a stored plan
// and a real worktree the fix driver can use (the heavy end of this file's
// own harness, mirroring job_test's own buildTicketInBuilding +
// advanceBuilding). It is pbTicketInReviewingWith(t, pbFakeRuntime(t)).
func pbTicketInReviewing(t *testing.T) (s *store.Store, ticketID int64) {
	t.Helper()
	return pbTicketInReviewingWith(t, pbFakeRuntime(t))
}

// pbTicketInReviewingWith is pbTicketInReviewing with rt in place of
// pbFakeRuntime(t): a caller that needs the planning or build turns to
// read from its own scripted fs.FS (#49 task 3's judgeHostTicketReady, a
// planning fixture with a host-kind scenario) drives the same path with
// that runtime instead.
func pbTicketInReviewingWith(t *testing.T, rt runtime.Runtime) (s *store.Store, ticketID int64) {
	t.Helper()
	s = newPostbuildTestStore(t)
	ticketID = pbSeedQueuedGitBackedTicket(t, s)
	pbAdvanceQueuedToPlanning(t, s, rt, ticketID)
	pbAdvancePlanningWithAnAnswer(t, s, rt, ticketID)
	pbAdvanceBuilding(t, s, rt, ticketID)
	if ticket := pbGetTicket(t, s, ticketID); ticket.State != stateReviewing {
		t.Fatalf("pbTicketInReviewingWith: ticket state = %q, want reviewing", ticket.State)
	}
	return s, ticketID
}

// ---- harness: a scripted runtime and canned responses (planning_test.go's
// own scriptedRuntime/scriptedStep/buildStep/questionResult, package
// job_test, unreachable from here) ----------------------------------------

type pbScriptedStep struct {
	res runtime.RunResult
	err error
}

type pbScriptedRuntime struct {
	t     *testing.T
	steps []pbScriptedStep
	calls int
	reqs  []runtime.RunRequest
}

func (s *pbScriptedRuntime) Run(_ context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	s.t.Helper()
	s.reqs = append(s.reqs, req)
	if s.calls >= len(s.steps) {
		s.t.Fatalf("pbScriptedRuntime: call %d has no scripted step (only %d scripted)", s.calls+1, len(s.steps))
	}
	step := s.steps[s.calls]
	s.calls++
	return step.res, step.err
}

// pbBuildStep builds a scriptedStep whose Response is a minimal, valid
// BuildResponse claiming a clean test and lint exit (0): this file's own
// scenarios never need a failing claim.
func pbBuildStep(filesChanged []string, extras []response.ExtraClaim, sessionID string) pbScriptedStep {
	return pbScriptedStep{res: runtime.RunResult{
		Response: &response.BuildResponse{
			Job: response.JobBuild, Outcome: response.OutcomeOk,
			Claims: response.BuildClaims{FilesChanged: filesChanged},
			Extras: extras, Report: "did something",
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

func pbQuestionResult(forJob response.Job, sessionID string) pbScriptedStep {
	return pbScriptedStep{res: runtime.RunResult{
		Response: &response.QuestionResponse{
			Job: forJob, Outcome: response.OutcomeQuestion,
			Questions: []response.Question{{
				Key: "q1", Title: "A question", Body: "Body.",
				Options:     []response.Option{{Key: "a", Text: "Option A"}, {Key: "b", Text: "Option B"}},
				Recommended: "a",
			}},
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

func pbPerimeterStep(reason, sessionID string) pbScriptedStep {
	return pbScriptedStep{res: runtime.RunResult{
		Response:  &response.PerimeterResponse{Job: response.JobPerimeter, Outcome: response.OutcomeOk, Reason: reason},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// pbFindOpenQuestionByKind finds ticketID's one open question of kind
// (building_test.go's own findOpenQuestionByKind, package job_test,
// unreachable from here).
func pbFindOpenQuestionByKind(t *testing.T, s *store.Store, ticketID int64, kind response.QuestionKind) store.MessageRow {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	for i := range open {
		var payload response.QuestionPayload
		if json.Unmarshal(open[i].Payload, &payload) == nil && payload.Kind == kind {
			return open[i]
		}
	}
	t.Fatalf("no open question of kind %s found", kind)
	return store.MessageRow{}
}

// pbAnswerPerimeterQuestion drives the real console draft/send path
// (building_test.go's own answerPerimeterQuestion, package job_test,
// unreachable from here).
func pbAnswerPerimeterQuestion(t *testing.T, s *store.Store, ticketID, questionID int64, decisions map[string]response.Decision) {
	t.Helper()
	for ref, d := range decisions {
		if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Item: &store.ItemDecision{Ref: ref, Decision: d}}); err != nil {
			t.Fatalf("SaveDraft(item %s): %v", ref, err)
		}
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
}

// pbFixText is the fix request text every pbOpenFixRequest call in this file
// uses: none of this task's scenarios care what it says, only that a fix
// unit of kind ci_log is open.
const pbFixText = "log tail"

// pbOpenFixRequest writes a "fix requested" marker for ticketID with the
// real watermark (Store.MaxRunID, design D18): after a real build, earlier
// task sessions already carry run ids, so a fixed watermark of 0 would make
// SessionAfter mistake one of their own sessions for the fix's own. Returns
// the marker's own message id (FixRequest.MessageID).
func pbOpenFixRequest(t *testing.T, s *store.Store, ticketID int64) int64 {
	t.Helper()
	ticket := pbGetTicket(t, s, ticketID)
	after, err := s.MaxRunID(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	msg, err := fixRequestMessage(ticket, FixKindCILog, pbFixText, after)
	if err != nil {
		t.Fatalf("fixRequestMessage: %v", err)
	}
	mid, err := s.InsertMessage(t.Context(), msg)
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	return mid
}

// pbRunPrelude runs postBuildPrelude once against deps built for ticketID,
// applying the commit when handled.
func pbRunPrelude(t *testing.T, s *store.Store, deps Deps, ticketID int64) (store.HandlerCommit, bool) {
	t.Helper()
	ticket := pbGetTicket(t, s, ticketID)
	commit, handled, err := postBuildPrelude(t.Context(), ticket, deps, response.EscalationOriginFix)
	if err != nil {
		t.Fatalf("postBuildPrelude: %v", err)
	}
	if handled {
		pbApply(t, s, ticket, commit)
	}
	return commit, handled
}

// ---- TestUnitInFlightByState -----------------------------------------

// TestUnitInFlightByState proves unitInFlight's own table (design section
// 5.4 change 3): in "building", the plan's lowest unlanded task; in
// "reviewing", the open fix request's own unit; with neither, ok is false.
func TestUnitInFlightByState(t *testing.T) {
	t.Parallel()
	plan := response.Plan{Delivery: response.Delivery{Tasks: []response.Task{
		{N: 1, Test: "t1", Text: "do a"},
		{N: 2, Test: "t2", Text: "do b"},
	}}}

	t.Run("building", func(t *testing.T) {
		t.Parallel()
		sha := "a"
		reports := []store.BuildReportRow{{Report: response.BuildReport{TaskN: 1, CommitSHA: &sha}}}
		ticket := store.Ticket{ID: 1, State: stateBuilding}
		u, ok, err := unitInFlight(t.Context(), ticket, Deps{}, plan, reports)
		if err != nil {
			t.Fatalf("unitInFlight: %v", err)
		}
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if u.TaskN != 2 {
			t.Errorf("u.TaskN = %d, want 2 (task 1 already landed)", u.TaskN)
		}
	})

	t.Run("reviewing", func(t *testing.T) {
		t.Parallel()
		s := newFixTestStore(t)
		ticket := seedFixTestTicket(t, s)
		ticket.State = stateReviewing
		d := Deps{Store: s}

		msg, err := fixRequestMessage(ticket, FixKindCILog, pbFixText, 3)
		if err != nil {
			t.Fatalf("fixRequestMessage: %v", err)
		}
		mid, err := s.InsertMessage(t.Context(), msg)
		if err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}

		u, ok, err := unitInFlight(t.Context(), ticket, d, response.Plan{}, nil)
		if err != nil {
			t.Fatalf("unitInFlight: %v", err)
		}
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if u.TaskN != 0 {
			t.Errorf("u.TaskN = %d, want 0 (a fix unit)", u.TaskN)
		}
		if u.FixRequestID == nil || *u.FixRequestID != mid {
			t.Errorf("u.FixRequestID = %v, want %d", u.FixRequestID, mid)
		}
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		s := newFixTestStore(t)
		ticket := seedFixTestTicket(t, s)
		ticket.State = stateReviewing
		d := Deps{Store: s}

		_, ok, err := unitInFlight(t.Context(), ticket, d, response.Plan{}, nil)
		if err != nil {
			t.Fatalf("unitInFlight: %v", err)
		}
		if ok {
			t.Fatal("ok = true, want false (no open fix request)")
		}
	})
}

// ---- TestPreludeRunsFixDriver, TestPreludeRoutesFixAnswerRound,
// TestPreludeRoutesPerimeterRound: step F and step R against a real fix
// unit --------------------------------------------------------------------

// TestPreludeRunsFixDriver proves step F (design section 5.5): with no
// rounds and an open fix request, postBuildPrelude runs the fix driver's own
// first turn.
func TestPreludeRunsFixDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	scriptRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{pbBuildStep([]string{pbHelloTxt}, nil, "fix-first-sess")}}
	deps := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), pbFixTestCmd)

	commit, handled := pbRunPrelude(t, s, deps, ticketID)
	if !handled {
		t.Fatal("handled = false, want true (an open fix request)")
	}
	if len(commit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one", commit.Runs)
	}
	if len(scriptRT.reqs) != 1 || scriptRT.reqs[0].Label != fixRunLabel {
		t.Fatalf("runtime requests = %+v, want one request labeled \"fix\"", scriptRT.reqs)
	}
	if !strings.Contains(scriptRT.reqs[0].Prompt, pbFixText) {
		t.Errorf("prompt = %q, want the fix text", scriptRT.reqs[0].Prompt)
	}
}

// TestPreludeRoutesFixAnswerRound proves step R's own build-job branch
// (design section 5.5): an owner's answer to the fix run's own question
// resumes that same session through postBuildPrelude, not DriveFix's own
// redundant internal check (fix.go), with the ownership check
// (postBuildRoundOwnedByOpenFix) confirming the round belongs to the
// currently open fix request.
func TestPreludeRoutesFixAnswerRound(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	scriptRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{pbQuestionResult(response.JobBuild, "fix-q-sess")}}
	deps := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), pbFixTestCmd)
	if _, handled := pbRunPrelude(t, s, deps, ticketID); !handled { // RUN: asks a question
		t.Fatal("handled = false, want true")
	}

	fixQ := pbFindOpenQuestionByKind(t, s, ticketID, response.QuestionKindQuestion)
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &fixQ.ID, Text: "Retry with a smaller batch."}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	scriptRT.steps = append(scriptRT.steps, pbBuildStep([]string{pbHelloTxt}, nil, "fix-q-sess"))
	deps2 := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), pbFixTestCmd)
	if _, handled := pbRunPrelude(t, s, deps2, ticketID); !handled { // step R: resume the answered round
		t.Fatal("handled = false, want true")
	}

	lastReq := scriptRT.reqs[len(scriptRT.reqs)-1]
	if lastReq.SessionID != "fix-q-sess" {
		t.Errorf("resume request SessionID = %q, want %q", lastReq.SessionID, "fix-q-sess")
	}
	if !strings.Contains(lastReq.Prompt, "Retry with a smaller batch.") {
		t.Errorf("resume prompt = %q, want the owner's own answer text", lastReq.Prompt)
	}
}

// TestPreludeRoutesPerimeterRound proves step R's own perimeter-kind branch
// (design section 5.5, #28 gap 3's "route a fix's perimeter round"
// requirement): the ASK question DESCRIBE posts after describing the fix
// run's one undeclared extra carries question kind perimeter, and
// postBuildPrelude routes it to building's own resolve, with the ownership
// check (postBuildRoundOwnedByOpenFix) confirming the round -- posted
// against the fix's own build run -- belongs to the currently open fix
// request (SessionAfter(req.AfterRunID)).
func TestPreludeRoutesPerimeterRound(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	// hello.txt already exists with this exact content from the earlier
	// three-task build (pbTicketInReviewing), so rewriting it here leaves no
	// real diff for git to see: only the undeclared extra is a real change,
	// so that is the only path this scenario claims changed.
	scriptRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{
		pbBuildStep([]string{pbExtraPath}, []response.ExtraClaim{{Path: pbExtraPath, Reason: pbExtraReason}}, "fix-perim-sess"),
	}}
	testCmd := "touch " + pbExtraPath + " && test -f hello.txt"
	deps := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), testCmd)
	if _, handled := pbRunPrelude(t, s, deps, ticketID); !handled { // RUN
		t.Fatal("handled = false, want true")
	}

	deps2 := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), testCmd)
	checkCommit, handled := pbRunPrelude(t, s, deps2, ticketID) // CHECK: claims ok, the extra still undecided
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(checkCommit.Messages) != 1 || !strings.HasPrefix(checkCommit.Messages[0].Body, "claims ok run ") {
		body := ""
		if len(checkCommit.Messages) > 0 {
			body = checkCommit.Messages[0].Body
		}
		t.Fatalf("CHECK commit.Messages body = %q, want the claims-ok marker", body)
	}

	scriptRT.steps = append(scriptRT.steps, pbPerimeterStep("Adds a small helper.", "fix-perim-describe-sess"))
	deps3 := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), testCmd)
	_, handled = pbRunPrelude(t, s, deps3, ticketID) // DESCRIBE + ASK (the one extra is also the last)
	if !handled {
		t.Fatal("handled = false, want true")
	}

	askQ := pbFindOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
	pbAnswerPerimeterQuestion(t, s, ticketID, askQ.ID, map[string]response.Decision{pbExtraPath: response.DecisionAccept})

	deps4 := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), testCmd)
	resolveCommit, handled := pbRunPrelude(t, s, deps4, ticketID) // step R: resolve the answered perimeter-kind round
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(resolveCommit.Artifacts) != 1 {
		t.Fatalf("resolve commit.Artifacts = %+v, want exactly one (the accepted file)", resolveCommit.Artifacts)
	}
	if len(resolveCommit.Messages) != 1 || !strings.HasPrefix(resolveCommit.Messages[0].Body, "perimeter resolved run ") {
		t.Fatalf("resolve commit.Messages = %+v, want the \"perimeter resolved\" marker", resolveCommit.Messages)
	}
}

// ---- TestPreludeFixRetryRestartsFix, TestPreludeFixRetryWithoutRunWritesMarker,
// TestPreludeCapResumesRetryFix, TestPreludeAbandon,
// TestPreludeBackToPlanningReplanUnsupported: resolvePostBuildEscalation,
// mostly built directly through CommitHandlerResult (escalateDirect,
// escalation_test.go's own accepted alternative to driving a real upstream
// failure, package job_test, unreachable from here) ------------------------

// pbTestEscalationPayload builds a schema-valid EscalationPayload for code
// and origin (escalation_test.go's own testEscalationPayload, package
// job_test, unreachable from here).
func pbTestEscalationPayload(code response.EscalationCode, origin response.EscalationOrigin) response.EscalationPayload {
	return response.EscalationPayload{
		Code: string(code), What: "what happened", Why: "why it happened", Tried: "what was tried",
		Options: []string{"retry", "planning", "abandon"}, Origin: string(origin),
	}
}

// pbEscalateDirect inserts an escalation message plus its linked question
// directly through CommitHandlerResult (escalation_test.go's own
// escalateDirect, package job_test, unreachable from here): a real
// HandlerCommit.Escalation, without reproducing whichever upstream failure
// would ordinarily produce it. Returns the linked question's id.
func pbEscalateDirect(t *testing.T, s *store.Store, ticketID int64, runID, sessionID *int64, code response.EscalationCode, origin response.EscalationOrigin) int64 {
	t.Helper()
	owner := "escalate-direct-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("pbEscalateDirect: claim: claimed=%v err=%v", claimed, err)
	}

	payload := pbTestEscalationPayload(code, origin)
	payload.SessionID = sessionID
	waiting := pbWaitingQuestions
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: &waiting,
		Escalation: &store.EscalationCommit{
			RunID: runID, Body: string(code) + ": " + payload.What, Payload: payload,
		},
	})
	if err != nil || !applied {
		t.Fatalf("pbEscalateDirect: CommitHandlerResult: applied=%v err=%v", applied, err)
	}

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) == 0 {
		t.Fatalf("pbEscalateDirect: QuestionsByState(open) = %v, %v, want at least one", open, err)
	}
	return open[len(open)-1].ID
}

// pbEscalationTextRetry and pbEscalationTextAbandon are the two option
// texts escalationOptionsFor (store/commit.go) assembles for a post-seal
// escalation (goconst: each repeats across this file's own fixture and
// assertions).
const (
	pbEscalationTextRetry   = "Retry"
	pbEscalationTextAbandon = "Abandon"
)

// pbEscalationQuestion inserts an escalation message plus its linked
// question directly through store.InsertMessage, with the question's own
// options and recommendation as given (ticket 60, review finding r1f2):
// pbLegacyEscalationQuestion and TestPreludeAcceptPickOnOtherEscalationReplans
// share this one body instead of each marshalling their own copy. Returns
// the linked question's id.
func pbEscalationQuestion(t *testing.T, s *store.Store, ticketID int64, code response.EscalationCode, origin response.EscalationOrigin, recommended string, options []response.Option) int64 {
	t.Helper()
	payload := pbTestEscalationPayload(code, origin)
	body := string(code) + ": " + payload.What
	escPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("pbEscalationQuestion: marshal escalation payload: %v", err)
	}
	escID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: "escalation", Author: authorZing, Body: body, Payload: escPayload,
	})
	if err != nil {
		t.Fatalf("pbEscalationQuestion: InsertMessage(escalation): %v", err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: recommended,
		Options:     options,
	})
	if err != nil {
		t.Fatalf("pbEscalationQuestion: marshal question payload: %v", err)
	}
	qID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, ParentID: &escID, Type: msgTypeQuestion, Author: authorZing,
		State: new(questionStateOpen), Body: body + "\n\nHow should Zing proceed?", Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("pbEscalationQuestion: InsertMessage(question): %v", err)
	}
	return qID
}

// pbLegacyEscalationQuestion inserts the fixed three-option payload every
// escalation offered before #47 item 2 (escalation_test.go's own
// legacyEscalationQuestion, package job_test, unreachable from here):
// "Retry", "Back to planning", and "Abandon", recommended "b".
// pbEscalateDirect now goes through the fixed escalateTx, which never
// recommends "b" post-seal any more (either answered explicitly or
// defaulted through roundRecommendedOption, planning.go), so it cannot
// produce this shape -- this helper simulates one of the escalations the
// database already carried before that fix shipped. Returns the linked
// question's id.
func pbLegacyEscalationQuestion(t *testing.T, s *store.Store, ticketID int64, code response.EscalationCode, origin response.EscalationOrigin) int64 {
	t.Helper()
	return pbEscalationQuestion(t, s, ticketID, code, origin, "b", []response.Option{
		{Key: "a", Text: pbEscalationTextRetry},
		{Key: "b", Text: "Back to planning"},
		{Key: "c", Text: pbEscalationTextAbandon},
	})
}

// pbAnswerEscalation answers questionID with option (escalation_test.go's
// own answerGateQuestion, package job_test, unreachable from here).
func pbAnswerEscalation(t *testing.T, s *store.Store, ticketID, questionID int64, option string) {
	t.Helper()
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Option: &option}); err != nil {
		t.Fatalf("SaveDraft(option): %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
}

// pbAnswerEscalationWithNote answers questionID with option plus a typed
// note (merge_test.go:1692-1702's own pattern): SaveDraft(option),
// SaveDraft(text), then SendBatch, so the owner's reply carries both the
// chosen option and free text into the round's Replies.
func pbAnswerEscalationWithNote(t *testing.T, s *store.Store, ticketID, questionID int64, option, note string) {
	t.Helper()
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Option: &option}); err != nil {
		t.Fatalf("SaveDraft(option): %v", err)
	}
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Text: note}); err != nil {
		t.Fatalf("SaveDraft(text): %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
}

// TestPreludeFixRetryRestartsFix proves 5.6's "fix with a run" retry row
// (#28 gap 3, design section 5.4 change 3): a fix run's own exec failure
// escalates origin fix with a run; the owner's retry restarts the fix with a
// fresh session (runFixFirst), notes and error fenced, the round resolved.
func TestPreludeFixRetryRestartsFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	// A fresh session and a terminalized "error" run of job "build", the
	// shape an exec failure or an agent's own error outcome leaves behind
	// (escalation_test.go's own reserveTerminalRun, package job_test,
	// unreachable from here).
	owner := "reserve-terminal-run-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, claimErr := s.Claim(t.Context(), ticketID, owner, expires)
	if claimErr != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, claimErr)
	}
	rsv, reserveErr := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: jobBuildName, Runtime: pbRuntimeClaude}, store.RunSeed{Model: pbModelClaudeX})
	if reserveErr != nil {
		t.Fatalf("Reserve: %v", reserveErr)
	}
	outcome, exitCode, agentSeconds := "error", 1, 1
	applied, commitErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	})
	if commitErr != nil || !applied {
		t.Fatalf("CommitHandlerResult: applied=%v err=%v", applied, commitErr)
	}

	runID, sessionID := rsv.RunID, rsv.SessionID
	qID := pbEscalateDirect(t, s, ticketID, &runID, nil, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginFix)
	pbAnswerEscalation(t, s, ticketID, qID, "a")

	scriptRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{pbBuildStep([]string{pbHelloTxt}, nil, "fix-retry-sess")}}
	deps := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), pbFixTestCmd)
	commit, handled := pbRunPrelude(t, s, deps, ticketID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(commit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one (a fresh run)", commit.Runs)
	}
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID == sessionID {
		t.Errorf("commit.Session = %+v, want a freshly reserved session (not the exhausted one, %d)", commit.Session, sessionID)
	}
	if commit.Session != nil && commit.Session.ExternalID != nil && *commit.Session.ExternalID != "fix-retry-sess" {
		t.Errorf("commit.Session.ExternalID = %q, want %q", *commit.Session.ExternalID, "fix-retry-sess")
	}
	if len(scriptRT.reqs) != 1 {
		t.Fatalf("runtime requests = %+v, want exactly one", scriptRT.reqs)
	}
	assertFencedPB(t, scriptRT.reqs[0].Prompt, "notes", "")
	assertFencedPB(t, scriptRT.reqs[0].Prompt, "error", "what happened")

	open, openErr := s.QuestionsByState(t.Context(), ticketID, "open")
	if openErr != nil {
		t.Fatalf("QuestionsByState: %v", openErr)
	}
	if len(open) != 0 {
		t.Errorf("open questions after retry = %+v, want none (the round resolved)", open)
	}
}

// TestPreludeFixRetryCarriesOwnerNote proves ticket #80 task 5: a
// fix-origin retry "with a run" (buildingHandler.retryFreshRun, through
// retryFreshFixRun) carries the owner's typed note, not just the chosen
// option, into the fresh session's own fenced "notes" input -- the same
// setup as TestPreludeFixRetryRestartsFix, but answered with a note instead
// of a bare option.
func TestPreludeFixRetryCarriesOwnerNote(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	owner := "reserve-terminal-run-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, claimErr := s.Claim(t.Context(), ticketID, owner, expires)
	if claimErr != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, claimErr)
	}
	rsv, reserveErr := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: jobBuildName, Runtime: pbRuntimeClaude}, store.RunSeed{Model: pbModelClaudeX})
	if reserveErr != nil {
		t.Fatalf("Reserve: %v", reserveErr)
	}
	outcome, exitCode, agentSeconds := "error", 1, 1
	applied, commitErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Runs: []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	})
	if commitErr != nil || !applied {
		t.Fatalf("CommitHandlerResult: applied=%v err=%v", applied, commitErr)
	}

	runID := rsv.RunID
	qID := pbEscalateDirect(t, s, ticketID, &runID, nil, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginFix)
	const retryNote = "rerun with the fixture data"
	pbAnswerEscalationWithNote(t, s, ticketID, qID, "a", retryNote)

	scriptRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{pbBuildStep([]string{pbHelloTxt}, nil, "fix-retry-sess")}}
	deps := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), pbFixTestCmd)
	commit, handled := pbRunPrelude(t, s, deps, ticketID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	_ = commit
	if len(scriptRT.reqs) != 1 {
		t.Fatalf("runtime requests = %+v, want exactly one", scriptRT.reqs)
	}
	assertFencedPB(t, scriptRT.reqs[0].Prompt, "notes", retryNote)
}

// assertFencedPB asserts prompt carries label's fenced input containing text
// (escalation_test.go's own assertFenced, package job_test, unreachable from
// here); an empty text only checks the label itself is present.
func assertFencedPB(t *testing.T, prompt, label, text string) {
	t.Helper()
	if !strings.Contains(prompt, label+":\n") {
		t.Errorf("prompt has no %q labeled input:\n%s", label, prompt)
	}
	if text != "" && !strings.Contains(prompt, text) {
		t.Errorf("prompt does not contain %q's text %q:\n%s", label, text, prompt)
	}
}

// TestPreludeFixRetryWithoutRunWritesMarker proves 5.6's "fix with no run"
// retry row (#28 gap 3): a no-run fix escalation (a step-0 environment
// failure) retries by writing the "retry requested" marker, no run
// attempted, the round resolved.
func TestPreludeFixRetryWithoutRunWritesMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	qID := pbEscalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginFix)
	pbAnswerEscalation(t, s, ticketID, qID, "a")

	deps := pbClaim(t, s, pbFakeRuntime(t), ticketID)
	commit, handled := pbRunPrelude(t, s, deps, ticketID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(commit.Runs) != 0 {
		t.Errorf("commit.Runs = %+v, want none (no run attempted)", commit.Runs)
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Body != markerRetryRequested {
		t.Fatalf("commit.Messages = %+v, want one %q marker", commit.Messages, markerRetryRequested)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}

// TestPreludeCapResumesRetryFix proves 5.6's cap_resumes row for a build
// session (#28 gap 3, design section 5.4 change 3): the fix's own build
// session hits the resume cap; the owner's retry runs retryCapResumesBuild
// with the fix unit -- a fresh fix session, notes and the preserved round's
// own answers.
func TestPreludeCapResumesRetryFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID := pbTicketInReviewing(t)
	pbOpenFixRequest(t, s, ticketID)

	// A fix's own build session, bumped straight to machine.toml's build
	// job cap (planning_test.go's own bumpResumesToCap, package job_test,
	// unreachable from here: no real claim-mismatch resume needed to prove
	// the retry's own routing).
	owner := "cap-resumes-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, claimErr := s.Claim(t.Context(), ticketID, owner, expires)
	if claimErr != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, claimErr)
	}
	rsv, reserveErr := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: jobBuildName, Runtime: pbRuntimeClaude}, store.RunSeed{Model: pbModelClaudeX})
	if reserveErr != nil {
		t.Fatalf("Reserve: %v", reserveErr)
	}
	extID := "fix-cap-sess"
	outcome, exitCode, agentSeconds := "ok", 0, 1
	applied, commitErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID},
		Runs:    []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	})
	if commitErr != nil || !applied {
		t.Fatalf("CommitHandlerResult: applied=%v err=%v", applied, commitErr)
	}
	maxResumes := pbMachine(t).Jobs[jobBuildName].MaxResumes
	for range maxResumes {
		claimed, claimErr := s.Claim(t.Context(), ticketID, owner, expires)
		if claimErr != nil || !claimed {
			t.Fatalf("bump claim: claimed=%v err=%v", claimed, claimErr)
		}
		applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Session: &store.SessionUpsert{ID: &rsv.SessionID, BumpResumes: true},
		})
		if err != nil || !applied {
			t.Fatalf("bump CommitHandlerResult: applied=%v err=%v", applied, err)
		}
	}

	sessionID := rsv.SessionID
	qID := pbEscalateDirect(t, s, ticketID, nil, &sessionID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	pbAnswerEscalation(t, s, ticketID, qID, "a")

	scriptRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{pbBuildStep([]string{pbHelloTxt}, nil, "fix-cap-retry-sess")}}
	deps := pbWithTestCmd(pbClaim(t, s, scriptRT, ticketID), pbGetTicket(t, s, ticketID), pbFixTestCmd)
	retryCommit, handled := pbRunPrelude(t, s, deps, ticketID) // step E: the cap_resumes escalation retries
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(retryCommit.Runs) != 1 {
		t.Fatalf("retryCommit.Runs = %+v, want exactly one (a fresh fix session)", retryCommit.Runs)
	}
	if retryCommit.Session == nil || retryCommit.Session.ID == nil || *retryCommit.Session.ID == sessionID {
		t.Errorf("retryCommit.Session = %+v, want a freshly reserved session (not the exhausted one, %d)", retryCommit.Session, sessionID)
	}
	if len(scriptRT.reqs) != 1 {
		t.Fatalf("runtime requests = %+v, want exactly one", scriptRT.reqs)
	}
	if scriptRT.reqs[0].Label != fixRunLabel {
		t.Errorf("retry request Label = %q, want \"fix\"", scriptRT.reqs[0].Label)
	}

	open, openErr := s.QuestionsByState(t.Context(), ticketID, "open")
	if openErr != nil {
		t.Fatalf("QuestionsByState: %v", openErr)
	}
	if len(open) != 0 {
		t.Errorf("open questions after retry = %+v, want none (the round resolved)", open)
	}
}

// TestPreludeAbandon proves 5.6's own choice c (design section 5.5, 5.6):
// abandon resolves every open question and transitions straight to
// abandoned, in every post-build state alike.
func TestPreludeAbandon(t *testing.T) {
	t.Parallel()
	s := newFixTestStore(t)
	ticket := pbSeedTicketInState(t, s, stateReviewing)

	qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginFix)
	pbAnswerEscalation(t, s, ticket.ID, qID, "c")

	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if commit.Next != stateAbandoned {
		t.Errorf("commit.Next = %q, want %q", commit.Next, stateAbandoned)
	}
	if !commit.ResolveAll {
		t.Error("commit.ResolveAll = false, want true")
	}

	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateAbandoned {
		t.Fatalf("ticket state = %q, want abandoned", final.State)
	}
}

// TestPreludeAbandonKeepsPostSealOptionIDs proves #47 item 2's option-ID
// stability through the real decode path: a post-seal escalation offers
// only Retry and Abandon, their keys stay "a" and "c" (Abandon is never
// renumbered to "b" just because Back to planning is missing), and
// answering the stored Abandon key -- read back from the question's own
// payload, not hardcoded -- resolves through store.AnswerQuestion (SaveDraft
// plus SendBatch) and abandons the ticket rather than sending it back to
// planning.
func TestPreludeAbandonKeepsPostSealOptionIDs(t *testing.T) {
	t.Parallel()
	s := newFixTestStore(t)
	ticket := pbSeedTicketInState(t, s, stateReviewing)

	qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginFix)

	msg, err := s.GetMessage(t.Context(), qID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	var qp response.QuestionPayload
	if err := json.Unmarshal(msg.Payload, &qp); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}

	wantOptions := []response.Option{{Key: "a", Text: pbEscalationTextRetry}, {Key: "c", Text: pbEscalationTextAbandon}}
	if !reflect.DeepEqual(qp.Options, wantOptions) {
		t.Fatalf("question.Options = %+v, want %+v (post-seal: no Back to planning, Abandon keeps key c)", qp.Options, wantOptions)
	}

	var abandonKey string
	for _, opt := range qp.Options {
		if opt.Text == pbEscalationTextAbandon {
			abandonKey = opt.Key
		}
	}
	if abandonKey == "" {
		t.Fatal("no option named Abandon among question.Options")
	}

	pbAnswerEscalation(t, s, ticket.ID, qID, abandonKey)

	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if commit.Next != stateAbandoned {
		t.Errorf("commit.Next = %q, want %q (not back to planning)", commit.Next, stateAbandoned)
	}
	if !commit.ResolveAll {
		t.Error("commit.ResolveAll = false, want true")
	}

	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateAbandoned {
		t.Fatalf("ticket state = %q, want abandoned (not sent back to planning)", final.State)
	}
}

// TestPreludeBackToPlanningReplanUnsupported proves 5.6's own choice b row
// (D14 of Package 8, design section 5.5): back to planning is deferred, so
// choice b, or a reply with no option, re-escalates replan_unsupported with
// the origin unchanged. #47 item 2 fixed escalateTx to stop offering "b"
// post-seal, and its own follow-up (roundRecommendedOption, planning.go)
// stopped a plain reply defaulting to "b" either, so this answers one of
// the escalations the database already carried before that fix shipped
// (pbLegacyEscalationQuestion, stored Recommended "b"): a fresh post-seal
// escalation's own reply-only default is TestEscalationReplyOnlyPostSealDefaultsToRetry
// (building_escalation_test.go, package job_test) instead.
func TestPreludeBackToPlanningReplanUnsupported(t *testing.T) {
	t.Parallel()
	s := newFixTestStore(t)
	ticket := pbSeedTicketInState(t, s, stateReviewing)

	qID := pbLegacyEscalationQuestion(t, s, ticket.ID, response.EscalationCodeEnvironment, response.EscalationOriginFix)
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Text: "not yet"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticket.ID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want a re-escalation")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeReplanUnsupported) {
		t.Errorf("commit.Escalation.Payload.Code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeReplanUnsupported)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginFix) {
		t.Errorf("commit.Escalation.Payload.Origin = %q, want fix (unchanged)", commit.Escalation.Payload.Origin)
	}
}

// TestPreludeAcceptPickOnOtherEscalationReplans proves ticket 60's own last
// goal: choice d is offered only on a review loops_exhausted question, so
// resolvePostBuildEscalation treats a d pick on any other escalation like
// b, re-escalating replan_unsupported with the origin unchanged, exactly as
// TestPreludeBackToPlanningReplanUnsupported's own choice b does.
func TestPreludeAcceptPickOnOtherEscalationReplans(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		code   response.EscalationCode
		origin response.EscalationOrigin
	}{
		{"origin review, code environment", response.EscalationCodeEnvironment, response.EscalationOriginReview},
		{"origin fix, code loops_exhausted", response.EscalationCodeLoopsExhausted, response.EscalationOriginFix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFixTestStore(t)
			ticket := pbSeedTicketInState(t, s, stateReviewing)

			qID := pbEscalationQuestion(t, s, ticket.ID, tc.code, tc.origin, escalationChoiceRetry, []response.Option{
				{Key: escalationChoiceRetry, Text: pbEscalationTextRetry},
				{Key: escalationChoiceAccept, Text: reviewAcceptRemainingOptionText},
				{Key: escalationChoiceAbandon, Text: pbEscalationTextAbandon},
			})
			pbAnswerEscalation(t, s, ticket.ID, qID, escalationChoiceAccept)

			deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
			commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
			if !handled {
				t.Fatal("handled = false, want true")
			}
			if commit.Escalation == nil {
				t.Fatal("commit.Escalation = nil, want a re-escalation")
			}
			if commit.Escalation.Payload.Code != string(response.EscalationCodeReplanUnsupported) {
				t.Errorf("commit.Escalation.Payload.Code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeReplanUnsupported)
			}
			if commit.Escalation.Payload.Origin != string(tc.origin) {
				t.Errorf("commit.Escalation.Payload.Origin = %q, want %q (unchanged)", commit.Escalation.Payload.Origin, tc.origin)
			}
			if commit.Next != "" {
				t.Errorf("commit.Next = %q, want %q", commit.Next, "")
			}
		})
	}
}
