package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	zing "zing"
)

// ---- Prefix -----------------------------------------------------------

// testMinimalRenderedProfile is a fixed, valid-enough profile string every
// Prefix test below that does not care about the profile's own text builds
// its Sandbox from.
const testMinimalRenderedProfile = "(version 1)\n"

// testHomeDir is the fixed HOME every Params/Host literal in this file that
// does not need a real, resolvable directory builds from (goconst: reused
// by testParams and the ParamsFor tests below).
const testHomeDir = "/Users/test/home"

// testReadPathEntry is the read_paths example every renderReadPaths and
// renderProfile test in this file reuses (goconst).
const testReadPathEntry = "/opt/homebrew/bin"

// testGOOSDarwin is the GOOS value every Load test in this file compares
// runtime.GOOS against, in its own not-macOS branch (goconst).
const testGOOSDarwin = "darwin"

// testSandboxExecArgv0 is argv[0] every Prefix test in this file expects
// (goconst).
const testSandboxExecArgv0 = "sandbox-exec"

// testParams is a Params literal every absolute-field test in this file
// starts from and overrides one field of, so a test failure names exactly
// the field it changed.
func testParams() Params {
	return Params{
		Home:        testHomeDir,
		Worktree:    "/Users/test/wt",
		RepoGit:     "/Users/test/repo/.git",
		DataDir:     "/Users/test/data",
		ZingBin:     "/Users/test/bin/zing",
		CacheRoot:   "/Users/test/cache",
		CacheShared: "/Users/test/cache/shared",
		RunDir:      "/Users/test/cache/run/abc123",
		Transcripts: "/Users/test/home/.claude/projects/wt",
		MDSCache:    "/Users/test/cache/mds",
	}
}

// TestPrefixOrder proves Prefix emits the ten -D flags in section 5.2's
// table order, followed by -p <profile>, with "sandbox-exec" as argv[0].
func TestPrefixOrder(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile}
	p := testParams()

	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}

	want := []string{
		testSandboxExecArgv0,
		"-D", "HOME=" + p.Home,
		"-D", "WORKTREE=" + p.Worktree,
		"-D", "REPO_GIT=" + p.RepoGit,
		"-D", "DATA_DIR=" + p.DataDir,
		"-D", "ZING_BIN=" + p.ZingBin,
		"-D", "CACHE_ROOT=" + p.CacheRoot,
		"-D", "CACHE_SHARED=" + p.CacheShared,
		"-D", "RUN_DIR=" + p.RunDir,
		"-D", "MDS_CACHE=" + p.MDSCache,
		"-D", "TRANSCRIPTS=" + p.Transcripts,
		"-D", "SSH_AUTH_SOCK=" + noAgentSocket,
		"-D", "SSH_AUTH_SOCK_REAL=" + noAgentSocket,
		"-p", sb.renderedProfile,
	}
	if !slices.Equal(argv, want) {
		t.Errorf("Prefix() =\n%v\nwant\n%v", argv, want)
	}
}

// TestPrefixRejectsUnsafeParam proves an unsafe param value (here, one
// containing a double quote) is refused with the exact error text section
// 5.2 gives, naming the offending param.
func TestPrefixRejectsUnsafeParam(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile}
	p := testParams()
	p.RepoGit = `/Users/test/repo"; rm -rf /`

	_, err := sb.Prefix(p)
	if err == nil {
		t.Fatal("Prefix: want an error for an unsafe REPO_GIT value, got nil")
	}
	want := "sandbox: param REPO_GIT has an unsafe value"
	if err.Error() != want {
		t.Errorf("Prefix error = %q, want %q", err.Error(), want)
	}
}

// TestPrefixRejectsRelativeParam proves a relative param value is refused
// the same way (every param value must be absolute, section 5.2).
func TestPrefixRejectsRelativeParam(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile}
	p := testParams()
	p.Worktree = "relative/path"

	_, err := sb.Prefix(p)
	if err == nil {
		t.Fatal("Prefix: want an error for a relative WORKTREE value, got nil")
	}
	want := "sandbox: param WORKTREE has an unsafe value"
	if err.Error() != want {
		t.Errorf("Prefix error = %q, want %q", err.Error(), want)
	}
}

// ---- the agent-socket params (#49) -------------------------------------

