// reviewing_session_limit_test.go tests task 5 of issue #45: tableCommit's
// capped-lens row, inserted right after the ctx.Err block, discards the
// whole review round with no commit whenever any lens hit the Claude
// session limit (design shape, owner decision Q3) -- ahead of the budget,
// sandbox, error-document, invalid-output and exec-failure rows, so a
// capped round never writes a "review round N failed" marker and never
// counts toward lensFailedTwiceWhat ("two review rounds in a row failed").
// It also proves the review fix for r1f6: the error carries every other
// lens's own good run (CappedRoundError.Finish, owner decision Q6), so the
// dispatcher's own park write (simulated here by reviewParkRound, since
// package job cannot import package dispatch) terminalizes those by their
// real outcome instead of sweeping them as capped too. It reuses
// reviewing_test.go's own harness (reviewTicketReady, reviewScriptsFS,
// reviewScriptKey, reviewRunsSince, reviewLensNames), and postbuild_test.go's
// (pbClaim, pbGetTicket), package job.
package job

import (
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// reviewCappedLensMessage is the Claude final message this file scripts one
// lens's turn with, well-formed enough for parseSessionLimit's ResetAt to
// actually parse, though tableCommit's capped row cares only that
// job.CappedUntil (here, claudeCapped) finds it true.
const reviewCappedLensMessage = "You've hit your session limit · resets 12:20pm (America/New_York)"

// cappedLens is the one lens this file scripts to hit the session limit
// (goconst: shared by TestGoodAttemptRuns and TestReviewRound_
// CappedLensDiscardsRound).
const cappedLens = "correctness"

// reviewCappedExitKey is reviewScriptKey's own "review/1-LENS/1.xml" stem
// with the fake runtime's own ".exit1" suffix (fake.go's effectKey, task
// 1): it scripts lens's round-1 turn to exit 1 with
// reviewCappedLensMessage as its final message, read ahead of the same
// stem's ".xml" turn (Fake.Run checks N.exit1 before N.xml).
func reviewCappedExitKey(lens string) string {
	return strings.TrimSuffix(reviewScriptKey(lens, 1), ".xml") + ".exit1"
}

// reviewParkRound is this file's own stand-in for the dispatcher's
// parkCapped (internal/dispatch/dispatch.go): package job cannot import
// package dispatch (the reverse direction compiles the real thing), so this
// calls the exact two store methods parkCapped itself calls -- CappedUntil
// for the reset and store.ParkRuns, fed CappedFinish(err) -- under the same
// (owner, expires) the attempt's own claim just used, and fails the test if
// either returns an error.
func reviewParkRound(t *testing.T, s *store.Store, ticketID int64, deps Deps, err error) store.ParkResult {
	t.Helper()
	until, capped := CappedUntil(err)
	if !capped {
		t.Fatalf("reviewParkRound: CappedUntil(%v) = (_, false), want true", err)
	}
	res, parkErr := s.ParkRuns(t.Context(), ticketID, deps.Owner, deps.Expires, until, CappedFinish(err))
	if parkErr != nil {
		t.Fatalf("ParkRuns: %v", parkErr)
	}
	return res
}

// TestGoodAttemptRuns proves goodAttemptRuns deterministically, with no
// runtime concurrency involved (review fix r1f6): a capped attempt's own
// run is excluded, a good attempt's own run is terminalized by its real
// outcome, and an attempt that never reserved (no HeldError Reserve call)
// contributes nothing.
func TestGoodAttemptRuns(t *testing.T) {
	t.Parallel()

	capped := lensAttempt{
		idx: 0, lens: cappedLens,
		rr:  runResult{Reserved: store.Reserved{RunID: 1, Turn: 0}},
		err: &runtime.SessionLimitError{Parsed: true},
	}
	good := lensAttempt{
		idx: 1, lens: discussLens,
		rr: runResult{
			Reserved: store.Reserved{RunID: 2, Turn: 0},
			Res:      runtime.RunResult{Response: &response.FindingsResponse{}},
		},
	}
	neverReserved := lensAttempt{
		idx: 2, lens: "quality",
		err: &HeldError{},
	}

	runs := goodAttemptRuns([]lensAttempt{capped, good, neverReserved})
	if len(runs) != 1 {
		t.Fatalf("goodAttemptRuns = %+v, want exactly one run (the good lens's own)", runs)
	}
	if runs[0].ID != 2 {
		t.Errorf("goodAttemptRuns[0].ID = %d, want 2 (the good lens's run id, not the capped one's)", runs[0].ID)
	}
	if runs[0].Outcome == nil || *runs[0].Outcome != string(response.OutcomeOk) {
		t.Errorf("goodAttemptRuns[0].Outcome = %v, want %q", runs[0].Outcome, response.OutcomeOk)
	}
}

// TestCappedRoundNote proves cappedRoundNote's own ordering rule (owner
// decision Q6, the rerun's own input note): empty with no "parked until"
// marker at all; non-empty once one exists with no later "review round"
// marker; empty again once a "review round ... done" marker lands after
// it, since that round closed normally and the next round entry is not a
// capped-discard rerun.
func TestCappedRoundNote(t *testing.T) {
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	d := Deps{Store: s}
	h := reviewingHandler{}

	note, err := h.cappedRoundNote(t.Context(), ticket, d)
	if err != nil {
		t.Fatalf("cappedRoundNote (no markers): %v", err)
	}
	if note != "" {
		t.Errorf("cappedRoundNote (no markers) = %q, want empty", note)
	}

	if _, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: "parked until 12:20pm America/New_York (run 9): Claude session limit",
	}); insertErr != nil {
		t.Fatalf("InsertMessage (parked): %v", insertErr)
	}

	note2, err := h.cappedRoundNote(t.Context(), ticket, d)
	if err != nil {
		t.Fatalf("cappedRoundNote (after park): %v", err)
	}
	if note2 != "the previous review round was discarded because of the Claude session limit" {
		t.Errorf("cappedRoundNote (after park) = %q, want the discard note", note2)
	}

	if _, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: "review round 1 done sha abc lenses correctness",
	}); insertErr != nil {
		t.Fatalf("InsertMessage (round done): %v", insertErr)
	}

	note3, err := h.cappedRoundNote(t.Context(), ticket, d)
	if err != nil {
		t.Fatalf("cappedRoundNote (after round done): %v", err)
	}
	if note3 != "" {
		t.Errorf("cappedRoundNote (after round done) = %q, want empty (the round closed normally)", note3)
	}
}

