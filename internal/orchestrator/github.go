package orchestrator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"
)

// GitHub is the orchestrator's window onto the GitHub API. The real
// implementation (*GitHubClient, below) wraps go-github v92; the e2e test
// injects a scripted fake, and github_test.go exercises the real
// implementation at the HTTP layer. Package 5 declares only the four
// methods it uses, and Package 9 keeps this interface at four: its own new
// reads and writes are methods of GitHubClient instead, each declared as a
// small interface by the package that calls it (job.PullRequests,
// job.Checks, and the M4 interfaces; PKG9-PLAN.md section 10.3).
type GitHub interface {
	// RepoDefaultBranch returns the repository's default branch name.
	RepoDefaultBranch(ctx context.Context, owner, repo string) (string, error)
	// RequiredChecks returns the required status-check contexts named by
	// either the branch's classic protection or a repository ruleset that
	// applies to it, merged and deduplicated by context name (D28, found
	// 2026-10-01: a branch can be protected by a ruleset alone, which
	// classic protection reads cannot see). When neither source names any
	// check it returns an empty slice and no error. Any other failure is
	// an error.
	RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, error)
	// CreateDraftPR opens a draft pull request and returns its URL and number.
	CreateDraftPR(ctx context.Context, owner, repo, head, base, title, body string) (url string, number int, err error)
	// FindPRByHead returns the open PR whose head branch is head and whose
	// base branch is base, or ok=false.
	FindPRByHead(ctx context.Context, owner, repo, head, base string) (url string, number int, ok bool, err error)
}

// ghHTTPTimeout bounds every call the real client makes, so a call never
// hangs forever even when a caller passes a context with no deadline
// (PKG5-PLAN.md section 6).
const ghHTTPTimeout = 30 * time.Second

// ghPerPage is the page size for every paginated GitHub list call this
// client makes (PKG9-PLAN.md section 10.3).
const ghPerPage = 100

// GitHubClient is the real GitHub implementation, wrapping go-github v92. It
// satisfies the four-method GitHub interface above and the narrower, job-
// package-declared interfaces over its fuller REST surface (job.PullRequests,
// job.Checks; PKG9-PLAN.md section 10.3): one client, one token, read by
// every caller through the small interface it needs.
type GitHubClient struct {
	c *github.Client
}

// Guarantee the concrete type satisfies the interface without returning the
// interface from the constructor (ireturn), the same pattern
// internal/tracker.GitHubTracker uses: NewGitHub already returns GitHub, so
// this line only documents that NewGitHubClient's own *GitHubClient still
// does, and that every job-package interface it also satisfies
// (internal/job/shipping.go, internal/job/shipping_test.go) leaves this one
// unwidened.
var _ GitHub = (*GitHubClient)(nil)

// NewGitHubClient builds the real client, authenticated with token, and
// returns an error because github.NewClient does: an empty token yields
// go-github's own "token must not be empty" (PKG5-PLAN.md section 13). The
// 30-second timeout is set with the WithTimeout client option rather than by
// mutating the client's *http.Client field directly: go-github v92's
// Client.Client() returns a defensive copy of that field, so assigning
// Timeout on it would have no effect on the requests the client actually
// sends.
func NewGitHubClient(token string) (*GitHubClient, error) {
	c, err := github.NewClient(github.WithAuthToken(token), github.WithTimeout(ghHTTPTimeout))
	if err != nil {
		return nil, fmt.Errorf("orchestrator: new github client: %w", err)
	}
	return &GitHubClient{c: c}, nil
}

// NewGitHub builds the real client and returns it as the four-method GitHub
// interface (PKG5-PLAN.md section 6): the same concrete client
// NewGitHubClient returns, typed narrowly for Package 5's callers.
func NewGitHub(token string) (GitHub, error) {
	return NewGitHubClient(token)
}

