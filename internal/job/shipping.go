// shipping.go declares the consumer-side interfaces the shipping state
// machine needs from GitHub, over orchestrator.GitHubClient's REST and
// GraphQL surface (PKG9-PLAN.md section 10.3). Declaring them here, not
// widening orchestrator.GitHub, keeps every existing fake of that
// four-method interface (cmd/zing/selftest.go, internal/dispatch/dispatch_test.go,
// internal/console/resume_e2e_test.go, the orchestrator tests) valid: a test
// of a handler that needs PullRequests, DraftFlips, or Checks implements
// only the small interface it uses. M3 task 6 adds PUBLISH (design section
// 8.2), the shipping decision tree's own step (4).
//
// Task 7 adds POLL (8.3 to 8.9) and the shipping rows of
// resolvePostBuildEscalation (5.6, postbuild.go): job.go's Registry()["shipping"]
// now points at shipHandler, skeleton.go's own shippingHandler is gone (the
// same swap task 7a, M2 task 8, made for judging.go's judgeHandler).
//
// M4 task 4 (respond.go) wires Project.Threads in for real: POLL's own
// fingerprint and its row 5 (an actionable thread starts a respond batch),
// decision tree step (1)'s "job respond" branch, step (2) (RESPOND's first
// turn and every resume), and the respond rows of resolvePostBuildEscalation.
// M4 task 5 (respond.go) adds decision tree step (3), APPLY (9.3). M4 task 6
// (respond.go) adds rows 1 and 2, FIX-REPLIES and RE-REQUEST, wired into
// poll below. M4 task 7 (this file) adds rows 3, 6, 6a, and 8: the
// draft-to-ready flip when CI is green and every thread is resolved, a
// reopened loop (failed CI, or any unresolved thread of any class)
// flipping a ready pull request back to draft and withdrawing a stale
// merge question in the same commit, the leftover resolve (9.5), and the
// informational marker an unclassified thread blocks rows 8 and 9 behind.
// Row 9, the merge question and MERGE itself, stays M4 task 8's; reaching
// its condition (CI green, zero unresolved threads, not draft) is
// ErrNoAction, not a silent no-op, since nothing before it can write the
// "merge asked" marker a resumed round would need.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// PullRequests is what the shipping handler needs to read and merge a pull
// request (M3 task 2, task 6 onward).
type PullRequests interface {
	// GetPR reads the pull request's current state (orchestrator.GitHubClient.GetPR).
	GetPR(ctx context.Context, owner, repo string, number int) (orchestrator.PRState, error)
	// Merge merges the pull request with sha pinning its head
	// (orchestrator.GitHubClient.Merge, called with sha set).
	Merge(ctx context.Context, owner, repo string, number int, sha, method, title string) (string, error)
}

// DraftFlips is what the shipping handler needs to flip a pull request
// between draft and ready for review (M4 task 1, task 7 onward).
type DraftFlips interface {
	// MarkReady marks the pull request ready for review
	// (orchestrator.GitHubClient.MarkReady, GraphQL).
	MarkReady(ctx context.Context, prNodeID string) error
	// ConvertToDraft converts the pull request back to a draft
	// (orchestrator.GitHubClient.ConvertToDraft, GraphQL).
	ConvertToDraft(ctx context.Context, prNodeID string) error
}

// Checks is what the shipping handler needs to decide CI state and fetch a
// failed job's log (M3 task 2, task 4 and task 7 onward).
type Checks interface {
	// ListCheckRuns lists every check run GitHub has recorded for sha, the
	// newest per name (orchestrator.GitHubClient.ListCheckRuns, filter
	// "latest", per_page 100, every page).
	ListCheckRuns(ctx context.Context, owner, repo, sha string) ([]orchestrator.CheckRun, error)
	// ListStatuses lists every legacy commit status for sha
	// (orchestrator.GitHubClient.ListStatuses, per_page 100, every page).
	ListStatuses(ctx context.Context, owner, repo, sha string) ([]orchestrator.CommitStatus, error)
	// RequiredCheckRules reads branch's required status checks, app-aware
	// (orchestrator.GitHubClient.RequiredCheckRules).
	RequiredCheckRules(ctx context.Context, owner, repo, branch string) ([]orchestrator.RequiredCheck, error)
	// JobLogTail returns the last lines lines of a failed Actions job's log
	// (orchestrator.GitHubClient.JobLogTail).
	JobLogTail(ctx context.Context, owner, repo string, jobID int64, lines int) (string, error)
	// RerunJob re-runs one failed Actions job
	// (orchestrator.GitHubClient.RerunJob).
	RerunJob(ctx context.Context, owner, repo string, jobID int64) error
}

// ShipTracker is what the shipping handler needs from the tracker (design
// section 8.6): PostPRLink posts PUBLISH's own PR-link comment (step 5,
// below); PostDone posts DONE's done comment, then closes the issue (task
// 7). Both check a hidden marker, in a comment authored by the tracker's
// own authenticated login, across every page of the issue's comments before
// posting, so each is safe to call again after a crash (design section 11).
// The dispatcher implements it over its own tracker and bindings and passes
// itself as Deps.Tracker (internal/dispatch/dispatch.go).
type ShipTracker interface {
	PostPRLink(ctx context.Context, projectID int64, ref, prURL string) error
	PostDone(ctx context.Context, projectID int64, ref, prURL, mergeSHA string) error
}

// shipHandler runs the real shipping state (design section 8). It is
// job.go's own Registry()["shipping"] entry.
type shipHandler struct{}

// Run is the shipping state's own decision tree (design section 8.1): the
// prelude (P), step (1)'s own "job respond" branch (RESPOND resume with
// answers, M4 task 4) and "merge" branch (MERGE-ANSWER, M4 task 8), step
// (2) (RESPOND's first turn and every resume, M4 task 4), step (3) (APPLY,
// M4 task 5, respond.go), step (4) PUBLISH when pr_url is still NULL, and
// step (5) POLL otherwise. Nothing before row 9 (8.5) or MERGE-ANSWER
// itself ever writes a "merge asked" marker, so an answered round of any
// other job or kind is a bug this reports loudly rather than guessing at.
func (h shipHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c, handled, err := postBuildPrelude(ctx, t, d, response.EscalationOriginShipping)
	if handled || err != nil {
		return c, err
	}

	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		round := rounds[0]
		kind, kindErr := newestQuestionKind(round)
		if kindErr != nil {
			return store.HandlerCommit{}, kindErr
		}
		if kind == response.QuestionKindMerge {
			return h.mergeAnswer(ctx, t, d, round)
		}
		if kind != response.QuestionKindQuestion || round.Job != jobRespondName {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: unexpected answered round (job %q kind %q)", round.Job, kind)
		}
		return h.resumeRespondAnswered(ctx, t, d, round)
	}

	if commit, handled2, err2 := h.enterRespondBatch(ctx, t, d); handled2 || err2 != nil {
		return commit, err2
	}

	respondRows, err := d.Store.RespondBatches(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond batches: %w", err)
	}
	a, hasApply, applyErr := applyArtifact(ctx, t, d, respondRows)
	if applyErr != nil {
		return store.HandlerCommit{}, applyErr
	}
	if hasApply {
		return h.apply(ctx, t, d, a)
	}

	if t.PRURL == nil {
		return h.publish(ctx, t, d)
	}
	return h.poll(ctx, t, d)
}

// The design 8.2 step 2 and step 4 escalation What texts, byte for byte
// from the plan; step 1 reuses noStoredPlanWhat, worktreeNotPreparedWhat,
// branchUnrecordedWhat, and treeDirtyBeforeReviewWhat directly, the same
// cross-state reuse judgeStartChecks (judging.go) already gives those four
// (reviewing.go's own comment on treeDirtyBeforeReviewWhat: the message is
// the same regardless of which state's own step hit it). Why is this
// file's own plain-sentence gloss on each one: the plan gives no exact Why
// for any of these, only What (and, for the PR-open failures, Tried).
const (
	judgeNotPassedWhat = "the judge has not passed this commit"
	judgeNotPassedWhy  = "PUBLISH opens a pull request only for a commit a judge round has actually passed"

	judgeRoundNamedTwiceWhy = "a round's own pass marker must name exactly one round, or PUBLISH cannot tell which round's verdicts to render"

	verdictMissingWhy = "every scenario in the sealed cohort needs its own verdict before PUBLISH can render the pull request's scenario table"
	verdictUnknownWhy = "a verdict for a scenario outside the sealed cohort means the judge round and the cohort have drifted apart"

	pushFailedWhat = "the ticket branch could not be pushed"
	pushFailedWhy  = "OpenDraftPR could not push the branch before it could open the pull request"

	prOpenRefusedWhat = "GitHub refused to open the pull request"
	prOpenRefusedWhy  = "OpenDraftPR's own create and find calls both need a token GitHub accepts and a repository it can see"
)

// publish is PUBLISH (design section 8.2): the ROUND 1-4 checks, origin
// shipping (step 1, publishChecks); the final verdict set a passing judge
// round and the sealed cohort together pick out (step 2, finalVerdicts);
// the pull request's own body (step 3, prBody) and OpenDraftPR, which
// pushes, creates the draft, and falls back to the PR already open at this
// head after a create failure (Package 5); PUBLISH's own three error
// shapes (step 4, publishOpenDraftPRFailure); the tracker's PR-link
// comment, posted before PUBLISH's own commit and skipped when already
// posted (step 5, design section 11); and the commit itself (step 6).
func (h shipHandler) publish(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	proj, wt, plan, escalation, err := publishChecks(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, judgeRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: judge round markers: %w", err)
	}
	cohort, err := judgeScenariosFor(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	verdictRows, err := d.Store.Verdicts(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: verdicts: %w", err)
	}
	sha, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: head sha: %w", err)
	}

	final, what, why := finalVerdicts(markers, cohort, verdictRows, sha)
	if what != "" {
		return shipEscalation(t, d, what, why, ""), nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: build reports: %w", err)
	}
	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: file events: %w", err)
	}

	pr := prBody(t, plan, final, cohort, reports, events)

	url, number, openErr := proj.Orch.OpenDraftPR(ctx, wt, pr)
	if openErr != nil {
		return publishOpenDraftPRFailure(t, d, openErr)
	}

	if postErr := d.Tracker.PostPRLink(ctx, t.ProjectID, t.TrackerRef, url); postErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: post pr link: %w", postErr)
	}

	c := baseCommit(t, d)
	c.SetPRURL = &url
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("pr opened %d", number),
	}}
	c.ClearPoll = true
	return c, nil
}

