package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

const (
	fakeClaudeScript     = "testdata/fake_claude.sh"
	testModel            = "claude-test-model"
	testResumedSessionID = "resumed-session-123"
)

// testTools and its two derived lists (design section 4.1's tool map) are
// shared across the tool-list unit tests and every fake-CLI test that
// needs a request, so the expected --tools/--allowedTools values never
// drift from what claudeToolLists actually computes for the same input.
var (
	testTools        = []string{"read", "grep", "glob", "bash_readonly"}
	wantToolNames    = []string{"Read", "Grep", "Glob", "Bash"}
	wantToolPatterns = []string{"Read", "Grep", "Glob", "Bash(zing validate:*)"}
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// requireUnix skips a test that drives the fake claude shell script on a
// platform the repo does not assume (design section 15; the golang skill
// scopes the project to macOS and Linux).
func requireUnix(t *testing.T) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake_claude.sh requires a POSIX shell; skipped on windows")
	}
}

// newFakeRequest builds a RunRequest that points a fake claude invocation
// at dir for its recorded argv, stdin, and environment, running mode, plus
// any extra KEY=VALUE pairs the mode itself reads.
func newFakeRequest(dir, mode string, extra ...string) RunRequest {
	env := append([]string{"FAKE_CLAUDE_DIR=" + dir, "FAKE_CLAUDE_MODE=" + mode}, extra...)
	return RunRequest{
		Job:      response.JobClassify,
		Model:    testModel,
		Prompt:   "the assembled prompt",
		Tools:    testTools,
		Env:      env,
		RunToken: "42",
	}
}

// wantArgv builds the expected argv tail every fake-CLI test shares (design
// section 4.1): everything after -p and the session/resume flag.
func wantArgv(sessionFlag ...string) []string {
	argv := append([]string{"-p"}, sessionFlag...)
	argv = append(argv,
		"--output-format", "json",
		"--strict-mcp-config",
		"--restricted",
		"--tools", strings.Join(wantToolNames, ","),
		"--allowedTools",
	)
	argv = append(argv, wantToolPatterns...)
	argv = append(argv,
		"--disable-slash-commands",
		"--permission-mode", "dontAsk",
		"--model", testModel,
	)
	return argv
}

func readRecordedFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readArgv(t *testing.T, dir string) []string {
	t.Helper()
	data := readRecordedFile(t, filepath.Join(dir, "argv"))
	if data == "" {
		return nil
	}
	lines := strings.Split(data, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func readRecordedEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	data := readRecordedFile(t, filepath.Join(dir, "env"))
	out := make(map[string]string)
	for line := range strings.SplitSeq(data, "\n") {
		if line == "" {
			continue
		}
		if name, val, ok := strings.Cut(line, "="); ok {
			out[name] = val
		}
	}
	return out
}

// ---- claudeToolLists (pure function, no process) ---------------------------

func TestClaudeToolLists(t *testing.T) {
	t.Parallel()

	names, patterns, err := claudeToolLists(testTools)
	if err != nil {
		t.Fatalf("claudeToolLists: %v", err)
	}
	if !slices.Equal(names, wantToolNames) {
		t.Errorf("names = %v, want %v", names, wantToolNames)
	}
	if !slices.Equal(patterns, wantToolPatterns) {
		t.Errorf("patterns = %v, want %v", patterns, wantToolPatterns)
	}
}

func TestClaudeToolLists_UnknownToolIsConstructionError(t *testing.T) {
	t.Parallel()

	_, _, err := claudeToolLists([]string{"read", "teleport"})
	if err == nil {
		t.Fatal("claudeToolLists with an unknown tool returned no error")
	}
}

// ---- argv, stdin, env (fake CLI) -------------------------------------------

func TestClaude_ArgvFirstTurn(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	want := wantArgv("--session-id", res.SessionID)
	if !slices.Equal(argv, want) {
		t.Errorf("argv =\n%v\nwant\n%v", argv, want)
	}
	if !uuidV4Pattern.MatchString(res.SessionID) {
		t.Errorf("SessionID = %q, not a v4 UUID", res.SessionID)
	}
}

func TestClaude_ArgvResume(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	req.SessionID = testResumedSessionID
	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	want := wantArgv("--resume", testResumedSessionID)
	if !slices.Equal(argv, want) {
		t.Errorf("argv =\n%v\nwant\n%v", argv, want)
	}
	if res.SessionID != testResumedSessionID {
		t.Errorf("SessionID = %q, want the echoed %q", res.SessionID, testResumedSessionID)
	}
}

func TestClaude_ForbiddenFlagsAbsent(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	c := NewClaude(fakeClaudeScript)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := strings.Join(readArgv(t, dir), " ")
	for _, forbidden := range []string{
		"bypassPermissions",
		"--dangerously-skip-permissions",
		"--allow-dangerously-skip-permissions",
		"--bare",
		"--safe-mode",
		"--max-turns",
	} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("argv contains forbidden flag %q: %s", forbidden, argv)
		}
	}
}

