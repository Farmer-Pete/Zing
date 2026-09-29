// building.go is the real "building" state handler (design section 6): step
// 0 (plan, worktree, branch reconcile, with verified adoption of an
// unrecorded commit), step 2 (the next unit to build), RUN (first turn
// only), CHECK, LAND, and the transition to "reviewing" once every task has
// landed. It replaces the skeleton's fake, fixture-driven buildingHandler
// (skeleton.go carried it through task 8, which routed it through runJob so
// every building tick already passed the sandbox rule).
//
// Later tasks own the rest of design section 6: an answered round (task 12,
// 13), DESCRIBE and ASK over an extra in the tree after a passing first
// check (task 10), RESOLVE (task 11), and any resume -- a claim-errors
// pending run, an interrupted run, or an invalid-output retry (task 12).
// This file returns ErrNoAction, logged at debug naming the step, whenever
// the state machine reaches one of those. CHECK still writes the "claims ok
// run <rid>" and "claim errors pending run <rid>" markers on every tick it
// runs, because those later tasks read them.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// jobBuildName is the job.go/machine.toml key building's own session and
// job config are keyed under, "build" (design section 4.3, 6.3).
const jobBuildName = string(response.JobBuild)

// checkCommandTimeout bounds each of CHECK's two command re-runs (design
// D16, section 6.4 step 1): 10 minutes, independently for test and lint.
const checkCommandTimeout = 10 * time.Minute

// The two artifact types this file writes and reads (design section 4.1,
// 6.3, 6.7): "build_report" carries one build unit's checked claims, and
// (later tasks) "file" carries one undeclared path's perimeter event. Both
// strings are also literal in internal/store/build_reads.go's own queries.
const (
	artifactTypeBuildReport = "build_report"
	artifactTypeFile        = "file"
)

// claimsTestExitPath and claimsLintExitPath are the two element paths
// CheckCommandsPassed and CheckBuildClaims (internal/response/claims.go)
// both report claim errors under: this file's own dedup step (a timeout's
// own message, and dropping CheckBuildClaims's duplicate of an exit
// CheckCommandsPassed already reported) matches on them by name.
const (
	claimsTestExitPath = "claims/test_exit"
	claimsLintExitPath = "claims/lint_exit"
)

// Marker head formats CHECK reads and writes (design section 4.2, 6.4):
// Marker's own exact-first-line match makes "claims ok run 4" and "claims ok
// run 42" two different markers.
const (
	markerClaimsOkFmt           = "claims ok run %d"
	markerClaimErrorsPendingFmt = "claim errors pending run %d"
)

// The section 6.1/6.4/6.7 escalation What texts, byte for byte from the
// plan. Why is this file's own plain-sentence gloss on each one: the plan
// gives no exact Why for these, only What and (for the adoption checks)
// Tried.
const (
	noStoredPlanWhat = "no stored plan for this ticket"
	noStoredPlanWhy  = "building needs a stored plan artifact to know what to build"

	worktreeNotPreparedWhat = "the worktree could not be prepared"
	worktreeNotPreparedWhy  = "the orchestrator could not create or attach the ticket's worktree"

	branchMissingRecordedWhat = "the ticket branch does not hold the commits Zing recorded"
	branchMissingRecordedWhy  = "a commit Zing recorded is missing from the ticket branch, or the branch holds them in a different order"

	unverifiableCommitWhat = "the ticket branch holds a commit Zing cannot verify"
	unverifiableCommitWhy  = "an unrecorded commit on the ticket branch failed the check named in Tried"

	foreignCommitsWhat = "the ticket branch holds commits Zing did not record"
	foreignCommitsWhy  = "the ticket branch carries more than one commit past what Zing has recorded"

	treeNotDiffedWhat = "the worktree could not be diffed"
	treeNotDiffedWhy  = "the worktree's changed paths could not be read"

	projectCommandsNotRunWhat = "the project commands could not run"
	projectCommandsNotRunWhy  = "the test or lint command could not be run at all"

	commitSigningFailedWhat = "commit signing failed"
	commitSigningFailedWhy  = "the commit could not be produced with a valid signature"

	taskNotCommittedWhat = "the task could not be committed"
	taskNotCommittedWhy  = "the approved paths could not be committed to the ticket branch"

	misnumberedTasksWhat = "the stored plan's tasks are not numbered 1 to n"
	misnumberedTasksWhy  = "task progress is keyed by task number, so every task must be numbered 1 to n in order"
)

