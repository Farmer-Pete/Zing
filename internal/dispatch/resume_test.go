package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/store"
)

// syncBuffer is a mutex-guarded bytes.Buffer, for a test that must poll a
// slog.TextHandler's output while Run's own goroutine may still be writing
// to it concurrently (ticket #89 review r1f13): a bare bytes.Buffer is not
// safe for that, only for a read that happens after Run has already
// returned.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForSignalThen loops on ch, checking cond right after each signal,
// until cond reports true or go test's own -timeout nears. Unlike a
// waitFor on ch followed by a separate waitUntil poll, this proves cond
// turned true because of a signal this call itself received, not because
// of some earlier, unrelated wake already sitting ahead of it (ticket #89
// review r1f14).
func waitForSignalThen(t *testing.T, ch <-chan struct{}, cond func() bool, what string) {
	t.Helper()

	deadline, ok := t.Deadline()
	var limit time.Time
	if ok {
		limit = time.Now().Add(time.Until(deadline) * 9 / 10)
	}

	for {
		if ok {
			remaining := time.Until(limit)
			if remaining <= 0 {
				t.Fatalf("%s: still waiting near go test's -timeout", what)
			}
			timer := time.NewTimer(remaining)
			select {
			case <-ch:
				timer.Stop()
			case <-timer.C:
				t.Fatalf("%s: still waiting near go test's -timeout", what)
			}
		} else {
			<-ch
		}
		if cond() {
			return
		}
	}
}

// failOnceHandler self-steals its own ticket's claim on its first call, the
// same way selfStealingHandler always does, so that one call fails closed;
// every later call instead returns a plain, successful transition commit,
// so a test can prove Resume lets the very same ticket run again once its
// claim has expired (owner decision Q3).
type failOnceHandler struct {
	calls atomic.Int64
}

func (h *failOnceHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if h.calls.Add(1) == 1 {
		if _, err := d.Store.CommitHandlerResult(ctx, store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}); err != nil {
			return store.HandlerCommit{}, err
		}
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: testStatePlanning, Reason: testSpyReason}, nil
}

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

	logBuf := &syncBuffer{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after the fail-closed commit")

	wantAlert2 := fmt.Sprintf("dispatcher stopped after fail-closed on ticket %d. Resume it from the console.", ticketID)
	waitUntil(t, func() bool { return strings.Contains(logBuf.String(), wantAlert2) }, "alert 2 to log while Run stays parked, before cancel")

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

// TestStopStatus_ReportsOwnerAndErrorStops proves StopStatus tells an
// owner's own POST /stop apart from a real dispatcher error (design section
// "shape" rules): a fresh dispatcher is not stopped; the store's stopped
// flag alone, with no in-memory stopErr, reports StopKindOwner with no
// cause, no ticket, and a zero At; an in-memory stopErr reports
// StopKindError with its own cause and a non-zero At.
func TestStopStatus_ReportsOwnerAndErrorStops(t *testing.T) {
	t.Parallel()

	t.Run("fresh", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

		status, err := d.StopStatus(t.Context())
		if err != nil {
			t.Fatalf("StopStatus: %v", err)
		}
		if status.Stopped {
			t.Errorf("Stopped = true, want false for a fresh dispatcher")
		}
		if status.InFlight != 0 {
			t.Errorf("InFlight = %d, want 0", status.InFlight)
		}
	})

	t.Run("owner", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

		if err := s.SetStopped(t.Context(), true); err != nil {
			t.Fatalf("SetStopped: %v", err)
		}

		status, err := d.StopStatus(t.Context())
		if err != nil {
			t.Fatalf("StopStatus: %v", err)
		}
		if !status.Stopped {
			t.Error("Stopped = false, want true once the store's stopped flag is set")
		}
		if status.Kind != dispatch.StopKindOwner {
			t.Errorf("Kind = %q, want %q", status.Kind, dispatch.StopKindOwner)
		}
		if status.Cause != "" {
			t.Errorf("Cause = %q, want empty for an owner stop", status.Cause)
		}
		if status.HasTicket {
			t.Error("HasTicket = true, want false for an owner stop")
		}
		if !status.At.IsZero() {
			t.Errorf("At = %v, want zero for an owner stop", status.At)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

		dispatch.SetStopForTest(d, errors.New("boom"))

		status, err := d.StopStatus(t.Context())
		if err != nil {
			t.Fatalf("StopStatus: %v", err)
		}
		if status.Kind != dispatch.StopKindError {
			t.Errorf("Kind = %q, want %q", status.Kind, dispatch.StopKindError)
		}
		if status.Cause != "boom" {
			t.Errorf("Cause = %q, want %q", status.Cause, "boom")
		}
		if status.HasTicket {
			t.Error("HasTicket = true, want false")
		}
		if status.At.IsZero() {
			t.Error("At = zero, want non-zero once setStop has recorded an error")
		}
	})
}

