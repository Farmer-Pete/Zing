package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ---- Prefix -----------------------------------------------------------

// testMinimalRenderedProfile is a fixed, valid-enough profile string every
// Prefix test below that does not care about the profile's own text builds
// its Sandbox from.
const testMinimalRenderedProfile = "(version 1)\n"

// testParams is a Params literal every absolute-field test in this file
// starts from and overrides one field of, so a test failure names exactly
// the field it changed.
func testParams() Params {
	return Params{
		Home:        "/Users/test/home",
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
	sb := Sandbox{renderedProfile: testMinimalRenderedProfile}
	p := testParams()

	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}

	want := []string{
		"sandbox-exec",
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

// ---- profile rendering -------------------------------------------------

// TestRenderReadPaths proves the ;;READ_PATHS;; substitution: one
// allow-file-read-data line per entry, and an empty string for an empty
// list (section 5.1).
func TestRenderReadPaths(t *testing.T) {
	if got := renderReadPaths(nil); got != "" {
		t.Errorf("renderReadPaths(nil) = %q, want empty", got)
	}

	got := renderReadPaths([]string{"/opt/homebrew/bin", "/Users/test/.local/share/mise"})
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
	if runtime.GOOS != "darwin" {
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

// ---- Reason -------------------------------------------------------------

// TestReasonNotMacOS proves Load reports reasonNotMacOS off darwin; it
// skips on darwin, where GOOS really is "darwin" and this branch cannot be
// observed.
func TestReasonNotMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
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
// against section 5.2's own worked example.
func TestParamsForTranscriptsWorkedExample(t *testing.T) {
	sb := Sandbox{host: Host{Home: "/Users/peter"}}
	p, err := sb.ParamsFor("/Users/peter/Code/personal/zing/.zing/wt/12", "/repo/.git", "/run/dir")
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	want := "/Users/peter/.claude/projects/-Users-peter-Code-personal-zing--zing-wt-12"
	if p.Transcripts != want {
		t.Errorf("Transcripts = %q, want %q", p.Transcripts, want)
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
	base := "(version 1)\n(allow default)\n;;READ_PATHS;;\n;;CONSOLE_DENY;;\n(deny mach-lookup)\n"
	got, err := renderProfile([]byte(base), []string{"/opt/homebrew/bin"}, 7420)
	if err != nil {
		t.Fatalf("renderProfile: %v", err)
	}
	if strings.Contains(got, readPathsPlaceholder) || strings.Contains(got, consoleDenyPlaceholder) {
		t.Errorf("renderProfile left a placeholder unreplaced:\n%s", got)
	}
	if !strings.Contains(got, `(allow file-read-data (subpath "/opt/homebrew/bin"))`) {
		t.Errorf("renderProfile did not render the read path:\n%s", got)
	}
	if !strings.Contains(got, `(deny network-outbound (remote tcp "*:7420"))`) {
		t.Errorf("renderProfile did not render the console deny:\n%s", got)
	}
	if !strings.Contains(got, "(deny mach-lookup)") {
		t.Errorf("renderProfile dropped unrelated profile text:\n%s", got)
	}
}
