package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// Path literals reused across this file's cases, pulled out as constants
// (matching worktree_test.go's testOwner/testRepo/mainBranch convention) so
// goconst does not flag the repeats.
const (
	readmePath      = "README.md"
	claudeMDPath    = "CLAUDE.md"
	perimeterGoPath = "internal/orchestrator/perimeter.go"
	scratchGoPath   = "internal/orchestrator/scratch.go"
	machineTomlPath = "machine.toml"
	sandboxGlob     = "sandbox/**"
	sandboxDeepPath = "sandbox/deep/file.go"
	sharedMDPath    = "shared.md"
	aGoPath         = "a.go"
	bGoPath         = "b.go"
	starMDGlob      = "*.md"
	docsExtraPath   = "docs/extra.md"
	newFileGoPath   = "app/newfile.go"
)

// wantExtras fails the test unless got equals want exactly, element by
// element. perimeter.go is not yet on the github.com/google/go-cmp
// dependency this repo's golang skill otherwise prefers for struct
// comparison in tests, so this stays a plain slice-equality helper rather
// than adding that module.
func wantExtras(t *testing.T, got, want []Extra) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d extras %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extra %d = %+v, want %+v (full got %+v, want %+v)", i, got[i], want[i], got, want)
		}
	}
}

// -----------------------------------------------------------------------
// Pure: Perimeter
// -----------------------------------------------------------------------

