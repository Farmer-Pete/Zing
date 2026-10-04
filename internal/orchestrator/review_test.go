package orchestrator

// review_test.go tests task 8's git reads (design section 10.1): HeadSHA,
// Diff, ChangedFilesBetween, ChangedFilesSinceBase, and IsAncestor, against
// real git in a temp dir. Repository and worktree setup reuses
// perimeter_test.go's preparePerimeterWorktree (newTestRepo,
// newTestOrchestrator, PrepareWorktree), the pattern every git-backed test
// in this package already follows; every additional git command this file
// itself needs to run -- staging and committing a file, reading a new
// commit's sha -- goes through gitfixture.Git rather than a second local
// exec.Command helper.

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"zing/internal/gitfixture"
)

// repoRootFor returns the main checkout directory a PrepareWorktree-created
// Worktree belongs to: wt.Dir() is "<repo>/.zing/wt/<ticketID>", so three
// directories up is "<repo>" (perimeter_test.go's TestHunkIgnoresTextconv
// uses the same climb to reach the shared repository).
func repoRootFor(wt Worktree) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(wt.Dir())))
}

// commitFile writes relPath under dir with content and commits it (through
// gitfixture.Git, unsigned: newTestRepo already set commit.gpgsign=false at
// the repo level), returning the new commit's sha.
func commitFile(ctx context.Context, t *testing.T, dir, relPath, content, message string) string {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, relPath), content)
	if out, err := gitfixture.Git(ctx, dir, "add", relPath); err != nil {
		t.Fatalf("git add %s: %v: %s", relPath, err, out)
	}
	if out, err := gitfixture.Git(ctx, dir, "commit", "-q", "-m", message); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	out, err := gitfixture.Git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// -----------------------------------------------------------------------
// HeadSHA
// -----------------------------------------------------------------------

func TestHeadSHA(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 901)

	want := commitFile(ctx, t, wt.Dir(), "head.txt", "content\n", "add head.txt")

	got, err := o.HeadSHA(ctx, wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if got != want {
		t.Errorf("HeadSHA = %q, want %q", got, want)
	}
}

// -----------------------------------------------------------------------
// Diff
// -----------------------------------------------------------------------

// TestDiffAgainstMergeBase proves Diff compares sha against its merge base
// with the default branch, not against the default branch's current tip: a
// commit made to main after the ticket branch diverged must not appear in
// the result, even though it is main's current HEAD.
func TestDiffAgainstMergeBase(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 902)
	root := repoRootFor(wt)

	commitFile(ctx, t, root, "README.md", "# main moved on\n", "advance main independently")
	sha := commitFile(ctx, t, wt.Dir(), "feature.go", "package feature\n", "add feature.go")

	diff, err := o.Diff(ctx, wt, sha)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "diff --git a/feature.go b/feature.go") {
		t.Errorf("Diff missing feature.go:\n%s", diff)
	}
	if strings.Contains(diff, "README.md") {
		t.Errorf("Diff mentions README.md, main's own later change outside the merge-base range:\n%s", diff)
	}
}

// TestDiffRefreshesBase proves Diff fetches the base itself (right after
// revalidate, not buried inside it): a commit that lands on origin's main
// after the ticket branch was cut still becomes the new merge-base for a
// later Diff call, while the diff itself still names only the ticket's own
// file and the branch's own HEAD never moves.
func TestDiffRefreshesBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, 733, "diff", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	ticketSHA := commitFile(ctx, t, wt.Dir(), "ticket.txt", "ticket\n", "add ticket.txt")

	newSHA := cloneAndCommitUpstream(ctx, t, remote, "upstream.txt", "upstream\n", "add upstream.txt")

	diff, err := o.Diff(ctx, wt, ticketSHA)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	gotBase := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main"))
	if gotBase != newSHA {
		t.Errorf("refs/zing/base/main = %s, want %s", gotBase, newSHA)
	}

	if !strings.Contains(diff, "diff --git a/ticket.txt b/ticket.txt") {
		t.Errorf("Diff missing ticket.txt:\n%s", diff)
	}
	if strings.Contains(diff, "upstream.txt") {
		t.Errorf("Diff mentions upstream.txt, which should not be in the ticket's own diff:\n%s", diff)
	}

	head := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
	if head != ticketSHA {
		t.Errorf("branch HEAD moved from %s to %s", ticketSHA, head)
	}

	fetched := findRecords(logs.records(t), "fetched base")
	if len(fetched) == 0 {
		t.Fatal("found no \"fetched base\" records")
	}
	last := fetched[len(fetched)-1]
	if got, want := last["ticket_id"], float64(733); got != want {
		t.Errorf("ticket_id = %v, want %v", got, want)
	}
	if last["sha"] != newSHA {
		t.Errorf("sha = %v, want %s", last["sha"], newSHA)
	}
}

