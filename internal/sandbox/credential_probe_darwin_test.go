//go:build darwin

// credential_probe_darwin_test.go runs PKG9-PLAN.md section 4.7 and 7.3's
// own host probes (M1 task 7, D26): the owner's own real credentials --
// gh's keychain-stored token, the keychain itself, ssh-agent, a real
// claude login -- against the changed build.sb and readonly.sb, on this
// build host, before anything else in the credential boundary is trusted.
//
// Every test here:
//
//   - is skipped unless ZING_LIVE_CLI=1 (set by the owner, never by this
//     harness or CI): several of these probes spend real Claude usage and
//     all of them touch the owner's real credentials.
//   - is skipped when ZING_SANDBOXED=1 (requireNotSandboxed,
//     sandbox_darwin_test.go): a sandboxed process cannot itself start
//     sandbox-exec.
//   - never prints a credential: an assertion checks for a non-zero exit,
//     an empty result, the absence of a "password=" line, or a byte
//     count/length, never the credential's own text.
//
// The owner has no ~/.zing/zing.toml yet (D26's own setup step, "the owner
// runs claude setup-token once and puts the token in zing.toml", has not
// happened for this host), so every probe that needs to log Claude in
// reads the token from ZING_PROBE_CLAUDE_TOKEN instead, never zing.toml,
// and skips with a clear message when it is empty; the token is never
// printed or logged. Run the whole file explicitly:
//
//	ZING_LIVE_CLI=1 ZING_PROBE_CLAUDE_TOKEN=<token> go test ./internal/sandbox/ -run TestProbe -v
//
// A failing probe is the early exit design section 19.1 and this task's
// own instructions name: stop and report, do not work around it.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	zing "zing"
	"zing/internal/gitfixture"
	"zing/internal/response"
	"zing/internal/runtime"
)

// requireLiveProbe is the gate every test in this file opens with: ZING_LIVE_CLI
// must be exactly "1", set by the owner. It also calls requireNotSandboxed
// (sandbox_darwin_test.go), since a sandboxed process cannot itself start
// sandbox-exec.
func requireLiveProbe(t *testing.T) {
	t.Helper()
	if os.Getenv("ZING_LIVE_CLI") != "1" {
		t.Skip("set ZING_LIVE_CLI=1 to run the credential boundary probes against the real host")
	}
	requireNotSandboxed(t)
}

// probeClaudeOAuthToken reads the Claude token these probes log in with
// from ZING_PROBE_CLAUDE_TOKEN (never zing.toml: the owner has none yet),
// skipping with a clear message when it is empty. It is never printed or
// logged.
func probeClaudeOAuthToken(t *testing.T) string {
	t.Helper()
	token := os.Getenv("ZING_PROBE_CLAUDE_TOKEN")
	if token == "" {
		t.Skip("set ZING_PROBE_CLAUDE_TOKEN (the output of `claude setup-token`) to run this probe")
	}
	return token
}

// probeTimeout bounds every ordinary probe command; probeHeavyTimeout
// bounds the handful that run a real build or test suite.
const (
	probeTimeout      = 60 * time.Second
	probeHeavyTimeout = 10 * time.Minute
)

// probeModel is the model every probe's own real claude call uses: these
// calls only prove the CLI can start and log in, never anything about
// model quality, so it matches machine.toml's own planning job model
// (opus, claude-opus-5-5) -- consistent with the "planning-style" framing these probes
// already use, and known to be a currently-supported, resolvable model id
// (unlike the previously hardcoded claude-3-5-haiku-20241022, retired
// February 19, 2026, which failed every probe here with "exited 1" and
// a deprecation notice, not a sandbox issue).
const probeModel = "claude-opus-5-5"

// probeSandboxes loads both changed profiles (build.sb and readonly.sb)
// for a probe to run under each in turn, failing the test if either does
// not load on this host.
func probeSandboxes(t *testing.T) map[string]Sandbox {
	t.Helper()
	return map[string]Sandbox{
		profileNameBuild:    newLoadedSandbox(t, nil, 7420),
		profileNameReadOnly: newLoadedReadonlySandbox(t),
	}
}

// probeParams builds Params for sb with the real host HOME (sb.host.Home,
// resolved by Load/LoadProfile through os.UserHomeDir) and throwaway
// WORKTREE/REPO_GIT/RUN_DIR/TRANSCRIPTS directories: every probe in this
// file is about the credential and home-read rules, which key off the
// real HOME, not about worktree or transcript isolation, which the rest
// of this package's own suite (sandbox_darwin_test.go) already covers
// with a fake HOME.
func probeParams(t *testing.T, sb Sandbox) Params {
	t.Helper()
	dirs := newTestDirs(t)
	p, err := sb.ParamsFor(dirs.worktree, dirs.repoGit, dirs.runDir)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	return p
}

