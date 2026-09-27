package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

var _ Runtime = Codex{}

// Codex runs a job through the real codex CLI (design section 4.1, D18).
// bin is the path to the binary; "" means codex on PATH.
type Codex struct {
	bin string
}

// NewCodex returns a Codex that runs bin, or "codex" on PATH when bin is "".
func NewCodex(bin string) Codex {
	return Codex{bin: bin}
}

// resolveBin returns the binary Run should execute: c.bin, or "codex" on
// PATH when c.bin is "".
func (c Codex) resolveBin() string {
	if c.bin == "" {
		return "codex"
	}
	return c.bin
}

// codexArgv assembles Codex's argv exactly per design section 4.1. The
// prompt never appears here: Run writes it to stdin, and the trailing "-"
// argument tells codex to read the prompt from there instead of a
// positional argument (verified against `codex exec --help` and `codex exec
// resume --help`, Codex 0.157.1).
//
// Host isolation (D18): --ignore-user-config drops $CODEX_HOME/config.toml
// (no MCP servers or hooks); --ignore-rules drops user or project execpolicy
// .rules files; both apply on every turn, first and resumed.
// --skip-git-repo-check lets Run run inside req.WorkDir without codex
// refusing a directory it does not recognize as a git repository.
// Read-only sandboxing is expressed differently on each subcommand: a first
// turn takes -s/--sandbox directly, but codex exec resume has no such flag
// (confirmed against its own --help), so a resume expresses the same policy
// with -c sandbox_mode="read-only" instead. The forbidden
// --dangerously-bypass-approvals-and-sandbox flag (real, and present in both
// --help outputs) never appears.
//
// req.Tools is deliberately not read here: codex exec has no per-tool
// allowlist flag, so the read-only sandbox above is the control instead
// (PKG7-PLAN.md section 4).
func codexArgv(req RunRequest, outPath string) []string {
	argv := []string{"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check"}
	if req.SessionID == "" {
		argv = append(argv, "-s", "read-only")
	} else {
		argv = append(argv, "-c", `sandbox_mode="read-only"`)
	}
	argv = append(argv, "-m", req.Model, "--json", "-o", outPath)
	if req.SessionID != "" {
		argv = append(argv, "resume", req.SessionID)
	}
	argv = append(argv, "-")
	return argv
}

// codexOutputDir creates a fresh 0700 directory holding the file Run passes
// to -o (design section 4.1): codex writes the run's final message there
// directly, never to stdout, so a chatty JSONL stream can never smuggle a
// second document past the final-message rule. os.MkdirTemp already creates
// the directory at mode 0700 (unaffected by any ordinary umask, since 0700
// has no group or other bits for one to clear); the file inside it gets an
// explicit chmod to 0600 as well, so both are guaranteed regardless of the
// platform's default CreateTemp mode. The caller must defer os.RemoveAll on
// the returned dir on every path -- a start failure after the directory
// exists included -- so a killed or crashed run never leaves the file
// behind.
func codexOutputDir() (dir, path string, err error) {
	dir, err = os.MkdirTemp("", "zing-codex-out-*")
	if err != nil {
		return "", "", fmt.Errorf("runtime: codex: create output dir: %w", err)
	}

	f, err := os.CreateTemp(dir, "output-*")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("runtime: codex: create output file: %w", err)
	}
	path = f.Name()
	if err := f.Close(); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("runtime: codex: close output file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("runtime: codex: chmod output file: %w", err)
	}
	return dir, path, nil
}

// readCapped reads path's contents up to limit+1 bytes and reports
// ErrOutputTooLarge when it holds more than limit (design section 4.1): the
// same 4 MiB bound stdout is held to also applies to the -o file, since
// that is where Codex's final message actually lives. The oversize content
// itself is never returned, matching capWriter's own "never retain past the
// cap" rule for stdout.
func readCapped(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("runtime: codex: open output file: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("runtime: codex: read output file: %w", err)
	}
	if len(data) > limit {
		return nil, ErrOutputTooLarge
	}
	return data, nil
}