// publishChecks is PUBLISH's own step 1 (design section 8.2): ROUND's own
// steps 1 to 4 (section 6.2), origin shipping -- the same four checks
// judgeStartChecks (judging.go) runs for "judging", with its own plan
// returned too (publish's own prBody call needs it, so this reads
// StoredPlan once rather than twice). escalation is non-nil, with every
// other return zeroed, the moment any check fails; a nil escalation with a
// nil error carries a real proj and wt the caller can build on.
func publishChecks(ctx context.Context, t store.Ticket, d Deps) (proj Project, wt orchestrator.Worktree, plan response.Plan, escalation *store.HandlerCommit, err error) {
	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, response.Plan{}, nil, fmt.Errorf("job: shipping: stored plan: %w", err)
	}
	if !havePlan {
		c := shipEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, "")
		return Project{}, orchestrator.Worktree{}, response.Plan{}, &c, nil
	}

	proj, wt, escCommit, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return shipEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return Project{}, orchestrator.Worktree{}, response.Plan{}, nil, err
	}
	if escCommit != nil {
		return Project{}, orchestrator.Worktree{}, response.Plan{}, escCommit, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, response.Plan{}, nil, fmt.Errorf("job: shipping: build reports: %w", err)
	}
	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, response.Plan{}, nil, fmt.Errorf("job: shipping: branch commits: %w", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		c := withBranch(shipEscalation(t, d, branchUnrecordedWhat, branchUnrecordedWhy, ""), wt)
		return Project{}, orchestrator.Worktree{}, response.Plan{}, &c, nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, response.Plan{}, nil, fmt.Errorf("job: shipping: changed paths: %w", err)
	}
	if len(changed) > 0 {
		quoted := make([]string, len(changed))
		for i, ch := range changed {
			quoted[i] = strconv.Quote(ch.Path)
		}
		tried := strings.Join(quoted, ", ")
		c := withBranch(shipEscalation(t, d, treeDirtyBeforeReviewWhat, treeDirtyBeforeReviewWhy, tried), wt)
		return Project{}, orchestrator.Worktree{}, response.Plan{}, &c, nil
	}

	return proj, wt, plan, nil, nil
}

// judgeRoundPassedLine is "judge round <n> passed", EVALUATE's own terminal
// marker (judging.go's judgeRoundMarkerPrefix family; "passed" is the one
// shape judging.go itself has no regex for, since EVALUATE's own code never
// needs to parse it back -- PUBLISH is its only reader).
var judgeRoundPassedLine = regexp.MustCompile(`^judge round ([1-9]\d*) passed$`)

// judgeRoundPassed returns the round of the newest "judge round <n> passed"
// marker among markers, and judgeNotPassedWhat/Why when none exists, or
// "judge round <n> passed twice" when markers carries more than one passed
// marker naming that same round.
func judgeRoundPassed(markers []store.MessageRow) (n int, what, why string) {
	var rounds []int
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := judgeRoundPassedLine.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		roundN, convErr := strconv.Atoi(sub[1])
		if convErr != nil {
			continue
		}
		rounds = append(rounds, roundN)
	}
	if len(rounds) == 0 {
		return 0, judgeNotPassedWhat, judgeNotPassedWhy
	}

	n = rounds[len(rounds)-1]
	count := 0
	for _, r := range rounds {
		if r == n {
			count++
		}
	}
	if count > 1 {
		return n, fmt.Sprintf("judge round %d passed twice", n), judgeRoundNamedTwiceWhy
	}
	return n, "", ""
}

// finalVerdicts is design section 8.2 step 2: the sealed cohort's own final
// verdict set, read from the judge round that actually passed, not from
// whichever round ran last. n is judgeRoundPassed's own round. For each
// scenario of cohort, in cohort order, the newest verdict row (by artifact
// id -- Verdicts' own append-only ORDER BY artifacts.id, judgeNewestVerdict)
// of round n for that scenario id wins: a check override row
// (judgerules.go's applyCheckExit) is always newer than the judge's own
// row, so it wins without any special case here. A round n row naming a
// scenario outside cohort is an error too. Every selected row's SHA must
// equal headSHA, PUBLISH's own proof that the judge passed this exact
// commit, not an earlier one since superseded. what is "" on success; any
// other value is the escalation's own What, paired with why.
func finalVerdicts(markers []store.MessageRow, cohort []response.Scenario, verdicts []store.VerdictRow, headSHA string) (final []response.VerdictArtifact, what, why string) {
	n, roundWhat, roundWhy := judgeRoundPassed(markers)
	if roundWhat != "" {
		return nil, roundWhat, roundWhy
	}

	inCohort := make(map[string]bool, len(cohort))
	for _, sc := range cohort {
		inCohort[sc.ID] = true
	}
	for i := range verdicts {
		if verdicts[i].Verdict.Round == n && !inCohort[verdicts[i].Verdict.Scenario] {
			return nil, "the passing judge round has a verdict for unknown scenario " + verdicts[i].Verdict.Scenario, verdictUnknownWhy
		}
	}

	final = make([]response.VerdictArtifact, 0, len(cohort))
	for _, sc := range cohort {
		row, ok := judgeNewestVerdict(verdicts, n, sc.ID)
		if !ok {
			return nil, "the passing judge round has no verdict for scenario " + sc.ID, verdictMissingWhy
		}
		if row.Verdict.SHA != headSHA {
			return nil, judgeNotPassedWhat, judgeNotPassedWhy
		}
		final = append(final, row.Verdict)
	}
	return final, "", ""
}

// publishOpenDraftPRFailure is PUBLISH's own step 4 (design section 8.2):
// ErrGitHubAuth or ErrGitHubNotFound (GitHub itself refused the create and
// find calls OpenDraftPR made) escalates; ErrGitHubUnavailable or a
// RateLimitedError returns the error so the dispatcher releases the claim
// and the next tick tries again; anything else -- OpenDraftPR's own push
// step failing, which carries neither sentinel -- escalates as a push
// failure.
func publishOpenDraftPRFailure(t store.Ticket, d Deps, openErr error) (store.HandlerCommit, error) {
	if errors.Is(openErr, orchestrator.ErrGitHubAuth) || errors.Is(openErr, orchestrator.ErrGitHubNotFound) {
		return shipEscalation(t, d, prOpenRefusedWhat, prOpenRefusedWhy, openErr.Error()), nil
	}
	if errors.Is(openErr, orchestrator.ErrGitHubUnavailable) {
		return store.HandlerCommit{}, openErr
	}
	if rle, ok := errors.AsType[orchestrator.RateLimitedError](openErr); ok {
		return store.HandlerCommit{}, rle
	}
	return shipEscalation(t, d, pushFailedWhat, pushFailedWhy, openErr.Error()), nil
}

// shipEscalationCode is escalationCommit (planning.go) plus this file's own
// "escalation written" log (design section 11), for every escalation
// shipping writes under any code: origin is always "shipping", and RunID
// and SessionID are both nil -- neither PUBLISH nor POLL ever makes a
// runtime call of its own.
func shipEscalationCode(t store.Ticket, d Deps, code, what, why, tried string) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginShipping))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginShipping)
}

// shipEscalation is shipEscalationCode under code "environment", every
// environment escalation PUBLISH and POLL write.
func shipEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	return shipEscalationCode(t, d, string(response.EscalationCodeEnvironment), what, why, tried)
}

// shipLoopsExhausted is shipEscalationCode under code "loops_exhausted"
// (design section 8.7): the shared shipping counter's own gate, mirroring
// judgeLoopsExhausted (judging.go) and reviewing.go's own equivalent.
func shipLoopsExhausted(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	return shipEscalationCode(t, d, string(response.EscalationCodeLoopsExhausted), what, why, tried)
}

// -----------------------------------------------------------------------
// POLL (design section 8.3 to 8.9, task 7)
// -----------------------------------------------------------------------

// reasonMerged is POLL's own DONE transition reason (design section 8.6
// step 3), distinct from skeleton.go's deleted reasonShipped: the plan
// pins this literal exactly, "merged", because DONE fires whoever merged
// the pull request, not only a merge Zing itself made (M4).
const reasonMerged = "merged"

// jobRespondName is jobs.respond's own machine.toml key (design D14,
// section 8.7): the shared shipping counter's own limit is
// jobs.respond.max_loops, not a shipping-specific one, because a CI fix
// and a review-thread fix draw from the same budget.
const jobRespondName = string(response.JobRespond)

// pollTreeDirtyWhat and pollTreeDirtyWhy are POLL's own step 1 dirty-tree
// row (design section 8.3): the literal What text there, "the worktree has
// uncommitted changes", is not treeDirtyBeforeReviewWhat's own "... before
// review" -- POLL runs long after review, so reusing that text would be
// wrong, not just differently worded (PKG9-PLAN.md line 1202 vs 724-725).
const (
	pollTreeDirtyWhat = "the worktree has uncommitted changes"
	pollTreeDirtyWhy  = "POLL reads git state from the ticket's own worktree, and a dirty tree means something changed it outside Zing's own commits"

	ghTokenRefusedWhat = "GitHub refused the token"                                                                //nolint:gosec // not a credential: an escalation's own display text, not a token value
	ghTokenRefusedWhy  = "every POLL read uses the same token; once GitHub refuses it, every later read would too" //nolint:gosec // not a credential: an escalation's own display text, not a token value

	prGoneWhat = "the pull request no longer exists"
	prGoneWhy  = "GitHub returned not-found for the pull request PUBLISH opened"

	foreignHeadWhat = "the pull request head is not the ticket branch"
	foreignHeadWhy  = "someone pushed to the pull request branch or rewrote it"

	unprotectedWhat = "the base branch requires no status check"
	unprotectedWhy  = "Zing merges only behind a required check; zing project add confirmed ci"

	prClosedWhat = "the pull request was closed without merging"
)

