package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	// notJSONStdout is TestClaudeFinalMessage's non-JSON case: both its
	// input and expected output (claudeFinalMessage falls back to stdout
	// unchanged), named once so goconst does not flag the repeated literal.
	notJSONStdout = "not json"
	// settingsFlag is the --settings argv flag every hook-settings test in
	// this file checks for, named once so goconst does not flag it.
	settingsFlag = "--settings"
	// denyTestCmd and denyLintCmd are the DenyBash entries
	// TestClaude_ArgvCarriesDenyHook checks, named once so goconst does not
	// flag the repeated literal.
	denyTestCmd = "make test"
	denyLintCmd = "make lint"
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

// TestClaude_ResumedRunDropsStaleStopHookState proves Run removes a stale
// Stop hook state file before starting a resumed session: a resumed run
// reuses its session id, so a prior run's leftover counts under the same
// TMPDIR must not leak into this run's RunResult. The fake claude never
// runs the hook itself, so any non-zero counts in the result could only
// have come from the pre-seeded file surviving.
func TestClaude_ResumedRunDropsStaleStopHookState(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	tmpDir := t.TempDir()
	req := newFakeRequest(dir, "success", "TMPDIR="+tmpDir)
	req.SessionID = testResumedSessionID

	statePath := filepath.Join(tmpDir, "zing-stop-hook-"+testResumedSessionID+".json")
	stale := `{"events":5,"blocks":3,"unread":2}`
	if err := os.WriteFile(statePath, []byte(stale), 0o600); err != nil {
		t.Fatalf("seed stale state file: %v", err)
	}

	c := NewClaude(fakeClaudeScript, testOAuthToken).WithStopHook("/opt/zing")
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.StopHookEvents != 0 || res.StopHookBlocks != 0 || res.StopHookUnread != 0 {
		t.Errorf("StopHookEvents/Blocks/Unread = %d/%d/%d, want 0/0/0 (the stale file's counts leaked)",
			res.StopHookEvents, res.StopHookBlocks, res.StopHookUnread)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file still exists at %q after the run", statePath)
	}
}

// TestClaude_CorruptStopHookStateLogsWarn proves that when the Stop hook's
// state file cannot be read after Wait, Run still reports zero counts (the
// plan's edge case: a missing or corrupt state file gives zero counts and
// no error) but logs a WARN naming the job, run_token, and the read error,
// so this case is distinguishable in the logs from a hook that never fired
// at all. Not parallel: it calls slog.SetDefault (codex_test.go's own
// pattern, TestRun_ErrStartLogsCause).
func TestClaude_CorruptStopHookStateLogsWarn(t *testing.T) {
	requireUnix(t)

	dir := t.TempDir()
	tmpDir := t.TempDir()
	// "success" mode's script blocks on `cat > stdin` until stdin closes, so
	// writing the corrupt file inside OnStart -- which runs before the
	// prompt is written and stdin closed -- lands before the child exits.
	req := newFakeRequest(dir, "success", "TMPDIR="+tmpDir)
	c := NewClaude(fakeClaudeScript, testOAuthToken).WithStopHook("/opt/zing")

	req.OnStart = func(info StartInfo) {
		statePath := filepath.Join(tmpDir, "zing-stop-hook-"+info.SessionID+".json")
		if err := os.WriteFile(statePath, []byte("not-json"), 0o600); err != nil {
			t.Errorf("seed corrupt state file: %v", err)
		}
	}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopHookEvents != 0 || res.StopHookBlocks != 0 || res.StopHookUnread != 0 {
		t.Errorf("counts = %d/%d/%d, want 0/0/0 for a corrupt state file", res.StopHookEvents, res.StopHookBlocks, res.StopHookUnread)
	}

	logged := logBuf.String()
	for _, want := range []string{"stop hook state unreadable", "job=classify", "run_token=42"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log = %q, want it to contain %q", logged, want)
		}
	}
}

