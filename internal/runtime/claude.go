package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"zing/internal/response"
)

// maxOutputBytes is the 4 MiB cap stdout is held to (design section 4.1):
// past this, Run keeps draining and discarding the rest so a chatty child
// can always exit, and reports ErrOutputTooLarge once it has.
const maxOutputBytes = 4 * 1024 * 1024

var _ Runtime = Claude{}

// Claude runs a job through the real claude CLI (design section 4.1, D18).
// bin is the path to the binary; "" means claude on PATH. oauthToken
// authenticates every run (PKG9-PLAN.md section 4.6, D26): the output of
// `claude setup-token`, appended to the child's environment as
// CLAUDE_CODE_OAUTH_TOKEN and never passed to any other runtime or command.
// zingBin, set only through WithStopHook, is the Zing binary a run's Stop
// hook settings invoke; "" turns the hook off.
type Claude struct {
	bin        string
	oauthToken string
	zingBin    string
}

// NewClaude returns a Claude that runs bin, or "claude" on PATH when bin is
// "", authenticating every run with oauthToken (PKG9-PLAN.md section 4.6).
func NewClaude(bin, oauthToken string) Claude {
	return Claude{bin: bin, oauthToken: oauthToken}
}

// WithStopHook returns a copy of c whose runs carry a Stop hook that runs
// zingBin validate --hook, passed through --settings only, so nothing is
// written to the user's or the repo's settings. "" turns it off.
func (c Claude) WithStopHook(zingBin string) Claude {
	c.zingBin = zingBin
	return c
}

// resolveBin returns the binary Run should execute: c.bin, or "claude" on
// PATH when c.bin is "".
func (c Claude) resolveBin() string {
	if c.bin == "" {
		return "claude"
	}
	return c.bin
}

// claudeToolNames maps a machine.toml tool name to its CLI spelling
// (design section 4.1): read/grep/glob/edit/write/bash pass straight
// through to their built-in tool name, and bash_readonly narrows Bash to
// one allowed command pattern instead of granting the whole tool.
var claudeToolNames = map[string]string{
	"read":          "Read",
	"grep":          "Grep",
	"glob":          "Glob",
	"edit":          "Edit",
	"write":         "Write",
	"bash":          "Bash",
	"bash_readonly": "Bash(zing validate:*)",
}

// claudeToolLists turns a job's machine tools into the two lists --tools
// and --allowedTools take (design section 4.1): --tools gets the
// deduplicated built-in name each pattern belongs to, in the order each
// built-in first appears (bash_readonly contributes only "Bash");
// --allowedTools gets every pattern, one per input tool. An unknown
// machine tool is a construction error, not a runtime failure: nothing
// runs.
func claudeToolLists(tools []string) (names, patterns []string, err error) {
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		pattern, ok := claudeToolNames[t]
		if !ok {
			return nil, nil, fmt.Errorf("runtime: claude: unknown machine tool %q", t)
		}
		// Cut returns the whole pattern, unsplit, when "(" is absent, which
		// is already the right builtin name for a plain tool like "Read".
		builtin, _, _ := strings.Cut(pattern, "(")
		if !seen[builtin] {
			seen[builtin] = true
			names = append(names, builtin)
		}
		patterns = append(patterns, pattern)
	}
	return names, patterns, nil
}

// claudeArgv assembles Claude's argv exactly per design section 4.1. The
// prompt never appears here: Run writes it to stdin and closes stdin
// instead. Design's --max-turns is deliberately absent: Claude Code
// 2.1.274 has no such flag (verified against --help; the only related
// flag is --max-budget-usd, a dollar cap, not a turn cap), so the per-job
// timeout runJob already applies through the context deadline is the only
// bound on a run.
func claudeArgv(req RunRequest, newSessionID, settings string) ([]string, error) {
	names, patterns, err := claudeToolLists(req.Tools)
	if err != nil {
		return nil, err
	}

	argv := []string{"-p"}
	if req.SessionID == "" {
		argv = append(argv, "--session-id", newSessionID)
	} else {
		argv = append(argv, "--resume", req.SessionID)
	}
	argv = append(argv,
		"--output-format", "json",
		"--strict-mcp-config",
		"--restricted",
		"--tools", strings.Join(names, ","),
		"--allowedTools",
	)
	argv = append(argv, patterns...)
	argv = append(argv,
		"--disable-slash-commands",
		"--permission-mode", "dontAsk",
		"--model", req.Model,
	)
	if settings != "" {
		argv = append(argv, "--settings", settings)
	}
	return argv, nil
}

