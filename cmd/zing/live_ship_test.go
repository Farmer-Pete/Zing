// live_ship_test.go is the opt-in live harness for the real shipping state
// machine's own PUBLISH and POLL (PKG9-PLAN.md section 8, 19.4 task 9): the
// one place this repository proves a real pull request, pushed and polled
// against real GitHub, rather than the scripted shipGitHub double every
// other test in internal/job drives.
//
// TestLiveShip pushes branches and opens pull requests on the owner's own
// scratch repository (D2, Q21): Farmer-Pete/zing-sandbox, a public repo
// whose main is protected by a ruleset requiring the "ci" check from a
// GitHub Actions workflow (app 15368) that runs "go vet ./... && go test
// ./...". It never runs by default or in CI: it checks ZING_LIVE_GITHUB and
// ZING_LIVE_REPO itself and skips unless both are set, matching this
// milestone's own real-GitHub gate (distinct from ZING_LIVE_CLI, which
// gates a real claude/codex CLI call -- this harness's own fix run uses the
// fake runtime, never a real model, so it needs no CLI token at all, only a
// real GitHub one). Run it explicitly, deliberately:
//
//	ZING_LIVE_GITHUB=1 ZING_LIVE_REPO=Farmer-Pete/zing-sandbox \
//	  go test ./cmd/zing -run TestLiveShip -v -count=1 -timeout 20m
//
// It reads the GitHub token through the config model (liveShipGitHubToken,
// the same way serve does), never an environment variable, and never prints
// or logs it. The local clone defaults to ~/Code/personal/<repo>, the
// layout this harness's own owner uses; ZING_LIVE_REPO_PATH overrides it.
//
// What it proves, in order (design section 8.1 to 8.6): PUBLISH pushes a
// zing/<id>-<slug> branch and opens a draft pull request with the section
// 8.10 body, and posts one tracker PR-link comment; POLL then sees the real
// Actions run go red on a commit this harness deliberately breaks, writes a
// ci_log fix request carrying the real log tail, drives that fix to landing
// through fix.go's own DriveFix (job.Registry()["shipping"], the fake
// runtime scripting the repair under cmd/zing/testdata/live-ship/), and
// POLL pushes the fix and polls again until the real Actions run goes
// green. M3 builds no ready flip or merge (design section 8.5 rows 8 and 9
// are M4's), so green CI only ever leaves the ticket polling: the test
// stops there, asserting the ticket is still "shipping" and not waiting on
// a merge question, then closes the pull request and deletes the branch in
// t.Cleanup so the sandbox repository stays clean for the next run.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	zing "zing"
	"zing/fixtures"
	"zing/internal/config"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// liveShipDefaultRepoSlug is the scratch repository TestLiveShip's own
// package doc points at: the only repository the owner has approved Zing
// pushing branches and opening pull requests against (D2, Q21).
const liveShipDefaultRepoSlug = "Farmer-Pete/zing-sandbox"

// liveShipPollWait is how long TestLiveShip sleeps between two real polls
// of GitHub: long enough not to hammer the Actions API or look abusive,
// short enough that a handful of polls fit inside liveShipDeadline.
const liveShipPollWait = 15 * time.Second

// liveShipDeadline is the task's own "total timeout of 15 minutes" for
// every real wait this harness makes on GitHub, covering both the wait for
// the first (deliberately broken) commit's CI to go red and the wait for
// the fix commit's CI to go green.
const liveShipDeadline = 15 * time.Minute

// liveShipGreetGo is Farmer-Pete/zing-sandbox's own greet.go, exactly as
// committed (00d3fa1): the content TestLiveShip's own fix lands back,
// matching cmd/zing/testdata/live-ship/build/fix/1.tree/greet.go byte for
// byte, so CHECK's own claimed-vs-actual diff and the real `go test ./...`
// both see the same restored file.
const liveShipGreetGo = `// Package zingsandbox is a scratch repository for Zing's live shipping tests.
package zingsandbox

// Greet returns a greeting for name.
func Greet(name string) string {
	return "Hello, " + name + "!"
}
`

