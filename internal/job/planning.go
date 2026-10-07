// planning.go is the real "planning" state handler (design section 5, 6.1,
// 6.2, 6.3, 6.4, 6.5, 6.7): classify a kindless ticket, open the planning
// interview once a kind is set, resume it once the owner (or a round's own
// answers) has something new to say, store a valid ready cohort, and review
// that cohort on its own tick, looping floor findings back into planning
// under max_loops. It replaces the skeleton's fake, fixture-driven
// planningHandler (skeleton.go carried it through task 5); the other five
// skeleton handlers (queued, building, reviewing, judging, shipping) are
// untouched.
//
// Task 6 built entry-decision steps 1(c), 1(d), 2, 3, 4, and 8 of section
// 5.1. Task 7a built step 5 (live validation errors) and the ready cohort
// store, behind a TEMPORARY shortcut straight to building. Task 7b removed
// that shortcut -- a valid ready now stores the cohort and leaves the
// ticket in planning, not waiting -- and built steps 6 and 7 (the review
// tick and the floor loop) and entry step 1(e) (an answered planreview
// round), behind its own TEMPORARY shortcut straight to building on a clean
// review. This task, 7c, replaces that shortcut with section 6.6's real
// gate: a clean review posts the gate question instead, and entry step 1(a)
// interprets the owner's answered gate round -- approve runs the seal
// pre-check and, on success, seals the cohort and transitions to building;
// reject (or a reply with no option) resumes or restarts planning with the
// owner's notes. Step 1(b), an answered escalation round, resolves through
// enterFromEscalationRound's own section 6.7 choice-by-origin table: choice
// c (abandon) resolves every open or answered question and transitions
// straight to abandoned with no runtime call; every other choice retries the
// run that escalated or falls back to resumeOrFresh, per that origin's own
// row.
package job

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	zing "zing"
	"zing/internal/fence"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// Job names, message and escalation text this file owns. msgTypeQuestion,
// questionStateOpen, waitingFlagQuestions, and authorZing are skeleton.go's
// own unexported mirrors of the store package's closed vocabulary; this file
// reuses them rather than redeclaring. msgTypeUpdate and authorSystem are
// new here: skeleton.go's handlers never wrote a system marker before this
// task.
const (
	jobClassifyName   = string(response.JobClassify)
	jobPlanningName   = string(response.JobPlanning)
	jobPlanreviewName = string(response.JobPlanreview)

	msgTypeUpdate = "update"
	authorSystem  = "system"

	// waitingFlagGate is design section 6.6's own waiting_on value (the
	// gate is a QuestionKind, and console_writes.go's kindForWaitReason maps
	// this value straight back to it): distinct from waitingFlagQuestions,
	// so SendBatch clears it only when the ticket's open gate question (not
	// some unrelated open question) is answered.
	waitingFlagGate = string(response.QuestionKindGate)

	// gateOptionApprove and gateOptionReject are the two option keys the
	// gate question ever offers (design section 6.6, D8): "a" recommended.
	gateOptionApprove = "a"
	gateOptionReject  = "b"

	// escalationChoiceRetry, escalationChoiceBack, escalationChoiceAbandon,
	// and escalationChoiceGrant are the option keys escalateTx's own linked
	// question ever offers (design D10, section 6.7; plan #51): "retry",
	// "back to planning", "abandon", and, only when the escalation's own
	// payload carries a FileGrant, "let task N also change PATH". An
	// escalation round carrying replies and no option at all resolves as
	// that question's own stored Recommended option, falling back to
	// escalationChoiceRetry when none is stored (roundChoice's own default,
	// #47 follow-up: it used to hardcode escalationChoiceBack here, which
	// re-escalated replan_unsupported for a plain reply on a post-seal
	// escalation -- back to planning cannot run there any more). Every
	// other question kind roundChoice ever sees (merge, gate, split,
	// perimeter, review) keeps the plain escalationChoiceBack default it
	// always had (PR #60 review, P1: only an escalation's own Kind may read
	// its Recommended back this way -- a merge question's Recommended is
	// always "a", meaning something else entirely there).
	//
	// escalationChoiceAccept is option d offered as an
	// EscalationCommit.ExtraOptions entry on two loops_exhausted questions:
	// review's "accept the remaining findings" (ticket 60) and plan
	// review's cap_loops "accept the plan and go to the gate". It shares
	// the "d" key with escalationChoiceGrant because a grant comes only
	// from a build escalation; checkEscalationOptions (store/commit.go)
	// rejects a duplicate key if that ever changed.
	escalationChoiceRetry   = "a"
	escalationChoiceBack    = "b"
	escalationChoiceAbandon = "c"
	escalationChoiceGrant   = "d"
	escalationChoiceAccept  = "d"

	responseInvalidWhat = "the model's final message failed validation twice in a row"

	loopsExhaustedWhat = "raise machine.toml's planreview max_loops, or abandon"
	loopsExhaustedWhy  = "the plan review has delivered the maximum number of floor-finding cycles machine.toml allows"

	runtimeExecFailedWhat = "the runtime could not complete this run"
	runtimeExecFailedWhy  = "the process failed to start, timed out, exceeded the output cap, or exited with no parseable result"

	budgetExhaustedWhat = "raise budget.agent_minutes_per_ticket or abandon"
	budgetExhaustedWhy  = "the ticket's spent agent time has reached the configured budget"

	resumesExhaustedWhatFmt = "raise machine.toml's %s max_resumes, or abandon"
	resumesExhaustedWhyFmt  = "the %s session has resumed the maximum number of times machine.toml allows"

	// nothingToDoNoCodeClaimsWhat is section 6.8's own nothing_to_do
	// escalation What text (task 8, tightened by F022): response.Validate's
	// CheckNothingToDoClaims already rejects any nothing_to_do response
	// carrying a code claim that is not verdict=false as an InvalidOutputError,
	// so by the time nothingToDoCommit runs, naming zero code claims is the
	// only way left for a nothing_to_do outcome to fail to prove there is
	// nothing to build.
	nothingToDoNoCodeClaimsWhat = "no code claims to verify"
	nothingToDoWhy              = "a nothing_to_do outcome must name at least one code claim and verify every code claim false to accept it automatically"

	reasonNothingToDo = "nothing to do"

	// The section 6.6 gate approve pre-check's own fixed What text, one
	// per failing branch (0, 1, 2, 3, 6; branches 4 and 5 succeed). Why is
	// shared across every branch: what actually differs, the What text,
	// already names the specific failure.
	sealFailedNoCohortWhat       = "no current plan cohort"
	sealFailedNoRunWhat          = "cohort has no producing run"
	sealFailedNoConfirmationWhat = "no confirmation for the current plan"
	sealFailedMismatchTwiceWhat  = "seal transaction mismatched twice"
	sealFailedBadCountWhatFmt    = "cohort has %d scenarios, want 2 to 30"
	sealFailedPartialWhatFmt     = "cohort is partially or inconsistently sealed (%d of %d)"
	sealFailedWhy                = "the gate's approval pre-check found the plan cohort is not ready to seal"
	reasonGateApproved           = "gate approved"
	reasonGateApprovedAlready    = "gate approved (already sealed)"

	// reasonAbandonedFmt is section 6.7 choice "c"'s own Reason text (design
	// D10): "owner abandoned after <code>", the escalation's own Code.
	reasonAbandonedFmt = "owner abandoned after %s"

	// gateApproveExplains is F013's own addition to the gate question's
	// body (design section 6.6, D8), rewritten by D32 (design section
	// 22.12.3): the objective alone does not say what choosing "Approve"
	// actually does, so this paragraph follows it, separated by a blank
	// line.
	gateApproveExplains = "Approve asks the planning agent whether any question is still open. If none is, " +
		"Zing seals this scenario set and moves the ticket to building. This cannot be undone. Until you " +
		"approve, writing in any settled question reopens it and withdraws this gate. Findings at or below " +
		"the quality floor were already fixed automatically; only findings above the floor are shown here."

	// gateApproveExplainsLoopsExhausted is gateApproveExplains' own
	// counterpart for issue #48's cap-reached gate (design section 5.1 step
	// 7, 6.6): maybeResumeFloorFindings posts this instead of escalating
	// loops_exhausted when every finding still in the stored artifact is at
	// or below the floor, so the owner, not the loop, decides. Unlike the
	// clean-review gate, these findings were never fixed automatically --
	// the cap stopped the resume loop before another cycle could try.
	gateApproveExplainsLoopsExhausted = "Approve asks the planning agent whether any question is still open. If none is, " +
		"Zing seals this scenario set and moves the ticket to building. This cannot be undone. Until you " +
		"approve, writing in any settled question reopens it and withdraws this gate. Plan review reached " +
		"machine.toml's planreview max_loops with only at-or-below-floor findings left; they were not fixed " +
		"automatically and are shown below for your decision."

	// planAcceptAtCapOptionText is option d's text on plan review's
	// cap_loops loops_exhausted question.
	planAcceptAtCapOptionText = "Accept the plan and go to the gate"

	// gateApproveExplainsOwnerChose is the gate text acceptPlanAtCap posts
	// when the owner picked d on the cap_loops escalation: unlike
	// gateApproveExplainsLoopsExhausted, findings above the floor remain.
	gateApproveExplainsOwnerChose = "Approve asks the planning agent whether any question is still open. If none is, " +
		"Zing seals this scenario set and moves the ticket to building. This cannot be undone. Until you " +
		"approve, writing in any settled question reopens it and withdraws this gate. Plan review reached " +
		"machine.toml's planreview max_loops with findings above the quality floor still open. The findings " +
		"below were not fixed, and you chose to see this gate anyway."

	// The three artifact types a stored ready cohort writes (design section
	// 6.5, 4.5): internal/store/schemas/artifacts/{plan,claims,scenario}.json
	// are their validated shapes, and internal/store/examples/artifacts holds
	// one worked example of each, the same JSON shape json.Marshal(resp.Plan),
	// json.Marshal(resp.Claims), and json.Marshal(one Scenario) already
	// produce.
	artifactTypePlan       = "plan"
	artifactTypeClaims     = "claims"
	artifactTypeScenario   = "scenario"
	artifactTypePlanreview = "planreview"

	// validationErrorsPendingPrefix and validationErrorsDeliveredPrefix are
	// the "update" marker bodies section 5.3 pairs through LiveMarker: a
	// failed ready check writes "<pending> run <rid>\n<errors, one per
	// line>"; entry step 5 resumes with those errors and writes "<delivered>
	// run <rid>" in the same commit.
	validationErrorsPendingPrefix   = "validation errors pending"
	validationErrorsDeliveredPrefix = "validation errors delivered"

	// minReadyScenarios and maxReadyScenarios are design section 6.5's
	// scenario-cohort bounds (the same jsonschema minItems=2, maxItems=30
	// response.ReadyResponse.Scenarios already carries for a document that
	// passed the runtime's own Layer 1 pass; checkScenarioShape restates it
	// because a ready entry point is this file's only defense once a caller
	// hands it a Response value that skipped that pass).
	minReadyScenarios = 2
	maxReadyScenarios = 30
)

// planningHandler runs the real planning state (design section 5.1): the
// entry decision reads the ticket's answered rounds and its latest planning
// session, and routes to classify (6.1), the planning first turn (6.2), or a
// resume (6.4). It replaces the skeleton's planningHandler of the same name.
type planningHandler struct{}

func (h planningHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		return h.enterFromRound(ctx, t, d, rounds[0])
	}

	if t.Kind == nil {
		return runClassify(ctx, t, d, nil, nil)
	}

	maxResumes := d.Machine.Jobs[jobPlanningName].MaxResumes
	sess, state, err := d.Store.LatestSession(ctx, t.ID, jobPlanningName, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: latest session: %w", err)
	}
	slog.Debug("planning entry decision", "ticket_id", t.ID, "branch", branchName(t.Branch), "session_state", sessionStateName(state))

	switch state {
	case store.SessionNone, store.SessionIdless:
		return runPlanningFirst(ctx, t, d, nil, nil)
	case store.SessionExhausted:
		// A ready produced on the final permitted resume stores a valid
		// cohort and leaves this session exhausted (readyCommit carries no
		// Next): review it before ever escalating the cap, or the owner sees
		// a misleading resumes_exhausted with a reviewable cohort sitting
		// unreviewed underneath it. The floor loop (maybeResumeFloorFindings)
		// stays unreachable here on purpose -- it needs a planning resume,
		// which an exhausted session cannot spend.
		if commit, handled, reviewErr := maybeReviewTick(ctx, t, d); handled {
			return commit, reviewErr
		}
		// D5 (design section 7.4): an exhausted session whose newest run was
		// cut short (store.Run.Interrupted) still resumes, free and
		// uncapped, rather than escalate resumes_exhausted -- the next,
		// non-interrupted resume is still blocked by the cap (section 11's
		// own edge case). planningInterruptedFallback tries the
		// stalled-invalid-retry and validation-errors branches first, so an
		// interrupted n==1 retry or a pending validation-errors marker still
		// carries its own text alongside the interrupted input, rather than
		// losing it to a plain interrupted-only resume.
		if commit, handled, fallbackErr := planningInterruptedFallback(ctx, t, d, sess); handled {
			return commit, fallbackErr
		}
		has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
		if hasErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: has escalation: %w", hasErr)
		}
		if has {
			return store.HandlerCommit{}, ErrNoAction
		}
		return capResumesEscalation(t, d, jobPlanningName, sess.ID), nil
	case store.SessionOpen:
		// Design section 7.5 bug 3: before the ordinary D14 n==1 check,
		// handle a newest run that is itself a stalled invalid retry -- one
		// ConsecutiveInvalidOutputs' own walk stops on, uncounted, because
		// it never got a response of its own. Left to the n==1 check alone,
		// this run's own missing marker makes n come back 0 with nothing
		// else pending, stalling on ErrNoAction forever (the bug).
		if commit, handled, stallErr := maybeResumeStalledInvalidRetry(ctx, t, d, sess); handled {
			return commit, stallErr
		}

		n, reason, invErr := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobPlanningName, &sess.ID)
		if invErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: consecutive invalid outputs: %w", invErr)
		}
		if n == 1 {
			return runPlanningResume(ctx, t, d, sess, nil, []prompt.NamedInput{prompt.Invalid(invalidRetryText(reason))}, n, true)
		}
		if commit, handled, resumeErr := maybeResumeValidationErrors(ctx, t, d, sess); handled {
			return commit, resumeErr
		}

		// Steps 4 and 5 (D31, design section 22.4): a conversation-only
		// resume delivers every owner message the ticket has not yet seen,
		// and a repair commit recovers a ticket D31 left unwaiting with an
		// open thread (rather than ErrNoAction forever), both ahead of the
		// review tick and the floor loop, which now also require every
		// thread settled (maybeReviewTick's own guard).
		conv, convErr := d.Store.PlanningConversation(ctx, t.ID)
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: planning conversation: %w", convErr)
		}
		if len(conv.Undelivered()) > 0 {
			charge, chargeErr := d.Store.SessionInterrupted(ctx, t.ID, sess.ID)
			if chargeErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: planning: session interrupted: %w", chargeErr)
			}
			// runPlanningResume itself ANDs charge against resumeCharge's own
			// bump (design D5, section 7.4) and appends the interrupted
			// input when the session's newest run carries it, so this call
			// site's own charge computation (SessionInterrupted, D31's
			// unrelated resume-charging table) stays exactly as it was.
			return runPlanningResume(ctx, t, d, sess, nil, nil, 0, charge)
		}
		if len(conv.Unsettled()) > 0 && t.WaitingOn == nil {
			return repairPlanningWaitCommit(t, d), nil
		}
		if len(conv.Unsettled()) > 0 {
			// Already correctly waiting on the owner (the ordinary case: a
			// thread is open and nothing is left to repair) -- ErrNoAction,
			// not a second repair commit every tick.
			return store.HandlerCommit{}, ErrNoAction
		}

		if commit, handled, resumeErr := maybeReviewTick(ctx, t, d); handled {
			return commit, resumeErr
		}
		if commit, handled, resumeErr := maybeResumeFloorFindings(ctx, t, d, sess); handled {
			return commit, resumeErr
		}
		// D5's own catch-all (design section 7.4's "planning, first turn"
		// row): nothing above claimed this tick, but the session's newest
		// run was cut short -- a first turn interrupted before it ever
		// answered (the external id was already written at start, 7.1, so
		// the session is open rather than idless), or any other resume
		// whose only story is "it was interrupted". A plain reconciled run
		// (Interrupted false) with nothing else pending keeps today's
		// ErrNoAction.
		if commit, handled, fallbackErr := planningInterruptedFallback(ctx, t, d, sess); handled {
			return commit, fallbackErr
		}
		return store.HandlerCommit{}, ErrNoAction
	}
	return store.HandlerCommit{}, ErrNoAction
}

// repairPlanningWaitCommit is entry step 5 (design section 22.4): a ticket
// whose planning session is open, has no undelivered owner message, and
// still carries an unsettled thread, but is not itself waiting (D31's own
// fence, design section 22.3 step 3, can leave it this way), gets
// waiting_on set to "questions" alone -- no runtime call -- rather than
// return ErrNoAction forever.
func repairPlanningWaitCommit(t store.Ticket, d Deps) store.HandlerCommit {
	c := baseCommit(t, d)
	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	slog.Info("planning waits on the owner", "ticket_id", t.ID)
	return c
}

// enterFromRound is section 5.1 step 1: round is the newest answered round
// (AnsweredRounds' first element). Its newest question's kind and its
// parent/job fields pick the branch (a) through (e).
func (h planningHandler) enterFromRound(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	newest := round.Questions[len(round.Questions)-1]
	var qp response.QuestionPayload
	if err := json.Unmarshal(newest.Payload, &qp); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: unmarshal round question %d payload: %w", newest.ID, err)
	}

	switch {
	case qp.Kind == response.QuestionKindGate:
		return h.enterFromGateRound(ctx, t, d, round)
	case qp.Kind == response.QuestionKindSplit:
		return h.enterFromSplitRound(ctx, t, d, round)
	case newest.ParentID != nil:
		// The newest question's own parent id, not round.ParentID: a
		// run-caused escalation's linked question carries the same run_id
		// as the escalation itself (escalateTx's own RunID: ec.RunID), so
		// AnsweredRounds groups it by run, the same as an ordinary
		// classify/planning/planreview question batch, and round.ParentID
		// (only ever filled for the no-run grouping, design section 4.5)
		// stays nil. The per-message parent id escalateTx always sets,
		// whichever grouping produced this round, is what actually tells an
		// escalation-linked question apart from an ordinary one.
		return h.enterFromEscalationRound(ctx, t, d, round, *newest.ParentID)
	// round.Job == jobPlanningName is unreachable (D31, design section
	// 22.4): AnsweredRounds no longer returns planning questions at all
	// (conversation_reads.go's planningQuestionsSQL excludes them), so no
	// round this switch ever sees can be keyed by a planning run any more.
	// enterFromPlanningRound is gone with it; answerInputsForRound stays,
	// still shared by building.go's own answered-round resume.
	case round.Job == jobClassifyName:
		rendered, err := renderRoundAnswers(round)
		if err != nil {
			return store.HandlerCommit{}, err
		}
		return runClassify(ctx, t, d, []prompt.NamedInput{prompt.Answers(rendered)}, questionIDs(round))
	case round.Job == jobPlanreviewName:
		rendered, err := renderRoundAnswers(round)
		if err != nil {
			return store.HandlerCommit{}, err
		}
		return runPlanReview(ctx, t, d, []prompt.NamedInput{prompt.Answers(rendered)}, questionIDs(round))
	default:
		return store.HandlerCommit{}, ErrNoAction
	}
}

// ---- 6.1 classify ---------------------------------------------------------

