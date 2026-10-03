// building.go is the real "building" state handler (design section 6): step
// 0 (plan, worktree, branch reconcile, with verified adoption of an
// unrecorded commit), step 2 (the next unit to build), RUN (first turn and
// every resume: an owner's answer, claim errors, an interrupted run, or an
// invalid output), CHECK, DESCRIBE and ASK, RESOLVE, LAND, the resume cap
// (design section 6.9's "resumes_exhausted is written once per session"),
// and the transition to "reviewing" once every task has landed. It replaces
// the skeleton's fake, fixture-driven buildingHandler (skeleton.go carried
// it through task 8, which routed it through runJob so every building tick
// already passed the sandbox rule).
//
// Task 13 adds the rest of design section 6: resolving a building
// escalation once the owner has answered it (retry, back to planning,
// abandon -- design section 6.9's own choice-by-origin table).
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	markerClaimsOkFmt                 = "claims ok run %d"
	markerClaimErrorsPendingFmt       = "claim errors pending run %d"
	markerClaimErrorsDeliveredFmt     = "claim errors delivered run %d"
	markerPerimeterResolvedFmt        = "perimeter resolved run %d"
	markerPerimeterQuestionDroppedFmt = "perimeter question dropped run %d"
)

// claimsPendingInput reads run rid's own "claim errors pending" marker and
// builds the "claims" input and the "claim errors delivered" message that
// marks it delivered (design section 6.4), shared by advanceCheckedRun's
// own first attempt at a freshly-checked run and advanceUnit's interrupted-
// resume re-send (F009, design section 7.4), which calls this for an
// earlier run than the one that is currently newest. ok is false when
// rid's marker is not pending, or a "claim errors delivered run <rid>"
// marker already exists for it (the resume that answered it already ran;
// never re-send).
func claimsPendingInput(ctx context.Context, t store.Ticket, d Deps, rid int64) (input prompt.NamedInput, deliveredMsg store.Message, ok bool, err error) {
	markerRow, pending, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(markerClaimErrorsPendingFmt, rid))
	if err != nil {
		return prompt.NamedInput{}, store.Message{}, false, fmt.Errorf("job: building: claim errors marker: %w", err)
	}
	if !pending {
		return prompt.NamedInput{}, store.Message{}, false, nil
	}
	_, delivered, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(markerClaimErrorsDeliveredFmt, rid))
	if err != nil {
		return prompt.NamedInput{}, store.Message{}, false, fmt.Errorf("job: building: claim errors delivered marker: %w", err)
	}
	if delivered {
		return prompt.NamedInput{}, store.Message{}, false, nil
	}
	_, errsText, _ := strings.Cut(markerRow.Body, "\n")
	input = prompt.NamedInput{Label: "claims", Text: errsText, Untrusted: true}
	deliveredMsg = store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf(markerClaimErrorsDeliveredFmt, rid),
	}
	return input, deliveredMsg, true, nil
}

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

	revertFailedWhat = "a rejected file could not be reverted"
	revertFailedWhy  = "orchestrator.RevertPaths could not restore or remove the rejected path"

	// treeExtraUnclaimedWhat and treeExtraUnclaimedWhy are review F046's own
	// escalation text: the tree changed after the unit's own "claims ok"
	// marker (a D19 survivor), so a tree extra now exists that the build
	// report's own Extras never claimed. A bare handler error here would
	// make the dispatcher release the claim and retry every tick with no
	// escalation at all (a livelock).
	treeExtraUnclaimedWhat = "the worktree holds a changed path the build never claimed"
	treeExtraUnclaimedWhy  = "the tree changed after the unit's own claims were checked, so the build report carries no claim for this path"

	// replanUnsupportedWhat and replanUnsupportedWhy are design D14's own
	// fixed text for an escalation round's choice "b" (back to planning),
	// or a reply with no option at all: back-to-planning on a building
	// ticket is deferred, so every origin re-escalates this instead
	// (design section 6.9).
	replanUnsupportedWhat = "replanning after the build started is not built yet; retry or abandon"
	replanUnsupportedWhy  = "the owner chose back to planning on a building ticket"
)

// markerRetryRequested is design section 6.9's own retry marker: written
// when a retry needs no fresh run of its own -- a build retry with no run
// (step 0, CHECK, LAND, or RESOLVE's own escalation), a perimeter retry, or
// a retry on a sandbox_unavailable escalation of any origin ("as build
// with no run") -- so the next tick simply re-enters the state machine at
// step 0 (build) or DESCRIBE (perimeter, which always retakes the first
// undescribed extra on its own).
const markerRetryRequested = "retry requested"

// labelPerimeter is the "perimeter" input label a build resume carries
// after a revert (design section 6.3's own resume input table): the
// notice is raw, never fenced, since it is Zing's own fixed wording, not
// owner- or model-supplied text.
const labelPerimeter = "perimeter"

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
// escalations building writes before any unit is chosen (design section
// 6.1: step 0's own plan/branch checks, and the task-only helpers that
// never run for a fix -- ensureUnitWorktree, buildTaskUnitInFlight): every
// one carries a nil RunID and SessionID, code "environment" (every
// remaining call site's own code, now that commandInfraEscalation's
// sandbox_unavailable branch moved to unitEscalation), and origin "build",
// so this fixes those rather than threading them through every call site.
func buildEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeEnvironment)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginBuild))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginBuild)
}

// unitEscalation is buildEscalation generalized over the unit a shared step
// -- one building's own Run and the fix driver both call -- was advancing
// when it hit an infrastructure failure with no run in scope (design
// section 5.4 change 2, #28 gap 2): origin is "fix" when u.TaskN == 0 (a
// fix unit is always task 0, design D22), else "build". check, land,
// advanceCheckedRun, describeOrAsk, describeOne, resolve, adopt, and
// commandInfraEscalation all call this instead of buildEscalation, each
// passing the unit it was already carrying.
func unitEscalation(t store.Ticket, d Deps, u unit, code, what, why, tried string) store.HandlerCommit {
	origin := originFor(u)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(origin))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, origin)
}

// originFor is design section 5.4 change 2's own rule: a fix unit (D22,
// TaskN always 0) escalates origin "fix"; a task unit escalates origin
// "build". runFirst, runBuildResume, resolve's own resume, and
// buildSuccessCommit's error outcome pass this to runAndRoute/
// errorOutcomeCommit instead of the hardcoded origin "build" they used
// before a fix unit shared them.
func originFor(u unit) response.EscalationOrigin {
	if u.TaskN == 0 {
		return response.EscalationOriginFix
	}
	return response.EscalationOriginBuild
}

// buildCapResumesEscalation is capResumesEscalation (planning.go) plus this
// file's own "escalation written" log (design section 11): every
// resumes_exhausted escalation this file raises -- an answered build
// question, a claims/interrupted/invalid resume, RESOLVE's own revert, or a
// perimeter-question resume -- carries origin cap_resumes and the exhausted
// session's own id, never a run id (the cap raised it, not a run).
func buildCapResumesEscalation(t store.Ticket, d Deps, sessionID int64) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", nil,
		"code", string(response.EscalationCodeResumesExhausted), "origin", string(response.EscalationOriginCapResumes))
	return capResumesEscalation(t, d, sessionID)
}

// ensureUnitWorktree is design section 6's own repeated step, shared by
// every entry point that needs a unit's worktree before it can do anything
// else (review F026, CLAUDE.md's "three repetitions before an
// abstraction"): resolve t's project, ensure its worktree, and log
// "worktree ensured" exactly as every call site did inline. A non-nil
// *store.HandlerCommit is the "worktree could not be prepared" escalation
// the caller returns as its own commit, with a nil error; err is ErrConfig
// when t.ProjectID names no project in d.Projects. No behaviour change:
// every caller's own return shape is unchanged, only where these four
// lines live.
func ensureUnitWorktree(ctx context.Context, t store.Ticket, d Deps) (Project, orchestrator.Worktree, *store.HandlerCommit, error) {
	return ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return buildEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
}

// ensureUnitWorktreeFor is ensureUnitWorktree generalized over a known unit
// (design section 5.4 change 3, #28 gap 3): its own worktree-preparation
// failure escalates through unitEscalation, tagging origin fix or build by
// u's own TaskN, instead of ensureUnitWorktree's hardcoded origin build.
// resumeBuildRound, retryFreshRun, retryCapResumesBuild,
// retryCapResumesPerimeter, and resolvePerimeterQuestion call this once
// their own unit is known.
func ensureUnitWorktreeFor(ctx context.Context, t store.Ticket, d Deps, u unit) (Project, orchestrator.Worktree, *store.HandlerCommit, error) {
	return ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
}

// ensureWorktreeOrEscalate is ensureUnitWorktree and ensureUnitWorktreeFor's
// shared body: resolve t's project, ensure its worktree, and log "worktree
// ensured" exactly as every call site did inline before this was factored
// out (review F026, CLAUDE.md's "three repetitions before an abstraction").
// A non-nil *store.HandlerCommit is onFail's own escalation, which the
// caller returns as its own commit, with a nil error; err is ErrConfig when
// t.ProjectID names no project in d.Projects.
func ensureWorktreeOrEscalate(ctx context.Context, t store.Ticket, d Deps, onFail func(errText string) store.HandlerCommit) (Project, orchestrator.Worktree, *store.HandlerCommit, error) {
	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return Project{}, orchestrator.Worktree{}, nil, ErrConfig
	}
	wt, created, err := proj.Orch.EnsureWorktree(ctx, t.ID, t.Title)
	if err != nil {
		c := onFail(err.Error())
		return Project{}, orchestrator.Worktree{}, &c, nil
	}
	slog.Info("worktree ensured", "ticket_id", t.ID, "branch", wt.Branch(), "created", created)
	return proj, wt, nil, nil
}

// unit is what one build session builds: task N of the plan (1..12), or
// task 0, a fix (design section 4.3, 8). Building's own Run entry point
// never builds a fix unit (task 14's StartFix and AdvanceFix do), but
// advanceCheckedRun and every step it shares with them (check, land,
// describeOrAsk, describeOne, resolve) treat TaskN 0 as a real value, not a
// building-only invariant: seedTaskN and buildLabel/perimeterLabel
// (runjob.go) are what tell the two apart.
type unit struct {
	TaskN int
	Title string // the commit subject

	// FixRequestID is the fix request marker's own message id (design D22,
	// section 5.2, 5.3): nil for a task unit, set for a fix unit, whose LAND
	// commit then also carries "fix landed <id> sha <sha>".
	FixRequestID *int64
}

// buildingHandler runs the real building state (design section 6): it
// replaces the skeleton's handler of the same name.
type buildingHandler struct{}

func (h buildingHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	// Step (E)/1: an answered round (design section 6.2). The newest
	// question's kind decides the branch before round.Job does: a
	// perimeter-kind question is always RESOLVE (section 6.6), whichever
	// run's own id it was posted against. Escalation resolution is task
	// 13's job.
	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		return h.enterFromRounds(ctx, t, d, rounds)
	}

	// Step 0: plan, worktree, branch.
	plan, _, ok, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: stored plan: %w", err)
	}
	if !ok {
		return buildEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}
	if !tasksNumberedOneToN(response.Tasks(plan)) {
		return buildEscalation(t, d, misnumberedTasksWhat, misnumberedTasksWhy, ""), nil
	}

	proj, wt, escalation, err := ensureUnitWorktree(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: build reports: %w", err)
	}

	unrecorded, prefixOK, err := unrecordedCommits(ctx, proj, wt, reports)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if !prefixOK {
		return withBranch(buildEscalation(t, d, branchMissingRecordedWhat, branchMissingRecordedWhy, ""), wt), nil
	}

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
		return withBranch(buildEscalation(t, d, foreignCommitsWhat, foreignCommitsWhy, ""), wt), nil
	}

	// Step 2: next unit.
	tasks := response.Tasks(plan)
	taskN, hasNext := nextTaskN(tasks, reports)
	if !hasNext {
		c := baseCommit(t, d)
		c.Next, c.Reason = stateReviewing, reasonBuildDone
		slog.Info("build done", "ticket_id", t.ID, "tasks", len(tasks), "commits", len(recordedShas(reports)))
		return withBranch(c, wt), nil
	}
	u, ok := unitFor(tasks, taskN)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: building: ticket %d: plan has no task %d", t.ID, taskN)
	}

	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	sess, state, newestRun, ok, err := d.Store.UnitSession(ctx, t.ID, taskN, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: unit session: %w", err)
	}

	slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "unit_session", "session_state", sessionStateName(state))

	return h.advanceUnit(ctx, t, d, proj, wt, plan, u, sess, state, newestRun, ok, reports)
}

