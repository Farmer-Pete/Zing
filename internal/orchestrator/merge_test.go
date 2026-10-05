package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	mergeSharedPath     = "shared.txt"
	mergeTicketOnlyPath = "ticketonly.txt"
	mergeBaseOnlyPath   = "baseonly.txt"
	mergeTestTitle      = "Merge main into the ticket branch"
	mergeTestFuncLine   = "a merge func line"
)

// mergeFixture is a signed repository with a ticket branch (ticketHead) and
// a main-branch commit (baseSHA) that both edit mergeSharedPath past their
// common ancestor, so merging baseSHA into the ticket branch conflicts on
// that one file.
type mergeFixture struct {
	repo       string
	o          *Orchestrator
	wt         Worktree
	ticketHead string
	baseSHA    string
}

// newMergeConflictFixture builds a mergeFixture whose ticket branch also
// adds mergeTicketOnlyPath and whose main-branch commit also adds
// mergeBaseOnlyPath, so a test can check a clean per-file merge alongside
// the one conflicting file.
func newMergeConflictFixture(t *testing.T, ticketID int64) mergeFixture {
	t.Helper()
	ctx := t.Context()

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one\n")
	runGit(ctx, t, repo, "add", mergeSharedPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "seed shared.txt")

	wt, err := o.PrepareWorktree(ctx, ticketID, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one TICKET\n")
	writeTestFile(t, filepath.Join(wt.Dir(), mergeTicketOnlyPath), "ticket only\n")
	runGit(ctx, t, wt.Dir(), "add", mergeSharedPath, mergeTicketOnlyPath)
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits shared.txt")
	ticketHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one MAIN\n")
	writeTestFile(t, filepath.Join(repo, mergeBaseOnlyPath), "base only\n")
	runGit(ctx, t, repo, "add", mergeSharedPath, mergeBaseOnlyPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "main edits shared.txt")
	baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	return mergeFixture{repo: repo, o: o, wt: wt, ticketHead: ticketHead, baseSHA: baseSHA}
}

func mergeTestMessage() CommitMessage {
	return CommitMessage{Title: mergeTestTitle, FuncLines: []string{mergeTestFuncLine}}
}

// assertMergeHeadFile checks MERGE_HEAD the way git itself resolves a
// pseudoref: by reading the file at its git-path directly, not through
// "git update-ref", which on git 2.55 does not create a MERGE_HEAD file
// (the gap this test exists to catch). It also checks that git's own
// rev-parse agrees, now that the file is restored.
func assertMergeHeadFile(ctx context.Context, t *testing.T, dir, wantSHA string) {
	t.Helper()
	gitPath := strings.TrimSpace(runGit(ctx, t, dir, "rev-parse", "--git-path", "MERGE_HEAD"))
	path := gitPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read MERGE_HEAD file %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(content)); got != wantSHA {
		t.Errorf("MERGE_HEAD file content = %q, want %q", got, wantSHA)
	}

	mergeHeadNow := strings.TrimSpace(runGit(ctx, t, dir, "rev-parse", "MERGE_HEAD"))
	if mergeHeadNow != wantSHA {
		t.Errorf("git rev-parse MERGE_HEAD = %q, want it restored to %q", mergeHeadNow, wantSHA)
	}
}

// -----------------------------------------------------------------------
// TestCommitMergeSignedTwoParents (task 1's named, demo test)
// -----------------------------------------------------------------------

func TestCommitMergeSignedTwoParents(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 1)

	conflicted, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA)
	if err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(conflicted, want) {
		t.Fatalf("StartBaseMerge conflicted = %v, want %v", conflicted, want)
	}

	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")

	sha, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage(), f.baseSHA)
	if err != nil {
		t.Fatalf("CommitMerge: unexpected error: %v", err)
	}
	if sha == "" {
		t.Fatal("CommitMerge: expected a non-empty sha")
	}
	t.Logf("merge sha %s, parents [%s %s]", sha, f.ticketHead, f.baseSHA)

	parents, err := f.o.CommitParents(ctx, f.wt, sha)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
	if want := []string{f.ticketHead, f.baseSHA}; !slices.Equal(parents, want) {
		t.Errorf("CommitParents = %v, want %v", parents, want)
	}

	signed, err := f.o.SignedStatus(ctx, f.wt, sha)
	if err != nil {
		t.Fatalf("SignedStatus: %v", err)
	}
	if !signed {
		t.Error("SignedStatus = false, want true")
	}

	status := runGit(ctx, t, f.wt.Dir(), "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after CommitMerge: %q", status)
	}
	if inProgress, err := f.o.mergeInProgress(ctx, f.wt); err != nil {
		t.Fatalf("mergeInProgress: %v", err)
	} else if inProgress {
		t.Error("mergeInProgress = true after CommitMerge, want false")
	}
}

// -----------------------------------------------------------------------
// StartBaseMerge
// -----------------------------------------------------------------------