// probeEnv is os.Environ() minus git's repository-location variables
// (gitfixture.Environ, so a GIT_DIR a git hook exported cannot send a
// probe's git at the real repository) (so PATH, HOME, and every ambient credential
// path like SSH_AUTH_SOCK reach the child) plus sb's own Env(p, ...)
// overrides (TMPDIR, GIT_CONFIG_GLOBAL, ZING_SANDBOXED, and so on),
// appended last so they win on a duplicate name (os/exec keeps the last
// value for a repeated name), mirroring sandbox_darwin_test.go's own
// TestChildSeesSandboxTmpdir.
func probeEnv(sb Sandbox, p Params) []string {
	return append(gitfixture.Environ(), sb.Env(p, os.Getenv("PATH"))...)
}

// runProbe runs args under sb's profile with p's params and env, bounded
// by probeTimeout, returning the real exit code and combined output. A
// caller whose command could show a credential in that output must not
// print it; see each test's own assertion.
func runProbe(t *testing.T, sb Sandbox, p Params, env []string, args ...string) (exitCode int, output string) {
	t.Helper()
	return runProbeIn(t, sb, p, "", env, probeTimeout, args...)
}

// runProbeIn is runProbe with an explicit working directory (dir == ""
// means the child's own default) and timeout, for the heavier probes that
// must run from a real checkout or need longer than probeTimeout.
func runProbeIn(t *testing.T, sb Sandbox, p Params, dir string, env []string, timeout time.Duration, args ...string) (exitCode int, output string) {
	t.Helper()
	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	argv = append(argv, args...)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: probe argv is built from this file's own fixed commands, never external input
	cmd.Dir = dir
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) { //nolint:modernize // matches sandbox_darwin_test.go's own comment on errors.As vs errors.AsType
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("run probe command %v: %v (output length %d)", args, runErr, len(out))
	return -1, ""
}

// runProbeStdin is runProbeIn plus an stdin reader (`git credential fill`'s
// own protocol=.../host=... input), run in dir. Its two callers only ever
// inspect the output text (a leaked password= or seeded-token line), never
// the exit code, so unlike runProbeIn it returns output alone; a non-zero
// exit is deliberately not fatal here, only *exec.ExitError's own missing
// (never-started) case is.
func runProbeStdin(t *testing.T, sb Sandbox, p Params, dir string, env []string, stdin string, args ...string) (output string) {
	t.Helper()
	argv, err := sb.Prefix(p)
	if err != nil {
		t.Fatalf("Prefix: %v", err)
	}
	argv = append(argv, args...)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // see runProbeIn
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) { //nolint:modernize // see runProbeIn
		return string(out)
	}
	t.Fatalf("run probe command %v: %v (output length %d)", args, runErr, len(out))
	return ""
}

// probeCredentialStdin is the fixed stdin every git credential fill probe
// in this file feeds it: protocol=https, host=github.com.
const probeCredentialStdin = "protocol=https\nhost=github.com\n\n"

// probeUUID returns a fresh v4 UUID (crypto/rand), for a claude
// --session-id or a unique, non-secret marker string. It is never a real
// credential.
func probeUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// claudeProbeTools is the tool list every claude probe run request carries:
// machine.toml's own planning job entry, the job these probes simulate
// ("planning-style" turns, PKG9-PLAN.md section 7.3), so Claude.Command
// builds the same --tools/--allowedTools flags a real run would.
var claudeProbeTools = []string{"read", "grep", "glob", "bash_readonly"}

// claudeTokenPattern matches an sk-ant- shaped token, so a probe failure's
// logged stderr never carries a live credential (D26, N2).
var claudeTokenPattern = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)

// redactClaudeToken returns b's text with any sk-ant- token replaced by a
// fixed placeholder, for a probe failure's own stderr log.
func redactClaudeToken(b []byte) string {
	return claudeTokenPattern.ReplaceAllString(string(b), "sk-ant-[REDACTED]")
}

