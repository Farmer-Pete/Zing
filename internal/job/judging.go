// judging.go holds the whole judging state machine (design section 7): the
// runJob hook that hands the judge its sealed scenarios (section 7.3, D19,
// the judge never opens the database, N6), START and RUN (the four
// pre-flight checks, the first turn, and every resume: an answered
// question, a coverage failure, an invalid output, or an interrupted run),
// and, as of M2 task 8, CHECK and EVALUATE -- the two branches under a
// "judge round <n> verdicts run <rid>" marker (section 7.5, 7.6) -- plus
// the judge rows of resolvePostBuildEscalation (section 5.6, postbuild.go).
// judgeHandler is now job.go's own Registry()["judging"] entry: skeleton.go's
// own fake pass-through judgingHandler is gone.
package job

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"
	"unicode/utf8"

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

// judgeCoveragePendingInput reads run rid's own "judge coverage failed"
// marker and builds the "coverage" input and the "judge coverage
// delivered" message that marks it delivered (design section 7.2), shared
// by enterCoverageResume's own first attempt at a freshly-run coverage
// failure and enterErrorResume's interrupted-resume re-send (F009, design
// section 7.4), which calls this for an earlier run than the one that is
// currently newest. ok is false when rid's marker is not pending, or a
// "judge coverage delivered run <rid>" marker already exists for it.
func judgeCoveragePendingInput(ctx context.Context, t store.Ticket, d Deps, rid int64) (input prompt.NamedInput, deliveredMsg store.Message, ok bool, err error) {
	pendingRow, pending, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(judgeCoverageFailedFmt, rid))
	if err != nil {
		return prompt.NamedInput{}, store.Message{}, false, fmt.Errorf("job: judging: coverage failed marker: %w", err)
	}
	if !pending {
		return prompt.NamedInput{}, store.Message{}, false, nil
	}
	_, delivered, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(judgeCoverageDeliveredFmt, rid))
	if err != nil {
		return prompt.NamedInput{}, store.Message{}, false, fmt.Errorf("job: judging: coverage delivered marker: %w", err)
	}
	if delivered {
		return prompt.NamedInput{}, store.Message{}, false, nil
	}
	_, errsText, _ := strings.Cut(pendingRow.Body, "\n")
	return judgeCoverageInput(errsText), store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf(judgeCoverageDeliveredFmt, rid),
	}, true, nil
}

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
// RUN's first turn and every resume (7.2), CHECK and EVALUATE (7.5, 7.6),
// and the decision tree (7.1) that routes a tick to one of them. It is
// job.go's own Registry()["judging"] entry.
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
		sub := judgeRoundVerdictsLine.FindStringSubmatch(firstLine)
		n, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: parse verdicts round %q: %w", firstLine, convErr)
		}
		sha, shaErr := judgeStartedSHA(markers, n)
		if shaErr != nil {
			return store.HandlerCommit{}, shaErr
		}
		return h.checkOrEvaluate(ctx, t, d, n, sha)

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

// judgeStartChecks is START's own four checks, factored out of start so
// retryFreshRound (5.6, "judge with a run") can run them again ahead of a
// brand-new round's own START+RUN, without START's own marker-only commit
// shape getting in the way: a stored plan, the worktree, every branch
// commit recorded, a clean tree (reviewing.go's ROUND steps 1 to 4), origin
// judge throughout. escalation is non-nil, with every other return zeroed,
// the moment any check fails; a nil escalation with a nil error carries a
// real sha and maxRunID the caller can build on.
func judgeStartChecks(ctx context.Context, t store.Ticket, d Deps) (sha string, maxRunID int64, escalation *store.HandlerCommit, err error) {
	_, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return "", 0, nil, fmt.Errorf("job: judging: stored plan: %w", err)
	}
	if !havePlan {
		c := judgeEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, "")
		return "", 0, &c, nil
	}

	proj, wt, escCommit, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return judgeEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return "", 0, nil, err
	}
	if escCommit != nil {
		return "", 0, escCommit, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return "", 0, nil, fmt.Errorf("job: judging: build reports: %w", err)
	}
	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return "", 0, nil, fmt.Errorf("job: judging: branch commits: %w", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		c := withBranch(judgeEscalation(t, d, branchUnrecordedWhat, branchUnrecordedWhy, ""), wt)
		return "", 0, &c, nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return "", 0, nil, fmt.Errorf("job: judging: changed paths: %w", err)
	}
	if len(changed) > 0 {
		quoted := make([]string, len(changed))
		for i, ch := range changed {
			quoted[i] = strconv.Quote(ch.Path)
		}
		tried := strings.Join(quoted, ", ")
		c := withBranch(judgeEscalation(t, d, treeDirtyBeforeReviewWhat, treeDirtyBeforeReviewWhy, tried), wt)
		return "", 0, &c, nil
	}

	sha, err = proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return "", 0, nil, fmt.Errorf("job: judging: head sha: %w", err)
	}
	maxRunID, err = d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return "", 0, nil, fmt.Errorf("job: judging: max run id: %w", err)
	}
	return sha, maxRunID, nil, nil
}

