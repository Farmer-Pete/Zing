package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/response"
)

const (
	signingIdentityEmail = "zing-signing-test@example.com"
	approvedTestFile     = "approved.txt"
	unrelatedTestFile    = "unrelated.txt"
	testCommitTitle      = "Add approved.txt"
	testFuncLine         = "main adds a file, called by nothing, calls nothing"
	testSingleFuncLine   = "a func line"
)

// -----------------------------------------------------------------------
// Pure: CommitMessage.Render
// -----------------------------------------------------------------------

func TestCommitMessageRender(t *testing.T) {
	t.Run("worked example 12.1", func(t *testing.T) {
		m := CommitMessage{
			Title: "Add the perimeter diff",
			FuncLines: []string{
				"Perimeter classifies changed paths, called by Orchestrator.runTask, calls matchPattern",
			},
			Fences: []response.Fence{{
				Path:           "internal/orchestrator/old.go",
				Symbol:         "scanTree",
				ExistedBecause: "existed because the walking skeleton diffed by hand",
			}},
		}

		got, err := m.Render()
		if err != nil {
			t.Fatalf("Render: unexpected error: %v", err)
		}

		want := "Add the perimeter diff\n\n" +
			"Perimeter classifies changed paths, called by Orchestrator.runTask, calls matchPattern\n\n" +
			"Fence: internal/orchestrator/old.go scanTree, existed because the walking skeleton diffed by hand\n\n" +
			"Co-Authored-By: Zing <zing@farmerpete.net>\n"

		if got != want {
			t.Errorf("Render() =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("no fences renders without the fence block", func(t *testing.T) {
		m := CommitMessage{
			Title:     "Fix the worktree exclude",
			FuncLines: []string{"ensureWorktreeExclude appends the line, called by PrepareWorktree"},
		}

		got, err := m.Render()
		if err != nil {
			t.Fatalf("Render: unexpected error: %v", err)
		}

		want := "Fix the worktree exclude\n\n" +
			"ensureWorktreeExclude appends the line, called by PrepareWorktree\n\n" +
			"Co-Authored-By: Zing <zing@farmerpete.net>\n"

		if got != want {
			t.Errorf("Render() =\n%q\nwant\n%q", got, want)
		}
		if strings.Contains(got, "Fence:") {
			t.Errorf("Render() with no fences should not contain a Fence: line, got %q", got)
		}
	})

	t.Run("empty title is an error", func(t *testing.T) {
		m := CommitMessage{FuncLines: []string{testSingleFuncLine}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for an empty title, got nil")
		}
	})

	t.Run("multiline title is an error", func(t *testing.T) {
		m := CommitMessage{Title: "line one\nline two", FuncLines: []string{testSingleFuncLine}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for a multiline title, got nil")
		}
	})

	t.Run("empty func line is an error", func(t *testing.T) {
		m := CommitMessage{Title: "a title", FuncLines: []string{testSingleFuncLine, ""}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for an empty func line, got nil")
		}
	})

	t.Run("multiline func line is an error", func(t *testing.T) {
		m := CommitMessage{Title: "a title", FuncLines: []string{"line one\nline two"}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for a multiline func line, got nil")
		}
	})
}

// -----------------------------------------------------------------------
// Real git: CommitTask and signedStatus
// -----------------------------------------------------------------------

// runGitStdout runs a real git command directly and returns stdout alone,
// mirroring what Runner.Output (and so signedStatus) actually reads. Unlike
// worktree_test.go's runGit, which returns CombinedOutput, this does not
// fold in a stderr warning line -- such as git's "gpg.ssh.allowedSignersFile
// needs to be configured..." notice -- that signedStatus's %G? reading never
// sees either, since it also reads stdout alone.
func runGitStdout(ctx context.Context, t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// gitConfigGlobal runs "git config --global key value" directly (not
// through a Runner, like worktree_test.go's checkRefFormat-style direct
// exec.CommandContext calls), threading ctx like every other git-invoking
// call in this package.
func gitConfigGlobal(ctx context.Context, t *testing.T, key, value string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "git", "config", "--global", key, value).CombinedOutput()
	if err != nil {
		t.Fatalf("git config --global %s %s: %v\n%s", key, value, err, out)
	}
}

// newSigningFixture isolates a fresh global git identity -- a temp HOME plus
// GIT_CONFIG_GLOBAL, so these tests never read or write the developer's own
// git identity or signing key (PKG5-PLAN.md section 11) -- and configures an
// ed25519 SSH signing key: user.name, user.email, commit.gpgsign=true,
// gpg.format=ssh, user.signingKey. When withAllowedSigners is true it also
// writes gpg.ssh.allowedSignersFile for that key, so "%G?" can report "G"
// instead of the "N" the build host reports without it.
func newSigningFixture(t *testing.T, withAllowedSigners bool) {
	t.Helper()
	ctx := t.Context()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig-global"))

	keyPath := filepath.Join(home, "id_ed25519")
	out, err := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "zing-test", "-f", keyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}

	gitConfigGlobal(ctx, t, "user.name", "Zing Signing Test")
	gitConfigGlobal(ctx, t, "user.email", signingIdentityEmail)
	gitConfigGlobal(ctx, t, "commit.gpgsign", "true")
	gitConfigGlobal(ctx, t, "gpg.format", "ssh")
	gitConfigGlobal(ctx, t, "user.signingKey", keyPath)

	if withAllowedSigners {
		pubBytes, readErr := os.ReadFile(keyPath + ".pub")
		if readErr != nil {
			t.Fatalf("read public key: %v", readErr)
		}
		fields := strings.Fields(string(pubBytes))
		if len(fields) < 2 {
			t.Fatalf("unexpected public key format: %q", pubBytes)
		}
		pubLine := fields[0] + " " + fields[1]

		signersFile := filepath.Join(home, "allowed_signers")
		writeTestFile(t, signersFile, signingIdentityEmail+" "+pubLine+"\n")
		gitConfigGlobal(ctx, t, "gpg.ssh.allowedSignersFile", signersFile)
	}
}

// newUnsignedFixture isolates a global git identity with signing explicitly
// disabled and no signing key configured at all, for the
// genuinely-unsigned-commit case: paired with stripDashSRunner, "git commit"
// then produces a plain, unsigned commit.
func newUnsignedFixture(t *testing.T) {
	t.Helper()
	ctx := t.Context()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig-global"))

	gitConfigGlobal(ctx, t, "user.name", "Zing Unsigned Test")
	gitConfigGlobal(ctx, t, "user.email", signingIdentityEmail)
	gitConfigGlobal(ctx, t, "commit.gpgsign", "false")
}

// newSigningTestRepo inits a real git repo with one commit on "main", for
// the CommitTask signing tests. Unlike worktree_test.go's newTestRepo, it
// sets no local user identity or commit.gpgsign: these tests need the
// global signing fixture's identity and config to reach every commit,
// including ones made in a worktree of this repo (a worktree shares its
// main checkout's top-level config).
func newSigningTestRepo(t *testing.T) string {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	runGit(ctx, t, resolved, "init", "-q", "-b", mainBranch)
	writeTestFile(t, filepath.Join(resolved, "README.md"), "# test repo\n")
	runGit(ctx, t, resolved, "add", "README.md")
	runGit(ctx, t, resolved, "commit", "-q", "-m", "initial commit")

	return resolved
}

// stripDashSRunner wraps a real Runner and removes a leading "-S" from any
// "git commit" call's argv, so CommitTask's "git commit -S -F <file>" runs
// as a plain, unsigned commit at the git layer, independent of whatever
// config would otherwise enforce signing. It exists only to exercise
// CommitTask's own signature check and reset path.
type stripDashSRunner struct {
	inner Runner
}

func (r stripDashSRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Run(ctx, dir, name, stripDashS(name, args)...)
}

func (r stripDashSRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Output(ctx, dir, name, args...)
}

func stripDashS(name string, args []string) []string {
	if name != "git" || len(args) == 0 || args[0] != "commit" {
		return args
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a != "-S" {
			out = append(out, a)
		}
	}
	return out
}

func TestCommitTask(t *testing.T) {
	t.Run("with allowed signers: commits, verifies, stages only approved", func(t *testing.T) {
		newSigningFixture(t, true)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 1, "", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "approved content\n")
		writeTestFile(t, filepath.Join(wt.Dir(), unrelatedTestFile), "unrelated content\n")

		msg := CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}

		sha, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg)
		if err != nil {
			t.Fatalf("CommitTask: unexpected error: %v", err)
		}
		if sha == "" {
			t.Fatal("CommitTask: expected a non-empty sha")
		}

		wantSHA := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
		if sha != wantSHA {
			t.Errorf("sha = %q, want %q (HEAD)", sha, wantSHA)
		}

		signed, verified, err := o.signedStatus(ctx, wt.Dir(), "HEAD")
		if err != nil {
			t.Fatalf("signedStatus: unexpected error: %v", err)
		}
		if !signed || !verified {
			t.Errorf("signedStatus = (signed=%v, verified=%v), want (true, true)", signed, verified)
		}

		code := strings.TrimSpace(runGitStdout(ctx, t, wt.Dir(), "show", "--no-patch", "--format=%G?", "HEAD"))
		if code != "G" {
			t.Errorf("%%G? = %q, want %q", code, "G")
		}

		committed := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
		if committed != approvedTestFile {
			t.Errorf("committed paths = %q, want %q", committed, approvedTestFile)
		}

		status := runGit(ctx, t, wt.Dir(), "status", "--porcelain")
		if !strings.Contains(status, unrelatedTestFile) {
			t.Errorf("expected %s to remain dirty and uncommitted, status = %q", unrelatedTestFile, status)
		}
	})

	t.Run("without allowed signers: commits and signs, unverifiable locally", func(t *testing.T) {
		newSigningFixture(t, false)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 2, "", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "approved content\n")

		msg := CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}

		sha, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg)
		if err != nil {
			t.Fatalf("CommitTask: unexpected error: %v", err)
		}
		if sha == "" {
			t.Fatal("CommitTask: expected a non-empty sha")
		}

		signed, verified, err := o.signedStatus(ctx, wt.Dir(), "HEAD")
		if err != nil {
			t.Fatalf("signedStatus: unexpected error: %v", err)
		}
		if !signed {
			t.Error("signedStatus: signed = false, want true (a gpgsig header should be present via the presence fallback)")
		}
		if verified {
			t.Error("signedStatus: verified = true, want false (no allowedSignersFile is configured)")
		}

		code := strings.TrimSpace(runGitStdout(ctx, t, wt.Dir(), "show", "--no-patch", "--format=%G?", "HEAD"))
		if code != "N" {
			t.Errorf("%%G? = %q, want %q (git cannot verify locally without allowedSignersFile)", code, "N")
		}

		raw := runGit(ctx, t, wt.Dir(), "cat-file", "-p", "HEAD")
		if !strings.Contains(raw, "gpgsig ") {
			t.Error("expected a gpgsig header in the raw commit object, proving the commit was signed")
		}
	})

	t.Run("a genuinely unsigned commit resets HEAD and errors", func(t *testing.T) {
		newUnsignedFixture(t)
		repo := newSigningTestRepo(t)
		ctx := t.Context()
		o := newTestOrchestrator(t, repo, stripDashSRunner{inner: execRunner{}})

		wt, err := o.PrepareWorktree(ctx, 3, "", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		priorHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))

		writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "approved content\n")

		msg := CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}

		_, err = o.CommitTask(ctx, wt, []string{approvedTestFile}, msg)
		if err == nil {
			t.Fatal("CommitTask: expected an error for a genuinely unsigned commit, got nil")
		}
		if !strings.Contains(err.Error(), "commit signing failed") {
			t.Errorf("CommitTask error = %q, want it to mention %q", err.Error(), "commit signing failed")
		}

		afterHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
		if afterHead != priorHead {
			t.Errorf("HEAD = %q after a failed signed commit, want it reset back to %q", afterHead, priorHead)
		}

		log := runGit(ctx, t, wt.Dir(), "log", "--oneline")
		if strings.Contains(log, testCommitTitle) {
			t.Error("expected no new commit to remain in the log after the reset")
		}
	})
}
