package orchestrator

// commonlock.go serializes this package's own writes to one repository's
// shared git state -- the common git dir every linked worktree shares --
// against every other orchestrator instance (in this process, possibly
// driving two different tickets at once) touching the same repository
// (design #45, section 8).
//
// Inventory. Every git call this package makes, through o.run or a locally
// built execRunner (judge.go's JudgeTree carries its own captured Runner,
// same rule), classified shared (writes something under the common git dir
// other than the worktree's own branch ref or its own admin-dir files) or
// per-worktree (a read of any kind, or a write confined to one worktree's
// own branch, index, or working tree). When unsure, the rule is shared;
// every row below that follows that rule is called out. No "git gc" or
// "git pack-refs" runs anywhere in this package today, so neither appears
// below; routing either through runCommon when one is ever added is the
// same shape as "git branch -D".
//
//	Call site                                  Subcommand                               Class
//	------------------------------------------  ---------------------------------------  -----------
//	commit.go CommitTask (priorHead, newSHA)    rev-parse HEAD                            per-worktree (read)
//	commit.go CommitTask (litRun)               add --pathspec-from-file=...              per-worktree (worktree's own index)
//	commit.go CommitTask (litRun)                commit -S -F ... --pathspec-from-file=... per-worktree (worktree's own branch ref)
//	commit.go resetAfterUnsignedCommit          reset --soft <head>                       per-worktree (worktree's own branch ref)
//	commit.go signedStatus                      show --no-patch --format=%G?              per-worktree (read)
//	commit.go signedStatusFallback              cat-file -p                               per-worktree (read)
//	commit.go CommitChanges                     diff-tree --no-commit-id ...               per-worktree (read)
//	commit.go CommitSubject                     show -s --format=%s                       per-worktree (read)
//	judge.go  JudgeTree.Remove                  worktree remove --force                    SHARED
//	judge.go  JudgeWorktree                     worktree add --detach --no-checkout        SHARED
//	judge.go  JudgeWorktree                     checkout --detach <sha>                    per-worktree (this checkout's own HEAD)
//	judge.go  readGovernanceFiles               show <default_branch>:<name>               per-worktree (read)
//	perimeter.go ChangedPaths                   status --porcelain=v1 ...                  per-worktree (read)
//	perimeter.go RevertPaths (litRun)           restore --staged [--worktree] ...          per-worktree (worktree's own index/working tree)
//	perimeter.go Hunk                           diff ...                                    per-worktree (read)
//	perimeter.go BranchCommits                  rev-list --reverse ...                     per-worktree (read)
//	push.go   Push                              push -u origin <refspec>                   SHARED (writes branch.<b>.* in the shared config)
//	push.go   unpushedShas                      log -z --format=%H ...                     per-worktree (read)
//	review.go HeadSHA                           rev-parse HEAD                             per-worktree (read)
//	review.go Diff                              merge-base, diff ...                       per-worktree (read)
//	review.go ChangedFilesBetween               diff --name-only --no-renames -z ...        per-worktree (read)
//	review.go ChangedFilesSinceBase             merge-base ...                              per-worktree (read)
//	review.go IsAncestor                        merge-base --is-ancestor                    per-worktree (read)
//	worktree.go checkRefFormat                  check-ref-format                            per-worktree (no repository touched at all)
//	worktree.go revalidate                      symbolic-ref --short HEAD                   per-worktree (read)
//	worktree.go GitCommonDir                    rev-parse --git-common-dir                  per-worktree (read)
//	worktree.go gitPathInfoExclude              rev-parse --git-path info/exclude           per-worktree (read)
//	worktree.go ensureWorktreeExclude           (no git call: direct info/exclude read+append) SHARED, see below
//	worktree.go runSparseCheckoutSet            sparse-checkout set --stdin                 per-worktree (worktree's own admin-dir sparse-checkout file)
//	worktree.go PrepareWorktree                 worktree add --no-checkout -b <branch> ...  SHARED
//	worktree.go PrepareWorktree                 sparse-checkout init --cone                 SHARED (no worktree-specific config yet: writes the shared config)
//	worktree.go PrepareWorktree                 checkout                                     per-worktree
//	worktree.go FilterDrivers                   config --get-regexp ...                     per-worktree (read)
//	worktree.go gitConfigGet                    config --get <key>                           per-worktree (read)
//	worktree.go cleanupWorktree                 branch -D <branch>                           SHARED
//	worktree.go cleanupWorktreeDir              worktree remove --force                      SHARED
//	worktree.go RemoveWorktree                  symbolic-ref --short HEAD                    per-worktree (read)
//	worktree.go RemoveWorktree                  worktree remove --force                      SHARED
//	worktree.go RemoveWorktree                  branch -D <branch>                           SHARED
//	worktree.go worktreePresent                 worktree list --porcelain                    per-worktree (read)
//	worktree.go branchExists                    branch --list <branch>                       per-worktree (read)
//	worktree.go matchingZingBranches             branch --list <patterns>                     per-worktree (read)
//	worktree.go reattachWorktree                worktree remove --force (clear stale reg.)   SHARED
//	worktree.go reattachWorktree                worktree add --no-checkout <dir> <branch>    SHARED
//	worktree.go reattachWorktree (run)          checkout                                      per-worktree
//	worktree.go ensureWorktreePresent           symbolic-ref --short HEAD                    per-worktree (read)
//
// Every SHARED row above goes through runCommon, whatever Runner it uses
// (worktree.go's "sparse-checkout init --cone" builds its own execRunner,
// like several per-worktree calls do, and passes that same value to
// runCommon rather than calling it directly). ensureWorktreeExclude is the
// one shared write that is not a git subcommand at all -- a direct read and
// append of the repository's info/exclude file -- so it holds commonMu
// itself, across both steps, through ensureWorktreeExcludeLocked below,
// rather than going through runCommon. Rule: no code holds commonMu while
// calling runCommon (no nesting); commonLockFor's own doc comment below
// lists every place in this package that takes the lock.
//
// judge.go's JudgeTree carries its own commonMu (captured at JudgeWorktree
// time, from the same registry entry its own Orchestrator resolved), since
// JudgeTree.Remove is called by internal/job directly on a value with no
// Orchestrator in scope (judging.go's own deferred cleanup). It reuses
// runCommonLocked, the same lock-and-retry body runCommon itself calls, so
// both paths share one implementation; see the (deliberate) deviation note
// in the final milestone report.

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// commonLocks is the process-wide registry handing out one *sync.Mutex per
// repository (keyed by its canonicalized common git dir), so two
// Orchestrator values pointing at the same repository -- config allows two
// project entries to do that -- serialize against each other, not just
// against their own other calls (design section 8).
var commonLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// commonLockFor returns the one *sync.Mutex every orchestrator for
// commonDir (already canonicalized by the caller) shares, creating it on
// first use.
//
// Lock sites. Three places in this package (and judge.go, same package)
// ever take the *sync.Mutex this returns: runCommon (every SHARED git call
// in the inventory above), ensureWorktreeExcludeLocked (the one shared
// write that is not a git subcommand, so it cannot go through runCommon),
// and JudgeTree.removeLocked (judge.go: a JudgeTree is called with no
// Orchestrator in scope, so it cannot reach runCommon either, and holds its
// own captured commonMu directly). Rule: no code holds the lock while
// calling runCommon (no nesting). This is the one place that list is
// spelled out; every lock site's own doc comment just names itself and
// points back here, rather than asserting its own count.
func commonLockFor(commonDir string) *sync.Mutex {
	commonLocks.mu.Lock()
	defer commonLocks.mu.Unlock()
	if commonLocks.m == nil {
		commonLocks.m = make(map[string]*sync.Mutex)
	}
	if mu, ok := commonLocks.m[commonDir]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	commonLocks.m[commonDir] = mu
	return mu
}

