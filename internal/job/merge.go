// merge.go is the base-merge unit (overview design): POLL's own dirty row
// (pollConflict, wired into shipping.go's poll right after the head-mismatch
// row) and, from task 5 onward, the merge unit itself. jobMergeName is
// machine.toml's "merge" job key.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

const (
	jobMergeName        = "merge"
	mergeRunLabel       = "merge"
	mergeableStateDirty = "dirty"

	conflictWhatFmt      = "PR #%d conflicts with %s"
	conflictOtherBaseWhy = "Zing merges only the project's default branch, %s, into a ticket branch"
	conflictLoopsWhyFmt  = "Zing already merged %s into this branch %d times and the pull request conflicts again; jobs.merge.max_loops is %d"
	baseNotFetchedWhy    = "Zing reads the base branch's sha before it starts a merge"

	mergeFailedWhat = "the base branch could not be merged"
	mergeFailedWhy  = "git merge refused or failed in the ticket's worktree"

	mergeDecisionWhat = "the merge needs a decision"
	mergeDecisionWhy  = "the merge agent found a conflict it cannot resolve without the owner"

	// mergeNoConflictReport is landMerge's own synthesized report body for a
	// clean merge that never ran the agent (rid nil, task 6's own "no-run"
	// case): CHECK alone decided there was nothing for it to do.
	mergeNoConflictReport = "The base branch merged with no conflicts and CHECK passed."
)

// pollConflict is POLL's own dirty row: GitHub builds no merge ref for a
// conflicting pull request, so CI never starts and waiting on it would wait
// forever. A base other than the project's own default branch, or a ticket
// that already opened jobs.merge.max_loops base merge requests, escalates
// instead of opening another one. Otherwise it fetches the base branch's
// current sha and writes the conflict notice and the request marker
// together, in one commit, with ClearPoll set.
func (h shipHandler) pollConflict(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, pr orchestrator.PRState, number int) (store.HandlerCommit, error) {
	what := fmt.Sprintf(conflictWhatFmt, number, pr.BaseRef)
	defaultBranch := proj.Orch.DefaultBranch()
	if pr.BaseRef != defaultBranch {
		c := shipEscalation(t, d, what, fmt.Sprintf(conflictOtherBaseWhy, defaultBranch), "")
		c.ClearPoll = true
		return c, nil
	}

	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, baseMergePrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: base merge markers: %w", err)
	}
	k := pollMergeCount(rows)
	maxLoops := d.Machine.Jobs[jobMergeName].MaxLoops
	if k >= maxLoops {
		c := shipEscalation(t, d, what, fmt.Sprintf(conflictLoopsWhyFmt, pr.BaseRef, k, maxLoops), "")
		c.ClearPoll = true
		return c, nil
	}

	sha, err := proj.Orch.FetchBase(ctx, wt)
	if err != nil {
		c := shipEscalation(t, d, what, baseNotFetchedWhy, err.Error())
		c.ClearPoll = true
		return c, nil
	}
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: max run id: %w", err)
	}

	req := baseMergeRequest{AfterRunID: maxRunID, BaseBranch: pr.BaseRef, BaseSHA: sha}
	c := baseCommit(t, d)
	c.Messages = []store.Message{
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: what},
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: req.body()},
	}
	c.ClearPoll = true
	slog.Info("base merge requested", "ticket_id", t.ID, "pr", number, "base", pr.BaseRef, "base_sha", sha, "after_run", maxRunID)
	return c, nil
}

// openBaseMerge reads the ticket's one open base merge request, when any,
// for driveOpenMerge (task 5) and retryMerge (task 7).
func openBaseMerge(ctx context.Context, t store.Ticket, d Deps) (baseMergeRequest, bool, error) {
	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, baseMergePrefix)
	if err != nil {
		return baseMergeRequest{}, false, fmt.Errorf("job: shipping: base merge markers: %w", err)
	}
	return openBaseMergeRequest(rows)
}

