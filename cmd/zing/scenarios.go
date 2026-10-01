// scenarios.go implements "zing scenarios" (PKG9-PLAN.md section 7.3, D19):
// the judge's read of its own run's sealed scenario cohort. It never opens
// the database (N6): the orchestrator writes the sealed cohort to one file
// under the judge run's own id right after Reserve (internal/job/judging.go's
// writeScenariosFile) and points ZING_SCENARIOS_FILE at it, so this command
// only ever needs to check the two env vars agree and copy the file.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	scenariosNoRunContext = "zing scenarios: no run context"
	scenariosNotJudge     = "zing scenarios: only a judge run may read scenarios"
)

// runScenarios is "zing scenarios"'s real entry point: the process's real
// environment, stdout, and stderr.
func runScenarios() int {
	return scenarios(os.Getenv, os.Stdout, os.Stderr)
}

// scenarios copies the judge run's own sealed scenario cohort to stdout
// byte for byte (PKG9-PLAN.md section 7.3):
//  1. ZING_RUN_TOKEN must be a positive decimal run id (parseRunToken);
//     else no run context, exit 2.
//  2. ZING_SCENARIOS_FILE must be set, absolute, and end in
//     "/judge/<token>/scenarios.xml" with <token> equal to ZING_RUN_TOKEN;
//     else only a judge run may read scenarios, exit 2. The file's own
//     existence is what "the run token names a judge run" means now (only
//     a judge run's own afterReserve hook ever writes one); this check only
//     catches a mistaken call, the seatbelt is the real control (the judge
//     profile's SCENARIOS_FILE is the only file this process can read
//     under DATA_DIR, D19).
//  3. The file is copied to stdout byte for byte; a read error is "zing
//     scenarios: <error>", exit 1.
func scenarios(getenv func(string) string, stdout, stderr io.Writer) int {
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

	f, err := os.Open(path) //nolint:gosec // G304: path is checked above against the exact run id this process's own ZING_RUN_TOKEN names; the seatbelt is the real control (comment above)
	if err != nil {
		fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
		return 1
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(stdout, f); err != nil {
		fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
		return 1
	}
	return 0
}

// scenariosFilePathOK reports whether path is an absolute path ending in
// "/judge/<runID>/scenarios.xml" (PKG9-PLAN.md section 7.3): the shape
// internal/job/judging.go's writeScenariosFile writes for runID's own
// judge run, and the only one this command ever copies.
func scenariosFilePathOK(path string, runID int64) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	want := string(filepath.Separator) + filepath.Join("judge", strconv.FormatInt(runID, 10), "scenarios.xml")
	return strings.HasSuffix(path, want)
}

// parseRunToken parses token as a positive decimal integer: no sign, no
// leading zero, no surrounding whitespace. strconv.FormatInt must reproduce
// token exactly, which is what runJob writes there in the first place
// (strconv.FormatInt(rsv.RunID, 10), internal/job/runjob.go), so any other
// spelling of the same number is rejected rather than silently accepted.
func parseRunToken(token string) (int64, bool) {
	n, err := strconv.ParseInt(token, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != token {
		return 0, false
	}
	return n, true
}
