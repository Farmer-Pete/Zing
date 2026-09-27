package dispatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	testWaitingGate      = "gate"
	testQuestionOpen     = "open"

	testReasonPlanReady = "plan ready"
	testRuntimeFake     = "fake"
)

// testProject is the one project every test in this file seeds. LocalPath
// is filled in per test by seedProject (testProjectDir): the planning
// handler's ready entry point (design section 6.5) opens it for real
// through os.OpenRoot to check a ready response's code claims, so it must
// be a real directory, not a placeholder path.
var testProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", Tracker: "github",
}

// readyClaimEvidencePath is the file every seeded test project carries, the
// same path fixtures/scripts/planning/2.xml's one code claim cites
// ("cmd/zing/main.go:60"), so response.CheckCodeClaims resolves it for real
// against testProjectDir's own os.Root (design section 6.5, D19).
const readyClaimEvidencePath = "cmd/zing/main.go"

// testProjectDir returns a fresh temp directory carrying
// readyClaimEvidencePath, so a ready check's os.OpenRoot(project.LocalPath)
// plus response.CheckCodeClaims can resolve the fixture cohort's one code
// claim for real.
func testProjectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, readyClaimEvidencePath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("testProjectDir: mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("testProjectDir: write %s: %v", readyClaimEvidencePath, err)
	}
	return dir
}

// testModels and testBudget are the job.Deps.Models and job.Deps.Budget
// every handler that calls runJob (classify, planning, since this task)
// needs to resolve a model alias and pass the agent-time budget check:
// the same alias table and an ample budget cmd/zing/selftest.go's own
// e2eModels/e2eBudget wire the real dispatcher with. fakeRuntime never
// reads Model, so the exact ids do not matter beyond matching machine.toml's
// alias names.
var testModels = map[string]string{
	"sonnet": "claude-sonnet-5",
	"opus":   "claude-opus-4-8",
	"fable":  "claude-fable-5-1",
	"codex":  "gpt-5.5",
}

const testBudget = 240 * time.Minute

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

// testRuntimeSet returns a runtime.Set mapping every machine.toml runtime
// name (claude, codex, fake) to rt (design section 4.1, D2: selftest and
// e2e map all three to one Fake), so a handler's
// d.Runtimes.For(d.Machine.Jobs[job].Runtime) lookup always resolves to rt
// regardless of which runtime name a job actually names.
func testRuntimeSet(t *testing.T, rt runtime.Runtime) runtime.Set {
	t.Helper()
	set, err := runtime.NewSet(map[string]runtime.Runtime{"claude": rt, "codex": rt, "fake": rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return set
}

// testDeps builds the job.Deps a handler test drives directly (bypassing
// the dispatcher), with rt resolvable under every machine.toml runtime name,
// the real, checked-in machine.toml as Deps.Machine, testModels/testBudget
// so classify and planning can resolve a model and pass the budget check,
// and Reserve wired to a real store.Reserve closure over (owner, expires),
// exactly as dispatch.Dispatcher.runAndCommit wires it for a real Tick
// (design D13).
func testDeps(t *testing.T, s *store.Store, rt runtime.Runtime, owner string, expires time.Time) job.Deps {
	t.Helper()
	return job.Deps{
		Store: s, Runtimes: testRuntimeSet(t, rt), Machine: loadMachine(t),
		Models: testModels, Budget: testBudget, Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, ticketID int64, su store.SessionUpsert, model string) (store.Reserved, error) {
			return s.Reserve(ctx, ticketID, owner, expires, su, model)
		},
	}
}

// seedProject inserts testProject and returns its id.
func seedProject(t *testing.T, s *store.Store) int64 {
	t.Helper()
	proj := testProject
	proj.LocalPath = testProjectDir(t)
	id, err := s.EnsureProject(t.Context(), proj)
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
		if state == testStatePlanning {
			advancePlanning(t, s, rt, ticketID)
			continue
		}
		runHandlerOnce(t, s, rt, ticketID, state)
	}
}

// advancePlanningMaxCalls bounds advancePlanning's own handler-call loop:
// classify (kind unset, stays planning), the first turn (posts questions,
// waits), the resume (stores the cohort, stays planning), the review tick
// (clean, posts the gate, design section 6.6), and the owner's approve
// (seals the cohort, transitions to building) is five calls; the headroom
// catches a stuck handler instead of hanging the test.
const advancePlanningMaxCalls = 6