// fixRequestedCILogPrefix and fixRequestedThreadsPrefix are the two marker
// families design section 8.7's shared shipping counter counts: every
// fixRequestMessage call under FixKindCILog or FixKindThreads writes a
// marker starting with one of these two exact prefixes.
const (
	fixRequestedCILogPrefix   = "fix requested ci_log after run "
	fixRequestedThreadsPrefix = "fix requested threads after run "
)

// ciWaitingPrefix is the informational marker design section 8.4 writes
// whenever an idle poll's own Missing set changes from the previous poll's.
const ciWaitingPrefix = "ci waiting "

// prDraftPrefix and prReadyPrefix are design section 5.1's own
// informational markers for the draft/ready flip (design section 8.5 rows
// 3 and 8, 8.9): no decision ever reads either back.
const (
	prDraftPrefix = "pr draft "
	prReadyPrefix = "pr ready "
)

// threadsBlockingPrefix is design section 5.1's own "threads blocking "
// marker (8.5 row 6a): the set of unclassified thread tids currently
// blocking the ready flip and the merge gate (9.1).
const threadsBlockingPrefix = "threads blocking "

// mergeMarkerPrefix groups every "merge " marker design section 8.8 writes
// for one pull request head: "asked" (row 9, M4 task 8), "withdrawn" (rows
// 3 to 5, this file), "held" (MERGE-ANSWER, M4 task 8), "refused" (MERGE,
// M4 task 8), and "retry" (MERGE's own "Base branch was modified" row).
// Row 3 (below) is the one row task 7 builds that reads this family back: a
// loop that just reopened on a head GitHub may still be showing a stale
// merge question for.
const mergeMarkerPrefix = "merge "

// mergeMarkerLine matches one "merge " marker's own first line: its kind
// and the sha it names.
var mergeMarkerLine = regexp.MustCompile(`^merge (asked|withdrawn|held|refused|retry) ([0-9a-f]{40})$`)

// newestMergeMarkerKind returns the kind ("asked", "withdrawn", "held",
// "refused", or "retry") of the newest "merge <kind> <sha>" marker among
// markers that names exactly sha, "" when none does (design section 8.9:
// "this head's own newest merge marker"). markers is MarkersWithPrefix's
// own oldest-first order, so the last match is the newest.
func newestMergeMarkerKind(markers []store.MessageRow, sha string) string {
	kind := ""
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := mergeMarkerLine.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		if sub[2] != sha {
			continue
		}
		kind = sub[1]
	}
	return kind
}

// withdrawMergeQuestionIfAsked appends a "merge withdrawn <sha>" marker to
// c, and resolves every open or answered question on the ticket, when
// mergeMarkers' own newest "merge ..." marker for sha is "asked" or "held"
// (design section 8.5 rows 3 to 5, 8.9): the loop just reopened on a head
// GitHub may still be showing a stale merge question for, so it is
// withdrawn in the same commit that notices the reopen -- "so the head can
// be asked about again once the loop is clean". A pending "merge retry"
// marker is withdrawn too, so a reopened loop never ends in a merge nobody
// re-approved. c.ResolveAll is safe to set unconditionally here because
// shipping's own POLL never asks any other kind of question (pollDone,
// above, resolves every question the same way).
func withdrawMergeQuestionIfAsked(c store.HandlerCommit, t store.Ticket, mergeMarkers []store.MessageRow, number int, sha string) store.HandlerCommit {
	kind := newestMergeMarkerKind(mergeMarkers, sha)
	switch kind {
	case mergeMarkerAsked, mergeMarkerHeld, mergeMarkerRetry:
	default:
		return c
	}
	if kind == mergeMarkerRetry {
		slog.Info("merge retry withdrawn", "ticket_id", t.ID, "pr", number, "head_sha", sha)
	}
	c.ResolveAll = true
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: "merge withdrawn " + sha,
	})
	return c
}

