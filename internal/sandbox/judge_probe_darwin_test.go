//go:build darwin

// judge_probe_darwin_test.go runs PKG9-PLAN.md section 7.3's own judge
// profile probes (M2 task 1, D19, D20) against the draft sandbox/judge.sb,
// on this build host, before anything in M2 ever wires the judge profile
// into a real run (M2 task 2 onward). Section 19.3 names this task "an
// early exit": a failing probe here stops the milestone, and the owner
// decides.
//
// Every test here is skipped when ZING_SANDBOXED=1 (requireNotSandboxed,
// sandbox_darwin_test.go): a sandboxed process cannot itself start
// sandbox-exec. Beyond that, three families of probe carry a further,
// separate gate:
//
//   - The ones that run the real Codex CLI (TestProbeNestedSeatbeltFails,
//     TestProbeCodexRunsWithFullAccessInsideProfile,
//     TestProbeCodexHomeLoginAndResume) need both ZING_LIVE_CLI=1 and a
//     real, already-logged-in judge Codex home named by
//     ZING_PROBE_JUDGE_CODEX_HOME (the owner's own
//     "CODEX_HOME=<dir> codex login", D27). This file never reads
//     ~/.zing itself; only that env var ever names a real Codex home.
//   - The ones that repeat M1 task 7's own credential probes under the
//     judge profile (gh, the keychain, ssh-agent, git credential fill and
//     helper denial) need ZING_LIVE_CLI=1: like credential_probe_darwin_test.go's
//     own probes, they touch the owner's real gh/keychain/ssh-agent login.
//   - TestProbeJudgeDeniesClaudeAndCodexState and TestProbeDeniesTokenFiles
//     read real paths under the real HOME (the owner's own ~/.claude,
//     ~/.codex, ~/.zing, ~/.ssh, ~/.config) and so also need
//     ZING_LIVE_CLI=1, even though they need no Codex login of their own.
//
// Every other test in this file (the SCENARIOS_FILE and DATA_DIR reads,
// and the judge checkout's git write denials) uses only synthetic, throwaway
// temp paths and runs in an ordinary `go test ./internal/sandbox/...` on
// macOS with no extra gate.
//
// None of these probes ever prints a credential: an assertion checks a
// non-zero exit, an empty result, the absence of a "password="/
// "Authorization" line, or a byte count/length, never the credential's own
// text.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	zing "zing"
	"zing/internal/gitfixture"
)

// ---- loading and proving the draft judge.sb --------------------------

// judgeDraftConsolePort is this file's own console-deny port: distinct
// from newLoadedSandbox's 7420 and newLoadedReadonlySandbox's 7421, so no
// probe here can ever collide with the other two profiles' own tests.
const judgeDraftConsolePort = 7422

// loadJudgeDraftProfile reads the draft sandbox/judge.sb through
// zing.Assets and resolves this host's values against dataDir, the same
// way LoadProfile does. It does not prove the result loads: judge.sb's
// own text references SCENARIOS_FILE and CODEX_HOME, two params Params
// and Prefix do not carry until M2 task 2 wires them in (PKG9-PLAN.md
// section 7.3), so proving it loads is this file's own job
// (judgeDraftProves), not LoadProfile's generic one.
func loadJudgeDraftProfile(t *testing.T, dataDir string) Sandbox {
	t.Helper()
	requireNotSandboxed(t)

	profile, err := zing.Assets.ReadFile("sandbox/judge.sb")
	if err != nil {
		t.Fatalf("read sandbox/judge.sb: %v", err)
	}
	host, err := resolveHost(dataDir)
	if err != nil {
		t.Fatalf("resolveHost: %v", err)
	}
	rendered, err := renderProfile(profile, nil, judgeDraftConsolePort)
	if err != nil {
		t.Fatalf("renderProfile(judge.sb): %v", err)
	}
	return Sandbox{host: host, renderedProfile: rendered, available: true}
}

