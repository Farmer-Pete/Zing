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
	closeStore := func() error {
		if dispRunning.Load() {
			storeClosedWhileDispRunning.Store(true)
		}
		return nil
	}

	err := drainAndShutdown(
		context.Background(),
		20*time.Millisecond,
		func() error { return nil },
		dispDone,
		forceDisp,
		func(context.Context) error { return nil },
		closeStore,
	)
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