// TestRun_PublishesAsStoppedRunsFinish proves Run publishes on the bus
// after every result it reads while parked, whatever stopped it (design
// section "shape" rules): a fail-closed's own InFlight count and an
// owner's own InFlight count both drop to 0, and the bus wakes a
// subscriber, once Run has read the one still-finishing worker's result.
func TestRun_PublishesAsStoppedRunsFinish(t *testing.T) {
	t.Run("fail_closed", func(t *testing.T) {
		s := newDispatchTestStore(t)
		aID := seedQueuedTicket(t, s, "fake#1")
		bID := seedQueuedTicket(t, s, "fake#2")

		started := make(chan int64, 1)
		release := make(chan struct{})
		reg := job.Registry()
		reg[testStateQueued] = &perTicketHandler{byTicket: map[int64]job.Handler{
			aID: selfStealingHandler{},
			bID: &barrierHandler{started: started, release: release, next: testStatePlanning, reason: testSpyReason},
		}}

		b := bus.New()
		d := newDispatcher(t, s, newFixtureTracker(t), b, fakeRuntime(t), reg, nil,
			dispatch.Config{MaxParallel: 2, Interval: 5 * time.Millisecond, Owner: testOwner})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runErrCh := make(chan error, 1)
		go func() { runErrCh <- d.Run(ctx) }()

		waitFor(t, started, "B's handler to start")
		waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after A's fail-closed commit")
		waitUntil(t, func() bool {
			status, err := d.StopStatus(t.Context())
			return err == nil && status.InFlight == 1
		}, "InFlight to settle at 1 (A's result read, B's still owed)")

		ch, cancelSub := b.Subscribe()
		defer cancelSub()
		close(release)

		waitForSignalThen(t, ch, func() bool {
			status, err := d.StopStatus(t.Context())
			return err == nil && status.InFlight == 0
		}, "a bus signal after which InFlight reads 0")

		status, err := d.StopStatus(t.Context())
		if err != nil {
			t.Fatalf("StopStatus: %v", err)
		}
		if status.Kind != dispatch.StopKindFailClosed {
			t.Errorf("Kind = %q, want %q", status.Kind, dispatch.StopKindFailClosed)
		}

		cancel()
		_ = waitFor(t, runErrCh, "Run to return") //nolint:errcheck // this test only proves Run returns after cancel
	})

	t.Run("owner", func(t *testing.T) {
		s := newDispatchTestStore(t)
		seedQueuedTicket(t, s, testFixtureRef)

		started := make(chan int64, 1)
		release := make(chan struct{})
		reg := job.Registry()
		reg[testStateQueued] = &barrierHandler{started: started, release: release, next: testStatePlanning, reason: testSpyReason}

		b := bus.New()
		d := newDispatcher(t, s, newFixtureTracker(t), b, fakeRuntime(t), reg, nil,
			dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runErrCh := make(chan error, 1)
		go func() { runErrCh <- d.Run(ctx) }()

		waitFor(t, started, "handler to start")

		if err := s.SetStopped(t.Context(), true); err != nil {
			t.Fatalf("SetStopped: %v", err)
		}
		waitUntil(t, func() bool {
			status, err := d.StopStatus(t.Context())
			return err == nil && status.Kind == dispatch.StopKindOwner && status.InFlight == 1
		}, "StopStatus to report owner with InFlight 1")

		ch, cancelSub := b.Subscribe()
		defer cancelSub()
		close(release)

		waitForSignalThen(t, ch, func() bool {
			status, err := d.StopStatus(t.Context())
			return err == nil && status.Kind == dispatch.StopKindOwner && status.InFlight == 0
		}, "a bus signal after which StopStatus reports owner with InFlight 0")

		cancel()
		_ = waitFor(t, runErrCh, "Run to return") //nolint:errcheck // this test only proves Run returns after cancel
	})
}

// TestResume_AfterFailClosedRunDispatchesNextTick is the plan's own demo
// (task 4): a real dispatcher fails closed on a ticket under Run, StopStatus
// names it, Resume clears the stop without Run ever returning, and the same
// ticket dispatches again on Run's very next tick -- no new Run, no serve
// restart.
func TestResume_AfterFailClosedRunDispatchesNextTick(t *testing.T) {
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	reg := job.Registry()
	handler := &failOnceHandler{}
	reg[testStateQueued] = handler

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
	waitUntil(t, func() bool {
		status, err := d.StopStatus(t.Context())
		return err == nil && status.InFlight == 0
	}, "InFlight to settle at 0")

	status, err := d.StopStatus(t.Context())
	if err != nil {
		t.Fatalf("StopStatus: %v", err)
	}
	if !status.Stopped {
		t.Error("Stopped = false, want true")
	}
	if status.Kind != dispatch.StopKindFailClosed {
		t.Errorf("Kind = %q, want %q", status.Kind, dispatch.StopKindFailClosed)
	}
	if !status.HasTicket || status.TicketID != ticketID {
		t.Errorf("HasTicket, TicketID = %v, %d, want true, %d", status.HasTicket, status.TicketID, ticketID)
	}
	if !strings.Contains(status.Cause, "the lease was lost") {
		t.Errorf("Cause = %q, want it to contain %q", status.Cause, "the lease was lost")
	}
	if status.At.IsZero() {
		t.Error("At = zero, want non-zero once setStop has recorded an error")
	}

	if _, stopped, err := s.Flags(t.Context()); err != nil {
		t.Fatalf("Flags: %v", err)
	} else if !stopped {
		t.Error("store stopped flag = false, want true after the fail-closed commit")
	}

	if err := d.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if _, stopped, err := s.Flags(t.Context()); err != nil {
		t.Fatalf("Flags: %v", err)
	} else if stopped {
		t.Error("store stopped flag = true, want false after Resume")
	}

	waitUntil(t, func() bool {
		return getTicket(t, s, ticketID).State == testStatePlanning
	}, "the ticket to dispatch again and reach planning")

	select {
	case runErr := <-runErrCh:
		t.Fatalf("Run returned %v after Resume, want it to keep running the same loop", runErr)
	default:
	}

	cancel()
	runErr := waitFor(t, runErrCh, "Run to return")
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run err = %v, want nil or context.Canceled", runErr)
	}

	logged := logBuf.String()
	wantResumeLine := "dispatcher resumed from the console"
	if !strings.Contains(logged, wantResumeLine) {
		t.Fatalf("log does not contain %q (log: %s)", wantResumeLine, logged)
	}
	wantAttrs := "kind=" + dispatch.StopKindFailClosed
	if !strings.Contains(logged, wantAttrs) {
		t.Errorf("log does not contain %q (log: %s)", wantAttrs, logged)
	}
	wantTicket := fmt.Sprintf("ticket_id=%d", ticketID)
	if !strings.Contains(logged, wantTicket) {
		t.Errorf("log does not contain %q (log: %s)", wantTicket, logged)
	}
}

