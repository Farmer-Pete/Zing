// postbuild.go is the post-build prelude (design section 5.5): steps E, R,
// and F, shared by the reviewing, judging, and shipping handlers' own entry
// points. In a post-build state every task unit has landed (design section
// 5.5), so a round of job "build" or "perimeter" always belongs to a fix,
// and openFixRequest names the unit any of the three states is mid-flight
// on, exactly as nextTaskN names it in "building" (unitInFlight,
// building.go). #28 gap 3.
package job

import (
	"context"
	"fmt"
	"log/slog"

	"zing/internal/response"
	"zing/internal/store"
)

// postBuildPrelude runs steps E, R, F, and M (design section 5.5, basesync.go
// task 3): an open base merge request owns the worktree until it lands or
// closes, exactly as an open fix request does, in every post-build state.
// handled is false when none applies and the caller's own state step
// machine should run instead.
func postBuildPrelude(ctx context.Context, t store.Ticket, d Deps, origin response.EscalationOrigin) (c store.HandlerCommit, handled bool, err error) {
	slog.Debug("postbuild entry decision", "ticket_id", t.ID, "state", t.State, "origin", string(origin))

	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: postbuild: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		c, handled, roundsErr := postBuildEnterFromRounds(ctx, t, d, rounds)
		if handled || roundsErr != nil {
			return c, handled, roundsErr
		}
	}

	req, open, err := openFixRequest(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: postbuild: open fix request: %w", err)
	}
	if open {
		commit, driveErr := DriveFix(ctx, t, d, req)
		return commit, true, driveErr
	}

	// An open base merge (merge.go) owns the worktree until it lands or
	// closes, in every post-build state: nothing else may read or move the
	// branch meanwhile. Only shipping's own POLL ever opened one before
	// basesync.go's review and judge points; every post-build state can
	// carry one now, so this prelude drives it for all three rather than
	// leaving it to shipHandler.Run alone.
	mc, merging, mergeErr := shipHandler{}.driveOpenMerge(ctx, t, d)
	if merging || mergeErr != nil {
		return mc, true, mergeErr
	}
	return store.HandlerCommit{}, false, nil
}

// postBuildEnterFromRounds is steps E and R (design section 5.5), applied in
// order over every answered round, newest first, the same shape
// buildingHandler.enterFromRounds gives "building": the only branches that
// ever move past the newest round are a build- or perimeter-kind round
// whose session does not belong to the currently open fix request
// (postBuildRoundOwnedByOpenFix's own ownership check -- #28 gap 3's "route
// a fix's perimeter round" requirement), or (reused from building) a
// build-job round already cap_resumes-escalated. A round whose job names
// neither "build" nor "perimeter" -- a review, judge, or respond round, none
// of which a post-build state's fix-only rounds can yet explain -- leaves
// handled false: the caller's own state step machine reads it instead.
func postBuildEnterFromRounds(ctx context.Context, t store.Ticket, d Deps, rounds []store.Round) (store.HandlerCommit, bool, error) {
	h := buildingHandler{}
	for _, round := range rounds {
		newest := round.Questions[len(round.Questions)-1]
		if newest.ParentID != nil {
			commit, err := resolvePostBuildEscalation(ctx, t, d, round, *newest.ParentID)
			return commit, true, err
		}

		kind, kindErr := newestQuestionKind(round)
		if kindErr != nil {
			return store.HandlerCommit{}, false, kindErr
		}

		switch {
		case kind == response.QuestionKindPerimeter:
			_, owned, ownErr := postBuildRoundOwnedByOpenFix(ctx, t, d, round)
			if ownErr != nil {
				return store.HandlerCommit{}, false, ownErr
			}
			if !owned {
				continue
			}
			commit, err := h.resolve(ctx, t, d, round)
			return commit, true, err

		case round.Job == jobBuildName:
			u, owned, ownErr := postBuildRoundOwnedByOpenFix(ctx, t, d, round)
			if ownErr != nil {
				return store.HandlerCommit{}, false, ownErr
			}
			if !owned {
				continue
			}
			commit, again, err := h.resumeBuildRound(ctx, t, d, round, u)
			if again {
				continue
			}
			return commit, true, err

		case round.Job == jobPerimeterName:
			commit, err := h.resolvePerimeterQuestion(ctx, t, d, round)
			return commit, true, err

		default:
			return store.HandlerCommit{}, false, nil
		}
	}
	return store.HandlerCommit{}, false, nil
}

