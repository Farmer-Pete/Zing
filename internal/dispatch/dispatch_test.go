package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
	"zing/internal/tracker"
)

// The pipeline state names and the owner id this file's tests share, named
// once so goconst has nothing to flag.
const (
	testStateQueued    = "queued"
	testStatePlanning  = "planning"
	testStateBuilding  = "building"
	testStateReviewing = "reviewing"
	testStateJudging   = "judging"
	testStateShipping  = "shipping"
	testStateDone      = "done"

	testOwner      = "test-host-1"
	testFixtureRef = "fake#1" // fixtures/tickets.toml's one ticket

	testWaitingQuestions = "questions"
	testQuestionOpen     = "open"
)

var testProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: "github",
}

// --- shared fixtures -------------------------------------------------------

// newDispatchTestStore opens a fresh Store on a temp-file database, closed
// on test cleanup.
func newDispatchTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// loadMachine loads the real, checked-in machine.toml, the same process
// definition zing serve loads.
func loadMachine(t *testing.T) *machine.Machine {
	t.Helper()
	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	return m
}

// fakeRuntime returns a *runtime.Fake serving the real, checked-in
// fixtures/scripts tree, the same tree zing serve wires up.
func fakeRuntime(t *testing.T) *runtime.Fake {
	t.Helper()
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	return runtime.NewFake(scriptsFS)
}

// newFixtureTracker returns a *tracker.Fixture reading the real, checked-in
// fixtures/tickets.toml.
func newFixtureTracker(t *testing.T) *tracker.Fixture {
	t.Helper()
	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}
	return tr
}

// seedProject inserts testProject and returns its id.
func seedProject(t *testing.T, s *store.Store) int64 {
	t.Helper()
	id, err := s.EnsureProject(t.Context(), testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return id
}

// seedQueuedTicket inserts one queued ticket under ref on testProject.
func seedQueuedTicket(t *testing.T, s *store.Store, ref string) int64 {
	t.Helper()
	projectID := seedProject(t, s)
	id, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: "a ticket", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	return id
}

func getTicket(t *testing.T, s *store.Store, id int64) store.Ticket {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTicket(%d): %v", id, err)
	}
	return ticket
}

// advanceTicket runs the real handler registered for each of states, in
// order, applying every commit directly against s (bypassing the
// dispatcher), so a test can arrange a ticket already sitting in the state
// right after the last one named. Mirrors
// internal/job/skeleton_test.go's advanceThroughStates.
func advanceTicket(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, states ...string) {
	t.Helper()
	for _, state := range states {
		ticket := getTicket(t, s, ticketID)
		if ticket.State != state {
			t.Fatalf("advanceTicket(%s): ticket state = %q, want %q", state, ticket.State, state)
		}
		runHandlerOnce(t, s, rt, ticketID, state)
		// Planning is two-phase (section 6.6): the first entry posts a
		// question and waits, and the resume advances only after the batch
		// is answered. Answer it and re-run planning so this helper leaves
		// the ticket in building, as its callers expect.
		if state == testStatePlanning {
			after := getTicket(t, s, ticketID)
			if after.WaitingOn != nil && *after.WaitingOn == testWaitingQuestions {
				answerOpenQuestion(t, s, ticketID)
				runHandlerOnce(t, s, rt, ticketID, state)
			}
		}
	}
}

// runHandlerOnce claims the ticket, runs its state's handler once, and applies
// the resulting commit directly against s, bypassing the dispatcher.
func runHandlerOnce(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, state string) {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	owner := fmt.Sprintf("advance-%d-%s", ticketID, state)
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("advanceTicket(%s): claim: claimed=%v err=%v", state, claimed, err)
	}
	commit, err := job.Registry()[state].Run(t.Context(), ticket, job.Deps{Store: s, Runtime: rt, Owner: owner, Expires: expires})
	if err != nil {
		t.Fatalf("advanceTicket(%s) Run: %v", state, err)
	}
	if err = job.ValidateCommit(ticket, commit); err != nil {
		t.Fatalf("advanceTicket(%s) ValidateCommit: %v", state, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), commit)
	if err != nil || !applied {
		t.Fatalf("advanceTicket(%s) CommitHandlerResult: applied=%v err=%v", state, applied, err)
	}
}

