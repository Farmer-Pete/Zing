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

// CappedRoundError is a capped review round's own error (tableCommit, owner
// decision Q3/Q6): Err is the capped lens's own error (a wrapped
// runtime.SessionLimitError or HeldError), and Finish carries every other
// lens's own terminal Run row -- the round's good results -- for the
// dispatcher's park write to terminalize with their own real outcome before
// it sweeps whatever lens run is still open as capped, so a lens that
// already finished is never rewritten as interrupted.
type CappedRoundError struct {
	Err    error
	Finish []store.Run
}

func (e *CappedRoundError) Error() string { return e.Err.Error() }
func (e *CappedRoundError) Unwrap() error { return e.Err }

// CappedFinish returns the Run rows a capped review round's own error
// carries to terminalize before parking (CappedRoundError), nil for any
// other capped error.
func CappedFinish(err error) []store.Run {
	var round *CappedRoundError
	if errors.As(err, &round) { //nolint:modernize // see errKind's own comment
		return round.Finish
	}
	return nil
}

// passThroughErr reports whether err is one of the four conditions every
// Claude-call handler (routeFailure, judgeRunAndRoute, respondRunAndRouteRaw,
// discussRunAndRoute) hands straight back to the dispatcher with no commit
// of its own: a shutdown cancel, a config error, a claim already lost
// before Reserve, or a capped Claude run or hold refusal. Named once so the
// same four-term list cannot drift between its several copies.
func passThroughErr(err error) bool {
	return errors.Is(err, runtime.ErrCanceled) || errors.Is(err, store.ErrClaimLost) || errors.Is(err, ErrConfig) || claudeCapped(err)
}

// recordCappedResume writes runJobWith's own "resumed after the Claude
// session limit" marker (design shape, "Resume marker") for a session whose
// newest run, before this one, was capped at cappedUntil under cappedRunID:
// a write failure is logged at WARN and never fails the run. The marker
// itself is written once per park (RecordCappedResume's own ordering rule),
// but every resumed run still gets its own "claude session limit resume"
// log line, with marker_written reporting whether this call was the one
// that wrote it, so every free resume is traceable by run_id even when
// another lens session already wrote the shared marker.
func recordCappedResume(ctx context.Context, d Deps, ticketID int64, rsv store.Reserved, cappedRunID int64, cappedUntil time.Time) {
	resumeCtx, cancel := onStartContext(ctx)
	defer cancel()
	written, err := d.Store.RecordCappedResume(resumeCtx, ticketID, rsv.RunID)
	if err != nil {
		slog.Warn("capped resume marker not written", "ticket_id", ticketID, "run_id", rsv.RunID, "error", err)
		return
	}
	slog.Info("claude session limit resume",
		"ticket_id", ticketID, "session_id", rsv.SessionID, "run_id", rsv.RunID,
		"capped_run_id", cappedRunID, "reset_at", cappedUntil.UTC().Format(time.RFC3339), "marker_written", written)
}