// advanceUnit is the unit-session switch shared by a task unit (building's
// own Run, above) and a fix unit (the fix driver, fix.go's DriveFix; design
// D22, #28 gap 1): first turn (no session found, or a session whose
// external_id never got far enough to be set) runs h.runFirst; outcome
// "ok" hands off to advanceCheckedRun (the claims-pending resume, CHECK,
// DESCRIBE, ASK, LAND); outcome "error" resumes with the "invalid" or
// "interrupted" input, through the resume cap gate; anything else is
// ErrNoAction. found is the caller's own UnitSession/SessionAfter ok: the
// fix driver never calls this with found true and state SessionIdless --
// its own "no usable session yet" case is folded into its own first-turn
// RUN before advanceUnit is ever reached, since a fix unit's first turn
// needs the fix's own prompt (prompt.ForFix), not h.runFirst's
// plan-and-task-text one -- so that branch below only ever fires for a
// task unit in practice.
func (h buildingHandler) advanceUnit(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, u unit, sess store.Session, state store.SessionState, newestRun store.Run, found bool, reports []store.BuildReportRow) (store.HandlerCommit, error) {
	if !found || state == store.SessionIdless {
		commit, runErr := h.runFirst(ctx, t, d, proj, wt, plan, u, len(response.Tasks(plan)), nil, nil)
		return withBranchResult(commit, runErr, wt)
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}

	switch outcome {
	case string(response.OutcomeOk):
		commit, runErr := h.advanceCheckedRun(ctx, t, d, proj, wt, plan, u, sess, state, newestRun.ID, reports)
		return withBranchResult(commit, runErr, wt)

	case string(response.OutcomeError):
		bump, gate := resumeCharge(newestRun)

		// F009 (design section 7.4): an interrupted claims resume re-sends
		// its original claims input plus the interrupted input, free and
		// uncapped, rather than losing the claims text -- walk back past
		// every run still inside this same interrupted chain to the
		// session's last settled (non-interrupted) run; if that run was
		// ok and its own "claim errors pending" marker is still undelivered
		// (the resume that was meant to answer it never finished), re-send
		// it now, sharing claimsPendingInput with advanceCheckedRun's own
		// first attempt at the same marker.
		if newestRun.Interrupted {
			priorRun, found, priorErr := priorNonInterruptedRun(ctx, d, t.ID, sess.ID, newestRun.ID)
			if priorErr != nil {
				return store.HandlerCommit{}, priorErr
			}
			if found && priorRun.Outcome != nil && *priorRun.Outcome == string(response.OutcomeOk) {
				claimsInput, deliveredMsg, pending, markerErr := claimsPendingInput(ctx, t, d, priorRun.ID)
				if markerErr != nil {
					return store.HandlerCommit{}, markerErr
				}
				if pending {
					interruptedInput := prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false}
					resumeCommit, resumeErr := h.runBuildResume(ctx, t, d, wt, u, sess, 0, nil, []prompt.NamedInput{claimsInput, interruptedInput}, bump)
					if resumeErr == nil && len(resumeCommit.Runs) > 0 {
						resumeCommit.Messages = append(resumeCommit.Messages, deliveredMsg)
					}
					return withBranchResult(resumeCommit, resumeErr, wt)
				}
			}
		}

		// One of two "needs a resume" cases design section 6.3/6.10 group
		// under a single error outcome: n==1 means the newest run of this
		// session carries a "response invalid run <rid>" marker of its own
		// (D14's first-strike case; a second consecutive invalid output
		// would already have escalated response_invalid, which this
		// invocation could never reach, the tick's own "not waiting"
		// precondition). n==0, with the ticket not waiting and the same
		// session's newest run still error, is a run that never got a
		// response at all -- a shutdown or dead-serve interrupt
		// (newestRun.Interrupted) or a plainer reconcile ExpireClaims left
		// behind. resumeCharge (job.go, design D5, section 7.4) tells the
		// two apart: an interrupted newest run resumes free and skips the
		// cap gate; anything else keeps today's charged, cap-gated resume.
		n, reason, invErr := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobBuildName, &sess.ID)
		if invErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: consecutive invalid outputs: %w", invErr)
		}
		input := prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false}
		if n == 1 {
			input = prompt.Invalid(invalidRetryText(reason))
		}
		if gate {
			capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, u.TaskN, sess, state)
			if !mayResume {
				return withBranchResult(capCommit, capErr, wt)
			}
		}
		resumeCommit, resumeErr := h.runBuildResume(ctx, t, d, wt, u, sess, n, nil, []prompt.NamedInput{input}, bump)
		return withBranchResult(resumeCommit, resumeErr, wt)

	default:
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", u.TaskN, "step", "unrecognized", "session_state", sessionStateName(state))
		return store.HandlerCommit{}, ErrNoAction
	}
}

// advanceCheckedRun is the unit session's outcome=="ok" branch (design
// section 6.4, 6.5, 6.6, 6.7): the claims-pending resume, the not-yet-
// checked CHECK, and -- once checked -- the tree read into DESCRIBE/ASK or
// the check-before-landing recheck that itself reaches LAND. advanceUnit
// shares it verbatim between a task unit and a fix unit (design D22): the
// resuming unit's own title is read from its own build_report
// (report.Report.Title) rather than from the plan, since runBuildResume
// only ever needs u.TaskN and u.Title, and both are always recoverable
// from the report by the time a claims-pending resume can exist at all --
// CHECK itself required a report to write that marker in the first place.
// u carries the caller's own FixRequestID through to LAND unchanged; only
// its Title is overwritten from the report. rid is the unit session's
// newest ok run.
func (h buildingHandler) advanceCheckedRun(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, u unit, sess store.Session, state store.SessionState, rid int64, reports []store.BuildReportRow) (store.HandlerCommit, error) {
	report, foundReport := findUnitReport(reports, rid)
	if !foundReport {
		return store.HandlerCommit{}, fmt.Errorf("job: building: ticket %d: no build_report for run %d", t.ID, rid)
	}
	u.Title = report.Report.Title

	// The claims input is exactly the marker's own error lines (design
	// section 6.4): priorInvalid is 0, not computed, since this session's
	// newest run is the "ok" one CHECK just wrote a pending marker for --
	// ConsecutiveInvalidOutputs' own walk stops at the first non-"error"
	// outcome, so it can only ever read 0 here. claimsPendingInput is
	// shared with advanceUnit's own interrupted-resume re-send (F009,
	// design section 7.4), which calls it for an earlier run than this
	// one's own rid.
	claimsInput, deliveredMsg, pending, markerErr := claimsPendingInput(ctx, t, d, rid)
	if markerErr != nil {
		return store.HandlerCommit{}, markerErr
	}
	if pending {
		capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, u.TaskN, sess, state)
		if !mayResume {
			return capCommit, capErr
		}
		resumeCommit, resumeErr := h.runBuildResume(ctx, t, d, wt, u, sess, 0, nil, []prompt.NamedInput{claimsInput}, true)
		if resumeErr == nil && len(resumeCommit.Runs) > 0 {
			resumeCommit.Messages = append(resumeCommit.Messages, deliveredMsg)
		}
		return resumeCommit, resumeErr
	}

	_, checked, okMarkerErr := d.Store.Marker(ctx, t.ID, fmt.Sprintf(markerClaimsOkFmt, rid))
	if okMarkerErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: claims ok marker: %w", okMarkerErr)
	}

	if !checked {
		return h.check(ctx, t, d, proj, wt, plan, u, rid, report, true)
	}

	events, evErr := d.Store.FileEvents(ctx, t.ID)
	if evErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: file events: %w", evErr)
	}
	declaredNow := declaredPaths(plan, events, nil, u.TaskN)
	changed, changedErr := proj.Orch.ChangedPaths(ctx, wt)
	if changedErr != nil {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, changedErr.Error()), nil
	}
	extras := orchestrator.Perimeter(changed, declaredNow, trustRoot, styleGuide)
	if len(extras) == 0 {
		return h.check(ctx, t, d, proj, wt, plan, u, rid, report, false)
	}

	return h.describeOrAsk(ctx, t, d, proj, wt, u, rid, report, events, changed, extras)
}

// ---- answered rounds: build question resume, resume cap ------------------

// enterFromRounds is design section 6.2's answered-round dispatch, applied
// in order over every answered round, not just the newest: the only branch
// that ever moves past rounds[0] is a build-job round whose session is
// already exhausted and already carries a cap_resumes escalation of its own
// (the preserved round is consumed later, by task 13's cap resolution);
// every other branch resolves or escalates from the first round alone.
func (h buildingHandler) enterFromRounds(ctx context.Context, t store.Ticket, d Deps, rounds []store.Round) (store.HandlerCommit, error) {
	for _, round := range rounds {
		// The newest question's own parent id, not round.ParentID (the
		// same distinction planning.go's enterFromRound draws): a
		// run-caused escalation's linked question carries the same run_id
		// as the escalation itself, so AnsweredRounds groups it by run,
		// same as an ordinary build/perimeter question batch, and
		// round.ParentID (only ever filled for the no-run grouping) stays
		// nil. Resolving an escalation round is task 13's job (design
		// section 6.1, 6.9); this file stops here.
		newest := round.Questions[len(round.Questions)-1]
		if newest.ParentID != nil {
			return h.enterFromEscalationRound(ctx, t, d, round, *newest.ParentID)
		}

		kind, kindErr := newestQuestionKind(round)
		if kindErr != nil {
			return store.HandlerCommit{}, kindErr
		}
		switch {
		case kind == response.QuestionKindPerimeter:
			return h.resolve(ctx, t, d, round)
		case round.Job == jobBuildName:
			u, escalation, uErr := unitForBuildRound(ctx, t, d, round)
			if uErr != nil {
				return store.HandlerCommit{}, uErr
			}
			if escalation != nil {
				return *escalation, nil
			}
			commit, again, err := h.resumeBuildRound(ctx, t, d, round, u)
			if again {
				continue
			}
			return commit, err
		case round.Job == jobPerimeterName:
			return h.resolvePerimeterQuestion(ctx, t, d, round)
		default:
			return store.HandlerCommit{}, fmt.Errorf("job: building: answered round of job %s", round.Job)
		}
	}
	return store.HandlerCommit{}, ErrNoAction
}

// unitForBuildRound derives the unit an answered build-job round belongs
// to (design D22, section 6.2, generalized for #28 gap 1's own "resumes
// that session through resumeBuildRound"): an open fix request whose own
// session (SessionAfter, the watermark rule of D18) is round's own session
// is the fix's round; otherwise it is buildTaskUnitInFlight's task, the
// unit still awaiting the owner's answer (a question outcome never inserts
// a build_report, so nextTaskN still names it).
func unitForBuildRound(ctx context.Context, t store.Ticket, d Deps, round store.Round) (unit, *store.HandlerCommit, error) {
	if round.SessionID == nil {
		return unit{}, nil, errors.New("job: building: answered round: round has no session id")
	}
	req, open, err := openFixRequest(ctx, d, t)
	if err != nil {
		return unit{}, nil, fmt.Errorf("job: building: answered round: open fix request: %w", err)
	}
	if open {
		maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
		fixSess, _, _, ok, sessErr := d.Store.SessionAfter(ctx, t.ID, jobBuildName, req.AfterRunID, maxResumes)
		if sessErr != nil {
			return unit{}, nil, fmt.Errorf("job: building: answered round: session after: %w", sessErr)
		}
		if ok && fixSess.ID == *round.SessionID {
			mid := req.MessageID
			return unit{TaskN: 0, Title: fixSubjectFor[req.Kind], FixRequestID: &mid}, nil, nil
		}
	}
	return buildTaskUnitInFlight(ctx, t, d)
}

// buildTaskUnitInFlight is unitForBuildRound's own task-unit case (design
// section 6.2): the plan's lowest unlanded task, the unit still awaiting
// the owner's answer.
func buildTaskUnitInFlight(ctx context.Context, t store.Ticket, d Deps) (unit, *store.HandlerCommit, error) {
	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return unit{}, nil, fmt.Errorf("job: building: answered round: stored plan: %w", err)
	}
	if !havePlan {
		c := buildEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, "")
		return unit{}, &c, nil
	}
	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return unit{}, nil, fmt.Errorf("job: building: answered round: build reports: %w", err)
	}
	taskN, hasNext := nextTaskN(response.Tasks(plan), reports)
	if !hasNext {
		return unit{}, nil, fmt.Errorf("job: building: answered round: ticket %d: no unit awaiting an answer", t.ID)
	}
	u, foundUnit := unitFor(response.Tasks(plan), taskN)
	if !foundUnit {
		return unit{}, nil, fmt.Errorf("job: building: answered round: ticket %d: plan has no task %d", t.ID, taskN)
	}
	return u, nil, nil
}

