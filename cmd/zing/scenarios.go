// scenarios.go implements "zing scenarios" (PKG7-PLAN.md section 8, D7,
// task 12): the judge's read of a ticket's sealed scenario cohort.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"zing/internal/response"
	"zing/internal/store"
)

const (
	scenariosNoRunContext = "zing scenarios: no run context"
	scenariosNotJudge     = "zing scenarios: only a judge run may read scenarios"
)

// runScenarios is "zing scenarios"'s real entry point: it opens the default
// store and hands off to scenarios, the testable core, with the process's
// real environment, stdout, and stderr. The command takes no ticket id
// argument (design section 8): ZING_RUN_TOKEN is a locator naming the
// invoking run, not a capability (D7, section 7.3, owner ruling), and
// scenarios reads that run's ticket straight from the database.
func runScenarios() int {
	ctx := context.Background()

	dbPath, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zing scenarios: %v\n", err)
		return 1
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zing scenarios: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	return scenarios(ctx, st, os.Getenv, os.Stdout, os.Stderr)
}

// scenarios prints ticketID's current, sealed scenario cohort as one
// <scenario> XML element per line, in artifacts.id (insertion) order, for
// the judge run named by ZING_RUN_TOKEN (design section 8):
//  1. ZING_RUN_TOKEN missing or not a positive decimal integer -> no run
//     context, exit 2.
//  2. store.RunContext reports no such run (sql.ErrNoRows) -> no run
//     context, exit 2; any other RunContext error is operational -> exit 1.
//  3. that run's job is not "judge" -> only a judge run may read scenarios,
//     exit 2.
//  4. the ticket has no current plan cohort, or its producing run is
//     unknown (a legacy null run_id) -> print nothing, exit 0.
//  5. the cohort's sealed scenario rows, decoded and re-encoded as XML ->
//     exit 0 (no sealed rows prints nothing).
func scenarios(ctx context.Context, st *store.Store, getenv func(string) string, stdout, stderr io.Writer) int {
	runID, ok := parseRunToken(getenv("ZING_RUN_TOKEN"))
	if !ok {
		fmt.Fprintln(stderr, scenariosNoRunContext)
		return 2
	}

	job, ticketID, err := st.RunContext(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		// The token names no run: a user-input problem, exit 2.
		fmt.Fprintln(stderr, scenariosNoRunContext)
		return 2
	}
	if err != nil {
		// An outage, a closed store, or a canceled context is operational,
		// not a bad token: exit 1, not the exit-2 no-run-context path.
		fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
		return 1
	}
	if job != "judge" {
		fmt.Fprintln(stderr, scenariosNotJudge)
		return 2
	}

	cohort, ok, err := st.CurrentCohort(ctx, ticketID)
	if err != nil {
		fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
		return 1
	}
	if !ok || cohort.RunID == nil {
		return 0
	}

	rows, err := st.ScenariosForRun(ctx, ticketID, *cohort.RunID, true)
	if err != nil {
		fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
		return 1
	}

	enc := xml.NewEncoder(stdout)
	for _, row := range rows {
		var sc response.Scenario
		if err := json.Unmarshal(row.Payload, &sc); err != nil {
			fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
			return 1
		}
		if err := enc.EncodeElement(sc, xml.StartElement{Name: xml.Name{Local: "scenario"}}); err != nil {
			fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
			return 1
		}
		if err := enc.Flush(); err != nil {
			fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
			return 1
		}
		if _, err := io.WriteString(stdout, "\n"); err != nil {
			fmt.Fprintf(stderr, "zing scenarios: %v\n", err)
			return 1
		}
	}
	return 0
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
