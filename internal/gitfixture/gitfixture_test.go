package gitfixture

import (
	"bytes"
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
	out, err := Git(t.Context(), dir, args...)
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

// TestCopyRepoSignsWithCopiedKey pins that CopyRepo's copy signs with its
// own key, not the source's: the copy's user.signingKey names a path
// under the copy, the source's own config is untouched, and a fresh
// signed commit made in the copy verifies against the copy's own public
// key.
func TestCopyRepoSignsWithCopiedKey(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	if err := NewSigningRepo(t.Context(), src); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}
	srcConfigBefore, err := os.ReadFile(filepath.Join(src, ".git", "config"))
	if err != nil {
		t.Fatalf("read src config: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "repo")
	if err = CopyRepo(t.Context(), src, dest); err != nil {
		t.Fatalf("CopyRepo: %v", err)
	}

	wantKey := filepath.Join(dest, ".git", "zing-fixture-key")
	gotKey := strings.TrimSpace(readGit(t, dest, "config", "--get", "user.signingKey"))
	if gotKey != wantKey {
		t.Errorf("dest user.signingKey = %q, want %q", gotKey, wantKey)
	}

	srcKey := strings.TrimSpace(readGit(t, src, "config", "--get", "user.signingKey"))
	wantSrcKey := filepath.Join(src, ".git", "zing-fixture-key")
	if srcKey != wantSrcKey {
		t.Errorf("src user.signingKey = %q, want %q (CopyRepo must not touch src)", srcKey, wantSrcKey)
	}
	srcConfigAfter, err := os.ReadFile(filepath.Join(src, ".git", "config"))
	if err != nil {
		t.Fatalf("read src config: %v", err)
	}
	if !bytes.Equal(srcConfigBefore, srcConfigAfter) {
		t.Errorf("CopyRepo modified src's config")
	}

	if err = runGit(t.Context(), dest, "commit", "-q", "--allow-empty", "-S", "-m", "copy commit"); err != nil {
		t.Fatalf("commit in dest: %v", err)
	}

	pubOut, err := exec.CommandContext(t.Context(), "ssh-keygen", "-y", "-f", wantKey).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen -y: %v: %s", err, pubOut)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	line := "zing-fixture@example.com " + string(pubOut)
	if err = os.WriteFile(allowed, []byte(line), 0o600); err != nil {
		t.Fatalf("write allowed signers: %v", err)
	}

	out, err := Git(t.Context(), dest, "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", "HEAD")
	if err != nil {
		t.Fatalf("verify-commit: %v\n%s", err, out)
	}
}

// TestCopyRepoCopiesBareRepo pins that CopyRepo also copies a bare
// repository (no .git subdirectory, no signing key), the shape the
// orchestrator's push target and a shipping stage's origin both use.
func TestCopyRepoCopiesBareRepo(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	if err := runGit(t.Context(), src, "init", "-q", "--bare", "-b", "main"); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "origin")
	if err := CopyRepo(t.Context(), src, dest); err != nil {
		t.Fatalf("CopyRepo: %v", err)
	}

	out := strings.TrimSpace(readGit(t, dest, "rev-parse", "--is-bare-repository"))
	if out != "true" {
		t.Errorf("rev-parse --is-bare-repository = %q, want %q", out, "true")
	}
}

// TestTemplateDisablesAutoMaintenance proves the template repository turns
// off git's automatic maintenance and gc, so no commit in it starts a
// background "git maintenance run --auto" whose objects/maintenance.lock
// could vanish while another test copies the template (seen in CI on PR
// #59).
func TestTemplateDisablesAutoMaintenance(t *testing.T) {
	t.Parallel()

	dir, err := template()
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	for key, want := range map[string]string{"maintenance.auto": "false", "gc.auto": "0"} {
		out, err := Git(t.Context(), dir, "config", "--get", key)
		if err != nil {
			t.Fatalf("git config --get %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
