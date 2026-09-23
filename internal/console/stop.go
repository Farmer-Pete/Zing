// stop.go: POST /stop (design section 6.11, the s/S keyboard keys), sat
// behind the same mutation guard as every other state-changing route
// (mw.go). {"all": true} sets the store's persisted "stopped" flag, the
// same flag the dispatcher's own Tick loop already honors before claiming
// further work (internal/store/spine.go's SetStopped). {"ticket": N} has no
// schema column to write yet: Package 4 tracks no per-ticket stop state, so
// this handler is honest about that rather than inventing one -- it logs
// the request and answers 204, leaving the actual per-ticket cancellation
// to Package 5's orchestrator.
package console

import (
	"log/slog"
	"net/http"
)

// stopRequest is POST /stop's body: console.js's stopEverything posts
// {all: true} and stopTicket posts {ticket: N} (design section 6.11).
// Exactly one is meaningful; a body naming neither is rejected with 400.
type stopRequest struct {
	All    bool  `json:"all"`
	Ticket int64 `json:"ticket"`
}

// handleStop is POST /stop (design section 6.11, 7.1):
//
//   - {"all": true} sets the "stopped" setting flag (store.SetStopped) and
//     publishes, 204.
//   - {"ticket": N} (N > 0) logs the per-ticket stop request at info with
//     the ticket id and answers 204. Package 4's schema has no per-ticket
//     stop column -- that arrives with Package 5's orchestrator -- so this
//     is deliberately a log line, not a store write; do not add a column
//     here to make it do more than that.
//   - A body naming neither (all absent/false and ticket absent/non-positive)
//     is 400.
func (c *console) handleStop(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)

	var req stopRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}

	switch {
	case req.All:
		if err := c.store.SetStopped(r.Context(), true); err != nil {
			slog.Error("console: set stopped flag", "err", err)
			http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
			return
		}
		c.bus.Publish()
	case req.Ticket > 0:
		// Per-ticket stop lands with Package 5's orchestrator; Package 4 has
		// no schema column to hold it. Log so the request is visible, and
		// answer 204 rather than silently dropping it.
		slog.Info("console: per-ticket stop requested (not yet enforced; lands with Package 5)", "ticket_id", req.Ticket)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
