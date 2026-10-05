package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

const (
	fakeCodexScript        = "testdata/fake_codex.sh"
	codexEventsFixture     = "testdata/codex_events.jsonl"
	codexResultFixture     = "testdata/codex_result.txt"
	testCodexModel         = "codex-test-model"
	testCodexResumeID      = "resumed-codex-session-456"
	pinnedCodexThreadID    = "0199e2b1-2c8b-7c53-8e9e-0a1b2c3d4e5f"
	forbiddenCodexBypass   = "--dangerously-bypass-approvals-and-sandbox"
	fakeCodexDefaultThread = "fake-codex-default-thread-id"
)

// judgeOkResultXML is a minimal, valid response.JudgeResponse "ok" document
// (one verdict, matching Verdicts' own jsonschema:"minItems=1"), for the
// full-access tests below: the fake script's own default -o content embeds
// job="planreview", which req.Job's judge value would otherwise reject as
// reasonWrongJob.
const judgeOkResultXML = `<zing job="judge" outcome="ok"><verdict scenario="s1" result="pass"><evidence>ok</evidence></verdict></zing>`

// newFakeCodexRequest builds a RunRequest that points a fake codex
// invocation at dir for its recorded argv, stdin, and environment, running
// mode, plus any extra KEY=VALUE pairs the mode itself reads. Job defaults
// to planreview: of the codex runtime's two jobs (planreview and, from
// PKG9-PLAN.md section 4.6 on, judge), every test in this file except the
// full-access ones below (which set Job to response.JobJudge directly)
// never needs anything but Codex's own ordinary read-only path.
func newFakeCodexRequest(dir, mode string, extra ...string) RunRequest {
	env := append([]string{"FAKE_CODEX_DIR=" + dir, "FAKE_CODEX_MODE=" + mode}, extra...)
	return RunRequest{
		Job:      response.JobPlanreview,
		Model:    testCodexModel,
		Prompt:   testPrompt,
		Env:      env,
		RunToken: "42",
	}
}

// wantCodexArgv builds the expected argv every fake-CLI test shares (design
// section 4.1), given the session-specific middle flag and the -o path this
// particular run used.
func wantCodexArgv(outPath string, sandboxFlag, tail []string) []string {
	argv := make([]string, 0, 4+len(sandboxFlag)+5+len(tail))
	argv = append(argv, "exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check")
	argv = append(argv, sandboxFlag...)
	argv = append(argv, "-m", testCodexModel, "--json", "-o", outPath)
	argv = append(argv, tail...)
	return argv
}

// outfileFromArgv returns the value following -o in argv, the -o path that
// particular run used.
func outfileFromArgv(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "-o" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatal("argv has no -o flag")
	return ""
}

