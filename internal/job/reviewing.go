// reviewing.go is the real "reviewing" state handler (design section 6): the
// post-build prelude (postbuild.go) runs first; ROUND (6.2) runs the
// ticket's review lenses in parallel, one commit per tick, bounded by
// Deps.LensesParallel; a lens that asks a question holds every other lens's
// own findings and the round waits (6.2a, ASKED); once the owner has
// answered every asking lens, CONTINUE resumes them (6.2a); a clean round
// with only at-or-below-floor findings requests a fix (FIXREQ, 6.8); one
// with findings above the floor posts the review question (6.4). It
// replaces the skeleton's reviewingHandler, which advanced straight to
// judging with no review job at all.
//
// TRIAGE (6.5) and DISCUSS (6.6) are task 11's own work: a round whose
// newest question is kind "review" (the 6.4 question, once answered) is not
// yet handled here and errors loudly rather than silently doing nothing.
// Re-review's own lens selection (6.7) and the loop gate escalations are
// wired (selectLenses already exists, task 8), but the two-in-a-row and
// loop-exhaustion escalations (6.8, 6.9) this task's own named tests do not
// exercise are exactly as the plan's text describes; task 12 adds its own
// live pass and the remaining escalation resumes.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// artifactTypeFinding is the artifacts.type value every stored review
// finding carries (design section 4.2; migrations/0001_init.sql's own
// artifacts.type CHECK is the source of truth, matched by
// internal/store/postbuild_reads.go's own private constant of the same
// name and value).
const artifactTypeFinding = "finding"

// jobReviewName is the job.go/machine.toml key ROUND's own sessions and job
// config are keyed under, "review" (design section 4.3, 6.2).
const jobReviewName = string(response.JobReview)

// The section 6.2/6.4 escalation What/Why texts this file writes that
// building.go has no equivalent constant for (a matching text, such as "no
// stored plan for this ticket" or "the worktree could not be prepared", is
// reused directly from building.go: the message is the same regardless of
// which state's own step hit it).
const (
	branchUnrecordedWhat = "the ticket branch holds commits Zing did not record"
	branchUnrecordedWhy  = "the branch does not match every commit Zing has recorded for this ticket, in order; with no fix open, an unrecorded commit is not Zing's"

	treeDirtyBeforeReviewWhat = "the worktree has uncommitted changes before review"
	treeDirtyBeforeReviewWhy  = "ChangedPaths found paths still dirty in the ticket's own worktree"

	noDiffWhat = "the branch has no diff against the base branch"
	noDiffWhy  = "Diff returned no text between the base branch and the branch head"

	lensAgentErrorWhy = "lens %s returned an error"

	lensFailedTwiceWhat = "two review rounds in a row failed"
	headMovedTwiceWhat  = "the ticket branch moved during two review rounds"
)

// errLensFailed is ROUND and CONTINUE's own roundCtx cancellation cause
// (design section 6.2 step 7): the first lens result that is neither a
// parsed ok nor a parsed question document cancels every lens still
// waiting on the semaphore, so a goroutine that never acquired it reserves
// nothing.
var errLensFailed = errors.New("job: reviewing: a lens returned a non-ok, non-question result")

// reviewEscalation is escalationCommit (planning.go) plus this file's own
// "escalation written" log (design section 11), for every environment
// escalation ROUND writes before or during a round: origin is always
// "review" (a fix's own escalations, origin "fix", are the fix driver's,
// entered through the post-build prelude before reviewingHandler.Run ever
// reaches ROUND).
func reviewEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeEnvironment)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginReview)
}

// reviewLenses returns machine.toml's jobs.review.lenses, in file order, as
// response.Lens values (design section 6.2 step 6, 6.7): round 1 (or any
// round after "failed" or "void") runs every one of them.
func reviewLenses(d Deps) []response.Lens {
	cfg := d.Machine.Jobs[jobReviewName]
	out := make([]response.Lens, len(cfg.Lenses))
	for i, l := range cfg.Lenses {
		out[i] = response.Lens(l)
	}
	return out
}

// reviewingHandler runs the real reviewing state (design section 6.2, 6.2a).
type reviewingHandler struct{}

func (h reviewingHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c, handled, err := postBuildPrelude(ctx, t, d, response.EscalationOriginReview)
	if handled || err != nil {
		return c, err
	}

	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		round := rounds[0]
		if round.Job != jobReviewName {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: unexpected answered round job %q", round.Job)
		}
		kind, kindErr := newestQuestionKind(round)
		if kindErr != nil {
			return store.HandlerCommit{}, kindErr
		}
		switch kind {
		case response.QuestionKindReview:
			return store.HandlerCommit{}, errors.New("job: reviewing: TRIAGE is not implemented yet (task 11)")
		case response.QuestionKindQuestion:
			return h.continueRound(ctx, t, d, rounds)
		default:
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: unexpected question kind %q", kind)
		}
	}

	return h.enterRound(ctx, t, d)
}

// ---- decision tree step 3: no round marker, or the newest one -------------

// reviewRoundMarkerPrefix is every "review round " marker's shared prefix
// (design section 5.1): done, failed, void, and asked markers all share it,
// so Store.MarkersWithPrefix returns every one of them, oldest first.
const reviewRoundMarkerPrefix = "review round "

// The four "review round " marker first-line shapes (design section 5.1).
var (
	reviewRoundDoneLine   = regexp.MustCompile(`^review round ([1-9]\d*) done sha ([0-9a-f]{40}) lenses (.+)$`)
	reviewRoundFailedLine = regexp.MustCompile(`^review round ([1-9]\d*) failed$`)
	reviewRoundVoidLine   = regexp.MustCompile(`^review round ([1-9]\d*) void$`)
	reviewRoundAskedLine  = regexp.MustCompile(`^review round ([1-9]\d*) asked$`)
)