// judgeDraftPrefix is Sandbox.Prefix extended with the two extra -D flags
// the draft judge.sb's own text references that Prefix does not emit yet
// (SCENARIOS_FILE, CODEX_HOME; M2 task 2 adds both to Params and Prefix
// for real, section 7.3). Each extra value is checked the same way
// checkParamValue checks every other param: absolute, no '"', '\', or
// newline.
func judgeDraftPrefix(sb Sandbox, p Params, scenariosFile, codexHome string) ([]string, error) {
	argv, err := sb.Prefix(p)
	if err != nil {
		return nil, err
	}
	for _, kv := range [...]struct{ name, value string }{
		{"SCENARIOS_FILE", scenariosFile},
		{"CODEX_HOME", codexHome},
	} {
		if checkErr := checkParamValue(kv.name, kv.value); checkErr != nil {
			return nil, checkErr
		}
	}
	// argv ends "... -p <profile>"; the extra -D flags must land before
	// "-p" (order among -D flags does not matter to sandbox-exec, only
	// that every param the profile text references has one).
	insertAt := len(argv) - 2
	out := make([]string, 0, len(argv)+4)
	out = append(out, argv[:insertAt]...)
	out = append(out, "-D", "SCENARIOS_FILE="+scenariosFile, "-D", "CODEX_HOME="+codexHome)
	out = append(out, argv[insertAt:]...)
	return out, nil
}

// judgeDraftProves runs /usr/bin/true under sb's rendered judge.sb with a
// throwaway scenarios file and Codex home inside a fresh run directory,
// the same shape Load's own proves() uses for build and readonly (fresh,
// generic parameters), failing the test if the profile does not load with
// both extra params supplied.
func judgeDraftProves(t *testing.T, sb Sandbox) {
	t.Helper()
	runDir, cleanup, err := sb.NewRunDir()
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	defer cleanup()

	scenariosFile := filepath.Join(runDir, "scenarios.xml")
	if writeErr := os.WriteFile(scenariosFile, []byte("<scenarios/>\n"), 0o600); writeErr != nil {
		t.Fatalf("write scenarios file: %v", writeErr)
	}
	codexHome := filepath.Join(runDir, "codex-home")
	if mkErr := os.MkdirAll(codexHome, 0o700); mkErr != nil {
		t.Fatalf("mkdir codex home: %v", mkErr)
	}

	p := Params{
		Home: sb.host.Home, Worktree: runDir, RepoGit: runDir, DataDir: sb.host.DataDir, ZingBin: sb.host.ZingBin,
		CacheRoot: sb.host.CacheRoot, CacheShared: sb.host.CacheShared, RunDir: runDir,
		Transcripts: runDir, MDSCache: sb.host.MDSCache,
	}
	argv, err := judgeDraftPrefix(sb, p, scenariosFile, codexHome)
	if err != nil {
		t.Fatalf("judgeDraftPrefix: %v", err)
	}
	argv = append(argv, "/usr/bin/true")

	ctx, cancel := context.WithTimeout(context.Background(), proveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed proof argv from this file's own temp paths
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("draft judge.sb did not load: %v (%s)", runErr, out)
	}
}

// newLoadedJudgeDraftSandbox loads the draft judge.sb against dataDir and
// proves it (judgeDraftProves), failing the test on either step.
func newLoadedJudgeDraftSandbox(t *testing.T, dataDir string) Sandbox {
	t.Helper()
	sb := loadJudgeDraftProfile(t, dataDir)
	judgeDraftProves(t, sb)
	return sb
}

// runJudgeDraftSandboxed runs args in dir (cmd.Dir; "" for the child's
// default) under sb's judge.sb profile with p's params plus the two
// judge-only values, returning the real exit code and combined output,
// the same contract runSandboxed (sandbox_darwin_test.go) and runProbeIn
// (credential_probe_darwin_test.go) already share.
func runJudgeDraftSandboxed(t *testing.T, sb Sandbox, p Params, scenariosFile, codexHome, dir string, args ...string) (exitCode int, output string) {
	t.Helper()
	argv, err := judgeDraftPrefix(sb, p, scenariosFile, codexHome)
	if err != nil {
		t.Fatalf("judgeDraftPrefix: %v", err)
	}
	argv = append(argv, args...)

	ctx, cancel := context.WithTimeout(context.Background(), sandboxCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed test argv from this file's own temp paths, never external input
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) { //nolint:modernize // matches sandbox_darwin_test.go's own comment on errors.As vs errors.AsType
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("run judge-draft sandboxed command %v: %v (output: %s)", args, runErr, out)
	return -1, string(out)
}

// ---- SCENARIOS_FILE and the rest of DATA_DIR: synthetic paths only ----

// TestProbeReadsScenariosFileOnly proves the judge profile's own literal
// SCENARIOS_FILE allow (PKG9-PLAN.md section 7.3, D19) reads exactly the
// one file named, and nothing else in the same directory: a second file
// right next to it stays unreadable, since DATA_DIR's own blanket deny
// covers everywhere under it except that one literal path.
func TestProbeReadsScenariosFileOnly(t *testing.T) {
	dirs := newTestDirs(t)
	sb := newLoadedJudgeDraftSandbox(t, dirs.dataDir)

	runDir := filepath.Join(dirs.dataDir, "judge", "run1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runDir, err)
	}
	scenariosFile := filepath.Join(runDir, "scenarios.xml")
	const marker = "zing-probe-scenario-marker"
	if err := os.WriteFile(scenariosFile, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatalf("write scenarios file: %v", err)
	}
	otherFile := filepath.Join(runDir, "other.xml")
	if err := os.WriteFile(otherFile, []byte("not the scenarios file\n"), 0o600); err != nil {
		t.Fatalf("write other file: %v", err)
	}
	codexHome := filepath.Join(dirs.dataDir, "codex-judge")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}

	p := dirs.params()
	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", "/bin/cat", scenariosFile); exitCode != 0 || !strings.Contains(out, marker) {
		t.Fatalf("cat SCENARIOS_FILE: exit=%d out=%q, want exit 0 containing %q", exitCode, out, marker)
	}
	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", "/bin/cat", otherFile); exitCode == 0 {
		t.Errorf("cat a second file next to SCENARIOS_FILE: want a non-zero exit, got 0 (output %q)", out)
	}
}