// answerOpenQuestion answers every open question on the ticket with its first
// offered option, so the planning batch is fully answered and the resume can
// run.
func answerOpenQuestion(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, testQuestionOpen)
	if err != nil {
		t.Fatalf("answerOpenQuestion: QuestionsByState: %v", err)
	}
	if len(open) == 0 {
		t.Fatalf("answerOpenQuestion: ticket %d has no open question", ticketID)
	}
	for i := range open {
		q := &open[i]
		var payload response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &payload); err != nil {
			t.Fatalf("answerOpenQuestion: unmarshal payload: %v", err)
		}
		if len(payload.Options) == 0 {
			t.Fatalf("answerOpenQuestion: question %d has no options", q.ID)
		}
		res, err := s.AnswerQuestion(t.Context(), store.AnswerInput{
			TicketID: ticketID, QuestionID: q.ID, Option: payload.Options[0].Key,
		})
		if err != nil || !res.Accepted {
			t.Fatalf("answerOpenQuestion: AnswerQuestion: accepted=%v conflict=%q err=%v", res.Accepted, res.Conflict, err)
		}
	}
}

// newDispatcher builds a Dispatcher over the real skeleton registry, unless
// reg is non-nil, in which case reg is used instead (a test's chance to
// substitute a spy handler for one state).
func newDispatcher(t *testing.T, s *store.Store, tr tracker.Tracker, b *bus.Broker, rt runtime.Runtime,
	reg map[string]job.Handler, bindings []dispatch.Binding, cfg dispatch.Config,
) *dispatch.Dispatcher {
	t.Helper()
	if reg == nil {
		reg = job.Registry()
	}
	d, err := dispatch.New(s, tr, b, loadMachine(t), reg, bindings, cfg, rt)
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}
	return d
}

// --- New ---------------------------------------------------------------------

// TestNew_MissingHandlerFailsAtNew proves a missing handler is caught at
// startup, never at a nil map read mid-tick (design section 6.8).
func TestNew_MissingHandlerFailsAtNew(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	m := loadMachine(t)
	reg := job.Registry()
	delete(reg, testStatePlanning)

	_, err := dispatch.New(s, newFixtureTracker(t), bus.New(), m, reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner}, fakeRuntime(t))
	if err == nil {
		t.Fatal("New with a missing handler: want an error, got nil")
	}
}

// --- Tick: reconcile, pick, claim, run, commit --------------------------------

// TestTick_ReconcilesAnExpiredClaimThenPicksAndRunsIt proves reconcile runs
// before pick (design section 6.8 step 1): a ticket whose claim already
// expired is freed and, in the same tick, claimed and advanced.
func TestTick_ReconcilesAnExpiredClaimThenPicksAndRunsIt(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	// Pre-claim with an expiry already in the past, simulating a claim a
	// prior tick left behind (or a crashed worker's lease).
	past := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, "stale-owner", past)
	if err != nil || !claimed {
		t.Fatalf("pre-claim: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want planning (reconciled then picked up)", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (cleared by the commit)", *final.ClaimOwner)
	}
}

// TestTick_IntakeInsertsAndDedupsOnASecondIntake proves intake (design
// section 6.8 step 3) inserts a new tracker ticket once, and a second tick's
// intake does not duplicate it. MaxParallel: 0 isolates intake's effect: the
// count guard (step 4) always blocks before pick, so the ticket is never
// claimed or advanced by either tick.
func TestTick_IntakeInsertsAndDedupsOnASecondIntake(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name}}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	first, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("after first Tick: %d tickets, want 1", len(first))
	}
	if first[0].TrackerRef != testFixtureRef || first[0].State != testStateQueued {
		t.Errorf("inserted ticket = %+v, want ref %s in state queued", first[0], testFixtureRef)
	}

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	second, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("after second Tick (dedup): %d tickets, want 1", len(second))
	}
}

