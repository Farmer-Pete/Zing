// codex_live_test.go spends the owner's real Codex usage. It is the one
// place this repository proves, against the real codex binary, that
// codexShellEnvArgs's shell_environment_policy.set key (codex.go) actually
// reaches the commands Codex's shell runs -- the claim
// TestCodexArgvShellEnvPolicy (codex_test.go) cannot make, since the fake
// codex script only records the argv Zing sent, never applies it.
//
// It never runs by default, in CI, or from any other test in this
// repository: TestLive_CodexShellEnvTmpdir skips unless ZING_LIVE_CLI=1 is
// set, and skips again unless codex is on PATH. Run it explicitly,
// deliberately:
//
//	ZING_LIVE_CLI=1 go test ./internal/runtime -run TestLive_CodexShellEnvTmpdir -v
//
// Codex is out of usage quota until 2026-10-30 (ticket Q4), so this test
// cannot run until then; until it does, codexShellEnvArgs's key is
// unconfirmed against the real CLI.
//
// This run is NOT sandboxed: ExecPrefix is judgeExecPrefix, a dummy
// "env PREFIX_MARKER=1" that only satisfies codexWantsFullAccess's own
// non-empty precondition (codex.go) and wraps nothing real, so
// codexSandboxArgs hands the real model "-s danger-full-access" -- full
// read/write/network access to this host, under whatever account runs the
// test, for the one shell command the prompt names. Building a real
// seatbelt prefix here (sandbox.Sandbox's judge profile) was left out: it
// would pull internal/sandbox's judge.sb loading into this package only
// for a test that cannot even run before 2026-10-30, and the ticket's own
// Q4 keeps a real codex host scenario out of scope until then. Know this
// before setting ZING_LIVE_CLI=1.
package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

// liveCodexModel is the model TestLive_CodexShellEnvTmpdir runs against:
// ZING_LIVE_CODEX_MODEL when set, else gpt-5.5.
func liveCodexModel() string {
	if m := os.Getenv("ZING_LIVE_CODEX_MODEL"); m != "" {
		return m
	}
	return "gpt-5.5"
}

// TestLive_CodexShellEnvTmpdir is codexShellEnvArgs's own live proof: a
// real codex judge run, told to print its shell's $TMPDIR, must print the
// run's own TMPDIR (run 1459's judge.sb denial, not the Darwin per-user
// temp directory the host's shell would otherwise show). Run's own parse
// error is expected and only logged -- the model's reply is a bare printf
// line, not a judge response document -- and the test reads
// RunResult.FinalMessage instead, which Run populates from Codex's -o file
// before parsing it.
func TestLive_CodexShellEnvTmpdir(t *testing.T) {
	skipUnlessLive(t)
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not found on PATH")
	}

	runTmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	req := RunRequest{
		Job:     response.JobJudge,
		Model:   liveCodexModel(),
		Prompt:  "Run this exact shell command, then reply with only the line it prints and nothing else:\n\nprintf 'TMPDIR=%s\\n' \"$TMPDIR\"",
		WorkDir: fakeRepo(t),
		// ExecPrefix only needs to be non-empty to satisfy
		// codexWantsFullAccess's judge-only precondition (PKG9-PLAN.md
		// section 4.6, D20); judgeExecPrefix (codex_test.go) is reused here
		// rather than a fresh "env" literal, to keep goconst's count at two.
		// It wraps nothing real, so this run is unsandboxed danger-full-access
		// (see the file header comment) -- not the seatbelt-wrapped prefix a
		// real judge run gets.
		ExecPrefix: judgeExecPrefix,
		Env:        []string{"TMPDIR=" + runTmp},
		Timeout:    3 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), req.Timeout)
	defer cancel()
	res, err := NewCodex("").Run(ctx, req)
	if err != nil {
		t.Logf("Run returned an error, expected since the reply is a bare printf line, not a judge response document: %v", err)
	}

	want := "TMPDIR=" + runTmp
	if !strings.Contains(res.FinalMessage, want) {
		t.Fatalf("final message = %q, want it to contain %q", res.FinalMessage, want)
	}
}
