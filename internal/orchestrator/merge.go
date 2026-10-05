package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// DefaultBranch is the project's default branch name, the only branch
// StartBaseMerge ever merges from.
func (o *Orchestrator) DefaultBranch() string { return o.proj.DefaultBranch }

// FetchBase fetches origin's default branch into refs/zing/base/<default>
// (fetchBase) and returns the resulting sha. A failed fetch falls back to
// the last fetched base, exactly as fetchBase does.
func (o *Orchestrator) FetchBase(ctx context.Context, wt Worktree) (string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: fetch base: %w", err)
	}
	sha, _, err := o.fetchBase(ctx, wt.ticketID)
	if err != nil {
		return "", fmt.Errorf("orchestrator: fetch base: %w", err)
	}
	return sha, nil
}

// ErrAlreadyMerged is StartBaseMerge's answer when sha is already an
// ancestor of HEAD: there is nothing to merge.
var ErrAlreadyMerged = errors.New("orchestrator: the base is already merged into the ticket branch")

// StartBaseMerge starts merging sha into the ticket branch and returns the
// paths git left unmerged in the index, sorted (empty for a clean merge).
// If a merge is already in progress (MERGE_HEAD resolves), it starts
// nothing and returns the current unmerged paths, so a later tick can call
// it again. It never commits: git merge --no-ff --no-commit --no-edit.
// Exit 1 from git merge is a conflict, not an error, as long as MERGE_HEAD
// exists afterwards; exit 1 with no MERGE_HEAD (git refused, for example a
// dirty tree) is an error naming git's output.
func (o *Orchestrator) StartBaseMerge(ctx context.Context, wt Worktree, sha string) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: start base merge: %w", err)
	}
	inProgress, err := o.mergeInProgress(ctx, wt)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: start base merge: %w", err)
	}
	if !inProgress {
		merged, err := o.IsAncestor(ctx, wt, sha, "HEAD")
		if err != nil {
			return nil, fmt.Errorf("orchestrator: start base merge: %w", err)
		}
		if merged {
			return nil, ErrAlreadyMerged
		}
		run := execRunner{drivers: wt.drivers}
		out, mergeErr := run.Run(ctx, wt.dir, "git", "-c", "merge.conflictStyle=merge", "merge", "--no-ff", "--no-commit", "--no-edit", sha)
		if mergeErr != nil && !isExitCode1(mergeErr) {
			return nil, fmt.Errorf("orchestrator: start base merge: git merge: %w: %s", mergeErr, strings.TrimSpace(out))
		}
		started, err := o.mergeInProgress(ctx, wt)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: start base merge: %w", err)
		}
		if !started {
			return nil, fmt.Errorf("orchestrator: start base merge: git merge left no MERGE_HEAD: %s", strings.TrimSpace(out))
		}
		o.log.Info("base merge started", "branch", wt.branch, "base_sha", sha)
	}
	return o.unmergedIndexPaths(ctx, wt)
}

// mergeInProgress reports whether MERGE_HEAD resolves in wt
// (git rev-parse -q --verify MERGE_HEAD; exit 1 is false).
func (o *Orchestrator) mergeInProgress(ctx context.Context, wt Worktree) (bool, error) {
	run := execRunner{drivers: wt.drivers}
	if _, err := run.Output(ctx, wt.dir, "git", "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		if isExitCode1(err) {
			return false, nil
		}
		return false, fmt.Errorf("orchestrator: merge in progress: %w", err)
	}
	return true, nil
}

// unmergedIndexPaths is git diff --name-only --diff-filter=U -z, sorted.
func (o *Orchestrator) unmergedIndexPaths(ctx context.Context, wt Worktree) ([]string, error) {
	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: unmerged index paths: %w", err)
	}
	return splitNulPaths(out), nil
}

