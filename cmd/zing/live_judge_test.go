// live_judge_test.go is the opt-in live harness for the real judging state
// machine's own RUN (PKG9-PLAN.md section 7.2, 19.3 task 9): the one place
// this repository proves a real judge round against the real, pinned codex
// CLI under the judge sandbox profile, with its own Codex home
// (judge_codex_home, D27), rather than the fake runtime every other test in
// this package drives.
//
// TestLiveJudge spends the owner's real Codex usage. It never runs by
// default, in CI, or from any other test: it checks ZING_LIVE_CLI itself
// and skips unless it is exactly "1", and only on macOS (the sandbox is
// darwin-only, design N9, section 5), reusing live_build_test.go's own gate
// (liveBuildSkipReason, newLiveStore, buildZingBinary) and
// live_review_test.go's own plan-fixture normalizer. It also skips when the
// owner's judge Codex home (judge_codex_home in zing.toml, ~/.zing/codex-judge
// by default) is not logged in, or when zing.toml names no [models] codex
// key (loadLiveJudgeConfig): the model id itself is never hardcoded here,
// since only the judge Codex home's own login knows which ones it accepts,
// and that is the owner's call, not this file's. Run it explicitly,
// deliberately:
//
//	ZING_LIVE_CLI=1 go test ./cmd/zing -run TestLiveJudge -v -timeout 30m
//
// Not parallel: it calls t.Setenv("PATH", ...) to prepend a real zing
// binary (buildZingBinary), the same way TestLiveBuild does, so the judge's
// own prompt ("run `zing scenarios`") can actually resolve that command
// inside the sandboxed codex process -- the judge profile's own ZING_BIN
// carve-out names the running test binary, not a binary called "zing".
package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	zing "zing"
	"zing/fixtures"
	"zing/internal/config"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// liveJudgeRepoName is this harness's own gitfixture repo and store project
// name: distinct from live_build_test.go's own "greeter" and
// live_review_test.go's own liveReviewRepoName.
const liveJudgeRepoName = "greeter-judge"

// liveJudgeGoMod is the fixture module's own go.mod: a standalone one-file
// program, not the greeter module live_build_test.go and live_review_test.go
// each build from testdata.
const liveJudgeGoMod = "module greeter\n\ngo 1.27\n"

// liveJudgeGreetGo is the ticket branch's one commit (PKG9-PLAN.md section
// 19.3 task 9: "a commit that passes two scenarios and fails one"): a small
// command-line program with one planted bug -- the second name given
// (index 1) is silently skipped -- so a run with two names never greets the
// second. liveJudgeScenarios' own s3 exists to catch exactly this; s1 and
// s2 never exercise the bug at all.
const liveJudgeGreetGo = `package main

import (
	"fmt"
	"os"
)

// main prints a greeting line for each name given on the command line, or
// one default greeting when none are given.
func main() {
	names := os.Args[1:]
	if len(names) == 0 {
		fmt.Println("Hello, friend!")
		return
	}
	for i, name := range names {
		if i == 1 {
			continue
		}
		fmt.Printf("Hello, %s!\n", name)
	}
}
`

// liveJudgeFailingScenario is the one scenario liveJudgeGreetGo's planted
// bug actually fails: the second of two names is never greeted.
const liveJudgeFailingScenario = "s3"

// liveJudgeScenarioGiven is every liveJudgeScenarios entry's own Given text:
// the three scenarios differ only in what they run and expect, never in
// what they start from, so goconst has one definition to point at instead
// of a third repeat of the same literal.
const liveJudgeScenarioGiven = "the greet program in this repository, built and run with `go run .`"