// runClassify is plan section 6.1: the D14 pre-check, the classify.md
// prompt fenced around the ticket plus extra and, on a second consecutive
// invalid output, the raw invalid reason, and routing runJob's result
// through the section 6.8 outcome table. extra carries the round's rendered
// answers when a round is being resolved (entry steps 1(d) and the
// classify-fresh call from the top-level Kind==nil branch carries none);
// resolveIDs, when non-empty, is that round's question ids.
func runClassify(ctx context.Context, t store.Ticket, d Deps, extra []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
	n, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobClassifyName, nil)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: classify: consecutive invalid outputs: %w", err)
	}

	inputs := append([]prompt.NamedInput{}, extra...)
	if n == 1 {
		inputs = append(inputs, prompt.Invalid(invalidRetryText(reason)))
	}

	jobCfg := d.Machine.Jobs[jobClassifyName]
	promptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: classify: %w", err)
	}
	schemas, err := renderSchemas(response.JobClassify, response.OutcomeBug, response.OutcomeFeature)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: classify: %w", err)
	}

	ticketText, err := specFor(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: classify: %w", err)
	}
	in := prompt.ForClassify(promptText, ticketText, inputs)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobClassifyName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobClassify, Prompt: assembled}
	return runAndRoute(ctx, d, t, jobClassifyName, su, req, n, freshSessionRecord, resolveIDs, response.EscalationOriginClassify,
		func(rr runResult) (store.HandlerCommit, error) {
			return classifySuccessCommit(t, d, rr, freshSessionRecord(rr), resolveIDs)
		}, nil, 0)
}

// classifySuccessCommit routes a classify run's parsed response (design
// section 6.8): bug/feature sets the ticket's kind and stays; the two
// universal outcomes (question, error) are shared with planning's own
// success routing (questionOutcomeCommit, errorOutcomeCommit).
func classifySuccessCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.ClassifyResponse:
		outcome := resp.Header().Outcome
		c := baseCommit(t, d)
		c.Runs = terminalRuns(rr, string(outcome))
		c.Session = sessionCommit
		kind := string(outcome)
		c.SetKind = &kind
		c.ResolveQuestions = resolveIDs
		return c, nil
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp.Questions, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginClassify), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: classify: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// ---- 6.2 planning first turn, 6.4 resume ----------------------------------

// runPlanningFirst is plan section 6.2: requires t.Kind, picks the prompt
// file by kind, layers the job's style files, fences the ticket plus extra,
// and routes runJob's result through planningSuccessCommit. extra and
// resolveIDs carry a resolved round's rendered answers exactly as
// runClassify's do. Every call also reads PlanningConversation and, when
// the ticket already has a planning thread, appends the fresh session's own
// transcript (design section 22.4, 22.6): a brand new ticket's very first
// call carries no threads yet and so appends nothing.
func runPlanningFirst(ctx context.Context, t store.Ticket, d Deps, extra []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
	if t.Kind == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: ticket %d has no kind", t.ID)
	}
	jobCfg := d.Machine.Jobs[jobPlanningName]

	var promptPath string
	switch *t.Kind {
	case string(response.OutcomeBug):
		promptPath = jobCfg.Prompt.Bug
	case string(response.OutcomeFeature):
		promptPath = jobCfg.Prompt.Feature
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: ticket %d has unknown kind %q", t.ID, *t.Kind)
	}

	promptText, err := readAsset(promptPath)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: %w", err)
	}
	styles := make([]string, len(jobCfg.Style))
	for i, p := range jobCfg.Style {
		s, styleErr := readAsset(p)
		if styleErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: %w", styleErr)
		}
		styles[i] = s
	}

	schemas, err := planningSchemas()
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: %w", err)
	}

	conv, err := d.Store.PlanningConversation(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: planning conversation: %w", err)
	}
	convExtra, throughBatch := conversationFreshInput(conv)

	ticketText, err := specFor(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: %w", err)
	}
	in, err := prompt.ForPlanningFirst(promptText, styles, d.Machine.Jobs[jobBuildName].TimeoutMinutes, ticketText,
		append(append([]prompt.NamedInput{}, extra...), convExtra...))
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: %w", err)
	}
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobPlanningName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobPlanning, Prompt: assembled}
	return runAndRoute(ctx, d, t, jobPlanningName, su, req, 0, freshSessionRecord, resolveIDs, response.EscalationOriginPlanningFirst,
		func(rr runResult) (store.HandlerCommit, error) {
			return planningSuccessCommit(ctx, t, d, rr, freshSessionRecord(rr), resolveIDs, response.EscalationOriginPlanningFirst, conv, throughBatch)
		}, nil, throughBatch)
}

// runPlanningResume is plan section 6.4 (and 6.3's resume-input shapes):
// requires an open session's external id, assembles the fixed ResumeHeader
// plus extra (either one prompt.Answer per answered question, or a single
// prompt.Invalid for the D14 retry step 4 drives), and routes runJob's
// result through planningSuccessCommit. priorInvalid is the consecutive
// invalid-output count the caller already read for this session (0 for a
// round-based resume, which carries no D14 check of its own; the actual
// count for step 4's automatic retry), so a second consecutive invalid
// output escalates response_invalid in the same commit that terminalizes it.
// charge becomes the resume's own SessionUpsert.BumpResumes (design section
// 22.4's resume-charging table): false only for an owner-delivery resume
// that followed no other, agent-driven reason. Every call also reads
// PlanningConversation and, when Undelivered() is non-empty, appends the
// resume block carrying every owner message this turn has not yet seen
// (design section 22.4, 22.5). resumeCharge (job.go, design D5, section
// 7.4), read from sess's own newest run, ANDs its own bump into charge
// (an interrupted newest run makes every caller's resume free, whatever
// charge it computed on its own) and, when that newest run is interrupted,
// appends the interrupted input to extra -- so every one of this
// function's five call sites (the D14 retry, the owner-delivery resume,
// the validation-errors resume, the floor-findings resume, and the
// answered-round resume) gets both halves of D5 for free.
func runPlanningResume(ctx context.Context, t store.Ticket, d Deps, sess store.Session, resolveIDs []int64, extra []prompt.NamedInput, priorInvalid int, charge bool) (store.HandlerCommit, error) {
	if sess.ExternalID == nil || *sess.ExternalID == "" {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: session %d has no external id", sess.ID)
	}

	newestRun, foundRun, err := d.Store.SessionNewestRun(ctx, sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: newest run: %w", err)
	}
	if foundRun {
		bump, _ := resumeCharge(newestRun)
		charge = charge && bump
		if newestRun.Interrupted {
			extra = append(append([]prompt.NamedInput{}, extra...), prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false})
		}
	}

	schemas, err := planningSchemas()
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: %w", err)
	}

	conv, err := d.Store.PlanningConversation(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: planning conversation: %w", err)
	}
	convExtra, throughBatch := conversationResumeInput(conv)

	in := prompt.ForPlanningResume(append(append([]prompt.NamedInput{}, extra...), convExtra...))
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: charge}
	req := runtime.RunRequest{Job: response.JobPlanning, SessionID: *sess.ExternalID, Prompt: assembled}
	sessionRecord := func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, rr) }
	return runAndRoute(ctx, d, t, jobPlanningName, su, req, priorInvalid, sessionRecord, resolveIDs, response.EscalationOriginPlanningResume,
		func(rr runResult) (store.HandlerCommit, error) {
			return planningSuccessCommit(ctx, t, d, rr, sessionRecord(rr), resolveIDs, response.EscalationOriginPlanningResume, conv, throughBatch)
		}, nil, throughBatch)
}

// planningInterruptedFallback is design D5's own catch-all (section 7.4's
// "planning, first turn" row, which also covers any other mid-planning
// resume no more specific branch above claims): sess's newest run carries
// store.Run.Interrupted, so it resumes free with the interrupted input
// (runPlanningResume adds that input itself, from the same newest run).
// Before falling back to that plain resume, it tries the same two specific
// branches the open-session case runs ahead of its own plain resume
// (maybeResumeStalledInvalidRetry, then maybeResumeValidationErrors): an
// exhausted session's interrupted newest run can just as well be the n==1
// invalid-output retry (section 7.5 bug 3) or carry a pending
// validation-errors marker, and either one's own text would otherwise be
// lost -- resumed with only the interrupted input and nothing else. Both
// calls still resume free: runPlanningResume's own resumeCharge handling
// (design D5) reads this same newest run and ANDs its bump into whatever
// charge they pass. handled is false, with no error, when the session has
// no run yet or its newest run was not interrupted -- the caller still
// falls through to ErrNoAction for a plain reconciled run with nothing else
// pending, exactly as today.
func planningInterruptedFallback(ctx context.Context, t store.Ticket, d Deps, sess store.Session) (store.HandlerCommit, bool, error) {
	newestRun, found, err := d.Store.SessionNewestRun(ctx, sess.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: interrupted fallback: newest run: %w", err)
	}
	if !found || !newestRun.Interrupted {
		return store.HandlerCommit{}, false, nil
	}
	if commit, handled, stallErr := maybeResumeStalledInvalidRetry(ctx, t, d, sess); handled {
		return commit, true, stallErr
	}
	if commit, handled, resumeErr := maybeResumeValidationErrors(ctx, t, d, sess); handled {
		return commit, true, resumeErr
	}
	commit, err := runPlanningResume(ctx, t, d, sess, nil, nil, 0, true)
	return commit, true, err
}

// maybeResumeStalledInvalidRetry is design section 7.5 bug 3 (the planning
// invalid-retry stall): sess's newest run ended "error" but carries no
// "response invalid run <rid>" marker of its own -- exactly the shape of a
// D14 retry turn (runPlanningResume's own n==1 branch) that was itself cut
// short before it ever answered. Left alone, ConsecutiveInvalidOutputs'
// walk (internal/store/planning_reads.go) stops on that run, uncounted,
// because a response-less run cannot carry the marker; n comes back 0, and
// with nothing else pending the handler stalls on ErrNoAction forever.
// This runs first and rebuilds the retry text from the run that actually
// triggered the chain -- sess's own run immediately before the newest one
// -- so the resume carries the same invalid-retry text the stalled turn
// itself was launched with. runPlanningResume's own resumeCharge handling
// (design D5) then decides, from this same newest run, whether the resume
// is free (genuinely interrupted) or charged and cap-gated (a plainer
// reconcile, section 7.5 bug 3's "reconciled" row): either way it also
// appends the interrupted input when the newest run is interrupted, next
// to the invalid-retry text this function supplies. handled is false when
// the newest run is not an unanswered error (the ordinary D14 n==1 check
// should run instead) or it has no predecessor carrying its own invalid
// marker (not a stalled retry at all): on an open session the ordinary
// D14 n==1 check runs next; on an exhausted session,
// planningInterruptedFallback (which calls this function first, before
// its own plain interrupted resume) tries maybeResumeValidationErrors
// next instead.
func maybeResumeStalledInvalidRetry(ctx context.Context, t store.Ticket, d Deps, sess store.Session) (store.HandlerCommit, bool, error) {
	newestRun, found, err := d.Store.SessionNewestRun(ctx, sess.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: stalled invalid retry: newest run: %w", err)
	}
	if !found || newestRun.Outcome == nil || *newestRun.Outcome != string(response.OutcomeError) {
		return store.HandlerCommit{}, false, nil
	}
	_, hasOwnMarker, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("response invalid run %d", newestRun.ID))
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: stalled invalid retry: own marker: %w", err)
	}
	if hasOwnMarker {
		return store.HandlerCommit{}, false, nil
	}
	reason, hadPriorRetry, err := priorInvalidReason(ctx, d, t.ID, sess.ID, newestRun.ID)
	if err != nil {
		return store.HandlerCommit{}, false, err
	}
	if !hadPriorRetry {
		return store.HandlerCommit{}, false, nil
	}
	// priorInvalid is 1, not 0 (PR review fix F1): this resume found the
	// preceding invalid marker, so it is itself the D14 retry turn. If the
	// recovered retry is invalid again, invalidOutputCommit's own
	// "priorInvalid == 1" check must see that and escalate response_invalid
	// (D14's two-strike rule), rather than silently resetting the chain and
	// retrying forever.
	commit, err := runPlanningResume(ctx, t, d, sess, nil, []prompt.NamedInput{prompt.Invalid(invalidRetryText(reason))}, 1, true)
	return commit, true, err
}

// priorInvalidReason looks up the "response invalid run <id>" marker (D14,
// design section 5.4) of sessionID's own newest run strictly before
// beforeRunID that is not itself part of the same interrupted-resume chain
// (job.go's priorNonInterruptedRun, PR review fix F2) -- the run that
// triggered the retry beforeRunID itself carried out. Walking past
// interrupted runs, not just to the immediately preceding one, matters
// when the stalled retry turn is itself interrupted more than once in a
// row: its own immediate predecessor is then another interrupted-chain
// member with no marker of its own, and landing there instead of on the
// run that actually carries the marker would silently drop the original
// invalid-retry text. found is false when beforeRunID is this session's
// very first run, or every run before it is itself an interrupted-chain
// member with no marker (an ordinary interrupted first turn, not a
// stalled retry).
func priorInvalidReason(ctx context.Context, d Deps, ticketID, sessionID, beforeRunID int64) (reason string, found bool, err error) {
	prior, found, err := priorNonInterruptedRun(ctx, d, ticketID, sessionID, beforeRunID)
	if err != nil {
		return "", false, fmt.Errorf("job: planning: prior invalid reason: %w", err)
	}
	if !found {
		return "", false, nil
	}
	row, ok, err := d.Store.Marker(ctx, ticketID, fmt.Sprintf("response invalid run %d", prior.ID))
	if err != nil {
		return "", false, fmt.Errorf("job: planning: prior invalid reason: marker: %w", err)
	}
	if !ok {
		return "", false, nil
	}
	_, reason, _ = strings.Cut(row.Body, "\n")
	return reason, true, nil
}

// planningSuccessCommit routes a planning run's parsed response (design
// section 6.8, 22.2, 22.4), shared by the first turn and the resume:
// questions and error are the same universal handling classify uses;
// replies is D31's own thread-only outcome; ready is section 6.5's real
// cohort check and store; children is childrenCommit, which stores the
// children artifact and posts the split question (split.go);
// nothing_to_do is nothingToDoCommit's own accept-or-escalate check
// (task 8). Every outcome but error can carry <replies> (design section
// 22.2): when the response does, checkConversation runs first against conv
// (the same PlanningConversation its caller already read to build this
// turn's prompt), and a failure writes the validation-errors-pending marker
// in place of whatever the outcome's own commit would have been, posting
// nothing (design section 22.2's own "a failure terminalizes the run,
// posts nothing"); otherwise the outcome's own commit gains the reply
// messages, the settles, and the delivered marker (conversationEffects),
// never touching Waiting beyond what the outcome's own commit already set
// (the fence inside CommitHandlerResult's applyConversationTx is what can
// still clear it, design section 22.3 step 3).
func planningSuccessCommit(ctx context.Context, t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin, conv store.PlanningConversation, throughBatch int64) (store.HandlerCommit, error) {
	resp := rr.Res.Response
	rl, hasReplies := resp.(replyLister)
	if hasReplies {
		if errs := checkConversation(resp.Header().Outcome, rl.ReplyList(), buildThreadState(conv), false); len(errs) > 0 {
			return conversationValidationErrorCommit(t, d, rr, sessionCommit, resolveIDs, errs), nil
		}
	}

	var c store.HandlerCommit
	var err error
	switch r := resp.(type) {
	case *response.PlanningQuestionsResponse:
		c, err = questionOutcomeCommit(t, d, rr, r.Questions, sessionCommit, resolveIDs)
	case *response.RepliesResponse:
		c, err = repliesOutcomeCommit(t, d, rr, sessionCommit, resolveIDs)
	case *response.ReadyResponse:
		c, err = readyCommit(ctx, t, d, rr, r, sessionCommit, resolveIDs)
	case *response.ChildrenResponse:
		c, err = childrenCommit(t, d, rr, r, sessionCommit, resolveIDs)
	case *response.NothingToDoResponse:
		c, err = nothingToDoCommit(t, d, rr, r, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, r, sessionCommit, resolveIDs, origin), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: planning: outcome %s not handled", resp.Header().Outcome)
	}
	if err != nil || !hasReplies {
		return c, err
	}

	msgs, cc := conversationEffects(t.ID, rr.Reserved.RunID, throughBatch, rl.ReplyList(), conv)
	c.Messages = append(c.Messages, msgs...)
	c.Conversation = &cc
	return c, nil
}

// conversationValidationErrorCommit is checkConversation's own failure
// commit (design section 22.2): the run terminalizes with its own outcome,
// resolveIDs still resolve (matching readyCommit's own content-validation
// failure), and the ticket stays in planning, not waiting, with the
// existing "validation errors pending" marker naming every path error --
// the same mechanism and the same entry-step-3 resume (planningHandler.Run)
// that already redelivers a failed ready response's own errors.
func conversationValidationErrorCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, errs []*response.PathError) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(rr.Res.Response.Header().Outcome))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("%s run %d\n%s", validationErrorsPendingPrefix, rr.Reserved.RunID, formatReadyErrors(errs)),
	}}
	return c
}

// repliesOutcomeCommit is the replies outcome's own commit (design section
// 22.4's table): it carries no new question, so terminalRuns stores "runs
// outcome question" exactly as planning's own questions/question outcome
// does (section 22.2: runs.outcome has no "replies" value and never will,
// D16's own single-transaction constraint), and Waiting is always
// "questions" -- the 22.2 rule already guarantees at least one planning
// question stays open after a valid replies response. The reply messages,
// the settles, and the delivered marker are attached by
// planningSuccessCommit's own caller, like every other outcome's.
func repliesOutcomeCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) { //nolint:unparam // error is always nil today; the signature matches every other outcome builder planningSuccessCommit's switch calls (questionOutcomeCommit, readyCommit, nothingToDoCommit), which can fail
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeQuestion))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c, nil
}

// errNothingToDoClaimNotFalse is nothingToDoCommit's own defensive sentinel
// (design section 6.8, F022): response.Validate's CheckNothingToDoClaims
// already rejects any nothing_to_do response carrying a code claim that is
// not verdict=false, as an InvalidOutputError, before a handler ever sees
// it, so this function should never observe one. It checks anyway, because
// a scripted test runtime (standing in for a compromised or buggy agent
// process) can hand a Response value straight to the handler with no
// validation pass in between, and a silent accept there would be worse than
// a loud one here.
var errNothingToDoClaimNotFalse = errors.New("nothing_to_do code claim not verified false")

// nothingToDoCommit is design section 6.8's nothing_to_do row (task 8,
// tightened by F022, option B: the validator owns the rule, not this
// handler): resp.Claims naming at least one code claim accepts
// automatically, since CheckNothingToDoClaims already guarantees every code
// claim it names is false by the time a real runtime's response reaches
// here; naming no code claim at all cannot prove there is nothing to build,
// and escalates instead. Acceptance terminalizes the run, transitions the
// ticket straight to done, and sets TrackerEffect, Kind
// store.TrackerEffectKindNothingToDo, so the dispatcher posts
// tracker.NothingToDoComment once, through the zing:nothing marker, and
// then closes the issue, after the commit lands (design D12); the
// escalation carries RunID and SessionID (a run did cause this) and leaves
// the ticket waiting on the owner's retry/planning/abandon choice, exactly
// like every other section 6.7 escalation.
func nothingToDoCommit(t store.Ticket, d Deps, rr runResult, resp *response.NothingToDoResponse, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	codeClaims := 0
	for _, cl := range resp.Claims {
		if cl.Kind != response.ClaimKindCode {
			continue
		}
		codeClaims++
		if cl.Verdict != response.ClaimVerdictFalse {
			return store.HandlerCommit{}, fmt.Errorf("nothing_to_do reached the handler with a non-false code claim: %w", errNothingToDoClaimNotFalse)
		}
	}

	if codeClaims > 0 {
		c := baseCommit(t, d)
		c.Runs = terminalRuns(rr, string(response.OutcomeNothingToDo))
		c.Session = sessionCommit
		c.ResolveQuestions = resolveIDs
		c.Next = stateDone
		c.Reason = reasonNothingToDo
		c.TrackerEffect = &store.TrackerEffect{Kind: store.TrackerEffectKindNothingToDo, Ref: t.TrackerRef, Notes: resp.Notes}
		return c, nil
	}

	c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
		string(response.EscalationCodeNothingToDoWithTrueClaims), nothingToDoNoCodeClaimsWhat, nothingToDoWhy, "", response.EscalationOriginNothingToDoClaims)
	c.Runs = terminalRuns(rr, string(response.OutcomeNothingToDo))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	return c, nil
}