func TestStartBaseMergeResumesInProgress(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 2)

	first, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA)
	if err != nil {
		t.Fatalf("StartBaseMerge (first): unexpected error: %v", err)
	}

	headBefore := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "HEAD"))
	placeholder := "mid-resolution placeholder\n"
	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), placeholder)

	second, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA)
	if err != nil {
		t.Fatalf("StartBaseMerge (second): unexpected error: %v", err)
	}
	if !slices.Equal(first, second) {
		t.Errorf("second StartBaseMerge paths = %v, want %v", second, first)
	}

	headAfter := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "HEAD"))
	if headAfter != headBefore {
		t.Errorf("HEAD = %q after a second StartBaseMerge, want unchanged %q", headAfter, headBefore)
	}

	got := readFileString(t, filepath.Join(f.wt.Dir(), mergeSharedPath))
	if got != placeholder {
		t.Errorf("shared.txt = %q after a second StartBaseMerge, want unchanged placeholder %q", got, placeholder)
	}
}

func TestStartBaseMergeAlreadyMerged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 3)

	// f.ticketHead's parent (the seed commit) is an ancestor of HEAD.
	seedSHA := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", f.ticketHead+"^"))

	conflicted, err := f.o.StartBaseMerge(ctx, f.wt, seedSHA)
	if err == nil {
		t.Fatalf("StartBaseMerge: expected an error, got conflicted=%v", conflicted)
	}
	if !errors.Is(err, ErrAlreadyMerged) {
		t.Errorf("StartBaseMerge error = %v, want ErrAlreadyMerged", err)
	}

	if inProgress, err := f.o.mergeInProgress(ctx, f.wt); err != nil {
		t.Fatalf("mergeInProgress: %v", err)
	} else if inProgress {
		t.Error("mergeInProgress = true after ErrAlreadyMerged, want false")
	}
}

func TestStartBaseMergeClean(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, 4, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), "ticket-file.txt"), "ticket\n")
	runGit(ctx, t, wt.Dir(), "add", "ticket-file.txt")
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits a different file")

	writeTestFile(t, filepath.Join(repo, "base-file.txt"), "base\n")
	runGit(ctx, t, repo, "add", "base-file.txt")
	runGit(ctx, t, repo, "commit", "-q", "-m", "main edits a different file")
	baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	conflicted, err := o.StartBaseMerge(ctx, wt, baseSHA)
	if err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	if len(conflicted) != 0 {
		t.Errorf("StartBaseMerge conflicted = %v, want empty", conflicted)
	}

	if inProgress, err := o.mergeInProgress(ctx, wt); err != nil {
		t.Fatalf("mergeInProgress: %v", err)
	} else if !inProgress {
		t.Error("mergeInProgress = false after a clean StartBaseMerge, want true")
	}

	staged := runGit(ctx, t, wt.Dir(), "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "base-file.txt") {
		t.Errorf("expected base-file.txt staged after a clean merge, staged = %q", staged)
	}
}

// TestStartBaseMergeDirtyTreeRefuses proves StartBaseMerge's own error path
// other than ErrAlreadyMerged: an uncommitted edit to the very file the
// base side also changed makes "git merge" refuse outright ("local
// changes ... would be overwritten by merge"), so StartBaseMerge returns
// an error naming git's own output and starts no merge at all.
func TestStartBaseMergeDirtyTreeRefuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 75)

	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "uncommitted local edit\n")

	conflicted, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA)
	if err == nil {
		t.Fatalf("StartBaseMerge: expected an error for a dirty tree, got conflicted=%v", conflicted)
	}
	if errors.Is(err, ErrAlreadyMerged) {
		t.Errorf("StartBaseMerge error = %v, want something other than ErrAlreadyMerged", err)
	}
	if !strings.Contains(err.Error(), "git merge") {
		t.Errorf("StartBaseMerge error = %q, want it to name git merge's own output", err.Error())
	}

	if inProgress, err := f.o.mergeInProgress(ctx, f.wt); err != nil {
		t.Fatalf("mergeInProgress: %v", err)
	} else if inProgress {
		t.Error("mergeInProgress = true after a refused StartBaseMerge, want false")
	}
}

// -----------------------------------------------------------------------
// ConflictMarkerPaths
// -----------------------------------------------------------------------

func TestConflictMarkerPaths(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 5)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	writeTestFile(t, filepath.Join(f.wt.Dir(), "equals.txt"), "=======\n")
	runGit(ctx, t, f.wt.Dir(), "add", "equals.txt")

	marked, err := f.o.ConflictMarkerPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(marked, want) {
		t.Fatalf("ConflictMarkerPaths = %v, want %v", marked, want)
	}

	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")

	marked, err = f.o.ConflictMarkerPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths (after rewrite): unexpected error: %v", err)
	}
	if len(marked) != 0 {
		t.Errorf("ConflictMarkerPaths (after rewrite) = %v, want empty", marked)
	}
}