// trustRoot and styleGuide are design section 12's two constant path lists
// (section 6.4): every CHECK, and the adoption checks, classify an extra
// against them through orchestrator.Perimeter.
var (
	trustRoot = []string{
		"machine.toml", "prompts/**", "internal/store/schemas/**", "sandbox/**",
		".github/workflows/**", "internal/store/migrations/**", "FACTORY.md",
	}
	styleGuide = []string{"CLAUDE.md", "AGENTS.md"}
)

// buildEscalation is escalationCommit (planning.go) plus this file's own
// "escalation written" log (design section 11), for the environment
// escalations building writes with no run in scope (design section 6.1,
// 6.4, 6.7: step 0, CHECK, and LAND's own infrastructure failures): every
// one carries a nil RunID and SessionID and origin "build", so this fixes
// those rather than threading them through every call site.
func buildEscalation(t store.Ticket, d Deps, code, what, why, tried string) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginBuild))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginBuild)
}

// unit is what one build session builds: task N of the plan, or a fix
// (design section 4.3). Building never builds a fix unit (task 14 does);
// TaskN is always 1..12 here.
type unit struct {
	TaskN int
	Title string // the commit subject
}

// buildingHandler runs the real building state (design section 6): it
// replaces the skeleton's handler of the same name.
type buildingHandler struct{}