// TestProbeDeniesDatabase proves the judge profile still denies zing.db
// and its WAL/SHM siblings under DATA_DIR (PKG9-PLAN.md D19: "No profile
// reads zing.db"), even though SCENARIOS_FILE and CODEX_HOME now carve
// two exceptions into that same DATA_DIR deny.
func TestProbeDeniesDatabase(t *testing.T) {
	dirs := newTestDirs(t)
	sb := newLoadedJudgeDraftSandbox(t, dirs.dataDir)

	scenariosFile := filepath.Join(dirs.dataDir, "judge", "run1", "scenarios.xml")
	if err := os.MkdirAll(filepath.Dir(scenariosFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(scenariosFile, []byte("<scenarios/>\n"), 0o600); err != nil {
		t.Fatalf("write scenarios file: %v", err)
	}
	codexHome := filepath.Join(dirs.dataDir, "codex-judge")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}

	p := dirs.params()
	for _, name := range []string{"zing.db", "zing.db-wal", "zing.db-shm"} {
		f := filepath.Join(dirs.dataDir, name)
		if err := os.WriteFile(f, []byte("not really a database"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
		if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", "/bin/cat", f); exitCode == 0 {
			t.Errorf("cat %s under the judge profile: want a non-zero exit, got 0 (output %q)", name, out)
		}
	}
}

// TestProbeDeniesDataDirOtherFiles proves the judge profile denies every
// other read and write under DATA_DIR, not just zing.db (PKG9-PLAN.md
// section 7.3: "Nothing else in DATA_DIR is readable: not zing.db ...,
// not zing.toml ..., not bin/ beyond ZING_BIN, not another run's scenarios
// file"), and that a plain write anywhere else in DATA_DIR still fails:
// only CODEX_HOME and the run's own SCENARIOS_FILE are carved out.
func TestProbeDeniesDataDirOtherFiles(t *testing.T) {
	dirs := newTestDirs(t)
	sb := newLoadedJudgeDraftSandbox(t, dirs.dataDir)

	scenariosFile := filepath.Join(dirs.dataDir, "judge", "run1", "scenarios.xml")
	if err := os.MkdirAll(filepath.Dir(scenariosFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(scenariosFile, []byte("<scenarios/>\n"), 0o600); err != nil {
		t.Fatalf("write scenarios file: %v", err)
	}
	codexHome := filepath.Join(dirs.dataDir, "codex-judge")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}

	otherRunScenarios := filepath.Join(dirs.dataDir, "judge", "run2", "scenarios.xml")
	if err := os.MkdirAll(filepath.Dir(otherRunScenarios), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(otherRunScenarios, []byte("<scenarios/>\n"), 0o600); err != nil {
		t.Fatalf("write other run's scenarios file: %v", err)
	}
	zingToml := filepath.Join(dirs.dataDir, "zing.toml")
	if err := os.WriteFile(zingToml, []byte("github_token = \"not-real\"\n"), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
	binEntry := filepath.Join(dirs.dataDir, "bin", "something-else")
	if err := os.MkdirAll(filepath.Dir(binEntry), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(binEntry, []byte("not a real binary"), 0o600); err != nil {
		t.Fatalf("write bin entry: %v", err)
	}

	p := dirs.params()
	for _, f := range []string{otherRunScenarios, zingToml, binEntry} {
		if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", "/bin/cat", f); exitCode == 0 {
			t.Errorf("cat %s under the judge profile: want a non-zero exit, got 0 (output %q)", f, out)
		}
	}

	writeTarget := filepath.Join(dirs.dataDir, "canary.txt")
	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", "/usr/bin/touch", writeTarget); exitCode == 0 {
		t.Errorf("touch a file in DATA_DIR outside CODEX_HOME/SCENARIOS_FILE: want a non-zero exit, got 0 (output %q)", out)
	}
	if _, err := os.Stat(writeTarget); err == nil {
		t.Error("a file was created in DATA_DIR despite the deny")
	}
}

// ---- the judge checkout's git write denials: a throwaway repo --------

// TestProbeJudgeGitWritesFail proves the judge profile's git access is
// read-only end to end, inside a judge-shaped checkout (PKG9-PLAN.md
// section 7.3's own table: WORKTREE read and write, REPO_GIT read, write
// denied): git commit, git update-ref, and git push each exit non-zero,
// the repository's refs are unchanged afterwards, and git log -1 still
// succeeds (read access is unaffected). The checkout here uses a plain
// `git worktree add --detach` rather than section 7.3's own
// "--detach --no-checkout": this task does not yet build the judge's own
// checkout (that is M2 task 4, internal/orchestrator/judge.go), and what
// this probe needs is a real tracked file to modify so "git commit -am x"
// fails for the reason under test (the denied write), not for having
// nothing to commit.
func TestProbeJudgeGitWritesFail(t *testing.T) {
	ctx := t.Context()
	repoDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = resolved
	}
	if err := gitfixture.NewSigningRepo(ctx, repoDir); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}
	shaOut, err := gitfixture.Git(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v (%s)", err, shaOut)
	}
	headSHA := strings.TrimSpace(string(shaOut))

	worktreeDir := filepath.Join(t.TempDir(), "judge-checkout")
	if out, worktreeErr := gitfixture.Git(ctx, repoDir, "worktree", "add", "--detach", worktreeDir, headSHA); worktreeErr != nil {
		t.Fatalf("git worktree add: %v (%s)", worktreeErr, out)
	}
	if resolved, evalErr := filepath.EvalSymlinks(worktreeDir); evalErr == nil {
		worktreeDir = resolved
	}

	repoGitOut, err := gitfixture.Git(ctx, worktreeDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		t.Fatalf("rev-parse --git-common-dir: %v (%s)", err, repoGitOut)
	}
	repoGit := strings.TrimSpace(string(repoGitOut))

	originDir := t.TempDir()
	if out, initErr := gitfixture.Git(ctx, originDir, "init", "-q", "--bare"); initErr != nil {
		t.Fatalf("git init --bare: %v (%s)", initErr, out)
	}
	if out, remoteErr := gitfixture.Git(ctx, repoDir, "remote", "add", "origin", originDir); remoteErr != nil {
		t.Fatalf("git remote add: %v (%s)", remoteErr, out)
	}

	refsBefore, err := gitfixture.Git(ctx, repoDir, "for-each-ref")
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}

	dirs := newTestDirs(t)
	sb := newLoadedJudgeDraftSandbox(t, dirs.dataDir)
	p, err := sb.ParamsFor(worktreeDir, repoGit, dirs.runDir)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	p.Home = dirs.home
	p.DataDir = dirs.dataDir
	p.ZingBin = dirs.zingBin
	p.CacheRoot = dirs.cacheRoot
	p.CacheShared = dirs.cacheShared
	p.MDSCache = dirs.mdsCache

	scenariosFile := filepath.Join(dirs.dataDir, "judge", "run1", "scenarios.xml")
	if mkErr := os.MkdirAll(filepath.Dir(scenariosFile), 0o700); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if writeErr := os.WriteFile(scenariosFile, []byte("<scenarios/>\n"), 0o600); writeErr != nil {
		t.Fatalf("write scenarios file: %v", writeErr)
	}
	codexHome := filepath.Join(dirs.dataDir, "codex-judge")
	if mkErr := os.MkdirAll(codexHome, 0o700); mkErr != nil {
		t.Fatalf("mkdir codex home: %v", mkErr)
	}

	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, worktreeDir,
		"/bin/sh", "-c", "echo change >> README.md && git commit -am x"); exitCode == 0 {
		t.Errorf("git commit -am x under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}
	if changed, readErr := os.ReadFile(filepath.Join(worktreeDir, "README.md")); readErr != nil || !strings.Contains(string(changed), "change") {
		t.Errorf("the worktree write itself (echo >> README.md, which WORKTREE allows) did not happen: err=%v content=%q", readErr, changed)
	}

	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, worktreeDir,
		"git", "update-ref", "refs/heads/zing-probe", "HEAD"); exitCode == 0 {
		t.Errorf("git update-ref under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}

	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, worktreeDir,
		"git", "push", "origin", "HEAD"); exitCode == 0 {
		t.Errorf("git push origin HEAD under the judge profile: want a non-zero exit, got 0 (output %q)", out)
	}

	if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, worktreeDir,
		"git", "log", "-1"); exitCode != 0 {
		t.Errorf("git log -1 under the judge profile: exit %d, want 0 (output %q)", exitCode, out)
	}

	refsAfter, err := gitfixture.Git(ctx, repoDir, "for-each-ref")
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	if !bytes.Equal(refsBefore, refsAfter) {
		t.Errorf("refs changed after the denied writes:\nbefore:\n%s\nafter:\n%s", refsBefore, refsAfter)
	}
}

// ---- credential probes repeated under the judge profile: real HOME ---
//
// Every test below this point reads real paths under the real HOME or
// runs a real credential-bearing CLI, and so is gated behind
// requireLiveProbe (ZING_LIVE_CLI=1, set by the owner, never by this
// harness or CI), mirroring credential_probe_darwin_test.go's own gate on
// the same family of probe.

// judgeCredentialProbeSetup loads the draft judge profile against a fresh
// temp data directory and builds Params from the real host HOME
// (ParamsFor fills Home, DataDir, ZingBin, CacheRoot, CacheShared, and
// MDSCache from the Sandbox's own host; see probeParams in
// credential_probe_darwin_test.go for the same pattern) plus a throwaway
// SCENARIOS_FILE and CODEX_HOME under that temp data directory.
func judgeCredentialProbeSetup(t *testing.T) (sb Sandbox, p Params, scenariosFile, codexHome string) {
	t.Helper()
	dirs := newTestDirs(t)
	sb = newLoadedJudgeDraftSandbox(t, dirs.dataDir)

	var err error
	p, err = sb.ParamsFor(dirs.worktree, dirs.repoGit, dirs.runDir)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}

	scenariosFile = filepath.Join(dirs.dataDir, "judge", "run1", "scenarios.xml")
	if mkErr := os.MkdirAll(filepath.Dir(scenariosFile), 0o700); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if writeErr := os.WriteFile(scenariosFile, []byte("<scenarios/>\n"), 0o600); writeErr != nil {
		t.Fatalf("write scenarios file: %v", writeErr)
	}
	codexHome = filepath.Join(dirs.dataDir, "codex-judge")
	if mkErr := os.MkdirAll(codexHome, 0o700); mkErr != nil {
		t.Fatalf("mkdir codex home: %v", mkErr)
	}
	return sb, p, scenariosFile, codexHome
}

