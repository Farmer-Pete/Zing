// live_build_test.go is the opt-in live harness for the real building state
// machine (PKG8-PLAN.md section 18, task 16): the one place this repository
// proves a real three-task build against the real, pinned claude CLI inside
// the real sandbox, rather than the fake runtime every other test in this
// package drives.
//
// TestLiveBuild spends the owner's real Claude usage and writes to the
// owner's real home directory's canary path, the fixture repo's .git, the
// host TMPDIR, and the host Go build cache; it never runs by default, in
// CI, or from any other test. It checks ZING_LIVE_CLI itself and skips
// unless it is exactly "1", and only on macOS (design N9, section 5: the
// sandbox is darwin-only, so a real build run refuses to start everywhere
// else). Run it explicitly, deliberately:
//
//	ZING_LIVE_CLI=1 go test ./cmd/zing -run TestLiveBuild -v -timeout 70m
//
// TestLiveBuildSkipsWithoutGate, TestLiveBuildFixtureIsValid, and
// TestLiveBuildHarnessOnFake run in every normal test suite: the first two
// prove the gate and the fixture without touching a runtime at all: the
// third proves runLiveBuildHarness itself -- the exact sequence TestLiveBuild
// drives -- against the fake runtime and sandbox.Off(), so every line but
// the wiring runs on every CI build.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	zing "zing"
	"zing/internal/bus"
	"zing/internal/console"
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

// liveModulePathFor joins name onto the fixture module directory this
// harness's plan.xml, its build fixtures, and its gitfixture repo all share
// (cmd/zing/testdata/live/module), so every reader of one path reads the
// same three files' content.
func liveModulePathFor(name string) string {
	return filepath.Join("testdata", "live", "module", name)
}

// liveBuildSkipReason reports why TestLiveBuild would skip given goos and
// the ZING_LIVE_CLI value, or "" to run it for real (PKG8-PLAN.md section
// 18 task 16). Splitting this out of TestLiveBuild lets
// TestLiveBuildSkipsWithoutGate prove the gate's own logic -- including
// that its message names the variable -- without needing ZING_LIVE_CLI
// itself set one way or the other in the process actually running the
// test suite.
func liveBuildSkipReason(goos, liveCLI string) string {
	if liveCLI != "1" {
		return "set ZING_LIVE_CLI=1 to run the live build harness against the real claude CLI"
	}
	if goos != "darwin" {
		return "the live build harness only runs on macOS: the sandbox is darwin-only (PKG8-PLAN.md section 5)"
	}
	return ""
}

// TestLiveBuildSkipsWithoutGate proves liveBuildSkipReason's own gate: with
// ZING_LIVE_CLI unset (or anything but "1"), it returns a non-empty reason
// that names the variable, so a plain `go test ./...` run always skips
// TestLiveBuild rather than spending real Claude usage.
func TestLiveBuildSkipsWithoutGate(t *testing.T) {
	reason := liveBuildSkipReason(goruntime.GOOS, "")
	if reason == "" {
		t.Fatal("liveBuildSkipReason returned no reason with ZING_LIVE_CLI unset, want a skip reason")
	}
	if !strings.Contains(reason, "ZING_LIVE_CLI") {
		t.Errorf("skip reason = %q, want it to name ZING_LIVE_CLI", reason)
	}
}

// TestLiveBuildFixtureIsValid proves the fixture plan itself, with no
// runtime and no store: cmd/zing/testdata/live/plan.xml parses as a
// planning/ready document and passes response.Validate, its tasks are
// numbered 1 to 3, and every file the plan declares with action="modify"
// already exists under testdata/live/module -- the plan can only modify a
// file the fixture module already carries; a "create" file need not exist
// yet, since building it is exactly what the task does.
func TestLiveBuildFixtureIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "live", "plan.xml"))
	if err != nil {
		t.Fatalf("read plan.xml: %v", err)
	}
	doc, err := response.Parse(data)
	if err != nil {
		t.Fatalf("parse plan.xml: %v", err)
	}
	if errs := response.Validate(doc, response.ValidateContext{Job: response.JobPlanning, Kind: response.KindFeature}); len(errs) > 0 {
		t.Fatalf("validate plan.xml: %v", errs[0])
	}
	resp, ok := doc.Response.(*response.ReadyResponse)
	if !ok {
		t.Fatalf("plan.xml response type = %T, want *response.ReadyResponse", doc.Response)
	}

	tasks := resp.Plan.Delivery.Tasks
	if len(tasks) != 3 {
		t.Fatalf("plan.xml has %d tasks, want 3", len(tasks))
	}
	for i, task := range tasks {
		if task.N != i+1 {
			t.Errorf("task[%d].N = %d, want %d", i, task.N, i+1)
		}
	}

	for _, f := range resp.Plan.Delivery.Files {
		if f.Action != response.FileActionModify {
			continue
		}
		p := liveModulePathFor(filepath.FromSlash(f.Path))
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("plan declares %s as modify, but it does not exist under testdata/live/module: %v", f.Path, statErr)
		}
	}
}

