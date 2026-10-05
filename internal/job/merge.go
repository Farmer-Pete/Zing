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

	mergeCheckWhat     = "the merge still fails CHECK after the agent's fix attempts"
	mergeCheckWhyFmt   = "check_loops for merge is %d; the last failing output is under Tried"
	mergeResumesWhat   = "the merge session ran out of resumes"
	mergeResumesWhyFmt = "max_resumes for merge is %d"
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

// mergeEscalation is mergeEscalationIDs with no session or run known yet:
// every merge-unit escalation written before the merge session exists
// (step 0 and step 1 of driveMerge, and a fresh run's own setup failures).
func mergeEscalation(t store.Ticket, d Deps, req baseMergeRequest, what, why, detail string) store.HandlerCommit {
	return mergeEscalationIDs(t, d, req, what, why, detail, nil, nil)
}

// mergeEscalationIDs is mergeEscalationCode under code "environment", the
// code every merge-unit escalation but the agent's own ErrorResponse uses.
func mergeEscalationIDs(t store.Ticket, d Deps, req baseMergeRequest, what, why, detail string, sessionID, runID *int64) store.HandlerCommit {
	return mergeEscalationCode(t, d, req, string(response.EscalationCodeEnvironment), what, why, detail, sessionID, runID)
}

// mergeEscalationCode is every merge-unit escalation's own "escalation
// written" log (design section 11) plus its commit, under a caller-chosen
// code, Tried = mergeTried(req, detail) and ClearPoll set: the merge
// unit's equivalent of shipEscalationCode, except that a merge escalation
// almost always does know which session and run it is about, unlike
// PUBLISH and POLL's own escalations, which never make a runtime call of
// their own. Logging through here instead of shipEscalation (which always
// logs nil ids) is what lets a merge escalation's log line be tied back to
// the session and run that caused it. mergeSuccessCommit's own
// QuestionResponse and ErrorResponse branches call this directly, since
// only they need a code other than "environment".
func mergeEscalationCode(t store.Ticket, d Deps, req baseMergeRequest, code, what, why, detail string, sessionID, runID *int64) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", runID,
		"code", code, "origin", string(response.EscalationOriginShipping), "request_id", req.MessageID)
	c := escalationCommit(t, d, runID, sessionID, code, what, why, mergeTried(req, detail), response.EscalationOriginShipping)
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
// Step 0's single-unrecorded-commit case is adoptMerge: a previous tick
// that committed the merge and crashed before recording it looks
// identical to a foreign commit until adoptMerge verifies it. Two or more
// unrecorded commits still escalate (foreignCommitsWhat/Why). A session
// whose newest run ended in error routes through mergeAfterError.
func (h shipHandler) driveMerge(ctx context.Context, t store.Ticket, d Deps, req baseMergeRequest) (store.HandlerCommit, error) {
	log := slog.With("ticket_id", t.ID, "request_id", req.MessageID)

	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return mergeEscalation(t, d, req, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		log.Debug("base merge step", "step", "worktree_error")
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		log.Debug("base merge step", "step", "worktree_not_prepared")
		return *escalation, nil
	}
	log.Debug("base merge step", "step", "worktree_ensured")

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		log.Debug("base merge step", "step", "build_reports_error")
		return store.HandlerCommit{}, fmt.Errorf("job: merge: build reports: %w", err)
	}
	unrecorded, prefixOK, err := unrecordedCommits(ctx, proj, wt, reports)
	if err != nil {
		log.Debug("base merge step", "step", "unrecorded_commits_error")
		return store.HandlerCommit{}, fmt.Errorf("job: merge: unrecorded commits: %w", err)
	}
	if !prefixOK {
		log.Debug("base merge step", "step", "branch_missing_recorded")
		return withBranch(mergeEscalation(t, d, req, branchMissingRecordedWhat, branchMissingRecordedWhy, ""), wt), nil
	}
	switch len(unrecorded) {
	case 0:
		// continue to step 1
	case 1:
		log.Debug("base merge step", "step", "adopt")
		commit, adoptErr := h.adoptMerge(ctx, t, d, proj, wt, req, reports, unrecorded[0])
		if adoptErr != nil {
			return store.HandlerCommit{}, adoptErr
		}
		return withBranch(commit, wt), nil
	default:
		log.Debug("base merge step", "step", "foreign_commits")
		return withBranch(mergeEscalation(t, d, req, foreignCommitsWhat, foreignCommitsWhy, ""), wt), nil
	}

	conflicted, startErr := proj.Orch.StartBaseMerge(ctx, wt, req.BaseSHA)
	if startErr != nil {
		if errors.Is(startErr, orchestrator.ErrAlreadyMerged) {
			log.Debug("base merge step", "step", "already_merged")
			return withBranch(baseMergeClosedCommit(t, d, req), wt), nil
		}
		log.Debug("base merge step", "step", "start_merge_failed")
		return withBranch(mergeEscalation(t, d, req, mergeFailedWhat, mergeFailedWhy, startErr.Error()), wt), nil
	}

	maxResumes := d.Machine.Jobs[jobMergeName].MaxResumes
	sess, state, newestRun, found, sessErr := d.Store.SessionAfter(ctx, t.ID, jobMergeName, req.AfterRunID, maxResumes)
	if sessErr != nil {
		log.Debug("base merge step", "step", "session_after_error")
		return store.HandlerCommit{}, fmt.Errorf("job: merge: session after: %w", sessErr)
	}

	if !found || state == store.SessionIdless {
		if len(conflicted) > 0 {
			log.Debug("base merge step", "step", "run_first")
			commit, runErr := h.runMergeFirst(ctx, t, d, proj, wt, req, conflicted, nil)
			return withBranchResult(commit, runErr, wt)
		}
		log.Debug("base merge step", "step", "check_no_run")
		commit, runErr := h.mergeCheck(ctx, t, d, proj, wt, req, nil, nil)
		return withBranchResult(commit, runErr, wt)
	}

	switch runOutcome(newestRun) {
	case string(response.OutcomeOk):
		rid := newestRun.ID
		log.Debug("base merge step", "run_id", rid, "step", "check")
		commit, runErr := h.mergeCheck(ctx, t, d, proj, wt, req, &rid, &sess)
		return withBranchResult(commit, runErr, wt)
	case string(response.OutcomeError):
		log.Debug("base merge step", "run_id", newestRun.ID, "step", "run_error")
		commit, runErr := h.mergeAfterError(ctx, t, d, wt, req, sess, newestRun)
		return withBranchResult(commit, runErr, wt)
	default:
		log.Debug("base merge step", "run_id", newestRun.ID, "outcome", runOutcome(newestRun), "step", "unrecognized")
		return store.HandlerCommit{}, ErrNoAction
	}
}