// claudeSettings is the JSON Claude.Command's --settings argument carries
// (design section 11, the Stop hook): a run's own hooks, passed on the
// command line only, so --restricted's own refusal of user, project and
// local settings files never hides it.
type claudeSettings struct {
	Hooks map[string][]claudeHookGroup `json:"hooks"`
}

// claudeHookGroup is one Claude Code hook matcher group; the Stop hook
// settings this package builds always hold exactly one.
type claudeHookGroup struct {
	Hooks []claudeHookCommand `json:"hooks"`
}

// claudeHookCommand is one Claude Code hook command entry.
type claudeHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// stopHook returns the --settings JSON and the state file path for one
// run; both "" when c has no zing binary configured (WithStopHook never
// called).
func (c Claude) stopHook(req RunRequest, sessionID string) (settings, statePath string, err error) {
	if c.zingBin == "" {
		return "", "", nil
	}
	statePath = stopHookStatePath(agentEnv(req), sessionID)
	settings, err = stopHookSettings(c.zingBin, req.Job, statePath)
	return settings, statePath, err
}

// stopHookStatePath names the Stop hook's state file for one run: the last
// TMPDIR= entry in env, or os.TempDir() when none is set, joined with
// "zing-stop-hook-" + sessionID + ".json" -- the session id keeps parallel
// runs on a shared host TMPDIR apart.
func stopHookStatePath(env []string, sessionID string) string {
	dir := os.TempDir()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			dir = v
		}
	}
	return filepath.Join(dir, "zing-stop-hook-"+sessionID+".json")
}

// stopHookSettings renders the --settings JSON for one run's Stop hook: a
// single Stop matcher group running zingBin validate --hook --job JOB
// --state PATH, every value shell-quoted since Claude Code runs the
// command through a shell.
func stopHookSettings(zingBin string, job response.Job, statePath string) (string, error) {
	cmd := shellQuote(zingBin) + " validate --hook --job " + shellQuote(string(job)) + " --state " + shellQuote(statePath)
	b, err := json.Marshal(claudeSettings{Hooks: map[string][]claudeHookGroup{
		"Stop": {{Hooks: []claudeHookCommand{{Type: "command", Command: cmd}}}},
	}})
	if err != nil {
		return "", fmt.Errorf("runtime: claude: stop hook settings: %w", err)
	}
	return string(b), nil
}

// shellQuote wraps s in single quotes for a POSIX shell, escaping an
// embedded single quote as close-quote, backslash-quote, open-quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// allowedParentEnv is the parent-process variables Run carries into the
// child when they are set (design section 4.1, decision D6). Everything
// else the calling process happens to have is not the child's business.
// USER is carried because claude -p reports "Not logged in" without it;
// LOGNAME alone does not stand in for it.
var allowedParentEnv = []string{"PATH", "HOME", "LANG", "GOPATH", "GOCACHE", "TMPDIR", "USER"}