// ---- 6.5 ready: the cohort check and store --------------------------------

// readyCommit is plan section 6.5's "On ready": checks, in order, the code
// claims against the project's real filesystem (through os.OpenRoot, so a
// symlink resolving outside the checkout cannot satisfy one, design D19),
// the scenario cohort's own shape, and the plan checker (kind-aware this
// time; the runtime's own Layer 1 pass, design D14, never learns the
// ticket's kind, since parseFinalMessage has no ticket to read one from).
// Failure terminalizes the run (outcome "ready") and writes the "validation
// errors pending" marker (design section 5.3), leaving the ticket in
// planning, not waiting, storing nothing. Success stores the plan, claims,
// and one artifact per scenario, all under the reserved run (the cohort
// key), terminalizes the run, and leaves the ticket in planning, not
// waiting: the review tick (entry step 6, maybeReviewTick) picks up the new
// cohort on the next tick. Task 6's shortcut straight to building is gone
// (task 7b); the pinned
// TestPlanningHandler_Resume_ReadyOutcomeStoresCohortAndStaysInPlanning
// proves it.
func readyCommit(ctx context.Context, t store.Ticket, d Deps, rr runResult, resp *response.ReadyResponse, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeReady))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs

	proj, err := d.Store.ProjectForTicket(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: project for ticket %d: %w", t.ID, err)
	}
	root, err := os.OpenRoot(proj.LocalPath)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: open project root %s: %w", proj.LocalPath, err)
	}
	defer root.Close()

	errs, err := checkReady(t, resp, root.FS())
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: %w", err)
	}
	required, err := dispositionsRequired(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: %w", err)
	}
	planXML, err := planXMLFor(resp.Plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: %w", err)
	}
	dispositionErrs := checkDispositions(required, resp.Plan.Dispositions, []byte(planXML))
	errs = append(errs, dispositionErrs...)
	if len(errs) > 0 {
		slog.Info("ready plan rejected", "ticket_id", t.ID, "run_id", rr.Reserved.RunID,
			"error_count", len(errs), "disposition_error_count", len(dispositionErrs), "required_count", len(required))
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("%s run %d\n%s", validationErrorsPendingPrefix, rr.Reserved.RunID, formatReadyErrors(errs)),
		}}
		return c, nil
	}

	artifacts, err := readyArtifacts(resp, rr.Reserved.RunID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: %w", err)
	}
	c.Artifacts = artifacts

	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: current cohort: %w", err)
	}
	planVersion := 1
	if ok {
		planVersion = cohort.PlanVersion + 1
	}
	slog.Info("artifacts stored", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "plan_version", planVersion, "scenario_count", len(resp.Scenarios))

	questions, err := disputeQuestionMessages(t.ID, required, resp.Plan.Dispositions)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: ready: %w", err)
	}
	if len(questions) > 0 {
		c.Messages = questions
		c.AttachRunToMsgs = true
		waiting := waitingFlagQuestions
		c.Waiting = &waiting
		slog.Info("disputed findings posted", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "count", len(questions))
	}
	return c, nil
}

// checkReady runs section 6.5's three "on ready" checks, in order, and
// returns every failure by element path the way `zing validate` reports
// them (response.PathError's own "path: msg" shape): the code claims, the
// scenario cohort's shape, then the plan checker. The only real error this
// returns is LoadChecklists failing to read its own embedded, trust-root
// checklists.toml, which would mean that file itself is broken.
func checkReady(t store.Ticket, resp *response.ReadyResponse, fsys fs.FS) ([]*response.PathError, error) {
	var errs []*response.PathError
	errs = append(errs, response.CheckCodeClaims(resp.Claims, fsys)...)
	errs = append(errs, checkScenarioShape(resp.Scenarios)...)

	lists, err := response.LoadChecklists()
	if err != nil {
		return nil, fmt.Errorf("load checklists: %w", err)
	}
	bug := t.Kind != nil && *t.Kind == string(response.OutcomeBug)
	errs = append(errs, response.CheckPlan(resp.Plan, resp.Scenarios, bug, lists, planPresenceSet(resp.Scenarios))...)
	return errs, nil
}

// checkScenarioShape is this file's own re-check of a ready response's
// scenario count (2 to 30) and each scenario's non-empty then (design
// section 6.5): response.ReadyResponse.Scenarios and Scenario.Then already
// carry jsonschema minItems=2, maxItems=30, and minLength=1, so a document
// that passed the runtime's own Layer 1 pass (design D14) already satisfies
// this; it defends the ready entry point itself against a Response value
// that reached here some other way (a test's scriptedRuntime, standing in
// for a compromised or buggy agent process, is the only caller that can).
// It also refuses a check that starts a nested sandbox (sandbox-exec or
// internal/sandbox's own probes), which skips and exits 0 inside the
// seatbelt the judge and CHECK already run under (#78). The only exemption
// is an internal/sandbox probe whose own expected skip is both affirmatively
// named by the scenario's then (not negated, as in "no longer skips") and
// proven by a grep for the "--- SKIP:" line in the check (#80, #129 s5); a
// bare sandbox-exec invocation stays refused even then, since the fix only
// needs to let a probe's own expected skip through, not every
// nested-sandbox check. It also refuses a then that expects a skip when its
// check doesn't grep that same line, since a bare go test exits 0 whether
// or not the test skipped; that second rule still fires on a negated skip
// then, since only the host-sandbox exemption's affirmative check is
// narrowed. A scenario of kind host runs on the owner's machine at judging,
// outside any sandbox (#137), so it is exempt from the /tmp and
// nested-sandbox refusals below; it still needs a non-blank check, and it
// still obeys the expected-skip rule. It also refuses rm -f and rm -rf in
// every kind, since Codex refuses them (#94).
func checkScenarioShape(scenarios []response.Scenario) []*response.PathError {
	var errs []*response.PathError
	if n := len(scenarios); n < minReadyScenarios || n > maxReadyScenarios {
		errs = append(errs, &response.PathError{
			Path: "scenarios/scenario",
			Msg:  fmt.Sprintf("need %d to %d scenarios, have %d", minReadyScenarios, maxReadyScenarios, n),
		})
	}
	for i, sc := range scenarios {
		errs = append(errs, checkScenarioRules(i, sc)...)
	}
	return errs
}

// checkScenarioRules is checkScenarioShape's own per-scenario half (#57):
// every rule that reads one scenario alone, with no dependency on the rest
// of the cohort, so judgeAmendment (judging.go) can run the same rules
// against a judge-proposed amendment before the owner ever sees it.
func checkScenarioRules(i int, sc response.Scenario) []*response.PathError {
	var errs []*response.PathError
	if strings.TrimSpace(sc.Then) == "" {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/then",
			Msg:  "then must not be empty",
		})
	}
	host := sc.Kind == response.ScenarioKindHost
	hasCheck := strings.TrimSpace(sc.Check) != ""
	// A host check runs on the owner's machine at judging, outside any
	// sandbox, so the /tmp and nested-sandbox refusals below do not
	// apply to it; it must still have a check to run.
	if host && !hasCheck {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  response.HostScenarioNeedsCheck,
		})
	}
	// A host check runs unsandboxed with only the gate's own reading of
	// its rendered text as approval, so a control or Unicode format
	// character (a bidi override, a zero-width character) that could
	// make the rendered command differ from what the shell runs is
	// refused here too.
	if host && hasCheck && response.HostCheckUnsafe(sc.Check) {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  response.HostCheckUnsafeMsg,
		})
	}
	// Zing re-runs every check under the build sandbox, which denies
	// writes to the host /tmp (bug fix: a live judge round failed every
	// check that built into /tmp, though the judge, which rewrote the
	// path, saw them pass).
	if !host && strings.Contains(sc.Check, "/tmp/") {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  "check must not write under /tmp, which the sandbox denies; use \"$TMPDIR\" instead",
		})
	}
	// Codex's command policy refuses rm -f and rm -rf ("rm -f style
	// commands are not permitted"), so a check that clears its state that
	// way fails inside the judge with no cause the owner can see (#94).
	// Every kind, host included.
	if rmForce.MatchString(sc.Check) {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  rmForceCheckMsg,
		})
	}
	// Zing runs every check inside a seatbelt sandbox (the judge's, then
	// CHECK's build sandbox), and seatbelt cannot start sandbox-exec, so
	// the sandbox probes skip and exit 0 (#78). A check that greps the
	// "--- SKIP:" line asserts the skip itself, so it proves the probe
	// skipped rather than hiding behind the sandbox's own skip.
	expectsSkip := skipWord.MatchString(sc.Then)
	assertsSkip := strings.Contains(sc.Check, skipLine)
	startsSeatbelt := strings.Contains(sc.Check, "sandbox-exec")
	runsSandboxProbes := strings.Contains(sc.Check, "internal/sandbox")
	// A then like "the test no longer skips" matches skipWord but
	// expects the opposite result, so the host-sandbox exemption below
	// must not fire for it: a grepped "--- SKIP:" would then prove the
	// wrong thing and reopen #78's hole. expectedSkipCheckMsg below
	// still fires on this same then (Q3, no_longer_skips_flagged); only
	// the exemption's affirmative check is narrowed.
	affirmsSkip := expectsSkip && !negatedSkipWord.MatchString(sc.Then)
	// sandbox-exec is never exempt: the fix only needs to let an
	// internal/sandbox probe's own expected skip through, and starting
	// the seatbelt directly is the exact nested-sandbox invocation #78
	// refused. An internal/sandbox check is exempt only when the check
	// proves the skip (assertsSkip) and the scenario affirmatively
	// expects it (affirmsSkip); a skip the then doesn't name, or
	// negates, still hides the behavior under test.
	exemptSandboxProbe := runsSandboxProbes && assertsSkip && affirmsSkip
	if !host && (startsSeatbelt || (runsSandboxProbes && !exemptSandboxProbe)) {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  hostSandboxCheckMsg,
		})
	}
	// A bare go test exits 0 whether or not the test skipped, so a then
	// that expects a skip needs a check that greps the skip line (#80,
	// #129 s5).
	if hasCheck && expectsSkip && !assertsSkip {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  expectedSkipCheckMsg,
		})
	}
	// Two of the three #86 sealed-check failures: a check that greps a
	// multi-word phrase straight against hard-wrapped prose (the phrase
	// can span the line break the prose wraps at) without joining the
	// lines first, and a check with an unquoted glob (the judge
	// agent's zsh login shell aborts on an unmatched glob, turning a
	// leading "!" into a false pass).
	if proseGrepWithoutJoin(sc.Check) {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  proseGrepCheckMsg,
		})
	}
	if word, ok := unquotedGlob(sc.Check); ok {
		errs = append(errs, &response.PathError{
			Path: "scenarios/" + indexedScenario(i) + "/check",
			Msg:  unquotedGlobCheckMsg(word),
		})
	}
	return errs
}

// proseGrep matches a grep invocation whose flags include F or q, followed
// (before the next pipe, semicolon, or ampersand) by a single- or
// double-quoted argument that contains whitespace, i.e. a phrase of more
// than one word.
var proseGrep = regexp.MustCompile(`\bgrep\b[^|;&]*-[A-Za-z]*[Fq][A-Za-z]*[^|;&]*('[^'|;&]*\s[^'|;&]*'|"[^"|;&]*\s[^"|;&]*")`)

// proseTarget matches a check that names a markdown file or a path under
// prompts/, the hard-wrapped prose a multi-word grep can miss.
var proseTarget = regexp.MustCompile(`\.md\b|prompts/`)

// joinsLines matches a check that pipes a file through tr replacing
// newlines or any whitespace run (the [:space:] class) with a single
// space before grepping it, the safe form that can't miss a phrase split
// across a wrapped line. It requires tr's own second operand to be a
// quoted single space, not just the newline or [:space:] class anywhere
// in the check: tr -d '\n' or tr -d '[:space:]' deletes the line break
// instead of replacing it with a space, so
// "two\nwords" becomes "twowords" and a phrase grep still misses it, and
// requiring the literal replacement rules that out (tr -d's one operand
// can never match the second, quoted-single-space group below).
var joinsLines = regexp.MustCompile(`\btr\b(?:\s+-s)?\s+('\\n'|"\\n"|'\[:space:\]'|"\[:space:\]")\s+(' '|" ")`)

// proseGrepWithoutJoin is true when a check greps a multi-word phrase (per
// proseGrep) against prose (per proseTarget) without first joining the
// file's lines (per joinsLines).
func proseGrepWithoutJoin(check string) bool {
	return proseGrep.MatchString(check) && proseTarget.MatchString(check) && !joinsLines.MatchString(check)
}

const proseGrepCheckMsg = `check greps a phrase of more than one word in hard-wrapped prose, so the phrase can span a line break; join the lines first, such as tr -s '[:space:]' ' ' < FILE | grep -qF 'two words'`

// unquotedGlob scans check rune by rune, tracking single-quote,
// double-quote, and backslash-escape state, and returns the
// whitespace-delimited word holding the first "*" or "?" that sits outside
// any quoting and isn't immediately preceded by "$" (a shell parameter
// such as "$?", not a glob). zsh (the judge agent's login shell) aborts on
// such a glob when it matches nothing, rather than passing it through
// literally the way bash does. wordStart only moves on whitespace outside
// both quote kinds, and the one loop keeps tracking that same quote state
// past the glob rune itself, all the way to the word's own end, so a shell
// word holding a quoted space, such as grep "a b"*.go, is reported whole
// rather than cut at the space inside its own quotes.
func unquotedGlob(check string) (string, bool) {
	runes := []rune(check)
	var inSingle, inDouble, escaped bool
	// paramDepth counts how many "${...}" parameter expansions the scan is
	// currently inside: a bare "*" or "?" there, such as the "?" in
	// "${VAR:?msg}", is shell syntax, not a filename glob, so it must not
	// be flagged (review thread t7c41ccb4b641b4ba). It only opens on "${",
	// never bare "{", so an ordinary brace expansion like {a,b}*.go still
	// gets its glob flagged.
	var paramDepth int
	wordStart, globAt := 0, -1
	for i, r := range runes {
		if escaped {
			escaped = false
			continue
		}
		switch {
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '{' && !inSingle && !inDouble && (paramDepth > 0 || (i > 0 && runes[i-1] == '$')):
			paramDepth++
		case r == '}' && !inSingle && !inDouble && paramDepth > 0:
			paramDepth--
		case unicode.IsSpace(r) && !inSingle && !inDouble:
			if globAt >= 0 {
				return string(runes[wordStart:i]), true
			}
			wordStart = i + 1
		case (r == '*' || r == '?') && !inSingle && !inDouble:
			if globAt < 0 && paramDepth == 0 && (i == 0 || runes[i-1] != '$') {
				globAt = i
			}
		}
	}
	if globAt >= 0 {
		return string(runes[wordStart:]), true
	}
	return "", false
}

func unquotedGlobCheckMsg(word string) string {
	return fmt.Sprintf(`check has an unquoted glob %s; zsh aborts on an unmatched glob, so quote it, such as --include='*.go'`, word)
}

const hostSandboxCheckMsg = "check runs the host sandbox (sandbox-exec or the internal/sandbox probes), which cannot start inside the sandbox Zing runs checks in, so its probes skip and prove nothing; leave it out of the sealed checks"

// rmForce matches rm used as a command word with a flag cluster holding f:
// rm -f, rm -rf, rm -fr, rm -Rf. form -f and a quoted 'rm -f' do not match.
var rmForce = regexp.MustCompile(`(^|[;&|(\s])rm\s+-[a-zA-Z]*f`)

const rmForceCheckMsg = "check must not use rm -f or rm -rf, which Codex refuses; write state under a fresh directory from mktemp -d instead"

// skipWord matches a then that names a skip as the expected result.
var skipWord = regexp.MustCompile(`(?i)\bskip(s|ped)?\b`)

// negatedSkipWord matches a then that names a skip only to deny it, such as
// "the test no longer skips": skipWord still matches that text, but the
// then expects the test to run, not to skip, so a check that merely greps
// "--- SKIP:" would prove the opposite of what the then says.
var negatedSkipWord = regexp.MustCompile(`(?i)\b(no longer|not|never|does not|will not|stopped)\s+skip`)

// skipLine is the go test -v line a check must grep to assert that a skip
// happened, since a bare go test exits 0 whether or not the test skipped.
const skipLine = "--- SKIP:"

const expectedSkipCheckMsg = "then expects a skip, but go test exits 0 whether or not the test skipped; run go test -v and grep the skip line, such as go test -v -run TestName ./pkg | grep -q -- '--- SKIP: TestName', so the check asserts the skip itself"

// indexedScenario formats a scenario's 0-based index the way response's own
// element-path grammar does (design section 6.4, internal/response/epath.go),
// e.g. indexedScenario(0) -> "scenario[0]".
func indexedScenario(i int) string {
	return fmt.Sprintf("scenario[%d]", i)
}

// planPresenceSet builds the presence flags response.CheckPlan reads
// (design section 6.6): plan/overview/problem, the first test's kind
// attribute, and each scenario's id are all schema-required fields (no
// "omitempty" on their xml tag), so a Response value that reached this
// point through a real Layer 1 pass already proved every one of them
// present; a hand-built Response used only in a test may not have, which is
// exactly what checkReady's own checks (CheckCodeClaims, checkScenarioShape)
// exist to catch instead of a presence-map lookup.
func planPresenceSet(scenarios []response.Scenario) map[string]bool {
	present := map[string]bool{
		"plan/overview/problem":            true,
		"plan/delivery/tests/test[0]/kind": true,
	}
	for i := range scenarios {
		present["scenarios/"+indexedScenario(i)+"/id"] = true
	}
	return present
}

