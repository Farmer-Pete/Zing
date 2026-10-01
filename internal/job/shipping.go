// shipping.go declares the consumer-side interfaces the shipping state
// machine needs from GitHub, over orchestrator.GitHubClient's REST surface
// (PKG9-PLAN.md section 10.3). Declaring them here, not widening
// orchestrator.GitHub, keeps every existing fake of that four-method
// interface (cmd/zing/selftest.go, internal/dispatch/dispatch_test.go,
// internal/console/resume_e2e_test.go, the orchestrator tests) valid: a test
// of a handler that needs PullRequests or Checks implements only the small
// interface it uses. M3 task 6 adds PUBLISH, POLL, and the rest of the
// shipping handler to this file.
package job

import (
	"context"

	"zing/internal/orchestrator"
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
