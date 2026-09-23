package job_test

import (
	"testing"

	zing "zing"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/store"
)

// TestValidate_RealMachineTOMLHasAHandlerForEveryNonTerminalState proves
// Registry() satisfies Validate against the real, checked-in machine.toml
// (design section 6.5): a missing handler must fail at startup, not at a
// nil map read mid-tick.
func TestValidate_RealMachineTOMLHasAHandlerForEveryNonTerminalState(t *testing.T) {
	t.Parallel()

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}

	if err := job.Validate(m, job.Registry()); err != nil {
		t.Fatalf("Validate(real machine.toml, Registry()): %v, want nil", err)
	}
}

// TestValidate_MissingHandlerFails proves Validate actually catches an
// absent handler, so the previous test's pass is not vacuous.
func TestValidate_MissingHandlerFails(t *testing.T) {
	t.Parallel()

	m := &machine.Machine{
		States: machine.States{Order: []string{testStateQueued, testStatePlanning, testStateDone}, Terminal: []string{testStateDone}},
	}
	reg := map[string]job.Handler{testStateQueued: job.Registry()[testStateQueued]} // testStatePlanning missing on purpose

	if err := job.Validate(m, reg); err == nil {
		t.Error("Validate with a missing handler: want an error, got nil")
	}
}

// TestValidate_TerminalStateNeedsNoHandler proves a terminal state is
// exempt: Registry() has no handler for the terminal state, and a machine
// whose only non-terminal state is handled must still pass.
func TestValidate_TerminalStateNeedsNoHandler(t *testing.T) {
	t.Parallel()

	m := &machine.Machine{
		States: machine.States{Order: []string{testStateQueued, testStateDone}, Terminal: []string{testStateDone}},
	}
	reg := map[string]job.Handler{testStateQueued: job.Registry()[testStateQueued]}

	if err := job.Validate(m, reg); err != nil {
		t.Errorf("Validate with done exempt: %v, want nil", err)
	}
}

// TestValidate_NilHandlerValueFails proves a nil Handler value under a
// present key is caught the same way a missing key is: the map has an entry
// for testStatePlanning, but it holds a nil interface value rather than a
// real handler, which would otherwise panic on the first Run call mid-tick
// rather than failing at startup.
func TestValidate_NilHandlerValueFails(t *testing.T) {
	t.Parallel()

	m := &machine.Machine{
		States: machine.States{Order: []string{testStateQueued, testStatePlanning, testStateDone}, Terminal: []string{testStateDone}},
	}
	reg := map[string]job.Handler{
		testStateQueued:   job.Registry()[testStateQueued],
		testStatePlanning: nil, // present key, nil value
	}

	if err := job.Validate(m, reg); err == nil {
		t.Error("Validate with a nil handler value: want an error, got nil")
	}
}

// --- ValidateCommit ----------------------------------------------------------

func TestValidateCommit_AcceptsALegalTransitionWithAReason(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{State: testStateQueued}
	commit := store.HandlerCommit{Next: testStatePlanning, Reason: testReasonPickedUp}

	if err := job.ValidateCommit(ticket, commit); err != nil {
		t.Errorf("ValidateCommit(legal edge, reason set): %v, want nil", err)
	}
}

func TestValidateCommit_RejectsAnIllegalEdge(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{State: testStateQueued}
	commit := store.HandlerCommit{Next: testStateBuilding, Reason: "skip ahead"}

	if err := job.ValidateCommit(ticket, commit); err == nil {
		t.Error("ValidateCommit(queued -> building): want an error, got nil")
	}
}

func TestValidateCommit_RejectsAnEmptyReasonOnATransition(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{State: testStateQueued}
	commit := store.HandlerCommit{Next: testStatePlanning}

	if err := job.ValidateCommit(ticket, commit); err == nil {
		t.Error("ValidateCommit(transition, no reason): want an error, got nil")
	}
}

func TestValidateCommit_NoTransitionNeedsNoReason(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{State: testStatePlanning}
	commit := store.HandlerCommit{Waiting: new("questions")}

	if err := job.ValidateCommit(ticket, commit); err != nil {
		t.Errorf("ValidateCommit(no transition, waiting only): %v, want nil", err)
	}
}

func TestValidateCommit_RejectsATransitionCombinedWithANonErrorWait(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{State: testStatePlanning}
	commit := store.HandlerCommit{Next: testStateBuilding, Reason: "plan ready", Waiting: new("questions")}

	if err := job.ValidateCommit(ticket, commit); err == nil {
		t.Error("ValidateCommit(transition + non-error waiting): want an error, got nil")
	}
}

// TestValidateCommit_RejectsATicketIDMismatch proves the first check ranks
// above every other rule: a commit built against a different ticket id than
// the one it is validated against is rejected outright, even though its
// shape (Next, Reason) would otherwise be legal.
func TestValidateCommit_RejectsATicketIDMismatch(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{ID: 1, State: testStateQueued}
	commit := store.HandlerCommit{TicketID: 2, Next: testStatePlanning, Reason: testReasonPickedUp}

	if err := job.ValidateCommit(ticket, commit); err == nil {
		t.Error("ValidateCommit(commit.TicketID != ticket.ID): want an error, got nil")
	}
}

