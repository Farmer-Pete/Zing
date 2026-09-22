package main

import "testing"

func TestRunSelftest_ReturnsZeroOnEmptyMachine(t *testing.T) {
	if got := runSelftest(); got != 0 {
		t.Errorf("runSelftest() = %d, want 0", got)
	}
}