func (h buildingHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	// Step (E)/1: an answered round. Building's own AnsweredRounds routing
	// (perimeter resolution, build question resumes, escalation
	// resolution) is tasks 10 through 13; this task only needs to make
	// sure such a round is never silently dropped.
	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		slog.Debug("building entry decision", "ticket_id", t.ID, "step", "answered_round", "session_state", "n/a")
		return store.HandlerCommit{}, ErrNoAction
	}

	// Step 0: plan, worktree, branch.
	plan, _, ok, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: stored plan: %w", err)
	}
	if !ok {
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}
	if !tasksNumberedOneToN(response.Tasks(plan)) {
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), misnumberedTasksWhat, misnumberedTasksWhy, ""), nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}

	wt, err := proj.Orch.EnsureWorktree(ctx, t.ID, t.Title)
	if err != nil {
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), worktreeNotPreparedWhat, worktreeNotPreparedWhy, err.Error()), nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: build reports: %w", err)
	}

	shas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: branch commits: %w", err)
	}
	slog.Info("worktree ensured", "ticket_id", t.ID, "branch", wt.Branch(), "created", len(shas) == 0)

	recorded := recordedShas(reports)
	if !isPrefixOf(recorded, shas) {
		return withBranch(buildEscalation(t, d, string(response.EscalationCodeEnvironment), branchMissingRecordedWhat, branchMissingRecordedWhy, ""), wt), nil
	}
	unrecorded := shas[len(recorded):]

	switch len(unrecorded) {
	case 0:
		// continue to step 2
	case 1:
		commit, adoptErr := h.adopt(ctx, t, d, proj, wt, plan, reports, unrecorded[0])
		if adoptErr != nil {
			return store.HandlerCommit{}, adoptErr
		}
		return withBranch(commit, wt), nil
	default:
		return withBranch(buildEscalation(t, d, string(response.EscalationCodeEnvironment), foreignCommitsWhat, foreignCommitsWhy, ""), wt), nil
	}

	// Step 2: next unit.
	tasks := response.Tasks(plan)
	taskN, hasNext := nextTaskN(tasks, reports)
	if !hasNext {
		c := baseCommit(t, d)
		c.Next, c.Reason = stateReviewing, reasonBuildDone
		slog.Info("build done", "ticket_id", t.ID, "tasks", len(tasks), "commits", len(recorded))
		return withBranch(c, wt), nil
	}
	u, ok := unitFor(tasks, taskN)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: building: ticket %d: plan has no task %d", t.ID, taskN)
	}

	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	_, state, newestRun, ok, err := d.Store.UnitSession(ctx, t.ID, taskN, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: unit session: %w", err)
	}

	slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "unit_session", "session_state", sessionStateName(state))

	if !ok || state == store.SessionIdless {
		commit, runErr := h.runFirst(ctx, t, d, proj, wt, plan, u, len(tasks))
		return withBranchResult(commit, runErr, wt)
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}

	switch outcome {
	case string(response.OutcomeOk):
		rid := newestRun.ID
		_, pending, markerErr := d.Store.Marker(ctx, t.ID, fmt.Sprintf(markerClaimErrorsPendingFmt, rid))
		if markerErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: claim errors marker: %w", markerErr)
		}
		if pending {
			slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "needs_resume_claims", "session_state", sessionStateName(state))
			return store.HandlerCommit{}, ErrNoAction
		}

		_, checked, okMarkerErr := d.Store.Marker(ctx, t.ID, fmt.Sprintf(markerClaimsOkFmt, rid))
		if okMarkerErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: claims ok marker: %w", okMarkerErr)
		}

		report, foundReport := findUnitReport(reports, rid)
		if !foundReport {
			return store.HandlerCommit{}, fmt.Errorf("job: building: ticket %d: no build_report for run %d", t.ID, rid)
		}

		if !checked {
			commit, checkErr := h.check(ctx, t, d, proj, wt, plan, taskN, rid, report, true)
			return withBranchResult(commit, checkErr, wt)
		}

		events, evErr := d.Store.FileEvents(ctx, t.ID)
		if evErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: file events: %w", evErr)
		}
		declaredNow := declaredPaths(plan, events, nil)
		changed, changedErr := proj.Orch.ChangedPaths(ctx, wt)
		if changedErr != nil {
			return withBranchResult(buildEscalation(t, d, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, changedErr.Error()), nil, wt)
		}
		extras := orchestrator.Perimeter(changed, declaredNow, trustRoot, styleGuide)
		if len(extras) == 0 {
			commit, checkErr := h.check(ctx, t, d, proj, wt, plan, taskN, rid, report, false)
			return withBranchResult(commit, checkErr, wt)
		}

		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "describe_or_ask", "session_state", sessionStateName(state))
		return store.HandlerCommit{}, ErrNoAction

	case string(response.OutcomeError):
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "needs_resume_error", "session_state", sessionStateName(state))
		return store.HandlerCommit{}, ErrNoAction

	default:
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "unrecognized", "session_state", sessionStateName(state))
		return store.HandlerCommit{}, ErrNoAction
	}
}

// ---- helpers: branch, prefix, adoption -------------------------------------

// withBranch sets SetBranch on c to wt's branch (design section 6.1 step
// 0.4): every commit this handler returns, once wt exists, carries it.
func withBranch(c store.HandlerCommit, wt orchestrator.Worktree) store.HandlerCommit {
	branch := wt.Branch()
	c.SetBranch = &branch
	return c
}

// withBranchResult is withBranch for a (commit, error) pair: an error
// passes through untouched (the commit alongside it, when non-zero, is not
// this handler's to finish building; runAndRoute's own callers never rely
// on SetBranch on an error path).
func withBranchResult(c store.HandlerCommit, err error, wt orchestrator.Worktree) (store.HandlerCommit, error) {
	if err != nil {
		return c, err
	}
	return withBranch(c, wt), nil
}

// recordedShas returns every commit_sha of reports, in artifact order
// (BuildReports' own ORDER BY id), skipping a report with none (not yet
// landed).
func recordedShas(reports []store.BuildReportRow) []string {
	var out []string
	for i := range reports {
		if reports[i].Report.CommitSHA != nil {
			out = append(out, *reports[i].Report.CommitSHA)
		}
	}
	return out
}

// isPrefixOf reports whether recorded equals shas' first len(recorded)
// elements, in order (design section 6.1 step 5).
func isPrefixOf(recorded, shas []string) bool {
	if len(recorded) > len(shas) {
		return false
	}
	for i, sha := range recorded {
		if shas[i] != sha {
			return false
		}
	}
	return true
}