// TestPrefixEmitsAgentSocketParams proves Prefix emits "-D
// SSH_AUTH_SOCK=..." and "-D SSH_AUTH_SOCK_REAL=..." right after TRANSCRIPTS
// when both fields are set, and that an unsafe value is refused by name
// only, with the rejected value never appearing in the error text.
func TestPrefixEmitsAgentSocketParams(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile}
	p := testParams()
	p.SSHAuthSock = "/Users/test/agent-raw.sock"
	p.SSHAuthSockReal = "/Users/test/agent-real.sock"

	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	want := []string{
		testSandboxExecArgv0,
		"-D", "HOME=" + p.Home,
		"-D", "WORKTREE=" + p.Worktree,
		"-D", "REPO_GIT=" + p.RepoGit,
		"-D", "DATA_DIR=" + p.DataDir,
		"-D", "ZING_BIN=" + p.ZingBin,
		"-D", "CACHE_ROOT=" + p.CacheRoot,
		"-D", "CACHE_SHARED=" + p.CacheShared,
		"-D", "RUN_DIR=" + p.RunDir,
		"-D", "MDS_CACHE=" + p.MDSCache,
		"-D", "TRANSCRIPTS=" + p.Transcripts,
		"-D", "SSH_AUTH_SOCK=" + p.SSHAuthSock,
		"-D", "SSH_AUTH_SOCK_REAL=" + p.SSHAuthSockReal,
		"-p", sb.renderedProfile,
	}
	if !slices.Equal(argv, want) {
		t.Errorf("Prefix() =\n%v\nwant\n%v", argv, want)
	}

	unsafe := `/Users/test/agent"; rm -rf /`
	p.SSHAuthSock = unsafe
	_, err = sb.Prefix(p)
	wantErr := "sandbox: param SSH_AUTH_SOCK has an unsafe value"
	if err == nil || err.Error() != wantErr {
		t.Errorf("Prefix() err = %v, want %q", err, wantErr)
	}
	if err != nil && strings.Contains(err.Error(), unsafe) {
		t.Errorf("Prefix() err = %q, must not contain the rejected value", err.Error())
	}
}

// TestPrefixAgentSocketSentinel proves Prefix substitutes noAgentSocket for
// both SSH_AUTH_SOCK and SSH_AUTH_SOCK_REAL when Params carries neither
// (section: every profile's deny rule always names a real param).
func TestPrefixAgentSocketSentinel(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile}
	p := testParams()

	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	for _, flag := range []string{"SSH_AUTH_SOCK=" + noAgentSocket, "SSH_AUTH_SOCK_REAL=" + noAgentSocket} {
		if !slices.Contains(argv, flag) {
			t.Errorf("Prefix() = %v, want it to contain %q", argv, flag)
		}
	}
}

// ---- profile rendering -------------------------------------------------

// TestRenderReadPaths proves the ;;READ_PATHS;; substitution: one
// allow-file-read-data line per entry, and an empty string for an empty
// list (section 5.1).
func TestRenderReadPaths(t *testing.T) {
	t.Parallel()
	if got := renderReadPaths(nil); got != "" {
		t.Errorf("renderReadPaths(nil) = %q, want empty", got)
	}

	got := renderReadPaths([]string{testReadPathEntry, "/Users/test/.local/share/mise"})
	want := "(allow file-read-data (subpath \"/opt/homebrew/bin\"))\n" +
		"(allow file-read-data (subpath \"/Users/test/.local/share/mise\"))"
	if got != want {
		t.Errorf("renderReadPaths(...) =\n%s\nwant\n%s", got, want)
	}
}

// TestRenderConsoleDeny proves the ;;CONSOLE_DENY;; substitution: the
// fixed-host deny rule with the decimal port, and an error for a port
// outside 1-65535 (section 5.1).
func TestRenderConsoleDeny(t *testing.T) {
	t.Parallel()
	got, err := renderConsoleDeny(7420)
	if err != nil {
		t.Fatalf("renderConsoleDeny(7420): %v", err)
	}
	want := `(deny network-outbound (remote tcp "*:7420"))`
	if got != want {
		t.Errorf("renderConsoleDeny(7420) = %q, want %q", got, want)
	}

	for _, bad := range []int{0, -1, 65536, 100000} {
		if _, err := renderConsoleDeny(bad); err == nil {
			t.Errorf("renderConsoleDeny(%d): want an error, got nil", bad)
		}
	}
}

// TestLoadRejectsBadPort proves Load records the sandbox unavailable with
// reason "profile rejected" when consolePort is out of range, on every
// platform (section 5.1: "The port is always rendered; a port outside 1 to
// 65535 makes Load record the sandbox as unavailable with the reason
// 'profile rejected'").
func TestLoadRejectsBadPort(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != testGOOSDarwin {
		sb := Load([]byte("(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n"), t.TempDir(), nil, 0)
		if sb.Available() {
			t.Fatal("Load with a bad port: want unavailable")
		}
		if sb.Reason() != reasonNotMacOS {
			t.Errorf("Reason() = %q, want %q (GOOS check runs first)", sb.Reason(), reasonNotMacOS)
		}
		return
	}
	sb := Load([]byte("(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n"), t.TempDir(), nil, 0)
	if sb.Available() {
		t.Fatal("Load with a bad port: want unavailable")
	}
	if sb.Reason() != reasonProfileRejected {
		t.Errorf("Reason() = %q, want %q", sb.Reason(), reasonProfileRejected)
	}
}

// ---- Env ---------------------------------------------------------------

