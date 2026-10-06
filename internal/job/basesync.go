// basesync.go is every baseSync point's shared body (overview design): the
// decision, at review round 1, at a judge round's start, and at shipping's
// own CI-failed row, whether main has moved under the ticket in a way that
// point cares about, and, when it has, the base merge request that opens
// to bring it in before the caller's own step runs.
package job

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/orchestrator"
	"zing/internal/store"
)

// baseSync is one base merge point: it opens a base merge request marked
// with point when main has moved past the branch (and, for review and
// judge, changed a file the ticket changed). opened false means the caller
// goes on exactly as before: either the branch is already current, the
// point is at its own jobs.merge.max_loops budget, or the base could not be
// read (logged, never escalated -- the owner's Q3 decision).
func baseSync(ctx context.Context, t store.Ticket, d Deps, point syncPoint) (c store.HandlerCommit, opened bool, err error) {
	log := slog.With("ticket_id", t.ID, "point", string(point))

	proj, wt, esc, err := ensureWorktreeOrEscalate(ctx, t, d, func(string) store.HandlerCommit { return store.HandlerCommit{} })
	if err != nil {
		return store.HandlerCommit{}, false, err
	}
	if esc != nil {
		log.Info("base sync", "result", "worktree") // the caller's own checks escalate it
		return store.HandlerCommit{}, false, nil
	}

	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, baseMergePrefix)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: base sync: base merge markers: %w", err)
	}
	if pointMergeCount(rows, point) >= d.Machine.Jobs[jobMergeName].MaxLoops {
		log.Info("base sync", "result", "max_loops")
		return store.HandlerCommit{}, false, nil
	}

	baseSHA, head, mb, readErr := readBaseAndHead(ctx, proj, wt)
	if readErr != nil {
		log.Warn("base sync", "result", "base_unreadable", "err", readErr)
		return store.HandlerCommit{}, false, nil
	}
	if mb == baseSHA {
		log.Info("base sync", "result", "not_moved")
		return store.HandlerCommit{}, false, nil
	}

	var overlap []string
	if point != syncPointCI {
		overlap, readErr = sharedPaths(ctx, proj, wt, mb, baseSHA, head)
		if readErr != nil {
			log.Warn("base sync", "result", "base_unreadable", "err", readErr)
			return store.HandlerCommit{}, false, nil
		}
		if len(overlap) == 0 {
			log.Info("base sync", "result", "no_overlap")
			return store.HandlerCommit{}, false, nil
		}
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: base sync: max run id: %w", err)
	}
	req := baseMergeRequest{AfterRunID: maxRunID, BaseBranch: proj.Orch.DefaultBranch(), BaseSHA: baseSHA, Point: point}
	c = baseCommit(t, d)
	c.Messages = []store.Message{
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: syncNotice(req, overlap)},
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: req.body()},
	}
	log.Info("base sync", "result", "opened", "base_sha", baseSHA, "after_run", maxRunID, "overlap", overlap)
	return c, true, nil
}

// readBaseAndHead reads the three shas baseSync needs: FetchBase, then
// HeadSHA, then MergeBase(head, baseSHA), each short-circuiting on the
// first error (the owner's Q3 decision: the caller logs a warning and goes
// on with no merge, rather than escalating).
func readBaseAndHead(ctx context.Context, proj Project, wt orchestrator.Worktree) (baseSHA, head, mb string, err error) {
	baseSHA, err = proj.Orch.FetchBase(ctx, wt)
	if err != nil {
		return "", "", "", err
	}
	head, err = proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return "", "", "", err
	}
	mb, err = proj.Orch.MergeBase(ctx, wt, head, baseSHA)
	if err != nil {
		return "", "", "", err
	}
	return baseSHA, head, mb, nil
}

// sharedPaths returns the sorted intersection of ChangedFilesBetween(mb,
// baseSHA) and ChangedFilesBetween(mb, head): the paths main changed since
// the fork point that the ticket branch also changed since that same fork
// point (review and judge's own overlap condition). ChangedFilesBetween
// already returns each side sorted, so filtering baseSHA's own list down to
// what head's also carries keeps the result sorted without a second sort.
func sharedPaths(ctx context.Context, proj Project, wt orchestrator.Worktree, mb, baseSHA, head string) ([]string, error) {
	baseChanged, err := proj.Orch.ChangedFilesBetween(ctx, wt, mb, baseSHA)
	if err != nil {
		return nil, err
	}
	headChanged, err := proj.Orch.ChangedFilesBetween(ctx, wt, mb, head)
	if err != nil {
		return nil, err
	}
	headSet := make(map[string]bool, len(headChanged))
	for _, p := range headChanged {
		headSet[p] = true
	}
	var out []string
	for _, p := range baseChanged {
		if headSet[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

// syncNotice is the update message baseSync writes just before the
// request, one line per point (overview design "Shape").
func syncNotice(req baseMergeRequest, overlap []string) string {
	sha7 := req.BaseSHA[:7]
	switch req.Point {
	case syncPointReview:
		return fmt.Sprintf("Merging %s at %s before review: it changed files this ticket also changes: %s", req.BaseBranch, sha7, strings.Join(overlap, ", "))
	case syncPointJudge:
		return fmt.Sprintf("Merging %s at %s before judging: it changed files this ticket also changes: %s", req.BaseBranch, sha7, strings.Join(overlap, ", "))
	default:
		return fmt.Sprintf("Merging %s at %s before asking for a CI fix: CI tests the pull request merged with %s, and %s moved since this branch last merged it", req.BaseBranch, sha7, req.BaseBranch, req.BaseBranch)
	}
}