// firstBytes returns the first n bytes of b, or all of it when shorter.
func firstBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// probeClaudeTurn runs one real claude turn through internal/runtime's own
// Claude.Command and Claude.Env (PKG9-PLAN.md M1 task 7): the exact argv
// and environment production's Claude.Run builds, not a hand-rolled
// approximation that can silently drift from it and fail for reasons
// unrelated to the sandbox under test. prefix is the sandbox-exec prefix
// (sb.Prefix(p)) for a sandboxed run, or nil to run claude directly, as
// TestProbeNoScenarioLeak's own unsandboxed control run does; extraEnv
// rides in req.Env, mirroring applySandbox's sb.Env(p, PATH) for a
// sandboxed run or applyPrivateTempRoot's private TMPDIR/
// CLAUDE_CODE_TMPDIR pair for an unsandboxed one (internal/job/runjob.go).
// resumeSessionID == "" is a first turn (a fresh session id is minted and
// returned); otherwise it resumes that session. It fails the test outright
// (the early-exit contract this whole file follows) on a non-zero exit,
// logging stderr's first 400 bytes with any sk-ant- token redacted.
func probeClaudeTurn(t *testing.T, token string, prefix, extraEnv []string, dir, label, resumeSessionID string) (sessionID string) {
	t.Helper()
	claude := runtime.NewClaude("", token)
	req := runtime.RunRequest{
		Job:        response.JobPlanning,
		Model:      probeModel,
		Prompt:     "Say ok and nothing else.",
		Tools:      claudeProbeTools,
		WorkDir:    dir,
		Env:        extraEnv,
		RunToken:   "probe",
		SessionID:  resumeSessionID,
		ExecPrefix: prefix,
	}
	name, args, sessionID, err := claude.Command(req)
	if err != nil {
		t.Fatalf("%s: build command: %v", label, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: argv is built by runtime.Claude.Command from this file's own fixed request, never external input
	cmd.Dir = dir
	cmd.Env = claude.Env(req)
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if runErr := cmd.Run(); runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) { //nolint:modernize // matches this package's own errors.As vs errors.AsType comment
			t.Fatalf("%s: start claude: %v", label, runErr)
		}
		exitCode = exitErr.ExitCode()
	}
	if exitCode != 0 {
		t.Logf("%s: stderr (first 400 bytes, redacted): %s", label, redactClaudeToken(firstBytes(stderr.Bytes(), 400)))
		t.Fatalf("EARLY EXIT (D26): %s exited %d (stdout length %d); stop and report, do not work around this", label, exitCode, stdout.Len())
	}
	return sessionID
}

// ---- TestProbeClaudeLoginWithToken -----------------------------------------

// TestProbeClaudeLoginWithToken proves claude -p's first turn and resume
// both succeed with CLAUDE_CODE_OAUTH_TOKEN set and no SecurityServer,
// under each changed profile (PKG9-PLAN.md section 7.3, D26). A failure
// here is the early exit: stop and report, no workaround.
func TestProbeClaudeLoginWithToken(t *testing.T) {
	requireLiveProbe(t)
	token := probeClaudeOAuthToken(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			if err := os.MkdirAll(p.Transcripts, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", p.Transcripts, err)
			}
			prefix, err := sb.Prefix(p)
			if err != nil {
				t.Fatalf("Prefix: %v", err)
			}
			extraEnv := sb.Env(p, os.Getenv("PATH"))

			sessionID := probeClaudeTurn(t, token, prefix, extraEnv, p.Worktree, "claude -p first turn under "+name, "")
			probeClaudeTurn(t, token, prefix, extraEnv, p.Worktree, "claude -p resume under "+name, sessionID)
		})
	}
}

// ---- TestProbeTLSWithoutSecurityServer -------------------------------------

// TestProbeTLSWithoutSecurityServer proves TLS still works (go mod
// download of a fresh module, and curl to proxy.golang.org) under the
// checked-in build.sb, with com.apple.SecurityServer denied and
// com.apple.trustd.agent absent from its mach-lookup allow list
// (sandbox/build.sb's own comment records why: Package 8 found both
// clean). tlsProbeProfile fails this test first if either is allowed
// again, before any TLS command runs.
func TestProbeTLSWithoutSecurityServer(t *testing.T) {
	requireLiveProbe(t)

	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read sandbox/build.sb: %v", err)
	}
	profile, err = tlsProbeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}

	sb := LoadProfile(profileNameBuild, profile, t.TempDir(), nil, 7420)
	if !sb.Available() {
		t.Fatalf("LoadProfile: unavailable, reason %q", sb.Reason())
	}
	p := probeParams(t, sb)
	env := probeEnv(sb, p)

	// modDir must be a sandbox-writable location, not an arbitrary
	// t.TempDir(): `go mod download` writes go.sum back into its own
	// working directory, and a dir outside the profile's own
	// writable set (WORKTREE, TRANSCRIPTS, MDS_CACHE, RUN_DIR,
	// CACHE_SHARED, /dev) fails that write with EPERM regardless of
	// whether TLS itself works -- the bug this probe first turned
	// up (go: updating go.sum: ... operation not permitted), unrelated
	// to trustd.agent or GOPATH/GOMODCACHE (both already point at
	// CACHE_SHARED's own writable subfolders through sb.Env()).
	modDir := filepath.Join(p.Worktree, "probe-mod")
	if mkErr := os.MkdirAll(modDir, 0o700); mkErr != nil {
		t.Fatalf("mkdir %s: %v", modDir, mkErr)
	}
	goMod := "module probe.invalid/m\n\ngo 1.23\n\nrequire rsc.io/quote v1.5.2\n"
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	modExit, modOut := runProbeIn(t, sb, p, modDir, env, probeTimeout, "go", "mod", "download")
	curlExit, _ := runProbe(t, sb, p, env, "curl", "-fsS", "--max-time", "15", "-o", os.DevNull, "https://proxy.golang.org")
	t.Logf("go mod download exit=%d, curl exit=%d", modExit, curlExit)

	if modExit != 0 {
		t.Errorf("go mod download exit=%d, want 0 (output %q)", modExit, modOut)
	}
	if curlExit != 0 {
		t.Errorf("curl exit=%d, want 0", curlExit)
	}
}

