// Package sandbox builds and applies the seatbelt profile every build and
// perimeter run is wrapped in on macOS (PKG8-PLAN.md section 5): the profile
// text in sandbox/build.sb is trust root, and this package fills in its
// per-run parameters, proves it loads, and renders the command prefix and
// environment a sandboxed process runs under. Load never returns an error: a
// failure is recorded on the returned Sandbox as unavailable, with a reason,
// so a caller decides for itself whether an unavailable sandbox is fatal
// (design D5, D9): a real build run refuses to start (job.ErrSandbox); a
// suite on the fake runtime runs unwrapped.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// proveTimeout bounds the proof run Load makes: a plain /usr/bin/true
// should exit instantly, but Load must never hang the caller on it.
const proveTimeout = 10 * time.Second

// The four closed reason strings Reason() returns for an unavailable
// Sandbox built by Load (section 5.4's own table). Off's own reason is not
// one of these four: it names why the sandbox was never attempted at all,
// not why an attempt failed.
const (
	reasonNotMacOS             = "not macOS"
	reasonSandboxExecNotFound  = "sandbox-exec not found"
	reasonUserCacheDirNotFound = "user cache directory not found"
	reasonProfileRejected      = "profile rejected"
	reasonOff                  = "off"
	// reasonNotLoaded is NotLoaded's own reason (PKG9-PLAN.md section 4.7):
	// M1's own Set.Judge, a profile name Set.For recognizes that this build
	// of serve does not load yet, distinct from Off's "off" (a suite on the
	// fake runtime that never attempts sandbox-exec at all).
	reasonNotLoaded = "not loaded"
)

// Params fills one profile's per-run "-D NAME=value" values (section 5.2).
// Every field must be absolute and free of '"', '\', and a newline; Prefix
// checks this before it ever builds an argv.
type Params struct {
	Home, Worktree, RepoGit, DataDir, ZingBin             string
	CacheRoot, CacheShared, RunDir, Transcripts, MDSCache string
}

// Host holds what is the same for every run on this machine (section 5.2):
// resolved once, at Load, and reused by every later ParamsFor call.
type Host struct {
	Home, DataDir, ZingBin, CacheRoot, CacheShared, MDSCache string
}

// Sandbox is a loaded (or deliberately unavailable) seatbelt profile: its
// rendered text, the host values every run on this machine shares, and
// whether it is safe to wrap a real run in.
type Sandbox struct {
	renderedProfile string
	host            Host
	available       bool
	reason          string
}

// Off returns a Sandbox that is never available, for a suite that drives the
// fake runtime and must never attempt sandbox-exec at all (design D5). serve
// never uses it (N9): a real build run fails closed instead.
func Off() Sandbox {
	return Sandbox{reason: reasonOff}
}

// NotLoaded returns an unavailable Sandbox with reason "not loaded", for a
// Set.For name this build of serve does not load yet (PKG9-PLAN.md section
// 4.7): M1's own Set.Judge, before M2 task 2 adds judge.sb and its load.
func NotLoaded() Sandbox {
	return Sandbox{reason: reasonNotLoaded}
}

// Set holds the three loaded profiles machine.toml's job.sandbox key can
// name (PKG9-PLAN.md section 4.7): build, readonly, and, from M2 on, judge.
// In M1, serve loads Build and ReadOnly and leaves Judge Off() with reason
// "not loaded".
type Set struct {
	Build, ReadOnly, Judge Sandbox
}

// profileNameBuild, profileNameReadOnly, and profileNameJudge are the three
// machine.toml job.sandbox values Set.For recognizes (section 4.7), named
// once so machine.go's own validation and this package's lookup never drift
// apart.
const (
	profileNameBuild    = "build"
	profileNameReadOnly = "readonly"
	profileNameJudge    = "judge"
)

// For returns the profile machine.toml names: "build", "readonly", or
// "judge". ok is false for any other name (section 4.7).
func (s Set) For(name string) (Sandbox, bool) {
	switch name {
	case profileNameBuild:
		return s.Build, true
	case profileNameReadOnly:
		return s.ReadOnly, true
	case profileNameJudge:
		return s.Judge, true
	default:
		return Sandbox{}, false
	}
}

// OffSet returns a Set of three Off() sandboxes, for a suite that drives
// the fake runtime and must never attempt sandbox-exec at all (section
// 4.7, design D5).
func OffSet() Set {
	return Set{Build: Off(), ReadOnly: Off(), Judge: Off()}
}

// setProfileOrder is the order FirstUnavailable reports in (section 4.7):
// build, readonly, judge.
var setProfileOrder = []string{profileNameBuild, profileNameReadOnly, profileNameJudge}