// RepoDefaultBranch calls Repositories.Get and reads GetDefaultBranch().
func (g *GitHubClient) RepoDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	repository, _, err := g.c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("orchestrator: repo default branch: %w", err)
	}
	return repository.GetDefaultBranch(), nil
}

// RequiredChecks returns RequiredCheckRules' merged contexts, deduplicated
// by name (D28): classic protection's checks, then its legacy contexts,
// then the rulesets' checks, in the order each name is first seen.
func (g *GitHubClient) RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, error) {
	rules, err := g.RequiredCheckRules(ctx, owner, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: required checks: %w", err)
	}

	seen := make(map[string]bool, len(rules))
	checks := make([]string, 0, len(rules))
	for _, rc := range rules {
		if rc.Context == "" || seen[rc.Context] {
			continue
		}
		seen[rc.Context] = true
		checks = append(checks, rc.Context)
	}
	return checks, nil
}

// CreateDraftPR calls PullRequests.Create with Draft set true. A failure is
// classified through classifyGitHubErr, the same as every other M3 task 2
// method, so PUBLISH (internal/job/shipping.go) can tell auth from
// unavailable from rate limit on a live failure.
func (g *GitHubClient) CreateDraftPR(ctx context.Context, owner, repo, head, base, title, body string) (url string, number int, err error) {
	pr, _, err := g.c.PullRequests.Create(ctx, owner, repo, github.CreatePullRequest{
		Title: new(title),
		Head:  head,
		Base:  base,
		Body:  new(body),
		Draft: new(true),
	})
	if err != nil {
		return "", 0, classifyGitHubErr(err)
	}
	return pr.GetHTMLURL(), pr.GetNumber(), nil
}

// FindPRByHead calls PullRequests.List with a Head filter of "owner:head", a
// Base filter of base, and State "open", returning the first match. The Base
// filter matters: without it, OpenDraftPR's fallback could return an open PR
// from wt.Branch into some other base entirely, not the default branch it
// meant to open one against. A request failure is classified through
// classifyGitHubErr, the same as every other M3 task 2 method, so PUBLISH
// can tell auth from unavailable from rate limit on a live failure; an empty
// result still returns ok=false and no error, unchanged.
func (g *GitHubClient) FindPRByHead(ctx context.Context, owner, repo, head, base string) (url string, number int, ok bool, err error) {
	prs, _, err := g.c.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:  owner + ":" + head,
		Base:  base,
		State: "open",
	})
	if err != nil {
		return "", 0, false, classifyGitHubErr(err)
	}
	if len(prs) == 0 {
		return "", 0, false, nil
	}
	return prs[0].GetHTMLURL(), prs[0].GetNumber(), true, nil
}

// PRState is one pull request's state, as job.PullRequests.GetPR and
// job.ReviewThreads' callers read it (PKG9-PLAN.md section 10.3).
type PRState struct {
	Number  int
	NodeID  string // GraphQL id, for MarkReady and ConvertToDraft
	State   string // "open" | "closed"
	Merged  bool
	Draft   bool
	HeadSHA string // 40 hex
	BaseRef string
	// MergeableState is GitHub's own mergeable_state ("clean", "blocked",
	// "unstable", ...); "clean" means GitHub has seen every required check
	// pass, app identities included.
	MergeableState string
	// MergeCommitSHA is GitHub's merge commit, 40 lowercase hex once Merged
	// is true; empty otherwise.
	MergeCommitSHA string
}

// CheckRun is one check run GitHub reports for a commit (PKG9-PLAN.md
// section 10.3), as EvaluateCI (internal/job) consumes it.
type CheckRun struct {
	ID         int64
	Name       string
	Status     string // queued | in_progress | completed | waiting | requested | pending
	Conclusion string // "" until completed; else success | failure | neutral | cancelled | skipped | timed_out | action_required | startup_failure | stale
	AppSlug    string // "github-actions" for Actions
	AppID      int64  // the GitHub App that created the run
	DetailsURL string
}

