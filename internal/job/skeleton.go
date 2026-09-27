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
	reasonBuildDone   = "build done"
	reasonReviewClean = "review clean"
	reasonJudgePassed = "judge passed"
	reasonShipped     = "shipped"
	runtimeFake       = "fake"
	msgTypeEscalation = "escalation"
	authorZing        = "zing"
	waitingFlagError  = "error"

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

// skeletonOrigin maps r's job to design section 6.7's closed EscalationOrigin
// set, for this file's placeholder escalateCommit, which every skeleton
// handler still shares. classify and planreview map directly; planning maps
// to planning_first for both entry and resume, because this helper cannot
// tell them apart from the job alone, and every job task 4 gives no
// dedicated origin (building, and everything after it) also falls back to
// planning_first -- harmless today, since no test exercises this file's
// escalation path for those jobs. Task 6 replaces this file's planning
// entries with real handlers that set their own correct Origin directly.
func skeletonOrigin(job response.Job) response.EscalationOrigin {
	switch job {
	case response.JobClassify:
		return response.EscalationOriginClassify
	case response.JobPlanreview:
		return response.EscalationOriginPlanreview
	default:
		return response.EscalationOriginPlanningFirst
	}
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
		Origin:  string(skeletonOrigin(r.Header().Job)),
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
	rt, err := d.Runtimes.For(d.Machine.Jobs[string(response.JobBuild)].Runtime)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: building: resolve runtime: %w", err)
	}

	res, err := rt.Run(ctx, runtime.RunRequest{Job: response.JobBuild, Label: buildLabel})
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

// outcomePtr returns a *string holding o's string value, the shape
// store.Run.Outcome takes.
func outcomePtr(o response.Outcome) *string {
	s := string(o)
	return &s
}