// TestEnvValues proves Env renders section 5.3's ten variables, in table
// order, from RunDir, CacheShared, ZingBin, and parentPath.
func TestEnvValues(t *testing.T) {
	t.Parallel()
	sb := Sandbox{}
	p := testParams()

	env := sb.Env(p, "/usr/bin:/bin")

	want := []string{
		"TMPDIR=" + filepath.Join(p.RunDir, "tmp"),
		"CLAUDE_CODE_TMPDIR=" + filepath.Join(p.RunDir, "claude-tmp"),
		"GOPATH=" + filepath.Join(p.CacheShared, "gopath"),
		"GOCACHE=" + filepath.Join(p.CacheShared, "go-build"),
		"GOMODCACHE=" + filepath.Join(p.CacheShared, "go-mod"),
		"GOLANGCI_LINT_CACHE=" + filepath.Join(p.CacheShared, "golangci-lint"),
		"XDG_CACHE_HOME=" + filepath.Join(p.CacheShared, "xdg"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"ZING_SANDBOXED=1",
		"PATH=" + filepath.Dir(p.ZingBin) + ":/usr/bin:/bin",
	}
	if !slices.Equal(env, want) {
		t.Errorf("Env() =\n%v\nwant\n%v", env, want)
	}
}

// ---- NewRunDir ----------------------------------------------------------

// TestNewRunDirIsPrivate proves NewRunDir creates a fresh, mode-0700 run
// directory (with its two children) each call, and that cleanup removes it
// and is safe to call twice.
func TestNewRunDirIsPrivate(t *testing.T) {
	t.Parallel()
	sb := Sandbox{host: Host{CacheRoot: t.TempDir()}}

	dir1, cleanup1, err := sb.NewRunDir()
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	dir2, cleanup2, err := sb.NewRunDir()
	if err != nil {
		t.Fatalf("NewRunDir (second call): %v", err)
	}
	if dir1 == dir2 {
		t.Fatalf("two NewRunDir calls returned the same directory %q", dir1)
	}

	for _, sub := range []string{"", "tmp", "claude-tmp"} {
		p := filepath.Join(dir1, sub)
		info, statErr := os.Stat(p)
		if statErr != nil {
			t.Fatalf("stat %s: %v", p, statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("mode of %s = %o, want 0700", p, perm)
		}
	}

	cleanup1()
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Errorf("dir1 %s still exists after cleanup", dir1)
	}
	cleanup1() // safe to call twice
	cleanup2()
}

// TestLoadRejectsEmptyDataDir proves Load records the sandbox unavailable
// with the reason it already produces for any host-resolve failure when
// dataDir is empty (review F044: resolveHost must fail closed rather than
// let filepath.EvalSymlinks("") silently resolve to the current directory).
func TestLoadRejectsEmptyDataDir(t *testing.T) {
	t.Parallel()
	profile := []byte("(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n")
	if runtime.GOOS != testGOOSDarwin {
		sb := Load(profile, "", nil, 7420)
		if sb.Available() {
			t.Fatal("Load with an empty data dir: want unavailable")
		}
		if sb.Reason() != reasonNotMacOS {
			t.Errorf("Reason() = %q, want %q (GOOS check runs first)", sb.Reason(), reasonNotMacOS)
		}
		return
	}
	sb := Load(profile, "", nil, 7420)
	if sb.Available() {
		t.Fatal("Load with an empty data dir: want unavailable")
	}
	if sb.Reason() != reasonUserCacheDirNotFound {
		t.Errorf("Reason() = %q, want %q", sb.Reason(), reasonUserCacheDirNotFound)
	}
}

// ---- Reason -------------------------------------------------------------

// TestReasonNotMacOS proves Load reports reasonNotMacOS off darwin; it
// skips on darwin, where GOOS really is "darwin" and this branch cannot be
// observed.
func TestReasonNotMacOS(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == testGOOSDarwin {
		t.Skip("this machine's GOOS is darwin; the not-macOS reason cannot be observed here")
	}
	sb := Load([]byte("(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n"), t.TempDir(), nil, 7420)
	if sb.Available() {
		t.Fatal("Load off darwin: want unavailable")
	}
	if sb.Reason() != reasonNotMacOS {
		t.Errorf("Reason() = %q, want %q", sb.Reason(), reasonNotMacOS)
	}
}

// TestOffIsNeverAvailable proves Off's own contract: never available, for
// suites on the fake runtime.
func TestOffIsNeverAvailable(t *testing.T) {
	t.Parallel()
	sb := Off()
	if sb.Available() {
		t.Error("Off(): Available() = true, want false")
	}
	if sb.Reason() == "" {
		t.Error("Off(): Reason() is empty, want a non-empty reason")
	}
}

// ---- transcripts encoding -------------------------------------------------

// TestParamsForTranscriptsWorkedExample proves ParamsFor's TRANSCRIPTS value
// against section 5.2's own worked example encoding rule (every byte outside
// [A-Za-z0-9] becomes '-'), applied to a real, existing worktree under
// t.TempDir() rather than the plan's own illustrative path: this task makes
// ParamsFor resolve WORKTREE with filepath.EvalSymlinks
// (TestParamsForRejectsMissingPath), so a path that does not exist on this
// machine, such as the plan's own literal example, is now an error.
func TestParamsForTranscriptsWorkedExample(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "zing", ".zing", "wt", "12")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", worktree, err)
	}

	sb := Sandbox{host: Host{Home: home}}
	p, err := sb.ParamsFor(worktree, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}

	resolvedWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", worktree, err)
	}
	want := filepath.Join(home, ".claude", "projects", encodeTranscriptDir(resolvedWorktree))
	if p.Transcripts != want {
		t.Errorf("Transcripts = %q, want %q", p.Transcripts, want)
	}
}

