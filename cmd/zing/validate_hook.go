package main

import (
	"fmt"
	"io"
	"slices"

	"zing/internal/response"
	"zing/internal/runtime"
)

const hookUsage = "usage: zing validate --hook --job JOB --state FILE"

// runStopHook is zing validate --hook: it exits 0 after any StopHook call
// and 1 on a usage error, never 2, which Claude Code reads as a block on
// the end of the turn.
func runStopHook(job, state string, rest []string, stdin io.Reader, stdout, stderr io.Writer) int {
	hasPositional := len(rest) != 0
	missingState := state == ""
	unknownJob := !slices.Contains(response.Job("").Values(), job)
	if hasPositional || missingState || unknownJob {
		fmt.Fprintln(stderr, hookUsage)
		return 1
	}
	out, err := runtime.StopHook(stdin, response.Job(job), state)
	if err != nil {
		fmt.Fprintf(stderr, "validate --hook: %v\n", err)
	}
	if len(out) > 0 {
		_, _ = stdout.Write(out) //nolint:errcheck // a closed stdout means claude already gave up on the hook
	}
	return 0
}
