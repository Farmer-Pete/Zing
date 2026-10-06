// session_limit_test.go -- #45's own demo: a Claude run that exits with the
// session-limit final message parks the ticket with no owner question, and
// resumes it for free once the parsed reset passes. Both tests drive the
// real dispatcher's Tick, a real store, and the real machine.toml against a
// Fake runtime scripted with fixtures.FS's own classify and planning turns
// plus one N.exit1 sibling, exactly as fixtures/scripts itself never does
// (design section 6.7, section 12 task 8): a session-limit exit is a
// runtime signal, not a scripted ticket outcome.
package dispatch_test

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// sessionLimitMsg is a Claude final message carrying a reset the grammar
// parses: "resets 12:20pm (America/New_York)".
const sessionLimitMsg = "You've hit your session limit · resets 12:20pm (America/New_York)"

// sessionLimitUnparseableMsg carries a reset the grammar cannot parse, so
// parseSessionLimit falls back to now plus 30 minutes.
const sessionLimitUnparseableMsg = "You've hit your session limit · resets soonish"

// nextSessionLimitReset mirrors runtime.parseSessionLimit's own "next
// instant" rule (design shape) so this test can compute, independently of
// the code under test, the reset a "resets H:MMam (ZONE)" message gives
// relative to now.
func nextSessionLimitReset(t *testing.T, now time.Time, zone string, hour, minute int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("time.LoadLocation(%s): %v", zone, err)
	}
	n := now.In(loc)
	c := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	if !c.After(now) {
		c = time.Date(n.Year(), n.Month(), n.Day()+1, hour, minute, 0, 0, loc)
	}
	return c
}

// sessionLimitFS builds an fstest.MapFS over fixtures.FS's own
// scripts/classify/1.xml and scripts/planning/1.xml, with exitScript spliced
// in as "planning/1.exit1" ahead of the real planning/1.xml copied to
// "planning/2.xml": the first planning turn exits with exitScript's own
// message, and a resume serves the real fixture's first (and only) planning
// turn, a question.
func sessionLimitFS(t *testing.T, exitScript string) fstest.MapFS {
	t.Helper()
	classify, err := fs.ReadFile(fixtures.FS, "scripts/classify/1.xml")
	if err != nil {
		t.Fatalf("read fixtures classify/1.xml: %v", err)
	}
	planningQuestion, err := fs.ReadFile(fixtures.FS, "scripts/planning/1.xml")
	if err != nil {
		t.Fatalf("read fixtures planning/1.xml: %v", err)
	}
	return fstest.MapFS{
		"classify/1.xml":   &fstest.MapFile{Data: classify},
		"planning/1.exit1": &fstest.MapFile{Data: []byte(exitScript)},
		"planning/2.xml":   &fstest.MapFile{Data: planningQuestion},
	}
}

// planningRuns returns ticketID's runs on its own "planning" job session
// alone: RunsForTicket spans every session a ticket ever opened, including
// the classify turn these tests' first Tick runs on a separate session, so
// a test asserting on the planning session's own run count must filter to
// it.
func planningRuns(t *testing.T, s *store.Store, ticketID int64) []store.Run {
	t.Helper()
	sess, ok, err := s.OpenSession(t.Context(), ticketID, "planning")
	if err != nil {
		t.Fatalf("OpenSession(planning): %v", err)
	}
	if !ok {
		t.Fatal("OpenSession(planning): not found")
	}
	all, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	var out []store.Run
	for _, r := range all {
		if r.SessionID == sess.ID {
			out = append(out, r)
		}
	}
	return out
}