// liveJudgeScenarios is the sealed cohort TestLiveJudge seeds: three
// behavior scenarios against the program liveJudgeGreetGo commits, run
// against the real system as a user would (prompts/judge.md), two it
// satisfies and one (liveJudgeFailingScenario) it does not.
func liveJudgeScenarios() []response.Scenario {
	return []response.Scenario{
		{
			ID: "s1", Kind: response.ScenarioKindBehavior,
			Given: liveJudgeScenarioGiven,
			When:  "it is run with no command-line arguments",
			Then:  `it prints exactly one line, "Hello, friend!", and exits 0`,
		},
		{
			ID: "s2", Kind: response.ScenarioKindBehavior,
			Given: liveJudgeScenarioGiven,
			When:  `it is run with the single argument "Ada"`,
			Then:  `it prints exactly one line, "Hello, Ada!", and exits 0`,
		},
		{
			ID: liveJudgeFailingScenario, Kind: response.ScenarioKindBehavior,
			Given: liveJudgeScenarioGiven,
			When:  `it is run with the two arguments "Ada" and "Grace", in that order`,
			Then:  `it prints two lines, "Hello, Ada!" then "Hello, Grace!", one greeting per name in the order given`,
		},
	}
}

// seedLiveJudgeCohort claims ticketID (still "queued", InsertTicket's own
// rule), stores a schema-valid plan artifact (START only checks one exists;
// RUN never reads its content, design section 7.2) and scenarios' own
// cohort, seals it, and moves the ticket straight to "judging" in one
// commit -- the same store-level shortcut moveLiveReviewTicketToReviewing
// (live_review_test.go) takes for "reviewing": this harness proves RUN
// itself, not every stage a real ticket walks through first.
func seedLiveJudgeCohort(t *testing.T, st *store.Store, ticketID int64, plan response.Plan, scenarios []response.Scenario) {
	t.Helper()
	const owner = "live-judge-seed"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := st.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("claim for seeding: %v", err)
	}
	if !claimed {
		t.Fatal("claim ticket for seeding: not claimed")
	}

	rsv, err := st.Reserve(t.Context(), ticketID, owner, expires,
		store.SessionUpsert{Job: liveJobPlanning, Runtime: runtimeNameFake}, store.RunSeed{Model: "live-judge-seed-model"})
	if err != nil {
		t.Fatalf("reserve seed run: %v", err)
	}
	runID := rsv.RunID

	planPayload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, insertErr := st.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, RunID: &runID, Type: "plan", Payload: planPayload}); insertErr != nil {
		t.Fatalf("insert plan: %v", insertErr)
	}
	for _, sc := range scenarios {
		scPayload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			t.Fatalf("marshal scenario %s: %v", sc.ID, marshalErr)
		}
		if _, insertErr := st.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, RunID: &runID, Type: "scenario", Payload: scPayload}); insertErr != nil {
			t.Fatalf("insert scenario %s: %v", sc.ID, insertErr)
		}
	}

	applied, err := st.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: "judging", Reason: "live judge harness: sealed the cohort and moved to judging",
		Seal: &store.SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: len(scenarios), At: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if !applied {
		t.Fatal("commit seed: not applied")
	}
}

// liveJudgeConfig is judge_codex_home and models.codex, both read through
// the config model, the same way serve does (PKG9-PLAN.md section 4.5):
// the owner's own zing.toml decides the real model id this harness passes
// to codex exec (jobs.judge's own "-m" argument, codexArgv), never a
// literal this file hardcodes -- the judge Codex home's own login decides
// which model ids it will even accept (a ChatGPT-plan login refuses some,
// proven by trial against its own ~/.zing/codex-judge/models_cache.json,
// a file this harness never reads itself: --ignore-user-config, design
// D18, keeps a judge run from reading anything under CODEX_HOME but
// SCENARIOS_FILE and its own session state), so the owner is the one who
// keeps this choice current, not this file.
type liveJudgeConfig struct {
	codexHome  string
	codexModel string
}

// liveJudgeRawModels is the one key loadLiveJudgeConfig reads straight from
// zing.toml, bypassing config.Load's own applyDefaults: config.Config.Models
// always carries some codex value after Load (applyDefaults substitutes
// "gpt-5.5" the moment zing.toml leaves [models] codex undefined), so
// reading it back from cfg itself could never distinguish the owner's own
// choice from that silent default -- exactly the literal this harness's
// first live runs proved the judge Codex home's own login refuses. Only
// toml.DecodeFile's own metadata (IsDefined), read directly here, can tell
// the two apart.
type liveJudgeRawModels struct {
	Models struct {
		Codex string `toml:"codex"`
	} `toml:"models"`
}