// formatReadyErrors renders errs one per element path per line ("path:
// msg", response.PathError's own Error() shape), the "validation errors
// pending" marker body a resumed prompt.Validation later carries back into
// the model (design section 5.3, 6.3).
func formatReadyErrors(errs []*response.PathError) string {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// readyArtifacts marshals a valid ready response's plan, claims, and one
// artifact per scenario (design section 6.5), each under runID (the cohort
// key), the same JSON shape internal/store/examples/artifacts and
// console.SeedDemo already use for these three artifact types. Version is
// left at its zero value on every one, so CommitHandlerResult assigns each
// (ticket, type) pair the next version past its current maximum.
func readyArtifacts(resp *response.ReadyResponse, runID int64) ([]store.Artifact, error) {
	normalizePlanArrays(&resp.Plan)
	planPayload, err := json.Marshal(resp.Plan)
	if err != nil {
		return nil, fmt.Errorf("marshal plan: %w", err)
	}
	claimsPayload, err := json.Marshal(resp.Claims)
	if err != nil {
		return nil, fmt.Errorf("marshal claims: %w", err)
	}

	artifacts := make([]store.Artifact, 0, 2+len(resp.Scenarios))
	artifacts = append(artifacts,
		store.Artifact{Type: artifactTypePlan, RunID: &runID, Payload: planPayload},
		store.Artifact{Type: artifactTypeClaims, RunID: &runID, Payload: claimsPayload},
	)
	for _, sc := range resp.Scenarios {
		payload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal scenario %s: %w", sc.ID, marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{Type: artifactTypeScenario, RunID: &runID, Payload: payload})
	}
	return artifacts, nil
}

// normalizePlanArrays replaces a decoded plan's nil slices with empty ones,
// for exactly the fields the artifacts/plan.json schema declares as a bare
// JSON array (no jsonschema minItems, so Layer 1 never guarantees one
// present, and no json ",omitempty" tag, so json.Marshal would otherwise
// emit null): Design.Changes and .Types, each TypeDef's own Transitions,
// Design.Migrations.Items, and Delivery.Deletions.Items. The XML "none"
// union (Migrations.None, Deletions.None) has no JSON counterpart -- the
// stored payload is always a plain, possibly empty, array either way.
func normalizePlanArrays(p *response.Plan) {
	if p.Design.Changes == nil {
		p.Design.Changes = []response.Change{}
	}
	if p.Design.Types == nil {
		p.Design.Types = []response.TypeDef{}
	}
	for i := range p.Design.Types {
		if p.Design.Types[i].Transitions == nil {
			p.Design.Types[i].Transitions = []response.Transition{}
		}
	}
	if p.Design.Migrations.Items == nil {
		p.Design.Migrations.Items = []response.Migration{}
	}
	if p.Delivery.Deletions.Items == nil {
		p.Delivery.Deletions.Items = []response.Fence{}
	}
}

// maybeResumeValidationErrors is section 5.1 step 5: a live "validation
// errors pending" marker (no later "validation errors delivered" marker for
// the same run) resumes the open session with those errors fenced behind
// prompt.Validation, and writes the "validation errors delivered run <rid>"
// marker in the same commit, carrying forward the pending marker's own run
// id rather than the resume's. handled is false when there is no live
// marker, so the caller falls through to the next entry-decision step. When
// the resume itself returns with no commit at all (runtime.ErrCanceled, or
// a pre-reserve failure returned unchanged), this leaves that empty commit
// and error untouched rather than fabricate a delivered marker for a call
// that stored nothing.
func maybeResumeValidationErrors(ctx context.Context, t store.Ticket, d Deps, sess store.Session) (commit store.HandlerCommit, handled bool, err error) {
	m, live, err := d.Store.LiveMarker(ctx, t.ID, validationErrorsPendingPrefix, validationErrorsDeliveredPrefix)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: live marker: %w", err)
	}
	if !live {
		return store.HandlerCommit{}, false, nil
	}

	firstLine, errsText, _ := strings.Cut(m.Body, "\n")
	rid := strings.TrimPrefix(firstLine, validationErrorsPendingPrefix+" run ")

	commit, err = runPlanningResume(ctx, t, d, sess, nil, []prompt.NamedInput{prompt.Validation(errsText)}, 0, true)
	if err != nil {
		return commit, true, err
	}
	commit.Messages = append(commit.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: validationErrorsDeliveredPrefix + " run " + rid,
	})
	return commit, true, nil
}

// ---- 6.5 plan review tick and the floor loop ------------------------------

// planreviewArtifactPayload is the stored shape of a "planreview" artifact
// (internal/store/schemas/artifacts/planreview.json): an object carrying
// only the surviving findings, never the FindingsResponse's own Head (job,
// outcome), which that schema's additionalProperties:false would reject.
type planreviewArtifactPayload struct {
	Findings []response.Finding `json:"findings"`
}

// planreviewPendingMarker and planreviewDeliveredMarker are section 5.3's
// version-scoped marker pair: no run id, unlike the validation-errors
// markers, because a review's own version already identifies which cohort a
// pending or delivered cycle belongs to.
func planreviewPendingMarker(version int) string {
	return fmt.Sprintf("planreview v%d pending", version)
}

func planreviewDeliveredMarker(version int) string {
	return fmt.Sprintf("planreview v%d delivered", version)
}

// gateCapMarker is issue #48's own fixed, version-scoped marker for a gate
// posted at machine.toml's planreview max_loops cap
// (maybeResumeFloorFindings's above==0 branch), written in the same commit
// as the gate. It is the one source both console's loadFindings and this
// file's own gateRejectExtra consult to tell a capped gate apart from a
// clean-review one: neither recomputes CountDeliveredReviews against
// current config, so raising max_loops after this gate already posted can
// never change what either does with it.
func gateCapMarker(version int) string {
	return fmt.Sprintf("gate cap reached plan v%d", version)
}

// maybeReviewTick is section 5.1 step 6: a stored cohort with no planreview
// artifact yet at its exact version starts the review tick fresh. handled is
// false when there is no cohort yet, or its planreview artifact already
// exists, so the caller falls through to step 7.
func maybeReviewTick(ctx context.Context, t store.Ticket, d Deps) (commit store.HandlerCommit, handled bool, err error) {
	// D31 (design section 22.4 entry step 6): no review or gate starts while
	// a planning thread is still open, whether this call came from an open
	// session (step 6 proper) or an exhausted one (planningHandler.Run's
	// SessionExhausted case) -- both share this one guard.
	conv, err := d.Store.PlanningConversation(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: planning conversation: %w", err)
	}
	if len(conv.Unsettled()) > 0 {
		return store.HandlerCommit{}, false, nil
	}

	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: current cohort: %w", err)
	}
	if !ok {
		return store.HandlerCommit{}, false, nil
	}
	_, exists, err := d.Store.PlanReviewAt(ctx, t.ID, cohort.PlanVersion)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: planreview at version %d: %w", cohort.PlanVersion, err)
	}
	if exists {
		return store.HandlerCommit{}, false, nil
	}
	commit, err = runPlanReview(ctx, t, d, nil, nil)
	return commit, true, err
}

// maybeResumeFloorFindings is section 5.1 step 7: a planreview artifact at
// the current cohort's version carrying at-or-below-floor findings, with a
// live "planreview vN pending" marker (no later "delivered" marker for that
// same version), resumes planning with those findings fenced (design
// section 6.3, 6.4) under machine.toml's max_loops, counting only delivered
// cycles (CountDeliveredReviews); at the cap it escalates loops_exhausted
// instead. handled is false when there is no cohort, no planreview artifact
// at its version, or no live pending marker for it, so the caller falls
// through to ErrNoAction.
func maybeResumeFloorFindings(ctx context.Context, t store.Ticket, d Deps, sess store.Session) (commit store.HandlerCommit, handled bool, err error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: current cohort: %w", err)
	}
	if !ok {
		return store.HandlerCommit{}, false, nil
	}
	review, exists, err := d.Store.PlanReviewAt(ctx, t.ID, cohort.PlanVersion)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: planreview at version %d: %w", cohort.PlanVersion, err)
	}
	if !exists {
		return store.HandlerCommit{}, false, nil
	}

	pending, delivered := planreviewPendingMarker(cohort.PlanVersion), planreviewDeliveredMarker(cohort.PlanVersion)
	_, live, err := d.Store.LiveMarker(ctx, t.ID, pending, delivered)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: live marker: %w", err)
	}
	if !live {
		return store.HandlerCommit{}, false, nil
	}

	var payload planreviewArtifactPayload
	if unmarshalErr := json.Unmarshal(review.Payload, &payload); unmarshalErr != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: unmarshal planreview artifact: %w", unmarshalErr)
	}
	atOrBelow := 0
	for i := range payload.Findings {
		if payload.Findings[i].Severity.Rank() <= d.Floor.Rank() {
			atOrBelow++
		}
	}
	above := len(payload.Findings) - atOrBelow

	n, err := d.Store.CountDeliveredReviews(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: count delivered reviews: %w", err)
	}
	if n >= d.Machine.Jobs[jobPlanreviewName].MaxLoops {
		// Issue #48: an above-floor finding still escalates loops_exhausted --
		// the loop itself never posts a gate against a mixed artifact. The
		// owner can: option d on this escalation (acceptPlanAtCap) posts the
		// gate anyway, since the rule exists so the owner, not the loop,
		// makes that call.
		if above == 0 {
			planArtifact, found, artErr := d.Store.GetArtifact(ctx, t.ID, artifactTypePlan)
			if artErr != nil {
				return store.HandlerCommit{}, false, fmt.Errorf("job: planning: get plan artifact: %w", artErr)
			}
			if !found {
				return store.HandlerCommit{}, false, fmt.Errorf("job: planning: ticket %d has a cohort but no plan artifact", t.ID)
			}
			var plan response.Plan
			if unmarshalErr := json.Unmarshal(planArtifact.Payload, &plan); unmarshalErr != nil {
				return store.HandlerCommit{}, false, fmt.Errorf("job: planning: unmarshal plan artifact: %w", unmarshalErr)
			}
			commit, err = postGateCommit(ctx, t, d, plan.Overview.Objective, gateApproveExplainsLoopsExhausted)
			if err != nil {
				return commit, true, err
			}
			// gateCapMarker (review P2 on issue #48's own PR) is the fixed,
			// per-version record of why this gate posted: console's loadFindings
			// and this file's own gateRejectExtra both read it instead of
			// recomputing CountDeliveredReviews against current config, so
			// raising max_loops after this gate posts can never change what
			// either one does with it.
			commit.Messages = append(commit.Messages, store.Message{
				TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: gateCapMarker(cohort.PlanVersion),
			})
			slog.Info("gate posted at loop cap", "ticket_id", t.ID, "plan_version", cohort.PlanVersion, "floor_findings", atOrBelow)
			return commit, true, nil
		}
		c := escalationCommit(t, d, nil, nil,
			string(response.EscalationCodeLoopsExhausted), loopsExhaustedWhat, loopsExhaustedWhy, "", response.EscalationOriginCapLoops)
		c.Escalation.ExtraOptions = []response.Option{{Key: escalationChoiceAccept, Text: planAcceptAtCapOptionText}}
		c.Escalation.Recommended = escalationChoiceAccept
		return c, true, nil
	}

	commit, err = runPlanningResume(ctx, t, d, sess, nil, floorResumeInputs(payload.Findings, d.Floor), 0, true)
	if err != nil {
		return commit, true, err
	}
	var runID int64
	if len(commit.Runs) == 1 {
		runID = commit.Runs[0].ID
	}
	slog.Info("floor findings delivered", "ticket_id", t.ID, "run_id", runID, "plan_version", cohort.PlanVersion,
		"at_or_below", atOrBelow, "needs_disposition", above)
	commit.Messages = append(commit.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: delivered,
	})
	return commit, true, nil
}

// runPlanReview is plan section 6.5's review tick (entry steps 6 and 1(e)):
// requires a stored cohort, the D14 pre-check (planreview, nil session --
// every review tick opens a fresh session, so the chain walks by job alone,
// unlike planning's own session-scoped check), the planreview.md prompt with
// each configured lens's "## In a plan" section appended, the ticket, the
// cohort's scenarios, and the stored plan re-rendered to XML, all fenced
// (D15), plus extra (an answered round's rendered answers, or the D14
// invalid-retry input) when present. extra and resolveIDs carry a resolved
// round's inputs exactly as runClassify's do.
func runPlanReview(ctx context.Context, t store.Ticket, d Deps, extra []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: current cohort: %w", err)
	}
	if !ok || cohort.RunID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: ticket %d has no plan cohort to review", t.ID)
	}

	n, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobPlanreviewName, nil)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: consecutive invalid outputs: %w", err)
	}
	inputs := append([]prompt.NamedInput{}, extra...)
	// The lenses' "For a bug:" rules need the classified kind; without it
	// the reviewer guesses from the ticket text. Kind comes from classify's
	// own closed vocabulary, so it is trusted.
	if t.Kind != nil {
		inputs = append(inputs, prompt.NamedInput{Label: "kind", Text: *t.Kind})
	}
	if n == 1 {
		inputs = append(inputs, prompt.Invalid(invalidRetryText(reason)))
	}

	prev, prevDispositions, err := previousReview(ctx, d, t.ID, cohort.PlanVersion)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(prev) > 0 {
		inputs = append(inputs, prompt.NamedInput{Label: previousFindingsLabel, Text: renderFindings(prev), Untrusted: true})
	}
	if len(prevDispositions) > 0 {
		inputs = append(inputs, prompt.NamedInput{Label: previousDispositionsLabel, Text: renderDispositions(prevDispositions), Untrusted: true})
	}

	jobCfg := d.Machine.Jobs[jobPlanreviewName]
	promptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: %w", err)
	}
	lensSections, err := lensSectionsFor(jobCfg.Lenses)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: %w", err)
	}

	planArtifact, ok, err := d.Store.GetArtifact(ctx, t.ID, artifactTypePlan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: get plan artifact: %w", err)
	}
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: ticket %d has a cohort but no plan artifact", t.ID)
	}
	var plan response.Plan
	if unmarshalErr := json.Unmarshal(planArtifact.Payload, &plan); unmarshalErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: unmarshal plan artifact: %w", unmarshalErr)
	}
	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: %w", err)
	}

	scenarioArtifacts, err := d.Store.ScenariosForRun(ctx, t.ID, *cohort.RunID, false)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: scenarios for run: %w", err)
	}
	scenariosRendered, err := renderScenariosForReview(scenarioArtifacts)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: %w", err)
	}

	schemas, err := renderSchemas(response.JobPlanreview, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: %w", err)
	}

	ticketText, err := specFor(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: %w", err)
	}
	in := prompt.ForPlanReview(promptText, lensSections, ticketText, scenariosRendered, planXML, inputs)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobPlanreviewName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobPlanreview, Prompt: assembled}
	return runAndRoute(ctx, d, t, jobPlanreviewName, su, req, n, freshSessionRecord, resolveIDs, response.EscalationOriginPlanreview,
		func(rr runResult) (store.HandlerCommit, error) {
			return planReviewSuccessCommit(ctx, t, d, rr, cohort, plan, planXML, prev, prevDispositions, freshSessionRecord(rr), resolveIDs)
		}, nil, 0)
}

// planReviewSuccessCommit routes a planreview run's parsed response (design
// section 6.8): the universal question and error outcomes are shared with
// classify and planning's own success routing; ok is the review's own
// outcome, planReviewOkCommit's job. plan is the cohort's own stored plan
// (already unmarshaled by the caller, runPlanReview, to render planXML), so
// a clean review's gate post (design section 6.6) can read its objective
// without a second store round trip.
func planReviewSuccessCommit(ctx context.Context, t store.Ticket, d Deps, rr runResult, cohort store.Cohort, plan response.Plan, planXML string, prev []response.Finding, prevDispositions []response.Disposition, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp.Questions, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginPlanreview), nil
	case *response.FindingsResponse:
		return planReviewOkCommit(ctx, t, d, rr, resp, cohort, plan, planXML, prev, prevDispositions, sessionCommit, resolveIDs)
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// planReviewOkCommit is design section 6.5's "on ok": drop every finding
// whose Location does not resolve as an element path in planXML
// (response.ResolvesInPlan), store the rest as a "planreview" artifact at
// the cohort's exact version, and split the survivors at the configured
// floor (severity.Rank() <= d.Floor.Rank() is at-or-below). No finding
// at-or-below the floor posts the section 6.6 gate in this same commit
// (design section 6.6's "Post" step, task 7c); otherwise this writes the
// "planreview vN pending" marker and leaves the ticket in planning, not
// waiting, for entry step 7 to pick up.
func planReviewOkCommit(ctx context.Context, t store.Ticket, d Deps, rr runResult, resp *response.FindingsResponse, cohort store.Cohort, plan response.Plan, planXML string, prev []response.Finding, prevDispositions []response.Disposition, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	kept := make([]response.Finding, 0, len(resp.Findings))
	dropped := 0
	for i := range resp.Findings {
		if !response.ResolvesInPlan([]byte(planXML), resp.Findings[i].Location) {
			dropped++
			continue
		}
		kept = append(kept, resp.Findings[i])
	}
	for i := range kept {
		kept[i].ID = fmt.Sprintf("p%d-f%d", cohort.PlanVersion, i+1)
	}
	markReopened(kept, prev, prevDispositions, d.Floor)

	var atOrBelow, above, reopened int
	for i := range kept {
		if kept[i].Severity.Rank() <= d.Floor.Rank() {
			atOrBelow++
		} else {
			above++
		}
		if kept[i].Reopens != "" {
			reopened++
		}
	}
	slog.Info("floor split", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "floor", string(d.Floor),
		"at_or_below", atOrBelow, "above", above, "dropped_unresolved", dropped, "reopened", reopened)

	payload, err := json.Marshal(planreviewArtifactPayload{Findings: kept})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: marshal findings: %w", err)
	}

	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeOk))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	c.Artifacts = []store.Artifact{{
		Type: artifactTypePlanreview, Version: cohort.PlanVersion, RunID: &rr.Reserved.RunID, Payload: payload,
	}}

	if atOrBelow == 0 {
		gc, gcErr := postGateCommit(ctx, t, d, plan.Overview.Objective, gateApproveExplains)
		if gcErr != nil {
			return store.HandlerCommit{}, gcErr
		}
		gc.Runs = c.Runs
		gc.Session = c.Session
		gc.ResolveQuestions = c.ResolveQuestions
		gc.Artifacts = c.Artifacts
		gc.AttachRunToMsgs = true
		slog.Info("gate posted", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "plan_version", cohort.PlanVersion)
		return gc, nil
	}

	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: planreviewPendingMarker(cohort.PlanVersion),
	}}
	return c, nil
}

// lensSectionsFor reads each of lenses' prompt files (machine.Jobs["planreview"].Lenses
// order, the same embed.FS path machine.go's own validatePaths checks:
// "prompts/lenses/<lens>.md") and returns each file's own "## In a plan"
// section (prompt.PlanLensSection), the piece prompt.ForPlanReview appends
// to the planreview prompt (design section 6.5).
func lensSectionsFor(lenses []string) ([]string, error) {
	out := make([]string, len(lenses))
	for i, lens := range lenses {
		text, err := readAsset("prompts/lenses/" + lens + ".md")
		if err != nil {
			return nil, err
		}
		section, sectionErr := prompt.PlanLensSection(text)
		if sectionErr != nil {
			return nil, fmt.Errorf("job: planreview: lens %s: %w", lens, sectionErr)
		}
		out[i] = section
	}
	return out, nil
}

// planXMLFor renders plan back to its XML form for the plan-review prompt
// (design section 6.5): internal/response has no dedicated renderer for
// response.Plan (it only ever decodes one, in Parse), so this uses
// encoding/xml's own marshaller against Plan's wire tags directly, with a
// "plan" start element standing in for the XMLName a decoded document
// carries.
func planXMLFor(plan response.Plan) (string, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeElement(plan, xml.StartElement{Name: xml.Name{Local: "plan"}}); err != nil {
		return "", fmt.Errorf("encode plan xml: %w", err)
	}
	return buf.String(), nil
}

// renderScenariosForReview renders artifacts (ScenariosForRun's own
// insertion order) one per line as "<id> [<kind>] given: ... when: ...
// then: ..." (design section 6.5), the cohort text prompt.ForPlanReview
// fences into the review prompt.
func renderScenariosForReview(artifacts []store.Artifact) (string, error) {
	lines := make([]string, len(artifacts))
	for i, a := range artifacts {
		var sc response.Scenario
		if err := json.Unmarshal(a.Payload, &sc); err != nil {
			return "", fmt.Errorf("unmarshal scenario artifact %d: %w", i, err)
		}
		lines[i] = fmt.Sprintf("%s [%s] given: %s when: %s then: %s", sc.ID, sc.Kind, sc.Given, sc.When, sc.Then)
	}
	return strings.Join(lines, "\n"), nil
}

