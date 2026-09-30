// fix.go is the fix-run entry point (design section 8): StartFix runs one
// fresh build run with a fix's own text in place of a task, and AdvanceFix
// runs the next CHECK, DESCRIBE, ASK, RESOLVE, or LAND step of the newest
// fix unit. A fix unit has task number 0, a nil task_n on its run, the
// label "fix", and the origin "fix" -- it shares every step function of
// section 6 with task units (building.go's own check, land, describeOrAsk,
// describeOne, resolve, and, since this task's own generalizing of it,
// advanceCheckedRun); StartFix and AdvanceFix only choose the unit.
//
// Nothing in serve calls either function yet: Package 9 wires the three
// producers (a review's own findings, a failed scenario, a failing CI
// check) and, with them, the answered-round dispatch a fix's own build or
// perimeter question would need once one is ever asked for real.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// FixKind is the three shapes a fix run's own input can take (design
// section 8).
type FixKind string

const (
	FixKindFindings FixKind = "findings"
	FixKindFailure  FixKind = "failure"
	FixKindCILog    FixKind = "ci_log"
)

// Values returns the three kinds, in that order.
func (FixKind) Values() []string {
	return []string{string(FixKindFindings), string(FixKindFailure), string(FixKindCILog)}
}

// FixInput is what StartFix turns into a build run's own first turn (design
// section 8): Text is the finding list, the failure report, or the CI log
// tail, always fenced when it enters the prompt.
type FixInput struct {
	Kind FixKind
	Text string
}

// fixRunLabel is runtime.RunRequest's own Label for every fix run (design
// section 6.3's own table: "the decimal task number, or fix for a fix
// unit"), first turn and every resume alike.
const fixRunLabel = "fix"

// fixSubjectFor and fixInputLabelFor are design section 8's own table: the
// commit subject a landed fix carries (unit.Title) and the prompt input
// label its own text carries, one pair per FixKind. The input label
// happens to equal the kind's own wire value in all three rows, but the
// two are kept as separate maps since the plan names them as two distinct
// columns, not one.
var fixSubjectFor = map[FixKind]string{
	FixKindFindings: "Fix review findings",
	FixKindFailure:  "Fix failed scenarios",
	FixKindCILog:    "Fix the failing check",
}

var fixInputLabelFor = map[FixKind]string{
	FixKindFindings: "findings",
	FixKindFailure:  "failure",
	FixKindCILog:    "ci_log",
}

// fixEscalation is buildEscalation with origin "fix" (design section 6.9):
// every escalation StartFix or AdvanceFix writes with no run in scope --
// a missing stored plan, or a worktree that could not be prepared, the
// only two infrastructure failures either function can hit before it
// ever reserves a run -- carries a nil RunID and SessionID, code
// "environment", and origin fix, the same shape buildEscalation gives a
// task unit's own infrastructure failures.
func fixEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeEnvironment)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginFix))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginFix)
}