// TestConflictMarkerPathsLongLine proves fileHasConflictMarkers never fails
// with bufio.ErrTooLong on a line longer than its old 1 MiB scanner buffer
// (a lockfile, a minified bundle, embedded data): a file with one such long
// line and no marker scans clean, and the real conflict in mergeSharedPath
// is still found alongside it (review thread t01081b994b9e0591).
func TestConflictMarkerPathsLongLine(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 6)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	longLine := strings.Repeat("a", 2*1024*1024) + "\n"
	writeTestFile(t, filepath.Join(f.wt.Dir(), "long.txt"), longLine)
	runGit(ctx, t, f.wt.Dir(), "add", "long.txt")

	marked, err := f.o.ConflictMarkerPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(marked, want) {
		t.Fatalf("ConflictMarkerPaths = %v, want %v", marked, want)
	}
}

// TestConflictMarkerPathsWiderMarker proves hasConflictMarkerPrefix
// recognizes a conflict marker wider than git's default seven characters:
// a path or .gitattributes can raise conflict-marker-size, and git then
// writes that many '<' or '>' characters instead of seven (review thread
// t156d0ccb1d965314).
func TestConflictMarkerPathsWiderMarker(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 7)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	wider := strings.Repeat("<", 12) + " HEAD\nline one TICKET\n" + strings.Repeat("=", 12) + "\nline one MAIN\n" + strings.Repeat(">", 12) + " " + f.baseSHA + "\n"
	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), wider)

	marked, err := f.o.ConflictMarkerPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(marked, want) {
		t.Fatalf("ConflictMarkerPaths = %v, want %v", marked, want)
	}
}

// TestConflictMarkerPathsUntracked proves ConflictMarkerPaths still finds a
// conflicting path once it is untracked (for example by "git rm --cached")
// but left on disk still holding its markers: a tracked-only diff against
// HEAD would miss it entirely (review thread t6abdd4b1b67ca490).
func TestConflictMarkerPathsUntracked(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 8)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	runGit(ctx, t, f.wt.Dir(), "rm", "--cached", "--force", mergeSharedPath)

	marked, err := f.o.ConflictMarkerPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(marked, want) {
		t.Fatalf("ConflictMarkerPaths = %v, want %v", marked, want)
	}
}

// TestConflictMarkerPathsBinaryConflict proves ConflictMarkerPaths reports
// a conflicting binary file even though it holds no text conflict marker
// line: git's own unmerged ("U") index status already proves the path
// unresolved, so ConflictMarkerPaths must not drop it just because
// filterConflictMarkerPaths finds no marker in its (binary) content
// (review thread tc2309919ac69222b).
func TestConflictMarkerPathsBinaryConflict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	const binPath = "image.bin"
	writeTestBinaryFile(t, filepath.Join(repo, binPath), []byte{0x00, 0x01, 0x02})
	runGit(ctx, t, repo, "add", binPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "seed image.bin")

	wt, err := o.PrepareWorktree(ctx, 9, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestBinaryFile(t, filepath.Join(wt.Dir(), binPath), []byte{0x00, 0x10, 0x11, 0x12})
	runGit(ctx, t, wt.Dir(), "add", binPath)
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits image.bin")

	writeTestBinaryFile(t, filepath.Join(repo, binPath), []byte{0x00, 0x20, 0x21, 0x22})
	runGit(ctx, t, repo, "add", binPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "main edits image.bin")
	baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	if _, mergeErr := o.StartBaseMerge(ctx, wt, baseSHA); mergeErr != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", mergeErr)
	}

	marked, err := o.ConflictMarkerPaths(ctx, wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths: unexpected error: %v", err)
	}
	if want := []string{binPath}; !slices.Equal(marked, want) {
		t.Fatalf("ConflictMarkerPaths = %v, want %v", marked, want)
	}
}

// writeTestBinaryFile is writeTestFile for raw, non-UTF8 content: a NUL
// byte is what makes git treat the path as binary.
func writeTestBinaryFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestPathsWithConflictMarkers proves PathsWithConflictMarkers over a real,
// already-committed merge: adoptMerge's own use, once MERGE_HEAD no longer
// resolves, scanning the commit's own changed paths rather than the index's
// unmerged ones. Given a candidate list naming both a file that still holds
// a marker and one that does not, only the former comes back; a candidate
// path that does not exist on disk is silently skipped.
func TestPathsWithConflictMarkers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 51)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	// Stage mergeSharedPath still holding its conflict markers, then commit
	// the merge, so the committed blob -- not just the live index -- still
	// holds them; mergeTicketOnlyPath is clean and never touched by the
	// merge at all.
	runGit(ctx, t, f.wt.Dir(), "add", mergeSharedPath)
	if _, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage(), f.baseSHA); err != nil {
		t.Fatalf("CommitMerge: unexpected error: %v", err)
	}
	if inProgress, err := f.o.mergeInProgress(ctx, f.wt); err != nil {
		t.Fatalf("mergeInProgress: %v", err)
	} else if inProgress {
		t.Fatalf("mergeInProgress = true after CommitMerge, want false")
	}

	candidates := []string{mergeSharedPath, mergeTicketOnlyPath, "missing.txt"}
	marked, err := f.o.PathsWithConflictMarkers(ctx, f.wt, candidates)
	if err != nil {
		t.Fatalf("PathsWithConflictMarkers: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(marked, want) {
		t.Fatalf("PathsWithConflictMarkers = %v, want %v", marked, want)
	}

	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")
	marked, err = f.o.PathsWithConflictMarkers(ctx, f.wt, candidates)
	if err != nil {
		t.Fatalf("PathsWithConflictMarkers (after rewrite): unexpected error: %v", err)
	}
	if len(marked) != 0 {
		t.Errorf("PathsWithConflictMarkers (after rewrite) = %v, want empty", marked)
	}
}