// nextTaskN is design section 6's step 2: the lowest task n with no landed
// (CommitSHA set) build_report, or hasNext=false when every task has
// landed.
func nextTaskN(tasks []response.Task, reports []store.BuildReportRow) (n int, hasNext bool) {
	landed := make(map[int]bool, len(reports))
	for i := range reports {
		if reports[i].Report.CommitSHA != nil {
			landed[reports[i].Report.TaskN] = true
		}
	}
	for _, tk := range tasks {
		if !landed[tk.N] {
			return tk.N, true
		}
	}
	return 0, false
}

// tasksNumberedOneToN reports whether tasks, in order, are numbered
// 1..len(tasks) (design section 4.1): progress is keyed by task number, so
// building's own step 0 repeats the same rule response.Validate's
// layer2Ready check applies at plan-ready time, catching a plan stored
// before that rule existed.
func tasksNumberedOneToN(tasks []response.Task) bool {
	for i, tk := range tasks {
		if tk.N != i+1 {
			return false
		}
	}
	return true
}

// unitFor finds tasks' entry numbered taskN and builds its unit (design
// section 4.3's commit subject rule): "Task <n>: " plus the first line of
// the task text with leading "#", "*", "-", and spaces trimmed, cut to 72
// runes.
func unitFor(tasks []response.Task, taskN int) (unit, bool) {
	for _, tk := range tasks {
		if tk.N == taskN {
			return unit{TaskN: taskN, Title: unitTitle(taskN, tk.Text)}, true
		}
	}
	return unit{}, false
}

