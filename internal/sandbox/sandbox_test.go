package sandbox

import (
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

// TestLoadRejectsEmptyDataDir proves Load records the sandbox unavailable
// with the reason it already produces for any host-resolve failure when
// dataDir is empty (review F044: resolveHost must fail closed rather than
// let filepath.EvalSymlinks("") silently resolve to the current directory).
func TestLoadRejectsEmptyDataDir(t *testing.T) {
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

// ---- Set: For, OffSet, FirstUnavailable (PKG9-PLAN.md section 4.7) --------

// TestSetFor proves Set.For's own three-name lookup, and that any other
// name reports ok=false.
func TestSetFor(t *testing.T) {
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
	available := Sandbox{available: true}
	unavailableBuild := Sandbox{reason: "build broke"}
	unavailableReadonly := Sandbox{reason: "readonly broke"}

	t.Run("every used profile available", func(t *testing.T) {
		s := Set{Build: available, ReadOnly: available, Judge: NotLoaded()}
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly}); got != "" {
			t.Errorf("FirstUnavailable() = %q, want empty", got)
		}
	})

	t.Run("an unused unloaded judge is not reported", func(t *testing.T) {
		s := Set{Build: available, ReadOnly: available, Judge: NotLoaded()}
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly}); got != "" {
			t.Errorf("FirstUnavailable() = %q, want empty (judge is unused)", got)
		}
	})

	t.Run("build unavailable and used", func(t *testing.T) {
		s := Set{Build: unavailableBuild, ReadOnly: available, Judge: NotLoaded()}
		want := "build: build broke"
		if got := s.FirstUnavailable([]string{profileNameBuild, profileNameReadOnly}); got != want {
			t.Errorf("FirstUnavailable() = %q, want %q", got, want)
		}
	})

	t.Run("build and readonly both unavailable reports build first", func(t *testing.T) {
		s := Set{Build: unavailableBuild, ReadOnly: unavailableReadonly, Judge: NotLoaded()}
		want := "build: build broke"
		if got := s.FirstUnavailable([]string{profileNameReadOnly, profileNameBuild}); got != want {
			t.Errorf("FirstUnavailable() = %q, want %q (build/readonly/judge order, not used's own order)", got, want)
		}
	})

	t.Run("judge used and unavailable", func(t *testing.T) {
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