// FilteredEnv builds the filtered environment two callers share (review
// F051): agentEnv below (Claude and Codex runs) and runShellCommand in
// internal/job/commands.go (the sandbox-probe, git, and lint/test commands
// a build or fix unit runs). It is the allowlisted parent variables, then
// extra, then a drop pass that removes anything shaped like a secret except
// ZING_RUN_TOKEN by its exact name -- so a GITHUB_TOKEN or an
// AWS_SECRET_ACCESS_KEY, whether inherited from the calling process's own
// environment or riding in on extra, can never reach the child. Duplicate
// names: os/exec keeps the last value for a repeated name, so a value in
// extra always wins over the same name from the parent allowlist.
func FilteredEnv(extra []string) []string {
	merged := make([]string, 0, len(allowedParentEnv)+len(extra))
	for _, name := range allowedParentEnv {
		if v, ok := os.LookupEnv(name); ok {
			merged = append(merged, name+"="+v)
		}
	}
	merged = append(merged, extra...)

	out := make([]string, 0, len(merged))
	for _, kv := range merged {
		name, _, _ := strings.Cut(kv, "=")
		if name == "ZING_RUN_TOKEN" || !envNameBlocked(name) {
			out = append(out, kv)
		}
	}
	return out
}

// bashTimeoutMarginMS is subtracted from req.Timeout before it becomes
// BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS (issue #54), so a bash
// command run near the end of a job's own timeout budget gets killed by the
// CLI's own timeout instead of by the job's context deadline landing on it
// mid-command.
const bashTimeoutMarginMS = 60_000

// bashTimeoutMS computes the value both BASH_DEFAULT_TIMEOUT_MS and
// BASH_MAX_TIMEOUT_MS get for a run with this timeout (issue #54): the
// job's own timeout minus a margin, so the CLI's bash timeout fires before
// the job's context deadline does. For a short job, where the margin would
// leave less than half the run, it uses half the run instead, so the value
// stays positive and always under the run's own deadline (a one-minute job
// gets 30 s, never a limit longer than the run itself).
func bashTimeoutMS(timeout time.Duration) int64 {
	return max(timeout.Milliseconds()-bashTimeoutMarginMS, timeout.Milliseconds()/2)
}

// agentEnv builds the filtered environment (design section 4.1), shared by
// both Claude and Codex: FilteredEnv over req.Env plus the variables every
// run needs. CLAUDE_CODE_PROMPT_CACHE_TTL and
// CLAUDE_CODE_DISABLE_BACKGROUND_TASKS are harmless to a codex run: they
// are environment variables, not flags, and codex ignores names it does
// not read. CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 (issue #54) stops a
// build or judge turn from losing its remaining work to a command it
// backgrounded: a backgrounded command is killed with the rest of the
// process group the moment the run ends (killProcessGroup), so letting the
// model background one at all only hides lost work behind a clean-looking
// turn. BASH_DEFAULT_TIMEOUT_MS and BASH_MAX_TIMEOUT_MS (also issue #54)
// bound every bash call the model makes to req.Timeout, so a single
// command cannot quietly consume the whole job's budget.
func agentEnv(req RunRequest) []string {
	bashTimeout := strconv.FormatInt(bashTimeoutMS(req.Timeout), 10)
	return FilteredEnv(append(req.Env,
		"CLAUDE_CODE_PROMPT_CACHE_TTL=1h",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"BASH_DEFAULT_TIMEOUT_MS="+bashTimeout,
		"BASH_MAX_TIMEOUT_MS="+bashTimeout,
		"ZING_RUN_TOKEN="+req.RunToken,
	))
}

// envNameBlocked reports whether name is shaped like a secret (design
// section 4.1): *_TOKEN, *_KEY, *_SECRET, or AWS_*. agentEnv checks this
// against every merged variable except ZING_RUN_TOKEN by exact name.
func envNameBlocked(name string) bool {
	switch {
	case strings.HasSuffix(name, "_TOKEN"), strings.HasSuffix(name, "_KEY"), strings.HasSuffix(name, "_SECRET"):
		return true
	case strings.HasPrefix(name, "AWS_"):
		return true
	default:
		return false
	}
}

