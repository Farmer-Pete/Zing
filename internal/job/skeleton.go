package job

import (
	"context"
	"encoding/json"
	"fmt"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// Canonical state-message reasons (design section 7.2).
const (
	reasonPickedUp    = "picked up"
	reasonPlanReady   = "plan ready"
	reasonBuildDone   = "build done"
	reasonReviewClean = "review clean"
	reasonJudgePassed = "judge passed"
	reasonShipped     = "shipped"
	runtimeFake       = "fake"
	msgTypeEscalation = "escalation"
	authorZing        = "zing"
	waitingFlagError  = "error"
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

// escalateCommit builds the section 6.7 error-branch commit: an escalation
// message carrying RunError's fields and the fixed local options, and
// Waiting set to "error". It carries no Next: the ticket stays in its
// current state, waiting on the escalation.
func escalateCommit(t store.Ticket, d Deps, r response.Response) (store.HandlerCommit, error) {
	errResp, ok := r.(*response.ErrorResponse)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: outcome error but response is %T, not *response.ErrorResponse", r)
	}

	payload, err := json.Marshal(response.EscalationPayload{
		Code:    string(errResp.Error.Code),
		What:    errResp.Error.What,
		Why:     errResp.Error.Why,
		Tried:   errResp.Error.Tried,
		Options: escalationOptions,
	})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: marshal escalation payload: %w", err)
	}

	c := baseCommit(t, d)
	waiting := waitingFlagError
	c.Waiting = &waiting
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeEscalation, Author: authorZing, Payload: payload,
	}}
	return c, nil
}

// ---- queued, reviewing, judging, shipping: code-only transitions --------

// queuedHandler advances a claimed ticket into planning (design section 6.5).
type queuedHandler struct{}

func (queuedHandler) Run(_ context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Next, c.Reason = statePlanning, reasonPickedUp
	return c, nil
}

// reviewingHandler advances straight to judging; the skeleton runs no
// review job (design section 6.5, the early-exit rule in section 0).
type reviewingHandler struct{}

func (reviewingHandler) Run(_ context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Next, c.Reason = stateJudging, reasonReviewClean
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

// ---- building: fresh fake run, no resume ---------------------------------

// buildingHandler runs the fake once (job build, label "1", fake turn 1),
// reads Header().Outcome, and on ok returns a commit that inserts a fresh
// session and its turn-0 run and transitions to reviewing (design section
// 6.6).
type buildingHandler struct{}

// buildLabel is the task number the skeleton's one scripted build task
// carries (fixtures/scripts/build/1/1.xml).
const buildLabel = "1"

func (buildingHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	res, err := d.Runtime.Run(ctx, runtime.RunRequest{Job: response.JobBuild, Label: buildLabel})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: run: %w", err)
	}

	switch res.Response.Header().Outcome {
	case response.OutcomeOk:
		c := baseCommit(t, d)
		externalID := res.SessionID
		c.Session = &store.SessionUpsert{Job: string(response.JobBuild), Runtime: runtimeFake, ExternalID: &externalID}
		c.Runs = []store.Run{{Turn: 0, Outcome: outcomePtr(response.OutcomeOk)}}
		c.Next, c.Reason = stateReviewing, reasonBuildDone
		return c, nil
	case response.OutcomeError:
		return escalateCommit(t, d, res.Response)
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: building: outcome %s is not handled", res.Response.Header().Outcome)
	}
}

// ---- planning: the task 4 silent-ring version ----------------------------

// planningHandler is the silent-ring version of the planning handler (task
// 4 only, design section 6.6 and section 12 row 4): it always starts a
// fresh session, runs the fake once (job planning, empty label, so fake
// turn 1), reads Header().Outcome, and on ready returns a commit that
// inserts the session and turn-0 run and transitions to building. It asks
// no question and never resumes; task 7 replaces this with the
// question-and-resume handler and swaps the planning fixture.
type planningHandler struct{}

func (planningHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	res, err := d.Runtime.Run(ctx, runtime.RunRequest{Job: response.JobPlanning})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: run: %w", err)
	}

	switch res.Response.Header().Outcome {
	case response.OutcomeReady:
		c := baseCommit(t, d)
		externalID := res.SessionID
		c.Session = &store.SessionUpsert{Job: string(response.JobPlanning), Runtime: runtimeFake, ExternalID: &externalID}
		c.Runs = []store.Run{{Turn: 0, Outcome: outcomePtr(response.OutcomeReady)}}
		c.Next, c.Reason = stateBuilding, reasonPlanReady
		return c, nil
	case response.OutcomeError:
		return escalateCommit(t, d, res.Response)
	default:
		return store.HandlerCommit{}, fmt.Errorf(
			"job: planning: outcome %s is not handled by the task 4 silent-ring handler", res.Response.Header().Outcome)
	}
}

// outcomePtr returns a *string holding o's string value, the shape
// store.Run.Outcome takes.
func outcomePtr(o response.Outcome) *string {
	s := string(o)
	return &s
}