// reviewRoundDoneCount reports how many "review round <k> done ..." markers
// the ticket carries: the decision tree's own "n := 1 + count(...)" (design
// section 6.1 step 3). A round's own number persists across a "failed" or
// "void" retry; it only advances once that round actually completes as
// "done".
func reviewRoundDoneCount(markers []store.MessageRow) int {
	n := 0
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		if reviewRoundDoneLine.MatchString(firstLine) {
			n++
		}
	}
	return n
}

// enterRound is decision tree step 3 (design section 6.1): no round marker,
// or the newest one is "failed" or "void", enters ROUND n; the newest is
// "asked" with its questions still open (no answered round reached
// reviewingHandler.Run's own AnsweredRounds check, or it would have routed
// to continueRound instead) waits; the newest is "done" for round n-1
// enters enterFromDone.
func (h reviewingHandler) enterRound(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, reviewRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: review round markers: %w", err)
	}
	n := reviewRoundDoneCount(markers) + 1

	if len(markers) == 0 {
		return h.round(ctx, t, d, n, "", false)
	}
	newest := markers[len(markers)-1]
	firstLine, _, _ := strings.Cut(newest.Body, "\n")

	switch {
	case reviewRoundDoneLine.MatchString(firstLine):
		return h.enterFromDone(ctx, t, d, n)
	case reviewRoundAskedLine.MatchString(firstLine):
		return store.HandlerCommit{}, ErrNoAction
	case reviewRoundFailedLine.MatchString(firstLine), reviewRoundVoidLine.MatchString(firstLine):
		return h.round(ctx, t, d, n, "", true)
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: unrecognized review round marker %q", firstLine)
	}
}

// enterFromDone is decision tree step 3's "newest round marker is done for
// round n-1" branch (design section 6.1): an undecided finding of round n-1
// waits; with no fix-requested-findings marker after the done marker, an
// empty fix list moves to judging and a non-empty one calls fixreq (FIXREQ,
// 6.8); otherwise a fix has already landed (or the post-build prelude would
// have run it), so round n runs again with 6.7's lens selection.
func (h reviewingHandler) enterFromDone(ctx context.Context, t store.Ticket, d Deps, n int) (store.HandlerCommit, error) {
	prevRound := n - 1

	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: findings: %w", err)
	}
	for i := range findings {
		f := &findings[i].Finding
		if f.Round == prevRound && !f.Held && f.Decision == nil {
			return store.HandlerCommit{}, ErrNoAction
		}
	}

	doneMarker, ok, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("review round %d done", prevRound))
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: done marker: %w", err)
	}
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: round %d has no done marker", prevRound)
	}

	fixReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedFindingsPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: fix requested findings markers: %w", err)
	}
	hasReqAfter := false
	for i := range fixReqs {
		if fixReqs[i].ID > doneMarker.ID {
			hasReqAfter = true
			break
		}
	}

	if !hasReqAfter {
		accepted := acceptedRoundFindings(findings, prevRound)
		if len(accepted) == 0 {
			c := baseCommit(t, d)
			c.Next, c.Reason = stateJudging, reasonReviewClean
			return c, nil
		}
		return h.fixreq(ctx, t, d, accepted)
	}

	return h.round(ctx, t, d, n, "", false)
}

// acceptedRoundFindings returns every finding of round with Decision accept,
// unheld, in Findings' own order (artifact id order).
func acceptedRoundFindings(findings []store.FindingRow, round int) []response.FindingArtifact {
	var out []response.FindingArtifact
	for i := range findings {
		f := findings[i].Finding
		if f.Round == round && !f.Held && f.Decision != nil && *f.Decision == response.FindingAccept {
			out = append(out, f)
		}
	}
	return out
}

// fixRequestedFindingsPrefix is the "fix requested findings after run "
// marker family FIXREQ writes (fixRequestMessage's own format, fix.go):
// enterFromDone reads it to tell whether round n-1's own findings already
// opened a fix request.
const fixRequestedFindingsPrefix = "fix requested findings after run "

// fixreq is FIXREQ (design section 6.8): k, the number of "fix requested
// findings" markers the ticket carries, gates against jobs.review.max_loops;
// under the gate, it opens a fix request with accepted's own fix text.
func (h reviewingHandler) fixreq(ctx context.Context, t store.Ticket, d Deps, accepted []response.FindingArtifact) (store.HandlerCommit, error) {
	allReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, "fix requested findings")
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: fix requested findings markers: %w", err)
	}
	k := len(allReqs)
	maxLoops := d.Machine.Jobs[jobReviewName].MaxLoops

	fixText := renderFixFindings(accepted)
	if k >= maxLoops {
		what := fmt.Sprintf("review findings remain after %d fix runs", k)
		why := fmt.Sprintf("max_loops for review is %d", maxLoops)
		return reviewLoopsExhausted(t, d, what, why, fixText), nil
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: max run id: %w", err)
	}
	msg, msgErr := fixRequestMessage(t, FixKindFindings, fixText, maxRunID)
	if msgErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: fix request message: %w", msgErr)
	}
	c := baseCommit(t, d)
	c.Messages = []store.Message{msg}
	return c, nil
}

// reviewLoopsExhausted is FIXREQ's own loops_exhausted escalation (design
// section 6.8): no run caused it, so RunID and SessionID are both nil.
func reviewLoopsExhausted(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeLoopsExhausted)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginReview)
}

// ---- ROUND (design section 6.2) -------------------------------------------

// lensAttempt is one lens's own runJob result, plus its position in the
// lens set that round (or continueRound) launched it from: idx breaks a tie
// among several lenses that failed the same way, in lens order, exactly as
// every other "in lens order" rule in section 6 does. sessionRecord builds
// that attempt's own terminalizing Session/Sessions entry: freshSessionRecord
// for a first-turn ROUND lens, a resumeSessionRecord closure bound to its
// own session id for a CONTINUE resume.
type lensAttempt struct {
	idx           int
	lens          response.Lens
	rr            runResult
	err           error
	sessionRecord func(runResult) *store.SessionUpsert
}

