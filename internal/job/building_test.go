// building_test.go tests the real building handler (building.go, task 9):
// step 0 (plan, worktree, branch reconcile, with verified adoption of an
// unrecorded commit), step 2 (the next unit), RUN's first turn, CHECK, and
// LAND. It reuses skeleton_test.go's shared fixtures (newJobTestStore, claim,
// apply, fakeRuntime, getTicket) and planning_test.go's scriptedRuntime, and
// drives job.Registry()["building"].Run directly, exactly as
// skeleton_test.go's and planning_test.go's own handler tests do. A real
// store, the fake runtime (fixtures/scripts/build/*), and real git
// (gitfixture, through seedQueuedGitBackedTicket) back every test here.
package job_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"zing/internal/job"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// The fixture's own declared paths and file content (fixtures/scripts/build,
// fixtures/scripts/planning/2.xml), named once so goconst has nothing to
// flag across this file's many scenarios.
const (
	helloTxt          = "hello.txt"
	extraTxt          = "extra.txt"
	helloWorldContent = "hello, world\n"

	// testExtraPath and testExtraReason are the single-extra scenario's own
	// path and builder reason, shared across the DESCRIBE and ASK tests
	// that only need one extra to prove their own point.
	testExtraPath   = "extra1.go"
	testExtraReason = "needed a helper"
)

// ---- shared building fixtures ----------------------------------------------

// buildTicketInBuilding drives a fresh, git-backed ticket from queued
// through planning (real classify, first turn, the fixture Q1 answer, the
// resume that stores the three-task ready cohort, the clean review tick,
// and the owner's gate approval) into "building", with its plan cohort
// sealed and ready to build (design section 6). rt is the *runtime.Fake
// that drove it, still serving fixtures/scripts/build/{1,2,3}/1.xml for
// whichever test keeps ticking it.
func buildTicketInBuilding(t *testing.T) (*store.Store, runtime.Runtime, int64) {
	t.Helper()
	s := newJobTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedGitBackedTicket(t, s)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	advancePlanningWithAnAnswer(t, s, rt, ticketID)
	if ticket := getTicket(t, s, ticketID); ticket.State != testStateBuilding {
		t.Fatalf("buildTicketInBuilding: ticket state = %q, want building", ticket.State)
	}
	return s, rt, ticketID
}

// claimForBuild is claimWithRuntimes (planning_test.go), plus the sandbox,
// command runner, and project wiring the real building handler needs
// (PKG8-PLAN.md section 4.3, 10): this suite always drives the fake
// runtime, never a real sandboxed process.
func claimForBuild(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) job.Deps {
	t.Helper()
	deps := claimWithRuntimes(t, s, rt, ticketID)
	deps.Sandbox = sandbox.Off()
	deps.RequireSandbox = false
	deps.Commands = job.NewCommandRunner(sandbox.Off(), false)
	deps.Projects = buildJobTestProjects(t, s)
	return deps
}

// buildWorktreeFor returns ticket's own job.Project and its worktree,
// reading deps.Projects rather than claiming the ticket again: EnsureWorktree
// and the git reads below it need no claim at all, so a caller inspecting
// the worktree around a Run call it will also apply reuses that same deps
// instead of spending a second claim on an already-claimed ticket.
func buildWorktreeFor(t *testing.T, deps job.Deps, ticket store.Ticket) (job.Project, orchestrator.Worktree) {
	t.Helper()
	proj, ok := deps.Projects[ticket.ProjectID]
	if !ok {
		t.Fatalf("buildWorktreeFor: no git-backed project wired for ticket %d", ticket.ID)
	}
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	return proj, wt
}

// buildStep builds a scriptedStep whose Response is a minimal, hand-built
// BuildResponse: a canned "ok" outcome no fixture script can express with
// the exact claims and extras a test needs (design section 4.1).
func buildStep(filesChanged []string, testExit, lintExit int, extras []response.ExtraClaim, sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response: &response.BuildResponse{
			Job: response.JobBuild, Outcome: response.OutcomeOk,
			Claims: response.BuildClaims{FilesChanged: filesChanged, TestExit: testExit, LintExit: lintExit},
			Extras: extras,
			Report: "did something",
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// rawGitCommit stages files and commits them without going through
// orchestrator.CommitTask, so a test can build a commit CommitTask itself
// would never produce (unsigned, or hand-made outside Zing's own git
// calls), simulating exactly what section 6.1's adoption checks guard
// against.
func rawGitCommit(t *testing.T, dir string, files []string, title string, sign bool) {
	t.Helper()
	addArgs := append([]string{"-C", dir, "add"}, files...)
	if out, err := exec.CommandContext(t.Context(), "git", addArgs...).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed argv, test-only
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitArgs := []string{"-C", dir, "commit", "-q", "-m", title}
	if sign {
		commitArgs = append(commitArgs, "-S")
	} else {
		commitArgs = append(commitArgs, "--no-gpg-sign")
	}
	if out, err := exec.CommandContext(t.Context(), "git", commitArgs...).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed argv, test-only
		t.Fatalf("git commit: %v: %s", err, out)
	}
}

// findUnlandedReport returns reports' unlanded (no CommitSHA) row for
// taskN, failing the test when none exists.
func findUnlandedReport(t *testing.T, reports []store.BuildReportRow, taskN int) store.BuildReportRow {
	t.Helper()
	for i := range reports {
		if reports[i].Report.TaskN == taskN && reports[i].Report.CommitSHA == nil {
			return reports[i]
		}
	}
	t.Fatalf("no unlanded build_report for task %d among %+v", taskN, reports)
	return store.BuildReportRow{}
}

// ---- the clean three-task build ---------------------------------------------

// TestBuildThreeTasksThreeSignedCommits is task 9's own demo (PKG8-PLAN.md
// section 19): the three fixture tasks land as three signed commits and the
// ticket reaches reviewing.
func TestBuildThreeTasksThreeSignedCommits(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)
	advanceBuilding(t, s, rt, ticketID)

	final := getTicket(t, s, ticketID)
	if final.State != testStateReviewing {
		t.Fatalf("final ticket state = %q, want reviewing", final.State)
	}

	reports, err := s.BuildReports(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	var shas []string
	for _, r := range reports {
		if r.Report.CommitSHA != nil {
			shas = append(shas, *r.Report.CommitSHA)
		}
	}
	if len(shas) != 3 {
		t.Fatalf("landed commit shas = %v, want exactly 3", shas)
	}

	deps := claimForBuild(t, s, rt, ticketID)
	proj, wt := buildWorktreeFor(t, deps, final)
	for _, sha := range shas {
		signed, statusErr := proj.Orch.SignedStatus(t.Context(), wt, sha)
		if statusErr != nil {
			t.Errorf("SignedStatus(%s): %v", sha, statusErr)
		}
		if !signed {
			t.Errorf("commit %s is not signed", sha)
		}
	}
}

// ---- RUN --------------------------------------------------------------------

// TestBuildRunGoesThroughRunJob proves RUN's first turn goes through runJob
// (design section 6.3): the session's runtime is "claude" (machine.toml's
// own build job), the reserved run's task_n is set, and the fake runtime's
// file effect landed inside the real worktree directory (WorkDir), not
// wherever else it might have run.
func TestBuildRunGoesThroughRunJob(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	maxResumes := deps.Machine.Jobs["build"].MaxResumes
	session, _, err := s.LatestSession(t.Context(), ticketID, "build", maxResumes)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if session.Runtime != "claude" {
		t.Errorf("session.Runtime = %q, want claude", session.Runtime)
	}

	_, _, run, ok, err := s.UnitSession(t.Context(), ticketID, 1, maxResumes)
	if err != nil {
		t.Fatalf("UnitSession: %v", err)
	}
	if !ok {
		t.Fatal("UnitSession: ok = false, want true")
	}
	if run.TaskN == nil || *run.TaskN != 1 {
		t.Errorf("run.TaskN = %v, want 1", run.TaskN)
	}

	_, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), helloTxt)); statErr != nil {
		t.Errorf("hello.txt not found in the worktree: %v (want the fake runtime's file effect written to WorkDir)", statErr)
	}
}

