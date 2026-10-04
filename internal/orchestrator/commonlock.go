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
//	judge.go  JudgeWorktree (fetchBase)          see worktree.go's fetchBase rows below      --
//	judge.go  readGovernanceFiles               show <baseRev>:<name>                       per-worktree (read)
//	perimeter.go ChangedPaths                   status --porcelain=v1 ...                  per-worktree (read)
//	perimeter.go RevertPaths (litRun)           restore --staged [--worktree] ...          per-worktree (worktree's own index/working tree)
//	perimeter.go Hunk                           diff ...                                    per-worktree (read)
//	perimeter.go BranchCommits                  rev-list --reverse <baseRev>..HEAD        per-worktree (read)
//	push.go   Push (fetchBase)                  see worktree.go's fetchBase rows below      --
//	push.go   Push                              push origin <refspec>                      per-worktree (network I/O; only writes refs/remotes/origin/<branch>, a ref update -- PR review fix C3)
//	push.go   Push                              config --local branch.<b>.remote/.merge    SHARED (writes branch.<b>.* in the shared config)
//	push.go   unpushedShas                      log -z --format=%H <baseRev>..<branch>    per-worktree (read)
//	review.go HeadSHA                           rev-parse HEAD                             per-worktree (read)
//	review.go Diff (fetchBase)                  see worktree.go's fetchBase rows below      --
//	review.go Diff                              merge-base, diff ...                       per-worktree (read)
//	review.go ChangedFilesBetween               diff --name-only --no-renames -z ...        per-worktree (read)
//	review.go ChangedFilesSinceBase             merge-base <baseRev> <to>                   per-worktree (read)
//	review.go IsAncestor                        merge-base --is-ancestor                    per-worktree (read)
//	worktree.go checkRefFormat                  check-ref-format                            per-worktree (no repository touched at all)
//	worktree.go revalidate                      symbolic-ref --short HEAD                   per-worktree (read)
//	worktree.go GitCommonDir                    rev-parse --git-common-dir                  per-worktree (read)
//	worktree.go gitPathInfoExclude              rev-parse --git-path info/exclude           per-worktree (read)
//	worktree.go ensureWorktreeExclude           (no git call: direct info/exclude read+append) SHARED, see below
//	worktree.go runSparseCheckoutSet            sparse-checkout set --stdin                 per-worktree (worktree's own admin-dir sparse-checkout file)
//	worktree.go fetchBase                       fetch --no-tags --no-write-fetch-head --no-auto-maintenance --no-recurse-submodules --refmap= origin +refs/heads/D:TMP  per-worktree (network I/O; --refmap= disables the remote's configured fetch mapping, --no-write-fetch-head/--no-auto-maintenance keep it from touching the shared FETCH_HEAD file or running gc, and --no-recurse-submodules keeps it from also fetching a populated submodule, so this writes only its own TMP ref under refs/zing/fetch/, never refs/remotes/origin/* or FETCH_HEAD, no lock held -- same reasoning as push.go's own Push row)
//	worktree.go revParseCommit, resolveBase     rev-parse --verify --quiet ...               per-worktree (read; also run under commonMu, inside updateBaseLocked/advanceBaseLocked, for a consistent view of baseRef)
//	worktree.go isAncestorRev                   merge-base --is-ancestor                     per-worktree (read, run under commonMu inside advanceBaseLocked, same reason)
//	worktree.go updateBaseLocked                update-ref <baseRef> <src> ""                SHARED (seeds baseRef from the local default branch after a failed fetch finds nothing to fall back to; holds commonMu itself, see below)
//	worktree.go advanceBaseLocked               update-ref <baseRef> <new> [<old>]           SHARED (moves baseRef forward after a successful fetch; runs under the commonMu its caller, updateBaseLocked, already holds -- see below)
//	worktree.go removeFetchRef                  update-ref -d <TMP>                          SHARED (deletes fetchBase's own temporary ref; holds commonMu itself, see below)
//	worktree.go baseRev                          (no git call: resolveBase, see above)        per-worktree (read)
//	worktree.go PrepareWorktree                 worktree add --no-checkout -b <branch> <baseRef> SHARED
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
// runCommon rather than calling it directly), except the "update-ref" rows
// fetchBase's own helpers write: updateBaseLocked and removeFetchRef each
// already hold commonMu themselves (the same "holds the lock itself" shape
// ensureWorktreeExclude and JudgeTree.removeLocked already use below), so
// they call o.run directly rather than through runCommon, which would try
// to take the lock a second time. advanceBaseLocked's own update-ref runs
// under the commonMu its caller, updateBaseLocked, already holds, rather
// than taking the lock a third time. ensureWorktreeExclude is the one shared
// write that is not a git subcommand at all -- a direct read and append of
// the repository's info/exclude file -- so it holds commonMu itself, across
// both steps, through ensureWorktreeExcludeLocked below, rather than going
// through runCommon. Rule: no code holds commonMu while calling runCommon
// (no nesting); commonLockFor's own doc comment below lists every place in
// this package that takes the lock.
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

