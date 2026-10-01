// This test file is white-box (package tracker, not tracker_test) so it
// can assert against the unexported errLinearNotImplemented sentinel with
// errors.Is, per PKG6-PLAN.md section 4.4.
package tracker

import (
	"context"
	"errors"
	"testing"
)

// TestLinear_EveryMethodReturnsNotImplemented proves every one of the five
// Tracker methods on Linear returns errLinearNotImplemented, per decision
// D5: linear is a compile-time stub only, never selected at runtime.
func TestLinear_EveryMethodReturnsNotImplemented(t *testing.T) {
	t.Parallel()

	var l Linear
	ctx := context.Background()

	if _, err := l.Intake(ctx, "proj", IntakeRule{}); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("Intake err = %v, want %v", err, errLinearNotImplemented)
	}
	if _, err := l.Fetch(ctx, "proj", "ref"); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("Fetch err = %v, want %v", err, errLinearNotImplemented)
	}
	if err := l.Comment(ctx, "proj", "ref", "body"); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("Comment err = %v, want %v", err, errLinearNotImplemented)
	}
	if _, err := l.FileTicket(ctx, "proj", NewTicket{}); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("FileTicket err = %v, want %v", err, errLinearNotImplemented)
	}
	if _, err := l.Collaborators(ctx, "proj"); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("Collaborators err = %v, want %v", err, errLinearNotImplemented)
	}
	if err := l.Close(ctx, "proj", "ref"); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("Close err = %v, want %v", err, errLinearNotImplemented)
	}
	if _, err := l.CommentContains(ctx, "proj", "ref", "needle"); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("CommentContains err = %v, want %v", err, errLinearNotImplemented)
	}
}

// TestLinear_SatisfiesTracker proves Linear{} satisfies the Tracker
// interface at compile time (the var _ Tracker = Linear{} assertion in
// linear.go), by using it as one through a variable of the interface type.
func TestLinear_SatisfiesTracker(t *testing.T) {
	t.Parallel()

	var tr Tracker = Linear{}
	if _, err := tr.Collaborators(context.Background(), "proj"); !errors.Is(err, errLinearNotImplemented) {
		t.Errorf("Collaborators via Tracker err = %v, want %v", err, errLinearNotImplemented)
	}
}
