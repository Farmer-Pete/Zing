package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Worktree is a prepared ticket worktree. Its fields are unexported, so only
// PrepareWorktree (same package) can build one; a caller cannot forge a
// Worktree pointing at the main checkout. PrepareWorktree also refuses to
// build one pointing at the default branch: ordinarily branch's "zing/"
// prefix already keeps it structurally distinct from a repository's default
// branch, but that alone would not catch the pathological case of a project
// whose default branch itself happens to be named "zing/...", so
// PrepareWorktree checks for it explicitly. Dir and Branch expose the
// values.
type Worktree struct {
	dir    string // <local_path>/.zing/wt/<ticket_id>
	branch string // zing/<ticket_id>-<slug>, or zing/<ticket_id> when slug is empty
}

func (w Worktree) Dir() string    { return w.dir }
func (w Worktree) Branch() string { return w.branch }

var (
	// slugCollapse replaces every run of characters branchName's slug
	// charset disallows with a single "-".
	slugCollapse = regexp.MustCompile(`[^a-z0-9._-]+`)
	// zingBranchPattern is the shape every zing/ ticket branch matches:
	// "zing/<ticket id>" or "zing/<ticket id>-<slug>".
	zingBranchPattern = regexp.MustCompile(`^zing/\d+(-[a-z0-9._-]+)?$`)
)

// branchName builds and validates the branch (PKG5-PLAN.md section 8.2).
// ticketID must be positive. slug is lowercased and reduced to
// [a-z0-9._-]; a run of any other character becomes a single "-"; leading
// and trailing "-" and "." are then trimmed. The candidate is checked
// against zingBranchPattern and, as a second and independent layer, against
// "git check-ref-format refs/heads/<name>" -- git's own ref-name rules (no
// ".." sequence, no trailing ".lock", no bare "." component, and so on) --
// so a candidate that slips past the pattern is still caught. So the branch
// can never be a bare ref, a refspec, or an invalid ref. Its "zing/" prefix
// also keeps it structurally distinct from a repository's default branch in
// the ordinary case; PrepareWorktree additionally rejects the pathological
// case where the default branch itself matches the zing/ pattern, since
// check-ref-format has no notion of "the default branch" to compare
// against.
//
// branchName takes ctx (a small, deliberate deviation from PKG5-PLAN.md
// section 8.2's signature, which omits it) because it runs a real git
// subprocess through checkRefFormat; every git-invoking function in this
// package threads ctx from its caller, and branchName should be no
// exception -- PrepareWorktree already has one to pass down.
func branchName(ctx context.Context, ticketID int64, slug string) (string, error) {
	if ticketID <= 0 {
		return "", fmt.Errorf("orchestrator: branch name: ticket id must be positive, got %d", ticketID)
	}

	sanitized := sanitizeSlug(slug)

	name := "zing/" + strconv.FormatInt(ticketID, 10)
	if sanitized != "" {
		name += "-" + sanitized
	}

	if !zingBranchPattern.MatchString(name) {
		return "", fmt.Errorf("orchestrator: branch name %q does not match the zing/ pattern", name)
	}
	if err := checkRefFormat(ctx, name); err != nil {
		return "", fmt.Errorf("orchestrator: branch name %q: %w", name, err)
	}

	return name, nil
}

func sanitizeSlug(slug string) string {
	lowered := strings.ToLower(slug)
	collapsed := slugCollapse.ReplaceAllString(lowered, "-")
	return strings.Trim(collapsed, "-.")
}

