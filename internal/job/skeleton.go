package job

import (
	"context"

	"zing/internal/store"
)

// Canonical state-message reasons (design section 7.2).
const (
	reasonPickedUp    = "picked up"
	reasonBuildDone   = "build done"
	reasonReviewClean = "review clean"
	reasonJudgePassed = "judge passed"
	reasonShipped     = "shipped"
	runtimeFake       = "fake"
	authorZing        = "zing"

	// The question message type and lifecycle values, named once here so
	// skeleton.go and planning.go carry identifiers rather than repeated
	// literals (design section 6.3, 6.6, 8.2). These mirror internal/store's
	// own unexported constants of the same values; this package cannot reach
	// those, and the values are part of the closed set migrations/0001_init.sql
	// fixes, not private store detail.
	msgTypeQuestion      = "question"
	questionStateOpen    = "open"
	waitingFlagQuestions = "questions"
)

// escalationOptions is the fixed local option set every escalation carries
// (design section 6.7; RunError itself carries no options).
var escalationOptions = []string{"retry", "planning", "abandon"}

// baseCommit fills the fields every commit a handler in this file returns
// shares: the ticket and the exact claim lease it must hand back as the
// fence (design section 6.5).
func baseCommit(t store.Ticket, d Deps) store.HandlerCommit {
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}
}

// ---- queued, judging, shipping: code-only transitions --------------------

// queuedHandler advances a claimed ticket into planning (design section 6.5).
type queuedHandler struct{}

func (queuedHandler) Run(_ context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Next, c.Reason = statePlanning, reasonPickedUp
	return c, nil
}

// judgingHandler advances straight to shipping; the skeleton runs no judge
// job (design section 6.5, the early-exit rule in section 0).
type judgingHandler struct{}

func (judgingHandler) Run(_ context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Next, c.Reason = stateShipping, reasonJudgePassed
	return c, nil
}

// shippingHandler advances a ticket to done; the skeleton does no git, no
// worktree, and no pull request (design section 4, non-goals).
type shippingHandler struct{}

func (shippingHandler) Run(_ context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Next, c.Reason = stateDone, reasonShipped
	return c, nil
}

// building and reviewing have their own real handlers now (building.go,
// task 9; reviewing.go, task 10): the skeleton's own buildingHandler and
// reviewingHandler existed because the walking skeleton needed both states
// to advance on nothing more than a scripted fake run, or, for reviewing,
// no run at all; those handlers now run the real state machine (design
// section 6) instead.