// tlsProbeProfile returns profile (sandbox/build.sb's text) unchanged when
// its mach-lookup allow list names neither com.apple.SecurityServer nor
// com.apple.trustd.agent, the state build.sb's own comment records (D26,
// Package 8), and an error naming the first one it finds otherwise.
func tlsProbeProfile(profile []byte) ([]byte, error) {
	for _, name := range []string{"com.apple.SecurityServer", "com.apple.trustd.agent"} {
		if strings.Contains(string(profile), `(global-name "`+name+`")`) {
			return nil, fmt.Errorf("sandbox/build.sb allows mach-lookup of %s again; update this probe", name)
		}
	}
	return profile, nil
}

// TestTLSProbeMatchesBuildProfile runs without ZING_LIVE_CLI or sandbox-exec:
// it proves TestProbeTLSWithoutSecurityServer's own profile check accepts
// the checked-in build.sb and rejects it with either Mach service added
// back, so the live probe and the profile cannot drift apart unseen.
func TestTLSProbeMatchesBuildProfile(t *testing.T) {
	t.Parallel()
	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read sandbox/build.sb: %v", err)
	}
	if _, err := tlsProbeProfile(profile); err != nil {
		t.Fatalf("checked-in build.sb: %v", err)
	}
	for _, name := range []string{"com.apple.SecurityServer", "com.apple.trustd.agent"} {
		readded := strings.Replace(string(profile), `(global-name "com.apple.system.opendirectoryd.libinfo")`,
			`(global-name "com.apple.system.opendirectoryd.libinfo") (global-name "`+name+`")`, 1)
		if _, err := tlsProbeProfile([]byte(readded)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("build.sb with %s added back: err = %v, want an error naming it", name, err)
		}
	}
}

// ---- TestProbeGhAuthTokenEmpty ---------------------------------------------

// TestProbeGhAuthTokenEmpty proves `gh auth token` cannot reach the
// keychain-stored token under either profile: non-zero exit, or empty
// output (PKG9-PLAN.md D26, N2).
func TestProbeGhAuthTokenEmpty(t *testing.T) {
	requireLiveProbe(t)
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not on PATH")
	}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			env := probeEnv(sb, p)
			exitCode, out := runProbe(t, sb, p, env, "gh", "auth", "token")
			if exitCode == 0 && strings.TrimSpace(out) != "" {
				t.Errorf("gh auth token under %s: exit 0 with %d bytes of output, want non-zero exit or empty output", name, len(strings.TrimSpace(out)))
			}
		})
	}
}

// ---- TestProbeKeychainDenied -----------------------------------------------

// TestProbeKeychainDenied proves `security find-internet-password` cannot
// reach the keychain under either profile: non-zero exit and no password
// in the output (PKG9-PLAN.md D26, N2).
func TestProbeKeychainDenied(t *testing.T) {
	requireLiveProbe(t)

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			env := probeEnv(sb, p)
			exitCode, out := runProbe(t, sb, p, env, "security", "find-internet-password", "-s", "github.com")
			if exitCode == 0 {
				t.Errorf("security find-internet-password under %s: exit 0, want non-zero", name)
			}
			if strings.Contains(strings.ToLower(out), "password") {
				t.Error("output mentions a password despite the deny")
			}
		})
	}
}

// ---- TestProbeCredentialFillEmpty ------------------------------------------

// gitCredentialOsxkeychainPath resolves git's own exec-path and returns
// the absolute path of git-credential-osxkeychain there, skipping the
// caller when it is not present.
func gitCredentialOsxkeychainPath(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-credential-osxkeychain")
	if _, statErr := os.Stat(p); statErr != nil {
		t.Skipf("git-credential-osxkeychain not found at %s", p)
	}
	return p
}