// runJudgeProbe is runJudgeDraftSandboxed with an explicit environment and
// timeout, mirroring runProbeIn (credential_probe_darwin_test.go) for the
// probes in this section that need a real credential-bearing environment
// rather than the plain inherited one.
func runJudgeProbe(t *testing.T, sb Sandbox, p Params, scenariosFile, codexHome, dir string, env []string, timeout time.Duration, args ...string) (exitCode int, output string) {
	t.Helper()
	argv, err := judgeDraftPrefix(sb, p, scenariosFile, codexHome)
	if err != nil {
		t.Fatalf("judgeDraftPrefix: %v", err)
	}
	argv = append(argv, args...)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed probe argv, no external input
	cmd.Dir = dir
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) { //nolint:modernize // see runJudgeDraftSandboxed
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("run judge probe command %v: %v (output length %d)", args, runErr, len(out))
	return -1, ""
}

// runJudgeProbeStdin is runJudgeProbe plus an stdin reader, mirroring
// runProbeStdin for the one judge credential probe that needs it
// (TestProbeCredentialFillEmptyJudge). Like runProbeStdin, only the
// output text matters to its callers, never the exit code, so a
// non-zero exit is not itself fatal here.
func runJudgeProbeStdin(t *testing.T, sb Sandbox, p Params, scenariosFile, codexHome, dir string, env []string, stdin string, args ...string) (output string) {
	t.Helper()
	argv, err := judgeDraftPrefix(sb, p, scenariosFile, codexHome)
	if err != nil {
		t.Fatalf("judgeDraftPrefix: %v", err)
	}
	argv = append(argv, args...)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: see runJudgeProbe
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) { //nolint:modernize // see runJudgeDraftSandboxed
		return string(out)
	}
	t.Fatalf("run judge probe command %v: %v (output length %d)", args, runErr, len(out))
	return ""
}

