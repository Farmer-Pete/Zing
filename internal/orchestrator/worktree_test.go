package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"zing/internal/gitfixture"
)

const (
	testOwner     = "acme"
	testRepo      = "widgets"
	mainBranch    = "main"
	absLocalPath  = "/tmp/widgets"
	branch7MySlug = "zing/7-my-slug"
	gitName       = "git"
	statusArg     = "status"
	configArg     = "config"
	ownDriverName = "own"
	gpgProgramKey = "gpg.program"
	hooksPathArg  = "core.hooksPath=/dev/null"
	fsmonitorArg  = "core.fsmonitor=false"
	driverZebra   = "zebra"
	driverAlpha   = "alpha"
)

// fakeGitHub is a no-op GitHub, enough to satisfy New's required parameter
// for tests in this file. None of them exercise a GitHub call.
type fakeGitHub struct{}

func (fakeGitHub) RepoDefaultBranch(context.Context, string, string) (branch string, err error) {
	return "", errors.New("fakeGitHub: not implemented")
}

func (fakeGitHub) RequiredChecks(context.Context, string, string, string) (checks []string, err error) {
	return nil, errors.New("fakeGitHub: not implemented")
}

func (fakeGitHub) CreateDraftPR(context.Context, string, string, string, string, string, string) (url string, number int, err error) {
	return "", 0, errors.New("fakeGitHub: not implemented")
}

func (fakeGitHub) FindPRByHead(context.Context, string, string, string, string) (url string, number int, ok bool, err error) {
	return "", 0, false, errors.New("fakeGitHub: not implemented")
}

// noCallRunner fails the test if either method is ever invoked. It proves a
// rejection happens before any git command runs.
type noCallRunner struct{ t *testing.T }

func (r noCallRunner) Run(_ context.Context, _, name string, args ...string) (string, error) {
	r.t.Fatalf("unexpected Run call: %s %s", name, strings.Join(args, " "))
	return "", nil
}

func (r noCallRunner) Output(_ context.Context, _, name string, args ...string) (string, error) {
	r.t.Fatalf("unexpected Output call: %s %s", name, strings.Join(args, " "))
	return "", nil
}

// failingRunner wraps a real Runner and forces an error for any command
// whose args slice fail reports true for. Every other command delegates to
// inner. It deterministically exercises PrepareWorktree's cleanup path
// without depending on a real git failure mode.
type failingRunner struct {
	inner Runner
	fail  func(args []string) bool
}

func (r failingRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if r.fail(args) {
		return "forced failure", errors.New("forced failure")
	}
	return r.inner.Run(ctx, dir, name, args...)
}

func (r failingRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	if r.fail(args) {
		return "", errors.New("forced failure")
	}
	return r.inner.Output(ctx, dir, name, args...)
}

// runGit runs a real git command directly (not through a Runner), for test
// setup and for independently verifying orchestrator behavior. It takes ctx
// as a parameter, like every git-invoking function in this package, rather
// than calling t.Context() itself: a call from inside a subtest closure
// that already has the enclosing test's ctx in scope should pass that one
// along, not silently swap in a context scoped to the subtest instead.
func runGit(ctx context.Context, t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Scrub inherited GIT_DIR and siblings so a test git command can never be
	// redirected at the real repository -- the same guarantee execRunner
	// gives production. This matters when the suite runs from inside a git
	// hook (for example lefthook's pre-push test-race), which exports GIT_DIR.
	cmd.Env = scrubGitLocationEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newTestRepo inits a real git repo in a fresh temp directory with a couple
// of commits on "main", across two subdirectories so sparse-checkout cone
// tests have something to include and exclude. It configures a throwaway
// local identity and disables signing at the repo level, so these tests
// never depend on -- or invoke -- the developer's own git identity or
// signing key, regardless of their global git config.
func newTestRepo(t *testing.T) string {
	t.Helper()

	ctx := t.Context()

	dir := t.TempDir()
	// Resolve symlinks (macOS's default TMPDIR is a symlink into
	// /private): PrepareWorktree and RemoveWorktree compare paths
	// literally against "git worktree list --porcelain" output, which
	// git reports in resolved form.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	runGit(ctx, t, resolved, "init", "-q", "-b", mainBranch)
	runGit(ctx, t, resolved, "config", "user.email", "zing-test@example.com")
	runGit(ctx, t, resolved, "config", "user.name", "Zing Test")
	runGit(ctx, t, resolved, "config", "commit.gpgsign", "false")

	writeTestFile(t, filepath.Join(resolved, "README.md"), "# test repo\n")
	runGit(ctx, t, resolved, "add", "README.md")
	runGit(ctx, t, resolved, "commit", "-q", "-m", "initial commit")

	writeTestFile(t, filepath.Join(resolved, "app", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(resolved, "docs", "extra.md"), "# extra\n")
	runGit(ctx, t, resolved, "add", "app/main.go", "docs/extra.md")
	runGit(ctx, t, resolved, "commit", "-q", "-m", "add app and docs")

	return resolved
}

func newTestOrchestrator(t *testing.T, localPath string, run Runner) *Orchestrator {
	t.Helper()
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: localPath, DefaultBranch: mainBranch}
	log := slog.New(slog.DiscardHandler)
	o, err := New(proj, fakeGitHub{}, run, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

func TestNew(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	valid := Project{Owner: testOwner, Repo: testRepo, LocalPath: absLocalPath, DefaultBranch: mainBranch}

	t.Run("valid project", func(t *testing.T) {
		t.Parallel()
		o, err := New(valid, fakeGitHub{}, execRunner{}, log)
		if err != nil {
			t.Fatalf("New: unexpected error: %v", err)
		}
		if diff := cmp.Diff(valid, o.proj); diff != "" {
			t.Errorf("proj mismatch (-want +got):\n%s", diff)
		}
	})

	cases := []struct {
		name string
		proj Project
	}{
		{"empty owner", Project{Repo: testRepo, LocalPath: absLocalPath, DefaultBranch: mainBranch}},
		{"empty repo", Project{Owner: testOwner, LocalPath: absLocalPath, DefaultBranch: mainBranch}},
		{"empty default branch", Project{Owner: testOwner, Repo: testRepo, LocalPath: absLocalPath}},
		{"relative local path", Project{Owner: testOwner, Repo: testRepo, LocalPath: testRepo, DefaultBranch: mainBranch}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(c.proj, fakeGitHub{}, execRunner{}, log)
			if err == nil {
				t.Fatalf("New(%+v): expected an error, got nil", c.proj)
			}
		})
	}

	// PR review finding: New used to return an orchestrator even when gh or
	// run was nil, which would panic the first time a method reached one of
	// them, rather than failing here at construction.
	t.Run("nil GitHub is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := New(valid, nil, execRunner{}, log); err == nil {
			t.Fatal("New: expected an error for a nil GitHub, got nil")
		}
	})

	t.Run("nil Runner is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := New(valid, fakeGitHub{}, nil, log); err == nil {
			t.Fatal("New: expected an error for a nil Runner, got nil")
		}
	})
}

func TestBranchName(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			ticketID int64
			slug     string
			want     string
		}{
			{"ticket only, no slug", 7, "", "zing/7"},
			{"lowercased", 7, "My Slug", branch7MySlug},
			{"punctuation collapsed to one dash, underscore kept", 7, "add!!the__thing", "zing/7-add-the__thing"},
			{"leading and trailing junk trimmed", 7, "--.foo.--", "zing/7-foo"},
			{"slug that fully sanitizes away falls back to ticket only", 7, "***", "zing/7"},
			{"dots kept inside the slug", 42, "v1.2.3", "zing/42-v1.2.3"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				got, err := branchName(c.ticketID, c.slug)
				if err != nil {
					t.Fatalf("branchName(%d, %q): unexpected error: %v", c.ticketID, c.slug, err)
				}
				if got != c.want {
					t.Errorf("branchName(%d, %q) = %q, want %q", c.ticketID, c.slug, got, c.want)
				}
			})
		}
	})

	t.Run("invalid ticket id", func(t *testing.T) {
		t.Parallel()
		for _, id := range []int64{0, -1, -100} {
			if _, err := branchName(id, "slug"); err == nil {
				t.Errorf("branchName(%d, \"slug\"): expected an error, got nil", id)
			}
		}
	})

	t.Run("a trailing .lock is rejected by checkRefFormat", func(t *testing.T) {
		t.Parallel()
		// "lock" is a legal slug character, so sanitizeSlug leaves it
		// untouched; the candidate matches zingBranchPattern but git's
		// own ref-name rule (no ref may end in ".lock") still rejects it.
		if _, err := branchName(7, "wip.lock"); err == nil {
			t.Fatal("branchName(7, \"wip.lock\"): expected an error, got nil")
		}
	})

	t.Run("a run of internal dots is rejected by checkRefFormat", func(t *testing.T) {
		t.Parallel()
		// sanitizeSlug only trims leading/trailing dots, so an internal
		// ".." survives to the candidate; git rejects two consecutive
		// dots anywhere in a ref name.
		if _, err := branchName(7, "a..b"); err == nil {
			t.Fatal("branchName(7, \"a..b\"): expected an error, got nil")
		}
	})
}

// TestCheckRefFormatMatchesGit proves checkRefFormat (the pure-Go ref-name
// check) agrees with real git on every case in this table: the oracle
// binary is exec.LookPath("git") until task 5 brings gitbin, which then
// becomes the binary of record.
func TestCheckRefFormatMatchesGit(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}

	cases := []struct {
		name      string
		ref       string
		wantValid bool
	}{
		{"zing branch", branch7MySlug, true},
		{"main", mainBranch, true},
		{"nested", "feature/x", true},
		{"internal dot", "a.b", true},
		{"bare at", "@", true},
		{"at not followed by brace", "a@b", true},
		{"non-ascii", "feature/ünï", true},
		{"lock as a substring, not a suffix", "x.lockx", true},
		{"empty", "", false},
		{"bare dot component", ".", false},
		{"bare dotdot", "..", false},
		{"internal dotdot", "a..b", false},
		{"component starting with dot", ".hidden", false},
		{"nested component starting with dot", "a/.b", false},
		{"ends in .lock", "x.lock", false},
		{"nested component ends in .lock", "a.lock/b", false},
		{"ends with dot", "end.", false},
		{"ends with slash", "end/", false},
		{"starts with slash", "/start", false},
		{"empty component", "a//b", false},
		{"space", "a b", false},
		{"tilde", "a~b", false},
		{"caret", "a^b", false},
		{"colon", "a:b", false},
		{"question mark", "a?b", false},
		{"asterisk", "a*b", false},
		{"open bracket", "a[b", false},
		{"backslash", "a\\b", false},
		{"at-brace sequence", "a@{b", false},
		{"tab", "tab\tx", false},
		{"del byte", "del\x7f", false},
		{"control byte", "ctl\x01", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			cmd := exec.CommandContext(ctx, gitPath, "check-ref-format", "refs/heads/"+c.ref) //nolint:gosec // argv-only, no shell; a fixed oracle binary and a table-driven test value
			cmd.Env = gitfixture.Environ()
			out, runErr := cmd.CombinedOutput()

			gitValid := runErr == nil
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("git check-ref-format refs/heads/%q: unexpected failure: %v: %s", c.ref, runErr, strings.TrimSpace(string(out)))
				}
			}

			if gitValid != c.wantValid {
				t.Fatalf("git check-ref-format refs/heads/%q: valid = %v, want %v (table is wrong)", c.ref, gitValid, c.wantValid)
			}

			gotValid := checkRefFormat(c.ref) == nil
			if gotValid != c.wantValid {
				t.Errorf("checkRefFormat(%q) valid = %v, want %v", c.ref, gotValid, c.wantValid)
			}
		})
	}
}

