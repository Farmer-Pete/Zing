package job

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/proc"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/store"
)

// commandResult is one CHECK command run (#55).
type commandResult struct {
	Kind     string        // "test" or "lint"
	Cmd      string        // the shell command, as configured
	Exit     int           // -1 when TimedOut
	TimedOut bool          // the shared check budget ran out
	Budget   time.Duration // the shared check budget, for the timeout line
	Output   string        // tailBuffer.Tail()
	Total    int64         // tailBuffer.Total()
	// NotRun is a command the shared budget left no time for: a failure,
	// since CHECK cannot pass a command it never ran.
	NotRun bool
}

func (r commandResult) failed() bool { return r.NotRun || r.TimedOut || r.Exit != 0 }

// The CHECK loop's marker heads and its cap escalation's text (#55 plan
// section 3).
const (
	markerCheckFailedPendingFmt   = "check failed pending run %d"
	markerCheckFailedDeliveredFmt = "check failed delivered run %d"
	checkLoopsExhaustedWhat       = "the project's test or lint command still fails after the builder's fix attempts"
	checkLoopsExhaustedWhyFmt     = "check_loops for build is %d; the last failing output is under Tried"
)

// The two CHECK command kinds, in run order.
const (
	checkKindTest = "test"
	checkKindLint = "lint"
)

// checkNow is the clock runCheckCommands measures its budget with; a test
// replaces it.
var checkNow = time.Now

// checkBudget is jobs.build.timeout_minutes as a Duration: the one budget
// test and lint share (#55 plan D1), which keeps both inside the building
// claim's lease of timeout_minutes plus claimGrace.
func checkBudget(d Deps) time.Duration {
	return time.Duration(d.Machine.Jobs[jobBuildName].TimeoutMinutes) * time.Minute
}

// runCheckCommands runs the project's test command, then its lint command,
// under one shared checkBudget measured from the start of test (plan D1).
// Lint gets only what test left. When test times out, lint does not run
// and is left out; when test passes but leaves no budget, lint is reported
// as not run, a failure, so CHECK never lands an unlinted tree. Each command's output is kept in a
// tailBuffer. Each command's process group is recorded in check_procs
// while it runs and cleared once it ends (plan D10), so a later serve
// never starts CHECK in this worktree while an orphaned command still
// writes to it. rid is the unit's newest ok run, or nil during adoption.
// An error is runCheckCommand's unclassified infrastructure error, for
// commandInfraEscalation.
func runCheckCommands(ctx context.Context, d Deps, t store.Ticket, wt orchestrator.Worktree, proj Project, rid *int64) ([]commandResult, error) {
	budget := checkBudget(d)
	budgetStart := checkNow()
	commands := []struct{ kind, cmd string }{{checkKindTest, proj.TestCmd}, {checkKindLint, proj.LintCmd}}
	results := make([]commandResult, 0, len(commands))
	for _, c := range commands {
		remaining := budget - checkNow().Sub(budgetStart)
		if remaining <= 0 {
			results = append(results, commandResult{Kind: c.kind, Cmd: c.cmd, Exit: -1, NotRun: true, Budget: budget})
			break
		}
		var gen int64
		onStart := func(pgid int) {
			gen = recordCheckStart(ctx, d, t.ID, c.kind, pgid, budgetStart)
		}
		r, err := runCheckCommand(ctx, d, t, wt, proj, rid, c.kind, c.cmd, remaining, onStart)
		if gen > 0 {
			clearCheckStart(ctx, d, t.ID, gen)
		}
		if err != nil {
			return nil, err
		}
		r.Budget = budget
		results = append(results, r)
		if r.TimedOut {
			break
		}
	}
	return results, nil
}