// TestProbeJudgeDeniesClaudeAndCodexState proves the judge-only deny
// block (PKG9-PLAN.md section 7.3, N6) wins over build.sb's own ~/.claude
// allow: a planted transcript under the real ~/.claude/projects, the real
// ~/.claude.json, and the real ~/.codex/auth.json (whichever of these
// exist on this host) are each unreadable under the judge profile. Only
// the length of a denied read is ever logged, never its content.
func TestProbeJudgeDeniesClaudeAndCodexState(t *testing.T) {
	requireLiveProbe(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	marker := probeUUID(t)
	otherProjectDir := filepath.Join(home, ".claude", "projects", "zing-judge-probe-"+marker)
	if mkErr := os.MkdirAll(otherProjectDir, 0o755); mkErr != nil { //nolint:gosec // 0755: matches the CLI's own transcript directory mode
		t.Fatalf("mkdir %s: %v", otherProjectDir, mkErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(otherProjectDir) })
	transcript := filepath.Join(otherProjectDir, probeUUID(t)+".jsonl")
	if writeErr := os.WriteFile(transcript, []byte(`{"marker":"`+marker+`"}`+"\n"), 0o600); writeErr != nil {
		t.Fatalf("write %s: %v", transcript, writeErr)
	}

	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)

	for _, target := range []string{
		transcript,
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".codex", "auth.json"),
	} {
		if _, statErr := os.Stat(target); statErr != nil {
			t.Logf("skip %s: %v (not present on this host)", target, statErr)
			continue
		}
		if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", "/bin/cat", target); exitCode == 0 {
			t.Errorf("cat %s under the judge profile: want a non-zero exit, got 0 (output length %d)", target, len(out))
		}
	}
}

