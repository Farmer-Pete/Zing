package job

import (
	"testing"

	"zing/internal/orchestrator"
)

// Compile-time assertions: *orchestrator.GitHubClient satisfies both
// interfaces shipping.go declares, so serve can fill job.Project.PullRequests
// and job.Project.Checks from the one shared client (PKG9-PLAN.md section
// 10.3). They live here, not in internal/orchestrator/github_test.go: that
// file's tests are internal to package orchestrator, and package job already
// imports package orchestrator in its regular code, so an internal
// orchestrator test importing job would be an import cycle. No GraphQL
// method exists yet (M4 task 1 adds DraftFlips and ReviewThreads).
var (
	_ PullRequests = (*orchestrator.GitHubClient)(nil)
	_ Checks       = (*orchestrator.GitHubClient)(nil)
)

// TestGitHubClientSatisfiesInterfaces documents the var block above: a
// failure here is a compile failure, not a test failure, so the body only
// has to exist.
func TestGitHubClientSatisfiesInterfaces(t *testing.T) {
	t.Parallel()
}
