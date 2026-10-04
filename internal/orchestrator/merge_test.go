package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
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
	if want := []string{mergeSharedPath}; !sliceEqual(conflicted, want) {
		t.Fatalf("StartBaseMerge conflicted = %v, want %v", conflicted, want)
	}

	writeTestFile(t, filepath.Join(f.wt.Dir(), mergeSharedPath), "line one RESOLVED\n")

	sha, err := f.o.CommitMerge(ctx, f.wt, mergeTestMessage())
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
	if want := []string{f.ticketHead, f.baseSHA}; !sliceEqual(parents, want) {
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
	if !sliceEqual(first, second) {
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
	if want := []string{mergeSharedPath}; !sliceEqual(marked, want) {
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
	if !sliceEqual(got, want) {
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

func TestCommitMergeUnsignedKeepsMergeInProgress(t *testing.T) {
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

	_, err = o.CommitMerge(ctx, wt, mergeTestMessage())
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

	mergeHeadNow := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "MERGE_HEAD"))
	if mergeHeadNow != baseSHA {
		t.Errorf("MERGE_HEAD = %q after a failed CommitMerge, want it restored to %q", mergeHeadNow, baseSHA)
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

	_, err = o.CommitMerge(ctx, wt, mergeTestMessage())
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

// -----------------------------------------------------------------------
// test helpers
// -----------------------------------------------------------------------

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is under a test's own temp worktree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
