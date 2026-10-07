// split.go gives *Dispatcher job.SplitTracker's two methods (design section
// 6.6's split variant, plan #74): FileSplitChild files one approved split
// child as a new tracker issue, and CloseSplitParent posts the parent's
// closing comment, once, then closes it -- the same marked-once shape
// PostPRLink and PostDone already share through postMarkedOnce.
package dispatch

import (
	"context"
	"fmt"

	"zing/internal/job"
	"zing/internal/tracker"
)

// Dispatcher satisfies job.SplitTracker through FileSplitChild and
// CloseSplitParent below, so Deps.Splitter (runAndCommit) can carry
// *Dispatcher directly, exactly as Deps.Tracker already does.
var _ job.SplitTracker = (*Dispatcher)(nil)

// FileSplitChild implements job.SplitTracker's FileSplitChild (design
// section 6.6's split variant): it resolves projectID's own tracker binding
// and files title/body as a new issue, with no DependsOn, because the job
// already wrote the "Depends on" line into body itself (splitChildBody).
func (d *Dispatcher) FileSplitChild(ctx context.Context, projectID int64, title, body string) (string, error) {
	b, ok := d.bindingForProject(projectID)
	if !ok {
		return "", fmt.Errorf("dispatch: no tracker binding for project %d", projectID)
	}
	ref, err := d.tracker.FileTicket(ctx, b.TrackerProject, tracker.NewTicket{Title: title, Body: body})
	if err != nil {
		return "", fmt.Errorf("dispatch: file split child: %w", err)
	}
	return ref, nil
}

// CloseSplitParent implements job.SplitTracker's CloseSplitParent (design
// section 6.6's split variant, owner decision Q1): posts body on ref the
// same marked-once way PostPRLink and PostDone do (postMarkedOnce), then
// closes it. Closing an already-closed issue succeeds (design 10.5), so a
// crash between the two calls, or a retried tick, never fails on the
// second one.
func (d *Dispatcher) CloseSplitParent(ctx context.Context, projectID int64, ref, body string) error {
	b, err := d.postMarkedOnce(ctx, projectID, ref, "split", func(Binding) string { return body })
	if err != nil {
		return err
	}
	if err := d.tracker.Close(ctx, b.TrackerProject, ref); err != nil {
		return fmt.Errorf("dispatch: close split parent: %w", err)
	}
	return nil
}