// start is START (design section 7.2): judgeStartChecks' own four checks,
// then the no-runtime-call commit: marker "judge round <n> started sha
// <HeadSHA> after run <MaxRunID>".
func (h judgeHandler) start(ctx context.Context, t store.Ticket, d Deps, n int) (store.HandlerCommit, error) {
	sha, maxRunID, escalation, err := judgeStartChecks(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
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
	if !found || state == store.SessionIdless {
		// No session yet, or one whose external_id never got set (bug fix,
		// design section 7.5 #1): a first turn that was itself interrupted
		// before the runtime ever echoed a session id back has nothing to
		// resume, so it runs fresh exactly as a first turn does, mirroring
		// fix.go's own "!ok || state == store.SessionIdless" check. Before
		// that, a pending host check (#49 task 3) runs one per tick, the
		// same way CHECK runs one scenario check per tick.
		c, ran, hostErr := h.runPendingHostCheck(ctx, t, d, n, sha)
		if ran || hostErr != nil {
			return c, hostErr
		}
		return h.runFirst(ctx, t, d, n, sha, nil)
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}

	switch outcome {
	case string(response.OutcomeOk):
		return h.enterCoverageResume(ctx, t, d, n, sha, sess, state, newestRun)
	case string(response.OutcomeError):
		return h.enterErrorResume(ctx, t, d, n, sha, sess, state, newestRun)
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
	coverageInput, deliveredMsg, pending, err := judgeCoveragePendingInput(ctx, t, d, newestRun.ID)
	if err != nil {
		return store.HandlerCommit{}, err
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

	commit, resumeErr := h.judgeResumeTurn(ctx, t, d, n, sha, sess, 0, nil, []prompt.NamedInput{coverageInput}, true,
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return judgeOkCommit(t, d, n, sha, scenarios, rr, sessionCommit, nil, true)
		})
	if resumeErr == nil && len(commit.Runs) > 0 {
		commit.Messages = append(commit.Messages, deliveredMsg)
	}
	return commit, resumeErr
}

// enterErrorResume is decision tree step (2)'s two "newest run error"
// branches (design section 7.1): one consecutive invalid output resumes
// with D14's own retry text; a run that never got a response at all
// resumes with the fixed sentence interruptedResumeText (job.go) carries;
// anything else (two or more consecutive invalid outputs, already
// escalated by the run that caused it) is ErrNoAction. resumeCharge
// (job.go, design D5, section 7.4), called on newestRun, says whether this
// resume is charged and cap-gated (an ordinary reconcile, today's
// behavior) or free and uncapped (newestRun.Interrupted: a shutdown or
// dead-serve interrupt).
func (h judgeHandler) enterErrorResume(ctx context.Context, t store.Ticket, d Deps, n int, sha string, sess store.Session, state store.SessionState, newestRun store.Run) (store.HandlerCommit, error) {
	bump, gate := resumeCharge(newestRun)

	// F009 (design section 7.4): an interrupted coverage resume re-sends
	// its original coverage input plus the interrupted input, free and
	// uncapped (building.go's advanceUnit, the model this mirrors).
	if newestRun.Interrupted {
		priorRun, found, priorErr := priorNonInterruptedRun(ctx, d, t.ID, sess.ID, newestRun.ID)
		if priorErr != nil {
			return store.HandlerCommit{}, priorErr
		}
		if found && priorRun.Outcome != nil && *priorRun.Outcome == string(response.OutcomeOk) {
			coverageInput, deliveredMsg, pending, markerErr := judgeCoveragePendingInput(ctx, t, d, priorRun.ID)
			if markerErr != nil {
				return store.HandlerCommit{}, markerErr
			}
			if pending {
				scenarios, scenErr := judgeScenariosFor(ctx, t, d)
				if scenErr != nil {
					return store.HandlerCommit{}, scenErr
				}
				if len(scenarios) == 0 {
					return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: no sealed scenarios on a coverage resume", t.ID)
				}
				interruptedInput := prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false}
				commit, resumeErr := h.judgeResumeTurn(ctx, t, d, n, sha, sess, 0, nil, []prompt.NamedInput{coverageInput, interruptedInput}, bump,
					func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
						return judgeOkCommit(t, d, n, sha, scenarios, rr, sessionCommit, nil, true)
					})
				if resumeErr == nil && len(commit.Runs) > 0 {
					commit.Messages = append(commit.Messages, deliveredMsg)
				}
				return commit, resumeErr
			}
		}
	}

	nInvalid, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobJudgeName, &sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: consecutive invalid outputs: %w", err)
	}

	var input prompt.NamedInput
	switch nInvalid {
	case 1:
		input = prompt.Invalid(invalidRetryText(reason))
	case 0:
		input = prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false}
	default:
		return store.HandlerCommit{}, ErrNoAction
	}

	if gate {
		capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, sess, state)
		if !mayResume {
			return capCommit, capErr
		}
	}

	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: no sealed scenarios on a resume", t.ID)
	}

	return h.judgeResumeTurn(ctx, t, d, n, sha, sess, nInvalid, nil, []prompt.NamedInput{input}, bump,
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

	// resumeCharge (job.go, design D5, section 7.4): an interrupted latest
	// run resumes this round free and bypasses the exhausted-cap gate
	// below, even on a session already at max_resumes.
	newestRun, foundRun, newestErr := d.Store.SessionNewestRun(ctx, sess.ID)
	if newestErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: answered round: newest run: %w", newestErr)
	}
	bump, gate := true, true
	if foundRun {
		bump, gate = resumeCharge(newestRun)
	}

	if gate {
		capCommit, mayResume, capErr := h.resumeCapGate(ctx, t, d, sess, state)
		if !mayResume {
			capCommit.ResolveQuestions = resolveIDs
			return capCommit, capErr
		}
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
	inputs := []prompt.NamedInput{prompt.Answers(answers)}
	if foundRun && newestRun.Interrupted {
		inputs = append(inputs, prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false})
	}

	return h.judgeResumeTurn(ctx, t, d, n, sha, sess, 0, resolveIDs, inputs, bump,
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
// hook (section 7.3). extra is nil for a normal first turn; retryFreshRound
// (5.6, "judge with a run") passes notes and error (fenced) instead, the
// same way a fresh fix run's own first turn carries them.
func (h judgeHandler) runFirst(ctx context.Context, t store.Ticket, d Deps, n int, sha string, extra []prompt.NamedInput) (store.HandlerCommit, error) {
	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return judgeEscalation(t, d, judgeNoSealedScenariosWhat, judgeNoSealedScenariosWhy, ""), nil
	}

	hostCount := 0
	for _, sc := range scenarios {
		if sc.Kind == response.ScenarioKindHost {
			hostCount++
		}
	}
	if hostCount > 0 {
		results, resErr := judgeHostResultsAt(ctx, t, d, sha)
		if resErr != nil {
			return store.HandlerCommit{}, resErr
		}
		text, textErr := judgeHostChecksText(scenarios, results)
		if textErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: round %d at sha %s: %w", n, sha, textErr)
		}
		slog.Debug("judge host results reused", "ticket_id", t.ID, "round", n, "sha", sha, "scenario_count", hostCount)
		extra = append(slices.Clone(extra), prompt.NamedInput{Label: judgeHostChecksLabel, Text: text, Untrusted: true})
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

	ticketText, err := specFor(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: %w", err)
	}
	in := prompt.ForJudge(jobPromptText, ticketText, extra)
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
// response_invalid. bump is the caller's own resumeCharge result (design
// D5, section 7.4): false only for an interrupted latest run's own free
// resume, true for every other resume this file sends.
func (h judgeHandler) judgeResumeTurn(
	ctx context.Context, t store.Ticket, d Deps, n int, sha string, sess store.Session,
	priorInvalid int, resolveIDs []int64, inputs []prompt.NamedInput, bump bool,
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
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: bump}
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
	rr, runErr := runJobWith(ctx, d, t, jobJudgeName, su, req, nil, nil, 0, hook)
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
		c, err := questionOutcomeCommit(t, d, rr, resp.Questions, sessionCommit, resolveIDs)
		return c, rr, err
	case *response.JudgeErrorResponse:
		c, err := judgeErrorCommit(ctx, t, d, rr, resp, sessionCommit, resolveIDs)
		return c, rr, err
	case *response.ErrorResponse:
		// Unreachable once the registry maps judge errors to
		// JudgeErrorResponse; kept so a hand-built Response still routes.
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginJudge), rr, nil
	default:
		return store.HandlerCommit{}, rr, fmt.Errorf("job: judging: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// judgeAmendmentDroppedPrefix starts the Tried line a refused or unresolvable
// amendment adds (#57, Q2): "amendment dropped: " plus the refusal text,
// appended to the plain cannot_run escalation's own Tried.
const judgeAmendmentDroppedPrefix = "amendment dropped: "

// judgeErrorCommit routes the judge's own error outcome (JudgeErrorResponse,
// #57): its RunError half is terminalized exactly as errorOutcomeCommit does
// for every other job's plain ErrorResponse, which also writes the plain
// cannot_run escalation every amendment branch below starts from. A nil
// Amendment (every code but cannot_run, checkJudgeAmendment) leaves that
// escalation untouched. Otherwise judgeAmendment resolves it against the
// ticket's sealed cohort: a refusal (an unknown scenario id, or a check
// checkScenarioRules refuses) drops the amendment and adds "amendment
// dropped: REFUSAL" to the escalation's own Tried; a usable one sets
// Payload.Amendment (escalateTx reads it to offer Accept/Edit it/Abandon
// instead of Retry/Abandon) and appends judgeAmendmentDiff's rendered old and
// new text to the escalation body.
func judgeErrorCommit(ctx context.Context, t store.Ticket, d Deps, rr runResult, resp *response.JudgeErrorResponse, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	plain := &response.ErrorResponse{Head: resp.Head, Error: resp.Error.RunError}
	c := errorOutcomeCommit(t, d, rr, plain, sessionCommit, resolveIDs, response.EscalationOriginJudge)
	a := resp.Error.Amendment
	if a == nil {
		return c, nil
	}

	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	old, amended, refusal := judgeAmendment(scenarios, *a)
	if refusal != "" {
		slog.Info("judge amendment dropped", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "scenario_id", a.Scenario, "refusal", refusal)
		c.Escalation.Payload.Tried = appendTried(c.Escalation.Payload.Tried, judgeAmendmentDroppedPrefix+refusal)
		return c, nil
	}

	slog.Info("judge amendment offered", "ticket_id", t.ID, "run_id", rr.Reserved.RunID, "scenario_id", amended.Scenario, "kind", string(amended.Kind))
	c.Escalation.Payload.Amendment = &amended
	c.Escalation.Body += "\n\n" + judgeAmendmentDiff(old, amended)
	return c, nil
}

// appendTried adds line to tried on its own line (#57): an empty tried
// becomes line, so a plain cannot_run with no prior Tried text still gets a
// clean single-line "amendment dropped: ..." rather than a leading blank.
func appendTried(tried, line string) string {
	if tried == "" {
		return line
	}
	return tried + "\n" + line
}

// judgeNoSealedScenarioRefusal is judgeAmendment's own unknown-id refusal
// prefix (#57, Q2): "no sealed scenario " plus the amendment's own scenario
// id, the owner's own wording for "amendment dropped: no sealed scenario
// <id>".
const judgeNoSealedScenarioRefusal = "no sealed scenario "

// judgeAmendment resolves a judge-proposed amendment against the ticket's
// sealed scenarios (#57): it fills an empty Kind from the named scenario's
// current kind, then runs checkScenarioRules -- the same per-scenario rules
// a ready cohort's own scenarios must pass -- against the result. Pure.
// refusal is "" when a is usable, in which case amended carries a's own
// fields with Kind resolved; old is the sealed scenario a would replace,
// zero only alongside the unknown-id refusal. A scenario id this cohort
// does not have refuses with judgeNoSealedScenarioRefusal plus that id,
// before checkScenarioRules ever runs (there is no scenario to check against).
func judgeAmendment(scenarios []response.Scenario, a response.Amendment) (old response.Scenario, amended response.Amendment, refusal string) {
	i := slices.IndexFunc(scenarios, func(sc response.Scenario) bool { return sc.ID == a.Scenario })
	if i == -1 {
		return response.Scenario{}, response.Amendment{}, judgeNoSealedScenarioRefusal + a.Scenario
	}
	old = scenarios[i]
	if a.Kind == "" {
		a.Kind = old.Kind
	}
	sc := response.Scenario{ID: a.Scenario, Kind: a.Kind, Given: a.Given, When: a.When, Then: a.Then, Check: a.Check}
	errs := checkScenarioRules(i, sc)
	if len(errs) != 0 {
		msgs := make([]string, len(errs))
		for j, e := range errs {
			msgs[j] = e.Msg
		}
		return old, response.Amendment{}, strings.Join(msgs, "; ")
	}
	return old, a, ""
}

// judgeAmendmentFenceFor picks the backtick fence judgeAmendmentDiff wraps
// one field's value in: a run one longer than the longest run of backticks
// already in text, never shorter than three, so the fence itself can never
// be mistaken for part of the fenced text.
func judgeAmendmentFenceFor(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(longest+1, 3))
}