// advancePlanning drives the real planning handler through as many calls as
// it now takes to reach building (design section 5.1): classify runs first
// on a kindless ticket and sets kind but carries no transition, so this
// loops the handler until either the ticket leaves planning or it waits on
// "questions" or "gate", in which case it answers the batch (answerOpenQuestion
// always takes the first offered option, "a" Approve for the gate) and keeps
// looping.
func advancePlanning(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64) {
	t.Helper()
	for range advancePlanningMaxCalls {
		runHandlerOnce(t, s, rt, ticketID, testStatePlanning)
		after := getTicket(t, s, ticketID)
		if after.State != testStatePlanning {
			return
		}
		if after.WaitingOn != nil && (*after.WaitingOn == testWaitingQuestions || *after.WaitingOn == testWaitingGate) {
			answerOpenQuestion(t, s, ticketID)
			continue
		}
		if after.WaitingOn != nil {
			t.Fatalf("advancePlanning: ticket waiting_on = %q, want questions, gate, or nil", *after.WaitingOn)
		}
	}
	t.Fatalf("advancePlanning: still in planning after %d handler calls", advancePlanningMaxCalls)
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
	commit, err := job.Registry()[state].Run(t.Context(), ticket, testDeps(t, s, rt, owner, expires))
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
// substitute a spy handler for one state). cfg.Models and cfg.Budget default
// to testModels/testBudget when the caller leaves them unset, so a bare
// dispatch.Config{MaxParallel: N, Owner: testOwner} literal still lets
// classify and planning resolve a model and pass the budget check once a
// real Tick reaches them.
func newDispatcher(t *testing.T, s *store.Store, tr tracker.Tracker, b *bus.Broker, rt runtime.Runtime,
	reg map[string]job.Handler, bindings []dispatch.Binding, cfg dispatch.Config,
) *dispatch.Dispatcher {
	t.Helper()
	if reg == nil {
		reg = job.Registry()
	}
	if cfg.Models == nil {
		cfg.Models = testModels
	}
	if cfg.Budget == 0 {
		cfg.Budget = testBudget
	}
	d, err := dispatch.New(s, tr, b, loadMachine(t), reg, bindings, cfg, testRuntimeSet(t, rt))
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

	_, err := dispatch.New(s, newFixtureTracker(t), bus.New(), m, reg, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner}, testRuntimeSet(t, fakeRuntime(t)))
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

	spy := &spyHandler{next: testStateBuilding, reason: testReasonPlanReady}
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

// TestTick_HandlerDeadlineSurvivesSlowIntakeNotEatenByIt proves the run
// deadline is computed from a fresh time.Now() taken right before running
// the handler (after the claim), not the tick-start now (design section
// 6.8 step 6, fix 9): a slow intake step, which runs earlier in the same
// Tick, must not eat into the handler's own timeout budget. planning's
// machine.toml timeout_minutes is 60.
func TestTick_HandlerDeadlineSurvivesSlowIntakeNotEatenByIt(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	projectID := seedProject(t, s)
	const intakeDelay = 300 * time.Millisecond
	slow := &slowTracker{Tracker: newFixtureTracker(t), delay: intakeDelay}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name}}

	spy := &spyHandler{next: testStateBuilding, reason: testReasonPlanReady}
	reg := job.Registry()
	reg[testStatePlanning] = spy

	d := newDispatcher(t, s, slow, bus.New(), rt, reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	tickStart := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterTick := time.Now()

	if spy.calls != 1 {
		t.Fatalf("spy.calls = %d, want 1", spy.calls)
	}
	if !spy.hasDeadline {
		t.Fatal("the handler's context carried no deadline, want now+timeout")
	}

	// Were the deadline still computed from the tick-start now (the bug),
	// it would sit at roughly tickStart+60m regardless of the slow intake.
	// The fix takes a fresh time.Now() after the claim, so the deadline must
	// land at least intakeDelay later than that, with a safety margin well
	// under intakeDelay so this assertion cannot pass by coincidence.
	const margin = 100 * time.Millisecond
	minDeadline := tickStart.Add(60*time.Minute + intakeDelay - margin)
	if spy.deadline.Before(minDeadline) {
		t.Errorf("handler deadline = %v, want at least %v (computed after the %v slow intake, not at tick start)",
			spy.deadline, minDeadline, intakeDelay)
	}
	maxDeadline := afterTick.Add(61 * time.Minute)
	if spy.deadline.After(maxDeadline) {
		t.Errorf("handler deadline = %v, want at most %v", spy.deadline, maxDeadline)
	}
}

// TestTick_ClaimExpirySurvivesSlowIntakeNotEatenByIt proves the claim expiry
// (Deps.Expires, the same value that fences the eventual commit) is computed
// from a fresh time.Now() taken after reconcile, intake, and count have
// already run (design section "dispatch" fix 5, cubic P2), not the
// tick-start now: a slow intake step must not shrink the lease's actual
// coverage, measured from the moment the ticket is really claimed, below
// timeout + claimGrace. planning's machine.toml timeout_minutes is 60;
// claimGrace is 5m, so the claim window is ~65m.
func TestTick_ClaimExpirySurvivesSlowIntakeNotEatenByIt(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	projectID := seedProject(t, s)
	const intakeDelay = 300 * time.Millisecond
	slow := &slowTracker{Tracker: newFixtureTracker(t), delay: intakeDelay}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name}}

	spy := &spyHandler{next: testStateBuilding, reason: testReasonPlanReady}
	reg := job.Registry()
	reg[testStatePlanning] = spy

	d := newDispatcher(t, s, slow, bus.New(), rt, reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	tickStart := time.Now()
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	afterTick := time.Now()

	if spy.calls != 1 {
		t.Fatalf("spy.calls = %d, want 1", spy.calls)
	}

	// Were expires still computed from the tick-start now (the bug), it
	// would sit at roughly tickStart+65m regardless of the slow intake. The
	// fix takes a fresh time.Now() after intake, so expires must land at
	// least intakeDelay later than that, with a safety margin well under
	// intakeDelay so this assertion cannot pass by coincidence.
	const margin = 100 * time.Millisecond
	minExpires := tickStart.Add(65*time.Minute + intakeDelay - margin)
	if spy.expires.Before(minExpires) {
		t.Errorf("claim expiry = %v, want at least %v (computed after the %v slow intake, not at tick start)",
			spy.expires, minExpires, intakeDelay)
	}
	maxExpires := afterTick.Add(66 * time.Minute)
	if spy.expires.After(maxExpires) {
		t.Errorf("claim expiry = %v, want at most %v", spy.expires, maxExpires)
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

// TestTick_ReleaseClaimFailsClosedWhenLeaseAlreadyLost proves the release
// path gets the same fail-closed treatment as the post-run commit path
// (design section 6.8 step 7, fix 8): a handler that drives the runtime once
// and then fails after a concurrent reconcile has already stolen its lease
// leaves releaseClaim's own fenced no-op commit unable to apply (applied =
// false, the lease already gone), and the dispatcher must stop the process
// and report ErrFailClosed rather than silently continuing as if the claim
// had been cleanly released, since the handler may already have advanced
// the runtime.
func TestTick_ReleaseClaimFailsClosedWhenLeaseAlreadyLost(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	counting := &countingRuntime{rt: rt}
	reg := job.Registry()
	reg[testStatePlanning] = &staleOwnerReleaseHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), counting, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	err := d.Tick(t.Context())
	if !errors.Is(err, dispatch.ErrFailClosed) {
		t.Fatalf("Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", err)
	}
	if got := counting.calls.Load(); got != 1 {
		t.Fatalf("runtime calls after the fail-closed tick = %d, want exactly 1", got)
	}

	_, stopped, flagsErr := s.Flags(t.Context())
	if flagsErr != nil {
		t.Fatalf("Flags: %v", flagsErr)
	}
	if !stopped {
		t.Error("stopped flag = false, want true after fail-closed on the release path")
	}
}

// --- post-handler writes survive a cancelled tick context (fix 4) --------

// TestTick_PostHandlerCommitSurvivesCancelledTickContext proves the
// post-handler commit runs under a detached, bounded context, not ctx
// itself (design section "dispatch" fix 4): a handler that cancels the tick
// context it was handed before returning its commit must still see that
// commit land, since a cancelled handler context (or the drain sequence's
// own force-cancel racing the same moment) must not be able to abort
// recording what the runtime already did.
func TestTick_PostHandlerCommitSurvivesCancelledTickContext(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	reg := job.Registry()
	reg[testStatePlanning] = &cancelingHandler{cancel: cancel, next: testStateBuilding, reason: testReasonPlanReady}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), rt, reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v, want nil (the commit must still land despite the cancelled tick context)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateBuilding {
		t.Errorf("final ticket state = %q, want building (the post-handler commit must survive ctx's own cancellation)", final.State)
	}
}