// outcomeString classifies a's own parsed document into the terminal
// outcome string every row but the first four uses (design section 6.2:
// "ok for a run that returned a parsed ok document, question or error for
// those outcomes, error for everything else").
func (a lensAttempt) outcomeString() string {
	if a.err == nil {
		switch a.rr.Res.Response.(type) {
		case *response.FindingsResponse:
			return string(response.OutcomeOk)
		case *response.QuestionResponse:
			return string(response.OutcomeQuestion)
		}
	}
	return string(response.OutcomeError)
}

// isGood reports whether a's own result is a parsed ok or question document
// -- the only two outcomes that do not cancel the round or continuation
// (design section 6.2 step 7).
func (a lensAttempt) isGood() bool {
	if a.err != nil {
		return false
	}
	switch a.rr.Res.Response.(type) {
	case *response.FindingsResponse, *response.QuestionResponse:
		return true
	default:
		return false
	}
}

// runLensesParallel runs one runtime turn per lens, bounded by
// d.LensesParallel in flight at once (design section 6.2 step 7): a
// semaphore channel sized to it gates each goroutine's own runJob call, and
// the first result that is not a parsed ok or question document cancels
// roundCtx, so a goroutine still waiting on the semaphore reserves nothing.
// build is called once per lens, inside its own goroutine, to assemble that
// lens's own SessionUpsert and RunRequest; it must not block. The returned
// attempts are sorted by idx (lens order), regardless of the order they
// actually finished in.
func runLensesParallel(
	ctx context.Context, d Deps, t store.Ticket,
	lenses []response.Lens,
	build func(lens response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert),
) []lensAttempt {
	roundCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	sem := make(chan struct{}, d.LensesParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var attempts []lensAttempt

	for i, lens := range lenses {
		wg.Add(1)
		go func(idx int, lens response.Lens) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-roundCtx.Done():
				return
			}
			defer func() { <-sem }()

			su, req, sessionRecord := build(lens)
			lensStr := string(lens)
			rr, err := runJob(roundCtx, d, t, jobReviewName, su, req, nil, &lensStr)

			at := lensAttempt{idx: idx, lens: lens, rr: rr, err: err, sessionRecord: sessionRecord}
			if !at.isGood() {
				cancel(errLensFailed)
			}

			mu.Lock()
			attempts = append(attempts, at)
			mu.Unlock()
		}(i, lens)
	}
	wg.Wait()

	slices.SortFunc(attempts, func(a, b lensAttempt) int { return a.idx - b.idx })
	return attempts
}

// terminalizeAttempts builds the Runs, Session, and Sessions a commit
// carries for every lens that actually reserved a run (design section 6.2:
// "Session holds the first reserved run's session record in lens order and
// Sessions holds the rest; a nil record (only on runtime.ErrStart) is
// skipped"), in idx (lens) order.
func terminalizeAttempts(attempts []lensAttempt) (runs []store.Run, session *store.SessionUpsert, sessions []store.SessionUpsert) {
	for i := range attempts {
		a := &attempts[i]
		if a.rr.Reserved.RunID == 0 {
			continue
		}
		runs = append(runs, terminalRuns(a.rr, a.outcomeString())...)

		rec := a.sessionRecord(a.rr)
		if rec == nil {
			continue
		}
		if session == nil {
			session = rec
			continue
		}
		sessions = append(sessions, *rec)
	}
	return runs, session, sessions
}

// firstBadAttempt returns the first (lens order) attempt matching pred, in
// idx order, so the table's several "any lens X" rows and their own "lens
// <l>: <reason>" text all name the same lens deterministically when more
// than one matches in the same round.
func firstBadAttempt(attempts []lensAttempt, pred func(lensAttempt) bool) (lensAttempt, bool) {
	for i := range attempts {
		if pred(attempts[i]) {
			return attempts[i], true
		}
	}
	return lensAttempt{}, false
}

// execFailureKind names one lens's own non-agent-authored failure for the
// "review round <n> failed" marker's reason (design section 6.2 row 7):
// "invalid output", "exec failed", "timed out", or "configuration error".
// ok is false when a's own error is none of these (an agent-authored error
// document routes through the agent-error row instead, checked first).
func execFailureKind(a lensAttempt) (string, bool) {
	var invErr *runtime.InvalidOutputError
	switch {
	case errors.As(a.err, &invErr): //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags
		return "invalid output", true
	case errors.Is(a.err, runtime.ErrTimeout):
		return "timed out", true
	case isExecFailure(a.err):
		return "exec failed", true
	case errors.Is(a.err, ErrConfig):
		return "configuration error", true
	default:
		return "", false
	}
}