// judgeAmendmentDiff renders the markdown the escalation body appends below
// the judge's own What/Why/Tried (#57): "Reason: " plus amended.Reason, a
// blank line, then, in order, Given, When, Then, Check and Kind, each as a
// "**Field**" heading followed by the old value fenced under "Now:" and the
// amended value fenced under "Amended:" -- the console's markdown parser is
// plain CommonMark, so this is paired code blocks per field rather than a
// table (design nongoal). Pure.
//
// The reason is agent-written text shown next to the owner's one-click
// Accept, so unlike every fenced field it is never allowed a line of its
// own: a newline in it could open its own "**Check**" heading, "Now:" and
// "Amended:" lines, and fenced blocks ahead of the real ones, spoofing the
// diff an owner reads before approving a check -- including, with kind
// host, a command that then runs unsandboxed (#57, r2f2 triage). Collapsing
// every CR and LF to a space keeps the reason on its own single line, so it
// can never start a markdown block of its own.
func judgeAmendmentDiff(old response.Scenario, amended response.Amendment) string {
	fields := []struct{ name, oldVal, newVal string }{
		{"Given", old.Given, amended.Given},
		{"When", old.When, amended.When},
		{"Then", old.Then, amended.Then},
		{"Check", old.Check, amended.Check},
		{"Kind", string(old.Kind), string(amended.Kind)},
	}
	reason := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, amended.Reason)

	var b strings.Builder
	b.WriteString("Reason: " + reason + "\n")
	for _, f := range fields {
		oldFence := judgeAmendmentFenceFor(f.oldVal)
		newFence := judgeAmendmentFenceFor(f.newVal)
		b.WriteString("\n**" + f.name + "**\n\n")
		b.WriteString("Now:\n\n")
		b.WriteString(oldFence + "\n" + f.oldVal + "\n" + oldFence + "\n\n")
		b.WriteString("Amended:\n\n")
		b.WriteString(newFence + "\n" + f.newVal + "\n" + newFence + "\n")
	}
	return b.String()
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

