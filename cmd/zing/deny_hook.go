package main

import (
	"flag"
	"fmt"
	"io"

	"zing/internal/runtime"
)

const denyHookUsage = "usage: zing deny-hook --deny COMMAND [--deny COMMAND ...]"

// runDenyHook is the Claude Code PreToolUse hook a build run's settings
// carry: exit 2 with the reason on stderr blocks the Bash call; 0 allows
// it; 1 is a usage error, which Claude Code does not treat as a block.
func runDenyHook(args []string, stdin io.Reader, stderr io.Writer) int {
	var deny []string
	fs := flag.NewFlagSet("deny-hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("deny", "a denied command prefix", func(v string) error {
		deny = append(deny, v)
		return nil
	})
	if err := fs.Parse(args); err != nil || len(deny) == 0 || len(fs.Args()) != 0 {
		fmt.Fprintln(stderr, denyHookUsage)
		return 1
	}

	reason, err := runtime.DenyHook(stdin, deny)
	if err != nil {
		fmt.Fprintf(stderr, "deny-hook: %v\n", err)
		return 0
	}
	if reason != "" {
		fmt.Fprintln(stderr, reason)
		return 2
	}
	return 0
}
