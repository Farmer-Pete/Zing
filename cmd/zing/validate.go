package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"zing/internal/response"
)

const usage = "usage: zing validate [--kind bug|feature] <file>"

// runValidate parses one <zing> document, validates it, and returns the
// process exit code: 0 when it is valid (silently), 1 when it is invalid
// (one error per line on stderr) or fails to parse, 2 for a usage or read
// error.
func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Usage below prints our own single line to stderr
	kind := fs.String("kind", string(response.KindFeature), "bug or feature")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rest := fs.Args()
	if len(rest) != 1 || (*kind != string(response.KindBug) && *kind != string(response.KindFeature)) {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	path := rest[0]

	data, err := os.ReadFile(path)
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