// ---- CHECK, EVALUATE (design section 7.5, 7.6, M2 task 8) ----------------

// judgeCheckMarkerPrefix is every "judge check " marker's shared prefix
// (design section 5.1): these never carry the "judge round " prefix (7.1's
// own note), so they need their own read.
const judgeCheckMarkerPrefix = "judge check "

// judgeCheckLine matches one "judge check <n> <id> exit <code>" marker's
// first line (design section 5.1); <code> is -1 for a timeout, otherwise a
// real process exit code.
var judgeCheckLine = regexp.MustCompile(`^judge check ([1-9]\d*) (s\d+) exit (-?\d+)$`)

// judgeCheckCouldNotRunWhat is CHECK's own "any other error" escalation text
// (design section 7.5 step 3): the command runner itself failed to run the
// scenario's own check command, as distinct from the command running and
// exiting non-zero, which is simply a fail verdict, not an escalation.
const judgeCheckCouldNotRunWhat = "a scenario check could not run"

// judgeCheckedScenarios returns the scenario ids round n's own CHECK has
// already written a "judge check <n> <id> exit <code>" marker for (design
// section 5.1, 7.1): the decision tree's own "a scenario with a check and
// no exit marker" test (7.1) is membership in the complement of this set.
func judgeCheckedScenarios(markers []store.MessageRow, n int) (map[string]bool, error) {
	out := make(map[string]bool, len(markers))
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := judgeCheckLine.FindStringSubmatch(firstLine)
		if sub == nil {
			return nil, fmt.Errorf("job: judging: unrecognized judge check marker %q", firstLine)
		}
		roundN, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return nil, fmt.Errorf("job: judging: parse judge check marker %q: %w", firstLine, convErr)
		}
		if roundN == n {
			out[sub[2]] = true
		}
	}
	return out, nil
}

// judgeNewestVerdict scans rows (Store.Verdicts' own ORDER BY artifacts.id)
// for the newest row of round n belonging to scenario id (design section
// 7.4's own append-only dedup rule: "the newest row per (Round, Scenario)
// wins"). ok is false when round n carries no row for that scenario at all.
func judgeNewestVerdict(rows []store.VerdictRow, n int, scenarioID string) (row store.VerdictRow, ok bool) {
	for _, r := range rows {
		if r.Verdict.Round == n && r.Verdict.Scenario == scenarioID {
			row, ok = r, true
		}
	}
	return row, ok
}

// judgeFinalVerdicts is EVALUATE's own "final" (design section 7.6 step 1):
// the newest verdict row per scenario of round n, in the cohort's own
// order. A scenario round n never got a verdict for (CheckCoverage already
// refused that at RUN time, so this should not happen on a round that ever
// reached CHECK or EVALUATE) is silently skipped; JudgePasses only ever
// sees the rows that exist.
func judgeFinalVerdicts(rows []store.VerdictRow, scenarios []response.Scenario, n int) []response.VerdictArtifact {
	final := make([]response.VerdictArtifact, 0, len(scenarios))
	for _, sc := range scenarios {
		if row, ok := judgeNewestVerdict(rows, n, sc.ID); ok {
			final = append(final, row.Verdict)
		}
	}
	return final
}

// checkOrEvaluate is decision tree step (2)'s "judge round <n> verdicts run
// <rid>" branch (design section 7.1): the cohort's first scenario, in
// cohort order, that carries a check command and no "judge check <n> <id>
// exit" marker yet goes to CHECK; once every such scenario has one, EVALUATE
// runs.
func (h judgeHandler) checkOrEvaluate(ctx context.Context, t store.Ticket, d Deps, n int, sha string) (store.HandlerCommit, error) {
	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if len(scenarios) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: round %d has no sealed scenarios on check/evaluate", t.ID, n)
	}

	checkMarkers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeCheckMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: judge check markers: %w", err)
	}
	checked, err := judgeCheckedScenarios(checkMarkers, n)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	rows, err := d.Store.Verdicts(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: verdicts: %w", err)
	}

	for _, sc := range scenarios {
		if sc.Check == "" || checked[sc.ID] {
			continue
		}
		judged, ok := judgeNewestVerdict(rows, n, sc.ID)
		if !ok {
			return store.HandlerCommit{}, fmt.Errorf("job: judging: ticket %d: round %d has no verdict for scenario %s", t.ID, n, sc.ID)
		}
		if sc.Kind == response.ScenarioKindHost {
			return h.hostVerdict(ctx, t, d, n, sha, sc, judged)
		}
		return h.check(ctx, t, d, n, sha, sc, judged)
	}

	return h.evaluate(ctx, t, d, n, judgeFinalVerdicts(rows, scenarios, n))
}

// check is CHECK (design section 7.5): a fresh judge checkout at the
// round's own frozen sha, one command re-run through d.Commands (wired to
// Sandboxes.Build: a check command is plan-written, so it runs sandboxed,
// and it has no reason to read scenarios), then the override row
// (judgerules.go's applyCheckExit) and the "judge check <n> <id> exit
// <code>" marker.
func (h judgeHandler) check(ctx context.Context, t store.Ticket, d Deps, n int, sha string, sc response.Scenario, judged store.VerdictRow) (store.HandlerCommit, error) {
	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}

	jt, jtErr := proj.Orch.JudgeWorktree(ctx, t.ID, sha)
	if jtErr != nil {
		return judgeEscalation(t, d, judgeCheckoutNotPreparedWhat, judgeCheckoutNotPreparedWhy, jtErr.Error()), nil
	}
	defer func() {
		if rmErr := jt.Remove(context.WithoutCancel(ctx)); rmErr != nil {
			slog.Warn("judge worktree removal failed", "ticket_id", t.ID, "run_id", int64OrZero(judged.RunID), "error", rmErr)
		}
	}()

	exit, runErr := d.Commands.Run(ctx, jt.Dir(), proj.RepoGit, sc.Check, checkCommandTimeout, CommandIO{})
	switch {
	case runErr == nil:
		// exit already holds the real exit code.
	case errors.Is(runErr, ErrCommandTimeout):
		exit = -1
	case errors.Is(runErr, ErrSandbox):
		return sandboxEscalationCommit(t, d, nil, response.EscalationOriginJudge, d.Sandboxes.Build.Reason()), nil
	case errors.Is(runErr, context.Canceled):
		return store.HandlerCommit{}, runtime.ErrCanceled
	default:
		return judgeEscalation(t, d, judgeCheckCouldNotRunWhat, runErr.Error(), sc.ID), nil
	}

	row := applyCheckExit(judged.Verdict, exit)
	payload, marshalErr := json.Marshal(row)
	if marshalErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: marshal check override for scenario %s: %w", sc.ID, marshalErr)
	}

	c := baseCommit(t, d)
	c.Artifacts = []store.Artifact{{Type: artifactTypeVerdict, RunID: judged.RunID, Payload: payload}}
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge check %d %s exit %d", n, sc.ID, exit),
	}}
	return c, nil
}

