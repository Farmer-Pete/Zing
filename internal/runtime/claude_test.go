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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zing/internal/response"
)

const (
	fakeClaudeScript     = "testdata/fake_claude.sh"
	testModel            = "claude-test-model"
	testResumedSessionID = "resumed-session-123"
	// testOAuthToken is the claude_oauth_token value every fake-CLI test in
	// this file that does not itself test the empty-token refusal
	// (TestClaude_RefusesEmptyOAuthToken) constructs its Claude with
	// (PKG9-PLAN.md section 4.6, D26).
	testOAuthToken = "test-claude-oauth-token"
	// testPrompt is the RunRequest.Prompt every fake-CLI test in this
	// package that does not itself care about the prompt's exact text uses
	// (goconst: four or more call sites compared this literal).
	testPrompt = "the assembled prompt"
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
		Prompt:   testPrompt,
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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

// TestClaudeEnvCarriesOAuthToken proves the child's environment holds
// CLAUDE_CODE_OAUTH_TOKEN exactly once, set to the configured value
// (PKG9-PLAN.md section 4.6, D26).
func TestClaudeEnvCarriesOAuthToken(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	c := NewClaude(fakeClaudeScript, testOAuthToken)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data := readRecordedFile(t, filepath.Join(dir, "env"))
	if count := strings.Count(data, "CLAUDE_CODE_OAUTH_TOKEN="); count != 1 {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN appears %d times in the child env, want 1", count)
	}
	env := readRecordedEnv(t, dir)
	if v, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; !ok || v != testOAuthToken {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, ok=%v, want %q", v, ok, testOAuthToken)
	}
}

// TestClaudeEnvDropsParentOAuthToken proves a parent process's own
// CLAUDE_CODE_OAUTH_TOKEN, and one set through req.Env, are both dropped:
// only Claude's own configured token ever reaches the child (PKG9-PLAN.md
// section 4.6, D26). Not parallel: t.Setenv cannot combine with
// t.Parallel.
func TestClaudeEnvDropsParentOAuthToken(t *testing.T) {
	requireUnix(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "parent-token-should-not-reach-child")

	dir := t.TempDir()
	req := newFakeRequest(dir, "success", "CLAUDE_CODE_OAUTH_TOKEN=req-env-token-should-not-reach-child")
	c := NewClaude(fakeClaudeScript, testOAuthToken)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data := readRecordedFile(t, filepath.Join(dir, "env"))
	if count := strings.Count(data, "CLAUDE_CODE_OAUTH_TOKEN="); count != 1 {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN appears %d times in the child env, want exactly 1", count)
	}
	env := readRecordedEnv(t, dir)
	if v := env["CLAUDE_CODE_OAUTH_TOKEN"]; v != testOAuthToken {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the configured %q, not a parent or req.Env value", v, testOAuthToken)
	}
}

// TestClaudeRefusesEmptyToken proves Claude.Run refuses to start the child
// at all when its own oauth token is empty (PKG9-PLAN.md section 4.6,
// D26): no process is started, so this needs no fake CLI and no
// requireUnix.
func TestClaudeRefusesEmptyToken(t *testing.T) {
	t.Parallel()

	c := NewClaude(fakeClaudeScript, "")
	res, err := c.Run(context.Background(), RunRequest{Job: response.JobClassify, Model: testModel})
	if !errors.Is(err, ErrNoOAuthToken) {
		t.Fatalf("err = %v, want ErrNoOAuthToken", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestAgentEnvHasNoSSHAuthSock proves allowedParentEnv carries no
// SSH_AUTH_SOCK (PKG9-PLAN.md section 4.6, D26, N2): a parent's own
// ssh-agent socket path must never reach an agent's environment, since the
// ssh-agent socket itself is also denied at the sandbox layer. Not
// parallel: t.Setenv cannot combine with t.Parallel.
func TestAgentEnvHasNoSSHAuthSock(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-agent.sock")

	env := agentEnv(RunRequest{})

	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "SSH_AUTH_SOCK" {
			t.Errorf("agentEnv() = %v, want no SSH_AUTH_SOCK entry", env)
		}
	}
}

// TestAgentEnvCarriesUser covers D6 (plan section 4.4): claude -p reports
// "Not logged in" when the child sees no USER, and LOGNAME alone does not
// stand in for it, so agentEnv must carry the parent's USER through.
func TestAgentEnvCarriesUser(t *testing.T) {
	// t.Setenv cannot combine with t.Parallel.
	t.Setenv("USER", "zing-test")

	env := agentEnv(RunRequest{})

	if !slices.Contains(env, "USER=zing-test") {
		t.Errorf("agentEnv() = %v, want it to contain %q", env, "USER=zing-test")
	}
}

// TestAgentEnvOmitsLogname guards against widening the fix into a second
// variable: LOGNAME is not in allowedParentEnv, so setting it in the parent
// must not make it appear in the child's environment.
func TestAgentEnvOmitsLogname(t *testing.T) {
	// t.Setenv cannot combine with t.Parallel.
	t.Setenv("LOGNAME", "zing-test")

	env := agentEnv(RunRequest{})

	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "LOGNAME" {
			t.Errorf("agentEnv() = %v, want no LOGNAME entry", env)
		}
	}
}

// TestFilteredEnvExtraOverridesAllowlist proves FilteredEnv (review F051)
// lets a value in extra override the same name from the parent allowlist --
// os/exec keeps the last value for a duplicate name, so extra's own value
// must sort after the allowlisted one in the returned slice -- and drops a
// secret-shaped name in extra the same way it drops one inherited from the
// parent. Not parallel: t.Setenv cannot combine with t.Parallel.
func TestFilteredEnvExtraOverridesAllowlist(t *testing.T) {
	t.Setenv("PATH", "/parent/path")

	env := FilteredEnv([]string{"PATH=/extra/path", "EVIL_TOKEN=x"})

	parentIdx := slices.Index(env, "PATH=/parent/path")
	extraIdx := slices.Index(env, "PATH=/extra/path")
	if parentIdx == -1 || extraIdx == -1 || extraIdx < parentIdx {
		t.Errorf("FilteredEnv() = %v, want both PATH values present with extra's own value last (os/exec keeps the last duplicate)", env)
	}
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "EVIL_TOKEN" {
			t.Errorf("FilteredEnv() = %v, want no EVIL_TOKEN entry", env)
		}
	}
}

// ---- fixture parse ----------------------------------------------------------

func TestClaude_FixtureParse_FirstTurn(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success", "FAKE_CLAUDE_RESULT_FILE=testdata/claude_result.json")
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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

	c := NewClaude(filepath.Join(t.TempDir(), "no-such-claude-binary"), testOAuthToken)
	res, err := c.Run(context.Background(), RunRequest{Job: response.JobClassify, Model: "m"})
	if !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// waitForFakeChild blocks until the fake CLI at dir has recorded its argv,
// which proves exec.CommandContext actually started the child. A test arms
// its cancel timer only after this returns: otherwise a slow Start() under
// parallel load could observe an already-cancelled context, and Claude.run
// maps every cmd.Start() failure to ErrStart -- not the ErrCanceled/ErrTimeout
// the process-lifecycle tests assert. Called from the test goroutine so its
// t.Fatalf is legal.
//
// It waits for the env file, not the argv file: both fakes write argv, then
// stdin, then env, and a shell redirection creates its file before the
// command fills it, so a reader that returned on argv's existence could see
// it empty or half-written (seen on Linux CI). env existing means argv is
// complete.
func waitForFakeChild(t *testing.T, dir string) {
	t.Helper()
	marker := filepath.Join(dir, "env")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("fake child did not record its environment at %s within 5s", marker)
}

func TestClaude_ErrTimeout(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	// A long-sleeping child so the job deadline, not the child exiting on its
	// own, ends the run. The deadline is armed generously (2s): the child
	// records its argv in milliseconds, so even under heavy parallel load
	// Start() completes well before it, and cmd.Start() never sees an
	// already-expired context (which Claude.run would map to ErrStart).
	req := newFakeRequest(dir, "sleep", "FAKE_CLAUDE_SLEEP_SECONDS=30")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	defer cancel()

	type outcome struct {
		res RunResult
		err error
	}
	done := make(chan outcome, 1)
	c := NewClaude(fakeClaudeScript, testOAuthToken)
	go func() {
		res, err := c.Run(ctx, req)
		done <- outcome{res, err}
	}()

	// Cancel only once the child is actually up, simulating a dispatcher
	// shutdown mid-run without racing a slow Start().
	waitForFakeChild(t, dir)
	cancel()

	got := <-done
	if !errors.Is(got.err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", got.err)
	}
	if got.res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", got.res.ExitCode)
	}
	if got.res.SessionID == "" {
		t.Error("SessionID is empty, want the generated uuid")
	}
}

func TestClaude_ExecErrorRealCode(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "exit_nonzero", "FAKE_CLAUDE_EXIT_CODE=3")
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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
	c := NewClaude(fakeClaudeScript, testOAuthToken)
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

// ---- ExecPrefix and process-group cleanup (design section 4.4, 5.5) -------

// TestClaudeArgvWithExecPrefix proves commandNameArgs' own contract: name =
// ExecPrefix[0], args = ExecPrefix[1:] + resolveBin() + argv. "env
// PREFIX_MARKER=1 <fake_claude.sh> <argv...>" is a real prefix a sandbox
// could plausibly build, using only a POSIX-standard binary: env sets
// PREFIX_MARKER in the child's own environment before exec'ing the fake
// script with the rest of ExecPrefix's contract intact -- the fake script
// still records the normal argv unchanged.
func TestClaudeArgvWithExecPrefix(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	req.ExecPrefix = []string{"env", "PREFIX_MARKER=1"}
	c := NewClaude(fakeClaudeScript, testOAuthToken)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	want := wantArgv("--session-id", res.SessionID)
	if !slices.Equal(argv, want) {
		t.Errorf("argv (after the prefix) =\n%v\nwant\n%v", argv, want)
	}

	env := readRecordedEnv(t, dir)
	if v, ok := env["PREFIX_MARKER"]; !ok || v != "1" {
		t.Errorf("PREFIX_MARKER = %q, ok=%v, want \"1\" (proves env ran ahead of the fake script)", v, ok)
	}
}

// TestClaudeKillsGroupAfterExit proves Run's process-group kill reaches a
// grandchild the CLI forked and disowned (design section 5.5): the fixture
// records the grandchild's pid, the test polls until that pid is gone, and
// only then checks that the canary the grandchild would have written two
// seconds in is absent. Polling for the death, not sleeping past the
// write, is what makes the negative assertion mean something (review F031).
func TestClaudeKillsGroupAfterExit(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	canary := filepath.Join(t.TempDir(), "canary")
	req := newFakeRequest(dir, "fork_delay_write", "FAKE_CLAUDE_CANARY="+canary)

	c := NewClaude(fakeClaudeScript, testOAuthToken)
	if _, err := c.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "grandchild_pid"))
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive 5s after Run returned", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Error("the canary file exists: the forked grandchild survived Run and wrote it")
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

	c := NewClaude(fakeClaudeScript, testOAuthToken)
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