// splitNulPaths splits a NUL-delimited git path list, dropping empty
// records, and returns the result sorted.
func splitNulPaths(out string) []string {
	paths := make([]string, 0)
	for p := range strings.SplitSeq(out, "\x00") {
		if p == "" {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// minConflictMarkerRun is git's default conflict-marker-size: the shortest
// run of '<' or '>' a conflict hunk's bounding line ever starts with. A
// path or .gitattributes can raise conflict-marker-size, which widens the
// run git writes, so hasConflictMarkerPrefix matches seven or more, not
// exactly seven.
const minConflictMarkerRun = 7

// hasConflictMarkerPrefix reports whether line starts with a run of seven
// or more '<' characters, or seven or more '>' characters, followed by a
// space: git's "merge" conflict style's own bounding lines, at any
// conflict-marker-size.
func hasConflictMarkerPrefix(line []byte) bool {
	return hasConflictMarkerRun(line, '<') || hasConflictMarkerRun(line, '>')
}

func hasConflictMarkerRun(line []byte, b byte) bool {
	n := 0
	for n < len(line) && line[n] == b {
		n++
	}
	return n >= minConflictMarkerRun && n < len(line) && line[n] == ' '
}

// ConflictMarkerPaths returns, sorted, every path still unresolved: a
// binary path the index lists as unmerged (unmergedIndexPaths: git's own
// "U" status) unconditionally, since a binary conflict leaves no text
// marker an agent could write or clear, plus, among the index's unmerged
// text paths and the working tree's other changed paths (MergeChangedPaths:
// tracked or untracked, since an agent can "git rm --cached" a conflicting
// path and leave its markers on disk), every one whose working-tree file
// has a line matching hasConflictMarkerPrefix. An absent file or a
// non-regular file is skipped (and, for the binary test, treated as text,
// so it is skipped the same way filterConflictMarkerPaths skips it). Files
// are read through os.OpenRoot(wt.dir), so a path never escapes the
// worktree.
func (o *Orchestrator) ConflictMarkerPaths(ctx context.Context, wt Worktree) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: conflict marker paths: %w", err)
	}
	unmerged, err := o.unmergedIndexPaths(ctx, wt)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: conflict marker paths: %w", err)
	}

	root, err := os.OpenRoot(wt.dir)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: conflict marker paths: %w", err)
	}
	defer root.Close()

	result := make(map[string]struct{}, len(unmerged))
	for _, p := range unmerged {
		binary, binErr := fileLooksBinary(root, p)
		if binErr != nil {
			return nil, fmt.Errorf("orchestrator: conflict marker paths: %s: %w", p, binErr)
		}
		if binary {
			result[p] = struct{}{}
		}
	}

	changed, err := o.MergeChangedPaths(ctx, wt)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: conflict marker paths: %w", err)
	}
	candidates := make([]string, 0, len(unmerged)+len(changed))
	seenCandidate := make(map[string]struct{}, len(unmerged)+len(changed))
	addCandidate := func(p string) {
		if _, already := result[p]; already {
			return
		}
		if _, already := seenCandidate[p]; already {
			return
		}
		seenCandidate[p] = struct{}{}
		candidates = append(candidates, p)
	}
	for _, p := range unmerged {
		addCandidate(p)
	}
	for _, p := range changed {
		addCandidate(p)
	}

	marked, err := filterConflictMarkerPaths(wt.dir, candidates)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: conflict marker paths: %w", err)
	}
	for _, p := range marked {
		result[p] = struct{}{}
	}

	out := make([]string, 0, len(result))
	for p := range result {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// looksBinarySniffLen is how much of a file's start fileLooksBinary reads:
// enough to catch the NUL byte git's own buffer_is_binary heuristic keys
// on, without reading an arbitrarily large file in full.
const looksBinarySniffLen = 8000

// fileLooksBinary reports whether path, read through root, starts with a
// NUL byte within its first looksBinarySniffLen bytes -- git's own
// heuristic for "binary", and the reason a binary conflict's working-tree
// file can never hold a text conflict marker. An absent file, a
// non-regular file, or an empty file is reported false (text) with no
// error, so the caller falls back to the marker scan, exactly as it does
// for every other such path.
func fileLooksBinary(root *os.Root, path string) (bool, error) {
	info, err := root.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}

	f, err := root.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, looksBinarySniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) >= 0, nil
}

// PathsWithConflictMarkers filters paths to the ones, sorted, whose current
// working-tree file holds a conflict marker line -- the same test
// ConflictMarkerPaths runs, but over a caller-supplied path list rather
// than the index's own unmerged paths. adoptMerge (merge job unit) uses
// this to check an already-committed merge's own changed paths, once
// MERGE_HEAD no longer resolves.
func (o *Orchestrator) PathsWithConflictMarkers(ctx context.Context, wt Worktree, paths []string) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: paths with conflict markers: %w", err)
	}
	marked, err := filterConflictMarkerPaths(wt.dir, paths)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: paths with conflict markers: %w", err)
	}
	return marked, nil
}

// filterConflictMarkerPaths is ConflictMarkerPaths' and
// PathsWithConflictMarkers' shared scan: candidates, sorted, whose
// working-tree file (read through os.OpenRoot(dir), so a path never
// escapes it) has a line starting "<<<<<<< " or ">>>>>>> ". An absent file
// or a non-regular file is skipped.
func filterConflictMarkerPaths(dir string, candidates []string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	marked := make([]string, 0)
	for _, p := range candidates {
		has, err := fileHasConflictMarkers(root, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if has {
			marked = append(marked, p)
		}
	}
	sort.Strings(marked)
	return marked, nil
}

// fileHasConflictMarkers reports whether path, read through root, has a
// line matching hasConflictMarkerPrefix. A path that does not exist, or
// that is not a regular file, is reported false with no error.
func fileHasConflictMarkers(root *os.Root, path string) (bool, error) {
	info, err := root.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}

	f, err := root.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	// bufio.Reader.ReadLine never fails on a long line: a line longer than
	// its buffer comes back in bounded chunks with isPrefix true until the
	// line ends, unlike bufio.Scanner, which gives up with ErrTooLong past
	// its fixed buffer. A conflict marker prefix is only ever eight bytes,
	// so only the first chunk of each line needs the check.
	r := bufio.NewReader(f)
	atLineStart := true
	for {
		chunk, isPrefix, readErr := r.ReadLine()
		if readErr != nil {
			if readErr == io.EOF {
				return false, nil
			}
			return false, readErr
		}
		if atLineStart && hasConflictMarkerPrefix(chunk) {
			return true, nil
		}
		atLineStart = !isPrefix
	}
}