// liveShipBrokenGreetGo is liveShipGreetGo with one deliberate, deterministic
// break: it still compiles and still passes "go vet ./...", but
// "go test ./..." fails, since greet_test.go asserts Greet("Zing") equals
// "Hello, Zing!" exactly. This is TestLiveShip's own "one signed commit
// that fails ci" (PKG9-PLAN.md 19.4 task 9).
const liveShipBrokenGreetGo = `// Package zingsandbox is a scratch repository for Zing's live shipping tests.
package zingsandbox

// Greet returns a greeting for name.
func Greet(name string) string {
	return "Hi, " + name
}
`

// liveShipTestCmd and liveShipLintCmd are the sandbox repository's own two
// CI commands (.github/workflows/ci.yml: "go vet ./... && go test ./..."),
// run separately here so job.Project's own TestCmd/LintCmd split matches
// what the fix driver's CHECK step (design section 5.3) actually verifies
// locally before LAND ever runs, the same two commands the real Actions
// job runs remotely afterward.
const (
	liveShipTestCmd = "go test ./..."
	liveShipLintCmd = "go vet ./..."
)

// liveShipArtifactTypeBuildReport is the "build_report" artifacts.type
// value (store.Store's own convention, internal/job/planning.go's
// unexported artifactTypeBuildReport): named here, rather than repeated as
// a bare literal, since this package already carries two other raw
// "build_report" literals (live_judge_test.go, live_review_test.go)
// goconst leaves alone below its three-occurrence floor.
const liveShipArtifactTypeBuildReport = "build_report"

// TestLiveShipFixtureIsValid proves the fix fixture itself, with no runtime
// and no store, the same way TestLiveBuildFixtureIsValid proves its own
// plan.xml: cmd/zing/testdata/live-ship/build/fix/1.xml parses as a build
// document, and the file it writes (1.tree/greet.go) matches liveShipGreetGo
// -- the sandbox repository's own greet.go -- byte for byte, so the fix
// really restores the repository to a state "go test ./..." passes, not
// just a claimed one.
func TestLiveShipFixtureIsValid(t *testing.T) {
	t.Parallel()
	scriptData, err := os.ReadFile(filepath.Join("testdata", "live-ship", "build", "fix", "1.xml"))
	if err != nil {
		t.Fatalf("read build/fix/1.xml: %v", err)
	}
	doc, err := response.Parse(scriptData)
	if err != nil {
		t.Fatalf("parse build/fix/1.xml: %v", err)
	}
	if _, ok := doc.Response.(*response.BuildResponse); !ok {
		t.Fatalf("build/fix/1.xml response type = %T, want *response.BuildResponse", doc.Response)
	}

	treeData, err := os.ReadFile(filepath.Join("testdata", "live-ship", "build", "fix", "1.tree", liveGreetGoFilename))
	if err != nil {
		t.Fatalf("read build/fix/1.tree/%s: %v", liveGreetGoFilename, err)
	}
	if string(treeData) != liveShipGreetGo {
		t.Errorf("build/fix/1.tree/%s does not match liveShipGreetGo byte for byte", liveGreetGoFilename)
	}
}

// liveShipSkipReason reports why TestLiveShip would skip given
// ZING_LIVE_GITHUB and ZING_LIVE_REPO, or "" to run it for real. Distinct
// from liveBuildSkipReason's own ZING_LIVE_CLI gate (see the package doc
// comment): this harness's fix run never calls a real claude or codex CLI,
// so it needs no CLI gate and no darwin-only restriction, only a real
// GitHub token and a repository to push to.
func liveShipSkipReason(liveGitHub, repo string) string {
	if liveGitHub != "1" {
		return "set ZING_LIVE_GITHUB=1 to run the live shipping harness against a real GitHub repository"
	}
	if repo == "" {
		return "set ZING_LIVE_REPO to the scratch repository's owner/name, for example " + liveShipDefaultRepoSlug
	}
	return ""
}