// ---- CHECK: a claim mismatch -------------------------------------------------

// TestBuildClaimMismatchWritesPendingMarker proves a files_changed mismatch
// (the run claims hello.txt changed, but the scripted runtime writes
// nothing) makes CHECK write the "claim errors pending" marker rather than
// landing (design section 6.4).
func TestBuildClaimMismatchWritesPendingMarker(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	mismatchRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "mismatch-sess")}}
	deps := claimForBuild(t, s, mismatchRT, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, rt, ticketID)
	commit2, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if commit2.Next != "" {
		t.Errorf("commit.Next = %q, want empty (a mismatch must not land)", commit2.Next)
	}
	if len(commit2.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one pending marker", commit2.Messages)
	}
	body := commit2.Messages[0].Body
	if !strings.HasPrefix(body, "claim errors pending run ") {
		t.Errorf("marker body = %q, want the claim errors pending run prefix", body)
	}
	if !strings.Contains(body, "claims/files_changed") {
		t.Errorf("marker body = %q, want a files_changed mismatch line", body)
	}
}

// TestCheckRejectsTruthfulFailingCommands proves a build that truthfully
// claims a failing command still fails CHECK (design section 6.4, 6.5's own
// CheckCommandsPassed doc): the observed re-run's exits, not the claim,
// decide.
func TestCheckRejectsTruthfulFailingCommands(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	// Truthfully claims both commands failed, but writes nothing: the real
	// re-run also observes test_exit 1 (no hello.txt), agreeing with the
	// claim -- and CHECK still refuses to land.
	failRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 1, 1, nil, "fail-sess")}}
	deps := claimForBuild(t, s, failRT, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, rt, ticketID)
	commit2, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if commit2.Next != "" || len(commit2.Artifacts) != 0 {
		t.Fatalf("commit = %+v, want no transition and no landed build_report", commit2)
	}
	if len(commit2.Messages) != 1 || !strings.HasPrefix(commit2.Messages[0].Body, "claim errors pending run ") {
		t.Fatalf("commit.Messages = %+v, want one claim errors pending marker", commit2.Messages)
	}
	if !strings.Contains(commit2.Messages[0].Body, "claims/test_exit: observed 1, want 0") {
		t.Errorf("marker body = %q, want a test_exit observed-1 line", commit2.Messages[0].Body)
	}
}

// TestCheckReadsTreeAfterCommands proves CHECK diffs the tree after both
// commands ran, not before (design section 6.4 step 2): a test command
// that writes both the declared file and an undeclared one produces a new
// extra, and CHECK's own worktree read reflects exactly what the commands
// left behind. The run's own claim names both paths truthfully (a
// scriptedRuntime, since the fixture's own fake turn writes nothing itself,
// so there is no seam here to claim what the project commands are about to
// do): CHECK's job is to diff after running them, not to predict them.
func TestCheckReadsTreeAfterCommands(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	extras := []response.ExtraClaim{{Path: extraTxt, Reason: "the test command writes it"}}
	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, extraTxt}, 0, 0, extras, "reads-tree-sess")}}
	deps := claimForBuild(t, s, scriptRT, ticketID)
	overrideProj := deps.Projects[ticket.ProjectID]
	overrideProj.TestCmd = "touch extra.txt && printf 'hello, world\\n' > hello.txt && test -f hello.txt"
	overrideProj.LintCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: overrideProj}

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, scriptRT, ticketID)
	deps2.Projects = map[int64]job.Project{ticket.ProjectID: overrideProj}
	commit2, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(commit2.Messages) != 1 || !strings.HasPrefix(commit2.Messages[0].Body, "claims ok run ") {
		t.Fatalf("commit.Messages = %+v, want a claims-ok marker (extra.txt is an undeclared extra)", commit2.Messages)
	}
	apply(t, s, ticket, commit2)

	_, wt := buildWorktreeFor(t, deps2, getTicket(t, s, ticketID))
	got, err := os.ReadFile(filepath.Join(wt.Dir(), helloTxt))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(got) != helloWorldContent {
		t.Errorf("hello.txt = %q, want %q (the test command's own write)", got, helloWorldContent)
	}
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), extraTxt)); statErr != nil {
		t.Errorf("extra.txt not found in the worktree: %v (want the test command's own write)", statErr)
	}
}

// ---- LAND -------------------------------------------------------------------

// TestBuildLandStagesOnlyChangedPaths proves LAND commits exactly the
// changed paths CHECK just read, nothing else (design section 6.7 step 1).
func TestBuildLandStagesOnlyChangedPaths(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps = claimForBuild(t, s, rt, ticketID)
	commit, err = job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // CHECK + LAND
	if err != nil {
		t.Fatalf("CHECK/LAND: %v", err)
	}
	apply(t, s, ticket, commit)

	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one landed build_report", commit.Artifacts)
	}
	var landed response.BuildReport
	if unmarshalErr := json.Unmarshal(commit.Artifacts[0].Payload, &landed); unmarshalErr != nil {
		t.Fatalf("unmarshal landed build_report: %v", unmarshalErr)
	}
	if landed.CommitSHA == nil {
		t.Fatal("landed.CommitSHA = nil, want a sha")
	}

	proj, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	changes, err := proj.Orch.CommitChanges(t.Context(), wt, *landed.CommitSHA)
	if err != nil {
		t.Fatalf("CommitChanges: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != helloTxt {
		t.Errorf("commit changes = %+v, want exactly [hello.txt]", changes)
	}
}

// ---- worktree ensured logging ------------------------------------------------

// TestBuildEnsuresWorktreeOncePerTick proves task 9a's third fix: the
// "worktree ensured" event (design section 11) fires exactly once per
// handler invocation, and its created field tells a tick that actually
// created the worktree apart from a later tick that only reopens it (RUN
// creates it; the following CHECK+LAND tick must report created=false).
func TestBuildEnsuresWorktreeOncePerTick(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN: creates the worktree
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps = claimForBuild(t, s, rt, ticketID)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	if _, err = job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps); err != nil { // CHECK + LAND
		t.Fatalf("CHECK/LAND: %v", err)
	}

	var ensured []string
	for line := range strings.SplitSeq(strings.TrimRight(logBuf.String(), "\n"), "\n") {
		if strings.Contains(line, "worktree ensured") {
			ensured = append(ensured, line)
		}
	}
	if len(ensured) != 1 {
		t.Fatalf("worktree ensured log lines = %d (%v), want exactly one per tick", len(ensured), ensured)
	}
	if !strings.Contains(ensured[0], "created=false") {
		t.Errorf("worktree ensured log = %q, want created=false (the worktree already existed from the RUN tick)", ensured[0])
	}
}

// ---- step 0: no stored plan, misnumbered tasks ------------------------------