// -----------------------------------------------------------------------
// StageResolvedPaths
// -----------------------------------------------------------------------

// assertPorcelainPrefix fails t unless "git status --porcelain" in dir has
// a line for path whose first two columns are wantPrefix, the shape
// TestStageResolvedPaths and its siblings each check for one path after
// calling StageResolvedPaths.
func assertPorcelainPrefix(ctx context.Context, t *testing.T, dir, path, wantPrefix string) {
	t.Helper()
	status := runGit(ctx, t, dir, "status", "--porcelain")
	found := false
	for line := range strings.SplitSeq(strings.TrimRight(status, "\n"), "\n") {
		if strings.HasSuffix(line, path) {
			found = true
			if !strings.HasPrefix(line, wantPrefix) {
				t.Errorf("status line for %s = %q, want prefix %q", path, line, wantPrefix)
			}
		}
	}
	if !found {
		t.Errorf("git status --porcelain = %q, wanted a line for %s", status, path)
	}
}

// TestStageResolvedPaths proves StageResolvedPaths' own Staged branch: a
// conflict the merge agent resolved in the working tree, with no markers
// left and no "git add", comes back in Staged and is actually staged, so
// ChangedPaths (which otherwise rejects a "UU" path outright) reads it as
// an ordinary Modified path afterwards.
func TestStageResolvedPaths(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 52)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")

	res, err := f.o.StageResolvedPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("StageResolvedPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(res.Staged, want) {
		t.Errorf("StageResolvedPaths Staged = %v, want %v", res.Staged, want)
	}
	if len(res.Marked) != 0 {
		t.Errorf("StageResolvedPaths Marked = %v, want empty", res.Marked)
	}
	if len(res.Binary) != 0 {
		t.Errorf("StageResolvedPaths Binary = %v, want empty", res.Binary)
	}

	assertPorcelainPrefix(ctx, t, f.wt.Dir(), mergeSharedPath, "M  ")

	if _, err := f.o.ChangedPaths(ctx, f.wt); err != nil {
		t.Errorf("ChangedPaths (after staging): unexpected error: %v", err)
	}
}

// TestStageResolvedPathsStagesRemovedPath proves StageResolvedPaths' Staged
// branch also covers a delete/modify conflict the merge agent settled by
// removing the file: fileLooksBinary and fileHasConflictMarkers both report
// false for an absent path, so it is staged too, and "git add -u" records
// the removal rather than erroring on a missing pathspec target.
func TestStageResolvedPathsStagesRemovedPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 55)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	if err := os.Remove(filepath.Join(f.wt.Dir(), mergeSharedPath)); err != nil {
		t.Fatalf("os.Remove: unexpected error: %v", err)
	}

	res, err := f.o.StageResolvedPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("StageResolvedPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(res.Staged, want) {
		t.Errorf("StageResolvedPaths Staged = %v, want %v", res.Staged, want)
	}
	if len(res.Marked) != 0 {
		t.Errorf("StageResolvedPaths Marked = %v, want empty", res.Marked)
	}
	if len(res.Binary) != 0 {
		t.Errorf("StageResolvedPaths Binary = %v, want empty", res.Binary)
	}

	assertPorcelainPrefix(ctx, t, f.wt.Dir(), mergeSharedPath, "D  ")

	if _, err := f.o.ChangedPaths(ctx, f.wt); err != nil {
		t.Errorf("ChangedPaths (after staging): unexpected error: %v", err)
	}
}

// TestStageResolvedPathsKeepsMarkedPath proves StageResolvedPaths never
// stages a path whose working-tree file still holds a conflict marker
// line: it comes back in Marked, left unstaged, so the index still shows
// "UU" for it.
func TestStageResolvedPathsKeepsMarkedPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 53)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	res, err := f.o.StageResolvedPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("StageResolvedPaths: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(res.Marked, want) {
		t.Errorf("StageResolvedPaths Marked = %v, want %v", res.Marked, want)
	}
	if len(res.Staged) != 0 {
		t.Errorf("StageResolvedPaths Staged = %v, want empty", res.Staged)
	}
	if len(res.Binary) != 0 {
		t.Errorf("StageResolvedPaths Binary = %v, want empty", res.Binary)
	}

	assertPorcelainPrefix(ctx, t, f.wt.Dir(), mergeSharedPath, "UU ")
}

