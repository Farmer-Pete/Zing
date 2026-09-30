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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	zing "zing"
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
	sb := newLoadedSandbox(t, nil, 7420)
	if !sb.Available() {
		t.Fatalf("sandbox unavailable: %s", sb.Reason())
	}
}

func TestDeniesHomeRead(t *testing.T) {
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
	sb := newLoadedSandbox(t, nil, 7420)
	dirs := newTestDirs(t)
	p := dirs.params()

	env := append(os.Environ(), sb.Env(p, os.Getenv("PATH"))...)
	exitCode, out := runSandboxedWithEnv(t, sb, p, env, "/bin/sh", "-c", "echo $TMPDIR")
	if exitCode != 0 {
		t.Fatalf("echo $TMPDIR: exit %d, want 0 (output %q)", exitCode, out)
	}
	want := filepath.Join(p.RunDir, "tmp")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("child TMPDIR = %q, want %q", got, want)
	}
}

// TestReadPathsAllowsExtra proves a sandbox.read_paths entry inside HOME is
// readable despite HOME's own blanket deny (section 5.4: "It exists for a
// toolchain installed under home").
func TestReadPathsAllowsExtra(t *testing.T) {
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

// TestAllowsTLSDownload proves outbound TLS still works (design section
// 12), skipping when this machine has no route to the public internet
// rather than failing the suite over an environment limitation.
func TestAllowsTLSDownload(t *testing.T) {
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