// ---- host-kind scenario checks (#49 task 3): run at judging, outside any
// sandbox, on the owner's machine, once per frozen sha per exact command --

// judgeHostMarkerPrefix is every "judge host " marker's shared prefix: a
// prefix distinct from "judge round " and "judge check " (7.1's own note),
// so Store.MarkersWithPrefix never confuses one family for another.
const judgeHostMarkerPrefix = "judge host "

// judgeHostFmt is one "judge host " marker's first line:
// "judge host <round> <scenario id> exit <code> cmd <sha256>". code is -1
// on a command-runner timeout.
const judgeHostFmt = "judge host %d %s exit %d cmd %s"

// judgeHostOutputCap is how many bytes of one host check's own output Zing
// keeps (judgeCapOutput): 64 KiB, distinct from checkOutputCap's 16 KiB,
// since a host check's own evidence reaches the judge, not just the
// builder.
const judgeHostOutputCap = 64 * 1024

// judgeHostChecksLabel is the host_checks prompt input's own label
// (prompt.NamedInput.Label), carrying every host result to the judge's
// first turn of a round (runFirst).
const judgeHostChecksLabel = "host_checks"

// judgeHostCheckCouldNotRunWhat is hostCheck's own "any other error"
// escalation text: the host command runner itself failed to run the
// scenario's own check command (distinct from judgeCheckCouldNotRunWhat,
// CHECK's own sandboxed re-run failure).
const judgeHostCheckCouldNotRunWhat = "a host check could not run"

// judgeHostLine matches one "judge host " marker's first line.
var judgeHostLine = regexp.MustCompile(`^judge host ([1-9]\d*) (s\d+) exit (-?\d+) cmd ([0-9a-f]{64})$`)

// judgeHostKey identifies one host result: the scenario and the sha256 of
// the exact command that ran. The frozen sha a lookup is scoped to is the
// map's own scope (judgeHostResults' sha parameter), not part of the key.
type judgeHostKey struct {
	ScenarioID string
	CmdSHA256  string
}

// judgeHostResult is one recorded host check: its exit code (-1 on
// timeout) and its capped output.
type judgeHostResult struct {
	Exit   int
	Output string
}

// judgeHostCmdHash is the lowercase hex sha256 of cmd's exact bytes,
// untrimmed: a host scenario's own result key, and the "cmd" field of its
// marker, so an owner edit of the check gives a fresh key with no result.
func judgeHostCmdHash(cmd string) string {
	sum := sha256.Sum256([]byte(cmd))
	return hex.EncodeToString(sum[:])
}

// judgeCapOutput returns at most limit bytes of valid UTF-8 from the end
// of raw: it keeps raw's last limit bytes, skips at most 3 leading UTF-8
// continuation bytes so the result starts on a rune, replaces invalid
// bytes with U+FFFD, and if that replacement grew past limit, drops runes
// from the front until it fits. The result is always valid UTF-8 and never
// longer than limit bytes.
func judgeCapOutput(raw []byte, limit int) string {
	if len(raw) > limit {
		raw = raw[len(raw)-limit:]
	}
	startsMidRune := func(b []byte) bool { return len(b) > 0 && !utf8.RuneStart(b[0]) }
	for skipped := 0; skipped < utf8.UTFMax-1 && startsMidRune(raw); skipped++ {
		raw = raw[1:]
	}
	s := strings.ToValidUTF8(string(raw), "�")
	for len(s) > limit {
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
	}
	return s
}

// judgeHostResults maps each key to its newest host result recorded by a
// round that started at sha: hostMarkers and roundMarkers are both
// MarkersWithPrefix's own oldest-first order, and the newest hostMarkers
// entry per key wins simply by overwriting the map as this walks forward.
// An unparseable "judge host " line, or a round with no started marker
// (judgeStartedSHA), is an error.
func judgeHostResults(hostMarkers, roundMarkers []store.MessageRow, sha string) (map[judgeHostKey]judgeHostResult, error) {
	out := make(map[judgeHostKey]judgeHostResult, len(hostMarkers))
	for i := range hostMarkers {
		firstLine, output, _ := strings.Cut(hostMarkers[i].Body, "\n")
		sub := judgeHostLine.FindStringSubmatch(firstLine)
		if sub == nil {
			return nil, fmt.Errorf("job: judging: unrecognized judge host marker %q", firstLine)
		}
		roundN, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return nil, fmt.Errorf("job: judging: parse judge host marker %q: %w", firstLine, convErr)
		}
		startedSHA, shaErr := judgeStartedSHA(roundMarkers, roundN)
		if shaErr != nil {
			return nil, fmt.Errorf("job: judging: judge host marker %q: %w", firstLine, shaErr)
		}
		if startedSHA != sha {
			continue
		}
		exit, convErr := strconv.Atoi(sub[3])
		if convErr != nil {
			return nil, fmt.Errorf("job: judging: parse judge host marker %q: %w", firstLine, convErr)
		}
		out[judgeHostKey{ScenarioID: sub[2], CmdSHA256: sub[4]}] = judgeHostResult{Exit: exit, Output: output}
	}
	return out, nil
}

// judgeHostResultsAt reads both marker families (Store.MarkersWithPrefix)
// and calls judgeHostResults for sha.
func judgeHostResultsAt(ctx context.Context, t store.Ticket, d Deps, sha string) (map[judgeHostKey]judgeHostResult, error) {
	hostMarkers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeHostMarkerPrefix)
	if err != nil {
		return nil, fmt.Errorf("job: judging: judge host markers: %w", err)
	}
	roundMarkers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeRoundMarkerPrefix)
	if err != nil {
		return nil, fmt.Errorf("job: judging: judge round markers: %w", err)
	}
	return judgeHostResults(hostMarkers, roundMarkers, sha)
}