// poll is POLL (design section 8.3 to 8.9): step 1's own three checks
// (pollChecks), the PR read and its own three terminal rows (merged,
// closed, a foreign or behind head), the check and status reads, and --
// since M3 builds neither the ready flip nor MERGE (8.5 rows 1 to 3, 5, 6,
// 6a, 8, and 9 are M4's) -- row 4 (CI failed) or an idle poll for anything
// else, pending or green alike.
func (h shipHandler) poll(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	proj, wt, escCommit, err := pollChecks(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escCommit != nil {
		return *escCommit, nil
	}

	local, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: head sha: %w", err)
	}
	number, err := parsePRNumber(*t.PRURL)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: %w", err)
	}

	pr, err := proj.PullRequests.GetPR(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		if c, handled := pollReadFailure(t, d, true, err); handled {
			return c, nil
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: get pr: %w", err)
	}

	if pr.Merged {
		return h.pollDone(ctx, t, d, *t.PRURL, pr.MergeCommitSHA)
	}
	if pr.State == "closed" {
		return h.pollClosed(t, d, number), nil
	}

	if pr.HeadSHA != local {
		return h.pollHeadMismatch(ctx, t, d, proj, wt, pr, local)
	}

	// GitHub builds no merge for a conflicting pull request, so CI never
	// starts; waiting on it would wait forever.
	if pr.MergeableState == mergeableStateDirty {
		return h.pollConflict(ctx, t, d, proj, wt, pr, number)
	}
	// GitHub refused this head's merge because main moved, and a strict
	// branch rule now reports it behind: merge main in rather than ask.
	if pr.MergeableState == mergeableStateBehind {
		mergeMarkers, markerErr := d.Store.MarkersWithPrefix(ctx, t.ID, mergeMarkerPrefix)
		if markerErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: merge markers: %w", markerErr)
		}
		if newestMergeMarkerKind(mergeMarkers, pr.HeadSHA) == mergeMarkerRetry {
			return h.pollConflict(ctx, t, d, proj, wt, pr, number)
		}
	}

	runs, err := proj.Checks.ListCheckRuns(ctx, proj.Owner, proj.Repo, local)
	if err != nil {
		if c, handled := pollReadFailure(t, d, false, err); handled {
			return c, nil
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: list check runs: %w", err)
	}
	statuses, err := proj.Checks.ListStatuses(ctx, proj.Owner, proj.Repo, local)
	if err != nil {
		if c, handled := pollReadFailure(t, d, false, err); handled {
			return c, nil
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: list statuses: %w", err)
	}
	required, err := proj.Checks.RequiredCheckRules(ctx, proj.Owner, proj.Repo, pr.BaseRef)
	if err != nil {
		if c, handled := pollReadFailure(t, d, false, err); handled {
			return c, nil
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: required check rules: %w", err)
	}
	threadsRaw, err := proj.Threads.ListThreads(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		if c, handled := pollReadFailure(t, d, false, err); handled {
			return c, nil
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: list threads: %w", err)
	}
	login, err := proj.Threads.Viewer(ctx)
	if err != nil {
		if c, handled := pollReadFailure(t, d, false, err); handled {
			return c, nil
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: viewer: %w", err)
	}

	// Rows 1 and 2 of design section 8.5 (M4 task 6, respond.go): a past
	// respond batch's own collected fix landed and is now in this head
	// (FIX-REPLIES), or some fix landed since the pull request opened and
	// this head's own stale reviewers have not been re-requested yet
	// (RE-REQUEST). Both outrank every row below, including CIUnprotected's
	// own escalation: neither reads CI at all, and a thread already fixed or
	// a stale reviewer is worth acting on whatever this tick's own CI result
	// turns out to be.
	respondRows, err := d.Store.RespondBatches(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: respond batches: %w", err)
	}
	fixA, fixSHA, fixOK, fixErr := h.fixRepliesPending(ctx, t, d, proj, wt, respondRows, pr.HeadSHA)
	if fixErr != nil {
		return store.HandlerCommit{}, fixErr
	}
	if fixOK {
		return h.fixReplies(ctx, t, d, proj, wt, threadsRaw, login, fixA, fixSHA)
	}
	landedSincePROpened, err := fixLandedAfterPROpened(ctx, t, d, number)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if landedSincePROpened {
		_, reRequested, markerErr := d.Store.Marker(ctx, t.ID, reRequestedMarkerFor(pr.HeadSHA))
		if markerErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: reviewers re-requested marker: %w", markerErr)
		}
		if !reRequested {
			return h.reRequest(ctx, t, d, proj, number, pr.HeadSHA, login)
		}
	}

	// classifyThreads and pollThreadsFrom are threadrules.go's and respond.go's
	// own pure helpers (design section 9.1, 8.3): every class feeds the
	// fingerprint (pollThreadsFrom). Row 5 (M4 task 4) drives off actionable
	// threads; rows 6 and 6a (this task) drive off leftover and unclassified
	// ones; row 3 (this task) and row 9 (M4 task 8) both read whether any
	// thread of any class is still unresolved. After readyCycleCap
	// thread-caused cycles (respond.go, skipThreadDraftFlip), row 3 flips
	// only for a failed CI.
	_, actionable, leftover, unclassified := classifyThreads(threadsRaw, login)
	pollThreads := pollThreadsFrom(threadsRaw)

	fp := pollFingerprint(pr, runs, statuses, required, pollThreads)
	result := prCI(pr, runs, statuses, required)
	anyUnresolved := len(actionable) > 0 || len(leftover) > 0 || len(unclassified) > 0
	skipFlip, err := skipThreadDraftFlip(ctx, t, d, pr, number, result.State == CIFailed, anyUnresolved)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	var rd rerunDecision
	if result.State == CIFailed {
		rd, err = h.ciRerunDecision(ctx, t, d, proj, result, runs, local)
		if err != nil {
			return store.HandlerCommit{}, err
		}
	}

	notes, err := h.rerunNotesFor(ctx, t, d, runs, local)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	c, err := h.pollRoute(ctx, t, d, proj, wt, pr, number, local, fp, result, rd, actionable, leftover, unclassified, login, anyUnresolved, skipFlip)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	c.Messages = append(c.Messages, notes...)
	return c, nil
}

// pollRoute is poll's own row switch (design section 8.5): CIUnprotected's
// escalation; the re-run rows a failed check's own rerunDecision picks
// between (rerunNow, rerunEscalate, rerunWait -- all three ahead of the
// draft flip, design Q4: a ready pull request stays ready while its one
// re-run is pending, and only a fix request that actually follows flips it
// to draft); the draft flip itself; the ci_log fix request once rd.Action
// is rerunFix; and every row below unchanged from before this task.
func (h shipHandler) pollRoute(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, pr orchestrator.PRState, number int, local, fp string, result CIResult, rd rerunDecision, actionable, leftover, unclassified []orchestrator.Thread, login string, anyUnresolved, skipFlip bool) (store.HandlerCommit, error) {
	switch {
	case result.State == CIUnprotected:
		c := shipEscalation(t, d, unprotectedWhat, unprotectedWhy, "")
		c.ClearPoll = true
		return c, nil
	case result.State == CIFailed && rd.Action == rerunNow:
		return h.pollRerun(ctx, t, d, proj, fp, rd.Reruns)
	case result.State == CIFailed && rd.Action == rerunEscalate:
		c := shipEscalation(t, d, rd.What, rd.Why, rd.Tried)
		c.ClearPoll = true
		return c, nil
	case result.State == CIFailed && rd.Action == rerunWait:
		return h.pollIdle(ctx, t, d, proj, pr, number, fp, result.Missing)
	case !pr.Draft && (result.State == CIFailed || anyUnresolved) && !skipFlip:
		return h.pollConvertToDraft(ctx, t, d, proj, pr, number, local)
	case result.State == CIFailed:
		return h.pollCIFailed(ctx, t, d, rd.Text)
	case len(actionable) > 0:
		return h.startRespondBatch(ctx, t, d, local, actionable, login)
	case len(leftover) > 0:
		return h.pollLeftover(ctx, t, d, proj, leftover)
	case len(unclassified) > 0:
		return h.pollUnclassifiedBlocking(ctx, t, d, fp, unclassified)
	case pr.Draft && result.State == CIPending && result.ReportedGreen:
		// Only checks that have not reported remain; some (AI reviewers)
		// run only once the pull request is ready, so mark it ready now.
		return h.pollMarkReady(ctx, t, d, proj, pr, local)
	case result.State == CIPending:
		return h.pollIdle(ctx, t, d, proj, pr, number, fp, result.Missing)
	case pr.Draft: // CIGreen, zero unresolved threads, still draft: row 8
		return h.pollMarkReady(ctx, t, d, proj, pr, local)
	default: // CIGreen, zero unresolved threads, not draft: row 9 (MERGE or the merge question, M4 task 8).
		return h.pollMergeGate(ctx, t, d, proj, wt, number, local, fp)
	}
}

// checkRerunEvents reads every check_rerun event on sha and decodes it,
// keeping each row's id and CreatedAt: ciRerunDecision needs both (the id
// against the retry marker, the time against rerunAppearWait), and
// rerunNotesFor needs neither but shares the same read and decode.
func checkRerunEvents(ctx context.Context, d Deps, ticketID int64, sha string) ([]priorRerun, error) {
	rows, err := d.Store.Events(ctx, ticketID, store.EventKindCheckRerun, store.EventFilter{SHA: sha})
	if err != nil {
		return nil, fmt.Errorf("job: shipping: poll: check_rerun events: %w", err)
	}
	out := make([]priorRerun, 0, len(rows))
	for i := range rows {
		var ev response.CheckRerunEvent
		if err := json.Unmarshal(rows[i].Payload, &ev); err != nil {
			return nil, fmt.Errorf("job: shipping: poll: check_rerun event %d: %w", rows[i].ID, err)
		}
		at := time.Time{}
		if rows[i].CreatedAt != nil {
			at = rows[i].CreatedAt.UTC()
		}
		out = append(out, priorRerun{Event: ev, ID: rows[i].ID, At: at})
	}
	return out, nil
}

// ciRerunDecision reads the failed checks' own facts and logs
// (readFailedChecks), this sha's own check_rerun events written since the
// newest "retry requested" marker (Retry resets every check's own budget,
// #91's Q1), the workflow runs still in flight (workflowRunsInFlight), and
// returns decideCIRerun's verdict with Text set to ciLogTextFrom's text --
// computed here, once, so both the fix row and a loops_exhausted
// escalation built from it (pollCIFailed) see the same text this tick
// decided on.
func (h shipHandler) ciRerunDecision(ctx context.Context, t store.Ticket, d Deps, proj Project, result CIResult, runs []orchestrator.CheckRun, sha string) (rerunDecision, error) {
	failed := readFailedChecks(ctx, proj.Checks, proj.Owner, proj.Repo, result.FailedRuns)
	text := ciLogTextFrom(failed, result.FailedStatuses)
	for i := range failed {
		if fc := &failed[i]; fc.LogErr != nil {
			slog.Warn("ci check log unreadable", "ticket_id", t.ID, "check", fc.Run.Name, "sha", sha, "workflow_run_id", fc.RunID, "job_id", fc.JobID, "error", fc.LogErr)
		}
	}

	retryRow, hasRetry, err := d.Store.Marker(ctx, t.ID, markerRetryRequested)
	if err != nil {
		return rerunDecision{}, fmt.Errorf("job: shipping: poll: retry requested marker: %w", err)
	}

	rows, err := checkRerunEvents(ctx, d, t.ID, sha)
	if err != nil {
		return rerunDecision{}, err
	}
	prior := make([]priorRerun, 0, len(rows))
	for _, p := range rows {
		if hasRetry && p.ID <= retryRow.ID {
			continue
		}
		prior = append(prior, p)
	}

	rd := decideCIRerun(time.Now().UTC(), sha, failed, len(result.FailedStatuses), prior, workflowRunsInFlight(runs))
	rd.Text = text
	return rd, nil
}

// ciRerunAPIErrorWhat is pollRerun's own escalation What, once RerunJob
// itself fails with neither a rate limit nor an unavailable GitHub.
const ciRerunAPIErrorWhat = "Zing could not re-run a failed CI check"

// pollRerun re-runs every planned job in order (design shape, "Per-check
// rule"): RerunJob first, then the check_rerun event recording it, so a
// crash between the two never records a re-run GitHub never made. Only a
// transient read failure -- an unavailable GitHub, a rate limit, or a
// workflow run GitHub says is not complete yet
// (orchestrator.ErrWorkflowRunIncomplete; decideCIRerun's own per-tick
// dedupe and workflowRunsInFlight check narrow this to a race between
// reading CI and calling RerunJob, r2f1) -- reschedules, keeping the
// check_rerun events already written this tick attached to the commit --
// a re-run GitHub did make before the one that failed must not go
// unrecorded, or the next tick's budget check would count it as never
// having happened and could re-run the same job a second time. Any other
// RerunJob failure, a refused token included (r3f2: the plan's own risk,
// a token with read but not actions-write access, is exactly this case,
// and the generic "GitHub refused the token" escalation pollReadFailure
// would otherwise give it names no check and no job), escalates instead
// with ciRerunAPIErrorWhat, carrying those same events.
func (h shipHandler) pollRerun(ctx context.Context, t store.Ticket, d Deps, proj Project, fp string, plan []plannedRerun) (store.HandlerCommit, error) {
	written := make([]store.Message, 0, len(plan))
	for _, p := range plan {
		if err := proj.Checks.RerunJob(ctx, proj.Owner, proj.Repo, p.JobID); err != nil {
			if errors.Is(err, orchestrator.ErrWorkflowRunIncomplete) || errors.Is(err, orchestrator.ErrGitHubUnavailable) {
				slog.Info("ci check re-run deferred", "ticket_id", t.ID, "check", p.Event.Check, "sha", p.Event.SHA, "workflow_run_id", p.Event.RunID, "job_id", p.JobID, "reruns_recorded", len(written), "error", err)
				c := pollScheduleOnly(t, d, time.Time{})
				c.Messages = written
				return c, nil
			}
			if rle, ok := errors.AsType[orchestrator.RateLimitedError](err); ok {
				slog.Info("ci check re-run deferred", "ticket_id", t.ID, "check", p.Event.Check, "sha", p.Event.SHA, "workflow_run_id", p.Event.RunID, "job_id", p.JobID, "reruns_recorded", len(written), "error", err)
				c := pollScheduleOnly(t, d, rle.ResetAt)
				c.Messages = written
				return c, nil
			}
			why := fmt.Sprintf("re-running %s (job %d) on %s failed: %s", p.Event.Check, p.JobID, shortSHA(p.Event.SHA), err)
			c := shipEscalation(t, d, ciRerunAPIErrorWhat, why, "")
			c.Messages = written
			c.ClearPoll = true
			return c, nil
		}

		msg, msgErr := store.NewEvent(t.ID, store.EventKindCheckRerun, p.Event)
		if msgErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: check_rerun event: %w", msgErr)
		}
		written = append(written, msg)
		slog.Info("ci check re-run", "ticket_id", t.ID, "check", p.Event.Check, "sha", p.Event.SHA, "workflow_run_id", p.Event.RunID, "check_run_id", p.Event.CheckRunID, "job_id", p.JobID, "reason", p.Event.Reason)
	}

	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)

	c := baseCommit(t, d)
	c.Waiting = t.WaitingOn
	c.Messages = written
	c.Poll = &store.PollUpdate{NextAt: next, IntervalS: iv, Fingerprint: fp}
	return c, nil
}

// rerunNotesFor reads every check_rerun and check_rerun_passed event on
// sha and calls rerunPassedNotes (design shape, "rerunPassedNotes"): every
// POLL tick, not only a CI-failed one, since a check that was re-run on an
// earlier tick can pass on any later one, whichever row that tick's own
// CI result otherwise routes to.
func (h shipHandler) rerunNotesFor(ctx context.Context, t store.Ticket, d Deps, runs []orchestrator.CheckRun, sha string) ([]store.Message, error) {
	rerunRows, err := checkRerunEvents(ctx, d, t.ID, sha)
	if err != nil {
		return nil, err
	}
	reruns := make([]response.CheckRerunEvent, len(rerunRows))
	for i, p := range rerunRows {
		reruns[i] = p.Event
	}

	passedRows, err := d.Store.Events(ctx, t.ID, store.EventKindCheckRerunPassed, store.EventFilter{SHA: sha})
	if err != nil {
		return nil, fmt.Errorf("job: shipping: poll: check_rerun_passed events: %w", err)
	}
	passed := make([]response.CheckRerunPassedEvent, 0, len(passedRows))
	for i := range passedRows {
		var ev response.CheckRerunPassedEvent
		if decodeErr := json.Unmarshal(passedRows[i].Payload, &ev); decodeErr != nil {
			return nil, fmt.Errorf("job: shipping: poll: check_rerun_passed event %d: %w", passedRows[i].ID, decodeErr)
		}
		passed = append(passed, ev)
	}

	notes, err := rerunPassedNotes(t.ID, sha, runs, reruns, passed)
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// pollConvertToDraft is design section 8.5 row 3 and 8.9 (M4 task 7): the
// loop just reopened (CI failed, or any unresolved thread of any class) on
// a pull request still marked ready, whoever made it ready -- so Zing
// converts it back to draft, writes the informational "pr draft <sha>"
// marker, and, in the same commit, withdraws any merge question this
// exact head's own newest merge marker still shows as asked or held
// (withdrawMergeQuestionIfAsked).
func (h shipHandler) pollConvertToDraft(ctx context.Context, t store.Ticket, d Deps, proj Project, pr orchestrator.PRState, number int, sha string) (store.HandlerCommit, error) {
	if err := proj.Flips.ConvertToDraft(ctx, pr.NodeID); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: convert to draft: %w", err)
	}

	mergeMarkers, err := d.Store.MarkersWithPrefix(ctx, t.ID, mergeMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: merge markers: %w", err)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: prDraftPrefix + sha,
	}}
	c = withdrawMergeQuestionIfAsked(c, t, mergeMarkers, number, sha)
	c.ClearPoll = true
	return c, nil
}

