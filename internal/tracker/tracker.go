// Package tracker is where tickets come from. The Tracker interface lets a
// fixture implementation and, later, a real GitHub adapter (Package 6) serve
// the same dispatcher behind one seam.
package tracker

import (
	"context"
	"errors"
)

// Tracker is the ticket source. The project name is passed per call so one
// implementation can serve many projects.
type Tracker interface {
	// Intake returns the tickets project has open under rule.
	Intake(ctx context.Context, project string, rule IntakeRule) ([]Ticket, error)
	// Fetch returns the single ticket ref within project.
	Fetch(ctx context.Context, project, ref string) (Ticket, error)
	// Issue returns the single issue ref within project, for manual intake
	// (PKG9-PLAN.md D29, POST /projects/{id}/pickup): the caller, not this
	// method, builds the exact console refusal message, since only the
	// caller knows the issue number the console form was given. Issue
	// returns ErrIssueNotFound, ErrIssueClosed, or ErrIssueIsPullRequest
	// (sentinels, matched with errors.Is) in place of a Ticket when ref
	// does not name a pickable open issue.
	Issue(ctx context.Context, project, ref string) (Ticket, error)
	// Comment posts body against ref within project.
	Comment(ctx context.Context, project, ref, body string) error
	// FileTicket creates t within project and returns its new ref.
	FileTicket(ctx context.Context, project string, t NewTicket) (ref string, err error)
	// Collaborators returns project's collaborator names.
	Collaborators(ctx context.Context, project string) ([]string, error)
	// Close closes ref within project, with reason "completed" (design
	// section 10.5, PKG9-PLAN.md section 8.6): closing an already-closed
	// issue succeeds, so a repeated call after a crash costs nothing.
	Close(ctx context.Context, project, ref string) error
	// CommentContains reports whether ref already carries a comment
	// containing needle, posted by the tracker's own authenticated login
	// (design section 10.5): a comment from any other account never
	// counts, so a spoofed marker cannot suppress a comment Zing must post
	// at most once (PKG9-PLAN.md section 8.2, 8.6, 11).
	CommentContains(ctx context.Context, project, ref, needle string) (bool, error)
}

// Ticket is a tracker ticket, not the store row.
type Ticket struct {
	Ref, Title, Body string
}

// NewTicket is what FileTicket takes to create a Ticket.
type NewTicket struct {
	Title, Body string
	DependsOn   []string
}

// IntakeRule narrows Intake to the tickets it should return.
type IntakeRule struct {
	Assignee string
}

// ErrIssueNotFound, ErrIssueClosed, and ErrIssueIsPullRequest are Issue's
// own sentinels (PKG9-PLAN.md D29): a caller matches one with errors.Is to
// build the exact console refusal message for an issue number it, not
// Issue, holds.
var (
	ErrIssueNotFound      = errors.New("tracker: issue not found")
	ErrIssueClosed        = errors.New("tracker: issue closed")
	ErrIssueIsPullRequest = errors.New("tracker: issue is a pull request")
)
