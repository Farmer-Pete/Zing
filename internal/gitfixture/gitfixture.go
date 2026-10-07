// Package gitfixture builds a throwaway git repository with one real,
// signed commit, for tests and for `zing selftest` that need a project to
// build against without depending on the developer's own git identity or
// signing key (plan section 9.4). It is a non-test package: production
// code (zing selftest) and tests both call it.
package gitfixture

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"zing/internal/gitbin"
)

// signingKeyName is the file each fixture's own copy of the shared signing
// key lives at, under the repository's own .git directory, so the key
// never leaves the repository it signs for.
const signingKeyName = "zing-fixture-key"

// templateOnce guards building the one template repository every
// NewSigningRepo call in this process copies. Each git command is a
// process spawn, and spawning from a race-instrumented test binary is
// slow (a fork copies its whole shadow address space), while a test
// binary can build hundreds of fixtures (internal/job seeds one per
// git-backed test). So the ssh-keygen and the nine git commands run once
// per binary, and NewSigningRepo copies the result with plain file
// writes. Every fixture still carries its own copy of the key at its
// usual repo-local path and its own repo-local config; only the bytes,
// and so the initial commit's sha, are shared (nothing asserts they
// differ between fixtures, and fixtures built in the same second already
// shared it, since an ed25519 signature is deterministic).
var (
	templateOnce sync.Once
	templateDir  string
	templateErr  error
)

// template returns the path to the process-wide template repository,
// building it on the first call. It lives under a directory os.MkdirTemp
// creates and this process never removes: it holds nothing but a
// one-commit repository and its disposable test-and-selftest signing key,
// so leaving it for the OS to reclaim is simpler than threading a cleanup
// hook through every caller.
func template() (string, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "zing-fixture-template-")
		if err != nil {
			templateErr = fmt.Errorf("gitfixture: temp dir for template repo: %w", err)
			return
		}
		// context.Background(), not a caller's ctx: this runs at most once
		// per process, and must not be left half-done by the first
		// caller's deadline or cancellation when later callers still need
		// the result.
		if err := buildSigningRepo(context.Background(), dir); err != nil {
			templateErr = err
			return
		}
		templateDir = dir
	})
	return templateDir, templateErr
}

// buildSigningRepo inits a git repository at dir on branch "main", writes
// a fresh ed25519 key to "<dir>/.git/zing-fixture-key", configures it
// through repo-local git config, and makes one signed commit.
func buildSigningRepo(ctx context.Context, dir string) error {
	if err := runGit(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		return err
	}

	keyPath := filepath.Join(dir, ".git", signingKeyName)
	out, err := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "zing-fixture", "-f", keyPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gitfixture: ssh-keygen: %w: %s", err, out)
	}

	config := [][2]string{
		{"user.name", "Zing Fixture"},
		{"user.email", "zing-fixture@example.com"},
		{"commit.gpgsign", "true"},
		{"gpg.format", "ssh"},
		{"user.signingKey", keyPath},
		// No automatic maintenance or gc: a commit would otherwise start
		// "git maintenance run --auto" in the background, and its
		// objects/maintenance.lock can appear and vanish while another test
		// copies this template (seen in CI on PR #59).
		{"maintenance.auto", "false"},
		{"gc.auto", "0"},
	}
	for _, kv := range config {
		if err := runGit(ctx, dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}

	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# fixture repo\n"), 0o600); err != nil {
		return fmt.Errorf("gitfixture: write README.md: %w", err)
	}
	if err := runGit(ctx, dir, "add", "README.md"); err != nil {
		return err
	}
	return runGit(ctx, dir, "commit", "-q", "-S", "-m", "initial commit")
}

// copyTree copies every directory and regular file under src to the same
// relative path under dest, keeping each one's permission bits. Anything
// else (a symlink, a socket) is an error: git init and one commit create
// neither. An entry that vanishes during the walk (a transient lock file)
// is skipped.
func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			content, err := os.ReadFile(path) //nolint:gosec // G304: path comes from walking this package's own template directory
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return os.WriteFile(target, content, info.Mode().Perm()) //nolint:gosec // G306,G703: target is under the caller's own dir, built from the template's relative paths, keeping git's own modes
		default:
			return fmt.Errorf("gitfixture: template entry %s is not a file or directory", rel)
		}
	})
}

