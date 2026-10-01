// judging.go holds the judging state machine (design section 7): the
// runJob hook that hands the judge its sealed scenarios (section 7.3, D19,
// the judge never opens the database, N6) and, as of M2 task 7
// ("Task 7a" of the handoff split: the handler is real and fully tested
// here, but Registry() still points "judging" at skeleton.go's fake
// pass-through, and cmd/zing/selftest.go is untouched, until task 8 wires
// CHECK and EVALUATE and can safely flip both switches at once -- see the
// handoff notes for the exact reasoning), START and RUN: the four
// pre-flight checks, the first turn, and every resume (an answered
// question, a coverage failure, an invalid output, or an interrupted run).
// CHECK and EVALUATE -- the two branches under a "judge round <n> verdicts
// run <rid>" marker -- are task 8's own work; this file's own decision tree
// returns ErrNoAction for that marker shape until task 8 lands.
//
// judgeHandler is a distinct type from skeleton.go's own judgingHandler
// (same package, so the two names cannot collide) for exactly that reason:
// task 8 deletes the skeleton's judgingHandler and points job.go's
// Registry() at this type instead, in the same commit that adds CHECK and
// EVALUATE, so judging can actually exit the state the moment it starts
// being driven by the dispatcher and selftest for real.
package job

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// judgeScenariosDirPerm and judgeScenariosFilePerm are the per-run
// scenarios folder's and file's own modes (PKG9-PLAN.md section 7.3, D19):
// private to the owner, since this is the one copy of the sealed scenario
// text a judge run's own sandbox is allowed to read.
const (
	judgeScenariosDirPerm  = 0o700
	judgeScenariosFilePerm = 0o600
)

// writeScenariosFile returns runJobWith's own afterReserve hook for a judge
// run (PKG9-PLAN.md section 7.3, D19): right after Reserve fixes the run
// id, it writes the ticket's current, sealed scenario cohort to
// <DATA_DIR>/judge/<run id>/scenarios.xml, one <scenario> element per line
// in artifacts.id (insertion) order -- the exact bytes zing scenarios now
// copies straight to stdout (cmd/zing/scenarios.go) -- appends
// ZING_SCENARIOS_FILE to req.Env, and returns the file's path (the judge
// profile's own SCENARIOS_FILE parameter) and a cleanup that removes the
// whole run directory. RUN's own step 1 (M2 task 7) already checks the
// ticket has a sealed cohort before ever calling runJobWith, so a missing
// or empty one here is a surprise this hook still refuses rather than
// writing an empty or stale file.
func writeScenariosFile(d Deps, t store.Ticket) afterReserve {
	return func(ctx context.Context, rsv store.Reserved, req *runtime.RunRequest) (string, func() error, error) {
		cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
		if err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: current cohort for ticket %d: %w", t.ID, err)
		}
		if !ok || cohort.RunID == nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: ticket %d has no sealed scenarios", t.ID)
		}

		rows, err := d.Store.ScenariosForRun(ctx, t.ID, *cohort.RunID, true)
		if err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: scenarios for ticket %d: %w", t.ID, err)
		}
		if len(rows) == 0 {
			return "", noopCleanupErr, fmt.Errorf("job: judge: ticket %d has no sealed scenarios", t.ID)
		}

		data, err := renderScenariosXML(rows)
		if err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: render scenarios for ticket %d: %w", t.ID, err)
		}

		dir := filepath.Join(d.DataDir, "judge", strconv.FormatInt(rsv.RunID, 10))
		if err := os.MkdirAll(dir, judgeScenariosDirPerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: create scenarios dir %s: %w", dir, err)
		}
		if err := os.Chmod(dir, judgeScenariosDirPerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: chmod scenarios dir %s: %w", dir, err)
		}

		path := filepath.Join(dir, "scenarios.xml")
		if err := os.WriteFile(path, data, judgeScenariosFilePerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: write scenarios file %s: %w", path, err)
		}
		if err := os.Chmod(path, judgeScenariosFilePerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: chmod scenarios file %s: %w", path, err)
		}

		req.Env = append(req.Env, "ZING_SCENARIOS_FILE="+path)
		cleanup := func() error { return os.RemoveAll(dir) }
		return path, cleanup, nil
	}
}