// wantEnvValue fails t unless env holds name=want exactly once.
func wantEnvValue(t *testing.T, env []string, name, want string) {
	t.Helper()
	var got []string
	for _, kv := range env {
		if n, v, ok := strings.Cut(kv, "="); ok && n == name {
			got = append(got, v)
		}
	}
	if len(got) != 1 {
		t.Errorf("%s appears %d times in agentEnv output %v, want exactly once", name, len(got), env)
		return
	}
	if got[0] != want {
		t.Errorf("%s = %q, want %q", name, got[0], want)
	}
}

// TestAgentEnvDisablesBackgroundTasks proves CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1
// rides in agentEnv's output unconditionally (plan #54), the same way
// CLAUDE_CODE_PROMPT_CACHE_TTL already does (TestClaude_EnvFilter): a
// backgrounded command outlives the run that started it, so every agent run
// needs this set, not just Claude's fake-CLI integration tests.
func TestAgentEnvDisablesBackgroundTasks(t *testing.T) {
	t.Parallel()

	env := agentEnv(RunRequest{})
	if !slices.Contains(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") {
		t.Errorf("agentEnv(RunRequest{}) = %v, want it to contain CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", env)
	}
}

// TestAgentEnvBashTimeoutsFromRequestTimeout proves BASH_DEFAULT_TIMEOUT_MS
// and BASH_MAX_TIMEOUT_MS both carry req.Timeout minus the 60s margin (plan
// #54): 45 minutes is 2700000ms, minus the 60000ms margin is 2640000, well
// clear of the 120000ms floor.
func TestAgentEnvBashTimeoutsFromRequestTimeout(t *testing.T) {
	t.Parallel()

	env := agentEnv(RunRequest{Timeout: 45 * time.Minute})
	wantEnvValue(t, env, "BASH_DEFAULT_TIMEOUT_MS", "2640000")
	wantEnvValue(t, env, "BASH_MAX_TIMEOUT_MS", "2640000")
}

