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
// TestLiveBuildSkipsWithoutGate, TestLiveBuildFixtureIsValid,
// TestLiveBuildHarnessOnFake, TestLiveBuildHarnessCapsRepeatedEscalations, and
// TestLiveBuildHarnessOwnerModeAnswersNothing run in every normal test suite:
// the first two prove the gate and the fixture without touching a runtime at
// all; the rest prove runLiveBuildHarness itself -- the exact sequence
// TestLiveBuild drives -- against the fake runtime and sandbox.Off(), so
// every line but the wiring runs on every CI build. ZING_LIVE_ANSWER=owner
// switches the harness to owner-answer mode: it answers nothing itself and
// waits for the console's own owner to. TestLiveBuildHarnessOnFake never
// sets it, so it stays on automatic answers.
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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	zing "zing"
	"zing/internal/bus"
	"zing/internal/config"
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

// liveHarnessT is the subset of *testing.T runLiveBuildHarness itself
// depends on (its ctx comes in as its own parameter instead, so the harness
// never calls t.Context()). It is small enough that
// TestLiveBuildHarnessCapsRepeatedEscalations and
// TestLiveBuildHarnessOwnerModeAnswersNothing can each run the harness
// against a recordingT instead of the real *testing.T: both tests prove the
// harness's own Fatalf call, and a real t.Fatalf would also fail whichever
// test called it, which is exactly what these two tests must not do to the
// test recording them. *testing.T satisfies this interface already, so
// every other caller just passes t.
type liveHarnessT interface {
	Helper()
	Cleanup(func())
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// recordingT is a liveHarnessT that records its first Fatalf call instead
// of failing a real test, then ends the calling goroutine with
// runtime.Goexit -- the same way testing.T.FailNow ends the goroutine that
// called it -- so the code under test stops exactly where a real Fatalf
// would have stopped it, while the goroutine that started it keeps running
// (design: the harness runs on its own goroutine so this Goexit cannot end
// the test function itself). Cleanup callbacks are recorded, not run
// immediately (design: goroutine.Goexit does still run this goroutine's own
// deferred functions, but a testing.T.Cleanup callback is not a defer); the
// caller runs them itself, in reverse order, through runCleanups once the
// goroutine has finished.
type recordingT struct {
	mu       sync.Mutex
	fataled  bool
	fatalMsg string
	logs     []string
	cleanups []func()
}

func (r *recordingT) Helper() {}

// Logf records the formatted message instead of writing it anywhere, so a
// caller that expects the harness to log something specific (the stale run
// directory notice) can check for it afterward through logsContaining.
func (r *recordingT) Logf(format string, args ...any) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recordingT) Cleanup(f func()) {
	r.mu.Lock()
	r.cleanups = append(r.cleanups, f)
	r.mu.Unlock()
}

func (r *recordingT) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fataled = true
	r.fatalMsg = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	goruntime.Goexit()
}

// message returns r's recorded Fatalf message, or ("", false) when Fatalf
// was never called.
func (r *recordingT) message() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatalMsg, r.fataled
}