// renderScenariosXML renders rows (ScenariosForRun's own insertion order)
// as one <scenario> element per line: the same encoding cmd/zing/scenarios.go
// used to print straight from the database, now written to the judge's own
// scenarios file instead and copied from there byte for byte.
func renderScenariosXML(rows []store.Artifact) ([]byte, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	for i, row := range rows {
		var sc response.Scenario
		if err := json.Unmarshal(row.Payload, &sc); err != nil {
			return nil, fmt.Errorf("unmarshal scenario artifact %d: %w", i, err)
		}
		if err := enc.EncodeElement(sc, xml.StartElement{Name: xml.Name{Local: "scenario"}}); err != nil {
			return nil, fmt.Errorf("encode scenario artifact %d: %w", i, err)
		}
		if err := enc.Flush(); err != nil {
			return nil, fmt.Errorf("flush scenario artifact %d: %w", i, err)
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ---- the judging state machine: START and RUN (design section 7.1, 7.2) --

// jobJudgeName is the job.go/machine.toml key the judging state machine's
// own sessions and job config are keyed under, "judge" (design section 4.3,
// 7.2), the same naming pattern reviewing.go's jobReviewName and
// building.go's jobBuildName already use.
const jobJudgeName = string(response.JobJudge)

// artifactTypeVerdict is the artifacts.type value every stored judge
// verdict carries (design section 4.2; migrations/0001_init.sql's own
// artifacts.type CHECK is the source of truth, matched by
// internal/store/postbuild_reads.go's own private constant of the same
// name and value, unreachable from this package).
const artifactTypeVerdict = "verdict"

// judgeRoundMarkerPrefix is every "judge round " marker's shared prefix
// (design section 5.1, 7.1): started, retry, verdicts, passed, and failed
// markers all share it ("judge check ..." does not, design section 7.1),
// so Store.MarkersWithPrefix returns every one of them, oldest first, and
// the newest is jm, the decision tree's own step (2) entry point.
const judgeRoundMarkerPrefix = "judge round "

// The four "judge round " marker first-line shapes this file reads (design
// section 5.1, 7.1); "judge round <n> passed" is EVALUATE's own terminal
// marker (task 8) and is never read here, since a passed round has already
// moved the ticket out of "judging" by the time any later tick could see it.
var (
	judgeRoundStartedLine  = regexp.MustCompile(`^judge round ([1-9]\d*) started sha ([0-9a-f]{40}) after run (\d+)$`)
	judgeRoundRetryLine    = regexp.MustCompile(`^judge round ([1-9]\d*) retry after run (\d+)$`)
	judgeRoundFailedLine   = regexp.MustCompile(`^judge round ([1-9]\d*) failed$`)
	judgeRoundVerdictsLine = regexp.MustCompile(`^judge round ([1-9]\d*) verdicts run (\d+)$`)
)

// judgeCoverageFailedFmt and judgeCoverageDeliveredFmt are the coverage
// marker heads RUN's own ok outcome and its coverage resume write (design
// section 5.1, 7.2): Marker's own exact-first-line match, keyed by run id.
const (
	judgeCoverageFailedFmt    = "judge coverage failed run %d"
	judgeCoverageDeliveredFmt = "judge coverage delivered run %d"
)

// The section 7.1, 7.2 escalation What/Why texts this file writes that
// building.go and reviewing.go have no equivalent constant for (a matching
// text -- noStoredPlanWhat/Why, worktreeNotPreparedWhat/Why from
// building.go; branchUnrecordedWhat/Why, treeDirtyBeforeReviewWhat/Why
// from reviewing.go -- is reused directly: the message is the same
// regardless of which state's own step hit it).
const (
	judgeCheckoutNotPreparedWhat = "the judge checkout could not be prepared"
	judgeCheckoutNotPreparedWhy  = "the orchestrator could not prepare the judge's detached checkout at the frozen sha"

	judgeNoSealedScenariosWhat = "the ticket has no sealed scenarios"
	judgeNoSealedScenariosWhy  = "judging needs the ticket's current scenario cohort sealed before it can run"

	judgeCoverageTwiceWhat = "the judge's verdicts are incomplete twice in a row"
)

// judgeHandler runs the real judging state (design section 7): START (7.2),
// RUN's first turn and every resume (7.2), and the decision tree (7.1) that
// routes a tick to one of them. See this file's own package doc comment for
// why it is not yet job.go's Registry() entry for "judging".
type judgeHandler struct{}

// judgeEscalation is escalationCommit (planning.go) plus this file's own
// "escalation written" log (design section 11), for every environment
// escalation START and RUN write before or outside any run: origin is
// always "judge".
func judgeEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeEnvironment)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginJudge))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginJudge)
}

