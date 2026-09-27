// planning.go is the real "planning" state handler (design section 5, 6.1,
// 6.2, 6.3, 6.4, 6.7): classify a kindless ticket, open the planning
// interview once a kind is set, and resume it once the owner (or a round's
// own answers) has something new to say. It replaces the skeleton's fake,
// fixture-driven planningHandler (skeleton.go carried it through task 5);
// the other five skeleton handlers (queued, building, reviewing, judging,
// shipping) are untouched.
//
// This task (6) builds entry-decision steps 1(c), 1(d), 2, 3, 4, and 8 of
// section 5.1: a gate round (1a), an escalation round (1b), a planreview
// round (1e), and steps 5-7 (live validation errors, the ready -> review
// tick, and floor-finding resumes) are each left as a TODO(task 7) returning
// ErrNoAction, so the dispatcher releases the claim rather than looping.
// The "ready" outcome is a temporary shortcut straight to building; task 7
// replaces it with the real cohort store and review tick.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	zing "zing"
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

	// reasonPlanReadyShortcut is deliberately not named reasonPlanReady (the
	// skeleton's own reason string, now removed): the text names itself a
	// shortcut so a reader (and TestPlanningHandler_Resume_ReadyOutcomeIsATemporaryShortcutToBuilding)
	// can find it and remove it once task 7 lands the real cohort store and
	// review tick.
	reasonPlanReadyShortcut = "plan ready (task 6 shortcut; task 7 stores the cohort and reviews it)"

	responseInvalidWhat = "the model's final message failed validation twice in a row"

	runtimeExecFailedWhat = "the runtime could not complete this run"
	runtimeExecFailedWhy  = "the process failed to start, timed out, exceeded the output cap, or exited with no parseable result"

	budgetExhaustedWhat = "raise budget.agent_minutes_per_ticket or abandon"
	budgetExhaustedWhy  = "the ticket's spent agent time has reached the configured budget"

	resumesExhaustedWhat = "raise machine.toml's planning max_resumes, or abandon"
	resumesExhaustedWhy  = "the planning session has resumed the maximum number of times machine.toml allows"

	splitUnsupportedWhat = "the plan says this ticket should be split into several tickets, which Zing does not yet build"
	splitUnsupportedWhy  = "the planning run returned a children outcome"

	nothingToDoArrivesWhat = "nothing_to_do handling arrives in task 8"
	nothingToDoArrivesWhy  = "task 8 checks each code claim before deciding done or an escalation"
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
		has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
		if hasErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: has escalation: %w", hasErr)
		}
		if has {
			return store.HandlerCommit{}, ErrNoAction
		}
		return capResumesEscalation(t, d, sess.ID), nil
	case store.SessionOpen:
		n, reason, invErr := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobPlanningName, &sess.ID)
		if invErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: planning: consecutive invalid outputs: %w", invErr)
		}
		if n == 1 {
			return runPlanningResume(ctx, t, d, sess, nil, []prompt.NamedInput{prompt.Invalid(invalidRetryText(reason))}, n)
		}
		// TODO(task 7): steps 5-7 of section 5.1 land here: a live
		// "validation errors pending" marker resumes with the fenced errors,
		// a stored ready cohort with no planreview artifact starts the
		// review tick, and a live "planreview vN pending" marker resumes
		// with the fenced findings (or escalates loops_exhausted at the
		// cap).
		return store.HandlerCommit{}, ErrNoAction
	}
	return store.HandlerCommit{}, ErrNoAction
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
		// TODO(task 7): route an answered gate round to section 6.6's
		// approve/reject handling.
		return store.HandlerCommit{}, ErrNoAction
	case round.ParentID != nil:
		// TODO(task 7): resolve an escalation round per section 6.7's
		// choice-by-origin table.
		return store.HandlerCommit{}, ErrNoAction
	case round.Job == jobPlanningName:
		return h.enterFromPlanningRound(ctx, t, d, round)
	case round.Job == jobClassifyName:
		rendered, err := renderRoundAnswers(round)
		if err != nil {
			return store.HandlerCommit{}, err
		}
		return runClassify(ctx, t, d, []prompt.NamedInput{prompt.Answers(rendered)}, questionIDs(round))
	case round.Job == jobPlanreviewName:
		// TODO(task 7): resolve a planreview round (design section 6.5).
		return store.HandlerCommit{}, ErrNoAction
	default:
		return store.HandlerCommit{}, ErrNoAction
	}
}

