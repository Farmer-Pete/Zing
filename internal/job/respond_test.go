package job

import (
	"testing"

	"zing/internal/orchestrator"
)

// Compile-time assertion: *orchestrator.GitHubClient satisfies
// ReviewThreads, so serve can fill job.Project.Threads from the one shared
// client (PKG9-PLAN.md section 10.3). It lives here, not in
// internal/orchestrator/github_test.go, for the same import-cycle reason
// shipping_test.go's matching assertions do.
var _ ReviewThreads = (*orchestrator.GitHubClient)(nil)

// TestGitHubClientSatisfiesReviewThreads documents the assertion above: a
// failure here is a compile failure, not a test failure, so the body only
// has to exist.
func TestGitHubClientSatisfiesReviewThreads(t *testing.T) {
	t.Parallel()
}