// logsContaining reports whether any Logf call r recorded contains substr.
func (r *recordingT) logsContaining(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// runCleanups runs every Cleanup callback r recorded, in reverse
// registration order (testing.T's own order), once the goroutine that ran
// the harness against r has finished.
func (r *recordingT) runCleanups() {
	r.mu.Lock()
	cleanups := r.cleanups
	r.mu.Unlock()
	for _, f := range slices.Backward(cleanups) {
		f()
	}
}

// newLiveStore opens a fresh store.Store for one test, in its own temp
// directory, and closes it in t's own Cleanup. Split out of
// runLiveBuildHarness (which takes the store as a parameter instead of
// opening its own) so a caller that expects the harness itself to fail
// (TestLiveBuildHarnessCapsRepeatedEscalations,
// TestLiveBuildHarnessOwnerModeAnswersNothing) still has a live store to
// read afterward: the harness's own goroutine exits through
// recordingT.Fatalf's runtime.Goexit before it would ever reach a return,
// so nothing it constructed internally would otherwise survive it.
func newLiveStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "zing.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// liveModulePathFor joins name onto the fixture module directory this
// harness's plan.xml, its build fixtures, and its gitfixture repo all share
// (cmd/zing/testdata/live/module), so every reader of one path reads the
// same three files' content.
func liveModulePathFor(name string) string {
	return filepath.Join("testdata", "live", "module", name)
}

// liveBuildSkipReason reports why TestLiveBuild would skip given the
// ZING_LIVE_CLI value, or "" to run it for real (PKG8-PLAN.md section 18
// task 16). It reads runtime.GOOS directly rather than taking it as a
// parameter: every caller in this package passes the real goruntime.GOOS
// anyway (none fakes a different OS to test the darwin-only branch in
// isolation), so a parameter here would only be unparam's own flagged
// "always the same value" case. Splitting this out of TestLiveBuild lets
// TestLiveBuildSkipsWithoutGate prove the gate's own logic -- including
// that its message names the variable -- without needing ZING_LIVE_CLI
// itself set one way or the other in the process actually running the
// test suite.
func liveBuildSkipReason(liveCLI string) string {
	if liveCLI != "1" {
		return "set ZING_LIVE_CLI=1 to run the live build harness against the real claude CLI"
	}
	if goruntime.GOOS != "darwin" {
		return "the live build harness only runs on macOS: the sandbox is darwin-only (PKG8-PLAN.md section 5)"
	}
	return ""
}

// liveClaudeOAuthToken reads claude_oauth_token through the config model
// (PKG9-PLAN.md section 4.5, 19.2 task 7), the same way serve does: the
// owner's real ~/.zing/zing.toml, never an environment variable. It skips,
// with a clear reason, when the config file cannot be loaded or carries no
// token, rather than failing the live harness outright.
func liveClaudeOAuthToken(t *testing.T) string {
	t.Helper()
	cfgPath, err := config.DefaultPath()
	if err != nil {
		t.Skipf("resolve zing.toml path: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Skipf("load %s: %v", cfgPath, err)
	}
	if cfg.ClaudeOAuthToken == "" {
		t.Skip("zing.toml carries no claude_oauth_token")
	}
	return cfg.ClaudeOAuthToken
}

// TestLiveBuildSkipsWithoutGate proves liveBuildSkipReason's own gate: with
// ZING_LIVE_CLI unset (or anything but "1"), it returns a non-empty reason
// that names the variable, so a plain `go test ./...` run always skips
// TestLiveBuild rather than spending real Claude usage.
func TestLiveBuildSkipsWithoutGate(t *testing.T) {
	t.Parallel()
	reason := liveBuildSkipReason("")
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
	t.Parallel()
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

// liveJobBuild is the sessions.job value every build-turn session carries
// (machine.toml's "build" job, response.JobBuild's own string form), named
// once so goconst has one definition to point at rather than a second raw
// "build" literal alongside the unrelated "go build" exec argv above.
const liveJobBuild = "build"

// liveJobPlanning is the sessions.job and tickets.state value "planning"
// (machine.toml's own job name and response.TicketStatePlanning's string
// form), named once so goconst has one definition every live harness --
// seedLiveBuildTicket here, seedLiveJudgeCohort in live_judge_test.go --
// points at instead of its own raw "planning" literal.
const liveJobPlanning = "planning"

// liveMsgTypeQuestion is the messages.type value "question", named once so
// goconst has one definition across this file's own gate-approval fixture
// and its question-filtering checks.
const liveMsgTypeQuestion = "question"

// liveGreetGoFilename is "greet.go", the one fixture source file name every
// live harness in this package plants, declares as a build claim, or both
// (this file, live_review_test.go, live_judge_test.go), named once so
// goconst has one definition to point at.
const liveGreetGoFilename = "greet.go"

// liveFakePollInterval is runLiveBuildHarness's own poll interval for every
// caller driving the fake runtime (through runLiveBuildHarnessRecording):
// there is no real agent to avoid hammering, only a local store and the
// fake's own canned turns, so the loop can tick as fast as it finishes
// work instead of waiting out TestLiveBuild's one-real-second pace.
const liveFakePollInterval = 10 * time.Millisecond

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
		store.SessionUpsert{Job: liveJobPlanning, Runtime: "claude"}, store.RunSeed{Model: model})
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
		Next:      liveJobPlanning,
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

	// D32 (design section 22.12.3a): the seal invariant needs a confirmed
	// approval. This harness seeds the gate flow directly (it never drives
	// the real confirming turn), so it writes the minimal fixture itself: a
	// gate question, its approving answer, and the confirming marker
	// binding both to plan version 1. The gate question carries the plan
	// run's own id, matching the real shape (a run-attached question,
	// design section 4.5) that AnsweredRounds and the building handler's
	// own round grouping expect; a run-less one groups as an orphaned
	// round instead, the same shape an escalation's linked question takes.
	gateQID, approveAID, gaErr := seedLiveGateApproval(ctx, st, ticketID, reserved.RunID)
	if gaErr != nil {
		return fmt.Errorf("seed the gate approval fixture: %w", gaErr)
	}

	applied, err = st.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID:         ticketID,
		Owner:            gateOwner,
		Expires:          gateExpires,
		Next:             liveStateBuilding,
		Reason:           "live harness: sealed the cohort and moved to building",
		ResolveQuestions: []int64{gateQID},
		Seal: &store.SealRequest{
			RunID: reserved.RunID, PlanVersion: 1, ExpectedCount: len(resp.Scenarios), At: time.Now().UTC(),
		},
		GateApproval: &store.GateApproval{QuestionID: gateQID, AnswerID: approveAID, PlanVersion: 1},
	})
	if err != nil {
		return fmt.Errorf("commit the seeded gate approval: %w", err)
	}
	if !applied {
		return fmt.Errorf("seed live build ticket: ticket %d lost its claim before the gate commit", ticketID)
	}
	return nil
}