// ---- ParamsFor symlink resolution -----------------------------------------

// TestParamsForResolvesSymlinks proves ParamsFor resolves Worktree, RepoGit,
// and RunDir before it ever uses them, so a seatbelt subpath rule matches
// what the kernel actually resolves a sandboxed child's own opens against
// (macOS's own /var -> /private/var, most notably; task 16a's own
// live-harness defect). Transcripts is derived from the resolved worktree,
// not the one ParamsFor was given, since the CLI names its transcript
// folder after the working directory it sees.
func TestParamsForResolvesSymlinks(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", realDir, err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", realDir, err)
	}

	sb := Sandbox{host: Host{Home: testHomeDir}}
	p, err := sb.ParamsFor(link, link, link)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	if p.Worktree != resolved {
		t.Errorf("Worktree = %q, want %q (resolved)", p.Worktree, resolved)
	}
	if p.RepoGit != resolved {
		t.Errorf("RepoGit = %q, want %q (resolved)", p.RepoGit, resolved)
	}
	if p.RunDir != resolved {
		t.Errorf("RunDir = %q, want %q (resolved)", p.RunDir, resolved)
	}
	wantTranscripts := transcriptsDir(sb.host.Home, resolved)
	if p.Transcripts != wantTranscripts {
		t.Errorf("Transcripts = %q, want %q", p.Transcripts, wantTranscripts)
	}
}

// TestParamsForRejectsMissingPath proves a param path that does not resolve
// (here, a WORKTREE that does not exist) is refused with the exact error
// text this task adds, naming the offending param, rather than silently
// carrying a path no rule will ever match.
func TestParamsForRejectsMissingPath(t *testing.T) {
	t.Parallel()
	sb := Sandbox{host: Host{Home: testHomeDir}}
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := sb.ParamsFor(missing, t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("ParamsFor with a missing WORKTREE: want an error, got nil")
	}
	want := "sandbox: param WORKTREE does not resolve:"
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("ParamsFor error = %q, want prefix %q", err.Error(), want)
	}
}

// ---- ZING_SANDBOXED skip predicate ----------------------------------------

// shouldSkipSandboxed reports whether ZING_SANDBOXED is set (section 5.3): a
// sandboxed process cannot itself start sandbox-exec ("sandbox_apply:
// Operation not permitted", measured on the build host), so every test that
// does skips instead of failing when it is already running inside one.
// Defined here, not in sandbox_darwin_test.go, so it compiles (and this
// file's own test can exercise it) on every platform, not only darwin.
func shouldSkipSandboxed() bool {
	return os.Getenv("ZING_SANDBOXED") != ""
}

// TestDarwinTestsSkipWhenSandboxed proves shouldSkipSandboxed (the predicate
// requireNotSandboxed, in sandbox_darwin_test.go, is built on) reports true
// exactly when ZING_SANDBOXED is set (section 5.3): a sandboxed process
// cannot itself start sandbox-exec, so every darwin test that does must skip
// rather than fail when it is already running inside the sandbox.
//
// Not parallel: it calls t.Setenv("ZING_SANDBOXED", ...) below.
func TestDarwinTestsSkipWhenSandboxed(t *testing.T) {
	if os.Getenv("ZING_SANDBOXED") != "" {
		t.Skip("already running under ZING_SANDBOXED; the unset case cannot be observed here")
	}
	if shouldSkipSandboxed() {
		t.Fatal("shouldSkipSandboxed() = true with ZING_SANDBOXED unset, want false")
	}

	t.Setenv("ZING_SANDBOXED", "1")
	if !shouldSkipSandboxed() {
		t.Error("shouldSkipSandboxed() = false with ZING_SANDBOXED set, want true")
	}
}