// judgeCapResumesEscalation is capResumesEscalation (planning.go) plus this
// file's own "escalation written" log (design section 11), the same
// buildCapResumesEscalation/reviewCapResumesEscalation pattern building.go
// and reviewing.go each give their own state.
func judgeCapResumesEscalation(t store.Ticket, d Deps, sessionID int64) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", nil,
		"code", string(response.EscalationCodeResumesExhausted), "origin", string(response.EscalationOriginCapResumes))
	return capResumesEscalation(t, d, sessionID)
}

// Run is the judging state's own decision tree (design section 7.1).
func (h judgeHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c, handled, err := postBuildPrelude(ctx, t, d, response.EscalationOriginJudge)
	if handled || err != nil {
		return c, err
	}

	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: judge round markers: %w", err)
	}

	// Decision tree step (1): an answered round of job "judge" is always a
	// plain agent question (kind "question") resumed with the owner's
	// answers -- the judge's own 6.4-style review question does not exist
	// (design section 7), so every other question kind here is a bug.
	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		round := rounds[0]
		if round.Job != jobJudgeName {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: unexpected answered round job %q", round.Job)
		}
		kind, kindErr := newestQuestionKind(round)
		if kindErr != nil {
			return store.HandlerCommit{}, kindErr
		}
		if kind != response.QuestionKindQuestion {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: unexpected question kind %q", kind)
		}
		return h.resumeAnswered(ctx, t, d, round, markers)
	}

	// Decision tree step (2): jm, the newest "judge round " marker.
	if len(markers) == 0 {
		return h.start(ctx, t, d, 1)
	}
	newest := markers[len(markers)-1]
	firstLine, _, _ := strings.Cut(newest.Body, "\n")

	switch {
	case judgeRoundFailedLine.MatchString(firstLine):
		sub := judgeRoundFailedLine.FindStringSubmatch(firstLine)
		m, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: parse failed round %q: %w", firstLine, convErr)
		}
		return h.start(ctx, t, d, m+1)

	case judgeRoundStartedLine.MatchString(firstLine):
		sub := judgeRoundStartedLine.FindStringSubmatch(firstLine)
		n, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: parse started round %q: %w", firstLine, convErr)
		}
		sha := sub[2]
		afterRunID, parseErr := strconv.ParseInt(sub[3], 10, 64)
		if parseErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: parse started round %q: %w", firstLine, parseErr)
		}
		return h.enterAfterStart(ctx, t, d, n, sha, afterRunID)

	case judgeRoundRetryLine.MatchString(firstLine):
		sub := judgeRoundRetryLine.FindStringSubmatch(firstLine)
		n, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: parse retry round %q: %w", firstLine, convErr)
		}
		afterRunID, parseErr := strconv.ParseInt(sub[2], 10, 64)
		if parseErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: parse retry round %q: %w", firstLine, parseErr)
		}
		sha, shaErr := judgeStartedSHA(markers, n)
		if shaErr != nil {
			return store.HandlerCommit{}, shaErr
		}
		return h.enterAfterStart(ctx, t, d, n, sha, afterRunID)

	case judgeRoundVerdictsLine.MatchString(firstLine):
		// CHECK (a scenario with a check and no exit marker) and EVALUATE
		// (none left) are task 8's own work (design section 7.1, 7.5, 7.6).
		return store.HandlerCommit{}, ErrNoAction

	default:
		return store.HandlerCommit{}, fmt.Errorf("job: judging: unrecognized judge round marker %q", firstLine)
	}
}

// judgeStartedSHA scans markers (Run's own MarkersWithPrefix read, oldest
// first) for round n's own "judge round <n> started sha <sha> after run
// <R>" marker and returns its sha: a retry marker names no sha of its own
// (design section 7.1: "sha from round n's started marker").
func judgeStartedSHA(markers []store.MessageRow, n int) (string, error) {
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := judgeRoundStartedLine.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		roundN, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return "", fmt.Errorf("job: judging: parse started round %q: %w", firstLine, convErr)
		}
		if roundN == n {
			return sub[2], nil
		}
	}
	return "", fmt.Errorf("job: judging: round %d has no started marker", n)
}