func TestPerimeter(t *testing.T) {
	t.Parallel()
	t.Run("worked example 12.2", func(t *testing.T) {
		t.Parallel()
		changed := []Change{
			{Path: perimeterGoPath, Code: Modified},
			{Path: scratchGoPath, Code: Untracked},
			{Path: claudeMDPath, Code: Modified},
		}
		declared := []string{perimeterGoPath}
		trustRoot := []string{
			machineTomlPath, "prompts/**", "internal/store/schemas/**",
			sandboxGlob, ".github/workflows/**", "internal/store/migrations/**", "FACTORY.md",
		}
		styleGuide := []string{claudeMDPath, "AGENTS.md"}

		got := Perimeter(changed, declared, trustRoot, styleGuide)
		want := []Extra{
			{Path: claudeMDPath, Marker: markerStyleGuide},
			{Path: scratchGoPath, Marker: ""},
		}
		wantExtras(t, got, want)
	})

	t.Run("a declared path is omitted by exact match, an undeclared path is an extra", func(t *testing.T) {
		t.Parallel()
		changed := []Change{
			{Path: aGoPath, Code: Modified},
			{Path: bGoPath, Code: Added},
		}
		got := Perimeter(changed, []string{aGoPath}, nil, nil)
		want := []Extra{{Path: bGoPath, Marker: ""}}
		wantExtras(t, got, want)
	})

	t.Run("declared match is exact path, not a glob", func(t *testing.T) {
		t.Parallel()
		// "internal/**" as a declared entry is not a pattern: it must match
		// a changed path byte for byte to be omitted.
		changed := []Change{{Path: "internal/orchestrator/x.go", Code: Modified}}
		got := Perimeter(changed, []string{"internal/**"}, nil, nil)
		want := []Extra{{Path: "internal/orchestrator/x.go", Marker: ""}}
		wantExtras(t, got, want)
	})

	t.Run("a path matching both trust-root and style-guide is marked trust root", func(t *testing.T) {
		t.Parallel()
		changed := []Change{{Path: sharedMDPath, Code: Modified}}
		got := Perimeter(changed, nil, []string{sharedMDPath}, []string{sharedMDPath})
		want := []Extra{{Path: sharedMDPath, Marker: markerTrustRoot}}
		wantExtras(t, got, want)
	})

	t.Run("a trust-root glob marks an extra", func(t *testing.T) {
		t.Parallel()
		changed := []Change{{Path: sandboxDeepPath, Code: Untracked}}
		got := Perimeter(changed, nil, []string{sandboxGlob}, nil)
		want := []Extra{{Path: sandboxDeepPath, Marker: markerTrustRoot}}
		wantExtras(t, got, want)
	})

	t.Run("result is sorted by path regardless of input order", func(t *testing.T) {
		t.Parallel()
		changed := []Change{
			{Path: "z.go", Code: Modified},
			{Path: aGoPath, Code: Modified},
			{Path: "m.go", Code: Modified},
		}
		got := Perimeter(changed, nil, nil, nil)
		if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Path < got[j].Path }) {
			t.Errorf("Perimeter() result not sorted by path: %+v", got)
		}
	})

	t.Run("no changes gives no extras", func(t *testing.T) {
		t.Parallel()
		got := Perimeter(nil, []string{aGoPath}, nil, nil)
		if len(got) != 0 {
			t.Errorf("Perimeter() = %+v, want empty", got)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: matchPattern
// -----------------------------------------------------------------------

func TestMatchPattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"no star: exact match", machineTomlPath, machineTomlPath, true},
		{"no star: mismatch is literal, not a prefix", machineTomlPath, "machine.toml.bak", false},
		{"prefix/** matches the bare prefix", sandboxGlob, "sandbox", true},
		{"prefix/** matches a nested path", sandboxGlob, sandboxDeepPath, true},
		{"prefix/** does not match a sibling with the same prefix text", sandboxGlob, "sandboxes/file.go", false},
		{"single * matches within one path segment", starMDGlob, claudeMDPath, true},
		{"single * does not cross a slash", starMDGlob, "docs/CLAUDE.md", false},
		{"path.Match: no match", starMDGlob, "CLAUDE.txt", false},
		{"path.Match: mid-segment star", "internal/store/schemas/*.json", "internal/store/schemas/ticket.json", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := matchPattern(c.pattern, c.path); got != c.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: statusFromXY
// -----------------------------------------------------------------------

// TestStatusFromXY covers the two PR review fixes to statusFromXY (finding
// M): "T" (typechange, such as a file swapped for a symlink) must map to
// Modified, and a code shorter than two characters must be a parse error
// rather than a panic from indexing xy[0].
func TestStatusFromXY(t *testing.T) {
	t.Parallel()
	t.Run("T (typechange) maps to Modified", func(t *testing.T) {
		t.Parallel()
		got, err := statusFromXY(" T")
		if err != nil {
			t.Fatalf("statusFromXY(\" T\"): unexpected error: %v", err)
		}
		if got != Modified {
			t.Errorf("statusFromXY(\" T\") = %v, want Modified", got)
		}
	})

	t.Run("empty input is a parse error, not a panic", func(t *testing.T) {
		t.Parallel()
		if _, err := statusFromXY(""); err == nil {
			t.Fatal("statusFromXY(\"\"): expected an error, got nil")
		}
	})

	t.Run("a single-character code is a parse error, not a panic", func(t *testing.T) {
		t.Parallel()
		if _, err := statusFromXY("A"); err == nil {
			t.Fatal("statusFromXY(\"A\"): expected an error, got nil")
		}
	})
}

// -----------------------------------------------------------------------
// Pure: CountWord
// -----------------------------------------------------------------------

// TestCountWord proves CountWord's own small-number spelling (design
// section 6.5's ASK message and PerimeterNotice both use it): one through
// nine spell out, and anything past nine falls back to its digits.
func TestCountWord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		want string
	}{
		{1, "one"},
		{2, "two"},
		{3, "three"},
		{4, "four"},
		{5, "five"},
		{6, "six"},
		{7, "seven"},
		{8, "eight"},
		{9, "nine"},
		{10, "10"},
		{42, "42"},
	}
	for _, tc := range cases {
		if got := CountWord(tc.n); got != tc.want {
			t.Errorf("CountWord(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------
// Pure: PerimeterNotice
// -----------------------------------------------------------------------

func TestPerimeterNotice(t *testing.T) {
	t.Parallel()
	t.Run("worked example 12.3 content", func(t *testing.T) {
		t.Parallel()
		reverted := []Extra{
			{Path: claudeMDPath, Marker: markerStyleGuide},
			{Path: scratchGoPath, Marker: ""},
		}
		got := PerimeterNotice(reverted)

		want := "You changed two files outside the set the plan declared. " +
			"They were reverted, so your task is not committed yet.\n\n" +
			"- CLAUDE.md (style guide)\n" +
			"- internal/orchestrator/scratch.go\n\n" +
			"Repair any declared file that depended on these reverts so the task builds " +
			"within its declared files. If you need one of these files in the plan, return a question " +
			"to the owner and say which file and why.\n"

		if got != want {
			t.Errorf("PerimeterNotice() =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("names each path and its marker", func(t *testing.T) {
		t.Parallel()
		reverted := []Extra{
			{Path: aGoPath, Marker: markerTrustRoot},
			{Path: "b.md", Marker: markerStyleGuide},
			{Path: "c.go", Marker: ""},
		}
		got := PerimeterNotice(reverted)
		for _, want := range []string{aGoPath, markerTrustRoot, "b.md", markerStyleGuide, "c.go"} {
			if !strings.Contains(got, want) {
				t.Errorf("PerimeterNotice() missing %q, got:\n%s", want, got)
			}
		}
	})

	t.Run("gives both the repair option and the question option", func(t *testing.T) {
		t.Parallel()
		got := PerimeterNotice([]Extra{{Path: aGoPath}})
		if !strings.Contains(got, "Repair") {
			t.Errorf("PerimeterNotice() missing the repair option, got:\n%s", got)
		}
		if !strings.Contains(got, "question") {
			t.Errorf("PerimeterNotice() missing the question option, got:\n%s", got)
		}
	})

	t.Run("says reverted, so the reader knows the change is gone", func(t *testing.T) {
		t.Parallel()
		got := PerimeterNotice([]Extra{{Path: aGoPath}})
		if !strings.Contains(got, "reverted") {
			t.Errorf("PerimeterNotice() does not say the change was reverted, got:\n%s", got)
		}
	})
}

// -----------------------------------------------------------------------
// Real-git integration: ChangedPaths, RevertPaths
// -----------------------------------------------------------------------

// preparePerimeterWorktree builds a fresh temp repo and a ticket worktree in
// it, and returns the context used to build it. It deliberately calls
// newTestRepo(t) before creating that context (mirroring every test in
// worktree_test.go): a t.Helper with its own ctx parameter, calling a helper
// that mints its own context internally, is what contextcheck flags as a
// non-inherited context, so no ctx exists yet at the point newTestRepo runs.
func preparePerimeterWorktree(t *testing.T, ticketID int64) (*Orchestrator, Worktree, context.Context) {
	t.Helper()
	repo := newTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})
	wt, err := o.PrepareWorktree(ctx, ticketID, "perimeter", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	return o, wt, ctx
}

func changeByPath(t *testing.T, changes []Change, p string) Change {
	t.Helper()
	for _, c := range changes {
		if c.Path == p {
			return c
		}
	}
	t.Fatalf("no Change for path %q in %+v", p, changes)
	return Change{}
}

func TestChangedPaths(t *testing.T) {
	t.Parallel()
	t.Run("sees a modified, an added, an untracked, and a deleted path", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 101)

		// Modified: an existing tracked file, edited but not staged.
		writeTestFile(t, filepath.Join(wt.Dir(), readmePath), "# changed\n")

		// Added: a new file, staged.
		writeTestFile(t, filepath.Join(wt.Dir(), "app", "newfile.go"), "package app\n")
		runGit(ctx, t, wt.Dir(), "add", newFileGoPath)

		// Untracked: a new file, never staged.
		writeTestFile(t, filepath.Join(wt.Dir(), "scratch.txt"), "scratch\n")

		// Deleted: an existing tracked file, removed but not staged.
		if err := os.Remove(filepath.Join(wt.Dir(), "docs", "extra.md")); err != nil {
			t.Fatalf("remove docs/extra.md: %v", err)
		}

		changes, err := o.ChangedPaths(ctx, wt)
		if err != nil {
			t.Fatalf("ChangedPaths: %v", err)
		}

		if got := changeByPath(t, changes, readmePath).Code; got != Modified {
			t.Errorf("README.md code = %v, want Modified", got)
		}
		if got := changeByPath(t, changes, newFileGoPath).Code; got != Added {
			t.Errorf("app/newfile.go code = %v, want Added", got)
		}
		if got := changeByPath(t, changes, "scratch.txt").Code; got != Untracked {
			t.Errorf("scratch.txt code = %v, want Untracked", got)
		}
		if got := changeByPath(t, changes, docsExtraPath).Code; got != Deleted {
			t.Errorf("docs/extra.md code = %v, want Deleted", got)
		}

		if !sort.SliceIsSorted(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path }) {
			t.Errorf("ChangedPaths() result not sorted by path: %+v", changes)
		}
	})

	t.Run("a rename is reported as a deleted old path and an added new path", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 102)

		runGit(ctx, t, wt.Dir(), "mv", readmePath, "README2.md")

		changes, err := o.ChangedPaths(ctx, wt)
		if err != nil {
			t.Fatalf("ChangedPaths: %v", err)
		}

		if got := changeByPath(t, changes, readmePath).Code; got != Deleted {
			t.Errorf("README.md code = %v, want Deleted", got)
		}
		if got := changeByPath(t, changes, "README2.md").Code; got != Added {
			t.Errorf("README2.md code = %v, want Added", got)
		}
	})

	t.Run("a conflict errors, naming the path", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 103)

		// Diverge app/main.go on a second branch built off the same base,
		// then merge it into the ticket branch to produce a real conflict.
		runGit(ctx, t, wt.Dir(), "checkout", "-b", "conflict-source", mainBranch)
		writeTestFile(t, filepath.Join(wt.Dir(), "app", "main.go"), "package main\n\n// branch B\n")
		runGit(ctx, t, wt.Dir(), "commit", "-a", "-q", "-m", "branch B change")

		runGit(ctx, t, wt.Dir(), "checkout", wt.Branch())
		writeTestFile(t, filepath.Join(wt.Dir(), "app", "main.go"), "package main\n\n// branch A\n")
		runGit(ctx, t, wt.Dir(), "commit", "-a", "-q", "-m", "branch A change")

		if out, err := (execRunner{}).Run(ctx, wt.Dir(), "git", "merge", "conflict-source"); err == nil {
			t.Fatalf("expected the merge to conflict, it succeeded: %s", out)
		}

		_, err := o.ChangedPaths(ctx, wt)
		if err == nil {
			t.Fatal("ChangedPaths: expected an error for an unmerged path, got nil")
		}
		if !strings.Contains(err.Error(), "app/main.go") {
			t.Errorf("error %q does not name the conflicting path", err.Error())
		}
	})
}

