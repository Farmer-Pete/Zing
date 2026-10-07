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
	"sync"
	"time"
	"unicode/utf8"

	"zing/internal/response"
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

// codexWantsFullAccess reports whether req's job needs danger-full-access
// (PKG9-PLAN.md section 4.6, D20): only the judge, since a seatbelt cannot
// start inside another and the judge profile already wraps Codex in
// Zing's own seatbelt. Every other job (today, only planreview) stays on
// Codex's own read-only sandbox.
func codexWantsFullAccess(req RunRequest) bool {
	return req.Job == response.JobJudge
}

// codexSandboxArgs returns the "-s ..."/"-c sandbox_mode=..." pair
// codexArgv appends, and codexWantsFullAccess's own fail-closed check: a
// request that wants full access with no exec prefix is refused before
// anything else is built (PKG9-PLAN.md section 4.6, D20).
func codexSandboxArgs(req RunRequest) ([]string, error) {
	fullAccess := codexWantsFullAccess(req)
	if fullAccess && len(req.ExecPrefix) == 0 {
		return nil, ErrCodexFullAccessNeedsExecPrefix
	}
	switch {
	case fullAccess && req.SessionID == "":
		return []string{"-s", "danger-full-access"}, nil
	case fullAccess:
		return []string{"-c", `sandbox_mode="danger-full-access"`}, nil
	case req.SessionID == "":
		return []string{"-s", "read-only"}, nil
	default:
		return []string{"-c", `sandbox_mode="read-only"`}, nil
	}
}

// codexSkillsOffSetting is the -c pair that turns off the judge's attempt to
// scan its own skill directories, including `~/.agents/skills` (confirmed on
// the host against Codex 0.160.0: `codex features list` names
// `skip_host_skill_discovery`, and `codex exec -c features.skip_host_skill_discovery=true
// --strict-config` accepts it as a known key; judge.sb already denies the
// read at the OS level, so this is a second, best-effort layer).
const codexSkillsOffSetting = "features.skip_host_skill_discovery=true"

// codexShellEnvVars are the req.Env names codexShellEnvArgs copies into
// Codex's shell_environment_policy.set, in this order.
var codexShellEnvVars = []string{"TMPDIR", "TMPPREFIX"}

// codexTOMLString escapes a value for a TOML basic string.
var codexTOMLString = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// codexShellEnvArgs returns one -c shell_environment_policy.set.NAME="VALUE"
// pair per codexShellEnvVars name present in env, last entry winning as in
// codexOutputBase. Codex builds the environment of the commands it runs
// from this policy, not from its own environment: run 1459's judge check
// saw the Darwin per-user TMPDIR, which judge.sb denies. The key follows
// Codex's config docs. Planning could not run it against 0.160.0
// (Codex out of quota until 2026-10-30), and codexArgv passes no
// --strict-config, so a misnamed key would be ignored silently.
// TestLive_CodexShellEnvTmpdir is its proof.
func codexShellEnvArgs(env []string) []string {
	var args []string
	for _, name := range codexShellEnvVars {
		value, found := "", false
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				value, found = v, true
			}
		}
		if found {
			args = append(args, "-c", "shell_environment_policy.set."+name+`="`+codexTOMLString.Replace(value)+`"`)
		}
	}
	return args
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
// Sandboxing is expressed differently on each subcommand: a first turn
// takes -s/--sandbox directly, but codex exec resume has no such flag
// (confirmed against its own --help), so a resume expresses the same
// policy with -c sandbox_mode="..." instead. Which policy, read-only or
// danger-full-access, is codexSandboxArgs's own call (PKG9-PLAN.md section
// 4.6, D20). The forbidden --dangerously-bypass-approvals-and-sandbox flag
// (real, and present in both --help outputs) never appears.
//
// A judge-job request also carries one judge-only "-c" pair right after the
// sandbox flags: codexSkillsOffSetting. Codex 0.160.0's config schema (104
// top-level fields) has no key that bounds how long one shell command may
// run -- background_terminal_max_timeout only controls when a still-running
// command is handed to a background terminal the agent polls, so it is not
// a substitute -- and prompts/judge.md's own instruction to run a long check
// in the background and poll it covers that case instead. Every other
// job's argv is unchanged.
//
// Every job, first turn and resume, also carries codexShellEnvArgs's own
// "-c" pairs right before -m: shell_environment_policy.set.TMPDIR and
// .TMPPREFIX, built from req.Env, so the commands Codex's shell runs (a
// judge's sealed `zing check SID`, among others) see the run's own temp
// directories rather than the host's shared ones the sandbox profiles deny.
// TestLive_CodexShellEnvTmpdir is the live proof that Codex applies them.
//
// req.Tools is deliberately not read here: codex exec has no per-tool
// allowlist flag, so the sandbox mode above is the control instead
// (PKG7-PLAN.md section 4).
func codexArgv(req RunRequest, outPath string) ([]string, error) {
	sandboxArgs, err := codexSandboxArgs(req)
	if err != nil {
		return nil, err
	}
	argv := []string{"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check"}
	argv = append(argv, sandboxArgs...)
	if req.Job == response.JobJudge {
		argv = append(argv, "-c", codexSkillsOffSetting)
	}
	argv = append(argv, codexShellEnvArgs(req.Env)...)
	argv = append(argv, "-m", req.Model, "--json", "-o", outPath)
	if req.SessionID != "" {
		argv = append(argv, "resume", req.SessionID)
	}
	argv = append(argv, "-")
	return argv, nil
}