// judgeRoundOwning finds the judge round (n, sha) whose own session is
// sessionID (design section 5.1's watermark rule, D18): it walks markers
// newest first, and for each "started" or "retry" line, asks
// Store.SessionAfter at that line's own watermark -- the same identity
// check postBuildRoundOwnedByOpenFix (postbuild.go) makes for a fix's own
// round -- returning as soon as one names sessionID. An answered judge
// question's own round is virtually always the newest (judging holds at
// most one open round at a time), so this only walks past it when a stale
// marker from an earlier, already-finished round still sits in the slice.
func judgeRoundOwning(ctx context.Context, t store.Ticket, d Deps, markers []store.MessageRow, sessionID int64) (n int, sha string, err error) {
	maxResumes := d.Machine.Jobs[jobJudgeName].MaxResumes
	for i := range slices.Backward(markers) {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		var roundN int
		var roundSHA string
		var afterRunID int64

		switch {
		case judgeRoundStartedLine.MatchString(firstLine):
			sub := judgeRoundStartedLine.FindStringSubmatch(firstLine)
			roundN, err = strconv.Atoi(sub[1])
			if err != nil {
				return 0, "", fmt.Errorf("job: judging: parse started round %q: %w", firstLine, err)
			}
			roundSHA = sub[2]
			afterRunID, err = strconv.ParseInt(sub[3], 10, 64)
			if err != nil {
				return 0, "", fmt.Errorf("job: judging: parse started round %q: %w", firstLine, err)
			}
		case judgeRoundRetryLine.MatchString(firstLine):
			sub := judgeRoundRetryLine.FindStringSubmatch(firstLine)
			roundN, err = strconv.Atoi(sub[1])
			if err != nil {
				return 0, "", fmt.Errorf("job: judging: parse retry round %q: %w", firstLine, err)
			}
			afterRunID, err = strconv.ParseInt(sub[2], 10, 64)
			if err != nil {
				return 0, "", fmt.Errorf("job: judging: parse retry round %q: %w", firstLine, err)
			}
			roundSHA, err = judgeStartedSHA(markers, roundN)
			if err != nil {
				return 0, "", err
			}
		default:
			continue
		}

		sess, _, _, ok, sessErr := d.Store.SessionAfter(ctx, t.ID, jobJudgeName, afterRunID, maxResumes)
		if sessErr != nil {
			return 0, "", fmt.Errorf("job: judging: session after: %w", sessErr)
		}
		if ok && sess.ID == sessionID {
			return roundN, roundSHA, nil
		}
	}
	return 0, "", fmt.Errorf("job: judging: session %d owns no judge round", sessionID)
}

// judgeScenariosFor returns the ticket's current sealed scenario cohort
// (design section 7.2 step 1), decoded: nil, nil when the ticket has no
// plan cohort, or its scenario cohort is empty or unsealed (the caller
// escalates or errors, depending on whether this is RUN's own first-turn
// check or a resume that should never see this).
func judgeScenariosFor(ctx context.Context, t store.Ticket, d Deps) ([]response.Scenario, error) {
	cohort, haveCohort, err := d.Store.CurrentCohort(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("job: judging: current cohort: %w", err)
	}
	if !haveCohort || cohort.RunID == nil {
		return nil, nil
	}
	rows, err := d.Store.ScenariosForRun(ctx, t.ID, *cohort.RunID, true)
	if err != nil {
		return nil, fmt.Errorf("job: judging: scenarios for run: %w", err)
	}
	scenarios := make([]response.Scenario, len(rows))
	for i, row := range rows {
		if err := json.Unmarshal(row.Payload, &scenarios[i]); err != nil {
			return nil, fmt.Errorf("job: judging: unmarshal scenario artifact %d: %w", i, err)
		}
	}
	return scenarios, nil
}

// ---- START (design section 7.2) -------------------------------------------

// start is START (design section 7.2): the same four checks ROUND's own
// steps 1 to 4 make (reviewing.go's round) -- a stored plan, the worktree,
// every branch commit recorded, a clean tree -- origin judge, then the
// no-runtime-call commit: marker "judge round <n> started sha <HeadSHA>
// after run <MaxRunID>".
func (h judgeHandler) start(ctx context.Context, t store.Ticket, d Deps, n int) (store.HandlerCommit, error) {
	_, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: stored plan: %w", err)
	}
	if !havePlan {
		return judgeEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return judgeEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: build reports: %w", err)
	}
	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: branch commits: %w", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		return withBranch(judgeEscalation(t, d, branchUnrecordedWhat, branchUnrecordedWhy, ""), wt), nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: changed paths: %w", err)
	}
	if len(changed) > 0 {
		quoted := make([]string, len(changed))
		for i, ch := range changed {
			quoted[i] = strconv.Quote(ch.Path)
		}
		tried := strings.Join(quoted, ", ")
		return withBranch(judgeEscalation(t, d, treeDirtyBeforeReviewWhat, treeDirtyBeforeReviewWhy, tried), wt), nil
	}

	sha, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: head sha: %w", err)
	}
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: max run id: %w", err)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge round %d started sha %s after run %d", n, sha, maxRunID),
	}}
	return c, nil
}