// liveShipGitHubToken reads github_token through the config model (PKG9-
// PLAN.md section 4.5), the same way serve does: the owner's real
// ~/.zing/zing.toml, never an environment variable, never printed or
// logged -- liveClaudeOAuthToken's own rule, github_token instead of
// claude_oauth_token. It skips, with a clear reason, when the config file
// cannot be loaded or carries no token.
func liveShipGitHubToken(t *testing.T) string {
	t.Helper()
	cfgPath, err := config.DefaultPath()
	if err != nil {
		t.Skipf("resolve zing.toml path: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Skipf("load %s: %v", cfgPath, err)
	}
	if cfg.GitHubToken == "" {
		t.Skip("zing.toml carries no github_token")
	}
	return cfg.GitHubToken
}

// liveShipRepoLocalPath resolves the scratch repository's own local clone:
// ZING_LIVE_REPO_PATH when set, else ~/Code/personal/<repo-name>, the
// layout this harness's own owner uses for every repository it clones.
func liveShipRepoLocalPath(t *testing.T, repoName string) string {
	t.Helper()
	if p := os.Getenv("ZING_LIVE_REPO_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	return filepath.Join(home, "Code", "personal", repoName)
}

// liveShipTracker is a minimal job.ShipTracker that records PostPRLink and
// PostDone calls instead of writing to a real issue tracker (PKG9-PLAN.md
// 19.4 task 9: the fixture tracker stands in here, since M3's own shipping.go
// declares ShipTracker as what the handler needs, independent of which
// tracker backs it, and nothing in this milestone's own task list touches
// internal/tracker). It never contacts GitHub Issues or Linear.
type liveShipTracker struct {
	prLinkCount, doneCount int
}

func (tr *liveShipTracker) PostPRLink(context.Context, int64, string, string) error {
	tr.prLinkCount++
	return nil
}

func (tr *liveShipTracker) PostDone(context.Context, int64, string, string) error {
	tr.doneCount++
	return nil
}

// liveShipPRNumber parses the pull request number back out of a GitHub pull
// request URL, mirroring shiprules.go's own parsePRNumber (unexported,
// package job) closely enough for this harness's own assertions and
// cleanup -- it does not need that function's exact error text, only the
// number.
func liveShipPRNumber(t *testing.T, prURL string) int {
	t.Helper()
	_, numStr, ok := strings.Cut(prURL, "/pull/")
	if !ok {
		t.Fatalf("pr url %q has no /pull/<number>", prURL)
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		t.Fatalf("pr url %q: %v", prURL, err)
	}
	return n
}

// liveShipPlan loads and returns the same schema-valid "greet" plan
// fixture live_judge_test.go's own seedLiveJudgeCohort parses
// (fixtures/scripts/planning/2.xml): artifacts.InsertArtifact validates
// every write against its JSON Schema (internal/store/schemas/artifacts/plan.json),
// which plan.json's own nested required fields and minItems rules make
// impractical to hand-build correctly here -- the fixture is already proven
// valid, and its own content (a greet package) happens to match this
// harness's own sandbox repository thematically besides. normalizeLivePlanArrays
// (live_build_test.go) fills the array fields response.Parse leaves nil,
// the same conversion every other live harness storing this same fixture
// already makes.
func liveShipPlan(t *testing.T) response.Plan {
	t.Helper()
	data, err := fixtures.FS.ReadFile("scripts/planning/2.xml")
	if err != nil {
		t.Fatalf("liveShipPlan: read fixture: %v", err)
	}
	doc, err := response.Parse(data)
	if err != nil {
		t.Fatalf("liveShipPlan: parse fixture: %v", err)
	}
	ready, ok := doc.Response.(*response.ReadyResponse)
	if !ok {
		t.Fatalf("liveShipPlan: fixture response type = %T, want *response.ReadyResponse", doc.Response)
	}
	plan := ready.Plan
	normalizeLivePlanArrays(&plan)
	return plan
}

// seedLiveShipTicket seeds ticketID -- freshly inserted "queued" -- straight
// into "shipping" with plan stored, a one-scenario sealed cohort, and a
// "judge round 1 passed" verdict for that scenario at sha, all in one
// claim and one commit (seedLiveJudgeCohort's own shortcut, live_judge_test.go:
// CommitHandlerResult enforces no state-adjacency rule of its own -- only
// job.ValidateCommit does, and this harness never calls it over a seed
// commit, the same way seedLiveJudgeCohort and seedLiveBuildTicket do not
// either). This harness exists to prove PUBLISH and POLL, not the planning,
// building, reviewing, and judging state machines that would ordinarily
// produce this shape.
func seedLiveShipTicket(t *testing.T, st *store.Store, ticketID int64, plan response.Plan, sha string) {
	t.Helper()
	ctx := t.Context()
	const owner = "live-ship-seed"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

	claimed, err := st.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedLiveShipTicket: claim: claimed=%v err=%v", claimed, err)
	}

	rsv, err := st.Reserve(ctx, ticketID, owner, expires,
		store.SessionUpsert{Job: liveJobPlanning, Runtime: runtimeNameFake}, store.RunSeed{Model: "live-ship-seed-model"})
	if err != nil {
		t.Fatalf("seedLiveShipTicket: reserve: %v", err)
	}
	runID := rsv.RunID

	planPayload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("seedLiveShipTicket: marshal plan: %v", err)
	}
	if _, insertErr := st.InsertArtifact(ctx, store.Artifact{TicketID: ticketID, RunID: &runID, Type: liveArtifactTypePlan, Payload: planPayload}); insertErr != nil {
		t.Fatalf("seedLiveShipTicket: insert plan: %v", insertErr)
	}

	// sealCohortTx (commit.go) refuses a cohort smaller than 2 scenarios
	// (design D16: count < 2 is a "count" mismatch, not just count !=
	// req.ExpectedCount), so this seeds two, not one.
	scenarios := []response.Scenario{
		{
			ID: "s1", Kind: response.ScenarioKindBehavior,
			Given: "the Greet function in this repository",
			When:  `it is called with "Zing"`,
			Then:  `it returns "Hello, Zing!"`,
		},
		{
			ID: "s2", Kind: response.ScenarioKindBehavior,
			Given: "the Greet function in this repository",
			When:  `it is called with "Ada"`,
			Then:  `it returns "Hello, Ada!"`,
		},
	}
	for _, sc := range scenarios {
		scenarioPayload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			t.Fatalf("seedLiveShipTicket: marshal scenario %s: %v", sc.ID, marshalErr)
		}
		if _, insertErr := st.InsertArtifact(ctx, store.Artifact{TicketID: ticketID, RunID: &runID, Type: "scenario", Payload: scenarioPayload}); insertErr != nil {
			t.Fatalf("seedLiveShipTicket: insert scenario %s: %v", sc.ID, insertErr)
		}

		verdict := response.VerdictArtifact{
			Scenario: sc.ID, Result: response.ResultPass, Evidence: "seeded directly for the live shipping harness",
			Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha,
		}
		verdictPayload, marshalErr := json.Marshal(verdict)
		if marshalErr != nil {
			t.Fatalf("seedLiveShipTicket: marshal verdict %s: %v", sc.ID, marshalErr)
		}
		if _, insertErr := st.InsertArtifact(ctx, store.Artifact{TicketID: ticketID, Type: "verdict", Version: 1, Payload: verdictPayload}); insertErr != nil {
			t.Fatalf("seedLiveShipTicket: insert verdict %s: %v", sc.ID, insertErr)
		}
	}

	report := response.BuildReport{
		TaskN:        1,
		FilesChanged: []string{liveGreetGoFilename}, TestExit: 1, LintExit: 0,
		Extras: []response.ExtraClaim{}, Fences: []response.Fence{},
		Report: "Broke Greet's wording on purpose, to prove CI goes red.",
		Title:  "Break Greet's wording to prove CI goes red", CommitSHA: &sha,
	}
	reportPayload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("seedLiveShipTicket: marshal build report: %v", err)
	}
	if _, insertErr := st.InsertArtifact(ctx, store.Artifact{TicketID: ticketID, Type: liveShipArtifactTypeBuildReport, Payload: reportPayload}); insertErr != nil {
		t.Fatalf("seedLiveShipTicket: insert build report: %v", insertErr)
	}

	applied, err := st.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: stateShippingLiveConst, Reason: "live ship harness: sealed the cohort and moved to shipping",
		Seal: &store.SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: len(scenarios), At: time.Now()},
		Messages: []store.Message{{
			TicketID: ticketID, Type: "update", Author: "system", Body: "judge round 1 passed",
		}},
	})
	if err != nil || !applied {
		t.Fatalf("seedLiveShipTicket: commit: applied=%v err=%v", applied, err)
	}
}

