package job

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

	// The question message type and lifecycle values, and the answer
	// message type, named once here so skeleton.go carries identifiers
	// rather than repeated literals (design section 6.3, 6.6, 8.2). These
	// mirror internal/store's own unexported constants of the same values;
	// this package cannot reach those, and the values are part of the
	// closed set migrations/0001_init.sql fixes, not private store detail.
	msgTypeQuestion       = "question"
	msgTypeAnswer         = "answer"
	questionStateOpen     = "open"
	questionStateAnswered = "answered"
	waitingFlagQuestions  = "questions"
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

// ---- planning: the question-and-resume handler (design section 6.6) -----

// planningHandler exercises the question and the resume path. It reads
// OpenSession(ticket, "planning") first: no open session means first entry
// (planningFirstEntry); an open session means resume (planningResume), which
// the dispatcher only reaches once the whole batch is answered and
// waiting_on has cleared.
type planningHandler struct{}

func (planningHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	sess, open, err := d.Store.OpenSession(ctx, t.ID, statePlanning)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: open session: %w", err)
	}
	if !open {
		return planningFirstEntry(ctx, t, d)
	}
	return planningResume(ctx, t, d, sess)
}

// planningFirstEntry runs the fake once with no session (fake turn 1) and,
// on a question outcome, posts one message per question, attaches them to
// the inserted turn-0 run, and waits on "questions". It carries no Next: the
// ticket stays in planning (design section 6.6 diagram, left column).
func planningFirstEntry(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	res, err := d.Runtime.Run(ctx, runtime.RunRequest{Job: response.JobPlanning})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first entry: run: %w", err)
	}

	switch res.Response.Header().Outcome {
	case response.OutcomeQuestion:
		qr, ok := res.Response.(*response.QuestionResponse)
		if !ok {
			return store.HandlerCommit{}, fmt.Errorf(
				"job: planning: first entry: outcome question but response is %T, not *response.QuestionResponse", res.Response)
		}
		msgs, err := questionMessages(t.ID, qr.Questions)
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: first entry: %w", err)
		}

		c := baseCommit(t, d)
		externalID := res.SessionID
		c.Session = &store.SessionUpsert{Job: string(response.JobPlanning), Runtime: runtimeFake, ExternalID: &externalID}
		c.Runs = []store.Run{{Turn: 0, Outcome: outcomePtr(response.OutcomeQuestion)}}
		c.Messages = msgs
		c.AttachRunToMsgs = true
		waiting := waitingFlagQuestions
		c.Waiting = &waiting
		return c, nil
	case response.OutcomeError:
		return escalateCommit(t, d, res.Response)
	default:
		return store.HandlerCommit{}, fmt.Errorf(
			"job: planning: first entry: outcome %s is not handled", res.Response.Header().Outcome)
	}
}

// planningResume reads the batch's sent answers, serializes them into the
// resumed run's prompt, runs the fake with the open session's external id
// (fake turn 2), and, on ready, resolves the batch and transitions to
// building (design section 6.6 diagram, right column). The batch is scoped
// to the session's turn-0 run (design section 6.6): only the questions that
// run posted, not every answered question on the ticket, so an unrelated
// answered question elsewhere on the ticket is never swept into this
// resume's ResolveQuestions.
func planningResume(ctx context.Context, t store.Ticket, d Deps, sess store.Session) (store.HandlerCommit, error) {
	if sess.ExternalID == nil || *sess.ExternalID == "" {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: session %d has no external id", sess.ID)
	}

	run0, ok, err := d.Store.FirstRun(ctx, sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: first run: %w", err)
	}
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: session %d has no turn-0 run", sess.ID)
	}

	answered, err := d.Store.QuestionsByRun(ctx, run0.ID, questionStateAnswered)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: questions by run: %w", err)
	}
	if len(answered) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: run %d has no answered questions", run0.ID)
	}

	prompt, err := resumePrompt(ctx, d, t.ID, answered)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: %w", err)
	}

	res, err := d.Runtime.Run(ctx, runtime.RunRequest{
		Job: response.JobPlanning, SessionID: *sess.ExternalID, Prompt: prompt,
	})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: run: %w", err)
	}

	switch res.Response.Header().Outcome {
	case response.OutcomeReady:
		ids := make([]int64, len(answered))
		for i := range answered {
			ids[i] = answered[i].ID
		}

		c := baseCommit(t, d)
		c.Session = &store.SessionUpsert{ID: &sess.ID, BumpResumes: true}
		c.Runs = []store.Run{{Turn: 1, Outcome: outcomePtr(response.OutcomeReady)}}
		c.ResolveQuestions = ids
		c.Next, c.Reason = stateBuilding, reasonPlanReady
		return c, nil
	case response.OutcomeError:
		return escalateCommit(t, d, res.Response)
	default:
		return store.HandlerCommit{}, fmt.Errorf(
			"job: planning: resume: outcome %s is not handled", res.Response.Header().Outcome)
	}
}