// TestRenderProfilePlaceholders is a small sanity check that renderProfile
// actually removes both placeholder lines and leaves the rest of a
// realistic profile untouched, guarding the two strings.Replace calls
// against a typo in the placeholder constants.
func TestRenderProfilePlaceholders(t *testing.T) {
	t.Parallel()
	base := "(version 1)\n(allow default)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n(deny mach-lookup)\n"
	got, err := renderProfile([]byte(base), []string{testReadPathEntry}, 7420)
	if err != nil {
		t.Fatalf("renderProfile: %v", err)
	}
	if strings.Contains(got, readPathsPlaceholder) || strings.Contains(got, consoleDenyPlaceholder) {
		t.Errorf("renderProfile left a placeholder unreplaced:\n%s", got)
	}
	if !strings.Contains(got, `(allow file-read-data (subpath "`+testReadPathEntry+`"))`) {
		t.Errorf("renderProfile did not render the read path:\n%s", got)
	}
	if !strings.Contains(got, `(deny network-outbound (remote tcp "*:7420"))`) {
		t.Errorf("renderProfile did not render the console deny:\n%s", got)
	}
	if !strings.Contains(got, "(deny mach-lookup)") {
		t.Errorf("renderProfile dropped unrelated profile text:\n%s", got)
	}
}

// TestLoadRejectsMissingPlaceholder proves Load records the sandbox
// unavailable with reason "profile rejected" when base is missing
// ;;CONSOLE_DENY;; (review F024: strings.Replace(..., 1) would otherwise
// silently no-op the replacement and render a profile with no console
// deny), on every platform.
func TestLoadRejectsMissingPlaceholder(t *testing.T) {
	t.Parallel()
	base := []byte("(version 1)\n;;READ_PATHS;;\n")
	if runtime.GOOS != testGOOSDarwin {
		sb := Load(base, t.TempDir(), nil, 7420)
		if sb.Available() {
			t.Fatal("Load with a missing placeholder: want unavailable")
		}
		if sb.Reason() != reasonNotMacOS {
			t.Errorf("Reason() = %q, want %q (GOOS check runs first)", sb.Reason(), reasonNotMacOS)
		}
		return
	}
	sb := Load(base, t.TempDir(), nil, 7420)
	if sb.Available() {
		t.Fatal("Load with a missing placeholder: want unavailable")
	}
	if sb.Reason() != reasonProfileRejected {
		t.Errorf("Reason() = %q, want %q", sb.Reason(), reasonProfileRejected)
	}
}

// TestLoadRejectsRepeatedPlaceholder proves the same for a base that carries
// ;;CONSOLE_DENY;; twice (review F024).
func TestLoadRejectsRepeatedPlaceholder(t *testing.T) {
	t.Parallel()
	base := []byte("(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n;;CONSOLE_DENY;;\n")
	if runtime.GOOS != testGOOSDarwin {
		sb := Load(base, t.TempDir(), nil, 7420)
		if sb.Available() {
			t.Fatal("Load with a repeated placeholder: want unavailable")
		}
		if sb.Reason() != reasonNotMacOS {
			t.Errorf("Reason() = %q, want %q (GOOS check runs first)", sb.Reason(), reasonNotMacOS)
		}
		return
	}
	sb := Load(base, t.TempDir(), nil, 7420)
	if sb.Available() {
		t.Fatal("Load with a repeated placeholder: want unavailable")
	}
	if sb.Reason() != reasonProfileRejected {
		t.Errorf("Reason() = %q, want %q", sb.Reason(), reasonProfileRejected)
	}
}

// TestLoadRejectsProfileBeforeResolvingHost proves Load reports a profile
// that does not render as "profile rejected" even when the host cannot be
// resolved: HOME points at a regular file, so os.UserCacheDir names a path
// under a file and resolveHost's MkdirAll fails, the same resolveHost
// failure the build sandbox causes by denying the cache root chmod when
// zing builds zing. The first call, with a valid profile, proves the
// forcing works; the second, with a bad port, is the regression.
//
// Not parallel: it calls t.Setenv("HOME", ...).
func TestLoadRejectsProfileBeforeResolvingHost(t *testing.T) {
	if runtime.GOOS != testGOOSDarwin {
		t.Skip("off darwin Load returns before either check")
	}
	dataDir := t.TempDir()
	homeFile := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(homeFile, nil, 0o600); err != nil {
		t.Fatalf("write %s: %v", homeFile, err)
	}
	t.Setenv("HOME", homeFile)

	valid := []byte("(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n")
	sb := Load(valid, dataDir, nil, 7420)
	if sb.Available() {
		t.Fatal("Load with an unresolvable home: want unavailable")
	}
	if sb.Reason() != reasonUserCacheDirNotFound {
		t.Fatalf("Reason() = %q, want %q (HOME as a file must make resolveHost fail)", sb.Reason(), reasonUserCacheDirNotFound)
	}

	sb = Load(valid, dataDir, nil, 0)
	if sb.Available() {
		t.Fatal("Load with a bad port: want unavailable")
	}
	if sb.Reason() != reasonProfileRejected {
		t.Errorf("Reason() = %q, want %q (profile is validated before the host is resolved)", sb.Reason(), reasonProfileRejected)
	}
}

// ---- Set: For, OffSet, FirstUnavailable (PKG9-PLAN.md section 4.7) --------