// enterFromPlanningRound is section 5.1 step 1(c): a round whose questions
// were posted by a planning run. An open session that still owns the round
// resumes it (6.4); an exhausted session that still owns it escalates
// resumes_exhausted without resolving the round; anything else (an older
// round, or no session at all any more) starts the planning first turn
// fresh with the round's answers, and resolves it there.
func (h planningHandler) enterFromPlanningRound(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	maxResumes := d.Machine.Jobs[jobPlanningName].MaxResumes
	sess, state, err := d.Store.LatestSession(ctx, t.ID, jobPlanningName, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: latest session for round: %w", err)
	}

	sameSession := round.SessionID != nil && *round.SessionID == sess.ID
	switch {
	case state == store.SessionOpen && sameSession:
		answers, answerErr := answerInputsForRound(round)
		if answerErr != nil {
			return store.HandlerCommit{}, answerErr
		}
		return runPlanningResume(ctx, t, d, sess, questionIDs(round), answers, 0)
	case state == store.SessionExhausted && sameSession:
		return capResumesEscalation(t, d, sess.ID), nil
	default:
		rendered, renderErr := renderRoundAnswers(round)
		if renderErr != nil {
			return store.HandlerCommit{}, renderErr
		}
		return runPlanningFirst(ctx, t, d, []prompt.NamedInput{prompt.Answers(rendered)}, questionIDs(round))
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

	in := prompt.ForClassify(promptText, t.Title+"\n\n"+t.Body, inputs)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobClassifyName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobClassify, Prompt: assembled}
	rr, runErr := runJob(ctx, d, t, jobClassifyName, su, req)
	sessionCommit := freshSessionRecord(rr)

	if runErr != nil {
		if c, ok, failErr := routeFailure(t, d, rr, runErr, n, sessionCommit, resolveIDs, response.EscalationOriginClassify); ok {
			return c, failErr
		}
		return store.HandlerCommit{}, fmt.Errorf("job: classify: unrecognized runJob error: %w", runErr)
	}
	return classifySuccessCommit(t, d, rr, sessionCommit, resolveIDs)
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
		return questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
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
// runClassify's do.
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

	in := prompt.ForPlanningFirst(promptText, styles, t.Title+"\n\n"+t.Body, extra)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobPlanningName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobPlanning, Prompt: assembled}
	rr, runErr := runJob(ctx, d, t, jobPlanningName, su, req)
	sessionCommit := freshSessionRecord(rr)

	if runErr != nil {
		if c, ok, failErr := routeFailure(t, d, rr, runErr, 0, sessionCommit, resolveIDs, response.EscalationOriginPlanningFirst); ok {
			return c, failErr
		}
		return store.HandlerCommit{}, fmt.Errorf("job: planning: first turn: unrecognized runJob error: %w", runErr)
	}
	return planningSuccessCommit(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginPlanningFirst)
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
func runPlanningResume(ctx context.Context, t store.Ticket, d Deps, sess store.Session, resolveIDs []int64, extra []prompt.NamedInput, priorInvalid int) (store.HandlerCommit, error) {
	if sess.ExternalID == nil || *sess.ExternalID == "" {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: session %d has no external id", sess.ID)
	}

	schemas, err := planningSchemas()
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: %w", err)
	}
	in := prompt.ForPlanningResume(extra)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: true}
	req := runtime.RunRequest{Job: response.JobPlanning, SessionID: *sess.ExternalID, Prompt: assembled}
	rr, runErr := runJob(ctx, d, t, jobPlanningName, su, req)
	sessionCommit := resumeSessionRecord(sess.ID, rr)

	if runErr != nil {
		if c, ok, failErr := routeFailure(t, d, rr, runErr, priorInvalid, sessionCommit, resolveIDs, response.EscalationOriginPlanningResume); ok {
			return c, failErr
		}
		return store.HandlerCommit{}, fmt.Errorf("job: planning: resume: unrecognized runJob error: %w", runErr)
	}
	return planningSuccessCommit(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginPlanningResume)
}

