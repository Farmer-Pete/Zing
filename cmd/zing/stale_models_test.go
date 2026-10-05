package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// staleFiles walks root and returns the root-relative paths of every .go or
// .toml file whose body contains needle, in walk order. It skips .git
// entirely, internal/store entirely (its tests store model ids as opaque
// strings and never resolve them, and its production code never embeds a
// model-id literal), and .zing, because .zing holds Zing's own worktrees,
// old checkouts of this repo that can carry retired ids.
func staleFiles(root string, needle []byte) ([]string, error) {
	skipDirs := map[string]bool{".git": true, ".zing": true, filepath.Join("internal", "store"): true}
	scanExts := map[string]bool{".go": true, ".toml": true}
	var stale []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !scanExts[filepath.Ext(path)] {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(body, needle) {
			stale = append(stale, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stale, nil
}

// TestNoFixturePinsOpus48 is the repository invariant behind the opus alias
// move: no Go or TOML file outside internal/store may still carry the
// retired default. internal/store is exempt entirely, not just its tests:
// its tests store the id as an opaque string and never resolve it, and its
// production code never embeds a model-id literal at all. .zing is also
// exempt, because it holds Zing's own worktrees: old checkouts of this repo
// that can carry the retired id even when the live tree does not. The
// needle is built from two halves so this file never matches itself.
func TestNoFixturePinsOpus48(t *testing.T) {
	t.Parallel()
	stale := []byte("claude-opus-4-" + "8")
	root := filepath.Join("..", "..")
	paths, err := staleFiles(root, stale)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, rel := range paths {
		t.Errorf("%s still pins %s; the opus alias defaults to claude-opus-5-5", rel, stale)
	}
}

// TestStaleFiles_SkipsZingWorktrees proves staleFiles does not descend into
// .zing, which holds Zing's own worktrees: old checkouts of this repo that
// can carry a needle the live tree no longer does.
func TestStaleFiles_SkipsZingWorktrees(t *testing.T) {
	t.Parallel()
	needle := []byte("claude-opus-4-" + "8")
	root := t.TempDir()

	write := func(rel string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, needle, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", full, err)
		}
	}

	write(filepath.Join(".zing", "wt", "1", "internal", "config", "config.go"))
	write(filepath.Join(".zing", "wt", "1", "internal", "store", "store_test.go"))
	write(filepath.Join("internal", "store", "y.go"))
	write(filepath.Join("cmd", "x.go"))
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile a.go: %v", err)
	}

	got, err := staleFiles(root, needle)
	if err != nil {
		t.Fatalf("staleFiles(%s): %v", root, err)
	}
	want := []string{filepath.Join("cmd", "x.go")}
	if !slices.Equal(got, want) {
		t.Errorf("staleFiles = %v, want %v", got, want)
	}
}
