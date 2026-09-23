// Command zing starts the Zing web server.
package main

import (
	"fmt"
	"log/slog"
	"os"
)

func main() {
	os.Exit(dispatch(os.Args))
}

// commandName returns the subcommand named in os.Args-shaped args, defaulting
// to "serve" when none is given.
func commandName(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return "serve"
}

// subArgs returns the arguments after the subcommand name, os.Args[2:]
// shaped: args[2:] when args carries at least a program name and a
// subcommand, else an empty slice. commandName defaults to "serve" when
// len(args) <= 1 (a bare `zing` invocation), so dispatch must not slice
// args[2:] unconditionally -- that panics with "slice bounds out of range"
// on exactly that bare invocation, since 2 > len(args).
func subArgs(args []string) []string {
	if len(args) < 2 {
		return nil
	}
	return args[2:]
}

// dispatch runs the subcommand named in os.Args-shaped args and returns the
// process exit code.
func dispatch(args []string) int {
	switch cmd := commandName(args); cmd {
	case "selftest":
		return runSelftest()
	case "validate":
		return runValidate(subArgs(args))
	case "serve":
		if err := run(subArgs(args)); err != nil {
			slog.Error("server stopped", "err", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "zing: unknown command %q\n", cmd)
		return 2
	}
}