// canonicalCommonDir resolves dir (GitCommonDir's own absolute result) to
// the form every orchestrator for the same repository agrees on, symlinks
// included, so a project entry reached through a symlinked path still
// shares the same registry entry as one reached directly.
func canonicalCommonDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve common dir %s: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve common dir %s: %w", dir, err)
	}
	return resolved, nil
}

// resolveCommonMu resolves (and caches, via o.commonMuOnce) this
// orchestrator's own shared mutex, lazily, on its first runCommon call
// (design section 8): o.GitCommonDir needs a working git call before this
// can resolve at all, so it cannot be built eagerly in New. A resolution
// failure is cached too (sync.Once runs its function exactly once either
// way): the common git dir is a property of the repository, not a
// transient condition, so a failure here is expected to keep failing on
// retry.
func (o *Orchestrator) resolveCommonMu(ctx context.Context) (*sync.Mutex, error) {
	o.commonMuOnce.Do(func() {
		dir, err := o.GitCommonDir(ctx)
		if err != nil {
			o.commonMuErr = fmt.Errorf("orchestrator: resolve common git dir: %w", err)
			return
		}
		resolved, err := canonicalCommonDir(dir)
		if err != nil {
			o.commonMuErr = fmt.Errorf("orchestrator: resolve common git dir: %w", err)
			return
		}
		o.commonMu = commonLockFor(resolved)
	})
	return o.commonMu, o.commonMuErr
}

