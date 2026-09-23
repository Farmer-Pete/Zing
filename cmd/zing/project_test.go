package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"zing/internal/config"
	"zing/internal/orchestrator"
)

// discoveredBranch is the fake's default branch across the tests below that
// exercise the RepoDefaultBranch -> RequiredChecks passthrough. It is
// deliberately not "main", so a projectAdd bug that hardcoded "main" instead
// of using the discovered branch would fail these tests.
const discoveredBranch = "release"

// fakeGitHub is a minimal, scripted stand-in for orchestrator.GitHub, local
// to this file: internal/orchestrator's own scripted fake
// (fakegithub_test.go) lives in a file this package must not touch, since
// it is Package 5's concurrent orchestrator_test.go work.
type fakeGitHub struct {
	defaultBranch    string
	defaultBranchErr error
	// checksByBranch maps a branch name to the RequiredChecks result for
	// it, so a test can prove RequiredChecks is called with the branch
	// RepoDefaultBranch actually discovered, not some other hardcoded one.
	checksByBranch map[string][]string
	checksErr      error
}

var _ orchestrator.GitHub = fakeGitHub{}

func (f fakeGitHub) RepoDefaultBranch(_ context.Context, _, _ string) (string, error) {
	if f.defaultBranchErr != nil {
		return "", f.defaultBranchErr
	}
	return f.defaultBranch, nil
}

func (f fakeGitHub) RequiredChecks(_ context.Context, _, _, branch string) ([]string, error) {
	if f.checksErr != nil {
		return nil, f.checksErr
	}
	return f.checksByBranch[branch], nil
}

func (fakeGitHub) CreateDraftPR(_ context.Context, _, _, _, _, _, _ string) (url string, number int, err error) {
	return "", 0, errors.New("fakeGitHub: CreateDraftPR not scripted for project add")
}

func (fakeGitHub) FindPRByHead(_ context.Context, _, _, _ string) (url string, number int, ok bool, err error) {
	return "", 0, false, errors.New("fakeGitHub: FindPRByHead not scripted for project add")
}

// writeProjectlessConfig writes a zing.toml with user and github_token but
// zero projects -- the state LoadForAdd permits and Load rejects -- and
// returns its path.
func writeProjectlessConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zing.toml")
	const body = "user = \"peter\"\ngithub_token = \"test-github-token\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
	return path
}