// TestTick_SessionLimitParksThenResumes is this task's named demo: a Claude
// run that exits non-zero with the session-limit final message leaves the
// run capped and the session parked until the parsed reset, with no owner
// question; held while the clock sits before the reset; and resumes the
// same session for free, with no owner answer, once the clock passes it
// (design shape demo, tests list).
func TestTick_SessionLimitParksThenResumes(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, fakeRuntime(t), ticketID, testStateQueued) // queued -> planning, no session opened yet

	capRT := runtime.NewFake(sessionLimitFS(t, sessionLimitMsg))

	before := time.Now()
	clock := before
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), capRT, nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner, Now: func() time.Time { return clock }})

	// Tick 1: the planning handler's own Kind==nil branch classifies.
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 1 (classify): %v", err)
	}
	classified := getTicket(t, s, ticketID)
	if classified.Kind == nil || *classified.Kind != "feature" {
		t.Fatalf("after Tick 1, ticket.Kind = %v, want feature", classified.Kind)
	}
	if classified.State != testStatePlanning {
		t.Fatalf("after Tick 1, ticket.State = %q, want planning", classified.State)
	}

	// Tick 2: the planning handler opens a session and exits 1 with the
	// session-limit message. beforeTick2 and afterTick2 bracket the real
	// time.Now() parseSessionLimit itself reads (runJobWith's Deps.Now is not
	// wired here): either candidate reset being close enough to run.CappedUntil
	// is accepted, so the real clock crossing the reset's own wall-clock
	// instant between the two reads cannot flake this test (r2f5).
	beforeTick2 := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 2 (planning, session limit): %v", err)
	}
	afterTick2 := time.Now()

	wantResetBefore := nextSessionLimitReset(t, beforeTick2, "America/New_York", 12, 20)
	wantResetAfter := nextSessionLimitReset(t, afterTick2, "America/New_York", 12, 20)

	runs := planningRuns(t, s, ticketID)
	if len(runs) != 1 {
		t.Fatalf("planningRuns after Tick 2 = %d runs, want 1", len(runs))
	}
	run := runs[0]
	if run.Outcome == nil || *run.Outcome != "error" {
		t.Errorf("run.Outcome = %v, want error", run.Outcome)
	}
	if !run.Interrupted {
		t.Error("run.Interrupted = false, want true")
	}
	if run.CappedUntil == nil {
		t.Fatal("run.CappedUntil = nil, want the parsed reset")
	}
	diffBefore := run.CappedUntil.Sub(wantResetBefore)
	diffAfter := run.CappedUntil.Sub(wantResetAfter)
	closeEnough := func(d time.Duration) bool { return d >= -5*time.Second && d <= 5*time.Second }
	if !closeEnough(diffBefore) && !closeEnough(diffAfter) {
		t.Errorf("run.CappedUntil = %v, want close to %v or %v (diffs %v, %v)",
			run.CappedUntil, wantResetBefore, wantResetAfter, diffBefore, diffAfter)
	}
	wantReset := wantResetBefore
	if !closeEnough(diffBefore) {
		wantReset = wantResetAfter
	}

	parked := getTicket(t, s, ticketID)
	if parked.WaitingOn != nil {
		t.Errorf("parked ticket.WaitingOn = %v, want nil (no owner question)", *parked.WaitingOn)
	}
	if parked.ClaimOwner != nil {
		t.Errorf("parked ticket.ClaimOwner = %v, want nil (claim cleared)", *parked.ClaimOwner)
	}

	until, held, err := s.ClaudeHold(t.Context())
	if err != nil {
		t.Fatalf("ClaudeHold: %v", err)
	}
	if !held {
		t.Fatal("ClaudeHold: held = false, want true")
	}
	if !until.Equal(*run.CappedUntil) {
		t.Errorf("ClaudeHold = %v, want %v", until, *run.CappedUntil)
	}

	wantParkedBody := fmt.Sprintf("parked until %s %s (run %d): Claude session limit",
		wantReset.Format("3:04pm"), wantReset.Location().String(), run.ID)

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var sawParked bool
	for _, m := range msgs {
		if m.Type == testMsgTypeEscalation {
			t.Fatalf("escalation message found: %q, want none (a capped run never escalates)", m.Body)
		}
		if strings.HasPrefix(m.Body, "parked until ") {
			sawParked = true
			if m.Body != wantParkedBody {
				t.Errorf("parked marker = %q, want %q", m.Body, wantParkedBody)
			}
		}
	}
	if !sawParked {
		t.Error(`no "parked until" marker found`)
	}

	// Tick 3: one minute before the reset, the parked ticket is skipped. The
	// global claude_hold_until setting is pushed into the past first, so
	// runJobWith's own hold gate cannot be what refuses this ticket: only
	// ListReadyCandidates' own NOT EXISTS clause, keyed on this run's own
	// capped_until, can still be skipping it (r2f6). If that clause were
	// dropped, the ticket would be claimed and a new run reserved here.
	if setErr := s.SetSettings(t.Context(), "claude_hold_until", "2000-01-01T00:00:00Z"); setErr != nil {
		t.Fatalf("SetSettings(claude_hold_until): %v", setErr)
	}
	clock = run.CappedUntil.Add(-time.Minute)
	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 3 (held): %v", err)
	}
	runsAfter3 := planningRuns(t, s, ticketID)
	if len(runsAfter3) != 1 {
		t.Fatalf("planningRuns after Tick 3 = %d runs, want still 1 (held)", len(runsAfter3))
	}
	heldTicket := getTicket(t, s, ticketID)
	if heldTicket.ClaimOwner != nil {
		t.Errorf("held ticket.ClaimOwner = %v, want nil", *heldTicket.ClaimOwner)
	}

	// Tick 4: one minute after the reset, the session resumes for free.
	clock = run.CappedUntil.Add(time.Minute)
	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 4 (resume): %v", err)
	}
	runsAfter4 := planningRuns(t, s, ticketID)
	if len(runsAfter4) != 2 {
		t.Fatalf("planningRuns after Tick 4 = %d runs, want 2", len(runsAfter4))
	}
	resumed := runsAfter4[1]
	if resumed.SessionID != run.SessionID {
		t.Errorf("resumed run.SessionID = %d, want %d (same session)", resumed.SessionID, run.SessionID)
	}
	if resumed.Outcome == nil || *resumed.Outcome != testQuestionLiteral {
		t.Errorf("resumed run.Outcome = %v, want question", resumed.Outcome)
	}

	sess, ok, err := s.OpenSession(t.Context(), ticketID, "planning")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !ok {
		t.Fatal("OpenSession: not found")
	}
	if sess.Resumes != 0 {
		t.Errorf("sess.Resumes = %d, want 0 (free resume)", sess.Resumes)
	}

	wantResumedBody := fmt.Sprintf("resumed after the Claude session limit (run %d)", resumed.ID)

	msgsAfter4, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after Tick 4: %v", err)
	}
	var sawResumed bool
	for _, m := range msgsAfter4 {
		if m.Type == testMsgTypeEscalation {
			t.Fatalf("escalation message found: %q, want none", m.Body)
		}
		if strings.HasPrefix(m.Body, "resumed after the Claude session limit") {
			sawResumed = true
			if m.Body != wantResumedBody {
				t.Errorf("resumed marker = %q, want %q", m.Body, wantResumedBody)
			}
		}
	}
	if !sawResumed {
		t.Error(`no "resumed after the Claude session limit" marker found`)
	}

	finalTicket := getTicket(t, s, ticketID)
	if finalTicket.WaitingOn == nil || *finalTicket.WaitingOn != testWaitingQuestions {
		t.Errorf("final ticket.WaitingOn = %v, want questions (resumed without an owner answer)", finalTicket.WaitingOn)
	}
}

