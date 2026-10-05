// sessionlimit.go -- #45: the Claude session-limit hold gate's own error
// type, the capped-error detector every Claude-call handler passes through
// unescalated, and the capped-resume marker runJobWith writes once a
// parked session resumes for free.
package job

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"zing/internal/runtime"
	"zing/internal/store"
)

// runtimeClaude is the one machine.toml runtime name runJobWith's hold gate
// refuses (design shape, "Hold"): a codex job keeps running while Claude is
// held.
const runtimeClaude = "claude"

// HeldError is runJobWith's refusal of a claude job, before Reserve, while
// the claude_hold_until setting is still in the future (design shape,
// "Hold").
type HeldError struct{ Until time.Time }

func (e *HeldError) Error() string {
	return "job: claude runs held until " + e.Until.UTC().Format(time.RFC3339)
}

// CappedUntil reports whether err is a Claude session-limit hit or a hold
// refusal, and the instant the ticket should wait for (design shape,
// "CappedUntil"): the dispatcher parks on either.
func CappedUntil(err error) (time.Time, bool) {
	var sl *runtime.SessionLimitError
	if errors.As(err, &sl) { //nolint:modernize // errcheck check-blank, as errKind
		return sl.ResetAt, true
	}
	var held *HeldError
	if errors.As(err, &held) { //nolint:modernize // same
		return held.Until, true
	}
	return time.Time{}, false
}

// claudeCapped is CappedUntil's bool alone, for the handler passthroughs
// that only need to know whether to escalate.
func claudeCapped(err error) bool {
	_, ok := CappedUntil(err)
	return ok
}

// recordCappedResume writes runJobWith's own "resumed after the Claude
// session limit" marker (design shape, "Resume marker") for a session whose
// newest run, before this one, was capped: a write failure is logged at
// WARN and never fails the run, and no line is logged at all when the
// marker was not actually written (RecordCappedResume's own ordering rule:
// one marker per park, however many lens sessions resume from it).
func recordCappedResume(ctx context.Context, d Deps, ticketID int64, rsv store.Reserved, cappedPrev store.Run) {
	resumeCtx, cancel := onStartContext(ctx)
	defer cancel()
	written, err := d.Store.RecordCappedResume(resumeCtx, ticketID, rsv.RunID)
	if err != nil {
		slog.Warn("capped resume marker not written", "ticket_id", ticketID, "run_id", rsv.RunID, "error", err)
		return
	}
	if !written {
		return
	}
	resetAt := ""
	if cappedPrev.CappedUntil != nil {
		resetAt = cappedPrev.CappedUntil.UTC().Format(time.RFC3339)
	}
	slog.Info("claude session limit resume",
		"ticket_id", ticketID, "session_id", rsv.SessionID, "run_id", rsv.RunID,
		"capped_run_id", cappedPrev.ID, "reset_at", resetAt)
}
