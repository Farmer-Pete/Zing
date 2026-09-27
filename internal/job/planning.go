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
// store, behind a TEMPORARY shortcut straight to building. This task, 7b,
// removes that shortcut -- a valid ready now stores the cohort and leaves
// the ticket in planning, not waiting -- and builds steps 6 and 7 (the
// review tick and the floor loop) and entry step 1(e) (an answered
// planreview round). A clean review's own shortcut straight to building
// (skipping section 6.6's gate) is now the TEMPORARY one; task 7c replaces
// it. A gate round (1a) and an escalation round (1b) are still left as a
// TODO(task 7) returning ErrNoAction, so the dispatcher releases the claim
// rather than looping.
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

	// reasonPlanReviewCleanShortcut is a TEMPORARY shortcut (design section
	// 6.5, task 7b): a plan review with no at-or-below-floor findings goes
	// straight to building rather than posting the section 6.6 gate. The
	// text names itself a shortcut so a reader (and
	// TestPlanningHandler_ReviewTick_CleanFloorIsATemporaryShortcutToBuilding)
	// can find it and remove it once task 7c posts the gate instead.
	//
	// TODO(task 7c): post the gate (design section 6.6) instead of this
	// shortcut.
	reasonPlanReviewCleanShortcut = "plan review clean (task 7b shortcut; task 7c posts the gate)"

	responseInvalidWhat = "the model's final message failed validation twice in a row"

	loopsExhaustedWhat = "raise machine.toml's planreview max_loops, or abandon"
	loopsExhaustedWhy  = "the plan review has delivered the maximum number of floor-finding cycles machine.toml allows"

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
		if commit, handled, resumeErr := maybeResumeValidationErrors(ctx, t, d, sess); handled {
			return commit, resumeErr
		}
		if commit, handled, resumeErr := maybeReviewTick(ctx, t, d); handled {
			return commit, resumeErr
		}
		if commit, handled, resumeErr := maybeResumeFloorFindings(ctx, t, d, sess); handled {
			return commit, resumeErr
		}
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
		rendered, err := renderRoundAnswers(round)
		if err != nil {
			return store.HandlerCommit{}, err
		}
		return runPlanReview(ctx, t, d, []prompt.NamedInput{prompt.Answers(rendered)}, questionIDs(round))
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
	return planningSuccessCommit(ctx, t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginPlanningFirst)
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
	return planningSuccessCommit(ctx, t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginPlanningResume)
}