// gitLocationEnv names the environment variables that redirect where git
// finds its repository, index, and object store (mirroring
// internal/orchestrator's own list). NewSigningRepo always runs git
// against an explicit working directory (cmd.Dir), so these are scrubbed
// from every git child's environment: inherited, most commonly when this
// runs from inside a git hook, they would override cmd.Dir and send a
// command at the wrong repository.
var gitLocationEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_COMMON_DIR",
	"GIT_PREFIX",
	"GIT_NAMESPACE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_CEILING_DIRECTORIES",
}

// Git runs one git command against dir with the repository-location
// variables scrubbed (gitLocationEnv) and returns its combined output. Test
// helpers in other packages use it in place of a bare exec.Command so a
// test that runs under a git hook (lefthook's pre-push exports GIT_DIR)
// cannot be redirected at the real repository. Found when the first
// Package 8 push failed every git-init test in three packages.
func Git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gitbin.Path(), args...) //nolint:gosec // G204: gitbin.Path() resolves the git binary itself, never caller input
	cmd.Dir = dir
	cmd.Env = Environ()
	return cmd.CombinedOutput()
}

// Environ is os.Environ() with the repository-location variables
// (gitLocationEnv) removed: the environment for a git child a test builds
// itself, when it needs more control than Git gives (stdout alone, or a
// sandboxed argv).
func Environ() []string {
	return scrubGitLocationEnv(os.Environ())
}

// NewSigningRepo makes dir a git repository on branch "main" with one
// signed commit. It copies this process's template repository (built
// once per binary, not per repo; see template), including its ed25519
// signing key at "<dir>/.git/zing-fixture-key", then points the copy's
// repo-local user.signingKey at that copy. All of its config is
// repository-local (user.name, user.email, commit.gpgsign, gpg.format,
// user.signingKey): it never runs "git config --global" and never writes
// outside dir.
func NewSigningRepo(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gitfixture: %w", err)
	}
	src, err := template()
	if err != nil {
		return err
	}
	if copyErr := copyTree(src, dir); copyErr != nil {
		return fmt.Errorf("gitfixture: copy template repo: %w", copyErr)
	}

	// git writes the key path into .git/config unquoted when it holds no
	// special characters; refuse a template whose config does not carry
	// it verbatim rather than leave the copy signing with the template's
	// own key file.
	configPath := filepath.Join(dir, ".git", "config")
	oldKey := filepath.Join(src, ".git", signingKeyName)
	newKey := filepath.Join(dir, ".git", signingKeyName)
	found, err := rewriteSigningKey(configPath, oldKey, newKey)
	if err != nil {
		return fmt.Errorf("gitfixture: %w", err)
	}
	if !found {
		return fmt.Errorf("gitfixture: template config does not name its key as %s", oldKey)
	}
	return nil
}

// rewriteSigningKey replaces the first "signingKey = OLD\n" line in the
// git config at configPath with "signingKey = NEW\n", so a copy of a
// signed repository signs with its own copy of the key rather than the
// source's. It reports whether that line was present: a config whose
// signingKey already names something else (or carries none) is left on
// disk untouched, and found is false.
func rewriteSigningKey(configPath, oldKey, newKey string) (found bool, err error) {
	config, err := os.ReadFile(configPath) //nolint:gosec // G304: configPath is under the caller's own dir
	if err != nil {
		return false, fmt.Errorf("read copied config: %w", err)
	}
	line := "signingKey = " + oldKey + "\n"
	if !strings.Contains(string(config), line) {
		return false, nil
	}
	rewritten := strings.Replace(string(config), line, "signingKey = "+newKey+"\n", 1)
	if writeErr := os.WriteFile(configPath, []byte(rewritten), 0o600); writeErr != nil { //nolint:gosec // G703: configPath is under the caller's own dir
		return false, fmt.Errorf("write copied config: %w", writeErr)
	}
	return true, nil
}