// commonMutex is a context-aware mutual-exclusion lock (PR review fix C4):
// commonMu used to be a plain *sync.Mutex, so a waiter stuck behind another
// ticket's long shared git write (a slow push, a stalled lock-contention
// retry) could not be interrupted even once its own ctx ended. It is a
// channel-based binary semaphore -- a capacity-1 channel holding a single
// token, present exactly when the lock is free -- not a sync.Mutex, so
// acquiring it can select on ctx.Done() alongside taking the token.
type commonMutex struct {
	ch chan struct{} // capacity 1; a token present means unlocked
}

// newCommonMutex returns a commonMutex ready to be locked, its one token
// already in place.
func newCommonMutex() *commonMutex {
	m := &commonMutex{ch: make(chan struct{}, 1)}
	m.ch <- struct{}{}
	return m
}

// Lock acquires m, waiting for the token or for ctx to end, whichever
// comes first. Every lock site below passes the same ctx its own git call
// (or file write) will use, so a caller that gives up while still waiting
// never also starts that call.
func (m *commonMutex) Lock(ctx context.Context) error {
	select {
	case <-m.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock returns m's token. A caller that never successfully Locked must
// never call this.
func (m *commonMutex) Unlock() {
	m.ch <- struct{}{}
}

// TryLock reports whether m was free and, if so, takes its token (ordinary
// sync.Mutex.TryLock semantics: the caller must Unlock an Locked m). It
// never waits. CommonMuHeldForTest (export_test.go) is its only caller.
func (m *commonMutex) TryLock() bool {
	select {
	case <-m.ch:
		return true
	default:
		return false
	}
}

// commonLocks is the process-wide registry handing out one *commonMutex
// per repository (keyed by its canonicalized common git dir), so two
// Orchestrator values pointing at the same repository -- config allows two
// project entries to do that -- serialize against each other, not just
// against their own other calls (design section 8).
var commonLocks struct {
	mu sync.Mutex
	m  map[string]*commonMutex
}

// commonLockFor returns the one *commonMutex every orchestrator for
// commonDir (already canonicalized by the caller) shares, creating it on
// first use.
//
// Lock sites. Five places in this package (and judge.go, same package) ever
// take the *commonMutex this returns: runCommon (every SHARED git call in
// the inventory above), ensureWorktreeExcludeLocked (the one shared write
// that is not a git subcommand, so it cannot go through runCommon),
// JudgeTree.removeLocked (judge.go: a JudgeTree is called with no
// Orchestrator in scope, so it cannot reach runCommon either, and holds its
// own captured commonMu directly), and fetchBase's own two helpers --
// updateBaseLocked and removeFetchRef -- which hold the lock themselves
// across a short sequence of rev-parse/merge-base/update-ref calls rather
// than one single SHARED command, so they cannot go through runCommon
// either. advanceBaseLocked, updateBaseLocked's own helper, never takes the
// lock itself: it runs entirely under the commonMu updateBaseLocked already
// holds. Rule: no code holds the lock while calling runCommon (no nesting).
// This is the one place that list is spelled out; every lock site's own doc
// comment just names itself and points back here, rather than asserting its
// own count.
func commonLockFor(commonDir string) *commonMutex {
	commonLocks.mu.Lock()
	defer commonLocks.mu.Unlock()
	if commonLocks.m == nil {
		commonLocks.m = make(map[string]*commonMutex)
	}
	if mu, ok := commonLocks.m[commonDir]; ok {
		return mu
	}
	mu := newCommonMutex()
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

// resolveCommonMu resolves (and caches, guarded by o.commonMuGuard) this
// orchestrator's own shared mutex, lazily, on its first runCommon call
// (design section 8): o.GitCommonDir needs a working git call before this
// can resolve at all, so it cannot be built eagerly in New.
//
// Only a success is cached (PR review fix C1): a prior sync.Once-based
// version cached a failure too, on the theory that the common git dir is a
// property of the repository, not a transient condition. That is not true
// of a canceled or timed-out ctx -- a caller that happens to resolve the
// mutex for the first time with a bad ctx (serve shutdown, in particular)
// would otherwise poison every later call, including cleanupWorktree's own
// detached, generously-timed-out ctx, with the same stale error forever.
// A mutex guard, not sync.Once, makes retrying after a failure possible;
// commonMuGuard is held only across this function's own body, never across
// a git call, so a failed GitCommonDir call here cannot deadlock against a
// concurrent caller of CommonMuHeldForTest (export_test.go), which itself
// calls back into this function.
func (o *Orchestrator) resolveCommonMu(ctx context.Context) (*commonMutex, error) {
	// The guard covers only the cached read and the publish, never the git
	// call itself, so a Runner that calls back into resolveCommonMu cannot
	// deadlock on it. Two concurrent resolvers may both run GitCommonDir;
	// commonLockFor hands both the same mutex, and the first to publish wins.
	o.commonMuGuard.Lock()
	cached := o.commonMu
	o.commonMuGuard.Unlock()
	if cached != nil {
		return cached, nil
	}
	dir, err := o.GitCommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: resolve common git dir: %w", err)
	}
	resolved, err := canonicalCommonDir(dir)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: resolve common git dir: %w", err)
	}
	o.commonMuGuard.Lock()
	defer o.commonMuGuard.Unlock()
	if o.commonMu == nil {
		o.commonMu = commonLockFor(resolved)
	}
	return o.commonMu, nil
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
	{"config", "--local"},
	{"sparse-checkout", "init"},
	{"update-ref"},
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
		// Checked before every attempt, including the first (PR review fix
		// C2): a ctx that ended while this call waited for commonMu itself
		// (runCommon's own mu.Lock(ctx)) must not still launch one more git
		// subprocess.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		out, err = r.Run(ctx, dir, "git", args...)
		if err == nil || !lockFileContentionPattern.MatchString(out) {
			return out, err
		}
		if attempt >= commonLockRetries {
			return out, err
		}
		// A select on ctx.Done(), not a bare time.Sleep (PR review fix C2):
		// this runs with commonMu held, so a bare sleep would also block
		// every other goroutine waiting on the same mutex for the full
		// delay even after shutdown has started, and would still launch
		// one more git subprocess once it woke.
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(commonLockRetryDelay):
		}
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
	if lockErr := mu.Lock(ctx); lockErr != nil {
		return "", lockErr
	}
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
	if lockErr := mu.Lock(ctx); lockErr != nil {
		return lockErr
	}
	defer mu.Unlock()
	return o.ensureWorktreeExclude(ctx)
}
