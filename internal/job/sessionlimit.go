// sessionlimit.go -- #45: the Claude session-limit hold gate's own error
// type, the capped-error detector every Claude-call handler passes through
// unescalated, and the capped-resume marker runJobWith writes once a
// parked session resumes for free.
package job

import (
	"context"
	"errors"
	"fmt"
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

// CappedInfo is the pieces the dispatcher's parkCapped needs for any capped
// Claude error (design shape, "CappedUntil"; simplification fix, one
// accessor in place of three): Until is the instant the ticket should wait
// for; Finish carries a capped review round's own already-finished lens
// runs (CappedRoundError.Finish, owner decision Q6) to terminalize before
// the park sweep, nil outside a round; Round is the review round number a
// round error discards, zero outside a round; DiscardMarker is the
// dispatcher's own "discarded review round N" marker body, built here so
// the review marker text stays in this package, empty outside a round
// (r2f9: scoped to review alone, never for some other job's own plain cap).
type CappedInfo struct {
	Until         time.Time
	Finish        []store.Run
	Round         int
	DiscardMarker string
}

// Capped reports whether err is a Claude session-limit hit, a capped review
// round, or a hold refusal, and the CappedInfo the dispatcher parks on.
func Capped(err error) (CappedInfo, bool) {
	var info CappedInfo
	var sl *runtime.SessionLimitError
	switch {
	case errors.As(err, &sl): //nolint:modernize // errcheck check-blank, as errKind
		info.Until = sl.ResetAt
	default:
		var held *HeldError
		if !errors.As(err, &held) { //nolint:modernize // same
			return CappedInfo{}, false
		}
		info.Until = held.Until
	}
	var round *CappedRoundError
	if errors.As(err, &round) { //nolint:modernize // same
		info.Finish = round.Finish
		info.Round = round.Round
		info.DiscardMarker = fmt.Sprintf("%s%d: Claude session limit", CappedRoundDiscardedPrefix, round.Round)
	}
	return info, true
}

// claudeCapped is Capped's bool alone, for the handler passthroughs that
// only need to know whether to escalate.
func claudeCapped(err error) bool {
	_, ok := Capped(err)
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
// review round" marker (Capped's DiscardMarker) can be scoped to review
// alone: any other capped run (planning, build, discuss, a lone review
// lens's own HeldError) never writes that marker.
type CappedRoundError struct {
	Err    error
	Finish []store.Run
	Round  int
}

func (e *CappedRoundError) Error() string { return e.Err.Error() }
func (e *CappedRoundError) Unwrap() error { return e.Err }

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
		slog.Warn("capped resume marker not written", "ticket_id", ticketID, "run_id", rsv.RunID, "error", err)
	}
	slog.Info("claude session limit resume",
		"ticket_id", ticketID, "session_id", rsv.SessionID, "run_id", rsv.RunID,
		"capped_run_id", prev.ID, "reset_at", prev.CappedUntil.UTC().Format(time.RFC3339), "marker_written", written)
}