// CommitStatus is one legacy commit status GitHub reports for a commit
// (PKG9-PLAN.md section 10.3), as EvaluateCI (internal/job) consumes it.
type CommitStatus struct {
	Context   string
	State     string // success | failure | error | pending
	TargetURL string
}

// RequiredCheck is one required status check of a branch's protection.
// AppID is nil for a legacy context, or for a modern check whose app_id is
// null or -1 (any source); else it is the only app whose check satisfies it.
type RequiredCheck struct {
	Context string
	AppID   *int64
}

// Review is one pull request review GitHub reports (PKG9-PLAN.md section
// 10.3), as job.ReviewThreads' callers consume it (M4).
type Review struct {
	Login, UserType, State, CommitID string // UserType "User" or "Bot"; State APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
}

// Thread is one pull request review thread GitHub reports (PKG9-PLAN.md
// section 10.3), as job.ReviewThreads' callers consume it (M4). Comments
// holds the last 100, oldest first; ThreadCommentsContain reads further
// back when the reply guard needs more than that.
type Thread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       int // 0 when GitHub reports null
	Comments   []ThreadComment
}

// ThreadComment is one comment of a Thread (PKG9-PLAN.md section 10.3).
type ThreadComment struct {
	ID        string // GraphQL id
	Author    string // login; "ghost" when GitHub reports none
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time // GitHub's updatedAt; changes when the comment is edited
}

// ListThreads calls the ZingThreads query, paged with reviewThreads'
// pageInfo, each node carrying its last 100 comments inline
// (job.ReviewThreads.ListThreads, M4; PKG9-PLAN.md section 10.3, 10.4). At
// most 10 pages (1000 threads); an eleventh is the error "orchestrator:
// graphql threads: more than 1000 review threads", never fetched.
func (g *GitHubClient) ListThreads(ctx context.Context, owner, repo string, number int) ([]Thread, error) {
	gql := graphQL{c: g.c}

	var out []Thread
	var after *string
	for page := 1; ; page++ {
		var resp zingThreadsData
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "after": after}
		if err := gql.do(ctx, "threads", qZingThreads, vars, &resp); err != nil {
			return nil, err
		}

		rt := resp.Repository.PullRequest.ReviewThreads
		for _, n := range rt.Nodes {
			out = append(out, n.toThread())
		}
		if !rt.PageInfo.HasNextPage {
			return out, nil
		}
		if page >= maxThreadPages {
			return nil, errors.New("orchestrator: graphql threads: more than 1000 review threads")
		}
		cursor := rt.PageInfo.EndCursor
		after = &cursor
	}
}

// ThreadCommentsContain calls the ZingThreadComments query, paged until
// GitHub reports no next page (no page limit), and reports whether any
// comment of the thread -- not only the last 100 -- is authored by author
// and contains needle (job.ReviewThreads.ThreadCommentsContain, M4;
// PKG9-PLAN.md section 10.3, 10.4).
func (g *GitHubClient) ThreadCommentsContain(ctx context.Context, threadID, needle, author string) (bool, error) {
	gql := graphQL{c: g.c}

	var after *string
	for {
		var resp zingThreadCommentsData
		vars := map[string]any{gqlVarThread: threadID, "after": after}
		if err := gql.do(ctx, "thread comments", qZingThreadComments, vars, &resp); err != nil {
			return false, err
		}

		for _, c := range resp.Node.Comments.Nodes {
			tc := c.toThreadComment()
			if tc.Author == author && strings.Contains(tc.Body, needle) {
				return true, nil
			}
		}
		if !resp.Node.Comments.PageInfo.HasNextPage {
			return false, nil
		}
		cursor := resp.Node.Comments.PageInfo.EndCursor
		after = &cursor
	}
}