// round is ROUND (design section 6.2): the plan, worktree, branch, and
// clean-tree checks, the diff, the lens set (6.7), the parallel run, and
// the commit-selection table (step 9). n is the round number the decision
// tree already computed; notes carries a retry's own owner notes (fenced),
// empty outside a retry (task 12); priorFailedOrVoid is true when the
// newest "review round <n>" marker already read "failed" or "void" before
// this attempt, gating the two-in-a-row escalations of row 7 and row 8.
func (h reviewingHandler) round(ctx context.Context, t store.Ticket, d Deps, n int, notes string, priorFailedOrVoid bool) (store.HandlerCommit, error) {
	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: stored plan: %w", err)
	}
	if !havePlan {
		return reviewEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return reviewEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: build reports: %w", err)
	}
	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: branch commits: %w", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		return withBranch(reviewEscalation(t, d, branchUnrecordedWhat, branchUnrecordedWhy, ""), wt), nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: changed paths: %w", err)
	}
	if len(changed) > 0 {
		quoted := make([]string, len(changed))
		for i, c := range changed {
			quoted[i] = strconv.Quote(c.Path)
		}
		tried := strings.Join(quoted, ", ")
		return withBranch(reviewEscalation(t, d, treeDirtyBeforeReviewWhat, treeDirtyBeforeReviewWhy, tried), wt), nil
	}

	sha, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: head sha: %w", err)
	}
	diff, err := proj.Orch.Diff(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return withBranch(reviewEscalation(t, d, noDiffWhat, noDiffWhy, ""), wt), nil
	}
	idx := orchestrator.ParseDiff(diff)

	lenses, err := h.lensesForRound(ctx, t, d, wt, n, sha)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: plan xml: %w", err)
	}
	jobCfg := d.Machine.Jobs[jobReviewName]
	jobPromptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}
	schemas, err := renderSchemas(response.JobReview, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}
	var extra []prompt.NamedInput
	if notes != "" {
		extra = append(extra, prompt.Notes(notes))
	}

	attempts := runLensesParallel(ctx, d, t, lenses, func(lens response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert) {
		codeSection, csErr := lensCodeSection(lens)
		if csErr != nil {
			return store.SessionUpsert{Job: jobReviewName, Runtime: jobCfg.Runtime}, runtime.RunRequest{}, freshSessionRecord
		}
		in, buildErr := prompt.ForReview(jobPromptText, string(lens), sha, codeSection, planXML, diff, extra)
		if buildErr != nil {
			return store.SessionUpsert{Job: jobReviewName, Runtime: jobCfg.Runtime}, runtime.RunRequest{}, freshSessionRecord
		}
		in.Schemas = schemas
		req := runtime.RunRequest{
			Job: response.JobReview, Label: fmt.Sprintf("%d-%s", n, lens), WorkDir: wt.Dir(),
			Prompt: prompt.Assemble(in),
		}
		return store.SessionUpsert{Job: jobReviewName, Runtime: jobCfg.Runtime}, req, freshSessionRecord
	})

	return h.roundCommit(ctx, t, d, proj, wt, n, sha, idx, attempts, priorFailedOrVoid)
}

// lensCodeSection reads lens's own prompt file and returns its "## In code"
// section (design section 6.2 step 6, 12.1).
func lensCodeSection(lens response.Lens) (string, error) {
	text, err := readAsset("prompts/lenses/" + string(lens) + ".md")
	if err != nil {
		return "", fmt.Errorf("job: reviewing: %w", err)
	}
	return prompt.CodeLensSection(text)
}

// lensesForRound is design section 6.7's own lens set: every lens for round
// 1; for round n >= 2, selectLenses over round n-1's own rows and the paths
// that changed between its own frozen sha and the current one. Round n-1's
// own frozen sha is read from any one of its own finding rows (kept or
// held: every row of a round carries that round's own SHA); a round with
// none at all (every lens clean with zero findings) has nothing to compare
// against, so the full lens set runs again rather than guessing.
func (h reviewingHandler) lensesForRound(ctx context.Context, t store.Ticket, d Deps, wt orchestrator.Worktree, n int, sha string) ([]response.Lens, error) {
	all := reviewLenses(d)
	if n <= 1 {
		return all, nil
	}

	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("job: reviewing: findings: %w", err)
	}
	prevRound := n - 1
	var prevRows []response.FindingArtifact
	prevSHA := ""
	for i := range findings {
		f := findings[i].Finding
		if f.Round != prevRound {
			continue
		}
		prevSHA = f.SHA
		if !f.Held {
			prevRows = append(prevRows, f)
		}
	}
	if prevSHA == "" {
		return all, nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return nil, ErrConfig
	}
	changedFiles, err := proj.Orch.ChangedFilesBetween(ctx, wt, prevSHA, sha)
	if err != nil {
		return nil, fmt.Errorf("job: reviewing: changed files between: %w", err)
	}
	return selectLenses(n, all, prevRows, changedFiles), nil
}

// roundCommit is ROUND's own commit-selection table (design section 6.2
// step 9): the first matching row wins. resolveIDs is always nil here: a
// fresh ROUND entry (enterRound) or a same-round retry after "failed" or
// "void" never carries an answered round of its own to resolve; CONTINUE's
// own commit is continueCommit, below, which does carry one.
func (h reviewingHandler) roundCommit(
	ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree,
	n int, sha string, idx orchestrator.DiffIndex, attempts []lensAttempt, priorFailedOrVoid bool,
) (store.HandlerCommit, error) {
	return h.tableCommit(ctx, t, d, proj, wt, n, sha, idx, attempts, priorFailedOrVoid, nil, nil, nil)
}