// TestAgentEnvBashTimeoutsStayUnderShortRunDeadline proves both bash
// timeout variables stay under the run's own deadline for short jobs
// (issue #54 review): at 3 minutes the margin rule gives 120000 ms, and a
// 1-minute job gets half the run, 30000 ms, never a limit longer than the
// run itself.
func TestAgentEnvBashTimeoutsStayUnderShortRunDeadline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"margin rule", 3 * time.Minute, "120000"},
		{"half the run", time.Minute, "30000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := agentEnv(RunRequest{Timeout: tc.timeout})
			wantEnvValue(t, env, "BASH_DEFAULT_TIMEOUT_MS", tc.want)
			wantEnvValue(t, env, "BASH_MAX_TIMEOUT_MS", tc.want)
		})
	}
}

// TestAgentEnvBashTimeoutsSurviveFilterDrop pins that the two bash timeout
// variable names are not shaped like a secret to envNameBlocked (plan #54):
// a *_MS suffix matches none of _TOKEN/_KEY/_SECRET/AWS_, so FilteredEnv's
// drop pass never strips them out of agentEnv's result. This is a
// regression test for exactly the drop pass that scrubs secrets.
func TestAgentEnvBashTimeoutsSurviveFilterDrop(t *testing.T) {
	t.Parallel()

	if envNameBlocked("BASH_DEFAULT_TIMEOUT_MS") {
		t.Error("envNameBlocked(BASH_DEFAULT_TIMEOUT_MS) = true, want false")
	}
	if envNameBlocked("BASH_MAX_TIMEOUT_MS") {
		t.Error("envNameBlocked(BASH_MAX_TIMEOUT_MS) = true, want false")
	}

	env := agentEnv(RunRequest{Timeout: 45 * time.Minute})
	wantEnvValue(t, env, "BASH_DEFAULT_TIMEOUT_MS", "2640000")
	wantEnvValue(t, env, "BASH_MAX_TIMEOUT_MS", "2640000")
}