// pollLeftover is LEFTOVER (design section 8.5 row 6, 9.5): every poll
// that finds a leftover thread resolves each of them, in tid order, and
// commits ClearPoll with no marker of its own -- the next poll reads the
// threads again, so a crash after a resolve converges, and someone who
// unresolves a thread Zing already answered, with no comment, sees it
// resolved again the next time; a comment is how to reopen the
// discussion (it becomes actionable instead).
func (h shipHandler) pollLeftover(ctx context.Context, t store.Ticket, d Deps, proj Project, leftover []orchestrator.Thread) (store.HandlerCommit, error) {
	sorted := append([]orchestrator.Thread(nil), leftover...)
	sort.Slice(sorted, func(i, j int) bool { return tid(sorted[i].ID) < tid(sorted[j].ID) })
	for _, th := range sorted {
		if err := proj.Threads.ResolveThread(ctx, th.ID); err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: resolve leftover thread: %w", err)
		}
	}

	c := baseCommit(t, d)
	c.ClearPoll = true
	return c, nil
}

// previousThreadsBlocking returns the tids the newest "threads blocking "
// marker on t carried (design section 5.1), nil when none exists or the
// newest one named none.
func previousThreadsBlocking(ctx context.Context, t store.Ticket, d Deps) ([]string, error) {
	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, threadsBlockingPrefix)
	if err != nil {
		return nil, fmt.Errorf("job: shipping: poll: threads blocking markers: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	tail := strings.TrimPrefix(rows[len(rows)-1].Body, threadsBlockingPrefix)
	if tail == "" {
		return nil, nil
	}
	return strings.Split(tail, ","), nil
}

// pollUnclassifiedBlocking is design section 8.5 row 6a: an idle commit
// (8.3's own backoff), plus the informational "threads blocking <tids>"
// marker whenever the unclassified set differs from the previous poll's
// own newest such marker. An unclassified thread blocks rows 8 and 9 (the
// ready flip and the merge question) until it changes class -- a human
// comment makes it actionable, or someone resolves it.
func (h shipHandler) pollUnclassifiedBlocking(ctx context.Context, t store.Ticket, d Deps, fp string, unclassified []orchestrator.Thread) (store.HandlerCommit, error) {
	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)

	c := baseCommit(t, d)
	c.Poll = &store.PollUpdate{NextAt: next, IntervalS: iv, Fingerprint: fp}

	tids := make([]string, len(unclassified))
	for i, th := range unclassified {
		tids[i] = tid(th.ID)
	}
	sort.Strings(tids)

	prev, err := previousThreadsBlocking(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if !slices.Equal(prev, tids) {
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: threadsBlockingPrefix + strings.Join(tids, ","),
		}}
	}
	return c, nil
}

// pollMarkReady is design section 8.5 row 8 and 8.9 (M4 task 7): CI green
// and zero unresolved threads of any class, on a pull request still
// marked draft -- Zing marks it ready for review and writes the
// informational "pr ready <sha>" marker.
func (h shipHandler) pollMarkReady(ctx context.Context, t store.Ticket, d Deps, proj Project, pr orchestrator.PRState, sha string) (store.HandlerCommit, error) {
	if err := proj.Flips.MarkReady(ctx, pr.NodeID); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: mark ready: %w", err)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: prReadyPrefix + sha,
	}}
	c.ClearPoll = true
	return c, nil
}

// pollChecks is POLL's own step 1 (design section 8.3): EnsureWorktree,
// every branch commit recorded (branchUnrecordedWhat/Why, reused from
// reviewing.go: the message is the same regardless of which state's own
// step hit it), and a clean tree (pollTreeDirtyWhat/Why, this file's own
// text -- not treeDirtyBeforeReviewWhat, see its own comment above).
// escalation is non-nil, with every other return zeroed, the moment a
// check fails; a nil escalation with a nil error carries a real proj and
// wt the caller can build on.
func pollChecks(ctx context.Context, t store.Ticket, d Deps) (proj Project, wt orchestrator.Worktree, escalation *store.HandlerCommit, err error) {
	proj, wt, escCommit, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return shipEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return Project{}, orchestrator.Worktree{}, nil, err
	}
	if escCommit != nil {
		return Project{}, orchestrator.Worktree{}, escCommit, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, nil, fmt.Errorf("job: shipping: poll: build reports: %w", err)
	}
	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, nil, fmt.Errorf("job: shipping: poll: branch commits: %w", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		c := withBranch(shipEscalation(t, d, branchUnrecordedWhat, branchUnrecordedWhy, ""), wt)
		c.ClearPoll = true
		return Project{}, orchestrator.Worktree{}, &c, nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return Project{}, orchestrator.Worktree{}, nil, fmt.Errorf("job: shipping: poll: changed paths: %w", err)
	}
	if len(changed) > 0 {
		c := withBranch(shipEscalation(t, d, pollTreeDirtyWhat, pollTreeDirtyWhy, ""), wt)
		c.ClearPoll = true
		return Project{}, orchestrator.Worktree{}, &c, nil
	}

	return proj, wt, nil, nil
}

// pollReadFailure classifies a GitHub read's own error during POLL (design
// section 8.3's "a read error" paragraph): ErrGitHubAuth escalates on any
// read; ErrGitHubNotFound escalates "the pull request no longer exists"
// only for forPR, the read that came directly off GetPR -- no other read
// has a named 404 row; ErrGitHubUnavailable and RateLimitedError end the
// poll with a schedule-only commit. handled is false, with the zero commit,
// when readErr is none of these: the caller wraps and propagates it as a
// bare internal error instead, so a read failure design 8.3 never names
// still fails loudly rather than silently rescheduling.
func pollReadFailure(t store.Ticket, d Deps, forPR bool, readErr error) (store.HandlerCommit, bool) {
	if errors.Is(readErr, orchestrator.ErrGitHubAuth) {
		c := shipEscalation(t, d, ghTokenRefusedWhat, ghTokenRefusedWhy, "")
		c.ClearPoll = true
		return c, true
	}
	if forPR && errors.Is(readErr, orchestrator.ErrGitHubNotFound) {
		c := shipEscalation(t, d, prGoneWhat, prGoneWhy, "")
		c.ClearPoll = true
		return c, true
	}
	if errors.Is(readErr, orchestrator.ErrGitHubUnavailable) {
		return pollScheduleOnly(t, d, time.Time{}), true
	}
	if rle, ok := errors.AsType[orchestrator.RateLimitedError](readErr); ok {
		return pollScheduleOnly(t, d, rle.ResetAt), true
	}
	return store.HandlerCommit{}, false
}

// failedInterval is design section 8.3's own schedule-only backoff: 30 when
// no interval is stored yet (a nil prevIntervalS, the first poll or the
// first poll to ever fail its reads), else the same doubling nextInterval's
// own "fingerprint unchanged" branch applies, min(2*prev, 300) -- there is
// no fresh fingerprint to compare against on a read failure, so the reset
// branch never applies here.
func failedInterval(prevIntervalS *int) int {
	if prevIntervalS == nil {
		return 30
	}
	return min(2*(*prevIntervalS), 300)
}

// pollScheduleOnly is design section 8.3's own schedule-only commit, for a
// read that failed with ErrGitHubUnavailable or RateLimitedError: the
// fingerprint column is left exactly as it is (NULL on a first poll), and
// the next poll is scheduled at max(now + iv, resetAt + 5s); resetAt's
// zero value drops out of that max whenever the failure carried no reset
// time (ErrGitHubUnavailable).
func pollScheduleOnly(t store.Ticket, d Deps, resetAt time.Time) store.HandlerCommit {
	iv := failedInterval(t.PollIntervalS)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)
	if atLeast := resetAt.Add(5 * time.Second); atLeast.After(next) {
		next = atLeast
	}

	c := baseCommit(t, d)
	c.PollSchedule = &store.PollSchedule{NextAt: next, IntervalS: iv}
	return c
}

