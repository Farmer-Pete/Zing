//go:build darwin

// Every test in this file drives the real, checked-in sandbox/build.sb
// through real sandbox-exec (PKG8-PLAN.md section 5.1, task 8). Each test
// builds its own isolated set of paths under t.TempDir() and feeds them to
// the profile as ordinary -D param values -- HOME, WORKTREE, REPO_GIT, and
// so on need not correspond to anything real on this machine, since the
// profile only ever sees the strings Prefix substitutes in -- so no test
// here reads a credential or touches a file in the real home directory
// (Load's own internal proof is the one exception: it resolves the real
// host values just enough to prove the profile loads, and touches nothing).
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	zing "zing"
	"zing/internal/gitbin"
	"zing/internal/gitfixture"
)

// requireNotSandboxed skips t when ZING_SANDBOXED is set: a sandboxed
// process cannot itself start sandbox-exec ("sandbox_apply: Operation not
// permitted", measured on the build host), so a test that runs one must skip
// rather than fail when this test binary is itself already running inside
// the sandbox (design section 5.3; TestDarwinTestsSkipWhenSandboxed in
// sandbox_test.go proves the predicate this calls).
func requireNotSandboxed(t *testing.T) {
	t.Helper()
	if shouldSkipSandboxed() {
		t.Skip("running inside a seatbelt sandbox: cannot start sandbox-exec")
	}
}

// sandboxCommandTimeout bounds every sandboxed command this file runs, so a
// hung child (a denied network call that blocks instead of refusing, say)
// fails the test promptly instead of hanging the suite.
const sandboxCommandTimeout = 20 * time.Second

// newLoadedSandbox reads the real, checked-in sandbox/build.sb through
// zing.Assets and loads it with readPaths and consolePort, failing the test
// if the profile does not load on this machine.
func newLoadedSandbox(t *testing.T, readPaths []string, consolePort int) Sandbox {
	t.Helper()
	requireNotSandboxed(t)

	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read sandbox/build.sb: %v", err)
	}
	sb := Load(profile, t.TempDir(), readPaths, consolePort)
	if !sb.Available() {
		t.Fatalf("Load: unavailable, reason %q", sb.Reason())
	}
	return sb
}

// testDirs is one test's own isolated set of profile-param locations, all
// under one t.TempDir() base, so no two tests can ever collide and nothing
// here reaches a real, shared location on the machine.
type testDirs struct {
	home, worktree, repoGit, dataDir, zingBin, cacheRoot, cacheShared, runDir, mdsCache string
}

// newTestDirs creates every directory testDirs names (all mode 0700) and
// points zingBin at this test binary's own executable, a real, readable,
// executable file that most tests never actually run -- only
// TestRunsZingBinFromDataDir overrides it with a path inside dataDir.
func newTestDirs(t *testing.T) testDirs {
	t.Helper()
	base := t.TempDir()
	// macOS's own /var -> /private/var symlink means t.TempDir()'s path and
	// the path the kernel resolves an open() against differ; seatbelt's own
	// subpath matching operates on the resolved path, so every param this
	// package builds must be symlink-resolved first (design section 5.2's
	// own "symlinks resolved" note on HOME and WORKTREE) or neither an
	// allow nor a deny rule ever matches what a sandboxed child actually
	// touches.
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	d := testDirs{
		home:        filepath.Join(base, "home"),
		worktree:    filepath.Join(base, "worktree"),
		repoGit:     filepath.Join(base, "repo-git"),
		dataDir:     filepath.Join(base, "data"),
		cacheRoot:   filepath.Join(base, "cache"),
		cacheShared: filepath.Join(base, "cache", "shared"),
		runDir:      filepath.Join(base, "cache", "run", "test-run"),
		mdsCache:    filepath.Join(base, "mds"),
	}
	for _, dir := range []string{d.home, d.worktree, d.repoGit, d.dataDir, d.cacheRoot, d.cacheShared, d.runDir, d.mdsCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	zingBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	d.zingBin = zingBin
	return d
}

func (d testDirs) params() Params {
	return Params{
		Home: d.home, Worktree: d.worktree, RepoGit: d.repoGit, DataDir: d.dataDir, ZingBin: d.zingBin,
		CacheRoot: d.cacheRoot, CacheShared: d.cacheShared, RunDir: d.runDir,
		Transcripts: filepath.Join(d.home, ".claude", "projects", "test"),
		MDSCache:    d.mdsCache,
	}
}

// runSandboxed runs args under sb's profile with p's params, returning the
// real exit code (design section 5.4's own CommandRunner-shaped contract:
// err == nil -> exitCode real) and the combined output, for a test's own
// assertions. It fails the test outright only when the command could not
// even start.
func runSandboxed(t *testing.T, sb Sandbox, p Params, args ...string) (exitCode int, output string) {
	t.Helper()
	return runSandboxedWithEnv(t, sb, p, nil, args...)
}

func runSandboxedWithEnv(t *testing.T, sb Sandbox, p Params, env []string, args ...string) (exitCode int, output string) {
	t.Helper()
	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	argv = append(argv, args...)

	ctx, cancel := context.WithTimeout(context.Background(), sandboxCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed test argv built from this test's own temp paths, never external input
	// A nil env still scrubs git's repository-location variables, so a
	// GIT_DIR a git hook exported cannot redirect a sandboxed git.
	cmd.Env = gitfixture.Environ()
	if env != nil {
		cmd.Env = env
	}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) { //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags (matches internal/runtime's own comment)
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("run sandboxed command %v: %v (output: %s)", args, runErr, out)
	return -1, string(out)
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil { //nolint:gosec // 0755: dst must be executable
		t.Fatalf("write %s: %v", dst, err)
	}
}

// acceptAndDiscard accepts and immediately closes every connection ln
// receives until Accept fails (the listener closing), so a test's "allowed"
// connection attempt completes instead of blocking on a peer that never
// accepts.
func acceptAndDiscard(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

// listenLoopback opens a fresh, ephemeral-port TCP listener on 127.0.0.1.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// tcpPort extracts the port addr (a net.Listener's own Addr()) is bound to.
func tcpPort(t *testing.T, addr net.Addr) int {
	t.Helper()
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", addr)
	}
	return tcpAddr.Port
}

// ---- profile load and the two directory-scoped denials ---------------

func TestProfileLoads(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	if !sb.Available() {
		t.Fatalf("sandbox unavailable: %s", sb.Reason())
	}
}

func TestDeniesHomeRead(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	secret := filepath.Join(dirs.home, "secret.txt")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/cat", secret)
	if exitCode == 0 {
		t.Errorf("cat a home file outside the allow list: want a non-zero exit, got 0 (output %q)", out)
	}
}

func TestDeniesDataDir(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	f := filepath.Join(dirs.dataDir, "zing.db")
	if err := os.WriteFile(f, []byte("scenario data"), 0o600); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/cat", f); exitCode == 0 {
		t.Errorf("cat a file in DATA_DIR: want a non-zero exit, got 0 (output %q)", out)
	}

	target := filepath.Join(dirs.dataDir, "new.txt")
	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode == 0 {
		t.Errorf("touch a file in DATA_DIR: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("a file was created in DATA_DIR despite the deny")
	}
}

// TestDeniesDataDirThroughSymlink proves the DATA_DIR deny rule still holds
// when the data directory is reached through a symlinked parent (review
// F044): seatbelt's "(subpath ...)" match runs against the kernel-resolved
// path, so a Sandbox loaded with dataDir behind a symlink must still deny a
// read inside the real directory. Unlike the sibling tests in this file,
// this one drives Load itself (not a hand-built Params), since the fix
// under test lives in resolveHost.
func TestDeniesDataDirThroughSymlink(t *testing.T) {
	t.Parallel()
	requireNotSandboxed(t)

	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "data"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dataDir := filepath.Join(link, "data")
	secret := filepath.Join(realDir, "data", "zing.db")
	if err := os.WriteFile(secret, []byte("scenario data"), 0o600); err != nil {
		t.Fatalf("write %s: %v", secret, err)
	}

	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read sandbox/build.sb: %v", err)
	}
	sb := Load(profile, dataDir, nil, 7420)
	if !sb.Available() {
		t.Fatalf("Load: unavailable, reason %q", sb.Reason())
	}

	dirs := newTestDirs(t)
	p, err := sb.ParamsFor(dirs.worktree, dirs.repoGit, dirs.runDir)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	p.Home = dirs.home
	p.ZingBin = dirs.zingBin
	p.CacheRoot = dirs.cacheRoot
	p.CacheShared = dirs.cacheShared
	p.MDSCache = dirs.mdsCache

	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", secret); exitCode == 0 {
		t.Errorf("cat a file in DATA_DIR reached through a symlinked parent: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestRunsZingBinFromDataDir proves the ZING_BIN literal is readable and
// executable even though its own DATA_DIR sits behind a blanket
// file-read*/file-write* deny (section 5.1's "allow file-read*
// process-exec (literal (param \"ZING_BIN\"))").
func TestRunsZingBinFromDataDir(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)

	zingBin := filepath.Join(dirs.dataDir, "zing-bin")
	copyExecutable(t, "/usr/bin/true", zingBin)

	p := dirs.params()
	p.ZingBin = zingBin

	exitCode, out := runSandboxed(t, sb, p, zingBin)
	if exitCode != 0 {
		t.Errorf("execute ZING_BIN from inside DATA_DIR: exit %d, want 0 (output %q)", exitCode, out)
	}
}

// ---- writes: denied by default, allowed by name -----------------------

func TestDeniesHomeWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	target := filepath.Join(dirs.home, "canary.txt")

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode == 0 {
		t.Errorf("touch a home file: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("canary file exists in HOME despite the deny")
	}
}

func TestDeniesHostTempWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	canary := filepath.Join(os.TempDir(), fmt.Sprintf("zing-sandbox-test-canary-%d.txt", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(canary) })

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", canary); exitCode == 0 {
		t.Errorf("touch a file in the host temp folder: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("canary file exists in the host temp folder despite the deny")
	}
}

func TestDeniesGitPointerWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	gitPointer := filepath.Join(dirs.worktree, ".git")

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", gitPointer); exitCode == 0 {
		t.Errorf("touch <WORKTREE>/.git: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(gitPointer); err == nil {
		t.Error("the .git pointer was created despite the deny")
	}
}