// tableCommit is roundCommit and continueRound's own shared commit-selection
// table (design section 6.2 step 9, 6.2a step 3): CONTINUE feeds it the
// resumed attempts, resolveIDs (every consumed round's own question ids),
// its own held rows folded into a clean success (heldFindings), and the
// lenses the original round already found done (priorDone, merged into a
// re-ask's own "done" list); ROUND feeds it a fresh attempt set, a nil
// resolveIDs, no held findings, and no prior-done lenses.
func (h reviewingHandler) tableCommit(
	ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree,
	n int, sha string, idx orchestrator.DiffIndex, attempts []lensAttempt, priorFailedOrVoid bool,
	resolveIDs []int64, heldFindings []response.Finding, priorDone []response.Lens,
) (store.HandlerCommit, error) {
	if ctx.Err() != nil {
		if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, runtime.ErrCanceled) }); ok {
			return store.HandlerCommit{}, at.err
		}
		return store.HandlerCommit{}, ctx.Err()
	}

	if _, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, ErrBudget) }); ok {
		c := budgetEscalationCommit(t, d, resolveIDs)
		c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
		return c, nil
	}

	if _, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, ErrSandbox) }); ok {
		return sandboxEscalationCommit(t, d, resolveIDs, response.EscalationOriginReview, d.Sandboxes.ReadOnly.Reason()), nil
	}

	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, store.ErrClaimLost) }); ok {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: lens %s: %w", at.lens, at.err)
	}

	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, ErrConfig) }); ok {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: lens %s: %w", at.lens, at.err)
	}

	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool {
		if a.err != nil {
			return false
		}
		_, isErr := a.rr.Res.Response.(*response.ErrorResponse)
		return isErr
	}); ok {
		errResp, ok := at.rr.Res.Response.(*response.ErrorResponse)
		if !ok {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: lens %s: expected an error document", at.lens)
		}
		c := escalationCommit(t, d, &at.rr.Reserved.RunID, &at.rr.Reserved.SessionID,
			string(errResp.Error.Code), errResp.Error.What, errResp.Error.Why, errResp.Error.Tried, response.EscalationOriginReview)
		c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = append(c.Messages, reviewRoundFailedMarker(t.ID, n, fmt.Sprintf(lensAgentErrorWhy, at.lens)))
		slog.Warn("escalation written", "ticket_id", t.ID, "session_id", at.rr.Reserved.SessionID, "run_id", at.rr.Reserved.RunID,
			"code", errResp.Error.Code, "origin", string(response.EscalationOriginReview))
		return c, nil
	}

	if at, kind, ok := firstExecFailure(attempts); ok {
		reason := fmt.Sprintf("lens %s: %s", at.lens, kind)
		c := baseCommit(t, d)
		c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = []store.Message{reviewRoundFailedMarker(t.ID, n, reason)}
		if priorFailedOrVoid {
			code := string(response.EscalationCodeRuntimeExecFailed)
			if kind == "invalid output" {
				code = string(response.EscalationCodeResponseInvalid)
			}
			slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
			c.Escalation = &store.EscalationCommit{
				Body: code + ": " + lensFailedTwiceWhat,
				Payload: response.EscalationPayload{
					Code: code, What: lensFailedTwiceWhat, Why: reason,
					Options: escalationOptions, Origin: string(response.EscalationOriginReview),
				},
			}
			waiting := waitingFlagQuestions
			c.Waiting = &waiting
		}
		return c, nil
	}

	sha2, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: head sha: %w", err)
	}
	if sha2 != sha {
		reason := fmt.Sprintf("head moved from %s to %s", sha, sha2)
		c := baseCommit(t, d)
		c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("review round %d void\n%s", n, reason),
		}}
		if priorFailedOrVoid {
			slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil,
				"code", string(response.EscalationCodeEnvironment), "origin", string(response.EscalationOriginReview))
			c.Escalation = &store.EscalationCommit{
				Body: string(response.EscalationCodeEnvironment) + ": " + headMovedTwiceWhat,
				Payload: response.EscalationPayload{
					Code: string(response.EscalationCodeEnvironment), What: headMovedTwiceWhat, Why: reason,
					Options: escalationOptions, Origin: string(response.EscalationOriginReview),
				},
			}
			waiting := waitingFlagQuestions
			c.Waiting = &waiting
		}
		return c, nil
	}

	anyQuestion := false
	for i := range attempts {
		if _, ok := attempts[i].rr.Res.Response.(*response.QuestionResponse); ok {
			anyQuestion = true
			break
		}
	}
	if anyQuestion {
		return h.askedCommit(t, d, n, sha, attempts, resolveIDs, priorDone, len(heldFindings))
	}

	return h.successCommit(ctx, t, d, n, sha, idx, attempts, resolveIDs, heldFindings)
}

// firstExecFailure returns the first (lens order) attempt whose own error
// is an invalid output, an exec failure, a timeout, or a configuration
// error -- row 7 of design section 6.2's own table, checked after the
// agent-authored error row so a parsed *response.ErrorResponse never
// double-matches here.
func firstExecFailure(attempts []lensAttempt) (lensAttempt, string, bool) {
	for i := range attempts {
		if attempts[i].err == nil {
			continue
		}
		if kind, ok := execFailureKind(attempts[i]); ok {
			return attempts[i], kind, true
		}
	}
	return lensAttempt{}, "", false
}

// reviewRoundFailedMarker is the "review round <n> failed" marker every
// failure row of design section 6.2's table writes, reason on its own
// second line.
func reviewRoundFailedMarker(ticketID int64, n int, reason string) store.Message {
	return store.Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("review round %d failed\n%s", n, reason),
	}
}

// fixRequestOrLoopsExhausted is FIXREQ's own body (design section 6.8),
// shared by enterFromDone's own fixreq entry and successCommit's inline
// one: under jobs.review.max_loops, it returns a "fix requested findings"
// marker (msg non-nil) the caller adds to its own commit; at or past the
// gate, it returns nil and the what/why a loops_exhausted escalation
// carries instead.
func fixRequestOrLoopsExhausted(ctx context.Context, t store.Ticket, d Deps, accepted []response.FindingArtifact) (msg *store.Message, what, why string, err error) {
	allReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, "fix requested findings")
	if err != nil {
		return nil, "", "", fmt.Errorf("job: reviewing: fix requested findings markers: %w", err)
	}
	k := len(allReqs)
	maxLoops := d.Machine.Jobs[jobReviewName].MaxLoops
	if k >= maxLoops {
		return nil, fmt.Sprintf("review findings remain after %d fix runs", k), fmt.Sprintf("max_loops for review is %d", maxLoops), nil
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return nil, "", "", fmt.Errorf("job: reviewing: max run id: %w", err)
	}
	m, msgErr := fixRequestMessage(t, FixKindFindings, renderFixFindings(accepted), maxRunID)
	if msgErr != nil {
		return nil, "", "", fmt.Errorf("job: reviewing: fix request message: %w", msgErr)
	}
	return &m, "", "", nil
}