// checkRefFormat runs "git check-ref-format refs/heads/<name>" directly with
// os/exec (branchName is the package's one pure-ish validator, with no
// Runner of its own), using the caller's ctx like every other git-invoking
// call in this package. It is git's own authority on ref-name rules,
// catching anything the package's own pattern missed.
func checkRefFormat(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "git", "check-ref-format", "refs/heads/"+name).CombinedOutput() //nolint:gosec // argv-only, no shell; name is git-syntax-checked by this very call
	if err != nil {
		return fmt.Errorf("git check-ref-format: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// revalidate confirms wt refers to a real, current zing/ ticket branch: the
// branch must match the zing/ form, must differ from the default branch,
// and must equal the branch actually checked out in wt.Dir. It is defense
// in depth on top of Worktree's unexported fields, run by every content-
// mutating method (CommitTask, Push -- later tasks) before it touches git
// state, so a wrong or stale worktree never commits or pushes.
// RemoveWorktree does not call this: a half-removed worktree may no longer
// have a checked-out HEAD to read, so it validates only the first two
// conditions (see RemoveWorktree).
func (o *Orchestrator) revalidate(ctx context.Context, wt Worktree) error {
	if err := o.validateZingBranch(ctx, wt.branch); err != nil {
		return err
	}

	head, err := o.run.Output(ctx, wt.dir, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("orchestrator: read checked-out branch in %s: %w", wt.dir, err)
	}
	head = strings.TrimSpace(head)
	if head != wt.branch {
		return fmt.Errorf("orchestrator: worktree %s has %q checked out, expected %q", wt.dir, head, wt.branch)
	}
	return nil
}

// validateZingBranch is the branch-shape half of revalidate: the branch
// must match the zing/ form, must differ from the default branch, and must
// be a git-legal ref name under "git check-ref-format" -- the same second,
// independent layer branchName checks a freshly built candidate against, so
// a zing/-shaped branch that was hand-crafted rather than produced by
// branchName (and so never ran through check-ref-format) is still rejected
// on the destructive paths that use validateZingBranch (revalidate,
// RemoveWorktree). It takes ctx to run that check; RemoveWorktree uses it
// directly, without the checked-out-HEAD check revalidate adds, since a
// half-removed worktree may have no HEAD to read.
func (o *Orchestrator) validateZingBranch(ctx context.Context, branch string) error {
	if !zingBranchPattern.MatchString(branch) {
		return fmt.Errorf("orchestrator: branch %q is not a zing/ ticket branch", branch)
	}
	if branch == o.proj.DefaultBranch {
		return fmt.Errorf("orchestrator: branch %q must not be the default branch", branch)
	}
	if err := checkRefFormat(ctx, branch); err != nil {
		return fmt.Errorf("orchestrator: branch %q: %w", branch, err)
	}
	return nil
}

// worktreeExcludeLine is the line PrepareWorktree ensures is present in the
// main checkout's .git/info/exclude, so the main checkout never tracks the
// worktree directory it creates under it.
const worktreeExcludeLine = ".zing/"

// gitPathInfoExclude resolves the repository's info/exclude path with
// "git -C <LocalPath> rev-parse --git-path info/exclude" (PR review fix),
// rather than hardcoding "<LocalPath>/.git/info/exclude": that hardcoded
// join is wrong whenever LocalPath is itself a git worktree or a submodule,
// where ".git" is a file (pointing at the real, shared gitdir elsewhere),
// not a directory, so joining straight onto it can never resolve to a real
// path. git itself already knows how to resolve this correctly in every
// case, worktree and submodule included, so this defers to it instead of
// reimplementing that resolution. The command's stdout is relative to
// LocalPath (cmd.Dir) unless git reports an absolute path itself, so a
// relative result is joined onto LocalPath and an absolute one is returned
// as-is.
func (o *Orchestrator) gitPathInfoExclude(ctx context.Context) (string, error) {
	out, err := o.run.Output(ctx, o.proj.LocalPath, "git", "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-path info/exclude: %w", err)
	}
	rel := strings.TrimSpace(out)
	if rel == "" {
		return "", errors.New("git rev-parse --git-path info/exclude: empty result")
	}
	if filepath.IsAbs(rel) {
		return rel, nil
	}
	return filepath.Join(o.proj.LocalPath, rel), nil
}

// ensureWorktreeExclude appends worktreeExcludeLine to the repository's
// info/exclude file if not already present. Refined from the design's
// "global gitignore" wording (PKG5-PLAN.md section 16): this is per-repo, so
// it needs no mutation of the user's global git config and is not committed.
// It resolves the file's real path through gitPathInfoExclude rather than
// assuming "<LocalPath>/.git/info/exclude" (PR review fix), and creates that
// path's parent directory if needed, since a repository's info/ directory is
// not guaranteed to already exist.
func (o *Orchestrator) ensureWorktreeExclude(ctx context.Context) (err error) {
	path, resolveErr := o.gitPathInfoExclude(ctx)
	if resolveErr != nil {
		return fmt.Errorf("orchestrator: resolve info/exclude: %w", resolveErr)
	}

	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("orchestrator: read %s: %w", path, readErr)
	}
	for line := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(line) == worktreeExcludeLine {
			return nil
		}
	}

	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return fmt.Errorf("orchestrator: create %s: %w", filepath.Dir(path), mkdirErr)
	}

	f, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr != nil {
		return fmt.Errorf("orchestrator: open %s: %w", path, openErr)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("orchestrator: close %s: %w", path, closeErr)
		}
	}()

	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	if _, err = f.WriteString(prefix + worktreeExcludeLine + "\n"); err != nil {
		err = fmt.Errorf("orchestrator: write %s: %w", path, err)
		return err
	}
	return nil
}