// FirstUnavailable returns "<name>: <reason>" for the first profile, in
// setProfileOrder, that some machine.toml job in used actually names and
// that did not load, or "" when every used profile is available (section
// 4.7). A profile no job uses never turns the console indicator red, even
// when it is Off() (M1's own Set.Judge, for instance).
func (s Set) FirstUnavailable(used []string) string {
	usedSet := make(map[string]bool, len(used))
	for _, name := range used {
		usedSet[name] = true
	}
	for _, name := range setProfileOrder {
		if !usedSet[name] {
			continue
		}
		sb, ok := s.For(name)
		if ok && !sb.Available() {
			return name + ": " + sb.Reason()
		}
	}
	return ""
}

// Available reports whether s loaded and proved itself.
func (s Sandbox) Available() bool { return s.available }

// Reason reports why s is unavailable, "" when it is available.
func (s Sandbox) Reason() string { return s.reason }

// cacheDirPerm is the mode every directory this package creates under the
// user's cache root is created and kept at (section 5.2): private to the
// owner, since a sandboxed build's own cache lives here.
const cacheDirPerm = 0o700

// Load resolves the host values, renders profile with readPaths and
// consolePort, and proves the result loads by running sandbox-exec against
// a throwaway run directory (design section 5.4 of PKG8-PLAN.md). It never
// returns an error: any failure is recorded on the returned Sandbox,
// unavailable, with one of the four closed reasons. Load is LoadProfile("build",
// ...) (PKG9-PLAN.md section 4.7): every earlier caller of Load keeps
// working unchanged now that loading is profile-aware.
func Load(profile []byte, dataDir string, readPaths []string, consolePort int) Sandbox {
	return LoadProfile(profileNameBuild, profile, dataDir, readPaths, consolePort)
}

// LoadProfile resolves the host values, renders profile with readPaths and
// consolePort, and proves the result loads by running sandbox-exec against
// a throwaway run directory (PKG9-PLAN.md section 4.7): the same proof
// every profile shares in M1 (a fresh /usr/bin/true under generic
// parameters). name is one of Set.For's three names; it names nothing
// about the proof yet (the judge profile's own scenarios-file proof
// arrives in M2 task 1) but is threaded through now so every later caller
// already names which profile it is loading. It never returns an error:
// any failure is recorded on the returned Sandbox, unavailable, with one
// of the four closed reasons.
func LoadProfile(name string, profile []byte, dataDir string, readPaths []string, consolePort int) Sandbox { //nolint:unparam,revive // name is reserved for M2's profile-specific proof (the judge's scenarios file); every M1 caller passes "build" or "readonly" and both take the same proof today
	if runtime.GOOS != "darwin" {
		return Sandbox{reason: reasonNotMacOS}
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return Sandbox{reason: reasonSandboxExecNotFound}
	}

	host, err := resolveHost(dataDir)
	if err != nil {
		return Sandbox{reason: reasonUserCacheDirNotFound}
	}

	rendered, err := renderProfile(profile, readPaths, consolePort)
	if err != nil {
		return Sandbox{host: host, reason: reasonProfileRejected}
	}
	sb := Sandbox{host: host, renderedProfile: rendered}

	if !sb.proves() {
		return Sandbox{host: host, renderedProfile: rendered, reason: reasonProfileRejected}
	}
	sb.available = true
	return sb
}