// TestProbeCredentialFillEmpty proves `git credential fill` for
// protocol=https, host=github.com never prints a password= line under
// either profile, across three credential.helper spellings (PKG9-PLAN.md
// section 7.3): osxkeychain by name, its absolute path, and gh's own
// credential helper.
func TestProbeCredentialFillEmpty(t *testing.T) {
	requireLiveProbe(t)

	helpers := []string{"osxkeychain", gitCredentialOsxkeychainPath(t), "!gh auth git-credential"}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			env := probeEnv(sb, p)
			for _, helper := range helpers {
				t.Run(helper, func(t *testing.T) {
					out := runProbeStdin(t, sb, p, "", env, probeCredentialStdin,
						"git", "-c", "credential.helper="+helper, "credential", "fill")
					if strings.Contains(out, "password=") {
						t.Errorf("git credential fill (helper=%s) under %s printed a password= line", helper, name)
					}
				})
			}
		})
	}
}

// ---- TestProbeSSHAgentDenied ------------------------------------------------

// sshAgentDenyLineMarker is the exact line currently checked into
// build.sb/readonly.sb (section 7.3's own first candidate).
const sshAgentDenyLineMarker = `(deny network-outbound (remote unix-socket (path-regex #"^/private/tmp/com\.apple\.launchd\.[^/]+/Listeners$")))`

// sshAgentDenyCandidates are section 7.3's two candidate rule forms, in
// order: the checked-in one, and the alternate this probe also tries.
var sshAgentDenyCandidates = []string{
	sshAgentDenyLineMarker,
	`(deny network-outbound (regex #"^/private/tmp/com\.apple\.launchd\.[^/]+/Listeners$"))`,
}

// TestProbeSSHAgentDenied proves `ssh-add -l`, with SSH_AUTH_SOCK set to
// the owner's real agent socket, cannot reach the agent (exit 2: cannot
// connect) under each of 7.3's two candidate rule forms, tried in order
// against the build profile. A candidate that loads but lets ssh-add -l
// list keys, or that does not load at all, is reported as a failure: if
// neither candidate blocks the agent, this task stops and reports (section
// 7.3: "if neither blocks it, M1 task 7 stops and reports").
func TestProbeSSHAgentDenied(t *testing.T) {
	requireLiveProbe(t)
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		t.Skip("SSH_AUTH_SOCK is not set; no real ssh-agent to probe against")
	}
	if _, err := exec.LookPath("ssh-add"); err != nil {
		t.Skip("ssh-add is not on PATH")
	}

	baseProfile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read sandbox/build.sb: %v", err)
	}
	if !strings.Contains(string(baseProfile), sshAgentDenyLineMarker) {
		t.Fatal("sandbox/build.sb no longer carries the checked-in ssh-agent deny line; update this probe's candidate list")
	}

	for i, candidate := range sshAgentDenyCandidates {
		t.Run(fmt.Sprintf("candidate %d", i+1), func(t *testing.T) {
			text := strings.Replace(string(baseProfile), sshAgentDenyLineMarker, candidate, 1)
			sb := LoadProfile(profileNameBuild, []byte(text), t.TempDir(), nil, 7420)
			if !sb.Available() {
				t.Fatalf("LoadProfile: unavailable, reason %q", sb.Reason())
			}
			p := probeParams(t, sb)
			env := probeEnv(sb, p)

			exitCode, out := runProbe(t, sb, p, env, "ssh-add", "-l")
			t.Logf("candidate %d: ssh-add -l exit=%d output_len=%d", i+1, exitCode, len(out))
			if exitCode != 2 {
				t.Errorf("candidate %d did not block ssh-add -l against the real agent: exit=%d, want 2 (cannot connect)", i+1, exitCode)
			}
		})
	}
}

// ---- TestProbeCredentialHelperDenied ---------------------------------------

// TestProbeCredentialHelperDenied proves the process-exec deny on
// git-credential-* (PKG9-PLAN.md D26, N2) actually stops a real helper
// binary from running: executing git-credential-osxkeychain by its
// absolute path under either profile is denied outright (non-zero exit,
// no output -- the process never starts, so it never even reads stdin).
// A seeded git-credential-store file is a separate, expected residual, not
// a hole (PKG9-PLAN.md's own risks list, Q20/D26): credential-store is a
// git builtin (git execs itself to run it, never a separate
// git-credential-store process), so the process-exec deny never sees it,
// and the token it reads lives in a plain file the agent could already
// `cat` -- this probe logs that case rather than failing on it.
func TestProbeCredentialHelperDenied(t *testing.T) {
	requireLiveProbe(t)

	helperPath := gitCredentialOsxkeychainPath(t)

	repoDir := t.TempDir()
	if out, err := gitfixture.Git(t.Context(), repoDir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	credFile := filepath.Join(repoDir, ".git-credentials")
	const seededToken = "seeded-fake-token-for-probe-only"
	if err := os.WriteFile(credFile, []byte("https://x-access-token:"+seededToken+"@github.com\n"), 0o600); err != nil {
		t.Fatalf("write seeded credentials file: %v", err)
	}
	if out, err := gitfixture.Git(t.Context(), repoDir, "config", "credential.helper", "store --file="+credFile); err != nil {
		t.Fatalf("git config credential.helper: %v (%s)", err, out)
	}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			env := probeEnv(sb, p)

			// The process never starts (sandbox-exec's own execvp() failure
			// message is expected in output here, not the helper's own: a
			// non-zero exit with no "password=" line proves the helper
			// itself never ran long enough to answer the credential
			// protocol on stdin).
			if exitCode, helperOut := runProbe(t, sb, p, env, helperPath, "get"); exitCode == 0 || strings.Contains(helperOut, "password=") {
				t.Errorf("exec git-credential-osxkeychain by absolute path under %s: want a non-zero exit and no password= line (denied before it could run), got exit=%d output=%q", name, exitCode, helperOut)
			}

			storeOut := runProbeStdin(t, sb, p, repoDir, env, probeCredentialStdin, "git", "credential", "fill")
			if strings.Contains(storeOut, seededToken) {
				t.Logf("residual (expected, PKG9-PLAN.md Q20/D26): the seeded git-credential-store token leaked through %s -- credential-store is a git builtin, not a git-credential-* process, so the process-exec deny never sees it, and it reads a plain file the agent could already cat", name)
			}
		})
	}
}