// TestStageResolvedPathsKeepsBinaryConflict proves StageResolvedPaths never
// stages a binary conflict (TestConflictMarkerPathsBinaryConflict's own
// setup): it comes back in Binary, left unstaged, so ConflictMarkerPaths
// still reports it afterwards.
func TestStageResolvedPathsKeepsBinaryConflict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	const binPath = "image.bin"
	writeTestBinaryFile(t, filepath.Join(repo, binPath), []byte{0x00, 0x01, 0x02})
	runGit(ctx, t, repo, "add", binPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "seed image.bin")

	wt, err := o.PrepareWorktree(ctx, 54, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestBinaryFile(t, filepath.Join(wt.Dir(), binPath), []byte{0x00, 0x10, 0x11, 0x12})
	runGit(ctx, t, wt.Dir(), "add", binPath)
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits image.bin")

	writeTestBinaryFile(t, filepath.Join(repo, binPath), []byte{0x00, 0x20, 0x21, 0x22})
	runGit(ctx, t, repo, "add", binPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "main edits image.bin")
	baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	if _, mergeErr := o.StartBaseMerge(ctx, wt, baseSHA); mergeErr != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", mergeErr)
	}

	res, err := o.StageResolvedPaths(ctx, wt)
	if err != nil {
		t.Fatalf("StageResolvedPaths: unexpected error: %v", err)
	}
	if want := []string{binPath}; !slices.Equal(res.Binary, want) {
		t.Errorf("StageResolvedPaths Binary = %v, want %v", res.Binary, want)
	}
	if len(res.Staged) != 0 {
		t.Errorf("StageResolvedPaths Staged = %v, want empty", res.Staged)
	}
	if len(res.Marked) != 0 {
		t.Errorf("StageResolvedPaths Marked = %v, want empty", res.Marked)
	}

	marked, err := o.ConflictMarkerPaths(ctx, wt)
	if err != nil {
		t.Fatalf("ConflictMarkerPaths: unexpected error: %v", err)
	}
	if want := []string{binPath}; !slices.Equal(marked, want) {
		t.Errorf("ConflictMarkerPaths = %v, want %v", marked, want)
	}
}

// -----------------------------------------------------------------------
// MergeSidePaths
// -----------------------------------------------------------------------

func TestMergeSidePaths(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 6)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	got, err := f.o.MergeSidePaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("MergeSidePaths: unexpected error: %v", err)
	}
	want := []string{mergeBaseOnlyPath, mergeSharedPath, mergeTicketOnlyPath}
	if !slices.Equal(got, want) {
		t.Errorf("MergeSidePaths = %v, want %v", got, want)
	}

	// No merge in progress in a fresh worktree.
	fresh := newSigningFixture(t, true)
	freshRepo := newSigningTestRepo(t, fresh)
	freshO := newTestOrchestrator(t, freshRepo, execRunner{})
	freshWT, err := freshO.PrepareWorktree(ctx, 60, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	if _, err := freshO.MergeSidePaths(ctx, freshWT); err == nil {
		t.Fatal("MergeSidePaths: expected an error with no merge in progress, got nil")
	}
}

// -----------------------------------------------------------------------
// CommitMerge: unsigned and no-merge-in-progress
// -----------------------------------------------------------------------

// signedStatusLiesUnsigned wraps a real Runner and makes exactly the
// post-commit signature check ("git show --no-patch --format=%G?" and its
// "git cat-file -p" presence fallback, both run through o.run) report an
// unsigned commit, while every other call -- including the commit itself --
// passes through unchanged. It exists because the real "git add"/"git
// commit -S" calls CommitMerge makes always run through a fresh execRunner
// built at the call site (commit.go), never through o.run, so no Runner
// substitution can make that commit itself fail or go unsigned; this is the
// only seam that can make CommitMerge believe a commit that really was made,
// and really is signed, came back unsigned, so the test below drives the
// reset-and-restore path against a real commit rather than one that never
// happened at all (a commit "git commit -S" refuses outright, as a genuinely
// broken signing key does, never moves HEAD, so it never reaches that path
// either -- see TestCommitMergeUnsignedKeepsMergeInProgress's first case).
type signedStatusLiesUnsigned struct {
	inner Runner
}

func (r signedStatusLiesUnsigned) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Run(ctx, dir, name, args...)
}

// gitShowSubcommand is "git show"'s own subcommand name, named so goconst
// sees one constant instead of a literal repeated across this package's own
// test files, each of which fakes a different git call by matching it.
const gitShowSubcommand = "show"