// runOutcome returns r's own outcome string, or "" when r has none yet
// (its reserved run has not answered): driveMerge's own newest-run switch
// and adoptMerge's own "is the newest run ok" test share this instead of
// each unpacking the nullable field inline.
func runOutcome(r store.Run) string {
	if r.Outcome == nil {
		return ""
	}
	return *r.Outcome
}

// baseMergeClosedCommit writes "base merge closed <id>" with ClearPoll:
// StartBaseMerge found sha already an ancestor of HEAD, so there is
// nothing left for this request to do.
func baseMergeClosedCommit(t store.Ticket, d Deps, req baseMergeRequest) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: baseMergeClosedBody(req.MessageID),
	}}
	c.ClearPoll = true
	return c
}

// mergeCommandInfraEscalation is commandInfraEscalation (building.go) for
// the merge unit: ErrSandbox escalates sandbox_unavailable; a wrapped
// context.Canceled returns runtime.ErrCanceled with no commit; anything
// else escalates environment/"the project commands could not run".
// sessionID and runID are logged and stored on the escalation so it can be
// tied back to the run whose CHECK commands failed to run at all; a
// caller with no run yet (adoptMerge, before it knows which session the
// commit belongs to) passes nil, nil.
func mergeCommandInfraEscalation(t store.Ticket, d Deps, req baseMergeRequest, err error, sessionID, runID *int64) (store.HandlerCommit, error) {
	switch {
	case errors.Is(err, ErrSandbox):
		return mergeEscalationIDs(t, d, req, sandboxUnavailableWhat, d.Sandboxes.Build.Reason(), "", sessionID, runID), nil
	case errors.Is(err, context.Canceled):
		return store.HandlerCommit{}, runtime.ErrCanceled
	default:
		return mergeEscalationIDs(t, d, req, projectCommandsNotRunWhat, projectCommandsNotRunWhy, err.Error(), sessionID, runID), nil
	}
}