// judgeHostResultFor looks up sc's result under its current sealed check:
// an owner edit of the check changes the key a lookup hashes, so a stale
// result never matches.
func judgeHostResultFor(results map[judgeHostKey]judgeHostResult, sc response.Scenario) (judgeHostResult, bool) {
	r, ok := results[judgeHostKey{ScenarioID: sc.ID, CmdSHA256: judgeHostCmdHash(sc.Check)}]
	return r, ok
}

// judgePendingHost returns the cohort's first scenario, in cohort order,
// of kind host with a non-blank check and no entry in results for its
// current key: runPendingHostCheck's and retryFreshRound's own "is a host
// check still due" test.
func judgePendingHost(scenarios []response.Scenario, results map[judgeHostKey]judgeHostResult) (response.Scenario, bool) {
	for _, sc := range scenarios {
		if sc.Kind != response.ScenarioKindHost || strings.TrimSpace(sc.Check) == "" {
			continue
		}
		if _, ok := judgeHostResultFor(results, sc); !ok {
			return sc, true
		}
	}
	return response.Scenario{}, false
}

// judgeHostChecksText renders the host_checks input: one block per host
// scenario with a non-blank check, in cohort order (the same filter
// judgePendingHost uses, so the two never disagree about which scenarios
// need a result), joined by a blank line; each block is "scenario <id>
// exit <code>" (plus " (timed out after 10m)" when code is -1), a newline,
// then the output with trailing newlines trimmed. A host scenario with no
// result for its current key is an error naming it: runPendingHostCheck
// already ran every pending one in the same tick runFirst reads this
// from, so that should not happen.
func judgeHostChecksText(scenarios []response.Scenario, results map[judgeHostKey]judgeHostResult) (string, error) {
	var blocks []string
	for _, sc := range scenarios {
		if sc.Kind != response.ScenarioKindHost || strings.TrimSpace(sc.Check) == "" {
			continue
		}
		res, ok := judgeHostResultFor(results, sc)
		if !ok {
			return "", fmt.Errorf("job: judging: scenario %s has no host result for its current check", sc.ID)
		}
		header := fmt.Sprintf("scenario %s exit %d", sc.ID, res.Exit)
		if res.Exit == -1 {
			header += " (timed out after 10m)"
		}
		blocks = append(blocks, header+"\n"+strings.TrimRight(res.Output, "\n"))
	}
	return strings.Join(blocks, "\n\n"), nil
}

// runPendingHostCheck runs the cohort's first host scenario, in cohort
// order, with no result for its key at sha, one per tick, the same way
// CHECK runs one scenario check per tick. ran is false when none is
// pending (an empty cohort, impossible here since the caller only reaches
// this once a sealed cohort is already known to exist, leaves ran false
// too; runFirst's own no-sealed-scenarios escalation still guards it).
func (h judgeHandler) runPendingHostCheck(ctx context.Context, t store.Ticket, d Deps, n int, sha string) (store.HandlerCommit, bool, error) {
	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, false, err
	}
	results, err := judgeHostResultsAt(ctx, t, d, sha)
	if err != nil {
		return store.HandlerCommit{}, false, err
	}
	sc, pending := judgePendingHost(scenarios, results)
	if !pending {
		return store.HandlerCommit{}, false, nil
	}
	c, err := h.hostCheck(ctx, t, d, n, sha, sc)
	return c, true, err
}

// hostCheck runs one host scenario's check, unsandboxed, in a fresh judge
// checkout at the round's own frozen sha, through Deps.HostCommands, and
// commits its own "judge host " marker recording the exit code, the
// check's own sha256, and up to judgeHostOutputCap bytes of its tail
// output (design section 5, #49 task 3). No verdict artifact is written
// here: hostVerdict turns a recorded result into one, at CHECK.
func (h judgeHandler) hostCheck(ctx context.Context, t store.Ticket, d Deps, n int, sha string, sc response.Scenario) (store.HandlerCommit, error) {
	cmdHash := judgeHostCmdHash(sc.Check)
	ids := []any{"ticket_id", t.ID, "round", n, "scenario_id", sc.ID, "sha", sha, "cmd_sha256", cmdHash}
	if d.HostCommands == nil {
		slog.Error("judge host check has no runner", ids...)
		return store.HandlerCommit{}, ErrConfig
	}
	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}

	jt, jtErr := proj.Orch.JudgeWorktree(ctx, t.ID, sha)
	if jtErr != nil {
		return judgeEscalation(t, d, judgeCheckoutNotPreparedWhat, judgeCheckoutNotPreparedWhy, jtErr.Error()), nil
	}
	defer func() {
		if rmErr := jt.Remove(context.WithoutCancel(ctx)); rmErr != nil {
			slog.Warn("judge worktree removal failed", "ticket_id", t.ID, "error", rmErr)
		}
	}()

	slog.Info("judge host check started", ids...)
	start := time.Now()
	out := newTailBuffer(judgeHostOutputCap)
	exit, runErr := d.HostCommands.Run(ctx, jt.Dir(), proj.RepoGit, sc.Check, checkCommandTimeout, CommandIO{Out: out})
	done := append(slices.Clone(ids), "output_bytes", out.Total(), "duration_ms", time.Since(start).Milliseconds())

	switch {
	case runErr == nil:
		slog.Info("judge host check finished", append(done, "exit", exit)...)
	case errors.Is(runErr, ErrCommandTimeout):
		exit = -1
		slog.Warn("judge host check timed out", append(done, "exit", exit)...)
	case errors.Is(runErr, context.Canceled):
		slog.Info("judge host check canceled", ids...)
		return store.HandlerCommit{}, runtime.ErrCanceled
	default:
		slog.Warn("judge host check could not run", append(slices.Clone(ids), "error", runErr.Error())...)
		return judgeEscalation(t, d, judgeHostCheckCouldNotRunWhat, runErr.Error(), sc.ID), nil
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf(judgeHostFmt, n, sc.ID, exit, cmdHash) + "\n" + judgeCapOutput(out.TailBytes(), judgeHostOutputCap),
	}}
	return c, nil
}

