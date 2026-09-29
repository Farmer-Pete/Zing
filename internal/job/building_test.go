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
	overrideProj.LintCmd = "true"
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