// readMode reads a mode string (as fake_codex.sh's stat_mode recorded it)
// from path.
func readMode(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

// ---- argv, stdin, env (fake CLI) --------------------------------------------

func TestCodex_ArgvFirstTurn(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success")
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	outPath := outfileFromArgv(t, argv)
	want := wantCodexArgv(outPath, []string{"-s", "read-only"}, []string{"-"})
	if !slices.Equal(argv, want) {
		t.Errorf("argv =\n%v\nwant\n%v", argv, want)
	}
	if res.SessionID != fakeCodexDefaultThread {
		t.Errorf("SessionID = %q, want the default fixture's thread id %q", res.SessionID, fakeCodexDefaultThread)
	}
}

func TestCodex_ArgvResume(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success")
	req.SessionID = testCodexResumeID
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	outPath := outfileFromArgv(t, argv)
	want := wantCodexArgv(outPath, []string{"-c", `sandbox_mode="read-only"`}, []string{"resume", testCodexResumeID, "-"})
	if !slices.Equal(argv, want) {
		t.Errorf("argv =\n%v\nwant\n%v", argv, want)
	}
	if res.SessionID != testCodexResumeID {
		t.Errorf("SessionID = %q, want the echoed %q", res.SessionID, testCodexResumeID)
	}
}

func TestCodex_ForbiddenFlagAbsent(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	for _, resume := range []bool{false, true} {
		dir := t.TempDir()
		req := newFakeCodexRequest(dir, "success")
		if resume {
			req.SessionID = testCodexResumeID
		}
		c := NewCodex(fakeCodexScript)
		if _, err := c.Run(context.Background(), req); err != nil {
			t.Fatalf("Run: %v", err)
		}

		argv := strings.Join(readArgv(t, dir), " ")
		if strings.Contains(argv, forbiddenCodexBypass) {
			t.Errorf("argv contains forbidden flag %q: %s", forbiddenCodexBypass, argv)
		}
	}
}

func TestCodex_PromptOnStdinNotArgv(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success")
	req.Prompt = "SECRET-PROMPT-MARKER the assembled prompt text"
	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stdin := readRecordedFile(t, filepath.Join(dir, "stdin"))
	if stdin != req.Prompt {
		t.Errorf("stdin = %q, want %q", stdin, req.Prompt)
	}
	argv := strings.Join(readArgv(t, dir), " ")
	if strings.Contains(argv, "SECRET-PROMPT-MARKER") {
		t.Error("prompt leaked into argv")
	}
	// The prompt is never a positional argument: the last argv token is
	// always the literal "-" that tells codex to read it from stdin.
	last := readArgv(t, dir)
	if last[len(last)-1] != "-" {
		t.Errorf("last argv token = %q, want \"-\"", last[len(last)-1])
	}
}

func TestCodex_EnvFilter(t *testing.T) {
	// t.Setenv cannot combine with t.Parallel.
	requireUnix(t)
	t.Setenv("ZING_UNRELATED_TEST_VAR", "should-not-leak")

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success", "GITHUB_TOKEN=x", "AWS_SECRET_ACCESS_KEY=y")
	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	env := readRecordedEnv(t, dir)
	if _, ok := env["GITHUB_TOKEN"]; ok {
		t.Error("GITHUB_TOKEN leaked into the child environment")
	}
	if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("AWS_SECRET_ACCESS_KEY leaked into the child environment")
	}
	if v, ok := env["ZING_RUN_TOKEN"]; !ok || v != "42" {
		t.Errorf("ZING_RUN_TOKEN = %q, ok=%v, want %q", v, ok, "42")
	}
	if v, ok := env["CLAUDE_CODE_PROMPT_CACHE_TTL"]; !ok || v != "1h" {
		t.Errorf("CLAUDE_CODE_PROMPT_CACHE_TTL = %q, ok=%v, want %q", v, ok, "1h")
	}
	if _, ok := env["ZING_UNRELATED_TEST_VAR"]; ok {
		t.Error("an unrelated parent variable leaked into the child environment")
	}
}

// TestCodexEnvHasNoOAuthToken proves a Codex run never carries
// CLAUDE_CODE_OAUTH_TOKEN, whether set in the parent process or passed
// through req.Env (PKG9-PLAN.md section 4.6, D26): only Claude.run ever
// appends it, after agentEnv, which Codex.run also calls but never adds the
// token to.
func TestCodexEnvHasNoOAuthToken(t *testing.T) {
	// t.Setenv cannot combine with t.Parallel.
	requireUnix(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "should-not-reach-a-codex-child")

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success", "CLAUDE_CODE_OAUTH_TOKEN=should-not-reach-either")
	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	env := readRecordedEnv(t, dir)
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN reached a codex child")
	}
}

// ---- the -o file: directory and file modes, cleanup ------------------------

func TestCodex_OutputFileModes(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success")
	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := readMode(t, filepath.Join(dir, "o_mode")); got != "600" {
		t.Errorf("-o file mode = %q, want 600", got)
	}
	if got := readMode(t, filepath.Join(dir, "o_dir_mode")); got != "700" {
		t.Errorf("-o directory mode = %q, want 700", got)
	}
}