// TestBuildNoStoredPlanEscalates proves step 0.1 (design section 6.1): a
// ticket that somehow reaches building with no stored plan escalates
// environment/"no stored plan for this ticket", with no run.
func TestBuildNoStoredPlanEscalates(t *testing.T) {
	s := newJobTestStore(t)
	ticketID := seedQueuedGitBackedTicket(t, s)

	owner := "force-building"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: testStateBuilding, Reason: "test forces building with no plan",
	})
	if err != nil || !applied {
		t.Fatalf("force to building: applied=%v err=%v", applied, err)
	}

	ticket := getTicket(t, s, ticketID)
	if ticket.State != testStateBuilding {
		t.Fatalf("ticket state = %q, want building", ticket.State)
	}
	deps := claimForBuild(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	const wantBody = "environment: no stored plan for this ticket"
	if commit.Escalation.Body != wantBody {
		t.Errorf("escalation body = %q, want %q", commit.Escalation.Body, wantBody)
	}
	if commit.Escalation.RunID != nil {
		t.Errorf("escalation RunID = %v, want nil (no run)", *commit.Escalation.RunID)
	}
}

// TestBuildRejectsMisnumberedStoredPlan proves step 0's own task-numbering
// guard: a stored plan whose tasks are not numbered 1..n in order
// escalates environment/"the stored plan's tasks are not numbered 1 to n"
// rather than reaching a task lookup that could panic or silently misfile
// progress.
func TestBuildRejectsMisnumberedStoredPlan(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	planArt, ok, err := s.GetArtifact(t.Context(), ticketID, "plan")
	if err != nil || !ok {
		t.Fatalf("GetArtifact(plan): ok=%v err=%v", ok, err)
	}
	var plan response.Plan
	if unmarshalErr := json.Unmarshal(planArt.Payload, &plan); unmarshalErr != nil {
		t.Fatalf("unmarshal plan: %v", unmarshalErr)
	}
	if len(plan.Delivery.Tasks) < 2 {
		t.Fatalf("fixture plan has %d tasks, want at least 2 to misnumber", len(plan.Delivery.Tasks))
	}
	plan.Delivery.Tasks[1].N = 3 // 1, 3, 3 is not 1..n in order
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, insertErr := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "plan", Version: planArt.Version + 1, Payload: payload}); insertErr != nil {
		t.Fatalf("InsertArtifact: %v", insertErr)
	}

	deps := claimForBuild(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	const wantBody = "environment: the stored plan's tasks are not numbered 1 to n"
	if commit.Escalation.Body != wantBody {
		t.Errorf("escalation body = %q, want %q", commit.Escalation.Body, wantBody)
	}
}

// ---- step 0: branch reconcile -----------------------------------------------

// TestBuildEscalatesWhenRecordedCommitMissing proves section 6.1 step 5's
// mismatch branch: a recorded commit no longer on the branch (rewritten
// history) escalates environment/"the ticket branch does not hold the
// commits Zing recorded".
func TestBuildEscalatesWhenRecordedCommitMissing(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, rt, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps = claimForBuild(t, s, rt, ticketID)
	commit, err = job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // CHECK + LAND
	if err != nil {
		t.Fatalf("CHECK/LAND: %v", err)
	}
	apply(t, s, ticket, commit)

	_, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	if out, resetErr := exec.CommandContext(t.Context(), "git", "-C", wt.Dir(), "reset", "--hard", "HEAD~1").CombinedOutput(); resetErr != nil {
		t.Fatalf("git reset --hard: %v: %s", resetErr, out)
	}

	ticket = getTicket(t, s, ticketID)
	deps = claimForBuild(t, s, rt, ticketID)
	commit, err = job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	const wantBody = "environment: the ticket branch does not hold the commits Zing recorded"
	if commit.Escalation.Body != wantBody {
		t.Errorf("escalation body = %q, want %q", commit.Escalation.Body, wantBody)
	}
}

// TestBuildEscalatesOnForeignCommits proves section 6.1 step 5's "anything
// else" branch: more than one unrecorded commit on the branch (two
// hand-made commits) escalates environment/"the ticket branch holds
// commits Zing did not record".
func TestBuildEscalatesOnForeignCommits(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, rt, ticketID)
	_, wt := buildWorktreeFor(t, deps, ticket)

	for i, name := range []string{"foreign1.txt", "foreign2.txt"} {
		if writeErr := os.WriteFile(filepath.Join(wt.Dir(), name), []byte("x"), 0o600); writeErr != nil {
			t.Fatalf("write %s: %v", name, writeErr)
		}
		rawGitCommit(t, wt.Dir(), []string{name}, fmt.Sprintf("hand-made commit %d", i), true)
	}

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	const wantBody = "environment: the ticket branch holds commits Zing did not record"
	if commit.Escalation.Body != wantBody {
		t.Errorf("escalation body = %q, want %q", commit.Escalation.Body, wantBody)
	}
}

// ---- step 0.5: verified adoption of an unrecorded commit --------------------

// prepareUnrecordedCommit drives task 1's RUN turn with a scripted response
// claiming claimFiles/lintExit (test_exit is always claimed truthfully as
// 0; only the "claims failed" case needs a claim that disagrees with the
// real re-run, and it varies lint_exit for that) and extras, writes
// writeFiles to the worktree and commits exactly their keys (signed unless
// signed is false), under title (the report's own Title when empty) --
// without ever calling the handler's own LAND. It is section 6.1's
// single-unrecorded-commit scenario every TestBuildAdoptionChecks case
// starts from.
func prepareUnrecordedCommit(t *testing.T, claimFiles []string, lintExit int, extras []response.ExtraClaim, titleOverride string, writeFiles map[string]string, signed bool) (*store.Store, int64, job.Deps, orchestrator.Worktree) {
	t.Helper()
	s, _, ticketID := buildTicketInBuilding(t)

	ticket := getTicket(t, s, ticketID)
	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep(claimFiles, 0, lintExit, extras, "adopt-sess")}}
	deps := claimForBuild(t, s, scriptRT, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	reports, err := s.BuildReports(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	report := findUnlandedReport(t, reports, 1)

	proj, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	approved := make([]string, 0, len(writeFiles))
	for path, content := range writeFiles {
		if writeErr := os.WriteFile(filepath.Join(wt.Dir(), path), []byte(content), 0o600); writeErr != nil {
			t.Fatalf("write %s: %v", path, writeErr)
		}
		approved = append(approved, path)
	}
	title := report.Report.Title
	if titleOverride != "" {
		title = titleOverride
	}
	if signed {
		if _, commitErr := proj.Orch.CommitTask(t.Context(), wt, approved, orchestrator.CommitMessage{Title: title, Fences: report.Report.Fences}); commitErr != nil {
			t.Fatalf("CommitTask: %v", commitErr)
		}
	} else {
		rawGitCommit(t, wt.Dir(), approved, title, false)
	}

	// Rebuild Projects fresh: a case that mutates deps.Projects's TestCmd or
	// LintCmd below starts from an unshared copy.
	deps.Projects = buildJobTestProjects(t, s)
	return s, ticketID, deps, wt
}

// assertAdoptionFails runs one more building tick and asserts it escalated
// the unverifiable-commit case with wantTried as the failing check's name
// (design section 6.1's own adoption table).
func assertAdoptionFails(t *testing.T, s *store.Store, ticketID int64, deps job.Deps, wantTried string) {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("adopt tick: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an unverifiable-commit escalation")
	}
	if commit.Escalation.Payload.Tried != wantTried {
		t.Errorf("escalation Tried = %q, want %q", commit.Escalation.Payload.Tried, wantTried)
	}
	const wantWhat = "the ticket branch holds a commit Zing cannot verify"
	if !strings.Contains(commit.Escalation.Body, wantWhat) {
		t.Errorf("escalation body = %q, want it to contain %q", commit.Escalation.Body, wantWhat)
	}
}