// resumeBuildRound is design section 6.2's round.Job=="build" branch: the
// owner's answer to a build run's own question resumes that session with
// one answer input per question (answerInputsForRound), resolving the
// round. u is the round's own unit (design D22: unitForBuildRound for
// building's own caller, known directly by the fix driver). When the
// session is exhausted, this either escalates resumes_exhausted once
// (again=false) or, when that escalation already exists, tells the caller
// to try the next answered round in turn (again=true): the preserved round
// is consumed by the cap resolution (design section 6.9, task 13).
func (h buildingHandler) resumeBuildRound(ctx context.Context, t store.Ticket, d Deps, round store.Round, u unit) (commit store.HandlerCommit, again bool, err error) {
	if round.SessionID == nil {
		return store.HandlerCommit{}, false, errors.New("job: building: answered round: round has no session id")
	}
	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	sess, state, err := d.Store.SessionByID(ctx, *round.SessionID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: building: answered round: session by id: %w", err)
	}

	// resumeCharge (job.go, design D5, section 7.4): an interrupted latest
	// run resumes this round free and bypasses the exhausted-cap escalation
	// below, even on a session already at max_resumes.
	newestRun, foundRun, newestErr := d.Store.SessionNewestRun(ctx, sess.ID)
	if newestErr != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: building: answered round: newest run: %w", newestErr)
	}
	bump, gate := true, true
	if foundRun {
		bump, gate = resumeCharge(newestRun)
	}

	if state == store.SessionExhausted && gate {
		has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
		if hasErr != nil {
			return store.HandlerCommit{}, false, fmt.Errorf("job: building: answered round: has escalation: %w", hasErr)
		}
		if has {
			slog.Debug("building entry decision", "ticket_id", t.ID, "step", "build_round_capped_again", "session_state", sessionStateName(state))
			return store.HandlerCommit{}, true, nil
		}
		return buildCapResumesEscalation(t, d, sess.ID), false, nil
	}

	_, wt, escalation, err := ensureUnitWorktreeFor(ctx, t, d, u)
	if err != nil {
		return store.HandlerCommit{}, false, err
	}
	if escalation != nil {
		return *escalation, false, nil
	}

	answers, ansErr := answerInputsForRound(round)
	if ansErr != nil {
		return store.HandlerCommit{}, false, ansErr
	}
	if foundRun && newestRun.Interrupted {
		answers = append(answers, prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false})
	}

	runCommit, runErr := h.runBuildResume(ctx, t, d, wt, u, sess, 0, questionIDs(round), answers, bump)
	result, resultErr := withBranchResult(runCommit, runErr, wt)
	return result, false, resultErr
}

// resumeCapGate is the decision tree's shared "needs a resume" gate (design
// section 6): an open session leaves the resume itself to the caller
// (mayResume=true, no commit produced here); an exhausted one either
// escalates resumes_exhausted once (mayResume=false, a real commit) or,
// when that escalation already exists for this session, returns
// ErrNoAction (mayResume=false, no commit) -- design section 6.9's own
// "written once per session" rule.
func (h buildingHandler) resumeCapGate(ctx context.Context, t store.Ticket, d Deps, taskN int, sess store.Session, state store.SessionState) (store.HandlerCommit, bool, error) {
	if state != store.SessionExhausted {
		return store.HandlerCommit{}, true, nil
	}
	has, err := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: building: has escalation: %w", err)
	}
	if has {
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "resume_capped", "session_state", sessionStateName(state))
		return store.HandlerCommit{}, false, ErrNoAction
	}
	return buildCapResumesEscalation(t, d, sess.ID), false, nil
}

// runBuildResume is the RUN resume request every "needs a resume" branch
// sends (design section 6.3): BuildResumeHeader plus inputs, sess's own
// external id, u's own label, routed through buildSuccessCommit.
// resolveIDs is nil for a claims/invalid/interrupted resume (none of them
// resolve a round) and the round's own question ids for an answered
// build-question resume. bump is the caller's own resumeCharge result
// (design D5, section 7.4): false only for an interrupted latest run's own
// free resume, true for every other resume this file sends.
func (h buildingHandler) runBuildResume(ctx context.Context, t store.Ticket, d Deps, wt orchestrator.Worktree, u unit, sess store.Session, priorInvalid int, resolveIDs []int64, inputs []prompt.NamedInput, bump bool) (store.HandlerCommit, error) {
	if sess.ExternalID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resume: session %d has no external id", sess.ID)
	}
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: bump}
	req := runtime.RunRequest{
		Job: response.JobBuild, Label: buildLabel(u.TaskN), WorkDir: wt.Dir(),
		SessionID: *sess.ExternalID, Prompt: prompt.Assemble(prompt.ForBuildResume(inputs)),
	}
	sessionRecord := func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, rr) }
	return runAndRoute(ctx, d, t, jobBuildName, su, req, priorInvalid, sessionRecord, resolveIDs, originFor(u),
		func(rr runResult) (store.HandlerCommit, error) {
			return buildSuccessCommit(t, d, rr, sessionRecord(rr), resolveIDs, u)
		}, seedTaskN(u.TaskN), 0)
}

// ---- escalation resolution (task 13, design section 6.9) ------------------

// enterFromEscalationRound is design section 6.9's own Write/Resolve for a
// building escalation: escID is the newest question's own parent id, the
// escalation message it answers (enterFromRounds' own "the newest
// question's own parent id" distinction). Entered before step 0 (design
// section 6.1: "before step 0"), so a step 0 failure never stands between
// the owner's answer and this resolution. Choice c (abandon) resolves
// every open or answered question and transitions straight to abandoned,
// regardless of origin; choice b, or a reply with no option at all
// (roundChoice's own default), always re-escalates replan_unsupported
// (D14); a retry on code sandbox_unavailable, whichever origin wrote it,
// behaves exactly like a build retry with no run; otherwise the origin
// alone decides (design section 6.9's own table).
func (h buildingHandler) enterFromEscalationRound(ctx context.Context, t store.Ticket, d Deps, round store.Round, escID int64) (store.HandlerCommit, error) {
	escMsg, payload, err := d.Store.EscalationByID(ctx, escID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation %d: %w", escID, err)
	}
	resolveIDs := questionIDs(round)
	choice := roundChoice(round)
	notes := joinReplies(round.Replies)
	errorText := payload.What + "\n" + payload.Why + "\n" + payload.Tried
	origin := response.EscalationOrigin(payload.Origin)

	var commit store.HandlerCommit
	preserved := 0

	switch {
	case choice == escalationChoiceAbandon:
		commit = abandonCommit(t, d, payload.Code)

	case choice != escalationChoiceRetry:
		commit = replanUnsupportedEscalation(t, d, resolveIDs, origin)

	case payload.Code == string(response.EscalationCodeSandboxUnavailable):
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginCapBudget:
		commit = recapBudgetEscalation(t, d, resolveIDs)

	case origin == response.EscalationOriginCapResumes:
		commit, preserved, err = h.retryCapResumes(ctx, t, d, resolveIDs, notes, int64OrZero(payload.SessionID))

	case origin == response.EscalationOriginPerimeter:
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case (origin == response.EscalationOriginBuild || origin == response.EscalationOriginFix) && escMsg.RunID != nil:
		commit, err = h.retryFreshRun(ctx, t, d, resolveIDs, notes, errorText)

	case origin == response.EscalationOriginBuild, origin == response.EscalationOriginFix:
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	default:
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation %d: unrecognized origin %q", escID, payload.Origin)
	}
	if err != nil {
		return commit, err
	}

	slog.Info("escalation resolved", "ticket_id", t.ID, "session_id", int64OrZero(payload.SessionID),
		"run_id", int64OrZero(escMsg.RunID), "code", payload.Code, "origin", payload.Origin,
		"choice", choice, "preserved_rounds", preserved)
	return commit, nil
}

// retryMarkerCommit is design section 6.9's "resolve the round, commit the
// marker retry requested, stay" action: shared by a build retry with no
// run (step 0, CHECK, LAND, or RESOLVE's own escalation, where the next
// tick re-enters the state machine at step 0), a perimeter retry (the next
// tick's DESCRIBE retakes the first undescribed extra on its own), and a
// retry on a sandbox_unavailable escalation of any origin ("as build with
// no run").
func (h buildingHandler) retryMarkerCommit(t store.Ticket, d Deps, resolveIDs []int64) store.HandlerCommit {
	c := baseCommit(t, d)
	c.ResolveQuestions = resolveIDs
	c.Messages = []store.Message{{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: markerRetryRequested}}
	return c
}

// replanUnsupportedEscalation is design D14's own "back to planning is
// deferred" row: re-escalate replan_unsupported with the origin unchanged,
// resolving the round that led here.
func replanUnsupportedEscalation(t store.Ticket, d Deps, resolveIDs []int64, origin response.EscalationOrigin) store.HandlerCommit {
	c := escalationCommit(t, d, nil, nil, string(response.EscalationCodeReplanUnsupported), replanUnsupportedWhat, replanUnsupportedWhy, "", origin)
	c.ResolveQuestions = resolveIDs
	return c
}

// retryFreshRun is design section 6.9's build/fix retry-with-a-run row (and
// 5.6's identical "fix with a run" row for a post-build state, #28 gap 3):
// RUN first turn of the unit in flight, fresh session, inputs notes and
// error (fenced), resolving the round. unitInFlight names that unit for
// whichever of the four states this ticket is in (design section 5.4
// change 3): a task unit in "building" (an error outcome never inserts a
// build_report, so unitInFlight's own nextTaskN still names it, exactly as
// resumeBuildRound draws the identical conclusion for an answered build
// question), or the open fix request's own unit in a post-build state,
// which retryFreshFixRun runs instead.
func (h buildingHandler) retryFreshRun(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, errorText string) (store.HandlerCommit, error) {
	plan, _, ok, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation retry: stored plan: %w", err)
	}
	if !ok {
		return buildEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}
	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation retry: build reports: %w", err)
	}
	u, hasUnit, err := unitInFlight(ctx, t, d, plan, reports)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation retry: unit in flight: %w", err)
	}
	if !hasUnit {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation retry: ticket %d: no unit in flight", t.ID)
	}
	if u.FixRequestID != nil {
		return h.retryFreshFixRun(ctx, t, d, plan, resolveIDs, notes, errorText)
	}

	proj, wt, escalation, err := ensureUnitWorktree(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	extra := []prompt.NamedInput{prompt.Notes(notes), prompt.Error(errorText)}
	commit, runErr := h.runFirst(ctx, t, d, proj, wt, plan, u, len(response.Tasks(plan)), extra, resolveIDs)
	return withBranchResult(commit, runErr, wt)
}

// retryFreshFixRun is retryFreshRun's own fix branch (design section 5.4
// change 3, 5.6's "fix with a run" row, #28 gap 3): re-reads the open fix
// request -- unitInFlight's own unit carries only its FixRequestID, not
// its Text, Kind, or watermark -- and runs runFixFirst with notes and
// error, resolving the round, exactly as retryFreshRun's own task branch
// runs runFirst.
func (h buildingHandler) retryFreshFixRun(ctx context.Context, t store.Ticket, d Deps, plan response.Plan, resolveIDs []int64, notes, errorText string) (store.HandlerCommit, error) {
	req, open, err := openFixRequest(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation retry: open fix request: %w", err)
	}
	if !open {
		return store.HandlerCommit{}, fmt.Errorf("job: building: escalation retry: ticket %d: no open fix request", t.ID)
	}

	proj, wt, escalation, err := ensureUnitWorktreeFor(ctx, t, d, fixUnit(req))
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	extra := []prompt.NamedInput{prompt.Notes(notes), prompt.Error(errorText)}
	commit, runErr := runFixFirst(ctx, t, d, proj, wt, plan, req, extra, resolveIDs)
	return withBranchResult(commit, runErr, wt)
}