// renderFindings renders findings one per line -- lens, severity, location,
// text, then fix -- the shape entry step 7 fences behind prompt.Findings
// when it resumes planning with the at-or-below-floor survivors (design
// section 6.3, 6.4).
func renderFindings(findings []response.Finding) string {
	lines := make([]string, len(findings))
	for i := range findings {
		f := &findings[i]
		line := fmt.Sprintf("[%s/%s] %s: %s (fix: %s)", f.Lens, f.Severity, f.Location, f.Text, f.Fix)
		if f.ID != "" {
			line = f.ID + " " + line
		}
		switch {
		case f.Reopens != "" && f.ReopensAfter == reopensAfterFixed:
			line += fmt.Sprintf(" (raised again: %s was marked fixed)", f.Reopens)
		case f.Reopens != "":
			line += fmt.Sprintf(" (raised again: %s got no disposition)", f.Reopens)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// renderDispositions renders dispositions one per line -- finding id, kind,
// then the fixed path or the disputed reason -- the review tick fences
// behind previousDispositionsLabel.
func renderDispositions(ds []response.Disposition) string {
	lines := make([]string, len(ds))
	for i, disp := range ds {
		detail := disp.Path
		if disp.Kind == response.DispositionDisputed {
			detail = disp.Reason
		}
		lines[i] = fmt.Sprintf("%s %s: %s", disp.Finding, disp.Kind, detail)
	}
	return strings.Join(lines, "\n")
}

// needsDispositionLabel is the fenced input carrying above-floor findings a
// planning resume delivers; the planner must answer each with a disposition
// of fixed or disputed (ticket 72, task 2 on).
const needsDispositionLabel = "needs_disposition"

// previousFindingsLabel and previousDispositionsLabel fence the plan
// review's own inputs about the last review below the version under review
// and what the plan did about each of its findings (ticket 72 task 3).
const (
	previousFindingsLabel     = "previous_findings"
	previousDispositionsLabel = "previous_dispositions"
)

// reopensAfterFixed and reopensAfterNoDisposition are Finding.ReopensAfter's
// two values: what the plan did with the earlier, same-location finding a
// new above-floor finding raises again (ticket 72 task 3, owner decision Q3).
const (
	reopensAfterFixed         = "fixed"
	reopensAfterNoDisposition = "no_disposition"
)

// floorResumeInputs splits findings at floor into the fenced inputs a
// planning resume carries: "findings" for the at-or-below-floor survivors
// and "needs_disposition" for the above-floor ones (owner decision Q1 on
// ticket 72: every above-floor finding, whatever the floor, goes back to the
// planner). Either side is left out of the result when it is empty.
func floorResumeInputs(findings []response.Finding, floor response.Severity) []prompt.NamedInput {
	var atOrBelow, above []response.Finding
	for i := range findings {
		if findings[i].Severity.Rank() <= floor.Rank() {
			atOrBelow = append(atOrBelow, findings[i])
		} else {
			above = append(above, findings[i])
		}
	}
	var inputs []prompt.NamedInput
	if len(atOrBelow) > 0 {
		inputs = append(inputs, prompt.Findings(renderFindings(atOrBelow)))
	}
	if len(above) > 0 {
		inputs = append(inputs, prompt.NamedInput{Label: needsDispositionLabel, Text: renderFindings(above), Untrusted: true})
	}
	return inputs
}

// gateFindingTextMaxRunes caps each finding's text on the owner-chose gate
// acceptPlanAtCap posts (ticket 66).
const gateFindingTextMaxRunes = 200

// renderGateFindings renders findings, in stored order, one markdown list
// line each: "- SEVERITY LOCATION TEXT", every whitespace run collapsed to
// one space and TEXT cut to its first gateFindingTextMaxRunes runes; "" for
// no findings.
func renderGateFindings(findings []response.Finding) string {
	lines := make([]string, len(findings))
	for i := range findings {
		f := &findings[i]
		text := []rune(strings.Join(strings.Fields(f.Text), " "))
		if len(text) > gateFindingTextMaxRunes {
			text = text[:gateFindingTextMaxRunes]
		}
		lines[i] = strings.Join(strings.Fields(fmt.Sprintf("- %s %s %s", f.Severity, f.Location, string(text))), " ")
	}
	return strings.Join(lines, "\n")
}

// ---- 6.6 the gate: post, approve/seal, reject -----------------------------

// gateQuestionMessage builds the section 6.6 "Post" message: kind gate,
// recommended "a", the fixed Approve/Reject chip pair, Key left empty for
// CommitHandlerResult's own fillQuestionKeyTx to allocate (design section
// 4.5, 6.7: "gate and planning questions use the same Q<n> allocation"), and
// body set to objective (the stored plan's overview objective text, the one
// sentence design D8 says the gate renders for the owner) then, blank-line
// separated, gateApproveExplains (F013): splitQuestionBody (console/views.go)
// cuts the body on its first newline, so objective still renders as the
// question's title and gateApproveExplains as its markdown body, exactly as
// every other question's Title/Body pair does.
func gateQuestionMessage(ticketID int64, objective, explains string) (store.Message, error) {
	payload, err := json.Marshal(response.QuestionPayload{
		Kind:        response.QuestionKindGate,
		State:       response.QuestionStateOpen,
		Recommended: gateOptionApprove,
		Options: []response.Option{
			{Key: gateOptionApprove, Text: "Approve"},
			{Key: gateOptionReject, Text: "Reject"},
		},
	})
	if err != nil {
		return store.Message{}, fmt.Errorf("job: gate: marshal question payload: %w", err)
	}
	return store.Message{
		TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing,
		State: new(questionStateOpen), Body: objective + "\n\n" + explains, Payload: payload,
	}, nil
}

// postGateCommit builds section 6.6's "Post" commit (design section 6.6,
// D8; issue #48): the gate question (gateQuestionMessage), Waiting set to
// waitingFlagGate, and the D32 conversation fence (design section 22.12.2)
// that tells a reopen racing this very post apart from one already
// delivered. It carries no Runs, Session, ResolveQuestions, or
// AttachRunToMsgs -- planReviewOkCommit, the caller with a live review run,
// overlays those itself; maybeResumeFloorFindings's cap branch has no run
// to attach, since the cap, not a run, produced this gate.
func postGateCommit(ctx context.Context, t store.Ticket, d Deps, objective, explains string) (store.HandlerCommit, error) {
	msg, err := gateQuestionMessage(t.ID, objective, explains)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	c := baseCommit(t, d)
	c.Messages = []store.Message{msg}
	waiting := waitingFlagGate
	c.Waiting = &waiting

	conv, err := d.Store.PlanningConversation(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: planning conversation: %w", err)
	}
	c.Conversation = &store.ConversationCommit{ThroughBatch: conv.Delivered}
	return c, nil
}

// enterFromGateRound is section 5.1 step 1(a), rewritten by D32 (design
// section 22.12.3): round is the answered gate round enterFromRound just
// identified by its newest question's kind. Option b, or a round carrying
// replies and no option at all, is a reject -- "resume or fresh" with the
// replies' bodies as notes (design section 6.6, 6.7), unchanged. Option a
// (approve) used to run the seal pre-check directly; now it does only once
// already confirmed (no cohort or no producing run still goes straight to
// gateApprove, which re-derives and escalates the same failure, design
// D16's branches 1 and 2); otherwise it runs the gate's own confirming turn
// (gateConfirmEntry) before ever sealing.
func (h planningHandler) enterFromGateRound(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	resolveIDs := questionIDs(round)
	if !gateRoundApproved(round) {
		notes := joinReplies(round.Replies)
		slog.Info("gate rejected", "ticket_id", t.ID)
		extra, extraErr := gateRejectExtra(ctx, t, d, notes)
		if extraErr != nil {
			return store.HandlerCommit{}, extraErr
		}
		return resumeOrFresh(ctx, t, d, extra, resolveIDs)
	}

	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: current cohort: %w", err)
	}
	if !ok || cohort.RunID == nil {
		return gateApprove(ctx, t, d, resolveIDs)
	}

	_, confirmed, err := d.Store.ConfirmedApprovalForVersion(ctx, t.ID, cohort.PlanVersion)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: confirmed approval: %w", err)
	}
	if confirmed {
		return gateApprove(ctx, t, d, resolveIDs)
	}

	gateQID := round.Questions[len(round.Questions)-1].ID
	approveRow, _, ok := newestChosenAnswer(round.Answers)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: approved round %d has no approving answer", gateQID)
	}
	notes := joinReplies(round.Replies)
	slog.Info("gate approval starts confirming turn", "ticket_id", t.ID, "question_id", gateQID, "plan_version", cohort.PlanVersion)
	return gateConfirmEntry(ctx, t, d, gateQID, approveRow.ID, cohort, notes)
}

// gateRejectExtra builds a rejected gate's resume extra (review P2 on issue
// #48's own PR): the owner's notes alone give the planner nothing to act on
// when the rejected gate was posted at the loop cap (gateCapMarker), since
// those stored findings were never fed back into planning -- the cap
// stopped the resume loop that would have done that. When the current
// cohort carries that marker, this fetches every finding still in the
// stored planreview artifact with storedPlanreviewFindings and fences them
// ahead of the notes, exactly as the floor-findings resume renders them. A
// capped gate's stored findings are not always all at-or-below-floor: the
// owner's own d pick on the cap_loops escalation (acceptPlanAtCap) posts
// the gate with above-floor findings still open, and the owner decided a
// reject should feed those back too (owner decision Q2). A clean-review
// gate (no marker) carries notes alone, unchanged.
func gateRejectExtra(ctx context.Context, t store.Ticket, d Deps, notes string) ([]prompt.NamedInput, error) {
	capped, err := rejectedGateWasCapped(ctx, t, d)
	if err != nil {
		return nil, err
	}
	if !capped {
		return []prompt.NamedInput{prompt.Notes(notes)}, nil
	}
	findings, err := storedPlanreviewFindings(ctx, t, d)
	if err != nil {
		return nil, err
	}
	return append(floorResumeInputs(findings, d.Floor), prompt.Notes(notes)), nil
}

// rejectedGateWasCapped reports whether the current cohort's exact plan
// version carries gateCapMarker: false, with no error, when there is no
// current cohort at all (nothing to check).
func rejectedGateWasCapped(ctx context.Context, t store.Ticket, d Deps) (bool, error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return false, fmt.Errorf("job: gate: current cohort: %w", err)
	}
	if !ok {
		return false, nil
	}
	_, found, err := d.Store.Marker(ctx, t.ID, gateCapMarker(cohort.PlanVersion))
	if err != nil {
		return false, fmt.Errorf("job: gate: cap marker: %w", err)
	}
	return found, nil
}

// newestChosenAnswer returns the row and option key of answers' newest sent
// row that named an option, ok false when none did (design section 6.6,
// 6.7's own "final choice wins" rule): a round answered more than once (a
// corrected chip click before the batch resolves) reads its final choice,
// not its first. newestChosenOption is a thin wrapper kept for
// roundChoice's own a/b/c read; gateApprove's own GateApproval (D32, design
// section 22.12.1) needs the row itself (its id is AID, its BatchID is BA),
// so both read this one scan rather than two that could drift.
func newestChosenAnswer(answers []store.MessageRow) (row store.MessageRow, option string, ok bool) {
	for i := range answers {
		var ap response.AnswerPayload
		if err := json.Unmarshal(answers[i].Payload, &ap); err == nil && ap.Option != nil {
			row, option, ok = answers[i], *ap.Option, true
		}
	}
	return row, option, ok
}

// newestChosenOption returns the option key of the newest sent answer among
// answers that named one, "" when none did. Shared by gateRoundApproved
// (the gate's own a/b choice) and roundChoice (an escalation round's a/b/c
// choice).
func newestChosenOption(answers []store.MessageRow) string {
	_, option, _ := newestChosenAnswer(answers)
	return option
}

// gateRoundApproved reports whether round's sent answer chose option "a"
// (design section 6.6).
func gateRoundApproved(round store.Round) bool {
	return newestChosenOption(round.Answers) == gateOptionApprove
}

// joinReplies renders replies' bodies newline-joined (design section 6.6,
// 6.7's "the replies' bodies joined by \n"), "" when there are none: a chip
// reject with no free-text reply carries empty notes rather than failing.
func joinReplies(replies []store.MessageRow) string {
	bodies := make([]string, len(replies))
	for i := range replies {
		bodies[i] = replies[i].Body
	}
	return strings.Join(bodies, "\n")
}

// resumeOrFresh is design section 6.7's "resume or fresh", shared by a gate
// rejection (section 6.6) and, in task 13, escalation resolution: an open
// planning session resumes (6.4) with extra; a kindless ticket has never
// classified, so it classifies fresh (6.1) with extra instead; anything else
// starts the planning first turn fresh (6.2) with extra. Every branch
// resolves resolveIDs in the same commit.
func resumeOrFresh(ctx context.Context, t store.Ticket, d Deps, extra []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
	maxResumes := d.Machine.Jobs[jobPlanningName].MaxResumes
	sess, state, err := d.Store.LatestSession(ctx, t.ID, jobPlanningName, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: resume or fresh: latest session: %w", err)
	}
	if state == store.SessionOpen {
		return runPlanningResume(ctx, t, d, sess, resolveIDs, extra, 0, true)
	}
	if t.Kind == nil {
		return runClassify(ctx, t, d, extra, resolveIDs)
	}
	return runPlanningFirst(ctx, t, d, extra, resolveIDs)
}

// ---- 6.7 escalation resolution (task 13) ----------------------------------

// roundChoice returns round's choice among escalationChoiceRetry,
// escalationChoiceBack, and escalationChoiceAbandon (design section 6.7's
// Resolve): the newest sent answer's chosen option, or, when the round
// carries replies and no option at all, whatever roundRecommendedOption
// defaults an unanswered round of this kind to. roundChoice is shared with
// every question kind a round can carry (building.go's and postbuild.go's
// own escalation rounds, and shipping.go's mergeAnswer, a merge round) --
// only an escalation question may default to its own stored Recommended
// (PR #60 review, P1): a merge question's Recommended is always "a"
// (mergeQuestionMessages, shipping.go), the opposite of what reading it
// back here would mean (it would silently merge a PR the owner only left
// a note on, never picked a chip for), so roundRecommendedOption gates
// that default on the round's own question Kind.
func roundChoice(round store.Round) string {
	if opt := newestChosenOption(round.Answers); opt != "" {
		return opt
	}
	return roundRecommendedOption(round)
}

// roundRecommendedOption is roundChoice's own "replies with no option"
// default. For an escalation question (Kind "question", escalateTx's own
// payload.Kind, store/commit.go) it reads that question's own stored
// Recommended option back (#47 follow-up): a freshly raised escalation's
// Recommended already follows escalationOptionsFor (store/commit.go), so
// this just carries that choice through unanswered; an escalation stored
// before that fix shipped keeps whatever it recommended then (store's own
// "existing stored escalations: not touched"). It falls back to
// escalationChoiceRetry only when an escalation's own payload has no
// parseable Recommended. For every other kind (merge, gate, split,
// perimeter, review) -- and when there is no question at all -- it returns
// escalationChoiceBack, exactly the one fixed default every kind had
// before #47 (PR #60 review, P1): escalationChoiceBack already means
// "hold" for a merge round (shipping.go's mergeAnswer: anything but
// escalationChoiceRetry holds), so this is not a new behavior for those
// kinds, only a name for the one they already had.
//
// An amended escalation (qp.Amendment != nil) is the one exception to
// reading Recommended straight back: escalationOptionsFor recommends "a"
// (Accept) there only to steer the owner's chip in the UI, and a reply
// with no option picked must never silently accept a judge-written check
// (#57, r1f9 triage: "a reply with no picked option must never accept an
// amendment"). So a reply with no option on an amended escalation always
// falls back to escalationChoiceBack (Edit it), which starts a fresh judge
// round with the scenario unchanged rather than applying anything.
func roundRecommendedOption(round store.Round) string {
	if len(round.Questions) == 0 {
		return escalationChoiceBack
	}
	var qp response.QuestionPayload
	q := round.Questions[len(round.Questions)-1]
	if err := json.Unmarshal(q.Payload, &qp); err != nil || qp.Kind != response.QuestionKindQuestion {
		return escalationChoiceBack
	}
	if qp.Amendment != nil {
		slog.Info("amended escalation reply without option, falling back to edit", "ticket_id", q.TicketID, "question_id", q.ID)
		return escalationChoiceBack
	}
	if qp.Recommended == "" {
		return escalationChoiceRetry
	}
	return qp.Recommended
}

// int64OrZero renders a nullable id for a log line as 0 when absent, never a
// bare pointer (design section 9's "structured, never a raw output" rule):
// escalation resolution's own session_id and run_id fields are both
// sometimes nil (design section 6.7's RunID/SessionID table).
func int64OrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// enterFromEscalationRound is section 5.1 step 1(b) and section 6.7's own
// Write/Resolve: escID is the newest question's own parent id, the
// escalation message it answers. choice is the round's own a/b/c choice
// (roundChoice); notes is the sent replies' bodies joined with "\n"
// (joinReplies, reused from the gate's own reject path); errorText is the
// escalation's what, why, and tried, newline-joined, the same "error" input
// shape section 6.3's resume inputs describe. Choice c (abandon) resolves
// every open or answered question and transitions straight to abandoned,
// with no runtime call, regardless of origin; every other combination
// routes through section 6.7's choice-by-origin table below.
func (h planningHandler) enterFromEscalationRound(ctx context.Context, t store.Ticket, d Deps, round store.Round, escID int64) (store.HandlerCommit, error) {
	escMsg, payload, err := d.Store.EscalationByID(ctx, escID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: escalation %d: %w", escID, err)
	}
	resolveIDs := questionIDs(round)
	choice := roundChoice(round)
	notes := joinReplies(round.Replies)
	errorText := payload.What + "\n" + payload.Why + "\n" + payload.Tried
	origin := response.EscalationOrigin(payload.Origin)
	capLoops := origin == response.EscalationOriginCapLoops && payload.Code == string(response.EscalationCodeLoopsExhausted)
	// Owner decision Q1: a reply with no chip on the cap_loops question
	// resolves as Retry even though d is recommended -- a note is for the
	// planner, and the gate cannot act on it (review's own rule,
	// postbuild.go's resolvePostBuildEscalation).
	replyOnlyDefaultedToAccept := choice == escalationChoiceAccept && newestChosenOption(round.Answers) == ""
	if capLoops && replyOnlyDefaultedToAccept {
		slog.Info("plan review loops_exhausted reply-only answer resolves as retry", "ticket_id", t.ID, "recommended", escalationChoiceAccept)
		choice = escalationChoiceRetry
	}
	notesAndError := []prompt.NamedInput{prompt.Notes(notes), prompt.Error(errorText)}

	var commit store.HandlerCommit
	preserved := 0

	switch {
	case choice == escalationChoiceAbandon:
		commit = abandonCommit(t, d, payload.Code)

	case origin == response.EscalationOriginClassify:
		// Both a and b classify fresh (design section 6.7: "kind is still
		// unset", so there is no session to resume or restart instead).
		commit, err = runClassify(ctx, t, d, notesAndError, resolveIDs)

	case origin == response.EscalationOriginPlanningFirst, origin == response.EscalationOriginPlanningResume,
		origin == response.EscalationOriginSplit, origin == response.EscalationOriginNothingToDoClaims:
		// Both a and b resume or fresh, identically (design section 6.7:
		// "a retry | split, nothing_to_do_claims | same as b").
		commit, err = resumeOrFresh(ctx, t, d, notesAndError, resolveIDs)

	case origin == response.EscalationOriginPlanreview && choice == escalationChoiceRetry:
		commit, err = runPlanReview(ctx, t, d, []prompt.NamedInput{prompt.Notes(notes)}, resolveIDs)
	case origin == response.EscalationOriginPlanreview:
		commit, err = resumeOrFresh(ctx, t, d, notesAndError, resolveIDs)

	case (origin == response.EscalationOriginGateApprove || origin == response.EscalationOriginSeal) && choice == escalationChoiceRetry:
		commit, err = gateApprove(ctx, t, d, resolveIDs)
	case origin == response.EscalationOriginGateApprove, origin == response.EscalationOriginSeal:
		commit, err = resumeOrFresh(ctx, t, d, notesAndError, resolveIDs)

	case origin == response.EscalationOriginCapResumes:
		// D31 (design section 22.4): resolveCapResumesEscalation no longer
		// preserves any older round (there is none left to preserve), so
		// preserved stays 0 here -- runPlanningFirst's own fresh transcript
		// carries the exhausted session's threads instead.
		commit, err = resolveCapResumesEscalation(ctx, t, d, notes, errorText, resolveIDs)

	case capLoops && choice == escalationChoiceAccept:
		commit, err = acceptPlanAtCap(ctx, t, d, resolveIDs)
	case origin == response.EscalationOriginCapLoops && choice == escalationChoiceRetry:
		findings, findErr := storedPlanreviewFindings(ctx, t, d)
		if findErr != nil {
			return store.HandlerCommit{}, findErr
		}
		commit, err = resumeOrFresh(ctx, t, d, append(floorResumeInputs(findings, d.Floor), prompt.Notes(notes)), resolveIDs)
	case origin == response.EscalationOriginCapLoops:
		commit, err = resumeOrFresh(ctx, t, d, notesAndError, resolveIDs)

	case origin == response.EscalationOriginCapBudget:
		// Both a and b retry once the budget has room, and re-escalate
		// wall_clock while it does not (design section 6.7).
		commit, err = retryCapBudget(ctx, t, d, resolveIDs)

	default:
		return store.HandlerCommit{}, fmt.Errorf("job: planning: escalation %d: unrecognized origin %q", escID, payload.Origin)
	}
	if err != nil {
		return commit, err
	}

	slog.Info("escalation resolved", "ticket_id", t.ID, "session_id", int64OrZero(payload.SessionID),
		"run_id", int64OrZero(escMsg.RunID), "code", payload.Code, "origin", payload.Origin,
		"choice", choice, "preserved_rounds", preserved)
	return commit, nil
}

