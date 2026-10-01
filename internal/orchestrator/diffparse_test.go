package orchestrator

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// nestedAGoPath and oldGoPath are reused across this file's cases (they
// also appear inside the hand-built diff bodies below, which sed a shared
// constant into would only obscure, so only the standalone occurrences use
// these), pulled out so goconst does not flag the repeats.
const (
	nestedAGoPath = "internal/x/a.go"
	oldGoPath     = "old.go"
)

// -----------------------------------------------------------------------
// Pure: ParseDiff
// -----------------------------------------------------------------------

// modifiedFileDiff, addedFileDiff, deletedFileDiff, and binaryFileDiff are
// small, hand-built unified diffs, one file each, in the shape "git diff
// --no-renames" actually emits (PKG9-PLAN.md section 10.1, 6.3's own
// worked example for the modified case).
const modifiedFileDiff = `diff --git a/internal/x/a.go b/internal/x/a.go
index 1111111..2222222 100644
--- a/internal/x/a.go
+++ b/internal/x/a.go
@@ -10,4 +10,6 @@ func Foo() {
-old line
+new line
+another line
@@ -40,0 +43,3 @@ func Bar() {
+added line
+added line
+added line
`

const addedFileDiff = `diff --git a/new.go b/new.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.go
@@ -0,0 +1,5 @@
+package x
+
+func New() {}
`

const deletedFileDiff = `diff --git a/old.go b/old.go
deleted file mode 100644
index 4444444..0000000
--- a/old.go
+++ /dev/null
@@ -1,20 +0,0 @@
-package x
`

const binaryFileDiff = `diff --git a/image.png b/image.png
index 5555555..6666666 100644
Binary files a/image.png and b/image.png differ
`

func TestParseDiff(t *testing.T) {
	t.Parallel()
	t.Run("modified file: 6.3's worked example ranges", func(t *testing.T) {
		t.Parallel()
		idx := ParseDiff(modifiedFileDiff)
		want := DiffIndex{
			nestedAGoPath: {{From: 10, To: 15}, {From: 43, To: 45}},
		}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("added file: new-side range from -0,0", func(t *testing.T) {
		t.Parallel()
		idx := ParseDiff(addedFileDiff)
		want := DiffIndex{"new.go": {{From: 1, To: 5}}}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("deleted file: old-side range, new side +0,0 ignored", func(t *testing.T) {
		t.Parallel()
		idx := ParseDiff(deletedFileDiff)
		want := DiffIndex{oldGoPath: {{From: 1, To: 20}}}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("binary file: path recorded, no range", func(t *testing.T) {
		t.Parallel()
		idx := ParseDiff(binaryFileDiff)
		want := DiffIndex{"image.png": nil}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
		if idx.Contains("image.png", 1) {
			t.Error(`Contains("image.png", 1) = true, want false: a binary file has no range`)
		}
	})

	t.Run("omitted counts default to 1", func(t *testing.T) {
		t.Parallel()
		diff := `diff --git a/one.go b/one.go
--- a/one.go
+++ b/one.go
@@ -5 +5 @@
-x
+y
`
		idx := ParseDiff(diff)
		want := DiffIndex{"one.go": {{From: 5, To: 5}}}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("zero-length new-side hunk contributes no range", func(t *testing.T) {
		t.Parallel()
		diff := `diff --git a/two.go b/two.go
--- a/two.go
+++ b/two.go
@@ -10,3 +10,0 @@
-gone
-gone
-gone
@@ -20,2 +17,2 @@
-old
-old2
+new
+new2
`
		idx := ParseDiff(diff)
		want := DiffIndex{"two.go": {{From: 17, To: 18}}}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a malformed hunk header is skipped, not fatal", func(t *testing.T) {
		t.Parallel()
		diff := `diff --git a/three.go b/three.go
--- a/three.go
+++ b/three.go
@@ not a real header @@
@@ -1,2 +1,2 @@
-a
-b
+a
+c
`
		idx := ParseDiff(diff)
		want := DiffIndex{"three.go": {{From: 1, To: 2}}}
		if diff := cmp.Diff(want, idx); diff != "" {
			t.Errorf("ParseDiff mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("empty diff is an empty index", func(t *testing.T) {
		t.Parallel()
		idx := ParseDiff("")
		if len(idx) != 0 {
			t.Errorf("ParseDiff(\"\") = %v, want empty", idx)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: DiffIndex.Contains
// -----------------------------------------------------------------------

func TestDiffIndexContains(t *testing.T) {
	t.Parallel()
	idx := DiffIndex{
		nestedAGoPath: {{From: 10, To: 15}, {From: 43, To: 45}},
		oldGoPath:     {{From: 1, To: 20}},
	}

	cases := []struct {
		name string
		path string
		line int
		want bool
	}{
		{"inside first range", nestedAGoPath, 12, true},
		{"just outside first range", nestedAGoPath, 16, false},
		{"inside second range", nestedAGoPath, 44, true},
		{"deleted file range", oldGoPath, 7, true},
		{"file not in the diff", "internal/x/b.go", 3, false},
		{"range lower bound is inclusive", nestedAGoPath, 10, true},
		{"range upper bound is inclusive", nestedAGoPath, 45, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := idx.Contains(tc.path, tc.line); got != tc.want {
				t.Errorf("Contains(%q, %d) = %v, want %v", tc.path, tc.line, got, tc.want)
			}
		})
	}
}
