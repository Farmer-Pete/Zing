// route.go holds the commit builders every job's run passes through on its
// way to a commit: runAndRoute and routeFailure, the escalation, error,
// invalid-output, and question commits, and the D14 invalid marker.
// Planning, plan review, build, review, judge, and respond all call them.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"zing/internal/fence"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

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
	// Chip d, between Retry and Abandon: Recommended stays unset, so a
	// reply with no chip resolves as "a" (roundChoice's default) and never
	// spends budget.
	c.Escalation.ExtraOptions = []response.Option{{Key: escalationChoiceRaiseBudget, Text: budgetRaiseOptionText}}
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
