package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestDrainAndShutdown_GracefulPath proves the ordinary path (design section
// 6.10): when dispDone closes well within drainDeadline, drainAndShutdown
// never calls forceDisp, and closeStore runs only once dispDone has
// observably closed, never before. dispDone closes on a short delay, not
// immediately, so a buggy implementation that ran shutdown/closeStore
// without first waiting on dispDone would be caught: closeStore would then
// run while dispDone was still open.
func TestDrainAndShutdown_GracefulPath(t *testing.T) {
	t.Parallel()

	dispDone := make(chan struct{})
	go func() {
		time.Sleep(5 * time.Millisecond)
		close(dispDone)
	}()

	var forceCalled atomic.Bool
	forceDisp := func() { forceCalled.Store(true) }

	var shutdownRanAfterDispDone atomic.Bool
	var closeStoreRanAfterDispDone atomic.Bool

	shutdown := func(context.Context) error {
		select {
		case <-dispDone:
			shutdownRanAfterDispDone.Store(true)
		default:
		}
		return nil
	}
	closeStore := func() error {
		select {
		case <-dispDone:
			closeStoreRanAfterDispDone.Store(true)
		default:
		}
		return nil
	}

	err := drainAndShutdown(
		context.Background(),
		time.Second, // far longer than the 5ms dispDone delay: the graceful path must win
		func() error { return nil },
		dispDone,
		forceDisp,
		shutdown,
		closeStore,
	)
	if err != nil {
		t.Fatalf("drainAndShutdown: %v", err)
	}
	if forceCalled.Load() {
		t.Error("forceDisp was called on the graceful path, want not called")
	}
	if !shutdownRanAfterDispDone.Load() {
		t.Error("shutdown ran before dispDone closed, want it to run only after")
	}
	if !closeStoreRanAfterDispDone.Load() {
		t.Error("closeStore ran before dispDone closed, want it to run only after")
	}
}

// TestDrainAndShutdown_TimeoutForcesCancelAndJoins proves the timeout
// backstop (design section 6.10): when dispDone does not close before the
// (tiny) drainDeadline, drainAndShutdown calls forceDisp, then blocks until
// dispDone closes (the forced dispatcher's simulated exit) before it ever
// calls closeStore. dispRunning is flipped false, independent of the
// dispDone channel, only once forceDisp has been observed and a short delay
// has passed, so closeStore checking dispRunning (not dispDone itself)
// catches a real ordering bug rather than just replaying the
// implementation's own control flow.
func TestDrainAndShutdown_TimeoutForcesCancelAndJoins(t *testing.T) {
	t.Parallel()

	dispDone := make(chan struct{})
	var forceCalled atomic.Bool
	var dispRunning atomic.Bool
	dispRunning.Store(true)

	forceDisp := func() { forceCalled.Store(true) }

	// Simulate the forced dispatcher goroutine: once forceDisp has been
	// called, it takes a little longer to actually exit, then flips
	// dispRunning false and closes dispDone, in that order.
	go func() {
		for !forceCalled.Load() {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(5 * time.Millisecond)
		dispRunning.Store(false)
		close(dispDone)
	}()

	var storeClosedWhileDispRunning atomic.Bool
	// closeStore always returns nil: it exists to observe ordering, not to
	// report a failure, but it must still satisfy drainAndShutdown's
	// closeStore func() error parameter.
	closeStore := func() error { //nolint:unparam // matches drainAndShutdown's closeStore func() error parameter
		if dispRunning.Load() {
			storeClosedWhileDispRunning.Store(true)
		}
		return nil
	}

	// drainAndShutdown's own forced-join wait (the "<-dispDone" right after
	// forceDisp(), inside its select's time.After branch) is deliberately
	// unbounded in production: the store must never close while a handler
	// may still be running (design section 6.10), so there is no deadline to
	// give it there. That means a regression that drops the forceDisp()
	// call, or otherwise stops dispDone from ever closing, would hang this
	// test (and `go test`) forever. Run it in a goroutine and bound the
	// wait here, in the test only, so that failure mode is a fast, clear
	// t.Fatal instead of a hang.
	done := make(chan error, 1)
	go func() {
		done <- drainAndShutdown(
			context.Background(),
			20*time.Millisecond,
			func() error { return nil },
			dispDone,
			forceDisp,
			func(context.Context) error { return nil },
			closeStore,
		)
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainAndShutdown did not return within 2s of the drain timeout; want forceDisp called and dispDone joined promptly (a regression dropping the forceDisp() call would hang here)")
	}
	if err != nil {
		t.Fatalf("drainAndShutdown: %v", err)
	}
	if !forceCalled.Load() {
		t.Fatal("forceDisp was not called on the timeout path, want called")
	}
	if storeClosedWhileDispRunning.Load() {
		t.Error("closeStore ran while the dispatcher was still (simulated) running, want it to run only after the forced join")
	}
}

// TestDrainAndShutdown_PropagatesShutdownAndStoreErrors proves the return
// value is the first of shutdown's and closeStore's errors, so a real
// failure in either is not swallowed.
func TestDrainAndShutdown_PropagatesShutdownAndStoreErrors(t *testing.T) {
	t.Parallel()

	dispDone := make(chan struct{})
	close(dispDone)

	wantShut := errors.New("shutdown boom")
	err := drainAndShutdown(
		context.Background(), time.Second, func() error { return nil }, dispDone,
		func() {}, func(context.Context) error { return wantShut }, func() error { return nil },
	)
	if !errors.Is(err, wantShut) {
		t.Errorf("drainAndShutdown (shutdown error) = %v, want %v", err, wantShut)
	}

	wantClose := errors.New("close boom")
	err = drainAndShutdown(
		context.Background(), time.Second, func() error { return nil }, dispDone,
		func() {}, func(context.Context) error { return nil }, func() error { return wantClose },
	)
	if !errors.Is(err, wantClose) {
		t.Errorf("drainAndShutdown (closeStore error) = %v, want %v", err, wantClose)
	}
}

// --- #45 milestone 5: the console stays up after a fail-closed stop -------

// TestWaitForShutdownTrigger_RealDispatcherFailureKeepsWaiting proves
// design section 4.7: a dispatcher goroutine ending with a real error (not
// nil, not context.Canceled) does not end the wait on its own. serve keeps
// waiting on errCh and ctx.Done() for an actual shutdown trigger, which
// here is a later ctx cancellation; dispTriggered still comes back true, so
// the dispatcher's own failure is still what serve eventually blames
// (dispatchFailure, used by shutdown).
func TestWaitForShutdownTrigger_RealDispatcherFailureKeepsWaiting(t *testing.T) {
	t.Parallel()

	errCh := make(chan error)
	dispDone := make(chan struct{})
	dispErr := errors.New("boom: dispatcher failed closed")

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	var serveErr error
	var consumedFromErrCh, dispTriggered bool
	go func() {
		consumedFromErrCh, dispTriggered, serveErr = waitForShutdownTrigger(ctx, errCh, dispDone, func() error { return dispErr })
		close(done)
	}()

	close(dispDone)

	// The wait must not have returned yet: a real dispatcher error alone
	// must not end it.
	select {
	case <-done:
		t.Fatal("waitForShutdownTrigger returned before any real shutdown trigger arrived")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForShutdownTrigger never returned after ctx was cancelled")
	}

	if serveErr != nil {
		t.Errorf("serveErr = %v, want nil (ctx.Done, not errCh, is what ended the wait)", serveErr)
	}
	if consumedFromErrCh {
		t.Error("consumedFromErrCh = true, want false")
	}
	if !dispTriggered {
		t.Error("dispTriggered = false, want true: the dispatcher's own earlier failure must still be named once a real trigger ends the wait")
	}
}

// TestWaitForShutdownTrigger_BenignDispDoneEndsWaitImmediately proves the
// pre-#45 behavior is unchanged for the one case design section 4.7 keeps
// it for: a dispatcher goroutine ending with nil (or context.Canceled)
// ends the wait right away, with dispTriggered true, exactly like today.
func TestWaitForShutdownTrigger_BenignDispDoneEndsWaitImmediately(t *testing.T) {
	t.Parallel()

	errCh := make(chan error)
	dispDone := make(chan struct{})
	close(dispDone)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	var consumedFromErrCh, dispTriggered bool
	var serveErr error
	go func() {
		consumedFromErrCh, dispTriggered, serveErr = waitForShutdownTrigger(ctx, errCh, dispDone, func() error { return nil })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForShutdownTrigger never returned for a benign (nil-error) dispDone")
	}
	if !dispTriggered {
		t.Error("dispTriggered = false, want true")
	}
	if consumedFromErrCh {
		t.Error("consumedFromErrCh = true, want false (dispDone, not errCh, is what ended the wait)")
	}
	if serveErr != nil {
		t.Errorf("serveErr = %v, want nil", serveErr)
	}
}

// TestResolveServeErr_DispatcherFailureNotMaskedByLaterListenerFailure
// proves PR review fix D3: a dispatcher failure that already set
// dispTriggered must win over a listener failure that happened to end the
// wait afterward (design section 4.7's own documented case: the dispatcher
// keeps serve's wait going until a real trigger arrives). Before this fix,
// shutdown's own "if serveErr == nil" ordering let the later listener
// failure silently mask the dispatcher's.
func TestResolveServeErr_DispatcherFailureNotMaskedByLaterListenerFailure(t *testing.T) {
	t.Parallel()
	dispErr := errors.New("boom: dispatcher failed closed")
	listenerErr := errors.New("boom: listener failed after the dispatcher")

	got := resolveServeErr(listenerErr, true, dispErr, nil)
	if !errors.Is(got, dispErr) {
		t.Errorf("resolveServeErr = %v, want it to wrap the dispatcher failure %v, not the later listener failure %v", got, dispErr, listenerErr)
	}
}

// TestResolveServeErr_ListenerFailureAloneIsReported proves resolveServeErr
// keeps today's behavior absent a dispatcher failure: an ordinary listener
// failure (dispTriggered false) is still what serve reports.
func TestResolveServeErr_ListenerFailureAloneIsReported(t *testing.T) {
	t.Parallel()
	listenerErr := errors.New("boom: listener failed")
	got := resolveServeErr(listenerErr, false, nil, nil)
	if !errors.Is(got, listenerErr) {
		t.Errorf("resolveServeErr = %v, want %v", got, listenerErr)
	}
}

// TestResolveServeErr_FallsBackToDrainError proves resolveServeErr's last
// priority: with no trigger error and no dispatcher failure, a real
// shutdown/closeStore failure from drainAndShutdown is still reported.
func TestResolveServeErr_FallsBackToDrainError(t *testing.T) {
	t.Parallel()
	drainErr := errors.New("boom: drain")
	got := resolveServeErr(nil, false, nil, drainErr)
	if !errors.Is(got, drainErr) {
		t.Errorf("resolveServeErr = %v, want %v", got, drainErr)
	}
}

// TestWaitForShutdownTrigger_ListenerFailureEndsWaitAndIsConsumed proves an
// HTTP listener failure (errCh) ends the wait and is reported back, even
// while the dispatcher is still healthy (dispDone never closes).
func TestWaitForShutdownTrigger_ListenerFailureEndsWaitAndIsConsumed(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)
	listenErr := errors.New("boom: listener failed")
	errCh <- listenErr
	dispDone := make(chan struct{}) // never closes

	consumedFromErrCh, dispTriggered, serveErr := waitForShutdownTrigger(t.Context(), errCh, dispDone, func() error { return nil })
	if !errors.Is(serveErr, listenErr) {
		t.Errorf("serveErr = %v, want %v", serveErr, listenErr)
	}
	if !consumedFromErrCh {
		t.Error("consumedFromErrCh = false, want true")
	}
	if dispTriggered {
		t.Error("dispTriggered = true, want false (the listener, not the dispatcher, ended the wait)")
	}
}