// TestProbeDeniesTokenFiles proves the judge profile denies ~/.zing
// (zing.toml holds the GitHub token), ~/.ssh, and ~/.config, none of
// which any profile's home-read allow ever named (PKG9-PLAN.md section
// 7.3's own list of what stays closed). Only the length of a denied read
// is ever logged, never its content.
func TestProbeDeniesTokenFiles(t *testing.T) {
	requireLiveProbe(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)

	for _, target := range []string{
		filepath.Join(home, ".zing", "zing.toml"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".config"),
	} {
		info, statErr := os.Stat(target)
		if statErr != nil {
			t.Logf("skip %s: %v (not present on this host)", target, statErr)
			continue
		}
		readArgs := []string{"/bin/cat", target}
		if info.IsDir() {
			readArgs = []string{"/bin/ls", target}
		}
		if exitCode, out := runJudgeDraftSandboxed(t, sb, p, scenariosFile, codexHome, "", readArgs...); exitCode == 0 {
			t.Errorf("read %s under the judge profile: want a non-zero exit, got 0 (output length %d)", target, len(out))
		}
	}
}

// TestProbeGhAuthTokenEmptyJudge repeats M1 task 7's own
// TestProbeGhAuthTokenEmpty under the judge profile (PKG9-PLAN.md section
// 19.3): `gh auth token` must still fail closed.
func TestProbeGhAuthTokenEmptyJudge(t *testing.T) {
	requireLiveProbe(t)
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not on PATH")
	}

	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)
	env := probeEnv(sb, p)
	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, "", env, probeTimeout, "gh", "auth", "token")
	if exitCode == 0 && strings.TrimSpace(out) != "" {
		t.Errorf("gh auth token under the judge profile: exit 0 with %d bytes of output, want non-zero exit or empty output", len(strings.TrimSpace(out)))
	}
}

// TestProbeKeychainDeniedJudge repeats M1 task 7's own
// TestProbeKeychainDenied under the judge profile.
func TestProbeKeychainDeniedJudge(t *testing.T) {
	requireLiveProbe(t)

	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)
	env := probeEnv(sb, p)
	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, "", env, probeTimeout, "security", "find-internet-password", "-s", "github.com")
	if exitCode == 0 {
		t.Errorf("security find-internet-password under the judge profile: exit 0, want non-zero")
	}
	if strings.Contains(strings.ToLower(out), "password") {
		t.Error("output mentions a password despite the deny")
	}
}