func TestRevalidate(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})
	ctx := t.Context()

	wt, err := o.PrepareWorktree(ctx, 1, "feature", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	t.Run("valid worktree passes", func(t *testing.T) {
		t.Parallel()
		if err := o.revalidate(ctx, wt); err != nil {
			t.Errorf("revalidate: unexpected error: %v", err)
		}
	})

	t.Run("non-zing branch is rejected", func(t *testing.T) {
		t.Parallel()
		bad := Worktree{dir: wt.dir, branch: "feature/not-zing"}
		if err := o.revalidate(ctx, bad); err == nil {
			t.Error("revalidate: expected an error for a non-zing branch, got nil")
		}
	})

	t.Run("the default branch is rejected", func(t *testing.T) {
		t.Parallel()
		bad := Worktree{dir: wt.dir, branch: mainBranch}
		if err := o.revalidate(ctx, bad); err == nil {
			t.Error("revalidate: expected an error for the default branch, got nil")
		}
	})

	t.Run("a worktree whose checked-out HEAD drifted is rejected", func(t *testing.T) {
		t.Parallel()
		// Its own worktree, not the shared wt above: this is the only
		// subtest that mutates its worktree's checked-out branch (the
		// others only read), and sharing wt with a parallel sibling that
		// reads it (valid worktree passes) would race.
		drifted, err := o.PrepareWorktree(ctx, 2, "drifted-source", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}
		runGit(ctx, t, drifted.dir, "checkout", "-b", "zing/1-drifted")
		if err := o.revalidate(ctx, drifted); err == nil {
			t.Error("revalidate: expected an error when HEAD no longer matches wt.branch, got nil")
		}
	})
}

func TestPrepareWorktree(t *testing.T) {
	t.Parallel()
	t.Run("creates the worktree and branch, full checkout", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, err := o.PrepareWorktree(ctx, 7, "my slug", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		wantDir := filepath.Join(repo, ".zing", "wt", "7")
		if wt.Dir() != wantDir {
			t.Errorf("Dir() = %q, want %q", wt.Dir(), wantDir)
		}
		if wt.Branch() != branch7MySlug {
			t.Errorf("Branch() = %q, want %q", wt.Branch(), branch7MySlug)
		}

		for _, rel := range []string{"README.md", "app/main.go", docsExtraPath} {
			if _, err := os.Stat(filepath.Join(wt.Dir(), rel)); err != nil {
				t.Errorf("expected %s to exist in a full checkout: %v", rel, err)
			}
		}

		head := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "symbolic-ref", "--short", "HEAD"))
		if head != wt.Branch() {
			t.Errorf("checked out branch = %q, want %q", head, wt.Branch())
		}

		branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch())
		if strings.TrimSpace(branches) == "" {
			t.Errorf("expected branch %q to exist in %s", wt.Branch(), repo)
		}
	})

	t.Run("applies a sparse cone", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, err := o.PrepareWorktree(ctx, 9, "cone", []string{"app"})
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		if _, err := os.Stat(filepath.Join(wt.Dir(), "app", "main.go")); err != nil {
			t.Errorf("expected app/main.go inside the cone: %v", err)
		}
		if _, err := os.Stat(filepath.Join(wt.Dir(), "README.md")); err != nil {
			t.Errorf("expected README.md at the cone root: %v", err)
		}
		if _, err := os.Stat(filepath.Join(wt.Dir(), "docs", "extra.md")); err == nil {
			t.Error("expected docs/extra.md to be excluded by the sparse cone, but it exists")
		}
	})

	t.Run("adds .zing/ to .git/info/exclude", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		if _, err := o.PrepareWorktree(ctx, 3, "", nil); err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		excludePath := filepath.Join(repo, ".git", "info", "exclude")
		contents, err := os.ReadFile(excludePath)
		if err != nil {
			t.Fatalf("read %s: %v", excludePath, err)
		}
		found := false
		for line := range strings.SplitSeq(string(contents), "\n") {
			if strings.TrimSpace(line) == worktreeExcludeLine {
				found = true
			}
		}
		if !found {
			t.Errorf(".git/info/exclude does not contain \".zing/\":\n%s", contents)
		}
	})

	t.Run("does not duplicate the exclude line on a second call", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		if _, err := o.PrepareWorktree(ctx, 4, "", nil); err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}
		if _, err := o.PrepareWorktree(ctx, 5, "", nil); err != nil {
			t.Fatalf("second PrepareWorktree: %v", err)
		}

		excludePath := filepath.Join(repo, ".git", "info", "exclude")
		contents, err := os.ReadFile(excludePath)
		if err != nil {
			t.Fatalf("read %s: %v", excludePath, err)
		}
		count := 0
		for line := range strings.SplitSeq(string(contents), "\n") {
			if strings.TrimSpace(line) == worktreeExcludeLine {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly one \".zing/\" line, found %d:\n%s", count, contents)
		}
	})

	t.Run("rejects an already-existing worktree directory", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		dir := filepath.Join(repo, ".zing", "wt", "11")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}

		_, err := o.PrepareWorktree(ctx, 11, "", nil)
		if err == nil {
			t.Fatal("PrepareWorktree: expected an error for an existing directory, got nil")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not name the path %q", err.Error(), dir)
		}

		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatalf("read dir %s: %v", dir, readErr)
		}
		if len(entries) != 0 {
			t.Errorf("PrepareWorktree must not touch a pre-existing directory, found %d entries", len(entries))
		}
	})

	t.Run("cleans up after a failure past worktree add", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		// The forced failure lands on the phase-2 config read
		// (readWorktreeGitConfig's FilterDrivers call, "git config
		// --get-regexp ..."), which runs after "git worktree add"
		// succeeds and before checkout -- exactly the "failure past
		// worktree add" this test is named for.
		failing := failingRunner{
			inner: execRunner{},
			fail: func(args []string) bool {
				return len(args) > 0 && args[0] == configArg
			},
		}
		o := newTestOrchestrator(t, repo, failing)
		ctx := t.Context()

		_, err := o.PrepareWorktree(ctx, 13, "boom", nil)
		if err == nil {
			t.Fatal("PrepareWorktree: expected an error, got nil")
		}

		dir := filepath.Join(repo, ".zing", "wt", "13")
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Errorf("expected %s to be removed after cleanup", dir)
		}

		branches := runGit(ctx, t, repo, "branch", "--list", "zing/13-boom")
		if strings.TrimSpace(branches) != "" {
			t.Errorf("expected branch zing/13-boom to be deleted after cleanup, branch --list said: %q", branches)
		}
	})

	// PR review finding P: the "zing/" prefix ordinarily keeps a ticket
	// branch structurally distinct from a repository's default branch, but
	// a project whose default branch itself happens to be named like a
	// zing/ ticket branch would otherwise slip past that assumption.
	// PrepareWorktree must reject this before touching git or the
	// filesystem, which noCallRunner and an unused LocalPath both prove.
	t.Run("rejects a computed branch that equals the default branch", func(t *testing.T) {
		t.Parallel()
		proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: absLocalPath, DefaultBranch: "zing/1"}
		log := slog.New(slog.DiscardHandler)
		o, err := New(proj, fakeGitHub{}, noCallRunner{t: t}, log)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if _, err := o.PrepareWorktree(t.Context(), 1, "", nil); err == nil {
			t.Fatal("PrepareWorktree: expected an error when the computed branch equals the default branch, got nil")
		}
	})

	// PR review finding: PrepareWorktree used to pass cone entries straight
	// through as argv to "git sparse-checkout set", so an entry beginning
	// with "-" would be parsed by git as an option rather than a path,
	// either erroring outright or being misinterpreted. Feeding cone through
	// stdin (runSparseCheckoutSet) means git never sees these as argv at
	// all, so a dash-prefixed path is included as a literal path like any
	// other.
	t.Run("a cone entry beginning with a dash is a literal path, not an option", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		ctx := t.Context()

		const dashDir = "-weird"
		writeTestFile(t, filepath.Join(repo, dashDir, "file.txt"), "dashed\n")
		runGit(ctx, t, repo, "add", "--", dashDir+"/file.txt")
		runGit(ctx, t, repo, "commit", "-q", "-m", "add a dash-prefixed directory")

		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 15, "dash", []string{dashDir})
		if err != nil {
			t.Fatalf("PrepareWorktree: unexpected error for a dash-prefixed cone entry: %v", err)
		}

		if _, err := os.Stat(filepath.Join(wt.Dir(), dashDir, "file.txt")); err != nil {
			t.Errorf("expected %s/file.txt inside the cone: %v", dashDir, err)
		}
	})

	// PR review finding: ensureWorktreeExclude used to hardcode
	// "<LocalPath>/.git/info/exclude", which cannot work when LocalPath is
	// itself a git worktree (".git" there is a file pointing at the real,
	// shared gitdir, not a directory). Resolving through
	// "git rev-parse --git-path info/exclude" finds the repository's real,
	// shared info/exclude regardless of where LocalPath sits.
	t.Run("resolves info/exclude correctly when LocalPath is itself a linked worktree", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		ctx := t.Context()

		linkedDir := filepath.Join(t.TempDir(), "linked-worktree")
		runGit(ctx, t, repo, "worktree", "add", "-b", "linked-branch", linkedDir, mainBranch)

		info, statErr := os.Lstat(filepath.Join(linkedDir, ".git"))
		if statErr != nil {
			t.Fatalf("test setup: stat %s/.git: %v", linkedDir, statErr)
		}
		if info.IsDir() {
			t.Fatalf("test setup: expected %s/.git to be a file (a linked worktree), got a directory", linkedDir)
		}

		o := newTestOrchestrator(t, linkedDir, execRunner{})

		if _, err := o.PrepareWorktree(ctx, 30, "nested", nil); err != nil {
			t.Fatalf("PrepareWorktree from inside a linked worktree: %v", err)
		}

		excludePath := filepath.Join(repo, ".git", "info", "exclude")
		contents, err := os.ReadFile(excludePath)
		if err != nil {
			t.Fatalf("read the main checkout's shared %s: %v", excludePath, err)
		}
		found := false
		for line := range strings.SplitSeq(string(contents), "\n") {
			if strings.TrimSpace(line) == worktreeExcludeLine {
				found = true
			}
		}
		if !found {
			t.Errorf("expected the main checkout's shared info/exclude (info/exclude is shared across worktrees) to contain \".zing/\", got:\n%s", contents)
		}
	})
}

