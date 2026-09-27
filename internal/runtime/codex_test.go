package runtime

import (
	"context"
	"encoding/hex"
	"errors"
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

// newFakeCodexRequest builds a RunRequest that points a fake codex
// invocation at dir for its recorded argv, stdin, and environment, running
// mode, plus any extra KEY=VALUE pairs the mode itself reads. Job is always
// planreview: the codex runtime only ever serves the plan-review job
// (design section 6.5).
func newFakeCodexRequest(dir, mode string, extra ...string) RunRequest {
	env := append([]string{"FAKE_CODEX_DIR=" + dir, "FAKE_CODEX_MODE=" + mode}, extra...)
	return RunRequest{
		Job:      response.JobPlanreview,
		Model:    testCodexModel,
		Prompt:   "the assembled prompt",
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
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	c := NewCodex(fakeCodexScript)
	if _, err := c.Run(ctx, req); !errors.Is(err, ErrCanceled) {
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

// ---- exit and errors --------------------------------------------------------

func TestCodex_ErrStart(t *testing.T) {
	t.Parallel()

	c := NewCodex(filepath.Join(t.TempDir(), "no-such-codex-binary"))
	res, err := c.Run(context.Background(), RunRequest{Job: response.JobPlanreview, Model: "m"})
	if !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestCodex_ErrTimeout(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "sleep", "FAKE_CODEX_SLEEP_SECONDS=5")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
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
	// The grandchild sleep must die with the rest of the process group, not
	// linger until its own 5s sleep would otherwise end.
	if elapsed > 3*time.Second {
		t.Errorf("Run took %v, want well under 5s (grandchild killed via the process group)", elapsed)
	}
}

func TestCodex_ErrCanceled(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeCodexRequest(dir, "sleep", "FAKE_CODEX_SLEEP_SECONDS=5")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	c := NewCodex(fakeCodexScript)
	res, err := c.Run(ctx, req)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
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
