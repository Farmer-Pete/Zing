// respond.go declares the consumer-side interface the respond job needs
// from GitHub's GraphQL and REST surface (PKG9-PLAN.md section 9, 10.3).
// Declaring it here, not widening orchestrator.GitHub, follows shipping.go's
// pattern: a test of a handler that needs ReviewThreads implements only
// this interface. M4 task 4 adds RESPOND and the rest of the respond
// handler to this file.
package job

import (
	"context"

	"zing/internal/orchestrator"
)

// ReviewThreads is what the respond handler needs to read and act on a
// pull request's review threads (PKG9-PLAN.md section 9, 10.3, M4 task 1
// onward).
type ReviewThreads interface {
	// ListThreads lists every review thread of the pull request, each with
	// its last 100 comments (orchestrator.GitHubClient.ListThreads, GraphQL,
	// every page).
	ListThreads(ctx context.Context, owner, repo string, number int) ([]orchestrator.Thread, error)
	// ThreadCommentsContain pages every comment of the thread, not only the
	// last 100, and reports whether one authored by author contains needle
	// (orchestrator.GitHubClient.ThreadCommentsContain, GraphQL).
	ThreadCommentsContain(ctx context.Context, threadID, needle, author string) (bool, error)
	// ReplyToThread posts body as a reply in the thread
	// (orchestrator.GitHubClient.ReplyToThread, GraphQL).
	ReplyToThread(ctx context.Context, threadID, body string) error
	// ResolveThread resolves the thread
	// (orchestrator.GitHubClient.ResolveThread, GraphQL).
	ResolveThread(ctx context.Context, threadID string) error
	// ListReviews lists every review of the pull request
	// (orchestrator.GitHubClient.ListReviews, REST, every page).
	ListReviews(ctx context.Context, owner, repo string, number int) ([]orchestrator.Review, error)
	// RequestReviewers requests one reviewer by login
	// (orchestrator.GitHubClient.RequestReviewers, REST).
	RequestReviewers(ctx context.Context, owner, repo string, number int, login string) error
	// Viewer returns the authenticated login, the token's own owner
	// (orchestrator.GitHubClient.Viewer, REST, D10), cached by callers that
	// need it more than once.
	Viewer(ctx context.Context) (string, error)
}