func TestRemoveWorktree(t *testing.T) {
	t.Parallel()
	t.Run("removes the worktree and branch", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, err := o.PrepareWorktree(ctx, 21, "gone", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		if err := o.RemoveWorktree(ctx, wt); err != nil {
			t.Fatalf("RemoveWorktree: %v", err)
		}

		if _, statErr := os.Stat(wt.Dir()); statErr == nil {
			t.Errorf("expected %s to be removed", wt.Dir())
		}
		branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch())
		if strings.TrimSpace(branches) != "" {
			t.Errorf("expected branch %q to be deleted, branch --list said: %q", wt.Branch(), branches)
		}
	})

	t.Run("is safe when the worktree is already gone but the branch remains", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, err := o.PrepareWorktree(ctx, 22, "half-gone", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		// Remove the worktree administratively through git directly,
		// bypassing RemoveWorktree, so only the branch is left behind.
		runGit(ctx, t, repo, "worktree", "remove", "--force", wt.Dir())

		if err := o.RemoveWorktree(ctx, wt); err != nil {
			t.Fatalf("RemoveWorktree: unexpected error for an already-removed worktree: %v", err)
		}

		branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch())
		if strings.TrimSpace(branches) != "" {
			t.Errorf("expected branch %q to be deleted, branch --list said: %q", wt.Branch(), branches)
		}
	})

	t.Run("rejects the default branch without removing anything", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, noCallRunner{t: t})
		ctx := t.Context()

		bad := Worktree{dir: filepath.Join(repo, ".zing", "wt", "99"), branch: mainBranch}
		if err := o.RemoveWorktree(ctx, bad); err == nil {
			t.Fatal("RemoveWorktree: expected an error for the default branch, got nil")
		}
	})

	t.Run("rejects a non-zing branch without removing anything", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, noCallRunner{t: t})
		ctx := t.Context()

		bad := Worktree{dir: filepath.Join(repo, ".zing", "wt", "99"), branch: "feature/not-zing"}
		if err := o.RemoveWorktree(ctx, bad); err == nil {
			t.Fatal("RemoveWorktree: expected an error for a non-zing branch, got nil")
		}
	})

	// PR review finding H: validateZingBranch used to check only the
	// zing/ pattern and the default branch, so a hand-crafted branch that
	// matches the pattern but is not a git-legal ref (never having run
	// through branchName's own check-ref-format layer) could still reach
	// RemoveWorktree's destructive paths. "lock" is a legal slug character,
	// so this branch matches zingBranchPattern, but git rejects a ref
	// ending in ".lock". checkRefFormat runs outside the Runner, so
	// noCallRunner still proves the rejection happens before any git
	// command goes through o.run.
	t.Run("rejects a zing/-shaped branch that check-ref-format rejects", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, noCallRunner{t: t})
		ctx := t.Context()

		bad := Worktree{dir: filepath.Join(repo, ".zing", "wt", "99"), branch: "zing/7-wip.lock"}
		if err := o.RemoveWorktree(ctx, bad); err == nil {
			t.Fatal("RemoveWorktree: expected an error for a git-invalid zing/ branch, got nil")
		}
	})

	// PR review finding: a worktree directory deleted outside git (an
	// "rm -rf", not "git worktree remove") leaves it registered in git's own
	// worktree administration under .git/worktrees/, which used to make
	// "git branch -D" refuse with "already checked out" even though nothing
	// is actually there. RemoveWorktree now always runs
	// "git worktree remove --force <wt.dir>" scoped to this one worktree,
	// which clears that stale registration on its own, so the branch still
	// gets deleted.
	t.Run("a worktree deleted outside git still gets its branch deleted", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, err := o.PrepareWorktree(ctx, 24, "rm-rf", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		if err := os.RemoveAll(wt.Dir()); err != nil {
			t.Fatalf("RemoveAll(%s): %v", wt.Dir(), err)
		}

		if err := o.RemoveWorktree(ctx, wt); err != nil {
			t.Fatalf("RemoveWorktree: unexpected error for a worktree deleted outside git: %v", err)
		}

		branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch())
		if strings.TrimSpace(branches) != "" {
			t.Errorf("expected branch %q to be deleted, branch --list said: %q", wt.Branch(), branches)
		}
	})

	// PR review finding: RemoveWorktree used to run "git worktree prune" over
	// the whole repository before removing wt.dir, which would silently
	// discard any other stale worktree registration in the repo -- for
	// example one on temporarily-unavailable network or removable storage --
	// not just the one being removed. RemoveWorktree now scopes its cleanup
	// to wt.dir alone, so removing one ticket's worktree must never disturb
	// an unrelated ticket's registration.
	t.Run("does not prune an unrelated registered worktree", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wtA, err := o.PrepareWorktree(ctx, 60, "a", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree(60): %v", err)
		}
		wtB, err := o.PrepareWorktree(ctx, 61, "b", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree(61): %v", err)
		}

		// Simulate wtB's worktree directory becoming unavailable (as if on
		// removable or network storage that dropped out) without git ever
		// being told: its registration under .git/worktrees/ survives, but
		// nothing is left on disk at wtB.Dir().
		if err := os.RemoveAll(wtB.Dir()); err != nil {
			t.Fatalf("RemoveAll(%s): %v", wtB.Dir(), err)
		}

		if err := o.RemoveWorktree(ctx, wtA); err != nil {
			t.Fatalf("RemoveWorktree(wtA): %v", err)
		}

		if _, statErr := os.Stat(wtA.Dir()); statErr == nil {
			t.Errorf("expected %s to be removed", wtA.Dir())
		}
		branchesA := runGit(ctx, t, repo, "branch", "--list", wtA.Branch())
		if strings.TrimSpace(branchesA) != "" {
			t.Errorf("expected branch %q to be deleted, branch --list said: %q", wtA.Branch(), branchesA)
		}

		// wtB's stale registration must survive: a global prune would have
		// silently discarded it. "git worktree list --porcelain" still
		// reports it, and its branch still exists, exactly as if it were on
		// storage waiting to come back.
		list := runGit(ctx, t, repo, "worktree", "list", "--porcelain")
		if !strings.Contains(list, wtB.Dir()) {
			t.Errorf("expected wtB's registration to survive removing wtA's worktree, worktree list:\n%s", list)
		}
		branchesB := runGit(ctx, t, repo, "branch", "--list", wtB.Branch())
		if strings.TrimSpace(branchesB) == "" {
			t.Errorf("expected branch %q to still exist (its worktree registration must not be pruned), branch --list said: %q", wtB.Branch(), branchesB)
		}
	})

	// PR review finding G: RemoveWorktree used to force-remove a present
	// worktree without checking what branch was actually checked out there.
	// A worktree directory whose HEAD was switched to some other branch
	// since Zing prepared it (repurposed out from under Zing) must be
	// refused, not force-removed.
	t.Run("refuses to remove a worktree whose HEAD was switched to another branch", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, err := o.PrepareWorktree(ctx, 23, "repurposed", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		runGit(ctx, t, wt.Dir(), "checkout", "-b", "some-other-branch")

		if err := o.RemoveWorktree(ctx, wt); err == nil {
			t.Fatal("RemoveWorktree: expected an error for a worktree checked out to another branch, got nil")
		}

		if _, statErr := os.Stat(wt.Dir()); statErr != nil {
			t.Errorf("expected the repurposed worktree to be left in place, stat err = %v", statErr)
		}
		branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch())
		if strings.TrimSpace(branches) == "" {
			t.Errorf("expected branch %q to still exist, branch --list said: %q", wt.Branch(), branches)
		}
	})
}

// cancelingFailingRunner wraps a real Runner, forces an error for any
// command args reports true for, and cancels cancel at the moment it does
// so -- so a caller reacting to that failure (PrepareWorktree calling
// cleanupWorktree) runs against an already-cancelled ctx. It exercises the
// fix that cleanupWorktree's own commands run on a context.WithoutCancel(ctx)
// detached from ctx's cancellation, mirroring commit_test.go's
// cancelAfterCommitRunner for resetAfterUnsignedCommit.
type cancelingFailingRunner struct {
	inner  Runner
	fail   func(args []string) bool
	cancel context.CancelFunc
}

func (r cancelingFailingRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if r.fail(args) {
		r.cancel()
		return "forced failure", errors.New("forced failure")
	}
	return r.inner.Run(ctx, dir, name, args...)
}

func (r cancelingFailingRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	if r.fail(args) {
		r.cancel()
		return "", errors.New("forced failure")
	}
	return r.inner.Output(ctx, dir, name, args...)
}

// TestPrepareWorktree_CleanupSurvivesCancelledContext proves the PR review
// fix to cleanupWorktree: it used to run "git worktree remove" and
// "git branch -D" on the same ctx PrepareWorktree was called with, so a ctx
// cancelled by the very failure that triggered cleanup (or one past its
// deadline) would leave the worktree directory and branch leaked, since
// cleanup could never run. ctx here is cancelled the instant the forced
// phase-2 config-read failure happens (the same call TestPrepareWorktree's
// "cleans up after a failure past worktree add" subtest forces), before
// cleanupWorktree runs -- yet the directory and branch still end up
// removed, since cleanupWorktree now runs on a context.WithoutCancel(ctx)
// detached from ctx's cancellation.
func TestPrepareWorktree_CleanupSurvivesCancelledContext(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	failing := cancelingFailingRunner{
		inner: execRunner{},
		fail: func(args []string) bool {
			return len(args) > 0 && args[0] == configArg
		},
		cancel: cancel,
	}
	o := newTestOrchestrator(t, repo, failing)

	_, err := o.PrepareWorktree(ctx, 40, "cancel", nil)
	if err == nil {
		t.Fatal("PrepareWorktree: expected an error, got nil")
	}

	dir := filepath.Join(repo, ".zing", "wt", "40")
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Errorf("expected %s to be removed by cleanup even under a cancelled ctx", dir)
	}

	branches := runGit(t.Context(), t, repo, "branch", "--list", "zing/40-cancel")
	if strings.TrimSpace(branches) != "" {
		t.Errorf("expected branch zing/40-cancel to be deleted by cleanup even under a cancelled ctx, branch --list said: %q", branches)
	}
}

// PR review finding B: worktreePresent compared "git worktree list
// --porcelain" paths (which git reports with symlinks resolved) against
// dir as passed in, unresolved. When o.proj.LocalPath is itself reached
// through a symlinked path component, PrepareWorktree's Worktree.dir is
// built from that unresolved path (a plain filepath.Join), so it would
// never match git's resolved form and RemoveWorktree would wrongly
// conclude the worktree was already gone, leaking it. This test
// deliberately builds the Orchestrator's LocalPath from a symlink rather
// than pre-resolving it, unlike every other test in this file.
func TestWorktreePresentAcrossASymlinkedLocalPath(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)

	parent := t.TempDir()
	link := filepath.Join(parent, "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	ctx := t.Context()
	o := newTestOrchestrator(t, link, execRunner{})

	wt, err := o.PrepareWorktree(ctx, 50, "symlinked", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	if err := o.RemoveWorktree(ctx, wt); err != nil {
		t.Fatalf("RemoveWorktree: unexpected error for a worktree reached through a symlinked local path: %v", err)
	}

	if _, statErr := os.Stat(wt.Dir()); statErr == nil {
		t.Errorf("expected %s to be removed, but it still exists (the leak worktreePresent's symlink fix prevents)", wt.Dir())
	}
	branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch())
	if strings.TrimSpace(branches) != "" {
		t.Errorf("expected branch %q to be deleted, branch --list said: %q", wt.Branch(), branches)
	}
}

// TestExecRunnerIgnoresInheritedGitDir proves the fix for the review's
// worktree-pollution incident: a git command must act on cmd.Dir, not on an
// inherited GIT_DIR. A git hook (lefthook's pre-push) exports GIT_DIR, and
// without scrubbing, every test and orchestrator git command was redirected
// at the real repository -- committing to it and running sparse-checkout on
// it. With scrubbing, an inherited GIT_DIR is ignored.
//
// Not parallel: it calls t.Setenv on GIT_DIR and GIT_WORK_TREE, which
// t.Parallel forbids.
func TestExecRunnerIgnoresInheritedGitDir(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	// Point GIT_DIR at an unrelated, bogus location for the whole process,
	// exactly as a git hook would export it.
	bogus := t.TempDir()
	t.Setenv("GIT_DIR", bogus)
	t.Setenv("GIT_WORK_TREE", bogus)

	wantDir, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatalf("resolve want git dir: %v", err)
	}
	resolve := func(label, raw string) {
		got, evalErr := filepath.EvalSymlinks(strings.TrimSpace(raw))
		if evalErr != nil {
			t.Fatalf("resolve %s git dir %q: %v", label, raw, evalErr)
		}
		if got != wantDir {
			t.Errorf("%s resolved git dir = %q, want %q (inherited GIT_DIR leaked through)", label, got, wantDir)
		}
	}

	resolve("runGit", runGit(ctx, t, repo, "rev-parse", "--absolute-git-dir"))

	// The production execRunner must give the same guarantee.
	out, err := execRunner{}.Output(ctx, repo, "git", "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatalf("execRunner git rev-parse: %v", err)
	}
	resolve("execRunner", out)
}

// TestScrubGitLocationEnv is a focused unit test of the env scrubber.
func TestScrubGitLocationEnv(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/somewhere/.git",
		"HOME=/home/x",
		"GIT_WORK_TREE=/somewhere",
		"GIT_INDEX_FILE=/somewhere/.git/index",
		"GIT_LITERAL_PATHSPECS=1",
		"LANG=C",
	}
	got := scrubGitLocationEnv(in)
	for _, kv := range got {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			t.Errorf("scrubGitLocationEnv kept a location var: %q", kv)
		}
	}
	// Non-location vars, including GIT_LITERAL_PATHSPECS, must survive.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/x", "GIT_LITERAL_PATHSPECS=1", "LANG=C"} {
		if !slices.Contains(got, want) {
			t.Errorf("scrubGitLocationEnv dropped %q", want)
		}
	}
}

// -----------------------------------------------------------------------
// hardenedGitArgs, execRunner's automatic hardening, NewRunner
// -----------------------------------------------------------------------

