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
