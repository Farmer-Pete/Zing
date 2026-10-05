package main

import (
	"os"
	"testing"
)

// TestMain lets the test binary stand in for zing: with ZING_TEST_MAIN=1 it
// runs dispatch(os.Args) instead of the tests, so a fake claude can run the
// real validate --hook path (TestStopHook_FakeClaudeBlocksThenAccepts) by
// invoking this very binary as its own Stop hook command.
func TestMain(m *testing.M) {
	if os.Getenv("ZING_TEST_MAIN") == "1" {
		os.Exit(dispatch(os.Args))
	}
	os.Exit(m.Run())
}