// newSessionUUID mints a v4 UUID from crypto/rand for a first turn (design
// section 4.1); a resume never calls this, it echoes req.SessionID
// instead.
func newSessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("runtime: claude: generate session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// capWriter retains at most limit bytes and keeps draining and discarding
// everything past it (design section 4.1), so a chatty child's stdout can
// never make Run buffer without bound and the child's stdout write end
// stays readable to EOF. overflowed reports whether more than limit bytes
// ever arrived, regardless of how many are retained.
type capWriter struct {
	mu    sync.Mutex
	limit int
	buf   bytes.Buffer
	total int64
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += int64(len(p))
	if room := w.limit - w.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

func (w *capWriter) overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > int64(w.limit)
}

func (w *capWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Bytes()
}

// countingWriter counts bytes written without retaining any of them; Run
// pairs it with a sha256.Hash through io.MultiWriter so stderr's length
// and digest are known without stderr itself ever being kept (design
// section 4.1, 10).
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// claudeResult is Claude Code's --output-format json result shape (design
// section 4.1): only the field Run reads out of the documented object;
// every other field (subtype, is_error, session_id, num_turns,
// duration_ms, ...) is ignored. RunResult.SessionID never comes from this
// struct: it is always the uuid Run generated or the session id the
// caller asked to resume (design section 4.1).
type claudeResult struct {
	Result string `json:"result"`
	// PermissionDenials lists the tool calls Claude Code's own permission
	// check refused this run; absent gives nil, which countValidateDenials
	// reads as 0 denials.
	PermissionDenials []claudePermissionDenial `json:"permission_denials"`
}

// claudePermissionDenial is one entry of claude's own permission_denials
// result field: the tool Claude Code refused to run and, for Bash, the
// command it refused.
type claudePermissionDenial struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// countValidateDenials counts stdout's permission_denials entries whose
// tool_name is Bash and whose tool_input.command contains "zing validate"
// (design section 11): the Claude Code calls the agent's shell made up
// instead of the one allowed Bash(zing validate:*) pattern. 0 when stdout
// does not decode as JSON or the field is absent.
func countValidateDenials(stdout []byte) int {
	var cr claudeResult
	if json.Unmarshal(stdout, &cr) != nil {
		return 0
	}
	n := 0
	for _, d := range cr.PermissionDenials {
		if d.ToolName == "Bash" && strings.Contains(d.ToolInput.Command, "zing validate") {
			n++
		}
	}
	return n
}

// claudeFinalMessage returns the text Claude's run treats as the final
// message: the JSON result object's "result" field when stdout decodes
// and that field is non-empty, otherwise stdout itself, so a crash or a
// truncated object still leaves its text behind.
func claudeFinalMessage(stdout []byte) string {
	var cr claudeResult
	if err := json.Unmarshal(stdout, &cr); err == nil && cr.Result != "" {
		return cr.Result
	}
	return string(stdout)
}

// claudeTranscriptPath is where the claude CLI writes this session's
// transcript: HOME/.claude/projects/ENC(workDir)/SESSION.jsonl, ENC
// replacing every byte outside [A-Za-z0-9] with '-' (the same encoding
// sandbox.encodeTranscriptDir uses for the TRANSCRIPTS profile parameter).
// workDir is resolved to an absolute, symlink-free path first, matching
// what finishSandboxRequest already hands Claude for a sandboxed run; an
// unsandboxed run's WorkDir can still hold symlink components. home is the
// last HOME= entry in env, falling back to os.UserHomeDir; "" when either
// path fails to resolve or sessionID is "".
func claudeTranscriptPath(env []string, workDir, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	absoluteWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	resolvedWorkDir, err := filepath.EvalSymlinks(absoluteWorkDir)
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects", encodeClaudeTranscriptDir(resolvedWorkDir), sessionID+".jsonl")
}

// encodeClaudeTranscriptDir mirrors sandbox.encodeTranscriptDir: every byte
// outside [A-Za-z0-9] becomes '-'. It is not shared with that package: an
// internal/sandbox darwin test file imports internal/runtime, so
// internal/runtime importing internal/sandbox back would cycle the
// sandbox test binary.
func encodeClaudeTranscriptDir(path string) string {
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// configureProcessGroup puts cmd in its own process group and arranges for
// ctx's cancellation to SIGKILL the whole group, not just cmd.Process
// (design section 4.1, shared by Claude and Codex): either CLI may itself
// fork children (a shell, a tool it runs), and killing only the direct
// child can leave a grandchild holding the child's stdout pipe open, which
// would make Wait block on a process neither runtime meant to keep alive.
// WaitDelay is the backstop: if some descendant still won't let go, Wait
// stops waiting on I/O after it rather than hanging forever.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
}

// killProcessGroup sends SIGKILL to cmd's whole process group, once Wait has
// already returned, on every path -- success and failure alike (design
// section 5.5): configureProcessGroup's own cmd.Cancel only reaches the
// group on ctx cancellation, so a descendant the CLI forked (a build task's
// own shell, say) that outlives a clean exit would otherwise survive Run
// returning. ESRCH (no such process: the group is already gone) is not
// logged; any other failure to signal it is, since it means a descendant may
// still be running past the point Run's caller believes the run is over.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("kill process group", "pid", cmd.Process.Pid, "error", err)
	}
}

