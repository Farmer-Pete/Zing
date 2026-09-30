package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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
	log := slog.New(slog.DiscardHandler)
	valid := Project{Owner: testOwner, Repo: testRepo, LocalPath: absLocalPath, DefaultBranch: mainBranch}

	t.Run("valid project", func(t *testing.T) {
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
		if _, err := New(valid, nil, execRunner{}, log); err == nil {
			t.Fatal("New: expected an error for a nil GitHub, got nil")
		}
	})

	t.Run("nil Runner is rejected", func(t *testing.T) {
		if _, err := New(valid, fakeGitHub{}, nil, log); err == nil {
			t.Fatal("New: expected an error for a nil Runner, got nil")
		}
	})
}

func TestBranchName(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
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
				got, err := branchName(t.Context(), c.ticketID, c.slug)
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
		for _, id := range []int64{0, -1, -100} {
			if _, err := branchName(t.Context(), id, "slug"); err == nil {
				t.Errorf("branchName(%d, \"slug\"): expected an error, got nil", id)
			}
		}
	})

	t.Run("a trailing .lock is rejected by check-ref-format", func(t *testing.T) {
		// "lock" is a legal slug character, so sanitizeSlug leaves it
		// untouched; the candidate matches zingBranchPattern but git's
		// own ref-name rule (no ref may end in ".lock") still rejects it.
		if _, err := branchName(t.Context(), 7, "wip.lock"); err == nil {
			t.Fatal("branchName(7, \"wip.lock\"): expected an error, got nil")
		}
	})

	t.Run("a run of internal dots is rejected by check-ref-format", func(t *testing.T) {
		// sanitizeSlug only trims leading/trailing dots, so an internal
		// ".." survives to the candidate; git rejects two consecutive
		// dots anywhere in a ref name.
		if _, err := branchName(t.Context(), 7, "a..b"); err == nil {
			t.Fatal("branchName(7, \"a..b\"): expected an error, got nil")
		}
	})
}

// TestCheckRefFormat exercises the git check-ref-format wrapper directly,
// with cases git's ref-name rules reject that branchName's own sanitizing
// never has occasion to produce (a bare "." component). It is the second,
// independent validation layer branchName relies on.
func TestCheckRefFormat(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{"valid zing branch", branch7MySlug, false},
		{"bare dot component", ".", true},
		{"trailing .lock", "zing/7-wip.lock", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkRefFormat(t.Context(), c.ref)
			if c.wantErr && err == nil {
				t.Errorf("checkRefFormat(%q): expected an error, got nil", c.ref)
			}
			if !c.wantErr && err != nil {
				t.Errorf("checkRefFormat(%q): unexpected error: %v", c.ref, err)
			}
		})
	}
}

func TestRevalidate(t *testing.T) {
	repo := newTestRepo(t)
	o := newTestOrchestrator(t, repo, execRunner{})
	ctx := t.Context()

	wt, err := o.PrepareWorktree(ctx, 1, "feature", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	t.Run("valid worktree passes", func(t *testing.T) {
		if err := o.revalidate(ctx, wt); err != nil {
			t.Errorf("revalidate: unexpected error: %v", err)
		}
	})

	t.Run("non-zing branch is rejected", func(t *testing.T) {
		bad := Worktree{dir: wt.dir, branch: "feature/not-zing"}
		if err := o.revalidate(ctx, bad); err == nil {
			t.Error("revalidate: expected an error for a non-zing branch, got nil")
		}
	})

	t.Run("the default branch is rejected", func(t *testing.T) {
		bad := Worktree{dir: wt.dir, branch: mainBranch}
		if err := o.revalidate(ctx, bad); err == nil {
			t.Error("revalidate: expected an error for the default branch, got nil")
		}
	})

	t.Run("a worktree whose checked-out HEAD drifted is rejected", func(t *testing.T) {
		runGit(ctx, t, wt.dir, "checkout", "-b", "zing/1-drifted")
		if err := o.revalidate(ctx, wt); err == nil {
			t.Error("revalidate: expected an error when HEAD no longer matches wt.branch, got nil")
		}
	})
}

func TestPrepareWorktree(t *testing.T) {
	t.Run("creates the worktree and branch, full checkout", func(t *testing.T) {
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
	t.Run("removes the worktree and branch", func(t *testing.T) {
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
		repo := newTestRepo(t)
		o := newTestOrchestrator(t, repo, noCallRunner{t: t})
		ctx := t.Context()

		bad := Worktree{dir: filepath.Join(repo, ".zing", "wt", "99"), branch: mainBranch}
		if err := o.RemoveWorktree(ctx, bad); err == nil {
			t.Fatal("RemoveWorktree: expected an error for the default branch, got nil")
		}
	})

	t.Run("rejects a non-zing branch without removing anything", func(t *testing.T) {
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
	t.Run("no drivers", func(t *testing.T) {
		got := hardenedGitArgs(nil, statusArg)
		want := []string{"-c", hooksPathArg, "-c", fsmonitorArg, statusArg}
		if !slices.Equal(got, want) {
			t.Errorf("hardenedGitArgs(nil, \"status\") = %q, want %q", got, want)
		}
	})

	t.Run("two drivers in sorted order", func(t *testing.T) {
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
	t.Run("a git call's argv starts with the two -c pairs", func(t *testing.T) {
		cmd := execRunner{}.command(t.Context(), absLocalPath, gitName, statusArg, "--porcelain")
		want := []string{gitName, "-c", hooksPathArg, "-c", fsmonitorArg, statusArg, "--porcelain"}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("cmd.Args = %q, want %q", cmd.Args, want)
		}
	})

	t.Run("drivers add their own -c pairs after the hooks pair", func(t *testing.T) {
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
	run, ok := NewRunner().(execRunner)
	if !ok {
		t.Fatalf("NewRunner() = %T, want execRunner", NewRunner())
	}
	if len(run.drivers) != 0 {
		t.Errorf("NewRunner().drivers = %v, want empty", run.drivers)
	}
}

// TestCheckRefFormatArgv proves checkRefFormat's argv (checkRefFormatArgs)
// carries the hardening prefix, even though it reads no repository.
func TestCheckRefFormatArgv(t *testing.T) {
	got := checkRefFormatArgs(branch7MySlug)
	want := []string{"-c", hooksPathArg, "-c", fsmonitorArg, "check-ref-format", "refs/heads/" + branch7MySlug}
	if !slices.Equal(got, want) {
		t.Errorf("checkRefFormatArgs(%q) = %q, want %q", branch7MySlug, got, want)
	}
}

// TestSparseCheckoutArgv proves runSparseCheckoutSet's argv
// (sparseCheckoutSetArgs) carries the hardening prefix, drivers included --
// "sparse-checkout set" materializes files exactly as "git checkout" does,
// so it needs the same driver override.
func TestSparseCheckoutArgv(t *testing.T) {
	t.Run("no drivers", func(t *testing.T) {
		got := sparseCheckoutSetArgs(nil)
		want := []string{"-c", hooksPathArg, "-c", fsmonitorArg, "sparse-checkout", "set", "--stdin"}
		if !slices.Equal(got, want) {
			t.Errorf("sparseCheckoutSetArgs(nil) = %q, want %q", got, want)
		}
	})

	t.Run("with drivers", func(t *testing.T) {
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
	t.Run("none configured", func(t *testing.T) {
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

// -----------------------------------------------------------------------
// EnsureWorktree
// -----------------------------------------------------------------------

func TestEnsureWorktree(t *testing.T) {
	t.Run("absent creates a fresh worktree", func(t *testing.T) {
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
func TestSigningProgramCheck(t *testing.T) {
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