// mergeCheck runs the project's commands, then reads the conflict markers
// and the outside-the-merge paths left in the tree (overview design "One
// merge tick"): a clean result lands the merge (landMerge). A failing
// result with rid nil (a clean merge that never ran the agent) runs the
// agent for the first time, with the check input in place of any
// conflicts (runMergeFirst); otherwise it resumes the merge session with
// the check input, charged (runMergeResume), gated by jobs.merge.check_loops
// then jobs.merge.max_resumes.
func (h shipHandler) mergeCheck(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, req baseMergeRequest, rid *int64, sess *store.Session) (store.HandlerCommit, error) {
	var sessionID *int64
	if sess != nil {
		sessionID = &sess.ID
	}

	results, err := runCheckCommands(ctx, d, t, wt, proj, rid)
	if err != nil {
		return mergeCommandInfraEscalation(t, d, req, err, sessionID, rid)
	}

	markers, err := proj.Orch.ConflictMarkerPaths(ctx, wt)
	if err != nil {
		return mergeEscalationIDs(t, d, req, treeNotDiffedWhat, treeNotDiffedWhy, err.Error(), sessionID, rid), nil
	}
	outside, err := mergeOutsidePaths(ctx, proj, wt)
	if err != nil {
		return mergeEscalationIDs(t, d, req, treeNotDiffedWhat, treeNotDiffedWhy, err.Error(), sessionID, rid), nil
	}

	text := mergeCheckText(results, markers, outside)
	slog.Info("merge check", "ticket_id", t.ID, "request_id", req.MessageID, "run_id", int64OrZero(rid), "check_failed", text != "", "markers", len(markers), "outside", len(outside))
	if text == "" {
		return h.landMerge(ctx, t, d, proj, wt, req, rid)
	}

	if rid == nil {
		return h.runMergeFirst(ctx, t, d, proj, wt, req, nil, []prompt.NamedInput{prompt.Check(text)})
	}

	checkLoops := d.Machine.Jobs[jobMergeName].CheckLoops
	if sess.Resumes >= checkLoops {
		return mergeEscalationIDs(t, d, req, mergeCheckWhat, fmt.Sprintf(mergeCheckWhyFmt, checkLoops), text, &sess.ID, rid), nil
	}
	maxResumes := d.Machine.Jobs[jobMergeName].MaxResumes
	if sess.Resumes >= maxResumes {
		return mergeEscalationIDs(t, d, req, mergeResumesWhat, fmt.Sprintf(mergeResumesWhyFmt, maxResumes), text, &sess.ID, rid), nil
	}

	return h.runMergeResume(ctx, t, d, wt, req, *sess, 0, []prompt.NamedInput{prompt.Check(text)}, true)
}