// runSparseCheckoutSet runs "git sparse-checkout set --stdin" in dir,
// feeding cone's entries to it one per line on stdin (PR review fix: option
// injection). PrepareWorktree used to pass cone straight through as argv
// ("git sparse-checkout set <cone...>"), so a cone entry beginning with "-"
// would be parsed by git as an option rather than a path. Feeding it through
// stdin instead means git never sees cone's entries as argv at all -- every
// argument here is a fixed literal, so there is nothing left for an entry to
// inject into. This runs its own *exec.Cmd directly, mirroring execRunner
// (the same GIT_DIR scrub via scrubGitLocationEnv), rather than going
// through the Runner interface: Runner (orchestrator.go) is Run/Output with
// no notion of stdin, and every other caller of it in this package is a fake
// in a test, so widening it for this one call would ripple through every
// test double for no other benefit.
func runSparseCheckoutSet(ctx context.Context, dir string, cone []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "sparse-checkout", "set", "--stdin")
	cmd.Dir = dir
	cmd.Env = scrubGitLocationEnv(os.Environ())
	cmd.Stdin = strings.NewReader(strings.Join(cone, "\n") + "\n")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// PrepareWorktree creates the worktree directory and branch and applies the
// sparse cone. cone is the list of cone paths to include; an empty cone
// means a full checkout. It first ensures <local_path>/.git/info/exclude
// contains ".zing/", so the main checkout never tracks the worktree. On any
// failure after "git worktree add" it force-removes the worktree and
// deletes the branch, then returns the original error (a cleanup error is
// logged, not returned).
//
// After computing branch, it errors if branch equals o.proj.DefaultBranch:
// the "zing/" prefix ordinarily keeps a ticket branch distinct from a
// repository's default branch, but a project whose default branch itself
// happens to be named "zing/..." would otherwise slip past that assumption
// and hand the caller a Worktree pointing at the default branch.
func (o *Orchestrator) PrepareWorktree(ctx context.Context, ticketID int64, slug string, cone []string) (Worktree, error) {
	branch, err := branchName(ctx, ticketID, slug)
	if err != nil {
		return Worktree{}, fmt.Errorf("orchestrator: prepare worktree: %w", err)
	}
	if branch == o.proj.DefaultBranch {
		return Worktree{}, fmt.Errorf("orchestrator: prepare worktree: branch %q must not be the default branch", branch)
	}

	dir := filepath.Join(o.proj.LocalPath, ".zing", "wt", strconv.FormatInt(ticketID, 10))
	if _, statErr := os.Stat(dir); statErr == nil {
		return Worktree{}, fmt.Errorf("orchestrator: prepare worktree: directory already exists: %s", dir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Worktree{}, fmt.Errorf("orchestrator: prepare worktree: stat %s: %w", dir, statErr)
	}

	if err := o.ensureWorktreeExclude(ctx); err != nil {
		return Worktree{}, fmt.Errorf("orchestrator: prepare worktree: %w", err)
	}

	o.log.Info("preparing worktree", "ticket_id", ticketID, "branch", branch, "dir", dir)

	if out, err := o.run.Run(ctx, o.proj.LocalPath, "git", "worktree", "add", "--no-checkout", "-b", branch, dir, o.proj.DefaultBranch); err != nil {
		return Worktree{}, fmt.Errorf("orchestrator: git worktree add: %w: %s", err, strings.TrimSpace(out))
	}

	wt := Worktree{dir: dir, branch: branch}

	if len(cone) > 0 {
		if out, err := o.run.Run(ctx, dir, "git", "sparse-checkout", "init", "--cone"); err != nil {
			o.cleanupWorktree(ctx, wt)
			return Worktree{}, fmt.Errorf("orchestrator: git sparse-checkout init: %w: %s", err, strings.TrimSpace(out))
		}
		if out, err := runSparseCheckoutSet(ctx, dir, cone); err != nil {
			o.cleanupWorktree(ctx, wt)
			return Worktree{}, fmt.Errorf("orchestrator: git sparse-checkout set: %w: %s", err, strings.TrimSpace(out))
		}
	}

	if out, err := o.run.Run(ctx, dir, "git", "checkout"); err != nil {
		o.cleanupWorktree(ctx, wt)
		return Worktree{}, fmt.Errorf("orchestrator: git checkout: %w: %s", err, strings.TrimSpace(out))
	}

	o.log.Info("worktree ready", "ticket_id", ticketID, "branch", branch, "dir", dir)

	return wt, nil
}

// cleanupWorktreeTimeout bounds the detached context cleanupWorktree runs
// under, mirroring resetUnsignedCommitTimeout in commit.go: a cleanup that
// can no longer inherit the caller's context must still complete in bounded
// time rather than hanging forever.
const cleanupWorktreeTimeout = 30 * time.Second

// cleanupWorktree force-removes a partially prepared worktree and deletes
// its branch after a failure past "git worktree add". A cleanup failure is
// logged, never returned, so the original failure is what the caller sees.
// It runs under a detached context (context.WithoutCancel(ctx), bounded by
// cleanupWorktreeTimeout) rather than ctx itself (PR review fix, mirroring
// commit.go's resetAfterUnsignedCommit): ctx is exactly the context whose
// failure -- cancellation or a passed deadline -- is what triggered this
// cleanup in the first place, so running the cleanup on that same ctx could
// mean it never runs at all, leaking the worktree directory and its branch.
func (o *Orchestrator) cleanupWorktree(ctx context.Context, wt Worktree) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWorktreeTimeout)
	defer cancel()

	if out, err := o.run.Run(cleanupCtx, o.proj.LocalPath, "git", "worktree", "remove", "--force", wt.dir); err != nil {
		o.log.Warn("cleanup: remove worktree failed", "branch", wt.branch, "dir", wt.dir, "err", err, "output", strings.TrimSpace(out))
	}
	if out, err := o.run.Run(cleanupCtx, o.proj.LocalPath, "git", "branch", "-D", wt.branch); err != nil {
		o.log.Warn("cleanup: delete branch failed", "branch", wt.branch, "err", err, "output", strings.TrimSpace(out))
	}
}

