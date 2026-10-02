package dispatch

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