// ---- RUN (design section 7.2) ---------------------------------------------

// enterAfterStart is decision tree step (2)'s "started" or "retry" branch
// (design section 7.1): S := SessionAfter(t, "judge", R); none runs the
// first turn; the newest run's own outcome decides which resume, if any,
// applies.
func (h judgeHandler) enterAfterStart(ctx context.Context, t store.Ticket, d Deps, n int, sha string, afterRunID int64) (store.HandlerCommit, error) {
	maxResumes := d.Machine.Jobs[jobJudgeName].MaxResumes
	sess, state, newestRun, found, err := d.Store.SessionAfter(ctx, t.ID, jobJudgeName, afterRunID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: session after: %w", err)
	}
	if !found {
		return h.runFirst(ctx, t, d, n, sha)
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}

	switch outcome {
	case string(response.OutcomeOk):
		return h.enterCoverageResume(ctx, t, d, n, sha, sess, state, newestRun)
	case string(response.OutcomeError):
		return h.enterErrorResume(ctx, t, d, n, sha, sess, state)
	default:
		return store.HandlerCommit{}, ErrNoAction
	}
}

// enterCoverageResume is decision tree step (2)'s "newest run ok, 'judge
// coverage failed run <rid>' undelivered" branch (design section 7.1): the
// newest run's own pending coverage marker, still unresolved, is what
// makes this branch fire at all -- resumeBuildRound's own
// "claim errors pending" check (building.go) is the model this mirrors.
func (h judgeHandler) enterCoverageResume(ctx context.Context, t store.Ticket, d Deps, n int, sha string, sess store.Session, state store.SessionState, newestRun store.Run) (store.HandlerCommit, error) {
	pendingRow, pending, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(judgeCoverageFailedFmt, newestRun.ID))
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: coverage failed marker: %w", err)
	}
	if !pending {
		return store.HandlerCommit{}, ErrNoAction
	}

	capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, sess, state)
	if !mayResume {
		return capCommit, capErr
	}

	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: no sealed scenarios on a coverage resume", t.ID)
	}

	_, errsText, _ := strings.Cut(pendingRow.Body, "\n")
	priorRunID := newestRun.ID

	commit, resumeErr := h.judgeResumeTurn(ctx, t, d, n, sha, sess, 0, nil, []prompt.NamedInput{judgeCoverageInput(errsText)},
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return judgeOkCommit(t, d, n, sha, scenarios, rr, sessionCommit, nil, true)
		})
	if resumeErr == nil && len(commit.Runs) > 0 {
		commit.Messages = append(commit.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf(judgeCoverageDeliveredFmt, priorRunID),
		})
	}
	return commit, resumeErr
}

// enterErrorResume is decision tree step (2)'s two "newest run error"
// branches (design section 7.1): one consecutive invalid output resumes
// with D14's own retry text; a session ExpireClaims left mid-run with no
// invalid-output marker of its own (interrupted) resumes with the fixed
// sentence building.go's own interruptedResumeText carries; anything else
// (two or more consecutive invalid outputs, already escalated by the run
// that caused it) is ErrNoAction.
func (h judgeHandler) enterErrorResume(ctx context.Context, t store.Ticket, d Deps, n int, sha string, sess store.Session, state store.SessionState) (store.HandlerCommit, error) {
	nInvalid, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobJudgeName, &sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: consecutive invalid outputs: %w", err)
	}

	var input prompt.NamedInput
	switch nInvalid {
	case 1:
		input = prompt.Invalid(invalidRetryText(reason))
	case 0:
		input = prompt.NamedInput{Label: "interrupted", Text: interruptedResumeText, Untrusted: false}
	default:
		return store.HandlerCommit{}, ErrNoAction
	}

	capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, sess, state)
	if !mayResume {
		return capCommit, capErr
	}

	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: no sealed scenarios on a resume", t.ID)
	}

	return h.judgeResumeTurn(ctx, t, d, n, sha, sess, nInvalid, nil, []prompt.NamedInput{input},
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return judgeOkCommit(t, d, n, sha, scenarios, rr, sessionCommit, nil, false)
		})
}