// waitingFlagReview is the review question's own waiting_on value (design
// section 6.4), distinct from waitingFlagQuestions: a review question is
// triaged (task 11), not answered the generic way.
const waitingFlagReview = "review"

// reviewItemText is Item.Text's own format for a review question (design
// section 6.4): "[<severity>] <lenses joined by +> <path>:<line> <text>
// Fix: <fix>", whitespace collapsed, cut to 600 runes with "..." appended
// when cut.
func reviewItemText(row response.FindingArtifact) string {
	lensNames := make([]string, len(row.Lenses))
	for i, l := range row.Lenses {
		lensNames[i] = string(l)
	}
	text := fmt.Sprintf("[%s] %s %s %s Fix: %s", row.Severity, strings.Join(lensNames, "+"), row.Location, row.Text, row.Fix)
	text = collapseWhitespace(text)
	if cut := cutRunes(text, 600); cut != text {
		return cut + "..."
	}
	return text
}

// reviewQuestionMessage is the review question (design section 6.4): runID
// is the round's first run in lens order (or the discuss run, task 11).
func reviewQuestionMessage(t store.Ticket, d Deps, n int, runID int64, above []response.FindingArtifact) (store.Message, error) {
	items := make([]response.Item, len(above))
	for i := range above {
		items[i] = response.Item{Ref: above[i].ID, Text: reviewItemText(above[i])}
	}
	payload, err := json.Marshal(response.QuestionPayload{
		Kind: response.QuestionKindReview, State: response.QuestionStateOpen,
		Recommended: "Accept each finding unless it is wrong",
		// Options is a bare JSON array in the schema (no omitempty), so a nil
		// slice -- Go's zero value, and every review question's own value:
		// section 6.4 gives it none -- must still marshal as [], not null.
		Options: []response.Option{},
		Items:   items,
	})
	if err != nil {
		return store.Message{}, fmt.Errorf("job: reviewing: marshal review question payload: %w", err)
	}
	body := fmt.Sprintf(
		"Triage review findings\n\nRound %d of the code review found %s above the floor (%s). "+
			"Accept a finding to fix it. Drop it to leave the code as it is. Discuss it to send your reply on this question to the lens that raised it.",
		n, orchestrator.CountNoun(len(above), "finding", "findings"), string(d.Floor),
	)
	state := questionStateOpen
	return store.Message{
		TicketID: t.ID, RunID: &runID, Type: msgTypeQuestion, Author: authorZing,
		State: &state, Body: body, Payload: payload,
	}, nil
}

// successCommit is ROUND's own success commit (design section 6.2 step 9's
// last row) and CONTINUE's own "all ok" outcome (6.2a step 3): extra is the
// round's own held findings (nil for a fresh ROUND; CONTINUE's held rows,
// converted back to response.Finding, for a completed continuation).
// Findings: survivors := FilterFindings(all, idx), then DedupFindings, then
// ids r<n>f<k>. At-or-below d.Floor get Decision accept. Then the first
// matching row: findings above the floor post the review question (6.4);
// else one or more survivors call FIXREQ (6.8); else Next = judging.
func (h reviewingHandler) successCommit(
	ctx context.Context, t store.Ticket, d Deps, n int, sha string, idx orchestrator.DiffIndex,
	attempts []lensAttempt, resolveIDs []int64, extra []response.Finding,
) (store.HandlerCommit, error) {
	runIDByLens := make(map[response.Lens]int64, len(attempts))
	all := append([]response.Finding(nil), extra...)
	for i := range attempts {
		fr, ok := attempts[i].rr.Res.Response.(*response.FindingsResponse)
		if !ok {
			continue
		}
		runIDByLens[attempts[i].lens] = attempts[i].rr.Reserved.RunID
		all = append(all, fr.Findings...)
	}

	survivors := FilterFindings(all, idx)
	merged := DedupFindings(survivors, reviewLenses(d))
	for i := range merged {
		merged[i].ID = fmt.Sprintf("r%df%d", n, i+1)
		merged[i].Round = n
		merged[i].SHA = sha
	}
	atOrBelow, above := splitByFloor(merged, d.Floor)
	stored := sortByID(append(append([]response.FindingArtifact{}, atOrBelow...), above...))

	c := baseCommit(t, d)
	c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
	c.ResolveQuestions = resolveIDs

	lensList := make([]string, len(attempts))
	for i := range attempts {
		lensList[i] = string(attempts[i].lens)
	}
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("review round %d done sha %s lenses %s\nkept %d dropped %d merged %d",
			n, sha, strings.Join(lensList, ","), len(merged), len(all)-len(survivors), len(survivors)-len(merged)),
	})

	artifacts := make([]store.Artifact, len(stored))
	for i := range stored {
		payload, err := json.Marshal(stored[i])
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: marshal finding artifact %s: %w", stored[i].ID, err)
		}
		runID := runIDByLens[stored[i].Lens]
		artifacts[i] = store.Artifact{Type: artifactTypeFinding, RunID: &runID, Payload: payload}
	}
	c.Artifacts = artifacts

	switch {
	case len(above) > 0:
		runID := attempts[0].rr.Reserved.RunID
		qMsg, qErr := reviewQuestionMessage(t, d, n, runID, above)
		if qErr != nil {
			return store.HandlerCommit{}, qErr
		}
		c.Messages = append(c.Messages, qMsg)
		waiting := waitingFlagReview
		c.Waiting = &waiting
		return c, nil

	case len(atOrBelow) > 0:
		msg, what, why, fixErr := fixRequestOrLoopsExhausted(ctx, t, d, atOrBelow)
		if fixErr != nil {
			return store.HandlerCommit{}, fixErr
		}
		if msg == nil {
			code := string(response.EscalationCodeLoopsExhausted)
			slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
			c.Escalation = &store.EscalationCommit{
				Body: code + ": " + what,
				Payload: response.EscalationPayload{
					Code: code, What: what, Why: why, Tried: renderFixFindings(atOrBelow),
					Options: escalationOptions, Origin: string(response.EscalationOriginReview),
				},
			}
			waiting := waitingFlagQuestions
			c.Waiting = &waiting
			return c, nil
		}
		c.Messages = append(c.Messages, *msg)
		return c, nil

	default:
		c.Next, c.Reason = stateJudging, reasonReviewClean
		return c, nil
	}
}

