// judge.go prepares and tears down the judge's own checkout (PKG9-PLAN.md
// section 10.2, D5): a detached worktree at the frozen sha, with CLAUDE.md
// and AGENTS.md always read from the default branch, never from the ticket
// branch, so a build run cannot steer the judge through either file.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// JudgeTree is one detached checkout prepared for the judge, at
// <local_path>/.zing/judge/<ticket_id>. Its fields are unexported, so only
// JudgeWorktree (same package) can build one carrying real git state; a
// caller cannot forge a JudgeTree pointing somewhere else. Dir exposes the
// path, and Remove needs nothing but what JudgeWorktree already captured on
// it, so a deferred call can tear it down with no Orchestrator in scope.
type JudgeTree struct {
	dir      string   // <local_path>/.zing/judge/<ticket_id>
	repoPath string   // o.proj.LocalPath: where "git worktree remove" runs
	drivers  []string // this checkout's own filter drivers (PKG8-PLAN.md section 7.2), overridden to empty on every git call JudgeTree itself makes
	run      Runner   // captured at JudgeWorktree time
	// commonMu is this repository's shared git-lock mutex (design section
	// 8, commonlock.go), captured at JudgeWorktree time: Remove's own
	// "worktree remove --force" is a shared write, and callers (internal/job)
	// invoke it directly, with no Orchestrator in scope to resolve one.
	commonMu *sync.Mutex
}

// Dir returns the checkout's absolute path.
func (j JudgeTree) Dir() string { return j.dir }

// Remove runs "git worktree remove --force <dir>" (a shared write, run
// under commonMu through runCommonLocked -- design section 8), then
// os.RemoveAll(dir). Git's "is not a working tree" failure -- the checkout
// already fully gone, disk and registration both -- is treated as success
// rather than propagated, so Remove is safe to call twice and safe to call
// on a checkout a previous, crashed run already half-removed by hand. Any
// other "git worktree remove" failure is returned as-is, without running
// RemoveAll: a real failure there (a lock held, a permission error) means
// git's own worktree administration may still need this directory, so
// Remove does not also delete it out from under that state.
func (j JudgeTree) Remove(ctx context.Context) error {
	out, err := j.removeLocked(ctx)
	if err != nil && !isNotAWorkingTreeErrorOutput(out) {
		return fmt.Errorf("orchestrator: judge worktree: remove: %w: %s", err, strings.TrimSpace(out))
	}
	if rmErr := os.RemoveAll(j.dir); rmErr != nil {
		return fmt.Errorf("orchestrator: judge worktree: remove dir %s: %w", j.dir, rmErr)
	}
	return nil
}

// removeLocked runs this tree's one shared git call with commonMu held
// (design section 8): the only place outside commonlock.go's own runCommon
// and ensureWorktreeExcludeLocked that takes the lock, since JudgeTree has
// no Orchestrator to call runCommon through.
func (j JudgeTree) removeLocked(ctx context.Context) (string, error) {
	j.commonMu.Lock()
	defer j.commonMu.Unlock()
	return runCommonLocked(ctx, j.run, j.repoPath, "worktree", "remove", "--force", j.dir)
}

// judgeGovernanceFiles are the two root files the judge's checkout always
// takes from the default branch (section 10.2, D5). Codex loads AGENTS.md
// by walking up from its working directory to the repository root, and the
// judge's working directory is this checkout's own root, so replacing only
// these two files at the root is enough to keep a build run's edits to
// either one from ever reaching the judge.
var judgeGovernanceFiles = []string{"CLAUDE.md", "AGENTS.md"}

// governanceFile is one governance file as read from the default branch:
// present is false when the file does not exist there at all, in which case
// content is meaningless.
type governanceFile struct {
	name    string
	content []byte
	present bool
}

