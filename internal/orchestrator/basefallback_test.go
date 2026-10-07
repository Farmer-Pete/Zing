package orchestrator

import (
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/gitfixture"
)

// TestTakeBaseFallback proves that a fetch which falls back because origin
// is unreachable records a BaseFallback that TakeBaseFallback returns
// exactly once, and that a second call reports nothing.
func TestTakeBaseFallback(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	missing := filepath.Join(t.TempDir(), "missing.git")
	runGit(ctx, t, repo, "remote", "add", "origin", missing)

	o := newTestOrchestrator(t, repo, execRunner{})

	wt, _, err := o.EnsureWorktree(ctx, 801, "stale-base")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if _, err := o.FetchBase(ctx, wt); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	wantSHA := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main"))

	f, ok := o.TakeBaseFallback(801)
	if !ok {
		t.Fatalf("TakeBaseFallback: ok = false, want true")
	}
	want := BaseFallback{Branch: mainBranch, SHA: wantSHA, Reason: "no_origin"}
	if f != want {
		t.Errorf("TakeBaseFallback = %+v, want %+v", f, want)
	}

	if _, ok := o.TakeBaseFallback(801); ok {
		t.Error("second TakeBaseFallback: ok = true, want false")
	}
}

// TestTakeBaseFallbackEmptyAfterFetch proves that a fetch which succeeds
// leaves TakeBaseFallback with nothing to report.
func TestTakeBaseFallbackEmptyAfterFetch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	repo := newTestRepo(t)
	if _, err := gitfixture.WithBareOrigin(ctx, repo); err != nil {
		t.Fatalf("WithBareOrigin: %v", err)
	}
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	o := newTestOrchestrator(t, repo, execRunner{})

	wt, _, err := o.EnsureWorktree(ctx, 802, "stale-base-ok")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if _, err := o.FetchBase(ctx, wt); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	if _, ok := o.TakeBaseFallback(802); ok {
		t.Error("TakeBaseFallback: ok = true, want false (the fetch succeeded)")
	}
}