func TestHardenedGitArgs(t *testing.T) {
	t.Parallel()
	t.Run("no drivers", func(t *testing.T) {
		t.Parallel()
		got := hardenedGitArgs(nil, statusArg)
		want := []string{"-c", hooksPathArg, "-c", fsmonitorArg, statusArg}
		if !slices.Equal(got, want) {
			t.Errorf("hardenedGitArgs(nil, \"status\") = %q, want %q", got, want)
		}
	})

	t.Run("two drivers in sorted order", func(t *testing.T) {
		t.Parallel()
		got := hardenedGitArgs([]string{driverZebra, driverAlpha}, statusArg)
		want := []string{
			"-c", hooksPathArg, "-c", fsmonitorArg,
			"-c", "filter.alpha.clean=", "-c", "filter.alpha.smudge=", "-c", "filter.alpha.process=", "-c", "filter.alpha.required=false",
			"-c", "filter.zebra.clean=", "-c", "filter.zebra.smudge=", "-c", "filter.zebra.process=", "-c", "filter.zebra.required=false",
			statusArg,
		}
		if !slices.Equal(got, want) {
			t.Errorf("hardenedGitArgs(unsorted, \"status\") =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("does not mutate the caller's drivers slice", func(t *testing.T) {
		t.Parallel()
		drivers := []string{driverZebra, driverAlpha}
		_ = hardenedGitArgs(drivers, statusArg)
		if drivers[0] != driverZebra || drivers[1] != driverAlpha {
			t.Errorf("hardenedGitArgs mutated the caller's slice: %v", drivers)
		}
	})
}

// TestExecRunnerDisablesHooksAndFsmonitor proves execRunner.command applies
// hardenedGitArgs to every "git" invocation -- so every git call any
// execRunner makes, whatever the caller asked for, carries the hooks- and
// fsmonitor-disabling prefix -- and leaves a non-git command's argv alone.
func TestExecRunnerDisablesHooksAndFsmonitor(t *testing.T) {
	t.Parallel()
	t.Run("a git call's argv starts with the two -c pairs", func(t *testing.T) {
		t.Parallel()
		cmd := execRunner{}.command(t.Context(), absLocalPath, gitName, statusArg, "--porcelain")
		want := []string{gitName, "-c", hooksPathArg, "-c", fsmonitorArg, statusArg, "--porcelain"}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("cmd.Args = %q, want %q", cmd.Args, want)
		}
	})

	t.Run("drivers add their own -c pairs after the hooks pair", func(t *testing.T) {
		t.Parallel()
		cmd := execRunner{drivers: []string{ownDriverName}}.command(t.Context(), absLocalPath, gitName, "add", "-A")
		want := []string{
			gitName, "-c", hooksPathArg, "-c", fsmonitorArg,
			"-c", "filter.own.clean=", "-c", "filter.own.smudge=", "-c", "filter.own.process=", "-c", "filter.own.required=false",
			"add", "-A",
		}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("cmd.Args = %q, want %q", cmd.Args, want)
		}
	})

	t.Run("a non-git command is unchanged", func(t *testing.T) {
		t.Parallel()
		cmd := execRunner{}.command(t.Context(), absLocalPath, "echo", "hi")
		want := []string{"echo", "hi"}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("cmd.Args = %q, want %q", cmd.Args, want)
		}
	})
}

// TestNewRunner proves NewRunner returns the real Runner, with no filter
// driver overrides of its own -- a caller scoping a runner to one Worktree
// builds a fresh execRunner{drivers: wt's drivers} rather than relying on
// this shared one to carry them.
func TestNewRunner(t *testing.T) {
	t.Parallel()
	run, ok := NewRunner().(execRunner)
	if !ok {
		t.Fatalf("NewRunner() = %T, want execRunner", NewRunner())
	}
	if len(run.drivers) != 0 {
		t.Errorf("NewRunner().drivers = %v, want empty", run.drivers)
	}
}

// TestSparseCheckoutArgv proves runSparseCheckoutSet's argv
// (sparseCheckoutSetArgs) carries the hardening prefix, drivers included --
// "sparse-checkout set" materializes files exactly as "git checkout" does,
// so it needs the same driver override.
func TestSparseCheckoutArgv(t *testing.T) {
	t.Parallel()
	t.Run("no drivers", func(t *testing.T) {
		t.Parallel()
		got := sparseCheckoutSetArgs(nil)
		want := []string{"-c", hooksPathArg, "-c", fsmonitorArg, "sparse-checkout", "set", "--stdin"}
		if !slices.Equal(got, want) {
			t.Errorf("sparseCheckoutSetArgs(nil) = %q, want %q", got, want)
		}
	})

	t.Run("with drivers", func(t *testing.T) {
		t.Parallel()
		got := sparseCheckoutSetArgs([]string{ownDriverName})
		want := []string{
			"-c", hooksPathArg, "-c", fsmonitorArg,
			"-c", "filter.own.clean=", "-c", "filter.own.smudge=", "-c", "filter.own.process=", "-c", "filter.own.required=false",
			"sparse-checkout", "set", "--stdin",
		}
		if !slices.Equal(got, want) {
			t.Errorf("sparseCheckoutSetArgs([\"own\"]) = %q, want %q", got, want)
		}
	})
}

// -----------------------------------------------------------------------
// FilterDrivers, GitCommonDir
// -----------------------------------------------------------------------

func TestFilterDrivers(t *testing.T) {
	t.Parallel()
	t.Run("none configured", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})

		got, err := o.FilterDrivers(t.Context(), repo)
		if err != nil {
			t.Fatalf("FilterDrivers: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("FilterDrivers = %v, want empty", got)
		}
	})

	t.Run("two drivers, sorted", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		ctx := t.Context()
		runGit(ctx, t, repo, "config", "filter.zebra.clean", "zebra-clean")
		runGit(ctx, t, repo, "config", "filter.alpha.smudge", "alpha-smudge")
		o := newTestOrchestrator(t, repo, execRunner{})

		got, err := o.FilterDrivers(ctx, repo)
		if err != nil {
			t.Fatalf("FilterDrivers: %v", err)
		}
		want := []string{driverAlpha, driverZebra}
		if !slices.Equal(got, want) {
			t.Errorf("FilterDrivers = %v, want %v", got, want)
		}
	})

	t.Run("one driver defined at two config levels is reported once", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		ctx := t.Context()
		runGit(ctx, t, repo, "config", "extensions.worktreeConfig", "true")
		runGit(ctx, t, repo, "config", "filter.own.clean", "own-clean")
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 70, "worktreeconfig", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}
		runGit(ctx, t, wt.Dir(), "config", "--worktree", "filter.own.smudge", "own-smudge")

		got, err := o.FilterDrivers(ctx, wt.Dir())
		if err != nil {
			t.Fatalf("FilterDrivers: %v", err)
		}
		want := []string{ownDriverName}
		if !slices.Equal(got, want) {
			t.Errorf("FilterDrivers = %v, want %v (deduplicated across config.worktree and repo-local config)", got, want)
		}
	})
}

func TestGitCommonDir(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})

	got, err := o.GitCommonDir(t.Context())
	if err != nil {
		t.Fatalf("GitCommonDir: %v", err)
	}

	want, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatalf("resolve want: %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve got %q: %v", got, err)
	}
	if gotResolved != want {
		t.Errorf("GitCommonDir = %q, want %q", gotResolved, want)
	}
}

// countingRunner wraps a real execRunner, counting how many times Run or
// Output actually reached it, guarded by a mutex so concurrent callers
// (TestGitCommonDirCachedPerOrchestrator's eight goroutines) count safely.
type countingRunner struct {
	mu    sync.Mutex
	runs  int
	outs  int
	inner execRunner
}

func (r *countingRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()
	return r.inner.Run(ctx, dir, name, args...)
}

func (r *countingRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.outs++
	r.mu.Unlock()
	return r.inner.Output(ctx, dir, name, args...)
}

func (r *countingRunner) outputCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outs
}

// TestGitCommonDirCachedPerOrchestrator proves GitCommonDir resolves git at
// most once per Orchestrator, even when several goroutines make their first
// call at once, and that a failed first call (a canceled ctx) caches
// nothing, so the next, healthy call still resolves successfully.
func TestGitCommonDirCachedPerOrchestrator(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	want, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatalf("resolve want: %v", err)
	}

	t.Run("concurrent first calls run git once and log once", func(t *testing.T) {
		t.Parallel()
		run := &countingRunner{}
		o, logs := newTestOrchestratorCapturingLog(t, repo, run)

		const n = 8
		start := make(chan struct{})
		results := make(chan string, n)
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				<-start
				got, err := o.GitCommonDir(t.Context())
				if err != nil {
					t.Errorf("GitCommonDir: %v", err)
					return
				}
				results <- got
			})
		}
		close(start)
		wg.Wait()
		close(results)

		for got := range results {
			gotResolved, err := filepath.EvalSymlinks(got)
			if err != nil {
				t.Fatalf("resolve got %q: %v", got, err)
			}
			if gotResolved != want {
				t.Errorf("GitCommonDir = %q, want %q", gotResolved, want)
			}
		}

		got, err := o.GitCommonDir(t.Context())
		if err != nil {
			t.Fatalf("GitCommonDir (one more call): %v", err)
		}
		if gotResolved, evalErr := filepath.EvalSymlinks(got); evalErr != nil || gotResolved != want {
			t.Errorf("GitCommonDir (one more call) = %q, want %q", got, want)
		}

		if calls := run.outputCalls(); calls != 1 {
			t.Errorf("rev-parse reached the Runner %d times, want 1", calls)
		}

		records := findRecords(logs.records(t), "git common dir resolved")
		if len(records) != 1 {
			t.Fatalf("found %d \"git common dir resolved\" records, want 1", len(records))
		}
		if records[0]["local_path"] != repo {
			t.Errorf("record[local_path] = %v, want %v", records[0]["local_path"], repo)
		}
		if records[0]["dir"] == "" || records[0]["dir"] == nil {
			t.Errorf("record[dir] is empty, want the resolved common dir")
		}
	})

	t.Run("a canceled first call caches nothing, so the next call still succeeds", func(t *testing.T) {
		t.Parallel()
		o := newTestOrchestrator(t, repo, execRunner{})

		canceledCtx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := o.GitCommonDir(canceledCtx); err == nil {
			t.Fatal("GitCommonDir(canceled ctx): want an error, got nil")
		}

		got, err := o.GitCommonDir(t.Context())
		if err != nil {
			t.Fatalf("GitCommonDir(healthy ctx) after a prior failure: %v, want success", err)
		}
		if gotResolved, evalErr := filepath.EvalSymlinks(got); evalErr != nil || gotResolved != want {
			t.Errorf("GitCommonDir = %q, want %q", got, want)
		}
	})
}

// -----------------------------------------------------------------------
// EnsureWorktree
// -----------------------------------------------------------------------

