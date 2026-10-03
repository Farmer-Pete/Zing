package orchestrator

// judge_test.go tests task 4's judge checkout (design section 10.2, D5):
// JudgeWorktree and JudgeTree.Remove, against real git in a temp dir. Setup
// reuses worktree_test.go's newTestRepo/newTestOrchestrator and
// perimeter_test.go's writeTestFile, plus review_test.go's commitFile, the
// pattern every git-backed test in this package already follows.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// judgeTicketDetached, judgeTicketGovernance, and so on name the ticket IDs
// each subtest below uses for its own ".zing/judge/<id>" and ".zing/wt/<id>"
// directories, so the real-git fixtures each subtest builds never collide.
const (
	judgeTicketDetached    = 900
	judgeTicketGovernance  = 901
	judgeTicketLeftover    = 902
	judgeTicketNoSmudge    = 903
	judgeTicketRemoveTwice = 904
)

// TestJudgeWorktreeDetachedAtSha proves JudgeWorktree checks sha out at
// <local_path>/.zing/judge/<ticket_id>, detached (no symbolic ref), with the
// branch's own content present.
func TestJudgeWorktreeDetachedAtSha(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, judgeTicketDetached, "judge-detached", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	sha := commitFile(ctx, t, wt.Dir(), "feature.txt", "v1\n", "add feature")

	jt, err := o.JudgeWorktree(ctx, judgeTicketDetached, sha)
	if err != nil {
		t.Fatalf("JudgeWorktree: %v", err)
	}

	wantDir := filepath.Join(repo, ".zing", "judge", "900")
	if jt.Dir() != wantDir {
		t.Errorf("Dir() = %q, want %q", jt.Dir(), wantDir)
	}

	head := strings.TrimSpace(runGit(ctx, t, jt.Dir(), "rev-parse", "HEAD"))
	if head != sha {
		t.Errorf("HEAD = %q, want %q", head, sha)
	}

	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = jt.Dir()
	cmd.Env = scrubGitLocationEnv(os.Environ())
	if err := cmd.Run(); err == nil {
		t.Error("git symbolic-ref -q HEAD succeeded, want a detached HEAD (no symbolic ref)")
	}

	if _, statErr := os.Stat(filepath.Join(jt.Dir(), "feature.txt")); statErr != nil {
		t.Errorf("expected feature.txt to be checked out: %v", statErr)
	}

	if err := jt.Remove(ctx); err != nil {
		t.Errorf("Remove: %v", err)
	}
}

// TestJudgeWorktreeGovernanceFromBase proves CLAUDE.md and AGENTS.md always
// come from the default branch (section 10.2, D5): a branch edit to
// AGENTS.md is replaced with the default branch's content, and a CLAUDE.md
// the branch adds but the default branch never had is removed, so a build
// run cannot steer the judge through either file.
func TestJudgeWorktreeGovernanceFromBase(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()

	writeTestFile(t, filepath.Join(repo, "AGENTS.md"), "base agents\n")
	runGit(ctx, t, repo, "add", "AGENTS.md")
	runGit(ctx, t, repo, "commit", "-q", "-m", "add base AGENTS.md")

	o := newTestOrchestrator(t, repo, execRunner{})
	wt, err := o.PrepareWorktree(ctx, judgeTicketGovernance, "judge-governance", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), "AGENTS.md"), "branch agents, steer the judge\n")
	writeTestFile(t, filepath.Join(wt.Dir(), "CLAUDE.md"), "branch-only claude, steer the judge\n")
	runGit(ctx, t, wt.Dir(), "add", "AGENTS.md", "CLAUDE.md")
	runGit(ctx, t, wt.Dir(), "commit", "-q", "-m", "edit governance files on the branch")
	sha := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	jt, err := o.JudgeWorktree(ctx, judgeTicketGovernance, sha)
	if err != nil {
		t.Fatalf("JudgeWorktree: %v", err)
	}

	gotAgents, err := os.ReadFile(filepath.Join(jt.Dir(), "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(gotAgents) != "base agents\n" {
		t.Errorf("AGENTS.md = %q, want the default branch's content %q", gotAgents, "base agents\n")
	}

	if _, statErr := os.Stat(filepath.Join(jt.Dir(), "CLAUDE.md")); !os.IsNotExist(statErr) {
		t.Errorf("CLAUDE.md present in the judge checkout (stat err = %v), want it removed: absent on the default branch", statErr)
	}

	if err := jt.Remove(ctx); err != nil {
		t.Errorf("Remove: %v", err)
	}
}