// TestTick_RespectsMaxParallel proves the count guard (design section 6.8
// step 4) blocks picking once active runs reach MaxParallel, leaving an
// otherwise-ready ticket untouched. Because CommitHandlerResult always
// clears the claim in the same transaction it applies (design section 6.3),
// an "active run" here is simulated directly the way a second, concurrent
// worker's claim would look.
func TestTick_RespectsMaxParallel(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	activeID := seedQueuedTicket(t, s, "fake#1")
	readyID := seedQueuedTicket(t, s, "fake#2")

	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), activeID, "another-worker", expires)
	if err != nil || !claimed {
		t.Fatalf("claim activeID: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	ready := getTicket(t, s, readyID)
	if ready.State != testStateQueued || ready.ClaimOwner != nil {
		t.Errorf("ready ticket = %+v, want untouched (state queued, unclaimed) while MaxParallel blocks", ready)
	}
}

// TestTick_SecondWorkersClaimIsRefused simulates a second worker already
// holding a ticket's claim: the dispatcher must not touch it (design section
// 6.8 step 6, "another worker holds it").
func TestTick_SecondWorkersClaimIsRefused(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, "other-worker", expires)
	if err != nil || !claimed {
		t.Fatalf("pre-claim: claimed=%v err=%v", claimed, err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 5, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner == nil || *final.ClaimOwner != "other-worker" {
		t.Errorf("final ticket claim owner = %v, want other-worker (untouched)", final.ClaimOwner)
	}
}

// TestTick_PicksTheFurthestAlongTicketOverQueuedOnesAndExcludesTerminal
// arranges one done ticket and one ticket already at building alongside a
// plain queued ticket, then proves a single tick picks the furthest-along
// non-terminal candidate first and never touches the terminal one (design
// section 6.8 step 5).
func TestTick_PicksTheFurthestAlongTicketOverQueuedOnesAndExcludesTerminal(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)

	doneID := seedQueuedTicket(t, s, "fake#1")
	advanceTicket(t, s, rt, doneID, testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging, testStateShipping)

	buildingID := seedQueuedTicket(t, s, "fake#2")
	advanceTicket(t, s, rt, buildingID, testStateQueued, testStatePlanning)

	queuedID := seedQueuedTicket(t, s, "fake#3")

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := getTicket(t, s, buildingID).State; got != testStateReviewing {
		t.Errorf("building ticket state = %q, want reviewing (picked first, furthest along)", got)
	}
	if got := getTicket(t, s, queuedID).State; got != testStateQueued {
		t.Errorf("queued ticket state = %q, want still queued (lower priority)", got)
	}
	if got := getTicket(t, s, doneID).State; got != testStateDone {
		t.Errorf("done ticket state = %q, want unchanged done (terminal, excluded)", got)
	}
}

// TestTick_NumericExternalIDTieBreak arranges two plain queued tickets, tied
// on pipeline position, whose tracker refs tie-break numerically, then
// proves one tick picks the lower numeric external id (design section 6.2,
// 6.8 step 5): "fake#3" before "fake#30", even though "30" sorts before "3"
// as text. A second tick is not driven here: once picked, "fake#3" moves to
// planning, which itself outranks any queued ticket by pipeline position
// (proved separately), so a further pick is no longer a tie-break case.
func TestTick_NumericExternalIDTieBreak(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	lowID := seedQueuedTicket(t, s, "fake#3")
	highID := seedQueuedTicket(t, s, "fake#30")

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := getTicket(t, s, lowID).State; got != testStatePlanning {
		t.Errorf("fake#3 state = %q, want planning (picked first)", got)
	}
	if got := getTicket(t, s, highID).State; got != testStateQueued {
		t.Errorf("fake#30 state = %q, want still queued (lost the tie-break)", got)
	}
}

// TestTick_DrainsWithoutStartingWork proves the drain flag (design section
// 6.8 step 2) stops Tick before it picks or claims anything.
func TestTick_DrainsWithoutStartingWork(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want untouched while draining", final)
	}
}

// TestTick_StopsWithoutStartingWork proves the stopped flag (design section
// 6.8 step 2) stops Tick the same way draining does.
func TestTick_StopsWithoutStartingWork(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	if err := s.SetStopped(t.Context(), true); err != nil {
		t.Fatalf("SetStopped: %v", err)
	}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued || final.ClaimOwner != nil {
		t.Errorf("final ticket = %+v, want untouched while stopped", final)
	}
}

