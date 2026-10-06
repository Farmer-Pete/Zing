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
// runtime.SessionLimitError or HeldError), Finish carries every other
// lens's own terminal Run row -- the round's good results -- for the
// dispatcher's park write to terminalize with their own real outcome before
// it sweeps whatever lens run is still open as capped, so a lens that
// already finished is never rewritten as interrupted, and Round is the
// review round number this discards, so the dispatcher's own "discarded
// review round" marker can be scoped to review alone (r2f9): any other
// capped run (planning, build, discuss, a lone review lens's own
// HeldError) never writes that marker.
type CappedRoundError struct {
	Err    error
	Finish []store.Run
	Round  int
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

// CappedRound reports the review round number a capped review round's own
// error discards (CappedRoundError.Round), for the dispatcher's own
// "discarded review round" marker (r2f9); ok is false for any other capped
// error, including a lone lens's own HeldError outside a round-wide cap.
func CappedRound(err error) (int, bool) {
	var round *CappedRoundError
	if errors.As(err, &round) { //nolint:modernize // see errKind's own comment
		return round.Round, true
	}
	return 0, false
}

// recordCappedResume writes runJobWith's own "resumed after the Claude
// session limit" marker (design shape, "Resume marker") for a session whose
// newest run, before this one, was prev, itself capped (prev.CappedUntil):
// a write failure is logged at WARN but never skips the "claude session
// limit resume" line below it (r2f17) -- the marker write is best-effort,
// the free resume itself is not, and the ticket requires logging every
// resume by run id and reset time regardless. The marker itself is written
// once per park (RecordCappedResume's own ordering rule), but every resumed
// run still gets its own "claude session limit resume" log line, with
// marker_written reporting whether this call was the one that wrote it, so
// every free resume is traceable by run_id even when another lens session
// already wrote the shared marker.
func recordCappedResume(ctx context.Context, d Deps, ticketID int64, rsv store.Reserved, prev store.Run) {
	resumeCtx, cancel := onStartContext(ctx)
	defer cancel()
	written, err := d.Store.RecordCappedResume(resumeCtx, ticketID, rsv.RunID)
	if err != nil {
		written = false
		slog.Warn("capped resume marker not written", "ticket_id", ticketID, "run_id", rsv.RunID, "error", err)
	}
	slog.Info("claude session limit resume",
		"ticket_id", ticketID, "session_id", rsv.SessionID, "run_id", rsv.RunID,
		"capped_run_id", prev.ID, "reset_at", prev.CappedUntil.UTC().Format(time.RFC3339), "marker_written", written)
}