// TestTick_ReleaseClaimSurvivesCancelledTickContext proves the release
// path's fenced no-op commit gets the same detached, bounded context (design
// section "dispatch" fix 4): a handler that cancels the tick context before
// returning a plain error must still see its claim released, rather than the
// release write itself failing because ctx was already cancelled.
func TestTick_ReleaseClaimSurvivesCancelledTickContext(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	reg := job.Registry()
	reg[testStateQueued] = &cancelingHandler{cancel: cancel, err: errors.New("boom: handler blew up after cancelling ctx")}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v, want nil (the claim release must still land despite the cancelled tick context)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released despite the cancelled tick context)", *final.ClaimOwner)
	}
}

// --- the minimal error path (design section 6.7) --------------------------

// errorScriptXML is a minimal, valid RunError document for the classify
// job: a universal error outcome with a code from the closed ErrorCode set.
// It is wired into an inline fstest.MapFS fake runtime, never added to the
// real fixtures/scripts tree, because the plan says the skeleton's real
// scripts never error (design section 6.7, section 12 task 8). classify,
// not planning, is the job this test's freshly-queued ticket actually runs
// first (design section 5.1 step 2: a nil Kind classifies before planning
// ever opens a session).
const errorScriptXML = `<zing job="classify" outcome="error">
  <error code="cannot_run">
    <what>The classify job's environment cannot run.</what>
    <why>The sandbox has no network access to reach the model.</why>
    <tried>Retried once; same failure.</tried>
  </error>
</zing>
`

// TestTick_ErrorOutcomeEscalates drives a ticket already claimed into
// planning against a fake runtime whose one scripted classify turn returns
// the universal error outcome, and proves the dispatcher applies the
// section 6.7 error-branch commit end to end: the ticket stays in its
// state, waiting on "error", with one escalation message authored "zing"
// whose EscalationPayload.Code is the script's RunError.Code (one of the
// four ErrorCode values) and whose Options are the fixed local
// retry/planning/abandon set.
func TestTick_ErrorOutcomeEscalates(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	rt := fakeRuntime(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)
	advanceTicket(t, s, rt, ticketID, testStateQueued) // queued -> planning, no session opened yet

	errFS := fstest.MapFS{"classify/1.xml": &fstest.MapFile{Data: []byte(errorScriptXML)}}
	errRT := runtime.NewFake(errFS)

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), errRT, nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStatePlanning {
		t.Errorf("final ticket state = %q, want unchanged planning (no Next on the error branch)", final.State)
	}
	// Section 6.7's Write rule sets waiting_on to "questions" for every
	// escalation, cap or run-caused alike (the linked question offers
	// retry/planning/abandon): this superseded the old skeleton's "error"
	// flag once planning.go (task 6) became the real escalation writer.
	if final.WaitingOn == nil || *final.WaitingOn != testWaitingQuestions {
		t.Errorf("final ticket waiting_on = %v, want questions", final.WaitingOn)
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

// TestRun_NotifyDrainReturnsPromptly proves NotifyDrain wakes Run well under
// a tick interval (design section 6.8, fix 5), rather than leaving it to
// notice draining only on the next ticker fire: with a long Interval, Run
// still returns almost immediately once SetDraining and NotifyDrain are
// called, because drainCh is buffered and Run's select observes it directly
// rather than waiting on the timer.
func TestRun_NotifyDrainReturnsPromptly(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, nil, bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{Interval: time.Hour, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	if err := s.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	d.NotifyDrain()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil on drain", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after NotifyDrain (want well under the 1h tick interval)")
	}
}