// TestBuildAdoptsVerifiedCommit proves the happy adoption path (design
// section 6.1): a signed commit that passes every check adopts cleanly,
// landing the build_report with that commit's own sha, without CommitTask
// ever running again.
func TestBuildAdoptsVerifiedCommit(t *testing.T) {
	s, ticketID, _, wt := prepareUnrecordedCommit(t, []string{helloTxt}, 0, nil, "", map[string]string{helloTxt: helloWorldContent}, true)

	sha, err := orchestratorHeadSHA(t, wt.Dir())
	if err != nil {
		t.Fatalf("read HEAD sha: %v", err)
	}

	// prepareUnrecordedCommit's own RUN already claimed and released the
	// ticket once (apply()); this tick needs its own fresh claim.
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, fakeRuntime(t), ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("adopt tick: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want none (a verified commit adopts cleanly)", commit.Escalation)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one landed build_report", commit.Artifacts)
	}
	var landed response.BuildReport
	if unmarshalErr := json.Unmarshal(commit.Artifacts[0].Payload, &landed); unmarshalErr != nil {
		t.Fatalf("unmarshal landed build_report: %v", unmarshalErr)
	}
	if landed.CommitSHA == nil || *landed.CommitSHA != sha {
		t.Errorf("landed.CommitSHA = %v, want %s", landed.CommitSHA, sha)
	}
	apply(t, s, ticket, commit)

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want still building (two more tasks remain)", final.State)
	}
}

// TestBuildAdoptionChecks proves section 6.1's seven-check adoption table:
// one case per row, each escalating with that row's own Tried name.
func TestBuildAdoptionChecks(t *testing.T) {
	t.Run("commands failed", func(t *testing.T) {
		s, ticketID, deps, _ := prepareUnrecordedCommit(t, []string{helloTxt}, 0, nil, "", map[string]string{helloTxt: helloWorldContent}, true)
		ticket := getTicket(t, s, ticketID)
		badProj := deps.Projects[ticket.ProjectID]
		badProj.TestCmd = "false"
		deps.Projects = map[int64]job.Project{ticket.ProjectID: badProj}
		assertAdoptionFails(t, s, ticketID, deps, "commands failed")
	})

	t.Run("tree not clean", func(t *testing.T) {
		s, ticketID, deps, wt := prepareUnrecordedCommit(t, []string{helloTxt}, 0, nil, "", map[string]string{helloTxt: helloWorldContent}, true)
		if writeErr := os.WriteFile(filepath.Join(wt.Dir(), "untracked.txt"), []byte("x"), 0o600); writeErr != nil {
			t.Fatalf("write untracked file: %v", writeErr)
		}
		assertAdoptionFails(t, s, ticketID, deps, "tree not clean")
	})

	t.Run("unsigned", func(t *testing.T) {
		s, ticketID, deps, _ := prepareUnrecordedCommit(t, []string{helloTxt}, 0, nil, "", map[string]string{helloTxt: helloWorldContent}, false)
		assertAdoptionFails(t, s, ticketID, deps, "unsigned")
	})

	t.Run("no report", func(t *testing.T) {
		s, rt, ticketID := buildTicketInBuilding(t)
		ticket := getTicket(t, s, ticketID)
		deps := claimForBuild(t, s, rt, ticketID)
		proj, wt := buildWorktreeFor(t, deps, ticket)
		if writeErr := os.WriteFile(filepath.Join(wt.Dir(), helloTxt), []byte(helloWorldContent), 0o600); writeErr != nil {
			t.Fatalf("write hello.txt: %v", writeErr)
		}
		// No RUN ever happened for task 1, so no build_report exists at all.
		if _, commitErr := proj.Orch.CommitTask(t.Context(), wt, []string{helloTxt}, orchestrator.CommitMessage{Title: "Task 1: Add the greeting file"}); commitErr != nil {
			t.Fatalf("CommitTask: %v", commitErr)
		}
		deps.Projects = buildJobTestProjects(t, s)
		assertAdoptionFails(t, s, ticketID, deps, "no report")
	})

	t.Run("subject mismatch", func(t *testing.T) {
		s, ticketID, deps, _ := prepareUnrecordedCommit(t, []string{helloTxt}, 0, nil, "a subject the report never gave", map[string]string{helloTxt: helloWorldContent}, true)
		assertAdoptionFails(t, s, ticketID, deps, "subject mismatch")
	})

	t.Run("claims failed", func(t *testing.T) {
		// The run claims lint_exit 1; the fixture project's real lint
		// command ("true") always exits 0, so the adoption re-run
		// disagrees with the stored claim.
		s, ticketID, deps, _ := prepareUnrecordedCommit(t, []string{helloTxt}, 1, nil, "", map[string]string{helloTxt: helloWorldContent}, true)
		assertAdoptionFails(t, s, ticketID, deps, "claims failed")
	})

	t.Run("undeclared path", func(t *testing.T) {
		extras := []response.ExtraClaim{{Path: extraTxt, Reason: "needed it"}}
		files := map[string]string{helloTxt: helloWorldContent, extraTxt: "extra\n"}
		s, ticketID, deps, _ := prepareUnrecordedCommit(t, []string{helloTxt, extraTxt}, 0, extras, "", files, true)
		assertAdoptionFails(t, s, ticketID, deps, "undeclared path")
	})
}

