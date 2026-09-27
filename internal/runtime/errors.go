package runtime

import (
	"errors"
	"fmt"
)

// The typed run failures (design section 4.1). Run returns (RunResult,
// error) where error is nil or exactly one of these four sentinels or two
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
	// ErrOutputTooLarge reports stdout or the final-message file exceeding
	// the 4 MiB cap.
	ErrOutputTooLarge = errors.New("runtime: output exceeded the 4 MiB cap")
)

// ExecError reports a process that ran and exited non-zero with no
// parseable result (design section 4.1). ExitCode is the real exit code, or
// -1 when the OS gave no numeric status (for example, a signal).
type ExecError struct {
	ExitCode int
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("runtime: process exited %d with no parseable result", e.ExitCode)
}

// InvalidOutputError reports a process that exited, but whose final message
// failed the document rule (design section 4.1). Reason is one of the five
// closed constants the final-message rule produces; the detailed validation
// error never lands here, only in RunResult.Log.
type InvalidOutputError struct {
	Reason string
}

func (e *InvalidOutputError) Error() string {
	return "runtime: invalid final message: " + e.Reason
}