// --- pickup comment wiring (plan section 6, 8) ------------------------------

// newTwoTicketFixture returns a *commentingFixture over an in-memory
// fixture with two tickets ("fake#1", "fake#2"), since the checked-in
// fixtures/tickets.toml carries only one and these tests need two new
// tickets in one intake.
func newTwoTicketFixture(t *testing.T) *commentingFixture {
	t.Helper()
	fsys := fstest.MapFS{"tickets.toml": &fstest.MapFile{Data: []byte(`project = "zing"

[[ticket]]
ref = "fake#1"
title = "one"
body = "body one"

[[ticket]]
ref = "fake#2"
title = "two"
body = "body two"
`)}}
	fx, err := tracker.NewFixture(fsys, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}
	return &commentingFixture{Fixture: fx}
}

// TestTick_IntakePostsPickupCommentForEachNewTicket proves intake posts the
// pickup comment for each newly inserted ticket, naming the binding's User
// (plan section 6, 8): one tick over two new tickets records exactly two
// pickup comments, and a second tick over the same, now-deduped, tickets
// leaves the recorded count at two (the earlier two remain; no new ones).
func TestTick_IntakePostsPickupCommentForEachNewTicket(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	const testBindingUser = "peter"
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: testBindingUser}}

	rec := newTwoTicketFixture(t)
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 {
		t.Fatalf("after first Tick: %d pickup comments, want 2", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("after first Tick: %d Comment attempts, want 2", attempts)
	}
	want := tracker.PickupComment(testBindingUser)
	wantRefs := map[string]bool{"fake#1": true, "fake#2": true}
	gotRefs := make(map[string]bool, len(got))
	for _, c := range got {
		gotRefs[c.ref] = true
		if c.body != want {
			t.Errorf("pickup comment body = %q, want %q", c.body, want)
		}
		if c.project != testProject.Name {
			t.Errorf("pickup comment project = %q, want %q", c.project, testProject.Name)
		}
	}
	// Each of the two new tickets' refs must have received its own comment,
	// not both landing on the same ref (which the earlier count-only
	// assertion could not have caught).
	if len(gotRefs) != len(wantRefs) {
		t.Errorf("pickup comments covered refs %v, want exactly %v (one comment per distinct new ref)", gotRefs, wantRefs)
	}
	for ref := range wantRefs {
		if !gotRefs[ref] {
			t.Errorf("pickup comments never covered ref %q, want one for every new ticket", ref)
		}
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got = rec.recorded(); len(got) != 2 {
		t.Errorf("after second Tick (dedup): %d pickup comments, want still 2 (no new posts)", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("after second Tick (dedup): %d Comment attempts, want still 2 (no new attempts)", attempts)
	}
}

// TestTick_IntakePickupCommentFailureIsBestEffort proves a Comment failure
// is best-effort (plan section 6): Tick still returns nil, both ticket rows
// remain inserted, the later ticket's comment is still attempted, and a
// later tick over the same, now-deduped, tickets adds no further calls (no
// retry, no double-post).
func TestTick_IntakePickupCommentFailureIsBestEffort(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: "peter"}}

	rec := newTwoTicketFixture(t)
	rec.failFirst = true
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), nil, bindings, dispatch.Config{MaxParallel: 0, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a pickup comment failure is best-effort)", err)
	}

	tickets, err := s.ListAllTickets(t.Context())
	if err != nil {
		t.Fatalf("ListAllTickets: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("tickets inserted = %d, want 2 (both rows remain despite the comment failure)", len(tickets))
	}

	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded pickup comments = %d, want 1 (the first call failed, the second still attempted and recorded)", len(got))
	}
	if got[0].ref != "fake#2" {
		t.Errorf("recorded comment ref = %q, want fake#2 (the later ticket's comment, since the first failed)", got[0].ref)
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("Comment attempts = %d, want 2 (one failed attempt for fake#1, one successful for fake#2)", attempts)
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got = rec.recorded(); len(got) != 1 {
		t.Errorf("recorded pickup comments after second Tick = %d, want still 1 (no retry, no double-post)", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 2 {
		t.Errorf("Comment attempts after second Tick = %d, want still 2 (no retry attempt)", attempts)
	}
}

// --- test doubles ----------------------------------------------------------

// commentingFixture is a recording Tracker test double for the pickup
// comment wiring (plan section 6, 8): it embeds a *tracker.Fixture for
// Intake, Fetch, FileTicket, and Collaborators, and overrides Comment to
// record every call under a mutex. failFirst, when set, makes exactly the
// first Comment call return an error and every later call succeed and
// record normally, so a test can exercise the best-effort failure contract
// without losing coverage of the ticket after it.
type commentingFixture struct {
	*tracker.Fixture

	mu        sync.Mutex
	comments  []recordedComment
	attempts  int
	failFirst bool
	failed    bool
}

var _ tracker.Tracker = (*commentingFixture)(nil)

// recordedComment is one recorded commentingFixture.Comment call.
type recordedComment struct {
	project, ref, body string
}

func (c *commentingFixture) Comment(_ context.Context, project, ref, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.failFirst && !c.failed {
		c.failed = true
		return errors.New("boom: pickup comment failed")
	}
	c.comments = append(c.comments, recordedComment{project: project, ref: ref, body: body})
	return nil
}

// recorded returns a fresh copy of every Comment call recorded so far.
func (c *commentingFixture) recorded() []recordedComment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedComment(nil), c.comments...)
}

// attemptCount returns the total number of Comment calls made so far,
// successful or not, so a test can assert the exact attempt count alongside
// the (possibly smaller) number that were actually recorded.
func (c *commentingFixture) attemptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