func TestEnsureWorktree(t *testing.T) {
	t.Parallel()
	t.Run("absent creates a fresh worktree", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		wt, created, err := o.EnsureWorktree(ctx, 200, "ensure")
		if err != nil {
			t.Fatalf("EnsureWorktree: %v", err)
		}
		if !created {
			t.Error("created = false, want true (a fresh worktree)")
		}

		wantDir := filepath.Join(repo, ".zing", "wt", "200")
		if wt.Dir() != wantDir {
			t.Errorf("Dir() = %q, want %q", wt.Dir(), wantDir)
		}
		wantBranch := "zing/200-ensure"
		if wt.Branch() != wantBranch {
			t.Errorf("Branch() = %q, want %q", wt.Branch(), wantBranch)
		}
		if _, statErr := os.Stat(filepath.Join(wt.Dir(), "README.md")); statErr != nil {
			t.Errorf("expected a full checkout: %v", statErr)
		}
	})

	t.Run("present reopens with the branch read from the worktree, not slug", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		first, err := o.PrepareWorktree(ctx, 201, "original-slug", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		wt, created, err := o.EnsureWorktree(ctx, 201, "a-different-slug-now")
		if err != nil {
			t.Fatalf("EnsureWorktree: %v", err)
		}
		if created {
			t.Error("created = true, want false (the worktree was already present)")
		}
		if wt.Dir() != first.Dir() {
			t.Errorf("Dir() = %q, want %q", wt.Dir(), first.Dir())
		}
		if wt.Branch() != first.Branch() {
			t.Errorf("Branch() = %q, want the original %q (branch comes from the worktree, not the new slug)", wt.Branch(), first.Branch())
		}
	})

	t.Run("a foreign directory errors", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})
		ctx := t.Context()

		dir := filepath.Join(repo, ".zing", "wt", "202")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}

		if _, _, err := o.EnsureWorktree(ctx, 202, "foreign"); err == nil {
			t.Fatal("EnsureWorktree: expected an error for a directory git does not recognize as a worktree, got nil")
		}
	})

	// PR review finding F001: ensureWorktreePresent used to run
	// "git symbolic-ref" and "git config" (readWorktreeGitConfig) in dir
	// before ever checking the .git pointer, so a pre-existing worktree
	// whose pointer was rewritten still ran git commands against whatever
	// it now names before being refused. checkGitPointer must run first, on
	// the bare dir alone, so a rewritten pointer is refused before any git
	// call reads dir's HEAD or config: forbiddenArgsRunner fails the test
	// outright the instant a "symbolic-ref" or "config" call is attempted,
	// and the refused error text (not a config- or HEAD-read error) is the
	// second, independent proof.
	t.Run("a rewritten .git pointer is refused before git symbolic-ref or git config ever runs in the worktree", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		ctx := t.Context()
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 203, "present", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		if writeErr := os.WriteFile(filepath.Join(wt.Dir(), ".git"), []byte("gitdir: /somewhere/else\n"), 0o644); writeErr != nil {
			t.Fatalf("rewrite .git pointer: %v", writeErr)
		}

		spy := forbiddenArgsRunner{t: t, inner: execRunner{}, forbidden: map[string]bool{"symbolic-ref": true, configArg: true}}
		spyOrch := newTestOrchestrator(t, repo, spy)

		_, _, err = spyOrch.EnsureWorktree(ctx, 203, "present")
		if err == nil {
			t.Fatal("EnsureWorktree: expected an error for a rewritten .git pointer, got nil")
		}
		wantSubstr := fmt.Sprintf("not ticket %d's worktree", 203)
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("EnsureWorktree error = %q, want it to contain %q (the refused error)", err.Error(), wantSubstr)
		}
	})
}

// forbiddenArgsRunner wraps a real Runner and fails the test outright if any
// call's first argument is one forbidden names, delegating every other call
// to inner. It proves ensureWorktreePresent's ordering fix (review finding
// F001): checkGitPointer must refuse a rewritten .git pointer before
// "git symbolic-ref" or "git config" (readWorktreeGitConfig) ever run
// against a pre-existing worktree directory.
type forbiddenArgsRunner struct {
	t         *testing.T
	inner     Runner
	forbidden map[string]bool
}

func (r forbiddenArgsRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if len(args) > 0 && r.forbidden[args[0]] {
		r.t.Fatalf("unexpected %s call: %s %s", args[0], name, strings.Join(args, " "))
	}
	return r.inner.Run(ctx, dir, name, args...)
}

func (r forbiddenArgsRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	if len(args) > 0 && r.forbidden[args[0]] {
		r.t.Fatalf("unexpected %s call: %s %s", args[0], name, strings.Join(args, " "))
	}
	return r.inner.Output(ctx, dir, name, args...)
}

// TestEnsureWorktreeReattachesAfterDeletion proves EnsureWorktree's second
// table row: a worktree directory removed by hand (not through git) is
// re-attached to its existing branch, and the branch's landed commit
// survives.
func TestEnsureWorktreeReattachesAfterDeletion(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})
	ctx := t.Context()

	wt, err := o.PrepareWorktree(ctx, 210, "reattach", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), "landed.txt"), "already landed\n")
	runGit(ctx, t, wt.Dir(), "add", "landed.txt")
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "a landed commit")
	landedSHA := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	if removeErr := os.RemoveAll(wt.Dir()); removeErr != nil {
		t.Fatalf("RemoveAll(%s): %v", wt.Dir(), removeErr)
	}

	reattached, created, err := o.EnsureWorktree(ctx, 210, "reattach")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if !created {
		t.Error("created = false, want true (the directory was removed by hand and this call recreated it)")
	}
	if reattached.Branch() != wt.Branch() {
		t.Errorf("Branch() = %q, want the original %q", reattached.Branch(), wt.Branch())
	}
	if _, statErr := os.Stat(filepath.Join(reattached.Dir(), "landed.txt")); statErr != nil {
		t.Errorf("expected the landed commit's file to survive reattachment: %v", statErr)
	}

	gotSHA := strings.TrimSpace(runGit(ctx, t, reattached.Dir(), "rev-parse", "HEAD"))
	if gotSHA != landedSHA {
		t.Errorf("HEAD = %q after reattachment, want the original landed commit %q", gotSHA, landedSHA)
	}
}

func TestEnsureWorktreeTwoBranchesErrors(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})
	ctx := t.Context()

	runGit(ctx, t, repo, "branch", "zing/220-first", mainBranch)
	runGit(ctx, t, repo, "branch", "zing/220-second", mainBranch)

	if _, _, err := o.EnsureWorktree(ctx, 220, "whatever"); err == nil {
		t.Fatal("EnsureWorktree: expected an error when two local branches match the ticket, got nil")
	}
}

// -----------------------------------------------------------------------
// The git signing program check
// -----------------------------------------------------------------------

// TestSigningProgramCheck drives checkSigningPrograms's closed grammar
// (PKG8-PLAN.md section 7.2) directly against a real repo's local config,
// through EnsureWorktree so the check runs exactly as production calls it.
//
// Not parallel itself: two of its own subtests below call t.Setenv (PATH,
// HOME), which t.Parallel forbids for the whole ancestor chain. Its other
// subtests, which touch no environment variable, are parallel on their
// own.
func TestSigningProgramCheck(t *testing.T) { //nolint:tparallel // two of its own subtests below call t.Setenv, so the parent itself cannot call Parallel
	cases := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{name: "unset", key: "", wantErr: false},
		{
			name:  "a bare name PATH resolves outside any disallowed root",
			key:   gpgProgramKey,
			value: "ssh-keygen",
		},
		{
			name:    "an absolute path outside is allowed",
			key:     gpgProgramKey,
			value:   "/usr/bin/true",
			wantErr: false,
		},
		{
			name:    "node signer.js has a space, forbidden",
			key:     gpgProgramKey,
			value:   "node signer.js",
			wantErr: true,
		},
		{
			name:    "sh -c ./sign.sh has a space, forbidden",
			key:     gpgProgramKey,
			value:   "sh -c ./sign.sh",
			wantErr: true,
		},
		{
			name:    "a quoted path is forbidden",
			key:     gpgProgramKey,
			value:   `"/usr/bin/true"`,
			wantErr: true,
		},
		{
			name:    "a relative path is refused",
			key:     gpgProgramKey,
			value:   "bin/sign",
			wantErr: true,
		},
		{
			name:    "defaultKeyCommand as ssh-add -L is allowed",
			key:     "gpg.ssh.defaultKeyCommand",
			value:   "ssh-add -L",
			wantErr: false,
		},
		{
			name:    "defaultKeyCommand with an extra argument is refused",
			key:     "gpg.ssh.defaultKeyCommand",
			value:   "ssh-add -L -f keys",
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := newTestRepo(t)
			ctx := t.Context()
			if c.key != "" {
				runGit(ctx, t, repo, "config", c.key, c.value)
			}
			o := newTestOrchestrator(t, repo, execRunner{})

			_, _, err := o.EnsureWorktree(ctx, 300, "signing")
			if c.wantErr && err == nil {
				t.Fatalf("EnsureWorktree: expected an error for %s=%q, got nil", c.key, c.value)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("EnsureWorktree: unexpected error for %s=%q: %v", c.key, c.value, err)
			}
		})
	}

	// Not parallel: it calls t.Setenv("PATH", ...) below.
	t.Run("a bare name that PATH resolves into the repository is refused", func(t *testing.T) {
		repo := newTestRepo(t)
		ctx := t.Context()

		binDir := filepath.Join(repo, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", binDir, err)
		}
		scriptPath := filepath.Join(binDir, "zing-signer")
		writeTestFile(t, scriptPath, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(scriptPath, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", scriptPath, err)
		}

		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		runGit(ctx, t, repo, "config", gpgProgramKey, "zing-signer")

		o := newTestOrchestrator(t, repo, execRunner{})
		if _, _, err := o.EnsureWorktree(ctx, 301, "path-into-repo"); err == nil {
			t.Fatal("EnsureWorktree: expected an error for a bare name PATH resolves into the repository, got nil")
		}
	})

	t.Run("a symlink that resolves into the repository is refused", func(t *testing.T) {
		t.Parallel()
		repo := newTestRepo(t)
		ctx := t.Context()

		target := filepath.Join(repo, "real-signer")
		writeTestFile(t, target, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", target, err)
		}

		outside := t.TempDir()
		link := filepath.Join(outside, "signer-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		runGit(ctx, t, repo, "config", gpgProgramKey, link)

		o := newTestOrchestrator(t, repo, execRunner{})
		if _, _, err := o.EnsureWorktree(ctx, 302, "symlink-into-repo"); err == nil {
			t.Fatal("EnsureWorktree: expected an error for a symlink resolving into the repository, got nil")
		}
	})

	// This subtest alone touches HOME (t.Setenv, restored automatically):
	// signingProgramDisallowedRoots must read the real home directory to
	// name <HOME>/.claude/projects, so proving that root is refused needs a
	// controlled HOME rather than the developer's own.
	t.Run("an absolute program inside HOME/.claude/projects is refused", func(t *testing.T) {
		repo := newTestRepo(t)
		ctx := t.Context()

		home := t.TempDir()
		t.Setenv("HOME", home)

		projectsDir := filepath.Join(home, ".claude", "projects")
		if err := os.MkdirAll(projectsDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", projectsDir, err)
		}
		programPath := filepath.Join(projectsDir, "signer")
		writeTestFile(t, programPath, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(programPath, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", programPath, err)
		}

		runGit(ctx, t, repo, "config", gpgProgramKey, programPath)

		o := newTestOrchestrator(t, repo, execRunner{})
		if _, _, err := o.EnsureWorktree(ctx, 303, "home-projects"); err == nil {
			t.Fatal("EnsureWorktree: expected an error for a program inside HOME/.claude/projects, got nil")
		}
	})
}

// TestSigningCheckCoversBuildWritableRoots proves checkSigningPrograms
// refuses a signing program inside any of Project.BuildWritableRoots (task
// 8): the sandbox cache root and the mds folder, the two locations a
// sandboxed build run can also write (design section 15), on top of
// LocalPath and the Claude Code transcripts folder it already covered.
func TestSigningCheckCoversBuildWritableRoots(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()

	writableRoot := t.TempDir()
	programPath := filepath.Join(writableRoot, "signer")
	writeTestFile(t, programPath, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(programPath, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", programPath, err)
	}
	runGit(ctx, t, repo, "config", gpgProgramKey, programPath)

	log := slog.New(slog.DiscardHandler)
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: repo, DefaultBranch: mainBranch, BuildWritableRoots: []string{writableRoot}}
	o, err := New(proj, fakeGitHub{}, execRunner{}, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := o.EnsureWorktree(ctx, 304, "build-writable-root"); err == nil {
		t.Fatal("EnsureWorktree: expected an error for a program inside a BuildWritableRoots entry, got nil")
	}
}

// TestNewRejectsRelativeWritableRoot proves New validates every
// BuildWritableRoots entry is absolute, the same rule it already applies to
// LocalPath (task 8).
func TestNewRejectsRelativeWritableRoot(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: absLocalPath, DefaultBranch: mainBranch, BuildWritableRoots: []string{"relative/cache"}}

	if _, err := New(proj, fakeGitHub{}, execRunner{}, log); err == nil {
		t.Fatal("New with a relative BuildWritableRoots entry: want an error, got nil")
	}
}

// -----------------------------------------------------------------------
// The filter-driver marker technique
// -----------------------------------------------------------------------

// configureMarkerFilterDriver configures name's clean and smudge commands to
// touch marker before passing content through unchanged, entirely through
// repo-local config, so a test can prove a hardened git call never invokes
// it (marker stays absent) versus an unhardened one (marker appears).
func configureMarkerFilterDriver(ctx context.Context, t *testing.T, repo, name, marker string) {
	t.Helper()
	cmd := fmt.Sprintf("touch %s && cat", marker)
	runGit(ctx, t, repo, "config", "filter."+name+".clean", cmd)
	runGit(ctx, t, repo, "config", "filter."+name+".smudge", cmd)
}

// assertMarkerAbsent fails the test if marker exists.
func assertMarkerAbsent(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("expected the filter driver's marker to be absent at %s (the driver must never run), stat err = %v", marker, err)
	}
}

// TestPrepareWorktreeRunsNoSmudgeFilter proves the checkout PrepareWorktree
// runs at the end of both a full and a sparse creation carries wt's own
// filter drivers overridden to empty, so a configured smudge filter -- run
// naturally by an unhardened "git checkout" -- never executes.
func TestPrepareWorktreeRunsNoSmudgeFilter(t *testing.T) {
	t.Parallel()
	setUpFilteredRepo := func(t *testing.T) (repo, marker string, ctx context.Context) {
		t.Helper()
		repo = newTestRepo(t)
		ctx = t.Context()
		marker = filepath.Join(t.TempDir(), "marker")

		writeTestFile(t, filepath.Join(repo, "filtered", "tracked.txt"), "content\n")
		runGit(ctx, t, repo, "add", "filtered/tracked.txt")
		runGit(ctx, t, repo, "commit", "-q", "-m", "add tracked.txt, unfiltered")

		configureMarkerFilterDriver(ctx, t, repo, "own", marker)
		writeTestFile(t, filepath.Join(repo, ".gitattributes"), "*.txt filter=own\n")
		runGit(ctx, t, repo, "add", ".gitattributes")
		runGit(ctx, t, repo, "commit", "-q", "-m", "declare the filter for *.txt")

		// Declaring the filter over an already-tracked *.txt path makes
		// this plain, unhardened setup git itself re-run the clean filter
		// against tracked.txt once .gitattributes takes effect -- nothing
		// to do with PrepareWorktree. Clear that setup-time marker so what
		// remains proves what PrepareWorktree itself did.
		_ = os.Remove(marker)

		return repo, marker, ctx
	}

	t.Run("full checkout", func(t *testing.T) {
		t.Parallel()
		repo, marker, ctx := setUpFilteredRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 400, "nosmudge", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(wt.Dir(), "filtered", "tracked.txt")); statErr != nil {
			t.Errorf("expected tracked.txt to be checked out: %v", statErr)
		}
		assertMarkerAbsent(t, marker)
	})

	t.Run("sparse checkout", func(t *testing.T) {
		t.Parallel()
		repo, marker, ctx := setUpFilteredRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 401, "nosmudge-sparse", []string{"filtered"})
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(wt.Dir(), "filtered", "tracked.txt")); statErr != nil {
			t.Errorf("expected tracked.txt inside the cone: %v", statErr)
		}
		assertMarkerAbsent(t, marker)
	})
}