// RemoveWorktree removes the worktree and deletes its branch. It first
// validates the branch (zing/ form, not the default branch, a git-legal ref
// per check-ref-format); an invalid or default branch is an error and
// nothing is removed. It then runs "git worktree prune" (PR review fix): if
// wt.dir was deleted outside git (an "rm -rf", not "git worktree remove"),
// git's own worktree administration under .git/worktrees/ still registers
// it as present, which both makes "git worktree list --porcelain" report a
// path that no longer exists on disk and makes "git branch -D <branch>"
// refuse with "already checked out" even though nothing is actually there;
// pruning first clears any such stale registration whose working directory
// is gone, so a worktree removed by hand still gets its branch deleted
// here, not leaked. Then, independently: if "git worktree list --porcelain"
// still shows wt.Dir after the prune (a live, present worktree, not a stale
// registration), it first reads the branch actually checked out there with
// "git -C wt.dir symbolic-ref --short HEAD" and refuses to proceed if it
// differs from wt.branch, so a worktree directory repurposed out from under
// Zing (checked out to some other branch since it was prepared) is never
// force-removed; only then does it remove it with "git worktree remove
// --force <dir>" (an already-absent worktree is fine and skips the HEAD
// check entirely, since a half-removed worktree may have no HEAD to read);
// if "git branch --list <branch>" shows the branch, it deletes it with
// "git branch -D <branch>" (an already-absent branch is fine). So the
// "worktree absent, branch present" state is safe and deterministic: the
// validated zing/ branch is still deleted, and the default branch is never
// touched.
func (o *Orchestrator) RemoveWorktree(ctx context.Context, wt Worktree) error {
	if err := o.validateZingBranch(ctx, wt.branch); err != nil {
		return fmt.Errorf("orchestrator: remove worktree: %w", err)
	}

	o.log.Info("removing worktree", "branch", wt.branch, "dir", wt.dir)

	if out, pruneErr := o.run.Run(ctx, o.proj.LocalPath, "git", "worktree", "prune"); pruneErr != nil {
		return fmt.Errorf("orchestrator: remove worktree: git worktree prune: %w: %s", pruneErr, strings.TrimSpace(out))
	}

	present, err := o.worktreePresent(ctx, wt.dir)
	if err != nil {
		return fmt.Errorf("orchestrator: remove worktree: %w", err)
	}
	if present {
		checkedOut, headErr := o.run.Output(ctx, wt.dir, "git", "symbolic-ref", "--short", "HEAD")
		if headErr != nil {
			return fmt.Errorf("orchestrator: remove worktree: read checked-out branch in %s: %w", wt.dir, headErr)
		}
		checkedOut = strings.TrimSpace(checkedOut)
		if checkedOut != wt.branch {
			return fmt.Errorf("orchestrator: remove worktree: %s has %q checked out, expected %q; refusing to force-remove a repurposed worktree",
				wt.dir, checkedOut, wt.branch)
		}

		if out, removeErr := o.run.Run(ctx, o.proj.LocalPath, "git", "worktree", "remove", "--force", wt.dir); removeErr != nil {
			return fmt.Errorf("orchestrator: remove worktree: git worktree remove: %w: %s", removeErr, strings.TrimSpace(out))
		}
	}

	branchPresent, err := o.branchExists(ctx, wt.branch)
	if err != nil {
		return fmt.Errorf("orchestrator: remove worktree: %w", err)
	}
	if branchPresent {
		if out, deleteErr := o.run.Run(ctx, o.proj.LocalPath, "git", "branch", "-D", wt.branch); deleteErr != nil {
			return fmt.Errorf("orchestrator: remove worktree: git branch -D: %w: %s", deleteErr, strings.TrimSpace(out))
		}
	}

	o.log.Info("worktree removed", "branch", wt.branch, "dir", wt.dir)

	return nil
}

// worktreePresent reports whether dir appears in "git worktree list
// --porcelain". Git reports each worktree's path with symlinks resolved, so
// dir is resolved with filepath.EvalSymlinks before comparing; otherwise a
// dir reached through a symlinked path component (o.proj.LocalPath itself,
// say) would never match git's resolved form, and RemoveWorktree would
// wrongly conclude the worktree is absent and leak it. When dir no longer
// exists at all, EvalSymlinks errors with a path-not-found error, which is
// exactly the "not present" case, not a real failure, so it is treated as
// present=false rather than propagated.
func (o *Orchestrator) worktreePresent(ctx context.Context, dir string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("resolve worktree dir %s: %w", dir, err)
	}

	out, err := o.run.Output(ctx, o.proj.LocalPath, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && path == resolved {
			return true, nil
		}
	}
	return false, nil
}

func (o *Orchestrator) branchExists(ctx context.Context, branch string) (bool, error) {
	out, err := o.run.Output(ctx, o.proj.LocalPath, "git", "branch", "--list", branch)
	if err != nil {
		return false, fmt.Errorf("git branch --list: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}