// TestSetFor proves Set.For's own three-name lookup, and that any other
// name reports ok=false.
func TestSetFor(t *testing.T) {
	t.Parallel()
	build := Sandbox{reason: "build-reason"}
	readonly := Sandbox{reason: "readonly-reason"}
	judge := Sandbox{reason: "judge-reason"}
	s := Set{Build: build, ReadOnly: readonly, Judge: judge}

	tests := []struct {
		name string
		want Sandbox
		ok   bool
	}{
		{profileNameBuild, build, true},
		{profileNameReadOnly, readonly, true},
		{"judge", judge, true},
		{"bogus", Sandbox{}, false},
		{"", Sandbox{}, false},
	}
	for _, tc := range tests {
		got, ok := s.For(tc.name)
		if ok != tc.ok {
			t.Errorf("For(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got.reason != tc.want.reason {
			t.Errorf("For(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestOffSet proves OffSet returns three unavailable sandboxes, each one
// Off's own contract.
func TestOffSet(t *testing.T) {
	t.Parallel()
	s := OffSet()
	for name, sb := range map[string]Sandbox{"Build": s.Build, "ReadOnly": s.ReadOnly, "Judge": s.Judge} {
		if sb.Available() {
			t.Errorf("OffSet().%s: Available() = true, want false", name)
		}
		if sb.Reason() == "" {
			t.Errorf("OffSet().%s: Reason() is empty, want a non-empty reason", name)
		}
	}
}

// TestFirstUnavailable proves Set.FirstUnavailable reports only a profile
// some job in used actually names, in build/readonly/judge order, and ""
// when every used profile is available: an unused, unloaded judge (M1's
// own Set.Judge, NotLoaded()) is never reported, since no M1 job names it.
func TestFirstUnavailable(t *testing.T) {
	t.Parallel()
	available := Sandbox{available: true}
	unavailableBuild := Sandbox{reason: "build broke"}
	unavailableReadonly := Sandbox{reason: "readonly broke"}

	t.Run("every used profile available", func(t *testing.T) {
		t.Parallel()
		s := Set{Build: available, ReadOnly: available, Judge: NotLoaded()}
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly}); got != "" {
			t.Errorf("FirstUnavailable() = %q, want empty", got)
		}
	})

	t.Run("an unused unloaded judge is not reported", func(t *testing.T) {
		t.Parallel()
		s := Set{Build: available, ReadOnly: available, Judge: NotLoaded()}
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly}); got != "" {
			t.Errorf("FirstUnavailable() = %q, want empty (judge is unused)", got)
		}
	})

	t.Run("build unavailable and used", func(t *testing.T) {
		t.Parallel()
		s := Set{Build: unavailableBuild, ReadOnly: available, Judge: NotLoaded()}
		want := "build: build broke"
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly}); got != want {
			t.Errorf("FirstUnavailable() = %q, want %q", got, want)
		}
	})

	t.Run("build and readonly both unavailable reports build first", func(t *testing.T) {
		t.Parallel()
		s := Set{Build: unavailableBuild, ReadOnly: unavailableReadonly, Judge: NotLoaded()}
		want := "build: build broke"
		if got := s.FirstUnavailable([]string{profileNameReadOnly, profileNameBuild}); got != want {
			t.Errorf("FirstUnavailable() = %q, want %q (build/readonly/judge order, not used's own order)", got, want)
		}
	})

	t.Run("judge used and unavailable", func(t *testing.T) {
		t.Parallel()
		s := Set{Build: available, ReadOnly: available, Judge: NotLoaded()}
		want := "judge: not loaded"
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly, "judge"}); got != want {
			t.Errorf("FirstUnavailable() = %q, want %q", got, want)
		}
	})
}

// TestRenderReadonlyPlaceholders proves the checked-in sandbox/readonly.sb
// carries each placeholder exactly once and renders clean, the same way
// build.sb already does (section 4.7: "Both files carry the two
// placeholder lines of build.sb, each exactly once").
func TestRenderReadonlyPlaceholders(t *testing.T) {
	t.Parallel()
	profile, err := zing.Assets.ReadFile("sandbox/readonly.sb")
	if err != nil {
		t.Fatalf("read sandbox/readonly.sb: %v", err)
	}
	if _, err := renderProfile(profile, nil, 7420); err != nil {
		t.Errorf("renderProfile(readonly.sb): %v", err)
	}
}

// TestRenderProfileEachPlaceholderOnce proves a base carrying each
// placeholder exactly once still renders, and that its output matches
// renderReadPaths and renderConsoleDeny byte-for-byte (review F024's
// require-exactly-one check must not change the successful-render output).
func TestRenderProfileEachPlaceholderOnce(t *testing.T) {
	t.Parallel()
	base := "(version 1)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n"
	got, err := renderProfile([]byte(base), []string{testReadPathEntry}, 7420)
	if err != nil {
		t.Fatalf("renderProfile: %v", err)
	}
	want := "(version 1)\n" + renderReadPaths([]string{testReadPathEntry}) + "\n" +
		`(deny network-outbound (remote tcp "*:7420"))` + "\n"
	if got != want {
		t.Errorf("renderProfile() =\n%q\nwant\n%q", got, want)
	}
}