// ---- TestProbeNoScenarioLeak ------------------------------------------------

// TestProbeNoScenarioLeak seeds a unique marker into a real, unsandboxed
// claude -p "planning-style" run (its own private temp root, exactly as
// runJob's applyPrivateTempRoot sets TMPDIR and CLAUDE_CODE_TMPDIR for a
// real classify/planning run: PKG9-PLAN.md section 7.3), then greps every
// location build.sb and readonly.sb can read that a Claude session might
// write -- ~/.claude except projects, ~/.claude.json, the host TMPDIR, and
// /private/tmp -- and asserts none of them ever surfaces the marker. A hit
// is the early exit: stop and report.
func TestProbeNoScenarioLeak(t *testing.T) {
	requireLiveProbe(t)
	token := probeClaudeOAuthToken(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}

	marker := "ZING-PROBE-SCENARIO-" + probeUUID(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}

	tmpRoot := t.TempDir()
	tmpDir := filepath.Join(tmpRoot, "tmp")
	claudeTmpDir := filepath.Join(tmpRoot, "claude-tmp")
	for _, d := range []string{tmpDir, claudeTmpDir} {
		if mkErr := os.MkdirAll(d, 0o700); mkErr != nil {
			t.Fatalf("mkdir %s: %v", d, mkErr)
		}
	}

	// A private TMPDIR/CLAUDE_CODE_TMPDIR pair, matching
	// applyPrivateTempRoot's own wiring (internal/job/runjob.go) for a job
	// naming no sandbox: req.Env, not the ambient process environment,
	// since this run goes through runtime.Claude's own Command/Env the same
	// way a real unsandboxed run does (PKG9-PLAN.md M1 task 7).
	extraEnv := []string{"TMPDIR=" + tmpDir, "CLAUDE_CODE_TMPDIR=" + claudeTmpDir}
	prompt := "This is an automated probe for Zing's sandbox credential boundary (PKG9-PLAN.md M1 task 7). " +
		"Reply with exactly one line containing this marker and nothing else: " + marker

	claude := runtime.NewClaude("", token)
	req := runtime.RunRequest{
		Job:      response.JobPlanning,
		Model:    probeModel,
		Prompt:   prompt,
		Tools:    claudeProbeTools,
		Env:      extraEnv,
		RunToken: "probe",
	}
	name, args, _, err := claude.Command(req)
	if err != nil {
		t.Fatalf("build command: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: argv is built by runtime.Claude.Command from this file's own fixed request, never external input
	cmd.Env = claude.Env(req)
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	cancel()
	if runErr != nil {
		t.Logf("stderr (first 400 bytes, redacted): %s", redactClaudeToken(firstBytes(stderr.Bytes(), 400)))
		t.Fatalf("EARLY EXIT: the unsandboxed, planning-style claude -p run failed (stdout length %d): %v", stdout.Len(), runErr)
	}

	locations := []string{
		filepath.Join(home, ".claude"), // grep excludes its own "projects" subdir below
		filepath.Join(home, ".claude.json"),
		os.TempDir(),
		"/private/tmp",
	}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			senv := probeEnv(sb, p)
			for _, loc := range locations {
				if _, statErr := os.Stat(loc); statErr != nil {
					continue // this location does not exist on this host; nothing to grep
				}
				exitCode, out := runProbe(t, sb, p, senv, "grep", "-r", "-l", "--exclude-dir=projects", marker, loc)
				if exitCode == 0 {
					t.Errorf("EARLY EXIT: the scenario marker was readable under %s at %s (%d bytes of matching paths); stop and report", name, loc, len(out))
				}
			}
		})
	}
}