// mergeTried is "base merge <id>", then "\n" and detail when detail is
// non-empty: every merge-unit escalation's own Tried text, so a later
// owner retry can find its way back to the request through baseMergeTriedID.
func mergeTried(req baseMergeRequest, detail string) string {
	tried := fmt.Sprintf("base merge %d", req.MessageID)
	if detail != "" {
		tried += "\n" + detail
	}
	return tried
}

// mergeEscalation is shipEscalation with Tried = mergeTried(req, detail)
// and ClearPoll set, for every merge-unit escalation from task 5 onward.
func mergeEscalation(t store.Ticket, d Deps, req baseMergeRequest, what, why, detail string) store.HandlerCommit {
	c := shipEscalation(t, d, what, why, mergeTried(req, detail))
	c.ClearPoll = true
	return c
}

// driveOpenMerge runs one merge-unit step when a request is open; merging
// is false, with nothing done, when none is (shipHandler.Run calls this
// right after the post-build prelude, before anything else shipping does).
func (h shipHandler) driveOpenMerge(ctx context.Context, t store.Ticket, d Deps) (c store.HandlerCommit, merging bool, err error) {
	req, open, err := openBaseMerge(ctx, t, d)
	if err != nil || !open {
		return store.HandlerCommit{}, false, err
	}
	c, err = h.driveMerge(ctx, t, d, req)
	return c, true, err
}

// driveMerge is one tick of the merge unit (overview design "One merge
// tick"): step 0 reconciles the worktree and the branch against the
// recorded build_reports, exactly as building's own Run and the fix driver
// do (unrecordedCommits); step 1 starts (or resumes) the git merge itself
// (StartBaseMerge); step 2 drives the merge session -- the first agent
// turn when there are conflicts to resolve, CHECK alone when there are
// not, or whatever the session's own newest run needs next.
//
// Until task 8 adds adoptMerge, a single unrecorded commit at the tip
// escalates exactly as two or more do (foreignCommitsWhat/Why); until task
// 7 adds mergeAfterError, a session whose newest run ended in error
// escalates mergeFailedWhat/Why instead of resuming it.
func (h shipHandler) driveMerge(ctx context.Context, t store.Ticket, d Deps, req baseMergeRequest) (store.HandlerCommit, error) {
	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return mergeEscalation(t, d, req, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}
	slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "worktree_ensured")

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: build reports: %w", err)
	}
	unrecorded, prefixOK, err := unrecordedCommits(ctx, proj, wt, reports)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if !prefixOK {
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "branch_missing_recorded")
		return withBranch(mergeEscalation(t, d, req, branchMissingRecordedWhat, branchMissingRecordedWhy, ""), wt), nil
	}
	if len(unrecorded) > 0 {
		// Task 8 replaces this with adoptMerge for the len==1 case: a
		// previous tick that committed the merge and crashed before
		// recording it looks identical to a foreign commit until then.
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "foreign_commits")
		return withBranch(mergeEscalation(t, d, req, foreignCommitsWhat, foreignCommitsWhy, ""), wt), nil
	}

	conflicted, startErr := proj.Orch.StartBaseMerge(ctx, wt, req.BaseSHA)
	if startErr != nil {
		if errors.Is(startErr, orchestrator.ErrAlreadyMerged) {
			slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "already_merged")
			return withBranch(baseMergeClosedCommit(t, d, req), wt), nil
		}
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "start_merge_failed")
		return withBranch(mergeEscalation(t, d, req, mergeFailedWhat, mergeFailedWhy, startErr.Error()), wt), nil
	}

	maxResumes := d.Machine.Jobs[jobMergeName].MaxResumes
	sess, state, newestRun, found, sessErr := d.Store.SessionAfter(ctx, t.ID, jobMergeName, req.AfterRunID, maxResumes)
	if sessErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: session after: %w", sessErr)
	}

	if !found || state == store.SessionIdless {
		if len(conflicted) > 0 {
			slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "run_first")
			commit, runErr := h.runMergeFirst(ctx, t, d, proj, wt, req, conflicted, nil)
			return withBranchResult(commit, runErr, wt)
		}
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "check_no_run")
		commit, runErr := h.mergeCheck(ctx, t, d, proj, wt, req, nil, nil)
		return withBranchResult(commit, runErr, wt)
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}
	switch outcome {
	case string(response.OutcomeOk):
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "check")
		rid := newestRun.ID
		commit, runErr := h.mergeCheck(ctx, t, d, proj, wt, req, &rid, &sess)
		return withBranchResult(commit, runErr, wt)
	case string(response.OutcomeError):
		// Task 7 replaces this with mergeAfterError's own interrupted/invalid/
		// already-escalated routing.
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "run_error")
		return withBranch(mergeEscalation(t, d, req, mergeFailedWhat, mergeFailedWhy, ""), wt), nil
	default:
		slog.Debug("base merge step", "ticket_id", t.ID, "request_id", req.MessageID, "step", "unrecognized")
		return store.HandlerCommit{}, ErrNoAction
	}
}