// cancelingHandler is a job.Handler test double that cancels a captured
// context.CancelFunc from inside Run, simulating the tick context becoming
// cancelled (a drain force-cancel racing the exact moment the handler
// finishes) right before the post-handler store writes run, then returns
// either a fixed, valid commit (next/reason set) or a plain error (err set).
// It exercises dispatch fix 4's detached, bounded post-handler context on
// both the commit path (TestTick_PostHandlerCommitSurvivesCancelledTickContext)
// and the release path (TestTick_ReleaseClaimSurvivesCancelledTickContext).
type cancelingHandler struct {
	cancel context.CancelFunc

	next, reason string
	err          error
}

func (h *cancelingHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	h.cancel()
	if h.err != nil {
		return store.HandlerCommit{}, h.err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: h.next, Reason: h.reason}, nil
}

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
	rt, err := d.Runtimes.For(d.Machine.Jobs[string(response.JobPlanning)].Runtime)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := rt.Run(ctx, runtime.RunRequest{Job: response.JobPlanning}); err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := d.Store.ExpireClaims(ctx, d.Expires.Add(time.Second)); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: testStateBuilding, Reason: testReasonPlanReady}, nil
}

// staleOwnerReleaseHandler runs one real fake-runtime turn (job planning),
// then simulates a concurrent reconcile stealing this ticket's lease
// mid-run the same way staleOwnerHandler does, but then returns a plain
// handler error instead of a commit. That drives the dispatcher's error
// path (runAndCommit -> releaseClaim), where the fenced no-op release commit
// now finds the lease already gone, exercising the release-path fail-closed
// behavior (fix 8) rather than the post-run-commit path staleOwnerHandler
// (above) exercises.
type staleOwnerReleaseHandler struct{}

func (staleOwnerReleaseHandler) Run(ctx context.Context, _ store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rt, err := d.Runtimes.For(d.Machine.Jobs[string(response.JobPlanning)].Runtime)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := rt.Run(ctx, runtime.RunRequest{Job: response.JobPlanning}); err != nil {
		return store.HandlerCommit{}, err
	}
	if _, err := d.Store.ExpireClaims(ctx, d.Expires.Add(time.Second)); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{}, errors.New("boom: handler blew up after the runtime already ran, lease now stolen")
}

// slowTracker wraps a Tracker and sleeps for delay before delegating Intake,
// so a test can simulate a slow intake step without touching the
// dispatcher's own timing code.
type slowTracker struct {
	tracker.Tracker
	delay time.Duration
}

func (s *slowTracker) Intake(ctx context.Context, project string, rule tracker.IntakeRule) ([]tracker.Ticket, error) {
	time.Sleep(s.delay)
	return s.Tracker.Intake(ctx, project, rule)
}

// --- the D16 seal-mismatch, ErrNoAction, and ErrCanceled dispatcher rules,
// and the D12 post-commit TrackerEffect (design section 4.5, 6.8) ----------

// testPlanPayload is examples/artifacts/plan.json verbatim: a real,
// schema-valid "plan" artifact, so the seal-mismatch test below fails at
// stage "count" (an empty scenario cohort) rather than at stage "plan"
// (design D16, section 4.5 check 1 then check 2).
const testPlanPayload = `{
  "overview": {
    "objective": "Stop checkout from crashing on an empty cart.",
    "context": "internal/cart handles cart state; internal/checkout reads it at payment time.",
    "problem": {
      "text": "checkout panics when cart.Items is nil instead of an empty slice.",
      "loop": {
        "cmd": "go test ./internal/cart/... -run TestEmptyCart",
        "text": "fails: nil pointer dereference in checkout.Total"
      },
      "repro": "create a cart, call Checkout without adding items",
      "hypotheses": [
        {
          "rank": 1,
          "cause": "NewCart never initializes Items",
          "prediction": "initializing Items to []Item{} makes the loop pass"
        }
      ]
    },
    "goals": ["checkout never panics on an empty cart"],
    "nongoals": ["changing the checkout API"]
  },
  "design": {
    "demo": {
      "cmd": "go run ./cmd/demo -empty-cart",
      "text": "an empty cart checks out for zero dollars instead of crashing"
    },
    "shape": "NewCart initializes Items to an empty slice; checkout reads it unchanged.",
    "changes": [
      {
        "path": "internal/cart/cart.go",
        "symbol": "NewCart",
        "kind": "modified",
        "callers": "checkout.New",
        "callees": "none",
        "before": "Items field left at its zero value (nil)",
        "after": "Items: make([]Item, 0)"
      }
    ],
    "types": [],
    "migrations": { "migrations": [] }
  },
  "delivery": {
    "files": [
      { "path": "internal/cart/cart.go", "action": "modify", "reason": "initialize Items to an empty slice" }
    ],
    "deletions": { "deletions": [] },
    "tests": [
      {
        "name": "TestEmptyCart_ReturnsEmptyOrder",
        "seam": "cart.NewCart",
        "kind": "regression",
        "mocks": "",
        "asserts": "checkout of a freshly created cart returns a zero-item order, no panic"
      }
    ],
    "tasks": [
      { "n": 1, "test": "TestEmptyCart_ReturnsEmptyOrder", "demo": true, "text": "Initialize cart.Items to an empty slice in NewCart." }
    ]
  },
  "review": {
    "trust_root": "none",
    "alternatives": ["guard checkout.Total with a nil check instead of fixing the source"],
    "risks": ["other constructors that build a Cart by struct literal still skip this initializer"]
  }
}
`