// stateShippingLiveConst is the "shipping" ticket state (job.go's own
// unexported stateShipping), named once here so this file's one direct
// literal use (seedLiveShipTicket's Next) is documented rather than a bare
// string.
const stateShippingLiveConst = "shipping"

// liveShipDeps is the handful of real, long-lived values every tick of
// TestLiveShip's own handler ticks needs: built once, reused by
// liveShipClaim on every claim.
type liveShipDeps struct {
	store     *store.Store
	machine   *machine.Machine
	orch      *orchestrator.Orchestrator
	repoGit   string
	projectID int64
	owner     string
	repo      string
	gh        *orchestrator.GitHubClient
	tracker   *liveShipTracker
}

// liveShipClaim claims ticketID fresh under its own (owner, expires) and
// returns the job.Deps one handler tick needs: PUBLISH and POLL never
// reserve a session of their own, but the fix driver's own RUN step does,
// so rt is always wired in, exactly as shipClaim (internal/job/shipping_test.go)
// wires its own fake runtime on every claim regardless of which step the
// coming tick will actually take.
func liveShipClaim(t *testing.T, d liveShipDeps, ticketID int64, owner string, expires time.Time, rt runtime.Runtime, cmds job.CommandRunner) job.Deps {
	t.Helper()
	ctx := t.Context()
	claimed, err := d.store.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("liveShipClaim: claim %s: claimed=%v err=%v", owner, claimed, err)
	}

	set, err := runtime.NewSet(map[string]runtime.Runtime{runtimeNameClaude: rt, runtimeNameCodex: rt, runtimeNameFake: rt})
	if err != nil {
		t.Fatalf("liveShipClaim: runtime set: %v", err)
	}

	return job.Deps{
		Store: d.store, Runtimes: set, Machine: d.machine, Models: e2eModels, Budget: e2eBudget, Floor: e2eFloor,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, tID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return d.store.Reserve(ctx, tID, owner, expires, su, seed)
		},
		Sandboxes: sandbox.OffSet(), RequireSandbox: false,
		Commands: cmds,
		// Threads is filled from the same *orchestrator.GitHubClient as
		// PullRequests and Checks, the way serve.go's own buildJobProjects
		// fills every job.Project field from its one shared client
		// (PKG9-PLAN.md section 10.3): since M4 task 4, POLL reads
		// Project.Threads.ListThreads and Viewer on every poll (design
		// section 8.3 step 5), so a Project missing it would panic on a nil
		// interface the first time POLL runs. Flips is not filled here: POLL
		// does not call it yet (M4 task 7 wires MarkReady/ConvertToDraft).
		Projects: map[int64]job.Project{d.projectID: {
			Orch: d.orch, RepoGit: d.repoGit, TestCmd: liveShipTestCmd, LintCmd: liveShipLintCmd,
			Owner: d.owner, Repo: d.repo, PullRequests: d.gh, Checks: d.gh, Threads: d.gh,
		}},
		DataDir: t.TempDir(), LensesParallel: 7,
		Tracker: d.tracker,
	}
}