// acceptPlanAtCap is option d on plan review's cap_loops loops_exhausted
// question: the owner, not the loop, chose to see the gate with findings
// above the floor still open. It posts the gate as the at-or-below-floor
// cap path does (postGateCommit plus gateCapMarker, so a reject and the
// console both read it as a capped gate), with no runtime call, and
// resolves the escalation round in the same commit.
func acceptPlanAtCap(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64) (store.HandlerCommit, error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: cap_loops accept: current cohort: %w", err)
	}
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: cap_loops accept: ticket %d has no current cohort", t.ID)
	}
	planArtifact, found, err := d.Store.GetArtifact(ctx, t.ID, artifactTypePlan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: cap_loops accept: get plan artifact: %w", err)
	}
	if !found {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: cap_loops accept: ticket %d has a cohort but no plan artifact", t.ID)
	}
	var plan response.Plan
	if err = json.Unmarshal(planArtifact.Payload, &plan); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: cap_loops accept: unmarshal plan artifact: %w", err)
	}
	findings, err := storedPlanreviewFindings(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	explains := gateApproveExplainsOwnerChose
	if list := renderGateFindings(findings); list != "" {
		explains += "\n\n" + list
	}
	c, err := postGateCommit(ctx, t, d, plan.Overview.Objective, explains)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	c.ResolveQuestions = resolveIDs
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: gateCapMarker(cohort.PlanVersion),
	})
	slog.Info("gate posted at loop cap by owner choice", "ticket_id", t.ID, "plan_version", cohort.PlanVersion, "findings", len(findings))
	return c, nil
}

// abandonCommit is section 6.7 choice "c" (design D10): every open or
// answered question on the ticket resolves (ResolveAll), the ticket
// transitions straight to abandoned, and no runtime call is made.
func abandonCommit(t store.Ticket, d Deps, code string) store.HandlerCommit {
	c := baseCommit(t, d)
	c.ResolveAll = true
	c.Next = stateAbandoned
	c.Reason = fmt.Sprintf(reasonAbandonedFmt, code)
	return c
}

// recapBudgetEscalation is retryCapBudget's still-over-budget branch (design
// section 6.7): re-escalate wall_clock in this same commit, with the
// unchanged What/Why text budgetEscalationCommit itself uses, resolving the
// round that led here.
func recapBudgetEscalation(t store.Ticket, d Deps, resolveIDs []int64) store.HandlerCommit {
	c := escalationCommit(t, d, nil, nil, string(response.EscalationCodeWallClock), budgetExhaustedWhat, budgetExhaustedWhy, "", response.EscalationOriginCapBudget)
	c.ResolveQuestions = resolveIDs
	return c
}

// retryCapBudget is the cap_budget retry row (design section 6.7):
// while the ticket's agent seconds still meet d.Budget, the comparison it
// shares with runJobWith via budgetExhausted, it re-escalates wall_clock
// (recapBudgetEscalation). Once the owner has raised
// budget.agent_minutes_per_ticket and restarted serve, it resolves the
// round and writes the "retry requested" marker instead, so the next tick
// retakes the refused call; a shipping ticket also clears its poll. Both
// branches log at INFO, so settings.log_level warn or error drops them.
func retryCapBudget(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64) (store.HandlerCommit, error) {
	agentSeconds, err := d.Store.AgentSecondsForTicket(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: cap_budget retry: agent seconds for ticket %d: %w", t.ID, err)
	}
	exhausted, capSeconds := budgetExhausted(agentSeconds, d.Budget)
	if exhausted {
		slog.Info("cap_budget retry still over budget", "ticket_id", t.ID, "agent_seconds", agentSeconds, "cap_seconds", capSeconds)
		return recapBudgetEscalation(t, d, resolveIDs), nil
	}
	slog.Info("cap_budget retry resumes", "ticket_id", t.ID, "state", t.State, "agent_seconds", agentSeconds, "cap_seconds", capSeconds)
	if t.State == stateShipping {
		return shipRetryMarkerCommit(t, d, resolveIDs), nil
	}
	return buildingHandler{}.retryMarkerCommit(t, d, resolveIDs), nil
}

// storedPlanreviewFindings returns every finding in the planreview artifact
// at the current cohort's exact version, in stored order; nil, nil when
// there is no cohort or no artifact at that version.
func storedPlanreviewFindings(ctx context.Context, t store.Ticket, d Deps) ([]response.Finding, error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("job: planning: stored planreview findings: current cohort: %w", err)
	}
	if !ok {
		return nil, nil
	}
	review, exists, err := d.Store.PlanReviewAt(ctx, t.ID, cohort.PlanVersion)
	if err != nil {
		return nil, fmt.Errorf("job: planning: stored planreview findings: planreview at version %d: %w", cohort.PlanVersion, err)
	}
	if !exists {
		return nil, nil
	}
	var payload planreviewArtifactPayload
	if unmarshalErr := json.Unmarshal(review.Payload, &payload); unmarshalErr != nil {
		return nil, fmt.Errorf("job: planning: stored planreview findings: unmarshal planreview artifact: %w", unmarshalErr)
	}
	return payload.Findings, nil
}

// previousReview returns the findings of ticketID's newest planreview
// artifact below version, and the dispositions of the plan stored at that
// review's version plus one -- the plan that answered it; all nil, nil when
// no review exists below version (ticket 72 task 3). runPlanReview calls
// this once per review run and threads the result through its success
// closure into planReviewOkCommit, so a review's own inputs and its reopens
// marking come from the same read.
func previousReview(ctx context.Context, d Deps, ticketID int64, version int) ([]response.Finding, []response.Disposition, error) {
	all, err := d.Store.ListArtifacts(ctx, ticketID)
	if err != nil {
		return nil, nil, fmt.Errorf("job: planreview: previous review: %w", err)
	}
	var review *store.Artifact
	for i := range all {
		a := &all[i]
		below := a.Type == artifactTypePlanreview && a.Version < version
		newer := review == nil || a.Version > review.Version
		if below && newer {
			review = a
		}
	}
	if review == nil {
		return nil, nil, nil
	}
	var payload planreviewArtifactPayload
	if unmarshalErr := json.Unmarshal(review.Payload, &payload); unmarshalErr != nil {
		return nil, nil, fmt.Errorf("job: planreview: unmarshal planreview artifact v%d: %w", review.Version, unmarshalErr)
	}
	for i := range all {
		if all[i].Type != artifactTypePlan || all[i].Version != review.Version+1 {
			continue
		}
		var plan response.Plan
		if unmarshalErr := json.Unmarshal(all[i].Payload, &plan); unmarshalErr != nil {
			return nil, nil, fmt.Errorf("job: planreview: unmarshal plan artifact v%d: %w", all[i].Version, unmarshalErr)
		}
		return payload.Findings, plan.Dispositions, nil
	}
	return payload.Findings, nil, nil
}

// markReopened sets Reopens and ReopensAfter on each above-floor finding in
// findings that is raised at the exact location of a previous above-floor
// finding (in prev) the plan marked fixed or left without a disposition; a
// disputed match never counts (owner decision Q3 on ticket 72: Zing matches
// on exact location and an above-floor severity, never on the reviewer
// naming the earlier id).
func markReopened(findings, prev []response.Finding, ds []response.Disposition, floor response.Severity) {
	kinds := make(map[string]response.DispositionKind, len(ds))
	for _, disp := range ds {
		kinds[disp.Finding] = disp.Kind
	}
	for i := range findings {
		if findings[i].Severity.Rank() <= floor.Rank() {
			continue
		}
		for j := range prev {
			p := &prev[j]
			sameSpot := p.ID != "" && p.Severity.Rank() > floor.Rank() && p.Location == findings[i].Location
			kind := kinds[p.ID]
			if !sameSpot || kind == response.DispositionDisputed {
				continue
			}
			findings[i].Reopens = p.ID
			findings[i].ReopensAfter = reopensAfterNoDisposition
			if kind == response.DispositionFixed {
				findings[i].ReopensAfter = reopensAfterFixed
			}
			break
		}
	}
}

// dispositionsRequired returns the above-floor findings, with ids, of the
// current cohort's planreview artifact when a floor loop started for that
// version (its pending marker exists); nil, nil when there is no cohort or
// no loop started. It does not depend on which path produced the ready
// plan: the pending marker, not the delivered one, is the one source
// readyCommit can read regardless of whether maybeResumeFloorFindings,
// gateRejectExtra, or the cap_loops retry produced this turn's resume.
func dispositionsRequired(ctx context.Context, t store.Ticket, d Deps) ([]response.Finding, error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("job: planning: dispositions required: current cohort: %w", err)
	}
	if !ok {
		return nil, nil
	}
	_, looped, err := d.Store.Marker(ctx, t.ID, planreviewPendingMarker(cohort.PlanVersion))
	if err != nil {
		return nil, fmt.Errorf("job: planning: dispositions required: pending marker: %w", err)
	}
	if !looped {
		return nil, nil
	}
	findings, err := storedPlanreviewFindings(ctx, t, d)
	if err != nil {
		return nil, err
	}
	var required []response.Finding
	for i := range findings {
		if findings[i].Severity.Rank() > d.Floor.Rank() && findings[i].ID != "" {
			required = append(required, findings[i])
		}
	}
	return required, nil
}

// checkDispositions checks a ready plan's dispositions against required:
// one valid entry per required finding, none for any other id (design
// section 6.5, ticket 72 task 2). The default kind branch is defensive:
// Layer 1 (response.validate.go's Values enum check) already rejects an
// unknown DispositionKind as invalid output before this runs; only
// TestCheckDispositions, calling this directly, reaches it.
func checkDispositions(required []response.Finding, ds []response.Disposition, planXML []byte) []*response.PathError {
	byID := make(map[string]response.Finding, len(required))
	for i := range required {
		byID[required[i].ID] = required[i]
	}
	var errs []*response.PathError
	seen := make(map[string]bool, len(ds))
	for i, disp := range ds {
		path := fmt.Sprintf("plan/dispositions/disposition[%d]", i+1)
		if _, ok := byID[disp.Finding]; !ok {
			errs = append(errs, &response.PathError{Path: path, Msg: fmt.Sprintf("finding %s was not in the needs_disposition input; remove this disposition", disp.Finding)})
			continue
		}
		if seen[disp.Finding] {
			errs = append(errs, &response.PathError{Path: path, Msg: fmt.Sprintf("finding %s already has a disposition; keep one", disp.Finding)})
			continue
		}
		seen[disp.Finding] = true
		switch disp.Kind {
		case response.DispositionFixed:
			if disp.Path == "" {
				errs = append(errs, &response.PathError{Path: path, Msg: "a fixed disposition names the plan element path you changed"})
			} else if !response.ResolvesInPlan(planXML, disp.Path) {
				errs = append(errs, &response.PathError{Path: path, Msg: fmt.Sprintf("path %s does not resolve in this plan", disp.Path)})
			}
		case response.DispositionDisputed:
			if strings.TrimSpace(disp.Reason) == "" {
				errs = append(errs, &response.PathError{Path: path, Msg: "a disputed disposition gives the reason the finding is wrong"})
			}
		default:
			errs = append(errs, &response.PathError{Path: path, Msg: fmt.Sprintf("kind %q must be fixed or disputed", disp.Kind)})
		}
	}
	for i := range required {
		f := &required[i]
		if !seen[f.ID] {
			errs = append(errs, &response.PathError{Path: "plan/dispositions", Msg: fmt.Sprintf(
				"finding %s (%s at %s: %s) needs a disposition: fixed with the path you changed, or disputed with a reason",
				f.ID, f.Severity, f.Location, f.Text)})
		}
	}
	return errs
}

// disputeOptionKeep and disputeOptionChange are the fixed Keep/Change chip
// texts a dispute question offers the owner (ticket 72 task 4, owner
// decision Q4), the same shape as gateQuestionMessage's Approve/Reject
// pair above.
const (
	disputeOptionKeep   = "Keep the plan: the planner's reason holds"
	disputeOptionChange = "Change the plan: the finding stands"
)

// disputeQuestionMessages builds one open planning question per disputed
// disposition in ds, in plan order, each naming the finding it disputes
// (design section 6.5, ticket 72 task 4). Key is left empty for
// CommitHandlerResult's own fillQuestionKeyTx to allocate, the same
// convention questionMessagesFor and gateQuestionMessage use. Recommended
// is "b" (change the plan), since an above-floor finding stands unless the
// owner agrees with the planner's reason. required is dispositionsRequired's
// result for this same ready plan, so byID always has an entry for every
// disputed finding reaching here: checkDispositions has already rejected
// any disposition naming an id outside required.
func disputeQuestionMessages(ticketID int64, required []response.Finding, ds []response.Disposition) ([]store.Message, error) {
	byID := make(map[string]response.Finding, len(required))
	for i := range required {
		byID[required[i].ID] = required[i]
	}
	var msgs []store.Message
	for _, disp := range ds {
		if disp.Kind != response.DispositionDisputed {
			continue
		}
		f := byID[disp.Finding]
		payload, err := json.Marshal(response.QuestionPayload{
			Kind:        response.QuestionKindQuestion,
			State:       response.QuestionStateOpen,
			Recommended: "b",
			Options: []response.Option{
				{Key: "a", Text: disputeOptionKeep},
				{Key: "b", Text: disputeOptionChange},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("job: marshal dispute question %s: %w", disp.Finding, err)
		}
		body := fmt.Sprintf(
			"Plan review finding %s is disputed\n\nThe reviewer raised this %s finding at %s:\n\n%s\n\nSuggested fix: %s\n\nThe planner disputes it:\n\n%s",
			f.ID, f.Severity, f.Location, f.Text, f.Fix, disp.Reason)
		msgs = append(msgs, store.Message{
			TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing,
			State: new(questionStateOpen), Body: body, Payload: payload,
		})
	}
	return msgs, nil
}

// resolveCapResumesEscalation is section 6.7's cap_resumes retry/back row
// (design D17): the exhausted session guarantees resumeOrFresh's own
// SessionOpen branch is unreachable, so this calls the planning first turn
// (section 6.2) directly, carrying notes and error. D31 (design section
// 22.4) dropped this function's own planning-round-preserving loop: no
// planning question reaches AnsweredRounds any more, so there is never a
// round left to fold in here -- runPlanningFirst's own fresh transcript
// (conversationFreshInput) now carries every one of the exhausted session's
// threads instead.
func resolveCapResumesEscalation(ctx context.Context, t store.Ticket, d Deps, notes, errorText string, resolveIDs []int64) (store.HandlerCommit, error) {
	extra := []prompt.NamedInput{prompt.Notes(notes), prompt.Error(errorText)}
	return runPlanningFirst(ctx, t, d, extra, resolveIDs)
}

// gateApprove is section 6.6's approve pre-check, in exact branch order
// (design D16, widened by D32, design section 22.12.1, 22.12.3a): branch 0
// (two seal-mismatch markers for the cohort) is evaluated once the cohort's
// run id is known, but wins over branches 3-6; every failing branch (0, 1,
// 2, 3, 6) escalates seal_failed with RunID nil, Origin seal, and resolves
// resolveIDs; branch 4 seals the cohort and moves to building; branch 5
// (already consistently sealed) moves to building with no new seal. D32
// inserts one more check, after branches 1 and 2 (no cohort, no producing
// run): the cohort's current plan version must be confirmed
// (ConfirmedApprovalForVersion), or this escalates seal_failed too, with
// What "no confirmation for the current plan" -- the retry path (a
// seal_failed or gate_approve escalation choosing retry, design section
// 22.12.3) has no round in hand, so it re-derives the same GateApproval
// this read builds, rather than carry one in from its caller.
// GateApproveSealRaceHook, nil in production, is a test-only seam (design
// D16's own TOCTOU commentary): called with the cohort's run id right after
// this function's own read of CohortSealState, so a test can seal one row
// through a second store handle in the window between that read and the
// commit this function builds from it.
func gateApprove(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64) (store.HandlerCommit, error) {
	cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: current cohort: %w", err)
	}
	if !ok {
		return sealFailedEscalation(t, d, sealFailedNoCohortWhat, resolveIDs), nil
	}
	if cohort.RunID == nil {
		return sealFailedEscalation(t, d, sealFailedNoRunWhat, resolveIDs), nil
	}
	runID := *cohort.RunID

	approval, confirmed, err := d.Store.ConfirmedApprovalForVersion(ctx, t.ID, cohort.PlanVersion)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: confirmed approval: %w", err)
	}
	if !confirmed {
		return sealFailedEscalation(t, d, sealFailedNoConfirmationWhat, resolveIDs), nil
	}

	mismatches, err := d.Store.CountSealMismatches(ctx, t.ID, runID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: count seal mismatches: %w", err)
	}
	if mismatches >= 2 {
		return sealFailedEscalation(t, d, sealFailedMismatchTwiceWhat, resolveIDs), nil
	}

	total, sealed, commonAt, err := d.Store.CohortSealState(ctx, t.ID, runID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: cohort seal state: %w", err)
	}
	if GateApproveSealRaceHook != nil {
		GateApproveSealRaceHook(runID)
	}
	if total < minReadyScenarios || total > maxReadyScenarios {
		what := fmt.Sprintf(sealFailedBadCountWhatFmt, total)
		return sealFailedEscalation(t, d, what, resolveIDs), nil
	}

	c := baseCommit(t, d)
	c.ResolveQuestions = resolveIDs
	switch {
	case sealed == 0:
		c.Seal = &store.SealRequest{RunID: runID, PlanVersion: cohort.PlanVersion, ExpectedCount: total, At: time.Now().UTC()}
		c.GateApproval = &approval
		c.Next = stateBuilding
		c.Reason = reasonGateApproved
		slog.Info("gate approved", "ticket_id", t.ID, "run_id", runID, "plan_version", cohort.PlanVersion)
		slog.Info("scenarios sealed", "ticket_id", t.ID, "cohort_run_id", runID, "plan_version", cohort.PlanVersion,
			"count", total, "sealed_at", c.Seal.At)
		return c, nil
	case sealed == total && commonAt != nil:
		c.GateApproval = &approval
		c.Next = stateBuilding
		c.Reason = reasonGateApprovedAlready
		slog.Info("gate already sealed", "ticket_id", t.ID, "run_id", runID, "plan_version", cohort.PlanVersion)
		return c, nil
	default:
		what := fmt.Sprintf(sealFailedPartialWhatFmt, sealed, total)
		return sealFailedEscalation(t, d, what, resolveIDs), nil
	}
}

// GateApproveSealRaceHook is a test-only seam (design D16, section 6.6
// branch 0). Production code never sets it; see gateApprove's own comment
// for exactly when it runs.
var GateApproveSealRaceHook func(runID int64)

// sealFailedEscalation builds every failing gate-approve branch's commit
// (design section 6.6): RunID nil (no run caused this, the pre-check did),
// Origin seal, code seal_failed, and resolveIDs resolved in the same commit.
func sealFailedEscalation(t store.Ticket, d Deps, what string, resolveIDs []int64) store.HandlerCommit {
	c := escalationCommit(t, d, nil, nil, string(response.EscalationCodeSealFailed), what, sealFailedWhy, "", response.EscalationOriginSeal)
	c.ResolveQuestions = resolveIDs
	return c
}