// TestClaude_ArgvCarriesStopHookSettings proves WithStopHook makes Run
// append --settings and its JSON as the argv's last two entries, and that
// the one Stop command it carries names the configured zing binary, the
// run's job, and a state path under the run's own TMPDIR, keyed by the
// session id Run actually used.
func TestClaude_ArgvCarriesStopHookSettings(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	tmpDir := t.TempDir()
	req := newFakeRequest(dir, "success", "TMPDIR="+tmpDir)
	c := NewClaude(fakeClaudeScript, testOAuthToken).WithStopHook("/opt/zing bin")
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	if len(argv) < 2 || argv[len(argv)-2] != settingsFlag {
		t.Fatalf("argv = %v, want the last two entries to be --settings and its JSON", argv)
	}
	var settings claudeSettings
	if err := json.Unmarshal([]byte(argv[len(argv)-1]), &settings); err != nil {
		t.Fatalf("decode --settings JSON %q: %v", argv[len(argv)-1], err)
	}
	groups, ok := settings.Hooks["Stop"]
	if !ok || len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("settings.Hooks = %+v, want one Stop group with one command", settings.Hooks)
	}
	wantStatePath := filepath.Join(tmpDir, "zing-stop-hook-"+res.SessionID+".json")
	// Written as a literal, not through shellQuote itself, so a break in
	// shellQuote's own escaping cannot move the expected and actual strings
	// together and still pass.
	wantCmd := "'/opt/zing bin' validate --hook --job 'classify' --state '" + wantStatePath + "'"
	if got := groups[0].Hooks[0].Command; got != wantCmd {
		t.Errorf("command = %q, want %q", got, wantCmd)
	}
}

// TestClaude_ArgvCarriesDenyHook proves that a run with DenyBash set gets a
// PreToolUse hook group, matcher Bash, running the configured zing binary's
// deny-hook subcommand with one --deny per entry in order, while the Stop
// group stays exactly as TestClaude_ArgvCarriesStopHookSettings expects. A
// run with DenyBash empty carries no PreToolUse key at all.
func TestClaude_ArgvCarriesDenyHook(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success")
	req.DenyBash = []string{denyTestCmd, denyLintCmd}
	c := NewClaude(fakeClaudeScript, testOAuthToken).WithStopHook("/bin/zing")
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readArgv(t, dir)
	if len(argv) < 2 || argv[len(argv)-2] != settingsFlag {
		t.Fatalf("argv = %v, want the last two entries to be --settings and its JSON", argv)
	}
	var settings claudeSettings
	if err := json.Unmarshal([]byte(argv[len(argv)-1]), &settings); err != nil {
		t.Fatalf("decode --settings JSON %q: %v", argv[len(argv)-1], err)
	}

	stopGroups, ok := settings.Hooks["Stop"]
	oneStopCommand := ok && len(stopGroups) == 1 && len(stopGroups[0].Hooks) == 1
	if !oneStopCommand {
		t.Fatalf("settings.Hooks[Stop] = %+v, want one group with one command", settings.Hooks["Stop"])
	}
	if stopGroups[0].Matcher != "" {
		t.Errorf("Stop matcher = %q, want empty", stopGroups[0].Matcher)
	}
	wantStopStatePath := filepath.Join(os.TempDir(), "zing-stop-hook-"+res.SessionID+".json")
	wantStopCmd := "'/bin/zing' validate --hook --job 'classify' --state '" + wantStopStatePath + "'"
	if got := stopGroups[0].Hooks[0].Command; got != wantStopCmd {
		t.Errorf("Stop command = %q, want %q", got, wantStopCmd)
	}

	denyGroups, ok := settings.Hooks["PreToolUse"]
	oneDenyCommand := ok && len(denyGroups) == 1 && len(denyGroups[0].Hooks) == 1
	if !oneDenyCommand {
		t.Fatalf("settings.Hooks[PreToolUse] = %+v, want one group with one command", settings.Hooks["PreToolUse"])
	}
	if denyGroups[0].Matcher != claudeBashTool {
		t.Errorf("PreToolUse matcher = %q, want %q", denyGroups[0].Matcher, claudeBashTool)
	}
	wantCmd := "'/bin/zing' deny-hook --deny 'make test' --deny 'make lint'"
	if got := denyGroups[0].Hooks[0].Command; got != wantCmd {
		t.Errorf("command = %q, want %q", got, wantCmd)
	}

	dir2 := t.TempDir()
	req2 := newFakeRequest(dir2, "success")
	c2 := NewClaude(fakeClaudeScript, testOAuthToken).WithStopHook("/bin/zing")
	if _, err := c2.Run(context.Background(), req2); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv2 := readArgv(t, dir2)
	if len(argv2) < 2 || argv2[len(argv2)-2] != settingsFlag {
		t.Fatalf("argv = %v, want the last two entries to be --settings and its JSON", argv2)
	}
	var settings2 claudeSettings
	if err := json.Unmarshal([]byte(argv2[len(argv2)-1]), &settings2); err != nil {
		t.Fatalf("decode --settings JSON %q: %v", argv2[len(argv2)-1], err)
	}
	if _, ok := settings2.Hooks["PreToolUse"]; ok {
		t.Errorf("settings.Hooks = %+v, want no PreToolUse key when DenyBash is empty", settings2.Hooks)
	}
}