func unitTitle(taskN int, taskText string) string {
	first, _, _ := strings.Cut(taskText, "\n")
	first = strings.TrimLeft(first, "#*- ")
	return cutRunes(fmt.Sprintf("Task %d: %s", taskN, first), 72)
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// findUnitReport returns the newest unlanded build_report row (CommitSHA
// nil) whose RunID is rid.
func findUnitReport(reports []store.BuildReportRow, rid int64) (store.BuildReportRow, bool) {
	for i := range slices.Backward(reports) {
		if reports[i].RunID == rid && reports[i].Report.CommitSHA == nil {
			return reports[i], true
		}
	}
	return store.BuildReportRow{}, false
}

// planFilePaths returns response.Files(plan)'s declared paths.
func planFilePaths(plan response.Plan) []string {
	files := response.Files(plan)
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// newestFileEventPerPath reduces events (FileEvents' own insertion order)
// to the newest row per path.
func newestFileEventPerPath(events []store.FileEventRow) map[string]store.FileEventRow {
	out := make(map[string]store.FileEventRow, len(events))
	for _, e := range events {
		out[e.File.Path] = e
	}
	return out
}

// acceptedPaths returns, sorted, every path whose newest file event carries
// Decision accept (design section 9.1's own "accepted" prompt input).
func acceptedPaths(events []store.FileEventRow) []string {
	var out []string
	for _, row := range newestFileEventPerPath(events) {
		if row.File.Decision != nil && *row.File.Decision == response.PerimeterAccept {
			out = append(out, row.File.Path)
		}
	}
	sort.Strings(out)
	return out
}

// declaredPaths is CHECK's declaredBefore/declaredNow (design section 6.4
// step 3): the plan's declared files plus every accepted path, or, with
// before non-nil, only the accepted paths whose deciding artifact id is
// lower than *before (declaredBefore); nil includes every accepted path
// (declaredNow).
func declaredPaths(plan response.Plan, events []store.FileEventRow, before *int64) []string {
	out := planFilePaths(plan)
	for _, row := range newestFileEventPerPath(events) {
		if row.File.Decision == nil || *row.File.Decision != response.PerimeterAccept {
			continue
		}
		if before != nil && row.ArtifactID >= *before {
			continue
		}
		out = append(out, row.File.Path)
	}
	return out
}

func changedPathList(changes []orchestrator.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Path
	}
	return out
}

func deletedPathList(changes []orchestrator.Change) []string {
	var out []string
	for _, c := range changes {
		if c.Code == orchestrator.Deleted {
			out = append(out, c.Path)
		}
	}
	return out
}

func extraPathList(extras []orchestrator.Extra) []string {
	out := make([]string, len(extras))
	for i, e := range extras {
		out[i] = e.Path
	}
	return out
}

// funcLines is design section 6.7's own helper: for each plan.Design.Changes
// entry, in plan order, whose Path is in approved, "<symbol> (<path>):
// called by <callers>; calls <callees>", with every whitespace run in
// callers and callees collapsed to one space, the line cut to 160 runes.
func funcLines(plan response.Plan, approved []string) []string {
	approvedSet := make(map[string]bool, len(approved))
	for _, p := range approved {
		approvedSet[p] = true
	}
	var lines []string
	for i := range plan.Design.Changes {
		ch := &plan.Design.Changes[i]
		if !approvedSet[ch.Path] {
			continue
		}
		callers := collapseWhitespace(ch.Callers)
		callees := collapseWhitespace(ch.Callees)
		line := fmt.Sprintf("%s (%s): called by %s; calls %s", ch.Symbol, ch.Path, callers, callees)
		lines = append(lines, cutRunes(line, 160))
	}
	return lines
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---- RUN --------------------------------------------------------------

// runFirst is RUN's first turn (design section 6.3): assembles
// prompt.ForBuild's inputs and routes runJob's result through
// buildSuccessCommit.
func (h buildingHandler) runFirst(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, u unit, totalTasks int) (store.HandlerCommit, error) {
	tasks := response.Tasks(plan)
	tk, ok := taskByN(tasks, u.TaskN)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: building: plan has no task %d", u.TaskN)
	}

	jobCfg := d.Machine.Jobs[jobBuildName]
	promptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", err)
	}

	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", err)
	}

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: file events: %w", err)
	}
	accepted := acceptedPaths(events)

	schemas, err := renderSchemas(response.JobBuild, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", err)
	}

	titlePrefix := fmt.Sprintf("Task %d: ", u.TaskN)
	bt := prompt.BuildTask{N: u.TaskN, Total: totalTasks, Title: strings.TrimPrefix(u.Title, titlePrefix), Text: tk.Text, Test: tk.Test}

	in, err := prompt.ForBuild(promptText, bt, proj.TestCmd, proj.LintCmd, t.Title+"\n\n"+t.Body, planXML, accepted, nil)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", err)
	}
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobBuildName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobBuild, Label: strconv.Itoa(u.TaskN), WorkDir: wt.Dir(), Prompt: assembled}
	n := u.TaskN
	return runAndRoute(ctx, d, t, jobBuildName, su, req, 0, freshSessionRecord, nil, response.EscalationOriginBuild,
		func(rr runResult) (store.HandlerCommit, error) {
			return buildSuccessCommit(t, d, rr, freshSessionRecord(rr), nil, u)
		}, &n)
}

func taskByN(tasks []response.Task, n int) (response.Task, bool) {
	for _, tk := range tasks {
		if tk.N == n {
			return tk, true
		}
	}
	return response.Task{}, false
}

// buildSuccessCommit routes a build run's parsed response (design section
// 6.8): ok inserts one build_report artifact; question and error are the
// universal outcomes classify and planning already share.
func buildSuccessCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, u unit) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.BuildResponse:
		// The artifacts/build_report.json schema declares extras and fences
		// as bare JSON arrays (no jsonschema minItems, so a document with no
		// <extra> or <fence> element leaves the Go slice nil), and
		// json.Marshal renders a nil slice as null, which the schema's
		// "type": "array" rejects. Normalize to an empty slice first,
		// mirroring planning.go's own normalizePlanArrays.
		extras := resp.Extras
		if extras == nil {
			extras = []response.ExtraClaim{}
		}
		fences := resp.Fences
		if fences == nil {
			fences = []response.Fence{}
		}
		report := response.BuildReport{
			TaskN:       u.TaskN,
			BuildClaims: resp.Claims,
			Extras:      extras,
			Fences:      fences,
			Report:      resp.Report,
			Title:       u.Title,
		}
		payload, err := json.Marshal(report)
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: marshal build report: %w", err)
		}
		c := baseCommit(t, d)
		c.Runs = terminalRuns(rr, string(response.OutcomeOk))
		c.Session = sessionCommit
		c.ResolveQuestions = resolveIDs
		c.Artifacts = []store.Artifact{{Type: artifactTypeBuildReport, RunID: &rr.Reserved.RunID, Payload: payload}}
		return c, nil
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginBuild), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: building: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// ---- CHECK, LAND --------------------------------------------------------