// gitDirOf returns root's git directory: "root/.git" when that is a
// directory, or root itself when root is a bare repository (no .git
// subdirectory, so root's own entries -- config, objects, refs -- are
// the git dir's entries).
func gitDirOf(root string) (string, error) {
	fi, err := os.Stat(filepath.Join(root, ".git"))
	if err == nil && fi.IsDir() {
		return filepath.Join(root, ".git"), nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return root, nil
}

// worktreeLink names one linked worktree found under a repository's git
// dir, by the admin directory's own name (the "NAME" in
// "worktrees/NAME") and the worktree's own directory, relative to the
// repository root (for example ".zing/wt/7"). worktreePaths resolves and
// validates both before any copy happens, so CopyRepo can relink the
// copy entirely in terms of dest, with no path naming src.
type worktreeLink struct {
	name string
	rel  string
}

// worktreePaths reads every linked worktree registered under gitDir
// (GITDIR/worktrees/NAME/gitdir) and returns each one's name and its
// worktree directory relative to srcReal (gitDir's resolved repository
// root). A worktree directory that no longer exists is skipped: its
// gitdir file names a worktree someone already removed. A worktree
// directory outside srcReal is an error, since CopyRepo has nothing to
// relink it against once src is gone.
func worktreePaths(srcReal, gitDir string) ([]worktreeLink, error) {
	worktreesDir := filepath.Join(gitDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var links []worktreeLink
	for _, entry := range entries {
		name := entry.Name()
		raw, readErr := os.ReadFile(filepath.Join(worktreesDir, name, "gitdir"))
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read worktree %s gitdir: %w", name, readErr)
		}

		target := strings.TrimSpace(string(raw))
		targetReal, resolveErr := filepath.EvalSymlinks(target)
		if errors.Is(resolveErr, fs.ErrNotExist) {
			continue
		}
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve worktree %s gitdir %s: %w", name, target, resolveErr)
		}

		rel, relErr := filepath.Rel(srcReal, filepath.Dir(targetReal))
		if relErr != nil {
			return nil, fmt.Errorf("relativize worktree %s: %w", name, relErr)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("worktree %s at %s is outside %s", name, targetReal, srcReal)
		}
		links = append(links, worktreeLink{name: name, rel: rel})
	}
	return links, nil
}

// CopyRepo copies the git repository (or bare repository) at src to
// dest, a path that does not yet exist, so a test can get its own
// private copy of a fixture built once per process instead of paying
// for git init, key generation, and a signed commit again. It rewrites
// the copy's user.signingKey, trying the key path named in src's config
// first as given and then resolved (filepath.EvalSymlinks), so it names
// the copy's own key under dest instead of src's. When src's config
// names no signingKey matching either form, the config is left alone.
// Every linked worktree under src (.zing/wt/TICKET-ID,
// .zing/judge/TICKET-ID) is relinked to work entirely under dest: see
// the package doc on worktreePaths and the relink step below for why
// this never runs "git worktree repair" and never writes under src.
func CopyRepo(ctx context.Context, src, dest string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gitfixture: %w", err)
	}

	srcReal, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("gitfixture: resolve src: %w", err)
	}
	srcGitDir, err := gitDirOf(src)
	if err != nil {
		return fmt.Errorf("gitfixture: stat src git dir: %w", err)
	}
	srcGitDirReal, err := gitDirOf(srcReal)
	if err != nil {
		return fmt.Errorf("gitfixture: stat src git dir: %w", err)
	}

	links, err := worktreePaths(srcReal, srcGitDirReal)
	if err != nil {
		return fmt.Errorf("gitfixture: %w", err)
	}

	if copyErr := copyTree(src, dest); copyErr != nil {
		return fmt.Errorf("gitfixture: copy repo: %w", copyErr)
	}

	destReal, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return fmt.Errorf("gitfixture: resolve dest: %w", err)
	}
	destGitDir, err := gitDirOf(dest)
	if err != nil {
		return fmt.Errorf("gitfixture: stat dest git dir: %w", err)
	}
	destGitDirReal, err := gitDirOf(destReal)
	if err != nil {
		return fmt.Errorf("gitfixture: stat dest git dir: %w", err)
	}

	configPath := filepath.Join(destGitDir, "config")
	newKey := filepath.Join(destGitDir, signingKeyName)
	oldKeyGiven := filepath.Join(srcGitDir, signingKeyName)
	found, err := rewriteSigningKey(configPath, oldKeyGiven, newKey)
	if err != nil {
		return fmt.Errorf("gitfixture: rewrite signing key: %w", err)
	}
	if !found {
		oldKeyResolved := filepath.Join(srcGitDirReal, signingKeyName)
		if _, err := rewriteSigningKey(configPath, oldKeyResolved, newKey); err != nil {
			return fmt.Errorf("gitfixture: rewrite signing key: %w", err)
		}
	}

	// Relink every worktree by writing both link files directly, naming
	// only destReal, rather than running "git worktree repair": while src
	// still exists, the copied .git file still names src's admin dir, and
	// repair would follow it and rewrite src's own reverse link to point
	// at dest, breaking src and racing any other copy made from it at the
	// same time.
	for _, link := range links {
		worktreeGit := filepath.Join(destReal, link.rel, ".git")
		adminGitdir := filepath.Join(destGitDirReal, "worktrees", link.name, "gitdir")

		worktreeGitContent := "gitdir: " + filepath.Join(destGitDirReal, "worktrees", link.name) + "\n"
		if writeErr := os.WriteFile(worktreeGit, []byte(worktreeGitContent), 0o644); writeErr != nil { //nolint:gosec // G306: the .git pointer file is always world-readable
			return fmt.Errorf("gitfixture: relink worktree %s: %w", link.name, writeErr)
		}
		adminGitdirContent := filepath.Join(destReal, link.rel, ".git") + "\n"
		if writeErr := os.WriteFile(adminGitdir, []byte(adminGitdirContent), 0o644); writeErr != nil { //nolint:gosec // G306: the admin gitdir file is always world-readable
			return fmt.Errorf("gitfixture: relink worktree %s: %w", link.name, writeErr)
		}
	}

	return nil
}