// ReplyToThread calls the ZingReply mutation (job.ReviewThreads.ReplyToThread,
// M4; PKG9-PLAN.md section 10.3, 10.4).
func (g *GitHubClient) ReplyToThread(ctx context.Context, threadID, body string) error {
	gql := graphQL{c: g.c}
	vars := map[string]any{gqlVarThread: threadID, "body": body}
	return gql.do(ctx, "reply", qZingReply, vars, nil)
}

// ResolveThread calls the ZingResolve mutation and checks the result's
// isResolved (job.ReviewThreads.ResolveThread, M4; PKG9-PLAN.md section
// 10.3, 10.4). A mismatch is "orchestrator: graphql resolve: result did not
// change state".
func (g *GitHubClient) ResolveThread(ctx context.Context, threadID string) error {
	gql := graphQL{c: g.c}
	var resp zingResolveData
	vars := map[string]any{gqlVarThread: threadID}
	if err := gql.do(ctx, "resolve", qZingResolve, vars, &resp); err != nil {
		return err
	}
	if !resp.ResolveReviewThread.Thread.IsResolved {
		return errors.New("orchestrator: graphql resolve: result did not change state")
	}
	return nil
}

// MarkReady calls the ZingReady mutation and checks the result's isDraft is
// false (job.DraftFlips.MarkReady, M4; PKG9-PLAN.md section 10.3, 10.4). A
// mismatch is "orchestrator: graphql ready: result did not change state".
func (g *GitHubClient) MarkReady(ctx context.Context, prNodeID string) error {
	gql := graphQL{c: g.c}
	var resp zingReadyData
	vars := map[string]any{"pr": prNodeID}
	if err := gql.do(ctx, "ready", qZingReady, vars, &resp); err != nil {
		return err
	}
	if resp.MarkPullRequestReadyForReview.PullRequest.IsDraft {
		return errors.New("orchestrator: graphql ready: result did not change state")
	}
	return nil
}

// ConvertToDraft calls the ZingDraft mutation and checks the result's
// isDraft is true (job.DraftFlips.ConvertToDraft, M4; PKG9-PLAN.md section
// 10.3, 10.4). A mismatch is "orchestrator: graphql draft: result did not
// change state".
func (g *GitHubClient) ConvertToDraft(ctx context.Context, prNodeID string) error {
	gql := graphQL{c: g.c}
	var resp zingDraftData
	vars := map[string]any{"pr": prNodeID}
	if err := gql.do(ctx, "draft", qZingDraft, vars, &resp); err != nil {
		return err
	}
	if !resp.ConvertPullRequestToDraft.PullRequest.IsDraft {
		return errors.New("orchestrator: graphql draft: result did not change state")
	}
	return nil
}

// GetPR calls PullRequests.Get and reports the pull request's current
// state (job.PullRequests.GetPR; PKG9-PLAN.md section 10.3).
func (g *GitHubClient) GetPR(ctx context.Context, owner, repo string, number int) (PRState, error) {
	pr, _, err := g.c.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return PRState{}, classifyGitHubErr(err)
	}
	return PRState{
		Number:  pr.GetNumber(),
		NodeID:  pr.GetNodeID(),
		State:   pr.GetState(),
		Merged:  pr.GetMerged(),
		Draft:   pr.GetDraft(),
		HeadSHA: pr.GetHead().GetSHA(),
		BaseRef: pr.GetBase().GetRef(),

		MergeableState: pr.GetMergeableState(),
		MergeCommitSHA: pr.GetMergeCommitSHA(),
	}, nil
}

// mergeRefusedStatus reports whether code is one of the three statuses
// PKG9-PLAN.md section 10.3 names as a refused merge: 405 (not mergeable
// this way), 409 (head moved), 422 (validation failed, e.g. required
// reviews or checks still outstanding).
func mergeRefusedStatus(code int) bool {
	return code == http.StatusMethodNotAllowed || code == http.StatusConflict || code == http.StatusUnprocessableEntity
}