// TestResume_Refusals proves Resume's three refusal cases (design section
// "shape" rules, task 4): draining, not stopped, and runs still finishing,
// each with its own exact ResumeRefusal sentence, and that none of them
// touch the store's stopped flag.
func TestResume_Refusals(t *testing.T) {
	t.Parallel()

	t.Run("not_stopped", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

		err := d.Resume(t.Context())
		var refusal *dispatch.ResumeRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("Resume err = %v, want a *dispatch.ResumeRefusal", err)
		}
		if refusal.Reason != "the dispatcher is not stopped" {
			t.Errorf("Reason = %q, want %q", refusal.Reason, "the dispatcher is not stopped")
		}
	})

	t.Run("draining", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

		dispatch.SetStopForTest(d, errors.New("boom"))
		if err := s.SetDraining(t.Context(), true); err != nil {
			t.Fatalf("SetDraining: %v", err)
		}

		err := d.Resume(t.Context())
		var refusal *dispatch.ResumeRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("Resume err = %v, want a *dispatch.ResumeRefusal", err)
		}
		if refusal.Reason != "zing is shutting down; there is nothing to resume" {
			t.Errorf("Reason = %q, want %q", refusal.Reason, "zing is shutting down; there is nothing to resume")
		}

		status, statusErr := d.StopStatus(t.Context())
		if statusErr != nil {
			t.Fatalf("StopStatus: %v", statusErr)
		}
		if !status.Stopped || status.Kind != dispatch.StopKindError {
			t.Errorf("StopStatus = %+v, want Stopped true and Kind %q (untouched by the refusal)", status, dispatch.StopKindError)
		}
	})

	t.Run("busy", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		aID := seedQueuedTicket(t, s, "fake#1")
		bID := seedQueuedTicket(t, s, "fake#2")

		started := make(chan int64, 1)
		release := make(chan struct{})
		reg := job.Registry()
		reg[testStateQueued] = &perTicketHandler{byTicket: map[int64]job.Handler{
			aID: selfStealingHandler{},
			bID: &barrierHandler{started: started, release: release, next: testStatePlanning, reason: testSpyReason},
		}}

		b := bus.New()
		d := newDispatcher(t, s, newFixtureTracker(t), b, fakeRuntime(t), reg, nil,
			dispatch.Config{MaxParallel: 2, Interval: 5 * time.Millisecond, Owner: testOwner})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runErrCh := make(chan error, 1)
		go func() { runErrCh <- d.Run(ctx) }()

		waitFor(t, started, "B's handler to start")
		waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after A's fail-closed commit")
		waitUntil(t, func() bool {
			status, err := d.StopStatus(t.Context())
			return err == nil && status.InFlight == 1
		}, "InFlight to settle at 1 (A's result read, B's still owed)")

		err := d.Resume(t.Context())
		var refusal *dispatch.ResumeRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("Resume err = %v, want a *dispatch.ResumeRefusal", err)
		}
		if refusal.Reason != "1 runs are still finishing; resume once they are done" {
			t.Errorf("Reason = %q, want %q", refusal.Reason, "1 runs are still finishing; resume once they are done")
		}

		if _, stopped, err := s.Flags(t.Context()); err != nil {
			t.Fatalf("Flags: %v", err)
		} else if !stopped {
			t.Error("store stopped flag = false, want true (untouched by the refusal)")
		}

		close(release)
		waitUntil(t, func() bool {
			status, err := d.StopStatus(t.Context())
			return err == nil && status.InFlight == 0
		}, "InFlight to settle at 0 once Run reads B's result")

		if err := d.Resume(t.Context()); err != nil {
			t.Fatalf("Resume: %v", err)
		}

		cancel()
		_ = waitFor(t, runErrCh, "Run to return") //nolint:errcheck // this test only proves Resume succeeds once InFlight is 0
	})

	t.Run("result_unread", func(t *testing.T) {
		t.Parallel()

		s := newDispatchTestStore(t)
		seedQueuedTicket(t, s, testFixtureRef)

		reg := job.Registry()
		reg[testStateQueued] = selfStealingHandler{}

		d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
			dispatch.Config{MaxParallel: 1, Owner: testOwner})

		if _, _, err := dispatch.FillForTest(t.Context(), d); err != nil {
			t.Fatalf("FillForTest: %v", err)
		}
		dispatch.WaitWorkersForTest(d)

		err := d.Resume(t.Context())
		var refusal *dispatch.ResumeRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("Resume err = %v, want a *dispatch.ResumeRefusal", err)
		}
		if refusal.Reason != "1 runs are still finishing; resume once they are done" {
			t.Errorf("Reason = %q, want %q", refusal.Reason, "1 runs are still finishing; resume once they are done")
		}

		if got := dispatch.PendingForTest(d); got != 1 {
			t.Errorf("PendingForTest = %d, want 1", got)
		}
	})
}

// TestResume_OwnerStopClearsFlag proves Resume clears an owner's own POST
// /stop {"all": true} too (owner decision Q1), with no in-memory stopErr to
// clear.
func TestResume_OwnerStopClearsFlag(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	b := bus.New()
	d := newDispatcher(t, s, newFixtureTracker(t), b, fakeRuntime(t), nil, nil, dispatch.Config{MaxParallel: 1, Owner: testOwner})

	if err := s.SetStopped(t.Context(), true); err != nil {
		t.Fatalf("SetStopped: %v", err)
	}

	ch, cancelSub := b.Subscribe()
	defer cancelSub()

	if err := d.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	waitFor(t, ch, "a bus signal once Resume succeeds")

	if _, stopped, err := s.Flags(t.Context()); err != nil {
		t.Fatalf("Flags: %v", err)
	} else if stopped {
		t.Error("store stopped flag = true, want false after Resume")
	}

	status, err := d.StopStatus(t.Context())
	if err != nil {
		t.Fatalf("StopStatus: %v", err)
	}
	if status.Stopped {
		t.Error("Stopped = true, want false after Resume")
	}
}