// retryCapResumes is design section 6.9's cap_resumes retry row: the
// escalated session's own job (build or perimeter) is read from
// EscalationPayload.SessionID (design section 6.9's own intro sentence),
// which decides which of the two branches below applies. preserved is how
// many rounds, beyond the escalation round itself, this call folds in and
// resolves (the "escalation resolved" log's own preserved_rounds field).
func (h buildingHandler) retryCapResumes(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes string, sessionID int64) (store.HandlerCommit, int, error) {
	sessions, err := d.Store.SessionsForTicket(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: sessions for ticket: %w", err)
	}
	exhausted, found := sessionByID(sessions, sessionID)
	if !found {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: no session %d", t.ID, sessionID)
	}

	allRounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: answered rounds: %w", err)
	}
	var preservedRounds []store.Round
	for _, r := range allRounds {
		if r.SessionID != nil && *r.SessionID == sessionID {
			preservedRounds = append(preservedRounds, r)
		}
	}

	switch exhausted.Job {
	case jobPerimeterName:
		return h.retryCapResumesPerimeter(ctx, t, d, resolveIDs, notes, preservedRounds)
	case jobReviewName:
		return reviewingHandler{}.retryCapResumesReview(ctx, t, d, resolveIDs, notes, sessionID)
	case jobJudgeName:
		return judgeHandler{}.retryCapResumesJudge(ctx, t, d, resolveIDs, sessionID, preservedRounds)
	case jobRespondName:
		return shipHandler{}.retryCapResumesRespond(ctx, t, d, resolveIDs, sessionID, preservedRounds)
	default:
		return h.retryCapResumesBuild(ctx, t, d, resolveIDs, notes, preservedRounds)
	}
}

// sessionByID finds id among sessions (design section 6.9's own "the
// escalated session's job ... is read from EscalationPayload.SessionID").
func sessionByID(sessions []store.Session, id int64) (store.Session, bool) {
	for _, s := range sessions {
		if s.ID == id {
			return s, true
		}
	}
	return store.Session{}, false
}

// retryCapResumesBuild is retryCapResumes' build-session branch (design
// section 6.9): every preserved round of kind question renders into one
// combined answers input (renderRoundAnswers, joined as
// resolveCapResumesEscalation does for planning); a preserved round of
// kind perimeter instead applies RESOLVE steps 1 to 4
// (applyPreservedPerimeterRound: decisions stored, rejected paths
// reverted) and contributes the perimeter notice; the fresh run is RUN's
// first turn of the same unit.
func (h buildingHandler) retryCapResumesBuild(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes string, preservedRounds []store.Round) (store.HandlerCommit, int, error) {
	plan, _, ok, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: stored plan: %w", err)
	}
	if !ok {
		return buildEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), 0, nil
	}
	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: build reports: %w", err)
	}
	u, hasUnit, err := unitInFlight(ctx, t, d, plan, reports)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: unit in flight: %w", err)
	}
	if !hasUnit {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: no unit in flight", t.ID)
	}

	// A fix unit's own fresh run (design section 5.4 change 3, 5.6's "cap_resumes,
	// exhausted session of job build" row for a post-build state, #28 gap 3) goes
	// through runFixFirst, not runFirst, so its own FixRequest -- unitInFlight's
	// own unit carries only its FixRequestID, not its Text, Kind, or watermark --
	// is re-read here.
	var req FixRequest
	if u.FixRequestID != nil {
		var open bool
		req, open, err = openFixRequest(ctx, d, t)
		if err != nil {
			return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: open fix request: %w", err)
		}
		if !open {
			return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: no open fix request", t.ID)
		}
	}

	proj, wt, escalation, err := ensureUnitWorktreeFor(ctx, t, d, u)
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}
	if escalation != nil {
		return *escalation, 0, nil
	}

	allResolveIDs := append([]int64{}, resolveIDs...)
	var answerParts []string
	var fileArtifacts []store.Artifact
	var revertedExtras []orchestrator.Extra

	for _, r := range preservedRounds {
		kind, kindErr := newestQuestionKind(r)
		if kindErr != nil {
			return store.HandlerCommit{}, 0, kindErr
		}
		if kind != response.QuestionKindPerimeter {
			rendered, renderErr := renderRoundAnswers(r)
			if renderErr != nil {
				return store.HandlerCommit{}, 0, renderErr
			}
			answerParts = append(answerParts, rendered)
			allResolveIDs = append(allResolveIDs, questionIDs(r)...)
			continue
		}

		artifacts, reverted, revertErr, applyErr := h.applyPreservedPerimeterRound(ctx, t, d, proj, wt, r)
		if applyErr != nil {
			return store.HandlerCommit{}, 0, applyErr
		}
		if revertErr != nil {
			return withBranch(unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), revertFailedWhat, revertFailedWhy, revertErr.Error()), wt), 0, nil
		}
		fileArtifacts = append(fileArtifacts, artifacts...)
		revertedExtras = append(revertedExtras, reverted...)
		allResolveIDs = append(allResolveIDs, questionIDs(r)...)
	}

	extra := []prompt.NamedInput{prompt.Notes(notes)}
	if len(answerParts) > 0 {
		extra = append(extra, prompt.Answers(strings.Join(answerParts, "\n\n")))
	}
	if len(revertedExtras) > 0 {
		extra = append(extra, prompt.NamedInput{Label: labelPerimeter, Text: orchestrator.PerimeterNotice(revertedExtras)})
	}

	var commit store.HandlerCommit
	var runErr error
	if u.FixRequestID != nil {
		commit, runErr = runFixFirst(ctx, t, d, proj, wt, plan, req, extra, allResolveIDs)
	} else {
		commit, runErr = h.runFirst(ctx, t, d, proj, wt, plan, u, len(response.Tasks(plan)), extra, allResolveIDs)
	}
	result, resultErr := withBranchResult(commit, runErr, wt)
	if resultErr != nil {
		return result, 0, resultErr
	}
	result.Artifacts = append(fileArtifacts, result.Artifacts...)
	return result, len(preservedRounds), nil
}

// applyPreservedPerimeterRound is RESOLVE steps 1 to 4 (design section
// 6.6), applied to round -- a perimeter-kind round preserved through a
// cap_resumes escalation on the build session (design section 6.9) --
// without RESOLVE's own "needs a resume" branch: the caller already knows
// the session is exhausted and folds the resume into its own fresh run.
// It returns the stored file artifacts and the orchestrator.Extra values
// actually reverted (empty when the round rejected nothing, or every
// rejection had already left the tree). revertErr is
// orchestrator.RevertPaths' own failure, for the caller to escalate
// environment/"a rejected file could not be reverted" (design section 6.6
// step 5's own revert-failure rule); err is any other, unexpected failure.
func (h buildingHandler) applyPreservedPerimeterRound(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, round store.Round) (artifacts []store.Artifact, reverted []orchestrator.Extra, revertErr, err error) {
	if round.RunID == nil {
		return nil, nil, nil, errors.New("job: building: cap_resumes retry: perimeter round has no run id")
	}
	rid := *round.RunID

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("job: building: cap_resumes retry: file events: %w", err)
	}
	proposed := newestFileEventPerPath(events)

	newest := round.Questions[len(round.Questions)-1]
	var qp response.QuestionPayload
	if unmarshalErr := json.Unmarshal(newest.Payload, &qp); unmarshalErr != nil {
		return nil, nil, nil, fmt.Errorf("job: building: cap_resumes retry: unmarshal question payload: %w", unmarshalErr)
	}
	decisions := mergedItemDecisions(round.Answers)

	artifacts = make([]store.Artifact, 0, len(qp.Items))
	var rejectedPaths []string
	accepted, rejectedN := 0, 0
	for _, item := range qp.Items {
		row, hasRow := proposed[item.Ref]
		if !hasRow {
			return nil, nil, nil, fmt.Errorf("job: building: cap_resumes retry: %q has no file artifact", item.Ref)
		}
		pd := response.PerimeterReject
		switch d2, hasDecision := decisions[item.Ref]; {
		case hasDecision && d2 == response.DecisionAccept:
			pd = response.PerimeterAccept
		case hasDecision && d2 == response.DecisionReject:
			pd = response.PerimeterReject
		default:
			slog.Warn("perimeter decision defaulted to reject", "ticket_id", t.ID, "path", strconv.Quote(item.Ref))
		}
		if pd == response.PerimeterAccept {
			accepted++
		} else {
			rejectedN++
			rejectedPaths = append(rejectedPaths, item.Ref)
		}
		fa := row.File
		fa.Decision = &pd
		payload, marshalErr := json.Marshal(fa)
		if marshalErr != nil {
			return nil, nil, nil, fmt.Errorf("job: building: cap_resumes retry: marshal file artifact: %w", marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{Type: artifactTypeFile, RunID: row.RunID, Payload: payload})
	}
	slog.Info("perimeter decided", "ticket_id", t.ID, "run_id", rid, "accepted", accepted, "rejected", rejectedN)

	if len(rejectedPaths) == 0 {
		return artifacts, nil, nil, nil
	}

	changed, changedErr := proj.Orch.ChangedPaths(ctx, wt)
	if changedErr != nil {
		return nil, nil, nil, fmt.Errorf("job: building: cap_resumes retry: changed paths: %w", changedErr)
	}
	changedByPath := make(map[string]orchestrator.Change, len(changed))
	for _, c := range changed {
		changedByPath[c.Path] = c
	}

	sort.Strings(rejectedPaths)
	var revertChanges []orchestrator.Change
	var revertExtras []orchestrator.Extra
	for _, p := range rejectedPaths {
		c, stillChanged := changedByPath[p]
		if !stillChanged {
			continue // design section 6.6 step 4: gone already, nothing to revert
		}
		revertChanges = append(revertChanges, c)
		revertExtras = append(revertExtras, orchestrator.Extra{Path: p, Marker: markerForFile(proposed[p].File)})
	}
	if len(revertChanges) == 0 {
		return artifacts, nil, nil, nil
	}

	if rvErr := proj.Orch.RevertPaths(ctx, wt, revertChanges); rvErr != nil {
		return nil, nil, rvErr, nil
	}
	slog.Warn("paths reverted", "ticket_id", t.ID, "run_id", rid, "count", len(revertChanges))
	return artifacts, revertExtras, nil, nil
}