// TestTick_ClaimUsesTheJobTimeoutAndRunsUnderThatDeadlineNotTheClaimGrace
// proves the run context's deadline is now+timeout, not the later
// now+timeout+5m claim expiry the lease carries as checkpoint grace (design
// section 6.8 step 6, 7). planning's machine.toml timeout_minutes is 60.
func TestTick_ClaimUsesTheJobTimeoutAndRunsUnderThatDeadlineNotTheClaimGrace(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	spy := &spyHandler{next: testStateBuilding, reason: "plan ready"}
	reg := job.Registry()
	reg[testStatePlanning] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	before := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	after := time.Now()

	if spy.calls != 1 {
		t.Fatalf("spy.calls = %d, want 1", spy.calls)
	}
	if !spy.hasDeadline {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}
	wantMin := before.Add(59 * time.Minute)
	wantMax := after.Add(61 * time.Minute)
	if spy.deadline.Before(wantMin) || spy.deadline.After(wantMax) {
		t.Errorf("run deadline = %v, want within [%v, %v] (~60m, the planning job timeout, not +65m)", spy.deadline, wantMin, wantMax)
	}

	claimGraceMin := before.Add(64 * time.Minute)
	claimGraceMax := after.Add(66 * time.Minute)
	if spy.expires.Before(claimGraceMin) || spy.expires.After(claimGraceMax) {
		t.Errorf("claim expiry (Deps.Expires) = %v, want within [%v, %v] (~65m: 60m timeout + 5m grace)", spy.expires, claimGraceMin, claimGraceMax)
	}
}

// TestTick_HandlerErrorBeforeAStateChangeReleasesClaimAndLeavesState proves
// a handler error is recoverable: the dispatcher logs it, releases the
// claim with a fenced no-op commit, and leaves the ticket's state and wait
// untouched for a later retry (design section 6.8 step 7).
func TestTick_HandlerErrorBeforeAStateChangeReleasesClaimAndLeavesState(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = &spyHandler{err: errors.New("boom: handler blew up before any state change")}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a handler error is recoverable, not fail-closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.WaitingOn != nil {
		t.Errorf("final ticket waiting_on = %v, want nil", *final.WaitingOn)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}
}

// TestTick_StaleOwnerCommitFailsClosedWithNoRedrive proves the Q-runtime
// fail-closed rule (design section 6.8 step 7, section 13): when the fence
// CommitHandlerResult checks no longer matches (simulated here the way a
// concurrent reconcile stealing the lease mid-run would look), the
// dispatcher logs, stops the process (store.SetStopped), and returns the
// condition from Tick, having called the runtime exactly once. A second
// Tick call, now that the store is stopped, returns cleanly and never
// re-drives the runtime.
func TestTick_StaleOwnerCommitFailsClosedWithNoRedrive(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	counting := &countingRuntime{rt: rt}
	reg := job.Registry()
	reg[testStatePlanning] = &staleOwnerHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), counting, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	err := d.Tick(t.Context())
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Fatalf("first Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Fatalf("runtime calls after the fail-closed tick = %d, want exactly 1", got)
	}

	_, stopped, flagsErr := s.Flags(t.Context())
	if flagsErr != nil {
		t.Fatalf("Flags: %v", flagsErr)
	}
	if !stopped {
		t.Error("stopped flag = false, want true after fail-closed")
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Errorf("second Tick (stopped): err = %v, want nil", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Errorf("runtime calls after the second tick = %d, want still 1 (no re-drive)", got)
	}
}

// --- the minimal error path (design section 6.7) --------------------------

// errorScriptXML is a minimal, valid RunError document for the planning job:
// a universal error outcome with a code from the closed ErrorCode set. It is
// wired into an inline fstest.MapFS fake runtime, never added to the real
// fixtures/scripts tree, because the plan says the skeleton's real scripts
// never error (design section 6.7, section 12 task 8).
const errorScriptXML = `<zing job="planning" outcome="error">
  <error code="cannot_run">
    <what>The planning job's environment cannot run.</what>
    <why>The sandbox has no network access to reach the model.</why>
    <tried>Retried once; same failure.</tried>
  </error>
</zing>
`