// ---- D32: the gate's confirming turn (design section 22.12.3) ------------

// gateConfirmEntry builds and runs the gate's own confirming turn from
// scratch: it reads the cohort's producing session (the error
// `job: gate: cohort session <id> has no external id` when that session
// was never externalized, a bug since a ready commit always stores one),
// then carries notes (the approval's own reply text, only when non-empty),
// the D14 invalid-output retry, a live validation-errors marker, and the
// undelivered conversation, in that priority order (design section 22.12.3,
// 22.4's own entry-step order), charging the resume (BumpResumes) only when
// an agent-driven reason rode along.
func gateConfirmEntry(ctx context.Context, t store.Ticket, d Deps, gateQID, approveAID int64, cohort store.Cohort, notes string) (store.HandlerCommit, error) {
	maxResumes := d.Machine.Jobs[jobPlanningName].MaxResumes
	run, err := d.Store.RunByID(ctx, *cohort.RunID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: cohort run %d: %w", *cohort.RunID, err)
	}
	sess, _, err := d.Store.SessionByID(ctx, run.SessionID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: cohort session: %w", err)
	}
	if sess.ExternalID == nil || *sess.ExternalID == "" {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: cohort session %d has no external id", sess.ID)
	}

	var inputs []prompt.NamedInput
	if notes != "" {
		inputs = append(inputs, prompt.Notes(notes))
	}

	n, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobPlanningName, &sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: consecutive invalid outputs: %w", err)
	}
	if n == 1 {
		inputs = append(inputs, prompt.Invalid(invalidRetryText(reason)))
		conv, convErr := d.Store.PlanningConversation(ctx, t.ID)
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: gate: planning conversation: %w", convErr)
		}
		convExtra, throughBatch := conversationResumeInput(conv)
		inputs = append(inputs, convExtra...)
		return runGateConfirm(ctx, t, d, sess, inputs, n, true, conv, throughBatch, gateQID, approveAID, cohort.PlanVersion)
	}

	if m, live, liveErr := d.Store.LiveMarker(ctx, t.ID, validationErrorsPendingPrefix, validationErrorsDeliveredPrefix); liveErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: live marker: %w", liveErr)
	} else if live {
		firstLine, errsText, _ := strings.Cut(m.Body, "\n")
		rid := strings.TrimPrefix(firstLine, validationErrorsPendingPrefix+" run ")
		inputs = append(inputs, prompt.Validation(errsText))
		conv, convErr := d.Store.PlanningConversation(ctx, t.ID)
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: gate: planning conversation: %w", convErr)
		}
		convExtra, throughBatch := conversationResumeInput(conv)
		inputs = append(inputs, convExtra...)
		commit, runErr := runGateConfirm(ctx, t, d, sess, inputs, 0, true, conv, throughBatch, gateQID, approveAID, cohort.PlanVersion)
		if runErr != nil {
			return commit, runErr
		}
		commit.Messages = append(commit.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: validationErrorsDeliveredPrefix + " run " + rid,
		})
		return commit, nil
	}

	conv, err := d.Store.PlanningConversation(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: planning conversation: %w", err)
	}
	convExtra, throughBatch := conversationResumeInput(conv)
	inputs = append(inputs, convExtra...)
	// A confirming turn triggered by the owner's own Approve, carrying
	// nothing agent-driven, is a free resume (design section 22.12.3,
	// 22.4's charging table): BumpResumes stays false even when it also
	// carries notes, since notes are the owner's own text, not a reason the
	// agent itself produced.
	return runGateConfirm(ctx, t, d, sess, inputs, 0, false, conv, throughBatch, gateQID, approveAID, cohort.PlanVersion)
}

// runGateConfirm assembles and runs the confirming turn's own prompt
// (prompt.ConfirmHeader in place of a job prompt, confirmSchemas in place
// of planningSchemas) and routes its result through
// confirmingTurnSuccessCommit. gateQID, approveAID, and planVersion carry
// through to that commit's own confirmed marker and, for every other
// outcome, the cancellation marker and gate resolution (design section
// 22.12.3). resumeCharge (job.go, design D5, section 7.4), read from sess's
// own newest run, ANDs its own bump into charge and, when that newest run
// is interrupted, appends the interrupted input -- the gate's own "already
// free" confirming turn (gateConfirmEntry's plain branch) is where this
// actually fires, design section 7.4's "gate confirming turn" row.
func runGateConfirm(
	ctx context.Context, t store.Ticket, d Deps, sess store.Session, inputs []prompt.NamedInput, priorInvalid int, charge bool,
	conv store.PlanningConversation, throughBatch int64, gateQID, approveAID int64, planVersion int,
) (store.HandlerCommit, error) {
	newestRun, foundRun, err := d.Store.SessionNewestRun(ctx, sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: confirm: newest run: %w", err)
	}
	if foundRun {
		bump, _ := resumeCharge(newestRun)
		charge = charge && bump
		if newestRun.Interrupted {
			inputs = append(append([]prompt.NamedInput{}, inputs...), prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false})
		}
	}

	schemas, err := confirmSchemas()
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: gate: confirm: %w", err)
	}

	in := prompt.ForPlanningConfirm(inputs)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: charge}
	req := runtime.RunRequest{Job: response.JobPlanning, SessionID: *sess.ExternalID, Prompt: assembled}
	sessionRecord := func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, rr) }
	return runAndRoute(ctx, d, t, jobPlanningName, su, req, priorInvalid, sessionRecord, nil, response.EscalationOriginGateApprove,
		func(rr runResult) (store.HandlerCommit, error) {
			return confirmingTurnSuccessCommit(ctx, t, d, rr, sessionRecord(rr), conv, throughBatch, gateQID, approveAID, planVersion)
		}, nil, throughBatch)
}

// confirmingTurnSuccessCommit routes the confirming turn's parsed response
// (design section 22.12.3's own outcome table): confirmed is the turn's own
// clean answer (confirmedOutcomeCommit); questions, ready, and error reuse
// planning's ordinary routing, then cancelApprovalCommit adds the
// cancellation marker and resolves the gate question, since the owner's
// approval no longer has anything left to seal. checkConversation runs
// first, with confirming=true, exactly like planningSuccessCommit's own
// Layer 2 gate.
func confirmingTurnSuccessCommit(
	ctx context.Context, t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert,
	conv store.PlanningConversation, throughBatch int64, gateQID, approveAID int64, planVersion int,
) (store.HandlerCommit, error) {
	resp := rr.Res.Response
	rl, hasReplies := resp.(replyLister)
	if hasReplies {
		if errs := checkConversation(resp.Header().Outcome, rl.ReplyList(), buildThreadState(conv), true); len(errs) > 0 {
			return conversationValidationErrorCommit(t, d, rr, sessionCommit, nil, errs), nil
		}
	}

	switch r := resp.(type) {
	case *response.ConfirmedResponse:
		return confirmedOutcomeCommit(t, d, rr, r, sessionCommit, conv, throughBatch, gateQID, approveAID, planVersion), nil
	case *response.PlanningQuestionsResponse:
		c, err := questionOutcomeCommit(t, d, rr, r.Questions, sessionCommit, nil)
		if err != nil {
			return c, err
		}
		return cancelApprovalCommit(t, c, rr, gateQID, rl, conv, throughBatch), nil
	case *response.ReadyResponse:
		c, err := readyCommit(ctx, t, d, rr, r, sessionCommit, nil)
		if err != nil {
			return c, err
		}
		return cancelApprovalCommit(t, c, rr, gateQID, rl, conv, throughBatch), nil
	case *response.ErrorResponse:
		c := errorOutcomeCommit(t, d, rr, r, sessionCommit, nil, response.EscalationOriginGateApprove)
		return cancelApprovalCommit(t, c, rr, gateQID, nil, conv, throughBatch), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: gate: confirming turn outcome %s not handled", resp.Header().Outcome)
	}
}

// confirmingMarkerBody renders the exact body checkGateApprovalTx (store
// package) parses back (design section 22.12.1): "gate confirmed run <R>
// plan v<V> gate <QID> answer <AID>".
func confirmingMarkerBody(runID int64, planVersion int, gateQID, approveAID int64) string {
	return fmt.Sprintf("gate confirmed run %d plan v%d gate %d answer %d", runID, planVersion, gateQID, approveAID)
}

// confirmedOutcomeCommit is the confirming turn's own clean answer (design
// section 22.12.3's table row "confirmed"): the run terminalizes "ok", the
// gate question stays "answered" (no ResolveQuestions: it is not done until
// the seal itself resolves it), and the commit writes the confirming
// marker binding this run, the cohort's plan version, and the gate's own
// QID/AID -- the next tick's enterFromGateRound finds it confirmed through
// ConfirmedApprovalForVersion and calls gateApprove.
func confirmedOutcomeCommit(
	t store.Ticket, d Deps, rr runResult, resp *response.ConfirmedResponse, sessionCommit *store.SessionUpsert,
	conv store.PlanningConversation, throughBatch int64, gateQID, approveAID int64, planVersion int,
) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeOk))
	c.Session = sessionCommit
	c.Messages = []store.Message{{
		TicketID: t.ID, ParentID: &gateQID, Type: msgTypeUpdate, Author: authorSystem,
		Body: confirmingMarkerBody(rr.Reserved.RunID, planVersion, gateQID, approveAID),
	}}
	msgs, cc := conversationEffects(t.ID, rr.Reserved.RunID, throughBatch, resp.Replies, conv)
	c.Messages = append(c.Messages, msgs...)
	c.Conversation = &cc
	slog.Info("gate confirmed", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "question_id", gateQID, "plan_version", planVersion)
	return c
}

// cancelApprovalCommit augments c -- an ordinary questions, ready, or error
// outcome commit the confirming turn produced -- with design section
// 22.12.3's own cancellation side effects: the gate question resolves
// (ResolveQuestions) and a cancellation marker records that the agent
// itself found something open, so the owner gets a fresh gate once
// planning reaches ready again. rl is nil for the error outcome, which
// carries no Conversation and so needs no conversationEffects.
func cancelApprovalCommit(
	t store.Ticket, c store.HandlerCommit, rr runResult, gateQID int64, rl replyLister,
	conv store.PlanningConversation, throughBatch int64,
) store.HandlerCommit {
	c.ResolveQuestions = append(c.ResolveQuestions, gateQID)
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, ParentID: &gateQID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("gate approval cancelled gate %d run %d", gateQID, rr.Reserved.RunID),
	})
	if rl != nil {
		msgs, cc := conversationEffects(t.ID, rr.Reserved.RunID, throughBatch, rl.ReplyList(), conv)
		c.Messages = append(c.Messages, msgs...)
		c.Conversation = &cc
	}
	slog.Info("gate approval cancelled", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "question_id", gateQID)
	return c
}

// ---- shared failure and escalation commit builders ------------------------

// runAndRoute is the one seam every runJob call passes through on its way to
// a commit (design F025): runClassify, runPlanningFirst, runPlanningResume,
// and runPlanReview differ only in the request they build and the success
// builder they route a clean run through, so this owns their identical
// tail. sessionRecord builds the Session field from the runResult runJob
// hands back (freshSessionRecord for a fresh call, a closure over
// resumeSessionRecord for a resume); success is the caller's own outcome
// router (classifySuccessCommit, planningSuccessCommit, or
// planReviewSuccessCommit), already bound to whatever else it needs.
//
// runJob's Reserve is the one write that can leave a run with a NULL
// outcome if nothing downstream ever terminalizes it (design D13): a
// runtime failure is already terminalized by routeFailure's own exec- and
// invalid-output branches, but a success builder's own post-run failure --
// a store read, a filesystem open, a json.Marshal -- used to escape as a
// plain error the dispatcher's error path could only release, never
// terminalize, orphaning the run forever. This seam catches every error
// that surfaces once Reserve has already run -- routeFailure's own
// "unrecognized" fallback and success's own error alike -- and funnels it
// through postRunFailure whenever a run was actually reserved
// (rr.Reserved.RunID != 0). When nothing was reserved (routeFailure already
// returns every pre-reserve case unchanged, and an unrecognized pre-reserve
// error does too), this returns the plain error unchanged, so the
// dispatcher's own releaseClaim path still runs and clears the claim.
// taskN is threaded straight through to runJob, and from there into
// RunSeed.TaskN: &n for a build or perimeter task unit, nil otherwise.
// Planning's four callers pass nil. throughBatch is threaded straight
// through to runJob, and from there into RunSeed.ThroughBatch (D31, design
// section 22.3): above 0 only for a planning resume or first turn that
// found PlanningConversation.Undelivered() non-empty; every other caller
// passes 0.
func runAndRoute(
	ctx context.Context, d Deps, t store.Ticket, jobName string,
	su store.SessionUpsert, req runtime.RunRequest, priorInvalid int,
	sessionRecord func(runResult) *store.SessionUpsert,
	resolveIDs []int64, origin response.EscalationOrigin,
	success func(rr runResult) (store.HandlerCommit, error),
	taskN *int, //nolint:unparam // planning's four callers pass nil; the building handler (task 9) passes &n
	throughBatch int64,
) (store.HandlerCommit, error) {
	rr, runErr := runJob(ctx, d, t, jobName, su, req, taskN, nil, throughBatch)
	sessionCommit := sessionRecord(rr)

	if runErr != nil {
		if c, ok, failErr := routeFailure(t, d, rr, runErr, priorInvalid, sessionCommit, resolveIDs, origin); ok {
			return c, failErr
		}
		wrapped := fmt.Errorf("job: %s: unrecognized runJob error: %w", jobName, runErr)
		if rr.Reserved.RunID != 0 {
			return postRunFailure(t, d, rr, sessionCommit, resolveIDs, origin, wrapped), nil
		}
		return store.HandlerCommit{}, wrapped
	}

	// errNothingToDoClaimNotFalse is nothingToDoCommit's own defensive
	// sentinel, not a post-run infrastructure failure: response.Validate
	// already guarantees a real runtime's response can never trip it, so it
	// only ever fires against a test's scripted runtime standing in for a
	// compromised or buggy agent process, and it must keep surfacing loud (a
	// bare error, no commit) rather than get smoothed into an owner-facing
	// escalation the same as a store or filesystem failure would.
	commit, err := success(rr)
	if err != nil && rr.Reserved.RunID != 0 && !errors.Is(err, errNothingToDoClaimNotFalse) {
		return postRunFailure(t, d, rr, sessionCommit, resolveIDs, origin, err), nil
	}
	return commit, err
}

// postRunFailedWhy is postRunFailure's own fixed Why text (design F025):
// unlike every other escalation this file writes, the run itself succeeded --
// what failed is Zing's own post-run bookkeeping, the same sentence
// regardless of which step's success builder hit it.
const postRunFailedWhy = "the agent's turn completed, but Zing could not store or check its result"

// postRunFailedWhatFor renders postRunFailure's own short, owner-facing What
// sentence naming the failing step (design F025). classify, planning_first,
// planning_resume, and planreview are the origins the four runAndRoute
// callers thread through today; build, fix, and perimeter are handled here
// ahead of their own callers (design section 4.1, Package 8); review,
// judge, and respond are handled here the same way, ahead of Package 9's
// own callers (design section 4.1).
func postRunFailedWhatFor(origin response.EscalationOrigin) string {
	switch origin {
	case response.EscalationOriginClassify:
		return "classifying the ticket"
	case response.EscalationOriginPlanningFirst, response.EscalationOriginPlanningResume:
		return "storing or checking the plan"
	case response.EscalationOriginPlanreview:
		return "storing the plan review"
	case response.EscalationOriginBuild, response.EscalationOriginFix:
		return "storing or checking the build result"
	case response.EscalationOriginPerimeter:
		return "storing the perimeter description"
	case response.EscalationOriginReview:
		return "storing the review findings"
	case response.EscalationOriginJudge:
		return "storing or checking the verdicts"
	case response.EscalationOriginRespond:
		return "storing the thread actions"
	default:
		return "storing or checking the agent's result"
	}
}

// postRunFailure is runAndRoute's own terminalizing commit for any error
// surfacing once runJob has already reserved a run (design F025, section
// 6.8): the run terminalizes as an error, the session records the same way
// every other terminalizing commit does, and an escalation carries the new
// post_run_failed code, err's own text as Tried, and origin exactly as
// runAndRoute threaded it through -- the producing step's own origin, not a
// new one invented here.
func postRunFailure(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin, err error) store.HandlerCommit {
	slog.Error("post-run failure", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "job", string(origin), "err", err)

	c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
		string(response.EscalationCodePostRunFailed), postRunFailedWhatFor(origin), postRunFailedWhy, err.Error(), origin)
	c.Runs = terminalRuns(rr, string(response.OutcomeError))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	return c
}

// schemaInvalidOrigins maps a ticket's state to the escalation origin
// SchemaInvalidEscalation gives a schema-invalid commit for that state
// (design section "dispatcher"): the state names the step whose handler
// built the rejected payload, in each case the one that handles that
// state's escalation with no run (RunID nil).
var schemaInvalidOrigins = map[string]response.EscalationOrigin{
	stateQueued:    response.EscalationOriginClassify,
	statePlanning:  response.EscalationOriginPlanningResume,
	stateBuilding:  response.EscalationOriginBuild,
	stateReviewing: response.EscalationOriginReview,
	stateJudging:   response.EscalationOriginJudge,
	stateShipping:  response.EscalationOriginShipping,
}

// SchemaInvalidEscalation turns a commit the store refused as
// schema-invalid into a post_run_failed escalation for the same ticket and
// lease (design section "dispatcher"): the dispatcher's own schema-invalid
// branch in runAndCommit calls this ahead of falling back to fail-closed.
// ok is false for a state with no origin, so the caller fails closed. The
// escalation's RunID is left nil (unlike postRunFailure, which names the
// run that failed), so each job's Retry takes its existing no-run path
// rather than trying to resume a run whose result was never stored.
func SchemaInvalidEscalation(t store.Ticket, failed store.HandlerCommit, err error) (store.HandlerCommit, bool) {
	origin, ok := schemaInvalidOrigins[t.State]
	if !ok {
		return store.HandlerCommit{}, false
	}

	var runs []store.Run
	ids := make([]string, 0, len(failed.Runs))
	for _, r := range failed.Runs {
		if r.ID <= 0 {
			continue
		}
		o := string(response.OutcomeError)
		runs = append(runs, store.Run{ID: r.ID, Turn: r.Turn, Outcome: &o, ExitCode: r.ExitCode, AgentSeconds: r.AgentSeconds})
		ids = append(ids, strconv.FormatInt(r.ID, 10))
	}
	runText := noneLiteral
	if len(ids) != 0 {
		runText = strings.Join(ids, ", ")
	}

	d := Deps{Owner: failed.Owner, Expires: failed.Expires}
	c := escalationCommit(t, d, nil, nil, string(response.EscalationCodePostRunFailed),
		postRunFailedWhatFor(origin), postRunFailedWhy, err.Error()+"\nrun ids: "+runText, origin)
	c.Runs = runs
	if failed.Session != nil && failed.Session.ID != nil {
		c.Session = failed.Session
	}
	for _, su := range failed.Sessions {
		if su.ID != nil {
			c.Sessions = append(c.Sessions, su)
		}
	}
	c.ResolveQuestions = failed.ResolveQuestions
	return c, true
}

