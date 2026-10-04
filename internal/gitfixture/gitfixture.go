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
	config, err := os.ReadFile(configPath) //nolint:gosec // G304: configPath is under the caller's own dir
	if err != nil {
		return fmt.Errorf("gitfixture: read copied config: %w", err)
	}
	oldKey := filepath.Join(src, ".git", signingKeyName)
	if !strings.Contains(string(config), "signingKey = "+oldKey+"\n") {
		return fmt.Errorf("gitfixture: template config does not name its key as %s", oldKey)
	}
	newKey := filepath.Join(dir, ".git", signingKeyName)
	rewritten := strings.Replace(string(config), "signingKey = "+oldKey+"\n", "signingKey = "+newKey+"\n", 1)
	if writeErr := os.WriteFile(configPath, []byte(rewritten), 0o600); writeErr != nil { //nolint:gosec // G703: configPath is under the caller's own dir
		return fmt.Errorf("gitfixture: write copied config: %w", writeErr)
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
