package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/config"
)

// --- productionRuntimes (task 14, D2) ---------------------------------------

// TestProductionRuntimes_ResolvesClaudeAndCodexNotFake proves productionRuntimes
// resolves the two real runtimes, claude and codex, and never maps the fake
// name (design D2: "Production wires the two real runtimes, claude and
// codex"; PKG7-PLAN.md task 14). runtime.Set exposes no size, so this cannot
// prove the set holds ONLY these two; it asserts the two resolve and fake
// does not.
func TestProductionRuntimes_ResolvesClaudeAndCodexNotFake(t *testing.T) {
	t.Parallel()

	rts, err := productionRuntimes()
	if err != nil {
		t.Fatalf("productionRuntimes: %v", err)
	}

	if _, err := rts.For(runtimeNameClaude); err != nil {
		t.Errorf("For(%q) = %v, want it to resolve", runtimeNameClaude, err)
	}
	if _, err := rts.For(runtimeNameCodex); err != nil {
		t.Errorf("For(%q) = %v, want it to resolve", runtimeNameCodex, err)
	}
	if _, err := rts.For(runtimeNameFake); err == nil {
		t.Errorf("For(%q): want an error, production must never map %q", runtimeNameFake, runtimeNameFake)
	}
}

// --- productionTracker (task 14) --------------------------------------------

// productionTrackerFixtureConfig returns a minimal *config.Config carrying
// two projects, each with a valid "owner/name" repo -- config.Project.Repo's
// own shape, since "zing project add"'s splitOwnerRepo (cmd/zing/project.go)
// already validates and stores it that way -- enough for productionTracker
// to build a real *tracker.GitHubTracker with no network call
// (tracker.NewGitHub only parses and stores; internal/tracker/github.go).
func productionTrackerFixtureConfig() *config.Config {
	return &config.Config{
		GitHubToken: "test-github-token",
		Projects: []config.Project{
			{Name: testServeProjectName, Repo: "Farmer-Pete/Zing"},
			{Name: "other", Repo: "Farmer-Pete/Other"},
		},
	}
}

// TestProductionTracker_BuildsAGitHubTrackerFromConfiguredProjects proves
// productionTracker builds a real *tracker.GitHubTracker from cfg.GitHubToken
// and one "owner/name" repo per configured project, with no network call.
func TestProductionTracker_BuildsAGitHubTrackerFromConfiguredProjects(t *testing.T) {
	t.Parallel()

	tr, err := productionTracker(productionTrackerFixtureConfig())
	if err != nil {
		t.Fatalf("productionTracker: %v", err)
	}
	if tr == nil {
		t.Fatal("productionTracker returned a nil *tracker.GitHubTracker")
	}
}

// TestProductionTracker_EmptyTokenErrors proves an empty github_token fails
// construction (tracker.NewGitHub / go-github's own "token must not be
// empty"), rather than building a tracker that would only fail later, at its
// first real call.
func TestProductionTracker_EmptyTokenErrors(t *testing.T) {
	t.Parallel()

	cfg := productionTrackerFixtureConfig()
	cfg.GitHubToken = ""

	if _, err := productionTracker(cfg); err == nil {
		t.Error("productionTracker with an empty token: want an error, got nil")
	}
}

// --- NewFixture reference scope (task 14) -----------------------------------

// newFixtureAllowedFiles are the two non-test files NewFixture (the fixture
// tracker) may still be referenced from: cmd/zing/selftest.go, its one
// remaining caller (design D2, D11: selftest keeps the Fake and the Fixture
// tracker), and internal/tracker/fixture.go, which defines it (a definition
// is not a "reference" to it from elsewhere). Every other non-"_test.go" file
// under cmd/ and internal/ must never mention it: task 14 replaces
// serve.go's own construction with tracker.NewGitHub.
var newFixtureAllowedFiles = map[string]bool{
	"cmd/zing/selftest.go":        true,
	"internal/tracker/fixture.go": true,
}

// TestNewFixtureReferencedOnlyFromSelftestOrTests walks every ".go" file
// under the module's cmd/ and internal/ directories and fails if "NewFixture"
// appears in one that is neither a "_test.go" file nor listed in
// newFixtureAllowedFiles (PKG7-PLAN.md task 14: "NewFixture must now be
// referenced ONLY from selftest and _test.go files").
func TestNewFixtureReferencedOnlyFromSelftestOrTests(t *testing.T) {
	t.Parallel()

	const needle = "NewFixture"
	var offenders []string

	for _, dir := range []string{"../../cmd", "../../internal"} {
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			rel, err := filepath.Rel("../..", path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if newFixtureAllowedFiles[rel] {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(needle)) {
				offenders = append(offenders, rel)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("%s referenced outside selftest.go and _test.go files: %v", needle, offenders)
	}
}