func (r signedStatusLiesUnsigned) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	isSignatureShow := name == "git" && len(args) > 0 && args[0] == gitShowSubcommand && slices.Contains(args, "--format=%G?")
	if isSignatureShow {
		return "N\n", nil
	}
	isCatFile := name == "git" && len(args) > 1 && args[0] == "cat-file" && args[1] == "-p"
	if isCatFile {
		return "tree deadbeef\nauthor a <a@example.com> 0 +0000\ncommitter a <a@example.com> 0 +0000\n\nno signature header here\n", nil
	}
	return r.inner.Output(ctx, dir, name, args...)
}

func TestCommitMergeUnsignedKeepsMergeInProgress(t *testing.T) {
	t.Parallel()

	t.Run("git commit -S refuses outright, nothing moves", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		repo := newUnsignedTestRepo(t)
		o := newTestOrchestrator(t, repo, execRunner{})

		writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one\n")
		runGit(ctx, t, repo, "add", mergeSharedPath)
		runGit(ctx, t, repo, "commit", "-q", "-m", "seed shared.txt")

		wt, err := o.PrepareWorktree(ctx, 7, "", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one TICKET\n")
		runGit(ctx, t, wt.Dir(), "add", mergeSharedPath)
		runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits shared.txt")
		priorHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

		writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one MAIN\n")
		runGit(ctx, t, repo, "add", mergeSharedPath)
		runGit(ctx, t, repo, "commit", "-q", "-m", "main edits shared.txt")
		baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

		if _, mergeErr := o.StartBaseMerge(ctx, wt, baseSHA); mergeErr != nil {
			t.Fatalf("StartBaseMerge: unexpected error: %v", mergeErr)
		}
		writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one RESOLVED\n")

		_, err = o.CommitMerge(ctx, wt, mergeTestMessage(), baseSHA)
		if err == nil {
			t.Fatal("CommitMerge: expected an error for a genuinely unsigned commit, got nil")
		}
		if !strings.Contains(err.Error(), "commit signing failed") {
			t.Errorf("CommitMerge error = %q, want it to mention %q", err.Error(), "commit signing failed")
		}

		afterHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
		if afterHead != priorHead {
			t.Errorf("HEAD = %q after a failed CommitMerge, want it reset back to %q", afterHead, priorHead)
		}

		assertMergeHeadFile(ctx, t, wt.Dir(), baseSHA)
	})

	t.Run("a real signed commit the check misreports is reset and MERGE_HEAD restored", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newMergeConflictFixture(t, 71)

		if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
			t.Fatalf("StartBaseMerge: unexpected error: %v", err)
		}
		writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")
		priorHead := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "HEAD"))

		f.o.run = signedStatusLiesUnsigned{inner: f.o.run}
		_, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage(), f.baseSHA)
		if err == nil {
			t.Fatal("CommitMerge: expected an error when the signature check reports unsigned, got nil")
		}
		if !strings.Contains(err.Error(), "commit signing failed") {
			t.Errorf("CommitMerge error = %q, want it to mention %q", err.Error(), "commit signing failed")
		}

		afterHead := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "HEAD"))
		if afterHead != priorHead {
			t.Errorf("HEAD = %q after the reset, want it back to %q", afterHead, priorHead)
		}
		assertMergeHeadFile(ctx, t, f.wt.Dir(), f.baseSHA)

		// With the signature check reading real git output again, CommitMerge
		// from the restored state succeeds.
		f.o.run = execRunner{}
		sha, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage(), f.baseSHA)
		if err != nil {
			t.Fatalf("CommitMerge (retry): unexpected error: %v", err)
		}
		signed, err := f.o.SignedStatus(ctx, f.wt, sha)
		if err != nil {
			t.Fatalf("SignedStatus: %v", err)
		}
		if !signed {
			t.Error("SignedStatus = false after the retry, want true")
		}
	})
}

func TestCommitMergeWrongMergeHeadRefuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 72)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")
	priorHead := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "HEAD"))

	wrongSHA := strings.Repeat("b", 40)
	_, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage(), wrongSHA)
	if err == nil {
		t.Fatal("CommitMerge: expected an error for a MERGE_HEAD mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "MERGE_HEAD") {
		t.Errorf("CommitMerge error = %q, want it to mention MERGE_HEAD", err.Error())
	}

	afterHead := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "HEAD"))
	if afterHead != priorHead {
		t.Errorf("HEAD = %q after a refused CommitMerge, want unchanged %q", afterHead, priorHead)
	}
	mergeHeadNow := strings.TrimSpace(runGit(ctx, t, f.wt.Dir(), "rev-parse", "MERGE_HEAD"))
	if mergeHeadNow != f.baseSHA {
		t.Errorf("MERGE_HEAD = %q after a refused CommitMerge, want unchanged %q", mergeHeadNow, f.baseSHA)
	}
}