// sealMismatchHandler reserves a fresh run through Deps.Reserve, proposes a
// real "plan" artifact for it, and requests a Seal whose ExpectedCount (2)
// can never match the cohort's real scenario count (0, since this handler
// inserts none): CommitHandlerResult's own tx fails at stage "count" and
// rolls the whole commit back, including the plan artifact insert, so a
// second call sees the same, still-empty cohort and mismatches the same way
// (design D16, section 4.5, 6.6 branch 0).
type sealMismatchHandler struct{}

func (sealMismatchHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	rsv, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x")
	if err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		Artifacts: []store.Artifact{{RunID: &rsv.RunID, Type: "plan", Version: 1, Payload: json.RawMessage(testPlanPayload)}},
		Seal:      &store.SealRequest{RunID: rsv.RunID, PlanVersion: 1, ExpectedCount: 2, At: time.Now()},
	}, nil
}

// countSealMismatchMarkers counts msgs' "update" messages whose body starts
// with "seal mismatch cohort " (design section 5.3's marker convention,
// D16): every caller below only ever counts this one marker.
func countSealMismatchMarkers(msgs []store.MessageRow) int {
	const prefix = "seal mismatch cohort"
	n := 0
	for i := range msgs {
		if msgs[i].Type == "update" && strings.HasPrefix(msgs[i].Body, prefix) {
			n++
		}
	}
	return n
}

// TestTick_SealMismatchReleasesClaimWritesMarkerAndContinues proves the D16
// dispatcher rule (design section 4.5, 6.6 branch 0): a seal transaction
// mismatch releases the claim, writes a "seal mismatch cohort <runID>"
// marker, never stops the dispatcher, and Tick returns nil so the ticket is
// picked up again on a later tick, which mismatches (and marks) the same
// way a second time.
func TestTick_SealMismatchReleasesClaimWritesMarkerAndContinues(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = sealMismatchHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a seal mismatch must not fail closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false (a seal mismatch must not fail closed)")
	}

	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 1 {
		t.Fatalf("seal mismatch markers after first tick = %d, want 1", got)
	}

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v, want nil", err)
	}
	msgs, err = s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after second tick: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 2 {
		t.Errorf("seal mismatch markers after second tick = %d, want 2", got)
	}
}

// seedGateReadyTicket seeds one project, one ticket already sitting in
// "planning" with kind "feature" (bypassing classify and the interview,
// which the real gate approve pre-check never touches), a plan cohort of n
// scenario artifacts (all unsealed) under a fresh reserved run, and one
// open, then answered ("a", approve), gate question attached to that run --
// the state design section 6.6's entry step 1(a) finds on its very first
// tick: an answered gate round ready to interpret. It returns the ticket id
// and the cohort's own run id.
func seedGateReadyTicket(t *testing.T, s *store.Store, n int) (ticketID, runID int64) {
	t.Helper()
	ctx := t.Context()

	projectID := seedProject(t, s)
	kind := "feature"
	ticketID, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: "gate-race#1", Title: "a ticket", Kind: &kind, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: InsertTicket: %v", err)
	}
	runHandlerOnce(t, s, fakeRuntime(t), ticketID, testStateQueued) // queued -> planning

	owner := "seed-gate-ready"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedGateReadyTicket: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(ctx, ticketID, owner, expires, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x")
	if err != nil {
		t.Fatalf("seedGateReadyTicket: reserve: %v", err)
	}

	extID := "seed-gate-ready-ext"
	artifacts := make([]store.Artifact, 0, 1+n)
	artifacts = append(artifacts, store.Artifact{RunID: &rsv.RunID, Type: "plan", Version: 1, Payload: json.RawMessage(testPlanPayload)})
	for i := range n {
		sc := response.Scenario{
			ID: fmt.Sprintf("s%d", i+1), Kind: response.ScenarioKindBehavior,
			Given: "the server is running", When: "a client sends a request", Then: "the response is correct",
		}
		payload, marshalErr := json.Marshal(sc)
		if marshalErr != nil {
			t.Fatalf("seedGateReadyTicket: marshal scenario: %v", marshalErr)
		}
		artifacts = append(artifacts, store.Artifact{RunID: &rsv.RunID, Type: "scenario", Payload: payload})
	}

	outcome, exitCode, agentSeconds := "ready", 0, 1
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session:   &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &extID},
		Runs:      []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
		Artifacts: artifacts,
	})
	if err != nil || !applied {
		t.Fatalf("seedGateReadyTicket: store the cohort: applied=%v err=%v", applied, err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindGate, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: marshal gate question payload: %v", err)
	}
	const authorZing = "zing" // avoids a third bare "zing" literal (goconst)
	qID, err := s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, RunID: &rsv.RunID, Type: "question", Author: authorZing,
		State: new("open"), Body: "the plan objective", Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("seedGateReadyTicket: insert gate question: %v", err)
	}
	if res, ansErr := s.AnswerQuestion(ctx, store.AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "a"}); ansErr != nil || !res.Accepted {
		t.Fatalf("seedGateReadyTicket: AnswerQuestion: %+v, %v", res, ansErr)
	}

	return ticketID, rsv.RunID
}

