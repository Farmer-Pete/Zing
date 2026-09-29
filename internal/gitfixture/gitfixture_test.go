package gitfixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// readGit runs a real git command against dir, for reading back what
// NewSigningRepo left behind, failing the test on any error. It is named
// apart from the package's own runGit (gitfixture.go), which returns an
// error rather than output, for the test's own use.
func readGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestNewSigningRepoFirstCommitIsSigned pins the shape NewSigningRepo
// promises: a repo on branch "main" whose first commit carries a "gpgsig"
// header, produced with a key it generated for itself, entirely through
// repo-local config (plan section 9.4).
func TestNewSigningRepoFirstCommitIsSigned(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	if err := NewSigningRepo(t.Context(), resolved); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}

	branch := strings.TrimSpace(readGit(t, resolved, "branch", "--show-current"))
	if branch != "main" {
		t.Errorf("branch = %q, want %q", branch, "main")
	}

	commit := readGit(t, resolved, "cat-file", "-p", "HEAD")
	found := false
	for line := range strings.SplitSeq(commit, "\n") {
		if strings.HasPrefix(line, "gpgsig ") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("HEAD commit has no gpgsig header:\n%s", commit)
	}

	keyPath := filepath.Join(resolved, ".git", "zing-fixture-key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("signing key not written under .git: %v", err)
	}
}

// TestNewSigningRepoLeavesGlobalConfigAlone pins "it never touches global
// git config": with HOME pointed at a fresh, empty directory for the
// whole test, NewSigningRepo must leave that directory with no
// git-config file of its own, proving every "git config" call it made
// was repo-local (plan section 9.4).
func TestNewSigningRepoLeavesGlobalConfigAlone(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	if err := NewSigningRepo(t.Context(), resolved); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}

	for _, p := range []string{
		filepath.Join(fakeHome, ".gitconfig"),
		filepath.Join(fakeHome, ".config", "git", "config"),
	} {
		if _, statErr := os.Stat(p); statErr == nil {
			t.Errorf("NewSigningRepo created a global git config file at %s", p)
		}
	}
}