// codexEventLine is the subset of one codex exec --json JSONL event's
// fields Run reads (design section 4.1): whichever of thread_id or
// session_id appears first on the stream names the session. Codex's
// documented event shape (openai/codex sdk/typescript/src/events.ts,
// commit f6ad902, itself generated from codex-rs/exec/src/exec_events.rs)
// names the first event on the stream "thread.started" and gives it a
// thread_id field; no event in that shape carries a session_id. Run accepts
// session_id too, in case a future or differently configured build calls
// the same concept that instead; every other field on every event type is
// ignored.
type codexEventLine struct {
	ThreadID  string `json:"thread_id"`
	SessionID string `json:"session_id"`
}

// firstCodexSessionID scans stdout's already-capped JSONL (capWriter,
// shared with claude.go) line by line for the first event carrying either
// field, and returns it, or "" when none is found: a first turn that
// crashes before any such event still returns a RunResult, just with no
// session id (design section 4.1, "for Codex once the JSONL event carrying
// it has been read"). A malformed or truncated line -- the stream's last
// line cut off by the 4 MiB cap, for instance -- is skipped rather than
// failing the run: the session id is best effort, never required for Run to
// succeed.
func firstCodexSessionID(stdout []byte) string {
	for line := range bytes.SplitSeq(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev codexEventLine
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.ThreadID != "" {
			return ev.ThreadID
		}
		if ev.SessionID != "" {
			return ev.SessionID
		}
	}
	return ""
}

// Run runs one turn of req.Job through the codex CLI (design section 4.1):
// argv per codexArgv, the prompt on stdin (never in argv), the environment
// per agentEnv (identical to Claude's), stdout capped and drained the same
// way claude.go drains it, and stderr streamed through a counting writer
// and a sha256 hash with nothing retained -- all shared with Claude's Run.
// Codex's own shape is the argv, the session id (the first JSONL event
// carrying one), and the final message, which -- unlike Claude's single
// JSON stdout object -- codex writes to the file named by -o, never to
// stdout. See errors.go for the typed failures this can return.
func (c Codex) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	start := time.Now()

	dir, outPath, err := codexOutputDir()
	if err != nil {
		slog.Error("codex run: output dir", "job", req.Job, "error", err)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrStart
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			slog.Warn("codex: remove temp output dir", "dir", dir, "err", rmErr)
		}
	}()

	argv := codexArgv(req, outPath)

	res, runErr := c.run(ctx, req, argv, outPath, start)

	slog.Info("codex run",
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

// run is Run's process lifecycle, once argv and the -o path are ready:
// start the child, wait for it, and classify however it ended (design
// section 4.1), sharing the process-group setup and outcome classification
// claude.go's own run uses. It never logs; Run does that once, for every
// path, after this returns.
func (c Codex) run(ctx context.Context, req RunRequest, argv []string, outPath string, start time.Time) (RunResult, error) {
	cmd := exec.CommandContext(ctx, c.resolveBin(), argv...) //nolint:gosec // G204: bin is an operator-configured path (NewCodex), argv is built by codexArgv from validated fields, never from raw external input
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

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = firstCodexSessionID(stdout.bytes())
	}

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

	output, err := readCapped(outPath, maxOutputBytes)
	if err != nil {
		if errors.Is(err, ErrOutputTooLarge) {
			return res, ErrOutputTooLarge
		}
		res.Log = err.Error()
		return res, &InvalidOutputError{Reason: reasonNoZingElement}
	}

	resp, logText, ferr := parseFinalMessage(string(output), req.Job)
	res.Log = logText
	if ferr != nil {
		return res, ferr
	}
	res.Response = resp
	return res, nil
}

// version runs `codex --version` and returns its trimmed output, for the
// smoke test that skips when the binary is not on PATH.
func (c Codex) version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.resolveBin(), "--version").Output() //nolint:gosec // G204: bin is an operator-configured path (NewCodex), same as run's own exec.CommandContext
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