// baseMergeClosedCommit writes "base merge closed <id>" with ClearPoll:
// StartBaseMerge found sha already an ancestor of HEAD, so there is
// nothing left for this request to do.
func baseMergeClosedCommit(t store.Ticket, d Deps, req baseMergeRequest) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("base merge closed %d", req.MessageID),
	}}
	c.ClearPoll = true
	return c
}

// mergeCommandInfraEscalation is commandInfraEscalation (building.go) for
// the merge unit: ErrSandbox escalates sandbox_unavailable; a wrapped
// context.Canceled returns runtime.ErrCanceled with no commit; anything
// else escalates environment/"the project commands could not run".
func mergeCommandInfraEscalation(t store.Ticket, d Deps, req baseMergeRequest, err error) (store.HandlerCommit, error) {
	switch {
	case errors.Is(err, ErrSandbox):
		return mergeEscalation(t, d, req, sandboxUnavailableWhat, d.Sandboxes.Build.Reason(), ""), nil
	case errors.Is(err, context.Canceled):
		return store.HandlerCommit{}, runtime.ErrCanceled
	default:
		return mergeEscalation(t, d, req, projectCommandsNotRunWhat, projectCommandsNotRunWhy, err.Error()), nil
	}
}

// mergeCheck runs the project's commands, then reads the conflict markers
// left in the tree (overview design "One merge tick"): a clean result
// lands the merge (landMerge); a failing one resumes the merge session
// with the check input, charged (runMergeResume). Task 6 adds the no-run
// branch (rid nil), the outside-the-merge path read, and the check_loops
// and max_resumes gates; until then, rid is always a real run's id and
// sess is always that run's own session.
func (h shipHandler) mergeCheck(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, req baseMergeRequest, rid *int64, sess *store.Session) (store.HandlerCommit, error) {
	results, err := runCheckCommands(ctx, d, t, wt, proj, rid)
	if err != nil {
		return mergeCommandInfraEscalation(t, d, req, err)
	}

	markers, err := proj.Orch.ConflictMarkerPaths(ctx, wt)
	if err != nil {
		return mergeEscalation(t, d, req, treeNotDiffedWhat, treeNotDiffedWhy, err.Error()), nil
	}

	text := mergeCheckText(results, markers, nil)
	slog.Info("merge check", "ticket_id", t.ID, "request_id", req.MessageID, "check_failed", text != "", "markers", len(markers))
	if text == "" {
		return h.landMerge(ctx, t, d, proj, wt, req, rid)
	}

	return h.runMergeResume(ctx, t, d, wt, req, *sess, 0, []prompt.NamedInput{prompt.Check(text)}, true)
}