// codexOutputDir creates a fresh 0700 directory holding the file Run passes
// to -o (design section 4.1): codex writes the run's final message there
// directly, never to stdout, so a chatty JSONL stream can never smuggle a
// second document past the final-message rule. base is where os.MkdirTemp
// creates it (PKG9-PLAN.md section 4.6): codexOutputBase's own value, the
// run's own TMPDIR when req.Env carries one, else os.TempDir(). A sandboxed
// Codex can write only its own run directory, so the -o file must live
// there, not the host's shared temp directory the profile denies.
// os.MkdirTemp already creates the directory at mode 0700 (unaffected by
// any ordinary umask, since 0700 has no group or other bits for one to
// clear); the file inside it gets an explicit chmod to 0600 as well, so
// both are guaranteed regardless of the platform's default CreateTemp
// mode. The caller must defer os.RemoveAll on the returned dir on every
// path -- a start failure after the directory exists included -- so a
// killed or crashed run never leaves the file behind.
func codexOutputDir(base string) (dir, path string, err error) {
	dir, err = os.MkdirTemp(base, "zing-codex-out-*")
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

// codexOutputBase returns the directory codexOutputDir creates its run's -o
// file under (PKG9-PLAN.md section 4.6): the value of TMPDIR in req.Env
// when set, else os.TempDir(), the same default os.MkdirTemp("", ...)
// already used before this task scoped the -o file to the sandbox's own
// run directory. req.Env is already the scrubbed, final environment the
// job layer builds (sandboxed or not, PKG9-PLAN.md section 7.3), so a
// plain linear scan is enough; the last TMPDIR entry wins, matching how
// os/exec itself resolves a duplicate name in cmd.Env.
func codexOutputBase(env []string) string {
	base := os.TempDir()
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			base = value
		}
	}
	return base
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

// readFinalMessageFile reads Codex's -o file for RunResult.FinalMessage,
// best effort: at most maxFinalMessageBytes+utf8.UTFMax bytes are read and
// then capped; a missing or unreadable file gives "" and logs nothing
// (classifyProcessOutcome and the existing readCapped path already report
// the run's real failure).
func readFinalMessageFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, int64(maxFinalMessageBytes)+utf8.UTFMax))
	if err != nil {
		return ""
	}
	return capFinalMessage(string(data))
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

// maxCodexStdoutBytes caps RunResult.Stdout: the last 64 KiB of the child's
// raw stdout, kept so an early error event in a run that dies within
// seconds survives even past a chatty stream (design: RunResult.Stdout).
const maxCodexStdoutBytes = 64 << 10

// tailWriter retains at most limit bytes, the most recently written ones,
// unlike capWriter, which keeps the first bytes up to its limit and drops
// everything after. Codex.run pairs it with capWriter on the same
// io.MultiWriter so a late error event, the kind that actually explains a
// run that dies within seconds, is never the part a chatty stream's cap
// drops.
type tailWriter struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.limit; over > 0 {
		w.buf = w.buf[over:]
	}
	return len(p), nil
}