// hostVerdict is CHECK for a host scenario (#49 task 3): it runs nothing.
// With a result for the scenario's current check, it turns the recorded
// exit into the override row (applyHostCheckExit) and the usual "judge
// check <n> <id> exit <code>" marker, exactly as check does for a
// sandboxed scenario. Without one, the owner edited the check after it
// ran, so judging the stale output would be wrong: this starts round n+1
// at the same sha instead, which reruns the edited check and the judge.
func (h judgeHandler) hostVerdict(ctx context.Context, t store.Ticket, d Deps, n int, sha string, sc response.Scenario, judged store.VerdictRow) (store.HandlerCommit, error) {
	results, err := judgeHostResultsAt(ctx, t, d, sha)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	cmdHash := judgeHostCmdHash(sc.Check)
	res, ok := judgeHostResultFor(results, sc)
	if !ok {
		newSHA, maxRunID, escalation, startErr := judgeStartChecks(ctx, t, d)
		if startErr != nil {
			return store.HandlerCommit{}, startErr
		}
		if escalation != nil {
			return *escalation, nil
		}
		slog.Warn("judge host check changed after it ran; starting a fresh round", "ticket_id", t.ID, "round", n,
			"scenario_id", sc.ID, "sha", sha, "cmd_sha256", cmdHash, "new_round", n+1)
		c := baseCommit(t, d)
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("judge round %d started sha %s after run %d", n+1, newSHA, maxRunID),
		}}
		return c, nil
	}

	row := applyHostCheckExit(judged.Verdict, res.Exit)
	slog.Info("judge host verdict", "ticket_id", t.ID, "round", n, "scenario_id", sc.ID, "sha", sha,
		"cmd_sha256", cmdHash, "exit", res.Exit, "result", string(row.Result))
	payload, marshalErr := json.Marshal(row)
	if marshalErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: marshal host check override for scenario %s: %w", sc.ID, marshalErr)
	}

	c := baseCommit(t, d)
	c.Artifacts = []store.Artifact{{Type: artifactTypeVerdict, RunID: judged.RunID, Payload: payload}}
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge check %d %s exit %d", n, sc.ID, res.Exit),
	}}
	return c, nil
}

// fixRequestedFailurePrefix is the "fix requested failure after run " marker
// family EVALUATE's own fail branch writes (fixRequestMessage's own format,
// fix.go): the loop gate counts these the same way reviewing.go's fixreq
// counts "fix requested findings" markers.
const fixRequestedFailurePrefix = "fix requested failure after run "

// judgeLoopsExhausted is EVALUATE's own loops_exhausted escalation (design
// section 7.6): no run caused it, so RunID and SessionID are both nil,
// mirroring reviewLoopsExhausted (reviewing.go).
func judgeLoopsExhausted(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeLoopsExhausted)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginJudge))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginJudge)
}

// retryJudgeLoopsExhausted is design section 5.6's "judge loops_exhausted"
// row: a fix request of kind failure with the remaining failures (the
// escalation's own Tried text, EVALUATE's renderFixFailures output) and the
// owner's notes appended, bypassing the loop gate for this one request --
// mirroring reviewingHandler's own retryReviewLoopsExhausted.
func (h judgeHandler) retryJudgeLoopsExhausted(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, tried string) (store.HandlerCommit, error) {
	text := tried
	if notes != "" {
		text = strings.TrimRight(text, "\n") + "\n\n" + notes
	}
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: loops_exhausted retry: max run id: %w", err)
	}
	msg, msgErr := fixRequestMessage(t, FixKindFailure, text, maxRunID)
	if msgErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: loops_exhausted retry: fix request message: %w", msgErr)
	}
	c := baseCommit(t, d)
	c.ResolveQuestions = resolveIDs
	c.Messages = []store.Message{msg}
	return c, nil
}

// evaluate is EVALUATE (design section 7.6): JudgePasses decides the round;
// a pass moves the ticket to shipping; a fail always writes "judge round <n>
// failed" with the failing scenario ids, then gates on jobs.judge.max_loops
// the same way reviewing.go's fixreq gates on jobs.review.max_loops -- under
// it, a "failure" fix request; at or over it, loops_exhausted.
func (h judgeHandler) evaluate(ctx context.Context, t store.Ticket, d Deps, n int, final []response.VerdictArtifact) (store.HandlerCommit, error) {
	pass, failures := JudgePasses(final)
	if pass {
		c := baseCommit(t, d)
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("judge round %d passed", n),
		}}
		c.Next, c.Reason = stateShipping, reasonJudgePassed
		return c, nil
	}

	ids := make([]string, len(failures))
	for i, f := range failures {
		ids[i] = f.Scenario
	}
	failedMarker := store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge round %d failed\n%s", n, strings.Join(ids, ",")),
	}

	allReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedFailurePrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: fix requested failure markers: %w", err)
	}
	k := len(allReqs)
	maxLoops := d.Machine.Jobs[jobJudgeName].MaxLoops

	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	failText := renderFixFailures(failures, scenarios)

	if k >= maxLoops {
		what := fmt.Sprintf("scenarios still fail after %d fix runs", k)
		why := fmt.Sprintf("max_loops for judge is %d", maxLoops)
		c := judgeLoopsExhausted(t, d, what, why, failText)
		c.Messages = []store.Message{failedMarker}
		return c, nil
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: max run id: %w", err)
	}
	msg, msgErr := fixRequestMessage(t, FixKindFailure, failText, maxRunID)
	if msgErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: fix request message: %w", msgErr)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{failedMarker, msg}
	return c, nil
}

// ---- the judge rows of resolvePostBuildEscalation (design section 5.6) ---

// judgeNewestRoundNumber reads the round number back from markers' own
// newest "judge round " marker, whichever of the four shapes it is (design
// section 5.1): retryFreshRound needs the round just abandoned to number
// the brand-new one it starts.
func judgeNewestRoundNumber(markers []store.MessageRow) (int, error) {
	if len(markers) == 0 {
		return 0, errors.New("job: judging: escalation retry: no judge round marker")
	}
	firstLine, _, _ := strings.Cut(markers[len(markers)-1].Body, "\n")
	for _, re := range []*regexp.Regexp{judgeRoundStartedLine, judgeRoundRetryLine, judgeRoundFailedLine, judgeRoundVerdictsLine} {
		sub := re.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		n, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			return 0, fmt.Errorf("job: judging: parse judge round marker %q: %w", firstLine, convErr)
		}
		return n, nil
	}
	return 0, fmt.Errorf("job: judging: escalation retry: unrecognized judge round marker %q", firstLine)
}

// retryCapResumesJudge is retryCapResumes' own judge-session branch (design
// section 5.6, "cap_resumes, exhausted session of job judge"): marker
// "judge round <n> retry after run <MaxRunID>", n the round the exhausted
// session belonged to (judgeRoundOwning), preserved rounds resolved. No
// fresh run starts here: the judging tree reads the marker as a fresh
// start of round n at that round's own sha (7.1) -- SessionAfter with the
// new watermark finds no session, so the next tick runs a first turn,
// which is also why, unlike retryFreshRound's own "with a run" row, no
// notes or error ever reach this one.
func (h judgeHandler) retryCapResumesJudge(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, sessionID int64, preservedRounds []store.Round) (store.HandlerCommit, int, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: judging: cap_resumes retry: judge round markers: %w", err)
	}
	n, _, err := judgeRoundOwning(ctx, t, d, markers, sessionID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: judging: cap_resumes retry: %w", err)
	}
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: judging: cap_resumes retry: max run id: %w", err)
	}

	allResolveIDs := append([]int64{}, resolveIDs...)
	for _, r := range preservedRounds {
		allResolveIDs = append(allResolveIDs, questionIDs(r)...)
	}

	c := baseCommit(t, d)
	c.ResolveQuestions = allResolveIDs
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge round %d retry after run %d", n, maxRunID),
	}}
	return c, len(preservedRounds), nil
}