// AddFile writes relPath (with content) under dir and commits it, signed,
// on top of whatever NewSigningRepo already committed there: a caller that
// needs the fixture repo to carry one more path already at HEAD (so a
// ready cohort's code claim can cite it, or so building's own perimeter
// check never sees it as an undeclared change) calls this once, after
// NewSigningRepo, using the same repo-local signing config. relPath's
// parent directories are created as needed.
func AddFile(ctx context.Context, dir, relPath string, content []byte) error {
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("gitfixture: mkdir for %s: %w", relPath, err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		return fmt.Errorf("gitfixture: write %s: %w", relPath, err)
	}
	if err := runGit(ctx, dir, "add", relPath); err != nil {
		return err
	}
	if err := runGit(ctx, dir, "commit", "-q", "-S", "-m", "add "+relPath); err != nil {
		return err
	}
	return nil
}

// WithBareOrigin inits a bare repository as a sibling of dir (a fresh
// temporary directory under dir's own parent) and adds it as dir's
// "origin" remote, so a real git push -- Orchestrator.Push, OpenDraftPR --
// has somewhere real to land without ever reaching GitHub (PKG9-PLAN.md
// section 19.4 task 6; internal/orchestrator/push_test.go's own
// newBareRemote plus addOrigin, shared here now that internal/job's
// shipping tests need the same shape). It returns the bare repository's
// own directory, so a test can read back what a push landed there (for
// example "git show-ref").
func WithBareOrigin(ctx context.Context, dir string) (remoteDir string, err error) {
	remoteDir, err = os.MkdirTemp(filepath.Dir(dir), "zing-fixture-origin-")
	if err != nil {
		return "", fmt.Errorf("gitfixture: temp dir for bare origin: %w", err)
	}
	if err := runGit(ctx, remoteDir, "init", "-q", "--bare", "-b", "main"); err != nil {
		return "", err
	}
	if err := runGit(ctx, dir, "remote", "add", "origin", remoteDir); err != nil {
		return "", err
	}
	return remoteDir, nil
}

// runGit runs one git command against dir with the location-redirecting
// environment variables scrubbed, so it can never be sent at a
// repository other than dir.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, gitbin.Path(), args...) //nolint:gosec // G204: gitbin.Path() resolves the git binary itself, never caller input
	cmd.Dir = dir
	cmd.Env = Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gitfixture: git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

// scrubGitLocationEnv drops every gitLocationEnv variable from env.
func scrubGitLocationEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, key := range gitLocationEnv {
			if strings.HasPrefix(kv, key+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
