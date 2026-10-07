package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------
// Pure: PullRequest.Body
// -----------------------------------------------------------------------

func TestPullRequestBody(t *testing.T) {
	t.Parallel()
	t.Run("seven sections render in order", func(t *testing.T) {
		t.Parallel()
		pr := PullRequest{
			Title:            "Add the perimeter diff",
			What:             "Adds Perimeter, the classifier for changed paths.",
			WorkingDemo:      "Run go test ./internal/orchestrator/...",
			Scenarios:        "- a declared path is omitted\n- an undeclared path is an Extra",
			AcceptedFindings: "Review reached its fix loop cap.\n\n- r3f1 minor a.go:1 x",
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
			"## Accepted review findings\n\n" + pr.AcceptedFindings + "\n\n" +
			"## Declared files\n\n" + pr.DeclaredFiles + "\n\n" +
			"## Chesterton's fence\n\n" + pr.ChestertonsFence + "\n\n" +
			"## Plan\n\n" + pr.PlanLink + "\n"

		if got != want {
			t.Errorf("Body() =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("an empty section renders its heading followed by None.", func(t *testing.T) {
		t.Parallel()
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

	t.Run("an empty AcceptedFindings is left out", func(t *testing.T) {
		t.Parallel()
		pr := PullRequest{Title: "Some title"}

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
		if strings.Contains(got, "Accepted review findings") {
			t.Errorf("Body() = %q, want no Accepted review findings section", got)
		}
	})

	t.Run("empty title is an error", func(t *testing.T) {
		t.Parallel()
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
	t.Parallel()
	t.Run("a signed branch pushes and the bare remote receives it", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
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
		t.Parallel()
		o := newTestOrchestrator(t, absLocalPath, noCallRunner{t: t})
		wt := Worktree{dir: absLocalPath, branch: mainBranch}

		if err := o.Push(t.Context(), wt); err == nil {
			t.Fatal("Push: expected an error for the default branch, got nil")
		}
	})

	t.Run("refused for a forged non-zing branch, before any git command runs", func(t *testing.T) {
		t.Parallel()
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

	findURL     string
	findNumber  int
	findOK      bool
	findErr     error
	findCalls   int
	gotFindBase string
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

func (g *scriptedPushGitHub) FindPRByHead(_ context.Context, _, _, _, base string) (url string, number int, ok bool, err error) {
	g.findCalls++
	g.gotFindBase = base
	if g.findErr != nil {
		return "", 0, false, g.findErr
	}
	return g.findURL, g.findNumber, g.findOK, nil
}

// flakyPushRunner wraps a real execRunner, failing the first failures calls
// to "git push origin ..." with out and a generic non-zero-exit error, then
// delegating every push call (and every other command, always) to
// execRunner. pushes counts every push call it has seen, failed or not, so
// a test can assert how many attempts pushOrigin made.
type flakyPushRunner struct {
	failures int
	out      string
	pushes   int
}

func (r *flakyPushRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if name == "git" && len(args) >= 2 && args[0] == "push" && args[1] == "origin" {
		r.pushes++
		if r.pushes <= r.failures {
			return r.out, errors.New("exit status 1")
		}
	}
	return execRunner{}.Run(ctx, dir, name, args...)
}

func (r *flakyPushRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return execRunner{}.Output(ctx, dir, name, args...)
}

// shortPushBackoff is TestOpenDraftPRRetriesGitHub5xx's and
// TestPushRetryLogs' own stand-in for the 30s/60s/120s production defaults:
// 3 entries of 1 millisecond, so a retrying test runs fast.
var shortPushBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

// TestOpenDraftPRRetriesGitHub5xx proves pushOrigin retries a GitHub 5xx up
// to 3 times (4 attempts in all) before OpenDraftPR escalates, and that any
// other push failure escalates after a single attempt.
func TestOpenDraftPRRetriesGitHub5xx(t *testing.T) {
	t.Parallel()

	t.Run("two 5xx then success", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		runner := &flakyPushRunner{failures: 2, out: "remote: Internal Server Error"}
		gh := &scriptedPushGitHub{createURL: "https://github.com/acme/widgets/pull/40", createNumber: 40}
		o := newTestOrchestratorWithGitHub(t, repo, runner, gh)
		o.pushBackoff = shortPushBackoff
		wt := prepareSignedCommit(ctx, t, o, 740)

		url, _, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err != nil {
			t.Fatalf("OpenDraftPR: unexpected error: %v", err)
		}
		if url != gh.createURL {
			t.Errorf("url = %q, want %q", url, gh.createURL)
		}
		if runner.pushes != 3 {
			t.Errorf("pushes = %d, want 3", runner.pushes)
		}
		if gh.createCalls != 1 {
			t.Errorf("createCalls = %d, want 1", gh.createCalls)
		}

		out := runGit(ctx, t, remote, "show-ref", "--verify", "refs/heads/"+wt.Branch())
		if !strings.Contains(out, "refs/heads/"+wt.Branch()) {
			t.Errorf("bare remote did not receive refs/heads/%s, show-ref = %q", wt.Branch(), out)
		}
	})

	t.Run("four 5xx fail", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		runner := &flakyPushRunner{failures: 4, out: "remote: Internal Server Error"}
		gh := &scriptedPushGitHub{}
		o := newTestOrchestratorWithGitHub(t, repo, runner, gh)
		o.pushBackoff = shortPushBackoff
		wt := prepareSignedCommit(ctx, t, o, 741)

		_, _, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err == nil {
			t.Fatal("OpenDraftPR: expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "(attempts: 4)") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "(attempts: 4)")
		}
		if !strings.Contains(err.Error(), "Internal Server Error") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "Internal Server Error")
		}
		if runner.pushes != 4 {
			t.Errorf("pushes = %d, want 4", runner.pushes)
		}
		if gh.createCalls != 0 {
			t.Errorf("createCalls = %d, want 0", gh.createCalls)
		}
	})

	t.Run("non 5xx fails at once", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		runner := &flakyPushRunner{failures: 4, out: "! [rejected] zing/742 -> zing/742 (fetch first)"}
		gh := &scriptedPushGitHub{}
		o := newTestOrchestratorWithGitHub(t, repo, runner, gh)
		o.pushBackoff = shortPushBackoff
		wt := prepareSignedCommit(ctx, t, o, 742)

		_, _, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err == nil {
			t.Fatal("OpenDraftPR: expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "(attempts: 1)") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "(attempts: 1)")
		}
		if runner.pushes != 1 {
			t.Errorf("pushes = %d, want 1", runner.pushes)
		}
	})

	t.Run("http 503 retried", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
		ctx := t.Context()
		remote := newBareRemote(ctx, t)
		addOrigin(ctx, t, repo, remote)

		runner := &flakyPushRunner{failures: 1, out: "error: RPC failed; HTTP 503 curl 22 The requested URL returned error: 503"}
		gh := &scriptedPushGitHub{createURL: "https://github.com/acme/widgets/pull/43", createNumber: 43}
		o := newTestOrchestratorWithGitHub(t, repo, runner, gh)
		o.pushBackoff = shortPushBackoff
		wt := prepareSignedCommit(ctx, t, o, 743)

		_, _, err := o.OpenDraftPR(ctx, wt, PullRequest{Title: testCommitTitle})
		if err != nil {
			t.Fatalf("OpenDraftPR: unexpected error: %v", err)
		}
		if runner.pushes != 2 {
			t.Errorf("pushes = %d, want 2", runner.pushes)
		}
	})
}

// TestIsGitHub5xx proves isGitHub5xx matches the owner's chosen phrases and
// HTTP/error codes (Q2), and does not fire on a bare 502 inside a SHA, a
// branch name, or a request ID, nor on a non-5xx rejection.
func TestIsGitHub5xx(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"internal server error", "remote: Internal Server Error", true},
		{"bad gateway", "remote: Bad Gateway", true},
		{"service unavailable", "remote: Service Unavailable", true},
		{"gateway timeout", "remote: Gateway Timeout", true},
		{"http 500", "error: RPC failed; HTTP 500", true},
		{"http 502", "error: RPC failed; HTTP 502", true},
		{"http 504", "error: RPC failed; HTTP 504", true},
		{"error prefixed 503", "The requested URL returned error: 503", true},
		{"sha holding 502", "fatal: unable to access: a1b2c3d4e5f6502890abcdef1234567890abcdef", false},
		{"branch named 502", "! [rejected] zing/502-fix -> zing/502-fix (non-fast-forward)", false},
		{"request id alone", "remote: Request ID AC8D:502:288832:310C81:6AC67999", false},
		{"stale branch", "! [rejected] zing/9 -> zing/9 (fetch first)", false},
		{"protected branch", "! [remote rejected] zing/9 -> zing/9 (protected branch hook declined)", false},
		{"auth failed", "remote: Authentication failed", false},
		{"http 404", "error: RPC failed; HTTP 404", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := isGitHub5xx(c.out); got != c.want {
				t.Errorf("isGitHub5xx(%q) = %v, want %v", c.out, got, c.want)
			}
		})
	}
}

// TestPushRetryDefaults proves New sets pushBackoff from a clone of
// pushRetryBackoff, and that mutating one Orchestrator's slice never
// touches the shared package variable.
func TestPushRetryDefaults(t *testing.T) {
	t.Parallel()
	o := newTestOrchestrator(t, absLocalPath, execRunner{})

	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	if len(o.pushBackoff) != len(want) {
		t.Fatalf("pushBackoff = %v, want %v", o.pushBackoff, want)
	}
	for i := range want {
		if o.pushBackoff[i] != want[i] {
			t.Errorf("pushBackoff[%d] = %v, want %v", i, o.pushBackoff[i], want[i])
		}
	}

	o.pushBackoff[0] = time.Hour
	if pushRetryBackoff[0] != 30*time.Second {
		t.Errorf("pushRetryBackoff[0] = %v, want unchanged 30s", pushRetryBackoff[0])
	}
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
	t.Parallel()
	t.Run("returns the created url and number", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
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
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
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
		if gh.gotFindBase != mainBranch {
			t.Errorf("FindPRByHead base = %q, want %q (the default branch, PR review finding L)", gh.gotFindBase, mainBranch)
		}
	})

	// PR review finding K: OpenDraftPR used to call Push before rendering
	// and validating pr.Body(), so an empty Title errored only after the
	// remote branch had already been updated. This repo deliberately has no
	// "origin" remote configured: if OpenDraftPR still reached Push before
	// the title check, this test would see a "no such remote" push error
	// instead of the title error it asserts on, and neither GitHub double
	// method would be safe to assume uncalled.
	//
	// This builds the Orchestrator directly through New rather than reusing
	// newTestOrchestratorWithGitHub above (mirroring orchestrator_test.go's
	// newE2EOrchestrator, and its own comment on why): that helper's Runner
	// parameter is called only with execRunner{} from every existing site,
	// and a fourth one here that agrees would trip golangci-lint's unparam
	// finding on it.
	t.Run("an empty title errors before any push", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
		ctx := t.Context()

		proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: repo, DefaultBranch: mainBranch}
		gh := &scriptedPushGitHub{}
		o, err := New(proj, gh, execRunner{}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		wt := prepareSignedCommit(ctx, t, o, 33)

		_, _, err = o.OpenDraftPR(ctx, wt, PullRequest{})
		if err == nil {
			t.Fatal("OpenDraftPR: expected an error for an empty title, got nil")
		}
		if !strings.Contains(err.Error(), "title") {
			t.Errorf("OpenDraftPR error = %q, want it to mention %q", err.Error(), "title")
		}
		if gh.createCalls != 0 {
			t.Errorf("CreateDraftPR called %d times, want 0 (must not reach GitHub for an invalid title)", gh.createCalls)
		}
		if gh.findCalls != 0 {
			t.Errorf("FindPRByHead called %d times, want 0 (must not reach GitHub for an invalid title)", gh.findCalls)
		}
	})

	t.Run("returns the create error when FindPRByHead also fails to find one", func(t *testing.T) {
		t.Parallel()
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
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

// TestPushArgvDisablesHooks proves Push's git calls carry the
// hooks-disabling prefix: a repo-local pre-push hook that writes a marker
// file leaves no marker after a successful Push.
func TestPushArgvDisablesHooks(t *testing.T) {
	t.Parallel()
	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	ctx := t.Context()
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)

	marker := filepath.Join(t.TempDir(), "marker")
	hookPath := filepath.Join(repo, ".git", "hooks", "pre-push")
	writeTestFile(t, hookPath, "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(hookPath, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", hookPath, err)
	}

	o := newTestOrchestrator(t, repo, execRunner{})
	wt := prepareSignedCommit(ctx, t, o, 33)

	if err := o.Push(ctx, wt); err != nil {
		t.Fatalf("Push: %v", err)
	}

	assertMarkerAbsent(t, marker)
}

// TestPushRefreshesBase proves Push fetches the base itself (right after
// revalidate, not buried inside it), and that unpushedShas reads that
// fetched baseRev rather than o.proj.DefaultBranch: origin gets an unsigned
// commit before the ticket branch is even cut, so the cut (and Push's own
// signed-commit check) must see it as already part of the base, not as one
// of the ticket's own unpushed commits. If unpushedShas read local main
// instead -- which never advances past its own push below and so never
// reaches that unsigned commit -- Push would wrongly find it in
// <local main>..<branch> and refuse it as unsigned. A second upstream
// commit after the cut then still advances refs/zing/base/<default> by the
// time Push returns, while the push itself still succeeds and the bare
// remote ends up with only the ticket's own signed commit.
func TestPushRefreshesBase(t *testing.T) {
	t.Parallel()
	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	ctx := t.Context()
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)
	runGit(ctx, t, repo, "push", "-q", "origin", mainBranch)

	unsignedSHA := cloneAndCommitUpstream(ctx, t, remote, "upstream1.txt", "upstream1\n", "add upstream1.txt, unsigned, before the cut")

	o, logs := newTestOrchestratorCapturingLog(t, repo, execRunner{})

	wt := prepareSignedCommit(ctx, t, o, 734)
	ticketSHA := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

	newSHA := cloneAndCommitUpstream(ctx, t, remote, "upstream2.txt", "upstream2\n", "add upstream2.txt, after the cut")

	if err := o.Push(ctx, wt); err != nil {
		t.Fatalf("Push: %v (the unsigned commit %s should already be part of the base, not one of the ticket's own unpushed commits)", err, unsignedSHA)
	}

	gotBase := strings.TrimSpace(runGit(ctx, t, repo, "rev-parse", "refs/zing/base/main"))
	if gotBase != newSHA {
		t.Errorf("refs/zing/base/main = %s, want %s", gotBase, newSHA)
	}

	gotRemote := strings.TrimSpace(runGit(ctx, t, remote, "rev-parse", "refs/heads/"+wt.Branch()))
	if gotRemote != ticketSHA {
		t.Errorf("remote %s = %s, want %s", wt.Branch(), gotRemote, ticketSHA)
	}

	fetched := findRecords(logs.records(t), "fetched base")
	if len(fetched) == 0 {
		t.Fatal("found no \"fetched base\" records")
	}
	last := fetched[len(fetched)-1]
	if got, want := last["ticket_id"], float64(734); got != want {
		t.Errorf("ticket_id = %v, want %v", got, want)
	}
	if last["sha"] != newSHA {
		t.Errorf("sha = %v, want %s", last["sha"], newSHA)
	}
}

