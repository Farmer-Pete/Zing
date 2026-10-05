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
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/runtime"
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
	// session-limit message.
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick 2 (planning, session limit): %v", err)
	}

	wantReset := nextSessionLimitReset(t, before, "America/New_York", 12, 20)

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
	if diff := run.CappedUntil.Sub(wantReset); diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("run.CappedUntil = %v, want close to %v (diff %v)", run.CappedUntil, wantReset, diff)
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
		}
	}
	if !sawParked {
		t.Error(`no "parked until" marker found`)
	}

	// Tick 3: one minute before the reset, the parked ticket is skipped.
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
