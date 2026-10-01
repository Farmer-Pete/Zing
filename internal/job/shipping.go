// shipping.go declares the consumer-side interfaces the shipping state
// machine needs from GitHub, over orchestrator.GitHubClient's REST and
// GraphQL surface (PKG9-PLAN.md section 10.3). Declaring them here, not
// widening orchestrator.GitHub, keeps every existing fake of that
// four-method interface (cmd/zing/selftest.go, internal/dispatch/dispatch_test.go,
// internal/console/resume_e2e_test.go, the orchestrator tests) valid: a test
// of a handler that needs PullRequests, DraftFlips, or Checks implements
// only the small interface it uses. M3 task 6 adds PUBLISH (design section
// 8.2), the shipping decision tree's own step (4); task 7 adds POLL and the
// rest (8.1, 8.3 to 8.9).
//
// shipHandler is a distinct type from skeleton.go's own shippingHandler,
// the same split task 7a (judging.go) took for "judging": job.go's
// Registry()["shipping"] still points at the skeleton until task 7 also
// adds POLL, because PUBLISH alone cannot carry a ticket out of "shipping"
// -- decision tree step 5, POLL, is not built yet, so a second tick would
// find pr_url already set and nothing left to do. Keeping the skeleton
// wired means the selftest e2e, which still needs one tick to leave
// "shipping", keeps passing; shipHandler is tested directly against a real
// git fixture and a fake GitHub (shipping_test.go), never through
// Registry().
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

// shipHandler runs the real shipping state (design section 8): PUBLISH
// (8.2) only, for now (see the file comment above). It is tested directly,
// never through job.go's Registry().
type shipHandler struct{}

// Run is the shipping state's own decision tree (design section 8.1): this
// task implements the prelude (P) and step (4), PUBLISH, the only step
// reachable with pr_url still NULL. Steps (1) to (3) (an answered merge or
// respond round) and (5), POLL, are task 7's; they can never actually fire
// yet, since PUBLISH makes no runtime call and asks no question of its own,
// so reaching them here is ErrNoAction rather than a silent no-op.
func (h shipHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c, handled, err := postBuildPrelude(ctx, t, d, response.EscalationOriginShipping)
	if handled || err != nil {
		return c, err
	}

	if t.PRURL == nil {
		return h.publish(ctx, t, d)
	}
	return store.HandlerCommit{}, ErrNoAction
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

// shipEscalation is escalationCommit (planning.go) plus this file's own
// "escalation written" log (design section 11), for every environment
// escalation PUBLISH writes: origin is always "shipping", and, like
// judgeEscalation and reviewEscalation, RunID and SessionID are both nil --
// PUBLISH makes no runtime call of its own.
func shipEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeEnvironment)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginShipping))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginShipping)
}