// ---- the judge profile's two extra parameters (PKG9-PLAN.md section 4.7, D27) ----

// testJudgeParams returns testParams with the judge profile's own two
// extra fields filled, for a Prefix test that expects them to succeed.
func testJudgeParams() Params {
	p := testParams()
	p.ScenariosFile = "/Users/test/data/judge/1/scenarios.xml"
	p.CodexHome = "/Users/test/codex-judge"
	return p
}

// TestPrefixEmitsScenariosFileForJudge proves Prefix appends "-D
// SCENARIOS_FILE=..." and "-D CODEX_HOME=..." after the fixed paramOrder
// flags, in that order, only for a Sandbox loaded under the judge name.
func TestPrefixEmitsScenariosFileForJudge(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile, name: profileNameJudge}
	p := testJudgeParams()

	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	want := []string{
		testSandboxExecArgv0,
		"-D", "HOME=" + p.Home,
		"-D", "WORKTREE=" + p.Worktree,
		"-D", "REPO_GIT=" + p.RepoGit,
		"-D", "DATA_DIR=" + p.DataDir,
		"-D", "ZING_BIN=" + p.ZingBin,
		"-D", "CACHE_ROOT=" + p.CacheRoot,
		"-D", "CACHE_SHARED=" + p.CacheShared,
		"-D", "RUN_DIR=" + p.RunDir,
		"-D", "MDS_CACHE=" + p.MDSCache,
		"-D", "TRANSCRIPTS=" + p.Transcripts,
		"-D", "SSH_AUTH_SOCK=" + noAgentSocket,
		"-D", "SSH_AUTH_SOCK_REAL=" + noAgentSocket,
		"-D", "SCENARIOS_FILE=" + p.ScenariosFile,
		"-D", "CODEX_HOME=" + p.CodexHome,
		"-p", sb.renderedProfile,
	}
	if !slices.Equal(argv, want) {
		t.Errorf("Prefix() =\n%v\nwant\n%v", argv, want)
	}
}

// TestPrefixOmitsUnsetJudgeParams proves Prefix emits no SCENARIOS_FILE or
// CODEX_HOME flag for a build- or readonly-shaped Params, where both
// fields stay their zero value (section 4.7: "empty for the other two
// profiles").
func TestPrefixOmitsUnsetJudgeParams(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile, name: profileNameBuild}
	argv, err := sb.Prefix(testParams())
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	for _, a := range argv {
		if strings.Contains(a, "SCENARIOS_FILE") || strings.Contains(a, "CODEX_HOME") {
			t.Errorf("Prefix() with both judge params unset carries one anyway: %v", argv)
		}
	}
}

// TestPrefixEmitsCodexHomeAloneOutsideJudge proves Prefix emits just the
// one judge-only flag a caller set, with no "both or neither" requirement,
// for a Sandbox not loaded under the judge name (internal/job's own
// TestRunJobJudgeParamsCodexHome relies on exactly this: it tests
// Deps.JudgeCodexHome's own wiring into Params.CodexHome in isolation,
// against a Sandbox loaded under a different name, without also having to
// wire a scenarios file that task belongs to a later task).
func TestPrefixEmitsCodexHomeAloneOutsideJudge(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile, name: profileNameBuild}
	p := testParams()
	p.CodexHome = "/Users/test/codex-judge"

	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	wantFlag := "CODEX_HOME=" + p.CodexHome
	if !slices.Contains(argv, wantFlag) {
		t.Errorf("Prefix() = %v, want it to contain %q", argv, wantFlag)
	}
	for _, a := range argv {
		if strings.Contains(a, "SCENARIOS_FILE") {
			t.Errorf("Prefix() carries a SCENARIOS_FILE flag with ScenariosFile unset: %v", argv)
		}
	}
}

// TestJudgeWithoutScenariosFileIsConfigError proves Prefix refuses to
// build a prefix for the judge profile when either SCENARIOS_FILE or
// CODEX_HOME is empty, rather than silently omitting the missing one
// (section 4.7, D19, D27).
func TestJudgeWithoutScenariosFileIsConfigError(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Params){
		"empty ScenariosFile": func(p *Params) { p.ScenariosFile = "" },
		"empty CodexHome":     func(p *Params) { p.CodexHome = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sb := Sandbox{renderedProfile: testMinimalRenderedProfile, name: profileNameJudge}
			p := testJudgeParams()
			mutate(&p)

			if _, err := sb.Prefix(p); !errors.Is(err, errJudgeParamsIncomplete) {
				t.Errorf("Prefix() err = %v, want errJudgeParamsIncomplete", err)
			}
		})
	}
}