// TestWorktreeConfigIsReadInWorktree proves FilterDrivers and
// checkSigningPrograms read config as it takes effect inside the ticket
// worktree, not the main checkout: a filter driver defined only through an
// "onbranch:zing/**" conditional include (which only matches from a
// worktree checked out to a zing/ branch) and a signing program defined
// only through config.worktree (which only exists for one specific linked
// worktree) are both seen.
func TestWorktreeConfigIsReadInWorktree(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, 500, "worktreecfg", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	includePath := filepath.Join(t.TempDir(), "onbranch.gitconfig")
	writeTestFile(t, includePath, "[filter \"onbranchdriver\"]\n\tclean = cat\n\tsmudge = cat\n")
	runGit(ctx, t, repo, "config", "includeIf.onbranch:zing/**.path", includePath)

	fromWorktree, err := o.FilterDrivers(ctx, wt.Dir())
	if err != nil {
		t.Fatalf("FilterDrivers(worktree): %v", err)
	}
	if !slices.Contains(fromWorktree, "onbranchdriver") {
		t.Errorf("FilterDrivers(worktree) = %v, want it to include the includeIf-only driver %q", fromWorktree, "onbranchdriver")
	}

	fromMain, err := o.FilterDrivers(ctx, repo)
	if err != nil {
		t.Fatalf("FilterDrivers(main checkout): %v", err)
	}
	if slices.Contains(fromMain, "onbranchdriver") {
		t.Errorf("FilterDrivers(main checkout) = %v, want it NOT to see the onbranch-only driver (main is on %q, not zing/**)", fromMain, mainBranch)
	}

	runGit(ctx, t, repo, "config", "extensions.worktreeConfig", "true")
	runGit(ctx, t, wt.Dir(), "config", "--worktree", "gpg.program", "node signer.js")

	if _, _, err := o.EnsureWorktree(ctx, 500, "worktreecfg"); err == nil {
		t.Fatal("EnsureWorktree: expected an error for a config.worktree-only signing program that fails the grammar, got nil")
	}
}

// TestFailedCheckRemovesNewWorktree proves that when the signing-program
// check fails for a freshly created worktree, PrepareWorktree's cleanup
// removes both the directory and the branch it just created.
func TestFailedCheckRemovesNewWorktree(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	runGit(ctx, t, repo, "config", "gpg.program", "node signer.js")
	o := newTestOrchestrator(t, repo, execRunner{})

	_, err := o.PrepareWorktree(ctx, 600, "failed-check", nil)
	if err == nil {
		t.Fatal("PrepareWorktree: expected an error for a disallowed signing program, got nil")
	}

	dir := filepath.Join(repo, ".zing", "wt", "600")
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Errorf("expected %s to be removed after the failed check", dir)
	}
	branches := runGit(ctx, t, repo, "branch", "--list", "zing/600-failed-check")
	if strings.TrimSpace(branches) != "" {
		t.Errorf("expected the freshly created branch to be deleted, branch --list said: %q", branches)
	}
}

// TestRevalidateRejectsRewrittenGitPointer proves revalidate's new check: a
// worktree whose ".git" pointer file no longer names this repository's real
// gitdir is rejected on every later call.
func TestRevalidateRejectsRewrittenGitPointer(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})
	ctx := t.Context()

	wt, err := o.PrepareWorktree(ctx, 610, "gitpointer", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	if err := o.revalidate(ctx, wt); err != nil {
		t.Fatalf("revalidate: unexpected error for a freshly prepared worktree: %v", err)
	}

	pointerPath := filepath.Join(wt.Dir(), ".git")
	if err := os.WriteFile(pointerPath, []byte("gitdir: /somewhere/else\n"), 0o644); err != nil {
		t.Fatalf("rewrite .git pointer: %v", err)
	}

	if err := o.revalidate(ctx, wt); err == nil {
		t.Fatal("revalidate: expected an error for a rewritten .git pointer, got nil")
	}
}

// TestCheckGitPointerToleratesNumericSuffix proves review finding F018:
// when another registered worktree already claims a directory with the
// same basename as this ticket's own worktree dir, git appends a numeric
// suffix to this worktree's admin dir under worktrees/ (for example "51"
// instead of "5"). checkGitPointer must still accept the pointer, since it
// is git's own real pointer for this worktree, not a rewritten one.
func TestCheckGitPointerToleratesNumericSuffix(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()

	// Register an unrelated worktree elsewhere whose directory shares
	// ticket 900's own worktree basename ("900"), so that when
	// PrepareWorktree adds <repo>/.zing/wt/900, git must suffix that
	// admin dir's name under worktrees/ to avoid colliding with the one
	// already registered under this basename.
	otherParent := t.TempDir()
	otherDir := filepath.Join(otherParent, "900")
	runGit(ctx, t, repo, "worktree", "add", "-b", "unrelated-900", otherDir, mainBranch)

	o := newTestOrchestrator(t, repo, execRunner{})
	wt, err := o.PrepareWorktree(ctx, 900, "suffix", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	if _, err := o.ChangedPaths(ctx, wt); err != nil {
		t.Errorf("ChangedPaths: unexpected error for a worktree whose admin dir git suffixed: %v", err)
	}
}

// TestCheckGitPointerRejectsAnotherTicketsAdminDir proves checkGitPointer's
// reverse-link check: a pointer naming a real admin dir under worktrees/ --
// so it passes the prefix check -- but belonging to a different, unrelated
// worktree (its own "gitdir" reverse link names that other worktree, not
// this one) must still be rejected. Otherwise a pointer rewritten to name
// any other registered worktree's admin dir would slip past the F018 fix.
func TestCheckGitPointerRejectsAnotherTicketsAdminDir(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})

	other, err := o.PrepareWorktree(ctx, 901, "other", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree(901): %v", err)
	}
	wt, err := o.PrepareWorktree(ctx, 902, "victim", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree(902): %v", err)
	}

	otherPointer, err := os.ReadFile(filepath.Join(other.Dir(), ".git"))
	if err != nil {
		t.Fatalf("read other worktree's .git pointer: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wt.Dir(), ".git"), otherPointer, 0o644); err != nil {
		t.Fatalf("rewrite .git pointer: %v", err)
	}

	if err := o.revalidate(ctx, wt); err == nil {
		t.Fatal("revalidate: expected an error for a pointer naming another ticket's admin dir, got nil")
	}
}

// TestFilterDriverNeverRuns proves a configured filter driver whose command
// is a worktree-reachable script never runs, across every content-touching
// method that reads or changes a worktree: CommitTask, RevertPaths,
// ChangedPaths, and Hunk.
func TestFilterDriverNeverRuns(t *testing.T) {
	t.Parallel()
	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	ctx := t.Context()

	marker := filepath.Join(t.TempDir(), "marker")
	configureMarkerFilterDriver(ctx, t, repo, "own", marker)
	writeTestFile(t, filepath.Join(repo, ".gitattributes"), "*.txt filter=own\n")
	runGit(ctx, t, repo, "add", ".gitattributes")
	runGit(ctx, t, repo, "commit", "-q", "-m", "declare the filter for *.txt")

	o := newTestOrchestrator(t, repo, execRunner{})
	wt, err := o.PrepareWorktree(ctx, 620, "filterdriver", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	assertMarkerAbsent(t, marker)

	const trackedFile = "tracked.txt"
	writeTestFile(t, filepath.Join(wt.Dir(), trackedFile), "content one\n")

	changed, err := o.ChangedPaths(ctx, wt)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(changed) == 0 {
		t.Fatal("ChangedPaths: expected at least one change")
	}
	assertMarkerAbsent(t, marker)

	if _, commitErr := o.CommitTask(ctx, wt, []string{trackedFile}, CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}); commitErr != nil {
		t.Fatalf("CommitTask: %v", commitErr)
	}
	assertMarkerAbsent(t, marker)

	writeTestFile(t, filepath.Join(wt.Dir(), trackedFile), "content two\n")
	if revertErr := o.RevertPaths(ctx, wt, []Change{{Path: trackedFile, Code: Modified}}); revertErr != nil {
		t.Fatalf("RevertPaths: %v", revertErr)
	}
	assertMarkerAbsent(t, marker)

	writeTestFile(t, filepath.Join(wt.Dir(), trackedFile), "content three\n")
	hunk, err := o.Hunk(ctx, wt, Change{Path: trackedFile, Code: Modified})
	if err != nil {
		t.Fatalf("Hunk: %v", err)
	}
	if hunk == "" {
		t.Error("Hunk: expected a non-empty diff")
	}
	assertMarkerAbsent(t, marker)
}

// -----------------------------------------------------------------------
// fetchBase: PrepareWorktree and BranchCommits cut from and compare
// against origin's default branch, not the local one.
// -----------------------------------------------------------------------

// logCapture is a thread-safe io.Writer a test hands to
// slog.NewJSONHandler, so it can both let a background goroutine log
// concurrently (fetchBase's own lock-contention tests) and decode what was
// written so far at any point.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// records decodes every complete JSON line written so far into a map keyed
// by slog's own field names (time, level, msg, and every attr).
func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	raw := c.buf.String()
	c.mu.Unlock()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// contains reports whether s appears anywhere in the raw log buffer
// captured so far, for tests proving a secret or a remote path never
// reaches a log record.
func (c *logCapture) contains(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.buf.String(), s)
}