// retryCapResumesPerimeter is retryCapResumes' perimeter-session branch
// (design section 6.9): the path comes from the file row whose RunID is a
// run of the exhausted perimeter session, and the fresh run is DESCRIBE
// for that path, with the preserved rounds' answers as a fenced answers
// input.
func (h buildingHandler) retryCapResumesPerimeter(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes string, preservedRounds []store.Round) (store.HandlerCommit, int, error) {
	if len(preservedRounds) == 0 {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: no preserved perimeter round", t.ID)
	}
	round := preservedRounds[0]
	if round.RunID == nil {
		return store.HandlerCommit{}, 0, errors.New("job: building: cap_resumes retry: perimeter round has no run id")
	}
	perimRunID := *round.RunID

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: file events: %w", err)
	}
	fileRow, foundRow := fileEventForRun(events, perimRunID)
	if !foundRow {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: no file artifact for perimeter run %d", t.ID, perimRunID)
	}
	path := fileRow.File.Path
	taskN := fileRow.File.TaskN

	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: stored plan: %w", err)
	}
	if !havePlan {
		return unitEscalation(t, d, unit{TaskN: taskN}, string(response.EscalationCodeEnvironment), noStoredPlanWhat, noStoredPlanWhy, ""), 0, nil
	}

	proj, wt, escalation, err := ensureUnitWorktreeFor(ctx, t, d, unit{TaskN: taskN})
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}
	if escalation != nil {
		return *escalation, 0, nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return unitEscalation(t, d, unit{TaskN: taskN}, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), 0, nil
	}
	declaredNow := declaredPaths(plan, events, nil, taskN)
	extras := orchestrator.Perimeter(changed, declaredNow, trustRoot, styleGuide)

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: build reports: %w", err)
	}
	report, foundReport := findUnlandedReportForUnit(reports, unit{TaskN: taskN}, nil)
	if !foundReport {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: no unlanded build_report for task %d", t.ID, taskN)
	}

	described := newestFileEventPerPath(events)
	var undescribed []int
	for idx, ex := range extras {
		if !isDescribed(described, ex.Path, report.ArtifactID) {
			undescribed = append(undescribed, idx)
		}
	}
	if len(undescribed) == 0 || extras[undescribed[0]].Path != path {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: building: cap_resumes retry: ticket %d: %q is not the first undescribed extra", t.ID, path)
	}
	i, isLast := undescribed[0], len(undescribed) == 1

	allResolveIDs := append([]int64{}, resolveIDs...)
	var answerParts []string
	for _, r := range preservedRounds {
		rendered, renderErr := renderRoundAnswers(r)
		if renderErr != nil {
			return store.HandlerCommit{}, 0, renderErr
		}
		answerParts = append(answerParts, rendered)
		allResolveIDs = append(allResolveIDs, questionIDs(r)...)
	}
	extra := []prompt.NamedInput{prompt.Notes(notes), prompt.Answers(strings.Join(answerParts, "\n\n"))}

	commit, describeErr := h.describeOne(ctx, t, d, proj, wt, unit{TaskN: taskN}, report.RunID, report, changed, extras, i, isLast, described, extra, allResolveIDs)
	result, resultErr := withBranchResult(commit, describeErr, wt)
	return result, len(preservedRounds), resultErr
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
		sha := reports[i].Report.CommitSHA
		// A fix that changed nothing lands at the existing HEAD, so its sha
		// repeats one already recorded; the branch holds it once.
		if sha != nil && !slices.Contains(out, *sha) {
			out = append(out, *sha)
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

// unrecordedCommits reads wt's own branch commits and reports the ones past
// what reports has recorded (design section 6.1 step 5, section 5.3 step 0
// and 5.4 change 4): building's own Run and the fix driver's own step 0
// both read this before deciding whether to escalate, continue, or adopt a
// single unrecorded commit, so the branch read and the prefix check live
// here once instead of twice. ok is false when recorded is not a prefix of
// the branch's own commits (a recorded commit is missing from the branch,
// or the branch holds them in a different order); the caller escalates
// branchMissingRecordedWhat/Why itself, since the two callers' own
// escalations carry different origins (building has no unit yet; a fix
// unit is always origin fix).
func unrecordedCommits(ctx context.Context, proj Project, wt orchestrator.Worktree, reports []store.BuildReportRow) (unrecorded []string, ok bool, err error) {
	shas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return nil, false, fmt.Errorf("job: building: branch commits: %w", err)
	}
	recorded := recordedShas(reports)
	if !isPrefixOf(recorded, shas) {
		return nil, false, nil
	}
	return shas[len(recorded):], true, nil
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

// unitFor finds tasks' entry numbered taskN and builds its unit, its
// commit subject built by unitTitle (design section 4.3's six-step rule).
func unitFor(tasks []response.Task, taskN int) (unit, bool) {
	for _, tk := range tasks {
		if tk.N == taskN {
			return unit{TaskN: taskN, Title: unitTitle(taskN, tk.Text)}, true
		}
	}
	return unit{}, false
}

// unitInFlight generalizes nextTaskN/unitFor's own "which unit is this
// ticket building" question over every state a unit can be mid-flight in
// (design section 5.4 change 3, #28 gap 3): in "building", the plan's
// lowest unlanded task, exactly what nextTaskN and unitFor already gave
// resumeBuildRound, retryFreshRun, and retryCapResumesBuild; in
// "reviewing", "judging", and "shipping", every task has landed (design
// section 5.5), so the unit in flight is always the open fix request's own
// unit. ok is false when there is none -- an already-done build (step 2's
// own "no next task" case) or a post-build state with no open fix request.
// resumeBuildRound, retryFreshRun, retryCapResumesBuild, and (task 4)
// adopt call this instead of nextTaskN, so each works unchanged whichever
// of the four states calls it.
func unitInFlight(ctx context.Context, t store.Ticket, d Deps, plan response.Plan, reports []store.BuildReportRow) (unit, bool, error) {
	switch t.State {
	case stateBuilding:
		taskN, hasNext := nextTaskN(response.Tasks(plan), reports)
		if !hasNext {
			return unit{}, false, nil
		}
		u, ok := unitFor(response.Tasks(plan), taskN)
		return u, ok, nil
	case stateReviewing, stateJudging, stateShipping:
		req, open, err := openFixRequest(ctx, d, t)
		if err != nil {
			return unit{}, false, fmt.Errorf("job: unit in flight: open fix request: %w", err)
		}
		if !open {
			return unit{}, false, nil
		}
		return fixUnit(req), true, nil
	default:
		return unit{}, false, nil
	}
}

// subjectSentenceRunes is step 2's own window (design section 4.3): a
// sentence end within the task text's first 60 runes ends the subject
// there, before "Task <n>: " is even prefixed.
const subjectSentenceRunes = 60

// subjectMaxRunes is step 4's own cap (design section 4.3): a subject
// longer than this is cut at the last space at or before this many runes,
// never mid-word.
const subjectMaxRunes = 72

// subjectTrimCutset is step 5's own trailing set: spaces, then any of the
// listed punctuation and a backtick, trimmed together in one pass since
// strings.TrimRight already removes a trailing run of mixed cutset
// characters, not just one.
const subjectTrimCutset = " ,;:.-(`"

// unitTitle builds one build unit's commit subject from its task text, the
// six-step rule of design section 4.3: trim a leading marker, cut at an
// early sentence end, prefix "Task <n>: ", cut a long subject at a word
// boundary rather than mid-word, trim trailing punctuation, and fix a
// stray unbalanced backtick the cut can leave behind. Step 6's fallback
// ("Task <n>" alone) falls out of steps 3-5 on its own: an empty or
// all-punctuation first line collapses to exactly that string, with
// nothing left to special-case.
func unitTitle(taskN int, taskText string) string {
	first, _, _ := strings.Cut(taskText, "\n")
	first = strings.TrimLeft(first, "#*- ")
	if before, ok := cutAtSentenceEnd(first, subjectSentenceRunes); ok {
		first = before
	}

	subject := fmt.Sprintf("Task %d: %s", taskN, first)
	subject = cutAtLastSpace(subject, subjectMaxRunes)
	subject = strings.TrimRight(subject, subjectTrimCutset)
	return fixOddBacktick(subject)
}

// cutAtSentenceEnd returns the text before a sentence end (". " or a final
// ".") within s's first limit runes, and whether one was found (design
// section 4.3 step 2).
func cutAtSentenceEnd(s string, limit int) (string, bool) {
	r := []rune(s)
	if limit > len(r) {
		limit = len(r)
	}
	for i := range limit {
		if r[i] != '.' {
			continue
		}
		if i+1 < len(r) && r[i+1] == ' ' {
			return string(r[:i]), true
		}
		if i == len(r)-1 {
			return string(r[:i]), true
		}
	}
	return s, false
}

// cutAtLastSpace cuts s at the last space at or before rune n, when s is
// longer than n runes (design section 4.3 step 4): a subject never splits
// a word. With no space in that span, it falls back to a hard cut at n.
func cutAtLastSpace(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	last := -1
	for i, ch := range r[:n] {
		if ch == ' ' {
			last = i
		}
	}
	if last == -1 {
		return string(r[:n])
	}
	return string(r[:last])
}

// fixOddBacktick removes the last backtick in s when s holds an odd number
// of them (design section 4.3 step 5): a cut that lands inside a fenced
// span leaves one unbalanced backtick behind, the exact defect version 1
// of this rule produced and the M1 validation caught.
func fixOddBacktick(s string) string {
	if strings.Count(s, "`")%2 == 0 {
		return s
	}
	i := strings.LastIndex(s, "`")
	return s[:i] + s[i+1:]
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
// Decision accept and is in scope for taskN (design section 9.1's own
// "accepted" prompt input; design rule 4's own extraInScope).
func acceptedPaths(plan response.Plan, events []store.FileEventRow, taskN int) []string {
	var out []string
	for _, row := range newestFileEventPerPath(events) {
		if row.File.Decision != nil && *row.File.Decision == response.PerimeterAccept && extraInScope(plan, row.File, taskN) {
			out = append(out, row.File.Path)
		}
	}
	sort.Strings(out)
	return out
}

// declaredPaths is CHECK's declaredBefore/declaredNow (design section 6.4
// step 3): the plan's declared files plus every accepted path in scope for
// taskN, or, with before non-nil, only the accepted paths whose deciding
// artifact id is lower than *before (declaredBefore); nil includes every
// accepted path in scope (declaredNow).
func declaredPaths(plan response.Plan, events []store.FileEventRow, before *int64, taskN int) []string {
	out := planFilePaths(plan)
	for _, row := range newestFileEventPerPath(events) {
		if row.File.Decision == nil || *row.File.Decision != response.PerimeterAccept {
			continue
		}
		if before != nil && row.ArtifactID >= *before {
			continue
		}
		if !extraInScope(plan, row.File, taskN) {
			continue
		}
		out = append(out, row.File.Path)
	}
	return out
}

// extraInScope reports whether accepted extra fa counts for the unit
// numbered taskN: always for a fix unit (0) or a plan with no task mapping,
// otherwise only when the owner accepted it for taskN.
func extraInScope(plan response.Plan, fa response.FileArtifact, taskN int) bool {
	return taskN == 0 || !response.TaskMapped(plan) || fa.TaskN == taskN
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
// buildSuccessCommit. extra carries the escalation-retry row's own "notes
// and error" or "notes and answers" inputs (design section 6.9); the step
// 2 entry point (Run, below) passes nil. resolveIDs resolves an escalation
// round's questions in the same commit as the fresh run's own
// terminalizing commit; step 2's own first turn resolves nothing, so it
// passes nil.
func (h buildingHandler) runFirst(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, u unit, totalTasks int, extra []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
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
	accepted := acceptedPaths(plan, events, u.TaskN)

	schemas, err := renderSchemas(response.JobBuild, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", err)
	}

	titlePrefix := fmt.Sprintf("Task %d: ", u.TaskN)
	bt := prompt.BuildTask{N: u.TaskN, Total: totalTasks, Title: strings.TrimPrefix(u.Title, titlePrefix), Text: tk.Text, Test: tk.Test}

	approvalNotes, err := d.Store.ApprovalNotes(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: approval notes: %w", err)
	}

	in, err := prompt.ForBuild(promptText, bt, proj.TestCmd, proj.LintCmd, t.Title+"\n\n"+t.Body, planXML, approvalNotes, accepted, extra)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", err)
	}
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobBuildName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobBuild, Label: strconv.Itoa(u.TaskN), WorkDir: wt.Dir(), Prompt: assembled}
	n := u.TaskN
	return runAndRoute(ctx, d, t, jobBuildName, su, req, 0, freshSessionRecord, resolveIDs, originFor(u),
		func(rr runResult) (store.HandlerCommit, error) {
			return buildSuccessCommit(t, d, rr, freshSessionRecord(rr), resolveIDs, u)
		}, &n, 0)
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
		claims := resp.Claims
		if claims.FilesChanged == nil {
			// A run that changed nothing decodes to a nil slice, which the
			// build_report schema refuses as null.
			claims.FilesChanged = []string{}
		}
		report := response.BuildReport{
			TaskN:       u.TaskN,
			BuildClaims: claims,
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
		return questionOutcomeCommit(t, d, rr, resp.Questions, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, originFor(u)), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: building: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// ---- CHECK, LAND --------------------------------------------------------

// runCheckCommand runs one CHECK command re-run (design section 6.4 step
// 1), logging "command re-run" (design section 11) regardless of outcome.
// rid is the unit's newest ok run when one is already known (CHECK); it is
// nil during step 0.5's adoption commands, run before the adopted commit's
// own run is identified (design section 6.1 step 5's checks 1-2 precede
// check 4, which is the first to name a run). A timeout reports exit -1
// with timedOut true and no error; any other CommandRunner failure
// (ErrSandbox, a wrapped context.Canceled, or anything else) is returned
// unclassified for the caller to route.
func runCheckCommand(ctx context.Context, d Deps, t store.Ticket, wt orchestrator.Worktree, proj Project, rid *int64, kind, shellCmd string) (exit int, timedOut bool, err error) {
	started := time.Now()
	exit, runErr := d.Commands.Run(ctx, wt.Dir(), proj.RepoGit, shellCmd, checkCommandTimeout)
	seconds := int(time.Since(started).Seconds())
	switch {
	case runErr == nil:
		slog.Info("command re-run", "ticket_id", t.ID, "run_id", int64OrZero(rid), "command", kind, "exit_code", exit, "seconds", seconds, "timed_out", false)
		return exit, false, nil
	case errors.Is(runErr, ErrCommandTimeout):
		slog.Info("command re-run", "ticket_id", t.ID, "run_id", int64OrZero(rid), "command", kind, "exit_code", -1, "seconds", seconds, "timed_out", true)
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
// its result directly rather than falling through. u is the unit whose
// commands failed (design section 5.4 change 2): check and adopt both call
// this, so the escalation it writes carries origin fix when u is a fix
// unit, origin build otherwise.
func commandInfraEscalation(t store.Ticket, d Deps, u unit, err error) (store.HandlerCommit, error) {
	switch {
	case errors.Is(err, ErrSandbox):
		return unitEscalation(t, d, u, string(response.EscalationCodeSandboxUnavailable), sandboxUnavailableWhat, d.Sandboxes.Build.Reason(), ""), nil
	case errors.Is(err, context.Canceled):
		return store.HandlerCommit{}, runtime.ErrCanceled
	default:
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), projectCommandsNotRunWhat, projectCommandsNotRunWhy, err.Error()), nil
	}
}

// claimsFilesChangedPath is the element path CheckBuildClaims reports a
// files_changed mismatch under; ownership errors share it.
const claimsFilesChangedPath = "claims/files_changed"

// foreignTaskPaths returns one claim error per path in changed that the
// plan assigns only to tasks other than taskN, in changed's order, each
// naming the owners as "task 2", "tasks 2 and 3", or "tasks 1, 2 and 4". A
// fix unit (taskN 0) and a plan with no task mapping (stored before files
// named tasks) get none: both keep whole-plan scope.
func foreignTaskPaths(plan response.Plan, taskN int, changed []string) []*response.PathError {
	if taskN == 0 || !response.TaskMapped(plan) {
		return nil
	}
	owners := make(map[string][]int)
	for _, f := range response.Files(plan) {
		owners[f.Path] = append(owners[f.Path], response.FileTasks(f)...)
	}
	var errs []*response.PathError
	for _, p := range changed {
		own := owners[p]
		if len(own) == 0 || slices.Contains(own, taskN) {
			continue
		}
		sorted := slices.Compact(slices.Sorted(slices.Values(own)))
		owner := fmt.Sprintf("task %d", sorted[0])
		if len(sorted) > 1 {
			parts := make([]string, len(sorted))
			for i, n := range sorted {
				parts[i] = strconv.Itoa(n)
			}
			owner = "tasks " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
		}
		errs = append(errs, &response.PathError{
			Path: claimsFilesChangedPath,
			Msg:  fmt.Sprintf("%s belongs to %s, not task %d", p, owner, taskN),
		})
	}
	return errs
}

// check runs design section 6.4's CHECK, shared by the first check and the
// check-before-landing recheck (firstCheck tells them apart only for the
// "claims ok" marker write). rid is the unit's newest ok run; report is
// that run's own build_report row.
func (h buildingHandler) check(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, u unit, rid int64, report store.BuildReportRow, firstCheck bool) (store.HandlerCommit, error) {
	testExit, testTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, &rid, "test", proj.TestCmd)
	if err != nil {
		return commandInfraEscalation(t, d, u, err)
	}
	lintExit, lintTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, &rid, "lint", proj.LintCmd)
	if err != nil {
		return commandInfraEscalation(t, d, u, err)
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), nil
	}

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: check: file events: %w", err)
	}
	artifactID := report.ArtifactID
	declaredBefore := declaredPaths(plan, events, &artifactID, u.TaskN)
	declaredNow := declaredPaths(plan, events, nil, u.TaskN)

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

	foreign := foreignTaskPaths(plan, u.TaskN, changedPathList(changed))
	if len(foreign) > 0 {
		msgs := make([]string, len(foreign))
		for i, e := range foreign {
			msgs[i] = e.Msg
		}
		slog.Warn("task scope violation", "ticket_id", t.ID, "run_id", rid, "task_n", u.TaskN, "foreign", msgs)
	}
	errs = append(errs, foreign...)

	slog.Info("claim check", "ticket_id", t.ID, "run_id", rid, "task_n", u.TaskN, "errors", len(errs), "changed", len(changed), "extras", len(extras))

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
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", u.TaskN, "step", "describe_new_extra", "session_state", "n/a")
		return store.HandlerCommit{}, ErrNoAction
	default:
		approved := changedPathList(changed)
		return h.land(ctx, t, d, proj, wt, plan, u, rid, report, approved)
	}
}