// routeFailure builds the commit for every runJob failure classify, the
// planning first turn, and its resume all handle alike (design section 5.4,
// 6.7, 6.8): the pre-reserve failures (ErrBudget, ErrConfig,
// store.ErrClaimLost) either escalate (ErrBudget) or return unchanged;
// runtime.ErrCanceled returns with no commit at all (design D13: the
// dispatcher leaves the claim for ExpireClaims to reconcile); an exec
// failure (ErrStart, ErrTimeout, ErrStalled, ErrOutputTooLarge,
// *runtime.ExecError) terminalizes the run and escalates
// runtime_exec_failed; an invalid output applies D14. ok is false when
// runErr names none of these, so the caller can report it as a bug rather
// than silently dropping it.
func routeFailure(
	t store.Ticket, d Deps, rr runResult, runErr error, priorInvalid int,
	sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin,
) (commit store.HandlerCommit, ok bool, err error) {
	switch {
	case errors.Is(runErr, runtime.ErrCanceled), claudeCapped(runErr):
		return store.HandlerCommit{}, true, runErr
	case errors.Is(runErr, ErrBudget):
		return budgetEscalationCommit(t, d, resolveIDs), true, nil
	case errors.Is(runErr, ErrSandbox):
		return sandboxEscalationCommit(t, d, resolveIDs, origin, d.Sandboxes.Build.Reason()), true, nil
	case errors.Is(runErr, ErrConfig), errors.Is(runErr, store.ErrClaimLost):
		return store.HandlerCommit{}, true, runErr
	}

	var invErr *runtime.InvalidOutputError
	if errors.As(runErr, &invErr) { //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags
		return invalidOutputCommit(t, d, rr, invErr, priorInvalid, sessionCommit, resolveIDs, origin), true, nil
	}
	if isExecFailure(runErr) {
		return execFailureCommit(t, d, rr, sessionCommit, resolveIDs, origin), true, nil
	}
	return store.HandlerCommit{}, false, nil
}

// execFailureSentinels are four of the five runtime failures section 6.8
// escalates as runtime_exec_failed: the process could not start, the job
// deadline killed it, Claude's idle watchdog killed a stalled run, or its
// output exceeded the 4 MiB cap. The fifth, exiting with no parseable
// result, is runtime.ExecError, which isExecFailure matches separately.
var execFailureSentinels = []error{runtime.ErrStart, runtime.ErrTimeout, runtime.ErrStalled, runtime.ErrOutputTooLarge}

// isExecFailure reports whether err is one of the five runtime failures
// section 6.8 escalates as runtime_exec_failed: the process could not
// start, the job deadline killed it, Claude's idle watchdog killed a
// stalled run, its output exceeded the 4 MiB cap, or it exited with no
// parseable result.
func isExecFailure(err error) bool {
	for _, sentinel := range execFailureSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	var execErr *runtime.ExecError
	return errors.As(err, &execErr) //nolint:modernize // see routeFailure's comment
}

// terminalRuns is the one Runs entry every committed path after runJob's
// Reserve terminalizes (design section 4.6, 6.8): the reserved run, its
// outcome, its real exit code, and its agent seconds (runtime.Seconds,
// rounded up, minimum 1).
//
// runs.outcome has no "replies" or "confirmed" value (section 22.2,
// 22.12.3), so storedRunOutcome maps those two to the values their own
// commits store. A path that passes a response's own outcome through, such
// as conversationValidationErrorCommit, would otherwise fail the CHECK and
// stop the dispatcher.
func terminalRuns(rr runResult, outcome string) []store.Run {
	exitCode := rr.Res.ExitCode
	agentSeconds := runtime.Seconds(rr.Res.AgentTime)
	o := storedRunOutcome(outcome)
	return []store.Run{{ID: rr.Reserved.RunID, Turn: rr.Reserved.Turn, Outcome: &o, ExitCode: &exitCode, AgentSeconds: &agentSeconds}}
}

// storedRunOutcome is the runs.outcome value for a response outcome:
// replies stores "question" (repliesOutcomeCommit), confirmed stores "ok"
// (the confirming turn's own commit), and every other outcome stores
// itself.
func storedRunOutcome(outcome string) string {
	switch response.Outcome(outcome) {
	case response.OutcomeReplies:
		return string(response.OutcomeQuestion)
	case response.OutcomeConfirmed:
		return string(response.OutcomeOk)
	default:
		return outcome
	}
}

// freshSessionRecord is the Session field a fresh (non-resume) terminalizing
// commit carries (design D13): nil when the runtime never learned a session
// id (only reachable on runtime.ErrStart), else the reserved session, with
// its external_id filled in.
func freshSessionRecord(rr runResult) *store.SessionUpsert {
	if rr.Res.SessionID == "" {
		return nil
	}
	ext := rr.Res.SessionID
	return &store.SessionUpsert{ID: &rr.Reserved.SessionID, ExternalID: &ext}
}

// resumeSessionRecord is the Session field a resume's terminalizing commit
// always carries (design section 4.2, 6.4): BumpResumes is no longer set
// here -- Reserve already charged this resume when it reserved the run, so
// setting it again at the terminalizing commit would charge it twice;
// ExternalID is filled in only when the runtime returned one (it is already
// set on an ordinary resume, and upsertSessionTx's own "WHERE external_id IS
// NULL" guard makes re-sending it a no-op).
func resumeSessionRecord(sessionID int64, rr runResult) *store.SessionUpsert {
	su := &store.SessionUpsert{ID: &sessionID}
	if rr.Res.SessionID != "" {
		ext := rr.Res.SessionID
		su.ExternalID = &ext
	}
	return su
}

// escalationCommit builds the section 6.7 Write shape: the escalation
// message and its linked question, with Waiting always "questions" (every
// escalation, cap or run-caused alike, leaves the ticket waiting on the
// owner's retry/planning/abandon choice). Callers that also terminalize a
// run add Runs, Session, and ResolveQuestions themselves.
func escalationCommit(t store.Ticket, d Deps, runID, sessionID *int64, code, what, why, tried string, origin response.EscalationOrigin) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Escalation = &store.EscalationCommit{
		RunID: runID,
		Body:  code + ": " + what,
		Payload: response.EscalationPayload{
			Code: code, What: what, Why: why, Tried: tried,
			Options: escalationOptions, SessionID: sessionID, Origin: string(origin),
		},
	}
	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c
}

// budgetEscalationCommit is ErrBudget's commit (design section 6.8): no run
// was ever reserved (runJob's budget check, step 4, runs before Reserve), so
// RunID and SessionID are both nil.
func budgetEscalationCommit(t store.Ticket, d Deps, resolveIDs []int64) store.HandlerCommit {
	c := escalationCommit(t, d, nil, nil, string(response.EscalationCodeWallClock), budgetExhaustedWhat, budgetExhaustedWhy, "", response.EscalationOriginCapBudget)
	// Resolve the answered round that triggered this run atomically with the
	// escalation, exactly as the sibling exec/error escalations do; otherwise
	// budget exhaustion leaves that gate or planning round open forever.
	c.ResolveQuestions = resolveIDs
	return c
}

// sandboxUnavailableWhat is the sandbox_unavailable escalation's own fixed
// What text (design section 5.5, 6.4): reason is the sandbox's own Reason(),
// carried as Why, so the owner sees exactly which of the four closed reasons
// (section 5.4) is blocking every build and perimeter tick.
const sandboxUnavailableWhat = "the build sandbox did not load"

// sandboxEscalationCommit is ErrSandbox's commit (design section 5.5): no
// run was ever reserved (runJob's sandbox step runs before Reserve), so
// RunID and SessionID are both nil, exactly like budgetEscalationCommit's
// own pre-reserve shape.
func sandboxEscalationCommit(t store.Ticket, d Deps, resolveIDs []int64, origin response.EscalationOrigin, reason string) store.HandlerCommit {
	c := escalationCommit(t, d, nil, nil, string(response.EscalationCodeSandboxUnavailable), sandboxUnavailableWhat, reason, "", origin)
	c.ResolveQuestions = resolveIDs
	return c
}

// capResumesEscalation is the resumes_exhausted escalation every job's
// exhausted session writes (design D17): RunID is nil (no run caused it,
// the session cap did), SessionID names the exhausted session, and What and
// Why name jobName, that session's own job.
func capResumesEscalation(t store.Ticket, d Deps, jobName string, sessionID int64) store.HandlerCommit {
	return escalationCommit(t, d, nil, &sessionID, string(response.EscalationCodeResumesExhausted),
		fmt.Sprintf(resumesExhaustedWhatFmt, jobName), fmt.Sprintf(resumesExhaustedWhyFmt, jobName), "", response.EscalationOriginCapResumes)
}

// execFailureCommit terminalizes the reserved run as an error and escalates
// runtime_exec_failed (design section 6.8): RunID and SessionID are both
// set, since a run always caused this. Tried is rr.Res.FailureDetail: empty
// for every failure kind but a Codex ExecError with no final message, which
// is the one case the runtime has anything to quote (design: "Codex runs
// that exit 1 within seconds leave no stderr, transcript, or cause").
func execFailureCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin) store.HandlerCommit {
	c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
		string(response.EscalationCodeRuntimeExecFailed), runtimeExecFailedWhat, runtimeExecFailedWhy, rr.Res.FailureDetail, origin)
	c.Runs = terminalRuns(rr, string(response.OutcomeError))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	return c
}

// errorOutcomeCommit terminalizes the reserved run as an error and
// escalates with the agent's own code, why, and tried (design section 6.8's
// "universal error" row).
func errorOutcomeCommit(t store.Ticket, d Deps, rr runResult, errResp *response.ErrorResponse, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin) store.HandlerCommit {
	c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
		string(errResp.Error.Code), errResp.Error.What, errResp.Error.Why, errResp.Error.Tried, origin)
	c.Runs = terminalRuns(rr, string(response.OutcomeError))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	return c
}

// invalidOutputCommit is D14 (design section 5.4): the run terminalizes as
// an error and this commit writes the "response invalid run <rid>" marker;
// the ticket stays put, not waiting. When priorInvalid (the count the
// caller read before this run) is already 1, this run is the second
// consecutive invalid output, and the same commit also escalates
// response_invalid, which is why it, uniquely among this file's failure
// commits, sometimes carries no Waiting and sometimes does.
func invalidOutputCommit(t store.Ticket, d Deps, rr runResult, invErr *runtime.InvalidOutputError, priorInvalid int, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeError))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: invalidMarkerBody(rr.Reserved.RunID, invErr),
	}}

	if priorInvalid == 1 {
		runID, sessionID := rr.Reserved.RunID, rr.Reserved.SessionID
		c.Escalation = &store.EscalationCommit{
			RunID: &runID,
			Body:  string(response.EscalationCodeResponseInvalid) + ": " + responseInvalidWhat,
			Payload: response.EscalationPayload{
				Code: string(response.EscalationCodeResponseInvalid), What: responseInvalidWhat, Why: invErr.Reason,
				Options: escalationOptions, SessionID: &sessionID, Origin: string(origin),
			},
		}
		waiting := waitingFlagQuestions
		c.Waiting = &waiting
	}
	return c
}

// questionOutcomeCommit is the universal question outcome (design section
// 6.8), shared by classify, planreview, and planning: one question message
// per questions, attached to the terminalized run, waiting on "questions".
// It takes the question slice itself, not a response type, because
// planning's own question outcome decodes to *response.PlanningQuestionsResponse
// (design section 22.2, D31) while every other job's still decodes to
// *response.QuestionResponse; both carry the same Questions shape.
func questionOutcomeCommit(t store.Ticket, d Deps, rr runResult, questions []response.Question, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	msgs, err := questionMessagesFor(t.ID, questions)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeQuestion))
	c.Session = sessionCommit
	c.Messages = msgs
	c.AttachRunToMsgs = true
	c.ResolveQuestions = resolveIDs
	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c, nil
}

// questionMessagesFor builds one question message per q in qs (design
// section 6.6's mapping table, reused by classify and planning alike, F030):
// the stored QuestionPayload leaves Key empty for CommitHandlerResult's own
// fillQuestionKeyTx to allocate in commit order (design section 4.5, 6.7:
// "gate and planning questions use the same Q<n> allocation" as an
// escalation's linked question and a gate question, gateQuestionMessage
// above), so a planning or classify batch's keys can never collide with one
// allocated some other way; q.Key (the model's own wire-format key) is never
// stored, since routing is by message id, not by that key. Recommended and
// Options map straight across; Kind and State are fixed at insert; the
// message Body carries Title as the heading, then Body.
func questionMessagesFor(ticketID int64, qs []response.Question) ([]store.Message, error) {
	msgs := make([]store.Message, 0, len(qs))
	for i, q := range qs {
		// A free-text question has no options. The question schema wants an
		// array, and a null fails the whole commit.
		options := q.Options
		if options == nil {
			options = []response.Option{}
		}
		payload, err := json.Marshal(response.QuestionPayload{
			Kind:        response.QuestionKindQuestion,
			State:       response.QuestionStateOpen,
			Recommended: q.Recommended,
			Options:     options,
		})
		if err != nil {
			return nil, fmt.Errorf("job: marshal question payload %d: %w", i, err)
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

// ---- rendering a round's answers into prompt inputs -----------------------

// questionIDs returns round's question ids, in the order AnsweredRounds
// grouped them (ascending, by id).
func questionIDs(round store.Round) []int64 {
	ids := make([]int64, len(round.Questions))
	for i := range round.Questions {
		ids[i] = round.Questions[i].ID
	}
	return ids
}

// groupByParent buckets msgs by ParentID, the shape both an answered
// round's Answers and its Replies arrive in (design section 4.5).
func groupByParent(msgs []store.MessageRow) map[int64][]store.MessageRow {
	out := make(map[int64][]store.MessageRow, len(msgs))
	for i := range msgs {
		if msgs[i].ParentID != nil {
			out[*msgs[i].ParentID] = append(out[*msgs[i].ParentID], msgs[i])
		}
	}
	return out
}

// optionTextFor returns the option text for key among options, or "" if
// key names none of them (a reply-only answer, or a stale option key).
func optionTextFor(options []response.Option, key string) string {
	for _, o := range options {
		if o.Key == key {
			return o.Text
		}
	}
	return ""
}

// renderAnswerText renders one question's key, stored body (title then
// body), every sent answer's chosen option and its text, and every sent
// reply, the shape section 6.3's per-question resume input describes.
// answers arrives in id order (store.messagesByParent); D30: the console
// now lets an already-answered question take a revised draft while the
// ticket still waits, so a question can carry more than one sent answer
// here. The first stays "chose ..."; every later one -- the current pick,
// since SendBatch only ever appends -- is marked "(revised)" so the model
// reads it as superseding the one(s) before it, not as a second, unrelated
// choice.
func renderAnswerText(q store.MessageRow, qp response.QuestionPayload, answers, replies []store.MessageRow) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n", qp.Key, q.Body)
	for i := range answers {
		var ap response.AnswerPayload
		if err := json.Unmarshal(answers[i].Payload, &ap); err == nil && ap.Option != nil {
			if i == 0 {
				fmt.Fprintf(&sb, "chose %s: %s\n", *ap.Option, optionTextFor(qp.Options, *ap.Option))
			} else {
				fmt.Fprintf(&sb, "chose %s: %s (revised)\n", *ap.Option, optionTextFor(qp.Options, *ap.Option))
			}
		}
	}
	for i := range replies {
		fmt.Fprintf(&sb, "reply: %s\n", replies[i].Body)
	}
	return sb.String()
}

// answerInputsForRound builds one prompt.Answer per round.Questions (design
// section 6.3, 6.4): a true resume's own input shape, one labeled block per
// question rather than the single combined block renderRoundAnswers builds
// for a fresh restart.
func answerInputsForRound(round store.Round) ([]prompt.NamedInput, error) {
	answersByQ, repliesByQ := groupByParent(round.Answers), groupByParent(round.Replies)
	out := make([]prompt.NamedInput, 0, len(round.Questions))
	for i := range round.Questions {
		q := &round.Questions[i]
		var qp response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &qp); err != nil {
			return nil, fmt.Errorf("job: unmarshal round question %d payload: %w", q.ID, err)
		}
		out = append(out, prompt.Answer(renderAnswerText(*q, qp, answersByQ[q.ID], repliesByQ[q.ID])))
	}
	return out, nil
}

// renderRoundAnswers renders round's questions and answers into one
// combined block (design section 5.1 steps 1(c) and 1(d): "prompt.Answers
// (rendered round)"), used when a round's answers restart classify or
// planning fresh rather than resuming a session.
func renderRoundAnswers(round store.Round) (string, error) {
	answersByQ, repliesByQ := groupByParent(round.Answers), groupByParent(round.Replies)
	parts := make([]string, 0, len(round.Questions))
	for i := range round.Questions {
		q := &round.Questions[i]
		var qp response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &qp); err != nil {
			return "", fmt.Errorf("job: unmarshal round question %d payload: %w", q.ID, err)
		}
		parts = append(parts, strings.TrimRight(renderAnswerText(*q, qp, answersByQ[q.ID], repliesByQ[q.ID]), "\n"))
	}
	return strings.Join(parts, "\n\n"), nil
}

// ---- small shared helpers --------------------------------------------------

// invalidRetryText is the D14 retry input's fixed wording (design section
// 5.1 step 4): the closed reason sentence wrapped in Zing's own framing,
// never owner- or model-supplied prose, so prompt.Invalid's "raw, never
// fenced" rule still holds.
func invalidRetryText(reason string) string {
	closed, detail, hasDetail := strings.Cut(reason, "\n")
	text := "your final message was not a valid zing document: " + closed + "; return exactly one"
	if hasDetail && detail != "" {
		// detail is the validator's error list from the marker's third line
		// (invalidMarkerBody); it can quote the model's own words, so it is
		// fenced like any other untrusted input.
		text += "\nThe validator's errors, quoted from your document:\n" + fence.Wrap(detail)
	}
	return text
}

// invalidMarkerBody is the "response invalid run <id>" marker's body: the
// closed reason on line two and, from line three on, Detail unchanged --
// for a failed validation, the validator's errors one per line, capped at
// 64 KiB by capDetail (Q1) -- which invalidRetryText fences back into the
// retry prompt and the console's responseInvalidLine shows the owner.
func invalidMarkerBody(runID int64, invErr *runtime.InvalidOutputError) string {
	body := fmt.Sprintf("response invalid run %d\n%s", runID, invErr.Reason)
	if invErr.Detail != "" {
		body += "\n" + invErr.Detail
	}
	return body
}

// readAsset reads path out of the embedded zing.Assets tree (machine.toml's
// own prompt and style paths), the same embed.FS the machine loader
// validates those paths against.
func readAsset(path string) (string, error) {
	data, err := fs.ReadFile(zing.Assets, path)
	if err != nil {
		return "", fmt.Errorf("job: read asset %s: %w", path, err)
	}
	return string(data), nil
}

// renderSchemas renders one response.RenderTemplate per outcome, in order,
// then the two universal outcomes question and error (design section 4.2's
// schema order rule).
func renderSchemas(job response.Job, outcomes ...response.Outcome) ([]string, error) {
	all := append(append([]response.Outcome{}, outcomes...), response.OutcomeQuestion, response.OutcomeError)
	out := make([]string, 0, len(all))
	for _, o := range all {
		s, err := response.RenderTemplate(job, o)
		if err != nil {
			return nil, fmt.Errorf("job: render template %s/%s: %w", job, o, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// planningSchemas is renderSchemas for the planning job's own outcome order
// (design section 4.2): questions, ready, children, nothing_to_do, then
// question, error.
func planningSchemas() ([]string, error) {
	return renderSchemas(response.JobPlanning, response.OutcomeQuestions, response.OutcomeReplies, response.OutcomeReady, response.OutcomeChildren, response.OutcomeNothingToDo)
}

// confirmSchemas is renderSchemas for the gate's own confirming turn (D32,
// design section 22.12.3): confirmed, questions, ready, then question,
// error. "Other planning turns never list confirmed": ordinary turns keep
// using planningSchemas, which never names it.
func confirmSchemas() ([]string, error) {
	return renderSchemas(response.JobPlanning, response.OutcomeConfirmed, response.OutcomeQuestions, response.OutcomeReady)
}

// sessionStateName renders a store.SessionState for the entry-decision log
// line (design section 5.1): never logged as a bare int.
func sessionStateName(s store.SessionState) string {
	switch s {
	case store.SessionNone:
		return noneLiteral
	case store.SessionIdless:
		return "idless"
	case store.SessionOpen:
		return "open"
	case store.SessionExhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}

// branchName reads a ticket's *string Branch field for logging, "" when nil.
func branchName(b *string) string {
	if b == nil {
		return ""
	}
	return *b
}