// resolveHost resolves every Host field section 5.2's table names: the
// user's home and this binary's own path, both with symlinks resolved; the
// data directory, also symlink-resolved (review F044: seatbelt's
// "(subpath ...)" match runs against the kernel-resolved path, so a data
// directory reached through a symlink would otherwise escape the DATA_DIR
// deny rule); the cache root and its "shared" subdirectory, created 0700;
// and the per-user cache folder's "mds" child, found through getconf
// (host_darwin.go) and symlink-resolved when it already exists.
func resolveHost(dataDir string) (Host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Host{}, fmt.Errorf("sandbox: resolve home directory: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(home); evalErr == nil {
		home = resolved
	}

	zingBin, err := os.Executable()
	if err != nil {
		return Host{}, fmt.Errorf("sandbox: resolve zing binary: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(zingBin); evalErr == nil {
		zingBin = resolved
	}

	// filepath.EvalSymlinks("") returns "." with no error, which would
	// silently turn an empty data dir into the current directory, so the
	// empty check runs first and fails closed on its own.
	if dataDir == "" {
		return Host{}, errors.New("sandbox: data dir is empty")
	}
	resolvedDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return Host{}, fmt.Errorf("sandbox: resolve data dir: %w", err)
	}
	dataDir = resolvedDataDir

	userCacheDir, err := darwinUserCacheDir()
	if err != nil || userCacheDir == "" {
		return Host{}, fmt.Errorf("sandbox: darwin user cache dir: %w", err)
	}
	mdsCache := filepath.Join(userCacheDir, "mds")
	if resolved, evalErr := filepath.EvalSymlinks(mdsCache); evalErr == nil {
		mdsCache = resolved
	}

	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return Host{}, fmt.Errorf("sandbox: resolve user cache dir: %w", err)
	}
	cacheRoot = filepath.Join(cacheRoot, "zing", "sandbox")
	cacheShared := filepath.Join(cacheRoot, "shared")
	if err := os.MkdirAll(cacheShared, cacheDirPerm); err != nil {
		return Host{}, fmt.Errorf("sandbox: create cache shared dir: %w", err)
	}
	if err := os.Chmod(cacheRoot, cacheDirPerm); err != nil {
		return Host{}, fmt.Errorf("sandbox: chmod cache root: %w", err)
	}
	if err := os.Chmod(cacheShared, cacheDirPerm); err != nil {
		return Host{}, fmt.Errorf("sandbox: chmod cache shared dir: %w", err)
	}

	return Host{
		Home: home, DataDir: dataDir, ZingBin: zingBin,
		CacheRoot: cacheRoot, CacheShared: cacheShared, MDSCache: mdsCache,
	}, nil
}