// land is design section 6.7's LAND: commit exactly approved, record the
// landed build_report, and transition to reviewing when this was the last
// task. When u is a fix unit (u.FixRequestID set), the commit also carries
// "fix landed <id> sha <sha>" (design D22, section 5.1, 5.3): the marker
// openFixRequest reads to know this request's own unit has landed.
func (h buildingHandler) land(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, u unit, rid int64, report store.BuildReportRow, approved []string) (store.HandlerCommit, error) {
	msg := orchestrator.CommitMessage{Title: report.Report.Title, FuncLines: funcLines(plan, approved), Fences: report.Report.Fences}
	var sha string
	var err error
	if len(approved) == 0 && u.FixRequestID != nil {
		// A fix that checked clean with nothing changed lands at the current
		// HEAD (bug fix: the failure came from outside the code, the builder
		// rightly changed nothing, and a fix could only land through a
		// commit, so every retry escalated). Review and the judge then run
		// again at the same sha.
		sha, err = proj.Orch.HeadSHA(ctx, wt)
	} else {
		sha, err = proj.Orch.CommitTask(ctx, wt, approved, msg)
	}
	if err != nil {
		what, why := taskNotCommittedWhat, taskNotCommittedWhy
		if strings.Contains(err.Error(), "commit signing failed") {
			what, why = commitSigningFailedWhat, commitSigningFailedWhy
		}
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), what, why, err.Error()), nil
	}

	c, err := landCommit(t, d, plan, u.TaskN, rid, report.Report, sha)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if u.FixRequestID != nil {
		c.Messages = append(c.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("fix landed %d sha %s", *u.FixRequestID, sha),
		})
	}
	slog.Info("unit landed", "ticket_id", t.ID, "run_id", rid, "task_n", u.TaskN, "commit_sha", sha, "files", len(approved))
	if c.Next != "" {
		slog.Info("build done", "ticket_id", t.ID, "tasks", len(response.Tasks(plan)), "commits", u.TaskN)
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

// ---- DESCRIBE, ASK ------------------------------------------------------

// jobPerimeterName is the job.go/machine.toml key DESCRIBE's own session
// and job config are keyed under, "perimeter" (design section 4.3, 6.5).
const jobPerimeterName = string(response.JobPerimeter)

// markerTrustRoot and markerStyleGuide are the two non-empty values
// orchestrator.Perimeter's own Extra.Marker carries (perimeter.go,
// unexported there): this file matches on them by value to fill
// FileArtifact.TrustRoot/StyleGuide and the ASK item text's bracketed
// prefix (design section 4.1, 6.5).
const (
	markerTrustRoot  = "trust root"
	markerStyleGuide = "style guide"
)

// describeOrAsk is design section 6.5's DESCRIBE and ASK, entered once
// CHECK has written "claims ok run <rid>" and left one or more extras in
// the tree. It describes one undeclared path per tick with a fresh
// perimeter run (DESCRIBE), and once every extra carries a description,
// posts the one question the owner decides them all from (ASK) -- in the
// same commit as the last DESCRIBE when that call is what completes the
// set, or alone, with no runtime call, when a later CHECK found a new
// extra after every earlier one was already described.
func (h buildingHandler) describeOrAsk(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, u unit, rid int64, report store.BuildReportRow, events []store.FileEventRow, changed []orchestrator.Change, extras []orchestrator.Extra) (store.HandlerCommit, error) {
	described := newestFileEventPerPath(events)

	var undescribed []int
	for i, ex := range extras {
		if !isDescribed(described, ex.Path, report.ArtifactID) {
			undescribed = append(undescribed, i)
		}
	}

	if len(undescribed) > 0 {
		return h.describeOne(ctx, t, d, proj, wt, u, rid, report, changed, extras, undescribed[0], len(undescribed) == 1, described, nil, nil)
	}

	// Every extra is described. A ticket already waiting on a perimeter
	// answer must not get a second, identical question: the owner's answer
	// is handled at step 1 (AnsweredRounds), before this step is ever
	// reached again, so checking the ticket's own Waiting here is enough
	// to make ASK idempotent without a further store read (design section
	// 6.5 step 5).
	if t.WaitingOn != nil && *t.WaitingOn == string(response.QuestionKindPerimeter) {
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", u.TaskN, "step", "perimeter_open", "session_state", "n/a")
		return store.HandlerCommit{}, ErrNoAction
	}

	return askCommit(t, d, u.TaskN, rid, perimeterItems(extras, described))
}

// isDescribed is design section 6.5 step 3: path is described when its
// newest file artifact (across the whole ticket; a stale row from an
// earlier unit is filtered out by the artifact id check) carries an id
// greater than the unit's own unlanded build_report and a non-empty
// Description.
func isDescribed(described map[string]store.FileEventRow, path string, reportArtifactID int64) bool {
	row, ok := described[path]
	return ok && row.ArtifactID > reportArtifactID && row.File.Description != ""
}

// describeOne runs one DESCRIBE turn for extras[i], the first undescribed
// path by path order (design section 6.5 step 4). isLast is true when this
// was the only undescribed extra left: a successful run's own commit then
// also does ASK, in the same store commit. answerInputs and resolveIDs are
// design section 6.9's own cap_resumes retry row, "the fresh run is
// DESCRIBE for that path with the preserved answers as a fenced answers
// input": the ordinary DESCRIBE tick (describeOrAsk) passes both nil.
func (h buildingHandler) describeOne(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, u unit, rid int64, report store.BuildReportRow, changed []orchestrator.Change, extras []orchestrator.Extra, i int, isLast bool, described map[string]store.FileEventRow, answerInputs []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
	extra := extras[i]
	change, ok := changeFor(changed, extra.Path)
	if !ok {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), treeExtraUnclaimedWhat, treeExtraUnclaimedWhy, extra.Path), nil
	}
	claim, ok := extraClaimFor(report.Report.Extras, extra.Path)
	if !ok {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), treeExtraUnclaimedWhat, treeExtraUnclaimedWhy, extra.Path), nil
	}

	hunk, err := proj.Orch.Hunk(ctx, wt, change)
	if err != nil {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), nil
	}

	jobCfg := d.Machine.Jobs[jobPerimeterName]
	promptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: describe: %w", err)
	}
	schemas, err := renderSchemas(response.JobPerimeter, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: describe: %w", err)
	}

	in := prompt.ForPerimeter(promptText, extra.Path, hunk, answerInputs)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	priorInvalid, _, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobPerimeterName, nil)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: describe: consecutive invalid outputs: %w", err)
	}

	su := store.SessionUpsert{Job: jobPerimeterName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobPerimeter, Label: perimeterLabel(u.TaskN, i+1), WorkDir: wt.Dir(), Prompt: assembled}
	return runAndRoute(ctx, d, t, jobPerimeterName, su, req, priorInvalid, freshSessionRecord, resolveIDs, response.EscalationOriginPerimeter,
		func(rr runResult) (store.HandlerCommit, error) {
			c, err := perimeterSuccessCommit(t, d, rr, extra, change, claim, u.TaskN, rid, isLast, extras, described)
			if err != nil {
				return store.HandlerCommit{}, err
			}
			c.ResolveQuestions = resolveIDs
			return c, nil
		}, seedTaskN(u.TaskN), 0)
}