// liveArtifactTypePlan, liveArtifactTypeClaims, and liveArtifactTypeScenario
// are the artifacts.type literals seedLiveBuildTicket stores the fixture
// plan's cohort under, matching internal/store/schemas/artifacts' own file
// names and internal/job/planning.go's own (unexported) artifactTypePlan,
// artifactTypeClaims, artifactTypeScenario constants.
const (
	liveArtifactTypePlan     = "plan"
	liveArtifactTypeClaims   = "claims"
	liveArtifactTypeScenario = "scenario"
)

// liveStateBuilding is the tickets.state seedLiveBuildTicket's gate commit
// moves the ticket to, named once so goconst has one definition to point at
// rather than a second raw "building" literal alongside production_test.go's
// and selftest.go's own.
const liveStateBuilding = "building"

// liveDefaultBranch matches gitfixture.NewSigningRepo's own fixed branch
// ("main"), named once so goconst has one definition to point at rather than
// a second raw literal.
const liveDefaultBranch = "main"

// liveTestCmd is the fixture "greeter" module's own test command, named once
// so goconst has one definition to point at.
const liveTestCmd = "go test ./..."

// normalizeLivePlanArrays mirrors internal/job/planning.go's own
// (unexported) normalizePlanArrays: a Plan decoded from XML leaves an
// absent array element as a nil Go slice, and json.Marshal writes a nil
// slice as JSON null, but the plan artifact's schema requires each of these
// fields to be present as a JSON array. Called once, right before marshaling
// the plan for storage.
func normalizeLivePlanArrays(p *response.Plan) {
	if p.Design.Changes == nil {
		p.Design.Changes = []response.Change{}
	}
	if p.Design.Types == nil {
		p.Design.Types = []response.TypeDef{}
	}
	for i := range p.Design.Types {
		if p.Design.Types[i].Transitions == nil {
			p.Design.Types[i].Transitions = []response.Transition{}
		}
	}
	if p.Design.Migrations.Items == nil {
		p.Design.Migrations.Items = []response.Migration{}
	}
	if p.Delivery.Deletions.Items == nil {
		p.Delivery.Deletions.Items = []response.Fence{}
	}
}

