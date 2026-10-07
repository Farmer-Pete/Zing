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

// verifyHeadSignedBy fails the test unless HEAD in dir verifies as signed
// by the ed25519 key at keyPath: it derives the public key, writes a
// throwaway allowed-signers file naming it for zing-fixture@example.com
// (the identity every fixture commit uses), and runs "git verify-commit".
func verifyHeadSignedBy(t *testing.T, dir, keyPath string) {
	t.Helper()

	pubOut, err := exec.CommandContext(t.Context(), "ssh-keygen", "-y", "-f", keyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen -y: %v: %s", err, pubOut)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	line := "zing-fixture@example.com " + string(pubOut)
	if err := os.WriteFile(allowed, []byte(line), 0o600); err != nil {
		t.Fatalf("write allowed signers: %v", err)
	}
	if out, err := Git(t.Context(), dir, "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", "HEAD"); err != nil {
		t.Fatalf("verify-commit: %v\n%s", err, out)
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

	verifyHeadSignedBy(t, dest, wantKey)
}

// TestCopyRepoRewritesResolvedSigningKey pins CopyRepo's second try at
// rewriteSigningKey: when src's config names its key by a path that
// differs, as a string, from the src path CopyRepo is given, but
// resolves (filepath.EvalSymlinks) to the same key file, CopyRepo still
// finds and rewrites it. src's leaf directory is real (a symlinked leaf
// would stop copyTree's walk before CopyRepo ever reaches the config),
// but an ancestor directory is a symlink, so the given path and its
// resolved form are different strings naming the same file. Without the
// second try, a copy reached through such a path would keep signing
// with src's own key, which does not survive src's removal.
func TestCopyRepoRewritesResolvedSigningKey(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	srcReal := filepath.Join(base, "real")
	if err := os.Mkdir(srcReal, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := NewSigningRepo(t.Context(), srcReal); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}

	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatalf("symlink base: %v", err)
	}
	src := filepath.Join(link, "real")

	dest := filepath.Join(t.TempDir(), "repo")
	if err := CopyRepo(t.Context(), src, dest); err != nil {
		t.Fatalf("CopyRepo: %v", err)
	}

	wantKey := filepath.Join(dest, ".git", "zing-fixture-key")
	gotKey := strings.TrimSpace(readGit(t, dest, "config", "--get", "user.signingKey"))
	if gotKey != wantKey {
		t.Errorf("dest user.signingKey = %q, want %q", gotKey, wantKey)
	}

	if err := runGit(t.Context(), dest, "commit", "-q", "--allow-empty", "-S", "-m", "copy commit"); err != nil {
		t.Fatalf("commit in dest: %v", err)
	}
	verifyHeadSignedBy(t, dest, wantKey)
}

// TestCopyRepoRewritesSigningKeyThroughNestedSymlinks pins the fix for a
// host failure TestCopyRepoRewritesResolvedSigningKey could not catch in
// every environment: on a host whose TMPDIR itself has a symlinked
// ancestor (macOS's /var, a symlink to /private/var), src's config names
// its key by a path that is itself only partly resolved, one level short
// of src's fully resolved form. Neither of rewriteSigningKey's two literal
// tries (as CopyRepo was given src, or src fully resolved) matched that
// in-between string, so the copy kept signing with src's own key. This
// test builds that same two-level gap with its own symlinks, so it fails
// without relying on where the test binary's TMPDIR happens to sit.
func TestCopyRepoRewritesSigningKeyThroughNestedSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realBase := filepath.Join(root, "realbase")
	if err := os.Mkdir(realBase, 0o755); err != nil {
		t.Fatalf("mkdir realbase: %v", err)
	}

	aliasA := filepath.Join(root, "aliasA")
	if err := os.Symlink(realBase, aliasA); err != nil {
		t.Fatalf("symlink aliasA: %v", err)
	}

	// The repo is created through aliasA, one symlink away from realBase,
	// so its config names its key by that once-resolved path, not by
	// realBase directly.
	createPath := filepath.Join(aliasA, "real")
	if err := NewSigningRepo(t.Context(), createPath); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}

	// aliasB adds a second symlink on top of aliasA, so resolving src all
	// the way (as CopyRepo's second try does) lands on realBase, a string
	// equal to neither the literal src CopyRepo was given nor the
	// once-resolved path the config already names.
	aliasB := filepath.Join(root, "aliasB")
	if err := os.Symlink(aliasA, aliasB); err != nil {
		t.Fatalf("symlink aliasB: %v", err)
	}
	src := filepath.Join(aliasB, "real")

	dest := filepath.Join(t.TempDir(), "repo")
	if err := CopyRepo(t.Context(), src, dest); err != nil {
		t.Fatalf("CopyRepo: %v", err)
	}

	wantKey := filepath.Join(dest, ".git", "zing-fixture-key")
	gotKey := strings.TrimSpace(readGit(t, dest, "config", "--get", "user.signingKey"))
	if gotKey != wantKey {
		t.Errorf("dest user.signingKey = %q, want %q", gotKey, wantKey)
	}

	if err := runGit(t.Context(), dest, "commit", "-q", "--allow-empty", "-S", "-m", "copy commit"); err != nil {
		t.Fatalf("commit in dest: %v", err)
	}
	verifyHeadSignedBy(t, dest, wantKey)
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

