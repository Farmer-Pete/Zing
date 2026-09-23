package orchestrator

import "context"

// GitHub is the orchestrator's window onto the GitHub API. The real
// implementation wraps go-github v92; the e2e test injects a scripted fake,
// and github_test.go exercises the real implementation at the HTTP layer.
// Package 5 declares only the four methods it uses. Package 9 adds Comment,
// MarkReady, ResolveThread, RequestReviewers, and Merge.
//
// This file declares the interface only (PKG5-PLAN.md section 8.5, Task 1
// scope). The real ghClient implementation, wrapping go-github v92, is a
// later task -- it does not exist yet, so nothing in this package imports
// go-github.
type GitHub interface {
	// RepoDefaultBranch returns the repository's default branch name.
	RepoDefaultBranch(ctx context.Context, owner, repo string) (string, error)
	// RequiredChecks returns the required status-check contexts on the named
	// branch's protection. When the branch has no protection it returns an
	// empty slice and no error. Any other failure is an error.
	RequiredChecks(ctx context.Context, owner, repo, branch string) ([]string, error)
	// CreateDraftPR opens a draft pull request and returns its URL and number.
	CreateDraftPR(ctx context.Context, owner, repo, head, base, title, body string) (url string, number int, err error)
	// FindPRByHead returns the open PR whose head branch is head, or ok=false.
	FindPRByHead(ctx context.Context, owner, repo, head string) (url string, number int, ok bool, err error)
}