// runCheckCommand runs one CHECK command re-run (design section 6.4 step
// 1), logging "command re-run" (design section 11) regardless of outcome.
// A timeout reports exit -1 with timedOut true and no error; any other
// CommandRunner failure (ErrSandbox, a wrapped context.Canceled, or
// anything else) is returned unclassified for the caller to route.
func runCheckCommand(ctx context.Context, d Deps, t store.Ticket, wt orchestrator.Worktree, proj Project, kind, shellCmd string) (exit int, timedOut bool, err error) {
	started := time.Now()
	exit, runErr := d.Commands.Run(ctx, wt.Dir(), proj.RepoGit, shellCmd, checkCommandTimeout)
	seconds := int(time.Since(started).Seconds())
	switch {
	case runErr == nil:
		slog.Info("command re-run", "ticket_id", t.ID, "command", kind, "exit_code", exit, "seconds", seconds, "timed_out", false)
		return exit, false, nil
	case errors.Is(runErr, ErrCommandTimeout):
		slog.Info("command re-run", "ticket_id", t.ID, "command", kind, "exit_code", -1, "seconds", seconds, "timed_out", true)
		return -1, true, nil
	default:
		return exit, false, runErr
	}
}

// commandInfraEscalation classifies a non-timeout CommandRunner error
// (design section 6.4 step 1): ErrSandbox escalates sandbox_unavailable; a
// wrapped context.Canceled returns runtime.ErrCanceled with no commit;
// anything else escalates environment/"the project commands could not
// run". Every case is handled -- CHECK's own two call sites always return
// its result directly rather than falling through.
func commandInfraEscalation(t store.Ticket, d Deps, err error) (store.HandlerCommit, error) {
	switch {
	case errors.Is(err, ErrSandbox):
		return buildEscalation(t, d, string(response.EscalationCodeSandboxUnavailable), sandboxUnavailableWhat, d.Sandbox.Reason(), ""), nil
	case errors.Is(err, context.Canceled):
		return store.HandlerCommit{}, runtime.ErrCanceled
	default:
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), projectCommandsNotRunWhat, projectCommandsNotRunWhy, err.Error()), nil
	}
}