// commandNameArgs resolves the name and args exec.CommandContext should
// start (design section 4.4): with no ExecPrefix, the resolved binary and
// argv unchanged; with one, ExecPrefix[0] as name and ExecPrefix[1:] plus
// the resolved binary plus argv as args, so the sandbox's own
// "sandbox-exec -D ... -p <profile>" prefix wraps the real claude
// invocation without argv itself ever changing shape.
func (c Claude) commandNameArgs(req RunRequest, argv []string) (name string, args []string) {
	if len(req.ExecPrefix) == 0 {
		return c.resolveBin(), argv
	}
	args = make([]string, 0, len(req.ExecPrefix)-1+1+len(argv))
	args = append(args, req.ExecPrefix[1:]...)
	args = append(args, c.resolveBin())
	args = append(args, argv...)
	return req.ExecPrefix[0], args
}

// classifyProcessOutcome applies the shared priority order both runtimes use
// once a process has exited (design section 4.1): ctx.Err() explains the
// exit before anything else, because after a kill a process's own exit
// status no longer distinguishes a job deadline from a parent shutdown;
// then whether stdout ran past the 4 MiB cap; then a plain non-zero or
// signalled exit. nil means the process exited cleanly and within bounds,
// and the caller should now look at whatever it produced. When it returns
// ErrTimeout or ErrCanceled, the caller resets its own exit code to -1, the
// value design section 4.1 states for both.
func classifyProcessOutcome(ctx context.Context, overflowed bool, waitErr error, exitCode int) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			return ErrTimeout
		case errors.Is(ctxErr, context.Canceled):
			return ErrCanceled
		}
	}
	if overflowed {
		return ErrOutputTooLarge
	}
	if waitErr != nil {
		return &ExecError{ExitCode: exitCode}
	}
	return nil
}

// Run runs one turn of req.Job through the claude CLI (design section
// 4.1): argv per claudeArgv, the prompt on stdin (never in argv) with
// stdin closed once it is written, the environment per agentEnv, stdout
// capped and drained through capWriter, and stderr streamed through a
// counting writer and a sha256 hash with nothing retained. See errors.go
// for the typed failures this can return.
func (c Claude) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	start := time.Now()

	if c.oauthToken == "" {
		slog.Error("claude run: no oauth token configured", "job", req.Job)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrNoOAuthToken
	}

	sessionID := req.SessionID
	var newUUID string
	if sessionID == "" {
		uuid, err := newSessionUUID()
		if err != nil {
			slog.Error("claude run: new session uuid", "job", req.Job, "error", err)
			return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrStart
		}
		newUUID = uuid
		sessionID = uuid
	}

	settings, statePath, err := c.stopHook(req, sessionID)
	if err != nil {
		slog.Error("claude run: stop hook settings", "job", req.Job, "error", err)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrStart
	}

	argv, err := claudeArgv(req, newUUID, settings)
	if err != nil {
		slog.Error("claude run: build argv", "job", req.Job, "error", err)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrStart
	}

	res, runErr := c.run(ctx, req, argv, sessionID, statePath, start)

	if errors.Is(runErr, ErrStart) {
		slog.Warn("claude run: start failed", "job", req.Job, "run_token", req.RunToken, "error", runErr)
	}

	slog.Info("claude run",
		"job", req.Job,
		"model", req.Model,
		"run_token", req.RunToken,
		"exit_code", res.ExitCode,
		"agent_seconds", Seconds(res.AgentTime),
		"stderr_len", res.StderrLen,
		"stderr_sha256", res.StderrSHA256,
	)
	return res, runErr
}

