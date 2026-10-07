package dispatch

import "context"

// SetAfterClaimForTest installs a hook fill calls synchronously right after
// a successful Claim for a ticket, before fill's own stop-check-and-launch
// critical section (design section 4.2 step 5). It exists only to let a
// test in the external dispatch_test package open that otherwise
// sub-microsecond race window -- pausing one goroutine there while another
// calls setStop, NotifyDrain, or cancels the pass's own ctx -- and must
// never be called from production code.
func SetAfterClaimForTest(d *Dispatcher, f func(ticketID int64)) {
	d.afterClaimForTest = f
}

// IsStoppedForTest reports whether setStop has been called on d yet, for a
// test that needs to poll for a worker's own failure to have taken effect
// before proceeding (design section 4.2, 4.6).
func IsStoppedForTest(d *Dispatcher) bool {
	return d.isStopped()
}

// SetStopForTest calls setStop(err) directly, letting a test simulate the
// dispatcher having already been told to stop (design section 4.1) without
// driving a real failing worker or a real drain.
func SetStopForTest(d *Dispatcher, err error) bool {
	return d.setStop(err)
}

// SetBeforeClaimForTest installs a hook fill calls synchronously right
// before attempting Claim for a ticket, for each candidate in pick order
// (design section 4.2 step 5). It exists only to let a test block a later
// candidate's claim attempt in the same pass until a concurrently running
// worker (launched for an earlier candidate in that same pass) has reached
// a specific point, making a race between that worker and fill's own
// continued pass deterministic instead of timing-dependent.
func SetBeforeClaimForTest(d *Dispatcher, f func(ticketID int64)) {
	d.beforeClaimForTest = f
}

// SetStopErrRecordedForTest installs a hook setStop calls synchronously,
// right after it is the first call to record a non-nil stopErr (design
// section 4.6), with that same error. It exists only to let a test learn
// the exact moment reportFirstError's own eventual description became
// fixed, without polling or sleeping.
func SetStopErrRecordedForTest(d *Dispatcher, f func(err error)) {
	d.stopErrRecordedForTest = f
}

// FillForTest calls d.fill with a fresh results channel of capacity
// cfg.MaxParallel, letting a test launch workers directly without driving a
// full Tick or Run pass.
func FillForTest(ctx context.Context, d *Dispatcher) (results chan runResult, launched int, err error) {
	results = make(chan runResult, d.cfg.MaxParallel)
	launched, err = d.fill(ctx, results)
	return results, launched, err
}

// WaitWorkersForTest blocks until every worker fill has ever launched on d
// has sent its result and returned, the same wait finish uses.
func WaitWorkersForTest(d *Dispatcher) {
	d.wg.Wait()
}

// PendingForTest reads d.pending under d.mu, for a test asserting the
// unread-result count directly.
func PendingForTest(d *Dispatcher) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending
}

// SetBeforeResumeClearForTest installs a hook Resume calls synchronously
// after it clears the store's stopped flag and right before its own
// re-check-and-clear critical section locks d.mu. It exists only to let a
// test open that race window -- call setStop (directly, or through park)
// from another goroutine while this goroutine is paused here -- and must
// never be called from production code.
func SetBeforeResumeClearForTest(d *Dispatcher, f func()) {
	d.beforeResumeClearForTest = f
}
