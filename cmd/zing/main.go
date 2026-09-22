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

// dispatch runs the subcommand named in os.Args-shaped args and returns the
// process exit code.
func dispatch(args []string) int {
	switch cmd := commandName(args); cmd {
	case "selftest":
		return runSelftest()
	case "validate":
		return runValidate(args[2:])
	case "serve":
		if err := run(); err != nil {
			slog.Error("server stopped", "err", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "zing: unknown command %q\n", cmd)
		return 2
	}
}