// TestTick_GateApproveTOCTOURace_MismatchReleasesThenPreCheckTakesBranch6
// proves the D16 TOCTOU race design section 6.6 branch 0's own commentary
// names as "unreachable in practice" under the single-owner claim, made
// reachable here through job.GateApproveSealRaceHook (a test-only seam):
// between the real gate approve pre-check's own read of CohortSealState
// (sealed == 0) and the commit it builds from that read (branch 4, sealing
// the whole cohort), a second store handle seals one scenario row through a
// raw SQL update, so the commit's own sealCohortTx sees an "update" stage
// mismatch (it sealed 2 of 3 rows, not all 3). The dispatcher's existing
// ErrSealMismatch rule (design D16, proved generically by
// TestTick_SealMismatchReleasesClaimWritesMarkerAndContinues above) releases
// the claim and writes one "seal mismatch cohort <runID>" marker without
// resolving the gate round (the whole mismatched tx rolled back); the next
// tick re-enters the same still-answered round, and this time
// CohortSealState reports 1 of 3 scenarios sealed -- branch 6 (design
// section 6.6), not branch 4 again -- so it escalates seal_failed and
// resolves the round, without writing a second marker.
func TestTick_GateApproveTOCTOURace_MismatchReleasesThenPreCheckTakesBranch6(t *testing.T) {
	// Not t.Parallel(): this test installs job.GateApproveSealRaceHook, a
	// package-level seam shared by every ticket's gate approve call, so it
	// must not run alongside another test whose own ticket might also reach
	// gateApprove concurrently. Go only runs t.Parallel() tests together
	// after every non-parallel test in the package has finished, so leaving
	// this one sequential is what keeps the hook's install/reset window
	// free of any other test's own gate approve call.
	dbPath := filepath.Join(t.TempDir(), "zing.db")
	s, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const scenarioCount = 3
	ticketID, runID := seedGateReadyTicket(t, s, scenarioCount)

	var raceOnce sync.Once
	job.GateApproveSealRaceHook = func(gotRunID int64) {
		raceOnce.Do(func() {
			if gotRunID != runID {
				t.Errorf("GateApproveSealRaceHook: runID = %d, want %d", gotRunID, runID)
			}
			db2, openErr := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
			if openErr != nil {
				t.Fatalf("race: open second db handle: %v", openErr)
			}
			defer func() { _ = db2.Close() }()
			if _, execErr := db2.ExecContext(t.Context(),
				`UPDATE artifacts SET sealed_at = ? WHERE id = (
					SELECT id FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ? ORDER BY id LIMIT 1
				)`,
				time.Now().UTC().Format(time.RFC3339), ticketID, gotRunID,
			); execErr != nil {
				t.Fatalf("race: seal one scenario row through the second handle: %v", execErr)
			}
		})
	}
	t.Cleanup(func() { job.GateApproveSealRaceHook = nil })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v, want nil (a seal mismatch must not fail closed)", err)
	}

	afterFirst := getTicket(t, s, ticketID)
	if afterFirst.State != testStatePlanning {
		t.Errorf("after the raced tick: ticket state = %q, want unchanged planning", afterFirst.State)
	}
	if afterFirst.ClaimOwner != nil {
		t.Errorf("after the raced tick: claim owner = %v, want nil (released)", *afterFirst.ClaimOwner)
	}
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after first tick: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 1 {
		t.Fatalf("seal mismatch markers after the raced tick = %d, want 1", got)
	}
	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil {
		t.Fatalf("QuestionsByState(answered) after first tick: %v", err)
	}
	if len(answered) != 1 {
		t.Fatalf("answered questions after the raced tick = %d, want 1 (the mismatched tx rolled back the resolution)", len(answered))
	}

	if err = d.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v, want nil", err)
	}

	afterSecond := getTicket(t, s, ticketID)
	if afterSecond.State != testStatePlanning {
		t.Errorf("after the second tick: ticket state = %q, want unchanged planning (branch 6 only escalates)", afterSecond.State)
	}
	if afterSecond.WaitingOn == nil || *afterSecond.WaitingOn != testWaitingQuestions {
		t.Errorf("after the second tick: waiting_on = %v, want questions (the seal_failed escalation)", afterSecond.WaitingOn)
	}

	msgs, err = s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages after second tick: %v", err)
	}
	if got := countSealMismatchMarkers(msgs); got != 1 {
		t.Errorf("seal mismatch markers after the second tick = %d, want still 1 (no new mismatch)", got)
	}
	var escalation *response.EscalationPayload
	for i := range msgs {
		if msgs[i].Type != "escalation" {
			continue
		}
		var p response.EscalationPayload
		if unmarshalErr := json.Unmarshal(msgs[i].Payload, &p); unmarshalErr != nil {
			t.Fatalf("unmarshal escalation payload: %v", unmarshalErr)
		}
		escalation = &p
	}
	if escalation == nil {
		t.Fatal("no escalation message found after the second tick")
	}
	if escalation.Code != string(response.EscalationCodeSealFailed) {
		t.Errorf("escalation.Code = %q, want seal_failed", escalation.Code)
	}
	wantWhat := "cohort is partially or inconsistently sealed (1 of 3)"
	if escalation.What != wantWhat {
		t.Errorf("escalation.What = %q, want %q", escalation.What, wantWhat)
	}

	resolved, err := s.QuestionsByState(t.Context(), ticketID, "resolved")
	if err != nil {
		t.Fatalf("QuestionsByState(resolved) after second tick: %v", err)
	}
	if len(resolved) != 1 {
		t.Errorf("resolved questions after the second tick = %d, want 1 (the gate round)", len(resolved))
	}
}

// noActionHandler always returns job.ErrNoAction: the entry decision found
// nothing to do this tick (design section 4.5, 5.1 step 8).
type noActionHandler struct{}

func (noActionHandler) Run(context.Context, store.Ticket, job.Deps) (store.HandlerCommit, error) {
	return store.HandlerCommit{}, job.ErrNoAction
}

