package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestNoFixturePinsOpus48 is the repository invariant behind the opus alias
// move: no Go or TOML file outside internal/store may still carry the
// retired default. internal/store is exempt entirely, not just its tests:
// its tests store the id as an opaque string and never resolve it, and its
// production code never embeds a model-id literal at all. The needle is
// built from two halves so this file never matches itself.
func TestNoFixturePinsOpus48(t *testing.T) {
	t.Parallel()
	stale := []byte("claude-opus-4-" + "8")
	root := filepath.Join("..", "..")
	skipDirs := map[string]bool{".git": true, filepath.Join("internal", "store"): true}
	scanExts := map[string]bool{".go": true, ".toml": true}
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
		if bytes.Contains(body, stale) {
			t.Errorf("%s still pins %s; the opus alias defaults to claude-opus-5-5", rel, stale)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