// TestShellQuote pins shellQuote's escaping with literal expectations, so a
// regression in it cannot hide behind a test that also builds its own
// expectation by calling shellQuote.
func TestShellQuote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "zing", "'zing'"},
		{"space", "/opt/zing bin", "'/opt/zing bin'"},
		{"single quote", "it's", `'it'\''s'`},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("%s: shellQuote(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestClaude_ArgvWithoutStopHookHasNoSettings proves a Claude built with
// NewClaude alone (WithStopHook never called) sends claudeArgv an empty
// settings string, so the resulting argv carries no --settings flag at
// all, matching every test built before the Stop hook existed.
func TestClaude_ArgvWithoutStopHookHasNoSettings(t *testing.T) {
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
	want := wantArgv("--session-id", res.SessionID) // the pre-hook baseline argv, which never had --settings
	if !slices.Equal(argv, want) {
		t.Errorf("argv =\n%v\nwant\n%v", argv, want)
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

// TestClaudeRun_CountsDeniedValidateCalls proves Run decodes a claude
// result's permission_denials and reports how many of them were a denied
// zing validate Bash call, through RunResult.ValidateDenied.
func TestClaudeRun_CountsDeniedValidateCalls(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	req := newFakeRequest(dir, "success", "FAKE_CLAUDE_RESULT_FILE=testdata/claude_result_denials.json")
	c := NewClaude(fakeClaudeScript, testOAuthToken)
	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ValidateDenied != 2 {
		t.Errorf("ValidateDenied = %d, want 2", res.ValidateDenied)
	}
}

// ---- exit and errors --------------------------------------------------------

func TestClaude_ErrStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := NewClaude(filepath.Join(dir, "no-such-claude-binary"), testOAuthToken)
	res, err := c.Run(context.Background(), RunRequest{Job: response.JobClassify, Model: "m"})
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

// ---- final message and transcript path (#43 split) -------------------------

// TestClaudeRun_FinalMessageOnInvalidOutput covers #43: a clean exit whose
// result text has no zing element must still leave that text in
// res.FinalMessage, and res.TranscriptPath must still be filled, so the
// console rail has something to link to even when the run is otherwise
// unusable.
func TestClaudeRun_FinalMessageOnInvalidOutput(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	const text = "plain text with no zing element at all"
	resultPath := filepath.Join(t.TempDir(), "result.json")
	resultJSON := fmt.Sprintf(`{"result":%q}`, text)
	if err := os.WriteFile(resultPath, []byte(resultJSON), 0o600); err != nil {
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
	if res.FinalMessage != text {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, text)
	}
	if !strings.HasSuffix(res.TranscriptPath, res.SessionID+".jsonl") {
		t.Errorf("TranscriptPath = %q, want it to end in %s.jsonl", res.TranscriptPath, res.SessionID)
	}
}

// TestClaudeRun_FinalMessageOnTimeout covers #43: a run killed by the job
// deadline must still carry whatever partial stdout the child wrote before
// it was killed.
func TestClaudeRun_FinalMessageOnTimeout(t *testing.T) {
	t.Parallel()
	requireUnix(t)

	dir := t.TempDir()
	const partial = "partial output before timeout"
	req := newFakeRequest(dir, "partial_then_sleep",
		"FAKE_CLAUDE_PARTIAL_OUTPUT="+partial, "FAKE_CLAUDE_SLEEP_SECONDS=30")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c := NewClaude(fakeClaudeScript, testOAuthToken)
	res, err := c.Run(ctx, req)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.FinalMessage != partial {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, partial)
	}
}

// TestClaudeRun_LogsLongTurnWithRunID proves Claude.run calls logLongTurns
// on every outcome (design shape, the long-turn path): a transcript with a
// 300 s turn logs one INFO "claude long turn" record carrying the run's
// own run_id, and a run with no transcript file logs one DEBUG "claude
// long turns: no transcript" record instead, with no error either way. Not
// parallel: it calls slog.SetDefault (TestClaude_CorruptStopHookStateLogsWarn's
// own pattern).
func TestClaudeRun_LogsLongTurnWithRunID(t *testing.T) {
	requireUnix(t)

	// Absolute, not fakeClaudeScript's bare relative path: cmd.Dir below is
	// a temp WorkDir, not this package's directory, and exec resolves a
	// relative binary path against cmd.Dir, not the test's own cwd.
	absFakeClaudeScript, err := filepath.Abs(fakeClaudeScript)
	if err != nil {
		t.Fatalf("resolve fake claude script path: %v", err)
	}

	t.Run("with transcript", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		workDir := t.TempDir()
		req := newFakeRequest(dir, "success", "HOME="+home)
		req.WorkDir = workDir
		req.SessionID = "s1"
		req.RunToken = "459"

		resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
		if err != nil {
			t.Fatalf("resolve workdir: %v", err)
		}
		transcriptDir := filepath.Join(home, ".claude", "projects", encodeClaudeTranscriptDir(resolvedWorkDir))
		if mkdirErr := os.MkdirAll(transcriptDir, 0o755); mkdirErr != nil {
			t.Fatalf("mkdir transcript dir: %v", mkdirErr)
		}
		fixture, err := os.ReadFile("testdata/claude_transcript_long.jsonl")
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		transcriptPath := filepath.Join(transcriptDir, "s1.jsonl")
		if writeErr := os.WriteFile(transcriptPath, fixture, 0o600); writeErr != nil {
			t.Fatalf("write transcript: %v", writeErr)
		}

		var logBuf bytes.Buffer
		prevDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prevDefault) })

		c := NewClaude(absFakeClaudeScript, testOAuthToken)
		res, err := c.Run(context.Background(), req)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.TranscriptPath != transcriptPath {
			t.Fatalf("TranscriptPath = %q, want %q", res.TranscriptPath, transcriptPath)
		}

		var turnRecords []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
			var rec map[string]any
			if decodeErr := json.Unmarshal([]byte(line), &rec); decodeErr != nil {
				t.Fatalf("decode log line %q: %v", line, decodeErr)
			}
			if rec["msg"] == claudeLongTurnLogMsg {
				turnRecords = append(turnRecords, rec)
			}
		}
		if len(turnRecords) != 1 {
			t.Fatalf("got %d claude long turn records, want 1: %v", len(turnRecords), turnRecords)
		}
		rec := turnRecords[0]
		if rec["run_id"] != "459" {
			t.Errorf("run_id = %v, want 459", rec["run_id"])
		}
		if rec["rank"] != float64(1) {
			t.Errorf("rank = %v, want 1", rec["rank"])
		}
		if rec["seconds"] != float64(300) {
			t.Errorf("seconds = %v, want 300", rec["seconds"])
		}
		if rec["output_tokens"] != float64(841) {
			t.Errorf("output_tokens = %v, want 841", rec["output_tokens"])
		}
		startedAt, ok := rec["started_at"].(string)
		if !ok {
			t.Fatalf("started_at missing or not a string: %v", rec["started_at"])
		}
		gotStart, err := time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			t.Fatalf("parse started_at %q: %v", startedAt, err)
		}
		wantStart := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
		if !gotStart.Equal(wantStart) {
			t.Errorf("started_at = %v, want %v", gotStart, wantStart)
		}
	})

	t.Run("on timeout", func(t *testing.T) {
		// #53 r2f3: logLongTurns sits ahead of classifyProcessOutcome in
		// Claude.run precisely so a run the job deadline kills still logs
		// its long turns; this proves that placement, not just the
		// success path above.
		dir := t.TempDir()
		home := t.TempDir()
		workDir := t.TempDir()
		req := newFakeRequest(dir, "sleep", "HOME="+home, "FAKE_CLAUDE_SLEEP_SECONDS=30")
		req.WorkDir = workDir
		req.SessionID = "s3"
		req.RunToken = "459"

		resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
		if err != nil {
			t.Fatalf("resolve workdir: %v", err)
		}
		transcriptDir := filepath.Join(home, ".claude", "projects", encodeClaudeTranscriptDir(resolvedWorkDir))
		if mkdirErr := os.MkdirAll(transcriptDir, 0o755); mkdirErr != nil {
			t.Fatalf("mkdir transcript dir: %v", mkdirErr)
		}
		fixture, err := os.ReadFile("testdata/claude_transcript_long.jsonl")
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		transcriptPath := filepath.Join(transcriptDir, "s3.jsonl")
		if writeErr := os.WriteFile(transcriptPath, fixture, 0o600); writeErr != nil {
			t.Fatalf("write transcript: %v", writeErr)
		}

		var logBuf bytes.Buffer
		prevDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prevDefault) })

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		c := NewClaude(absFakeClaudeScript, testOAuthToken)
		_, runErr := c.Run(ctx, req)
		if !errors.Is(runErr, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", runErr)
		}

		var turnRecords []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
			var rec map[string]any
			if decodeErr := json.Unmarshal([]byte(line), &rec); decodeErr != nil {
				t.Fatalf("decode log line %q: %v", line, decodeErr)
			}
			if rec["msg"] == claudeLongTurnLogMsg {
				turnRecords = append(turnRecords, rec)
			}
		}
		if len(turnRecords) != 1 {
			t.Fatalf("got %d claude long turn records, want 1: %v", len(turnRecords), turnRecords)
		}
		rec := turnRecords[0]
		if rec["run_id"] != "459" {
			t.Errorf("run_id = %v, want 459", rec["run_id"])
		}
		if rec["seconds"] != float64(300) {
			t.Errorf("seconds = %v, want 300", rec["seconds"])
		}
	})

	t.Run("without transcript", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		workDir := t.TempDir()
		req := newFakeRequest(dir, "success", "HOME="+home)
		req.WorkDir = workDir
		req.SessionID = "s2"
		req.RunToken = "459"

		var logBuf bytes.Buffer
		prevDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prevDefault) })

		c := NewClaude(absFakeClaudeScript, testOAuthToken)
		if _, err := c.Run(context.Background(), req); err != nil {
			t.Fatalf("Run: %v", err)
		}

		var sawNoTranscript bool
		for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
			var rec map[string]any
			if decodeErr := json.Unmarshal([]byte(line), &rec); decodeErr != nil {
				t.Fatalf("decode log line %q: %v", line, decodeErr)
			}
			if rec["msg"] == claudeLongTurnLogMsg {
				t.Errorf("got a claude long turn record with no transcript file: %v", rec)
			}
			if rec["msg"] == "claude long turns: no transcript" {
				sawNoTranscript = true
				if rec["run_id"] != "459" {
					t.Errorf("run_id = %v, want 459", rec["run_id"])
				}
			}
		}
		if !sawNoTranscript {
			t.Errorf("log = %q, want a claude long turns: no transcript record", logBuf.String())
		}
	})
}