// ---- TestProbePlanningTranscriptDenied -------------------------------------

// TestProbePlanningTranscriptDenied plants a transcript file at
// ~/.claude/projects/<other project>/<uuid>.jsonl, the shape a real
// planning session writes, and proves it is unreadable under either
// profile while a file in the run's own TRANSCRIPTS folder is still both
// writable and readable, and claude -p still starts and resumes cleanly
// (PKG9-PLAN.md section 7.3, a Package 8 holdout fix).
func TestProbePlanningTranscriptDenied(t *testing.T) {
	requireLiveProbe(t)
	token := probeClaudeOAuthToken(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	marker := probeUUID(t)
	otherProjectDir := filepath.Join(home, ".claude", "projects", "zing-probe-other-project-"+marker)
	if mkErr := os.MkdirAll(otherProjectDir, 0o755); mkErr != nil { //nolint:gosec // 0755: matches the CLI's own transcript directory mode
		t.Fatalf("mkdir %s: %v", otherProjectDir, mkErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(otherProjectDir) })
	otherTranscript := filepath.Join(otherProjectDir, probeUUID(t)+".jsonl")
	if writeErr := os.WriteFile(otherTranscript, []byte(`{"marker":"`+marker+`"}`+"\n"), 0o600); writeErr != nil {
		t.Fatalf("write %s: %v", otherTranscript, writeErr)
	}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			env := probeEnv(sb, p)

			if exitCode, out := runProbe(t, sb, p, env, "/bin/cat", otherTranscript); exitCode == 0 {
				t.Errorf("EARLY EXIT: another session's transcript was readable under %s (output length %d); stop and report", name, len(out))
			}

			if mkErr := os.MkdirAll(p.Transcripts, 0o700); mkErr != nil {
				t.Fatalf("mkdir %s: %v", p.Transcripts, mkErr)
			}
			ownTranscript := filepath.Join(p.Transcripts, "own.jsonl")
			if exitCode, out := runProbe(t, sb, p, env, "/usr/bin/touch", ownTranscript); exitCode != 0 {
				t.Fatalf("touch the run's own TRANSCRIPTS file under %s: exit %d (output %q)", name, exitCode, out)
			}
			if exitCode, out := runProbe(t, sb, p, env, "/bin/cat", ownTranscript); exitCode != 0 {
				t.Errorf("cat the run's own TRANSCRIPTS file under %s: exit %d, want 0 (output %q)", name, exitCode, out)
			}

			prefix, err := sb.Prefix(p)
			if err != nil {
				t.Fatalf("Prefix: %v", err)
			}
			extraEnv := sb.Env(p, os.Getenv("PATH"))
			sessionID := probeClaudeTurn(t, token, prefix, extraEnv, p.Worktree, "claude -p first turn under "+name+" (own transcript folder)", "")
			probeClaudeTurn(t, token, prefix, extraEnv, p.Worktree, "claude -p resume under "+name+" (own transcript folder)", sessionID)
		})
	}
}

// ---- TestBuildProfileStillPassesPackage8Proofs -----------------------------

// probeRepoRoot returns this git worktree's top-level directory: the real
// repository, not a throwaway temp directory, since the heavy proofs below
// (go test, make lint, make test-js, zing validate) are only meaningful
// against it.
func probeRepoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--show-toplevel")
	cmd.Env = gitfixture.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// probeRepoGitDir returns repoRoot's REPO_GIT (git rev-parse
