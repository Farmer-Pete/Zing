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
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	zing "zing"
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

// probeModel is the model every probe's own real claude call uses: small
// and cheap, since these calls only prove the CLI can start and log in,
// never anything about model quality.
const probeModel = "claude-3-5-haiku-20241022"

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

// probeEnv is os.Environ() (so PATH, HOME, and every ambient credential
// path like SSH_AUTH_SOCK reach the child) plus sb's own Env(p, ...)
// overrides (TMPDIR, GIT_CONFIG_GLOBAL, ZING_SANDBOXED, and so on),
// appended last so they win on a duplicate name (os/exec keeps the last
// value for a repeated name), mirroring sandbox_darwin_test.go's own
// TestChildSeesSandboxTmpdir.
func probeEnv(sb Sandbox, p Params) []string {
	return append(os.Environ(), sb.Env(p, os.Getenv("PATH"))...)
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

// probeClaudeTurn runs one real `claude -p` turn under sb/p/env, first
// turn when resumeSessionID == "", else a resume of it; label names the
// failure for the caller's own early-exit message. It fails the test
// outright (the early-exit contract this whole file follows) rather than
// returning an error a caller might work around.
func probeClaudeTurn(t *testing.T, sb Sandbox, p Params, env []string, dir, label, sessionID, resumeSessionID string) {
	t.Helper()
	args := []string{"claude", "-p", "Say ok and nothing else.", "--output-format", "json", "--model", probeModel}
	if resumeSessionID == "" {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", resumeSessionID)
	}
	exitCode, out := runProbeIn(t, sb, p, dir, env, probeTimeout, args...)
	if exitCode != 0 {
		t.Fatalf("EARLY EXIT (D26): %s exited %d (output length %d); stop and report, do not work around this", label, exitCode, len(out))
	}
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
			env := append(probeEnv(sb, p), "CLAUDE_CODE_OAUTH_TOKEN="+token)
			sessionID := probeUUID(t)

			probeClaudeTurn(t, sb, p, env, p.Worktree, "claude -p first turn under "+name, sessionID, "")
			probeClaudeTurn(t, sb, p, env, p.Worktree, "claude -p resume under "+name, "", sessionID)
		})
	}
}

// ---- TestProbeTLSWithoutSecurityServer -------------------------------------

// TestProbeTLSWithoutSecurityServer proves TLS still works (go mod
// download of a fresh module, and curl to proxy.golang.org) with
// com.apple.SecurityServer denied, and reports whether it also works with
// com.apple.trustd.agent additionally denied: 7.3 says "trustd.agent stays
// only if TLS verification fails without it"; this probe's own log is
// what decides. It fails the test only on the checked-in profile (with
// trustd.agent): the without-trustd.agent variant is diagnostic, logged,
// not asserted, since either outcome is a legitimate answer to record in
// sandbox/build.sb's own comment.
func TestProbeTLSWithoutSecurityServer(t *testing.T) {
	requireLiveProbe(t)

	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read sandbox/build.sb: %v", err)
	}
	const trustdLine = `(global-name "com.apple.trustd.agent")`
	if !strings.Contains(string(profile), trustdLine) {
		t.Fatal("sandbox/build.sb no longer carries the checked-in trustd.agent mach-lookup line; update this probe")
	}
	withoutTrustdText := strings.Replace(string(profile), trustdLine, "", 1)

	variants := map[string][]byte{
		"with trustd.agent (checked in)": profile,
		"without trustd.agent":           []byte(withoutTrustdText),
	}

	for name, text := range variants {
		t.Run(name, func(t *testing.T) {
			sb := LoadProfile(profileNameBuild, text, t.TempDir(), nil, 7420)
			if !sb.Available() {
				t.Fatalf("LoadProfile: unavailable, reason %q", sb.Reason())
			}
			p := probeParams(t, sb)
			env := probeEnv(sb, p)

			modDir := t.TempDir()
			goMod := "module probe.invalid/m\n\ngo 1.23\n\nrequire rsc.io/quote v1.5.2\n"
			if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goMod), 0o600); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}
			modExit, _ := runProbeIn(t, sb, p, modDir, env, probeTimeout, "go", "mod", "download")
			curlExit, _ := runProbe(t, sb, p, env, "curl", "-fsS", "--max-time", "15", "-o", os.DevNull, "https://proxy.golang.org")
			t.Logf("%s: go mod download exit=%d, curl exit=%d", name, modExit, curlExit)

			if name == "with trustd.agent (checked in)" {
				if modExit != 0 {
					t.Errorf("go mod download exit=%d, want 0 (the checked-in profile must keep TLS working)", modExit)
				}
				if curlExit != 0 {
					t.Errorf("curl exit=%d, want 0 (the checked-in profile must keep TLS working)", curlExit)
				}
			}
		})
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