// mergeOutsidePaths is mergeCheck's own outside-the-merge read: every path
// MergeChangedPaths reports that is not among MergeSidePaths, the set
// either side of the merge may legitimately touch. MergeChangedPaths
// (unlike orchestrator.ChangedPaths) tolerates a path the index still
// carries unmerged, exactly what a real, unresolved conflict's own hunk
// leaves behind until CommitMerge's own "git add -u".
func mergeOutsidePaths(ctx context.Context, proj Project, wt orchestrator.Worktree) ([]string, error) {
	allowed, err := proj.Orch.MergeSidePaths(ctx, wt)
	if err != nil {
		return nil, err
	}
	changed, err := proj.Orch.MergeChangedPaths(ctx, wt)
	if err != nil {
		return nil, err
	}
	return pathsOutside(changed, allowed), nil
}

// pathsOutside returns, in changed's own order, every path not in allowed:
// mergeOutsidePaths' and adoptMerge's own shared "is this outside the
// merge" set difference.
func pathsOutside(changed, allowed []string) []string {
	allowedSet := make(map[string]bool, len(allowed))
	for _, p := range allowed {
		allowedSet[p] = true
	}
	var outside []string
	for _, p := range changed {
		if !allowedSet[p] {
			outside = append(outside, p)
		}
	}
	return outside
}

// landMerge commits and records the merge (overview design "One merge
// tick"): report is the build_report of run rid (findUnitReport), or, when
// rid is nil, the synthesized clean-merge report (mergeNoConflictReport).
// The commit message carries only Title and FuncLines -- never the agent's
// own Fences, since a merge commit's message is never the agent's to write
// (mergeReportFor's own doc comment). A CommitMerge error naming "commit
// signing failed" escalates commitSigningFailedWhat/Why, any other
// escalates taskNotCommittedWhat/Why, both with the error as detail.
func (h shipHandler) landMerge(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, req baseMergeRequest, rid *int64) (store.HandlerCommit, error) {
	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: build reports: %w", err)
	}
	if rid == nil {
		if carried, ok, carryErr := priorMergeRunID(ctx, t, d, req); carryErr != nil {
			return store.HandlerCommit{}, carryErr
		} else if ok {
			rid = &carried
		}
	}
	report, err := mergeReportFor(t, rid, req, reports)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	msg := orchestrator.CommitMessage{Title: report.Title, FuncLines: mergeFuncLines(req, report.FilesChanged)}
	sha, commitErr := proj.Orch.CommitMerge(ctx, wt, msg, req.BaseSHA)
	if commitErr != nil {
		what, why := taskNotCommittedWhat, taskNotCommittedWhy
		if strings.Contains(commitErr.Error(), "commit signing failed") {
			what, why = commitSigningFailedWhat, commitSigningFailedWhy
		}
		return mergeEscalationIDs(t, d, req, what, why, commitErr.Error(), nil, rid), nil
	}

	slog.Info("base merge landed", "ticket_id", t.ID, "request_id", req.MessageID, "run_id", int64OrZero(rid), "commit_sha", sha)
	return mergeLandedCommit(t, d, req, rid, report, sha)
}

// mergeReportFor is landMerge's and adoptMerge's own report lookup, over
// reports the caller already read: the unlanded build_report of run rid
// (findUnitReport), or, when rid is nil, the synthesized report a clean
// merge with no agent run ever produces. Title is always mergeTitle(req),
// overriding whatever the agent's own report carried -- a merge commit's
// own message is never the agent's to write.
func mergeReportFor(t store.Ticket, rid *int64, req baseMergeRequest, reports []store.BuildReportRow) (response.BuildReport, error) {
	var report response.BuildReport
	if rid == nil {
		report = response.BuildReport{
			TaskN:        0,
			FilesChanged: []string{},
			Extras:       []response.ExtraClaim{},
			Fences:       []response.Fence{},
			Report:       mergeNoConflictReport,
		}
	} else {
		row, found := findUnitReport(reports, *rid)
		if !found {
			return response.BuildReport{}, fmt.Errorf("job: merge: ticket %d: no build_report for run %d", t.ID, *rid)
		}
		report = row.Report
	}
	report.Title = mergeTitle(req)
	return report, nil
}

