//go:build !race

package orchestrator

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// TestRunCommonLocked_CancelDuringRetryDelayReturnsCtxErr proves PR review
// fix C2: a ctx canceled while runCommonLocked is waiting out its own
// retry delay returns ctx.Err() at once, instead of sleeping out the full
// delay (and then checking it only before the next git call, as a bare
// time.Sleep would have). synctest.Wait reaches the exact moment the
// goroutine is parked in the retry delay's select, rather than guessing
// with a wall-clock sleep.
//
// testing/synctest is not supported under -race (a known upstream
// limitation), so this test lives in its own !race file; go test -race
// still builds and passes the rest of the package.
func TestRunCommonLocked_CancelDuringRetryDelayReturnsCtxErr(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &countingLockRunner{failCount: 1000}
		ctx, cancel := context.WithCancel(t.Context())

		errc := make(chan error, 1)
		go func() {
			_, err := runCommonLocked(ctx, r, "/x", "config", "--get", "foo")
			errc <- err
		}()

		synctest.Wait() // the goroutine is now parked in the retry delay's select
		cancel()

		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("runCommonLocked = %v, want context.Canceled", err)
		}
		if r.calls != 1 {
			t.Errorf("calls = %d, want 1 (canceled during the first retry delay)", r.calls)
		}
	})
}

// TestCommonMutex_LockReturnsCtxErrWhileWaiting proves PR review fix C4: a
// waiter blocked on an already-locked commonMutex gives up as soon as its
// own ctx ends, rather than blocking until the holder releases it --
// runCommon and every other lock site thread their own ctx through to
// exactly this call. synctest.Wait reaches the exact moment the second
// Lock is parked waiting, rather than guessing with a wall-clock timeout.
//
// testing/synctest is not supported under -race (a known upstream
// limitation), so this test lives in its own !race file; go test -race
// still builds and passes the rest of the package.
func TestCommonMutex_LockReturnsCtxErrWhileWaiting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		mu := newCommonMutex()
		if err := mu.Lock(t.Context()); err != nil {
			t.Fatalf("first Lock: %v", err)
		}
		// mu is now held and deliberately never released in this test.

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- mu.Lock(ctx) }()

		synctest.Wait() // the second Lock is now parked waiting for mu or ctx

		select {
		case err := <-done:
			t.Fatalf("second Lock returned %v before ctx was even canceled, want it still waiting", err)
		default:
		}

		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("second Lock = %v, want context.Canceled", err)
		}
	})
}
