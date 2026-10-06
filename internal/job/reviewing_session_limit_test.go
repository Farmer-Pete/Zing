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
	"errors"
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
// calls the exact store methods parkCapped itself calls -- Capped for the
// reset, the finish rows, and the discard marker body; store.ParkRuns; and,
// when ParkRuns actually recorded something, the one "discarded review
// round" InsertMessage parkCapped itself writes -- under the same (owner,
// expires) the attempt's own claim just used, and fails the test if any of
// them returns an error.
func reviewParkRound(t *testing.T, s *store.Store, ticketID int64, deps Deps, err error) store.ParkResult {
	t.Helper()
	info, capped := Capped(err)
	if !capped {
		t.Fatalf("reviewParkRound: Capped(%v) = (_, false), want true", err)
	}
	res, parkErr := s.ParkRuns(t.Context(), ticketID, deps.Owner, deps.Expires, info.Until, info.Finish)
	if parkErr != nil {
		t.Fatalf("ParkRuns: %v", parkErr)
	}
	if info.DiscardMarker != "" && res.Applied && (len(res.RunIDs) != 0 || len(info.Finish) != 0) {
		if _, insErr := s.InsertMessage(t.Context(), store.Message{
			TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: info.DiscardMarker,
		}); insErr != nil {
			t.Fatalf("InsertMessage (discarded review round): %v", insErr)
		}
	}
	return res
}

// TestFinishedLensRuns proves finishedLensRuns' own filter deterministically,
// with no runtime concurrency involved (review fix r1f6, r2f11, r3f7): a
// capped attempt's own run is excluded, a canceled attempt's own run (one
// runLensesParallel's roundCtx cut off after another lens hit the cap) is
// excluded too since it did not fail on its own, a good attempt's own run is
// terminalized by its real outcome, an attempt that never reserved (no
// HeldError Reserve call) contributes nothing, and a retried attempt whose
// own first try was invalid but whose retry hit the cap keeps that
// first-try run (its own real "error" outcome) even though its own capped
// retry run is excluded.
func TestFinishedLensRuns(t *testing.T) {
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
		sessionRecord: freshSessionRecord,
	}
	neverReserved := lensAttempt{
		idx: 2, lens: "quality",
		err: &HeldError{},
	}
	canceled := lensAttempt{
		idx: 3, lens: "completeness",
		rr:  runResult{Reserved: store.Reserved{RunID: 4, Turn: 0}},
		err: runtime.ErrCanceled,
	}
	retriedThenCapped := lensAttempt{
		idx: 4, lens: "performance",
		rr:  runResult{Reserved: store.Reserved{RunID: 6, Turn: 1}},
		err: &runtime.SessionLimitError{Parsed: true},
		firstTry: &lensFirstTry{
			rr:     runResult{Reserved: store.Reserved{RunID: 5, Turn: 0}},
			invErr: &runtime.InvalidOutputError{},
		},
	}

	runs := finishedLensRuns([]lensAttempt{capped, good, neverReserved, canceled, retriedThenCapped})
	if len(runs) != 2 {
		t.Fatalf("finishedLensRuns = %+v, want exactly two runs (the good lens's own and the retried lens's own first try)", runs)
	}
	if runs[0].ID != 2 {
		t.Errorf("finishedLensRuns[0].ID = %d, want 2 (the good lens's run id, not the capped or canceled one's)", runs[0].ID)
	}
	if runs[0].Outcome == nil || *runs[0].Outcome != string(response.OutcomeOk) {
		t.Errorf("finishedLensRuns[0].Outcome = %v, want %q", runs[0].Outcome, response.OutcomeOk)
	}
	if runs[1].ID != 5 {
		t.Errorf("finishedLensRuns[1].ID = %d, want 5 (the retried lens's own first-try run, not its capped retry)", runs[1].ID)
	}
	if runs[1].Outcome == nil || *runs[1].Outcome != string(response.OutcomeError) {
		t.Errorf("finishedLensRuns[1].Outcome = %v, want %q (the first try's own real failure, not the cap)", runs[1].Outcome, response.OutcomeError)
	}
}