// ghErrMessage returns the message GitHub sent back with err, when err
// carries a *github.ErrorResponse; otherwise err's own text.
func ghErrMessage(err error) string {
	if ere, ok := errors.AsType[*github.ErrorResponse](err); ok {
		return ere.Message
	}
	return err.Error()
}

// Merge calls PullRequests.Merge with sha pinning the head commit, so
// GitHub refuses the merge if the head moved since the caller last read it.
// A result with merged == false, or a 405, 409, or 422 response, is
// ErrMergeRefused wrapped with GitHub's own message (job.PullRequests.Merge;
// PKG9-PLAN.md section 10.3).
func (g *GitHubClient) Merge(ctx context.Context, owner, repo string, number int, sha, method, title string) (string, error) {
	result, resp, err := g.c.PullRequests.Merge(ctx, owner, repo, number, "", &github.PullRequestOptions{
		SHA:         sha,
		MergeMethod: method,
		CommitTitle: title,
	})
	if err != nil {
		if resp != nil && mergeRefusedStatus(resp.StatusCode) {
			return "", fmt.Errorf("%w: %s", ErrMergeRefused, ghErrMessage(err))
		}
		return "", classifyGitHubErr(err)
	}
	if !result.GetMerged() {
		return "", fmt.Errorf("%w: %s", ErrMergeRefused, result.GetMessage())
	}
	return result.GetSHA(), nil
}