// seedLiveBuildTicket lands ticketID directly in "building" with resp's
// plan, claims, and scenario cohort stored and sealed (PKG8-PLAN.md section
// 18 task 16: "through the store's own validated writes, the way SeedDemo
// does"), through the same two Claim/Reserve/CommitHandlerResult sequences a
// real ready-outcome commit (internal/job/planning.go's readyCommit) and a
// real gate approval (gateApprove) use: the first terminalizes a "planning"
// run with the cohort's artifacts and leaves the ticket in "planning"; the
// second seals that same run's cohort and moves the ticket to "building".
// This never drives classify or planning turns itself: the harness exists
// to prove building, not the stages before it.
func seedLiveBuildTicket(ctx context.Context, st *store.Store, ticketID int64, resp *response.ReadyResponse, model string) error {
	const planOwner = "live-harness-plan"
	planExpires := time.Now().Add(time.Hour)
	claimed, err := st.Claim(ctx, ticketID, planOwner, planExpires)
	if err != nil {
		return fmt.Errorf("claim for the seeded plan run: %w", err)
	}
	if !claimed {
		return fmt.Errorf("seed live build ticket: ticket %d is already claimed", ticketID)
	}

	reserved, err := st.Reserve(ctx, ticketID, planOwner, planExpires,
		store.SessionUpsert{Job: "planning", Runtime: "claude"}, store.RunSeed{Model: model})
	if err != nil {
		return fmt.Errorf("reserve the seeded plan run: %w", err)
	}

	plan := resp.Plan
	normalizeLivePlanArrays(&plan)
	planPayload, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	claimsPayload, err := json.Marshal(resp.Claims)
	if err != nil {
		return fmt.Errorf("marshal claims: %w", err)
	}

	runID := reserved.RunID
	artifacts := []store.Artifact{
		{Type: liveArtifactTypePlan, RunID: &runID, Payload: planPayload},
		{Type: liveArtifactTypeClaims, RunID: &runID, Payload: claimsPayload},
	}
	for _, sc := range resp.Scenarios {
		scPayload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			return fmt.Errorf("marshal scenario %s: %w", sc.ID, marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{Type: liveArtifactTypeScenario, RunID: &runID, Payload: scPayload})
	}

	outcome := "ready"
	exitCode, agentSeconds := 0, 5
	applied, err := st.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID:  ticketID,
		Owner:     planOwner,
		Expires:   planExpires,
		Runs:      []store.Run{{ID: reserved.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Artifacts: artifacts,
		Next:      "planning",
		Reason:    "live harness: seeded a ready cohort",
	})
	if err != nil {
		return fmt.Errorf("commit the seeded plan run: %w", err)
	}
	if !applied {
		return fmt.Errorf("seed live build ticket: ticket %d lost its claim before the plan commit", ticketID)
	}

	const gateOwner = "live-harness-gate"
	gateExpires := time.Now().Add(time.Hour)
	claimed, err = st.Claim(ctx, ticketID, gateOwner, gateExpires)
	if err != nil {
		return fmt.Errorf("claim for the seeded gate approval: %w", err)
	}
	if !claimed {
		return fmt.Errorf("seed live build ticket: ticket %d is already claimed for the gate", ticketID)
	}

	applied, err = st.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID,
		Owner:    gateOwner,
		Expires:  gateExpires,
		Next:     liveStateBuilding,
		Reason:   "live harness: sealed the cohort and moved to building",
		Seal: &store.SealRequest{
			RunID: reserved.RunID, PlanVersion: 1, ExpectedCount: len(resp.Scenarios), At: time.Now().UTC(),
		},
	})
	if err != nil {
		return fmt.Errorf("commit the seeded gate approval: %w", err)
	}
	if !applied {
		return fmt.Errorf("seed live build ticket: ticket %d lost its claim before the gate commit", ticketID)
	}
	return nil
}

// newLiveFixtureRepo builds the "greeter" gitfixture repository this
// harness builds against (PKG8-PLAN.md section 18 task 16, section 9.4):
// gitfixture.NewSigningRepo's own initial signed commit, then this
// package's own testdata/live/module/{go.mod,README.md,greet.go} added on
// top, each its own signed commit, so HEAD carries exactly the base state
// plan.xml's tasks build from. It returns the repository's directory,
// which the caller passes to runLiveBuildHarness; a caller that also needs
// to place a canary inside the repository's .git directory (TestLiveBuild)
// does so between this call and that one.
func newLiveFixtureRepo(t *testing.T) string {
	t.Helper()
	ctx := t.Context()

	dir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := gitfixture.NewSigningRepo(ctx, dir); err != nil {
		t.Fatalf("build gitfixture repo: %v", err)
	}
	for _, name := range []string{"go.mod", "README.md", "greet.go"} {
		content, err := os.ReadFile(liveModulePathFor(name))
		if err != nil {
			t.Fatalf("read fixture module %s: %v", name, err)
		}
		if err := gitfixture.AddFile(ctx, dir, name, content); err != nil {
			t.Fatalf("add %s to gitfixture repo: %v", name, err)
		}
	}
	return dir
}

// buildZingBinary builds this module's zing binary into dir, named "zing",
// so a sandboxed build run's PATH (prepended with dir by the caller) can
// resolve and run `zing validate` the same way a real installed zing would
// (PKG8-PLAN.md section 18 task 16, section 5.3's PATH rule).
func buildZingBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "zing")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "zing/cmd/zing") //nolint:gosec // G204: fixed argv, test-only
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build zing/cmd/zing: %v\n%s", err, out)
	}
	return dir
}