// TestProbeCredentialFillEmptyJudge repeats M1 task 7's own
// TestProbeCredentialFillEmpty under the judge profile, across the same
// three credential.helper spellings.
func TestProbeCredentialFillEmptyJudge(t *testing.T) {
	requireLiveProbe(t)

	helpers := []string{"osxkeychain", gitCredentialOsxkeychainPath(t), "!gh auth git-credential"}
	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)
	env := probeEnv(sb, p)
	for _, helper := range helpers {
		t.Run(helper, func(t *testing.T) {
			out := runJudgeProbeStdin(t, sb, p, scenariosFile, codexHome, "", env, probeCredentialStdin,
				"git", "-c", "credential.helper="+helper, "credential", "fill")
			if strings.Contains(out, "password=") {
				t.Errorf("git credential fill (helper=%s) under the judge profile printed a password= line", helper)
			}
		})
	}
}

// TestProbeSSHAgentDeniedJudge repeats M1 task 7's own
// TestProbeSSHAgentDenied under the judge profile, against the checked-in
// candidate rule build.sb and judge.sb both currently carry.
func TestProbeSSHAgentDeniedJudge(t *testing.T) {
	requireLiveProbe(t)
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		t.Skip("SSH_AUTH_SOCK is not set; no real ssh-agent to probe against")
	}
	if _, err := exec.LookPath("ssh-add"); err != nil {
		t.Skip("ssh-add is not on PATH")
	}

	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)
	env := probeEnv(sb, p)
	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, "", env, probeTimeout, "ssh-add", "-l")
	t.Logf("ssh-add -l under the judge profile: exit=%d output_len=%d", exitCode, len(out))
	if exitCode != 2 {
		t.Errorf("ssh-add -l did not get blocked under the judge profile: exit=%d, want 2 (cannot connect)", exitCode)
	}
}

// TestProbeCredentialHelperDeniedJudge seeds a repo-local
// credential.helper store carrying a fake, unique token, and points it at
// an httptest TLS server that requires basic auth and records every
// Authorization header it receives, then proves `git ls-remote` under the
// judge profile exits non-zero and the server records no credential: the
// process-exec deny on git-credential-* (D26, N2) stops the helper from
// ever running, so git authenticates with nothing at all.
func TestProbeCredentialHelperDeniedJudge(t *testing.T) {
	requireLiveProbe(t)

	var mu sync.Mutex
	var authHeaders []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")

	repoDir := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	credFile := filepath.Join(repoDir, ".git-credentials")
	const seededToken = "seeded-fake-token-for-judge-probe-only"
	if err := os.WriteFile(credFile, []byte("https://x-access-token:"+seededToken+"@"+host+"\n"), 0o600); err != nil {
		t.Fatalf("write seeded credentials file: %v", err)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "-C", repoDir, "config", "credential.helper", "store --file="+credFile).CombinedOutput(); err != nil {
		t.Fatalf("git config credential.helper: %v (%s)", err, out)
	}

	sb, p, scenariosFile, codexHome := judgeCredentialProbeSetup(t)
	env := append(probeEnv(sb, p), "GIT_SSL_NO_VERIFY=true")

	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, repoDir, env, probeTimeout,
		"git", "-c", "http.sslVerify=false", "ls-remote", srv.URL+"/r")
	if exitCode == 0 {
		t.Errorf("git ls-remote under the judge profile: want a non-zero exit (no credential reached the server), got 0 (output %q)", out)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, h := range authHeaders {
		if h != "" {
			t.Errorf("the server recorded a non-empty Authorization header: the seeded credential reached it (%d bytes)", len(h))
		}
	}
}

// ---- Codex under the judge profile: needs a real judge Codex login ----

// probeJudgeCodexHome reads the real, already-logged-in judge Codex home
// (the owner's own "CODEX_HOME=<dir> codex login", D27) this probe runs
// Codex against, from ZING_PROBE_JUDGE_CODEX_HOME, skipping with a clear
// message when it is empty. This file never reads ~/.zing itself; only
// the owner's own env var ever names a real Codex home.
func probeJudgeCodexHome(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("ZING_PROBE_JUDGE_CODEX_HOME")
	if dir == "" {
		t.Skip("set ZING_PROBE_JUDGE_CODEX_HOME to an already-logged-in Codex home (CODEX_HOME=<dir> codex login) to run this probe")
	}
	return dir
}

// requireCodex skips t when the codex binary is not on PATH.
func requireCodex(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not on PATH")
	}
}