// TestPush_PushRunsUnlockedUpstreamRunsLocked proves PR review fix C3: the
// network "git push" itself (no -u) runs without commonMu held, and only
// the upstream calls, "git config --local branch.<b>.remote/.merge" -- the ones that write
// branch.<b>.* in the shared config -- runs with it held. A slow or
// stalled remote must never block every other ticket's shared git writes
// in this repository for the push's own network round trip.
func TestPush_PushRunsUnlockedUpstreamRunsLocked(t *testing.T) {
	t.Parallel()
	fixture := newSigningFixture(t, true)
	repo := newSigningTestRepo(t, fixture)
	ctx := t.Context()
	remote := newBareRemote(ctx, t)
	addOrigin(ctx, t, repo, remote)

	o := newTestOrchestrator(t, repo, execRunner{})
	// Warm resolveCommonMu's cache with a plain Runner first, the same way
	// TestOrchestratorSerializesCommonGitWrites does (commonlock_test.go),
	// before swapping in the recording Runner: otherwise CommonMuHeldForTest's
	// own reentrant call into resolveCommonMu, from inside rec's Run below,
	// would race the first, still-uncached resolution.
	if _, err := o.resolveCommonMu(ctx); err != nil {
		t.Fatalf("resolveCommonMu: %v", err)
	}

	rec := &recordingRunner{o: o}
	o.run = rec

	wt := prepareSignedCommit(ctx, t, o, 90)
	if err := o.Push(ctx, wt); err != nil {
		t.Fatalf("Push: %v", err)
	}

	var sawPush, sawUpstream bool
	for _, ev := range rec.snapshot() {
		switch {
		case len(ev.args) >= 2 && ev.args[0] == "push" && ev.args[1] == "origin":
			sawPush = true
			if ev.locked {
				t.Error("git push ran with commonMu held, want unlocked")
			}
		case len(ev.args) >= 2 && ev.args[0] == "config" && ev.args[1] == "--local":
			sawUpstream = true
			if !ev.locked {
				t.Errorf("git %v ran without commonMu held, want locked", ev.args)
			}
		}
	}
	if !sawPush {
		t.Error("never observed a git push call")
	}
	if !sawUpstream {
		t.Error("never observed a git config --local upstream call")
	}
}