// resumeAnswered is decision tree step (1) (design section 7.1): the owner
// has answered the judge's own plain agent question; this resumes the same
// session with an "answers" input, the same shape every other job's
// answered-question resume takes (design section 6.3's own resume input
// table).
func (h judgeHandler) resumeAnswered(ctx context.Context, t store.Ticket, d Deps, round store.Round, markers []store.MessageRow) (store.HandlerCommit, error) {
	if round.SessionID == nil {
		return store.HandlerCommit{}, errors.New("job: judging: answered round has no session id")
	}
	maxResumes := d.Machine.Jobs[jobJudgeName].MaxResumes
	sess, state, err := d.Store.SessionByID(ctx, *round.SessionID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: session by id: %w", err)
	}
	resolveIDs := questionIDs(round)

	n, sha, err := judgeRoundOwning(ctx, t, d, markers, *round.SessionID)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, sess, state)
	if !mayResume {
		capCommit.ResolveQuestions = resolveIDs
		return capCommit, capErr
	}

	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: no sealed scenarios on a resume", t.ID)
	}

	answers, ansErr := renderRoundAnswers(round)
	if ansErr != nil {
		return store.HandlerCommit{}, ansErr
	}

	return h.judgeResumeTurn(ctx, t, d, n, sha, sess, 0, resolveIDs, []prompt.NamedInput{prompt.Answers(answers)},
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return judgeOkCommit(t, d, n, sha, scenarios, rr, sessionCommit, resolveIDs, false)
		})
}

// resumeCapGate is the decision tree's own shared "needs a resume" gate
// (design section 6.9, mirroring building.go's resumeCapGate and
// reviewing.go's own equivalent inline check): an open session leaves the
// resume itself to the caller; an exhausted one either escalates
// resumes_exhausted once or, when that escalation already exists for this
// session, returns ErrNoAction.
func (h judgeHandler) resumeCapGate(ctx context.Context, t store.Ticket, d Deps, sess store.Session, state store.SessionState) (store.HandlerCommit, bool, error) {
	if state != store.SessionExhausted {
		return store.HandlerCommit{}, true, nil
	}
	has, err := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: judging: has escalation: %w", err)
	}
	if has {
		return store.HandlerCommit{}, false, ErrNoAction
	}
	return judgeCapResumesEscalation(t, d, sess.ID), false, nil
}

// judgeCoverageInput is the "coverage" input a coverage resume carries
// (design section 7.2): the marker's own error lines, fenced.
func judgeCoverageInput(text string) prompt.NamedInput {
	return prompt.NamedInput{Label: "coverage", Text: text, Untrusted: true}
}

// runFirst is RUN's own first turn (design section 7.2 steps 1 to 4): the
// sealed cohort check, the judge checkout, the prompt (no plan, N6), and
// the runtime call, through judgeRunAndRoute and the writeScenariosFile
// hook (section 7.3).
func (h judgeHandler) runFirst(ctx context.Context, t store.Ticket, d Deps, n int, sha string) (store.HandlerCommit, error) {
	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return judgeEscalation(t, d, judgeNoSealedScenariosWhat, judgeNoSealedScenariosWhy, ""), nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}

	jt, jtErr := proj.Orch.JudgeWorktree(ctx, t.ID, sha)
	if jtErr != nil {
		return judgeEscalation(t, d, judgeCheckoutNotPreparedWhat, judgeCheckoutNotPreparedWhy, jtErr.Error()), nil
	}
	var rr runResult
	defer func() {
		if rmErr := jt.Remove(context.WithoutCancel(ctx)); rmErr != nil {
			slog.Warn("judge worktree removal failed", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "error", rmErr)
		}
	}()

	jobCfg := d.Machine.Jobs[jobJudgeName]
	jobPromptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: %w", err)
	}
	schemas, err := renderSchemas(response.JobJudge, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: %w", err)
	}

	in := prompt.ForJudge(jobPromptText, t.Title+"\n\n"+t.Body, nil)
	in.Schemas = schemas

	req := runtime.RunRequest{
		Job: response.JobJudge, Label: strconv.Itoa(n), WorkDir: jt.Dir(),
		Prompt: prompt.Assemble(in),
		Env:    []string{"CODEX_HOME=" + d.JudgeCodexHome},
	}
	su := store.SessionUpsert{Job: jobJudgeName, Runtime: jobCfg.Runtime}

	var commit store.HandlerCommit
	var runErr error
	commit, rr, runErr = judgeRunAndRoute(ctx, d, t, su, req, 0, freshSessionRecord, nil, writeScenariosFile(d, t),
		func(res runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return judgeOkCommit(t, d, n, sha, scenarios, res, sessionCommit, nil, false)
		})
	return commit, runErr
}

