package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testOwner     = "acme"
	testRepo      = "widgets"
	mainBranch    = "main"
	absLocalPath  = "/tmp/widgets"
	branch7MySlug = "zing/7-my-slug"
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
		if o.proj != valid {
			t.Errorf("proj = %+v, want %+v", o.proj, valid)
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

		for _, rel := range []string{"README.md", "app/main.go", "docs/extra.md"} {
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
		failing := failingRunner{
			inner: execRunner{},
			fail: func(args []string) bool {
				return len(args) > 0 && args[0] == "checkout"
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

		linkedDir := filepath.Join(filepath.Dir(repo), "linked-worktree")
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
	// is actually there. RemoveWorktree now runs "git worktree prune" first,
	// which clears that stale registration, so the branch still gets
	// deleted.
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
	return r.inner.Output(ctx, dir, name, args...)
}

// TestPrepareWorktree_CleanupSurvivesCancelledContext proves the PR review
// fix to cleanupWorktree: it used to run "git worktree remove" and
// "git branch -D" on the same ctx PrepareWorktree was called with, so a ctx
// cancelled by the very failure that triggered cleanup (or one past its
// deadline) would leave the worktree directory and branch leaked, since
// cleanup could never run. ctx here is cancelled the instant the forced
// "checkout" failure happens, before cleanupWorktree runs -- yet the
// directory and branch still end up removed, since cleanupWorktree now runs
// on a context.WithoutCancel(ctx) detached from ctx's cancellation.
func TestPrepareWorktree_CleanupSurvivesCancelledContext(t *testing.T) {
	repo := newTestRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	failing := cancelingFailingRunner{
		inner: execRunner{},
		fail: func(args []string) bool {
			return len(args) > 0 && args[0] == "checkout"
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