// sharedGitSubcommandPrefixes is the argv prefix of every shared git call
// the inventory above lists, used only by
// TestOrchestratorSerializesCommonGitWrites (through
// SharedGitSubcommandsForTest, export_test.go): the one list both the
// inventory comment and that test check every observed call against, so an
// inventory entry added here without a matching runCommon call at its call
// site fails the test, and a shared call added at a new site without an
// entry here is a gap the inventory review in code review must catch.
var sharedGitSubcommandPrefixes = [][]string{
	{"worktree", "add"},
	{"worktree", "remove"},
	{"branch", "-D"},
	{"push", "-u"},
	{"sparse-checkout", "init"},
}

// isSharedGitCommand reports whether args (a git call's own argument list,
// without the leading "git") starts with one of sharedGitSubcommandPrefixes.
func isSharedGitCommand(args []string) bool {
	for _, prefix := range sharedGitSubcommandPrefixes {
		if len(args) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if args[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// commonLockRetries is the number of retries runCommonLocked allows past
// the first attempt (design section 8: "up to 3 times"), so a command can
// run up to commonLockRetries+1 times in total before it gives up.
const commonLockRetries = 3

// commonLockRetryDelay is how long runCommonLocked waits between retries.
const commonLockRetryDelay = 200 * time.Millisecond

// lockFileContentionPattern matches git's own message for a held lock file
// -- index.lock, config.lock, packed-refs.lock, or a ref lock -- the one
// failure shape runCommonLocked retries rather than returning at once
// (design section 8): "fatal: Unable to create '<path>.lock': File
// exists." This covers collisions with git activity the shared mutex
// cannot see at all: an agent's own git commands inside its worktree, or
// "gc --auto" firing on its own.
var lockFileContentionPattern = regexp.MustCompile(`\.lock': File exists`)

// runCommonLocked runs one git command through r with commonMu already
// held, retrying up to commonLockRetries times, commonLockRetryDelay
// apart, when the command's combined output matches
// lockFileContentionPattern; any other failure, or success, returns at
// once. It is runCommon's and JudgeTree.Remove's shared body (see the
// deviation note in commonlock.go's own top-of-file comment): the only two
// callers that ever run with commonMu held.
func runCommonLocked(ctx context.Context, r Runner, dir string, args ...string) (string, error) {
	var out string
	var err error
	for attempt := 0; ; attempt++ {
		out, err = r.Run(ctx, dir, "git", args...)
		if err == nil || !lockFileContentionPattern.MatchString(out) {
			return out, err
		}
		if attempt >= commonLockRetries {
			return out, err
		}
		time.Sleep(commonLockRetryDelay)
	}
}

// runCommon runs one shared git command (the inventory above) through r --
// whichever Runner the call site already uses, o.run or a locally built
// execRunner -- with this repository's shared mutex held for the whole
// call, retried per runCommonLocked's own rule (design section 8). A
// failure to resolve the shared mutex itself (GitCommonDir failing) is
// returned without running anything.
func (o *Orchestrator) runCommon(ctx context.Context, r Runner, dir string, args ...string) (string, error) {
	mu, err := o.resolveCommonMu(ctx)
	if err != nil {
		return "", err
	}
	mu.Lock()
	defer mu.Unlock()
	return runCommonLocked(ctx, r, dir, args...)
}

// ensureWorktreeExcludeLocked holds commonMu across ensureWorktreeExclude's
// own read-check-append of info/exclude (design section 8): that write is
// shared (every worktree under this repository shares one info/exclude),
// but it is a plain file read and append, not a git subcommand, so it
// cannot go through runCommon. commonLockFor's own doc comment lists every
// lock site in this package, this one included.
func (o *Orchestrator) ensureWorktreeExcludeLocked(ctx context.Context) error {
	mu, err := o.resolveCommonMu(ctx)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	return o.ensureWorktreeExclude(ctx)
}