// orchestratorHeadSHA reads dir's own HEAD sha directly, so a test can learn
// what prepareUnrecordedCommit just committed without re-deriving it from
// BranchCommits.
func orchestratorHeadSHA(t *testing.T, dir string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").CombinedOutput() //nolint:gosec // G204: fixed argv, test-only
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// ---- DESCRIBE, ASK (task 10) ------------------------------------------------

// perimeterStep builds a scriptedStep whose Response is a minimal, hand-built
// PerimeterResponse (design section 4.1, 6.5): a canned "ok" outcome
// carrying the perimeter run's own one-sentence description.
func perimeterStep(reason, sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response:  &response.PerimeterResponse{Job: response.JobPerimeter, Outcome: response.OutcomeOk, Reason: reason},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// perimeterQuestionStep builds a scriptedStep whose Response is a minimal
// QuestionResponse, the universal outcome a perimeter run can return
// instead of describing (design section 6.5 step 4's "success question").
func perimeterQuestionStep(sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response: &response.QuestionResponse{
			Job: response.JobPerimeter, Outcome: response.OutcomeQuestion,
			Questions: []response.Question{{Key: "q1", Title: "Which file?", Body: "Say which one you mean.", Options: []response.Option{}, Recommended: "keep it as is"}},
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// perimeterErrorStep builds a scriptedStep whose Response is a minimal
// ErrorResponse (design section 6.5 step 4's "success error").
func perimeterErrorStep(sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response: &response.ErrorResponse{
			Job: response.JobPerimeter, Outcome: response.OutcomeError,
			Error: response.RunError{Code: response.ErrorCodeOther, What: "could not read the hunk", Why: "the file looked binary"},
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// perimeterScenario drives a fresh ticket from queued through a task-1 RUN
// that truthfully claims filesChanged (hello.txt plus every path in
// extraReasons) and one ExtraClaim per extraReasons entry, then CHECK,
// using a project whose real test command creates every one of those paths
// for real, so the tree genuinely holds them when CHECK reads it (design
// section 6.4 step 2). It returns the store, the ticket id, rid (task 1's
// unit run, the id the ASK message must carry), and the scriptedRuntime,
// left open for the caller to append its own DESCRIBE steps in path order
// (extraReasons' keys, sorted).
func perimeterScenario(t *testing.T, extraReasons map[string]string) (s *store.Store, ticketID, rid int64, scriptRT *scriptedRuntime) {
	t.Helper()
	s, _, ticketID = buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	paths := make([]string, 0, len(extraReasons))
	for p := range extraReasons {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	filesChanged := append([]string{helloTxt}, paths...)
	extras := make([]response.ExtraClaim, len(paths))
	for i, p := range paths {
		extras[i] = response.ExtraClaim{Path: p, Reason: extraReasons[p]}
	}

	scriptRT = &scriptedRuntime{t: t, steps: []scriptedStep{buildStep(filesChanged, 0, 0, extras, "run-sess")}}
	deps := claimForBuild(t, s, scriptRT, ticketID)
	overrideProj := deps.Projects[ticket.ProjectID]
	overrideProj.TestCmd = "printf 'hello, world\\n' > hello.txt && touch " + strings.Join(paths, " ") + " && test -f hello.txt"
	overrideProj.LintCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: overrideProj}

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, scriptRT, ticketID)
	deps2.Projects = map[int64]job.Project{ticket.ProjectID: overrideProj}
	commit2, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(commit2.Messages) != 1 || !strings.HasPrefix(commit2.Messages[0].Body, "claims ok run ") {
		t.Fatalf("CHECK commit.Messages = %+v, want a claims-ok marker (the extras are still undecided)", commit2.Messages)
	}
	apply(t, s, ticket, commit2)

	reports, err := s.BuildReports(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	rid = findUnlandedReport(t, reports, 1).RunID

	return s, ticketID, rid, scriptRT
}

// describeTick runs one more building tick, fresh-claimed against scriptRT,
// applies its commit, and returns it.
func describeTick(t *testing.T, s *store.Store, scriptRT *scriptedRuntime, ticketID int64) store.HandlerCommit {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, scriptRT, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("describe tick: %v", err)
	}
	apply(t, s, ticket, commit)
	return commit
}

// TestPerimeterOneRunPerExtra proves DESCRIBE takes one tick per extra
// (design section 6.5 step 4): two extras need two ticks, each landing
// exactly one file artifact, and only the second (the last undescribed
// one) also carries the ASK question.
func TestPerimeterOneRunPerExtra(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{
		testExtraPath: testExtraReason,
		"extra2.go":   "needed another helper",
	})
	scriptRT.steps = append(scriptRT.steps,
		perimeterStep("Adds a small helper.", "perim-sess-1"),
		perimeterStep("Adds another small helper.", "perim-sess-2"),
	)

	first := describeTick(t, s, scriptRT, ticketID)
	if len(first.Artifacts) != 1 {
		t.Fatalf("first DESCRIBE commit.Artifacts = %+v, want exactly one file artifact", first.Artifacts)
	}
	if len(first.Messages) != 0 {
		t.Errorf("first DESCRIBE commit.Messages = %+v, want none (one extra is still undescribed)", first.Messages)
	}

	events, err := s.FileEvents(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("file events after the first DESCRIBE = %d, want 1", len(events))
	}

	second := describeTick(t, s, scriptRT, ticketID)
	if len(second.Artifacts) != 1 {
		t.Fatalf("second DESCRIBE commit.Artifacts = %+v, want exactly one file artifact", second.Artifacts)
	}
	if len(second.Messages) != 1 {
		t.Fatalf("second DESCRIBE commit.Messages = %+v, want one perimeter question (the last extra)", second.Messages)
	}

	events, err = s.FileEvents(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("file events after both DESCRIBEs = %d, want 2", len(events))
	}

	if len(scriptRT.reqs) != 3 {
		t.Fatalf("scriptRT saw %d requests, want 3 (one RUN, two DESCRIBEs)", len(scriptRT.reqs))
	}
}

// TestPerimeterAskInLastDescribeCommit proves the ASK message carries the
// build run's own id, set explicitly (design section 6.5, the "Boundaries"
// rule that a question always names the build run, never AttachRunToMsgs).
func TestPerimeterAskInLastDescribeCommit(t *testing.T) {
	s, ticketID, rid, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps, perimeterStep("Adds a small helper.", "perim-sess-1"))

	commit := describeTick(t, s, scriptRT, ticketID)
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one question message", commit.Messages)
	}
	msg := commit.Messages[0]
	if msg.RunID == nil || *msg.RunID != rid {
		t.Errorf("question RunID = %v, want the build run %d", msg.RunID, rid)
	}
	if commit.Waiting == nil || *commit.Waiting != string(response.QuestionKindPerimeter) {
		t.Errorf("commit.Waiting = %v, want %q", commit.Waiting, response.QuestionKindPerimeter)
	}
}

// TestPerimeterItemText proves Item.Text's exact format (design section
// 6.5): the marker in brackets when set, then "Builder: <reason> ", then
// "Change: <description>" -- covering no marker, trust root, and style
// guide.
func TestPerimeterItemText(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{
		"extra_plain.go": "a plain reason",
		"machine.toml":   "a trust root reason",
		"CLAUDE.md":      "a style guide reason",
	})
	// Path order (sorted): CLAUDE.md, extra_plain.go, machine.toml.
	scriptRT.steps = append(scriptRT.steps,
		perimeterStep("Adjusts the style guide.", "perim-sess-1"),
		perimeterStep("Adds a helper.", "perim-sess-2"),
		perimeterStep("Tweaks the machine config.", "perim-sess-3"),
	)

	describeTick(t, s, scriptRT, ticketID)
	describeTick(t, s, scriptRT, ticketID)
	final := describeTick(t, s, scriptRT, ticketID)

	if len(final.Messages) != 1 {
		t.Fatalf("final commit.Messages = %+v, want one question", final.Messages)
	}
	var payload response.QuestionPayload
	if err := json.Unmarshal(final.Messages[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if len(payload.Items) != 3 {
		t.Fatalf("payload.Items = %+v, want 3", payload.Items)
	}
	want := map[string]string{
		"CLAUDE.md":      "[style guide] Builder: a style guide reason Change: Adjusts the style guide.",
		"extra_plain.go": "Builder: a plain reason Change: Adds a helper.",
		"machine.toml":   "[trust root] Builder: a trust root reason Change: Tweaks the machine config.",
	}
	for _, item := range payload.Items {
		wantText, ok := want[item.Ref]
		if !ok {
			t.Fatalf("unexpected item ref %q", item.Ref)
		}
		if item.Text != wantText {
			t.Errorf("item %q text = %q, want %q", item.Ref, item.Text, wantText)
		}
	}
}

// TestPerimeterNoAskWithoutExtras proves a clean build (no extras) never
// raises a perimeter question (design section 6.5's own precondition: "run
// only with one or more extras in the tree").
func TestPerimeterNoAskWithoutExtras(t *testing.T) {
	s, rt, ticketID := buildTicketInBuilding(t)
	advanceBuilding(t, s, rt, ticketID)

	open, err := s.QuestionsByState(t.Context(), ticketID, string(response.QuestionStateOpen))
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	for _, m := range open {
		var payload response.QuestionPayload
		if json.Unmarshal(m.Payload, &payload) == nil && payload.Kind == response.QuestionKindPerimeter {
			t.Fatalf("a clean build with no extras must not raise a perimeter question; got %+v", payload)
		}
	}
}

// TestPerimeterRunQuestionStoresPath proves a perimeter run's own question
// outcome still links the run to its path (design section 6.5 step 4): a
// file artifact with the perimeter run's id, the builder's reason and
// markers, and no description.
func TestPerimeterRunQuestionStoresPath(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps, perimeterQuestionStep("perim-q-sess"))

	commit := describeTick(t, s, scriptRT, ticketID)
	if commit.Waiting == nil || *commit.Waiting != testWaitingQuestions {
		t.Fatalf("commit.Waiting = %v, want %q (the model's own question outcome)", commit.Waiting, testWaitingQuestions)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one file artifact linking the question to its path", commit.Artifacts)
	}

	events, err := s.FileEvents(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("file events = %d, want 1", len(events))
	}
	if events[0].File.Path != testExtraPath {
		t.Errorf("file event path = %q, want %q", events[0].File.Path, testExtraPath)
	}
	if events[0].File.Description != "" {
		t.Errorf("file event description = %q, want empty (no description until the question is answered)", events[0].File.Description)
	}
	if events[0].RunID == nil {
		t.Fatal("file event RunID = nil, want the perimeter run's id")
	}
}

// TestPerimeterFailureDescribesAgain proves invalid output, an exec
// failure, and an error outcome each leave the path undescribed, and the
// next tick describes it again with a fresh session, never a resume
// (design section 6.5 step 4's own "every other continuation is a fresh
// run").
func TestPerimeterFailureDescribesAgain(t *testing.T) {
	cases := []struct {
		name string
		step scriptedStep
	}{
		{"invalid output", invalidResult("not well-formed", "perim-fail-sess")},
		{"exec failure", scriptedStep{res: runtime.RunResult{SessionID: "", ExitCode: -1}, err: runtime.ErrStart}},
		{"error outcome", perimeterErrorStep("perim-fail-sess")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
			scriptRT.steps = append(scriptRT.steps, tc.step)

			failCommit := describeTick(t, s, scriptRT, ticketID)
			if len(failCommit.Artifacts) != 0 {
				t.Fatalf("failure commit.Artifacts = %+v, want none (the path stays undescribed)", failCommit.Artifacts)
			}

			events, err := s.FileEvents(t.Context(), ticketID)
			if err != nil {
				t.Fatalf("FileEvents: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("file events after the failure = %d, want 0", len(events))
			}

			sessionsBefore, err := s.SessionsForTicket(t.Context(), ticketID)
			if err != nil {
				t.Fatalf("SessionsForTicket: %v", err)
			}

			scriptRT.steps = append(scriptRT.steps, perimeterStep("Adds a small helper.", "perim-retry-sess"))
			retryCommit := describeTick(t, s, scriptRT, ticketID)
			if len(retryCommit.Artifacts) != 1 {
				t.Fatalf("retry commit.Artifacts = %+v, want exactly one file artifact", retryCommit.Artifacts)
			}

			sessionsAfter, err := s.SessionsForTicket(t.Context(), ticketID)
			if err != nil {
				t.Fatalf("SessionsForTicket: %v", err)
			}
			if len(sessionsAfter) != len(sessionsBefore)+1 {
				t.Errorf("sessions after the retry = %d, want %d (a fresh session, not a resume)", len(sessionsAfter), len(sessionsBefore)+1)
			}
		})
	}
}

// TestPerimeterSecondInvalidEscalates proves the invalid-output rule
// applies to perimeter runs too (design section 6.5 step 4, Package 7 D9):
// the second consecutive invalid perimeter output escalates
// response_invalid.
func TestPerimeterSecondInvalidEscalates(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps,
		invalidResult("not well-formed", "perim-invalid-1"),
		invalidResult("not well-formed", "perim-invalid-2"),
	)

	first := describeTick(t, s, scriptRT, ticketID)
	if first.Escalation != nil {
		t.Fatalf("first invalid commit.Escalation = %+v, want nil (first strike)", first.Escalation)
	}

	second := describeTick(t, s, scriptRT, ticketID)
	if second.Escalation == nil {
		t.Fatal("second invalid commit.Escalation = nil, want response_invalid")
	}
	if second.Escalation.Payload.Code != testCodeResponseInvalid {
		t.Errorf("second invalid escalation code = %q, want response_invalid", second.Escalation.Payload.Code)
	}
}

// ---- RESOLVE, and the perimeter run's own question (task 11) ---------------

// findOpenQuestionByKind returns ticketID's one open question of kind,
// failing the test when none exists.
func findOpenQuestionByKind(t *testing.T, s *store.Store, ticketID int64, kind response.QuestionKind) store.MessageRow {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, string(response.QuestionStateOpen))
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

// answerPerimeterQuestion drives the real console draft/send path
// (SaveDraft, then SendBatch) against questionID: one item draft per entry
// of decisions, all sent in one batch, so AnsweredRounds sees one round
// with every item decided (design section 6.6's own precondition: "Every
// item has a decision").
func answerPerimeterQuestion(t *testing.T, s *store.Store, ticketID, questionID int64, decisions map[string]response.Decision) {
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

// countPerimeterQuestions counts every kind=perimeter question message the
// ticket has ever carried, open, answered, or resolved alike.
func countPerimeterQuestions(t *testing.T, s *store.Store, ticketID int64) int {
	t.Helper()
	all, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	n := 0
	for i := range all {
		if all[i].Type != testMsgTypeQuestion {
			continue
		}
		var payload response.QuestionPayload
		if json.Unmarshal(all[i].Payload, &payload) == nil && payload.Kind == response.QuestionKindPerimeter {
			n++
		}
	}
	return n
}

// TestResolveAllAcceptedLandsWithExtra proves design section 6.6's
// all-accepted branch, example 13.2's own tick 4/5 shape: RESOLVE stores
// the decision and stays, the next tick's check before landing (design
// section 6.4 steps 3-6) passes with no extras left, and LAND stages the
// accepted path along with the declared ones -- the landed build_report
// keeping its original extra element untouched.
func TestResolveAllAcceptedLandsWithExtra(t *testing.T) {
	s, ticketID, rid, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps, perimeterStep("Adds a small helper.", "perim-sess-1"))
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE + ASK

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
	answerPerimeterQuestion(t, s, ticketID, q.ID, map[string]response.Decision{testExtraPath: response.DecisionAccept})

	resolveCommit := describeTick(t, s, scriptRT, ticketID) // RESOLVE: accept, stay
	if resolveCommit.Next != "" {
		t.Errorf("RESOLVE commit.Next = %q, want empty (RESOLVE never lands by itself)", resolveCommit.Next)
	}
	wantMarker := fmt.Sprintf(markerPerimeterResolvedFmtForTest, rid)
	if len(resolveCommit.Messages) != 1 || resolveCommit.Messages[0].Body != wantMarker {
		t.Fatalf("RESOLVE commit.Messages = %+v, want exactly [%q]", resolveCommit.Messages, wantMarker)
	}

	landCommit := describeTick(t, s, scriptRT, ticketID) // check before landing + LAND
	if len(landCommit.Artifacts) != 1 {
		t.Fatalf("LAND commit.Artifacts = %+v, want exactly one landed build_report", landCommit.Artifacts)
	}
	var landed response.BuildReport
	if err := json.Unmarshal(landCommit.Artifacts[0].Payload, &landed); err != nil {
		t.Fatalf("unmarshal landed build_report: %v", err)
	}
	if landed.CommitSHA == nil {
		t.Fatal("landed.CommitSHA = nil, want a sha")
	}
	if len(landed.Extras) != 1 || landed.Extras[0].Path != testExtraPath {
		t.Errorf("landed.Extras = %+v, want the original extra element kept", landed.Extras)
	}

	deps := claimForBuild(t, s, scriptRT, ticketID)
	proj, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	changes, err := proj.Orch.CommitChanges(t.Context(), wt, *landed.CommitSHA)
	if err != nil {
		t.Fatalf("CommitChanges: %v", err)
	}
	found := false
	for _, c := range changes {
		if c.Path == testExtraPath {
			found = true
		}
	}
	if !found {
		t.Errorf("landed commit paths = %+v, want %q among them", changes, testExtraPath)
	}
}

// markerPerimeterResolvedFmtForTest mirrors building.go's own unexported
// markerPerimeterResolvedFmt (design section 6.6 step 6): this file cannot
// reach the job package's own private constant, and the marker's exact
// wording is part of what this test proves.
const markerPerimeterResolvedFmtForTest = "perimeter resolved run %d"

// TestResolveRejectedIsRevertedAndResumed proves design section 6.6's
// reject branch, example 13.2's own tick 4 shape: the rejected path is
// reverted from the tree, the build session is resumed with
// orchestrator.PerimeterNotice, and the fresh run's own report -- once
// landed -- excludes the reverted path entirely.
func TestResolveRejectedIsRevertedAndResumed(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps,
		perimeterStep("Adds a small helper.", "perim-sess-1"),
		buildStep([]string{helloTxt}, 0, 0, nil, "resume-sess"),
	)
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE + ASK

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
	answerPerimeterQuestion(t, s, ticketID, q.ID, map[string]response.Decision{testExtraPath: response.DecisionReject})

	resolveCommit := describeTick(t, s, scriptRT, ticketID) // RESOLVE: revert + resume
	if len(resolveCommit.Runs) != 1 {
		t.Fatalf("RESOLVE commit.Runs = %+v, want exactly one (the resumed build run)", resolveCommit.Runs)
	}

	if len(scriptRT.reqs) == 0 {
		t.Fatal("scriptRT recorded no requests")
	}
	lastReq := scriptRT.reqs[len(scriptRT.reqs)-1]
	if !strings.Contains(lastReq.Prompt, "were reverted") {
		t.Errorf("resume prompt = %q, want it to carry orchestrator.PerimeterNotice's own wording", lastReq.Prompt)
	}
	if lastReq.SessionID == "" {
		t.Error("resume request carries no session id, want the build session's own external id")
	}

	deps := claimForBuild(t, s, scriptRT, ticketID)
	ticket := getTicket(t, s, ticketID)
	proj, wt := buildWorktreeFor(t, deps, ticket)
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), testExtraPath)); !os.IsNotExist(statErr) {
		t.Errorf("stat %s after reject: err=%v, want not-exist (reverted)", testExtraPath, statErr)
	}

	landCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // check + LAND for the fresh report
	if err != nil {
		t.Fatalf("CHECK/LAND: %v", err)
	}
	apply(t, s, ticket, landCommit)
	if len(landCommit.Artifacts) != 1 {
		t.Fatalf("LAND commit.Artifacts = %+v, want exactly one landed build_report", landCommit.Artifacts)
	}
	var landed response.BuildReport
	if unmarshalErr := json.Unmarshal(landCommit.Artifacts[0].Payload, &landed); unmarshalErr != nil {
		t.Fatalf("unmarshal landed build_report: %v", unmarshalErr)
	}
	if landed.CommitSHA == nil {
		t.Fatal("landed.CommitSHA = nil, want a sha")
	}

	changes, err := proj.Orch.CommitChanges(t.Context(), wt, *landed.CommitSHA)
	if err != nil {
		t.Fatalf("CommitChanges: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != helloTxt {
		t.Errorf("landed commit changes = %+v, want exactly [hello.txt] (the rejected path never reaches a commit)", changes)
	}
}

