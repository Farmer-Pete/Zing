package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/config"
	zdispatch "zing/internal/dispatch"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// --- productionRuntimes (task 14, D2) ---------------------------------------

// TestProductionRuntimes_ResolvesClaudeAndCodexNotFake proves productionRuntimes
// resolves the two real runtimes, claude and codex, and never maps the fake
// name (design D2: "Production wires the two real runtimes, claude and
// codex"; PKG7-PLAN.md task 14). runtime.Set exposes no size, so this cannot
// prove the set holds ONLY these two; it asserts the two resolve and fake
// does not.
func TestProductionRuntimes_ResolvesClaudeAndCodexNotFake(t *testing.T) {
	t.Parallel()

	rts, err := productionRuntimes("test-claude-oauth-token")
	if err != nil {
		t.Fatalf("productionRuntimes: %v", err)
	}

	if _, err := rts.For(runtimeNameClaude); err != nil {
		t.Errorf("For(%q) = %v, want it to resolve", runtimeNameClaude, err)
	}
	if _, err := rts.For(runtimeNameCodex); err != nil {
		t.Errorf("For(%q) = %v, want it to resolve", runtimeNameCodex, err)
	}
	if _, err := rts.For(runtimeNameFake); err == nil {
		t.Errorf("For(%q): want an error, production must never map %q", runtimeNameFake, runtimeNameFake)
	}
}

// --- productionTracker (task 14) --------------------------------------------

// productionTrackerFixtureConfig returns a minimal *config.Config carrying
// two projects, each with a valid "owner/name" repo -- config.Project.Repo's
// own shape, since "zing project add"'s splitOwnerRepo (cmd/zing/project.go)
// already validates and stores it that way -- enough for productionTracker
// to build a real *tracker.GitHubTracker with no network call
// (tracker.NewGitHub only parses and stores; internal/tracker/github.go).
func productionTrackerFixtureConfig() *config.Config {
	return &config.Config{
		GitHubToken: "test-github-token",
		Projects: []config.Project{
			{Name: testServeProjectName, Repo: "Farmer-Pete/Zing"},
			{Name: "other", Repo: "Farmer-Pete/Other"},
		},
	}
}

// TestProductionTracker_BuildsAGitHubTrackerFromConfiguredProjects proves
// productionTracker builds a real *tracker.GitHubTracker from cfg.GitHubToken
// and one "owner/name" repo per configured project, with no network call.
func TestProductionTracker_BuildsAGitHubTrackerFromConfiguredProjects(t *testing.T) {
	t.Parallel()

	tr, err := productionTracker(productionTrackerFixtureConfig())
	if err != nil {
		t.Fatalf("productionTracker: %v", err)
	}
	if tr == nil {
		t.Fatal("productionTracker returned a nil *tracker.GitHubTracker")
	}
}

// TestProductionTracker_EmptyTokenErrors proves an empty github_token fails
// construction (tracker.NewGitHub / go-github's own "token must not be
// empty"), rather than building a tracker that would only fail later, at its
// first real call.
func TestProductionTracker_EmptyTokenErrors(t *testing.T) {
	t.Parallel()

	cfg := productionTrackerFixtureConfig()
	cfg.GitHubToken = ""

	if _, err := productionTracker(cfg); err == nil {
		t.Error("productionTracker with an empty token: want an error, got nil")
	}
}

// --- NewFixture reference scope (task 14) -----------------------------------

// newFixtureAllowedFiles are the two non-test files NewFixture (the fixture
// tracker) may still be referenced from: cmd/zing/selftest.go, its one
// remaining caller (design D2, D11: selftest keeps the Fake and the Fixture
// tracker), and internal/tracker/fixture.go, which defines it (a definition
// is not a "reference" to it from elsewhere). Every other non-"_test.go" file
// under cmd/ and internal/ must never mention it: task 14 replaces
// serve.go's own construction with tracker.NewGitHub.
var newFixtureAllowedFiles = map[string]bool{
	"cmd/zing/selftest.go":        true,
	"internal/tracker/fixture.go": true,
}