// liveShipApply validates commit against ticket, applies it, and returns
// ticketID's refreshed row -- job.ValidateCommit then store.CommitHandlerResult,
// the same two-step pbApply (internal/job) and the other live harnesses'
// own commit helpers already take.
func liveShipApply(t *testing.T, st *store.Store, ticket store.Ticket, commit store.HandlerCommit) store.Ticket {
	t.Helper()
	if err := job.ValidateCommit(ticket, commit); err != nil {
		t.Fatalf("liveShipApply: validate commit: %v\ncommit: %+v", err, commit)
	}
	applied, err := st.CommitHandlerResult(t.Context(), commit)
	if err != nil || !applied {
		t.Fatalf("liveShipApply: commit: applied=%v err=%v", applied, err)
	}
	refreshed, err := st.GetTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("liveShipApply: get ticket: %v", err)
	}
	return refreshed
}

// liveShipFirstLine returns s's own first line, for a short, readable
// t.Logf/t.Fatalf of a marker body that may carry several.
func liveShipFirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// liveShipHasMessagePrefix reports whether any of msgs carries a Body
// starting with prefix (fixRequestedCILogPrefix and the "fix landed "
// marker, shipping.go and fix.go, both unexported: this file matches their
// known literal text directly, the same way it matches "judge round 1
// passed" in seedLiveShipTicket, rather than reaching into package job for
// an unexported constant).
func liveShipHasMessagePrefix(msgs []store.Message, prefix string) (store.Message, bool) {
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, prefix) {
			return m, true
		}
	}
	return store.Message{}, false
}

