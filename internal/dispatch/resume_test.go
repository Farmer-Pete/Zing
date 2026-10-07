package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
)

// TestRun_ParksAfterFailClosedUntilCancelled proves Run no longer returns
// on a worker error (task 2, design section "shape" rules): it parks --
// stop set, both alerts logged, no further claim or launch -- and keeps
// running its select loop until a drain or a context cancel ends it, so a
// console Resume (task 4) can bring the very same Run back without a serve
// restart.
func TestRun_ParksAfterFailClosedUntilCancelled(t *testing.T) {
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	reg[testStateQueued] = selfStealingHandler{}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after the fail-closed commit")

	select {
	case err := <-runErrCh:
		t.Fatalf("Run returned %v within 100ms of parking, want it to stay parked until cancelled", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	runErr := waitFor(t, runErrCh, "Run to return")
	if !errors.Is(runErr, dispatch.ErrFailClosed) {
		t.Errorf("Run err = %v, want errors.Is(err, dispatch.ErrFailClosed)", runErr)
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run err = %v, want errors.Is(err, context.Canceled)", runErr)
	}

	logged := logBuf.String()
	wantAlert2 := fmt.Sprintf("dispatcher stopped after fail-closed on ticket %d. Resume it from the console.", ticketID)
	if got := strings.Count(logged, wantAlert2); got != 1 {
		t.Errorf("alert 2 (%q) appeared %d times, want exactly 1 (log: %s)", wantAlert2, got, logged)
	}
	wantAlert1 := fmt.Sprintf("fail-closed on ticket %d", ticketID)
	if i1, i2 := strings.Index(logged, wantAlert1), strings.Index(logged, wantAlert2); i1 < 0 || i2 < 0 || i2 < i1 {
		t.Errorf("alerts out of order (alert1 at %d, alert2 at %d); log: %s", i1, i2, logged)
	}
}

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