func TestCodex_OutputDirRemovedAfterEveryPath(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	cases := []struct {
		name string
		mode string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"success", "success", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }},
		{"exec_error", "exit_nonzero", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }},
		{"timeout", "sleep", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			req := newFakeCodexRequest(dir, tc.mode, "FAKE_CODEX_SLEEP_SECONDS=5")
			ctx, cancel := tc.ctx()
			defer cancel()

			c := NewCodex(fakeCodexScript)
			if _, err := c.Run(ctx, req); err != nil {
				t.Logf("Run: %v (mode %s may legitimately fail; this test only checks dir cleanup)", err, tc.mode)
			}

			argv := readArgv(t, dir)
			outPath := outfileFromArgv(t, argv)
			outDir := filepath.Dir(outPath)
			if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
				t.Errorf("output dir %s still exists after Run (mode %s)", outDir, tc.mode)
			}
		})
	}
}

func TestCodex_OutputDirRemovedAfterCancel(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "sleep", "FAKE_CODEX_SLEEP_SECONDS=5")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	c := NewCodex(fakeCodexScript)
	go func() {
		_, err := c.Run(ctx, req)
		done <- err
	}()

	// Cancel only once the child is up, so a slow Start() cannot turn this
	// into an ErrStart instead of the cancel this test drives.
	waitForFakeChild(t, dir)
	cancel()

	if err := <-done; !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}

	argv := readArgv(t, dir)
	outPath := outfileFromArgv(t, argv)
	outDir := filepath.Dir(outPath)
	if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
		t.Errorf("output dir %s still exists after a canceled Run", outDir)
	}
}

// ---- the final message: from -o, not stdout, session id --------------------

func TestCodex_FinalMessageFromOutputFileNotStdout(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success")
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	fr, ok := res.Response.(*response.FindingsResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *FindingsResponse", res.Response)
	}
	// The script's default -o content carries one finding; its default
	// stdout JSONL embeds a zero-finding document instead (see
	// fake_codex.sh's default_events vs default_result). Getting one proves
	// the -o file's document won, not stdout's.
	if len(fr.Findings) != 1 {
		t.Errorf("Findings = %d, want 1 (from the -o file, not stdout's embedded document)", len(fr.Findings))
	}
}

func TestCodex_SessionIDFromFixtureFirstTurn(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success", "FAKE_CODEX_EVENTS_FILE="+codexEventsFixture)
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != pinnedCodexThreadID {
		t.Errorf("SessionID = %q, want the fixture's thread_id %q", res.SessionID, pinnedCodexThreadID)
	}
}

func TestCodex_SessionIDEchoOnResume(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success", "FAKE_CODEX_EVENTS_FILE="+codexEventsFixture)
	req.SessionID = testCodexResumeID
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != testCodexResumeID {
		t.Errorf("SessionID = %q, want echoed %q, not the fixture's thread_id", res.SessionID, testCodexResumeID)
	}
}