// planningSuccessCommit routes a planning run's parsed response (design
// section 6.8), shared by the first turn and the resume: questions and
// error are the same universal handling classify uses; ready is TEMPORARY,
// a shortcut straight to building (TODO(task 7): validate the claims,
// scenarios, and plan, and store the cohort instead); children and
// nothing_to_do each escalate rather than doing their real section 6.8
// handling, which arrive in tasks 7 and 8.
func planningSuccessCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
	case *response.ReadyResponse:
		c := baseCommit(t, d)
		c.Runs = terminalRuns(rr, string(response.OutcomeReady))
		c.Session = sessionCommit
		c.ResolveQuestions = resolveIDs
		// TODO(task 7): check the claims and scenarios (design section 6.5)
		// and store the cohort artifacts instead of shortcutting to building.
		c.Next = stateBuilding
		c.Reason = reasonPlanReadyShortcut
		return c, nil
	case *response.ChildrenResponse:
		c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
			string(response.EscalationCodeSplitUnsupported), splitUnsupportedWhat, splitUnsupportedWhy, "", response.EscalationOriginSplit)
		c.Runs = terminalRuns(rr, string(response.OutcomeChildren))
		c.Session = sessionCommit
		c.ResolveQuestions = resolveIDs
		return c, nil
	case *response.NothingToDoResponse:
		// TODO(task 8): check each code claim; nothing_to_do handling
		// (design section 6.8) replaces this unconditional escalation.
		c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
			string(response.EscalationCodeOther), nothingToDoArrivesWhat, nothingToDoArrivesWhy, "", response.EscalationOriginNothingToDoClaims)
		c.Runs = terminalRuns(rr, string(response.OutcomeNothingToDo))
		c.Session = sessionCommit
		c.ResolveQuestions = resolveIDs
		return c, nil
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, origin), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: planning: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// ---- shared failure and escalation commit builders ------------------------