// TestLiveShip is PKG9-PLAN.md section 19.4 task 9's own live harness: see
// the package doc comment for what it proves and how to run it.
func TestLiveShip(t *testing.T) {
	reason := liveShipSkipReason(os.Getenv("ZING_LIVE_GITHUB"), os.Getenv("ZING_LIVE_REPO"))
	if reason != "" {
		t.Skip(reason)
	}
	repoSlug := os.Getenv("ZING_LIVE_REPO")
	ghOwner, ghRepo, err := splitOwnerRepo(repoSlug)
	if err != nil {
		t.Fatalf("split owner/repo %q: %v", repoSlug, err)
	}

	token := liveShipGitHubToken(t)
	ghClient, err := orchestrator.NewGitHubClient(token)
	if err != nil {
		t.Fatalf("new github client: %v", err)
	}
	// rawGH is a second, independent go-github client built from the same
	// token (never logged, never printed, PKG9-PLAN.md section 4.5):
	// orchestrator.GitHubClient wraps its own *github.Client privately, so
	// cleanup -- closing the pull request and deleting the branch, neither
	// of which orchestrator.GitHubClient exposes (its own REST surface is
	// read, create-draft, and a sha-pinned merge; PKG9-PLAN.md section
	// 10.3) -- talks to go-github directly instead.
	rawGH, err := github.NewClient(github.WithAuthToken(token))
	if err != nil {
		t.Fatalf("build raw github client for cleanup: %v", err)
	}

	localPath := liveShipRepoLocalPath(t, ghRepo)
	if _, statErr := os.Stat(localPath); statErr != nil {
		t.Skipf("scratch repository clone not found at %s: %v", localPath, statErr)
	}

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("load machine.toml: %v", err)
	}

	st := newLiveStore(t)

	orch, err := orchestrator.New(orchestrator.Project{
		Owner: ghOwner, Repo: ghRepo, LocalPath: localPath, DefaultBranch: liveDefaultBranch,
	}, ghClient, orchestrator.NewRunner(), nil)
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	repoGit, err := orch.GitCommonDir(t.Context())
	if err != nil {
		t.Fatalf("git common dir: %v", err)
	}

	// Tracker's own value never matters here: this harness wires its own
	// liveShipTracker straight into job.Deps.Tracker below, bypassing
	// dispatch.Dispatcher's tracker-by-project-binding lookup entirely
	// (PKG9-PLAN.md 19.4 task 9: a fake stands in for the real tracker).
	// testServeTracker ("github") is reused rather than a fresh literal,
	// since the store still requires some non-empty value for this column.
	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: ghRepo, RepoURL: "https://github.com/" + repoSlug, LocalPath: localPath,
		Tracker: testServeTracker, DefaultBranch: liveDefaultBranch,
	})
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	const ticketTitle = "Fix the greet wording regression"
	ticketID, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: fmt.Sprintf("live-ship-%d", time.Now().UnixNano()),
		Title: ticketTitle, Body: "TestLiveShip's own harness ticket.", State: testServeStateQueued,
	})
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}

	wt, _, err := orch.EnsureWorktree(t.Context(), ticketID, ticketTitle)
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	// Reverse registration order (t.Cleanup is LIFO): the pull-request-close
	// and branch-delete cleanup registered after PUBLISH below runs first,
	// against real GitHub; this one, local only, runs last.
	t.Cleanup(func() {
		if removeErr := orch.RemoveWorktree(context.Background(), wt); removeErr != nil {
			t.Logf("cleanup: remove worktree: %v", removeErr)
		}
	})

	if writeErr := os.WriteFile(filepath.Join(wt.Dir(), liveGreetGoFilename), []byte(liveShipBrokenGreetGo), 0o644); writeErr != nil {
		t.Fatalf("write the breaking greet.go: %v", writeErr)
	}
	breakSHA, err := orch.CommitTask(t.Context(), wt, []string{liveGreetGoFilename}, orchestrator.CommitMessage{
		Title:     "Break Greet's wording to prove CI goes red",
		FuncLines: []string{"Greet: zingsandbox/greet.go, called by TestGreet, calls nothing else."},
	})
	if err != nil {
		t.Fatalf("commit the breaking change: %v", err)
	}
	t.Logf("committed the breaking change: %s", breakSHA)

	seedLiveShipTicket(t, st, ticketID, liveShipPlan(t), breakSHA)

	deps := liveShipDeps{
		store: st, machine: m, orch: orch, repoGit: repoGit, projectID: projectID,
		owner: ghOwner, repo: ghRepo, gh: ghClient, tracker: &liveShipTracker{},
	}
	fake := runtime.NewFake(os.DirFS(filepath.Join("testdata", "live-ship")))
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	freshExpires := func() time.Time { return time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second) }

	ticket, err := st.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("get ticket: %v", err)
	}

	// ---- PUBLISH (design section 8.2) --------------------------------

	publishDeps := liveShipClaim(t, deps, ticketID, "live-ship-publish", freshExpires(), fake, cmds)
	publishCommit, err := job.Registry()["shipping"].Run(t.Context(), ticket, publishDeps)
	if err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
	if publishCommit.Escalation != nil {
		t.Fatalf("PUBLISH escalated: %+v", publishCommit.Escalation.Payload)
	}
	ticket = liveShipApply(t, st, ticket, publishCommit)
	if ticket.PRURL == nil {
		t.Fatal("PUBLISH: ticket.PRURL is still nil")
	}
	prURL := *ticket.PRURL
	prNumber := liveShipPRNumber(t, prURL)
	t.Logf("opened draft pull request: %s", prURL)

	t.Cleanup(func() {
		ctx := context.Background()
		if _, _, editErr := rawGH.PullRequests.Edit(ctx, ghOwner, ghRepo, prNumber, &github.PullRequest{State: new("closed")}); editErr != nil {
			t.Logf("cleanup: close pull request #%d: %v", prNumber, editErr)
		}
		if _, delErr := rawGH.Git.DeleteRef(ctx, ghOwner, ghRepo, "heads/"+wt.Branch()); delErr != nil {
			t.Logf("cleanup: delete branch %s: %v", wt.Branch(), delErr)
		}
	})

	if deps.tracker.prLinkCount != 1 {
		t.Errorf("tracker PostPRLink calls after PUBLISH = %d, want 1", deps.tracker.prLinkCount)
	}

	// ---- POLL until the real Actions run goes red (design section 8.5 row 4) ----

	deadline := time.Now().Add(liveShipDeadline)
	var fixRequested bool
	for !fixRequested {
		if time.Now().After(deadline) {
			t.Fatalf("CI did not fail on the breaking commit %s within the %s deadline", breakSHA, liveShipDeadline)
		}

		pollDeps := liveShipClaim(t, deps, ticketID, "live-ship-poll-red", freshExpires(), fake, cmds)
		commit, runErr := job.Registry()["shipping"].Run(t.Context(), ticket, pollDeps)
		if runErr != nil {
			t.Fatalf("POLL while waiting for CI to fail: %v", runErr)
		}
		if commit.Escalation != nil {
			t.Fatalf("POLL escalated while waiting for CI to fail: %+v", commit.Escalation.Payload)
		}
		if msg, ok := liveShipHasMessagePrefix(commit.Messages, "fix requested ci_log"); ok {
			fixRequested = true
			t.Logf("ci failed as expected: %s", liveShipFirstLine(msg.Body))
		}
		ticket = liveShipApply(t, st, ticket, commit)
		if !fixRequested {
			time.Sleep(liveShipPollWait)
		}
	}

	// ---- drive the fix to landing (fix.go's DriveFix, through shipHandler's postBuildPrelude) ----

	var landed bool
	for i := range 4 {
		fixDeps := liveShipClaim(t, deps, ticketID, fmt.Sprintf("live-ship-fix-%d", i), freshExpires(), fake, cmds)
		commit, runErr := job.Registry()["shipping"].Run(t.Context(), ticket, fixDeps)
		if runErr != nil {
			t.Fatalf("fix driver tick %d: %v", i, runErr)
		}
		if commit.Escalation != nil {
			t.Fatalf("fix driver escalated at tick %d: %+v", i, commit.Escalation.Payload)
		}
		ticket = liveShipApply(t, st, ticket, commit)
		if msg, ok := liveShipHasMessagePrefix(commit.Messages, "fix landed "); ok {
			landed = true
			t.Logf("fix landed: %s", liveShipFirstLine(msg.Body))
			break
		}
	}
	if !landed {
		t.Fatal("the fix did not land within 4 ticks")
	}

	fixSHA, err := orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("head sha after the fix: %v", err)
	}
	if fixSHA == breakSHA {
		t.Fatal("the fix driver landed no new commit")
	}
	t.Logf("fix commit: %s", fixSHA)

	// ---- POLL again: push the fix, then wait for the real Actions run to go green ----

	var green bool
	for !green {
		if time.Now().After(deadline) {
			t.Fatalf("CI did not go green on the fix commit %s within the overall %s deadline", fixSHA, liveShipDeadline)
		}

		pollDeps := liveShipClaim(t, deps, ticketID, "live-ship-poll-green", freshExpires(), fake, cmds)
		commit, runErr := job.Registry()["shipping"].Run(t.Context(), ticket, pollDeps)
		if runErr != nil {
			t.Fatalf("POLL after the fix: %v", runErr)
		}
		if commit.Escalation != nil {
			t.Fatalf("POLL escalated after the fix: %+v", commit.Escalation.Payload)
		}
		if msg, ok := liveShipHasMessagePrefix(commit.Messages, "fix requested ci_log"); ok {
			t.Fatalf("CI failed again on the fix commit: %s", liveShipFirstLine(msg.Body))
		}
		ticket = liveShipApply(t, st, ticket, commit)

		runs, runsErr := ghClient.ListCheckRuns(t.Context(), ghOwner, ghRepo, fixSHA)
		if runsErr != nil {
			t.Fatalf("list check runs: %v", runsErr)
		}
		statuses, statusesErr := ghClient.ListStatuses(t.Context(), ghOwner, ghRepo, fixSHA)
		if statusesErr != nil {
			t.Fatalf("list statuses: %v", statusesErr)
		}
		required, requiredErr := ghClient.RequiredCheckRules(t.Context(), ghOwner, ghRepo, liveDefaultBranch)
		if requiredErr != nil {
			t.Fatalf("required check rules: %v", requiredErr)
		}
		if result := job.EvaluateCI(runs, statuses, required); result.State == job.CIGreen {
			green = true
			t.Logf("ci green on the fix commit %s", fixSHA)
			continue
		}
		time.Sleep(liveShipPollWait)
	}

	// ---- M3's own stopping point: CI green, still polling (design section 8.5: rows 8 and 9 are M4's) ----

	final, err := st.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("final get ticket: %v", err)
	}
	if final.State != stateShippingLiveConst {
		t.Errorf("final ticket state = %q, want still %q", final.State, stateShippingLiveConst)
	}
	if final.WaitingOn != nil {
		t.Errorf("final ticket waiting_on = %q, want nil (still polling, not waiting on a merge question)", *final.WaitingOn)
	}
	if final.PRURL == nil || *final.PRURL != prURL {
		t.Errorf("final ticket pr_url = %v, want %q", final.PRURL, prURL)
	}
	if deps.tracker.doneCount != 0 {
		t.Errorf("tracker PostDone calls = %d, want 0 (M3 never reaches done)", deps.tracker.doneCount)
	}
}