func TestRevertPaths(t *testing.T) {
	t.Parallel()
	t.Run("undoes a modified file", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 201)

		original, err := os.ReadFile(filepath.Join(wt.Dir(), readmePath))
		if err != nil {
			t.Fatalf("read original README.md: %v", err)
		}
		writeTestFile(t, filepath.Join(wt.Dir(), readmePath), "# changed\n")

		if revertErr := o.RevertPaths(ctx, wt, []Change{{Path: readmePath, Code: Modified}}); revertErr != nil {
			t.Fatalf("RevertPaths: %v", revertErr)
		}

		got, err := os.ReadFile(filepath.Join(wt.Dir(), readmePath))
		if err != nil {
			t.Fatalf("read reverted README.md: %v", err)
		}
		if !bytes.Equal(got, original) {
			t.Errorf("README.md content = %q, want the original %q", got, original)
		}

		changes, err := o.ChangedPaths(ctx, wt)
		if err != nil {
			t.Fatalf("ChangedPaths: %v", err)
		}
		for _, c := range changes {
			if c.Path == readmePath {
				t.Errorf("README.md is still reported changed: %+v", c)
			}
		}
	})

	t.Run("undoes a staged-new file", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 202)

		writeTestFile(t, filepath.Join(wt.Dir(), "app", "newfile.go"), "package app\n")
		runGit(ctx, t, wt.Dir(), "add", newFileGoPath)

		if err := o.RevertPaths(ctx, wt, []Change{{Path: newFileGoPath, Code: Added}}); err != nil {
			t.Fatalf("RevertPaths: %v", err)
		}

		if _, err := os.Stat(filepath.Join(wt.Dir(), "app", "newfile.go")); err == nil {
			t.Error("app/newfile.go still exists after revert")
		}

		changes, err := o.ChangedPaths(ctx, wt)
		if err != nil {
			t.Fatalf("ChangedPaths: %v", err)
		}
		for _, c := range changes {
			if c.Path == newFileGoPath {
				t.Errorf("app/newfile.go is still reported changed: %+v", c)
			}
		}
	})

	t.Run("undoes an untracked file", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 203)

		writeTestFile(t, filepath.Join(wt.Dir(), "scratch.txt"), "scratch\n")

		if err := o.RevertPaths(ctx, wt, []Change{{Path: "scratch.txt", Code: Untracked}}); err != nil {
			t.Fatalf("RevertPaths: %v", err)
		}

		if _, err := os.Stat(filepath.Join(wt.Dir(), "scratch.txt")); err == nil {
			t.Error("scratch.txt still exists after revert")
		}
	})

	t.Run("errors if a path is left changed", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 204)

		// README.md is genuinely Modified, but we deliberately pass the
		// wrong Code (Untracked), so RevertPaths just os.Removes it instead
		// of restoring it. That leaves the tracked file deleted on disk,
		// which ChangedPaths still reports as changed, so the post-revert
		// recheck must catch it and error.
		writeTestFile(t, filepath.Join(wt.Dir(), readmePath), "# changed\n")

		err := o.RevertPaths(ctx, wt, []Change{{Path: readmePath, Code: Untracked}})
		if err == nil {
			t.Fatal("RevertPaths: expected an error for a path left changed, got nil")
		}
		if !strings.Contains(err.Error(), readmePath) {
			t.Errorf("error %q does not name the leftover path", err.Error())
		}
	})

	// PR review finding F: RevertPaths is content-mutating and destructive
	// (it calls os.Remove) but used to trust Change.Path unsanitized. A
	// path that escapes the worktree via "../" must be rejected before any
	// file is touched.
	t.Run("errors on a path that escapes the worktree", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 205)

		err := o.RevertPaths(ctx, wt, []Change{{Path: "../escape", Code: Untracked}})
		if err == nil {
			t.Fatal("RevertPaths: expected an error for a path escaping the worktree, got nil")
		}
	})

	// PR review finding F: RevertPaths used to never revalidate wt, so it
	// could act on a worktree whose checked-out branch no longer matches
	// wt.branch.
	t.Run("errors for a worktree whose checked-out branch drifted", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 206)

		runGit(ctx, t, wt.Dir(), "checkout", "-b", "zing/206-drifted")

		err := o.RevertPaths(ctx, wt, []Change{{Path: readmePath, Code: Modified}})
		if err == nil {
			t.Fatal("RevertPaths: expected an error for a worktree whose checked-out branch drifted, got nil")
		}
	})

	// PR review finding: validateRevertPath used to check containment only
	// lexically ("../" in the textual path), so a symlinked path component
	// inside the worktree pointing elsewhere on disk could still resolve
	// outside it even though the textual Path never contains "..". This
	// builds exactly that: a symlink inside the worktree pointing at an
	// unrelated temp directory outside it, with an ordinary filename
	// appended after the symlink component in Path.
	t.Run("errors on a symlink-escape attempt", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 207)

		outside := t.TempDir()
		writeTestFile(t, filepath.Join(outside, "secret.txt"), "outside the worktree\n")

		linkName := "escape-link"
		if err := os.Symlink(outside, filepath.Join(wt.Dir(), linkName)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		err := o.RevertPaths(ctx, wt, []Change{{Path: linkName + "/secret.txt", Code: Untracked}})
		if err == nil {
			t.Fatal("RevertPaths: expected an error for a path escaping the worktree through a symlink, got nil")
		}

		if _, statErr := os.Stat(filepath.Join(outside, "secret.txt")); statErr != nil {
			t.Errorf("expected the file outside the worktree to survive untouched: %v", statErr)
		}
	})

	// PR review finding: a directory pathspec passed to "git restore" or
	// os.Remove behaves unexpectedly (a whole subtree, not the single path
	// the caller asked for). validateRevertPath now rejects Path when it
	// names a directory.
	t.Run("errors when the path is a directory", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 208)

		err := o.RevertPaths(ctx, wt, []Change{{Path: "app", Code: Untracked}})
		if err == nil {
			t.Fatal("RevertPaths: expected an error for a path that is a directory, got nil")
		}

		if _, statErr := os.Stat(filepath.Join(wt.Dir(), "app", "main.go")); statErr != nil {
			t.Errorf("expected app/main.go to survive untouched: %v", statErr)
		}
	})
}

