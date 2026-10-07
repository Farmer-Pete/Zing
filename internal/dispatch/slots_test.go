package dispatch_test

import (
	"testing"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
)

// TestSlots_ReportsInflightOwnerAndMaxParallel proves Slots reports this
// dispatcher's claim owner, its live max_parallel, and exactly the ticket
// ids it is running right now, in ascending order, and that the snapshot
// goes empty again once every run finishes.
func TestSlots_ReportsInflightOwnerAndMaxParallel(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	id1 := seedQueuedTicket(t, s, "fake#1")
	id2 := seedQueuedTicket(t, s, "fake#2")

	started := make(chan int64, 2)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &barrierHandler{started: started, release: release}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 2, Owner: testOwner})

	tickErrCh := make(chan error, 1)
	go func() { tickErrCh <- d.Tick(t.Context()) }()

	waitFor(t, started, "first handler to start")
	waitFor(t, started, "second handler to start")

	snap := d.Slots()
	if snap.Owner != testOwner {
		t.Errorf("Slots().Owner = %q, want %q", snap.Owner, testOwner)
	}
	if snap.MaxParallel != 2 {
		t.Errorf("Slots().MaxParallel = %d, want 2", snap.MaxParallel)
	}
	if want := []int64{id1, id2}; !equalIDs(snap.Inflight, want) {
		t.Errorf("Slots().Inflight = %v, want %v (ascending)", snap.Inflight, want)
	}

	close(release)
	if err := waitFor(t, tickErrCh, "Tick to return"); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	snap = d.Slots()
	if len(snap.Inflight) != 0 {
		t.Errorf("Slots().Inflight after Tick returned = %v, want empty", snap.Inflight)
	}
	if snap.Inflight == nil {
		t.Error("Slots().Inflight after Tick returned = nil, want a non-nil empty slice")
	}
}

// TestSlots_FreshDispatcherHasEmptyInflight proves a dispatcher that has
// never ticked reports an empty, non-nil Inflight.
func TestSlots_FreshDispatcherHasEmptyInflight(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner})

	snap := d.Slots()
	if snap.Inflight == nil {
		t.Error("Slots().Inflight = nil, want a non-nil empty slice")
	}
	if len(snap.Inflight) != 0 {
		t.Errorf("Slots().Inflight = %v, want empty", snap.Inflight)
	}
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i, id := range got {
		if id != want[i] {
			return false
		}
	}
	return true
}