// run is Run's process lifecycle, once argv is built: start the child,
// wait for it, and classify however it ended (design section 4.1). It
// never logs; Run does that once, for every path, after this returns.
func (c Claude) run(ctx context.Context, req RunRequest, argv []string, sessionID, statePath string, start time.Time) (RunResult, error) {
	name, args := c.commandNameArgs(req, argv)
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: bin is an operator-configured path (NewClaude), argv is built by claudeArgv from validated fields, and ExecPrefix (when set) is the sandbox's own prefix (sandbox.Sandbox.Prefix) -- never raw external input
	cmd.Dir = req.WorkDir
	if statePath != "" {
		// A resumed session reuses its session id, so a stale state file
		// from a prior run under the same TMPDIR must not leak its counts
		// into this one.
		_ = os.Remove(statePath) //nolint:errcheck // absent is the normal case
	}
	// The token is appended after agentEnv, not passed through it, so
	// FilteredEnv's own drop pass (which removes anything *_TOKEN-shaped,
	// including a parent or req.Env CLAUDE_CODE_OAUTH_TOKEN) never has to
	// know about it, and only the claude runtime's own configured value
	// ever reaches a child (PKG9-PLAN.md section 4.6, D26).
	cmd.Env = append(agentEnv(req), "CLAUDE_CODE_OAUTH_TOKEN="+c.oauthToken)
	configureProcessGroup(cmd)

	stdout := &capWriter{limit: maxOutputBytes}
	cmd.Stdout = stdout

	var stderrCount countingWriter
	stderrHash := sha256.New()
	stderrCap := &capWriter{limit: maxStderrBytes}
	cmd.Stderr = io.MultiWriter(&stderrCount, stderrHash, stderrCap)

	// The start handshake (design section 7.1, #45): stdin is a pipe, not a
	// reader handed to cmd.Stdin up front, so nothing reaches the child
	// until the goroutine below writes it -- after req.OnStart has run and
	// returned. If serve dies between Start and that write, the child's
	// stdin simply closes with no prompt and it exits having done no work.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, startErr(err)
	}

	if err = cmd.Start(); err != nil {
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, startErr(err)
	}

	if req.OnStart != nil {
		req.OnStart(StartInfo{PID: cmd.Process.Pid, SessionID: sessionID})
	}

	go func() {
		_, _ = io.WriteString(stdin, req.Prompt) //nolint:errcheck // EPIPE means the agent already exited; cmd.Wait reports the real outcome
		_ = stdin.Close()                        //nolint:errcheck // same: a close error here never changes the run's outcome
	}()

	waitErr := cmd.Wait()
	killProcessGroup(cmd)

	var hookState stopHookState
	if statePath != "" {
		hookState, _ = readStopHookState(statePath) //nolint:errcheck // a missing or corrupt file gives zero counts, same as a run with no hook
		_ = os.Remove(statePath)                    //nolint:errcheck // a leftover file is reset at the next run's own removal before Start
	}

	res := RunResult{
		AgentTime:      time.Since(start),
		SessionID:      sessionID,
		ExitCode:       exitCodeFrom(waitErr),
		StderrLen:      stderrCount.n,
		StderrSHA256:   shortHex(stderrHash.Sum(nil)),
		Stderr:         stderrCap.bytes(),
		FinalMessage:   capFinalMessage(claudeFinalMessage(stdout.bytes())),
		TranscriptPath: claudeTranscriptPath(cmd.Env, req.WorkDir, sessionID),
		StopHookEvents: hookState.Events,
		StopHookBlocks: hookState.Blocks,
		StopHookUnread: hookState.Unread,
		ValidateDenied: countValidateDenials(stdout.bytes()),
	}

	if outcomeErr := classifyProcessOutcome(ctx, stdout.overflowed(), waitErr, res.ExitCode); outcomeErr != nil {
		if errors.Is(outcomeErr, ErrTimeout) || errors.Is(outcomeErr, ErrCanceled) {
			res.ExitCode = -1
		}
		return res, outcomeErr
	}

	var cr claudeResult
	if err := json.Unmarshal(stdout.bytes(), &cr); err != nil {
		res.Log = fmt.Sprintf("decode output: %v", err)
		return res, &InvalidOutputError{Reason: reasonNoZingElement}
	}

	resp, logText, ferr := parseFinalMessage(cr.Result, req.Job, req.RunToken)
	res.Log = logText
	if ferr != nil {
		return res, ferr
	}
	res.Response = resp
	return res, nil
}