// landMerge commits and records the merge (overview design "One merge
// tick"): report is the build_report of run rid (findUnitReport), or, when
// rid is nil, the synthesized clean-merge report (mergeNoConflictReport).
// report.Title is always mergeTitle(req), overriding whatever the agent's
// own report carried. A CommitMerge error naming "commit signing failed"
// escalates commitSigningFailedWhat/Why, any other escalates
// taskNotCommittedWhat/Why, both with the error as detail.
func (h shipHandler) landMerge(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, req baseMergeRequest, rid *int64) (store.HandlerCommit, error) {
	report, err := mergeReportFor(ctx, t, d, rid)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	report.Title = mergeTitle(req)

	msg := orchestrator.CommitMessage{Title: report.Title, FuncLines: mergeFuncLines(req, report.FilesChanged), Fences: report.Fences}
	sha, commitErr := proj.Orch.CommitMerge(ctx, wt, msg)
	if commitErr != nil {
		what, why := taskNotCommittedWhat, taskNotCommittedWhy
		if strings.Contains(commitErr.Error(), "commit signing failed") {
			what, why = commitSigningFailedWhat, commitSigningFailedWhy
		}
		return mergeEscalation(t, d, req, what, why, commitErr.Error()), nil
	}

	slog.Info("base merge landed", "ticket_id", t.ID, "request_id", req.MessageID, "sha", sha)
	return mergeLandedCommit(t, d, req, rid, report, sha)
}

// mergeReportFor is landMerge's own report lookup: the unlanded
// build_report of run rid (findUnitReport), or, when rid is nil, the
// synthesized report a clean merge with no agent run ever produces.
func mergeReportFor(ctx context.Context, t store.Ticket, d Deps, rid *int64) (response.BuildReport, error) {
	if rid == nil {
		return response.BuildReport{
			TaskN:        0,
			FilesChanged: []string{},
			Extras:       []response.ExtraClaim{},
			Fences:       []response.Fence{},
			Report:       mergeNoConflictReport,
		}, nil
	}
	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return response.BuildReport{}, fmt.Errorf("job: merge: build reports: %w", err)
	}
	row, found := findUnitReport(reports, *rid)
	if !found {
		return response.BuildReport{}, fmt.Errorf("job: merge: ticket %d: no build_report for run %d", t.ID, *rid)
	}
	return row.Report, nil
}

// mergeLandedCommit is landMerge's own terminal commit: one build_report
// artifact (RunID rid, the report with CommitSHA set to sha) plus "base
// merge landed <id> sha <sha>", with ClearPoll. The ticket stays in
// shipping: the next POLL finds the pull request's head an ancestor of
// local HEAD and pushes.
func mergeLandedCommit(t store.Ticket, d Deps, req baseMergeRequest, rid *int64, report response.BuildReport, sha string) (store.HandlerCommit, error) {
	landed := report
	landed.CommitSHA = &sha
	payload, err := json.Marshal(landed)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: marshal landed build report: %w", err)
	}

	c := baseCommit(t, d)
	c.Artifacts = []store.Artifact{{Type: artifactTypeBuildReport, RunID: rid, Payload: payload}}
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("base merge landed %d sha %s", req.MessageID, sha),
	}}
	c.ClearPoll = true
	return c, nil
}