// TestTick_SessionLimitHoldSkipsOtherClaudeTicket proves the global hold's
// own reach (design shape, "Hold"; acceptance "While any run is capped,
// hold new Claude dispatches until the reset"): once ticket A parks, a
// second, unrelated ticket B -- never itself parked -- is claimed and
// released on the very next tick with no run reserved and no "parked
// until" marker of its own, because runJobWith's hold gate refuses its
// claude job (here, classify) before Reserve; the dispatcher's own
// parkCapped still clears B's claim through the same ParkRuns call the
// design's "HeldError with no open runs" edge case describes.
func TestTick_SessionLimitHoldSkipsOtherClaudeTicket(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketA := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, fakeRuntime(t), ticketA, testStateQueued)
	ticketB := seedQueuedTicket(t, s, "fake#2")
	advanceTicket(t, s, fakeRuntime(t), ticketB, testStateQueued)

	capRT := runtime.NewFake(sessionLimitFS(t, sessionLimitMsg))
	// MaxParallel 1, not 2: with ListReadyCandidates' own "ORDER BY id" and
	// #45's fill filling every free slot, a MaxParallel of 2 would run A
	// and B's own classify-then-planning turns concurrently, racing B's own
	// session-limit hit against A's park setting the hold; one slot keeps
	// the two tickets' own turns strictly in id order instead.
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), capRT, nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner})

	// Tick 1: ticket A (lower id) classifies; ticket B is not picked.
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 1 (ticket A classifies): %v", err)
	}
	// Tick 2: ticket A's own real planning turn hits the session limit and
	// parks, setting the global hold.
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 2 (ticket A parks): %v", err)
	}
	parkedA := getTicket(t, s, ticketA)
	if parkedA.ClaimOwner != nil {
		t.Fatalf("ticket A claim = %v, want nil (parked)", *parkedA.ClaimOwner)
	}
	if _, held, err := s.ClaudeHold(t.Context()); err != nil {
		t.Fatalf("ClaudeHold: %v", err)
	} else if !held {
		t.Fatal("ClaudeHold: held = false, want true after ticket A parks")
	}

	runsBBefore, err := s.RunsForTicket(t.Context(), ticketB)
	if err != nil {
		t.Fatalf("RunsForTicket(B) before: %v", err)
	}
	if len(runsBBefore) != 0 {
		t.Fatalf("RunsForTicket(B) before Tick 3 = %d runs, want 0 (never picked yet)", len(runsBBefore))
	}

	// Tick 3: ticket B (the only remaining candidate) is claimed and
	// released with no run reserved: its own classify turn is a claude
	// job, and the hold ticket A's park just set is still in the future.
	if tickErr := d.Tick(t.Context()); tickErr != nil {
		t.Fatalf("Tick 3 (ticket B held): %v", tickErr)
	}
	heldB := getTicket(t, s, ticketB)
	if heldB.ClaimOwner != nil {
		t.Errorf("ticket B claim = %v, want nil (claimed then released)", *heldB.ClaimOwner)
	}
	if heldB.Kind != nil {
		t.Errorf("ticket B Kind = %v, want nil (no classify turn ran)", *heldB.Kind)
	}

	runsBAfter, err := s.RunsForTicket(t.Context(), ticketB)
	if err != nil {
		t.Fatalf("RunsForTicket(B) after: %v", err)
	}
	if len(runsBAfter) != len(runsBBefore) {
		t.Errorf("RunsForTicket(B) = %d runs, want unchanged %d (held before Reserve)", len(runsBAfter), len(runsBBefore))
	}

	msgsB, err := s.ListMessages(t.Context(), ticketB)
	if err != nil {
		t.Fatalf("ListMessages(B): %v", err)
	}
	for _, m := range msgsB {
		if strings.HasPrefix(m.Body, "parked until ") {
			t.Errorf("ticket B message %+v starts with %q, want none (B never itself parked)", m, "parked until ")
		}
	}
}

