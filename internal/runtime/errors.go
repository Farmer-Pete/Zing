package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// The typed run failures (design section 4.1). Run returns (RunResult,
// error) where error is nil or exactly one of these sentinels or two
// typed errors, so a caller (job.runJob, section 4.6) routes on the failure
// kind with errors.Is/errors.As rather than parsing text. This package only
// declares the shapes; the real runtimes that produce them land in tasks 5
// (claude) and 9 (codex).
var (
	// ErrStart reports a process that could not start.
	ErrStart = errors.New("runtime: process could not start")
	// ErrTimeout reports a process the job deadline killed.
	ErrTimeout = errors.New("runtime: job deadline exceeded")
	// ErrCanceled reports a process killed because the parent context was
	// canceled (dispatcher shutdown), not because the job deadline expired.
	ErrCanceled = errors.New("runtime: parent context canceled")
	// ErrStalled reports a Claude process the idle watchdog killed: its
	// session transcript did not grow for RunRequest.IdleTimeout.
	// RunResult.LastEvent holds the last growth it saw.
	ErrStalled = errors.New("runtime: no transcript growth within the idle limit")
	// ErrOutputTooLarge reports stdout or the final-message file exceeding
	// the 4 MiB cap.
	ErrOutputTooLarge = errors.New("runtime: output exceeded the 4 MiB cap")
	// ErrNoOAuthToken reports an empty Claude oauth token (PKG9-PLAN.md
	// section 4.6, D26): Claude.Run refuses to start the child at all
	// rather than exec a CLI that can only fail to log in.
	ErrNoOAuthToken = errors.New("runtime: claude: no oauth token configured")
	// ErrCodexFullAccessNeedsExecPrefix reports a judge-job Codex.Run call
	// with no ExecPrefix (PKG9-PLAN.md section 4.6, D20): the judge needs
	// danger-full-access because a seatbelt cannot start inside another,
	// but only once Zing's own judge profile is actually wrapping the
	// process. With no exec prefix that outer profile is not in place, so
	// Codex.Run refuses to start rather than run Codex with no containment
	// at all.
	ErrCodexFullAccessNeedsExecPrefix = errors.New("runtime: codex: full access needs an exec prefix")
)

// ExecError reports a process that ran and exited non-zero with no
// parseable result (design section 4.1). ExitCode is the real exit code, or
// -1 when the OS gave no numeric status (for example, a signal).
type ExecError struct {
	ExitCode int
	// Transient is the pattern codexTransientMatch found in a Codex run's
	// FailureDetail (Codex only, and only when that detail came from an
	// error or turn.failed event): one of 429, rate limit, 500, 502, 503,
	// 504, connection reset, stream disconnected, or "" when no such
	// pattern matched, or for every other runtime. job.retryTransient
	// retries the run once, on this same ExecError, when it is non-empty.
	// Not part of Error().
	Transient string
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("runtime: process exited %d with no parseable result", e.ExitCode)
}

// InvalidOutputError reports a process that exited, but whose final message
// failed the document rule (design section 4.1). Reason is one of the five
// closed constants the final-message rule produces. Detail carries the
// parser's or validator's own error text, capped by capDetail.
type InvalidOutputError struct {
	Reason string
	// Detail is the parser's error for reasonNoZingElement or the
	// validator's error list for reasonFailedValidation, empty otherwise. It can quote the model's text, so it is never part
	// of Error() and the job layer fences it before a prompt sees it.
	Detail string
}

func (e *InvalidOutputError) Error() string {
	return "runtime: invalid final message: " + e.Reason
}

// startErr wraps the cause of a failed cmd.StdinPipe or cmd.Start under
// ErrStart (design section "startErr keeps the cause and drops the path",
// #23): the syscall.Errno, exec.ErrNotFound/ErrDot, or context error inside
// err, never the *os.PathError or *exec.Error around it, whose own text
// carries the binary path. errors.Is(err, ErrStart) always holds on the
// result; any cause this function does not recognize yields bare ErrStart,
// losing the diagnosis but never leaking a path.
func startErr(err error) error {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return fmt.Errorf("%w: %w", ErrStart, errno)
	}
	for _, cause := range []error{exec.ErrNotFound, exec.ErrDot, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%w: %w", ErrStart, cause)
		}
	}
	return ErrStart
}