// priorMergeRunID finds the newest ok merge run from req's own closed
// predecessor (RetryOf), for a reopened request whose own merge needs no
// agent turn at all: a signing failure (CommitMerge) can close and reopen
// a request after an agent already resolved every conflict, leaving the
// worktree still mid-merge with nothing left unmerged; the reopened
// request's own StartBaseMerge then reports no conflicts and its own
// SessionAfter finds no session past its own fresh watermark, so without
// this lookup landMerge would synthesize an empty "no conflicts" report
// and discard the predecessor's real one (files changed, fences, and all).
// ok is false when req has no predecessor (RetryOf == 0), the predecessor
// row cannot be found or parsed, or the predecessor's own session never
// produced an ok run.
func priorMergeRunID(ctx context.Context, t store.Ticket, d Deps, req baseMergeRequest) (runID int64, ok bool, err error) {
	if req.RetryOf == 0 {
		return 0, false, nil
	}
	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, baseMergePrefix)
	if err != nil {
		return 0, false, fmt.Errorf("job: merge: prior run: base merge markers: %w", err)
	}
	prior, found, perr := baseMergeRequestByID(rows, req.RetryOf)
	if perr != nil {
		// A malformed predecessor marker just means "no report to carry
		// over", not a hard failure; landMerge falls back to its own
		// synthesized report. Logged, since it silently discards whatever
		// real report the predecessor's own agent run produced.
		slog.Warn("base merge predecessor marker malformed", "ticket_id", t.ID, "request_id", req.MessageID, "retry_of", req.RetryOf, "err", perr)
		return 0, false, nil
	}
	if !found {
		return 0, false, nil
	}

	maxResumes := d.Machine.Jobs[jobMergeName].MaxResumes
	_, _, newestRun, sessFound, err := d.Store.SessionAfter(ctx, t.ID, jobMergeName, prior.AfterRunID, maxResumes)
	if err != nil {
		return 0, false, fmt.Errorf("job: merge: prior run: session after: %w", err)
	}
	if !sessFound || runOutcome(newestRun) != string(response.OutcomeOk) {
		return 0, false, nil
	}
	return newestRun.ID, true, nil
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
		Body: baseMergeLandedBody(req.MessageID, sha),
	}}
	c.ClearPoll = true
	return c, nil
}