func TestClaude_PromptOnStdinNotArgv(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	req.Prompt = "SECRET-PROMPT-MARKER the assembled prompt text"
	c := NewClaude(fakeClaudeScript)
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
}

func TestClaude_EnvFilter(t *testing.T) {
	// t.Setenv cannot combine with t.Parallel.
	requireUnix(t)
	t.Setenv("ZING_UNRELATED_TEST_VAR", "should-not-leak")

	dir := t.TempDir()
	req := newFakeRequest(dir, "success", "GITHUB_TOKEN=x", "AWS_SECRET_ACCESS_KEY=y")
	c := NewClaude(fakeClaudeScript)
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

// ---- fixture parse ----------------------------------------------------------

func TestClaude_FixtureParse_FirstTurn(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success", "FAKE_CLAUDE_RESULT_FILE=testdata/claude_result.json")
	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !uuidV4Pattern.MatchString(res.SessionID) {
		t.Errorf("SessionID = %q, not a v4 UUID", res.SessionID)
	}
	if res.SessionID == "9f8b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d" {
		t.Error("SessionID came from the fixture's own session_id field, not the generated uuid")
	}
	cr, ok := res.Response.(*response.ClassifyResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *ClassifyResponse", res.Response)
	}
	if !strings.Contains(cr.Reason, "nil pointer dereference") {
		t.Errorf("Reason = %q", cr.Reason)
	}
}

func TestClaude_FixtureParse_Resume(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success", "FAKE_CLAUDE_RESULT_FILE=testdata/claude_result.json")
	req.SessionID = "prior-session-id"
	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != "prior-session-id" {
		t.Errorf("SessionID = %q, want echoed %q", res.SessionID, "prior-session-id")
	}
}

// ---- exit and errors --------------------------------------------------------

func TestClaude_ErrStart(t *testing.T) {
	t.Parallel()

	c := NewClaude(filepath.Join(t.TempDir(), "no-such-claude-binary"))
	res, err := c.Run(context.Background(), RunRequest{Job: response.JobClassify, Model: "m"})
	if !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestClaude_ErrTimeout(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "sleep", "FAKE_CLAUDE_SLEEP_SECONDS=5")

	// The job deadline killing a real child process is inherent to what
	// this test proves; there is no channel to wait on instead of the
	// clock.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(ctx, req)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if res.SessionID == "" {
		t.Error("SessionID is empty, want the generated uuid")
	}
}

func TestClaude_ErrCanceled(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "sleep", "FAKE_CLAUDE_SLEEP_SECONDS=5")

	ctx, cancel := context.WithCancel(context.Background())
	// Simulates a dispatcher shutdown arriving mid-run; there is no signal
	// short of real wall-clock time to wait on before cancelling a process
	// that is deliberately still running.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(ctx, req)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if res.SessionID == "" {
		t.Error("SessionID is empty, want the generated uuid")
	}
}

func TestClaude_ExecErrorRealCode(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "exit_nonzero", "FAKE_CLAUDE_EXIT_CODE=3")
	c := NewClaude(fakeClaudeScript)
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

func TestClaude_ExecErrorSignal(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "signal_kill")
	c := NewClaude(fakeClaudeScript)
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

func TestClaude_OutputTooLarge(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "big_stdout")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	c := NewClaude(fakeClaudeScript)
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

func TestClaude_StderrMetadata(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "big_stderr")
	c := NewClaude(fakeClaudeScript)
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

// TestClaude_DecodeErrorIsInvalidOutput covers F026: a clean exit whose
// stdout is not valid JSON must return *InvalidOutputError (design section
// 4.1's closed Run contract), not a bare wrapped error, so routeFailure
// (internal/job/planning.go) can terminalize the run instead of orphaning
// it.
func TestClaude_DecodeErrorIsInvalidOutput(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(resultPath, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("write result file: %v", err)
	}
	req := newFakeRequest(dir, "success", "FAKE_CLAUDE_RESULT_FILE="+resultPath)

	c := NewClaude(fakeClaudeScript)
	res, err := c.Run(context.Background(), req)

	var invalidErr *InvalidOutputError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("err = %v, want *InvalidOutputError", err)
	}
	if invalidErr.Reason != reasonNoZingElement {
		t.Errorf("Reason = %q, want %q", invalidErr.Reason, reasonNoZingElement)
	}
	if res.Log == "" {
		t.Error("Log is empty, want the decode error detail")
	}
}