// TestProbeCredentialHelperDenied seeds a repo-local credential.helper
// store carrying a fake, unique token, then proves `git credential fill`
// under each profile never surfaces it: the process-exec deny on
// git-credential-* (PKG9-PLAN.md D26, N2) must stop git from ever running
// git-credential-store at all, so the seeded token gives this probe a
// concrete, definitive signal a real-credential probe cannot.
func TestProbeCredentialHelperDenied(t *testing.T) {
	requireLiveProbe(t)

	repoDir := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	credFile := filepath.Join(repoDir, ".git-credentials")
	const seededToken = "seeded-fake-token-for-probe-only"
	if err := os.WriteFile(credFile, []byte("https://x-access-token:"+seededToken+"@github.com\n"), 0o600); err != nil {
		t.Fatalf("write seeded credentials file: %v", err)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "-C", repoDir, "config", "credential.helper", "store --file="+credFile).CombinedOutput(); err != nil {
		t.Fatalf("git config credential.helper: %v (%s)", err, out)
	}

	for name, sb := range probeSandboxes(t) {
		t.Run(name, func(t *testing.T) {
			p := probeParams(t, sb)
			env := probeEnv(sb, p)

			out := runProbeStdin(t, sb, p, repoDir, env, probeCredentialStdin, "git", "credential", "fill")

			if strings.Contains(out, seededToken) {
				t.Errorf("the seeded credential (git-credential-store) leaked through the profile's process-exec deny under %s", name)
			}
			if strings.Contains(out, "password=") {
				t.Errorf("git credential fill under %s printed a password= line", name)
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

	env := append(os.Environ(), "CLAUDE_CODE_OAUTH_TOKEN="+token, "TMPDIR="+tmpDir, "CLAUDE_CODE_TMPDIR="+claudeTmpDir)
	sessionID := probeUUID(t)
	prompt := "This is an automated probe for Zing's sandbox credential boundary (PKG9-PLAN.md M1 task 7). " +
		"Reply with exactly one line containing this marker and nothing else: " + marker

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--session-id", sessionID, "--output-format", "json", "--model", probeModel) //nolint:gosec // G204: fixed probe argv, no external input
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	cancel()
	if runErr != nil {
		t.Fatalf("EARLY EXIT: the unsandboxed, planning-style claude -p run failed (output length %d): %v", len(out), runErr)
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

			claudeEnv := append(append([]string{}, env...), "CLAUDE_CODE_OAUTH_TOKEN="+token)
			sessionID := probeUUID(t)
			probeClaudeTurn(t, sb, p, claudeEnv, p.Worktree, "claude -p first turn under "+name+" (own transcript folder)", sessionID, "")
			probeClaudeTurn(t, sb, p, claudeEnv, p.Worktree, "claude -p resume under "+name+" (own transcript folder)", "", sessionID)
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
	out, err := exec.CommandContext(t.Context(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// probeRepoGitDir returns repoRoot's REPO_GIT (git rev-parse
// --path-format=absolute --git-common-dir).
func probeRepoGitDir(t *testing.T, repoRoot string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-common-dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestBuildProfileStillPassesPackage8Proofs re-runs every row of Package
// 8's own section 2 proof table that used the build profile, under the
// changed build.sb (PKG9-PLAN.md section 7.3): the Mach and keychain
// changes alter what several of them relied on (the Claude login used to
// live in the keychain). Each row logs its exit code; most also assert it.
func TestBuildProfileStillPassesPackage8Proofs(t *testing.T) {
	requireLiveProbe(t)
	token := probeClaudeOAuthToken(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
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

	row("curl proxy.golang.org", repoRoot, env, 0, "curl", "-fsS", "--max-time", "15", "-o", os.DevNull, "https://proxy.golang.org")

	t.Run("console port denied", func(t *testing.T) {
		exitCode, _ := runProbe(t, sb, p, env, "/usr/bin/nc", "-z", "-w", "2", "127.0.0.1", "7420")
		if exitCode == 0 {
			t.Error("nc to the console port succeeded, want denied")
		}
	})

	t.Run("escape: open", func(t *testing.T) {
		exitCode, _ := runProbe(t, sb, p, env, "/usr/bin/open", "-g", "-a", "TextEdit")
		if exitCode == 0 {
			t.Error("open succeeded, want denied")
		}
	})
	t.Run("escape: osascript", func(t *testing.T) {
		exitCode, _ := runProbe(t, sb, p, env, "/usr/bin/osascript", "-e", `tell application "Finder" to activate`)
		if exitCode == 0 {
			t.Error("osascript succeeded, want denied")
		}
	})
	t.Run("escape: launchctl submit", func(t *testing.T) {
		label := "com.zing.probe." + probeUUID(t)
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanupCtx, "launchctl", "remove", label).Run() //nolint:gosec,errcheck // best-effort cleanup, matches sandbox_darwin_test.go's own TestDeniesLaunchctlSubmit
		})
		exitCode, _ := runProbe(t, sb, p, env, "/bin/launchctl", "submit", "-l", label, "--", "/usr/bin/true")
		if exitCode == 0 {
			t.Error("launchctl submit succeeded, want denied")
		}
	})
	t.Run("escape: write into .git", func(t *testing.T) {
		target := filepath.Join(p.RepoGit, "zing-probe-canary")
		exitCode, _ := runProbe(t, sb, p, env, "/usr/bin/touch", target)
		if exitCode == 0 {
			t.Error("touch inside .git succeeded, want denied")
			_ = os.Remove(target)
		}
	})
}