// StageResult sorts StageResolvedPaths' own unmerged paths into three
// disjoint, sorted sets.
type StageResult struct {
	// Staged is every unmerged path that held no conflict marker and was
	// not binary: StageResolvedPaths ran "git add -u" on these.
	Staged []string
	// Marked is every unmerged path whose working-tree file still has a
	// conflict marker line. Left unstaged.
	Marked []string
	// Binary is every unmerged path fileLooksBinary reported binary. Left
	// unstaged: a binary conflict's file never carries a text marker an
	// agent could clear.
	Binary []string
}

// Unresolved reports whether any path stayed unstaged: a marked or binary
// conflict the merge agent has not cleared.
func (r StageResult) Unresolved() bool {
	return len(r.Marked) > 0 || len(r.Binary) > 0
}

// StageResolvedPaths reads the index's own unmerged paths and sorts each one
// into res.Binary (fileLooksBinary), res.Marked (fileHasConflictMarkers, the
// same marker scan ConflictMarkerPaths uses), or res.Staged (neither). It
// then runs "git add -u" on res.Staged, so a conflict the merge agent
// resolved in the working tree but never told git about reaches CHECK
// staged, exactly as CommitMerge's own "git add -u" would stage it at
// commit time. It is an error when no merge is in progress.
func (o *Orchestrator) StageResolvedPaths(ctx context.Context, wt Worktree) (StageResult, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %w", err)
	}
	inProgress, err := o.mergeInProgress(ctx, wt)
	if err != nil {
		return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %w", err)
	}
	if !inProgress {
		return StageResult{}, errors.New("orchestrator: stage resolved paths: no merge in progress")
	}

	unmerged, err := o.unmergedIndexPaths(ctx, wt)
	if err != nil {
		return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %w", err)
	}

	root, err := os.OpenRoot(wt.dir)
	if err != nil {
		return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %w", err)
	}
	defer root.Close()

	res := StageResult{Staged: make([]string, 0), Marked: make([]string, 0), Binary: make([]string, 0)}
	for _, p := range unmerged {
		binary, binErr := fileLooksBinary(root, p)
		if binErr != nil {
			return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %s: %w", p, binErr)
		}
		if binary {
			res.Binary = append(res.Binary, p)
			continue
		}
		marked, markErr := fileHasConflictMarkers(root, p)
		if markErr != nil {
			return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %s: %w", p, markErr)
		}
		if marked {
			res.Marked = append(res.Marked, p)
			continue
		}
		res.Staged = append(res.Staged, p)
	}

	if len(res.Staged) > 0 {
		litRun := execRunner{extraEnv: literalPathspecEnv, drivers: wt.drivers}
		if err := runPathspecCommand(ctx, litRun, wt.dir, res.Staged, "add", "-u"); err != nil {
			return StageResult{}, fmt.Errorf("orchestrator: stage resolved paths: %w", err)
		}
	}

	return res, nil
}

// MergeSidePaths returns, sorted, every path that differs between HEAD
// and MERGE_HEAD: the set a merge may legitimately change. It is an error
// when no merge is in progress.
func (o *Orchestrator) MergeSidePaths(ctx context.Context, wt Worktree) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: merge side paths: %w", err)
	}
	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "diff", "--name-only", "--no-renames", "-z", "HEAD", "MERGE_HEAD")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: merge side paths: %w", err)
	}
	return splitNulPaths(out), nil
}

// BaseLog returns the full messages of at most 200 commits on sha that
// HEAD lacks, newest first, each as its sha line then its message.
func (o *Orchestrator) BaseLog(ctx context.Context, wt Worktree, sha string) (string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: base log: %w", err)
	}
	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "log", "-n", "200", "--format=%H%n%B", "HEAD.."+sha)
	if err != nil {
		return "", fmt.Errorf("orchestrator: base log: %w", err)
	}
	return out, nil
}

// CommitParents returns sha's parent shas in order (first parent first).
func (o *Orchestrator) CommitParents(ctx context.Context, wt Worktree, sha string) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: commit parents: %w", err)
	}
	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: commit parents: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return nil, fmt.Errorf("orchestrator: commit parents: empty rev-list output for %s", sha)
	}
	return fields[1:], nil
}