// exitCodeFrom reports the real exit code waitErr carries (design section
// 4.1): 0 for a nil error, the OS exit code for an *exec.ExitError, or -1
// when the OS gave no numeric status (typically a signal) or the error is
// not even an ExitError.
func exitCodeFrom(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	// errors.As, not the modernize-suggested errors.AsType: AsType's (E, bool)
	// result has E discarded via _, and errcheck's check-blank (this repo's
	// config) flags that discard since E is itself error-shaped.
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) { //nolint:modernize // see comment above
		return exitErr.ExitCode()
	}
	return -1
}

// shortHex returns the first 12 hex characters of sum (design section
// 4.1), the only form a stderr digest is ever allowed to take.
func shortHex(sum []byte) string {
	return hex.EncodeToString(sum)[:12]
}

// Command returns the exact argv Run would execute for req -- ExecPrefix
// applied the same way commandNameArgs applies it in run -- and the
// session id Run would use (req.SessionID echoed back on a resume, a fresh
// one minted on a first turn). Exported for the sandbox package's own host
// credential probes (PKG9-PLAN.md section 7.3, M1 task 7): a probe that
// hand-rolls its own claude argv can silently drift from what production
// actually runs, so it builds the command through this instead.
func (c Claude) Command(req RunRequest) (name string, args []string, sessionID string, err error) {
	sessionID = req.SessionID
	var newUUID string
	if sessionID == "" {
		newUUID, err = newSessionUUID()
		if err != nil {
			return "", nil, "", fmt.Errorf("runtime: claude: new session uuid: %w", err)
		}
		sessionID = newUUID
	}
	settings, _, err := c.stopHook(req, sessionID)
	if err != nil {
		return "", nil, "", err
	}
	argv, err := claudeArgv(req, newUUID, settings)
	if err != nil {
		return "", nil, "", err
	}
	name, args = c.commandNameArgs(req, argv)
	return name, args, sessionID, nil
}

// Env returns the exact child environment Run would use for req: the
// filtered parent allowlist plus req.Env (agentEnv), with the configured
// oauth token appended last, matching run's own cmd.Env construction.
// Exported for the same reason as Command.
func (c Claude) Env(req RunRequest) []string {
	return append(agentEnv(req), "CLAUDE_CODE_OAUTH_TOKEN="+c.oauthToken)
}

// version runs `claude --version` and returns its trimmed output, for the
// smoke test that skips when the binary is not on PATH.
func (c Claude) version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.resolveBin(), "--version").Output() //nolint:gosec // G204: bin is an operator-configured path (NewClaude), same as run's own exec.CommandContext
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
