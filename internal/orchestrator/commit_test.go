package orchestrator

import (
	"context"
	"errors"
	"fmt"
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
	testGenericTitle     = "a title"
	testTwoLineValue     = "line one\nline two"
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
		m := CommitMessage{Title: testTwoLineValue, FuncLines: []string{testSingleFuncLine}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for a multiline title, got nil")
		}
	})

	t.Run("empty func line is an error", func(t *testing.T) {
		m := CommitMessage{Title: testGenericTitle, FuncLines: []string{testSingleFuncLine, ""}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for an empty func line, got nil")
		}
	})

	t.Run("multiline func line is an error", func(t *testing.T) {
		m := CommitMessage{Title: testGenericTitle, FuncLines: []string{testTwoLineValue}}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for a multiline func line, got nil")
		}
	})

	// PR review finding N: Render validated Title and FuncLines but trusted
	// Fences, so a multiline (or empty) Fence field could inject extra
	// commit lines. Fences comes from Package 8's model output, so it must
	// be validated the same way.
	t.Run("a fence with a multiline ExistedBecause is an error", func(t *testing.T) {
		m := CommitMessage{
			Title:     testGenericTitle,
			FuncLines: []string{testSingleFuncLine},
			Fences: []response.Fence{{
				Path:           "a.go",
				Symbol:         "foo",
				ExistedBecause: testTwoLineValue,
			}},
		}
		if _, err := m.Render(); err == nil {
			t.Fatal("Render: expected an error for a fence with a multiline ExistedBecause, got nil")
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

// signingFixture holds the ed25519 SSH signing key material a repo needs to
// sign commits: a key path and, when the test wants local verification, an
// allowed-signers file for that key. It carries no environment variable and
// no git config of its own; newSigningTestRepo is what applies it, and only
// to one repo's local config.
type signingFixture struct {
	keyPath            string
	allowedSignersFile string // empty when withAllowedSigners was false
}

// newSigningFixture generates a fresh ed25519 SSH signing key in its own
// temp directory and, when withAllowedSigners is true, an allowed-signers
// file naming signingIdentityEmail for that key. Unlike the fixture this
// replaces, it never touches HOME, GIT_CONFIG_GLOBAL, or "git config
// --global": ssh-keygen writes to an explicit -f path and git reads
// user.signingKey as an absolute path, so no global git identity is ever
// set. The returned signingFixture is inert until newSigningTestRepo
// applies it to one repo's local config, so a stray git command anywhere
// else -- including in the real repository this test binary happens to run
// inside -- can never pick up a test signing identity.
func newSigningFixture(t *testing.T, withAllowedSigners bool) signingFixture {
	t.Helper()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	out, err := exec.CommandContext(t.Context(), "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "zing-test", "-f", keyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}

	fixture := signingFixture{keyPath: keyPath}
	if !withAllowedSigners {
		return fixture
	}

	pubBytes, readErr := os.ReadFile(keyPath + ".pub")
	if readErr != nil {
		t.Fatalf("read public key: %v", readErr)
	}
	fields := strings.Fields(string(pubBytes))
	if len(fields) < 2 {
		t.Fatalf("unexpected public key format: %q", pubBytes)
	}
	pubLine := fields[0] + " " + fields[1]

	fixture.allowedSignersFile = filepath.Join(dir, "allowed_signers")
	writeTestFile(t, fixture.allowedSignersFile, signingIdentityEmail+" "+pubLine+"\n")

	return fixture
}

// newSigningTestRepo inits a real git repo with one commit on "main" and
// configures fixture's signing key entirely through LOCAL git config:
// user.name, user.email, commit.gpgsign, gpg.format, user.signingKey, and
// -- when fixture carries one -- gpg.ssh.allowedSignersFile. A git worktree
// shares its parent repo's local config (these are all repo-level
// settings), so every worktree PrepareWorktree creates under this repo
// signs and verifies exactly the same way, with no global git state
// involved anywhere.
func newSigningTestRepo(t *testing.T, fixture signingFixture) string {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	runGit(ctx, t, resolved, "init", "-q", "-b", mainBranch)
	runGit(ctx, t, resolved, "config", "user.name", "Zing Signing Test")
	runGit(ctx, t, resolved, "config", "user.email", signingIdentityEmail)
	runGit(ctx, t, resolved, "config", "commit.gpgsign", "true")
	runGit(ctx, t, resolved, "config", "gpg.format", "ssh")
	runGit(ctx, t, resolved, "config", "user.signingKey", fixture.keyPath)
	if fixture.allowedSignersFile != "" {
		runGit(ctx, t, resolved, "config", "gpg.ssh.allowedSignersFile", fixture.allowedSignersFile)
	}

	writeTestFile(t, filepath.Join(resolved, "README.md"), "# test repo\n")
	runGit(ctx, t, resolved, "add", "README.md")
	runGit(ctx, t, resolved, "commit", "-q", "-m", "initial commit")

	return resolved
}

// newUnsignedTestRepo inits a real git repo with signing explicitly
// disabled and, critically, user.signingKey pointing at a path that does
// not exist, entirely through LOCAL git config, for the
// genuinely-unsigned-commit case. commit.gpgsign=false alone is not
// enough: CommitTask's own git commit call always passes "-S", which
// forces an attempt to sign regardless of commit.gpgsign, so this repo
// also points gpg.format and user.signingKey at a key git cannot load --
// "git commit -S" then fails outright, deterministically, on any host,
// including one whose real global git config (unrelated to this repo)
// already has a working signing key configured. Like newSigningTestRepo,
// this never touches global git config.
func newUnsignedTestRepo(t *testing.T) string {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	runGit(ctx, t, resolved, "init", "-q", "-b", mainBranch)
	runGit(ctx, t, resolved, "config", "user.name", "Zing Unsigned Test")
	runGit(ctx, t, resolved, "config", "user.email", signingIdentityEmail)
	runGit(ctx, t, resolved, "config", "commit.gpgsign", "false")
	runGit(ctx, t, resolved, "config", "gpg.format", "ssh")
	runGit(ctx, t, resolved, "config", "user.signingKey", filepath.Join(resolved, "no-such-signing-key"))

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
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
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

	// PR review finding C: CommitTask used to stage only approved with
	// "git add", but then run "git commit -S -F <file>" with no pathspec,
	// which commits the WHOLE index -- so anything a caller (or a prior
	// step) had already staged before CommitTask ran would be swept into
	// the commit too. This proves an unrelated file staged before CommitTask
	// runs is left out of the commit and stays staged afterward.
	t.Run("with allowed signers: an unrelated already-staged file is not included in the commit", func(t *testing.T) {
		fixture := newSigningFixture(t, true)
		repo := newSigningTestRepo(t, fixture)
		ctx := t.Context()
		o := newTestOrchestrator(t, repo, execRunner{})

		wt, err := o.PrepareWorktree(ctx, 4, "", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "approved content\n")
		writeTestFile(t, filepath.Join(wt.Dir(), unrelatedTestFile), "unrelated content\n")
		runGit(ctx, t, wt.Dir(), "add", unrelatedTestFile)

		msg := CommitMessage{Title: testCommitTitle, FuncLines: []string{testFuncLine}}

		if _, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg); err != nil {
			t.Fatalf("CommitTask: unexpected error: %v", err)
		}

		committed := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
		if committed != approvedTestFile {
			t.Errorf("committed paths = %q, want only %q (not the already-staged %q)", committed, approvedTestFile, unrelatedTestFile)
		}

		staged := runGit(ctx, t, wt.Dir(), "diff", "--cached", "--name-only")
		if !strings.Contains(staged, unrelatedTestFile) {
			t.Errorf("expected %s to remain staged after CommitTask, staged = %q", unrelatedTestFile, staged)
		}
	})

	t.Run("without allowed signers: commits and signs, unverifiable locally", func(t *testing.T) {
		fixture := newSigningFixture(t, false)
		repo := newSigningTestRepo(t, fixture)
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
		repo := newUnsignedTestRepo(t)
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

// -----------------------------------------------------------------------
// signedStatus: a scripted Runner drives every %G? code
// -----------------------------------------------------------------------

// commitHeaderWithGpgsig and commitHeaderWithoutGpgsig are "git cat-file -p"
// bodies for signedStatusFallback's table cases: one with a "gpgsig " header
// line before the blank line separating headers from the message, one
// without.
const (
	commitHeaderWithGpgsig = "tree deadbeef\n" +
		"author A <a@example.com> 0 +0000\n" +
		"committer A <a@example.com> 0 +0000\n" +
		"gpgsig -----BEGIN SSH SIGNATURE-----\n" +
		" U1NIU0lHAAAA\n" +
		" -----END SSH SIGNATURE-----\n" +
		"\n" +
		"a commit message\n"
	commitHeaderWithoutGpgsig = "tree deadbeef\n" +
		"author A <a@example.com> 0 +0000\n" +
		"committer A <a@example.com> 0 +0000\n" +
		"\n" +
		"a commit message\n"
)

// scriptedSignedStatusRunner is a fake Runner for signedStatus's table test:
// it returns a fixed "git show --no-patch --format=%G?" code and, for the
// fallback path, a fixed "git cat-file -p" body, so every %G? code
// signedStatus's switch (PR review finding D) can be driven directly rather
// than reproducing each one with a real signing key and host configuration.
type scriptedSignedStatusRunner struct {
	gCode   string
	catFile string
}

func (scriptedSignedStatusRunner) Run(context.Context, string, string, ...string) (string, error) {
	return "", errors.New("scriptedSignedStatusRunner: Run not implemented")
}

func (r scriptedSignedStatusRunner) Output(_ context.Context, _, _ string, args ...string) (string, error) {
	switch {
	case len(args) > 0 && args[0] == "show":
		return r.gCode, nil
	case len(args) > 0 && args[0] == "cat-file":
		return r.catFile, nil
	default:
		return "", fmt.Errorf("scriptedSignedStatusRunner: unexpected Output call: %s", strings.Join(args, " "))
	}
}

// TestSignedStatus drives every %G? code signedStatus's switch handles (PR
// review finding D): "G" is fully verified; "U" is signed but must not be
// over-trusted as verified; "N", "E", "X", and "Y" fall back to the gpgsig
// presence check; "B" and "R" are real signing problems; anything else is an
// error.
func TestSignedStatus(t *testing.T) {
	cases := []struct {
		name         string
		gCode        string
		catFile      string
		wantSigned   bool
		wantVerified bool
		wantErr      bool
	}{
		{name: "G is fully verified", gCode: "G", wantSigned: true, wantVerified: true},
		{name: "U is signed but not verified, not over-trusted", gCode: "U", wantSigned: true, wantVerified: false},
		{name: "N with a gpgsig header falls back to signed, unverified", gCode: "N", catFile: commitHeaderWithGpgsig, wantSigned: true},
		{name: "N without a gpgsig header falls back to genuinely unsigned", gCode: "N", catFile: commitHeaderWithoutGpgsig},
		{name: "E with a gpgsig header falls back to signed, unverified", gCode: "E", catFile: commitHeaderWithGpgsig, wantSigned: true},
		{name: "E without a gpgsig header falls back to genuinely unsigned", gCode: "E", catFile: commitHeaderWithoutGpgsig},
		{name: "X with a gpgsig header falls back to signed, unverified", gCode: "X", catFile: commitHeaderWithGpgsig, wantSigned: true},
		{name: "X without a gpgsig header falls back to genuinely unsigned", gCode: "X", catFile: commitHeaderWithoutGpgsig},
		{name: "Y with a gpgsig header falls back to signed, unverified", gCode: "Y", catFile: commitHeaderWithGpgsig, wantSigned: true},
		{name: "Y without a gpgsig header falls back to genuinely unsigned", gCode: "Y", catFile: commitHeaderWithoutGpgsig},
		{name: "B is a real signing problem", gCode: "B"},
		{name: "R is a real signing problem", gCode: "R"},
		{name: "an unrecognized code is an error", gCode: "Q", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := scriptedSignedStatusRunner{gCode: c.gCode, catFile: c.catFile}
			o := newTestOrchestrator(t, absLocalPath, run)

			signed, verified, err := o.signedStatus(t.Context(), absLocalPath, "HEAD")
			if c.wantErr {
				if err == nil {
					t.Fatal("signedStatus: expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("signedStatus: unexpected error: %v", err)
			}
			if signed != c.wantSigned || verified != c.wantVerified {
				t.Errorf("signedStatus = (signed=%v, verified=%v), want (%v, %v)", signed, verified, c.wantSigned, c.wantVerified)
			}
		})
	}
}

// TestSignedStatusFallback_HeaderScanStopsAtBlankLine proves PR review
// finding A: signedStatusFallback used to scan every line of
// "git cat-file -p", including the commit message body, for a line starting
// "gpgsig ". An unsigned commit whose title happens to start with
// "gpgsig " was then falsely reported signed. The fix scans only the header
// block, stopping at the first blank line, so this genuinely unsigned
// commit is correctly reported unsigned.
func TestSignedStatusFallback_HeaderScanStopsAtBlankLine(t *testing.T) {
	repo := newUnsignedTestRepo(t)
	ctx := t.Context()
	o := newTestOrchestrator(t, repo, execRunner{})

	runGit(ctx, t, repo, "commit", "--allow-empty", "-q", "-m", "gpgsig fake")

	raw := runGit(ctx, t, repo, "cat-file", "-p", "HEAD")
	parts := strings.SplitN(raw, "\n\n", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "gpgsig fake") {
		t.Fatalf("test setup: expected the commit message body to start with \"gpgsig fake\", raw object:\n%s", raw)
	}

	signed, verified, err := o.signedStatusFallback(ctx, repo, "HEAD")
	if err != nil {
		t.Fatalf("signedStatusFallback: unexpected error: %v", err)
	}
	if signed {
		t.Error("signedStatusFallback: signed = true, want false for a genuinely unsigned commit whose message body starts with \"gpgsig \"")
	}
	if verified {
		t.Error("signedStatusFallback: verified = true, want false")
	}
}

// -----------------------------------------------------------------------
// resetAfterUnsignedCommit: the reset survives a cancelled ctx
// -----------------------------------------------------------------------

// cancelAfterCommitRunner wraps a real Runner and cancels cancel right after
// any "git commit" call returns, so the caller's ctx is already done by the
// time CommitTask goes on to verify the signature and, on failure, calls
// resetAfterUnsignedCommit -- exercising PR review finding E's fix, that the
// reset runs under a context detached from ctx rather than ctx itself.
type cancelAfterCommitRunner struct {
	inner  Runner
	cancel context.CancelFunc
}

func (r cancelAfterCommitRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := r.inner.Run(ctx, dir, name, args...)
	if name == "git" && len(args) > 0 && args[0] == "commit" {
		r.cancel()
	}
	return out, err
}

func (r cancelAfterCommitRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Output(ctx, dir, name, args...)
}

// TestResetAfterUnsignedCommit_SurvivesCancelledContext proves PR review
// finding E: resetAfterUnsignedCommit used to run "git reset --soft" on the
// same ctx as the commit it is cleaning up after, so a ctx cancelled (or
// past its deadline) between the commit and the reset would leave an
// unsigned commit sitting at HEAD, since the reset could never run. The
// commit here is forced unsigned (stripDashSRunner) so CommitTask reaches
// its failure path deterministically, and ctx is cancelled the instant the
// "git commit" call returns, before CommitTask's own signature check and
// reset run -- yet HEAD still ends up reset, since the reset now runs on a
// context.WithoutCancel(ctx) detached from ctx's cancellation.
func TestResetAfterUnsignedCommit_SurvivesCancelledContext(t *testing.T) {
	repo := newUnsignedTestRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	run := cancelAfterCommitRunner{inner: stripDashSRunner{inner: execRunner{}}, cancel: cancel}
	o := newTestOrchestrator(t, repo, run)

	wt, err := o.PrepareWorktree(ctx, 5, "", nil)
	if err != nil {
		t.Fatalf("PrepareWorktree: %v", err)
	}

	priorHead := strings.TrimSpace(runGit(t.Context(), t, wt.Dir(), "rev-parse", "HEAD"))

	writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "should never land\n")
	msg := CommitMessage{Title: "Should never land", FuncLines: []string{testFuncLine}}

	if _, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg); err == nil {
		t.Fatal("CommitTask: expected an error for a genuinely unsigned commit, got nil")
	}

	afterHead := strings.TrimSpace(runGit(t.Context(), t, wt.Dir(), "rev-parse", "HEAD"))
	if afterHead != priorHead {
		t.Errorf("HEAD = %q after a failed signed commit under a cancelled ctx, want it reset back to %q", afterHead, priorHead)
	}
}