// TestDeniesGitConfigWrite proves REPO_GIT is read-only end to end: it
// carries no write-allow rule at all, so a write anywhere under it,
// including config and a worktree's own config.worktree, is refused by the
// same blanket write deny that covers everywhere outside the allow list.
func TestDeniesGitConfigWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)

	for _, rel := range []string{"config", filepath.Join("worktrees", "abc123", "config.worktree")} {
		target := filepath.Join(dirs.repoGit, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode == 0 {
			t.Errorf("touch %s: want a non-zero exit (REPO_GIT is read-only), got 0 (output %q)", rel, out)
		}
	}
}

func TestAllowsWorktreeWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	target := filepath.Join(dirs.worktree, "hello.txt")

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a worktree file: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("worktree file was not created: %v", err)
	}
}

// TestAllowsWorktreeWriteThroughSymlink proves a worktree reached through a
// symlink is still writable (task 16a's own live-harness defect: the build
// harness's worktree path went through macOS's own /var -> /private/var
// symlink, so the profile's WORKTREE rule, built from the path as given,
// never matched what the kernel resolved a write against). ParamsFor now
// resolves WORKTREE before Prefix ever builds a rule from it, so this test
// fails before that fix and passes after.
func TestAllowsWorktreeWriteThroughSymlink(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)

	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", realDir, err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p, err := sb.ParamsFor(link, dirs.repoGit, dirs.runDir)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	p.Home = dirs.home
	p.DataDir = dirs.dataDir
	p.ZingBin = dirs.zingBin
	p.CacheRoot = dirs.cacheRoot
	p.CacheShared = dirs.cacheShared
	p.MDSCache = dirs.mdsCache

	target := filepath.Join(link, "hello.txt")
	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a worktree file reached through a symlink: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(filepath.Join(realDir, "hello.txt")); err != nil {
		t.Errorf("worktree file was not created at the resolved path: %v", err)
	}
}

func TestAllowsRunDirWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	target := filepath.Join(dirs.runDir, "hello.txt")

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a run-dir file: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("run-dir file was not created: %v", err)
	}
}

// TestChildSeesSandboxTmpdir proves a sandboxed child actually observes the
// TMPDIR value Env computes, not merely that the write-allow rule covers it.
func TestChildSeesSandboxTmpdir(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	p := dirs.params()

	env := append(gitfixture.Environ(), sb.Env(p, os.Getenv("PATH"))...)
	exitCode, out := runSandboxedWithEnv(t, sb, p, env, "/bin/sh", "-c", "echo $TMPDIR")
	if exitCode != 0 {
		t.Fatalf("echo $TMPDIR: exit %d, want 0 (output %q)", exitCode, out)
	}
	want := filepath.Join(p.RunDir, "tmp")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("child TMPDIR = %q, want %q", got, want)
	}
}

// TestChildZshHeredocUnderBuildProfile proves a sandboxed zsh can write its
// heredoc temp file under the run's own TMPPREFIX, and cannot without it
// (section 5.3's /tmp denial, surfaced as zsh's own heredoc temp file,
// default /tmp/zsh, which both profiles deny).
func TestChildZshHeredocUnderBuildProfile(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/zsh"); err != nil {
		t.Skip("/bin/zsh not present on this machine")
	}
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	p := dirs.params()
	if err := os.MkdirAll(filepath.Join(p.RunDir, "tmp"), 0o700); err != nil {
		t.Fatalf("mkdir %s/tmp: %v", p.RunDir, err)
	}

	heredocScript := "cat <<'EOF'\nheredoc-ok\nEOF\n"

	t.Run("with_tmpprefix", func(t *testing.T) {
		t.Parallel()
		env := append(gitfixture.Environ(), sb.Env(p, os.Getenv("PATH"))...)
		exitCode, out := runSandboxedWithEnv(t, sb, p, env, "/bin/zsh", "-f", "-c", heredocScript)
		if exitCode != 0 {
			t.Fatalf("zsh heredoc with TMPPREFIX: exit %d, want 0 (output %q)", exitCode, out)
		}
		if got := strings.TrimSpace(out); got != "heredoc-ok" {
			t.Errorf("zsh heredoc output = %q, want %q", got, "heredoc-ok")
		}
	})

	t.Run("without_tmpprefix", func(t *testing.T) {
		t.Parallel()
		var env []string
		for _, kv := range append(gitfixture.Environ(), sb.Env(p, os.Getenv("PATH"))...) {
			if strings.HasPrefix(kv, "TMPPREFIX=") {
				continue
			}
			env = append(env, kv)
		}
		exitCode, out := runSandboxedWithEnv(t, sb, p, env, "/bin/zsh", "-f", "-c", heredocScript)
		if exitCode == 0 {
			t.Errorf("zsh heredoc without TMPPREFIX: want a non-zero exit (the /tmp denial), got 0 (output %q)", out)
		}
	})
}

// TestReadPathsAllowsExtra proves a sandbox.read_paths entry inside HOME is
// readable despite HOME's own blanket deny (section 5.4: "It exists for a
// toolchain installed under home").
func TestReadPathsAllowsExtra(t *testing.T) {
	t.Parallel()
	dirs := newTestDirs(t)
	extra := filepath.Join(dirs.home, ".local", "share", "mise")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", extra, err)
	}
	marker := filepath.Join(extra, "marker.txt")
	if err := os.WriteFile(marker, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write %s: %v", marker, err)
	}

	sb := newLoadedSandbox(t, []string{extra}, 7420)
	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/cat", marker); exitCode != 0 {
		t.Errorf("cat a read_paths entry inside HOME: exit %d, want 0 (output %q)", exitCode, out)
	}
}

// ---- the console port -------------------------------------------------

func TestDeniesConsolePort(t *testing.T) {
	t.Parallel()
	ln := listenLoopback(t)
	defer func() { _ = ln.Close() }()
	go acceptAndDiscard(ln)
	port := tcpPort(t, ln.Addr())

	sb := newLoadedSandbox(t, nil, port)
	dirs := newTestDirs(t)

	for _, host := range []string{"127.0.0.1", "localhost"} {
		exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/nc", "-z", "-w", "2", host, strconv.Itoa(port))
		if exitCode == 0 {
			t.Errorf("nc to %s:%d (the configured console port): want a non-zero exit, got 0 (output %q)", host, port, out)
		}
	}
}

func TestAllowsOtherLocalPort(t *testing.T) {
	t.Parallel()
	blocked := listenLoopback(t)
	defer func() { _ = blocked.Close() }()
	other := listenLoopback(t)
	defer func() { _ = other.Close() }()
	go acceptAndDiscard(blocked)
	go acceptAndDiscard(other)

	blockedPort := tcpPort(t, blocked.Addr())
	otherPort := tcpPort(t, other.Addr())

	sb := newLoadedSandbox(t, nil, blockedPort)
	dirs := newTestDirs(t)

	exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/nc", "-z", "-w", "2", "127.0.0.1", strconv.Itoa(otherPort))
	if exitCode != 0 {
		t.Errorf("nc to the non-configured port %d: exit %d, want 0 (output %q)", otherPort, exitCode, out)
	}
}

