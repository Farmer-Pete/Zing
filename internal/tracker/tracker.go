// Package tracker is where tickets come from. The Tracker interface lets a
// fixture implementation and, later, a real GitHub adapter (Package 6) serve
// the same dispatcher behind one seam.
package tracker

import "context"

// Tracker is the ticket source. The project name is passed per call so one
// implementation can serve many projects.
type Tracker interface {
	// Intake returns the tickets project has open under rule.
	Intake(ctx context.Context, project string, rule IntakeRule) ([]Ticket, error)
	// Fetch returns the single ticket ref within project.
	Fetch(ctx context.Context, project, ref string) (Ticket, error)
	// Comment posts body against ref within project.
	Comment(ctx context.Context, project, ref, body string) error
	// FileTicket creates t within project and returns its new ref.
	FileTicket(ctx context.Context, project string, t NewTicket) (ref string, err error)
	// Collaborators returns project's collaborator names.
	Collaborators(ctx context.Context, project string) ([]string, error)
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
