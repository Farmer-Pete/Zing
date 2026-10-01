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

// ---- queued: the one remaining code-only transition -----------------------

// queuedHandler advances a claimed ticket into planning (design section 6.5).
type queuedHandler struct{}

func (queuedHandler) Run(_ context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Next, c.Reason = statePlanning, reasonPickedUp
	return c, nil
}

// building, reviewing, judging, and shipping have their own real handlers
// now (building.go, task 9; reviewing.go, task 10; judging.go, M2 task 8;
// shipping.go, M3 tasks 6 and 7): the skeleton's own buildingHandler,
// reviewingHandler, judgingHandler, and shippingHandler existed because the
// walking skeleton needed every state to advance on nothing more than a
// scripted fake run, or, for reviewing, judging, and shipping, no run at
// all until each state's own real machine landed; those handlers now run
// the real state machine (design sections 6, 7, 8) instead.