// TestClaudeFinalMessage is claudeFinalMessage's own unit test (no process):
// the JSON result object's "result" field wins when present and non-empty,
// otherwise stdout itself comes back unchanged.
func TestClaudeFinalMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"result field", `{"result":"hi"}`, "hi"},
		{"empty result field", `{"result":""}`, `{"result":""}`},
		{"not json", notJSONStdout, notJSONStdout},
		{"empty input", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := claudeFinalMessage([]byte(tc.stdout)); got != tc.want {
				t.Errorf("claudeFinalMessage(%q) = %q, want %q", tc.stdout, got, tc.want)
			}
		})
	}
}

// TestCountValidateDenials is countValidateDenials's own unit test: an
// absent field gives 0, malformed JSON gives 0, a denied Bash command
// containing "zing validate" counts, and a denied non-Bash tool does not.
func TestCountValidateDenials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
		want   int
	}{
		{"absent field", `{"result":"hi"}`, 0},
		{"malformed json", "not json", 0},
		{"denied bash validate call", `{"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"go run ./cmd/zing validate -"}}]}`, 1},
		{"denied non-bash tool", `{"permission_denials":[{"tool_name":"Read","tool_input":{"command":"zing validate -"}}]}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := countValidateDenials([]byte(tc.stdout)); got != tc.want {
				t.Errorf("countValidateDenials(%q) = %d, want %d", tc.stdout, got, tc.want)
			}
		})
	}
}

// TestClaudeTranscriptPath is claudeTranscriptPath's own unit test: it
// builds HOME/.claude/projects/ENC(workDir)/SESSION.jsonl against workDir
// resolved to an absolute, symlink-free path, the last HOME= entry in env
// wins, and an empty session id gives "". workDir must exist on disk, since
// claudeTranscriptPath resolves it with filepath.EvalSymlinks.
func TestClaudeTranscriptPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	encodedDir := encodeClaudeTranscriptDir(resolvedDir)

	got := claudeTranscriptPath([]string{"HOME=/h"}, dir, "s1")
	want := filepath.Join(string(filepath.Separator)+"h", ".claude", "projects", encodedDir, "s1.jsonl")
	if got != want {
		t.Errorf("claudeTranscriptPath() = %q, want %q", got, want)
	}

	got = claudeTranscriptPath([]string{"HOME=/old", "HOME=/new"}, dir, "s1")
	want = filepath.Join(string(filepath.Separator)+"new", ".claude", "projects", encodedDir, "s1.jsonl")
	if got != want {
		t.Errorf("claudeTranscriptPath() with two HOME entries = %q, want the last one %q", got, want)
	}

	if got := claudeTranscriptPath([]string{"HOME=/h"}, dir, ""); got != "" {
		t.Errorf("claudeTranscriptPath() with empty session = %q, want \"\"", got)
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
