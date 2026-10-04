package main

import (
	"testing"
	"time"
)

// waitFor receives the next value from ch, bounded only by go test's
// -timeout: if t.Deadline() reports none (-timeout 0), it waits forever.
// Otherwise it fails with a message naming what it was waiting for once
// nine tenths of the time remaining before the deadline has passed, well
// before go test's own timeout panic.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	deadline, ok := t.Deadline()
	if !ok {
		return <-ch
	}

	timer := time.NewTimer(time.Until(deadline) * 9 / 10)
	defer timer.Stop()

	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("%s: still waiting near go test's -timeout", what)
		var zero T
		return zero
	}
}