// newLiveConsoleServer starts the real console (design section 9.2, task
// 15) over ln, an already-reserved listener: reserved first, by the
// caller, so its port is known before the caller renders the sandbox
// profile's own console-deny rule (sandbox.Load's consolePort parameter),
// which must name the real port a sandboxed run must never reach.
func newLiveConsoleServer(st *store.Store, b *bus.Broker, m *machine.Machine, logHandler *console.Handler, ln net.Listener, sandboxReason string) (*httptest.Server, error) {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}
	const livePushToken = "live-harness-push-token" //nolint:gosec // not a credential: a fixed placeholder no route in this harness checks
	handler := console.New(st, b, m, []string{"127.0.0.1"}, addr.Port, logHandler, nil, livePushToken, e2eFloor, sandboxReason)
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		return nil, fmt.Errorf("close the placeholder listener: %w", err)
	}
	srv.Listener = ln
	srv.Start()
	return srv, nil
}

// answerAllOpenQuestions answers every one of ticketID's currently open
// questions through the console's own real POST /draft then POST /send
// (design section 6.7), the same two calls a browser's chip click and send
// chord make: an item-kind question (perimeter, review) accepts every item;
// an option-kind question (question, gate, split, merge) picks its first
// offered option. It does nothing when there is nothing open.
func answerAllOpenQuestions(ctx context.Context, st *store.Store, base string, ticketID int64) error {
	open, err := st.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		return fmt.Errorf("questions by state: %w", err)
	}
	if len(open) == 0 {
		return nil
	}

	for i := range open {
		var payload response.QuestionPayload
		if err := json.Unmarshal(open[i].Payload, &payload); err != nil {
			return fmt.Errorf("unmarshal question %d payload: %w", open[i].ID, err)
		}

		if len(payload.Items) > 0 {
			for _, item := range payload.Items {
				body := fmt.Sprintf(`{"ticket":%d,"question":%d,"item":{"ref":%q,"decision":"accept"}}`,
					ticketID, open[i].ID, item.Ref)
				if err := postSelftestConsole(ctx, base, "/draft", body); err != nil {
					return fmt.Errorf("draft item %q for question %d: %w", item.Ref, open[i].ID, err)
				}
			}
			continue
		}

		if len(payload.Options) == 0 {
			return fmt.Errorf("question %d has neither items nor options", open[i].ID)
		}
		chosen := payload.Options[0].Key
		body := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":%q}`, ticketID, open[i].ID, chosen)
		if err := postSelftestConsole(ctx, base, "/draft", body); err != nil {
			return fmt.Errorf("draft option %q for question %d: %w", chosen, open[i].ID, err)
		}
	}

	sendBody := fmt.Sprintf(`{"ticket":%d}`, ticketID)
	return postSelftestConsole(ctx, base, "/send", sendBody)
}

// liveBuildResult is what runLiveBuildHarness hands back once ticketID has
// reached "reviewing": enough for a caller to run its own assertions
// (signed commits, session runtimes, canaries) without the harness itself
// hard-coding which ones a given caller needs -- TestLiveBuild and
// TestLiveBuildHarnessOnFake check different things against the same shape.
type liveBuildResult struct {
	Store    *store.Store
	TicketID int64
	Orch     *orchestrator.Orchestrator
	Worktree orchestrator.Worktree
}