// TestTick_ErrorOutcomeEscalates drives a ticket already claimed into
// planning against a fake runtime whose one scripted turn returns the
// universal error outcome, and proves the dispatcher applies the section
// 6.7 error-branch commit end to end: the ticket stays in its state,
// waiting on "error", with one escalation message authored "zing" whose
// EscalationPayload.Code is the script's RunError.Code (one of the four
// ErrorCode values) and whose Options are the fixed local
// retry/planning/abandon set.
func TestTick_ErrorOutcomeEscalates(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued) // queued -> planning, no session opened yet

	errFS := fstest.MapFS{"planning/1.xml": &fstest.MapFile{Data: []byte(errorScriptXML)}}
	errRT := runtime.NewFake(errFS)

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), errRT, nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want unchanged planning (no Next on the error branch)", final.State)
	}
	if final.WaitingOn == nil || *final.WaitingOn != "error" {
		t.Errorf("final ticket waiting_on = %v, want error", final.WaitingOn)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (cleared by the commit)", *final.ClaimOwner)
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var escalation *store.MessageRow
	for i := range msgs {
		if msgs[i].Type == "escalation" {
			escalation = &msgs[i]
		}
	}
	if escalation == nil {
		t.Fatal("no escalation message persisted")
	}
	if escalation.Author != "zing" {
		t.Errorf("escalation message author = %q, want zing", escalation.Author)
	}

	var payload response.EscalationPayload
	if err := json.Unmarshal(escalation.Payload, &payload); err != nil {
		t.Fatalf("unmarshal escalation payload: %v", err)
	}
	validCodes := map[string]bool{
		string(response.ErrorCodePlanGap): true, string(response.ErrorCodeCannotRun): true,
		string(response.ErrorCodeEnvironment): true, string(response.ErrorCodeOther): true,
	}
	if !validCodes[payload.Code] {
		t.Errorf("escalation payload.Code = %q, want one of the four RunError.Code values", payload.Code)
	}
	if payload.Code != string(response.ErrorCodeCannotRun) {
		t.Errorf("escalation payload.Code = %q, want %q (the script's RunError.Code)", payload.Code, response.ErrorCodeCannotRun)
	}
	wantOptions := []string{"retry", "planning", "abandon"}
	if !slices.Equal(payload.Options, wantOptions) {
		t.Errorf("escalation payload.Options = %v, want %v", payload.Options, wantOptions)
	}
}

// --- Run -----------------------------------------------------------------

// TestRun_ReturnsWhenContextIsCancelled proves Run ticks on cfg.Interval and
// exits once ctx is done (design section 6.8).
func TestRun_ReturnsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, nil, bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{Interval: 5 * time.Millisecond, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := d.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run() = %v, want context.DeadlineExceeded", err)
	}
}

// TestRun_ReturnsAfterTheCurrentTickWhenDraining proves Run stops as soon as
// the drain flag is set, after its current tick finishes (design section
// 6.8).
func TestRun_ReturnsAfterTheCurrentTickWhenDraining(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	d := newDispatcher(t, s, nil, bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{Interval: 5 * time.Millisecond, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil on drain", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the drain flag was set")
	}
}

// --- test doubles ----------------------------------------------------------

// spyHandler is a job.Handler test double: it records every call, its
// context's deadline, and the claim it was handed, and either returns err or
// a fixed, valid commit built from next/reason (or a fenced no-op when next
// is empty).
type spyHandler struct {
	calls       int
	hasDeadline bool
	deadline    time.Time
	expires     time.Time

	next, reason string
	err          error
}

func (h *spyHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	h.calls++
	h.expires = d.Expires
	if dl, ok := ctx.Deadline(); ok {
		h.hasDeadline = true
		h.deadline = dl
	}
	if h.err != nil {
		return store.HandlerCommit{}, h.err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: h.next, Reason: h.reason}, nil
}

// countingRuntime wraps a runtime.Runtime and counts every Run call, so a
// test can assert the fake was driven exactly once and never re-driven.
type countingRuntime struct {
	rt    runtime.Runtime
	calls atomic.Int64
}

func (c *countingRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	c.calls.Add(1)
	return c.rt.Run(ctx, req)
}

// staleOwnerHandler runs one real fake-runtime turn (job planning), then
// simulates a concurrent reconcile stealing this ticket's lease mid-run by
// expiring every claim as of just past its own Expires, and finally returns
// a commit that would otherwise be perfectly legal (planning -> building).
// CommitHandlerResult's fence then no longer matches, so the dispatcher must
// fail closed rather than re-drive the fake session it already advanced.
type staleOwnerHandler struct{}

func (staleOwnerHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if _, err := d.Runtime.Run(ctx, runtime.RunRequest{Job: response.JobPlanning}); err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := d.Store.ExpireClaims(ctx, d.Expires.Add(time.Second)); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: testStateBuilding, Reason: "plan ready"}, nil
}
