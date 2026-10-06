// check.go implements "zing check SID" (#86 follow-up, PKG9-PLAN.md section
// 7.3): the judge's own way to run one sealed scenario's check command,
// through /bin/sh rather than the judge agent's own shell, so the agent's
// login shell (zsh, which aborts on an unmatched glob before a negated
// grep ever runs) never decides a check's result. judge.md tells the judge
// to use this instead of pasting a check into its own shell.
package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"zing/internal/job"
	"zing/internal/response"
)

const (
	checkUsage      = "usage: zing check SID"
	checkTimeout    = 10 * time.Minute
	checkTimeoutMsg = "zing check: timed out after 10m"
)

// checkIDPattern mirrors response.Scenario's own id shape
// (jsonschema:"pattern=^s[0-9]+$").
var checkIDPattern = regexp.MustCompile(`^s\d+$`)

// runCheck is "zing check SID"'s real entry point: the process's real
// arguments, environment, working directory, and stdout/stderr.
func runCheck() int {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zing check: %v\n", err)
		return 1
	}
	return check(os.Args[2:], os.Getenv, dir, os.Environ(), os.Stdout, os.Stderr)
}

// check runs one sealed scenario's check command (PKG9-PLAN.md section 7.3):
//  1. The same two env checks "zing scenarios" makes (scenariosNoRunContext,
//     scenariosNotJudge): only a judge run's own sandbox, the one place
//     that can read its own ZING_SCENARIOS_FILE, ever reaches a real
//     check.
//  2. Exactly one argument, shaped like a scenario id (s followed by
//     digits); else checkUsage, exit 2.
//  3. The named scenario is read from the scenarios file; an unknown id
//     or a scenario with no check command each exit 2.
//  4. The check command runs through job.RunShell in dir, with env as its
//     whole environment and a 10 minute timeout, its output copied to
//     stdout as it runs. A timeout exits 124; any other run error exits
//     1; otherwise check returns the command's own exit code.
func check(args []string, getenv func(string) string, dir string, env []string, stdout, stderr io.Writer) int {
	runID, ok := parseRunToken(getenv("ZING_RUN_TOKEN"))
	if !ok {
		fmt.Fprintln(stderr, scenariosNoRunContext)
		return 2
	}
	path := getenv("ZING_SCENARIOS_FILE")
	if !scenariosFilePathOK(path, runID) {
		fmt.Fprintln(stderr, scenariosNotJudge)
		return 2
	}

	if len(args) != 1 || !checkIDPattern.MatchString(args[0]) {
		fmt.Fprintln(stderr, checkUsage)
		return 2
	}
	id := args[0]

	sc, found, err := readScenario(path, id)
	if err != nil {
		fmt.Fprintf(stderr, "zing check: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "zing check: unknown scenario %s\n", id)
		return 2
	}
	if sc.Check == "" {
		fmt.Fprintf(stderr, "zing check: scenario %s has no check command\n", id)
		return 2
	}

	exitCode, runErr := job.RunShell(context.Background(), dir, sc.Check, env, checkTimeout, job.CommandIO{Out: stdout})
	switch {
	case errors.Is(runErr, job.ErrCommandTimeout):
		fmt.Fprintln(stderr, checkTimeoutMsg)
		return 124
	case runErr != nil:
		fmt.Fprintf(stderr, "zing check: %v\n", runErr)
		return 1
	default:
		return exitCode
	}
}

// readScenario decodes each top-level <scenario> element in path (the
// shape renderScenariosXML writes: one per line, no shared root) and
// returns the one whose id matches id, with found false when none does.
func readScenario(path, id string) (sc response.Scenario, found bool, err error) {
	f, openErr := os.Open(path) //nolint:gosec // G304: path is checked by scenariosFilePathOK, above, against this process's own ZING_RUN_TOKEN before this is ever called
	if openErr != nil {
		return response.Scenario{}, false, openErr
	}
	defer func() { _ = f.Close() }()

	dec := xml.NewDecoder(f)
	for {
		tok, tokErr := dec.Token()
		if errors.Is(tokErr, io.EOF) {
			return response.Scenario{}, false, nil
		}
		if tokErr != nil {
			return response.Scenario{}, false, tokErr
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "scenario" {
			continue
		}
		var cur response.Scenario
		if decErr := dec.DecodeElement(&cur, &start); decErr != nil {
			return response.Scenario{}, false, decErr
		}
		if cur.ID == id {
			return cur, true, nil
		}
	}
}