// TestReviewRound_CappedLensDiscardsRound proves tableCommit's capped-lens
// row (design goal "A capped review lens discards the whole round with no
// commit, so the round-failure count does not move", owner decision Q3,
// picked option a) together with its own Finish mechanism (owner decision
// Q6): two separate round attempts where lens "correctness" hits the
// session limit each return an error job.CappedUntil recognizes and an
// empty commit, so neither writes a "review round" marker and neither
// feeds lensFailedTwiceWhat's two-in-a-row escalation; and, once the
// dispatcher's own ParkRuns call is simulated (reviewParkRound), every
// other lens's own run keeps its own real outcome and no capped_until,
// while only the capped lens's run is swept as interrupted with
// capped_until set.
func TestReviewRound_CappedLensDiscardsRound(t *testing.T) {
	t.Parallel()

	s, ticket, before := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewCappedExitKey(cappedLens): reviewCappedLensMessage,
	})
	rt := runtime.NewFake(scripts)

	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			// The real story this loop re-plays (#45's own ticket: "two
			// capped review rounds counted as two review rounds in a row
			// failed") is two session-limit hits on two separate days, each
			// well after its own reset passed. runJobWith's own hold gate
			// would otherwise refuse every lens here before Reserve, since
			// attempt 1's own park just raised claude_hold_until to
			// tomorrow: this stands in for that reset already having
			// passed, the same way store.ParkRuns' own tests seed a past
			// hold directly rather than waiting on a real clock.
			if setErr := s.SetSettings(t.Context(), "claude_hold_until", "2000-01-01T00:00:00Z"); setErr != nil {
				t.Fatalf("attempt %d: SetSettings(claude_hold_until): %v", attempt, setErr)
			}
		}
		deps := pbClaim(t, s, rt, ticket.ID)
		ticketNow := pbGetTicket(t, s, ticket.ID)
		commit, err := (reviewingHandler{}).Run(t.Context(), ticketNow, deps)
		if err == nil {
			t.Fatalf("attempt %d: Run: err = nil, want a capped error", attempt)
		}
		if _, capped := CappedUntil(err); !capped {
			t.Fatalf("attempt %d: CappedUntil(%v) = (_, false), want true", attempt, err)
		}
		if commit.Escalation != nil {
			t.Errorf("attempt %d: commit.Escalation = %+v, want nil", attempt, commit.Escalation)
		}
		if len(commit.Messages) != 0 {
			t.Errorf("attempt %d: commit.Messages = %+v, want none", attempt, commit.Messages)
		}
		if commit.Waiting != nil {
			t.Errorf("attempt %d: commit.Waiting = %q, want nil", attempt, *commit.Waiting)
		}

		// tableCommit's capped row writes no HandlerCommit at all: the round's
		// own good lens runs are terminalized, and the capped lens's own run
		// swept, only by this ParkRuns call -- the dispatcher's own write in
		// production -- not by anything reviewingHandler.Run itself applied.
		res := reviewParkRound(t, s, ticket.ID, deps, err)
		if !res.Applied {
			t.Fatalf("attempt %d: ParkRuns: Applied = false, want true", attempt)
		}
		if len(res.RunIDs) != 1 {
			t.Fatalf("attempt %d: ParkRuns.RunIDs = %v, want exactly one (the capped lens's own run)", attempt, res.RunIDs)
		}

		runs := reviewRunsSince(t, s, ticket.ID, before)
		var sawCapped, sawGood int
		for _, r := range runs {
			if r.Lens == nil {
				continue
			}
			if *r.Lens == cappedLens {
				sawCapped++
				if !r.Interrupted || r.CappedUntil == nil {
					t.Errorf("attempt %d: capped lens run %+v, want interrupted with capped_until set", attempt, r)
				}
				continue
			}
			sawGood++
			// Not "ok" specifically: runLensesParallel's own cancel on the
			// first bad attempt (design section 6.2 step 7) can also abort
			// a lens that was still mid-flight when the capped one failed
			// fast, and Q6 keeps that lens's own real outcome, whatever it
			// is, rather than forcing it to "ok". The property under test
			// is that it is never swept as capped.
			if r.Outcome == nil {
				t.Errorf("attempt %d: good lens %q run.Outcome = nil, want a real outcome", attempt, *r.Lens)
			}
			if r.Interrupted || r.CappedUntil != nil {
				t.Errorf("attempt %d: good lens %q run %+v, want not interrupted and no capped_until", attempt, *r.Lens, r)
			}
		}
		if sawCapped != 1 {
			t.Errorf("attempt %d: saw %d capped-lens runs since the last attempt, want 1", attempt, sawCapped)
		}
		// sawGood is not asserted against a fixed count, or even > 0:
		// runLensesParallel launches every lens at once and cancels on the
		// first bad attempt (design section 6.2 step 7, runLensesParallel),
		// so whether any other lens's own goroutine reaches its own
		// Reserve call before the capped lens's cancel fires is a genuine
		// scheduler race, not something this test controls. TestGoodAttemptRuns
		// (below) proves the Finish-building logic itself deterministically;
		// this end-to-end test's own always-true property is res.RunIDs
		// above: exactly the capped lens's own run, whatever else raced.
		_ = sawGood
		before, err = s.MaxRunID(t.Context(), ticket.ID)
		if err != nil {
			t.Fatalf("attempt %d: MaxRunID: %v", attempt, err)
		}
	}

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, reviewRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix(%q): %v", reviewRoundMarkerPrefix, err)
	}
	if len(markers) != 0 {
		t.Errorf("MarkersWithPrefix(%q) = %+v, want none (a capped round writes no round marker)", reviewRoundMarkerPrefix, markers)
	}

	messages, err := s.ListMessages(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var parkedMarkers int
	for _, m := range messages {
		if strings.HasPrefix(m.Body, "review round") {
			t.Errorf("message %+v starts with %q, want no round marker from a capped round", m, "review round")
		}
		if strings.Contains(m.Body, lensFailedTwiceWhat) {
			t.Errorf("message %+v contains %q, want a capped round never to count toward it", m, lensFailedTwiceWhat)
		}
		if strings.HasPrefix(m.Body, "parked until ") {
			parkedMarkers++
		}
	}
	if parkedMarkers != 2 {
		t.Errorf("parked markers = %d, want 2 (one per attempt)", parkedMarkers)
	}
}
