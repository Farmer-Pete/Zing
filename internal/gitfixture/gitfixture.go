// Package gitfixture builds a throwaway git repository with one real,
// signed commit, for tests and for `zing selftest` that need a project to
// build against without depending on the developer's own git identity or
// signing key (plan section 9.4). It is a non-test package: production
// code (zing selftest) and tests both call it.
package gitfixture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// signingKeyName is the file ssh-keygen writes the fixture's ed25519
// signing key to, under the repository's own .git directory, so the key
// never leaves the repository it signs for.
const signingKeyName = "zing-fixture-key"

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
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = scrubGitLocationEnv(os.Environ())
	return cmd.CombinedOutput()
}

// NewSigningRepo inits a git repository at dir on branch "main" with one
// signed commit. It generates a fresh ed25519 signing key with
// ssh-keygen under "<dir>/.git/zing-fixture-key" and configures it
// entirely through that repository's own local git config (user.name,
// user.email, commit.gpgsign, gpg.format, user.signingKey). It never
// runs "git config --global" and never writes outside dir.
func NewSigningRepo(ctx context.Context, dir string) error {
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
	if err := runGit(ctx, dir, "commit", "-q", "-S", "-m", "initial commit"); err != nil {
		return err
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

// runGit runs one git command against dir with the location-redirecting
// environment variables scrubbed, so it can never be sent at a
// repository other than dir.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = scrubGitLocationEnv(os.Environ())
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