// TestJudgeWorktreeReplacesLeftover proves a second JudgeWorktree call
// replaces a checkout a previous call left behind (a crash before cleanup
// ran, section 10.2 step 1), rather than failing on an already-present
// directory or keeping stale content.
func TestJudgeWorktreeReplacesLeftover(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})

	// Two sibling branches off the same base, each adding only its own
	// file, so sha2's tree genuinely lacks one.txt -- unlike two sequential
	// commits on one branch, where the second would still contain the
	// first's file and so could never prove the leftover's content gone.
	wtOne, err := o.PrepareWorktree(ctx, judgeTicketLeftover, "judge-leftover-one", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree(one): %v", err)
	}
	sha1 := commitFile(ctx, t, wtOne.Dir(), "one.txt", "v1\n", "add one")

	wtTwo, err := o.PrepareWorktree(ctx, judgeTicketLeftover+1, "judge-leftover-two", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree(two): %v", err)
	}
	sha2 := commitFile(ctx, t, wtTwo.Dir(), "two.txt", "v2\n", "add two")

	if _, firstErr := o.JudgeWorktree(ctx, judgeTicketLeftover, sha1); firstErr != nil {
		t.Fatalf("JudgeWorktree(sha1): %v", firstErr)
	}
	// Simulate a crash: the first checkout is left in place, never removed.

	jt2, err := o.JudgeWorktree(ctx, judgeTicketLeftover, sha2)
	if err != nil {
		t.Fatalf("JudgeWorktree(sha2): %v", err)
	}

	wantDir := filepath.Join(repo, ".zing", "judge", "902")
	if jt2.Dir() != wantDir {
		t.Fatalf("Dir() = %q, want %q", jt2.Dir(), wantDir)
	}

	head := strings.TrimSpace(runGit(ctx, t, jt2.Dir(), "rev-parse", "HEAD"))
	if head != sha2 {
		t.Errorf("HEAD = %q, want %q (the second call's sha, not the leftover's)", head, sha2)
	}
	if _, statErr := os.Stat(filepath.Join(jt2.Dir(), "one.txt")); !os.IsNotExist(statErr) {
		t.Errorf("one.txt from the leftover checkout is still present (stat err = %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(jt2.Dir(), "two.txt")); statErr != nil {
		t.Errorf("two.txt missing from the replacement checkout: %v", statErr)
	}

	if err := jt2.Remove(ctx); err != nil {
		t.Errorf("Remove: %v", err)
	}
}

// TestJudgeWorktreeRunsNoSmudgeFilter proves the checkout JudgeWorktree runs
// carries its own filter drivers overridden to empty (mirroring
// TestPrepareWorktreeRunsNoSmudgeFilter), so a configured smudge filter --
// run naturally by an unhardened "git checkout" -- never executes.
func TestJudgeWorktreeRunsNoSmudgeFilter(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	marker := filepath.Join(t.TempDir(), "marker")

	writeTestFile(t, filepath.Join(repo, "filtered", "tracked.txt"), "content\n")
	runGit(ctx, t, repo, "add", "filtered/tracked.txt")
	runGit(ctx, t, repo, "commit", "-q", "-m", "add tracked.txt, unfiltered")

	configureMarkerFilterDriver(ctx, t, repo, "own", marker)
	writeTestFile(t, filepath.Join(repo, ".gitattributes"), "*.txt filter=own\n")
	runGit(ctx, t, repo, "add", ".gitattributes")
	runGit(ctx, t, repo, "commit", "-q", "-m", "declare the filter for *.txt")
	// Declaring the filter over an already-tracked *.txt path makes this
	// plain, unhardened setup git itself re-run the clean filter once
	// .gitattributes takes effect -- nothing to do with JudgeWorktree.
	// Clear that setup-time marker so what remains proves what
	// JudgeWorktree itself did.
	_ = os.Remove(marker)

	sha := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "HEAD"))

	o := newTestOrchestrator(t, repo, execRunner{})
	jt, err := o.JudgeWorktree(ctx, judgeTicketNoSmudge, sha)
	if err != nil {
		t.Fatalf("JudgeWorktree: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(jt.Dir(), "filtered", "tracked.txt")); statErr != nil {
		t.Errorf("expected tracked.txt to be checked out: %v", statErr)
	}
	assertMarkerAbsent(t, marker)

	if err := jt.Remove(ctx); err != nil {
		t.Errorf("Remove: %v", err)
	}
}

// TestJudgeTreeRemoveTwice proves Remove is safe to call twice: the second
// call, with the checkout already gone, still returns nil (section 10.2).
func TestJudgeTreeRemoveTwice(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})

	wt, err := o.PrepareWorktree(ctx, judgeTicketRemoveTwice, "judge-remove-twice", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}
	sha := commitFile(ctx, t, wt.Dir(), "x.txt", "v1\n", "add x")

	jt, err := o.JudgeWorktree(ctx, judgeTicketRemoveTwice, sha)
	if err != nil {
		t.Fatalf("JudgeWorktree: %v", err)
	}

	if err := jt.Remove(ctx); err != nil {
		t.Fatalf("Remove (first call): %v", err)
	}
	if _, statErr := os.Stat(jt.Dir()); !os.IsNotExist(statErr) {
		t.Fatalf("judge checkout dir still present after Remove (stat err = %v)", statErr)
	}

	if err := jt.Remove(ctx); err != nil {
		t.Errorf("Remove (second call): %v, want nil: safe to call twice", err)
	}
}

// TestJudgeTreeRemoveReturnsError proves a real "git worktree remove"
// failure (anything other than git's "is not a working tree" message) is
// returned, not swallowed. It builds a JudgeTree directly with a
// failingRunner, since this package's tests already use that technique
// (worktree_test.go) to force a deterministic git failure without depending
// on a real git failure mode.
func TestJudgeTreeRemoveReturnsError(t *testing.T) {
	t.Parallel()
	repoPath := t.TempDir()
	dir := filepath.Join(repoPath, ".zing", "judge", "905")

	failing := failingRunner{
		inner: execRunner{},
		fail: func(args []string) bool {
			return slices.Contains(args, "remove")
		},
	}
	jt := JudgeTree{dir: dir, repoPath: repoPath, run: failing, commonMu: newCommonMutex()}

	if err := jt.Remove(t.Context()); err == nil {
		t.Fatal("Remove: want an error for a forced git worktree remove failure, got nil")
	}
}