// TestRevertPathsRunsNoHook proves RevertPaths' git calls carry the
// hooks-disabling prefix: a repo-local post-checkout hook that writes a
// marker file leaves no marker after a successful RevertPaths.
func TestRevertPathsRunsNoHook(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 209)

	marker := filepath.Join(t.TempDir(), "marker")
	// Hooks are shared across every worktree of a repository, living in
	// the main checkout's ".git/hooks", not the linked worktree's own
	// ".git" pointer file.
	repoDir := filepath.Dir(filepath.Dir(filepath.Dir(wt.Dir()))) // <repo>/.zing/wt/<id> -> <repo>
	hookPath := filepath.Join(repoDir, ".git", "hooks", "post-checkout")
	writeTestFile(t, hookPath, "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(hookPath, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", hookPath, err)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), readmePath), "# changed\n")
	if err := o.RevertPaths(ctx, wt, []Change{{Path: readmePath, Code: Modified}}); err != nil {
		t.Fatalf("RevertPaths: %v", err)
	}

	assertMarkerAbsent(t, marker)
}

// -----------------------------------------------------------------------
// Real-git integration: Hunk, BranchCommits
// -----------------------------------------------------------------------

func TestHunk(t *testing.T) {
	t.Parallel()
	t.Run("modified", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 700)
		writeTestFile(t, filepath.Join(wt.Dir(), readmePath), "# changed\n")

		got, err := o.Hunk(ctx, wt, Change{Path: readmePath, Code: Modified})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if !strings.Contains(got, "# changed") {
			t.Errorf("Hunk = %q, want it to contain the new content", got)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 701)
		if err := os.Remove(filepath.Join(wt.Dir(), "docs", "extra.md")); err != nil {
			t.Fatalf("remove docs/extra.md: %v", err)
		}

		got, err := o.Hunk(ctx, wt, Change{Path: docsExtraPath, Code: Deleted})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if !strings.Contains(got, "-# extra") {
			t.Errorf("Hunk = %q, want it to show the removed content", got)
		}
	})

	t.Run("added", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 702)
		writeTestFile(t, filepath.Join(wt.Dir(), "app", "newfile.go"), "package app\n")
		runGit(ctx, t, wt.Dir(), "add", newFileGoPath)

		got, err := o.Hunk(ctx, wt, Change{Path: newFileGoPath, Code: Added})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if !strings.Contains(got, "package app") {
			t.Errorf("Hunk = %q, want it to contain the new file's content", got)
		}
	})

	t.Run("untracked", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 703)
		writeTestFile(t, filepath.Join(wt.Dir(), "scratch.txt"), "scratch content\n")

		got, err := o.Hunk(ctx, wt, Change{Path: "scratch.txt", Code: Untracked})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if !strings.Contains(got, "scratch content") {
			t.Errorf("Hunk = %q, want it to contain the untracked file's content", got)
		}
	})

	t.Run("binary", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 704)
		binPath := filepath.Join(wt.Dir(), "image.bin")
		if err := os.WriteFile(binPath, []byte{0x00, 0x01, 0x02, 0x00, 0xFF}, 0o644); err != nil {
			t.Fatalf("write %s: %v", binPath, err)
		}
		runGit(ctx, t, wt.Dir(), "add", "image.bin")

		got, err := o.Hunk(ctx, wt, Change{Path: "image.bin", Code: Added})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if got != "binary file" {
			t.Errorf("Hunk = %q, want the literal %q", got, "binary file")
		}
	})

	// PR review finding F023: Hunk used to test
	// strings.Contains(out, binaryDiffMarker) over the whole diff, so a
	// text file whose content happens to contain the literal text
	// "Binary files " anywhere -- a real hunk line, not git's own binary
	// marker -- was misreported as "binary file", hiding the real hunk
	// from the perimeter review. The fix anchors the marker to a line
	// start, so a content line containing that text mid-diff no longer
	// matches.
	t.Run("a text file whose content contains the binary marker text is not mistaken for binary", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 7040)

		const content = "Binary files a/x and b/x differ\n"
		writeTestFile(t, filepath.Join(wt.Dir(), "notbinary.txt"), content)
		runGit(ctx, t, wt.Dir(), "add", "notbinary.txt")

		got, err := o.Hunk(ctx, wt, Change{Path: "notbinary.txt", Code: Added})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if got == "binary file" {
			t.Fatal("Hunk: reported binary file for a text file, want the real hunk")
		}
		if !strings.Contains(got, "Binary files a/x and b/x differ") {
			t.Errorf("Hunk = %q, want it to contain the real hunk text", got)
		}
	})

	t.Run("a hunk larger than 64 KiB is cut", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 705)

		var b strings.Builder
		for i := range 20000 {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		writeTestFile(t, filepath.Join(wt.Dir(), "big.txt"), b.String())
		runGit(ctx, t, wt.Dir(), "add", "big.txt")

		got, err := o.Hunk(ctx, wt, Change{Path: "big.txt", Code: Added})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if !strings.HasSuffix(got, hunkCutSuffix) {
			t.Errorf("Hunk does not end with the cut suffix %q, got suffix %q", hunkCutSuffix, got[max(0, len(got)-60):])
		}
		// PR review finding F020: the cut used to always land at exactly
		// hunkMaxBytes, which can split a multi-byte rune. The fix backs
		// the cut index off to the nearest rune start, so the cut can now
		// land a few bytes short of hunkMaxBytes; <= (not ==) is the
		// correct assertion for ASCII content too, where no backing off is
		// ever needed and the two still coincide.
		if len(got) > hunkMaxBytes+len(hunkCutSuffix) {
			t.Errorf("len(Hunk) = %d, want <= %d", len(got), hunkMaxBytes+len(hunkCutSuffix))
		}
	})

	// PR review finding F020: Hunk used to cut with out[:hunkMaxBytes],
	// which can split a multi-byte rune straddling that exact byte offset,
	// producing invalid UTF-8. This calibrates the real diff header length
	// for one file (through Hunk itself, so it tracks whatever git actually
	// produces, not a guessed constant), then rewrites the same path with
	// repeating 2-byte runes phase-shifted so the byte at hunkMaxBytes
	// falls on a continuation byte, and proves the fix backs off to the
	// rune boundary instead of splitting it.
	t.Run("a hunk cut is backed off to a rune boundary", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 7050)
		const path = "multiboundary.txt"

		writeTestFile(t, filepath.Join(wt.Dir(), path), "~\n")
		runGit(ctx, t, wt.Dir(), "add", path)
		calib, err := o.Hunk(ctx, wt, Change{Path: path, Code: Added})
		if err != nil {
			t.Fatalf("Hunk (calibration): %v", err)
		}
		marker := strings.LastIndex(calib, "+~")
		if marker < 0 {
			t.Fatalf("calibration: could not find the content marker in %q", calib)
		}
		contentStart := marker + 1 // the byte right after "+"

		// R is where hunkMaxBytes falls relative to the start of this
		// file's content. A 2-byte rune repeated from contentStart has rune
		// starts at even relative offsets; padding by one extra ASCII byte
		// when R is already even flips the parity so the rune straddling
		// hunkMaxBytes is guaranteed split, deterministically, whatever
		// this header's exact length turns out to be.
		r := hunkMaxBytes - contentStart
		pad := ""
		if r%2 == 0 {
			pad = "_"
		}

		runeCount := hunkMaxBytes/2 + 100
		var b strings.Builder
		b.WriteString(pad)
		for range runeCount {
			b.WriteRune('é') // U+00E9, 2 bytes in UTF-8
		}
		b.WriteString("\n")
		writeTestFile(t, filepath.Join(wt.Dir(), path), b.String())
		runGit(ctx, t, wt.Dir(), "add", path)

		got, err := o.Hunk(ctx, wt, Change{Path: path, Code: Added})
		if err != nil {
			t.Fatalf("Hunk: %v", err)
		}
		if !utf8.ValidString(got) {
			t.Errorf("Hunk cut a multi-byte rune in half, result is not valid UTF-8: %q", got[max(0, len(got)-40):])
		}
		if len(got) > hunkMaxBytes+len(hunkCutSuffix) {
			t.Errorf("len(Hunk) = %d, want <= %d", len(got), hunkMaxBytes+len(hunkCutSuffix))
		}
	})

	t.Run("an unrecognized status is an error", func(t *testing.T) {
		t.Parallel()
		o, wt, ctx := preparePerimeterWorktree(t, 706)
		if _, err := o.Hunk(ctx, wt, Change{Path: "x", Code: Status(99)}); err == nil {
			t.Fatal("Hunk: expected an error for an unrecognized status, got nil")
		}
	})
}