// TestNewFixtureReferencedOnlyFromSelftestOrTests walks every ".go" file
// under the module's cmd/ and internal/ directories and fails if "NewFixture"
// appears in one that is neither a "_test.go" file nor listed in
// newFixtureAllowedFiles (PKG7-PLAN.md task 14: "NewFixture must now be
// referenced ONLY from selftest and _test.go files").
func TestNewFixtureReferencedOnlyFromSelftestOrTests(t *testing.T) {
	t.Parallel()

	const needle = "NewFixture"
	var offenders []string

	for _, dir := range []string{"../../cmd", "../../internal"} {
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			rel, err := filepath.Rel("../..", path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if newFixtureAllowedFiles[rel] {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(needle)) {
				offenders = append(offenders, rel)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("%s referenced outside selftest.go and _test.go files: %v", needle, offenders)
	}
}

// --- the sandbox (task 8) ------------------------------------------------

// TestServeRequiresSandbox proves serve's own dispatcher config always sets
// RequireSandbox true (design N9, section 10): no config key overrides it.
func TestServeRequiresSandbox(t *testing.T) {
	t.Parallel()
	if !serveRequireSandbox {
		t.Error("serveRequireSandbox = false, want true (a real build run must refuse to start without a loaded sandbox)")
	}
}

// TestServeLoadsBuildAndReadonly proves serveSandbox loads the build and
// readonly profiles (PKG9-PLAN.md section 4.7). Build and readonly are
// really attempted here (sandbox.LoadProfile), proving the wiring reaches
// them at all; whether this host can run sandbox-exec at all is
// internal/sandbox's own suite's concern, not this one's, so an
// unavailable result only needs a non-empty reason.
func TestServeLoadsBuildAndReadonly(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	dataDir := t.TempDir()

	sbSet, err := serveSandbox(cfg, dataDir)
	if err != nil {
		t.Fatalf("serveSandbox: %v", err)
	}
	if !sbSet.Build.Available() && sbSet.Build.Reason() == "" {
		t.Error("Build.Reason() is empty for an unavailable sandbox")
	}
	if !sbSet.ReadOnly.Available() && sbSet.ReadOnly.Reason() == "" {
		t.Error("ReadOnly.Reason() is empty for an unavailable sandbox")
	}
}

// TestServeLoadsJudge proves serveSandbox also loads the judge profile
// (PKG9-PLAN.md section 4.7; M2 task 2 adds judge.sb and its own load, in
// place of M1's Set.Judge, which was sandbox.NotLoaded()). Judge's own
// proof needs a real scenarios-file read and a real CODEX_HOME write
// (section 4.7's own worked example), so this only needs a non-empty
// reason on an unavailable result, the same as Build and ReadOnly above.
func TestServeLoadsJudge(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	dataDir := t.TempDir()

	sbSet, err := serveSandbox(cfg, dataDir)
	if err != nil {
		t.Fatalf("serveSandbox: %v", err)
	}
	if !sbSet.Judge.Available() && sbSet.Judge.Reason() == "" {
		t.Error("Judge.Reason() is empty for an unavailable sandbox")
	}
}

// TestServeBuildsOneOrchestratorPerProject proves buildJobProjects builds
// exactly one job.Project per configured project, keyed by its store
// project id, each carrying its own repository's real git common dir.
func TestServeBuildsOneOrchestratorPerProject(t *testing.T) {
	t.Parallel()

	repoA := newTestGitRepo(t)
	repoB := newTestGitRepo(t)
	cfgProjects := []config.Project{
		{Name: "alpha", Repo: "acme/alpha", Path: repoA, Tracker: testServeTracker},
		{Name: "beta", Repo: "acme/beta", Path: repoB, Tracker: testServeTracker},
	}
	bindings := []zdispatch.Binding{
		{StoreProjectID: 10, TrackerProject: "alpha"},
		{StoreProjectID: 20, TrackerProject: "beta"},
	}
	gh, err := orchestrator.NewGitHubClient("test-github-token")
	if err != nil {
		t.Fatalf("orchestrator.NewGitHubClient: %v", err)
	}

	projects, err := buildJobProjects(t.Context(), cfgProjects, bindings, gh, sandbox.Off())
	if err != nil {
		t.Fatalf("buildJobProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("len(projects) = %d, want 2", len(projects))
	}
	for id, wantRepo := range map[int64]string{10: repoA, 20: repoB} {
		p, ok := projects[id]
		if !ok {
			t.Fatalf("projects[%d] missing", id)
		}
		if p.Orch == nil {
			t.Errorf("projects[%d].Orch is nil", id)
		}
		// git itself resolves symlinks in its own --path-format=absolute
		// output (macOS's /var -> /private/var, most commonly), so the
		// comparison must too.
		resolved, err := filepath.EvalSymlinks(wantRepo)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", wantRepo, err)
		}
		wantGitDir := filepath.Join(resolved, ".git")
		if p.RepoGit != wantGitDir {
			t.Errorf("projects[%d].RepoGit = %q, want %q", id, p.RepoGit, wantGitDir)
		}
	}
}

// TestServeProjectsHaveM3Interfaces proves buildJobProjects fills every
// job.Project's Owner, Repo, PullRequests, and Checks from the configured
// repo and the one shared *orchestrator.GitHubClient (PKG9-PLAN.md section
// 10.3, M3 task 2), ahead of shipping's POLL (task 7) ever reading them.
func TestServeProjectsHaveM3Interfaces(t *testing.T) {
	t.Parallel()

	const gammaProject = "gamma"

	repoA := newTestGitRepo(t)
	cfgProjects := []config.Project{
		{Name: gammaProject, Repo: "acme/" + gammaProject, Path: repoA, Tracker: testServeTracker},
	}
	bindings := []zdispatch.Binding{
		{StoreProjectID: 10, TrackerProject: gammaProject},
	}
	gh, err := orchestrator.NewGitHubClient("test-github-token")
	if err != nil {
		t.Fatalf("orchestrator.NewGitHubClient: %v", err)
	}

	projects, err := buildJobProjects(t.Context(), cfgProjects, bindings, gh, sandbox.Off())
	if err != nil {
		t.Fatalf("buildJobProjects: %v", err)
	}

	p, ok := projects[10]
	if !ok {
		t.Fatal("projects[10] missing")
	}
	if p.Owner != "acme" {
		t.Errorf("Owner = %q, want %q", p.Owner, "acme")
	}
	if p.Repo != gammaProject {
		t.Errorf("Repo = %q, want %q", p.Repo, gammaProject)
	}
	if p.PullRequests == nil {
		t.Error("PullRequests is nil")
	}
	if p.Checks == nil {
		t.Error("Checks is nil")
	}
}

// productionTestModels is the job.Deps.Models alias table the tests below
// wire every claim/run with: the exact model ids do not matter, since every
// runtime here is a *runtime.Fake or a spy.
var productionTestModels = map[string]string{
	modelAliasSonnet: "claude-sonnet-5", modelAliasOpus: "claude-opus-4-8",
	modelAliasFable: "claude-fable-5-1", modelAliasCodex: "gpt-5.5",
}

// neverCalledRuntime fails the test if its Run is ever called: proves the
// sandbox gate refuses a sandboxed job before the runtime does anything.
type neverCalledRuntime struct{ t *testing.T }

func (n neverCalledRuntime) Run(context.Context, runtime.RunRequest) (runtime.RunResult, error) {
	n.t.Helper()
	n.t.Fatal("runtime.Run was called; the sandbox gate should have refused before any run")
	return runtime.RunResult{}, nil
}

// neverCalledCommandRunner fails the test if its Run is ever called.
type neverCalledCommandRunner struct{ t *testing.T }

func (n neverCalledCommandRunner) Run(context.Context, string, string, string, time.Duration) (int, error) {
	n.t.Helper()
	n.t.Fatal("CommandRunner.Run was called; the sandbox gate should have refused before any command ran")
	return -1, nil
}

// answerOneOpenQuestion answers ticketID's one open question with option
// (or its first offered option, when option is ""), through
// store.AnswerQuestion, exactly as the console's POST /answer would.
func answerOneOpenQuestion(t *testing.T, st *store.Store, ticketID int64, option string) {
	t.Helper()
	open, err := st.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) == 0 {
		t.Fatal("QuestionsByState(open): no open questions")
	}
	if option == "" {
		var payload response.QuestionPayload
		if unmarshalErr := json.Unmarshal(open[0].Payload, &payload); unmarshalErr != nil {
			t.Fatalf("unmarshal question payload: %v", unmarshalErr)
		}
		if len(payload.Options) == 0 {
			t.Fatal("question has no options")
		}
		option = payload.Options[0].Key
	}
	result, err := st.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: option})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q", result.Conflict)
	}
}