// postBuildRoundOwnedByOpenFix reports whether round's own session belongs
// to the currently open fix request (design section 5.5's own D22
// ownership check, #28 gap 3): SessionAfter, keyed by the open request's own
// watermark, finds the fix's own current session, matched against
// round.SessionID -- the same check unitForBuildRound (building.go) makes
// for "building"'s own answered build rounds, generalized here since a
// post-build state has no task-unit fallback to fall back to. owned is
// false, with no error, when no fix is open or the round's session does not
// match it: the round belongs to something this prelude does not yet
// explain, so the caller moves on to the next round rather than acting on
// someone else's.
func postBuildRoundOwnedByOpenFix(ctx context.Context, t store.Ticket, d Deps, round store.Round) (unit, bool, error) {
	if round.SessionID == nil {
		return unit{}, false, nil
	}
	req, open, err := openFixRequest(ctx, d, t)
	if err != nil {
		return unit{}, false, fmt.Errorf("job: postbuild: open fix request: %w", err)
	}
	if !open {
		return unit{}, false, nil
	}
	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes
	fixSess, _, _, ok, err := d.Store.SessionAfter(ctx, t.ID, jobBuildName, req.AfterRunID, maxResumes)
	if err != nil {
		return unit{}, false, fmt.Errorf("job: postbuild: session after: %w", err)
	}
	if !ok || fixSess.ID != *round.SessionID {
		return unit{}, false, nil
	}
	return fixUnit(req), true, nil
}