// parseFirstCodexSessionID scans raw (a codex exec --json JSONL stream)
// for the first event carrying thread_id or session_id, mirroring
// internal/runtime's own firstCodexSessionID without importing that
// package into this one.
func parseFirstCodexSessionID(raw string) string {
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			ThreadID  string `json:"thread_id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.ThreadID != "" {
			return ev.ThreadID
		}
		if ev.SessionID != "" {
			return ev.SessionID
		}
	}
	return ""
}

// TestProbeNestedSeatbeltFails proves Codex's own `-s read-only` sandbox
// cannot start a second time inside the judge profile's own seatbelt
// (PKG9-PLAN.md D20: "a seatbelt cannot start inside another"): `codex
// exec -s read-only` running `echo ok` fails.
func TestProbeNestedSeatbeltFails(t *testing.T) {
	requireLiveProbe(t)
	requireCodex(t)
	codexHome := probeJudgeCodexHome(t)

	sb, p, scenariosFile, _ := judgeCredentialProbeSetup(t)
	env := append(probeEnv(sb, p), "CODEX_HOME="+codexHome)

	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, p.Worktree, env, probeTimeout,
		"codex", "exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "-s", "read-only", "echo ok")
	if exitCode == 0 {
		t.Errorf("codex exec -s read-only inside the judge profile: want a non-zero exit (nested seatbelt), got 0 (output length %d)", len(out))
	}
}

// TestProbeCodexRunsWithFullAccessInsideProfile proves Codex runs under
// Zing's own seatbelt with its own sandboxing turned off (D20:
// `-s danger-full-access`, only ever passed with a non-empty
// RunRequest.ExecPrefix): `go env GOCACHE` succeeds and the reply
// includes a path under the run directory, the sandbox's own GOCACHE
// redirection (section 5.3).
func TestProbeCodexRunsWithFullAccessInsideProfile(t *testing.T) {
	requireLiveProbe(t)
	requireCodex(t)
	codexHome := probeJudgeCodexHome(t)

	sb, p, scenariosFile, _ := judgeCredentialProbeSetup(t)
	env := append(probeEnv(sb, p), "CODEX_HOME="+codexHome)
	wantGOCACHE := filepath.Join(p.CacheShared, "go-build")

	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, p.Worktree, env, probeHeavyTimeout,
		"codex", "exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "-s", "danger-full-access",
		"Run `go env GOCACHE` and reply with exactly its output and nothing else.")
	if exitCode != 0 {
		t.Fatalf("codex exec -s danger-full-access inside the judge profile: exit %d, want 0 (output length %d)", exitCode, len(out))
	}
	if !strings.Contains(out, wantGOCACHE) {
		t.Errorf("codex's reply did not include the sandbox's own GOCACHE (%s); the run directory redirection may not be reaching it", wantGOCACHE)
	}
}

// TestProbeCodexHomeLoginAndResume proves Codex logs in and resumes from
// the judge's own dedicated CODEX_HOME, with com.apple.SecurityServer
// denied and ~/.codex unreadable (PKG9-PLAN.md D27, 7.3): a first turn
// succeeds and writes a session file under <judge_codex_home>/sessions
// (proving the CODEX_HOME write allow after the global deny), and `codex
// exec resume` of that session succeeds and writes there again.
func TestProbeCodexHomeLoginAndResume(t *testing.T) {
	requireLiveProbe(t)
	requireCodex(t)
	codexHome := probeJudgeCodexHome(t)

	sb, p, scenariosFile, _ := judgeCredentialProbeSetup(t)
	env := append(probeEnv(sb, p), "CODEX_HOME="+codexHome)
	sessionsDir := filepath.Join(codexHome, "sessions")

	exitCode, out := runJudgeProbe(t, sb, p, scenariosFile, codexHome, p.Worktree, env, probeTimeout,
		"codex", "exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "-s", "danger-full-access", "--json",
		"Say ok and nothing else.")
	if exitCode != 0 {
		t.Fatalf("codex exec first turn under the judge profile: exit %d, want 0 (output length %d)", exitCode, len(out))
	}
	sessionID := parseFirstCodexSessionID(out)
	if sessionID == "" {
		t.Fatal("no session id found in the first turn's JSONL output")
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("read %s after the first turn: entries=%d err=%v (the CODEX_HOME write allow may not be reaching it)", sessionsDir, len(entries), err)
	}

	exitCode, out = runJudgeProbe(t, sb, p, scenariosFile, codexHome, p.Worktree, env, probeTimeout,
		"codex", "exec", "resume", sessionID, "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check",
		"-c", `sandbox_mode="danger-full-access"`, "--json", "Say ok again.")
	if exitCode != 0 {
		t.Fatalf("codex exec resume under the judge profile: exit %d, want 0 (output length %d)", exitCode, len(out))
	}
}
