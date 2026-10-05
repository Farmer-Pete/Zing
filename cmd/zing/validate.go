package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"zing/internal/response"
)

const usage = "usage: zing validate [--kind bug|feature] <file|->"

// runValidate parses one <zing> document, validates it, and returns the
// process exit code: 0 when it is valid (silently), 1 when it is invalid
// (one error per line on stderr) or fails to parse, 2 for a usage or read
// error. A file argument of "-" reads the document from standard input,
// so a job whose sandbox cannot write files can still validate. --hook
// routes to runStopHook instead: zing validate --hook is the Claude Code
// Stop hook command a run's own --settings carries (internal/runtime).
func runValidate(args []string) int {
	return runValidateFrom(args, os.Stdin)
}

// runValidateFrom is runValidate reading "-" from stdin.
func runValidateFrom(args []string, stdin io.Reader) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Usage below prints our own single line to stderr
	kind := fs.String("kind", string(response.KindFeature), "bug or feature")
	hook := fs.Bool("hook", false, "run as the Claude Code Stop hook")
	job := fs.String("job", "", "with --hook: the running job")
	state := fs.String("state", "", "with --hook: the hook's state file")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if hookInvoked(args) {
			fmt.Fprintln(os.Stderr, hookUsage)
			return 1 // a Stop hook's exit 2 would block the turn
		}
		return 2
	}
	if *hook {
		return runStopHook(*job, *state, fs.Args(), stdin, os.Stdout, os.Stderr)
	}

	rest := fs.Args()
	if len(rest) != 1 || (*kind != string(response.KindBug) && *kind != string(response.KindFeature)) {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	path := rest[0]

	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 2
	}

	doc, err := response.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 2
	}

	ctx := response.ValidateContext{Kind: response.Kind(*kind), FS: os.DirFS(cwd)}
	errs := response.Validate(doc, ctx)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, e)
	}
	if len(errs) > 0 {
		return 1
	}
	return 0
}

// hookInvoked reports whether args starts with the --hook flag, in any
// form Go's flag package accepts (-hook, --hook, or either with a
// =value), so a malformed --hook invocation still exits 1, never 2, which
// Claude Code reads as a block on the end of the turn. It checks only
// args[0]: flag.Parse stops at the first flag it cannot parse, so a flag
// named --hook anywhere after an earlier bad flag (for example
// "--bogus --hook") never reaches --hook's own parsing and must exit 2,
// the ordinary usage error, not 1.
func hookInvoked(args []string) bool {
	if len(args) == 0 {
		return false
	}
	name, _, _ := strings.Cut(args[0], "=")
	return name == "-hook" || name == "--hook"
}