// pollDone is DONE (design section 8.6): the tracker writes (PostDone, run
// through Deps.Tracker -- the dispatcher itself, PKG9-PLAN.md section 17.1)
// come first, the store commit last, so a crash before the commit repeats
// step 1 safely (PostDone's own marker guard) and a crash after it has
// nothing left to lose.
func (h shipHandler) pollDone(ctx context.Context, t store.Ticket, d Deps, prURL, mergeSHA string) (store.HandlerCommit, error) {
	if err := d.Tracker.PostDone(ctx, t.ProjectID, t.TrackerRef, prURL, mergeSHA); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: post done: %w", err)
	}

	c := baseCommit(t, d)
	c.Next, c.Reason = stateDone, reasonMerged
	c.ResolveAll = true
	c.ClearPoll = true
	return c, nil
}

// pollClosed is CLOSED (design section 8.6): escalate pr_closed; a retry
// (5.6's own shipping, pr_closed row) just polls again, so the loop
// continues if the owner reopened it, or escalates again if not.
func (h shipHandler) pollClosed(t store.Ticket, d Deps, number int) store.HandlerCommit {
	why := fmt.Sprintf("pull request #%d is closed", number)
	c := shipEscalationCode(t, d, string(response.EscalationCodePRClosed), prClosedWhat, why, "")
	c.ClearPoll = true
	return c
}

// pollHeadMismatch is design section 8.3 step 4: pr.HeadSHA and local
// disagree. An ancestor relationship means Zing's own branch simply moved
// on since the pull request last saw it, so Zing pushes again; otherwise a
// human pushed to or rewrote the pull request branch, which escalates
// (foreignHeadWhat/Why), since an unrecorded commit there is not Zing's.
func (h shipHandler) pollHeadMismatch(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, pr orchestrator.PRState, local string) (store.HandlerCommit, error) {
	anc, ancErr := proj.Orch.IsAncestor(ctx, wt, pr.HeadSHA, local)
	if ancErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: is ancestor: %w", ancErr)
	}
	if !anc {
		tried := fmt.Sprintf("pr head %s, ticket branch %s", pr.HeadSHA, local)
		c := shipEscalation(t, d, foreignHeadWhat, foreignHeadWhy, tried)
		c.ClearPoll = true
		return c, nil
	}

	if pushErr := proj.Orch.Push(ctx, wt); pushErr != nil {
		c := shipEscalation(t, d, pushFailedWhat, pushFailedWhy, pushErr.Error())
		c.ClearPoll = true
		return c, nil
	}

	c := baseCommit(t, d)
	c.ClearPoll = true
	return c, nil
}

// pollCIFailed is design section 8.5 row 4: the CI point first (overview
// design, owner's Q1 note) -- CI tests the pull request merged with the
// base, so a base that moved since this branch last merged it is merged in
// before anything else, with no shared-path condition, and a fix is asked
// for only once CI fails again on the merged head -- then the shared gate
// of 8.7, then a ci_log fix request carrying text -- ciRerunDecision's own
// ciLogTextFrom text, reached once a check's own flaky or no_log re-run
// budget is already spent (rd.Action rerunFix) -- or loops_exhausted when
// the gate is already at jobs.respond.max_loops (D14). Rows 5 and 6a's own
// merge-question withdrawal has nothing to withdraw in M3 (no code
// anywhere in this milestone ever writes a "merge asked" or "merge held"
// marker), so it is not built here.
func (h shipHandler) pollCIFailed(ctx context.Context, t store.Ticket, d Deps, text string) (store.HandlerCommit, error) {
	if c, opened, err := baseSync(ctx, t, d, syncPointCI); err != nil || opened {
		c.ClearPoll = opened
		return c, err
	}

	ciReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedCILogPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: ci_log fix requests: %w", err)
	}
	threadReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedThreadsPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: threads fix requests: %w", err)
	}
	k := len(ciReqs) + len(threadReqs)
	maxLoops := d.Machine.Jobs[jobRespondName].MaxLoops

	ok, what, why, tried := shippingGate(k, maxLoops, string(FixKindCILog), 0, text)
	if !ok {
		c := shipLoopsExhausted(t, d, what, why, tried)
		c.ClearPoll = true
		return c, nil
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: max run id: %w", err)
	}
	msg, msgErr := fixRequestMessage(t, FixKindCILog, text, maxRunID)
	if msgErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: fix request message: %w", msgErr)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{msg}
	c.ClearPoll = true
	return c, nil
}

// pollIdle is design section 8.5 row 7 (CI pending): the backoff commit of
// 8.3 (Poll{NextAt, IntervalS, Fingerprint}), plus the informational "ci
// waiting <names>" marker (design section 8.4) whenever Missing differs
// from the previous poll's own newest such marker. c.Waiting is carried
// forward from t.WaitingOn (M4 task 8, row 7's own "Waiting unchanged"):
// CommitHandlerResult's own ticket UPDATE writes waiting_on = c.Waiting
// unconditionally, nil included, so a commit that never touches it would
// otherwise silently clear a still-open merge question's own "merge"
// wait -- nil carries forward as nil, the ordinary case, so this changes
// nothing when nothing is waiting.
//
// Task 6 adds the review-bot nudge: on a non-draft pull request, each
// configured review-bot check still missing gets reviewBotAction's own
// verdict (shiprules.go) applied to its own clock markers on pr.HeadSHA.
// Every check's verdict is worked out first, before any side effect: if
// any one of them escalates, that escalation is returned in place of c,
// before CommentOnPR is called for any check, so a later check's
// escalation can never discard an earlier check's own already-decided
// start or nudge marker. Otherwise, in config order, start writes the
// missing marker, nudge posts the bot's trigger comment and writes the
// nudged marker, and wait leaves this tick's commit as it already is.
func (h shipHandler) pollIdle(ctx context.Context, t store.Ticket, d Deps, proj Project, pr orchestrator.PRState, number int, fp string, missing []string) (store.HandlerCommit, error) {
	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)

	c := baseCommit(t, d)
	c.Waiting = t.WaitingOn
	c.Poll = &store.PollUpdate{NextAt: next, IntervalS: iv, Fingerprint: fp}

	prevMissing, err := previousCIWaiting(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if !slices.Equal(prevMissing, missing) {
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: ciWaitingPrefix + strings.Join(missing, ","),
		}}
	}

	if pr.Draft {
		return c, nil
	}

	retryRow, hasRetry, err := d.Store.Marker(ctx, t.ID, markerRetryRequested)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: retry requested marker: %w", err)
	}

	now := time.Now().UTC()
	plans := make([]reviewBotPlan, 0, len(d.ReviewBots.Checks))
	for _, check := range d.ReviewBots.Checks {
		if !slices.Contains(missing, check.Check) {
			continue
		}

		missingHead := reviewBotMissingHead(pr.HeadSHA, check.Check)
		since, err := reviewBotMarkerSince(ctx, t, d, missingHead, retryRow.ID, hasRetry)
		if err != nil {
			return store.HandlerCommit{}, err
		}
		nudgedHead := reviewBotNudgedHead(pr.HeadSHA, check.Check)
		nudgedAt, err := reviewBotMarkerSince(ctx, t, d, nudgedHead, retryRow.ID, hasRetry)
		if err != nil {
			return store.HandlerCommit{}, err
		}

		action := reviewBotAction(now, since, nudgedAt, d.ReviewBots.Wait)
		if action == reviewBotEscalate {
			why := fmt.Sprintf("required check %s has not reported on %s since %s", check.Check, shortSHA(pr.HeadSHA), since.UTC().Format(time.RFC3339))
			tried := fmt.Sprintf("posted %s at %s", check.Trigger, nudgedAt.UTC().Format(time.RFC3339))
			ec := shipEscalation(t, d, reviewBotSilentWhat, why, tried)
			ec.ClearPoll = true
			return ec, nil
		}
		plans = append(plans, reviewBotPlan{check: check, action: action, missingHead: missingHead, nudgedHead: nudgedHead})
	}

	for _, p := range plans {
		switch p.action {
		case reviewBotStart:
			c.Messages = append(c.Messages, store.Message{
				TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
				Body: reviewBotMarkerBody(p.missingHead, now),
			})
		case reviewBotNudge:
			if err := proj.Threads.CommentOnPR(ctx, proj.Owner, proj.Repo, number, p.check.Trigger); err != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: comment on pr: %w", err)
			}
			slog.Info("review bot nudged", "ticket_id", t.ID, "check", p.check.Check)
			c.Messages = append(c.Messages, store.Message{
				TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
				Body: reviewBotMarkerBody(p.nudgedHead, now),
			})
		case reviewBotWait, reviewBotEscalate:
			// reviewBotWait: nothing to do this tick, the backoff commit
			// already built above stands. reviewBotEscalate never reaches
			// here: the loop above returns before appending it to plans.
		}
	}

	return c, nil
}

// reviewBotPlan is one configured check's own reviewBotAction verdict,
// worked out before any side effect (pollIdle, above): a later check's
// escalation must never discard an earlier check's already-decided start
// or nudge, so every check is decided first and only a non-escalating
// plan is applied, in config order.
type reviewBotPlan struct {
	check                   ReviewBotCheck
	action                  reviewBotStep
	missingHead, nudgedHead string
}

// reviewBotMarkerSince reads headKey's own newest marker (one of
// reviewBotMissingHead or reviewBotNudgedHead) and returns its own second
// line's time, straight to the pointer shape reviewBotAction takes: nil
// when there is none, or when it is older (by message id) than the newest
// "retry requested" marker on the ticket -- Retry restarts the review-bot
// clock (design "Shape").
func reviewBotMarkerSince(ctx context.Context, t store.Ticket, d Deps, headKey string, retryID int64, hasRetry bool) (*time.Time, error) {
	row, found, err := d.Store.Marker(ctx, t.ID, headKey)
	if err != nil {
		return nil, fmt.Errorf("job: shipping: poll: review bot marker %q: %w", headKey, err)
	}
	beforeRetry := hasRetry && row.ID < retryID
	if !found || beforeRetry {
		return nil, nil //nolint:nilnil // no marker (or one superseded by Retry) is a legitimate result, not an error
	}
	at, ok := reviewBotMarkerTime(row.Body)
	if !ok {
		return nil, nil //nolint:nilnil // a malformed marker body never happens in practice (reviewBotMarkerBody is its only writer); treated the same as no marker
	}
	return &at, nil
}