// ---- no hand-off to an unsandboxed process -----------------------------

func TestDeniesOpen(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary.txt")
	script := filepath.Join(dir, "run.command")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+canary+"\n"), 0o755); err != nil { //nolint:gosec // 0755: must be executable
		t.Fatalf("write %s: %v", script, err)
	}

	exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/open", script)
	if exitCode == 0 {
		t.Errorf("open run.command under the profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("canary file exists: open handed the script to an unsandboxed process")
	}
}

func TestDeniesOsascriptToApplication(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary.txt")
	script := fmt.Sprintf(`tell application "Terminal" to do script "touch %s"`, canary)

	exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/osascript", "-e", script)
	if exitCode == 0 {
		t.Errorf("osascript telling Terminal under the profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("canary file exists: osascript reached Terminal")
	}
}

func TestDeniesLaunchctlSubmit(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary.txt")
	label := fmt.Sprintf("com.zing.sandboxtest.%d", os.Getpid())
	t.Cleanup(func() {
		// In case the submit somehow got through, remove the job label so no
		// stray launchd job survives this test (boundary rule).
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "launchctl", "remove", label).Run() //nolint:gosec,errcheck // G204: label is this test's own fixed, PID-derived string; the result is a best-effort cleanup
	})

	exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/launchctl", "submit", "-l", label, "--", "/usr/bin/touch", canary)
	if exitCode == 0 {
		t.Errorf("launchctl submit under the profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("canary file exists: launchctl submit ran the job")
	}
}

// ---- the readonly profile (PKG9-PLAN.md section 4.7, 7.3) -----------------

// newLoadedReadonlySandbox reads the real, checked-in sandbox/readonly.sb
// through zing.Assets and loads it with no extra read paths, on port 7421
// (distinct from newLoadedSandbox's 7420, so a test using both at once
// never collides), failing the test if the profile does not load on this
// machine.
func newLoadedReadonlySandbox(t *testing.T) Sandbox {
	t.Helper()
	requireNotSandboxed(t)

	profile, err := zing.Assets.ReadFile("sandbox/readonly.sb")
	if err != nil {
		t.Fatalf("read sandbox/readonly.sb: %v", err)
	}
	sb := LoadProfile(profileNameReadOnly, profile, t.TempDir(), nil, 7421)
	if !sb.Available() {
		t.Fatalf("LoadProfile(readonly): unavailable, reason %q", sb.Reason())
	}
	return sb
}

func TestReadonlyProfileLoads(t *testing.T) {
	t.Parallel()
	sb := newLoadedReadonlySandbox(t)
	if !sb.Available() {
		t.Fatalf("sandbox unavailable: %s", sb.Reason())
	}
}

// TestReadonlyDeniesWorktreeWrite proves the readonly profile's write block
// carries no WORKTREE allow at all (section 7.3: "A review or respond run
// writes nothing in the worktree, D4"), unlike build.sb.
func TestReadonlyDeniesWorktreeWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedReadonlySandbox(t)
	dirs := newTestDirs(t)
	target := filepath.Join(dirs.worktree, "hello.txt")

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode == 0 {
		t.Errorf("touch a worktree file under the readonly profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("worktree file was created despite the readonly profile's deny")
	}
}

// TestReadonlyAllowsRunDirWrite proves the readonly profile still allows a
// write into RUN_DIR (section 7.3's own write block).
func TestReadonlyAllowsRunDirWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedReadonlySandbox(t)
	dirs := newTestDirs(t)
	target := filepath.Join(dirs.runDir, "hello.txt")

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a run-dir file under the readonly profile: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("run-dir file was not created: %v", err)
	}
}

// TestReadonlyAllowsTranscriptWrite proves the readonly profile allows a
// write into TRANSCRIPTS (section 7.3's own write block: the Claude CLI
// must still be able to write its own transcript).
func TestReadonlyAllowsTranscriptWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedReadonlySandbox(t)
	dirs := newTestDirs(t)
	p := dirs.params()
	if err := os.MkdirAll(p.Transcripts, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", p.Transcripts, err)
	}
	target := filepath.Join(p.Transcripts, "hello.txt")

	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a transcripts file under the readonly profile: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("transcripts file was not created: %v", err)
	}
}