// runLiveBuildHarness drives PKG8-PLAN.md section 18 task 16's live build
// harness to completion, sharing every line but the wiring between
// TestLiveBuild (the real claude CLI, the real sandbox) and
// TestLiveBuildHarnessOnFake (the fake runtime, sandbox.Off()): it seeds one
// ticket directly into "building" with a stored three-task plan and a
// sealed scenario cohort (seedLiveBuildTicket), starts the real dispatcher
// and the real console over ln, and ticks until the ticket reaches
// "reviewing" or maxWait passes, answering every open question along the
// way (design section 6.5's DESCRIBE/ASK perimeter round, and task 3's own
// farewell-wording question) through the console's real POST /draft and
// POST /send. projDir is an already-built gitfixture "greeter" repository
// (newLiveFixtureRepo); the caller owns it so it can place a canary inside
// its .git directory before this runs.
//
// The one orchestrator this builds carries a GitHub client
// (selftestGitHub) that returns an error on every call: the build never
// reaches GitHub, and Projects, Sandbox, RequireSandbox, and Commands are
// copied straight from this call's own parameters, mirroring serve.go's own
// production wiring (buildJobProjects, serveSandbox) rather than
// hand-rolling a second shape for a test.
func runLiveBuildHarness(t *testing.T, projDir string, rts runtime.Set, sb sandbox.Sandbox, requireSandbox bool, cmds job.CommandRunner, ln net.Listener, maxWait time.Duration) liveBuildResult {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("load machine.toml: %v", err)
	}

	projectID, err := st.EnsureProject(ctx, store.Project{
		Name: "greeter", RepoURL: "https://example.invalid/greeter", LocalPath: projDir,
		Tracker: "github", DefaultBranch: liveDefaultBranch,
	})
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	orch, err := orchestrator.New(orchestrator.Project{
		Owner: "zing-live-harness", Repo: "greeter", LocalPath: projDir, DefaultBranch: liveDefaultBranch,
		BuildWritableRoots: sandboxBuildWritableRoots(sb),
	}, selftestGitHub{}, orchestrator.NewRunner(), nil)
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	repoGit, err := orch.GitCommonDir(ctx)
	if err != nil {
		t.Fatalf("git common dir: %v", err)
	}

	planData, err := os.ReadFile(filepath.Join("testdata", "live", "plan.xml"))
	if err != nil {
		t.Fatalf("read plan.xml: %v", err)
	}
	doc, err := response.Parse(planData)
	if err != nil {
		t.Fatalf("parse plan.xml: %v", err)
	}
	resp, ok := doc.Response.(*response.ReadyResponse)
	if !ok {
		t.Fatalf("plan.xml response type = %T, want *response.ReadyResponse", doc.Response)
	}

	ticketID, err := st.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: "live-1",
		Title: resp.Plan.Overview.Objective, Body: resp.Plan.Overview.Context,
		State: "queued",
	})
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}
	if seedErr := seedLiveBuildTicket(ctx, st, ticketID, resp, e2eModels[modelAliasSonnet]); seedErr != nil {
		t.Fatalf("seed live build ticket: %v", seedErr)
	}

	b := bus.New()
	d, err := zdispatch.New(st, nil, b, m, job.Registry(), nil, zdispatch.Config{
		Interval: time.Second, MaxParallel: 1, Owner: "live-harness",
		Models: e2eModels, Budget: e2eBudget, Floor: e2eFloor,
		Projects: map[int64]job.Project{
			projectID: {Orch: orch, RepoGit: repoGit, TestCmd: liveTestCmd, LintCmd: "go vet ./..."},
		},
		Sandbox:        sb,
		RequireSandbox: requireSandbox,
		Commands:       cmds,
	}, rts)
	if err != nil {
		t.Fatalf("build dispatcher: %v", err)
	}

	logHandler := console.NewHandler(io.Discard, new(slog.LevelVar), nil)
	srv, err := newLiveConsoleServer(st, b, m, logHandler, ln, sb.Reason())
	if err != nil {
		t.Fatalf("start console server: %v", err)
	}
	t.Cleanup(srv.Close)
	t.Logf("console: %s", srv.URL)
	fmt.Println("console:", srv.URL) //nolint:forbidigo // PKG8-PLAN.md section 18 task 16: printed to standard output as well as the test log

	// A plain for{} loop, exited only through the "reviewing" return below:
	// Go's own terminating-statement rule (a for loop with no break) is what
	// lets this function end here with no further statement and no separate
	// "missing return" placeholder, since t.Fatalf itself (runtime.Goexit)
	// is not something the compiler can see as terminating.
	deadline := time.Now().Add(maxWait)
	for {
		if tickErr := d.Tick(ctx); tickErr != nil {
			t.Fatalf("tick: %v", tickErr)
		}
		ticket, getErr := st.GetTicket(ctx, ticketID)
		if getErr != nil {
			t.Fatalf("get ticket: %v", getErr)
		}
		if ticket.State == "reviewing" {
			return liveBuildResult{Store: st, TicketID: ticketID, Orch: orch, Worktree: mustEnsureLiveWorktree(t, orch, ticketID, ticket.Title)}
		}
		// waiting_on names which question kind is blocking (design section
		// 6.7): "questions" for a plain build question (questionOutcomeCommit),
		// "perimeter" for the owner's file-perimeter round (askCommit). Either
		// way there is an open question to answer, so any non-nil value is
		// the same signal here.
		if ticket.WaitingOn != nil {
			if answerErr := answerAllOpenQuestions(ctx, st, srv.URL, ticketID); answerErr != nil {
				t.Fatalf("answer open questions: %v", answerErr)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticket %d did not reach reviewing within %s (state=%s, waiting_on=%v)",
				ticketID, maxWait, ticket.State, ticket.WaitingOn)
		}
		time.Sleep(time.Second)
	}
}