// routeFailure builds the commit for every runJob failure classify, the
// planning first turn, and its resume all handle alike (design section 5.4,
// 6.7, 6.8): the pre-reserve failures (ErrBudget, ErrConfig,
// store.ErrClaimLost) either escalate (ErrBudget) or return unchanged;
// runtime.ErrCanceled returns with no commit at all (design D13: the
// dispatcher leaves the claim for ExpireClaims to reconcile); an exec
// failure (ErrStart, ErrTimeout, ErrOutputTooLarge, *runtime.ExecError)
// terminalizes the run and escalates runtime_exec_failed; an invalid output
// applies D14. ok is false when runErr names none of these, so the caller
// can report it as a bug rather than silently dropping it.
func routeFailure(
	t store.Ticket, d Deps, rr runResult, runErr error, priorInvalid int,
	sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin,
) (commit store.HandlerCommit, ok bool, err error) {
	switch {
	case errors.Is(runErr, runtime.ErrCanceled):
		return store.HandlerCommit{}, true, runErr
	case errors.Is(runErr, ErrBudget):
		return budgetEscalationCommit(t, d), true, nil
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

// isExecFailure reports whether err is one of the four runtime failures
// section 6.8 escalates as runtime_exec_failed: the process could not
// start, the job deadline killed it, its output exceeded the 4 MiB cap, or
// it exited with no parseable result.
func isExecFailure(err error) bool {
	if errors.Is(err, runtime.ErrStart) || errors.Is(err, runtime.ErrTimeout) || errors.Is(err, runtime.ErrOutputTooLarge) {
		return true
	}
	var execErr *runtime.ExecError
	return errors.As(err, &execErr) //nolint:modernize // see routeFailure's comment
}

// terminalRuns is the one Runs entry every committed path after runJob's
// Reserve terminalizes (design section 4.6, 6.8): the reserved run, its
// outcome, its real exit code, and its agent seconds (runtime.Seconds,
// rounded up, minimum 1).
func terminalRuns(rr runResult, outcome string) []store.Run {
	exitCode := rr.Res.ExitCode
	agentSeconds := runtime.Seconds(rr.Res.AgentTime)
	o := outcome
	return []store.Run{{ID: rr.Reserved.RunID, Turn: rr.Reserved.Turn, Outcome: &o, ExitCode: &exitCode, AgentSeconds: &agentSeconds}}
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
// always carries (design section 6.4): BumpResumes is set whether or not
// this attempt succeeded, since a resume attempt is spent either way;
// ExternalID is filled in only when the runtime returned one (it is already
// set on an ordinary resume, and upsertSessionTx's own "WHERE external_id IS
// NULL" guard makes re-sending it a no-op).
func resumeSessionRecord(sessionID int64, rr runResult) *store.SessionUpsert {
	su := &store.SessionUpsert{ID: &sessionID, BumpResumes: true}
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
func budgetEscalationCommit(t store.Ticket, d Deps) store.HandlerCommit {
	return escalationCommit(t, d, nil, nil, string(response.EscalationCodeWallClock), budgetExhaustedWhat, budgetExhaustedWhy, "", response.EscalationOriginCapBudget)
}

// capResumesEscalation is the resumes_exhausted escalation entry steps 1(c)
// and 3 both write (design D17, section 5.1): RunID is nil (no run caused
// it, the session cap did), SessionID names the exhausted session.
func capResumesEscalation(t store.Ticket, d Deps, sessionID int64) store.HandlerCommit {
	return escalationCommit(t, d, nil, &sessionID, string(response.EscalationCodeResumesExhausted), resumesExhaustedWhat, resumesExhaustedWhy, "", response.EscalationOriginCapResumes)
}

// execFailureCommit terminalizes the reserved run as an error and escalates
// runtime_exec_failed (design section 6.8): RunID and SessionID are both
// set, since a run always caused this.
func execFailureCommit(t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin) store.HandlerCommit {
	c := escalationCommit(t, d, &rr.Reserved.RunID, &rr.Reserved.SessionID,
		string(response.EscalationCodeRuntimeExecFailed), runtimeExecFailedWhat, runtimeExecFailedWhy, "", origin)
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
		Body: fmt.Sprintf("response invalid run %d\n%s", rr.Reserved.RunID, invErr.Reason),
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
// 6.8), shared by classify and planning: one question message per
// qr.Questions, attached to the terminalized run, waiting on "questions".
func questionOutcomeCommit(t store.Ticket, d Deps, rr runResult, qr *response.QuestionResponse, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	msgs, err := questionMessagesFor(t.ID, qr.Questions)
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
// section 6.6's mapping table, reused by classify and planning alike): the
// stored QuestionPayload maps Key (uppercased: the wire pattern
// ^[qQ][0-9]+$ is looser than the stored ^Q[0-9]+$), Recommended, and
// Options straight across, with Kind and State fixed at insert; the message
// Body carries Title as the heading, then Body. The commit allocates each
// message's actual Q<n> once Key is left empty here... but section 6.7 says
// only an escalation's own linked question gets an allocated key; a
// planning or classify question batch's keys instead come straight from the
// model's own q.Key, uppercased, exactly as the skeleton's questionMessages
// did.
func questionMessagesFor(ticketID int64, qs []response.Question) ([]store.Message, error) {
	msgs := make([]store.Message, 0, len(qs))
	for _, q := range qs {
		payload, err := json.Marshal(response.QuestionPayload{
			Key:         strings.ToUpper(q.Key),
			Kind:        response.QuestionKindQuestion,
			State:       response.QuestionStateOpen,
			Recommended: q.Recommended,
			Options:     q.Options,
		})
		if err != nil {
			return nil, fmt.Errorf("job: marshal question payload for %s: %w", q.Key, err)
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
func renderAnswerText(q store.MessageRow, qp response.QuestionPayload, answers, replies []store.MessageRow) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n", qp.Key, q.Body)
	for i := range answers {
		var ap response.AnswerPayload
		if err := json.Unmarshal(answers[i].Payload, &ap); err == nil && ap.Option != nil {
			fmt.Fprintf(&sb, "chose %s: %s\n", *ap.Option, optionTextFor(qp.Options, *ap.Option))
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
	return "your final message was not a valid zing document: " + reason + "; return exactly one"
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
	return renderSchemas(response.JobPlanning, response.OutcomeQuestions, response.OutcomeReady, response.OutcomeChildren, response.OutcomeNothingToDo)
}

// sessionStateName renders a store.SessionState for the entry-decision log
// line (design section 5.1): never logged as a bare int.
func sessionStateName(s store.SessionState) string {
	switch s {
	case store.SessionNone:
		return "none"
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
