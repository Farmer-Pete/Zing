// Package runtime runs one Zing job turn against a model and returns its
// parsed response. Package 2 ships one working implementation, the
// scripted Fake (fake.go), and two stubs, Claude and Codex (claude.go,
// codex.go), whose real runs land in a later package.
package runtime

import (
	"context"
	"time"

	"zing/internal/response"
)

// Runtime runs one turn of a job and returns its parsed response.
type Runtime interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// RunRequest is what a caller asks a Runtime to run (design section 11).
type RunRequest struct {
	// Job is the job this run serves; the fake runtime keys its script on it.
	Job response.Job
	// Label is the task number or lens name, empty for a whole-job run. It is
	// part of the fake runtime's script key (design section 11, decision Q-c).
	Label string
	// Model is the exact model id from zing.toml.
	Model string
	// Prompt is the assembled prompt for this turn, inputs already fenced.
	Prompt string
	// Tools is the allowed tool list for the run.
	Tools []string
	// WorkDir is the working directory for the run.
	WorkDir string
	// Env is the scrubbed environment for the run.
	Env []string
	// Timeout bounds the run.
	Timeout time.Duration
	// IdleTimeout, when above 0, arms Claude's idle watchdog (Claude only):
	// the run is killed and reported as ErrStalled once its session
	// transcript has not grown for this long. 0 turns the watchdog off.
	// runJobWith sets it from machine.toml's idle_minutes. Codex and the
	// Fake ignore it.
	IdleTimeout time.Duration
	// MaxTurns caps the turns in one run (default 20). Claude does not read
	// this field in Claude Code 2.1.274: that CLI has no --max-turns flag
	// (verified against --help; the closest related flag, --max-budget-usd,
	// is a dollar cap, not a turn cap), so the per-job timeout runJob
	// already applies through the context deadline is the only bound on a
	// Claude run. The field stays for a future CLI version, or a runtime,
	// that does read it.
	MaxTurns int
	// SessionID resumes a prior run's session; empty starts a new one.
	SessionID string
	// RunToken rides in the run's env so a later zing scenarios finds this
	// ticket's scenarios.
	RunToken string
	// ExecPrefix, when non-empty, is prepended to the command: the process
	// started is ExecPrefix[0] with arguments ExecPrefix[1:], then the
	// resolved binary, then the runtime's own argv (design section 4.4). The
	// sandbox package builds it (sandbox.Sandbox.Prefix). The fake runtime
	// ignores it.
	ExecPrefix []string
	// OnStart, when non-nil, is called once, synchronously, right after the
	// process starts (design section 7.1, #45): Claude and Codex call it
	// after cmd.Start() succeeds and before the prompt is written to stdin
	// (the start handshake, below), so a caller can record the process's
	// identity before any work happens. The fake runtime calls it with PID 0
	// before running the scripted turn. OnStart must not block
	// indefinitely; runJobWith's own closure bounds its store write to 10s.
	OnStart func(StartInfo)
	// DenyBash lists the Bash command prefixes a build run may not run
	// (Claude only): Claude's PreToolUse hook blocks a call one matches.
	// Empty adds no hook. Codex and the Fake ignore it.
	DenyBash []string
}

// StartInfo is what a runtime reports to RunRequest.OnStart right after its
// process starts (design section 7.1, #45): PID is the process group
// leader (0 for the fake runtime, which has no real process), and
// SessionID is the agent session id when it is already known at start (the
// minted or resumed id for Claude; "" for Codex on a first turn, whose
// session id is not known until the first JSONL event arrives).
type StartInfo struct {
	PID       int
	SessionID string
}

// RunResult is what a Runtime returns for one turn (design section 6.9,
// 4.1). StderrLen and StderrSHA256 (the first 12 hex characters) are
// populated on every path once the process has started. Stderr holds the
// first maxStderrBytes of it, which the job layer saves to a private file
// and never logs (owner decision: discarding stderr left a failed run
// impossible to diagnose).
type RunResult struct {
	Response     response.Response
	SessionID    string
	Log          string
	AgentTime    time.Duration
	ExitCode     int
	StderrLen    int64
	StderrSHA256 string
	Stderr       []byte
	// FinalMessage is the agent's raw final message, capped by
	// capFinalMessage, populated whenever the runtime produces final
	// output; empty when it does not. Never logged.
	FinalMessage string
	// TranscriptPath is where the runtime's own transcript lives: set by
	// Claude's own runtime, and by job.runJobWith for Codex, which writes
	// Stdout to its own file under DATA_DIR/runs once Run returns. "" for
	// the Fake and a process that never started.
	TranscriptPath string
	// Stdout holds the last maxCodexStdoutBytes (64 KiB) of the child's raw
	// stdout (Codex only): the codex exec --json event stream, which is
	// where an error that kills the run within seconds, before any final
	// message exists, is actually reported. After job.runJobWith retries a
	// transient failure, it is the last 64 KiB of both attempts' stdout
	// joined. nil for Claude, the Fake, and a process that never started.
	// Never logged.
	Stdout []byte
	// FailureDetail is the runtime's own diagnosis of a failed run. For
	// Codex, Codex's own diagnosis of why a run with no final message
	// exited non-zero: the message of the last error or turn.failed event
	// on Stdout, or, when no such event is found, the last 20 non-empty
	// lines of Stdout. For Claude, stallDetail's text on ErrStalled. At
	// most 2048 bytes of valid UTF-8. Empty unless the runtime returned a
	// non-nil *ExecError or ErrStalled with an empty FinalMessage.
	// job.runJobWith's retryTransient and retryTimeout may rewrite it into
	// a note naming the matched transient pattern, or the first attempt and
	// the retry's own failure.
	FailureDetail string
	// LastEvent is the poll time of the last transcript growth Claude's
	// idle watchdog saw (Claude only, and only when RunRequest.IdleTimeout
	// was above 0); the zero time when the watchdog was off or saw no
	// growth.
	LastEvent time.Time
	// StopHookEvents, StopHookBlocks and StopHookUnread are the Claude Code
	// Stop hook's own counters for this run (Claude only, when a Stop hook
	// ran); all 0 for Codex, the Fake, and a Claude run with no hook.
	StopHookEvents int
	StopHookBlocks int
	StopHookUnread int
	// ValidateDenied counts this run's denied Bash calls whose command
	// contained "zing validate" (Claude only; 0 for Codex and the Fake).
	ValidateDenied int
}

// maxStderrBytes caps RunResult.Stderr.
const maxStderrBytes = 64 << 10

// Seconds rounds d up to a whole second, minimum 1, so every caller that
// reports agent time -- a Runtime's own logging and the job layer's
// runJob -- shares one definition of "agent seconds" (design section 4.1,
// 4.6): a run that took any time at all reports at least one second, and
// a run of exactly N seconds reports N, not N+1.
func Seconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	// Compute the quotient and remainder separately, rather than adding
	// (time.Second - 1) up front, so a duration within one second of
	// time.Duration's maximum cannot overflow and wrap to a bogus value.
	secs := int(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return max(secs, 1)
}