// TestAcceptedPathAsksOnceOnly proves design section 14's own edge case ("An
// accepted path is changed again by a later task: it is declared now; no
// question"), applied within one unit: a round mixing one accepted and one
// rejected extra reverts and resumes for the rejected path only, and the
// fresh report's own re-check never asks about the accepted path again,
// even though describeOrAsk runs fresh against a brand-new build_report.
func TestAcceptedPathAsksOnceOnly(t *testing.T) {
	const acceptedPath = "extra_ok.go"
	const rejectedPath = "extra_bad.go"
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{
		acceptedPath: "needed a helper",
		rejectedPath: "not actually needed",
	})
	// Path order (sorted): extra_bad.go, extra_ok.go.
	scriptRT.steps = append(scriptRT.steps,
		perimeterStep("Adds a helper the task does not use.", "perim-sess-bad"),
		perimeterStep("Adds a helper the task uses.", "perim-sess-ok"),
		// acceptedPath is already declared by the time this fresh report is
		// checked (its accept decision was inserted in the same RESOLVE
		// commit that produced this run, at a lower artifact id): the
		// resumed run claims it as an ordinary changed path, not a new
		// extra.
		buildStep([]string{helloTxt, acceptedPath}, 0, 0, nil, "resume-sess"),
	)
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE extra_bad.go
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE extra_ok.go + ASK

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
	answerPerimeterQuestion(t, s, ticketID, q.ID, map[string]response.Decision{
		acceptedPath: response.DecisionAccept,
		rejectedPath: response.DecisionReject,
	})

	describeTick(t, s, scriptRT, ticketID)               // RESOLVE: revert extra_bad.go, resume
	landCommit := describeTick(t, s, scriptRT, ticketID) // check (first, for the fresh report) + LAND directly

	if len(landCommit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one landed build_report", landCommit.Artifacts)
	}
	var landed response.BuildReport
	if err := json.Unmarshal(landCommit.Artifacts[0].Payload, &landed); err != nil {
		t.Fatalf("unmarshal landed build_report: %v", err)
	}
	if landed.CommitSHA == nil {
		t.Fatal("landed.CommitSHA = nil, want a sha (extra_ok.go must not need a second question)")
	}

	deps := claimForBuild(t, s, scriptRT, ticketID)
	proj, wt := buildWorktreeFor(t, deps, getTicket(t, s, ticketID))
	changes, err := proj.Orch.CommitChanges(t.Context(), wt, *landed.CommitSHA)
	if err != nil {
		t.Fatalf("CommitChanges: %v", err)
	}
	gotPaths := make([]string, 0, len(changes))
	for _, c := range changes {
		gotPaths = append(gotPaths, c.Path)
	}
	sort.Strings(gotPaths)
	want := []string{acceptedPath, helloTxt}
	if len(gotPaths) != len(want) || gotPaths[0] != want[0] || gotPaths[1] != want[1] {
		t.Errorf("landed commit paths = %v, want %v", gotPaths, want)
	}

	if n := countPerimeterQuestions(t, s, ticketID); n != 1 {
		t.Errorf("perimeter questions ever asked = %d, want exactly 1 (the accepted path must not be asked about twice)", n)
	}
}

