package dispatch_test

import (
	"testing"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
)

// TestPending_CountsUnreadResults proves the dispatcher's pending count
// tracks launched workers whose result no caller has read yet: it goes up
// when fill launches a worker, stays up once that worker has sent its
// result and left inflight until a caller actually reads it off the
// results channel, and comes back down to 0 once Tick has read every
// result it launched (design "shape" rules, task 1).
func TestPending_CountsUnreadResults(t *testing.T) {
	t.Parallel()

	t.Run("unread", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		seedQueuedTicket(t, s, testFixtureRef)

		reg := job.Registry()
		reg[testStateQueued] = &spyHandler{next: testStatePlanning, reason: testSpyReason}

		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
			dispatch.Config{MaxParallel: 1, Owner: testOwner})

		_, launched, err := dispatch.FillForTest(t.Context(), d)
		if err != nil {
			t.Fatalf("FillForTest: %v", err)
		}
		if launched != 1 {
			t.Fatalf("launched = %d, want 1", launched)
		}

		// WaitWorkersForTest returns only once the worker has sent its
		// result and left inflight, but nobody has read that result yet.
		dispatch.WaitWorkersForTest(d)

		if got := dispatch.PendingForTest(d); got != 1 {
			t.Errorf("PendingForTest = %d, want 1 (result sent but never read)", got)
		}
	})

	t.Run("tick", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		seedQueuedTicket(t, s, testFixtureRef)

		reg := job.Registry()
		reg[testStateQueued] = &spyHandler{next: testStatePlanning, reason: testSpyReason}

		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
			dispatch.Config{MaxParallel: 1, Owner: testOwner})

		if err := d.Tick(t.Context()); err != nil {
			t.Fatalf("Tick: %v", err)
		}

		if got := dispatch.PendingForTest(d); got != 0 {
			t.Errorf("PendingForTest = %d, want 0 once Tick has read every result it launched", got)
		}
	})
}
