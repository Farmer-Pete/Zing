package dispatch

import (
	"context"
	"fmt"

	"zing/internal/job"
	"zing/internal/tracker"
)

// issueText reads ref's current body and its owner's comments from tr under
// b's tracker project, filtered for b.User.
func issueText(ctx context.Context, tr tracker.Tracker, b Binding, ref string) (job.IssueText, error) {
	tk, err := tr.Fetch(ctx, b.TrackerProject, ref)
	if err != nil {
		return job.IssueText{}, fmt.Errorf("dispatch: issue text %s: fetch: %w", ref, err)
	}
	cs, err := tr.Comments(ctx, b.TrackerProject, ref)
	if err != nil {
		return job.IssueText{}, fmt.Errorf("dispatch: issue text %s: comments: %w", ref, err)
	}
	mine := tracker.OwnerComments(cs, b.User)
	return job.IssueText{Body: tk.Body, OwnerComments: tracker.RenderComments(mine), CommentCount: len(mine)}, nil
}

// IssueText implements job.TicketSource over the dispatcher's own tracker
// and bindings, as PostPRLink does for job.ShipTracker.
func (d *Dispatcher) IssueText(ctx context.Context, projectID int64, ref string) (job.IssueText, error) {
	b, ok := d.bindingForProject(projectID)
	if !ok {
		return job.IssueText{}, fmt.Errorf("dispatch: issue text: no binding for project %d", projectID)
	}
	return issueText(ctx, d.tracker, b, ref)
}