// TestReadonlyDeniesDataDir proves the readonly profile denies DATA_DIR
// whole, the same as build.sb (section 7.3: "No profile reads zing.db").
func TestReadonlyDeniesDataDir(t *testing.T) {
	t.Parallel()
	sb := newLoadedReadonlySandbox(t)
	dirs := newTestDirs(t)
	f := filepath.Join(dirs.dataDir, "zing.db")
	if err := os.WriteFile(f, []byte("scenario data"), 0o600); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/cat", f); exitCode == 0 {
		t.Errorf("cat a file in DATA_DIR under the readonly profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestReadonlyDeniesKeychainRead proves the readonly profile no longer
// allows ~/Library/Keychains (PKG9-PLAN.md D26, N2): the Claude CLI logs in
// with CLAUDE_CODE_OAUTH_TOKEN instead.
func TestReadonlyDeniesKeychainRead(t *testing.T) {
	t.Parallel()
	sb := newLoadedReadonlySandbox(t)
	dirs := newTestDirs(t)
	keychainFile := filepath.Join(dirs.home, "Library", "Keychains", "login.keychain-db")
	if err := os.MkdirAll(filepath.Dir(keychainFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(keychainFile, []byte("not a real keychain"), 0o600); err != nil {
		t.Fatalf("write %s: %v", keychainFile, err)
	}

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/cat", keychainFile); exitCode == 0 {
		t.Errorf("cat ~/Library/Keychains/... under the readonly profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestBuildDeniesKeychainRead proves build.sb also no longer allows
// ~/Library/Keychains (PKG9-PLAN.md D26, N2), mirroring
// TestReadonlyDeniesKeychainRead for the build profile.
func TestBuildDeniesKeychainRead(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	keychainFile := filepath.Join(dirs.home, "Library", "Keychains", "login.keychain-db")
	if err := os.MkdirAll(filepath.Dir(keychainFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(keychainFile, []byte("not a real keychain"), 0o600); err != nil {
		t.Fatalf("write %s: %v", keychainFile, err)
	}

	if exitCode, out := runSandboxed(t, sb, dirs.params(), "/bin/cat", keychainFile); exitCode == 0 {
		t.Errorf("cat ~/Library/Keychains/... under the build profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestBuildDeniesGitCredentialHelperExec proves build.sb's process-exec
// deny on git-credential-* helpers (D26, N2): a fake helper script that
// would otherwise print a password is refused before it can ever run.
func TestBuildDeniesGitCredentialHelperExec(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)

	helperDir := t.TempDir()
	helper := filepath.Join(helperDir, "git-credential-fake")
	script := "#!/bin/sh\necho password=stolen\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil { //nolint:gosec // 0755: must be executable
		t.Fatalf("write %s: %v", helper, err)
	}

	exitCode, out := runSandboxed(t, sb, dirs.params(), helper)
	if exitCode == 0 {
		t.Errorf("exec a git-credential-* helper under the profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if strings.Contains(out, "password=") {
		t.Errorf("the denied helper's output leaked a password= line: %q", out)
	}
}

// TestAllowsTLSDownload proves outbound TLS still works (design section
// 12), skipping when this machine has no route to the public internet
// rather than failing the suite over an environment limitation.
func TestAllowsTLSDownload(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)

	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", "proxy.golang.org:443")
	if err != nil {
		t.Skipf("no network reachable (%v); skipping", err)
	}
	_ = conn.Close()

	exitCode, out := runSandboxed(t, sb, dirs.params(), "/usr/bin/curl", "-fsS", "--max-time", "10", "-o", os.DevNull, "https://proxy.golang.org")
	if exitCode != 0 {
		t.Errorf("curl https://proxy.golang.org under the profile: exit %d, want 0 (output %q)", exitCode, out)
	}
}

// ---- the judge profile (PKG9-PLAN.md section 4.7, 7.3, D19, D27; M2 task 2) ----

// newLoadedJudgeSandbox reads the real, checked-in sandbox/judge.sb through
// zing.Assets and loads it with no extra read paths, on port 7422 (distinct
// from newLoadedSandbox's 7420 and newLoadedReadonlySandbox's 7421, so a
// test using more than one profile at once never collides), failing the
// test if the profile does not load on this machine.
func newLoadedJudgeSandbox(t *testing.T) Sandbox {
	t.Helper()
	requireNotSandboxed(t)

	profile, err := zing.Assets.ReadFile("sandbox/judge.sb")
	if err != nil {
		t.Fatalf("read sandbox/judge.sb: %v", err)
	}
	sb := LoadProfile(profileNameJudge, profile, t.TempDir(), nil, 7422)
	if !sb.Available() {
		t.Fatalf("LoadProfile(judge): unavailable, reason %q", sb.Reason())
	}
	return sb
}

// judgeParams returns dirs' own Params with the judge profile's two extra
// fields filled (section 4.7): a scenarios file under DATA_DIR, at the
// same "judge/<run_id>/scenarios.xml" shape a real run uses (section 7.3),
// so a test actually exercises the literal carve-out against DATA_DIR's
// own blanket deny -- RUN_DIR itself sits under CACHE_ROOT, which build.sb
// already allows reading whole, so a scenarios file placed there would
// prove nothing about the literal rule. CodexHome is a directory distinct
// from both, so a CODEX_HOME write test and a RUN_DIR write test can never
// be confused for each other.
func (d testDirs) judgeParams(t *testing.T) Params {
	t.Helper()
	p := d.params()
	scenariosDir := filepath.Join(d.dataDir, "judge", "1")
	if err := os.MkdirAll(scenariosDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", scenariosDir, err)
	}
	p.ScenariosFile = filepath.Join(scenariosDir, "scenarios.xml")
	if err := os.WriteFile(p.ScenariosFile, []byte("<scenarios/>\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p.ScenariosFile, err)
	}
	p.CodexHome = filepath.Join(filepath.Dir(d.runDir), "codex-home")
	if err := os.MkdirAll(p.CodexHome, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", p.CodexHome, err)
	}
	return p
}

func TestJudgeProfileLoads(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	if !sb.Available() {
		t.Fatalf("sandbox unavailable: %s", sb.Reason())
	}
}

// TestJudgeAllowsCodexHomeWrite proves the judge profile allows a write
// under its own CODEX_HOME parameter (section 7.3: "the CODEX_HOME write
// allow sits after the global write deny"), while the same write under the
// real ~/.codex stays denied, the same as build.sb.
func TestJudgeAllowsCodexHomeWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	target := filepath.Join(p.CodexHome, "auth.json")
	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a CODEX_HOME file under the judge profile: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("CODEX_HOME file was not created: %v", err)
	}

	realCodexHome := filepath.Join(p.Home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(realCodexHome), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/touch", realCodexHome); exitCode == 0 {
		t.Errorf("touch a file under the real ~/.codex under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeCodexHomeRuleAfterDeny proves the rendered judge profile's
// CODEX_HOME write allow line comes after "(deny file-write* (subpath
// \"/\"))", the ordering section 7.3 requires for it to win (seatbelt:
// later rules win).
func TestJudgeCodexHomeRuleAfterDeny(t *testing.T) {
	t.Parallel()
	profile, err := zing.Assets.ReadFile("sandbox/judge.sb")
	if err != nil {
		t.Fatalf("read sandbox/judge.sb: %v", err)
	}
	text := string(profile)

	denyIdx := strings.Index(text, `(deny file-write* (subpath "/"))`)
	// LastIndex, not Index: judge.sb also allows reading CODEX_HOME (section
	// 7.3's read block, before the write block this test cares about), so
	// the write-allow's own CODEX_HOME line is the later of the two.
	allowIdx := strings.LastIndex(text, `(subpath (param "CODEX_HOME"))`)
	if denyIdx == -1 {
		t.Fatal(`judge.sb does not carry (deny file-write* (subpath "/"))`)
	}
	if allowIdx == -1 {
		t.Fatal(`judge.sb does not carry a (subpath (param "CODEX_HOME")) line`)
	}
	if allowIdx < denyIdx {
		t.Errorf("the CODEX_HOME write allow (offset %d) comes before the global write deny (offset %d), want after", allowIdx, denyIdx)
	}
}

// TestJudgeAllowsScenariosFileRead proves the judge profile's literal
// SCENARIOS_FILE allow reaches the one file it names and nothing else in
// the same directory (section 4.7, D19).
func TestJudgeAllowsScenariosFileRead(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", p.ScenariosFile); exitCode != 0 {
		t.Fatalf("cat SCENARIOS_FILE under the judge profile: exit %d, want 0 (output %q)", exitCode, out)
	}

	sibling := filepath.Join(filepath.Dir(p.ScenariosFile), "other.xml")
	if err := os.WriteFile(sibling, []byte("not the scenarios file\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", sibling, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", sibling); exitCode == 0 {
		t.Errorf("cat a sibling file next to SCENARIOS_FILE: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeScenariosFileMatchesThroughDataDirSymlink proves
// cmd/zing/serve.go's own data-directory resolution formula -- raw :=
// filepath.Dir(dbPath); dataDir, err := filepath.EvalSymlinks(raw), the
// one resolved value then threaded into both the judge sandbox profile and
// every judge run's own scenarios file (PKG9-PLAN.md section 19.3 task 9).
// A live TestLiveJudge run first found this: LoadProfile already resolves
// DATA_DIR's own symlinks internally (resolveHost, review F044), but a
// scenarios file path built from an unresolved, symlink-reached data
// directory never matched the kernel-resolved path the judge profile's
// literal SCENARIOS_FILE rule compares against, so a real judge's own
// `zing scenarios` failed with "operation not permitted" against a file
// that genuinely existed. This reproduces serve's own formula starting
// from a dbPath reached through a symlink to the data directory, loads the
// judge profile with the resolved result (exactly as serveSandbox does),
// and proves a scenarios file built from that same resolved value is
// readable under it.
func TestJudgeScenariosFileMatchesThroughDataDirSymlink(t *testing.T) {
	t.Parallel()
	requireNotSandboxed(t)

	dirs := newTestDirs(t)
	link := filepath.Join(filepath.Dir(dirs.dataDir), "data-link")
	if err := os.Symlink(dirs.dataDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	dbPath := filepath.Join(link, "zing.db")
	resolvedDataDir, err := filepath.EvalSymlinks(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	if resolvedDataDir != dirs.dataDir {
		t.Fatalf("resolved data dir = %s, want %s (newTestDirs' own, already resolved)", resolvedDataDir, dirs.dataDir)
	}

	profile, err := zing.Assets.ReadFile("sandbox/judge.sb")
	if err != nil {
		t.Fatalf("read sandbox/judge.sb: %v", err)
	}
	sb := LoadProfile(profileNameJudge, profile, resolvedDataDir, nil, 7424)
	if !sb.Available() {
		t.Fatalf("LoadProfile(judge): unavailable, reason %q", sb.Reason())
	}

	p := dirs.judgeParams(t)
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", p.ScenariosFile); exitCode != 0 {
		t.Fatalf("cat SCENARIOS_FILE, loaded through a data dir reached via a symlink: exit %d, want 0 (output %q)", exitCode, out)
	}
}

// TestJudgeDeniesDatabase proves the judge profile denies zing.db under
// DATA_DIR whole, the same as build.sb and readonly.sb (section 4.7, N6:
// "No profile reads zing.db").
func TestJudgeDeniesDatabase(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	f := filepath.Join(dirs.dataDir, "zing.db")
	if err := os.WriteFile(f, []byte("scenario data"), 0o600); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", f); exitCode == 0 {
		t.Errorf("cat zing.db under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeDeniesClaudeJSON proves the judge profile denies
// ~/.claude.json, using a temp HOME rather than the owner's real one: a
// live probe (TestProbeJudgeDeniesClaudeAndCodexState) found this file
// readable despite judge.sb's own deny, because that deny named only
// file-read*, which does not out-order the file-read-data allow build.sb's
// shared block carries for the same path -- seatbelt does not apply
// "later wins" between a wildcard op and the specific op a competing rule
// names; only a rule naming the same op can out-order another. The fix
// names file-read-data on the judge's own deny too.
func TestJudgeDeniesClaudeJSON(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	claudeJSON := filepath.Join(p.Home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{"marker":"not for the judge"}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", claudeJSON, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", claudeJSON); exitCode == 0 {
		t.Errorf("cat ~/.claude.json under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeDeniesZingToml proves the judge profile denies zing.toml the
// same way (section 4.7: "Nothing else in DATA_DIR is readable: ... not
// zing.toml (the GitHub token)").
func TestJudgeDeniesZingToml(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	f := filepath.Join(dirs.dataDir, "zing.toml")
	if err := os.WriteFile(f, []byte("github_token = \"secret\"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", f); exitCode == 0 {
		t.Errorf("cat zing.toml under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeCanonicalizesCodexHomeInsideDataDir is a regression test for a
// live judge run: production requires judge_codex_home inside DATA_DIR, and
// Codex canonicalizes CODEX_HOME at startup, which lstats DATA_DIR itself.
// The DATA_DIR deny covered that folder, so Codex exited 1 ("failed to
// canonicalize CODEX_HOME"). judgeParams puts CodexHome outside DATA_DIR,
// which is why the other judge tests missed it. The fix allows only the
// folder's metadata: listing it stays denied.
func TestJudgeCanonicalizesCodexHomeInsideDataDir(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)
	p.CodexHome = filepath.Join(dirs.dataDir, "codex-judge")
	if err := os.MkdirAll(p.CodexHome, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", p.CodexHome, err)
	}

	if exitCode, out := runSandboxed(t, sb, p, "/bin/realpath", p.CodexHome); exitCode != 0 {
		t.Errorf("realpath CODEX_HOME inside DATA_DIR under the judge profile: exit %d, want 0 (output %q)", exitCode, out)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/ls", dirs.dataDir); exitCode == 0 {
		t.Errorf("list DATA_DIR under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// ---- denying outbound git transports (#49) -----------------------------

// gitPushListenerPort is git's own well-known port for the git:// protocol,
// and the fixed (not ephemeral) port TestBuildDeniesGitPush and
// TestJudgeDeniesGitPush bind their stand-in remote to: the push under
// test must dial this exact port for the profile's own deny to be what is
// under test. Neither test calls t.Parallel, since both bind it.
const gitPushListenerPort = 9418

// countingAccepts accepts and discards every connection ln receives,
// incrementing *accepts for each, until Accept fails (the listener
// closing). Run it in a goroutine the way acceptAndDiscard's callers do;
// unlike acceptAndDiscard, this also counts, so a test can assert a denied
// connect never reached the stand-in remote at all.
func countingAccepts(ln net.Listener, accepts *int64) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		atomic.AddInt64(accepts, 1)
		_ = conn.Close()
	}
}

// waitForAccept dials network/addr itself, from this test process (never
// through the sandbox), and blocks, with a deadline, until *accepts has
// counted that connection, then returns the total. countingAccepts' own Accept
// loop drains its listener's backlog strictly in the order connections
// arrived, so by the time this control connection's own accept is
// counted, every connection a sandboxed command made earlier is already
// counted too, whether that command's own connect was denied or not
// (review r1f5): reading *accepts right after a sandboxed command returns
// can otherwise race countingAccepts' own goroutine, in either direction
// -- a denied connect's non-count might not be observed yet, and an
// allowed connect's own count might not be either.
func waitForAccept(t *testing.T, network, addr string, accepts *int64) int64 {
	t.Helper()
	before := atomic.LoadInt64(accepts)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		t.Fatalf("waitForAccept: control dial: %v", err)
	}
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := atomic.LoadInt64(accepts); got > before {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForAccept: accept count never advanced past %d", before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wantOperationNotPermitted is the text every denied connect or exec this
// file's git-push probes assert on: seatbelt's own refusal text for both a
// network-outbound deny and a process-exec deny (judge.sb's own file-deny
// rules already quote it the same way).
const wantOperationNotPermitted = "Operation not permitted"

// testDeniesGitPush is TestBuildDeniesGitPush's and TestJudgeDeniesGitPush's
// shared body (#49): a signed git repository under worktree and a stand-in
// git:// remote listening on gitPushListenerPort, then three outbound
// attempts the profile must refuse. It is not safe to call with
// t.Parallel, since it binds that fixed port.
func testDeniesGitPush(t *testing.T, sb Sandbox, p Params, worktree string) {
	t.Helper()

	if err := gitfixture.NewSigningRepo(t.Context(), worktree); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", gitPushListenerPort))
	if err != nil {
		t.Fatalf("listen 127.0.0.1:%d: %v", gitPushListenerPort, err)
	}
	defer func() { _ = ln.Close() }()
	var accepts int64
	go countingAccepts(ln, &accepts)

	// SSH_AUTH_SOCK is dropped, not merely left unset by gitfixture.Environ():
	// this test's own parent process may run under a real agent, and the
	// push under test must not be able to reach it.
	env := slices.DeleteFunc(gitfixture.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "SSH_AUTH_SOCK=")
	})
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND=ssh -F /dev/null -o BatchMode=yes -o ConnectTimeout=2 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentityAgent=none",
	)

	exitCode, out := runSandboxedWithEnv(t, sb, p, env, gitbin.Path(), "-C", worktree, "push", "git://127.0.0.1:9418/x.git", "HEAD")
	if exitCode == 0 {
		t.Errorf("git push git://127.0.0.1:9418/x.git: want a non-zero exit, got 0 (output %q)", out)
	}
	if !strings.Contains(out, wantOperationNotPermitted) {
		t.Errorf("git push git:// output = %q, want it to contain %q", out, wantOperationNotPermitted)
	}
	if got := waitForAccept(t, "tcp", fmt.Sprintf("127.0.0.1:%d", gitPushListenerPort), &accepts); got != 1 {
		t.Errorf("git:// listener accepted %d connections (including this test's own control probe), want 1: the sandboxed push must never have reached it", got)
	}

	exitCode, out = runSandboxedWithEnv(t, sb, p, env, gitbin.Path(), "-C", worktree, "push", "ssh://git@127.0.0.1:22/x.git", "HEAD")
	if exitCode == 0 {
		t.Errorf("git push ssh://git@127.0.0.1:22/x.git: want a non-zero exit, got 0 (output %q)", out)
	}
	if !strings.Contains(out, wantOperationNotPermitted) {
		t.Errorf("git push ssh:// output = %q, want it to contain %q", out, wantOperationNotPermitted)
	}

	exitCode, out = runSandboxedWithEnv(t, sb, p, env, "/usr/bin/nc", "-v", "-z", "-w", "2", "127.0.0.1", "22")
	if exitCode == 0 {
		t.Errorf("nc -z 127.0.0.1 22: want a non-zero exit, got 0 (output %q)", out)
	}
	if !strings.Contains(out, wantOperationNotPermitted) {
		t.Errorf("nc -z 127.0.0.1 22 output = %q, want it to contain %q", out, wantOperationNotPermitted)
	}

	// Exec-only probes (review r4f1): the ssh:// push above dials
	// 127.0.0.1:22, so it would still fail the same way with the
	// process-exec deny removed -- ssh's own connect would still hit the
	// port 22 deny. These two run ssh with no network target at all
	// (-V just prints a version and exits), so only the exec deny can
	// explain a refusal.
	exitCode, out = runSandboxedWithEnv(t, sb, p, env, "/usr/bin/ssh", "-V")
	if exitCode == 0 {
		t.Errorf("exec /usr/bin/ssh -V: want a non-zero exit, got 0 (output %q)", out)
	}
	if !strings.Contains(out, wantOperationNotPermitted) {
		t.Errorf("exec /usr/bin/ssh -V output = %q, want it to contain %q", out, wantOperationNotPermitted)
	}

	// A copy of ssh earlier in PATH (a Homebrew install, say) must be
	// denied the same way: the rule matches any path ending in "/ssh",
	// not just the literal /usr/bin/ssh.
	altSSHDir := filepath.Join(p.RunDir, "bin")
	if err := os.MkdirAll(altSSHDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", altSSHDir, err)
	}
	altSSH := filepath.Join(altSSHDir, "ssh")
	copyExecutable(t, "/usr/bin/ssh", altSSH)
	exitCode, out = runSandboxedWithEnv(t, sb, p, env, altSSH, "-V")
	if exitCode == 0 {
		t.Errorf("exec %s -V: want a non-zero exit, got 0 (output %q)", altSSH, out)
	}
	if !strings.Contains(out, wantOperationNotPermitted) {
		t.Errorf("exec %s -V output = %q, want it to contain %q", altSSH, out, wantOperationNotPermitted)
	}

	// Positive control: an executable in the same directory, under a name
	// that doesn't end in "/ssh", must still run, showing the two denials
	// above are what block the exec and not some other profile or binary
	// reason. This is a plain shell script rather than a renamed copy of
	// ssh's own bytes: the kernel kills a copy of that signed system
	// binary on exec from an unexpected path regardless of the sandbox
	// profile (observed on the build host as exit -1 with no output, the
	// signal-killed shape, not "Operation not permitted"), which would
	// fail this control for a reason that has nothing to do with the rule
	// under test.
	notSSH := filepath.Join(altSSHDir, "ssh2")
	if err := os.WriteFile(notSSH, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // 0755: must be executable
		t.Fatalf("write %s: %v", notSSH, err)
	}
	exitCode, out = runSandboxedWithEnv(t, sb, p, env, notSSH, "-V")
	if exitCode != 0 {
		t.Errorf("exec %s -V (not named ssh): exit %d, want 0 (output %q)", notSSH, exitCode, out)
	}

	// Positive control (review r2f2): an unrelated, allowed port must
	// still be reachable from inside the same sandbox, Params, and env.
	// Without this, a zero accept count or a non-zero exit on 9418 or 22
	// above could just as well mean git or nc never reached the network
	// stack at all, for some unrelated profile or binary reason, and the
	// port-22/9418 denies would never have been exercised.
	controlLn := listenLoopback(t)
	defer func() { _ = controlLn.Close() }()
	controlPort := tcpPort(t, controlLn.Addr())
	var controlAccepts int64
	go countingAccepts(controlLn, &controlAccepts)

	_, _ = runSandboxedWithEnv(t, sb, p, env, gitbin.Path(), "-C", worktree, "push", fmt.Sprintf("git://127.0.0.1:%d/x.git", controlPort), "HEAD")
	if got := waitForAccept(t, "tcp", fmt.Sprintf("127.0.0.1:%d", controlPort), &controlAccepts); got != 2 {
		t.Errorf("control listener accepted %d connections (including this test's own control probe), want 2: the sandboxed push to an allowed port must have reached it", got)
	}

	exitCode, out = runSandboxedWithEnv(t, sb, p, env, "/usr/bin/nc", "-v", "-z", "-w", "2", "127.0.0.1", strconv.Itoa(controlPort))
	if exitCode != 0 {
		t.Errorf("nc -z 127.0.0.1 %d (an allowed port): exit %d, want 0 (output %q)", controlPort, exitCode, out)
	}
}

// TestBuildDeniesGitPush is #49's own regression test under build.sb: run
// outside the sandbox (requireNotSandboxed, through newLoadedSandbox), it
// reproduces the pattern that pushed a real branch to GitHub -- a git push
// over ssh:// or git://, plus a bare TCP connect to port 22 -- and proves
// build.sb's new rules refuse every one of them.
func TestBuildDeniesGitPush(t *testing.T) {
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	testDeniesGitPush(t, sb, dirs.params(), dirs.worktree)
}

// TestJudgeDeniesGitPush is TestBuildDeniesGitPush's own counterpart under
// judge.sb, the profile #49 actually ran inside.
func TestJudgeDeniesGitPush(t *testing.T) {
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	testDeniesGitPush(t, sb, dirs.judgeParams(t), dirs.worktree)
}

// ---- denying a custom ssh-agent socket (#49) ---------------------------

// listenUnix opens a unix-socket listener at path through
// net.ListenConfig (noctx: a bare net.Listen is disallowed).
func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	return ln
}

// testDeniesAgentSocket is TestBuildDeniesAgentSocket's and
// TestJudgeDeniesAgentSocket's shared body: two listeners standing in for
// an ssh-agent, one reached directly (SSHAuthSockReal) and the other
// reached through a symlink (SSHAuthSock), plus a sibling socket the deny
// must not touch. Past the main case with both params set, it also tries
// SSH_AUTH_SOCK alone, dialing the exact path the rule names, so each rule
// has a case that fails without it (review r1f4). It does not try aliasing
// the socket through an unnamed path (a different symlink, or a
// non-canonical spelling); that belongs to the exploratory
// testAgentSocketAliasing below, kept out of this function and out of the
// plan's own demo loop because it rests on an assumption the plan does not
// make: that seatbelt's path-literal match runs against the resolved vnode
// rather than a connect's own sun_path (review r3f1). The socket directory
// is created directly under /tmp, not under t.TempDir()'s own deeper base, to stay
// well under a unix socket's 104-byte sun_path limit, and is also used as
// p.RunDir so a write-gated false failure (the socket directory itself
// being outside every write-allow) can never be mistaken for the network
// deny this test is actually about.
func testDeniesAgentSocket(t *testing.T, sb Sandbox, p Params) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "zsa") //nolint:usetesting // a unix socket's sun_path is 104 bytes; t.TempDir() nests too deep under macOS's own temp root
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if resolved, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
		dir = resolved
	}
	p.RunDir = dir

	agentSock := filepath.Join(dir, "agent.sock")
	otherSock := filepath.Join(dir, "other.sock")
	linkSock := filepath.Join(dir, "link.sock")

	agentLn := listenUnix(t, agentSock)
	defer func() { _ = agentLn.Close() }()
	var agentAccepts int64
	go countingAccepts(agentLn, &agentAccepts)

	otherLn := listenUnix(t, otherSock)
	defer func() { _ = otherLn.Close() }()
	var otherAccepts int64
	go countingAccepts(otherLn, &otherAccepts)

	if symlinkErr := os.Symlink(agentSock, linkSock); symlinkErr != nil {
		t.Fatalf("symlink: %v", symlinkErr)
	}

	// wantAgentTotal is agentLn's own running total, advanced by one with
	// every denyAgentSock call below (review r1f5): a deny case that lets
	// a connect through would add one of its own, on top of this
	// function's own control connections.
	wantAgentTotal := int64(0)
	denyAgentSock := func(path, label string) {
		if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/nc", "-U", "-w", "2", path); exitCode == 0 {
			t.Errorf("nc -U %s (%s): want a non-zero exit, got 0 (output %q)", path, label, out)
		}
		wantAgentTotal++
		if got := waitForAccept(t, "unix", agentSock, &agentAccepts); got != wantAgentTotal {
			t.Errorf("agent socket listener accepted %d connections (including this function's own control probes) after %s, want %d: the sandboxed connect must never have reached it", got, label, wantAgentTotal)
		}
	}

	// Both fields set, the way a real run's ParamsFor carries them: the
	// symlink and the real path must both be denied.
	p.SSHAuthSock = linkSock
	p.SSHAuthSockReal = agentSock
	denyAgentSock(linkSock, "both params set, dialing the symlink")
	denyAgentSock(agentSock, "both params set, dialing the real path")

	// Only SSH_AUTH_SOCK names the socket; SSH_AUTH_SOCK_REAL is left at
	// Prefix's own sentinel. The pair above never shows this rule matters
	// on its own (review r1f4).
	p.SSHAuthSock = agentSock
	p.SSHAuthSockReal = ""
	denyAgentSock(agentSock, "only SSH_AUTH_SOCK set")

	// Restore both params, the way a real run carries them.
	p.SSHAuthSock = linkSock
	p.SSHAuthSockReal = agentSock

	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/nc", "-U", "-w", "2", otherSock); exitCode != 0 {
		t.Errorf("nc -U %s (a socket the profile does not name): exit %d, want 0 (output %q)", otherSock, exitCode, out)
	}
	if got := waitForAccept(t, "unix", otherSock, &otherAccepts); got != 2 {
		t.Errorf("other socket listener accepted %d connections (including this test's own control probe), want 2: the sandboxed connect must have reached it once", got)
	}
}

// TestBuildDeniesAgentSocket proves build.sb denies a connect to the
// socket the parent's own SSH_AUTH_SOCK names, both through the path as
// given and through its symlink-resolved path, while leaving an unrelated
// socket reachable (#49's own agent-socket gap).
func TestBuildDeniesAgentSocket(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	testDeniesAgentSocket(t, sb, dirs.params())
}

// TestJudgeDeniesAgentSocket is TestBuildDeniesAgentSocket's own
// counterpart under judge.sb.
func TestJudgeDeniesAgentSocket(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	testDeniesAgentSocket(t, sb, dirs.judgeParams(t))
}

// testAgentSocketAliasing is exploratory, not part of the plan's own demo
// loop (`Test(Build|Judge)Denies(GitPush|AgentSocket)`): both its cases
// assume seatbelt's path-literal unix-socket match runs against the
// resolved vnode rather than a connect's own sun_path, an assumption the
// plan is explicit it does not know holds (review r3f1). If the kernel
// matches sun_path instead, both cases below fail on their own, without
// taking the plan's required TestBuildDeniesAgentSocket or
// TestJudgeDeniesAgentSocket -- or its demo loop -- down with them. Kept
// here as a standing probe of that assumption rather than deleted, so a
// kernel change that starts allowing an alias is caught by name.
func testAgentSocketAliasing(t *testing.T, sb Sandbox, p Params) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "zsa") //nolint:usetesting // a unix socket's sun_path is 104 bytes; t.TempDir() nests too deep under macOS's own temp root
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if resolved, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
		dir = resolved
	}
	p.RunDir = dir

	agentSock := filepath.Join(dir, "agent.sock")
	linkSock := filepath.Join(dir, "link.sock")

	agentLn := listenUnix(t, agentSock)
	defer func() { _ = agentLn.Close() }()
	var agentAccepts int64
	go countingAccepts(agentLn, &agentAccepts)

	if symlinkErr := os.Symlink(agentSock, linkSock); symlinkErr != nil {
		t.Fatalf("symlink: %v", symlinkErr)
	}

	wantAgentTotal := int64(0)

	// Only SSH_AUTH_SOCK_REAL names the socket; SSH_AUTH_SOCK is left at
	// Prefix's own sentinel. Dialing through linkSock, a symlink the
	// resolved-path rule does not name literally, shows whether the deny
	// reaches an alias of the real path and not just the real path itself
	// (review r1f4).
	p.SSHAuthSock = ""
	p.SSHAuthSockReal = agentSock
	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/nc", "-U", "-w", "2", linkSock); exitCode == 0 {
		t.Errorf("nc -U %s (only SSH_AUTH_SOCK_REAL set, dialing a symlink to it): want a non-zero exit, got 0 (output %q)", linkSock, out)
	}
	wantAgentTotal++
	if got := waitForAccept(t, "unix", agentSock, &agentAccepts); got != wantAgentTotal {
		t.Errorf("agent socket listener accepted %d connections after dialing a symlink with only SSH_AUTH_SOCK_REAL set, want %d", got, wantAgentTotal)
	}

	// Restore both params, the way a real run carries them.
	p.SSHAuthSock = linkSock
	p.SSHAuthSockReal = agentSock

	// A symlink the sandboxed process itself creates, under its own
	// writable RunDir, pointing straight at the real agent socket: if
	// path-literal matched a connect's own sun_path rather than the
	// resolved vnode, this alias would reach the agent neither named path
	// denies (review r1f6).
	aliasSock := filepath.Join(dir, "alias.sock")
	aliasCmd := fmt.Sprintf("ln -s %s %s && exec /usr/bin/nc -U -w 2 %s", agentSock, aliasSock, aliasSock)
	if exitCode, out := runSandboxed(t, sb, p, "/bin/sh", "-c", aliasCmd); exitCode == 0 {
		t.Errorf("nc -U a sandbox-created alias symlink to the denied agent socket: want a non-zero exit, got 0 (output %q)", out)
	}
	wantAgentTotal++
	if got := waitForAccept(t, "unix", agentSock, &agentAccepts); got != wantAgentTotal {
		t.Errorf("agent socket listener accepted %d connections after the alias-symlink case, want %d: path-literal must match the resolved vnode, not a connect's own sun_path", got, wantAgentTotal)
	}

	// A non-canonical spelling of the same path (an extra "/./" element):
	// if path-literal matched only the exact byte string, this would also
	// slip past the deny despite naming the identical vnode (review
	// r1f6).
	noncanonical := dir + "/./agent.sock"
	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/nc", "-U", "-w", "2", noncanonical); exitCode == 0 {
		t.Errorf("nc -U %s (a non-canonical spelling of the denied agent socket): want a non-zero exit, got 0 (output %q)", noncanonical, out)
	}
	wantAgentTotal++
	if got := waitForAccept(t, "unix", agentSock, &agentAccepts); got != wantAgentTotal {
		t.Errorf("agent socket listener accepted %d connections after dialing a non-canonical spelling of the denied path, want %d", got, wantAgentTotal)
	}
}

// TestBuildAgentSocketDenyMatchesResolvedPath is testAgentSocketAliasing
// under build.sb. Not named to match the plan's demo loop regex, so it is
// not one of the four probes the plan's design counts on (review r3f1).
func TestBuildAgentSocketDenyMatchesResolvedPath(t *testing.T) {
	t.Parallel()
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	testAgentSocketAliasing(t, sb, dirs.params())
}

// TestJudgeAgentSocketDenyMatchesResolvedPath is
// TestBuildAgentSocketDenyMatchesResolvedPath's own counterpart under
// judge.sb.
func TestJudgeAgentSocketDenyMatchesResolvedPath(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeSandbox(t)
	dirs := newTestDirs(t)
	testAgentSocketAliasing(t, sb, dirs.judgeParams(t))
}

// captureSandboxAgentSocketLog swaps slog's default logger for a
// debug-level text handler writing into the returned buffer, restoring the
// previous default on t's cleanup. LoadProfile's own "sandbox agent
// socket" debug record (review r1f1) is the only thing any caller here
// reads back out of it.
func captureSandboxAgentSocketLog(t *testing.T) *strings.Builder {
	t.Helper()
	prev := slog.Default()
	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// assertSandboxAgentSocketLog asserts logBuf holds exactly one "sandbox
// agent socket" record, naming profile "build" and the given branch, and
// that it contains none of secrets (the values resolveHost derived from
// SSH_AUTH_SOCK in the subtest that built logBuf): LoadProfile's own
// comment promises the socket path never appears in a log line (review
// r1f1), and nothing before this change actually read the record back to
// check it.
func assertSandboxAgentSocketLog(t *testing.T, logBuf *strings.Builder, branch string, secrets ...string) {
	t.Helper()
	logged := logBuf.String()
	if n := strings.Count(logged, "sandbox agent socket"); n != 1 {
		t.Errorf("log contains %d %q records, want 1 (log: %s)", n, "sandbox agent socket", logged)
	}
	if !strings.Contains(logged, "profile=build") {
		t.Errorf("log = %q, want it to contain %q", logged, "profile=build")
	}
	if !strings.Contains(logged, "branch="+branch) {
		t.Errorf("log = %q, want it to contain %q", logged, "branch="+branch)
	}
	for _, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Errorf("log = %q, must not contain %q", logged, secret)
		}
	}
}

// TestParamsForCarriesAgentSocket proves resolveHost's own SSH_AUTH_SOCK
// resolution, carried through LoadProfile and ParamsFor, for each of
// resolveHost's three non-empty branches (#49). Not parallel: it calls
// t.Setenv.
func TestParamsForCarriesAgentSocket(t *testing.T) {
	requireNotSandboxed(t)

	t.Run("resolved", func(t *testing.T) {
		logBuf := captureSandboxAgentSocketLog(t)
		dir, mkdirErr := os.MkdirTemp("/tmp", "zsa") //nolint:usetesting // a unix socket's sun_path is 104 bytes; t.TempDir() nests too deep under macOS's own temp root
		if mkdirErr != nil {
			t.Fatalf("MkdirTemp: %v", mkdirErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if resolved, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
			dir = resolved
		}

		agentSock := filepath.Join(dir, "agent.sock")
		ln := listenUnix(t, agentSock)
		t.Cleanup(func() { _ = ln.Close() })

		linkSock := filepath.Join(dir, "link.sock")
		if symlinkErr := os.Symlink(agentSock, linkSock); symlinkErr != nil {
			t.Fatalf("symlink: %v", symlinkErr)
		}

		t.Setenv("SSH_AUTH_SOCK", linkSock)
		sb := newLoadedSandbox(t, nil, 7420)
		dirs := newTestDirs(t)
		p, err := sb.ParamsFor(dirs.worktree, dirs.repoGit, dirs.runDir)
		if err != nil {
			t.Fatalf("ParamsFor: %v", err)
		}
		if p.SSHAuthSock != linkSock {
			t.Errorf("SSHAuthSock = %q, want %q", p.SSHAuthSock, linkSock)
		}
		if p.SSHAuthSockReal != agentSock {
			t.Errorf("SSHAuthSockReal = %q, want %q", p.SSHAuthSockReal, agentSock)
		}
		if sb.host.agentSockBranch != "resolved" {
			t.Errorf("agentSockBranch = %q, want %q", sb.host.agentSockBranch, "resolved")
		}
		assertSandboxAgentSocketLog(t, logBuf, "resolved", linkSock, agentSock)
	})

	// A relative SSH_AUTH_SOCK fails the whole load closed, rather than
	// being silently carried as if it were unset: the resolveHost error
	// surfaces the same way any of its other failures do, before
	// LoadProfile ever reaches its "sandbox agent socket" debug record,
	// so no log capture is needed here.
	t.Run("relative", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "relative/agent.sock")
		profile, err := zing.Assets.ReadFile("sandbox/build.sb")
		if err != nil {
			t.Fatalf("read sandbox/build.sb: %v", err)
		}
		sb := Load(profile, t.TempDir(), nil, 7420)
		if sb.Available() {
			t.Fatal("Load: want a relative SSH_AUTH_SOCK to make the sandbox unavailable, got available")
		}
		if sb.Reason() != reasonUserCacheDirNotFound {
			t.Errorf("Reason() = %q, want %q", sb.Reason(), reasonUserCacheDirNotFound)
		}
	})

	// A socket that has not been created yet -- common for a
	// socket-activated or not-yet-started agent -- still canonicalizes
	// through its deepest existing ancestor directory: a temp dir under
	// /tmp, which macOS itself symlinks to /private/tmp, stands in for
	// that ancestor, so SSHAuthSockReal must reflect the resolved
	// ancestor rather than the raw /tmp-prefixed path.
	t.Run("unresolved leaf, symlinked ancestor", func(t *testing.T) {
		logBuf := captureSandboxAgentSocketLog(t)
		dir, mkdirErr := os.MkdirTemp("/tmp", "zsa") //nolint:usetesting // see the "resolved" subtest above: needs a real /tmp ancestor, not t.TempDir()'s own root
		if mkdirErr != nil {
			t.Fatalf("MkdirTemp: %v", mkdirErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		resolvedDir, evalErr := filepath.EvalSymlinks(dir)
		if evalErr != nil {
			t.Fatalf("EvalSymlinks(%q): %v", dir, evalErr)
		}
		if resolvedDir == dir {
			t.Skipf("%q does not sit under a symlinked ancestor on this machine", dir)
		}

		missing := filepath.Join(dir, "does-not-exist.sock")
		wantReal := filepath.Join(resolvedDir, "does-not-exist.sock")
		t.Setenv("SSH_AUTH_SOCK", missing)
		sb := newLoadedSandbox(t, nil, 7420)
		dirs := newTestDirs(t)
		p, err := sb.ParamsFor(dirs.worktree, dirs.repoGit, dirs.runDir)
		if err != nil {
			t.Fatalf("ParamsFor: %v", err)
		}
		if p.SSHAuthSock != missing {
			t.Errorf("SSHAuthSock = %q, want %q", p.SSHAuthSock, missing)
		}
		if p.SSHAuthSockReal != wantReal {
			t.Errorf("SSHAuthSockReal = %q, want %q (the canonicalized ancestor), not the raw path", p.SSHAuthSockReal, wantReal)
		}
		if sb.host.agentSockBranch != "resolved" {
			t.Errorf("agentSockBranch = %q, want %q", sb.host.agentSockBranch, "resolved")
		}
		assertSandboxAgentSocketLog(t, logBuf, "resolved", missing, wantReal)
	})
}

// ---- the judge-claude profile (plan 105, task 3) -----------------------

// newLoadedJudgeClaudeSandbox reads the real, checked-in
// sandbox/judge-claude.sb through zing.Assets and loads it with no extra
// read paths, on port 7425 (distinct from every other newLoaded*Sandbox
// helper's own port in this file, so a test using more than one profile at
// once never collides), failing the test if the profile does not load on
// this machine.
func newLoadedJudgeClaudeSandbox(t *testing.T) Sandbox {
	t.Helper()
	requireNotSandboxed(t)

	profile, err := zing.Assets.ReadFile("sandbox/judge-claude.sb")
	if err != nil {
		t.Fatalf("read sandbox/judge-claude.sb: %v", err)
	}
	sb := LoadProfile(profileNameJudgeClaude, profile, t.TempDir(), nil, 7425)
	if !sb.Available() {
		t.Fatalf("LoadProfile(judge-claude): unavailable, reason %q", sb.Reason())
	}
	return sb
}

func TestJudgeClaudeProfileLoads(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	if !sb.Available() {
		t.Fatalf("sandbox unavailable: %s", sb.Reason())
	}
}

// TestJudgeClaudeAllowsScenariosFileRead proves the judge-claude profile's
// literal SCENARIOS_FILE allow reaches the one file it names and nothing
// else in the same directory, the same as judge.sb (owner decision Q1: the
// scenarios file stays at DATA_DIR/judge/RUN/scenarios.xml).
func TestJudgeClaudeAllowsScenariosFileRead(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", p.ScenariosFile); exitCode != 0 {
		t.Fatalf("cat SCENARIOS_FILE under the judge-claude profile: exit %d, want 0 (output %q)", exitCode, out)
	}

	sibling := filepath.Join(filepath.Dir(p.ScenariosFile), "other.xml")
	if err := os.WriteFile(sibling, []byte("not the scenarios file\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", sibling, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", sibling); exitCode == 0 {
		t.Errorf("cat a sibling file next to SCENARIOS_FILE: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeClaudeDeniesDatabase proves the judge-claude profile denies
// zing.db under DATA_DIR whole, the same as judge.sb.
func TestJudgeClaudeDeniesDatabase(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	f := filepath.Join(dirs.dataDir, "zing.db")
	if err := os.WriteFile(f, []byte("scenario data"), 0o600); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", f); exitCode == 0 {
		t.Errorf("cat zing.db under the judge-claude profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeClaudeAllowsClaudeStateRead proves the judge-claude profile
// allows the Claude CLI's own reads under ~/.claude and ~/.claude.json,
// build.sb's own carve-out, in place of judge.sb's CODEX_HOME reads.
func TestJudgeClaudeAllowsClaudeStateRead(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	claudeJSON := filepath.Join(p.Home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{"marker":"claude state"}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", claudeJSON, err)
	}
	settings := filepath.Join(p.Home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(settings), err)
	}
	if err := os.WriteFile(settings, []byte(`{"marker":"claude settings"}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", settings, err)
	}

	for _, f := range []string{claudeJSON, settings} {
		if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", f); exitCode != 0 {
			t.Errorf("cat %s under the judge-claude profile: exit %d, want 0 (output %q)", f, exitCode, out)
		}
	}
}

// TestJudgeClaudeAllowsTranscriptWrite proves the judge-claude profile
// allows a write into TRANSCRIPTS, the run's own transcript folder: the
// Claude CLI must still be able to write its own transcript.
func TestJudgeClaudeAllowsTranscriptWrite(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)
	if err := os.MkdirAll(p.Transcripts, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", p.Transcripts, err)
	}
	target := filepath.Join(p.Transcripts, "x.jsonl")

	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/touch", target); exitCode != 0 {
		t.Fatalf("touch a transcripts file under the judge-claude profile: exit %d, want 0 (output %q)", exitCode, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("transcripts file was not created: %v", err)
	}
}

// TestJudgeClaudeDeniesOtherTranscripts proves the judge-claude profile
// denies a transcript folder other than TRANSCRIPTS itself (N6: no other
// session's transcript), the same as judge.sb's own deny-then-allow pair
// around ~/.claude/projects.
func TestJudgeClaudeDeniesOtherTranscripts(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	other := filepath.Join(p.Home, ".claude", "projects", "other-session", "t.jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(other), err)
	}
	if err := os.WriteFile(other, []byte("not this run's transcript\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", other, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", other); exitCode == 0 {
		t.Errorf("cat another session's transcript under the judge-claude profile: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestJudgeClaudeDeniesCodexState proves the judge-claude profile still
// denies ~/.codex (the ~/.codex deny stays, design section "changes"),
// even though this profile has no CODEX_HOME param at all. The blanket
// file-read-data deny on all of HOME already fails a plain cat here, so
// that alone would pass even without the ~/.codex rule; /usr/bin/stat only
// needs file-read* metadata access, which the HOME-wide deny does not
// cover, so it isolates the ~/.codex rule's own extra reach (review r1f4).
func TestJudgeClaudeDeniesCodexState(t *testing.T) {
	t.Parallel()
	sb := newLoadedJudgeClaudeSandbox(t)
	dirs := newTestDirs(t)
	p := dirs.judgeParams(t)

	codexAuth := filepath.Join(p.Home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(codexAuth), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(codexAuth), err)
	}
	if err := os.WriteFile(codexAuth, []byte(`{"marker":"not for claude"}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", codexAuth, err)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/bin/cat", codexAuth); exitCode == 0 {
		t.Errorf("cat ~/.codex/auth.json under the judge-claude profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if exitCode, out := runSandboxed(t, sb, p, "/usr/bin/stat", codexAuth); exitCode == 0 {
		t.Errorf("stat ~/.codex/auth.json under the judge-claude profile: want a non-zero exit, got 0 (output %q)", out)
	}
}