// previousCIWaiting returns the names the newest "ci waiting <names>"
// marker on t carried (design section 8.4), nil when none exists or the
// newest one named none.
func previousCIWaiting(ctx context.Context, t store.Ticket, d Deps) ([]string, error) {
	rows, err := d.Store.MarkersWithPrefix(ctx, t.ID, ciWaitingPrefix)
	if err != nil {
		return nil, fmt.Errorf("job: shipping: poll: ci waiting markers: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	tail := strings.TrimPrefix(rows[len(rows)-1].Body, ciWaitingPrefix)
	if tail == "" {
		return nil, nil
	}
	return strings.Split(tail, ","), nil
}

// shipRetryMarkerCommit is buildingHandler.retryMarkerCommit (building.go)
// plus ClearPoll: design section 5.6's shipping, pr_closed and shipping,
// any-other-code retry rows both write the plain "retry requested" marker
// and ClearPoll, unlike the shared retryMarkerCommit every other state's
// own escalation row reuses, since every shipping commit but an idle poll
// or the merge question sets ClearPoll (design section 8.1).
func shipRetryMarkerCommit(t store.Ticket, d Deps, resolveIDs []int64) store.HandlerCommit {
	c := buildingHandler{}.retryMarkerCommit(t, d, resolveIDs)
	c.ClearPoll = true
	return c
}

// shipThreadsAidLine matches "respond <aid>", the second line of a shared-
// gate Tried text under kind "threads" (shiprules.go's shippingGate).
var shipThreadsAidLine = regexp.MustCompile(`^respond (\d+)$`)

// retryShippingLoopsExhausted is design section 5.6's "shipping,
// loops_exhausted" retry row: Tried's own first line names the kind
// (shippingGate's own shape); for "threads" its second line names the
// respond artifact id the gate held open, and the rest is the fix text;
// for "ci_log" the rest, from line 2, is the fix text directly. The fix
// request is rebuilt from that text plus the owner's own notes and written
// with the gate skipped, exactly as 8.7 names it. For "threads" the same
// commit also writes "respond applied <aid>" with line 3 "fix request
// after run <R>" (the same R the fix request's own marker carries), so
// FIX-REPLIES (M4) later finds the request and the next tick's step (3)
// never writes a second one; its own line 2, "replied 0 fixing 1 skipped
// 0", is honest about what this commit itself did -- it posted no reply
// and skipped no thread, because it never contacted GitHub; it only wrote
// the one fix request APPLY had already decided on.
func (h shipHandler) retryShippingLoopsExhausted(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, tried string) (store.HandlerCommit, error) {
	kind, rest, ok := strings.Cut(tried, "\n")
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: loops_exhausted retry: malformed tried text %q", tried)
	}

	var aid int64
	requestText := rest
	if kind == string(FixKindThreads) {
		aidLine, afterAid, hasAfter := strings.Cut(rest, "\n")
		if !hasAfter {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: loops_exhausted retry: malformed tried text %q", tried)
		}
		sub := shipThreadsAidLine.FindStringSubmatch(aidLine)
		if sub == nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: loops_exhausted retry: malformed tried text %q", tried)
		}
		var convErr error
		aid, convErr = strconv.ParseInt(sub[1], 10, 64)
		if convErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: loops_exhausted retry: %w", convErr)
		}
		requestText = afterAid
	}

	text := requestText
	if notes != "" {
		text = strings.TrimRight(text, "\n") + "\n\n" + notes
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: loops_exhausted retry: max run id: %w", err)
	}
	msg, msgErr := fixRequestMessage(t, FixKind(kind), text, maxRunID)
	if msgErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: loops_exhausted retry: fix request message: %w", msgErr)
	}

	c := baseCommit(t, d)
	c.ResolveQuestions = resolveIDs
	c.Messages = []store.Message{msg}
	if kind == string(FixKindThreads) {
		c.Messages = append(c.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("respond applied %d\nreplied 0 fixing 1 skipped 0\nfix request after run %d", aid, maxRunID),
		})
	}
	return c, nil
}

// -----------------------------------------------------------------------
// Merge (design section 8.8, 8.9, task 8)
// -----------------------------------------------------------------------

// waitingMerge is legalWaiting's own "merge" flag (job.go): the merge
// question's own Waiting value, and row 7's and row 10's own carried-
// forward value while it is still open.
const waitingMerge = "merge"

// mergeOptions is the merge question's own fixed two-option closed set
// (design section 8.8): "a" merges now, "b" holds.
var mergeOptions = []response.Option{{Key: "a", Text: "Merge now"}, {Key: "b", Text: "Hold"}}

// The four reasons MERGE's own same-tick precondition re-read can refuse a
// merge (design section 8.8's own "A failed condition ... the reason").
const (
	mergeReasonHeadMoved  = "the head moved"
	mergeReasonCINotGreen = "CI is not green"
	mergeReasonThreadOpen = "a review thread is open"
	mergeReasonDraft      = "the pull request is a draft"
)

// mergeMarkerLine's own five kinds, named once for mergeAskable and
// newestMergeAskedSHA.
const (
	mergeMarkerAsked     = "asked"
	mergeMarkerWithdrawn = "withdrawn"
	mergeMarkerHeld      = "held"
	mergeMarkerRetry     = "retry"
)

// mergeBaseModified is the fixed substring GitHub's own merge refusal
// carries when another pull request's merge moved main out from under this
// one (the ticket's own repro); MERGE retries this refusal instead of
// asking the owner (design section 8.8's "Base branch was modified" row).
const mergeBaseModified = "Base branch was modified"

// mergeRetryDelay is the floor MERGE waits before its own automatic retry
// of a "Base branch was modified" refusal: a scheduled tick, not a sleep
// inside this handler (Q1's own decision) -- the next POLL, at or after
// this delay, decides whether to retry or to base-merge.
const mergeRetryDelay = 10 * time.Second

// mergeAskable is design section 8.5 row 9's own "the head is askable"
// rule: the newest of a head's merge asked, merge withdrawn, and merge
// held markers is none (kind "") or merge withdrawn.
func mergeAskable(kind string) bool {
	return kind == "" || kind == mergeMarkerWithdrawn
}

// shortSHA is prbody.go's own sha7 truncation (design section 8.8, 8.10),
// named here for the merge question's own body text.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// mergeQuestionMessages builds the merge question itself and its own
// "merge asked <sha>" marker together (design section 8.8's "The merge
// question"), reused by both row 9's first ask and MERGE's own re-ask
// after a GitHub refusal.
func mergeQuestionMessages(t store.Ticket, number int, sha, reason string) ([]store.Message, error) {
	payload, err := json.Marshal(response.QuestionPayload{
		Kind: response.QuestionKindMerge, State: response.QuestionStateOpen,
		Recommended: "a", Options: mergeOptions,
	})
	if err != nil {
		return nil, fmt.Errorf("job: shipping: merge question: marshal payload: %w", err)
	}
	body := fmt.Sprintf(
		"Merge pull request #%d?\n\nEvery check is green on %s and no review thread is open. Zing needs you because %s.",
		number, shortSHA(sha), reason,
	)
	return []store.Message{
		{TicketID: t.ID, Type: msgTypeQuestion, Author: authorZing, State: new(questionStateOpen), Body: body, Payload: payload},
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge " + mergeMarkerAsked + " " + sha},
	}, nil
}

// askMerge is design section 8.8's own "The merge question": the question
// itself, waiting "merge", and Poll with 8.3's own backoff. Design section
// 8.1's own blanket rule ("every commit in shipping sets ClearPoll, except
// POLL's idle commits and the merge question commit, which set Poll")
// names this commit as one of the two exceptions.
func (h shipHandler) askMerge(t store.Ticket, d Deps, number int, sha, reason, fp string) (store.HandlerCommit, error) {
	msgs, err := mergeQuestionMessages(t, number, sha, reason)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)
	waiting := waitingMerge

	c := baseCommit(t, d)
	c.Messages = msgs
	c.Waiting = &waiting
	c.Poll = &store.PollUpdate{NextAt: next, IntervalS: iv, Fingerprint: fp}
	return c, nil
}

// pollMergeWait is design section 8.5 row 10 (M4 task 8): CI green, zero
// unresolved threads, not draft, and the head is not askable -- still
// "asked" (open, unanswered) or "held" (resolved, waiting for the loop to
// reopen and close again, design section 8.8's MERGE-ANSWER). An idle
// commit with 8.3's own backoff; c.Waiting carries t.WaitingOn forward
// unchanged, exactly as pollIdle's own row 7 does: "merge" while the
// question is still open and unanswered (SendBatch has not cleared
// waiting_on yet), or nil once a held sha already cleared it at answer
// time, in which case POLL simply falls back onto its own ordinary
// next_poll_at schedule.
func (h shipHandler) pollMergeWait(t store.Ticket, d Deps, fp string) store.HandlerCommit {
	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)

	c := baseCommit(t, d)
	c.Waiting = t.WaitingOn
	c.Poll = &store.PollUpdate{NextAt: next, IntervalS: iv, Fingerprint: fp}
	return c
}

// pollMergeGate is design section 8.5 row 9 (M4 task 8): the head must be
// askable (mergeAskable) before anything else happens -- otherwise row
// 10's own idle wait applies. An askable head runs mergeDecision
// (shiprules.go) over the files changed since the default branch
// (orchestrator.ChangedFilesSinceBase): the rule allowing it calls MERGE
// directly, with no round to resolve; anything else asks the merge
// question instead.
func (h shipHandler) pollMergeGate(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, number int, sha, fp string) (store.HandlerCommit, error) {
	mergeMarkers, err := d.Store.MarkersWithPrefix(ctx, t.ID, mergeMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: merge markers: %w", err)
	}
	kind := newestMergeMarkerKind(mergeMarkers, sha)
	if kind == mergeMarkerRetry {
		slog.Info("merge retrying", "ticket_id", t.ID, "pr", number, "head_sha", sha)
		return h.merge(ctx, t, d, proj, number, sha, nil)
	}
	if !mergeAskable(kind) {
		return h.pollMergeWait(t, d, fp), nil
	}

	changed, err := proj.Orch.ChangedFilesSinceBase(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: changed files since base: %w", err)
	}
	auto, reason := mergeDecision(d.MergeRule, changed)
	if auto {
		return h.merge(ctx, t, d, proj, number, sha, nil)
	}
	return h.askMerge(t, d, number, sha, reason, fp)
}

