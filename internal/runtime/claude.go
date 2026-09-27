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
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxOutputBytes is the 4 MiB cap stdout is held to (design section 4.1):
// past this, Run keeps draining and discarding the rest so a chatty child
// can always exit, and reports ErrOutputTooLarge once it has.
const maxOutputBytes = 4 * 1024 * 1024

var _ Runtime = Claude{}

// Claude runs a job through the real claude CLI (design section 4.1, D18).
// bin is the path to the binary; "" means claude on PATH.
type Claude struct {
	bin string
}

// NewClaude returns a Claude that runs bin, or "claude" on PATH when bin is
// "".
func NewClaude(bin string) Claude {
	return Claude{bin: bin}
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
func claudeArgv(req RunRequest, newSessionID string) ([]string, error) {
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
	return argv, nil
}

// allowedParentEnv is the parent-process variables Run carries into the
// child when they are set (design section 4.1). Everything else the
// calling process happens to have is not the child's business.
var allowedParentEnv = []string{"PATH", "HOME", "LANG", "GOPATH", "GOCACHE", "TMPDIR"}

// agentEnv builds the filtered environment (design section 4.1), shared by
// both Claude and Codex: the allowlisted parent variables, then req.Env,
// then the two variables every run needs, then a drop pass that removes
// anything shaped like a secret except ZING_RUN_TOKEN by its exact name --
// so a GITHUB_TOKEN or an AWS_SECRET_ACCESS_KEY riding in on req.Env can
// never reach the child. CLAUDE_CODE_PROMPT_CACHE_TTL is harmless to a
// codex run: it is an environment variable, not a flag, and codex ignores
// names it does not read.
func agentEnv(req RunRequest) []string {
	merged := make([]string, 0, len(allowedParentEnv)+len(req.Env)+2)
	for _, name := range allowedParentEnv {
		if v, ok := os.LookupEnv(name); ok {
			merged = append(merged, name+"="+v)
		}
	}
	merged = append(merged, req.Env...)
	merged = append(merged, "CLAUDE_CODE_PROMPT_CACHE_TTL=1h", "ZING_RUN_TOKEN="+req.RunToken)

	out := make([]string, 0, len(merged))
	for _, kv := range merged {
		name, _, _ := strings.Cut(kv, "=")
		if name == "ZING_RUN_TOKEN" || !envNameBlocked(name) {
			out = append(out, kv)
		}
	}
	return out
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

	sessionID := req.SessionID
	var newUUID string
	if sessionID == "" {
		uuid, err := newSessionUUID()
		if err != nil {
			return RunResult{}, err
		}
		newUUID = uuid
		sessionID = uuid
	}

	argv, err := claudeArgv(req, newUUID)
	if err != nil {
		return RunResult{}, err
	}

	res, runErr := c.run(ctx, req, argv, sessionID, start)

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
func (c Claude) run(ctx context.Context, req RunRequest, argv []string, sessionID string, start time.Time) (RunResult, error) {
	cmd := exec.CommandContext(ctx, c.resolveBin(), argv...) //nolint:gosec // G204: bin is an operator-configured path (NewClaude), argv is built by claudeArgv from validated fields, never from raw external input
	cmd.Dir = req.WorkDir
	cmd.Env = agentEnv(req)
	cmd.Stdin = strings.NewReader(req.Prompt)
	configureProcessGroup(cmd)

	stdout := &capWriter{limit: maxOutputBytes}
	cmd.Stdout = stdout

	var stderrCount countingWriter
	stderrHash := sha256.New()
	cmd.Stderr = io.MultiWriter(&stderrCount, stderrHash)

	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrStart
	}

	waitErr := cmd.Wait()

	res := RunResult{
		AgentTime:    time.Since(start),
		SessionID:    sessionID,
		ExitCode:     exitCodeFrom(waitErr),
		StderrLen:    stderrCount.n,
		StderrSHA256: shortHex(stderrHash.Sum(nil)),
	}

	if outcomeErr := classifyProcessOutcome(ctx, stdout.overflowed(), waitErr, res.ExitCode); outcomeErr != nil {
		if errors.Is(outcomeErr, ErrTimeout) || errors.Is(outcomeErr, ErrCanceled) {
			res.ExitCode = -1
		}
		return res, outcomeErr
	}

	var cr claudeResult
	if err := json.Unmarshal(stdout.bytes(), &cr); err != nil {
		return res, fmt.Errorf("runtime: claude: decode output: %w", err)
	}

	resp, logText, ferr := parseFinalMessage(cr.Result, req.Job)
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

// version runs `claude --version` and returns its trimmed output, for the
// smoke test that skips when the binary is not on PATH.
func (c Claude) version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.resolveBin(), "--version").Output() //nolint:gosec // G204: bin is an operator-configured path (NewClaude), same as run's own exec.CommandContext
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
