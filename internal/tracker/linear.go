package tracker

import (
	"context"
	"errors"
)

// errLinearNotImplemented is what every Linear method returns.
var errLinearNotImplemented = errors.New("linear: not implemented")

// Linear is the stub tracker. Every method returns errLinearNotImplemented.
// It exists so the interface has a second implementation and Package 6
// records the linear seam; config never selects it in v1 (decision Q39).
type Linear struct{}

var _ Tracker = Linear{}

// Intake always fails: see errLinearNotImplemented.
func (Linear) Intake(context.Context, string, IntakeRule) ([]Ticket, error) {
	return nil, errLinearNotImplemented
}

// Fetch always fails: see errLinearNotImplemented.
func (Linear) Fetch(context.Context, string, string) (Ticket, error) {
	return Ticket{}, errLinearNotImplemented
}

// Issue always fails: see errLinearNotImplemented (PKG9-PLAN.md D29: "Linear
// stub returns not-implemented").
func (Linear) Issue(context.Context, string, string) (Ticket, error) {
	return Ticket{}, errLinearNotImplemented
}

// Comment always fails: see errLinearNotImplemented.
func (Linear) Comment(context.Context, string, string, string) error {
	return errLinearNotImplemented
}

// FileTicket always fails: see errLinearNotImplemented.
func (Linear) FileTicket(context.Context, string, NewTicket) (string, error) {
	return "", errLinearNotImplemented
}

// Collaborators always fails: see errLinearNotImplemented.
func (Linear) Collaborators(context.Context, string) ([]string, error) {
	return nil, errLinearNotImplemented
}

// Close always fails: see errLinearNotImplemented.
func (Linear) Close(context.Context, string, string) error {
	return errLinearNotImplemented
}

// CommentContains always fails: see errLinearNotImplemented.
func (Linear) CommentContains(context.Context, string, string, string) (bool, error) {
	return false, errLinearNotImplemented
}

// Comments always fails: see errLinearNotImplemented.
func (Linear) Comments(context.Context, string, string) ([]Comment, error) {
	return nil, errLinearNotImplemented
}