// TestScenariosFileParamIsChecked proves SCENARIOS_FILE and CODEX_HOME go
// through the same checkParamValue safety rule as every other param: an
// unsafe or relative value is refused, naming the offending param.
func TestScenariosFileParamIsChecked(t *testing.T) {
	t.Parallel()
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile, name: profileNameJudge}

	t.Run("relative ScenariosFile", func(t *testing.T) {
		t.Parallel()
		p := testJudgeParams()
		p.ScenariosFile = "relative/scenarios.xml"
		_, err := sb.Prefix(p)
		want := "sandbox: param SCENARIOS_FILE has an unsafe value"
		if err == nil || err.Error() != want {
			t.Errorf("Prefix() err = %v, want %q", err, want)
		}
	})

	t.Run("unsafe CodexHome", func(t *testing.T) {
		t.Parallel()
		p := testJudgeParams()
		p.CodexHome = `/Users/test/codex"; rm -rf /`
		_, err := sb.Prefix(p)
		want := "sandbox: param CODEX_HOME has an unsafe value"
		if err == nil || err.Error() != want {
			t.Errorf("Prefix() err = %v, want %q", err, want)
		}
	})
}

// TestJudgeProofWritesTempScenariosFile proves judgeProof (the judge
// profile's own LoadProfile proof step, section 4.7) writes a temp
// scenarios file at mode 0600 under runDir, with its literal path passed
// as p.ScenariosFile, and a temp Codex home directory as p.CodexHome; both
// live under runDir, so NewRunDir's own cleanup -- not judgeProof itself --
// is what removes them once the proof returns.
func TestJudgeProofWritesTempScenariosFile(t *testing.T) {
	t.Parallel()
	sb := Sandbox{name: profileNameJudge}
	runDir := t.TempDir()
	var p Params

	cmd, err := sb.judgeProof(runDir, &p)
	if err != nil {
		t.Fatalf("judgeProof: %v", err)
	}

	if p.ScenariosFile == "" || filepath.Dir(p.ScenariosFile) != runDir {
		t.Errorf("p.ScenariosFile = %q, want a file under %q", p.ScenariosFile, runDir)
	}
	info, statErr := os.Stat(p.ScenariosFile)
	if statErr != nil {
		t.Fatalf("stat %s: %v", p.ScenariosFile, statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode of %s = %o, want 0600", p.ScenariosFile, perm)
	}
	wantCmd := []string{"/bin/cat", p.ScenariosFile}
	if !slices.Equal(cmd, wantCmd) {
		t.Errorf("judgeProof command = %v, want %v", cmd, wantCmd)
	}

	if p.CodexHome == "" || filepath.Dir(p.CodexHome) != runDir {
		t.Errorf("p.CodexHome = %q, want a directory under %q", p.CodexHome, runDir)
	}
	if info, statErr := os.Stat(p.CodexHome); statErr != nil || !info.IsDir() {
		t.Errorf("stat %s: info=%v err=%v, want an existing directory", p.CodexHome, info, statErr)
	}

	// The scenarios file and codex home both live under runDir, so removing
	// runDir (NewRunDir's own cleanup, which proves calls after judgeProof)
	// removes them too; judgeProof itself never removes anything.
	if err := os.RemoveAll(runDir); err != nil {
		t.Fatalf("RemoveAll(runDir): %v", err)
	}
	if _, err := os.Stat(p.ScenariosFile); !os.IsNotExist(err) {
		t.Errorf("scenarios file still exists after runDir is removed (stat err = %v)", err)
	}
}

// ---- denying outbound git transports (#49) -----------------------------

// TestProfilesDenyGitTransports is sandbox-safe (it reads text, never runs
// sandbox-exec), so a build itself can watch it fail before the fix and
// pass after. It proves build.sb and judge.sb both carry the port 22/9418
// outbound deny and the "/ssh$" exec deny the #49 fix adds.
func TestProfilesDenyGitTransports(t *testing.T) {
	t.Parallel()
	wantTCPDeny := `(deny network-outbound (remote tcp "*:22") (remote tcp "*:9418"))`
	wantSSHExecDeny := `(deny process-exec (regex #"/ssh$"))`
	wantAgentSocketDeny := "(deny network-outbound\n" +
		"  (remote unix-socket (path-literal (param \"SSH_AUTH_SOCK\")))\n" +
		"  (remote unix-socket (path-literal (param \"SSH_AUTH_SOCK_REAL\"))))"

	for _, name := range []string{"sandbox/build.sb", "sandbox/judge.sb"} {
		profile, err := zing.Assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(profile)
		if !strings.Contains(text, wantTCPDeny) {
			t.Errorf("%s does not contain %q", name, wantTCPDeny)
		}
		if !strings.Contains(text, wantSSHExecDeny) {
			t.Errorf("%s does not contain %q", name, wantSSHExecDeny)
		}
		if !strings.Contains(text, wantAgentSocketDeny) {
			t.Errorf("%s does not contain %q", name, wantAgentSocketDeny)
		}
	}
}
