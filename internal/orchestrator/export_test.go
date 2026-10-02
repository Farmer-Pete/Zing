package orchestrator

import "context"

// CommonMuHeldForTest reports whether this orchestrator's own shared git
// lock (commonlock.go, design section 8) is held by someone else right
// now. It is a non-blocking probe: TryLock succeeds (so this returns
// false) exactly when nothing holds the mutex, including when this very
// goroutine is the one calling from inside a Runner's Run or Output that
// runCommon invoked with the lock already held -- sync.Mutex has no
// goroutine affinity, so a held lock fails TryLock regardless of which
// goroutine asks. A momentarily acquired lock is released at once, so this
// probe never itself changes whether the mutex is held.
//
// Exported to tests (export_test.go, built only for `go test`) so
// TestOrchestratorSerializesCommonGitWrites' own recording Runner can
// observe, from outside commonlock.go, whether each call it intercepts ran
// with the lock held.
func (o *Orchestrator) CommonMuHeldForTest(ctx context.Context) bool {
	mu, err := o.resolveCommonMu(ctx)
	if err != nil {
		return false
	}
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

// SharedGitSubcommandsForTest returns the argv prefix of every git call
// this package's own inventory (commonlock.go) classifies shared: the
// single list TestOrchestratorSerializesCommonGitWrites checks every
// recorded call against, so an inventory entry added without a runCommon
// call at its site fails that test (design section 8).
func SharedGitSubcommandsForTest() [][]string {
	out := make([][]string, len(sharedGitSubcommandPrefixes))
	copy(out, sharedGitSubcommandPrefixes)
	return out
}