// perimeterSuccessCommit routes a perimeter run's parsed response (design
// section 6.5 step 4): ok inserts one file artifact carrying the
// description, adding ASK to the same commit when this was the last
// undescribed extra; question and error are the universal outcomes
// classify and build already share, with the question case also linking a
// no-description file row to the run so a later resume can find its path
// (design section 6.2's "perimeter-job question" round).
func perimeterSuccessCommit(t store.Ticket, d Deps, rr runResult, extra orchestrator.Extra, change orchestrator.Change, claim response.ExtraClaim, taskN int, buildRunID int64, isLast bool, extras []orchestrator.Extra, described map[string]store.FileEventRow) (store.HandlerCommit, error) {
	perimRunID := rr.Reserved.RunID

	switch resp := rr.Res.Response.(type) {
	case *response.PerimeterResponse:
		fa := fileArtifactFor(extra, change, claim, taskN, resp.Reason)
		payload, err := json.Marshal(fa)
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: describe: marshal file artifact: %w", err)
		}
		c := baseCommit(t, d)
		c.Runs = terminalRuns(rr, string(response.OutcomeOk))
		c.Session = freshSessionRecord(rr)
		c.Artifacts = []store.Artifact{{Type: artifactTypeFile, RunID: &perimRunID, Payload: payload}}
		slog.Info("perimeter described", "ticket_id", t.ID, "run_id", perimRunID, "task_n", taskN, "path", strconv.Quote(extra.Path), "marker", extra.Marker)

		if isLast {
			items := perimeterItems(extras, describedWith(described, extra.Path, fa))
			askC, askErr := askCommit(t, d, taskN, buildRunID, items)
			if askErr != nil {
				return store.HandlerCommit{}, askErr
			}
			c.Messages = askC.Messages
			c.Waiting = askC.Waiting
		}
		return c, nil

	case *response.QuestionResponse:
		c, err := questionOutcomeCommit(t, d, rr, resp.Questions, freshSessionRecord(rr), nil)
		if err != nil {
			return store.HandlerCommit{}, err
		}
		fa := fileArtifactFor(extra, change, claim, taskN, "")
		payload, err := json.Marshal(fa)
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: describe: marshal file artifact: %w", err)
		}
		c.Artifacts = append(c.Artifacts, store.Artifact{Type: artifactTypeFile, RunID: &perimRunID, Payload: payload})
		return c, nil

	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, freshSessionRecord(rr), nil, response.EscalationOriginPerimeter), nil

	default:
		return store.HandlerCommit{}, fmt.Errorf("job: building: describe: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// fileArtifactFor builds one FileArtifact for extra (design section 4.1,
// 6.5): action from change's git status, reason the builder's own
// ExtraClaim.Reason, trust_root/style_guide from extra's own marker, and
// description (empty when the perimeter run has none to give yet: the
// question-outcome case, whose row only links the run to the path).
func fileArtifactFor(extra orchestrator.Extra, change orchestrator.Change, claim response.ExtraClaim, taskN int, description string) response.FileArtifact {
	return response.FileArtifact{
		Path: extra.Path, Action: fileActionFor(change.Code), Reason: claim.Reason,
		TrustRoot:   extra.Marker == markerTrustRoot,
		StyleGuide:  extra.Marker == markerStyleGuide,
		TaskN:       taskN,
		Description: description,
	}
}

// describedWith returns described with path's entry replaced by fa, a
// shallow copy so the caller's own map is untouched: the just-completed
// DESCRIBE has no store row yet when this same tick's commit also does ASK
// (design section 6.5 step 4's "same commit").
func describedWith(described map[string]store.FileEventRow, path string, fa response.FileArtifact) map[string]store.FileEventRow {
	out := make(map[string]store.FileEventRow, len(described)+1)
	maps.Copy(out, described)
	out[path] = store.FileEventRow{File: fa}
	return out
}

// perimeterItems builds one Item per extra, in path order (design section
// 6.5's "ASK adds to the commit"): Item.Ref is the path, and Item.Text is
// the marker in brackets when set, then the builder's own reason, then the
// perimeter run's own description.
func perimeterItems(extras []orchestrator.Extra, described map[string]store.FileEventRow) []response.Item {
	items := make([]response.Item, len(extras))
	for i, ex := range extras {
		items[i] = response.Item{Ref: ex.Path, Text: itemText(described[ex.Path].File)}
	}
	return items
}

// itemText is Item.Text's own format (design section 6.5): the marker in
// brackets when set ("[trust root] " or "[style guide] "), then
// "Builder: <reason> ", then "Change: <description>".
func itemText(fa response.FileArtifact) string {
	prefix := ""
	switch {
	case fa.TrustRoot:
		prefix = "[trust root] "
	case fa.StyleGuide:
		prefix = "[style guide] "
	}
	return prefix + "Builder: " + fa.Reason + " Change: " + fa.Description
}

// askCommit is design section 6.5's ASK: one perimeter question, one item
// per extra in path order, carrying the build run's own id explicitly (not
// AttachRunToMsgs: an ASK-alone commit reserves no run at all), no runtime
// call.
func askCommit(t store.Ticket, d Deps, taskN int, rid int64, items []response.Item) (store.HandlerCommit, error) {
	noun := "file"
	if len(items) != 1 {
		noun = "files"
	}
	body := fmt.Sprintf(
		"Confirm the file perimeter\n\nTask %d changed %s %s outside the plan's declared files. Accept a file to commit it. Reject a file to revert it.",
		taskN, orchestrator.CountWord(len(items)), noun,
	)
	payload, err := json.Marshal(response.QuestionPayload{
		Kind: response.QuestionKindPerimeter, State: response.QuestionStateOpen,
		Recommended: "Decide each file", Options: []response.Option{}, Items: items,
	})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: ask: marshal question payload: %w", err)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, RunID: &rid, Type: msgTypeQuestion, Author: authorZing,
		State: new(questionStateOpen), Body: body, Payload: payload,
	}}
	waiting := string(response.QuestionKindPerimeter)
	c.Waiting = &waiting
	slog.Info("perimeter asked", "ticket_id", t.ID, "run_id", rid, "task_n", taskN, "items", len(items))
	return c, nil
}

// changeFor finds path's own Change in changed.
func changeFor(changed []orchestrator.Change, path string) (orchestrator.Change, bool) {
	for _, c := range changed {
		if c.Path == path {
			return c, true
		}
	}
	return orchestrator.Change{}, false
}

// extraClaimFor finds path's own ExtraClaim in claims (a build_report's own
// Extras): the builder's justification, design section 6.5's own "the
// builder's ExtraClaim.Reason".
func extraClaimFor(claims []response.ExtraClaim, path string) (response.ExtraClaim, bool) {
	for _, c := range claims {
		if c.Path == path {
			return c, true
		}
	}
	return response.ExtraClaim{}, false
}

// fileActionFor maps a changed path's git status to the file action an
// extra's FileArtifact carries (design section 4.1): Added and Untracked
// become create, Modified becomes modify, Deleted becomes delete.
func fileActionFor(code orchestrator.Status) response.FileAction {
	switch code {
	case orchestrator.Added, orchestrator.Untracked:
		return response.FileActionCreate
	case orchestrator.Deleted:
		return response.FileActionDelete
	default:
		return response.FileActionModify
	}
}

// ---- RESOLVE, and the perimeter run's own question (task 11) -------------

// newestQuestionKind unmarshals round's newest question's payload and
// returns its Kind (design section 6.2's own first test, checked before
// round.Job): a perimeter-kind question is always RESOLVE, whichever run's
// id the round is grouped by.
func newestQuestionKind(round store.Round) (response.QuestionKind, error) {
	newest := round.Questions[len(round.Questions)-1]
	var qp response.QuestionPayload
	if err := json.Unmarshal(newest.Payload, &qp); err != nil {
		return "", fmt.Errorf("job: building: unmarshal round question %d payload: %w", newest.ID, err)
	}
	return qp.Kind, nil
}

// mergedItemDecisions merges every sent answer's AnswerPayload.Items into
// one ref->decision map, newest answer winning per ref (design section 6.6
// step 1): answers arrive in ascending id order (AnsweredRounds' own
// Answers), so a later maps.Copy simply overwrites an earlier one.
func mergedItemDecisions(answers []store.MessageRow) map[string]response.Decision {
	out := make(map[string]response.Decision)
	for i := range answers {
		var ap response.AnswerPayload
		if err := json.Unmarshal(answers[i].Payload, &ap); err != nil {
			continue
		}
		maps.Copy(out, ap.Items)
	}
	return out
}

// markerForFile renders one decided or proposed FileArtifact's marker as an
// orchestrator.Extra's own Marker value (design section 4.1, 6.5):
// "trust root" when TrustRoot, "style guide" when StyleGuide, else "".
func markerForFile(fa response.FileArtifact) string {
	switch {
	case fa.TrustRoot:
		return markerTrustRoot
	case fa.StyleGuide:
		return markerStyleGuide
	default:
		return ""
	}
}

// findUnlandedReportForUnit returns reports' unlanded (no CommitSHA) row
// for u, the unit currently in flight (design D22, #28 gap 1). A nil
// runIDs matches on u.TaskN alone, unchanged from before this rename: a
// task's own number is never reused, so no row of a different unit can
// share it. A non-nil runIDs (SessionRunIDs of a session after the
// watermark, design section 5.3) also requires the row's own RunID to
// belong to it, the scoping a fix unit needs since its TaskN is always 0,
// shared by every fix that ever ran on the ticket.
func findUnlandedReportForUnit(reports []store.BuildReportRow, u unit, runIDs []int64) (store.BuildReportRow, bool) {
	var runSet map[int64]bool
	if runIDs != nil {
		runSet = make(map[int64]bool, len(runIDs))
		for _, id := range runIDs {
			runSet[id] = true
		}
	}
	for i := range slices.Backward(reports) {
		if reports[i].Report.TaskN != u.TaskN || reports[i].Report.CommitSHA != nil {
			continue
		}
		if runSet != nil && !runSet[reports[i].RunID] {
			continue
		}
		return reports[i], true
	}
	return store.BuildReportRow{}, false
}

// fileEventForRun returns the newest file event whose RunID equals runID
// (design section 6.2: "the file artifact whose RunID equals round.RunID").
func fileEventForRun(events []store.FileEventRow, runID int64) (store.FileEventRow, bool) {
	for i := range slices.Backward(events) {
		if events[i].RunID != nil && *events[i].RunID == runID {
			return events[i], true
		}
	}
	return store.FileEventRow{}, false
}

// extraFor finds path's own Extra in extras, and its index: the position
// resolvePerimeterQuestion's own resume label (perimeterLabel) must match
// the position describeOne originally minted that path's own session label
// from (review F052 class).
func extraFor(extras []orchestrator.Extra, path string) (orchestrator.Extra, int, bool) {
	for i, e := range extras {
		if e.Path == path {
			return e, i, true
		}
	}
	return orchestrator.Extra{}, -1, false
}

