package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v92/github"
)

// GitHub is the orchestrator's window onto the GitHub API. The real
// implementation (ghClient, below) wraps go-github v92; the e2e test injects
// a scripted fake, and github_test.go exercises the real implementation at
// the HTTP layer. Package 5 declares only the four methods it uses. Package
// 9 adds Comment, MarkReady, ResolveThread, RequestReviewers, and Merge.
type GitHub interface {
	// RepoDefaultBranch returns the repository's default branch name.
	RepoDefaultBranch(ctx context.Context, owner, repo string) (string, error)
	// RequiredChecks returns the required status-check contexts on the named
	// branch's protection. When the branch has no protection it returns an
	// empty slice and no error. Any other failure is an error.
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

// ghClient is the real GitHub implementation, wrapping go-github v92.
type ghClient struct {
	c *github.Client
}

// NewGitHub builds the real client, authenticated with token, and returns an
// error because github.NewClient does: an empty token yields go-github's own
// "token must not be empty" (PKG5-PLAN.md section 13). The 30-second timeout
// is set with the WithTimeout client option rather than by mutating the
// client's *http.Client field directly: go-github v92's Client.Client()
// returns a defensive copy of that field, so assigning Timeout on it would
// have no effect on the requests the client actually sends.
func NewGitHub(token string) (GitHub, error) {
	c, err := github.NewClient(github.WithAuthToken(token), github.WithTimeout(ghHTTPTimeout))
	if err != nil {
		return nil, fmt.Errorf("orchestrator: new github client: %w", err)
	}
	return ghClient{c: c}, nil
}

// RepoDefaultBranch calls Repositories.Get and reads GetDefaultBranch().
func (g ghClient) RepoDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	repository, _, err := g.c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("orchestrator: repo default branch: %w", err)
	}
	return repository.GetDefaultBranch(), nil
}

// RequiredChecks calls Repositories.GetBranchProtection. An unprotected
// branch surfaces as errors.Is(err, github.ErrBranchNotProtected), which
// this method treats as an empty result and no error; any other failure
// (a missing repo or branch, a hidden authorization failure) is returned
// wrapped, so a misconfiguration never reads as "no checks". On success it
// unions the modern RequiredStatusChecks.Checks[].Context shape with the
// legacy RequiredStatusChecks.Contexts shape, deduplicated, preserving the
// order each context is first seen in (checks, then legacy contexts).
func (g ghClient) RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, error) {
	protection, _, err := g.c.Repositories.GetBranchProtection(ctx, owner, repo, branch)
	if err != nil {
		if errors.Is(err, github.ErrBranchNotProtected) {
			return nil, nil
		}
		return nil, fmt.Errorf("orchestrator: required checks: %w", err)
	}

	rsc := protection.GetRequiredStatusChecks()
	modern := rsc.GetChecks()
	legacy := rsc.GetContexts()

	seen := make(map[string]bool, len(modern)+len(legacy))
	checks := make([]string, 0, len(modern)+len(legacy))
	appendContext := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		checks = append(checks, name)
	}
	for _, c := range modern {
		appendContext(c.GetContext())
	}
	for _, name := range legacy {
		appendContext(name)
	}

	return checks, nil
}

// CreateDraftPR calls PullRequests.Create with Draft set true.
func (g ghClient) CreateDraftPR(ctx context.Context, owner, repo, head, base, title, body string) (url string, number int, err error) {
	pr, _, err := g.c.PullRequests.Create(ctx, owner, repo, github.CreatePullRequest{
		Title: new(title),
		Head:  head,
		Base:  base,
		Body:  new(body),
		Draft: new(true),
	})
	if err != nil {
		return "", 0, fmt.Errorf("orchestrator: create draft pr: %w", err)
	}
	return pr.GetHTMLURL(), pr.GetNumber(), nil
}

// FindPRByHead calls PullRequests.List with a Head filter of "owner:head", a
// Base filter of base, and State "open", returning the first match. The Base
// filter matters: without it, OpenDraftPR's fallback could return an open PR
// from wt.Branch into some other base entirely, not the default branch it
// meant to open one against.
func (g ghClient) FindPRByHead(ctx context.Context, owner, repo, head, base string) (url string, number int, ok bool, err error) {
	prs, _, err := g.c.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:  owner + ":" + head,
		Base:  base,
		State: "open",
	})
	if err != nil {
		return "", 0, false, fmt.Errorf("orchestrator: find pr by head: %w", err)
	}
	if len(prs) == 0 {
		return "", 0, false, nil
	}
	return prs[0].GetHTMLURL(), prs[0].GetNumber(), true, nil
}