// newTestOrchestratorCapturingLog mirrors newTestOrchestrator, but with a
// JSON-handler logger writing into a logCapture a test can decode, instead
// of the silent slog.DiscardHandler every other test in this file uses. The
// handler's level is Debug, so a test can see GitCommonDir's and
// readWorktreeGitConfig's Debug-level records too.
func newTestOrchestratorCapturingLog(t *testing.T, localPath string, run Runner) (*Orchestrator, *logCapture) {
	t.Helper()
	logs := &logCapture{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: localPath, DefaultBranch: mainBranch}
	o, err := New(proj, fakeGitHub{}, run, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, logs
}

// findRecords returns every decoded record whose "msg" field equals msg.
func findRecords(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, rec := range records {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// Log field names assertRecord's callers below check repeatedly, pulled
// out as constants (goconst) rather than repeating the literal at each
// call site.
const (
	logFieldTicketID   = "ticket_id"
	logFieldSHA        = "sha"
	logFieldReason     = "reason"
	logFieldFetchedSHA = "fetched_sha"
)

// assertRecord compares rec against want key by key, reporting each
// mismatched field on its own line, instead of one combined boolean
// condition that only says "the record" was wrong without saying which
// field.
func assertRecord(t *testing.T, rec, want map[string]any) {
	t.Helper()
	for key, wantVal := range want {
		if gotVal := rec[key]; gotVal != wantVal {
			t.Errorf("record[%q] = %v, want %v (record = %v)", key, gotVal, wantVal, rec)
		}
	}
}

// cloneAndCommitUpstream clones remote into a fresh temp directory, commits
// relPath there with content, and pushes main back to remote, standing in
// for a second developer who has already pushed what the local checkout
// under test has not yet pulled. It returns the new commit's sha.
func cloneAndCommitUpstream(ctx context.Context, t *testing.T, remote, relPath, content, message string) string {
	t.Helper()
	tmp := t.TempDir()
	clone := filepath.Join(tmp, "clone")
	runGit(ctx, t, tmp, "clone", "-q", remote, clone)
	runGit(ctx, t, clone, "config", "user.email", "zing-test@example.com")
	runGit(ctx, t, clone, "config", "user.name", "Zing Test")
	runGit(ctx, t, clone, "config", "commit.gpgsign", "false")
	writeTestFile(t, filepath.Join(clone, relPath), content)
	runGit(ctx, t, clone, "add", relPath)
	runGit(ctx, t, clone, "commit", "-q", "-m", message)
	runGit(ctx, t, clone, "push", "-q", "origin", mainBranch)
	return strings.TrimSpace(runGit(ctx, t, clone, "rev-parse", "HEAD"))
}

// forEachRefEmpty reports whether "git for-each-ref <pattern>" lists
// nothing, for asserting a temporary fetch ref (or every refs/zing/ ref) was
// cleaned up or never created.
func forEachRefEmpty(ctx context.Context, t *testing.T, dir, pattern string) bool {
	t.Helper()
	return strings.TrimSpace(runGit(ctx, t, dir, "for-each-ref", pattern)) == ""
}

// TestPrepareWorktreeCutsFromFetchedBase proves the ticket's bug directly:
// a new ticket branch is cut from origin's default branch as of a fetch
// PrepareWorktree runs itself, not from whatever the owner's local main
// happens to be, and BranchCommits then lists only the ticket's own commit
// -- not the commit origin already had before the ticket branch existed.
func TestPrepareWorktreeCutsFromFetchedBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	localMainSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", mainBranch))
	upstreamSHA := cloneAndCommitUpstream(ctx, t, remote, "upstream.txt", "upstream\n", "add upstream.txt")

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, 730, "fetch", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	head := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
	if head != upstreamSHA {
		t.Fatalf("HEAD of new ticket branch = %s, want %s (origin is ahead of local main %s)", head, upstreamSHA, localMainSHA)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), "ticket.txt"), "ticket\n")
	runGit(ctx, t, wt.Dir(), "add", "ticket.txt")
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket commit")
	ticketSHA := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	shas, err := o.BranchCommits(ctx, wt)
	if err != nil {
		t.Fatalf("BranchCommits: %v", err)
	}
	if !slices.Equal(shas, []string{ticketSHA}) {
		t.Errorf("BranchCommits = %v, want [%s]", shas, ticketSHA)
	}

	gotLocalMain := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", mainBranch))
	if gotLocalMain != localMainSHA {
		t.Errorf("local main moved from %s to %s", localMainSHA, gotLocalMain)
	}

	if !forEachRefEmpty(ctx, t, repo, "refs/zing/fetch/") {
		t.Errorf("refs/zing/fetch/ not cleaned up")
	}

	if gotOriginMain := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/remotes/origin/main")); gotOriginMain != localMainSHA {
		t.Errorf("refs/remotes/origin/main = %s, want it to stay %s (only fetchBase's own TMP ref and refs/zing/base/ should move on a fetch)", gotOriginMain, localMainSHA)
	}

	if _, statErr := os.Stat(filepath.Join(repo, ".git", "FETCH_HEAD")); !os.IsNotExist(statErr) {
		t.Errorf("stat .git/FETCH_HEAD = %v, want it absent: fetchBase's fetch must not write the owner's shared FETCH_HEAD", statErr)
	}

	fetched := findRecords(logs.records(t), "fetched base")
	if len(fetched) != 1 {
		t.Fatalf("found %d \"fetched base\" records, want 1", len(fetched))
	}
	rec := fetched[0]
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if got, want := rec["ticket_id"], float64(730); got != want {
		t.Errorf("ticket_id = %v, want %v", got, want)
	}
	if rec["ref"] != "refs/zing/base/main" {
		t.Errorf("ref = %v, want refs/zing/base/main", rec["ref"])
	}
	if rec["sha"] != upstreamSHA {
		t.Errorf("sha = %v, want %s", rec["sha"], upstreamSHA)
	}
}

// gateRunner wraps a real Runner and, the first time a Run call's args
// matches match, blocks after delegating until release is closed --
// letting a test observe that the real command has already completed (via
// ready) before anything later happens. Every other call, and every call
// once this gate has already fired once, passes straight through.
type gateRunner struct {
	inner   Runner
	match   func(args []string) bool
	ready   chan struct{}
	release chan struct{}
	fired   atomic.Bool
}

func (r *gateRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := r.inner.Run(ctx, dir, name, args...)
	matched := err == nil && r.match(args)
	if matched && r.fired.CompareAndSwap(false, true) {
		close(r.ready)
		<-r.release
	}
	return out, err
}

func (r *gateRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Output(ctx, dir, name, args...)
}

// TestFetchBaseOlderFetchFinishingLastKeepsNewerBase proves fetchBase only
// ever advances refs/zing/base/<default> forward: a fetch for one ticket
// that started first, but whose own "git fetch" subprocess is held up
// (gateRunner) until a second, later-started fetch for another ticket has
// already advanced the base to a newer commit, must not move the base
// backward to the older commit it carried.
func TestFetchBaseOlderFetchFinishingLastKeepsNewerBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)
	sha1 := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", mainBranch))

	gate := &gateRunner{
		inner: execRunner{},
		match: func(args []string) bool { return len(args) > 0 && args[0] == "fetch" },
		ready: make(chan struct{}), release: make(chan struct{}),
	}
	o := newTestOrchestrator(t, repo, gate)

	type result struct {
		sha string
		err error
	}
	done := make(chan result, 1)
	go func() {
		sha, _, err := o.fetchBase(ctx, 740)
		done <- result{sha, err}
	}()

	<-gate.ready // ticket 740's own "git fetch" (carrying sha1) has completed, but fetchBase(740) is still paused before it can take commonMu.

	sha2 := cloneAndCommitUpstream(ctx, t, remote, "upstream2.txt", "v2\n", "advance origin again")

	got741, _, err := o.fetchBase(ctx, 741)
	if err != nil {
		t.Fatalf("fetchBase(741): %v", err)
	}
	if got741 != sha2 {
		t.Fatalf("fetchBase(741) = %s, want %s", got741, sha2)
	}
	if base := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main")); base != sha2 {
		t.Fatalf("refs/zing/base/main = %s after fetchBase(741), want %s", base, sha2)
	}

	close(gate.release)
	res := <-done
	if res.err != nil {
		t.Fatalf("fetchBase(740): %v", res.err)
	}
	if res.sha != sha2 {
		t.Errorf("fetchBase(740) = %s, want %s (the newer base already in place, even though its own fetch carried %s)", res.sha, sha2, sha1)
	}

	if base := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main")); base != sha2 {
		t.Errorf("refs/zing/base/main = %s after the older fetch finished last, want it to stay %s", base, sha2)
	}
	if !forEachRefEmpty(ctx, t, repo, "refs/zing/fetch/") {
		t.Errorf("refs/zing/fetch/ not cleaned up")
	}
}

// forcePushOrphanCommit clones remote, creates an orphan commit that shares
// no history with remote's current main, and force-pushes it to main --
// standing in for a history-rewriting force-push upstream that no longer
// descends from what this repository last fetched. It returns the new
// commit's sha.
func forcePushOrphanCommit(ctx context.Context, t *testing.T, remote, relPath, content, message string) string {
	t.Helper()
	tmp := t.TempDir()
	clone := filepath.Join(tmp, "clone")
	runGit(ctx, t, tmp, "clone", "-q", remote, clone)
	runGit(ctx, t, clone, "config", "user.email", "zing-test@example.com")
	runGit(ctx, t, clone, "config", "user.name", "Zing Test")
	runGit(ctx, t, clone, "config", "commit.gpgsign", "false")
	runGit(ctx, t, clone, "checkout", "-q", "--orphan", "diverged")
	writeTestFile(t, filepath.Join(clone, relPath), content)
	runGit(ctx, t, clone, "add", relPath)
	runGit(ctx, t, clone, "commit", "-q", "-m", message)
	runGit(ctx, t, clone, "push", "-q", "--force", "origin", "diverged:"+mainBranch)
	return strings.TrimSpace(runGit(ctx, t, clone, "rev-parse", "HEAD"))
}

// TestFetchBaseDivergedOriginKeepsCurrentBase proves a force-pushed origin
// whose new main no longer descends from the base fetchBase already
// recorded is reported as "diverged" and leaves the base exactly where it
// was, rather than silently jumping to a commit with no ancestry relation
// to what Zing has already counted as the ticket's own.
func TestFetchBaseDivergedOriginKeepsCurrentBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})
	sha1, _, err := o.fetchBase(ctx, 744)
	if err != nil {
		t.Fatalf("fetchBase (first, successful): %v", err)
	}

	divergedSHA := forcePushOrphanCommit(ctx, t, remote, "diverged.txt", "diverged\n", "force-pushed, unrelated history")

	gotSHA, _, err := o.fetchBase(ctx, 744)
	if err != nil {
		t.Fatalf("fetchBase (diverged origin): %v", err)
	}
	if gotSHA != sha1 {
		t.Errorf("fetchBase = %s, want %s (the current base, unmoved)", gotSHA, sha1)
	}
	if base := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main")); base != sha1 {
		t.Errorf("refs/zing/base/main = %s, want it unchanged at %s", base, sha1)
	}
	if !forEachRefEmpty(ctx, t, repo, "refs/zing/fetch/") {
		t.Errorf("refs/zing/fetch/ not cleaned up")
	}

	recs := findRecords(logs.records(t), "fetched base diverged, keeping current")
	if len(recs) != 1 {
		t.Fatalf("found %d \"fetched base diverged, keeping current\" records, want 1", len(recs))
	}
	rec := recs[0]
	assertRecord(t, rec, map[string]any{
		"level":            "WARN",
		logFieldTicketID:   float64(744),
		logFieldSHA:        sha1,
		logFieldFetchedSHA: divergedSHA,
		logFieldReason:     "diverged",
	})
}

// cancelAfterFetchRunner wraps a real Runner and cancels a captured
// context.CancelFunc right after a successful "git fetch" call, so a test
// can force fetchBase's own later lock attempt to run against an
// already-canceled context deterministically, without racing a real lock
// holder against a timer.
type cancelAfterFetchRunner struct {
	inner  Runner
	cancel context.CancelFunc
}

func (r cancelAfterFetchRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := r.inner.Run(ctx, dir, name, args...)
	if err == nil && len(args) > 0 && args[0] == "fetch" {
		r.cancel()
	}
	return out, err
}