// runMergeFirst is RUN's first turn for the merge unit: the merge job
// prompt (prompts/merge.md), the ticket and plan, conflicted (fenced, one
// path per line) and the base branch's own commit log (BaseLog), routed
// through mergeSuccessCommit. extra carries a retry's own notes input
// (task 7); a fresh request's own first turn passes nil.
func (h shipHandler) runMergeFirst(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, req baseMergeRequest, conflicted []string, extra []prompt.NamedInput) (store.HandlerCommit, error) {
	cfg := d.Machine.Jobs[jobMergeName]
	promptText, err := readAsset(cfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: %w", err)
	}
	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: stored plan: %w", err)
	}
	if !havePlan {
		return mergeEscalation(t, d, req, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}
	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: %w", err)
	}
	baseLog, err := proj.Orch.BaseLog(ctx, wt, req.BaseSHA)
	if err != nil {
		return mergeEscalation(t, d, req, mergeFailedWhat, mergeFailedWhy, err.Error()), nil
	}
	schemas, err := renderSchemas(response.JobBuild, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: %w", err)
	}
	if req.Notes != "" {
		extra = append(extra, prompt.Notes(req.Notes))
	}
	in, err := prompt.ForMerge(promptText, proj.TestCmd, proj.LintCmd, t.Title+"\n\n"+t.Body, planXML, strings.Join(conflicted, "\n"), baseLog, extra)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: %w", err)
	}
	in.Schemas = schemas
	su := store.SessionUpsert{Job: jobMergeName, Runtime: cfg.Runtime}
	runReq := runtime.RunRequest{Job: response.JobBuild, Label: mergeRunLabel, WorkDir: wt.Dir(), Prompt: prompt.Assemble(in)}
	return runAndRoute(ctx, d, t, jobMergeName, su, runReq, 0, freshSessionRecord, nil, response.EscalationOriginShipping,
		func(rr runResult) (store.HandlerCommit, error) {
			return mergeSuccessCommit(t, d, rr, freshSessionRecord(rr), req)
		}, nil, 0)
}

// runMergeResume is runBuildResume (building.go) for the merge session:
// sess's own external id, mergeRunLabel, routed through mergeSuccessCommit.
// A nil ExternalID is the error "job: merge: resume: session <id> has no
// external id".
func (h shipHandler) runMergeResume(ctx context.Context, t store.Ticket, d Deps, wt orchestrator.Worktree, req baseMergeRequest, sess store.Session, priorInvalid int, inputs []prompt.NamedInput, bump bool) (store.HandlerCommit, error) {
	if sess.ExternalID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: resume: session %d has no external id", sess.ID)
	}
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: bump}
	runReq := runtime.RunRequest{
		Job: response.JobBuild, Label: mergeRunLabel, WorkDir: wt.Dir(),
		SessionID: *sess.ExternalID, Prompt: prompt.Assemble(prompt.ForBuildResume(inputs)),
	}
	sessionRecord := func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, rr) }
	return runAndRoute(ctx, d, t, jobMergeName, su, runReq, priorInvalid, sessionRecord, nil, response.EscalationOriginShipping,
		func(rr runResult) (store.HandlerCommit, error) {
			return mergeSuccessCommit(t, d, rr, sessionRecord(rr), req)
		}, nil, 0)
}

// mergeSuccessCommit routes the merge agent's answer (overview design
// "One merge tick"): an ok build response reuses buildSuccessCommit
// (building.go) under a task-0 unit titled mergeTitle(req), inserting one
// build_report artifact exactly as a task or fix unit's own first turn
// does; a question or an error escalates directly -- never through
// buildSuccessCommit, whose own question/error routing would tag origin
// fix for a task-0 unit -- with Tried carrying mergeTried(req, ...) so a
// later owner retry (task 7) can find its way back to this request.
func mergeSuccessCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, req baseMergeRequest) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.BuildResponse:
		return buildSuccessCommit(t, d, rr, sessionCommit, nil, unit{TaskN: 0, Title: mergeTitle(req)})
	case *response.QuestionResponse:
		c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
			string(response.EscalationCodeEnvironment), mergeDecisionWhat, mergeDecisionWhy,
			mergeTried(req, mergeQuestionText(resp.Questions)), response.EscalationOriginShipping)
		c.Runs = terminalRuns(rr, string(response.OutcomeQuestion))
		c.Session = sessionCommit
		c.ClearPoll = true
		return c, nil
	case *response.ErrorResponse:
		c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
			string(resp.Error.Code), resp.Error.What, resp.Error.Why,
			mergeTried(req, resp.Error.Tried), response.EscalationOriginShipping)
		c.Runs = terminalRuns(rr, string(response.OutcomeError))
		c.Session = sessionCommit
		c.ClearPoll = true
		return c, nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: merge: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}
