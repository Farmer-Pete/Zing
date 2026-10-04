// merge.go is the base-merge unit (overview design): POLL's own dirty row
// (pollConflict, wired into shipping.go's poll right after the head-mismatch
// row) and, from task 5 onward, the merge unit itself. jobMergeName is
// machine.toml's "merge" job key.
package job

import (
	"context"
	"fmt"
	"log/slog"

	"zing/internal/orchestrator"
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