// askedCommit is ASKED (design section 6.2a): one or more lenses asked and
// every other lens returned ok. priorDone is empty for a fresh round's own
// first ask; a re-ask (CONTINUE, when a resumed lens asks again) passes the
// lenses the original round already found done, merged into this commit's
// own "done" list, in lens order; heldSoFar is the count of held rows the
// round already carries, so a re-ask's own new held rows continue that
// same per-round k sequence rather than restarting it.
func (h reviewingHandler) askedCommit(
	t store.Ticket, d Deps, n int, sha string, attempts []lensAttempt, resolveIDs []int64,
	priorDone []response.Lens, heldSoFar int,
) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
	c.ResolveQuestions = resolveIDs

	var askingRunIDs []int64
	doneSet := make(map[response.Lens]bool, len(attempts)+len(priorDone))
	for _, l := range priorDone {
		doneSet[l] = true
	}
	var artifacts []store.Artifact
	k := heldSoFar

	for i := range attempts {
		a := &attempts[i]
		switch resp := a.rr.Res.Response.(type) {
		case *response.QuestionResponse:
			runID := a.rr.Reserved.RunID
			askingRunIDs = append(askingRunIDs, runID)
			msgs, err := questionMessagesFor(t.ID, resp.Questions)
			if err != nil {
				return store.HandlerCommit{}, err
			}
			for j := range msgs {
				msgs[j].RunID = &runID
			}
			c.Messages = append(c.Messages, msgs...)

		case *response.FindingsResponse:
			runID := a.rr.Reserved.RunID
			doneSet[a.lens] = true
			for _, f := range resp.Findings {
				k++
				row := response.FindingArtifact{
					Lens: f.Lens, Severity: f.Severity, Location: f.Location, Text: f.Text, Fix: f.Fix, PlanRef: f.PlanRef,
					Held: true, ID: fmt.Sprintf("r%dh%d", n, k), Round: n, SHA: sha, Lenses: []response.Lens{f.Lens},
				}
				payload, err := json.Marshal(row)
				if err != nil {
					return store.HandlerCommit{}, fmt.Errorf("job: reviewing: marshal held finding %s: %w", row.ID, err)
				}
				artifacts = append(artifacts, store.Artifact{Type: artifactTypeFinding, RunID: &runID, Payload: payload})
			}
		}
	}
	c.Artifacts = artifacts

	slices.Sort(askingRunIDs)
	runsStrs := make([]string, len(askingRunIDs))
	for i, id := range askingRunIDs {
		runsStrs[i] = strconv.FormatInt(id, 10)
	}
	var doneLensNames []string
	for _, l := range reviewLenses(d) {
		if doneSet[l] {
			doneLensNames = append(doneLensNames, string(l))
		}
	}
	doneStr := "-"
	if len(doneLensNames) > 0 {
		doneStr = strings.Join(doneLensNames, ",")
	}
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("review round %d asked\nruns %s\ndone %s", n, strings.Join(runsStrs, ","), doneStr),
	})

	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c, nil
}

// ---- CONTINUE (design section 6.2a) ---------------------------------------

// parseAskedMarker parses M, the newest "review round <n> asked" marker
// (design section 5.1): line 2 "runs <rid1,...>" (ascending) and line 3
// "done <lens1,...>" ("-" for none).
func parseAskedMarker(m store.MessageRow) (n int, runIDs []int64, doneLenses []response.Lens, err error) {
	lines := strings.Split(m.Body, "\n")
	if len(lines) < 3 {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed (fewer than 3 lines)", m.ID)
	}
	match := reviewRoundAskedLine.FindStringSubmatch(lines[0])
	if match == nil {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed first line %q", m.ID, lines[0])
	}
	n, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: round number: %w", m.ID, err)
	}

	runsField, ok := strings.CutPrefix(lines[1], "runs ")
	if !ok {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed line 2 %q", m.ID, lines[1])
	}
	for s := range strings.SplitSeq(runsField, ",") {
		id, idErr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if idErr != nil {
			return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: run id %q: %w", m.ID, s, idErr)
		}
		runIDs = append(runIDs, id)
	}

	doneField, ok := strings.CutPrefix(lines[2], "done ")
	if !ok {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed line 3 %q", m.ID, lines[2])
	}
	if doneField != "-" {
		for s := range strings.SplitSeq(doneField, ",") {
			doneLenses = append(doneLenses, response.Lens(strings.TrimSpace(s)))
		}
	}
	return n, runIDs, doneLenses, nil
}

// heldFindingsForRound returns round n's own held findings, converted back
// to response.Finding (the shape a fresh lens result carries), and the
// frozen sha every row of the round shares. ok is false when the round
// carries no held row at all (every "done" lens returned zero findings):
// heldFindingsForRound has nothing to compare, so continueRound treats the
// round as unchanged rather than guessing.
func heldFindingsForRound(ctx context.Context, d Deps, ticketID int64, n int) (findings []response.Finding, sha string, ok bool, err error) {
	rows, err := d.Store.Findings(ctx, ticketID)
	if err != nil {
		return nil, "", false, fmt.Errorf("job: reviewing: findings: %w", err)
	}
	for i := range rows {
		f := rows[i].Finding
		if f.Round != n || !f.Held {
			continue
		}
		sha = f.SHA
		ok = true
		findings = append(findings, response.Finding{
			Lens: f.Lens, Severity: f.Severity, Location: f.Location, Text: f.Text, Fix: f.Fix, PlanRef: f.PlanRef,
		})
	}
	return findings, sha, ok, nil
}

