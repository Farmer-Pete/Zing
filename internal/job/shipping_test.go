package job

import (
	"testing"

	"zing/internal/orchestrator"
)

// Compile-time assertions: *orchestrator.GitHubClient satisfies every
// interface shipping.go declares, so serve can fill job.Project.PullRequests,
// job.Project.Flips, and job.Project.Checks from the one shared client
// (PKG9-PLAN.md section 10.3). They live here, not in
// internal/orchestrator/github_test.go: that file's tests are internal to
// package orchestrator, and package job already imports package
// orchestrator in its regular code, so an internal orchestrator test
// importing job would be an import cycle. respond_test.go holds the matching
// assertion for ReviewThreads, declared in respond.go.
var (
	_ PullRequests = (*orchestrator.GitHubClient)(nil)
	_ DraftFlips   = (*orchestrator.GitHubClient)(nil)
	_ Checks       = (*orchestrator.GitHubClient)(nil)
)

// TestGitHubClientSatisfiesInterfaces documents the var block above: a
// failure here is a compile failure, not a test failure, so the body only
// has to exist.
func TestGitHubClientSatisfiesInterfaces(t *testing.T) {
	t.Parallel()
}