// ListCheckRuns calls Checks.ListCheckRunsForRef with filter "latest",
// paginated to completion (job.Checks.ListCheckRuns; PKG9-PLAN.md section
// 10.3). EvaluateCI (internal/job), not this method, reduces runs to the
// newest per (name, app id).
func (g *GitHubClient) ListCheckRuns(ctx context.Context, owner, repo, sha string) ([]CheckRun, error) {
	opts := &github.ListCheckRunsOptions{Filter: new("latest")}
	opts.PerPage = ghPerPage

	var out []CheckRun
	for {
		results, resp, err := g.c.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return nil, classifyGitHubErr(err)
		}
		for _, cr := range results.CheckRuns {
			out = append(out, CheckRun{
				ID:         cr.GetID(),
				Name:       cr.GetName(),
				Status:     cr.GetStatus(),
				Conclusion: cr.GetConclusion(),
				AppSlug:    cr.GetApp().GetSlug(),
				AppID:      cr.GetApp().GetID(),
				DetailsURL: cr.GetDetailsURL(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// ListStatuses calls Repositories.GetCombinedStatus, paginated to
// completion (job.Checks.ListStatuses; PKG9-PLAN.md section 10.3).
func (g *GitHubClient) ListStatuses(ctx context.Context, owner, repo, sha string) ([]CommitStatus, error) {
	opts := &github.ListOptions{PerPage: ghPerPage}

	var out []CommitStatus
	for {
		combined, resp, err := g.c.Repositories.GetCombinedStatus(ctx, owner, repo, sha, opts)
		if err != nil {
			return nil, classifyGitHubErr(err)
		}
		for _, s := range combined.Statuses {
			out = append(out, CommitStatus{
				Context:   s.GetContext(),
				State:     s.GetState(),
				TargetURL: s.GetTargetURL(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// RequiredCheckRules returns the required status checks named by either
// classic branch protection or a repository ruleset that applies to the
// branch, merged and deduplicated by (context, app id)
// (job.Checks.RequiredCheckRules; PKG9-PLAN.md section 10.3; D28, found
// 2026-10-01: Farmer-Pete/Zing is protected only by a ruleset, which
// classic protection reads cannot see). Neither source existing is not an
// error, the app-aware analog of RequiredChecks above; a genuine failure
// from either source is.
func (g *GitHubClient) RequiredCheckRules(ctx context.Context, owner, repo, branch string) ([]RequiredCheck, error) {
	classic, err := g.classicRequiredCheckRules(ctx, owner, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: required check rules: classic protection: %w", err)
	}
	rules, err := g.rulesetRequiredCheckRules(ctx, owner, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: required check rules: rules: %w", err)
	}
	return mergeRequiredCheckRules(classic, rules), nil
}

// classicRequiredCheckRules calls Repositories.GetBranchProtection and
// returns its required status checks, the modern RequiredStatusChecks.
// Checks[] shape first (with each check's app id), then the legacy
// RequiredStatusChecks.Contexts shape (always a nil app id). Any 404 means
// this source has no checks and returns an empty result and no error (D28):
// go-github's own github.ErrBranchNotProtected sentinel for the usual
// "Branch not protected" message, or a differently worded 404 classified
// through classifyGitHubErr. Any other failure is classified and returned.
func (g *GitHubClient) classicRequiredCheckRules(ctx context.Context, owner, repo, branch string) ([]RequiredCheck, error) {
	protection, _, err := g.c.Repositories.GetBranchProtection(ctx, owner, repo, branch)
	if err != nil {
		if errors.Is(err, github.ErrBranchNotProtected) {
			return nil, nil
		}
		classified := classifyGitHubErr(err)
		if errors.Is(classified, ErrGitHubNotFound) {
			return nil, nil
		}
		return nil, classified
	}

	rsc := protection.GetRequiredStatusChecks()
	var out []RequiredCheck
	for _, c := range rsc.GetChecks() {
		out = append(out, RequiredCheck{Context: c.Context, AppID: normalizeAppID(c.AppID)})
	}
	for _, name := range rsc.GetContexts() {
		out = append(out, RequiredCheck{Context: name})
	}
	return out, nil
}

// rulesetRequiredCheckRules calls Repositories.ListRulesForBranch (GET
// /repos/{owner}/{repo}/rules/branches/{branch}), paginated to completion,
// and returns every required_status_checks rule's checks as RequiredCheck
// values (D28). A 404 means no ruleset applies to the branch at all, the
// ruleset analog of an unprotected branch, and returns an empty slice and
// no error; any other failure is classified and returned.
func (g *GitHubClient) rulesetRequiredCheckRules(ctx context.Context, owner, repo, branch string) ([]RequiredCheck, error) {
	opts := &github.ListOptions{PerPage: ghPerPage}

	var out []RequiredCheck
	for {
		rules, resp, err := g.c.Repositories.ListRulesForBranch(ctx, owner, repo, branch, opts)
		if err != nil {
			classified := classifyGitHubErr(err)
			if errors.Is(classified, ErrGitHubNotFound) {
				return nil, nil
			}
			return nil, classified
		}
		for _, rule := range rules.RequiredStatusChecks {
			for _, c := range rule.Parameters.RequiredStatusChecks {
				out = append(out, RequiredCheck{Context: c.Context, AppID: normalizeAppID(c.IntegrationID)})
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// normalizeAppID maps a check's raw app id field onto RequiredCheck.AppID:
// nil when the field itself is nil or -1 (GitHub's "any source" value), a
// copy of the app id otherwise, matching RequiredCheck's documented rule.
func normalizeAppID(appID *int64) *int64 {
	if appID == nil || *appID == -1 {
		return nil
	}
	return new(*appID)
}

// mergeRequiredCheckRules merges a and b, in that order, dropping an entry
// of b whose context and app id (including both having no app id) exactly
// duplicate one already kept from a (D28).
func mergeRequiredCheckRules(a, b []RequiredCheck) []RequiredCheck {
	type key struct {
		context string
		appID   int64
		hasApp  bool
	}
	seen := make(map[key]bool, len(a)+len(b))
	out := make([]RequiredCheck, 0, len(a)+len(b))
	add := func(rc RequiredCheck) {
		k := key{context: rc.Context}
		if rc.AppID != nil {
			k.appID = *rc.AppID
			k.hasApp = true
		}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, rc)
	}
	for _, rc := range a {
		add(rc)
	}
	for _, rc := range b {
		add(rc)
	}
	return out
}

// ListReviews calls PullRequests.ListReviews, paginated to completion
// (job.ReviewThreads.ListReviews, M4; PKG9-PLAN.md section 10.3).
func (g *GitHubClient) ListReviews(ctx context.Context, owner, repo string, number int) ([]Review, error) {
	opts := &github.ListOptions{PerPage: ghPerPage}

	var out []Review
	for {
		reviews, resp, err := g.c.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, classifyGitHubErr(err)
		}
		for _, r := range reviews {
			out = append(out, Review{
				Login:    r.GetUser().GetLogin(),
				UserType: r.GetUser().GetType(),
				State:    r.GetState(),
				CommitID: r.GetCommitID(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// RequestReviewers calls PullRequests.RequestReviewers with the one login
// (job.ReviewThreads.RequestReviewers, M4; PKG9-PLAN.md section 10.3).
// GitHub ignores a login that is already a requested reviewer, so this is
// safe to repeat.
func (g *GitHubClient) RequestReviewers(ctx context.Context, owner, repo string, number int, login string) error {
	_, _, err := g.c.PullRequests.RequestReviewers(ctx, owner, repo, number, github.ReviewersRequest{Reviewers: []string{login}})
	if err != nil {
		return classifyGitHubErr(err)
	}
	return nil
}

// Viewer calls Users.Get(ctx, "") and returns the authenticated login
// (job.ReviewThreads.Viewer, M4; PKG9-PLAN.md section 10.3, D10): the
// token's own owner, cached by every caller that needs it more than once.
func (g *GitHubClient) Viewer(ctx context.Context) (string, error) {
	u, _, err := g.c.Users.Get(ctx, "")
	if err != nil {
		return "", classifyGitHubErr(err)
	}
	return u.GetLogin(), nil
}

// CommentOnPR posts body as a plain conversation comment on the pull
// request (job.ReviewThreads.CommentOnPR, M4; PKG9-PLAN.md section 10.3).
// GitHub's issue comments endpoint also serves pull requests, since a pull
// request is an issue under the hood.
func (g *GitHubClient) CommentOnPR(ctx context.Context, owner, repo string, number int, body string) error {
	if _, _, err := g.c.Issues.CreateComment(ctx, owner, repo, number, github.IssueCommentRequest{Body: body}); err != nil {
		return classifyGitHubErr(err)
	}
	return nil
}

// maxLogLineBytes bounds one kept line of a job log (job.Checks.JobLogTail;
// PKG9-PLAN.md section 10.3): a longer line is cut and ends "[line cut]".
// It also sizes the bufio.Reader tailLog reads through, so no one line is
// ever buffered past this size.
const maxLogLineBytes = 64 * 1024

// maxLogTotalBytes bounds how much of a job log tailLog reads at all
// (PKG9-PLAN.md section 10.3): reading stops once this many bytes have been
// seen, and the tail then ends with the line "[log cut at 64 MiB]".
const maxLogTotalBytes = 64 * 1024 * 1024

// JobLogTail calls Actions.GetWorkflowJobLogs for the signed URL, then
// fetches it with a plain http.Client carrying no Authorization header --
// the signed URL carries its own signature, and the GitHub token must never
// reach the storage host that serves it (PKG9-PLAN.md section 10.3) -- and
// returns its last lines lines through tailLog.
func (g *GitHubClient) JobLogTail(ctx context.Context, owner, repo string, jobID int64, lines int) (string, error) {
	logURL, _, err := g.c.Actions.GetWorkflowJobLogs(ctx, owner, repo, jobID, 3)
	if err != nil {
		return "", classifyGitHubErr(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, logURL.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("orchestrator: job log tail: build request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("orchestrator: job log tail: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("orchestrator: job log tail: fetch: status %d", resp.StatusCode)
	}

	tail, err := tailLog(resp.Body, lines)
	if err != nil {
		return "", fmt.Errorf("orchestrator: job log tail: %w", err)
	}
	return tail, nil
}

// logGroupRunPrefix is an Actions "##[group]Run " step header.
const logGroupRunPrefix = "##[group]Run "

// logErrorPrefix is an Actions "##[error]" annotation line.
const logErrorPrefix = "##[error]"

// hasLogPrefix reports whether line starts with prefix, optionally after a
// single leading timestamp token and a space, as Actions prefixes raw job
// logs. It is the non-regexp equivalent of `^(\S+ )?` + prefix: tailLog
// calls it per line of a job log that can run into the millions, where
// under the race detector regexp's internal machine pool made the cost
// enough to blow past JobLogTail's client timeout (observed: a 64 MiB log
// of short lines took 33s under -race against 1.3s without it).
func hasLogPrefix(line, prefix string) bool {
	if strings.HasPrefix(line, prefix) {
		return true
	}
	sp := strings.IndexByte(line, ' ')
	return sp > 0 && strings.HasPrefix(line[sp+1:], prefix)
}

// tailLog reads r line by line and returns, by default, the last n lines
// joined with "\n" -- the fallback ring below. A line longer than
// maxLogLineBytes is cut to that many bytes and ends "[line cut]"; the
// rest of that line is still counted toward maxLogTotalBytes but not kept.
// Reading stops once maxLogTotalBytes have been seen, and the fallback
// result then ends with the line "[log cut at 64 MiB]". r's own
// bufio.Reader buffer is sized to maxLogLineBytes, so ReadLine returns
// isPrefix=true exactly when a line exceeds that size.
//
// Alongside the fallback ring, tailLog tracks the first failed step: a
// line starting with logGroupRunPrefix starts a new step, discarding any
// step seen so far that had no error. A line starting with logErrorPrefix
// marks the current step failed and flushes its pending lines (the lines
// seen since the step header or the last error, also capped at n) into
// that step's buffer. Once a step has failed, reading stops at the next
// step header, so a later step's output cannot replace it. If any step
// failed, tailLog returns that step's buffer -- from its header to its
// last error line, capped at n -- instead of the plain ring.
func tailLog(r io.Reader, n int) (string, error) {
	limited := &io.LimitedReader{R: r, N: maxLogTotalBytes + 1}
	br := bufio.NewReaderSize(limited, maxLogLineBytes)

	pushCapped := func(buf []string, s string) []string {
		buf = append(buf, s)
		if len(buf) > n {
			buf = buf[len(buf)-n:]
		}
		return buf
	}

	ring := make([]string, 0, n)
	var stepBuf, pending []string
	failedStepFound := false

	for {
		first, isPrefix, err := br.ReadLine()
		if len(first) > 0 || err == nil {
			line := string(first)
			if isPrefix {
				for isPrefix {
					_, isPrefix, err = br.ReadLine()
					if err != nil {
						break
					}
				}
				line += "[line cut]"
			}
			ring = pushCapped(ring, line)

			switch {
			case hasLogPrefix(line, logGroupRunPrefix):
				if failedStepFound {
					return strings.Join(stepBuf, "\n"), nil
				}
				stepBuf = []string{line}
				pending = nil
			case hasLogPrefix(line, logErrorPrefix):
				for _, p := range pending {
					stepBuf = pushCapped(stepBuf, p)
				}
				stepBuf = pushCapped(stepBuf, line)
				pending = nil
				failedStepFound = true
			default:
				pending = pushCapped(pending, line)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("read log: %w", err)
			}
			break
		}
	}

	if failedStepFound {
		return strings.Join(stepBuf, "\n"), nil
	}

	if limited.N == 0 {
		ring = pushCapped(ring, "[log cut at 64 MiB]")
	}

	return strings.Join(ring, "\n"), nil
}