// loadLiveJudgeConfig reads liveJudgeConfig and skips, with a clear reason,
// when zing.toml cannot be loaded, the judge Codex home is not logged in
// (checkJudgeCodexLogin, serve.go), or zing.toml itself leaves [models]
// codex undefined -- naming that key so the owner knows exactly which
// zing.toml line to set -- mirroring liveClaudeOAuthToken's own
// skip-rather-than-fail rule.
func loadLiveJudgeConfig(t *testing.T, m *machine.Machine) liveJudgeConfig {
	t.Helper()
	cfgPath, err := config.DefaultPath()
	if err != nil {
		t.Skipf("resolve zing.toml path: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Skipf("load %s: %v", cfgPath, err)
	}
	if loginErr := checkJudgeCodexLogin(m, cfg.JudgeCodexHome); loginErr != nil {
		t.Skip(loginErr.Error())
	}

	var raw liveJudgeRawModels
	md, decodeErr := toml.DecodeFile(cfgPath, &raw)
	if decodeErr != nil {
		t.Skipf("re-read %s for [models] codex: %v", cfgPath, decodeErr)
	}
	if !md.IsDefined("models", "codex") || raw.Models.Codex == "" {
		t.Skipf("%s leaves [models] codex undefined; set one the judge Codex home's own login accepts (its default, %q, is not)", cfgPath, cfg.Models.Codex)
	}

	return liveJudgeConfig{codexHome: cfg.JudgeCodexHome, codexModel: raw.Models.Codex}
}

// TestLiveJudge proves a real judge round (design section 7.2) against a
// real, planted scenario failure through the real, pinned codex CLI under
// the judge sandbox profile (PKG9-PLAN.md section 19.3 task 9): one verdict
// per scenario, the failing one (s3) a "fail", and the judge's own detached
// checkout removed afterward.
//
// This builds its own store, project, and ticket directly (InsertTicket,
// InsertArtifact), the same shortcut TestLiveReview takes: RUN is one
// handler tick (after one marker-only START tick), and this harness exists
// to prove those two ticks against the real CLI, not the whole pipeline
// around them.
//
//	ZING_LIVE_CLI=1 go test ./cmd/zing -run TestLiveJudge -v -timeout 30m
func TestLiveJudge(t *testing.T) {
	if reason := liveBuildSkipReason(os.Getenv("ZING_LIVE_CLI")); reason != "" {
		t.Skip(reason)
	}

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("load machine.toml: %v", err)
	}
	liveCfg := loadLiveJudgeConfig(t, m)

	st := newLiveStore(t)

	dir := t.TempDir()
	if err = gitfixture.NewSigningRepo(t.Context(), dir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}
	if err = gitfixture.AddFile(t.Context(), dir, "go.mod", []byte(liveJudgeGoMod)); err != nil {
		t.Fatalf("add go.mod: %v", err)
	}

	// A real zing binary, ahead of the running test binary on PATH: the
	// judge's own prompt (prompts/judge.md) runs `zing scenarios` inside
	// the sandboxed codex process, and the judge profile's own ZING_BIN
	// carve-out names this test binary's own path (sandbox.resolveHost),
	// not a binary literally called "zing" (see the package doc comment).
	binDir := buildZingBinary(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	orch, err := orchestrator.New(orchestrator.Project{
		Owner: "zing-live-judge", Repo: liveJudgeRepoName, LocalPath: dir, DefaultBranch: liveDefaultBranch,
	}, liveReviewGitHub{}, orchestrator.NewRunner(), nil)
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	repoGit, err := orch.GitCommonDir(t.Context())
	if err != nil {
		t.Fatalf("git common dir: %v", err)
	}

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: liveJudgeRepoName, RepoURL: "https://example.invalid/" + liveJudgeRepoName, LocalPath: dir,
		Tracker: testServeTracker, DefaultBranch: liveDefaultBranch,
	})
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	const ticketTitle = "Add the greet program"
	ticketID, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "live-judge#1", Title: ticketTitle,
		Body: "Add a small greeting program.", State: testServeStateQueued,
	})
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}

	wt, _, err := orch.EnsureWorktree(t.Context(), ticketID, ticketTitle)
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	if addErr := gitfixture.AddFile(t.Context(), wt.Dir(), liveGreetGoFilename, []byte(liveJudgeGreetGo)); addErr != nil {
		t.Fatalf("add greet.go: %v", addErr)
	}
	sha, err := orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}

	claims := response.BuildClaims{FilesChanged: []string{liveGreetGoFilename}, TestExit: 0, LintExit: 0}
	report := response.BuildReport{
		TaskN:       1,
		BuildClaims: claims,
		Extras:      []response.ExtraClaim{}, Fences: []response.Fence{},
		Report: "Added the greet program.",
		Title:  ticketTitle, CommitSHA: &sha,
	}
	reportPayload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal build report: %v", err)
	}
	if _, insertErr := st.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "build_report", Payload: reportPayload}); insertErr != nil {
		t.Fatalf("insert build report: %v", insertErr)
	}

	planData, err := fixtures.FS.ReadFile("scripts/planning/2.xml")
	if err != nil {
		t.Fatalf("read plan fixture: %v", err)
	}
	doc, err := response.Parse(planData)
	if err != nil {
		t.Fatalf("parse plan fixture: %v", err)
	}
	ready, ok := doc.Response.(*response.ReadyResponse)
	if !ok {
		t.Fatalf("plan fixture response = %T, want *response.ReadyResponse", doc.Response)
	}
	plan := ready.Plan
	normalizeLiveReviewPlanArrays(&plan)

	scenarios := liveJudgeScenarios()
	seedLiveJudgeCohort(t, st, ticketID, plan, scenarios)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a console listener: %v", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}

	// Symlink-resolved before either consumer sees it: sandbox.LoadProfile
	// resolves DATA_DIR internally for the profile's own literal
	// SCENARIOS_FILE match (sandbox.go's resolveHost, review F044's own
	// lesson -- macOS's /var -> /private/var), but writeScenariosFile
	// (judging.go) builds the file it writes from Deps.DataDir verbatim;
	// the two must agree, or the seatbelt's literal match on
	// SCENARIOS_FILE fails with "operation not permitted" even though the
	// file genuinely exists at that path.
	dataDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	judgeProfile, err := zing.Assets.ReadFile("sandbox/judge.sb")
	if err != nil {
		t.Fatalf("read embedded judge sandbox profile: %v", err)
	}
	judgeSb := sandbox.LoadProfile("judge", judgeProfile, dataDir, nil, addr.Port)
	if !judgeSb.Available() {
		t.Fatalf("judge sandbox did not load: %s", judgeSb.Reason())
	}

	rts, err := runtime.NewSet(map[string]runtime.Runtime{runtimeNameCodex: runtime.NewCodex("")})
	if err != nil {
		t.Fatalf("build runtime set: %v", err)
	}

	// buildLiveJudgeDeps claims ticketID fresh under its own (owner,
	// expires) and returns the Deps a handler tick needs, with Reserve
	// fenced to that same claim: CommitHandlerResult always releases the
	// claim it commits under (commit.go's own UPDATE sets claim_owner and
	// claim_expires_at back to NULL on every commit, not only a state
	// change), so START's own marker-only commit releases it exactly as a
	// real dispatcher tick would -- the next tick, RUN, needs a fresh claim
	// of its own, the same way the unit suite's own pbClaim is called once
	// per tick (judging_test.go's TestJudgeRunStoresVerdicts).
	buildLiveJudgeDeps := func(owner string, expires time.Time) job.Deps {
		t.Helper()
		claimed, claimErr := st.Claim(t.Context(), ticketID, owner, expires)
		if claimErr != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", owner, claimed, claimErr)
		}
		return job.Deps{
			Store: st, Runtimes: rts, Machine: m, Models: map[string]string{modelAliasCodex: liveCfg.codexModel},
			Budget: 60 * time.Minute, Floor: response.SeverityMinor,
			Owner: owner, Expires: expires,
			Reserve: func(ctx context.Context, tID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
				return st.Reserve(ctx, tID, owner, expires, su, seed)
			},
			Sandboxes:      sandbox.Set{Build: sandbox.NotLoaded(), ReadOnly: sandbox.NotLoaded(), Judge: judgeSb},
			RequireSandbox: true,
			Commands:       job.NewCommandRunner(sandbox.Off(), false),
			Projects:       map[int64]job.Project{projectID: {Orch: orch, RepoGit: repoGit}},
			DataDir:        dataDir, LensesParallel: 7, JudgeCodexHome: liveCfg.codexHome,
		}
	}

	ticket, err := st.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("get ticket: %v", err)
	}

	startDeps := buildLiveJudgeDeps("live-judge-start", time.Now().Add(10*time.Minute).UTC().Truncate(time.Second))
	startCommit, err := job.Registry()["judging"].Run(t.Context(), ticket, startDeps)
	if err != nil {
		t.Fatalf("judge START: %v", err)
	}
	if startErr := job.ValidateCommit(ticket, startCommit); startErr != nil {
		t.Fatalf("validate START commit: %v", startErr)
	}
	applied, err := st.CommitHandlerResult(t.Context(), startCommit)
	if err != nil || !applied {
		t.Fatalf("commit START: applied=%v err=%v", applied, err)
	}

	ticket, err = st.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("get ticket after START: %v", err)
	}

	runDeps := buildLiveJudgeDeps("live-judge-run", time.Now().Add(45*time.Minute).UTC().Truncate(time.Second))
	runCommit, err := job.Registry()["judging"].Run(t.Context(), ticket, runDeps)
	if err != nil {
		t.Fatalf("judge RUN: %v", err)
	}
	if runErr := job.ValidateCommit(ticket, runCommit); runErr != nil {
		t.Fatalf("validate RUN commit: %v", runErr)
	}
	applied, err = st.CommitHandlerResult(t.Context(), runCommit)
	if err != nil || !applied {
		t.Fatalf("commit RUN: applied=%v err=%v", applied, err)
	}

	rows, err := st.Verdicts(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("verdicts: %v", err)
	}
	if len(rows) != len(scenarios) {
		t.Fatalf("verdict rows = %d, want %d (one per scenario): %+v", len(rows), len(scenarios), rows)
	}
	byScenario := make(map[string]response.VerdictArtifact, len(rows))
	for _, row := range rows {
		byScenario[row.Verdict.Scenario] = row.Verdict
	}
	for _, sc := range scenarios {
		v, has := byScenario[sc.ID]
		if !has {
			t.Errorf("no verdict for scenario %s", sc.ID)
			continue
		}
		if v.Round != 1 {
			t.Errorf("verdict %s round = %d, want 1", sc.ID, v.Round)
		}
		if v.SHA != sha {
			t.Errorf("verdict %s sha = %s, want %s", sc.ID, v.SHA, sha)
		}
		if v.Evidence == "" {
			t.Errorf("verdict %s has no evidence", sc.ID)
		}
		want := response.ResultPass
		if sc.ID == liveJudgeFailingScenario {
			want = response.ResultFail
		}
		if v.Result != want {
			t.Errorf("verdict %s result = %q, want %q; evidence: %s", sc.ID, v.Result, want, v.Evidence)
		}
	}

	judgeDir := filepath.Join(dir, ".zing", "judge", strconv.FormatInt(ticketID, 10))
	if _, statErr := os.Stat(judgeDir); !os.IsNotExist(statErr) {
		t.Errorf("judge checkout %s still exists after the run (stat err: %v), want it removed", judgeDir, statErr)
	}
}