// resolve is design section 6.6's RESOLVE: entered from step 1 when the
// newest question of round is kind perimeter. It applies the owner's
// per-path accept/reject decisions, reverts every rejected path still in
// the tree, and resumes the build session with the perimeter notice when a
// revert happened. When every decision is accept (or every reject was
// already gone from the tree), it stores the decisions and stays: the next
// tick reaches Run's own "checked, no undecided extra" branch, which calls
// h.check with firstCheck false -- the check before landing -- and lands
// once it passes (design section 6.4 steps 3 to 6).
func (h buildingHandler) resolve(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	if round.RunID == nil {
		return store.HandlerCommit{}, errors.New("job: building: resolve: round has no run id")
	}
	rid := *round.RunID
	resolveIDs := questionIDs(round)

	newest := round.Questions[len(round.Questions)-1]
	var qp response.QuestionPayload
	if err := json.Unmarshal(newest.Payload, &qp); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: unmarshal question payload: %w", err)
	}

	proj, wt, escalation, err := ensureUnitWorktree(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: build reports: %w", err)
	}
	report, foundReport := findUnitReport(reports, rid)
	if !foundReport {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: ticket %d: no build_report for run %d", t.ID, rid)
	}
	// u names the unit this answered perimeter round belongs to (design
	// section 5.4 change 2): report.Report.TaskN is 0 for a fix unit (D22),
	// so unitEscalation and originFor below tag origin fix or build
	// correctly, whichever unit's own extras the owner just decided.
	u := unit{TaskN: report.Report.TaskN, Title: report.Report.Title}

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: file events: %w", err)
	}
	proposed := newestFileEventPerPath(events)

	decisions := mergedItemDecisions(round.Answers)

	artifacts := make([]store.Artifact, 0, len(qp.Items))
	var rejectedPaths []string
	accepted, rejected := 0, 0
	for _, item := range qp.Items {
		row, hasRow := proposed[item.Ref]
		if !hasRow {
			return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: %q has no file artifact", item.Ref)
		}

		pd := response.PerimeterReject
		switch d2, hasDecision := decisions[item.Ref]; {
		case hasDecision && d2 == response.DecisionAccept:
			pd = response.PerimeterAccept
		case hasDecision && d2 == response.DecisionReject:
			pd = response.PerimeterReject
		default:
			slog.Warn("perimeter decision defaulted to reject", "ticket_id", t.ID, "path", strconv.Quote(item.Ref))
		}
		if pd == response.PerimeterAccept {
			accepted++
		} else {
			rejected++
			rejectedPaths = append(rejectedPaths, item.Ref)
		}

		fa := row.File
		fa.Decision = &pd
		payload, marshalErr := json.Marshal(fa)
		if marshalErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: marshal file artifact: %w", marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{Type: artifactTypeFile, RunID: row.RunID, Payload: payload})
	}
	slog.Info("perimeter decided", "ticket_id", t.ID, "run_id", rid, "accepted", accepted, "rejected", rejected)

	resolvedCommit := func() store.HandlerCommit {
		c := baseCommit(t, d)
		c.Artifacts = artifacts
		c.ResolveQuestions = resolveIDs
		c.Messages = []store.Message{{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: fmt.Sprintf(markerPerimeterResolvedFmt, rid)}}
		return withBranch(c, wt)
	}

	if len(rejectedPaths) == 0 {
		return resolvedCommit(), nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), nil
	}
	changedByPath := make(map[string]orchestrator.Change, len(changed))
	for _, c := range changed {
		changedByPath[c.Path] = c
	}

	sort.Strings(rejectedPaths)
	var revertChanges []orchestrator.Change
	var revertExtras []orchestrator.Extra
	for _, p := range rejectedPaths {
		c, stillChanged := changedByPath[p]
		if !stillChanged {
			continue // design section 6.6 step 4: gone already, nothing to revert
		}
		revertChanges = append(revertChanges, c)
		revertExtras = append(revertExtras, orchestrator.Extra{Path: p, Marker: markerForFile(proposed[p].File)})
	}

	if len(revertChanges) == 0 {
		return resolvedCommit(), nil
	}

	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	sess, state, err := d.Store.LatestSession(ctx, t.ID, jobBuildName, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: latest session: %w", err)
	}
	if state == store.SessionExhausted {
		// The cap is consulted before anything is stored or reverted
		// (design section 6.6 step 5): the round stays answered and
		// unresolved, no file artifact is written, and no path is
		// reverted, whichever way the escalation check comes out. The cap
		// resolution that re-applies this preserved round is task 13's job
		// (design section 6.9).
		has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
		if hasErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: has escalation: %w", hasErr)
		}
		if has {
			slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", report.Report.TaskN, "step", "resolve_resume_capped", "session_state", sessionStateName(state))
			return store.HandlerCommit{}, ErrNoAction
		}
		return withBranch(buildCapResumesEscalation(t, d, sess.ID), wt), nil
	}
	if sess.ExternalID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve: session %d has no external id", sess.ID)
	}

	if revertErr := proj.Orch.RevertPaths(ctx, wt, revertChanges); revertErr != nil {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), revertFailedWhat, revertFailedWhy, revertErr.Error()), nil
	}
	slog.Warn("paths reverted", "ticket_id", t.ID, "run_id", rid, "count", len(revertChanges))

	notice := orchestrator.PerimeterNotice(revertExtras)
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: true}
	req := runtime.RunRequest{
		Job: response.JobBuild, Label: buildLabel(report.Report.TaskN), WorkDir: wt.Dir(),
		SessionID: *sess.ExternalID, Prompt: prompt.Assemble(prompt.ForBuildResume([]prompt.NamedInput{{Label: labelPerimeter, Text: notice}})),
	}
	sessionRecord := func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, rr) }
	commit, runErr := runAndRoute(ctx, d, t, jobBuildName, su, req, 0, sessionRecord, resolveIDs, originFor(u),
		func(rr runResult) (store.HandlerCommit, error) {
			c, successErr := buildSuccessCommit(t, d, rr, sessionRecord(rr), resolveIDs, u)
			if successErr != nil {
				return store.HandlerCommit{}, successErr
			}
			c.Artifacts = append(artifacts, c.Artifacts...)
			return c, nil
		}, seedTaskN(report.Report.TaskN), 0)
	return withBranchResult(commit, runErr, wt)
}

// resolvePerimeterQuestion is design section 6.2's third answered-round
// branch (round.Job == "perimeter"): a model question a perimeter run asked
// while describing one extra (design section 6.5 step 4's own "success
// question"). It resumes that perimeter session with the owner's answer
// when the path is still an extra in the tree, and drops the question --
// resolving the round with the "perimeter question dropped" marker --
// when the path is gone (an agent recreating a rejected extra, or an owner
// reverting one by hand, before the perimeter run answers back).
func (h buildingHandler) resolvePerimeterQuestion(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	if round.RunID == nil {
		return store.HandlerCommit{}, errors.New("job: building: resolve perimeter round: round has no run id")
	}
	perimRunID := *round.RunID
	resolveIDs := questionIDs(round)

	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: file events: %w", err)
	}
	fileRow, foundRow := fileEventForRun(events, perimRunID)
	if !foundRow {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: ticket %d: no file artifact for perimeter run %d", t.ID, perimRunID)
	}
	path := fileRow.File.Path
	taskN := fileRow.File.TaskN

	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: stored plan: %w", err)
	}
	if !havePlan {
		return unitEscalation(t, d, unit{TaskN: taskN}, string(response.EscalationCodeEnvironment), noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	proj, wt, escalation, err := ensureUnitWorktreeFor(ctx, t, d, unit{TaskN: taskN})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return unitEscalation(t, d, unit{TaskN: taskN}, string(response.EscalationCodeEnvironment), treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), nil
	}
	declaredNow := declaredPaths(plan, events, nil, taskN)
	extras := orchestrator.Perimeter(changed, declaredNow, trustRoot, styleGuide)

	extra, extraIndex, stillExtra := extraFor(extras, path)
	if !stillExtra {
		c := baseCommit(t, d)
		c.ResolveQuestions = resolveIDs
		c.Messages = []store.Message{{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: fmt.Sprintf(markerPerimeterQuestionDroppedFmt, perimRunID)}}
		slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "perimeter_question_dropped", "session_state", "n/a")
		return withBranch(c, wt), nil
	}

	if round.SessionID == nil {
		return store.HandlerCommit{}, errors.New("job: building: resolve perimeter round: round has no session id")
	}
	maxResumes := d.Machine.Jobs[jobPerimeterName].MaxResumes
	sess, state, err := d.Store.SessionByID(ctx, *round.SessionID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: session by id: %w", err)
	}
	if state == store.SessionExhausted {
		// As above (resolve): the perimeter session's own cap escalates
		// once, origin cap_resumes, naming the perimeter session; folding
		// this round back in is task 13's job (design section 6.9).
		has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
		if hasErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: has escalation: %w", hasErr)
		}
		if has {
			slog.Debug("building entry decision", "ticket_id", t.ID, "task_n", taskN, "step", "resolve_perimeter_resume_capped", "session_state", sessionStateName(state))
			return store.HandlerCommit{}, ErrNoAction
		}
		return withBranch(buildCapResumesEscalation(t, d, sess.ID), wt), nil
	}
	if sess.ExternalID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: session %d has no external id", sess.ID)
	}

	change, foundChange := changeFor(changed, path)
	if !foundChange {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: %q not found in changed paths", path)
	}
	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: build reports: %w", err)
	}
	report, foundReport := findUnlandedReportForUnit(reports, unit{TaskN: taskN}, nil)
	if !foundReport {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: ticket %d: no unlanded build_report for task %d", t.ID, taskN)
	}
	claim, foundClaim := extraClaimFor(report.Report.Extras, path)
	if !foundClaim {
		return unitEscalation(t, d, unit{TaskN: taskN}, string(response.EscalationCodeEnvironment), treeExtraUnclaimedWhat, treeExtraUnclaimedWhy, path), nil
	}

	priorInvalid, _, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobPerimeterName, nil)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve perimeter round: consecutive invalid outputs: %w", err)
	}
	answers, err := answerInputsForRound(round)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	assembled := prompt.Assemble(prompt.ForPerimeterResume(answers))

	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: true}
	req := runtime.RunRequest{Job: response.JobPerimeter, Label: perimeterLabel(taskN, extraIndex+1), WorkDir: wt.Dir(), SessionID: *sess.ExternalID, Prompt: assembled}
	sessionRecord := func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, rr) }
	commit, runErr := runAndRoute(ctx, d, t, jobPerimeterName, su, req, priorInvalid, sessionRecord, resolveIDs, response.EscalationOriginPerimeter,
		func(rr runResult) (store.HandlerCommit, error) {
			// perimeterSuccessCommit sets no ResolveQuestions of its own (its
			// only other caller, describeOne, is always a fresh run with no
			// round to resolve): this resume's own round must be resolved
			// here, on every outcome (ok, question, error) alike.
			c, successErr := perimeterSuccessCommit(t, d, rr, extra, change, claim, taskN, report.RunID, false, nil, nil)
			if successErr != nil {
				return store.HandlerCommit{}, successErr
			}
			c.ResolveQuestions = resolveIDs
			return c, nil
		}, seedTaskN(taskN), 0)
	return withBranchResult(commit, runErr, wt)
}

// ---- step 0.5: verified adoption of an unrecorded commit -------------------

// adopt is design section 6.1's single-unrecorded-commit branch, shared by
// building's own Run and the fix driver's own step 0 (design section 5.4
// change 4, #28 gap 4): it runs the seven checks in order, escalating
// unverifiable_commit at the first failure (Tried names the failing
// check), and otherwise returns the LAND commit for sha without ever
// calling CommitTask again -- carrying "fix landed <id> sha <sha>" too
// when the unit in flight is a fix unit.
func (h buildingHandler) adopt(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, reports []store.BuildReportRow, sha string) (store.HandlerCommit, error) {
	// u is read first, ahead of every check below (design section 5.4
	// change 2): unitInFlight is pure over t.State plus this call's own
	// plan/reports parameters (a store read only for a fix unit, to find
	// the open request), so nothing is lost by knowing the unit before the
	// commands run, and every escalation adopt can raise -- including the
	// two commandInfraEscalation calls right below -- can then carry u's
	// own origin (fix when u.TaskN == 0, else build) instead of a
	// hardcoded "build".
	u, hasUnit, err := unitInFlight(ctx, t, d, plan, reports)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: unit in flight: %w", err)
	}
	fail := func(check string) store.HandlerCommit {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), unverifiableCommitWhat, unverifiableCommitWhy, check)
	}
	if !hasUnit {
		return fail("no report"), nil
	}

	testExit, testTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, nil, "test", proj.TestCmd)
	if err != nil {
		return commandInfraEscalation(t, d, u, err)
	}
	lintExit, lintTimedOut, err := runCheckCommand(ctx, d, t, wt, proj, nil, "lint", proj.LintCmd)
	if err != nil {
		return commandInfraEscalation(t, d, u, err)
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

	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	var newestRun store.Run
	var ok bool
	if u.FixRequestID == nil {
		_, _, newestRun, ok, err = d.Store.UnitSession(ctx, t.ID, u.TaskN, maxResumes)
	} else {
		// A fix unit's own session is found by watermark, not task number
		// (design D22, section 5.3 step 1): u.FixRequestID names the same
		// request unitInFlight just read, so re-reading it here costs one
		// more store read but keeps adopt's own session lookup self-
		// contained, needing nothing from its caller beyond plan/reports.
		var req FixRequest
		var reqOK bool
		req, reqOK, err = openFixRequest(ctx, d, t)
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: building: adopt: open fix request: %w", err)
		}
		if !reqOK {
			return fail("no report"), nil
		}
		_, _, newestRun, ok, err = d.Store.SessionAfter(ctx, t.ID, jobBuildName, req.AfterRunID, maxResumes)
	}
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
	declaredBefore := declaredPaths(plan, events, &artifactID, u.TaskN)
	declaredNow := declaredPaths(plan, events, nil, u.TaskN)
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

	if foreign := foreignTaskPaths(plan, u.TaskN, changedPathList(commitChanges)); len(foreign) > 0 {
		msgs := make([]string, len(foreign))
		for i, e := range foreign {
			msgs[i] = e.Msg
		}
		slog.Warn("task scope violation", "ticket_id", t.ID, "run_id", newestRun.ID, "task_n", u.TaskN, "commit_sha", sha, "foreign", msgs)
		return fail("another task's path"), nil
	}

	if len(extras) > 0 {
		return fail("undeclared path"), nil
	}

	slog.Warn("unrecorded commit adopted", "ticket_id", t.ID, "task_n", u.TaskN, "commit_sha", sha)
	c, err := landCommit(t, d, plan, u.TaskN, newestRun.ID, report.Report, sha)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if u.FixRequestID != nil {
		// design D22, section 5.1, 5.3: adopting an unrecorded commit for a
		// fix unit lands it exactly as h.land does, so openFixRequest reads
		// the same "fix landed <id> sha <sha>" marker either way.
		c.Messages = append(c.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("fix landed %d sha %s", *u.FixRequestID, sha),
		})
	}
	return c, nil
}