// --path-format=absolute --git-common-dir).
func probeRepoGitDir(t *testing.T, repoRoot string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Env = gitfixture.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-common-dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// package8SandboxProofs is every automated proof PKG8-PLAN.md section 18's
// task 8 wrote for the sandbox rules of section 5, exactly as named there:
// the two directory-scoped denials, the writable set, the console-port and
// escape-hatch (open/osascript/launchctl) denials, and TLS still working
// (version 7's own probe, folded into task 8). They already run in every
// plain `go test ./internal/sandbox/...`, against whatever build.sb is
// currently embedded; listing them here and calling them as subtests makes
// this file's own ZING_LIVE_CLI-gated run re-assert every one of them by
// name, rather than re-deriving the same assertions from Makefile targets.
var package8SandboxProofs = []struct {
	name string
	fn   func(*testing.T)
}{
	{"TestProfileLoads", TestProfileLoads},
	{"TestDeniesHomeRead", TestDeniesHomeRead},
	{"TestDeniesDataDir", TestDeniesDataDir},
	{"TestRunsZingBinFromDataDir", TestRunsZingBinFromDataDir},
	{"TestDeniesHomeWrite", TestDeniesHomeWrite},
	{"TestDeniesHostTempWrite", TestDeniesHostTempWrite},
	{"TestDeniesGitPointerWrite", TestDeniesGitPointerWrite},
	{"TestDeniesGitConfigWrite", TestDeniesGitConfigWrite},
	{"TestAllowsWorktreeWrite", TestAllowsWorktreeWrite},
	{"TestAllowsRunDirWrite", TestAllowsRunDirWrite},
	{"TestChildSeesSandboxTmpdir", TestChildSeesSandboxTmpdir},
	{"TestReadPathsAllowsExtra", TestReadPathsAllowsExtra},
	{"TestDeniesConsolePort", TestDeniesConsolePort},
	{"TestAllowsOtherLocalPort", TestAllowsOtherLocalPort},
	{"TestDeniesOpen", TestDeniesOpen},
	{"TestDeniesOsascriptToApplication", TestDeniesOsascriptToApplication},
	{"TestDeniesLaunchctlSubmit", TestDeniesLaunchctlSubmit},
	{"TestAllowsTLSDownload", TestAllowsTLSDownload},
}

// TestBuildProfileStillPassesPackage8Proofs re-runs Package 8's own host
// proofs (PKG8-PLAN.md section 5, section 19's "make ci green" and "zing
// selftest green", and task 8's named tests) under the changed build.sb
// (PKG9-PLAN.md section 7.3): the Mach and keychain changes alter what
// several of them relied on (the Claude login used to live in the
// keychain). Every sandbox-rule proof that already has a Package 8 test
// runs as that test, via package8SandboxProofs, instead of a
// re-implementation. The rows that need the real repository, the real
// toolchain, or the owner's real login -- which no existing test covers --
// stay here: the claude login, the full suite, make lint, make test-js,
// zing validate, and a fresh module's TLS download. Each row logs its exit
// code; most also assert it.
func TestBuildProfileStillPassesPackage8Proofs(t *testing.T) {
	requireLiveProbe(t)
	token := probeClaudeOAuthToken(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}

	for _, proof := range package8SandboxProofs {
		t.Run(proof.name, proof.fn)
	}

	repoRoot := probeRepoRoot(t)
	repoGit := probeRepoGitDir(t, repoRoot)

	sb := newLoadedSandbox(t, nil, 7420)
	runDir, cleanup, err := sb.NewRunDir()
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	t.Cleanup(cleanup)
	p, err := sb.ParamsFor(repoRoot, repoGit, runDir)
	if err != nil {
		t.Fatalf("ParamsFor: %v", err)
	}
	if mkErr := os.MkdirAll(p.Transcripts, 0o700); mkErr != nil {
		t.Fatalf("mkdir %s: %v", p.Transcripts, mkErr)
	}
	env := probeEnv(sb, p)

	// row runs one proof row, logging its exit code and asserting it
	// against wantExit.
	row := func(name string, dir string, rowEnv []string, wantExit int, args ...string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			exitCode, out := runProbeIn(t, sb, p, dir, rowEnv, probeHeavyTimeout, args...)
			t.Logf("%s: exit=%d output_len=%d", name, exitCode, len(out))
			if exitCode != wantExit {
				t.Errorf("%s: exit=%d, want %d", name, exitCode, wantExit)
			}
		})
	}

	claudeEnv := append(append([]string{}, env...), "CLAUDE_CODE_OAUTH_TOKEN="+token)
	sessionID := probeUUID(t)
	row("claude -p first turn", p.Worktree, claudeEnv, 0,
		"claude", "-p", "Say ok and nothing else.", "--session-id", sessionID, "--output-format", "json", "--model", probeModel)
	row("claude -p resume", p.Worktree, claudeEnv, 0,
		"claude", "-p", "Say ok again.", "--resume", sessionID, "--output-format", "json", "--model", probeModel)
	row("go test -count=1 ./...", repoRoot, env, 0, "go", "test", "-count=1", "./...")
	row("make lint", repoRoot, env, 0, "make", "lint")
	row("make test-js", repoRoot, env, 0, "make", "test-js")
	row("zing validate", repoRoot, env, 0, "go", "run", "./cmd/zing", "validate")

	t.Run("go mod download (fresh module, TLS)", func(t *testing.T) {
		modDir := t.TempDir()
		goMod := "module probe.invalid/m\n\ngo 1.23\n\nrequire rsc.io/quote v1.5.2\n"
		if writeErr := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o600); writeErr != nil {
			t.Fatalf("write go.mod: %v", writeErr)
		}
		exitCode, out := runProbeIn(t, sb, p, modDir, env, probeHeavyTimeout, "go", "mod", "download")
		t.Logf("go mod download: exit=%d output_len=%d", exitCode, len(out))
		if exitCode != 0 {
			t.Errorf("go mod download: exit=%d, want 0", exitCode)
		}
	})
}