// writeConfigWithProject writes a zing.toml already carrying one project
// named name, and returns its path.
func writeConfigWithProject(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zing.toml")
	body := fmt.Sprintf(`
user = "peter"
github_token = "test-github-token"

[[projects]]
name = %q
repo = "owner/existing"
path = "/tmp/existing"
tracker = "github"

[projects.commands]
test = "go test ./..."
lint = "golangci-lint run"
`, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
	return path
}

// projectAddArgs builds a valid "zing project add" argument list for repo
// and path, the two fields tests vary.
func projectAddArgs(name, repo, path string) []string {
	return []string{
		"--name", name,
		"--repo", repo,
		"--path", path,
		"--test", "go test ./...",
		"--lint", "golangci-lint run",
	}
}

// TestProjectAdd_WritesProjectWithDiscoveredDefaultBranch proves the happy
// path (PKG5-PLAN.md section 11's project_test.go row): a repo whose
// protection includes "ci" writes the project with the branch
// RepoDefaultBranch discovered, and this doubles as the "bootstraps from a
// project-less config" case, since writeProjectlessConfig starts at zero
// projects.
func TestProjectAdd_WritesProjectWithDiscoveredDefaultBranch(t *testing.T) {
	t.Parallel()

	cfgPath := writeProjectlessConfig(t)
	gh := fakeGitHub{
		defaultBranch:  discoveredBranch,
		checksByBranch: map[string][]string{discoveredBranch: {"lint", "ci"}},
	}
	dir := t.TempDir()

	err := projectAdd(t.Context(), cfgPath, gh, projectAddArgs("zing", "Farmer-Pete/Zing", dir))
	if err != nil {
		t.Fatalf("projectAdd: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after projectAdd: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects = %+v, want exactly one", cfg.Projects)
	}
	got := cfg.Projects[0]
	if got.Name != "zing" || got.Repo != "Farmer-Pete/Zing" || got.Path != dir ||
		got.DefaultBranch != discoveredBranch || got.Tracker != "github" ||
		got.Commands.Test != "go test ./..." || got.Commands.Lint != "golangci-lint run" {
		t.Errorf("saved project = %+v, want name=zing repo=Farmer-Pete/Zing path=%s default_branch=%s tracker=github", got, dir, discoveredBranch)
	}
}

// TestProjectAdd_MissingCIGivesExactErrorAndWritesNothing proves step 6's
// exact-string contract and its no-write guarantee.
func TestProjectAdd_MissingCIGivesExactErrorAndWritesNothing(t *testing.T) {
	t.Parallel()

	cfgPath := writeProjectlessConfig(t)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config before: %v", err)
	}

	gh := fakeGitHub{
		defaultBranch:  "main",
		checksByBranch: map[string][]string{"main": {"lint", "build"}}, // no "ci"
	}

	err = projectAdd(t.Context(), cfgPath, gh, projectAddArgs("zing", "Farmer-Pete/Zing", t.TempDir()))
	if err == nil {
		t.Fatal("projectAdd() = nil, want an error for a missing ci check")
	}
	const want = "branch protection missing required check ci"
	if err.Error() != want {
		t.Errorf("projectAdd() error = %q, want the exact string %q", err.Error(), want)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("zing.toml changed after a rejected projectAdd, want it untouched")
	}
}

// TestProjectAdd_DuplicateNameIsFatalAndWritesNothing proves step 3: a
// project whose name already exists is rejected before any GitHub call or
// write, per PKG5-PLAN.md section 13's edge-case table.
func TestProjectAdd_DuplicateNameIsFatalAndWritesNothing(t *testing.T) {
	t.Parallel()

	cfgPath := writeConfigWithProject(t, "zing")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config before: %v", err)
	}

	// A GitHub client that would fail the test if ever called: the
	// duplicate-name check must reject before step 4 reaches it.
	gh := fakeGitHub{defaultBranchErr: errors.New("must not be called for a duplicate name")}

	err = projectAdd(t.Context(), cfgPath, gh, projectAddArgs("zing", "Farmer-Pete/Zing", t.TempDir()))
	if err == nil {
		t.Fatal("projectAdd() = nil, want an error for a duplicate project name")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("zing.toml changed after a rejected duplicate-name projectAdd, want it untouched")
	}
}

// TestProjectAdd_RejectsRelativePath proves --path must be absolute
// (PKG5-PLAN.md section 10 step 2), before any GitHub call.
func TestProjectAdd_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	cfgPath := writeProjectlessConfig(t)
	gh := fakeGitHub{defaultBranchErr: errors.New("must not be called for an invalid flag set")}

	err := projectAdd(t.Context(), cfgPath, gh, projectAddArgs("zing", "Farmer-Pete/Zing", "relative/path"))
	if err == nil {
		t.Fatal("projectAdd() = nil, want an error for a relative --path")
	}
}

// TestProjectAdd_RejectsMalformedRepo proves --repo must split on exactly
// one "/".
func TestProjectAdd_RejectsMalformedRepo(t *testing.T) {
	t.Parallel()

	cfgPath := writeProjectlessConfig(t)
	gh := fakeGitHub{defaultBranchErr: errors.New("must not be called for a malformed repo")}

	err := projectAdd(t.Context(), cfgPath, gh, projectAddArgs("zing", "not-a-valid-repo", t.TempDir()))
	if err == nil {
		t.Fatal("projectAdd() = nil, want an error for a --repo with no owner/repo slash")
	}
}