func (r cancelAfterFetchRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Output(ctx, dir, name, args...)
}

// TestFetchBaseRemovesTmpRefWhenLockFails proves the temporary fetch ref is
// cleaned up even when fetchBase's own attempt to take commonMu afterward
// fails (here, because the caller's context ended while waiting for it):
// the deferred removeFetchRef call runs on every path past a successful
// fetch, lock failure included.
func TestFetchBaseRemovesTmpRefWhenLockFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	fctx, cancel := context.WithCancel(ctx)
	run := cancelAfterFetchRunner{inner: execRunner{}, cancel: cancel}
	o, logs := newTestOrchestratorCapturingLog(t, repo, run)

	// Warm and take commonMu from the test itself, so fetchBase's own
	// Lock call (once its ctx is canceled) blocks rather than racing a
	// real holder.
	mu, err := o.resolveCommonMu(ctx)
	if err != nil {
		t.Fatalf("resolveCommonMu: %v", err)
	}
	if err := mu.Lock(ctx); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, _, fetchErr := o.fetchBase(fctx, 742)
		errCh <- fetchErr
	}()

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		for _, rec := range findRecords(logs.records(t), "fetch base error") {
			if rec["reason"] == "lock_failed" && rec["ticket_id"] == float64(742) {
				found = true
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("did not observe an ERROR \"fetch base error\" record with reason lock_failed within 5s")
	}

	mu.Unlock()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("fetchBase: expected a non-nil error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetchBase did not return after the lock was released")
	}

	if !forEachRefEmpty(ctx, t, repo, "refs/zing/fetch/") {
		t.Errorf("refs/zing/fetch/ not cleaned up")
	}
	if recs := findRecords(logs.records(t), "fetch ref cleanup failed"); len(recs) != 0 {
		t.Errorf("unexpected \"fetch ref cleanup failed\" record(s): %v", recs)
	}
}

// TestFetchBaseGitErrorsAreNotAnswers proves a git failure that is neither
// "the command ran and found nothing" (rev-parse/merge-base exit 1) nor a
// successful fetch is never silently treated as an answer: it is returned
// as an error, with the base ref left exactly as it was.
func TestFetchBaseGitErrorsAreNotAnswers(t *testing.T) {
	t.Parallel()

	t.Run("rev-parse", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		repo := newTestRepo(t)

		failing := failingRunner{
			inner: execRunner{},
			// Only "rev-parse --verify ..." (revParseCommit's own call),
			// not the plain "rev-parse --git-common-dir" resolveCommonMu
			// itself needs, which must keep working so this reaches
			// resolveBase's own forced failure rather than a lock failure.
			fail: func(args []string) bool { return len(args) > 1 && args[0] == "rev-parse" && args[1] == "--verify" },
		}
		o, logs := newTestOrchestratorCapturingLog(t, repo, failing)

		if _, _, err := o.fetchBase(ctx, 743); err == nil {
			t.Fatal("fetchBase: expected an error, got nil")
		}

		if !forEachRefEmpty(ctx, t, repo, "refs/zing/base/") {
			t.Errorf("refs/zing/base/ was written despite the forced rev-parse failure")
		}
		recs := findRecords(logs.records(t), "fetch base error")
		if len(recs) != 1 {
			t.Fatalf("found %d \"fetch base error\" records, want 1", len(recs))
		}
		assertRecord(t, recs[0], map[string]any{
			logFieldReason:   "resolve_failed",
			logFieldTicketID: float64(743),
		})
	})

	t.Run("merge-base", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		repo := newTestRepo(t)
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)
		runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

		o := newTestOrchestrator(t, repo, execRunner{})
		if _, _, err := o.fetchBase(ctx, 743); err != nil {
			t.Fatalf("fetchBase (seed): %v", err)
		}
		sha1 := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main"))

		cloneAndCommitUpstream(ctx, t, remote, "upstream3.txt", "v2\n", "advance origin")

		failing := failingRunner{
			inner: execRunner{},
			fail:  func(args []string) bool { return len(args) > 0 && args[0] == "merge-base" },
		}
		o2, cap2 := newTestOrchestratorCapturingLog(t, repo, failing)

		if _, _, err := o2.fetchBase(ctx, 743); err == nil {
			t.Fatal("fetchBase: expected an error, got nil")
		}

		if base := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main")); base != sha1 {
			t.Errorf("refs/zing/base/main = %s, want it unchanged at %s", base, sha1)
		}
		recs := findRecords(cap2.records(t), "fetch base error")
		if len(recs) != 1 || recs[0]["reason"] != "ancestry_failed" {
			t.Errorf("fetch base error records = %v, want exactly one with reason ancestry_failed", recs)
		}
		if !forEachRefEmpty(ctx, t, repo, "refs/zing/fetch/") {
			t.Errorf("refs/zing/fetch/ not cleaned up")
		}
	})
}

// TestFetchBaseFallsBackToLastFetchedBase proves an origin that has become
// unreachable after a prior successful fetch leaves the base at its last
// fetched value, rather than failing the caller or silently advancing to
// something newer it cannot actually reach.
func TestFetchBaseFallsBackToLastFetchedBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})
	sha1, _, err := o.fetchBase(ctx, 731)
	if err != nil {
		t.Fatalf("fetchBase (first, successful): %v", err)
	}

	cloneAndCommitUpstream(ctx, t, remote, "upstream4.txt", "v2\n", "advance origin, never fetched locally")

	missingPath := filepath.Join(t.TempDir(), "gone")
	runGit(ctx, t, repo, "remote", "set-url", "origin", missingPath)

	gotSHA, fetched, err := o.fetchBase(ctx, 731)
	if err != nil {
		t.Fatalf("fetchBase (origin unreachable): %v", err)
	}
	if fetched {
		t.Error("fetchBase: fetched = true, want false (the fetch itself failed)")
	}
	if gotSHA != sha1 {
		t.Errorf("fetchBase = %s, want %s (the last fetched base)", gotSHA, sha1)
	}
	if base := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main")); base != sha1 {
		t.Errorf("refs/zing/base/main = %s, want it unchanged at %s", base, sha1)
	}

	recs := findRecords(logs.records(t), "fetch base failed, using last fetched base")
	if len(recs) != 1 {
		t.Fatalf("found %d \"fetch base failed, using last fetched base\" records, want 1", len(recs))
	}
	rec := recs[0]
	assertRecord(t, rec, map[string]any{
		"level":          "WARN",
		logFieldTicketID: float64(731),
		logFieldSHA:      sha1,
		logFieldReason:   "no_origin",
	})
	if logs.contains(missingPath) {
		t.Error("the missing remote path leaked into the log buffer")
	}
}

// TestFetchBaseSeedsFromLocalMainWithoutOrigin proves a repository with no
// origin at all still gets a usable base: fetchBase seeds
// refs/zing/base/<default> from the local default branch rather than
// failing the caller.
func TestFetchBaseSeedsFromLocalMainWithoutOrigin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := newTestRepo(t)
	localMainSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", mainBranch))

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})

	gotSHA, fetched, err := o.fetchBase(ctx, 732)
	if err != nil {
		t.Fatalf("fetchBase: %v", err)
	}
	if fetched {
		t.Error("fetchBase: fetched = true, want false (there is no origin to fetch from)")
	}
	if gotSHA != localMainSHA {
		t.Errorf("fetchBase = %s, want %s (local main)", gotSHA, localMainSHA)
	}
	if base := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main")); base != localMainSHA {
		t.Errorf("refs/zing/base/main = %s, want %s", base, localMainSHA)
	}
	if gotLocalMain := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", mainBranch)); gotLocalMain != localMainSHA {
		t.Errorf("local main moved from %s to %s", localMainSHA, gotLocalMain)
	}

	recs := findRecords(logs.records(t), "fetch base failed, using last fetched base")
	if len(recs) != 1 {
		t.Fatalf("found %d WARN records, want 1", len(recs))
	}
	rec := recs[0]
	assertRecord(t, rec, map[string]any{
		logFieldTicketID: float64(732),
		logFieldSHA:      localMainSHA,
		logFieldReason:   "no_origin",
	})
}

// TestFetchBaseEmptyOriginReportsRemoteRefMissing proves an origin that
// exists but has never received the default branch (an empty bare remote,
// the state every newBareRemote starts in) is reported as
// "remote_ref_missing", distinct from "no_origin", and still falls back to
// seeding from local main.
func TestFetchBaseEmptyOriginReportsRemoteRefMissing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	localMainSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", mainBranch))

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})

	gotSHA, fetched, err := o.fetchBase(ctx, 737)
	if err != nil {
		t.Fatalf("fetchBase: %v", err)
	}
	if fetched {
		t.Error("fetchBase: fetched = true, want false (origin has nothing on main)")
	}
	if gotSHA != localMainSHA {
		t.Errorf("fetchBase = %s, want %s (local main)", gotSHA, localMainSHA)
	}

	recs := findRecords(logs.records(t), "fetch base failed, using last fetched base")
	if len(recs) != 1 || recs[0]["reason"] != "remote_ref_missing" {
		t.Errorf("records = %v, want exactly one with reason remote_ref_missing", recs)
	}
	if logs.contains(remote) {
		t.Error("the bare remote's path leaked into the log buffer")
	}
}

// TestFetchBaseSeedFailureNamesRefAndSource proves a default branch that
// does not exist locally either (so there is nothing to seed from) is a
// returned error naming both the base ref and the source ref, not a panic
// or a silently empty base.
func TestFetchBaseSeedFailureNamesRefAndSource(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := newTestRepo(t)

	logs := &logCapture{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: repo, DefaultBranch: "trunk"}
	o, err := New(proj, fakeGitHub{}, execRunner{}, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _, fetchErr := o.fetchBase(ctx, 736)
	if fetchErr == nil {
		t.Fatal("fetchBase: expected an error, got nil")
	}
	if !strings.Contains(fetchErr.Error(), "refs/zing/base/trunk") || !strings.Contains(fetchErr.Error(), "refs/heads/trunk") {
		t.Errorf("fetchBase error %q does not name both refs/zing/base/trunk and refs/heads/trunk", fetchErr.Error())
	}

	verify := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "refs/zing/base/trunk")
	verify.Dir = repo
	verify.Env = scrubGitLocationEnv(os.Environ())
	if out, err := verify.CombinedOutput(); err == nil {
		t.Errorf("rev-parse --verify --quiet refs/zing/base/trunk succeeded (%q), want exit 1: refs/zing/base/trunk was written despite the failed seed", out)
	}

	recs := findRecords(logs.records(t), "fetch base error")
	if len(recs) != 1 {
		t.Fatalf("found %d \"fetch base error\" records, want 1", len(recs))
	}
	assertRecord(t, recs[0], map[string]any{
		logFieldTicketID: float64(736),
		"ref":            "refs/zing/base/trunk",
		logFieldReason:   "seed_failed",
	})
}

// TestFetchBaseRejectsInvalidDefaultBranch proves a project whose default
// branch is not a legal git ref name (New itself only checks non-empty, and
// accepts it) is refused by fetchBase before anything under refs/zing/ is
// touched, rather than handed straight to "git fetch" or "git update-ref".
func TestFetchBaseRejectsInvalidDefaultBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := newTestRepo(t)

	logs := &logCapture{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: repo, DefaultBranch: "bad..name"}
	o, err := New(proj, fakeGitHub{}, execRunner{}, log)
	if err != nil {
		t.Fatalf("New: %v, want New to accept an invalid default branch name (it only checks non-empty)", err)
	}

	_, _, err = o.fetchBase(ctx, 738)
	if err == nil {
		t.Fatal("fetchBase: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "bad..name") {
		t.Errorf("fetchBase error %q does not name the invalid default branch", err.Error())
	}
	if !forEachRefEmpty(ctx, t, repo, "refs/zing/") {
		t.Errorf("refs/zing/ was written despite the invalid default branch")
	}

	recs := findRecords(logs.records(t), "fetch base error")
	if len(recs) != 1 {
		t.Fatalf("found %d \"fetch base error\" records, want 1", len(recs))
	}
	assertRecord(t, recs[0], map[string]any{
		logFieldTicketID: float64(738),
		logFieldReason:   "invalid_default_branch",
	})
}