// mustEnsureLiveWorktree reattaches the worktree building's own step 0
// already created for ticketID, using the same (ticketID, title) pair every
// building tick passes EnsureWorktree (internal/job/building.go), so the
// caller gets the real Worktree value to inspect commits against.
func mustEnsureLiveWorktree(t *testing.T, orch *orchestrator.Orchestrator, ticketID int64, title string) orchestrator.Worktree {
	t.Helper()
	wt, _, err := orch.EnsureWorktree(t.Context(), ticketID, title)
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	return wt
}

// liveCanary is one canary file TestLiveBuild places before the harness
// runs and checks byte-identical after (PKG8-PLAN.md section 18 task 16):
// its own random name and fixed content prove nothing outside the run's
// declared writable roots was touched. It removes itself in its own
// t.Cleanup regardless of the test's outcome.
type liveCanary struct {
	path    string
	content []byte
}

// placeLiveCanary writes a fresh liveCanary under dir.
func placeLiveCanary(t *testing.T, dir string) liveCanary {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("read random canary name bytes: %v", err)
	}
	name := "zing-live-canary-" + hex.EncodeToString(buf) + ".txt"
	content := []byte("zing live build harness canary: " + name + "\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write canary %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return liveCanary{path: path, content: content}
}

// assertLiveCanaryUnchanged fails the test if c's file is missing or its
// content changed since placeLiveCanary wrote it.
func assertLiveCanaryUnchanged(t *testing.T, c liveCanary) {
	t.Helper()
	got, err := os.ReadFile(c.path)
	if err != nil {
		t.Errorf("canary %s: %v", c.path, err)
		return
	}
	if !bytes.Equal(got, c.content) {
		t.Errorf("canary %s changed: got %q, want %q", c.path, got, c.content)
	}
}

// mustSandboxCacheRoot reads sb's own CacheRoot host field through
// ParamsFor, the only way to reach it (sandbox.Sandbox carries no exported
// getter of its own; serve.go's own sandboxBuildWritableRoots reads it the
// same way).
func mustSandboxCacheRoot(t *testing.T, sb sandbox.Sandbox) string {
	t.Helper()
	p, err := sb.ParamsFor("", "", "")
	if err != nil {
		t.Fatalf("sandbox params for cache root: %v", err)
	}
	return p.CacheRoot
}

