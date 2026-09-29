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

// buildingHandler runs the fake once through runJob (job "build", label
// "1", fake turn 1; PKG8-PLAN.md task 8), so every building tick passes the
// section 5.5 sandbox rule the same way a real build run will: unlike every
// other skeleton handler, it now reads its Header().Outcome from a
// runResult rather than calling the runtime directly, and terminalizes the
// reserved run with terminalRuns and freshSessionRecord (planning.go, task
// 6). On ok it transitions to reviewing (design section 6.6); task 9
// replaces this handler with the real building state machine.
type buildingHandler struct{}

// buildLabel is the task number the skeleton's one scripted build task
// carries (fixtures/scripts/build/1/1.xml).
const buildLabel = "1"

// buildSessionRuntime is the runtime name runJob resolves the build job's
// session under (machine.toml's own jobs.build.runtime): "claude" in
// production, and in every test suite here, whatever runtime.Runtime a
// Deps.Runtimes registers under that name -- a real claude runtime, or the
// same *runtime.Fake every other machine.toml name in the suite resolves to
// (claim's own doc comment, skeleton_test.go).
const buildSessionRuntime = "claude"

func (buildingHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	su := store.SessionUpsert{Job: string(response.JobBuild), Runtime: buildSessionRuntime}
	req := runtime.RunRequest{Job: response.JobBuild, Label: buildLabel}
	rr, runErr := runJob(ctx, d, t, string(response.JobBuild), su, req, nil)
	if runErr != nil {
		if c, ok, failErr := routeFailure(t, d, rr, runErr, 0, freshSessionRecord(rr), nil, response.EscalationOriginBuild); ok {
			return c, failErr
		}
		return store.HandlerCommit{}, fmt.Errorf("job: building: %w", runErr)
	}

	switch rr.Res.Response.Header().Outcome {
	case response.OutcomeOk:
		c := baseCommit(t, d)
		c.Session = freshSessionRecord(rr)
		c.Runs = terminalRuns(rr, string(response.OutcomeOk))
		c.Next, c.Reason = stateReviewing, reasonBuildDone
		return c, nil
	case response.OutcomeError:
		return escalateCommit(t, d, rr.Res.Response)
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: building: outcome %s is not handled", rr.Res.Response.Header().Outcome)
	}
}