// TestCappedRoundNote proves cappedRoundNote's own ordering rule (owner
// decision Q6, the rerun's own input note) and its own scope fix (r2f9):
// empty with no "discarded review round" marker at all; still empty once a
// "parked until" marker lands for some other job's own cap, since that
// marker never names a review round; non-empty once a "discarded review
// round" marker itself lands with no later "review round" marker; empty
// again once a "review round ... done" marker lands after it, since that
// round closed normally and the next round entry is not a capped-discard
// rerun.
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

	// A "parked until" marker from some other job's own cap (planning,
	// build, a lone HeldError lens) must not trigger the note: only a
	// review round's own CappedRoundDiscardedPrefix marker does (r2f9).
	if _, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: "parked until 12:20pm America/New_York (run 9): Claude session limit",
	}); insertErr != nil {
		t.Fatalf("InsertMessage (parked, unrelated job): %v", insertErr)
	}

	noteUnrelated, err := h.cappedRoundNote(t.Context(), ticket, d)
	if err != nil {
		t.Fatalf("cappedRoundNote (after unrelated park): %v", err)
	}
	if noteUnrelated != "" {
		t.Errorf("cappedRoundNote (after unrelated park) = %q, want empty (not a review round's own cap)", noteUnrelated)
	}

	if _, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: CappedRoundDiscardedPrefix + "1: Claude session limit",
	}); insertErr != nil {
		t.Fatalf("InsertMessage (discarded round): %v", insertErr)
	}

	note2, err := h.cappedRoundNote(t.Context(), ticket, d)
	if err != nil {
		t.Fatalf("cappedRoundNote (after discarded round): %v", err)
	}
	if note2 != "the previous review round was discarded because of the Claude session limit" {
		t.Errorf("cappedRoundNote (after discarded round) = %q, want the discard note", note2)
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
// session limit each return a *CappedRoundError for round 1, with an empty
// commit, so neither writes a "review round" marker and neither feeds
// lensFailedTwiceWhat's two-in-a-row escalation; and, once the dispatcher's
// own ParkRuns call is simulated (reviewParkRound), every other lens's own
// run keeps its own real outcome and no capped_until, while only the capped
// lens's run is swept as interrupted with capped_until set.
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
		if _, capped := Capped(err); !capped {
			t.Fatalf("attempt %d: Capped(%v) = (_, false), want true", attempt, err)
		}
		var roundErr *CappedRoundError
		if !errors.As(err, &roundErr) {
			t.Fatalf("attempt %d: errors.As(%v, &CappedRoundError) = false, want true", attempt, err)
		}
		if roundErr.Round != 1 {
			t.Errorf("attempt %d: CappedRoundError.Round = %d, want 1", attempt, roundErr.Round)
		}
		for _, r := range roundErr.Finish {
			if r.Lens != nil && *r.Lens == cappedLens {
				t.Errorf("attempt %d: CappedRoundError.Finish contains the capped lens's own run %+v, want it excluded", attempt, r)
			}
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
		// At least the capped lens's own run is swept here. A lens
		// runLensesParallel's own cancel cut off mid-flight (design section
		// 6.2 step 7, a genuine scheduler race this test does not control)
		// is excluded from Finish the same way (r2f11), so it is swept here
		// too, which can make len(res.RunIDs) more than 1.
		if len(res.RunIDs) < 1 {
			t.Fatalf("attempt %d: ParkRuns.RunIDs = %v, want at least one (the capped lens's own run)", attempt, res.RunIDs)
		}

		runs := reviewRunsSince(t, s, ticket.ID, before)
		var sawCapped int
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
			// A lens that already finished (isGood, Q6) keeps its own real
			// outcome and is never swept. A lens the round's own cancel cut
			// off mid-flight is recorded the same way as the capped one:
			// interrupted with capped_until set (r2f11), not as a plain
			// "error" outcome, since the cap ended it, not its own failure.
			switch {
			case r.Interrupted && r.CappedUntil != nil:
			case !r.Interrupted && r.CappedUntil == nil && r.Outcome != nil:
			default:
				t.Errorf("attempt %d: lens %q run %+v, want either finished with a real outcome or interrupted with capped_until set", attempt, *r.Lens, r)
			}
		}
		if sawCapped != 1 {
			t.Errorf("attempt %d: saw %d capped-lens runs since the last attempt, want 1", attempt, sawCapped)
		}
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

// TestReviewRound_CappedRoundRerunsEveryLensFresh proves the owner's final
// Q17 revision of Q6: after a capped review round, every lens of the
// round, capped or completed, runs again as a new run of the same round on
// its own fresh session -- no lens resumes its old session -- and the
// rerun's own prompt carries the "discarded because of the Claude session
// limit" note (r3f9). Once every lens of the rerun returns clean, the round
// closes exactly like any other: one "review round 1 done" marker, no
// different from a round that was never capped at all.
func TestReviewRound_CappedRoundRerunsEveryLensFresh(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()

	s, ticket, before := reviewTicketReady(t)
	cappedScripts := reviewScriptsFS(map[string]string{
		reviewCappedExitKey(cappedLens): reviewCappedLensMessage,
	})
	deps := pbClaim(t, s, runtime.NewFake(cappedScripts), ticket.ID)
	ticketNow := pbGetTicket(t, s, ticket.ID)
	_, err := (reviewingHandler{}).Run(t.Context(), ticketNow, deps)
	if err == nil {
		t.Fatal("Run (round 1, capped): err = nil, want a capped error")
	}
	if _, capped := Capped(err); !capped {
		t.Fatalf("Capped(%v) = (_, false), want true", err)
	}
	if res := reviewParkRound(t, s, ticket.ID, deps, err); !res.Applied {
		t.Fatal("ParkRuns: Applied = false, want true")
	}

	// runLensesParallel cancels the round as soon as the capped lens's own
	// bad attempt is seen, so a lens still waiting on the semaphore (a
	// genuine scheduler race TestReviewRound_CappedLensDiscardsRound's own
	// comment already notes) may never reserve a run at all: this round's
	// own session count is not deterministic, only that the capped lens's
	// own run is among whatever did reserve.
	cappedRuns := reviewRunsSince(t, s, ticket.ID, before)
	sessionIDsByLens := make(map[string]int64, len(cappedRuns))
	for _, r := range cappedRuns {
		if r.Lens != nil {
			sessionIDsByLens[*r.Lens] = r.SessionID
		}
	}
	if _, ok := sessionIDsByLens[cappedLens]; !ok {
		t.Fatalf("round 1 (capped) sessions = %+v, want the capped lens %q among them", sessionIDsByLens, cappedLens)
	}
	afterFirstRound, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}

	// The reset already passed: the hold ParkRuns just raised is pushed into
	// the past, the same way TestReviewRound_CappedLensDiscardsRound's own
	// second attempt stands in for a real reset without waiting on a real
	// clock.
	if setErr := s.SetSettings(t.Context(), "claude_hold_until", "2000-01-01T00:00:00Z"); setErr != nil {
		t.Fatalf("SetSettings(claude_hold_until): %v", setErr)
	}

	okRT := &recordingRuntime{inner: runtime.NewFake(reviewScriptsFS(nil))}
	deps2 := pbClaim(t, s, okRT, ticket.ID)
	ticketNow2 := pbGetTicket(t, s, ticket.ID)
	commit2, err2 := (reviewingHandler{}).Run(t.Context(), ticketNow2, deps2)
	if err2 != nil {
		t.Fatalf("Run (round 1, rerun): %v", err2)
	}
	if commit2.Next != stateJudging || commit2.Reason != reasonReviewClean {
		t.Fatalf("commit2 = {Next: %q, Reason: %q}, want {%q, %q}", commit2.Next, commit2.Reason, stateJudging, reasonReviewClean)
	}
	pbApply(t, s, ticket, commit2)

	if _, ok := reviewMarker(t, s, ticket.ID, "review round 1 done"); !ok {
		t.Error(`no "review round 1 done" marker after the rerun (want the round to close normally)`)
	}

	rerunRuns := reviewRunsSince(t, s, ticket.ID, afterFirstRound)
	seenLenses := make(map[string]bool, len(reviewLensNames))
	for _, r := range rerunRuns {
		if r.Lens == nil {
			continue
		}
		seenLenses[*r.Lens] = true
		if r.ID <= afterFirstRound {
			t.Errorf("rerun lens %q run %+v, want a new run id after the discarded round's own", *r.Lens, r)
		}
		if priorSession, ok := sessionIDsByLens[*r.Lens]; ok && r.SessionID == priorSession {
			t.Errorf("rerun lens %q run %+v reused session %d from the discarded round, want a fresh session", *r.Lens, r, priorSession)
		}
	}
	for _, lens := range reviewLensNames {
		if !seenLenses[lens] {
			t.Errorf("lens %q never reran, want every lens of the round -- capped or completed -- to run again", lens)
		}
	}

	if len(okRT.reqs) == 0 {
		t.Fatal("recordingRuntime: no request recorded")
	}
	const wantNote = "the previous review round was discarded because of the Claude session limit"
	for _, req := range okRT.reqs {
		if !strings.Contains(req.Prompt, wantNote) {
			t.Errorf("rerun request (label %q) prompt = %q, want it to contain %q", req.Label, req.Prompt, wantNote)
		}
	}
}