// seedLiveGateApproval seeds the minimal gate approval the seal invariant
// needs (D32, design section 22.12.1, 22.12.3a): a gate question, its
// approving answer, and the confirming marker binding both to plan version
// 1. It returns the gate question id and the approving answer id.
func seedLiveGateApproval(ctx context.Context, st *store.Store, ticketID, runID int64) (gateQID, approveAID int64, err error) {
	gatePayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindGate, State: response.QuestionStateAnswered,
		Recommended: "a", Options: []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		return 0, 0, err
	}
	gateQID, err = st.InsertMessage(ctx, store.Message{
		TicketID: ticketID, RunID: &runID, Type: liveMsgTypeQuestion, Author: "zing", State: new("answered"), Body: "Q1", Payload: gatePayload,
	})
	if err != nil {
		return 0, 0, err
	}
	approveAID, err = st.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &gateQID, Type: "answer", Author: "you", State: new("sent"), Payload: []byte(`{"option":"a"}`),
	})
	if err != nil {
		return 0, 0, err
	}
	_, err = st.InsertMessage(ctx, store.Message{
		TicketID: ticketID, ParentID: &gateQID, Type: "update", Author: "system",
		Body: fmt.Sprintf("gate confirmed run 1 plan v1 gate %d answer %d", gateQID, approveAID),
	})
	return gateQID, approveAID, err
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
	for _, name := range []string{"go.mod", "README.md", liveGreetGoFilename} {
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
	// tracker and user are left zero (nil, ""): this live harness does not
	// exercise POST /projects/{id}/pickup (PKG9-PLAN.md D29).
	handler := console.New(st, b, m, []string{"127.0.0.1"}, addr.Port, logHandler, nil, livePushToken, e2eFloor, sandboxReason, nil, "", nil)
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

// liveEscalationRetryCap bounds how many times runLiveBuildHarness answers
// consecutive escalations that share the same code (follow-up commit,
// "Cap the live harness retries"): the first live run looped, spending one
// real Sonnet run per retry on a single defect, because nothing here
// noticed the same code kept coming back rather than the build making
// progress.
const liveEscalationRetryCap = 3

// liveOwnerAnswerEnv and liveOwnerAnswerValue are the environment variable
// and value that switch runLiveBuildHarness to owner-answer mode
// (follow-up commit): ZING_LIVE_ANSWER=owner answers nothing itself and
// waits for the console's own owner to; any other value, or none, keeps
// the automatic answers TestLiveBuild and TestLiveBuildHarnessOnFake both
// rely on.
const (
	liveOwnerAnswerEnv   = "ZING_LIVE_ANSWER"
	liveOwnerAnswerValue = "owner"
)

// openEscalationQuestion is one of ticketID's still-open question messages
// that answers an escalation (its ParentID names the escalation message,
// internal/store/commit.go's escalateTx), paired with that escalation's own
// decoded payload.
type openEscalationQuestion struct {
	questionID   int64
	escalationID int64
	payload      response.EscalationPayload
}

// openEscalationQuestions returns every one of ticketID's still-open,
// escalation-linked question messages, each paired with its escalation's
// own payload; a plain build or perimeter question (no ParentID) is not
// one of these.
func openEscalationQuestions(ctx context.Context, st *store.Store, ticketID int64) ([]openEscalationQuestion, error) {
	msgs, err := st.ListMessages(ctx, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	escalations := make(map[int64]response.EscalationPayload)
	for i := range msgs {
		if msgs[i].Type != "escalation" {
			continue
		}
		var p response.EscalationPayload
		if json.Unmarshal(msgs[i].Payload, &p) == nil {
			escalations[msgs[i].ID] = p
		}
	}

	var open []openEscalationQuestion
	for i := range msgs {
		if msgs[i].Type != liveMsgTypeQuestion || msgs[i].ParentID == nil {
			continue
		}
		if msgs[i].State == nil || *msgs[i].State != "open" {
			continue
		}
		payload, ok := escalations[*msgs[i].ParentID]
		if !ok {
			continue
		}
		open = append(open, openEscalationQuestion{questionID: msgs[i].ID, escalationID: *msgs[i].ParentID, payload: payload})
	}
	return open, nil
}

// formatEscalationCapMessage is runLiveBuildHarness's Fatalf text once code
// has repeated count times (the code, the newest escalation's own What
// text, and the count, exactly as the follow-up commit asks for).
func formatEscalationCapMessage(ticketID int64, code, what string, count int) string {
	return fmt.Sprintf("ticket %d: escalation %s repeated %d times (cap %d), newest: %s",
		ticketID, code, count, liveEscalationRetryCap, what)
}

// describeOpenQuestions names every one of ticketID's currently open
// questions by id and title (its Body's first line, the same title/body
// split views.go's own splitQuestionBody renders), for the timeout message
// below: "(none)" when there is nothing open.
func describeOpenQuestions(ctx context.Context, st *store.Store, ticketID int64) (string, error) {
	open, err := st.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		return "", fmt.Errorf("questions by state: %w", err)
	}
	if len(open) == 0 {
		return "(none)", nil
	}
	titles := make([]string, len(open))
	for i := range open {
		title, _, _ := strings.Cut(open[i].Body, "\n")
		titles[i] = fmt.Sprintf("Q(message %d): %s", open[i].ID, title)
	}
	return strings.Join(titles, "; "), nil
}

// listRunRootEntries returns the names of every entry directly under
// runRoot, or nil when runRoot does not exist yet -- a sandbox cache that
// has never held a run, or, in a fake-runtime test, a run root nothing has
// written to.
func listRunRootEntries(runRoot string) ([]string, error) {
	entries, err := os.ReadDir(runRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", runRoot, err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// runLiveBuildHarness drives PKG8-PLAN.md section 18 task 16's live build
// harness to completion, sharing every line but the wiring between
// TestLiveBuild (the real claude CLI, the real sandbox) and every fake-
// runtime caller (TestLiveBuildHarnessOnFake,
// TestLiveBuildHarnessCapsRepeatedEscalations,
// TestLiveBuildHarnessOwnerModeAnswersNothing): it seeds one ticket
// directly into "building" with a stored three-task plan and a sealed
// scenario cohort (seedLiveBuildTicket), starts the real dispatcher and the
// real console over ln, and ticks until the ticket reaches "reviewing" or
// maxWait passes. st is a store the caller already opened (newLiveStore)
// and owns: a caller that expects this to fail before it ever returns
// still has a live store to read afterward. projDir is an already-built
// gitfixture "greeter" repository (newLiveFixtureRepo); the caller owns it
// so it can place a canary inside its .git directory before this runs.
//
// Two safety rules run every tick a question is open, before either mode
// answers anything (follow-up commit, "Cap the live harness retries and add
// an owner-answer mode"):
//
//  1. The retry cap. Every still-open, escalation-linked question is
//     counted by its escalation's own code, the first time this call sees
//     that escalation's id; when a code's count would reach
//     liveEscalationRetryCap, t.Fatalf fires with the code, the newest
//     escalation's What text, and the count, and nothing is answered past
//     the cap.
//  2. Owner-answer mode. When ZING_LIVE_ANSWER=owner, the harness answers
//     nothing itself: it logs "waiting on the owner: <url>" once per open
//     question (by message id) and keeps ticking, relying entirely on the
//     console's own owner to answer, bounded by the same maxWait deadline.
//     Any other value, or none, answers automatically through
//     answerAllOpenQuestions, exactly as before (design section 6.5's
//     DESCRIBE/ASK perimeter round, and a plain build question, through the
//     console's real POST /draft and POST /send).
//
// The one orchestrator this builds carries a GitHub client
// (selftestGitHub) that returns an error on every call: the build never
// reaches GitHub, and Projects, Sandbox, RequireSandbox, and Commands are
// copied straight from this call's own parameters, mirroring serve.go's own
// production wiring (buildJobProjects, serveSandbox) rather than
// hand-rolling a second shape for a test.
//
// runRoot is the sandbox run root (<CacheRoot>/run): before the dispatcher
// starts, this lists its current entries and logs each once, through
// t.Logf, as "stale run directory from an earlier run: <name>" (follow-up
// commit, "Ignore stale run directories in the live harness" -- an earlier
// live run the owner stopped by hand left one behind, and its own cleanup
// never ran to remove it). Once the ticket reaches "reviewing", any entry
// that was not already there fails the run with its name; a stale entry
// never does.
//
// pollInterval is how long each loop pass sleeps between ticks: TestLiveBuild
// passes a full second so a real agent run is not hammered with ticks while
// it thinks; the fake-runtime callers, with nothing to wait on but a local
// store and the fake's own canned turns, pass a short interval instead so
// the fixture's handful of turns do not cost a real second apiece.
func runLiveBuildHarness(ctx context.Context, t liveHarnessT, st *store.Store, projDir string, rts runtime.Set, sb sandbox.Sandbox, requireSandbox bool, cmds job.CommandRunner, ln net.Listener, runRoot string, maxWait, pollInterval time.Duration) liveBuildResult {
	t.Helper()

	staleRunEntries, err := listRunRootEntries(runRoot)
	if err != nil {
		t.Fatalf("list sandbox run root before the run: %v", err)
	}
	stale := make(map[string]bool, len(staleRunEntries))
	for _, name := range staleRunEntries {
		stale[name] = true
		t.Logf("stale run directory from an earlier run: %s", name)
	}

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("load machine.toml: %v", err)
	}

	projectID, err := st.EnsureProject(ctx, store.Project{
		Name: "greeter", RepoURL: "https://example.invalid/greeter", LocalPath: projDir,
		Tracker: testServeTracker, DefaultBranch: liveDefaultBranch,
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
		Sandboxes:      sandbox.Set{Build: sb},
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

	ownerMode := os.Getenv(liveOwnerAnswerEnv) == liveOwnerAnswerValue
	escalationCounts := map[string]int{}   // escalation code -> how many this call has counted
	countedEscalations := map[int64]bool{} // escalation message id -> already counted, so a still-open one is never counted twice
	printedWaitingOn := map[int64]bool{}   // question message id -> already logged, owner mode only

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
			afterRunEntries, listErr := listRunRootEntries(runRoot)
			if listErr != nil {
				t.Fatalf("list sandbox run root after the run: %v", listErr)
			}
			var newEntries []string
			for _, name := range afterRunEntries {
				if !stale[name] {
					newEntries = append(newEntries, name)
				}
			}
			if len(newEntries) != 0 {
				t.Fatalf("sandbox run root %s still has new entries after the run: %v", runRoot, newEntries)
			}
			return liveBuildResult{Store: st, TicketID: ticketID, Orch: orch, Worktree: mustEnsureLiveWorktree(ctx, t, orch, ticketID, ticket.Title)}
		}
		// waiting_on names which question kind is blocking (design section
		// 6.7): "questions" for a plain build question (questionOutcomeCommit)
		// or an escalation's own linked question (escalationCommit),
		// "perimeter" for the owner's file-perimeter round (askCommit). Either
		// way there is an open question to answer, so any non-nil value is
		// the same signal here.
		if ticket.WaitingOn != nil {
			escalated, escErr := openEscalationQuestions(ctx, st, ticketID)
			if escErr != nil {
				t.Fatalf("open escalation questions: %v", escErr)
			}
			for i := range escalated {
				eq := escalated[i]
				if countedEscalations[eq.escalationID] {
					continue
				}
				countedEscalations[eq.escalationID] = true
				escalationCounts[eq.payload.Code]++
				if escalationCounts[eq.payload.Code] >= liveEscalationRetryCap {
					t.Fatalf("%s", formatEscalationCapMessage(ticketID, eq.payload.Code, eq.payload.What, escalationCounts[eq.payload.Code]))
				}
			}

			if ownerMode {
				open, openErr := st.QuestionsByState(ctx, ticketID, "open")
				if openErr != nil {
					t.Fatalf("questions by state: %v", openErr)
				}
				for i := range open {
					if printedWaitingOn[open[i].ID] {
						continue
					}
					printedWaitingOn[open[i].ID] = true
					t.Logf("waiting on the owner: %s", srv.URL)
					fmt.Println("waiting on the owner:", srv.URL) //nolint:forbidigo // follow-up commit: printed to standard output as well as the test log, matching the console-URL line above
				}
			} else if answerErr := answerAllOpenQuestions(ctx, st, srv.URL, ticketID); answerErr != nil {
				t.Fatalf("answer open questions: %v", answerErr)
			}
		}
		if time.Now().After(deadline) {
			desc, descErr := describeOpenQuestions(ctx, st, ticketID)
			if descErr != nil {
				t.Fatalf("ticket %d did not reach reviewing within %s (state=%s, waiting_on=%v); describe open questions: %v",
					ticketID, maxWait, ticket.State, ticket.WaitingOn, descErr)
			}
			t.Fatalf("ticket %d did not reach reviewing within %s (state=%s, waiting_on=%v, open questions: %s)",
				ticketID, maxWait, ticket.State, ticket.WaitingOn, desc)
		}
		time.Sleep(pollInterval)
	}
}

// mustEnsureLiveWorktree reattaches the worktree building's own step 0
// already created for ticketID, using the same (ticketID, title) pair every
// building tick passes EnsureWorktree (internal/job/building.go), so the
// caller gets the real Worktree value to inspect commits against.
func mustEnsureLiveWorktree(ctx context.Context, t liveHarnessT, orch *orchestrator.Orchestrator, ticketID int64, title string) orchestrator.Worktree {
	t.Helper()
	wt, _, err := orch.EnsureWorktree(ctx, ticketID, title)
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

// newLiveFakeRunRoot returns a run-root path for a fake-runtime caller,
// which never wraps a real sandboxed process (sandbox.Off() is always
// unavailable) and so has no real <CacheRoot>/run to point at: a fresh,
// unpopulated directory under t's own temp dir stands in for one. It need
// not exist yet -- listRunRootEntries treats a missing run root as empty,
// the same as a sandbox cache that has never held a run.
func newLiveFakeRunRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "run")
}

// TestLiveBuildHarnessOnFake proves runLiveBuildHarness itself -- the exact
// sequence TestLiveBuild drives -- against the fake runtime and
// sandbox.Off() with RequireSandbox false (PKG8-PLAN.md section 18 task
// 16): the three fixture build turns, the perimeter DESCRIBE/ASK round for
// task 2's undeclared README.md edit, and task 3's own farewell-wording
// question all land, and the ticket reaches "reviewing" with three signed
// commits.
func TestLiveBuildHarnessOnFake(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	st := newLiveStore(t)
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
	res := runLiveBuildHarness(t.Context(), t, st, projDir, rts, sb, false, job.NewCommandRunner(sb, false), ln, newLiveFakeRunRoot(t), 2*time.Minute, liveFakePollInterval)

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
//
// Not parallel: it calls t.Setenv("PATH", ...) to prepend a stub claude
// binary, and it already skips by default (ZING_LIVE_CLI), so it never
// shares the suite's wall clock budget with the parallel tests anyway.
func TestLiveBuild(t *testing.T) {
	if reason := liveBuildSkipReason(os.Getenv("ZING_LIVE_CLI")); reason != "" {
		t.Skip(reason)
	}

	oauthToken := liveClaudeOAuthToken(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	gocacheOut, err := exec.CommandContext(t.Context(), "go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOCACHE: %v", err)
	}
	gocache := strings.TrimSpace(string(gocacheOut))

	st := newLiveStore(t)
	projDir := newLiveFixtureRepo(t)

	homeCanary := placeLiveCanary(t, home)
	gitCanary := placeLiveCanary(t, filepath.Join(projDir, ".git"))
	tmpCanary := placeLiveCanary(t, os.TempDir())
	cacheCanary := placeLiveCanary(t, gocache)

	binDir := buildZingBinary(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rts, err := runtime.NewSet(map[string]runtime.Runtime{
		runtimeNameClaude: runtime.NewClaude("", oauthToken), runtimeNameCodex: runtime.NewCodex(""),
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

	res := runLiveBuildHarness(t.Context(), t, st, projDir, rts, sb, true, job.NewCommandRunner(sb, true), ln, runRoot, 60*time.Minute, time.Second)

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
		if s.Job == liveJobBuild && s.Runtime != "claude" {
			t.Errorf("build session %d runtime = %q, want claude", s.ID, s.Runtime)
		}
	}

	msgs, err := res.Store.ListMessages(t.Context(), res.TicketID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var perimeterQuestions, buildQuestions int
	for i := range msgs {
		if msgs[i].Type != liveMsgTypeQuestion {
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

	assertLiveCanaryUnchanged(t, homeCanary)
	assertLiveCanaryUnchanged(t, gitCanary)
	assertLiveCanaryUnchanged(t, tmpCanary)
	assertLiveCanaryUnchanged(t, cacheCanary)
}

// runLiveBuildHarnessRecording runs runLiveBuildHarness against a
// recordingT on its own goroutine, so the harness's own Fatalf -- the retry
// cap, a timeout, or anything else -- ends that goroutine (recordingT.Fatalf's
// runtime.Goexit) instead of the goroutine running the *testing.T this
// function was itself called from. It waits for that goroutine to finish,
// runs every Cleanup the harness registered (closing the console server it
// started), and returns the recordingT so the caller can inspect whether it
// failed, its message, and everything it logged (TestLiveBuildHarnessIgnoresStaleRunDir
// reads the stale-run-directory notice this way, without that Logf call
// needing anywhere else to go).
func runLiveBuildHarnessRecording(t *testing.T, st *store.Store, projDir string, rts runtime.Set, sb sandbox.Sandbox, requireSandbox bool, cmds job.CommandRunner, ln net.Listener, runRoot string, maxWait time.Duration) *recordingT {
	t.Helper()
	rec := &recordingT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLiveBuildHarness(t.Context(), rec, st, projDir, rts, sb, requireSandbox, cmds, ln, runRoot, maxWait, liveFakePollInterval)
	}()
	<-done
	rec.runCleanups()
	return rec
}

// runLiveBuildHarnessExpectingFatal is runLiveBuildHarnessRecording for a
// caller that expects the harness to fail: it fails the real t when the
// harness returned instead of calling Fatalf, and otherwise returns the
// recorded message.
func runLiveBuildHarnessExpectingFatal(t *testing.T, st *store.Store, projDir string, rts runtime.Set, sb sandbox.Sandbox, requireSandbox bool, cmds job.CommandRunner, ln net.Listener, runRoot string, maxWait time.Duration) string {
	t.Helper()
	rec := runLiveBuildHarnessRecording(t, st, projDir, rts, sb, requireSandbox, cmds, ln, runRoot, maxWait)
	msg, fataled := rec.message()
	if !fataled {
		t.Fatal("runLiveBuildHarness returned instead of calling Fatalf; want it to fail")
	}
	return msg
}

// TestLiveBuildHarnessCapsRepeatedEscalations proves item 1 of the
// follow-up commit "Cap the live harness retries and add an owner-answer
// mode": a fake build turn that always returns the same environment error
// escalates, the harness answers "Retry" (through the generic
// answerAllOpenQuestions, its first offered option) twice, each retry
// reserving a fresh build session and run, and on the third occurrence of
// the same code it fails instead of answering, naming the code, the count,
// and the newest escalation's own What text -- and no fourth run is ever
// reserved.
func TestLiveBuildHarnessCapsRepeatedEscalations(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	st := newLiveStore(t)
	projDir := newLiveFixtureRepo(t)

	fake := runtime.NewFake(os.DirFS(filepath.Join("testdata", "live-escalation-cap")))
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
	msg := runLiveBuildHarnessExpectingFatal(t, st, projDir, rts, sb, false, job.NewCommandRunner(sb, false), ln, newLiveFakeRunRoot(t), 2*time.Minute)

	for _, want := range []string{string(response.EscalationCodeEnvironment), "3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fatal message %q does not contain %q", msg, want)
		}
	}

	tickets, err := st.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("list all tickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(tickets))
	}

	sessions, err := st.SessionsForTicket(t.Context(), tickets[0].ID)
	if err != nil {
		t.Fatalf("sessions for ticket: %v", err)
	}
	buildSessions := make(map[int64]bool, len(sessions))
	for _, s := range sessions {
		if s.Job == liveJobBuild {
			buildSessions[s.ID] = true
		}
	}

	runs, err := st.RunsForTicket(t.Context(), tickets[0].ID)
	if err != nil {
		t.Fatalf("runs for ticket: %v", err)
	}
	var buildRuns int
	for _, r := range runs {
		if buildSessions[r.SessionID] {
			buildRuns++
		}
	}
	// Every retry reserves a fresh build session (retryFreshRun), each with
	// exactly one run, so the build run count and the build session count
	// are the same thing here; the cap must stop before a fourth of either.
	if buildRuns != liveEscalationRetryCap {
		t.Fatalf("build runs = %d, want %d (the cap must stop before a fourth is reserved)", buildRuns, liveEscalationRetryCap)
	}
}

// TestLiveBuildHarnessOwnerModeAnswersNothing proves item 2 of the
// follow-up commit: with ZING_LIVE_ANSWER=owner, a fake build turn whose
// first turn asks a question is left open for the whole, short maxWait --
// the harness never drafts or sends an answer, which a still-"open"
// question in the store after the timeout proves directly -- and the
// timeout's own Fatalf names that open question.
//
// Not parallel: it calls t.Setenv(liveOwnerAnswerEnv, ...) below.
func TestLiveBuildHarnessOwnerModeAnswersNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Setenv(liveOwnerAnswerEnv, liveOwnerAnswerValue)

	st := newLiveStore(t)
	projDir := newLiveFixtureRepo(t)

	fake := runtime.NewFake(os.DirFS(filepath.Join("testdata", "live-owner-mode")))
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
	// Three ticks, one second apart, plus headroom: long enough to prove
	// the harness keeps ticking without answering, short enough that a
	// wedged owner-answer path fails this test promptly instead of hanging
	// it.
	const shortMaxWait = 3500 * time.Millisecond
	msg := runLiveBuildHarnessExpectingFatal(t, st, projDir, rts, sb, false, job.NewCommandRunner(sb, false), ln, newLiveFakeRunRoot(t), shortMaxWait)

	if !strings.Contains(msg, "did not reach reviewing") {
		t.Errorf("fatal message %q does not describe a timeout", msg)
	}

	tickets, err := st.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("list all tickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want 1", len(tickets))
	}
	ticketID := tickets[0].ID

	open, err := st.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("questions by state: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open questions = %d, want 1 (the harness must never have answered it)", len(open))
	}
	title, _, _ := strings.Cut(open[0].Body, "\n")
	if !strings.Contains(msg, title) {
		t.Errorf("fatal message %q does not name the open question %q", msg, title)
	}

	answered, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	answerCount := 0
	for i := range answered {
		if answered[i].Type == "answer" {
			answerCount++
		}
	}
	// D32 (design section 22.12.3a): seedLiveBuildTicket now seeds one real
	// "answer" message of its own -- the gate's approving pick, needed to
	// satisfy the seal invariant -- so exactly one is expected here, not
	// zero; the harness itself, in owner mode, must still never send a
	// second one.
	if answerCount != 1 {
		t.Errorf("answer messages = %d, want exactly 1 (the seeded gate approval; the harness itself must send none in owner mode)", answerCount)
	}
}

// TestLiveBuildHarnessIgnoresStaleRunDir proves the follow-up commit "Ignore
// stale run directories in the live harness": a run root that already holds
// one directory before the harness ever starts is not this run's own doing
// -- the second real run's one failing assertion was exactly this, a
// directory an earlier, hand-stopped live run left behind, whose own
// cleanup never ran. The harness must pass the same three-task build
// TestLiveBuildHarnessOnFake drives, logging the pre-existing entry's name
// once, and must not fail over it.
func TestLiveBuildHarnessIgnoresStaleRunDir(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	st := newLiveStore(t)
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

	runRoot := newLiveFakeRunRoot(t)
	if mkdirErr := os.MkdirAll(runRoot, 0o755); mkdirErr != nil {
		t.Fatalf("mkdir run root: %v", mkdirErr)
	}
	const staleName = "stale-run-from-an-earlier-live-run"
	if mkdirErr := os.Mkdir(filepath.Join(runRoot, staleName), 0o755); mkdirErr != nil {
		t.Fatalf("mkdir stale run directory: %v", mkdirErr)
	}

	sb := sandbox.Off()
	rec := runLiveBuildHarnessRecording(t, st, projDir, rts, sb, false, job.NewCommandRunner(sb, false), ln, runRoot, 2*time.Minute)

	if msg, fataled := rec.message(); fataled {
		t.Fatalf("harness failed over a stale run directory it should have ignored: %s", msg)
	}
	if !rec.logsContaining("stale run directory from an earlier run: " + staleName) {
		t.Error("harness did not log the stale run directory's own message naming it")
	}

	entries, err := os.ReadDir(runRoot)
	if err != nil {
		t.Fatalf("read run root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != staleName {
		t.Errorf("run root entries = %v, want only the untouched stale directory %q", entries, staleName)
	}
}