// check runs design section 6.4's CHECK, shared by the first check and the
// check-before-landing recheck (firstCheck tells them apart only for the
// "claims ok" marker write). rid is the unit's newest ok run; report is
// that run's own build_report row.
func (h buildingHandler) check(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, taskN int, rid int64, report store.BuildReportRow, firstCheck bool) (store.HandlerCommit, error) {
	testExit, testTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, "test", proj.TestCmd)
	if err != nil {
		return commandInfraEscalation(t, d, err)
	}
	lintExit, lintTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, "lint", proj.LintCmd)
	if err != nil {
		return commandInfraEscalation(t, d, err)
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), nil
	}

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: check: file events: %w", err)
	}
	artifactID := report.ArtifactID
	declaredBefore := declaredPaths(plan, events, &artifactID)
	declaredNow := declaredPaths(plan, events, nil)

	claimed := orchestrator.Perimeter(changed, declaredBefore, trustRoot, styleGuide)
	extras := orchestrator.Perimeter(changed, declaredNow, trustRoot, styleGuide)

	cmdErrs := response.CheckCommandsPassed(testExit, lintExit)
	for i, e := range cmdErrs {
		if e.Path == claimsTestExitPath && testTimedOut {
			cmdErrs[i] = &response.PathError{Path: claimsTestExitPath, Msg: "timed out after 10m"}
		}
		if e.Path == claimsLintExitPath && lintTimedOut {
			cmdErrs[i] = &response.PathError{Path: claimsLintExitPath, Msg: "timed out after 10m"}
		}
	}
	cmdPaths := make(map[string]bool, len(cmdErrs))
	for _, e := range cmdErrs {
		cmdPaths[e.Path] = true
	}

	obs := response.BuildObservation{FilesChanged: changedPathList(changed), TestExit: testExit, LintExit: lintExit}
	claimErrs := response.CheckBuildClaims(report.Report.BuildClaims, obs)

	resp := &response.BuildResponse{Claims: report.Report.BuildClaims, Extras: report.Report.Extras, Fences: report.Report.Fences}
	treeErrs := response.CheckBuildTree(resp, response.BuildTree{
		Changed: changedPathList(changed), Deleted: deletedPathList(changed), Extras: extraPathList(claimed),
	})

	var errs []*response.PathError
	errs = append(errs, cmdErrs...)
	for _, e := range claimErrs {
		if (e.Path == claimsTestExitPath || e.Path == claimsLintExitPath) && cmdPaths[e.Path] {
			continue
		}
		errs = append(errs, e)
	}
	errs = append(errs, treeErrs...)

	slog.Info("claim check", "ticket_id", t.ID, "run_id", rid, "task_n", taskN, "errors", len(errs), "changed", len(changed), "extras", len(extras))

	switch {
	case len(errs) > 0:
		lines := make([]string, len(errs))
		for i, e := range errs {
			lines[i] = e.Error()
		}
		body := fmt.Sprintf(markerClaimErrorsPendingFmt, rid) + "\n" + strings.Join(lines, "\n")
		c := baseCommit(t, d)
		c.Messages = []store.Message{{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: body}}
		return c, nil
	case len(extras) > 0 && firstCheck:
		c := baseCommit(t, d)
		c.Messages = []store.Message{{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: fmt.Sprintf(markerClaimsOkFmt, rid)}}
		return c, nil
	case len(extras) > 0:
		// A command changed the tree since the first check, after the
		// owner had already decided every earlier extra (task 11's
		// RESOLVE). Describing the new one is task 10's job.
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "describe_new_extra", "session_state", "n/a")
		return store.HandlerCommit{}, ErrNoAction
	default:
		approved := changedPathList(changed)
		return h.land(ctx, t, d, proj, wt, plan, taskN, rid, report, approved)
	}
}

// land is design section 6.7's LAND: commit exactly approved, record the
// landed build_report, and transition to reviewing when this was the last
// task.
func (h buildingHandler) land(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, taskN int, rid int64, report store.BuildReportRow, approved []string) (store.HandlerCommit, error) {
	msg := orchestrator.CommitMessage{Title: report.Report.Title, FuncLines: funcLines(plan, approved), Fences: report.Report.Fences}
	sha, err := proj.Orch.CommitTask(ctx, wt, approved, msg)
	if err != nil {
		what, why := taskNotCommittedWhat, taskNotCommittedWhy
		if strings.Contains(err.Error(), "commit signing failed") {
			what, why = commitSigningFailedWhat, commitSigningFailedWhy
		}
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), what, why, err.Error()), nil
	}

	c, err := landCommit(t, d, plan, taskN, rid, report.Report, sha)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	slog.Info("unit landed", "ticket_id", t.ID, "run_id", rid, "task_n", taskN, "commit_sha", sha, "files", len(approved))
	if c.Next != "" {
		slog.Info("build done", "ticket_id", t.ID, "tasks", len(response.Tasks(plan)), "commits", taskN)
	}
	return c, nil
}

// landCommit builds the LAND commit shape (design section 6.7 steps 3-4,
// 6.8's transition-out row): one build_report artifact copying report with
// CommitSHA set to sha, and Next = reviewing when taskN is the plan's last
// task. It never calls CommitTask itself, so step 0's adoption path
// (h.adopt) shares it without re-committing a sha that is already on the
// branch.
func landCommit(t store.Ticket, d Deps, plan response.Plan, taskN int, rid int64, report response.BuildReport, sha string) (store.HandlerCommit, error) {
	landed := report
	landed.CommitSHA = &sha
	payload, err := json.Marshal(landed)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: marshal landed build report: %w", err)
	}

	c := baseCommit(t, d)
	c.Artifacts = []store.Artifact{{Type: artifactTypeBuildReport, RunID: &rid, Payload: payload}}

	tasks := response.Tasks(plan)
	if len(tasks) > 0 && taskN == tasks[len(tasks)-1].N {
		c.Next, c.Reason = stateReviewing, reasonBuildDone
	}
	return c, nil
}

