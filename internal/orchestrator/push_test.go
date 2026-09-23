package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// Pure: PullRequest.Body
// -----------------------------------------------------------------------

func TestPullRequestBody(t *testing.T) {
	t.Run("six sections render in order", func(t *testing.T) {
		pr := PullRequest{
			Title:            "Add the perimeter diff",
			What:             "Adds Perimeter, the classifier for changed paths.",
			WorkingDemo:      "Run go test ./internal/orchestrator/...",
			Scenarios:        "- a declared path is omitted\n- an undeclared path is an Extra",
			DeclaredFiles:    "| Path | Note |\n| --- | --- |\n| perimeter.go | new |",
			ChestertonsFence: "None removed.",
			PlanLink:         "https://example.com/plan",
		}

		got, err := pr.Body()
		if err != nil {
			t.Fatalf("Body: unexpected error: %v", err)
		}

		want := "## What\n\n" + pr.What + "\n\n" +
			"## Working demo\n\n" + pr.WorkingDemo + "\n\n" +
			"## Scenarios\n\n" + pr.Scenarios + "\n\n" +
			"## Declared files\n\n" + pr.DeclaredFiles + "\n\n" +
			"## Chesterton's fence\n\n" + pr.ChestertonsFence + "\n\n" +
			"## Plan\n\n" + pr.PlanLink + "\n"

		if got != want {
			t.Errorf("Body() =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("an empty section renders its heading followed by None.", func(t *testing.T) {
		pr := PullRequest{Title: "A title"}

		got, err := pr.Body()
		if err != nil {
			t.Fatalf("Body: unexpected error: %v", err)
		}

		want := "## What\n\nNone.\n\n" +
			"## Working demo\n\nNone.\n\n" +
			"## Scenarios\n\nNone.\n\n" +
			"## Declared files\n\nNone.\n\n" +
			"## Chesterton's fence\n\nNone.\n\n" +
			"## Plan\n\nNone.\n"

		if got != want {
			t.Errorf("Body() =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("empty title is an error", func(t *testing.T) {
		pr := PullRequest{What: "something"}
		if _, err := pr.Body(); err == nil {
			t.Fatal("Body: expected an error for an empty title, got nil")
		}
	})
}

// -----------------------------------------------------------------------
// Real git: Push
// -----------------------------------------------------------------------

// newBareRemote inits a bare repo in a fresh temp directory, to stand in
// for "origin" in the push tests. Like worktree_test.go's newTestRepo, it
// runs git through runGit (which threads ctx via exec.CommandContext)
// rather than a bare exec.Command call.
func newBareRemote(ctx context.Context, t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	runGit(ctx, t, resolved, "init", "-q", "--bare", "-b", mainBranch)

	return resolved
}

// addOrigin adds remoteDir as repoDir's "origin" remote.
func addOrigin(ctx context.Context, t *testing.T, repoDir, remoteDir string) {
	t.Helper()
	runGit(ctx, t, repoDir, "remote", "add", "origin", remoteDir)
}

func TestPush(t *testing.T) {
	t.Run("a signed branch pushes and the bare remote receives it", func(t *testing.T) {
		newSigningFixture(t, true)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 20, "", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "approved content\n")
		msg := CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}
		if _, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg); err != nil {
			t.Fatalf("CommitTask: %v", err)
		}

		if err := o.Push(ctx, wt); err != nil {
			t.Fatalf("Push: unexpected error: %v", err)
		}

		out := runGit(ctx, t, remote, "show-ref", "--verify", "refs/heads/"+wt.Branch())
		if !strings.Contains(out, "refs/heads/"+wt.Branch()) {
			t.Errorf("bare remote did not receive refs/heads/%s, show-ref = %q", wt.Branch(), out)
		}
	})

	t.Run("refused for the default branch, before any git command runs", func(t *testing.T) {
		o := newTestOrchestrator(t, absLocalPath, noCallRunner{t: t})
		wt := Worktree{dir: absLocalPath, branch: mainBranch}

		if err := o.Push(t.Context(), wt); err == nil {
			t.Fatal("Push: expected an error for the default branch, got nil")
		}
	})

	t.Run("refused for a forged non-zing branch, before any git command runs", func(t *testing.T) {
		o := newTestOrchestrator(t, absLocalPath, noCallRunner{t: t})
		wt := Worktree{dir: absLocalPath, branch: "not-zing/anything"}

		if err := o.Push(t.Context(), wt); err == nil {
			t.Fatal("Push: expected an error for a non-zing branch, got nil")
		}
	})
}

// -----------------------------------------------------------------------
// OpenDraftPR: real git plus a scripted GitHub double
// -----------------------------------------------------------------------

// scriptedPushGitHub is a configurable GitHub double for OpenDraftPR tests.
// Unlike worktree_test.go's fakeGitHub (which exists only to satisfy New's
// required parameter and always errors), this records the arguments
// CreateDraftPR and FindPRByHead were called with and returns scripted
// results, so a test can assert OpenDraftPR's push-then-create-then-find-
// fallback sequence.
type scriptedPushGitHub struct {
	createURL    string
	createNumber int
	createErr    error
	createCalls  int
	gotHead      string
	gotBase      string
	gotTitle     string
	gotBody      string

	findURL    string
	findNumber int
	findOK     bool
	findErr    error
	findCalls  int
}

func (g *scriptedPushGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errors.New("scriptedPushGitHub: RepoDefaultBranch not implemented")
}

func (g *scriptedPushGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errors.New("scriptedPushGitHub: RequiredChecks not implemented")
}

func (g *scriptedPushGitHub) CreateDraftPR(_ context.Context, _, _, head, base, title, body string) (url string, number int, err error) {
	g.createCalls++
	g.gotHead, g.gotBase, g.gotTitle, g.gotBody = head, base, title, body
	if g.createErr != nil {
		return "", 0, g.createErr
	}
	return g.createURL, g.createNumber, nil
}

func (g *scriptedPushGitHub) FindPRByHead(context.Context, string, string, string) (url string, number int, ok bool, err error) {
	g.findCalls++
	if g.findErr != nil {
		return "", 0, false, g.findErr
	}
	return g.findURL, g.findNumber, g.findOK, nil
}

// newTestOrchestratorWithGitHub mirrors worktree_test.go's
// newTestOrchestrator, but with a caller-supplied GitHub double instead of
// the always-erroring fakeGitHub{}, since OpenDraftPR needs one that
// actually returns scripted PR data.
func newTestOrchestratorWithGitHub(t *testing.T, localPath string, run Runner, gh GitHub) *Orchestrator {
	t.Helper()
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: localPath, DefaultBranch: mainBranch}
	log := slog.New(slog.DiscardHandler)
	o, err := New(proj, gh, run, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

// prepareSignedCommit prepares a worktree for ticketID and commits one
// approved file to it, signed, for tests that exercise Push or OpenDraftPR
// past the commit stage.
func prepareSignedCommit(ctx context.Context, t *testing.T, o *Orchestrator, ticketID int64) Worktree {
	t.Helper()

	wt, err := o.PrepareWorktree(ctx, ticketID, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "approved content\n")
	msg := CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}
	if _, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg); err != nil {
		t.Fatalf("CommitTask: %v", err)
	}

	return wt
}