func TestCommitMergeRefusesWithoutMerge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, 8, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	priorHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	_, err = o.CommitMerge(ctx, wt, mergeTestMessage(), strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("CommitMerge: expected an error with no merge in progress, got nil")
	}
	const want = "orchestrator: commit merge: no merge in progress"
	if err.Error() != want {
		t.Errorf("CommitMerge error = %q, want %q", err.Error(), want)
	}

	afterHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
	if afterHead != priorHead {
		t.Errorf("HEAD = %q after a refused CommitMerge, want unchanged %q", afterHead, priorHead)
	}
}

// TestCommitMergeStagesSidePathDeletions proves CommitMerge's own two-step
// staging survives a merge-side path a resolved merge removes entirely:
// one file the ticket branch deletes and main leaves untouched, and
// another main deletes that the ticket branch never touched. Git resolves
// both automatically -- gone from the index and the working tree before
// CommitMerge ever runs -- so naming either one in an explicit "git add
// -A" pathspec would make git refuse with "pathspec '<p>' did not match
// any files" if CommitMerge did not filter them out first
// (mergeSidePathsPresent).
func TestCommitMergeStagesSidePathDeletions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const (
		ticketDeletesPath = "ticket-deletes-this.txt"
		mainDeletesPath   = "main-deletes-this.txt"
	)

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one\n")
	writeTestFile(t, filepath.Join(repo, ticketDeletesPath), "present at the common ancestor\n")
	writeTestFile(t, filepath.Join(repo, mainDeletesPath), "present at the common ancestor\n")
	runGit(ctx, t, repo, "add", mergeSharedPath, ticketDeletesPath, mainDeletesPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "seed")

	wt, err := o.PrepareWorktree(ctx, 73, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one TICKET\n")
	runGit(ctx, t, wt.Dir(), "rm", "-q", ticketDeletesPath)
	runGit(ctx, t, wt.Dir(), "add", mergeSharedPath)
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits shared.txt and deletes its own file")
	ticketHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one MAIN\n")
	runGit(ctx, t, repo, "rm", "-q", mainDeletesPath)
	runGit(ctx, t, repo, "add", mergeSharedPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "main edits shared.txt and deletes its own file")
	baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	conflicted, err := o.StartBaseMerge(ctx, wt, baseSHA)
	if err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(conflicted, want) {
		t.Fatalf("StartBaseMerge conflicted = %v, want %v", conflicted, want)
	}
	for _, p := range []string{ticketDeletesPath, mainDeletesPath} {
		if _, statErr := os.Lstat(filepath.Join(wt.Dir(), p)); !os.IsNotExist(statErr) {
			t.Fatalf("%s: want it already resolved away by git merge, Lstat error = %v", p, statErr)
		}
	}

	writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one RESOLVED\n")

	sha, err := o.CommitMerge(ctx, wt, mergeTestMessage(), baseSHA)
	if err != nil {
		t.Fatalf("CommitMerge: unexpected error: %v", err)
	}

	parents, err := o.CommitParents(ctx, wt, sha)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
	if want := []string{ticketHead, baseSHA}; !slices.Equal(parents, want) {
		t.Errorf("CommitParents = %v, want %v", parents, want)
	}

	tree := runGit(ctx, t, wt.Dir(), "ls-tree", "-r", "--name-only", sha)
	for _, p := range []string{ticketDeletesPath, mainDeletesPath} {
		if strings.Contains(tree, p) {
			t.Errorf("ls-tree %s = %q, want it to omit the deleted %s", sha, tree, p)
		}
	}
	if !strings.Contains(tree, mergeSharedPath) {
		t.Errorf("ls-tree %s = %q, want it to include %s", sha, tree, mergeSharedPath)
	}

	status := runGit(ctx, t, wt.Dir(), "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after CommitMerge: %q", status)
	}
}

// TestCommitMergeStagesPathMadeUntracked proves CommitMerge's own explicit
// "git add -A" pass (mergeSidePathsPresent): once the agent runs
// "git rm --cached" on a merge-side path -- still present on disk, no
// longer tracked -- "git add -u" alone would leave it out of the commit
// entirely, since -u only restages already-tracked changes.
func TestCommitMergeStagesPathMadeUntracked(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 74)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")
	runGit(ctx, t, f.wt.Dir(), "rm", "-q", "--cached", mergeBaseOnlyPath)

	sha, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage(), f.baseSHA)
	if err != nil {
		t.Fatalf("CommitMerge: unexpected error: %v", err)
	}

	tree := runGit(ctx, t, f.wt.Dir(), "ls-tree", "-r", "--name-only", sha)
	if !strings.Contains(tree, mergeBaseOnlyPath) {
		t.Errorf("ls-tree %s = %q, want it to include %s even though it was rm --cached before the commit", sha, tree, mergeBaseOnlyPath)
	}

	status := runGit(ctx, t, f.wt.Dir(), "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after CommitMerge: %q", status)
	}
}