// TestTick_SessionLimitUnparseableParks30Minutes proves the grammar's own
// fallback (design shape, "Session-limit message grammar"): a reset clause
// the scanner cannot parse parks for 30 minutes, not never.
func TestTick_SessionLimitUnparseableParks30Minutes(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, fakeRuntime(t), ticketID, testStateQueued)

	capRT := runtime.NewFake(sessionLimitFS(t, sessionLimitUnparseableMsg))
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), capRT, nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 1 (classify): %v", err)
	}

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 2 (planning, unparseable session limit): %v", err)
	}
	after := time.Now()

	runs := planningRuns(t, s, ticketID)
	if len(runs) != 1 {
		t.Fatalf("planningRuns = %d runs, want 1", len(runs))
	}
	run := runs[0]
	if run.CappedUntil == nil {
		t.Fatal("run.CappedUntil = nil, want now plus 30 minutes")
	}
	earliest := before.Add(29 * time.Minute)
	latest := after.Add(31 * time.Minute)
	if run.CappedUntil.Before(earliest) || run.CappedUntil.After(latest) {
		t.Errorf("run.CappedUntil = %v, want between %v and %v", run.CappedUntil, earliest, latest)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.Type == testMsgTypeEscalation {
			t.Fatalf("escalation message found: %q, want none", m.Body)
		}
	}
}

// capAfterN is a runtime.Runtime that returns a parsed "ok" review document
// for every call but its own Nth, which returns the session-limit error
// instead: with LensesParallel 1, runLensesParallel's own semaphore
// serializes every lens's call to Run one at a time (design shape), so
// counting calls identifies a real call order this test does not otherwise
// control -- which actual lens wins the race for the Nth call is
// irrelevant to what TestTick_ReviewRoundCappedLensParksOnlyThatRun checks
// (r2f3).
type capAfterN struct {
	n     int32
	calls atomic.Int32
}

