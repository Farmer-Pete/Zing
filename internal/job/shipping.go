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
// M4 task 5 (respond.go) adds decision tree step (3), APPLY (9.3). 8.5's
// rows 1 to 3, 6, 6a, 8, and 9 (FIX-REPLIES, RE-REQUEST, the draft/ready
// flip, the leftover resolve, the unclassified blocking marker, and MERGE)
// stay later M4 tasks'. Reaching one of those unbuilt rows is ErrNoAction,
// not a silent no-op, since nothing before them can write the marker or
// round shape that would route there.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
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
	PostDone(ctx context.Context, projectID int64, ref, prURL string) error
}

// shipHandler runs the real shipping state (design section 8). It is
// job.go's own Registry()["shipping"] entry.
type shipHandler struct{}

// Run is the shipping state's own decision tree (design section 8.1): the
// prelude (P), step (1)'s own "job respond" branch (RESPOND resume with
// answers, M4 task 4), step (2) (RESPOND's first turn and every resume, M4
// task 4), step (3) (APPLY, M4 task 5, respond.go), step (4) PUBLISH when
// pr_url is still NULL, and step (5) POLL otherwise. Step (1)'s own "merge"
// branch (MERGE-ANSWER) is M4 task 8's: nothing before it ever writes a
// "merge asked" or "merge held" marker, so an answered round of any other
// job or kind is a bug this reports loudly rather than guessing at.
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

	pr := prBody(t.ID, plan, final, cohort, reports, events)

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
		return h.pollDone(ctx, t, d, *t.PRURL)
	}
	if pr.State == "closed" {
		return h.pollClosed(t, d, number), nil
	}

	if pr.HeadSHA != local {
		return h.pollHeadMismatch(ctx, t, d, proj, wt, pr, local)
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
	// classifyThreads and pollThreadsFrom are threadrules.go's and respond.go's
	// own pure helpers (design section 9.1, 8.3): every class feeds the
	// fingerprint (pollThreadsFrom), but only actionable threads drive row 5
	// below (M4 task 4). Rows 3, 6, 6a, 8, and 9 -- the draft/ready flip, the
	// leftover resolve, the unclassified blocking marker, and the merge
	// question -- are M4 tasks 7 and 8's own reads of resolved/leftover/
	// unclassified; this task only wires the real thread list through.
	_, actionable, _, _ := classifyThreads(threadsRaw, login)
	pollThreads := pollThreadsFrom(threadsRaw)

	fp := pollFingerprint(pr, runs, statuses, required, pollThreads)
	result := EvaluateCI(runs, statuses, required)

	switch {
	case result.State == CIUnprotected:
		c := shipEscalation(t, d, unprotectedWhat, unprotectedWhy, "")
		c.ClearPoll = true
		return c, nil
	case result.State == CIFailed:
		return h.pollCIFailed(ctx, t, d, proj, result)
	case len(actionable) > 0:
		return h.startRespondBatch(ctx, t, d, local, actionable, login)
	default: // CIPending, CIGreen with no actionable thread: M4 tasks 7, 8 add the ready flip and merge (8.5 rows 8, 9).
		return h.pollIdle(ctx, t, d, fp, result.Missing)
	}
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
func (h shipHandler) pollDone(ctx context.Context, t store.Ticket, d Deps, prURL string) (store.HandlerCommit, error) {
	if err := d.Tracker.PostDone(ctx, t.ProjectID, t.TrackerRef, prURL); err != nil {
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

// pollCIFailed is design section 8.5 row 4: the shared gate of 8.7, then a
// ci_log fix request with ciLogText's own log tails, or loops_exhausted
// when the gate is already at jobs.respond.max_loops (D14). Rows 5 and 6a's
// own merge-question withdrawal has nothing to withdraw in M3 (no code
// anywhere in this milestone ever writes a "merge asked" or "merge held"
// marker), so it is not built here.
func (h shipHandler) pollCIFailed(ctx context.Context, t store.Ticket, d Deps, proj Project, result CIResult) (store.HandlerCommit, error) {
	text := ciLogText(ctx, proj.Checks, proj.Owner, proj.Repo, result.FailedRuns, result.FailedStatuses)

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

// pollIdle is design section 8.5 row 7 (CI pending) and, in M3 only
// (8.5 rows 8 and 9 are M4's), row's own CI green case too: the backoff
// commit of 8.3 (Poll{NextAt, IntervalS, Fingerprint}), plus the
// informational "ci waiting <names>" marker (design section 8.4) whenever
// Missing differs from the previous poll's own newest such marker.
func (h shipHandler) pollIdle(ctx context.Context, t store.Ticket, d Deps, fp string, missing []string) (store.HandlerCommit, error) {
	iv := nextInterval(t.PollFingerprint, t.PollIntervalS, fp)
	next := time.Now().UTC().Truncate(time.Second).Add(time.Duration(iv) * time.Second)

	c := baseCommit(t, d)
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
	return c, nil
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
