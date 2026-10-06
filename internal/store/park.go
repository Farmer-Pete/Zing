// park.go -- the Claude session limit's one write path (#45): parking a
// ticket's open runs, reading and raising the global hold, and marking a
// capped session's free resume. ParkRuns shares interruptClaimedRuns
// (interrupt.go) with the shutdown and dead-serve-reclaim interrupt, so
// there is one place that terminalizes an open run as interrupted.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// claudeHoldSettingKey is the settings row runJobWith's hold gate reads
// before Reserve (design shape, "Hold"): a fixed-layout UTC string
// (formatTime), raised by ParkRuns, never cleared.
const claudeHoldSettingKey = "claude_hold_until"

// cappedParkedPrefix and cappedResumedPrefix are the two system "update"
// message bodies ParkRuns and RecordCappedResume write, and the prefixes
// RecordCappedResume's own ordering query matches them by.
const (
	cappedParkedPrefix  = "parked until "
	cappedResumedPrefix = "resumed after the Claude session limit"
)

// ParkResult is what ParkRuns did: Applied false when the claim fence
// missed (nothing written); RunIDs the open runs it parked, ascending, nil
// when the claim had none open; FinishedRunIDs the finish rows it
// terminalized by their own real outcome first (owner decision Q6), in the
// same order given, for a caller's own logging (r2f21).
type ParkResult struct {
	Applied        bool
	RunIDs         []int64
	FinishedRunIDs []int64
}

// ParkRuns is InterruptRuns for a Claude session limit (or a hold refusal,
// #45): finish is terminalized first, each by its own real outcome (owner
// decision Q6: a review lens that already finished before the cap keeps its
// own run record true), then every run of ticketID's sessions still open
// becomes interrupted with capped_until = until, the settings
// claude_hold_until row rises to until when later, one "parked until"
// update is written, and the claim is cleared, all in one transaction
// fenced on owner and expires exactly like InterruptRuns.
func (s *Store) ParkRuns(ctx context.Context, ticketID int64, owner string, expires, until time.Time, finish []Run) (ParkResult, error) {
	runIDs, finishedIDs, applied, err := s.interruptClaimedRuns(ctx, ticketID, owner, expires, interruptOpts{Park: &until, Finish: finish})
	if err != nil {
		return ParkResult{}, err
	}
	return ParkResult{Applied: applied, RunIDs: runIDs, FinishedRunIDs: finishedIDs}, nil
}

// ClaudeHold returns the claude_hold_until setting in time.Local; ok is
// false when the row is absent or NULL (GetSetting's own "exists but NULL"
// case, which reads back as an empty string).
func (s *Store) ClaudeHold(ctx context.Context) (until time.Time, ok bool, err error) {
	value, present, err := s.GetSetting(ctx, claudeHoldSettingKey)
	if err != nil {
		return time.Time{}, false, err
	}
	if !present || value == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(fixedTimeLayout, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("claude hold: parse %q: %w", value, err)
	}
	return t.Local(), true, nil
}

// upsertClaudeHoldTx raises the claude_hold_until setting to until, or
// seeds it, never lowering an already-stored value: formatTime's fixed
// 20-byte layout makes SQLite's MAX() a correct instant comparison on the
// TEXT column.
func upsertClaudeHoldTx(ctx context.Context, tx *sql.Tx, until time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = MAX(COALESCE(value, ''), excluded.value)`,
		claudeHoldSettingKey, formatTime(until),
	); err != nil {
		return fmt.Errorf("upsert claude hold: %w", err)
	}
	return nil
}

// insertParkedMarkerTx writes ParkRuns' own "parked until" update, inside
// the same transaction as the runs it stamped.
func (s *Store) insertParkedMarkerTx(ctx context.Context, tx *sql.Tx, ticketID int64, until time.Time, runIDs []int64) error {
	return s.insertMessageTx(ctx, tx, Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
		Body: parkedMarkerBody(until, runIDs),
	})
}

// parkedMarkerBody gives "parked until 12:20pm America/New_York (run 1625):
// Claude session limit", or "...(runs 1625, 1626): ..." for more than one
// run, ids ascending (design shape). The clock is until.Format("3:04pm");
// the zone is until.Location().String(), or until.Format("MST") when that
// string is "Local".
func parkedMarkerBody(until time.Time, runIDs []int64) string {
	zone := until.Location().String()
	if zone == "Local" {
		zone = until.Format("MST")
	}
	return fmt.Sprintf("%s%s %s (%s): Claude session limit", cappedParkedPrefix, until.Format("3:04pm"), zone, runIDList(runIDs))
}

// runIDList renders runIDs (already ascending) as "run 1625" for one, or
// "runs 1625, 1626" for more.
func runIDList(runIDs []int64) string {
	if len(runIDs) == 1 {
		return "run " + strconv.FormatInt(runIDs[0], 10)
	}
	parts := make([]string, len(runIDs))
	for i, id := range runIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "runs " + strings.Join(parts, ", ")
}

// RecordCappedResume writes "resumed after the Claude session limit (run
// runID)" for ticketID, only when the ticket's newest "parked until" update
// has a greater message id than its newest "resumed after the Claude
// session limit" update, or no such resumed update exists yet -- so N lens
// sessions resuming after one park write exactly one marker, and a later
// park can trigger another. written reports whether a row was inserted.
func (s *Store) RecordCappedResume(ctx context.Context, ticketID, runID int64) (written bool, err error) {
	body := fmt.Sprintf("%s (run %d)", cappedResumedPrefix, runID)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, type, author, body)
		 SELECT ?, ?, ?, ?
		 WHERE EXISTS (
			SELECT 1 FROM messages parked
			WHERE parked.ticket_id = ? AND parked.type = ? AND parked.author = ? AND parked.body LIKE ?
			  AND parked.id > COALESCE((
				SELECT MAX(resumed.id) FROM messages resumed
				WHERE resumed.ticket_id = ? AND resumed.type = ? AND resumed.author = ? AND resumed.body LIKE ?
			  ), 0)
		 )`,
		ticketID, msgTypeUpdate, authorSystem, body,
		ticketID, msgTypeUpdate, authorSystem, cappedParkedPrefix+"%",
		ticketID, msgTypeUpdate, authorSystem, cappedResumedPrefix+"%",
	)
	if err != nil {
		return false, fmt.Errorf("record capped resume: ticket %d: %w", ticketID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record capped resume: ticket %d: %w", ticketID, err)
	}
	return n > 0, nil
}