// JudgeWorktree prepares the judge's checkout at sha (section 10.2):
//
//  1. Remove any checkout left at the path by a crash (JudgeTree.Remove's
//     own rules).
//  2. Read CLAUDE.md and AGENTS.md from the default branch with
//     "git show <default_branch>:<name>", before the checkout exists, so a
//     build run that edited either file on the ticket branch cannot steer
//     the judge.
//  3. "git worktree add --detach --no-checkout <dir> <sha>".
//  4. Read this checkout's own filter drivers and "git checkout --detach
//     <sha>" with those drivers overridden, so a configured smudge filter
//     never runs.
//  5. Apply the files read in step 2: a present one is written to
//     <dir>/<name> at mode 0644 (replacing whatever checkout left there);
//     one absent on the default branch is removed from <dir> if checkout
//     put one there.
//
// A failure after step 3 removes the checkout before returning the error.
func (o *Orchestrator) JudgeWorktree(ctx context.Context, ticketID int64, sha string) (JudgeTree, error) {
	dir := filepath.Join(o.proj.LocalPath, ".zing", "judge", strconv.FormatInt(ticketID, 10))

	mu, muErr := o.resolveCommonMu(ctx)
	if muErr != nil {
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: %w", muErr)
	}

	leftover := JudgeTree{dir: dir, repoPath: o.proj.LocalPath, run: o.run, commonMu: mu}
	if err := leftover.Remove(ctx); err != nil {
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: remove leftover: %w", err)
	}

	governance, err := o.readGovernanceFiles(ctx)
	if err != nil {
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: %w", err)
	}

	if addOut, addErr := o.runCommon(ctx, o.run, o.proj.LocalPath, "worktree", "add", "--detach", "--no-checkout", dir, sha); addErr != nil {
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: git worktree add: %w: %s", addErr, strings.TrimSpace(addOut))
	}

	jt := JudgeTree{dir: dir, repoPath: o.proj.LocalPath, run: o.run, commonMu: mu}

	drivers, err := o.FilterDrivers(ctx, dir)
	if err != nil {
		o.cleanupJudgeTree(ctx, jt)
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: %w", err)
	}
	jt.drivers = drivers

	run := execRunner{drivers: drivers}
	if out, checkoutErr := run.Run(ctx, dir, "git", "checkout", "--detach", sha); checkoutErr != nil {
		o.cleanupJudgeTree(ctx, jt)
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: git checkout: %w: %s", checkoutErr, strings.TrimSpace(out))
	}

	if applyErr := applyGovernanceFiles(dir, governance); applyErr != nil {
		o.cleanupJudgeTree(ctx, jt)
		return JudgeTree{}, fmt.Errorf("orchestrator: judge worktree: %w", applyErr)
	}

	return jt, nil
}

// readGovernanceFiles reads each of judgeGovernanceFiles from the default
// branch with "git show <default_branch>:<name>". A file absent there
// (isMissingAtRevision) is recorded as such, not an error; any other
// failure is.
func (o *Orchestrator) readGovernanceFiles(ctx context.Context) ([]governanceFile, error) {
	files := make([]governanceFile, 0, len(judgeGovernanceFiles))
	for _, name := range judgeGovernanceFiles {
		out, err := o.run.Output(ctx, o.proj.LocalPath, "git", "show", o.proj.DefaultBranch+":"+name)
		switch {
		case err == nil:
			files = append(files, governanceFile{name: name, content: []byte(out), present: true})
		case isMissingAtRevision(err):
			files = append(files, governanceFile{name: name, present: false})
		default:
			return nil, fmt.Errorf("git show %s:%s: %w", o.proj.DefaultBranch, name, err)
		}
	}
	return files, nil
}

// isMissingAtRevision reports whether err is "git show <rev>:<path>" failing
// because path does not exist at rev: exit code 128 whose message contains
// "does not exist" (git's own wording, "fatal: path '<path>' does not exist
// in '<rev>'"). Any other failure -- a real repository error, an unreadable
// object -- does not match, so the caller treats it as a real error rather
// than as "absent".
func isMissingAtRevision(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		return false
	}
	return strings.Contains(err.Error(), "does not exist")
}

// applyGovernanceFiles writes each present file in files to dir at mode
// 0644 (step 5), replacing whatever checkout left there, and removes an
// absent one from dir if checkout put one there; an absent file dir never
// had is left alone.
func applyGovernanceFiles(dir string, files []governanceFile) error {
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if f.present {
			if err := os.WriteFile(path, f.content, 0o644); err != nil { //nolint:gosec // G306: 0644 matches every other file this checkout puts on disk (git checkout itself writes at 0644); CLAUDE.md and AGENTS.md carry no secret
				return fmt.Errorf("write %s: %w", path, err)
			}
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// cleanupJudgeTree removes jt after a failure partway through JudgeWorktree,
// logging rather than returning a removal failure so the original error is
// what the caller sees (mirrors cleanupWorktreeDir in worktree.go). It runs
// under a detached context, bounded by cleanupWorktreeTimeout, for the same
// reason cleanupWorktreeDir does: ctx may be exactly what is failing.
func (o *Orchestrator) cleanupJudgeTree(ctx context.Context, jt JudgeTree) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWorktreeTimeout)
	defer cancel()
	if err := jt.Remove(cleanupCtx); err != nil {
		o.log.Warn("cleanup: remove judge worktree failed", "dir", jt.dir, "err", err)
	}
}