// mergePreconditionFailed is design section 8.8's own MERGE refusal for
// one of the four local preconditions (not a GitHub refusal, mergeGitHubRefused
// below): the informational "merge refused <sha>" marker with its own
// reason line, "merge withdrawn <sha>" in the same commit so the head can
// be asked about again once the loop is clean, and -- on the Merge now
// path -- the answered round resolved too (resolveIDs, empty on the
// automatic path, which never had a round to begin with). Design section
// 8.1's own blanket rule makes this ClearPoll: it is neither "POLL's idle
// commits" nor "the merge question commit", the two named exceptions, and
// a cleared poll means the very next tick re-runs ordinary POLL from the
// top, the simplest way for "the next poll's rows handle the reopened
// loop" to actually happen.
func (h shipHandler) mergePreconditionFailed(t store.Ticket, d Deps, sha, reason string, resolveIDs []int64) store.HandlerCommit {
	c := baseCommit(t, d)
	c.WithdrawQuestions = resolveIDs
	c.Messages = []store.Message{
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge refused " + sha + "\n" + reason},
		{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge withdrawn " + sha},
	}
	c.ClearPoll = true
	return c
}

// mergeGitHubRefused is design section 8.8's own ErrMergeRefused row. A
// refusal whose message contains mergeBaseModified, on a head with no
// earlier "merge retry <sha>" marker, is the ticket's own "main moved
// under the merge" row: it writes "merge refused <sha>" with the fixed
// reason line, then "merge retry <sha>", and schedules the next poll at
// least mergeRetryDelay out -- no question. Every other refusal, and a
// second "Base branch was modified" refusal on a head that already
// carries a retry marker, asks as it always has: the bare informational
// "merge refused <sha>" marker (no reason line -- the reason goes in the
// re-asked question's own body instead), then the merge question asked
// again with "GitHub refused the merge: <GitHub's own message>" as its
// reason, waiting "merge", and Poll with 8.3's own backoff (the second of
// design section 8.1's two ClearPoll exceptions: this re-asks). ghErr's
// own message is read by trimming orchestrator.ErrMergeRefused's own
// sentinel text off the wrapped error's Error() string, rather than
// hardcoding it a second time.
func (h shipHandler) mergeGitHubRefused(ctx context.Context, t store.Ticket, d Deps, number int, sha string, ghErr error, resolveIDs []int64, fp string) (store.HandlerCommit, error) {
	refusal := "other"
	if strings.Contains(ghErr.Error(), mergeBaseModified) {
		refusal = "base_modified"
		markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, mergeMarkerPrefix)
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: merge markers: %w", err)
		}
		retried := slices.ContainsFunc(markers, func(m store.MessageRow) bool {
			firstLine, _, _ := strings.Cut(m.Body, "\n")
			sub := mergeMarkerLine.FindStringSubmatch(firstLine)
			if sub == nil {
				return false
			}
			return sub[1] == mergeMarkerRetry && sub[2] == sha
		})
		if !retried {
			// Main moved under the merge: no question; the next POLL,
			// at least mergeRetryDelay out, base-merges or retries once.
			next := time.Now().UTC().Truncate(time.Second).Add(mergeRetryDelay + time.Second)
			c := baseCommit(t, d)
			c.WithdrawQuestions = resolveIDs
			c.Messages = []store.Message{
				{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge refused " + sha + "\n" + mergeBaseModified},
				{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge " + mergeMarkerRetry + " " + sha},
			}
			c.Poll = &store.PollUpdate{NextAt: next, IntervalS: 30, Fingerprint: fp}
			slog.Info("merge refused, retrying", "ticket_id", t.ID, "pr", number, "head_sha", sha, "refusal", refusal, "retry_at", next)
			return c, nil
		}
	}
	slog.Info("merge refused, asking the owner", "ticket_id", t.ID, "pr", number, "head_sha", sha, "refusal", refusal)

	reason := "GitHub refused the merge: " + strings.TrimPrefix(ghErr.Error(), orchestrator.ErrMergeRefused.Error()+": ")
	askMsgs, err := mergeQuestionMessages(t, number, sha, reason)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)
	waiting := waitingMerge

	c := baseCommit(t, d)
	c.WithdrawQuestions = resolveIDs
	c.Messages = append([]store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge refused " + sha,
	}}, askMsgs...)
	c.Waiting = &waiting
	c.Poll = &store.PollUpdate{NextAt: next, IntervalS: iv, Fingerprint: fp}
	return c, nil
}

// merge is MERGE (design section 8.8): a same-tick re-read of the pull
// request, CI, and threads, pinned to sha (N4) -- independent of whatever
// read decided to call it, whether row 9's own automatic gate or a "Merge
// now" answer MERGE-ANSWER reads days later -- then either the real
// GitHub merge call or a refusal that asks again. resolveIDs is the merge
// question's own round, non-empty only on the Merge now path
// (mergeAnswer); the automatic path calls this with no round to resolve.
func (h shipHandler) merge(ctx context.Context, t store.Ticket, d Deps, proj Project, number int, sha string, resolveIDs []int64) (store.HandlerCommit, error) {
	pr, err := proj.PullRequests.GetPR(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: get pr: %w", err)
	}
	runs, err := proj.Checks.ListCheckRuns(ctx, proj.Owner, proj.Repo, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: list check runs: %w", err)
	}
	statuses, err := proj.Checks.ListStatuses(ctx, proj.Owner, proj.Repo, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: list statuses: %w", err)
	}
	required, err := proj.Checks.RequiredCheckRules(ctx, proj.Owner, proj.Repo, pr.BaseRef)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: required check rules: %w", err)
	}
	threadsRaw, err := proj.Threads.ListThreads(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: list threads: %w", err)
	}
	login, err := proj.Threads.Viewer(ctx)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: viewer: %w", err)
	}

	_, actionable, leftover, unclassified := classifyThreads(threadsRaw, login)
	anyUnresolved := len(actionable) > 0 || len(leftover) > 0 || len(unclassified) > 0
	result := prCI(pr, runs, statuses, required)
	fp := pollFingerprint(pr, runs, statuses, required, pollThreadsFrom(threadsRaw))

	reason := ""
	switch {
	case pr.HeadSHA != sha:
		reason = mergeReasonHeadMoved
	case pr.State != "open" || pr.Draft:
		reason = mergeReasonDraft
	case result.State != CIGreen:
		reason = mergeReasonCINotGreen
	case anyUnresolved:
		reason = mergeReasonThreadOpen
	}
	if reason != "" {
		return h.mergePreconditionFailed(t, d, sha, reason, resolveIDs), nil
	}

	title := fmt.Sprintf("%s (#%d)", prTitle(t.Title, t.TrackerRef), number)

	if _, mergeErr := proj.PullRequests.Merge(ctx, proj.Owner, proj.Repo, number, sha, d.MergeRule.Method, title); mergeErr != nil {
		if errors.Is(mergeErr, orchestrator.ErrMergeRefused) {
			return h.mergeGitHubRefused(ctx, t, d, number, sha, mergeErr, resolveIDs, fp)
		}
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge: %w", mergeErr)
	}
	slog.Info("pr merged", "ticket_id", t.ID, "pr", number, "head_sha", sha)

	c := baseCommit(t, d)
	c.WithdrawQuestions = resolveIDs
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "pr merged " + sha,
	}}
	c.ClearPoll = true
	return c, nil
}

// newestMergeAskedSHA returns the sha of the newest "merge asked" marker
// among markers (design section 8.8's own "MERGE-ANSWER. sha := the sha
// of the newest merge asked marker"): unlike newestMergeMarkerKind, this
// is not scoped to one sha -- it scans every "merge " marker on the
// ticket for the newest one whose own kind is "asked". markers is
// MarkersWithPrefix's own oldest-first order, so the last match is the
// newest.
func newestMergeAskedSHA(markers []store.MessageRow) (sha string, ok bool) {
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := mergeMarkerLine.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		if sub[1] != mergeMarkerAsked {
			continue
		}
		sha, ok = sub[2], true
	}
	return sha, ok
}

// mergeAnswer is MERGE-ANSWER (design section 8.8): option a (roundChoice's
// own escalationChoiceRetry) calls MERGE with the newest "merge asked"
// marker's own sha, resolving the round; option b, or a reply with no
// option (roundChoice's own default), holds instead -- the informational
// "merge held <sha>" marker, the round resolved the same way, ClearPoll
// (design section 8.1's blanket rule: this is neither an idle commit nor
// the merge question commit). t.WaitingOn is already nil by the time this
// runs either way: answering a question, through the console's own
// SendBatch, already cleared waiting_on at answer time (clearMatchingWaitTx,
// internal/store/console_writes.go), before this handler ever sees the
// round.
func (h shipHandler) mergeAnswer(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge answer: no project %d", t.ProjectID)
	}
	if t.PRURL == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge answer: ticket %d has no pr url", t.ID)
	}
	number, err := parsePRNumber(*t.PRURL)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge answer: %w", err)
	}

	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, mergeMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge answer: merge markers: %w", err)
	}
	sha, ok := newestMergeAskedSHA(markers)
	if !ok {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: merge answer: ticket %d has no open merge asked marker", t.ID)
	}

	resolveIDs := questionIDs(round)
	if roundChoice(round) == escalationChoiceRetry {
		return h.merge(ctx, t, d, proj, number, sha, resolveIDs)
	}

	c := baseCommit(t, d)
	c.WithdrawQuestions = resolveIDs
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge " + mergeMarkerHeld + " " + sha,
	}}
	c.ClearPoll = true
	return c, nil
}