// planningSuccessCommit routes a planning run's parsed response (design
// section 6.8), shared by the first turn and the resume: questions and
// error are the same universal handling classify uses; ready is section
// 6.5's real cohort check and store, still followed by task 6's TEMPORARY
// shortcut straight to building once the cohort is stored (task 7c removes
// it); children and nothing_to_do each escalate rather than doing their
// real section 6.8 handling, which arrive in tasks 7 and 8.
func planningSuccessCommit(ctx context.Context, t store.Ticket, d Deps, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, origin response.EscalationOrigin) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
	case *response.ReadyResponse:
		return readyCommit(ctx, t, d, rr, resp, sessionCommit, resolveIDs)
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
	if len(errs) > 0 {
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
func checkScenarioShape(scenarios []response.Scenario) []*response.PathError {
	var errs []*response.PathError
	if n := len(scenarios); n < minReadyScenarios || n > maxReadyScenarios {
		errs = append(errs, &response.PathError{
			Path: "scenarios/scenario",
			Msg:  fmt.Sprintf("need %d to %d scenarios, have %d", minReadyScenarios, maxReadyScenarios, n),
		})
	}
	for i, sc := range scenarios {
		if strings.TrimSpace(sc.Then) == "" {
			errs = append(errs, &response.PathError{
				Path: "scenarios/" + indexedScenario(i) + "/then",
				Msg:  "then must not be empty",
			})
		}
	}
	return errs
}

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

	commit, err = runPlanningResume(ctx, t, d, sess, nil, []prompt.NamedInput{prompt.Validation(errsText)}, 0)
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

// maybeReviewTick is section 5.1 step 6: a stored cohort with no planreview
// artifact yet at its exact version starts the review tick fresh. handled is
// false when there is no cohort yet, or its planreview artifact already
// exists, so the caller falls through to step 7.
func maybeReviewTick(ctx context.Context, t store.Ticket, d Deps) (commit store.HandlerCommit, handled bool, err error) {
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
	atOrBelow := make([]response.Finding, 0, len(payload.Findings))
	for _, f := range payload.Findings {
		if f.Severity.Rank() <= d.Floor.Rank() {
			atOrBelow = append(atOrBelow, f)
		}
	}

	n, err := d.Store.CountDeliveredReviews(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: planning: count delivered reviews: %w", err)
	}
	if n >= d.Machine.Jobs[jobPlanreviewName].MaxLoops {
		c := escalationCommit(t, d, nil, nil,
			string(response.EscalationCodeLoopsExhausted), loopsExhaustedWhat, loopsExhaustedWhy, "", response.EscalationOriginCapLoops)
		return c, true, nil
	}

	commit, err = runPlanningResume(ctx, t, d, sess, nil, []prompt.NamedInput{prompt.Findings(renderFindings(atOrBelow))}, 0)
	if err != nil {
		return commit, true, err
	}
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
	if n == 1 {
		inputs = append(inputs, prompt.Invalid(invalidRetryText(reason)))
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

	in := prompt.ForPlanReview(promptText, lensSections, t.Title+"\n\n"+t.Body, scenariosRendered, planXML, inputs)
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	su := store.SessionUpsert{Job: jobPlanreviewName, Runtime: jobCfg.Runtime}
	req := runtime.RunRequest{Job: response.JobPlanreview, Prompt: assembled}
	rr, runErr := runJob(ctx, d, t, jobPlanreviewName, su, req)
	sessionCommit := freshSessionRecord(rr)

	if runErr != nil {
		if c, ok, failErr := routeFailure(t, d, rr, runErr, n, sessionCommit, resolveIDs, response.EscalationOriginPlanreview); ok {
			return c, failErr
		}
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: unrecognized runJob error: %w", runErr)
	}
	return planReviewSuccessCommit(t, d, rr, cohort, planXML, sessionCommit, resolveIDs)
}

// planReviewSuccessCommit routes a planreview run's parsed response (design
// section 6.8): the universal question and error outcomes are shared with
// classify and planning's own success routing; ok is the review's own
// outcome, planReviewOkCommit's job.
func planReviewSuccessCommit(t store.Ticket, d Deps, rr runResult, cohort store.Cohort, planXML string, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	switch resp := rr.Res.Response.(type) {
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginPlanreview), nil
	case *response.FindingsResponse:
		return planReviewOkCommit(t, d, rr, resp, cohort, planXML, sessionCommit, resolveIDs)
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: planreview: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// planReviewOkCommit is design section 6.5's "on ok": drop every finding
// whose Location does not resolve as an element path in planXML
// (response.ResolvesInPlan), store the rest as a "planreview" artifact at
// the cohort's exact version, and split the survivors at the configured
// floor (severity.Rank() <= d.Floor.Rank() is at-or-below). No finding
// at-or-below the floor takes the TEMPORARY clean shortcut straight to
// building (task 7c replaces it with the section 6.6 gate); otherwise this
// writes the "planreview vN pending" marker and leaves the ticket in
// planning, not waiting, for entry step 7 to pick up.
func planReviewOkCommit(t store.Ticket, d Deps, rr runResult, resp *response.FindingsResponse, cohort store.Cohort, planXML string, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	kept := make([]response.Finding, 0, len(resp.Findings))
	dropped := 0
	for _, f := range resp.Findings {
		if !response.ResolvesInPlan([]byte(planXML), f.Location) {
			dropped++
			continue
		}
		kept = append(kept, f)
	}

	var atOrBelow, above int
	for _, f := range kept {
		if f.Severity.Rank() <= d.Floor.Rank() {
			atOrBelow++
		} else {
			above++
		}
	}
	slog.Info("floor split", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "floor", string(d.Floor),
		"at_or_below", atOrBelow, "above", above, "dropped_unresolved", dropped)

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
		c.Next = stateBuilding
		c.Reason = reasonPlanReviewCleanShortcut
		return c, nil
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
	for i, f := range findings {
		lines[i] = fmt.Sprintf("[%s/%s] %s: %s (fix: %s)", f.Lens, f.Severity, f.Location, f.Text, f.Fix)
	}
	return strings.Join(lines, "\n")
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