func TestFirstCodexSessionID_Fixture(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(codexEventsFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if got := firstCodexSessionID(data); got != pinnedCodexThreadID {
		t.Errorf("firstCodexSessionID = %q, want %q", got, pinnedCodexThreadID)
	}
}

func TestFirstCodexSessionID_AcceptsSessionIDField(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"session.started","session_id":"sid-123"}` + "\n" +
		`{"type":"turn.completed"}` + "\n")
	if got := firstCodexSessionID(data); got != "sid-123" {
		t.Errorf("firstCodexSessionID = %q, want %q", got, "sid-123")
	}
}

func TestCodex_FixtureParse(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "success", "FAKE_CODEX_RESULT_FILE="+codexResultFixture)
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	fr, ok := res.Response.(*response.FindingsResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *FindingsResponse", res.Response)
	}
	if len(fr.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(fr.Findings))
	}
	if fr.Findings[0].Lens != response.LensSecurity {
		t.Errorf("Findings[0].Lens = %q, want %q", fr.Findings[0].Lens, response.LensSecurity)
	}
}

// TestCodexRun_FinalMessage covers #43: an invalid -o document still leaves
// its text in res.FinalMessage, and Codex never fills TranscriptPath --
// Codex names its rollout files by date and thread id, a layout no current
// code knows.
func TestCodexRun_FinalMessage(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	const content = "no zing document here at all"
	resultPath := writeCodexResultFile(t, content)
	req := newFakeCodexRequest(dir, "success", "FAKE_CODEX_RESULT_FILE="+resultPath)

	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)

	var invalidErr *InvalidOutputError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("err = %v, want *InvalidOutputError", err)
	}
	if invalidErr.Reason != reasonNoZingElement {
		t.Errorf("Reason = %q, want %q", invalidErr.Reason, reasonNoZingElement)
	}
	if res.FinalMessage != content {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, content)
	}
	if res.TranscriptPath != "" {
		t.Errorf("TranscriptPath = %q, want empty", res.TranscriptPath)
	}
}

// ---- exit and errors --------------------------------------------------------

func TestCodex_ErrStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := NewCodex(filepath.Join(dir, "no-such-codex-binary"))
	res, err := c.Run(context.Background(), RunRequest{Job: response.JobPlanreview, Model: "m"})
	if !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	const wantText = "runtime: process could not start: no such file or directory"
	if err.Error() != wantText {
		t.Errorf("err.Error() = %q, want %q", err.Error(), wantText)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("err.Error() = %q, leaks the temp dir path", err.Error())
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestRun_ErrStartLogsCause proves Codex.Run and Claude.Run each log a WARN
// "<runtime> run: start failed" line, carrying job, run_token, and the
// wrapped error, whenever run returns ErrStart (design section "Logging",
// #23). Not parallel: it calls slog.SetDefault to capture the line, which
// swaps the process-wide default logger (runjob_test.go:1300-1301's own
// pattern).
func TestRun_ErrStartLogsCause(t *testing.T) {
	dir := t.TempDir()
	const runToken = "42"
	const prompt = "do not log this prompt"

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	codex := NewCodex(filepath.Join(dir, "no-such-codex-binary"))
	if _, err := codex.Run(context.Background(), RunRequest{Job: response.JobPlanreview, Model: "m", RunToken: runToken, Prompt: prompt}); !errors.Is(err, ErrStart) {
		t.Fatalf("codex Run err = %v, want ErrStart", err)
	}

	claude := NewClaude(filepath.Join(dir, "no-such-claude-binary"), testOAuthToken)
	if _, err := claude.Run(context.Background(), RunRequest{Job: response.JobClassify, Model: "m", RunToken: runToken, Prompt: prompt}); !errors.Is(err, ErrStart) {
		t.Fatalf("claude Run err = %v, want ErrStart", err)
	}

	logged := logBuf.String()
	for _, tc := range []struct {
		runtime string
		job     string
	}{
		{"codex", string(response.JobPlanreview)},
		{"claude", string(response.JobClassify)},
	} {
		want := tc.runtime + " run: start failed"
		var line string
		for candidate := range strings.SplitSeq(logged, "\n") {
			if strings.Contains(candidate, want) {
				line = candidate
				break
			}
		}
		if line == "" {
			t.Fatalf("log missing a %q line; got:\n%s", want, logged)
		}
		for _, want := range []string{
			"level=WARN",
			"job=" + tc.job,
			"run_token=" + runToken,
			"no such file or directory",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("%s start-failed line missing %q; got:\n%s", tc.runtime, want, line)
			}
		}
	}
	if strings.Contains(logged, dir) {
		t.Errorf("log leaks the temp dir path; got:\n%s", logged)
	}
	if strings.Contains(logged, prompt) {
		t.Errorf("log leaks the prompt; got:\n%s", logged)
	}
}

func TestCodex_ErrTimeout(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	// A long-sleeping child so the deadline, not the child exiting on its own,
	// ends the run; the deadline is armed generously (2s) so a slow Start()
	// under load never sees an already-expired context (Codex.run would map
	// that to ErrStart, not ErrTimeout).
	req := newFakeCodexRequest(dir, "sleep", "FAKE_CODEX_SLEEP_SECONDS=30")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(ctx, req)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	// The grandchild sleep must die with the rest of the process group at the
	// deadline, not linger until its own 30s sleep would otherwise end.
	if elapsed > 10*time.Second {
		t.Errorf("Run took %v, want it killed near the ~2s deadline (grandchild killed via the process group)", elapsed)
	}
}

func TestCodex_ErrCanceled(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "sleep", "FAKE_CODEX_SLEEP_SECONDS=5")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res RunResult
		err error
	}
	done := make(chan outcome, 1)
	c := NewCodex(fakeCodexScript)
	go func() {
		res, err := c.Run(ctx, req)
		done <- outcome{res, err}
	}()

	// Cancel only once the child is actually up, so a slow Start() cannot make
	// this return ErrStart instead of the ErrCanceled asserted here.
	waitForFakeChild(t, dir)
	cancel()

	got := <-done
	if !errors.Is(got.err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", got.err)
	}
	if got.res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", got.res.ExitCode)
	}
}

func TestCodex_ExecErrorRealCode(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "exit_nonzero", "FAKE_CODEX_EXIT_CODE=3")
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)

	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("err = %v, want *ExecError", err)
	}
	if execErr.ExitCode != 3 {
		t.Errorf("ExecError.ExitCode = %d, want 3", execErr.ExitCode)
	}
	if res.ExitCode != 3 {
		t.Errorf("res.ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestCodex_ExecErrorSignal(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "signal_kill")
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)

	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("err = %v, want *ExecError", err)
	}
	if execErr.ExitCode != -1 {
		t.Errorf("ExecError.ExitCode = %d, want -1 (no numeric status)", execErr.ExitCode)
	}
	if res.ExitCode != -1 {
		t.Errorf("res.ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestCodex_OutputTooLarge_FromOutputFile(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "big_output")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(ctx, req)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("err = %v, want ErrOutputTooLarge", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (the real exit code)", res.ExitCode)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Run took %v, want well under the 20s deadline (no spurious timeout)", elapsed)
	}
}

func TestCodex_StderrMetadata(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "big_stderr")
	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	const want int64 = 8 * 1024 * 1024
	if res.StderrLen != want {
		t.Errorf("StderrLen = %d, want %d", res.StderrLen, want)
	}
	if len(res.StderrSHA256) != 12 {
		t.Errorf("StderrSHA256 = %q, want 12 hex chars", res.StderrSHA256)
	}
	if _, err := hex.DecodeString(res.StderrSHA256); err != nil {
		t.Errorf("StderrSHA256 = %q, not hex: %v", res.StderrSHA256, err)
	}
	if strings.Contains(res.Log, "eeeeee") {
		t.Error("Log retained raw stderr content")
	}
}

// ---- the five closed InvalidOutputError reasons (design section 4.1) -------

func writeCodexResultFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write result file: %v", err)
	}
	return path
}

func TestCodex_InvalidOutput(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	validFinding := `<zing job="planreview" outcome="ok"><finding lens="quality" severity="nit" location="plan/objective"><text>ok</text><fix>ok</fix></finding></zing>`

	tests := []struct {
		name       string
		content    string
		wantReason string
	}{
		{"zero roots", "no zing document here at all", reasonNoZingElement},
		{"two roots", validFinding + validFinding, reasonMultipleZingDocs},
		{"wrong job", `<zing job="classify" outcome="bug"><reason>x</reason></zing>`, reasonWrongJob},
		{"unsupported outcome", `<zing job="planreview" outcome="nope"></zing>`, reasonUnsupportedOutcome},
		{
			"failed validation",
			`<zing job="planreview" outcome="ok"><finding lens="quality" severity="nit" location="plan/objective"><text></text><fix>ok</fix></finding></zing>`,
			reasonFailedValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			resultPath := writeCodexResultFile(t, tc.content)
			req := newFakeCodexRequest(dir, "success", "FAKE_CODEX_RESULT_FILE="+resultPath)
			c := NewCodex(fakeCodexScript)
			_, err := c.Run(context.Background(), req)

			var invalidErr *InvalidOutputError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("err = %v, want *InvalidOutputError", err)
			}
			if invalidErr.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", invalidErr.Reason, tc.wantReason)
			}
		})
	}
}

// TestCodex_MissingOutputFileIsInvalidOutput covers F026: codex exiting 0
// without ever writing the -o file is a readCapped failure that is not
// ErrOutputTooLarge, so Run must map it to *InvalidOutputError (design
// section 4.1's closed Run contract) rather than a bare wrapped error.
func TestCodex_MissingOutputFileIsInvalidOutput(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "no_output_file")

	c := NewCodex(fakeCodexScript)
	res, err := c.Run(context.Background(), req)

	var invalidErr *InvalidOutputError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("err = %v, want *InvalidOutputError", err)
	}
	if invalidErr.Reason != reasonNoZingElement {
		t.Errorf("Reason = %q, want %q", invalidErr.Reason, reasonNoZingElement)
	}
	if res.Log == "" {
		t.Error("Log is empty, want the read-output-file error detail")
	}
}

// ---- the judge job's full access (PKG9-PLAN.md section 4.6, D20) ----------

// judgeExecPrefix is a real prefix a sandbox could plausibly build, using
// only a POSIX-standard binary (mirroring claude_test.go's own
// TestClaudeArgvWithExecPrefix): "env PREFIX_MARKER=1 <fake_codex.sh>
// <argv...>". It also satisfies codexWantsFullAccess's own precondition --
// a non-empty ExecPrefix -- for every test below that needs one.
var judgeExecPrefix = []string{"env", "PREFIX_MARKER=1"}

// newFakeJudgeRequest is newFakeCodexRequest with Job set to
// response.JobJudge and FAKE_CODEX_RESULT_FILE pointed at judgeOkResultXML,
// so a full-access test's own c.Run call parses cleanly end to end instead
// of failing reasonWrongJob against the fake script's own default
// planreview document. ExecPrefix is left for the caller to set (empty for
// TestCodexRefusesFullAccessWithoutPrefix, judgeExecPrefix for everything
// else).
func newFakeJudgeRequest(t *testing.T, dir string) RunRequest {
	t.Helper()
	req := newFakeCodexRequest(dir, "success", "FAKE_CODEX_RESULT_FILE="+writeCodexResultFile(t, judgeOkResultXML))
	req.Job = response.JobJudge
	return req
}

// TestCodexArgvFullAccessOnlyWithPrefix proves a judge-job request with a
// non-empty ExecPrefix gets "-s danger-full-access" on a first turn and
// "-c sandbox_mode=\"danger-full-access\"" on a resume (PKG9-PLAN.md
// section 4.6, D20), in place of the ordinary read-only pair every other
// job still gets (TestCodex_ArgvFirstTurn, TestCodex_ArgvResume).
func TestCodexArgvFullAccessOnlyWithPrefix(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	t.Run("first turn", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		req := newFakeJudgeRequest(t, dir)
		req.ExecPrefix = judgeExecPrefix
		c := NewCodex(fakeCodexScript)
		if _, err := c.Run(context.Background(), req); err != nil {
			t.Fatalf("Run: %v", err)
		}

		argv := readArgv(t, dir)
		outPath := outfileFromArgv(t, argv)
		want := wantCodexArgv(outPath, []string{"-s", "danger-full-access"}, []string{"-"})
		if !slices.Equal(argv, want) {
			t.Errorf("argv =\n%v\nwant\n%v", argv, want)
		}
	})

	t.Run("resume", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		req := newFakeJudgeRequest(t, dir)
		req.ExecPrefix = judgeExecPrefix
		req.SessionID = testCodexResumeID
		c := NewCodex(fakeCodexScript)
		if _, err := c.Run(context.Background(), req); err != nil {
			t.Fatalf("Run: %v", err)
		}

		argv := readArgv(t, dir)
		outPath := outfileFromArgv(t, argv)
		want := wantCodexArgv(outPath, []string{"-c", `sandbox_mode="danger-full-access"`}, []string{"resume", testCodexResumeID, "-"})
		if !slices.Equal(argv, want) {
			t.Errorf("argv =\n%v\nwant\n%v", argv, want)
		}
	})
}

// TestCodexRefusesFullAccessWithoutPrefix proves a judge-job request with
// no ExecPrefix fails closed before the process ever starts (PKG9-PLAN.md
// section 4.6, D20): Codex must never run with no containment at all.
func TestCodexRefusesFullAccessWithoutPrefix(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeJudgeRequest(t, dir)

	c := NewCodex(fakeCodexScript)
	_, err := c.Run(context.Background(), req)
	if !errors.Is(err, ErrCodexFullAccessNeedsExecPrefix) {
		t.Fatalf("err = %v, want ErrCodexFullAccessNeedsExecPrefix", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "argv")); !os.IsNotExist(statErr) {
		t.Error("the fake CLI recorded an argv: want it never started")
	}
}

// TestCodexArgvNeverBypassFlag proves the forbidden
// --dangerously-bypass-approvals-and-sandbox flag never appears even under
// the judge's own full-access mode (TestCodex_ForbiddenFlagAbsent already
// covers the ordinary read-only path).
func TestCodexArgvNeverBypassFlag(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	for _, resume := range []bool{false, true} {
		dir := t.TempDir()
		req := newFakeJudgeRequest(t, dir)
		req.ExecPrefix = judgeExecPrefix
		if resume {
			req.SessionID = testCodexResumeID
		}
		c := NewCodex(fakeCodexScript)
		if _, err := c.Run(context.Background(), req); err != nil {
			t.Fatalf("Run: %v", err)
		}

		argv := strings.Join(readArgv(t, dir), " ")
		if strings.Contains(argv, forbiddenCodexBypass) {
			t.Errorf("argv contains forbidden flag %q: %s", forbiddenCodexBypass, argv)
		}
	}
}

// TestCodexHonorsExecPrefix proves Codex.run's own commandNameArgs honors
// ExecPrefix exactly as Claude.run's does (PKG9-PLAN.md section 4.6):
// name = ExecPrefix[0], args = ExecPrefix[1:] + resolveBin() + argv, with
// the fake script still recording the normal argv unchanged.
func TestCodexHonorsExecPrefix(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeJudgeRequest(t, dir)
	req.ExecPrefix = judgeExecPrefix
	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	outPath := outfileFromArgv(t, argv)
	want := wantCodexArgv(outPath, []string{"-s", "danger-full-access"}, []string{"-"})
	if !slices.Equal(argv, want) {
		t.Errorf("argv (after the prefix) =\n%v\nwant\n%v", argv, want)
	}

	env := readRecordedEnv(t, dir)
	if v, ok := env["PREFIX_MARKER"]; !ok || v != "1" {
		t.Errorf("PREFIX_MARKER = %q, ok=%v, want \"1\" (proves env ran ahead of the fake script)", v, ok)
	}
}

// ---- the -o file's own directory (PKG9-PLAN.md section 4.6) ---------------

// TestCodexOutputDirUnderRunTmp proves codexOutputDir creates its -o
// directory under the run's own TMPDIR (req.Env), not the host's shared
// os.TempDir(): a sandboxed Codex can write only its own run directory, so
// the -o file must live there.
func TestCodexOutputDirUnderRunTmp(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	runTmp := filepath.Join(t.TempDir(), "run-tmp")
	if err := os.MkdirAll(runTmp, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runTmp, err)
	}

	req := newFakeCodexRequest(dir, "success", "TMPDIR="+runTmp)
	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	outPath := outfileFromArgv(t, argv)
	// outPath is <base>/zing-codex-out-<rand>/output-<rand>: its
	// directory's own parent must be runTmp.
	if got := filepath.Dir(filepath.Dir(outPath)); got != runTmp {
		t.Errorf("-o file's directory's parent = %q, want %q (TMPDIR)", got, runTmp)
	}
}