func TestOpenDraftPR(t *testing.T) {
	t.Run("returns the created url and number", func(t *testing.T) {
		newSigningFixture(t, true)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		gh := &scriptedPushGitHub{
			createURL:    "https://github.com/acme/widgets/pull/9",
			createNumber: 9,
		}
		o := newTestOrchestratorWithGitHub(t, repo, execRunner{}, gh)
		wt := prepareSignedCommit(ctx, t, o, 30)

		url, number, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err != nil {
			t.Fatalf("OpenDraftPR: unexpected error: %v", err)
		}
		if url != gh.createURL {
			t.Errorf("url = %q, want %q", url, gh.createURL)
		}
		if number != gh.createNumber {
			t.Errorf("number = %d, want %d", number, gh.createNumber)
		}
		if gh.gotHead != wt.Branch() {
			t.Errorf("CreateDraftPR head = %q, want %q", gh.gotHead, wt.Branch())
		}
		if gh.gotBase != mainBranch {
			t.Errorf("CreateDraftPR base = %q, want %q", gh.gotBase, mainBranch)
		}
		if gh.findCalls != 0 {
			t.Errorf("FindPRByHead called %d times, want 0 (CreateDraftPR succeeded)", gh.findCalls)
		}

		out := runGit(ctx, t, remote, "show-ref", "--verify", "refs/heads/"+wt.Branch())
		if !strings.Contains(out, "refs/heads/"+wt.Branch()) {
			t.Errorf("bare remote did not receive refs/heads/%s, show-ref = %q", wt.Branch(), out)
		}
	})

	t.Run("falls back to FindPRByHead when CreateDraftPR errors", func(t *testing.T) {
		newSigningFixture(t, true)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		gh := &scriptedPushGitHub{
			createErr:  errors.New("create pr: boom"),
			findURL:    "https://github.com/acme/widgets/pull/5",
			findNumber: 5,
			findOK:     true,
		}
		o := newTestOrchestratorWithGitHub(t, repo, execRunner{}, gh)
		wt := prepareSignedCommit(ctx, t, o, 31)

		url, number, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err != nil {
			t.Fatalf("OpenDraftPR: unexpected error: %v", err)
		}
		if url != gh.findURL {
			t.Errorf("url = %q, want %q", url, gh.findURL)
		}
		if number != gh.findNumber {
			t.Errorf("number = %d, want %d", number, gh.findNumber)
		}
		if gh.createCalls != 1 {
			t.Errorf("CreateDraftPR called %d times, want 1", gh.createCalls)
		}
		if gh.findCalls != 1 {
			t.Errorf("FindPRByHead called %d times, want 1", gh.findCalls)
		}
	})

	t.Run("returns the create error when FindPRByHead also fails to find one", func(t *testing.T) {
		newSigningFixture(t, true)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		gh := &scriptedPushGitHub{
			createErr: errors.New("create pr: boom"),
			findOK:    false,
		}
		o := newTestOrchestratorWithGitHub(t, repo, execRunner{}, gh)
		wt := prepareSignedCommit(ctx, t, o, 32)

		_, _, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err == nil {
			t.Fatal("OpenDraftPR: expected an error when create fails and no existing pr is found, got nil")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("OpenDraftPR error = %q, want it to mention the create error %q", err.Error(), "boom")
		}
	})
}