// bytes returns a copy of what w currently holds.
func (w *tailWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// maxFailureDetailBytes caps RunResult.FailureDetail (design:
// codexFailureDetail).
const maxFailureDetailBytes = 2048

// noStdoutFailureDetail is codexFailureDetail's result for stdout with no
// non-empty line at all: a process that died before printing anything.
const noStdoutFailureDetail = "codex wrote nothing to stdout"

// codexEventError and codexEventTurnFailed are the two failure event types
// codexFailureDetail and codexCommandRejection both read.
const (
	codexEventError      = "error"
	codexEventTurnFailed = "turn.failed"
)

// codexFailureEventLine is the subset of one codex exec --json JSONL
// event's fields codexFailureDetail reads: an "error" event's own message,
// or a "turn.failed" event's error.message (Codex's documented event shape,
// openai/codex sdk/typescript/src/events.ts, commit f6ad902).
type codexFailureEventLine struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CapFailureDetail cuts s on a rune boundary so the result is at most
// maxFailureDetailBytes. Exported so job.retryTransient can cut the retry
// note it builds from two attempts' FailureDetail to the same bound, on the
// same rune boundary, rather than growing its own copy of this rule.
func CapFailureDetail(s string) string {
	if len(s) <= maxFailureDetailBytes {
		return s
	}
	cut := maxFailureDetailBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// codexFailureDetail is Codex's own diagnosis of why a run with no final
// message exited non-zero (design: RunResult.FailureDetail): it walks
// stdout's JSONL lines once, remembering the last line whose type is
// "error" (its message) or "turn.failed" (its error.message) and whose
// message is non-empty after TrimSpace, and every trimmed non-empty line in
// case none is found. An event line with an empty message is skipped, so an
// earlier event that did carry a message, or the last-20-lines fallback,
// still applies rather than this function returning an empty detail. When
// it finds a usable event, it returns that message with fromEvent true.
// Otherwise it returns the last 20 non-empty lines of stdout (joined by
// "\n"), or noStdoutFailureDetail when stdout has no non-empty line at all,
// both with fromEvent false. The result is always at most
// maxFailureDetailBytes, cut on a rune boundary.
func codexFailureDetail(stdout []byte) (detail string, fromEvent bool) {
	var lastEventMessage string
	haveEvent := false
	var nonEmpty []string
	for line := range bytes.SplitSeq(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		nonEmpty = append(nonEmpty, string(line))

		var ev codexFailureEventLine
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		var msg string
		switch ev.Type {
		case codexEventError:
			msg = ev.Message
		case codexEventTurnFailed:
			msg = ev.Error.Message
		default:
			continue
		}
		if msg = strings.TrimSpace(msg); msg != "" {
			lastEventMessage, haveEvent = msg, true
		}
	}
	if haveEvent {
		return CapFailureDetail(lastEventMessage), true
	}

	if len(nonEmpty) == 0 {
		return noStdoutFailureDetail, false
	}
	const maxTailLines = 20
	if len(nonEmpty) > maxTailLines {
		nonEmpty = nonEmpty[len(nonEmpty)-maxTailLines:]
	}
	return CapFailureDetail(strings.Join(nonEmpty, "\n")), false
}

// codexTransientPatterns is codexTransientMatch's closed list (design
// goals: "A Codex failure whose error text matches..."), checked in this
// order so the first match wins; none of them ever matches inside
// usageLimitPhrase, which codexTransientMatch checks first.
var codexTransientPatterns = []string{
	"429", "rate limit", "500", "502", "503", "504", "connection reset", "stream disconnected",
}

// usageLimitPhrase is codexTransientMatch's own carve-out: Codex's usage-cap
// message often contains "429" as a parenthetical, but running the same
// prompt again 2s later never clears a usage cap the way it can a transient
// 429 from the API itself.
const usageLimitPhrase = "usage limit"

// codexTransientMatch reports which of codexTransientPatterns appears in
// detail, a codexFailureDetail result that came from an error or
// turn.failed event (design: codexTransientMatch), or "" when none does or
// detail itself names a usage limit. Case-insensitive.
func codexTransientMatch(detail string) string {
	lower := strings.ToLower(detail)
	if strings.Contains(lower, usageLimitPhrase) {
		return ""
	}
	for _, pattern := range codexTransientPatterns {
		if strings.Contains(lower, pattern) {
			return pattern
		}
	}
	return ""
}

// commandRejectionPhrase is the text Codex's command policy puts in every
// refusal, such as "rm -f style commands are not permitted. Use a safer
// approach" (#94).
const commandRejectionPhrase = "are not permitted"

// codexRejectionEventLine is the subset of one JSONL event that
// codexCommandRejection reads. It embeds codexFailureEventLine for the
// Type, Message, and Error.Message fields the two parsers share, and adds
// Item for the command_execution case codexFailureDetail never looks at.
type codexRejectionEventLine struct {
	codexFailureEventLine
	Item struct {
		Type             string `json:"type"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		Status           string `json:"status"`
		ExitCode         *int   `json:"exit_code"`
	} `json:"item"`
}

// codexCommandRejection finds Codex's refusal of a command (#94): the last
// matching line in stdout, or failing that in stderr. It returns the
// refused command (empty when the event names none) and the one line
// holding commandRejectionPhrase, verbatim, or two empty strings. Only a
// non-JSON line, an error or turn.failed event, or a command_execution
// item that did not complete with exit code 0 counts. Agent messages,
// reasoning, and successful commands that merely quote the phrase, such as
// a cat of this file, never do.
func codexCommandRejection(stdout, stderr []byte) (command, line string) {
	for _, stream := range [][]byte{stdout, stderr} {
		for raw := range bytes.SplitSeq(stream, []byte("\n")) {
			raw = bytes.TrimSpace(raw)
			if !bytes.Contains(raw, []byte(commandRejectionPhrase)) {
				continue
			}
			var ev codexRejectionEventLine
			if json.Unmarshal(raw, &ev) != nil {
				command, line = "", string(raw)
				continue
			}
			switch {
			case ev.Item.Type == "command_execution":
				clean := ev.Item.Status == "completed" && ev.Item.ExitCode != nil && *ev.Item.ExitCode == 0
				if l := phraseLine(ev.Item.AggregatedOutput); l != "" && !clean {
					command, line = strings.TrimSpace(ev.Item.Command), l
				}
			case ev.Item.Type != "":
				// Any other item only quotes text; it never refuses.
			case ev.Type == codexEventError:
				if l := phraseLine(ev.Message); l != "" {
					command, line = "", l
				}
			case ev.Type == codexEventTurnFailed:
				if l := phraseLine(ev.Error.Message); l != "" {
					command, line = "", l
				}
			}
		}
		if line != "" {
			return command, line
		}
	}
	return "", ""
}

// phraseLine returns the first trimmed line of s holding
// commandRejectionPhrase, or "".
func phraseLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); strings.Contains(l, commandRejectionPhrase) {
			return l
		}
	}
	return ""
}

// codexRejectionDetail puts a command rejection in front of Codex's own
// failure detail (owner decision Q2), so the 2048-byte cap never cuts the
// cause: "codex refused a command: COMMAND: LINE; codex's own error:
// DETAIL". It drops "COMMAND: " when command is empty, and drops the
// detail half when detail is blank or already holds line. With no line it
// returns detail unchanged.
func codexRejectionDetail(command, line, detail string) string {
	if line == "" {
		return detail
	}
	head := "codex refused a command: " + line
	if command != "" {
		head = "codex refused a command: " + command + ": " + line
	}
	if strings.TrimSpace(detail) == "" || strings.Contains(detail, line) {
		return CapFailureDetail(head)
	}
	return CapFailureDetail(head + "; codex's own error: " + detail)
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

	// Checked before anything touches the filesystem: a judge-job request
	// with no exec prefix refuses outright (PKG9-PLAN.md section 4.6,
	// D20), the same sandbox-mode decision codexArgv makes below once
	// outPath is known.
	if _, err := codexSandboxArgs(req); err != nil {
		slog.Error("codex run: build argv", "job", req.Job, "error", err)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, err
	}

	dir, outPath, err := codexOutputDir(codexOutputBase(req.Env))
	if err != nil {
		slog.Error("codex run: output dir", "job", req.Job, "error", err)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, ErrStart
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			slog.Warn("codex: remove temp output dir", "dir", dir, "err", rmErr)
		}
	}()

	argv, err := codexArgv(req, outPath)
	if err != nil {
		slog.Error("codex run: build argv", "job", req.Job, "error", err)
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, err
	}

	res, runErr := c.run(ctx, req, argv, outPath, start)

	if errors.Is(runErr, ErrStart) {
		slog.Warn("codex run: start failed", "job", req.Job, "run_token", req.RunToken, "error", runErr)
	}

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

// commandNameArgs resolves the name and args exec.CommandContext should
// start (PKG9-PLAN.md section 4.6: "Codex.run honors ExecPrefix exactly as
// Claude.run does"): with no ExecPrefix, the resolved binary and argv
// unchanged; with one, ExecPrefix[0] as name and ExecPrefix[1:] plus the
// resolved binary plus argv as args, so the sandbox's own "sandbox-exec -D
// ... -p <profile>" prefix wraps the real codex invocation without argv
// itself ever changing shape (claude.go's own commandNameArgs, mirrored
// here rather than shared: the two runtimes resolve their own binary
// differently and neither needs the other's).
func (c Codex) commandNameArgs(req RunRequest, argv []string) (name string, args []string) {
	if len(req.ExecPrefix) == 0 {
		return c.resolveBin(), argv
	}
	args = make([]string, 0, len(req.ExecPrefix)-1+1+len(argv))
	args = append(args, req.ExecPrefix[1:]...)
	args = append(args, c.resolveBin())
	args = append(args, argv...)
	return req.ExecPrefix[0], args
}

// run is Run's process lifecycle, once argv and the -o path are ready:
// start the child, wait for it, and classify however it ended (design
// section 4.1), sharing the process-group setup and outcome classification
// claude.go's own run uses. It never logs; Run does that once, for every
// path, after this returns.
func (c Codex) run(ctx context.Context, req RunRequest, argv []string, outPath string, start time.Time) (RunResult, error) {
	name, args := c.commandNameArgs(req, argv)
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: bin is an operator-configured path (NewCodex), argv is built by codexArgv from validated fields, and ExecPrefix (when set) is the sandbox's own prefix (sandbox.Sandbox.Prefix) -- never raw external input
	cmd.Dir = req.WorkDir
	cmd.Env = agentEnv(req)
	configureProcessGroup(cmd)

	stdout := &capWriter{limit: maxOutputBytes}
	tail := &tailWriter{limit: maxCodexStdoutBytes}
	cmd.Stdout = io.MultiWriter(stdout, tail)

	var stderrCount countingWriter
	stderrHash := sha256.New()
	stderrCap := &capWriter{limit: maxStderrBytes}
	cmd.Stderr = io.MultiWriter(&stderrCount, stderrHash, stderrCap)

	// The start handshake (design section 7.1, #45): see claude.go's own
	// run for why stdin is a pipe written only after OnStart returns.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, startErr(err)
	}

	if err = cmd.Start(); err != nil {
		return RunResult{ExitCode: -1, AgentTime: time.Since(start)}, startErr(err)
	}

	if req.OnStart != nil {
		req.OnStart(StartInfo{PID: cmd.Process.Pid, SessionID: req.SessionID})
	}

	go func() {
		_, _ = io.WriteString(stdin, req.Prompt) //nolint:errcheck // EPIPE means the agent already exited; cmd.Wait reports the real outcome
		_ = stdin.Close()                        //nolint:errcheck // same: a close error here never changes the run's outcome
	}()

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
		Stderr:       stderrCap.bytes(),
		Stdout:       tail.bytes(),
	}
	res.FinalMessage = readFinalMessageFile(outPath)

	if outcomeErr := classifyProcessOutcome(ctx, stdout.overflowed(), waitErr, res.ExitCode); outcomeErr != nil {
		if errors.Is(outcomeErr, ErrTimeout) || errors.Is(outcomeErr, ErrCanceled) {
			res.ExitCode = -1
		}
		var execErr *ExecError
		if errors.As(outcomeErr, &execErr) && res.FinalMessage == "" {
			var fromEvent bool
			res.FailureDetail, fromEvent = codexFailureDetail(res.Stdout)
			if fromEvent {
				execErr.Transient = codexTransientMatch(res.FailureDetail)
			}
			// After the transient match, so a rejection's own text never
			// makes a run retryable (#94).
			command, line := codexCommandRejection(stdout.bytes(), res.Stderr)
			res.FailureDetail = codexRejectionDetail(command, line, res.FailureDetail)
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

	resp, logText, ferr := parseFinalMessage(string(output), req.Job, req.RunToken)
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