// ---- step 0.5: verified adoption of an unrecorded commit -------------------

// adopt is design section 6.1's single-unrecorded-commit branch: it runs
// the seven checks in order, escalating unverifiable_commit at the first
// failure (Tried names the failing check), and otherwise returns the LAND
// commit for sha without ever calling CommitTask again.
func (h buildingHandler) adopt(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, reports []store.BuildReportRow, sha string) (store.HandlerCommit, error) {
	fail := func(check string) store.HandlerCommit {
		return buildEscalation(t, d, string(response.EscalationCodeEnvironment), unverifiableCommitWhat, unverifiableCommitWhy, check)
	}

	testExit, testTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, "test", proj.TestCmd)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: test command: %w", err)
	}
	lintExit, lintTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, "lint", proj.LintCmd)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: lint command: %w", err)
	}
	if testExit != 0 || lintExit != 0 || testTimedOut || lintTimedOut {
		return fail("commands failed"), nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: changed paths: %w", err)
	}
	if len(changed) != 0 {
		return fail("tree not clean"), nil
	}

	signed, err := proj.Orch.SignedStatus(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: signed status: %w", err)
	}
	if !signed {
		return fail("unsigned"), nil
	}

	tasks := response.Tasks(plan)
	taskN, hasNext := nextTaskN(tasks, reports)
	if !hasNext {
		return fail("no report"), nil
	}
	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	_, _, newestRun, ok, err := d.Store.UnitSession(ctx, t.ID, taskN, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: unit session: %w", err)
	}
	if !ok || newestRun.Outcome == nil || *newestRun.Outcome != string(response.OutcomeOk) {
		return fail("no report"), nil
	}
	report, ok := findUnitReport(reports, newestRun.ID)
	if !ok {
		return fail("no report"), nil
	}

	subject, err := proj.Orch.CommitSubject(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: commit subject: %w", err)
	}
	if subject != report.Report.Title {
		return fail("subject mismatch"), nil
	}

	commitChanges, err := proj.Orch.CommitChanges(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: commit changes: %w", err)
	}

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: file events: %w", err)
	}
	// claimed (declaredBefore-based) is what CheckBuildTree judges the
	// report's own extra elements against -- what was undeclared when the
	// run wrote its report; extras (declaredNow-based) is check 7's own
	// "every path is declared or accepted" test (design section 6.1 step 5,
	// 6.4 steps 3-5). The two agree today (no accepted file event exists
	// yet, since DESCRIBE and RESOLVE are task 10/11), but adopt must still
	// compute them the same distinct way check does.
	artifactID := report.ArtifactID
	declaredBefore := declaredPaths(plan, events, &artifactID)
	declaredNow := declaredPaths(plan, events, nil)
	claimed := orchestrator.Perimeter(commitChanges, declaredBefore, trustRoot, styleGuide)
	extras := orchestrator.Perimeter(commitChanges, declaredNow, trustRoot, styleGuide)

	cmdErrs := response.CheckCommandsPassed(testExit, lintExit)
	obs := response.BuildObservation{FilesChanged: changedPathList(commitChanges), TestExit: testExit, LintExit: lintExit}
	claimErrs := response.CheckBuildClaims(report.Report.BuildClaims, obs)
	resp := &response.BuildResponse{Claims: report.Report.BuildClaims, Extras: report.Report.Extras, Fences: report.Report.Fences}
	treeErrs := response.CheckBuildTree(resp, response.BuildTree{
		Changed: changedPathList(commitChanges), Deleted: deletedPathList(commitChanges), Extras: extraPathList(claimed),
	})
	if len(cmdErrs) > 0 || len(claimErrs) > 0 || len(treeErrs) > 0 {
		return fail("claims failed"), nil
	}

	if len(extras) > 0 {
		return fail("undeclared path"), nil
	}

	slog.Warn("unrecorded commit adopted", "ticket_id", t.ID, "task_n", taskN, "commit_sha", sha)
	return landCommit(t, d, plan, taskN, newestRun.ID, report.Report, sha)
}
