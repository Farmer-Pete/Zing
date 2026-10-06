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
	"errors"
	"log/slog"
	"net/http"

	"zing/internal/store"
)

// reasonConsoleAbandon is the state message AbandonTicket writes when the
// owner abandons a ticket directly (as opposed to a restart's own reason).
const reasonConsoleAbandon = "owner abandoned from the console"

// actionRefusal is a console action's own refusal: Status is the HTTP
// status to answer with, Reason is the exact response body. Unlike
// *store.AbandonError, this is raised by the console package itself (for
// example, fetchIssue's tracker refusals in a later task).
type actionRefusal struct {
	Status int
	Reason string
}

func (e *actionRefusal) Error() string { return e.Reason }

// abandonErrorStatus maps a *store.AbandonError's Code to the HTTP status
// writeActionError answers with, keyed by store's own exported constants so
// a renamed or added code fails to compile here instead of silently falling
// back to status 0.
var abandonErrorStatus = map[store.AbandonCode]int{
	store.AbandonCodeNotFound: http.StatusNotFound,
	store.AbandonCodeTerminal: http.StatusConflict,
	store.AbandonCodeClaimed:  http.StatusConflict,
}

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
		http.Error(w, abandonErr.Reason, abandonErrorStatus[abandonErr.Code])
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