// adoptMerge records sha, the single unrecorded commit at the tip
// (driveMerge's own step 0), when a previous tick committed the merge
// through CommitMerge and crashed before recording it: sha must have
// exactly two parents, the second equal to req.BaseSHA and the first equal
// to the branch's own last recorded commit (recordedShas(reports)), and be
// signed. Unlike that trusted shape, an ordinary git commit an agent (or
// anyone with bash in the worktree) made during the merge session can carry
// the same two parents and a real signature, so adopt never trusts shape
// alone: it also re-runs CHECK's own gates -- the project's commands, a
// clean tree, every path the commit touched confined to the merge's own
// side set (git diff parents[0]..parents[1]), and no conflict marker left
// in any of them -- exactly what mergeCheck already requires of a tick that
// commits for real. Unlike building's own adopt, there is no subject or
// claims check here -- a merge commit's own message is never the agent's to
// write -- so a clean StartBaseMerge (task 6's own no-run case) and an
// agent-resolved merge adopt exactly alike: the report is the newest ok
// merge run's report in this request's session (mergeReportFor), or the
// synthesized one when the session has none. Any failed check escalates
// unverifiableCommitWhat/Why, detail naming which one failed.
func (h shipHandler) adoptMerge(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, req baseMergeRequest, reports []store.BuildReportRow, sha string) (store.HandlerCommit, error) {
	fail := func(detail string) store.HandlerCommit {
		return mergeEscalation(t, d, req, unverifiableCommitWhat, unverifiableCommitWhy, detail)
	}

	parents, err := proj.Orch.CommitParents(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: commit parents: %w", err)
	}
	recorded := recordedShas(reports)
	isMerge := len(parents) == 2
	mergesRequestedBase := isMerge && parents[1] == req.BaseSHA
	extendsRecordedTip := isMerge && len(recorded) > 0 && parents[0] == recorded[len(recorded)-1]
	if !mergesRequestedBase || !extendsRecordedTip {
		return fail("not this request's merge commit"), nil
	}

	signed, err := proj.Orch.SignedStatus(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: signed status: %w", err)
	}
	if !signed {
		return fail("unsigned"), nil
	}

	results, err := runCheckCommands(ctx, d, t, wt, proj, nil)
	if err != nil {
		return mergeCommandInfraEscalation(t, d, req, err, nil, nil)
	}
	if len(failedKinds(results)) > 0 {
		return fail("commands failed"), nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: changed paths: %w", err)
	}
	if len(changed) != 0 {
		return fail("tree not clean"), nil
	}

	commitPaths, err := proj.Orch.ChangedFilesBetween(ctx, wt, parents[0], sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: changed files between: %w", err)
	}
	mergeSidePaths, err := proj.Orch.ChangedFilesBetween(ctx, wt, parents[0], parents[1])
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: changed files between: %w", err)
	}
	if outside := pathsOutside(commitPaths, mergeSidePaths); len(outside) > 0 {
		return fail("outside the merge"), nil
	}

	markers, err := proj.Orch.PathsWithConflictMarkers(ctx, wt, commitPaths)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: conflict marker paths: %w", err)
	}
	if len(markers) > 0 {
		return fail("conflict markers remain"), nil
	}

	maxResumes := d.Machine.Jobs[jobMergeName].MaxResumes
	_, _, newestRun, found, err := d.Store.SessionAfter(ctx, t.ID, jobMergeName, req.AfterRunID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: adopt: session after: %w", err)
	}
	var rid *int64
	if found && runOutcome(newestRun) == string(response.OutcomeOk) {
		id := newestRun.ID
		rid = &id
	}

	report, err := mergeReportFor(t, rid, req, reports)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	slog.Warn("base merge commit adopted", "ticket_id", t.ID, "request_id", req.MessageID, "run_id", int64OrZero(rid), "commit_sha", sha)
	return mergeLandedCommit(t, d, req, rid, report, sha)
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
		c := mergeEscalationCode(t, d, req, string(response.EscalationCodeEnvironment), mergeDecisionWhat, mergeDecisionWhy,
			mergeQuestionText(resp.Questions), &rr.Reserved.SessionID, &rr.Reserved.RunID)
		c.Runs = terminalRuns(rr, string(response.OutcomeQuestion))
		c.Session = sessionCommit
		return c, nil
	case *response.ErrorResponse:
		c := mergeEscalationCode(t, d, req, string(resp.Error.Code), resp.Error.What, resp.Error.Why,
			resp.Error.Tried, &rr.Reserved.SessionID, &rr.Reserved.RunID)
		c.Runs = terminalRuns(rr, string(response.OutcomeError))
		c.Session = sessionCommit
		return c, nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: merge: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// mergeAfterError handles a merge session whose newest run ended in error