// retryFreshRound is design section 5.6's "judge with a run" row: the
// session RUN's last turn reserved is dead (an agent error outcome, an exec
// failure, or a second response_invalid in a row -- every judge-origin code
// that is not cap_resumes or loops_exhausted, both resolved elsewhere in
// that table), so retrying means a brand-new round rather than another
// resume of that session. judgeStartChecks' own four checks and marker, then
// RUN's first turn with inputs notes and error (fenced), land in the one
// commit that also resolves the escalation round.
func (h judgeHandler) retryFreshRound(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, errorText string) (store.HandlerCommit, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: escalation retry: judge round markers: %w", err)
	}
	prevN, err := judgeNewestRoundNumber(markers)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	sha, maxRunID, escalation, err := judgeStartChecks(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	n := prevN + 1
	startMsg := store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("judge round %d started sha %s after run %d", n, sha, maxRunID),
	}

	// A host check still pending at the new round's own sha (#49 task 3)
	// cannot be run on this tick: runFirst needs every host result in hand
	// before it builds host_checks, and the owner's retry notes have
	// nowhere to go until the next tick, which runs the pending check and
	// re-enters at enterAfterStart. That happens only when the owner edited
	// a host check between the escalation and this retry.
	scenarios, scenErr := judgeScenariosFor(ctx, t, d)
	if scenErr != nil {
		return store.HandlerCommit{}, scenErr
	}
	results, resErr := judgeHostResultsAt(ctx, t, d, sha)
	if resErr != nil {
		return store.HandlerCommit{}, resErr
	}
	if sc, pending := judgePendingHost(scenarios, results); pending {
		slog.Warn("judge retry: host checks pending at the new round's sha; retry notes not delivered",
			"ticket_id", t.ID, "round", n, "scenario_id", sc.ID, "sha", sha, "cmd_sha256", judgeHostCmdHash(sc.Check))
		c := baseCommit(t, d)
		c.Messages = []store.Message{startMsg}
		c.ResolveQuestions = resolveIDs
		return c, nil
	}

	extra := []prompt.NamedInput{prompt.Notes(notes), prompt.Error(errorText)}
	commit, runErr := h.runFirst(ctx, t, d, n, sha, extra)
	if runErr != nil {
		return store.HandlerCommit{}, runErr
	}
	commit.Messages = append([]store.Message{startMsg}, commit.Messages...)
	commit.ResolveQuestions = resolveIDs
	return commit, nil
}

// acceptAmendment is resolvePostBuildEscalation's own "Accept the amended
// check" row (#57, Q1): it never runs the judge in the same tick (a judge
// run here would read the sealed scenarios before this commit's own edit
// lands, and so would run the old check) -- it only commits the amendment
// through HandlerCommit.ScenarioEdit and the "judge round <n> started"
// marker together, the same no-runtime-call shape START's own start method
// gives round 1. The next tick's decision tree reads that marker as a fresh
// round and runs RUN's first turn itself (enterAfterStart), the same path
// retryFreshRound already relies on for a pending host check. judgeAmendment
// runs again here, against the ticket's current sealed cohort, because the
// rules or the scenario can have changed between the escalation and this
// answer; a refusal here re-escalates plain cannot_run exactly as a refusal
// at judgeErrorCommit's own first attempt does, carrying the same runID and
// sessionID the original escalation carried, so a later Retry on it takes
// the same origin-judge-with-a-run path (retryFreshRound) that Retry on the
// escalation it replaces would have taken (#57, r3f3 review).
func (h judgeHandler) acceptAmendment(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, payload response.EscalationPayload, runID, sessionID *int64) (store.HandlerCommit, error) {
	scenarios, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	_, a, refusal := judgeAmendment(scenarios, *payload.Amendment)
	if refusal != "" {
		slog.Warn("judge amendment refused at accept", "ticket_id", t.ID, "scenario_id", payload.Amendment.Scenario, "question_ids", resolveIDs, "refusal", refusal)
		tried := appendTried(payload.Tried, judgeAmendmentDroppedPrefix+refusal)
		code := string(response.EscalationCodeCannotRun)
		// judgeEscalation's own generic "escalation written" Warn (design
		// section 11) does not fire for an escalation built directly
		// through escalationCommit the way this one is, so acceptAmendment
		// writes it itself.
		slog.Warn("escalation written", "ticket_id", t.ID, "session_id", int64OrZero(sessionID), "run_id", int64OrZero(runID), "code", code, "origin", string(response.EscalationOriginJudge))
		c := escalationCommit(t, d, runID, sessionID, code, payload.What, payload.Why, tried, response.EscalationOriginJudge)
		c.ResolveQuestions = resolveIDs
		return c, nil
	}

	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: judging: accept amendment: judge round markers: %w", err)
	}
	prevN, err := judgeNewestRoundNumber(markers)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	// h.start is RUN's own round-start commit (judging.go): one marker
	// message, or the escalation judgeStartChecks itself raised (no stored
	// plan, or the worktree not ready). Reusing it keeps the marker format
	// written in exactly one place (#57, r2f3 triage).
	c, err := h.start(ctx, t, d, prevN+1)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if c.Escalation != nil {
		// judgeStartChecks escalated instead of starting the round: the
		// owner's Accept is discarded along with it, since nothing below
		// applies the amendment. Without this line the only trace is
		// judgeEscalation's own generic "escalation written" Warn, which
		// carries no scenario_id or question_ids and does not say an
		// accepted amendment went unapplied (#57, r2f4 triage).
		slog.Warn("judge amendment not applied: start checks escalated", "ticket_id", t.ID, "scenario_id", a.Scenario, "question_ids", resolveIDs)
		return c, nil
	}

	slog.Info("judge amendment accepted", "ticket_id", t.ID, "scenario_id", a.Scenario, "kind", string(a.Kind), "round", prevN+1, "question_ids", resolveIDs)
	c.ScenarioEdit = &store.ScenarioEdit{Ref: a.Scenario, Kind: a.Kind, Given: a.Given, When: a.When, Then: a.Then, Check: a.Check, Reason: a.Reason}
	c.ResolveQuestions = resolveIDs
	return c, nil
}