// StartFix runs one fresh build run with in.Text in place of a task
// (design section 8). It validates before it touches the worktree or
// reserves a run: a kind outside the three named by FixKind.Values returns
// the error "job: unknown fix kind <kind>"; an in.Text that is empty after
// strings.TrimSpace returns the error "job: fix input is empty".
func StartFix(ctx context.Context, t store.Ticket, d Deps, in FixInput) (store.HandlerCommit, error) {
	subject, knownKind := fixSubjectFor[in.Kind]
	if !knownKind {
		return store.HandlerCommit{}, fmt.Errorf("job: unknown fix kind %s", in.Kind)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return store.HandlerCommit{}, errors.New("job: fix input is empty")
	}
	label := fixInputLabelFor[in.Kind]

	plan, _, ok, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: stored plan: %w", err)
	}
	if !ok {
		return fixEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}
	wt, created, err := proj.Orch.EnsureWorktree(ctx, t.ID, t.Title)
	if err != nil {
		return fixEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, err.Error()), nil
	}
	slog.Info("worktree ensured", "ticket_id", t.ID, "branch", wt.Branch(), "created", created)

	jobCfg := d.Machine.Jobs[jobBuildName]
	promptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}
	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}
	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: file events: %w", err)
	}
	accepted := acceptedPaths(events)
	schemas, err := renderSchemas(response.JobBuild, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}

	in2, err := prompt.ForFix(promptText, subject, label, text, proj.TestCmd, proj.LintCmd, t.Title+"\n\n"+t.Body, planXML, accepted, nil)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}
	in2.Schemas = schemas
	assembled := prompt.Assemble(in2)

	u := unit{TaskN: 0, Title: subject}
	su := store.SessionUpsert{Job: jobBuildName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobBuild, Label: fixRunLabel, WorkDir: wt.Dir(), Prompt: assembled}
	commit, runErr := runAndRoute(ctx, d, t, jobBuildName, su, req, 0, freshSessionRecord, nil, response.EscalationOriginFix,
		func(rr runResult) (store.HandlerCommit, error) {
			return buildSuccessCommit(t, d, rr, freshSessionRecord(rr), nil, u)
		}, nil) // taskN nil: RunSeed.TaskN is nil for a fix (design section 8)
	return withBranchResult(commit, runErr, wt)
}

// AdvanceFix runs the next CHECK, DESCRIBE, ASK, RESOLVE, or LAND step of
// the newest fix unit (design section 8). It returns ErrNoAction when that
// unit has landed, when no fix unit has been started at all, or when the
// unit's own session has not yet completed a first turn (StartFix, not
// this function, owns the first turn). RUN's own resume paths -- an
// owner's answer to a build question, a claim error, an invalid output, or
// an interrupted run -- are not part of AdvanceFix's own scope; a fresh
// StartFix call carries the retry instead, until Package 9 wires a fix's
// own answered-round dispatch.
func AdvanceFix(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		round := rounds[0]
		newest := round.Questions[len(round.Questions)-1]
		if newest.ParentID == nil {
			kind, kindErr := newestQuestionKind(round)
			if kindErr != nil {
				return store.HandlerCommit{}, kindErr
			}
			if kind == response.QuestionKindPerimeter {
				h := buildingHandler{}
				return h.resolve(ctx, t, d, round)
			}
		}
	}

	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: stored plan: %w", err)
	}
	if !havePlan {
		return fixEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: build reports: %w", err)
	}
	// Landed, not merely present: a fix's own unlanded build_report row
	// (from its first "ok" outcome) is never replaced in place, only
	// joined by a second, landed row once LAND runs (landCommit's own
	// shape), so nextTaskN's own "any landed row marks the unit done"
	// rule -- not findUnlandedReportForTask's own newest-unlanded search
	// -- is the right test here (design section 6.8's own transition-out
	// reasoning, applied to a fix unit instead of a task).
	landed := false
	for i := range reports {
		if reports[i].Report.TaskN == 0 && reports[i].Report.CommitSHA != nil {
			landed = true
			break
		}
	}
	if landed {
		return store.HandlerCommit{}, ErrNoAction
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}
	wt, created, err := proj.Orch.EnsureWorktree(ctx, t.ID, t.Title)
	if err != nil {
		return fixEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, err.Error()), nil
	}
	slog.Info("worktree ensured", "ticket_id", t.ID, "branch", wt.Branch(), "created", created)

	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	sess, state, newestRun, ok, err := d.Store.UnitSession(ctx, t.ID, 0, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: unit session: %w", err)
	}
	if !ok || state == store.SessionIdless {
		return store.HandlerCommit{}, ErrNoAction
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}
	if outcome != string(response.OutcomeOk) {
		return store.HandlerCommit{}, ErrNoAction
	}

	h := buildingHandler{}
	commit, runErr := h.advanceCheckedRun(ctx, t, d, proj, wt, plan, 0, sess, state, newestRun.ID, reports)
	return withBranchResult(commit, runErr, wt)
}