// TestLiveBuildHarnessOnFake proves runLiveBuildHarness itself -- the exact
// sequence TestLiveBuild drives -- against the fake runtime and
// sandbox.Off() with RequireSandbox false (PKG8-PLAN.md section 18 task
// 16): the three fixture build turns, the perimeter DESCRIBE/ASK round for
// task 2's undeclared README.md edit, and task 3's own farewell-wording
// question all land, and the ticket reaches "reviewing" with three signed
// commits.
func TestLiveBuildHarnessOnFake(t *testing.T) {
	projDir := newLiveFixtureRepo(t)

	fake := runtime.NewFake(os.DirFS(filepath.Join("testdata", "live")))
	rts, err := runtime.NewSet(map[string]runtime.Runtime{
		runtimeNameClaude: fake, runtimeNameCodex: fake, runtimeNameFake: fake,
	})
	if err != nil {
		t.Fatalf("build runtime set: %v", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a console listener: %v", err)
	}

	sb := sandbox.Off()
	res := runLiveBuildHarness(t, projDir, rts, sb, false, job.NewCommandRunner(sb, false), ln, 2*time.Minute)

	shas, err := res.Orch.BranchCommits(t.Context(), res.Worktree)
	if err != nil {
		t.Fatalf("branch commits: %v", err)
	}
	if len(shas) != 3 {
		t.Fatalf("branch commits = %d, want 3", len(shas))
	}
}

// TestLiveBuild is PKG8-PLAN.md section 18 task 16's own live harness: a
// real three-task build on the real claude CLI inside the real sandbox. See
// the package doc comment for how to run it. It asserts: three signed
// commits; one or more perimeter questions; one or more build questions;
// every build session's runtime is claude; every sandboxed run directory is
// gone; and its four canaries -- one in the home root, one in the fixture
// repository's .git, one in the host TMPDIR, one in the host Go build cache
// -- are byte-identical after the run.
func TestLiveBuild(t *testing.T) {
	if reason := liveBuildSkipReason(goruntime.GOOS, os.Getenv("ZING_LIVE_CLI")); reason != "" {
		t.Skip(reason)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	gocacheOut, err := exec.CommandContext(t.Context(), "go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOCACHE: %v", err)
	}
	gocache := strings.TrimSpace(string(gocacheOut))

	projDir := newLiveFixtureRepo(t)

	homeCanary := placeLiveCanary(t, home)
	gitCanary := placeLiveCanary(t, filepath.Join(projDir, ".git"))
	tmpCanary := placeLiveCanary(t, os.TempDir())
	cacheCanary := placeLiveCanary(t, gocache)

	binDir := buildZingBinary(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rts, err := runtime.NewSet(map[string]runtime.Runtime{
		runtimeNameClaude: runtime.NewClaude(""), runtimeNameCodex: runtime.NewCodex(""),
	})
	if err != nil {
		t.Fatalf("build runtime set: %v", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a console listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}

	dataDir := t.TempDir()
	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read embedded sandbox profile: %v", err)
	}
	sb := sandbox.Load(profile, dataDir, nil, addr.Port)
	if !sb.Available() {
		t.Fatalf("sandbox did not load: %s", sb.Reason())
	}
	runRoot := filepath.Join(mustSandboxCacheRoot(t, sb), "run")

	res := runLiveBuildHarness(t, projDir, rts, sb, true, job.NewCommandRunner(sb, true), ln, 60*time.Minute)

	shas, err := res.Orch.BranchCommits(t.Context(), res.Worktree)
	if err != nil {
		t.Fatalf("branch commits: %v", err)
	}
	if len(shas) != 3 {
		t.Fatalf("branch commits = %d, want 3", len(shas))
	}
	for _, sha := range shas {
		signed, signedErr := res.Orch.SignedStatus(t.Context(), res.Worktree, sha)
		if signedErr != nil {
			t.Errorf("signed status %s: %v", sha, signedErr)
			continue
		}
		if !signed {
			t.Errorf("commit %s is not signed", sha)
		}
	}

	sessions, err := res.Store.SessionsForTicket(t.Context(), res.TicketID)
	if err != nil {
		t.Fatalf("sessions for ticket: %v", err)
	}
	for _, s := range sessions {
		if s.Job == "build" && s.Runtime != "claude" {
			t.Errorf("build session %d runtime = %q, want claude", s.ID, s.Runtime)
		}
	}

	msgs, err := res.Store.ListMessages(t.Context(), res.TicketID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var perimeterQuestions, buildQuestions int
	for i := range msgs {
		if msgs[i].Type != "question" {
			continue
		}
		var payload response.QuestionPayload
		if unmarshalErr := json.Unmarshal(msgs[i].Payload, &payload); unmarshalErr != nil {
			t.Fatalf("unmarshal question %d payload: %v", msgs[i].ID, unmarshalErr)
		}
		// This ticket never reaches planning, review, split, or merge, so
		// kind perimeter and kind question here can only have come from
		// building's own perimeter job and the build job's own question
		// outcome, respectively (design section 6.5, 6.6).
		switch payload.Kind {
		case response.QuestionKindPerimeter:
			perimeterQuestions++
		case response.QuestionKindQuestion:
			buildQuestions++
		}
	}
	if perimeterQuestions == 0 {
		t.Error("no perimeter questions were posted, want one or more (task 2's undeclared README.md edit)")
	}
	if buildQuestions == 0 {
		t.Error("no build questions were posted, want one or more (task 3's farewell wording)")
	}

	entries, err := os.ReadDir(runRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read sandbox run root %s: %v", runRoot, err)
	}
	if len(entries) != 0 {
		t.Errorf("sandbox run directory %s still has %d entries after the run", runRoot, len(entries))
	}

	assertLiveCanaryUnchanged(t, homeCanary)
	assertLiveCanaryUnchanged(t, gitCanary)
	assertLiveCanaryUnchanged(t, tmpCanary)
	assertLiveCanaryUnchanged(t, cacheCanary)
}
