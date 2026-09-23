package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
	starMDGlob      = "*.md"
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
	t.Run("worked example 12.2", func(t *testing.T) {
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
		changed := []Change{
			{Path: aGoPath, Code: Modified},
			{Path: "b.go", Code: Added},
		}
		got := Perimeter(changed, []string{aGoPath}, nil, nil)
		want := []Extra{{Path: "b.go", Marker: ""}}
		wantExtras(t, got, want)
	})

	t.Run("declared match is exact path, not a glob", func(t *testing.T) {
		// "internal/**" as a declared entry is not a pattern: it must match
		// a changed path byte for byte to be omitted.
		changed := []Change{{Path: "internal/orchestrator/x.go", Code: Modified}}
		got := Perimeter(changed, []string{"internal/**"}, nil, nil)
		want := []Extra{{Path: "internal/orchestrator/x.go", Marker: ""}}
		wantExtras(t, got, want)
	})

	t.Run("a path matching both trust-root and style-guide is marked trust root", func(t *testing.T) {
		changed := []Change{{Path: sharedMDPath, Code: Modified}}
		got := Perimeter(changed, nil, []string{sharedMDPath}, []string{sharedMDPath})
		want := []Extra{{Path: sharedMDPath, Marker: markerTrustRoot}}
		wantExtras(t, got, want)
	})

	t.Run("a trust-root glob marks an extra", func(t *testing.T) {
		changed := []Change{{Path: sandboxDeepPath, Code: Untracked}}
		got := Perimeter(changed, nil, []string{sandboxGlob}, nil)
		want := []Extra{{Path: sandboxDeepPath, Marker: markerTrustRoot}}
		wantExtras(t, got, want)
	})

	t.Run("result is sorted by path regardless of input order", func(t *testing.T) {
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
			if got := matchPattern(c.pattern, c.path); got != c.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: PerimeterNotice
// -----------------------------------------------------------------------

func TestPerimeterNotice(t *testing.T) {
	t.Run("worked example 12.3 content", func(t *testing.T) {
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
		got := PerimeterNotice([]Extra{{Path: aGoPath}})
		if !strings.Contains(got, "Repair") {
			t.Errorf("PerimeterNotice() missing the repair option, got:\n%s", got)
		}
		if !strings.Contains(got, "question") {
			t.Errorf("PerimeterNotice() missing the question option, got:\n%s", got)
		}
	})

	t.Run("says reverted, so the reader knows the change is gone", func(t *testing.T) {
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
	t.Run("sees a modified, an added, an untracked, and a deleted path", func(t *testing.T) {
		o, wt, ctx := preparePerimeterWorktree(t, 101)

		// Modified: an existing tracked file, edited but not staged.
		writeTestFile(t, filepath.Join(wt.Dir(), readmePath), "# changed\n")

		// Added: a new file, staged.
		writeTestFile(t, filepath.Join(wt.Dir(), "app", "newfile.go"), "package app\n")
		runGit(ctx, t, wt.Dir(), "add", "app/newfile.go")

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
		if got := changeByPath(t, changes, "app/newfile.go").Code; got != Added {
			t.Errorf("app/newfile.go code = %v, want Added", got)
		}
		if got := changeByPath(t, changes, "scratch.txt").Code; got != Untracked {
			t.Errorf("scratch.txt code = %v, want Untracked", got)
		}
		if got := changeByPath(t, changes, "docs/extra.md").Code; got != Deleted {
			t.Errorf("docs/extra.md code = %v, want Deleted", got)
		}

		if !sort.SliceIsSorted(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path }) {
			t.Errorf("ChangedPaths() result not sorted by path: %+v", changes)
		}
	})

	t.Run("a rename is reported as a deleted old path and an added new path", func(t *testing.T) {
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
	t.Run("undoes a modified file", func(t *testing.T) {
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
		o, wt, ctx := preparePerimeterWorktree(t, 202)

		writeTestFile(t, filepath.Join(wt.Dir(), "app", "newfile.go"), "package app\n")
		runGit(ctx, t, wt.Dir(), "add", "app/newfile.go")

		if err := o.RevertPaths(ctx, wt, []Change{{Path: "app/newfile.go", Code: Added}}); err != nil {
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
			if c.Path == "app/newfile.go" {
				t.Errorf("app/newfile.go is still reported changed: %+v", c)
			}
		}
	})

	t.Run("undoes an untracked file", func(t *testing.T) {
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
}