// questionMessages builds one question message per q in qs: the stored
// QuestionPayload maps Key, Recommended, and Options straight across, with
// Kind fixed to "question" and State fixed to "open" at insert (design
// section 6.6's mapping table); the message Body carries Title as the
// heading, then Body, title first.
func questionMessages(ticketID int64, qs []response.Question) ([]store.Message, error) {
	msgs := make([]store.Message, 0, len(qs))
	for _, q := range qs {
		// q.Key comes off the wire matching response.Question's own pattern
		// (^[qQ][0-9]+$), but the stored QuestionPayload.Key is the tighter
		// ^Q[0-9]+$: uppercase it here so a lowercase wire key (q1) still
		// persists as a schema-valid Q1.
		payload, err := json.Marshal(response.QuestionPayload{
			Key:         strings.ToUpper(q.Key),
			Kind:        response.QuestionKindQuestion,
			State:       response.QuestionStateOpen,
			Recommended: q.Recommended,
			Options:     q.Options,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal question payload for %s: %w", q.Key, err)
		}
		msgs = append(msgs, store.Message{
			TicketID: ticketID,
			Type:     msgTypeQuestion,
			Author:   authorZing,
			State:    new(questionStateOpen),
			Body:     q.Title + "\n\n" + q.Body,
			Payload:  payload,
		})
	}
	return msgs, nil
}

// resumePrompt reads every message on ticketID once, pairs each of
// answered's questions with its sent answer, and serializes the batch into a
// readable prompt string. The fake ignores this prompt; a real Package 7
// runtime reads it (design section 6.6).
func resumePrompt(ctx context.Context, d Deps, ticketID int64, answered []store.MessageRow) (string, error) {
	all, err := d.Store.ListMessages(ctx, ticketID)
	if err != nil {
		return "", fmt.Errorf("list messages: %w", err)
	}
	answerByQuestion := make(map[int64]store.MessageRow, len(answered))
	for i := range all {
		m := &all[i]
		if m.Type == msgTypeAnswer && m.ParentID != nil {
			answerByQuestion[*m.ParentID] = *m
		}
	}

	var sb strings.Builder
	for i := range answered {
		q := &answered[i]
		var qp response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &qp); err != nil {
			return "", fmt.Errorf("unmarshal question %d payload: %w", q.ID, err)
		}
		option := ""
		if ans, ok := answerByQuestion[q.ID]; ok {
			var ap response.AnswerPayload
			if err := json.Unmarshal(ans.Payload, &ap); err != nil {
				return "", fmt.Errorf("unmarshal answer for question %d: %w", q.ID, err)
			}
			if ap.Option != nil {
				option = *ap.Option
			}
		}
		fmt.Fprintf(&sb, "%s: %s\n", qp.Key, option)
	}
	return sb.String(), nil
}

// outcomePtr returns a *string holding o's string value, the shape
// store.Run.Outcome takes.
func outcomePtr(o response.Outcome) *string {
	s := string(o)
	return &s
}