// configureMarkerTextconv writes a small script that touches marker before
// cat-ing its one argument, and configures it as driver's textconv command,
// so a test can prove Hunk's --no-textconv flag stops it from ever running.
func configureMarkerTextconv(ctx context.Context, t *testing.T, repo, driver, marker string) {
	t.Helper()
	scriptPath := filepath.Join(repo, "textconv-"+driver+".sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\ntouch "+marker+"\ncat \"$1\"\n")
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", scriptPath, err)
	}
	runGit(ctx, t, repo, "config", "diff."+driver+".textconv", scriptPath)
}

// TestHunkIgnoresTextconv proves Hunk's --no-textconv flag: a
// "diff.<driver>.textconv" configured for the changed path's extension,
// with a command that writes a marker file, leaves no marker after Hunk.
func TestHunkIgnoresTextconv(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 707)

	repoDir := filepath.Dir(filepath.Dir(filepath.Dir(wt.Dir())))
	marker := filepath.Join(t.TempDir(), "marker")
	configureMarkerTextconv(ctx, t, repoDir, "x", marker)
	writeTestFile(t, filepath.Join(wt.Dir(), ".gitattributes"), "*.md diff=x\n")

	writeTestFile(t, filepath.Join(wt.Dir(), "docs", "extra.md"), "# changed extra\n")

	got, err := o.Hunk(ctx, wt, Change{Path: docsExtraPath, Code: Modified})
	if err != nil {
		t.Fatalf("Hunk: %v", err)
	}
	if got == "" {
		t.Error("Hunk: expected a non-empty diff")
	}
	assertMarkerAbsent(t, marker)
}