// (overview design "One merge tick"): an interrupted newest run resumes
// free with the fixed interrupted input, skipping the invalid-output
// check entirely, since nothing about it answered the prompt at all; a
// first invalid output (no escalation yet, ConsecutiveInvalidOutputs == 1)
// resumes charged with the invalid input, giving the agent one chance to
// return a valid document before response_invalid would otherwise
// escalate; anything else means the failure already escalated once and the
// owner retried it through the generic shipping row (retryMerge), so this
// session is done and a fresh one starts over the same half-resolved tree
// (reopenMerge).
func (h shipHandler) mergeAfterError(ctx context.Context, t store.Ticket, d Deps, wt orchestrator.Worktree, req baseMergeRequest, sess store.Session, newest store.Run) (store.HandlerCommit, error) {
	log := slog.With("ticket_id", t.ID, "request_id", req.MessageID, "run_id", newest.ID)

	if newest.Interrupted {
		bump, _ := resumeCharge(newest)
		log.Debug("base merge step", "step", "resume_interrupted")
		input := prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false}
		return h.runMergeResume(ctx, t, d, wt, req, sess, 0, []prompt.NamedInput{input}, bump)
	}

	n, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobMergeName, &sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: consecutive invalid outputs: %w", err)
	}
	if n == 1 {
		maxResumes := d.Machine.Jobs[jobMergeName].MaxResumes
		if sess.Resumes >= maxResumes {
			log.Debug("base merge step", "step", "resumes_exhausted")
			return mergeEscalationIDs(t, d, req, mergeResumesWhat, fmt.Sprintf(mergeResumesWhyFmt, maxResumes), "", &sess.ID, &newest.ID), nil
		}
		log.Debug("base merge step", "step", "resume_invalid", "invalid_count", n)
		return h.runMergeResume(ctx, t, d, wt, req, sess, 1, []prompt.NamedInput{prompt.Invalid(invalidRetryText(reason))}, true)
	}

	log.Debug("base merge step", "step", "reopen_after_error")
	return h.reopenMerge(ctx, t, d, req, "", nil)
}

// isBaseMergeTried reports whether tried's first line is "base merge <id>":
// resolvePostBuildEscalation's own routing test for every merge-unit
// escalation, since they all share origin shipping with escalation codes
// that mean nothing merge-specific on their own.
func isBaseMergeTried(tried string) bool {
	_, ok := baseMergeTriedID(tried)
	return ok
}

// retryMerge is the owner's retry on a merge-unit escalation (overview
// design "Request lifecycle"): when the ticket's one open request is the
// one tried names, reopenMerge closes it and opens its successor carrying
// the owner's notes; otherwise a later tick already landed or closed it on
// its own before the owner answered, and the generic shipping "retry
// requested" marker is all there is left to write.
func (h shipHandler) retryMerge(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, tried string) (store.HandlerCommit, error) {
	id, ok := baseMergeTriedID(tried)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: retry: malformed tried text %q", tried)
	}
	req, open, err := openBaseMerge(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if !open || req.MessageID != id {
		slog.Info("base merge retry on a settled request", "ticket_id", t.ID, "tried_request_id", id, "open", open, "open_request_id", req.MessageID)
		return shipRetryMarkerCommit(t, d, resolveIDs), nil
	}
	return h.reopenMerge(ctx, t, d, req, notes, resolveIDs)
}

// reopenMerge closes req and opens its successor in one commit (overview
// design "Request lifecycle" and "Markers" table): "base merge closed <req
// id>", then a fresh request carrying req's own base branch and sha,
// RetryOf req's id, and Notes the two notes joined by a blank line with
// empty parts dropped. The worktree is left exactly as it is: StartBaseMerge
// finds the merge still in progress and the fresh run continues from the
// half-resolved tree.
func (h shipHandler) reopenMerge(ctx context.Context, t store.Ticket, d Deps, req baseMergeRequest, notes string, resolveIDs []int64) (store.HandlerCommit, error) {
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: merge: reopen: max run id: %w", err)
	}
	var parts []string
	if req.Notes != "" {
		parts = append(parts, req.Notes)
	}
	if notes != "" {
		parts = append(parts, notes)
	}
	next := baseMergeRequest{
		AfterRunID: maxRunID, BaseBranch: req.BaseBranch, BaseSHA: req.BaseSHA,
		RetryOf: req.MessageID, Notes: strings.Join(parts, "\n\n"),
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: baseMergeClosedBody(req.MessageID)},
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: next.body()},
	}
	c.ResolveQuestions = resolveIDs
	c.ClearPoll = true
	slog.Info("base merge reopened", "ticket_id", t.ID, "closed_request_id", req.MessageID)
	return c, nil
}