// judgeResumeTurn runs one judge resume -- an answered question, a coverage
// failure, an invalid output, or an interrupted run -- each creating the
// judge worktree afresh at the round's own frozen sha (design section 7.2:
// "the session's working directory is unchanged") and resuming through
// prompt.ForJudgeResume. onOk handles the resumed run's own JudgeResponse
// outcome (judgeOkCommit); a question or error outcome, and every runtime
// failure short of that, routes exactly as judgeRunAndRoute's universal
// cases do. priorInvalid is D14's own chain length (building.go's
// advanceUnit is the model this mirrors): the caller's own
// ConsecutiveInvalidOutputs read for an invalid-output resume, 0 for every
// other resume kind, since only a second invalid output in a row escalates
// response_invalid.
func (h judgeHandler) judgeResumeTurn(
	ctx context.Context, t store.Ticket, d Deps, n int, sha string, sess store.Session,
	priorInvalid int, resolveIDs []int64, inputs []prompt.NamedInput,
	onOk func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error),
) (store.HandlerCommit, error) {
	if sess.ExternalID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: resume: session %d has no external id", sess.ID)
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}
	jt, jtErr := proj.Orch.JudgeWorktree(ctx, t.ID, sha)
	if jtErr != nil {
		return judgeEscalation(t, d, judgeCheckoutNotPreparedWhat, judgeCheckoutNotPreparedWhy, jtErr.Error()), nil
	}
	var rr runResult
	defer func() {
		if rmErr := jt.Remove(context.WithoutCancel(ctx)); rmErr != nil {
			slog.Warn("judge worktree removal failed", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "error", rmErr)
		}
	}()

	schemas, err := renderSchemas(response.JobJudge, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: %w", err)
	}
	in := prompt.ForJudgeResume(inputs)
	in.Schemas = schemas

	req := runtime.RunRequest{
		Job: response.JobJudge, Label: strconv.Itoa(n), WorkDir: jt.Dir(),
		SessionID: *sess.ExternalID, Prompt: prompt.Assemble(in),
		Env: []string{"CODEX_HOME=" + d.JudgeCodexHome},
	}
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: true}
	sessionRecord := func(r runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, r) }

	var commit store.HandlerCommit
	var runErr error
	commit, rr, runErr = judgeRunAndRoute(ctx, d, t, su, req, priorInvalid, sessionRecord, resolveIDs, writeScenariosFile(d, t), onOk)
	return commit, runErr
}