// TestValidateCommit_RejectsAWhollyEmptyCommit proves a commit that carries
// no Next, Waiting, Messages, Runs, ResolveQuestions, or Session is rejected:
// it would apply nothing but the claim-release fence, which is
// CommitHandlerResult's own releaseClaim no-op, never a handler's commit.
func TestValidateCommit_RejectsAWhollyEmptyCommit(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{ID: 1, State: testStateQueued}
	commit := store.HandlerCommit{TicketID: 1}

	if err := job.ValidateCommit(ticket, commit); err == nil {
		t.Error("ValidateCommit(wholly empty commit): want an error, got nil")
	}
}

func TestValidateCommit_RejectsAWaitingValueOutsideTheEightFlags(t *testing.T) {
	t.Parallel()

	ticket := store.Ticket{State: testStatePlanning}
	commit := store.HandlerCommit{Waiting: new("bogus")}

	if err := job.ValidateCommit(ticket, commit); err == nil {
		t.Error("ValidateCommit(waiting=bogus): want an error, got nil")
	}
}

func TestValidateCommit_EveryDesignSection7_1EdgeIsLegal(t *testing.T) {
	t.Parallel()

	edges := []struct{ from, to string }{
		{testStateQueued, testStatePlanning},
		{testStatePlanning, testStateBuilding},
		{testStateBuilding, testStateReviewing},
		{testStateReviewing, testStateJudging},
		{testStateJudging, testStateShipping},
		{testStateShipping, testStateDone},
	}
	for _, e := range edges {
		ticket := store.Ticket{State: e.from}
		commit := store.HandlerCommit{Next: e.to, Reason: "go"}
		if err := job.ValidateCommit(ticket, commit); err != nil {
			t.Errorf("ValidateCommit(%s -> %s): %v, want nil", e.from, e.to, err)
		}
	}
}

// --- OrderCandidates ----------------------------------------------------------

func TestOrderCandidates_ReverseStateOrderWins(t *testing.T) {
	t.Parallel()

	order := []string{testStateQueued, testStatePlanning, testStateBuilding, testStateReviewing, testStateJudging, testStateShipping}
	candidates := []store.Ticket{
		{ID: 1, State: testStateQueued, TrackerRef: "1"},
		{ID: 2, State: testStateShipping, TrackerRef: "2"},
		{ID: 3, State: testStateBuilding, TrackerRef: "3"},
	}

	got := job.OrderCandidates(candidates, order)
	want := []int64{2, 3, 1} // shipping (index 5), building (index 2), queued (index 0)
	assertIDOrder(t, got, want)
}

func TestOrderCandidates_NumericExternalIDTieBreak(t *testing.T) {
	t.Parallel()

	order := []string{testStateQueued}
	candidates := []store.Ticket{
		{ID: 1, State: testStateQueued, TrackerRef: "fake#10"},
		{ID: 2, State: testStateQueued, TrackerRef: "fake#2"},
	}

	got := job.OrderCandidates(candidates, order)
	// "fake#2" (numeric 2) must sort before "fake#10" (numeric 10), even
	// though "10" < "2" lexically as text.
	assertIDOrder(t, got, []int64{2, 1})
}

func TestOrderCandidates_NonNumericRefSortsLast(t *testing.T) {
	t.Parallel()

	order := []string{testStateQueued}
	candidates := []store.Ticket{
		{ID: 1, State: testStateQueued, TrackerRef: "no-digits"},
		{ID: 2, State: testStateQueued, TrackerRef: testRefFake1},
	}

	got := job.OrderCandidates(candidates, order)
	assertIDOrder(t, got, []int64{2, 1})
}

func TestOrderCandidates_RowIDIsTheFinalTieBreak(t *testing.T) {
	t.Parallel()

	order := []string{testStateQueued}
	candidates := []store.Ticket{
		{ID: 5, State: testStateQueued, TrackerRef: testRefFake1},
		{ID: 3, State: testStateQueued, TrackerRef: testRefFake1},
	}

	got := job.OrderCandidates(candidates, order)
	assertIDOrder(t, got, []int64{3, 5})
}

func TestOrderCandidates_DoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	order := []string{testStateQueued}
	candidates := []store.Ticket{
		{ID: 5, State: testStateQueued, TrackerRef: testRefFake1},
		{ID: 3, State: testStateQueued, TrackerRef: testRefFake1},
	}
	original := append([]store.Ticket(nil), candidates...)

	job.OrderCandidates(candidates, order)

	for i := range candidates {
		if candidates[i].ID != original[i].ID {
			t.Errorf("OrderCandidates mutated its input slice: [%d] = %d, want unchanged %d", i, candidates[i].ID, original[i].ID)
		}
	}
}

func assertIDOrder(t *testing.T, got []store.Ticket, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("OrderCandidates returned %d tickets, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("OrderCandidates[%d].ID = %d, want %d", i, got[i].ID, id)
		}
	}
}