// TestResolveDefaultsUnknownDecisionToReject proves design section 6.6 step
// 2's defensive default: the console's own SaveDraft already refuses a
// perimeter item decision outside accept/reject (internal/store/console_
// writes.go), so this seeds an already-answered round directly through
// store.InsertMessage -- the one path that can still carry a decision
// SaveDraft would have refused -- and proves RESOLVE treats it as a reject
// rather than panicking or silently dropping the path, logging the
// defaulted warning design section 11 names.
func TestResolveDefaultsUnknownDecisionToReject(t *testing.T) {
	s, ticketID, rid, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps,
		perimeterStep("Adds a small helper.", "perim-sess-1"),
		buildStep([]string{helloTxt}, 0, 0, nil, "resume-defaulted-sess"),
	)
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE + the real ASK (left open, unused)

	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q9", Kind: response.QuestionKindPerimeter, State: response.QuestionStateAnswered,
		Recommended: "Decide each file", Options: []response.Option{},
		Items: []response.Item{{Ref: testExtraPath, Text: "Builder: " + testExtraReason}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	qID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, RunID: &rid, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State: new("answered"), Body: "Confirm the file perimeter", Payload: payload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(question): %v", err)
	}

	answerPayload, err := json.Marshal(response.AnswerPayload{Items: map[string]response.Decision{testExtraPath: response.DecisionDiscuss}})
	if err != nil {
		t.Fatalf("marshal answer payload: %v", err)
	}
	if _, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, ParentID: &qID, Type: "answer", Author: "you",
		State: new("sent"), Payload: answerPayload,
	}); insertErr != nil {
		t.Fatalf("InsertMessage(answer): %v", insertErr)
	}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, scriptRT, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RESOLVE: %v", err)
	}
	if !strings.Contains(logBuf.String(), "perimeter decision defaulted to reject") {
		t.Errorf("log missing the defaulted-to-reject warning; got:\n%s", logBuf.String())
	}
	apply(t, s, ticket, commit)

	events, err := s.FileEvents(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	var decided *response.PerimeterDecision
	for _, e := range events {
		if e.File.Path == testExtraPath && e.File.Decision != nil {
			decided = e.File.Decision
		}
	}
	if decided == nil || *decided != response.PerimeterReject {
		t.Errorf("decided path %s decision = %v, want reject", testExtraPath, decided)
	}
}