// judgeRunAndRoute runs one judge turn (runJobWith, with the scenarios-file
// hook of section 7.3) and routes its result exactly as every other job's
// universal outcome table does (design section 6.8), with judge's own
// sandbox reason in place of build's (reviewing.go's discussRunAndRoute is
// the model this mirrors: routeFailure, planning.go, hardcodes
// d.Sandboxes.Build.Reason(), which is wrong for any other job's own
// profile). It also hands the caller back the run (rr) it just made, so a
// caller that opened its own judge worktree can log that run's id if its
// own deferred removal fails (design section 7.3).
func judgeRunAndRoute(
	ctx context.Context, d Deps, t store.Ticket, su store.SessionUpsert, req runtime.RunRequest,
	priorInvalid int, sessionRecord func(runResult) *store.SessionUpsert, resolveIDs []int64,
	hook afterReserve,
	onOk func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error),
) (store.HandlerCommit, runResult, error) {
	rr, runErr := runJobWith(ctx, d, t, jobJudgeName, su, req, nil, nil, hook)
	sessionCommit := sessionRecord(rr)

	if runErr != nil {
		switch {
		case errors.Is(runErr, runtime.ErrCanceled), errors.Is(runErr, ErrConfig), errors.Is(runErr, store.ErrClaimLost):
			return store.HandlerCommit{}, rr, runErr
		case errors.Is(runErr, ErrBudget):
			return budgetEscalationCommit(t, d, resolveIDs), rr, nil
		case errors.Is(runErr, ErrSandbox):
			return sandboxEscalationCommit(t, d, resolveIDs, response.EscalationOriginJudge, d.Sandboxes.Judge.Reason()), rr, nil
		}
		var invErr *runtime.InvalidOutputError
		if errors.As(runErr, &invErr) { //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags
			return invalidOutputCommit(t, d, rr, invErr, priorInvalid, sessionCommit, resolveIDs, response.EscalationOriginJudge), rr, nil
		}
		if isExecFailure(runErr) {
			return execFailureCommit(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginJudge), rr, nil
		}
		wrapped := fmt.Errorf("job: judging: unrecognized runJob error: %w", runErr)
		if rr.Reserved.RunID != 0 {
			return postRunFailure(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginJudge, wrapped), rr, nil
		}
		return store.HandlerCommit{}, rr, wrapped
	}

	switch resp := rr.Res.Response.(type) {
	case *response.JudgeResponse:
		c, err := onOk(rr, sessionCommit)
		return c, rr, err
	case *response.QuestionResponse:
		c, err := questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
		return c, rr, err
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginJudge), rr, nil
	default:
		return store.HandlerCommit{}, rr, fmt.Errorf("job: judging: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// judgeOkCommit is RUN's own "ok" outcome (design section 7.2 step 5):
// errs := CheckCoverage(scenarios, resp.Verdicts) (judgerules.go). No
// errors terminalizes ok and stores one verdict artifact per verdict in
// cohort order, plus the round's own "judge round <n> verdicts run <rid>"
// marker. Errors on this round's first coverage attempt (secondFailure
// false: a first-turn run, or a resume that was not itself a coverage
// resume) terminalize ok and write "judge coverage failed run <rid>" with
// one error per line, so the next tick's cap gate resumes with them.
// Errors on a coverage resume's own ok outcome (secondFailure true: this
// run IS that resume) terminalize ok and escalate response_invalid
// instead, since the judge's verdicts are incomplete twice in a row.
// resolveIDs is the answered round's own question ids for resumeAnswered's
// own ok outcome (it must resolve the round it just answered, the same way
// every other outcome already does, or the next tick would read the same
// answered round again), nil for every other caller.
func judgeOkCommit(t store.Ticket, d Deps, n int, sha string, scenarios []response.Scenario, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, secondFailure bool) (store.HandlerCommit, error) {
	jr, ok := rr.Res.Response.(*response.JudgeResponse)
	if !ok {
		return store.HandlerCommit{}, errors.New("job: judging: expected a judge document")
	}
	errs := CheckCoverage(scenarios, jr.Verdicts)

	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeOk))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs

	if len(errs) == 0 {
		byScenario := make(map[string]response.Verdict, len(jr.Verdicts))
		for _, v := range jr.Verdicts {
			byScenario[v.Scenario] = v
		}
		runID := rr.Reserved.RunID
		artifacts := make([]store.Artifact, 0, len(scenarios))
		for _, sc := range scenarios {
			v, has := byScenario[sc.ID]
			if !has {
				continue // CheckCoverage already refused a missing verdict
			}
			va := response.VerdictArtifact{Verdict: v, Kind: sc.Kind, Round: n, SHA: sha}
			payload, marshalErr := json.Marshal(va)
			if marshalErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: judging: marshal verdict %s: %w", sc.ID, marshalErr)
			}
			artifacts = append(artifacts, store.Artifact{Type: artifactTypeVerdict, RunID: &runID, Payload: payload})
		}
		c.Artifacts = artifacts
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("judge round %d verdicts run %d", n, rr.Reserved.RunID),
		}}
		return c, nil
	}

	if !secondFailure {
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf(judgeCoverageFailedFmt, rr.Reserved.RunID) + "\n" + strings.Join(errs, "\n"),
		}}
		return c, nil
	}

	code := string(response.EscalationCodeResponseInvalid)
	runID, sessionID := rr.Reserved.RunID, rr.Reserved.SessionID
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", runID, "code", code, "origin", string(response.EscalationOriginJudge))
	c.Escalation = &store.EscalationCommit{
		RunID: &runID,
		Body:  code + ": " + judgeCoverageTwiceWhat,
		Payload: response.EscalationPayload{
			Code: code, What: judgeCoverageTwiceWhat, Why: strings.Join(errs, "; "),
			Options: escalationOptions, SessionID: &sessionID, Origin: string(response.EscalationOriginJudge),
		},
	}
	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c, nil
}