// proves runs the section 5.4 proof command: sandbox-exec, every param
// filled (WORKTREE, REPO_GIT, and TRANSCRIPTS all pointed at one fresh run
// directory, since the proof only needs the profile to load, not a
// fine-grained boundary), the rendered profile, and /usr/bin/true.
func (s Sandbox) proves() bool {
	runDir, cleanup, err := s.NewRunDir()
	if err != nil {
		return false
	}
	defer cleanup()

	p := Params{
		Home: s.host.Home, Worktree: runDir, RepoGit: runDir, DataDir: s.host.DataDir, ZingBin: s.host.ZingBin,
		CacheRoot: s.host.CacheRoot, CacheShared: s.host.CacheShared, RunDir: runDir,
		Transcripts: runDir, MDSCache: s.host.MDSCache,
	}
	argv, err := s.Prefix(p)
	if err != nil {
		return false
	}
	argv = append(argv, "/usr/bin/true")

	ctx, cancel := context.WithTimeout(context.Background(), proveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is built by Prefix from validated host and run-dir paths, never from model-influenced input
	return cmd.Run() == nil
}

// runIDBytes is the number of random bytes NewRunDir reads to build a run
// id: 8 bytes hex-encode to the 16 lowercase hex characters section 5.2
// specifies.
const runIDBytes = 8

// NewRunDir creates <CACHE_ROOT>/run/<id>, with "tmp" and "claude-tmp"
// inside, all mode 0700, id sixteen lowercase hex characters from
// crypto/rand (section 5.2). cleanup removes the whole directory and is
// safe to call twice (os.RemoveAll on an already-removed path is a no-op).
func (s Sandbox) NewRunDir() (dir string, cleanup func(), err error) {
	var b [runIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("sandbox: generate run id: %w", err)
	}
	id := hex.EncodeToString(b[:])

	dir = filepath.Join(s.host.CacheRoot, "run", id)
	for _, sub := range []string{"", "tmp", "claude-tmp"} {
		p := filepath.Join(dir, sub)
		if err := os.MkdirAll(p, cacheDirPerm); err != nil {
			return "", nil, fmt.Errorf("sandbox: create run dir %s: %w", p, err)
		}
		if err := os.Chmod(p, cacheDirPerm); err != nil {
			return "", nil, fmt.Errorf("sandbox: chmod run dir %s: %w", p, err)
		}
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	return dir, cleanup, nil
}

// ParamsFor fills the per-run values: the host's own fields, plus worktree,
// repoGit, and runDir, each symlink-resolved (section 5.2's "symlinks
// resolved" note on WORKTREE, extended here to REPO_GIT and RUN_DIR for the
// same reason), plus Transcripts, derived from the resolved worktree
// (section 5.2's worked example) since the CLI names its transcript folder
// after the working directory it sees. Seatbelt's own subpath match runs
// against the path the kernel resolves, not the one a caller wrote down;
// macOS's own /var -> /private/var symlink means the two differ for every
// worktree under a temp directory, so a rule built from the unresolved path
// never matches a real write (task 16a). A path that does not resolve --
// missing, or a dangling symlink -- is an error, since a rule built from it
// would name a path no write can ever match either.
func (s Sandbox) ParamsFor(worktree, repoGit, runDir string) (Params, error) {
	resolvedWorktree, err := resolveParam("WORKTREE", worktree)
	if err != nil {
		return Params{}, err
	}
	resolvedRepoGit, err := resolveParam("REPO_GIT", repoGit)
	if err != nil {
		return Params{}, err
	}
	resolvedRunDir, err := resolveParam("RUN_DIR", runDir)
	if err != nil {
		return Params{}, err
	}
	return Params{
		Home:        s.host.Home,
		Worktree:    resolvedWorktree,
		RepoGit:     resolvedRepoGit,
		DataDir:     s.host.DataDir,
		ZingBin:     s.host.ZingBin,
		CacheRoot:   s.host.CacheRoot,
		CacheShared: s.host.CacheShared,
		RunDir:      resolvedRunDir,
		Transcripts: transcriptsDir(s.host.Home, resolvedWorktree),
		MDSCache:    s.host.MDSCache,
	}, nil
}

// resolveParam resolves value's symlinks for the per-run param named name,
// wrapping a failure with section 5.2's error text so the caller learns
// which param's path did not resolve.
func resolveParam(name, value string) (string, error) {
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", fmt.Errorf("sandbox: param %s does not resolve: %w", name, err)
	}
	return resolved, nil
}

// transcriptsDir builds TRANSCRIPTS (section 5.2's worked example): worktree
// with every byte outside [A-Za-z0-9] replaced by '-', under
// "<home>/.claude/projects".
func transcriptsDir(home, worktree string) string {
	return filepath.Join(home, ".claude", "projects", encodeTranscriptDir(worktree))
}

func encodeTranscriptDir(path string) string {
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// paramOrder is section 5.2's table order, the order Prefix emits -D flags
// in and the order Load's proof and every Prefix caller shares.
var paramOrder = []string{"HOME", "WORKTREE", "REPO_GIT", "DATA_DIR", "ZING_BIN", "CACHE_ROOT", "CACHE_SHARED", "RUN_DIR", "MDS_CACHE", "TRANSCRIPTS"}

// paramValues returns p's fields in paramOrder.
func paramValues(p Params) []string {
	return []string{p.Home, p.Worktree, p.RepoGit, p.DataDir, p.ZingBin, p.CacheRoot, p.CacheShared, p.RunDir, p.MDSCache, p.Transcripts}
}

// checkParamValue enforces section 5.2's safety rule: every param value must
// be absolute and must not contain '"', '\', or a newline.
func checkParamValue(name, value string) error {
	if !filepath.IsAbs(value) || strings.ContainsAny(value, "\"\\\n") {
		return fmt.Errorf("sandbox: param %s has an unsafe value", name)
	}
	return nil
}

// Prefix returns the command prefix: sandbox-exec -D HOME=<..> -D
// WORKTREE=<..> ... -p <profile>, with the -D flags in paramOrder (section
// 5.4). It validates every param value first (checkParamValue), in that
// same order, so the first unsafe value's own name is what the error names.
func (s Sandbox) Prefix(p Params) ([]string, error) {
	values := paramValues(p)
	argv := make([]string, 0, 2+2*len(paramOrder)+2)
	argv = append(argv, "sandbox-exec")
	for i, name := range paramOrder {
		if err := checkParamValue(name, values[i]); err != nil {
			return nil, err
		}
		argv = append(argv, "-D", name+"="+values[i])
	}
	argv = append(argv, "-p", s.renderedProfile)
	return argv, nil
}

// envOrder is section 5.3's table order.
var envOrder = []string{
	"TMPDIR", "CLAUDE_CODE_TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE",
	"GOLANGCI_LINT_CACHE", "XDG_CACHE_HOME", "GIT_CONFIG_GLOBAL", "ZING_SANDBOXED", "PATH",
}

// Env returns section 5.3's variables as NAME=value, in envOrder.
// parentPath is appended after this binary's own directory in PATH.
func (s Sandbox) Env(p Params, parentPath string) []string {
	values := []string{
		filepath.Join(p.RunDir, "tmp"),
		filepath.Join(p.RunDir, "claude-tmp"),
		filepath.Join(p.CacheShared, "gopath"),
		filepath.Join(p.CacheShared, "go-build"),
		filepath.Join(p.CacheShared, "go-mod"),
		filepath.Join(p.CacheShared, "golangci-lint"),
		filepath.Join(p.CacheShared, "xdg"),
		"/dev/null",
		"1",
		filepath.Dir(p.ZingBin) + ":" + parentPath,
	}
	env := make([]string, len(envOrder))
	for i, name := range envOrder {
		env[i] = name + "=" + values[i]
	}
	return env
}