// TestCommitMergeDirectoryReplacedByFile proves mergeSidePathsPresent
// treats a merge-side path as absent when its own directory component is
// resolved away into a plain file: Lstat on such a path fails with
// ENOTDIR, which os.IsNotExist does not recognize, and CommitMerge must
// not treat that as a real error (review thread t17445ba115e42a7c).
func TestCommitMergeDirectoryReplacedByFile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const nestedPath = "foo/bar.txt"

	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	o := newTestOrchestrator(t, repo, execRunner{})

	writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one\n")
	runGit(ctx, t, repo, "add", mergeSharedPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "seed")

	wt, err := o.PrepareWorktree(ctx, 76, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one TICKET\n")
	runGit(ctx, t, wt.Dir(), "add", mergeSharedPath)
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "ticket edits shared.txt")
	ticketHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	writeTestFile(t, filepath.Join(repo, mergeSharedPath), "line one MAIN\n")
	writeTestFile(t, filepath.Join(repo, nestedPath), "nested\n")
	runGit(ctx, t, repo, "add", mergeSharedPath, nestedPath)
	runGit(ctx, t, repo, "commit", "-q", "-m", "main edits shared.txt and adds foo/bar.txt")
	baseSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	conflicted, err := o.StartBaseMerge(ctx, wt, baseSHA)
	if err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}
	if want := []string{mergeSharedPath}; !slices.Equal(conflicted, want) {
		t.Fatalf("StartBaseMerge conflicted = %v, want %v", conflicted, want)
	}

	// Resolve shared.txt, then replace the whole "foo" directory the base
	// side added with a plain file of the same name: foo/bar.txt (a
	// MergeSidePaths entry) is now an impossible path.
	writeTestFile(t, filepath.Join(wt.Dir(), mergeSharedPath), "line one RESOLVED\n")
	runGit(ctx, t, wt.Dir(), "rm", "-q", "-r", "-f", "foo")
	writeTestFile(t, filepath.Join(wt.Dir(), "foo"), "foo is now a file\n")
	runGit(ctx, t, wt.Dir(), "add", "foo")

	sha, err := o.CommitMerge(ctx, wt, mergeTestMessage(), baseSHA)
	if err != nil {
		t.Fatalf("CommitMerge: unexpected error: %v", err)
	}

	parents, err := o.CommitParents(ctx, wt, sha)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
	if want := []string{ticketHead, baseSHA}; !slices.Equal(parents, want) {
		t.Errorf("CommitParents = %v, want %v", parents, want)
	}

	status := runGit(ctx, t, wt.Dir(), "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after CommitMerge: %q", status)
	}
}

// -----------------------------------------------------------------------
// MergeChangedPaths
// -----------------------------------------------------------------------

// TestMergeChangedPaths proves MergeChangedPaths over a real, unresolved
// conflict (perimeter.go): the index still carries mergeSharedPath
// unmerged, mergeBaseOnlyPath is staged clean from the base side, and an
// untracked file in a subdirectory the merge never touched should still be
// reported, since MergeChangedPaths lists the working tree's own changes,
// not the merge's own side set. mergeTicketOnlyPath, already part of HEAD
// before the merge and untouched by the base side, carries no status at
// all and so is absent from the result.
func TestMergeChangedPaths(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newMergeConflictFixture(t, 9)

	if _, err := f.o.StartBaseMerge(ctx, f.wt, f.baseSHA); err != nil {
		t.Fatalf("StartBaseMerge: unexpected error: %v", err)
	}

	const untrackedPath = "scratch/untracked.txt"
	if err := os.MkdirAll(filepath.Join(f.wt.Dir(), "scratch"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestFile(t, filepath.Join(f.wt.Dir(), untrackedPath), "not part of the merge\n")

	got, err := f.o.MergeChangedPaths(ctx, f.wt)
	if err != nil {
		t.Fatalf("MergeChangedPaths: unexpected error: %v", err)
	}
	want := []string{mergeBaseOnlyPath, untrackedPath, mergeSharedPath}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("MergeChangedPaths = %v, want %v", got, want)
	}

	// No merge in progress in a fresh worktree.
	fresh := newSigningFixture(t, true)
	freshRepo := newSigningTestRepo(t, fresh)
	freshO := newTestOrchestrator(t, freshRepo, execRunner{})
	freshWT, err := freshO.PrepareWorktree(ctx, 61, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	_, err = freshO.MergeChangedPaths(ctx, freshWT)
	if err == nil {
		t.Fatal("MergeChangedPaths: expected an error with no merge in progress, got nil")
	}
	const wantErr = "orchestrator: merge changed paths: no merge in progress"
	if err.Error() != wantErr {
		t.Errorf("MergeChangedPaths error = %q, want %q", err.Error(), wantErr)
	}
}

// -----------------------------------------------------------------------
// test helpers
// -----------------------------------------------------------------------

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is under a test's own temp worktree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