// TestCopyRepoRelinksWorktree pins that CopyRepo also relinks a linked
// worktree so it works entirely under dest, with no reference back to src:
// git itself works in the copied worktree, the copied .git pointer has the
// shape the orchestrator's checkGitPointer requires, "git worktree list"
// in dest names only paths under dest, a signed commit in the copy
// verifies with the copied key, and src is left byte-for-byte untouched.
func TestCopyRepoRelinksWorktree(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	if err := NewSigningRepo(t.Context(), src); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}
	if err := runGit(t.Context(), src, "worktree", "add", "-q", "-b", "wt7", filepath.Join(".zing", "wt", "7")); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	srcWorktreeGitdir := filepath.Join(src, ".git", "worktrees", "7", "gitdir")
	srcGitdirBefore, err := os.ReadFile(srcWorktreeGitdir)
	if err != nil {
		t.Fatalf("read src worktree gitdir: %v", err)
	}

	dest := t.TempDir()
	if copyErr := CopyRepo(t.Context(), src, dest); copyErr != nil {
		t.Fatalf("CopyRepo: %v", copyErr)
	}
	destReal, err := filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatalf("resolve dest: %v", err)
	}

	destWorktree := filepath.Join(dest, ".zing", "wt", "7")
	if out, statusErr := Git(t.Context(), destWorktree, "status", "--porcelain"); statusErr != nil {
		t.Fatalf("git status in copied worktree: %v\n%s", statusErr, out)
	}

	commonDirOut := strings.TrimSpace(readGit(t, dest, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	wantPrefix := "gitdir: " + commonDirOut + "/worktrees/"
	gotPointer, err := os.ReadFile(filepath.Join(destWorktree, ".git"))
	if err != nil {
		t.Fatalf("read copied worktree .git: %v", err)
	}
	if !strings.HasPrefix(string(gotPointer), wantPrefix) {
		t.Errorf(".git pointer = %q, want prefix %q", gotPointer, wantPrefix)
	}

	listOut := readGit(t, dest, "worktree", "list", "--porcelain")
	for line := range strings.SplitSeq(listOut, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimPrefix(line, "worktree ")
		if !strings.HasPrefix(p, destReal) {
			t.Errorf("worktree list line %q names a path outside dest %q", line, destReal)
		}
	}

	if commitErr := runGit(t.Context(), destWorktree, "commit", "-q", "--allow-empty", "-S", "-m", "copy worktree commit"); commitErr != nil {
		t.Fatalf("commit in copied worktree: %v", commitErr)
	}
	wantKey := filepath.Join(dest, ".git", "zing-fixture-key")
	verifyHeadSignedBy(t, destWorktree, wantKey)

	srcGitdirAfter, err := os.ReadFile(srcWorktreeGitdir)
	if err != nil {
		t.Fatalf("read src worktree gitdir: %v", err)
	}
	if !bytes.Equal(srcGitdirBefore, srcGitdirAfter) {
		t.Errorf("CopyRepo modified src's worktree gitdir file")
	}
	if out, err := Git(t.Context(), filepath.Join(src, ".zing", "wt", "7"), "status", "--porcelain"); err != nil {
		t.Fatalf("git status in src worktree after copy: %v\n%s", err, out)
	}
	srcListOut := readGit(t, src, "worktree", "list", "--porcelain")
	if strings.Contains(srcListOut, destReal) {
		t.Errorf("src worktree list names a path under dest:\n%s", srcListOut)
	}
}

// TestCopyRepoRejectsWorktreeOutsideRepo pins that CopyRepo refuses to copy
// a repository whose linked worktree lives outside the repository: there is
// nothing under src to resolve that worktree's relative path against once
// it is copied elsewhere.
func TestCopyRepoRejectsWorktreeOutsideRepo(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	if err := NewSigningRepo(t.Context(), src); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside-worktree")
	if err := runGit(t.Context(), src, "worktree", "add", "-q", "-b", "wtout", outside); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "repo")
	err := CopyRepo(t.Context(), src, dest)
	if err == nil {
		t.Fatalf("CopyRepo: want error, got nil")
	}
	if !strings.Contains(err.Error(), "is outside") {
		t.Errorf("CopyRepo error = %q, want it to contain %q", err.Error(), "is outside")
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