// reviewCapResumesEscalation is capResumesEscalation (planning.go) plus
// this file's own "escalation written" log (design section 6.2a step 2):
// an exhausted asking lens's own session, escalated once; the round it
// belongs to stays answered (no ResolveQuestions), so a retry (task 12, 5.6)
// can still read it.
func reviewCapResumesEscalation(t store.Ticket, d Deps, sessionID int64) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", nil,
		"code", string(response.EscalationCodeResumesExhausted), "origin", string(response.EscalationOriginCapResumes))
	return capResumesEscalation(t, d, sessionID)
}

// continueRound is CONTINUE (design section 6.2a): once every question of
// the newest "review round <n> asked" marker M is answered, resume each
// asking lens's own session in parallel, under the same semaphore ROUND
// uses. rounds is reviewingHandler.Run's own AnsweredRounds read, reused
// here rather than read twice.
func (h reviewingHandler) continueRound(ctx context.Context, t store.Ticket, d Deps, rounds []store.Round) (store.HandlerCommit, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, reviewRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: review round markers: %w", err)
	}
	if len(markers) == 0 {
		return store.HandlerCommit{}, errors.New("job: reviewing: continue: no review round marker")
	}
	newest := markers[len(markers)-1]
	if !reviewRoundAskedLine.MatchString(strings.SplitN(newest.Body, "\n", 2)[0]) {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: newest review round marker is not asked: %q", newest.Body)
	}
	n, runIDs, priorDone, err := parseAskedMarker(newest)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	byRunID := make(map[int64]store.Round, len(rounds))
	for _, r := range rounds {
		if r.RunID != nil {
			byRunID[*r.RunID] = r
		}
	}
	answered := make([]store.Round, 0, len(runIDs))
	var resolveIDs []int64
	for _, rid := range runIDs {
		r, ok := byRunID[rid]
		if !ok {
			return store.HandlerCommit{}, ErrNoAction
		}
		answered = append(answered, r)
		resolveIDs = append(resolveIDs, questionIDs(r)...)
	}

	// Step 1 (design section 6.2a): HeadSHA(wt) must equal the round's own
	// sha before the cap gate ever runs, else the round is void regardless
	// of any asking session's own resume state.
	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return reviewEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	headSHA, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: head sha: %w", err)
	}
	heldFindings, roundSHA, haveHeld, err := heldFindingsForRound(ctx, d, t.ID, n)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	sha := headSHA
	if haveHeld {
		sha = roundSHA
	}
	if haveHeld && headSHA != roundSHA {
		c := baseCommit(t, d)
		c.ResolveQuestions = resolveIDs
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("review round %d void\nhead moved from %s to %s", n, roundSHA, headSHA),
		}}
		return c, nil
	}

	// Step 2: for each asking run, the cap gate; an exhausted session
	// escalates once with no run started for any lens.
	maxResumes := d.Machine.Jobs[jobReviewName].MaxResumes
	type asker struct {
		lens    response.Lens
		sess    store.Session
		answers string
	}
	askers := make([]asker, 0, len(answered))
	for _, r := range answered {
		run, runErr := d.Store.RunByID(ctx, *r.RunID)
		if runErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: run by id: %w", runErr)
		}
		if run.Lens == nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: run %d carries no lens", *r.RunID)
		}
		sess, state, sessErr := d.Store.SessionByID(ctx, *r.SessionID, maxResumes)
		if sessErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: session by id: %w", sessErr)
		}
		if state == store.SessionExhausted {
			has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
			if hasErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: has escalation: %w", hasErr)
			}
			if has {
				return store.HandlerCommit{}, ErrNoAction
			}
			return reviewCapResumesEscalation(t, d, sess.ID), nil
		}
		answers, ansErr := renderRoundAnswers(r)
		if ansErr != nil {
			return store.HandlerCommit{}, ansErr
		}
		askers = append(askers, asker{lens: response.Lens(*run.Lens), sess: sess, answers: answers})
	}

	diff, err := proj.Orch.Diff(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: diff: %w", err)
	}
	idx := orchestrator.ParseDiff(diff)

	schemas, err := renderSchemas(response.JobReview, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}

	lenses := make([]response.Lens, len(askers))
	byLens := make(map[response.Lens]asker, len(askers))
	for i := range askers {
		lenses[i] = askers[i].lens
		byLens[askers[i].lens] = askers[i]
	}

	attempts := runLensesParallel(ctx, d, t, lenses, func(lens response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert) {
		a := byLens[lens]
		in := prompt.ForReviewResume([]prompt.NamedInput{prompt.Answers(a.answers)})
		in.Schemas = schemas
		req := runtime.RunRequest{
			Job: response.JobReview, Label: fmt.Sprintf("%d-%s", n, lens), WorkDir: wt.Dir(),
			SessionID: derefString(a.sess.ExternalID), Prompt: prompt.Assemble(in),
		}
		su := store.SessionUpsert{ID: &a.sess.ID, BumpResumes: true}
		sessionID := a.sess.ID
		return su, req, func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sessionID, rr) }
	})

	return h.tableCommit(ctx, t, d, proj, wt, n, sha, idx, attempts, false, resolveIDs, heldFindings, priorDone)
}

// derefString returns *s, or "" for a nil s: a resumed session's own
// external id is always set by the time it can be resumed (D13), but the
// zero value is the safe, obviously-wrong fallback rather than a panic.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