// recordCheckStart is a CHECK command's OnStart body, mirroring runjob.go's
// recordRunStart: it reads the group leader's start token ("" with a WARN
// when it cannot) and records the group under the claim (plan D10). A
// failed write is logged at WARN and the command keeps running: the same
// accepted risk #45 takes for runs, since no row means reclaim treats the
// command as gone. It returns the record's generation, 0 when nothing was
// recorded.
func recordCheckStart(ctx context.Context, d Deps, ticketID int64, kind string, pgid int, budgetStart time.Time) int64 {
	token, err := proc.StartToken(pgid)
	if err != nil {
		slog.Warn("start token unavailable", "ticket_id", ticketID, "command", kind, "pgid", pgid, "error", err)
		token = ""
	}
	startCtx, cancel := onStartContext(ctx)
	defer cancel()
	gen, err := d.Store.RecordCheckStart(startCtx, ticketID, d.Owner, d.Expires, kind, pgid, token, time.Now(), budgetStart)
	if err != nil {
		slog.Warn("record check start failed", "ticket_id", ticketID, "command", kind, "pgid", pgid, "error", err)
		return 0
	}
	slog.Info("check command started", "ticket_id", ticketID, "command", kind, "pgid", pgid, "gen", gen)
	return gen
}

// clearCheckStart deletes the CHECK row of generation gen once its command
// ended, under a context detached from a canceled tick. A failure is
// logged at WARN: the stale row names a dead group, which the next record
// replaces or reclaim clears.
func clearCheckStart(ctx context.Context, d Deps, ticketID, gen int64) {
	clearCtx, cancel := onStartContext(ctx)
	defer cancel()
	if err := d.Store.ClearCheckStart(clearCtx, ticketID, gen); err != nil {
		slog.Warn("clear check start failed", "ticket_id", ticketID, "gen", gen, "error", err)
	}
}