func (c *capAfterN) Run(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
	call := c.calls.Add(1)
	sessionID := fmt.Sprintf("cap-after-n-sess-%d", call)
	if call == c.n {
		return runtime.RunResult{SessionID: sessionID, ExitCode: 1, FinalMessage: sessionLimitMsg},
			&runtime.SessionLimitError{ResetAt: time.Now().Add(time.Hour), Parsed: true}
	}
	return runtime.RunResult{
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
		Response: &response.FindingsResponse{Job: response.JobReview, Outcome: response.OutcomeOk},
	}, nil
}

// TestTick_ReviewRoundCappedLensParksOnlyThatRun proves the dispatcher's own
// wiring of job.CappedFinish(err) into store.ParkRuns (design shape, owner
// decision Q6, review fix r2f3): with the review lenses forced serial
// (LensesParallel 1), a lens that completes before another lens hits the
// session limit keeps its own real "ok" outcome, not interrupted and with
// no capped_until, while only the capped lens's own run is parked. Were the
// dispatcher to pass nil instead of job.CappedFinish(err) at this call
// site, ParkRuns would sweep every open run -- including the lens that
// already finished -- as interrupted with capped_until set, failing the
// "finished, not interrupted" assertion below.
func TestTick_ReviewRoundCappedLensParksOnlyThatRun(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedGitBackedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued, testStatePlanning, testStateBuilding)

	beforeMaxID, err := s.MaxRunID(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MaxRunID (before): %v", err)
	}

	capRT := &capAfterN{n: 3}
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), capRT, nil, nil,
		dispatch.Config{
			MaxParallel: 1, Owner: testOwner, LensesParallel: 1,
			Projects: buildTestProjects(t, s), Sandboxes: sandbox.OffSet(),
		})

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (review round, session limit): %v", err)
	}

	parked := getTicket(t, s, ticketID)
	if parked.State != testStateReviewing {
		t.Fatalf("ticket.State = %q, want still reviewing (a capped round writes no commit)", parked.State)
	}
	if parked.ClaimOwner != nil {
		t.Errorf("ticket.ClaimOwner = %v, want nil (parked)", *parked.ClaimOwner)
	}
	if parked.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %v, want nil (no owner question)", *parked.WaitingOn)
	}

	all, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	var newRuns []store.Run
	for _, r := range all {
		if r.ID > beforeMaxID {
			newRuns = append(newRuns, r)
		}
	}
	if len(newRuns) != 3 {
		t.Fatalf("new runs = %+v, want 3 (two finished lenses, one capped)", newRuns)
	}

	var sawCapped, sawFinished int
	for _, r := range newRuns {
		switch {
		case r.Interrupted && r.CappedUntil != nil:
			sawCapped++
			if r.Outcome == nil || *r.Outcome != "error" {
				t.Errorf("capped run.Outcome = %v, want error", r.Outcome)
			}
		case !r.Interrupted && r.CappedUntil == nil:
			sawFinished++
			if r.Outcome == nil || *r.Outcome != "ok" {
				t.Errorf("finished run.Outcome = %v, want ok", r.Outcome)
			}
		default:
			t.Errorf("run %+v, want either finished (ok, not interrupted) or capped (interrupted, capped_until set)", r)
		}
	}
	if sawCapped != 1 {
		t.Errorf("saw %d capped runs, want 1", sawCapped)
	}
	if sawFinished != 2 {
		t.Errorf("saw %d finished runs, want 2 (ParkRuns' Finish, from job.CappedFinish, kept their real outcome)", sawFinished)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var sawDiscarded bool
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, "review round") {
			t.Errorf("message %+v starts with %q, want none (a capped round writes no round marker)", m, "review round")
		}
		if m.Body == job.CappedRoundDiscardedPrefix+"1: Claude session limit" {
			sawDiscarded = true
		}
	}
	if !sawDiscarded {
		t.Errorf(`no %q marker found for round 1 (r2f9)`, job.CappedRoundDiscardedPrefix+"1: Claude session limit")
	}
}