// TestDiffIgnoresTextconv proves Diff's --no-textconv flag, mirroring
// perimeter_test.go's TestHunkIgnoresTextconv: a "diff.<driver>.textconv"
// configured for the changed path's extension, with a command that writes
// a marker file, leaves no marker after Diff.
func TestDiffIgnoresTextconv(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 903)
	root := repoRootFor(wt)

	marker := filepath.Join(t.TempDir(), "marker")
	configureMarkerTextconv(ctx, t, root, "x", marker)
	writeTestFile(t, filepath.Join(wt.Dir(), ".gitattributes"), "*.md diff=x\n")

	sha := commitFile(ctx, t, wt.Dir(), "notes.md", "# notes\n", "add notes.md")

	diff, err := o.Diff(ctx, wt, sha)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == "" {
		t.Error("Diff: expected a non-empty diff")
	}
	assertMarkerAbsent(t, marker)
}

// -----------------------------------------------------------------------
// ChangedFilesBetween, ChangedFilesSinceBase
// -----------------------------------------------------------------------

func TestChangedFilesBetween(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 904)

	sha0, err := o.HeadSHA(ctx, wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	commitFile(ctx, t, wt.Dir(), bGoPath, "package b\n", "add b.go")
	sha1 := commitFile(ctx, t, wt.Dir(), aGoPath, "package a\n", "add a.go")

	got, err := o.ChangedFilesBetween(ctx, wt, sha0, sha1)
	if err != nil {
		t.Fatalf("ChangedFilesBetween: %v", err)
	}
	want := []string{aGoPath, bGoPath}
	if !slices.Equal(got, want) {
		t.Errorf("ChangedFilesBetween = %v, want %v (sorted)", got, want)
	}
}

// TestChangedFilesSinceBase proves it is ChangedFilesBetween(merge-base,
// to): main's own later, independent change must not appear, only the
// ticket branch's own changed files, sorted.
func TestChangedFilesSinceBase(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 905)
	root := repoRootFor(wt)

	commitFile(ctx, t, root, "README.md", "# main moved on\n", "advance main independently")

	commitFile(ctx, t, wt.Dir(), bGoPath, "package b\n", "add b.go")
	sha := commitFile(ctx, t, wt.Dir(), aGoPath, "package a\n", "add a.go")

	got, err := o.ChangedFilesSinceBase(ctx, wt, sha)
	if err != nil {
		t.Fatalf("ChangedFilesSinceBase: %v", err)
	}
	want := []string{aGoPath, bGoPath}
	if !slices.Equal(got, want) {
		t.Errorf("ChangedFilesSinceBase = %v, want %v (sorted, no README.md)", got, want)
	}
}

// -----------------------------------------------------------------------
// IsAncestor
// -----------------------------------------------------------------------

func TestIsAncestor(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 906)
	root := repoRootFor(wt)

	base, err := o.HeadSHA(ctx, wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	mainOnly := commitFile(ctx, t, root, "README.md", "# main moved on\n", "advance main independently")
	ticketSHA := commitFile(ctx, t, wt.Dir(), "feature.go", "package feature\n", "add feature.go")

	t.Run("the branch point is an ancestor of the ticket branch", func(t *testing.T) {
		t.Parallel()
		ok, err := o.IsAncestor(ctx, wt, base, ticketSHA)
		if err != nil {
			t.Fatalf("IsAncestor: %v", err)
		}
		if !ok {
			t.Error("IsAncestor(base, ticketSHA) = false, want true")
		}
	})

	t.Run("a divergent main-only commit is not an ancestor of the ticket branch", func(t *testing.T) {
		t.Parallel()
		ok, err := o.IsAncestor(ctx, wt, mainOnly, ticketSHA)
		if err != nil {
			t.Fatalf("IsAncestor: %v", err)
		}
		if ok {
			t.Error("IsAncestor(mainOnly, ticketSHA) = true, want false")
		}
	})
}