// driveTicketToBuilding claims and runs ticketID's handler, state by state,
// through queued -> planning (classify, the first entry, an answered
// question, the ready resume, the clean review tick, an approved gate),
// until it reaches "building" -- the same ring
// TestRing_QueuedToDoneAnsweringOneQuestion (internal/job/skeleton_test.go)
// drives further, stopped here one state short of it. Every call runs
// unsandboxed (Sandbox: sandbox.Off(), RequireSandbox: false): only the
// caller's own separate, final building tick exercises the sandbox gate.
func driveTicketToBuilding(t *testing.T, st *store.Store, m *machine.Machine, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	reg := job.Registry()

	run := func(state string) {
		ticket, err := st.GetTicket(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		owner := "production-test-owner"
		expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
		claimed, err := st.Claim(t.Context(), ticketID, owner, expires)
		if err != nil || !claimed {
			t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
		}
		set, err := runtime.NewSet(map[string]runtime.Runtime{runtimeNameClaude: rt, runtimeNameCodex: rt, runtimeNameFake: rt})
		if err != nil {
			t.Fatalf("runtime.NewSet: %v", err)
		}
		deps := job.Deps{
			Store: st, Runtimes: set, Machine: m, Models: productionTestModels,
			Budget: time.Hour, Floor: response.SeverityMinor, Owner: owner, Expires: expires,
			Reserve: func(ctx context.Context, tid int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
				return st.Reserve(ctx, tid, owner, expires, su, seed)
			},
			Sandboxes: sandbox.OffSet(), RequireSandbox: false, Commands: job.NewCommandRunner(sandbox.Off(), false),
			DataDir: t.TempDir(),
		}
		commit, err := reg[state].Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("%s handler.Run: %v", state, err)
		}
		if validateErr := job.ValidateCommit(ticket, commit); validateErr != nil {
			t.Fatalf("ValidateCommit: %v", validateErr)
		}
		applied, err := st.CommitHandlerResult(t.Context(), commit)
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if !applied {
			t.Fatal("CommitHandlerResult: applied = false")
		}
	}

	run("queued") // -> planning

	const maxPlanningCalls = 10
	for range maxPlanningCalls {
		run("planning")
		ticket, err := st.GetTicket(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		if ticket.State == "building" {
			return
		}
		if ticket.WaitingOn == nil {
			continue
		}
		switch *ticket.WaitingOn {
		case "questions":
			answerOneOpenQuestion(t, st, ticketID, "")
		case "gate":
			answerOneOpenQuestion(t, st, ticketID, "a")
		default:
			t.Fatalf("driveTicketToBuilding: waiting_on = %q, want questions, gate, or nil", *ticket.WaitingOn)
		}
	}
	t.Fatalf("driveTicketToBuilding: ticket did not reach building within %d planning calls", maxPlanningCalls)
}

// TestProductionBuildNeedsSandbox proves that with the production wiring
// (serveRequireSandbox) and an unavailable sandbox, a building tick
// escalates sandbox_unavailable and never calls the runtime or the command
// runner (design section 5.5, N9).
func TestProductionBuildNeedsSandbox(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}

	// A real, signed gitfixture repository (PKG8-PLAN.md section 9.4, 10),
	// not the bare newTestGitRepo other tests in this package use: the real
	// building handler's own EnsureWorktree needs a commit to branch off of.
	// The fixture ready cohort's one code claim cites cmd/zing/main.go:60,
	// committed here on top of gitfixture's own initial commit.
	repoDir := t.TempDir()
	if fixErr := gitfixture.NewSigningRepo(t.Context(), repoDir); fixErr != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", fixErr)
	}
	if addErr := gitfixture.AddFile(t.Context(), repoDir, filepath.Join("cmd", testServeProjectName, "main.go"), []byte("package main\n")); addErr != nil {
		t.Fatalf("gitfixture.AddFile: %v", addErr)
	}

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: testServeProjectName, RepoURL: "https://example.invalid/zing", LocalPath: repoDir, Tracker: testServeTracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testServeTicketRef, Title: "t", State: testServeStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	fake := runtime.NewFake(scriptsFS)

	driveTicketToBuilding(t, st, m, fake, ticketID)

	ticket, err := st.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.State != "building" {
		t.Fatalf("ticket.State = %q, want building", ticket.State)
	}

	// The production gate: RequireSandbox is always true
	// (serveRequireSandbox), the sandbox is unavailable, and the runtime
	// and command runner must never be called.
	owner := "production-test-owner-gate"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := st.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	spyRT := neverCalledRuntime{t: t}
	set, err := runtime.NewSet(map[string]runtime.Runtime{runtimeNameClaude: spyRT, runtimeNameCodex: spyRT, runtimeNameFake: spyRT})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	// gitfixture.NewSigningRepo always inits its repo on branch "main".
	const gitfixtureDefaultBranch = "main"
	orch, err := orchestrator.New(
		orchestrator.Project{Owner: "fixture", Repo: "fixture", LocalPath: repoDir, DefaultBranch: gitfixtureDefaultBranch},
		selftestGitHub{}, orchestrator.NewRunner(), nil)
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	repoGit, err := orch.GitCommonDir(t.Context())
	if err != nil {
		t.Fatalf("GitCommonDir: %v", err)
	}
	deps := job.Deps{
		Store: st, Runtimes: set, Machine: m, Models: productionTestModels,
		Budget: time.Hour, Floor: response.SeverityMinor, Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, tid int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return st.Reserve(ctx, tid, owner, expires, su, seed)
		},
		Sandboxes: sandbox.OffSet(), RequireSandbox: serveRequireSandbox,
		Commands: neverCalledCommandRunner{t: t},
		Projects: map[int64]job.Project{
			projectID: {Orch: orch, RepoGit: repoGit, TestCmd: "test -f hello.txt", LintCmd: "true"},
		},
	}

	commit, err := job.Registry()["building"].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("building Run: %v", err)
	}
	if commit.Escalation == nil || commit.Escalation.Payload.Code != "sandbox_unavailable" {
		t.Fatalf("commit.Escalation = %+v, want a sandbox_unavailable escalation", commit.Escalation)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (an escalation, not a transition)", commit.Next)
	}
}