// TestBranchCommitsOrder proves BranchCommits returns the ticket branch's
// own commits, oldest first.
func TestBranchCommitsOrder(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 710)

	names := []string{"one.txt", "two.txt", "three.txt"}
	shas := make([]string, 0, len(names))
	for i, name := range names {
		writeTestFile(t, filepath.Join(wt.Dir(), name), fmt.Sprintf("content %d\n", i))
		runGit(ctx, t, wt.Dir(), "add", name)
		runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", fmt.Sprintf("commit %d", i))
		shas = append(shas, strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD")))
	}

	got, err := o.BranchCommits(ctx, wt)
	if err != nil {
		t.Fatalf("BranchCommits: %v", err)
	}
	if !slices.Equal(got, shas) {
		t.Errorf("BranchCommits = %v, want %v (oldest first)", got, shas)
	}
}

// TestBranchCommitsWithoutBaseRefUsesLocalDefault proves baseRev's own
// fallback: a ticket created before fetchBase ever ran (or one whose base
// ref was removed by hand) still gets a usable comparison, against the
// local default branch, rather than an error.
func TestBranchCommitsWithoutBaseRefUsesLocalDefault(t *testing.T) {
	t.Parallel()
	o, wt, ctx := preparePerimeterWorktree(t, 735)

	runGit(ctx, t, wt.Dir(), "update-ref", "-d", "refs/zing/base/main")

	writeTestFile(t, filepath.Join(wt.Dir(), "solo.txt"), "content\n")
	runGit(ctx, t, wt.Dir(), "add", "solo.txt")
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "solo commit")
	sha := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	got, err := o.BranchCommits(ctx, wt)
	if err != nil {
		t.Fatalf("BranchCommits: %v", err)
	}
	if !slices.Equal(got, []string{sha}) {
		t.Errorf("BranchCommits = %v, want [%s]", got, sha)
	}
}