// checkInputText renders the failed results as the check input (plan
// section 3): one section per failed command, in run order, separated by
// one blank line. It is "" when nothing failed.
func checkInputText(results []commandResult) string {
	var sections []string
	for _, r := range results {
		if !r.failed() {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s command: %s\n", r.Kind, r.Cmd)
		if r.NotRun {
			fmt.Fprintf(&b, "%s did not run: the CHECK budget ran out after the test command", r.Kind)
			sections = append(sections, b.String())
			continue
		}
		if r.TimedOut {
			fmt.Fprintf(&b, "exit code: none (killed when the %dm check budget ran out)\n", int(r.Budget.Minutes()))
		} else {
			fmt.Fprintf(&b, "exit code: %d\n", r.Exit)
		}
		out := strings.TrimRight(r.Output, "\n")
		switch {
		case out == "":
			b.WriteString("output: none")
		case r.Total > checkOutputCap:
			fmt.Fprintf(&b, "output (cut to the last %d of %d bytes):\n%s", checkOutputCap, r.Total, out)
		default:
			b.WriteString("output:\n" + out)
		}
		sections = append(sections, strings.TrimRight(b.String(), "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// checkedExit is the observed exit of kind among results, for the "claim
// check" log: -1 when it timed out or did not run.
func checkedExit(results []commandResult, kind string) int {
	for _, r := range results {
		if r.Kind == kind && !r.TimedOut && !r.NotRun {
			return r.Exit
		}
	}
	return -1
}

// failedKinds names the commands that failed, in run order.
func failedKinds(results []commandResult) []string {
	var kinds []string
	for _, r := range results {
		if r.failed() {
			kinds = append(kinds, r.Kind)
		}
	}
	return kinds
}

// pendingMarkerBody reads run rid's pendingFmt marker and returns the text
// after its first line, plus the delivered message that marks it answered.
// ok is false when the marker is absent, or its deliveredFmt marker
// already exists (the resume that answered it already ran; never re-send).
func pendingMarkerBody(ctx context.Context, t store.Ticket, d Deps, rid int64, pendingFmt, deliveredFmt string) (body string, delivered store.Message, ok bool, err error) {
	row, pending, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(pendingFmt, rid))
	if err != nil {
		return "", store.Message{}, false, fmt.Errorf("job: building: pending marker for run %d: %w", rid, err)
	}
	if !pending {
		return "", store.Message{}, false, nil
	}
	_, done, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(deliveredFmt, rid))
	if err != nil {
		return "", store.Message{}, false, fmt.Errorf("job: building: delivered marker for run %d: %w", rid, err)
	}
	if done {
		return "", store.Message{}, false, nil
	}
	_, body, _ = strings.Cut(row.Body, "\n")
	delivered = store.Message{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: fmt.Sprintf(deliveredFmt, rid)}
	return body, delivered, true, nil
}

// checkResume is what CHECK left undelivered for one ok run.
type checkResume struct {
	Inputs    []prompt.NamedInput // claims first, then check
	Delivered []store.Message     // one delivered marker per input, same order
	Claims    bool                // claim errors pending, undelivered
	Check     bool                // check failed pending, undelivered
	CheckText string              // the check input text (the cap escalation's Tried)
}

// pendingCheckResume reads run rid's two pending markers. advanceCheckedRun
// and advanceUnit's interrupted re-send (F009) both use it.
func pendingCheckResume(ctx context.Context, t store.Ticket, d Deps, rid int64) (checkResume, error) {
	var cr checkResume
	claimsText, claimsMsg, claims, err := pendingMarkerBody(ctx, t, d, rid, markerClaimErrorsPendingFmt, markerClaimErrorsDeliveredFmt)
	if err != nil {
		return checkResume{}, err
	}
	if claims {
		cr.Claims = true
		cr.Inputs = append(cr.Inputs, prompt.NamedInput{Label: "claims", Text: claimsText, Untrusted: true})
		cr.Delivered = append(cr.Delivered, claimsMsg)
	}
	checkText, checkMsg, check, err := pendingMarkerBody(ctx, t, d, rid, markerCheckFailedPendingFmt, markerCheckFailedDeliveredFmt)
	if err != nil {
		return checkResume{}, err
	}
	if check {
		cr.Check = true
		cr.CheckText = checkText
		cr.Inputs = append(cr.Inputs, prompt.Check(checkText))
		cr.Delivered = append(cr.Delivered, checkMsg)
	}
	return cr, nil
}

// checkLoopGate caps the CHECK loop (plan D4, D5): n is the number of
// "check failed delivered" markers naming a run of sess. Below
// jobs.build.check_loops it allows the resume; at the cap it returns the
// loops_exhausted escalation with checkText, the last output, under Tried.
// A fresh session starts the count at 0 again.
func (h buildingHandler) checkLoopGate(ctx context.Context, t store.Ticket, d Deps, u unit, sess store.Session, rid int64, checkText string) (store.HandlerCommit, bool, error) {
	runIDs, err := d.Store.SessionRunIDs(ctx, sess.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: building: check loop gate: %w", err)
	}
	prefix := strings.TrimSuffix(markerCheckFailedDeliveredFmt, "%d")
	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, prefix)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: building: check loop gate: %w", err)
	}
	n := 0
	for i := range rows {
		head, _, _ := strings.Cut(rows[i].Body, "\n")
		id, perr := strconv.ParseInt(strings.TrimPrefix(head, prefix), 10, 64)
		if perr == nil && slices.Contains(runIDs, id) {
			n++
		}
	}
	limit := d.Machine.Jobs[jobBuildName].CheckLoops
	if n < limit {
		return store.HandlerCommit{}, true, nil
	}
	code := string(response.EscalationCodeLoopsExhausted)
	origin := originFor(u)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sess.ID, "run_id", rid, "code", code, "origin", string(origin), "check_loops", n)
	why := fmt.Sprintf(checkLoopsExhaustedWhyFmt, limit)
	return escalationCommit(t, d, &rid, &sess.ID, code, checkLoopsExhaustedWhat, why, checkText, origin), false, nil
}