// TestResolveNoEmptyQuestionAfterAccept proves RESOLVE's own accept-all
// commit never re-raises the perimeter question it just answered, and
// clears the ticket's wait (design section 6.6 step 6: "commit the file
// artifacts, ResolveQuestions, and the marker ... stay").
func TestResolveNoEmptyQuestionAfterAccept(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps, perimeterStep("Adds a small helper.", "perim-sess-1"))
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE + ASK

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
	answerPerimeterQuestion(t, s, ticketID, q.ID, map[string]response.Decision{testExtraPath: response.DecisionAccept})

	resolveCommit := describeTick(t, s, scriptRT, ticketID) // RESOLVE

	if resolveCommit.Waiting != nil {
		t.Errorf("RESOLVE commit.Waiting = %q, want nil", *resolveCommit.Waiting)
	}
	for _, m := range resolveCommit.Messages {
		if m.Type == testMsgTypeQuestion {
			t.Errorf("RESOLVE posted a new question: %+v, want none", m)
		}
	}

	final := getTicket(t, s, ticketID)
	if final.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q after RESOLVE, want nil", *final.WaitingOn)
	}
}

// TestPerimeterRunAnswerResumesItsSession proves design section 6.2's third
// answered-round branch (round.Job == "perimeter"): the owner's answer to a
// perimeter run's own question resumes that same session (BumpResumes, the
// same external session id), rather than starting a fresh one, and the
// resumed run's own description lands as the path's file event.
func TestPerimeterRunAnswerResumesItsSession(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	const wantDescription = "Uses a hyphen, matching the style guide."
	scriptRT.steps = append(scriptRT.steps,
		perimeterQuestionStep("perim-q-sess"),
		perimeterStep(wantDescription, "perim-q-sess"),
	)
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE returns a question

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindQuestion)
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &q.ID, Text: "Use a hyphen."}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	sessionsBefore, err := s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}

	commit := describeTick(t, s, scriptRT, ticketID) // resolvePerimeterQuestion: resume

	if len(commit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one file artifact (the description)", commit.Artifacts)
	}
	if len(commit.ResolveQuestions) == 0 {
		t.Error("commit.ResolveQuestions is empty, want the round resolved")
	}

	sessionsAfter, err := s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	if len(sessionsAfter) != len(sessionsBefore) {
		t.Errorf("sessions after answering = %d, want %d (a resume, not a fresh session)", len(sessionsAfter), len(sessionsBefore))
	}

	if len(scriptRT.reqs) == 0 {
		t.Fatal("scriptRT recorded no requests")
	}
	lastReq := scriptRT.reqs[len(scriptRT.reqs)-1]
	if lastReq.SessionID != "perim-q-sess" {
		t.Errorf("resume request SessionID = %q, want %q (the perimeter run's own external id)", lastReq.SessionID, "perim-q-sess")
	}

	events, err := s.FileEvents(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	described := false
	for _, e := range events {
		if e.File.Path == testExtraPath && e.File.Description == wantDescription {
			described = true
		}
	}
	if !described {
		t.Errorf("no file event carries the resumed run's own description %q; events = %+v", wantDescription, events)
	}
}

// TestPerimeterQuestionDroppedWhenPathGone proves design section 6.2's
// third answered-round branch's other outcome: when the path the perimeter
// run asked about is no longer an extra (removed from the tree by hand
// here, standing in for an agent's own revert or recreate), the round
// resolves with the "perimeter question dropped" marker and no runtime
// call at all.
func TestPerimeterQuestionDroppedWhenPathGone(t *testing.T) {
	s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps, perimeterQuestionStep("perim-q-sess"))
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE returns a question

	events, err := s.FileEvents(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	var perimRunID int64
	for _, e := range events {
		if e.File.Path == testExtraPath && e.RunID != nil {
			perimRunID = *e.RunID
		}
	}
	if perimRunID == 0 {
		t.Fatal("no file event carries the perimeter run's own id")
	}

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindQuestion)
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &q.ID, Text: "It's fine either way."}); draftErr != nil {
		t.Fatalf("SaveDraft: %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticketID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, scriptRT, ticketID)
	_, wt := buildWorktreeFor(t, deps, ticket)
	if removeErr := os.Remove(filepath.Join(wt.Dir(), testExtraPath)); removeErr != nil {
		t.Fatalf("remove %s: %v", testExtraPath, removeErr)
	}

	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("resolve perimeter round: %v", err)
	}
	if len(commit.Runs) != 0 {
		t.Errorf("commit.Runs = %+v, want none (a dropped question makes no runtime call)", commit.Runs)
	}
	wantMarker := fmt.Sprintf("perimeter question dropped run %d", perimRunID)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != wantMarker {
		t.Fatalf("commit.Messages = %+v, want exactly [%q]", commit.Messages, wantMarker)
	}
	if len(commit.ResolveQuestions) == 0 {
		t.Error("commit.ResolveQuestions is empty, want the round's question ids resolved")
	}
}
