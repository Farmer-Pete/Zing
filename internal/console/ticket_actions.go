// ticket_actions.go: POST /tickets/{id}/abandon and POST /tickets/{id}/
// restart (#65), the owner's Abandon and Restart from planning actions.
// Each route is one function: handleAbandon calls abandonTicket,
// store.AbandonTicket does the write. writeActionError is the one place
// that maps a refusal -- a *store.AbandonError or an *actionRefusal -- to
// its HTTP status, so the two routes answer the same way a refusal of the
// same shape always does.
package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"zing/internal/store"
)

// reasonConsoleAbandon is the state message AbandonTicket writes when the
// owner abandons a ticket directly (as opposed to a restart's own reason).
const reasonConsoleAbandon = "owner abandoned from the console"

// reasonConsoleRestart is the state message AbandonTicket writes on the old
// ticket when the owner restarts it from planning.
const reasonConsoleRestart = "owner restarted from planning"

// actionRefusal is a console action's own refusal: Status is the HTTP
// status to answer with, Reason is the exact response body. Unlike
// *store.AbandonError, this is raised by the console package itself (for
// example, fetchIssue's tracker refusals in a later task).
type actionRefusal struct {
	Status int
	Reason string
}

func (e *actionRefusal) Error() string { return e.Reason }

// writeActionError answers w for err, raised by the named action on
// ticketID: a *store.AbandonError or an *actionRefusal answers its own
// status with Reason as the body, logged at INFO as a refusal; any other
// error is logged at ERROR and answered 500 with genericServerErrorBody.
func (c *console) writeActionError(w http.ResponseWriter, action string, ticketID int64, err error) {
	var abandonErr *store.AbandonError
	var refusal *actionRefusal
	switch {
	case errors.As(err, &abandonErr):
		slog.Info("console: ticket action refused", "action", action, "ticket_id", ticketID, "reason", abandonErr.Reason)
		status := http.StatusConflict
		if abandonErr.Code == store.AbandonCodeNotFound {
			status = http.StatusNotFound
		}
		http.Error(w, abandonErr.Reason, status)
	case errors.As(err, &refusal):
		slog.Info("console: ticket action refused", "action", action, "ticket_id", ticketID, "reason", refusal.Reason)
		http.Error(w, refusal.Reason, refusal.Status)
	default:
		slog.Error("console: ticket action", "action", action, "ticket_id", ticketID, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
	}
}

// handleAbandon is POST /tickets/{id}/abandon: the owner's Abandon action.
// 204 and a bus publish on success; a refusal answers its status with the
// reason as the body (writeActionError).
func (c *console) handleAbandon(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePositiveID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := c.abandonTicket(r.Context(), id); err != nil {
		c.writeActionError(w, "abandon", id, err)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}

// abandonTicket is the owner's Abandon action: the one function its route
// (handleAbandon) calls, per the owner rule that every owner action lives
// in the console behind one such function.
func (c *console) abandonTicket(ctx context.Context, id int64) error {
	return c.store.AbandonTicket(ctx, id, reasonConsoleAbandon)
}

// handleRestart is POST /tickets/{id}/restart: the owner's Restart from
// planning action. 200 with {"ticket":NEW-ID} and a bus publish on
// success; a refusal answers its status with the reason as the body
// (writeActionError).
func (c *console) handleRestart(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePositiveID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	newID, err := c.restartTicket(r.Context(), id)
	if err != nil {
		c.writeActionError(w, "restart", id, err)
		return
	}

	c.bus.Publish()
	w.Header().Set("Content-Type", contentTypeJSON)
	if err := json.NewEncoder(w).Encode(restartResponseBody{Ticket: newID}); err != nil {
		slog.Error("console: write restart response", "old_ticket_id", id, "new_ticket_id", newID, "err", err)
	}
}

// restartResponseBody is POST /tickets/{id}/restart's 200 body.
type restartResponseBody struct {
	Ticket int64 `json:"ticket"`
}

// canRestart reports whether a ticket in state may be restarted (#65):
// store.CanAbandon accepts it outright, or it is abandoned with no live
// successor already at its ref. actionsFor (views.go) and restartTicket
// both call this instead of each keeping its own list, so the button
// offered and the route accepted can never drift (r1f7).
func canRestart(state string, liveSuccessor bool) bool {
	return store.CanAbandon(state) || (state == ticketStateAbandoned && !liveSuccessor)
}

// liveSuccessorTicket returns the ticket currently holding base in
// projectID, when it is not id itself and not abandoned -- the live
// successor console.ticketActions (views.go) and restartTicket both need
// to decide whether a restart is offered, or already taken (r1f7). found
// is false when no such ticket exists.
func (c *console) liveSuccessorTicket(ctx context.Context, projectID, id int64, base string) (succ store.Ticket, found bool, err error) {
	cur, exists, err := c.store.TicketByRef(ctx, projectID, base)
	if err != nil {
		return store.Ticket{}, false, fmt.Errorf("console: live successor for ref %s: %w", base, err)
	}
	isLiveSuccessor := exists && cur.ID != id && cur.State != ticketStateAbandoned
	if !isLiveSuccessor {
		return store.Ticket{}, false, nil
	}
	return cur, true, nil
}

// restartTicket is the owner's Restart from planning action: the one
// function its route (handleRestart) calls. It reuses the pickup path
// (fetchIssue, insertFresh): it re-reads the issue, refuses a closed,
// missing, or pull-request ref with the old ticket untouched, abandons the
// old ticket (unless it is already abandoned), and inserts a fresh queued
// ticket for the same ref.
func (c *console) restartTicket(ctx context.Context, id int64) (int64, error) {
	t, err := c.store.GetTicket(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &actionRefusal{Status: http.StatusNotFound, Reason: fmt.Sprintf("no ticket %d", id)}
	}
	if err != nil {
		return 0, err
	}

	base, _ := store.SplitAttemptRef(t.TrackerRef)

	if t.ClaimOwner != nil {
		return 0, &actionRefusal{Status: http.StatusConflict, Reason: store.AbandonClaimedReason}
	}

	var succ store.Ticket
	var succFound bool
	if t.State == ticketStateAbandoned {
		succ, succFound, err = c.liveSuccessorTicket(ctx, t.ProjectID, id, base)
		if err != nil {
			return 0, err
		}
	}
	if !canRestart(t.State, succFound) {
		if succFound {
			return 0, &actionRefusal{Status: http.StatusConflict, Reason: fmt.Sprintf("issue #%s is already ticket %d", base, succ.ID)}
		}
		return 0, &actionRefusal{Status: http.StatusConflict, Reason: fmt.Sprintf("ticket %d is %s; only a live or abandoned ticket restarts", id, t.State)}
	}

	trackerProject, found, err := c.projectName(ctx, t.ProjectID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("console: restart ticket %d: project %d not configured", id, t.ProjectID)
	}

	tk, err := c.fetchIssue(ctx, trackerProject, base)
	if err != nil {
		return 0, err
	}

	if t.State != ticketStateAbandoned {
		if abandonErr := c.store.AbandonTicket(ctx, id, reasonConsoleRestart); abandonErr != nil {
			return 0, abandonErr
		}
	}

	newID, retiredRef, err := c.insertFresh(ctx, t.ProjectID, trackerProject, tk)
	if err != nil {
		slog.WarnContext(ctx, "console: restart abandoned old ticket but insert failed",
			"old_ticket_id", id, "project_id", t.ProjectID, "ref", base, "retired_ref", retiredRef, "err", err)
		return 0, fmt.Errorf("console: restart ticket %d: insert fresh ticket for ref %s (old ticket retired to %q): %w", id, base, retiredRef, err)
	}

	slog.InfoContext(ctx, "console: ticket restarted",
		"old_ticket_id", id, "new_ticket_id", newID, "project_id", t.ProjectID, "ref", base, "retired_ref", retiredRef)
	return newID, nil
}