// TestTick_ErrNoActionReleasesClaimWithoutStopping proves job.ErrNoAction
// releases the claim through the same no-op-commit path any other handler
// error uses, but never stops the dispatcher (design section 4.5).
func TestTick_ErrNoActionReleasesClaimWithoutStopping(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = noActionHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (ErrNoAction must not fail closed)", err)
	}

	final := getTicket(t, s, ticketID)
	if final.State != testStateQueued {
		t.Errorf("final ticket state = %q, want unchanged queued", final.State)
	}
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (released)", *final.ClaimOwner)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped = true, want false (ErrNoAction must not fail closed)")
	}
}

// cancelingReserveHandler reserves a fresh run, then returns
// runtime.ErrCanceled with no commit, simulating a parent-context
// cancellation (dispatcher shutdown) the runtime itself reported mid-call
// (design D13, section 4.5).
type cancelingReserveHandler struct{}

func (cancelingReserveHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if _, err := d.Reserve(ctx, t.ID, store.SessionUpsert{Job: testStatePlanning, Runtime: testRuntimeFake}, "claude-x"); err != nil {
		return store.HandlerCommit{}, err
	}
	return store.HandlerCommit{}, runtime.ErrCanceled
}

// TestTick_ErrCanceledLeavesClaimForExpireClaimsToReconcile proves
// runtime.ErrCanceled leaves the claim in place -- neither released nor
// fail-closed -- so the reserved run sits with a NULL outcome until the
// lease expires, at which point ExpireClaims (already exercised by task 4a)
// reconciles it to error/-1 (design D13, section 4.5).
func TestTick_ErrCanceledLeavesClaimForExpireClaimsToReconcile(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = cancelingReserveHandler{}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil, dispatch.Config{MaxParallel: 2, Owner: testOwner})
	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (ErrCanceled must not fail closed)", err)
	}

	claimed := getTicket(t, s, ticketID)
	if claimed.State != testStateQueued {
		t.Errorf("ticket state = %q, want unchanged queued", claimed.State)
	}
	if claimed.ClaimOwner == nil || claimed.ClaimExpiresAt == nil {
		t.Fatal("claim was released, want it left in place for ExpireClaims to reconcile")
	}

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 (the reserved run)", len(runs))
	}
	if runs[0].Outcome != nil {
		t.Errorf("run outcome = %q, want nil before the lease expires", *runs[0].Outcome)
	}

	if _, expireErr := s.ExpireClaims(t.Context(), claimed.ClaimExpiresAt.Add(time.Second)); expireErr != nil {
		t.Fatalf("ExpireClaims: %v", expireErr)
	}

	runs, err = s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket after ExpireClaims: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome == nil || *runs[0].Outcome != "error" {
		t.Fatalf("after ExpireClaims: run = %+v, want outcome error", runs[0])
	}
	if runs[0].ExitCode == nil || *runs[0].ExitCode != -1 {
		t.Errorf("after ExpireClaims: run exit_code = %v, want -1", runs[0].ExitCode)
	}
}

// trackerEffectHandler proposes a commit carrying only a TrackerEffect: no
// state transition, so ValidateCommit's non-empty rule is satisfied by
// TrackerEffect alone (design section 4.5).
type trackerEffectHandler struct{}

func (trackerEffectHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	return store.HandlerCommit{
		TicketID: t.ID, Owner: d.Owner, Expires: d.Expires,
		TrackerEffect: &store.TrackerEffect{Ref: testFixtureRef, Notes: "already handled elsewhere"},
	}, nil
}

// TestTick_TrackerEffectPostsNothingToDoCommentAfterCommit proves the D12
// post-commit effect (design section 4.5, 6.8): once CommitHandlerResult has
// applied, the dispatcher resolves the ticket's project binding and posts
// tracker.NothingToDoComment for that binding's user.
func TestTick_TrackerEffectPostsNothingToDoCommentAfterCommit(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	const bindingUser = "peter"
	if _, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	}); err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: bindingUser}}

	rec := &commentingFixture{Fixture: newFixtureTracker(t)}
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("tracker comments = %d, want 1", len(got))
	}
	wantBody := tracker.NothingToDoComment(bindingUser, "already handled elsewhere")
	if got[0].body != wantBody {
		t.Errorf("comment body =\n%q\nwant\n%q", got[0].body, wantBody)
	}
	if got[0].ref != testFixtureRef {
		t.Errorf("comment ref = %q, want %q", got[0].ref, testFixtureRef)
	}
	if got[0].project != testProject.Name {
		t.Errorf("comment project = %q, want %q", got[0].project, testProject.Name)
	}
}

// TestTick_TrackerEffectFailureIsBestEffort proves a failing tracker comment
// only warns: Tick still returns nil, and the ticket's own commit (already
// applied before the tracker call runs) is untouched (design D12).
func TestTick_TrackerEffectFailureIsBestEffort(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testFixtureRef, Title: "t", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = trackerEffectHandler{}
	bindings := []dispatch.Binding{{StoreProjectID: projectID, TrackerProject: testProject.Name, User: "peter"}}

	rec := &commentingFixture{Fixture: newFixtureTracker(t)}
	rec.failFirst = true
	d := newDispatcher(t, s, rec, bus.New(), fakeRuntime(t), reg, bindings, dispatch.Config{MaxParallel: 2, Owner: testOwner})

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v, want nil (a tracker comment failure is best-effort)", err)
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("recorded comments = %d, want 0 (the one attempt failed)", len(got))
	}
	if attempts := rec.attemptCount(); attempts != 1 {
		t.Errorf("Comment attempts = %d, want 1", attempts)
	}

	final := getTicket(t, s, ticketID)
	if final.ClaimOwner != nil {
		t.Errorf("final ticket claim owner = %v, want nil (the ticket's own commit still applied)", *final.ClaimOwner)
	}
}
