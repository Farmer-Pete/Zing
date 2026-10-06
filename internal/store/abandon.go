// abandon.go: Store.AbandonTicket (#65), the one store method behind the
// owner's Abandon action. It moves any unclaimed, non-terminal ticket to
// abandoned in one transaction: it clears waiting_on and the three poll
// columns, resolves every open or answered question, and writes one state
// message carrying the reason. It leaves every earlier message in place.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"zing/internal/response"
)

// abandonableStates is the one list of states the owner can abandon from.
// CanAbandon and AbandonTicket both read it, so they cannot drift.
var abandonableStates = []string{"queued", "planning", "building", "reviewing", "judging", "shipping"}

// CanAbandon reports whether a ticket in state may be abandoned. The store's
// own write and the console's Abandon button both ask this, so they cannot
// drift apart.
func CanAbandon(state string) bool {
	return slices.Contains(abandonableStates, state)
}

// AbandonCode is AbandonError's Code: one of the named constants below.
type AbandonCode string

// AbandonError's Code values.
const (
	AbandonCodeNotFound AbandonCode = "not_found"
	AbandonCodeTerminal AbandonCode = "terminal"
	AbandonCodeClaimed  AbandonCode = "claimed"
)

// AbandonError is AbandonTicket's one refusal shape: Code picks the HTTP
// status a caller maps it to, Reason is the one owner-facing sentence. A
// refusal changes nothing.
type AbandonError struct {
	Code   AbandonCode
	Reason string
}

func (e *AbandonError) Error() string { return e.Reason }

// AbandonClaimedReason is the one sentence a claimed ticket is refused with,
// shared by the console's disabled-button note.
const AbandonClaimedReason = "A run holds this ticket; try again when it finishes."

// AbandonTicket moves ticketID to abandoned in one transaction: it clears
// waiting_on and the three poll columns, resolves every open or answered
// question, and writes one state message carrying reason. It refuses, and
// changes nothing, when ticketID does not exist (AbandonCodeNotFound), is
// already in a terminal state CanAbandon rejects (AbandonCodeTerminal), or
// is currently claimed (AbandonCodeClaimed).
func (s *Store) AbandonTicket(ctx context.Context, ticketID int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("abandon ticket: empty reason")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abandon ticket %d: begin tx: %w", ticketID, err)
	}
	defer rollback(tx)

	var state string
	var claimOwner sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state, claim_owner FROM tickets WHERE id = ?`, ticketID).Scan(&state, &claimOwner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &AbandonError{Code: AbandonCodeNotFound, Reason: fmt.Sprintf("no ticket %d", ticketID)}
	case err != nil:
		return fmt.Errorf("abandon ticket %d: load: %w", ticketID, err)
	}
	if !CanAbandon(state) {
		return &AbandonError{Code: AbandonCodeTerminal, Reason: fmt.Sprintf("ticket %d is already %s", ticketID, state)}
	}
	if claimOwner.Valid {
		return &AbandonError{Code: AbandonCodeClaimed, Reason: AbandonClaimedReason}
	}

	if _, err = tx.ExecContext(ctx,
		`UPDATE tickets SET state = 'abandoned', waiting_on = NULL, next_poll_at = NULL,
		 poll_interval_s = NULL, poll_fingerprint = NULL
		 WHERE id = ? AND state = ? AND claim_owner IS NULL`, ticketID, state); err != nil {
		return fmt.Errorf("abandon ticket %d: update: %w", ticketID, err)
	}

	if _, err = tx.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE ticket_id = ? AND type = ? AND state IN (?, ?)`,
		questionStateResolved, ticketID, msgTypeQuestion, questionStateOpen, questionStateAnswered); err != nil {
		return fmt.Errorf("abandon ticket %d: resolve questions: %w", ticketID, err)
	}

	payload, err := json.Marshal(response.StatePayload{From: response.TicketState(state), To: response.TicketStateAbandoned, Reason: reason})
	if err != nil {
		return fmt.Errorf("abandon ticket %d: marshal state: %w", ticketID, err)
	}
	if err = s.insertMessageTx(ctx, tx, Message{TicketID: ticketID, Type: msgTypeState, Author: authorSystem, Payload: payload}); err != nil {
		return fmt.Errorf("abandon ticket %d: insert state message: %w", ticketID, err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("abandon ticket %d: commit: %w", ticketID, err)
	}

	slog.InfoContext(ctx, "ticket abandoned", "ticket_id", ticketID, "from", state, "reason", reason)
	return nil
}