// resolvePostBuildEscalation is design section 5.6's own Write/Resolve for a
// post-build escalation (#28 gap 3): escID is the newest question's own
// parent id. Choice c (abandon) is abandonCommit in every state; choice b,
// or a reply with no option, re-escalates replan_unsupported with the
// origin unchanged (D14 of Package 8), exactly as building's own
// enterFromEscalationRound resolves the same two choices. Choice a (retry)
// follows design section 5.6's own choice-by-origin table: fix (with and
// without a run), perimeter, cap_resumes on a build, perimeter, or review
// session (retryCapResumes' own job switch), cap_budget, sandbox_unavailable,
// review, judge, and shipping (each with the same loops_exhausted fix
// request, any other code's own "retry requested" marker, or, judge only,
// "with a run"'s own fresh round retry -- judging.go's retryFreshRound).
// cap_budget's own row is retryCapBudget (#25, planning.go): once the
// ticket's spent agent seconds leave room under d.Budget, it resolves the
// round and writes the "retry requested" marker -- plus ClearPoll, through
// shipRetryMarkerCommit, when the ticket is in shipping -- and otherwise
// re-escalates wall_clock, unchanged from before #25.
// Shipping's own loops_exhausted row is shipHandler.retryShippingLoopsExhausted
// (shipping.go, task 7); its pr_closed and every other code share
// shipRetryMarkerCommit, the same "retry requested" marker plus ClearPoll --
// except a merge-unit escalation (isBaseMergeTried, merge.go task 7), which
// takes priority over loops_exhausted and every other code and routes to
// shipHandler.retryMerge instead, since Tried names the open base merge
// request rather than anything loops_exhausted or pr_closed would mean.
// The respond rows (M4 task 4, respond.go) are shipHandler.retryRespondWithRun
// ("respond, with a run": a fresh batch run immediately with notes and
// error) and shipHandler.retryRespondNoRun ("respond, with no run":
// shipRetryMarkerCommit, same as shipping's own any-other-code row); the
// cap_resumes row for job respond is shipHandler.retryCapResumesRespond,
// dispatched from retryCapResumes' own job switch (building.go), the same
// way judge's is.
//
// Choice d on a review loops_exhausted escalation, while the ticket is still
// reviewing, accepts the findings left and moves the ticket on to judging
// (reviewingHandler.acceptReviewLoopsExhausted, ticket 60) -- but only as an
// explicit pick: a reply with no chosen option on that one question resolves
// as Retry even when d is recommended (owner decision Q3), rewritten ahead
// of this switch. Choice d on any other escalation is treated like b, and
// reaches the choice != escalationChoiceRetry row below.
func resolvePostBuildEscalation(ctx context.Context, t store.Ticket, d Deps, round store.Round, escID int64) (store.HandlerCommit, error) {
	h := buildingHandler{}
	escMsg, payload, err := d.Store.EscalationByID(ctx, escID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: postbuild: escalation %d: %w", escID, err)
	}
	resolveIDs := questionIDs(round)
	choice := roundChoice(round)
	notes := joinReplies(round.Replies)
	errorText := payload.What + "\n" + payload.Why + "\n" + payload.Tried
	origin := response.EscalationOrigin(payload.Origin)
	reviewLoops := origin == response.EscalationOriginReview && payload.Code == string(response.EscalationCodeLoopsExhausted)

	if reviewLoops && choice == escalationChoiceAccept && newestChosenOption(round.Answers) == "" {
		choice = escalationChoiceRetry
	}

	var commit store.HandlerCommit
	preserved := 0

	switch {
	case choice == escalationChoiceAbandon:
		commit = abandonCommit(t, d, payload.Code)

	case choice == escalationChoiceAccept && reviewLoops && t.State == stateReviewing:
		commit, err = reviewingHandler{}.acceptReviewLoopsExhausted(ctx, t, d, resolveIDs)

	case choice != escalationChoiceRetry:
		commit = replanUnsupportedEscalation(t, d, resolveIDs, origin)

	case payload.Code == string(response.EscalationCodeSandboxUnavailable):
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginCapBudget:
		commit, err = retryCapBudget(ctx, t, d, resolveIDs)

	case origin == response.EscalationOriginCapResumes:
		commit, preserved, err = h.retryCapResumes(ctx, t, d, resolveIDs, notes, int64OrZero(payload.SessionID))

	case origin == response.EscalationOriginPerimeter:
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginFix && escMsg.RunID != nil:
		commit, err = h.retryFreshRun(ctx, t, d, resolveIDs, notes, errorText, nil)

	case origin == response.EscalationOriginFix:
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginReview && payload.Code == string(response.EscalationCodeLoopsExhausted):
		commit, err = reviewingHandler{}.retryReviewLoopsExhausted(ctx, t, d, resolveIDs, notes, payload.Tried)

	case origin == response.EscalationOriginReview:
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginJudge && payload.Code == string(response.EscalationCodeLoopsExhausted):
		commit, err = judgeHandler{}.retryJudgeLoopsExhausted(ctx, t, d, resolveIDs, notes, payload.Tried)

	case origin == response.EscalationOriginJudge && escMsg.RunID != nil:
		commit, err = judgeHandler{}.retryFreshRound(ctx, t, d, resolveIDs, notes, errorText)

	case origin == response.EscalationOriginJudge:
		commit = h.retryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginShipping && isBaseMergeTried(payload.Tried):
		commit, err = shipHandler{}.retryMerge(ctx, t, d, resolveIDs, notes, payload.Tried)

	case origin == response.EscalationOriginShipping && payload.Code == string(response.EscalationCodeLoopsExhausted):
		commit, err = shipHandler{}.retryShippingLoopsExhausted(ctx, t, d, resolveIDs, notes, payload.Tried)

	case origin == response.EscalationOriginShipping:
		commit = shipRetryMarkerCommit(t, d, resolveIDs)

	case origin == response.EscalationOriginRespond && escMsg.RunID != nil:
		commit, err = shipHandler{}.retryRespondWithRun(ctx, t, d, resolveIDs, notes, errorText)

	case origin == response.EscalationOriginRespond:
		commit = shipHandler{}.retryRespondNoRun(t, d, resolveIDs)

	default:
		return store.HandlerCommit{}, fmt.Errorf("job: postbuild: escalation %d: unrecognized origin %q", escID, payload.Origin)
	}
	if err != nil {
		return commit, err
	}

	slog.Info("escalation resolved", "ticket_id", t.ID, "session_id", int64OrZero(payload.SessionID),
		"run_id", int64OrZero(escMsg.RunID), "code", payload.Code, "origin", payload.Origin,
		"choice", choice, "preserved_rounds", preserved)
	return commit, nil
}
