// stop.go: POST /stop (design section 6.11, the s/S keyboard keys), sat
// behind the same mutation guard as every other state-changing route
// (mw.go). {"all": true} sets the store's persisted "stopped" flag, the
// same flag the dispatcher's own Tick loop already honors before claiming
// further work (internal/store/spine.go's SetStopped). {"ticket": N} has no
// schema column to write yet: Package 4 tracks no per-ticket stop state, so
// this handler answers 501 rather than claiming a stop it cannot perform,
// leaving the actual per-ticket cancellation to Package 5's orchestrator.
package console

import (
	"log/slog"
	"net/http"
)

// stopRequest is POST /stop's body: console.js's stopEverything posts
// {all: true} and stopTicket posts {ticket: N} (design section 6.11).
// Exactly one of All and a positive Ticket is meaningful; a body naming
// both, or naming neither, is rejected with 400.
type stopRequest struct {
	All    bool  `json:"all"`
	Ticket int64 `json:"ticket"`
}

// perTicketStopUnimplementedBody is POST /stop {"ticket": N}'s 501 body
// (review fix, package 4 re-review): Package 4's schema has no per-ticket
// stop column, so returning 204 for that case reported success for a no-op.
// Answering 501 instead of inventing a column here is honest about the gap
// until Package 5's orchestrator lands.
const perTicketStopUnimplementedBody = "per-ticket stop arrives with Package 5 (the orchestrator)"

// handleStop is POST /stop (design section 6.11, 7.1):
//
//   - A body naming both all=true and a positive ticket is ambiguous and is
//     rejected with 400 before either branch below runs.
//   - {"all": true} sets the "stopped" setting flag (store.SetStopped) and
//     publishes, 204.
//   - {"ticket": N} (N > 0), all absent or false: 501, since Package 4 has
//     no schema column for a per-ticket stop and cannot honestly perform one
//     yet -- that arrives with Package 5's orchestrator.
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
	case req.All && req.Ticket > 0:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	case req.All:
		if err := c.store.SetStopped(r.Context(), true); err != nil {
			slog.Error("console: set stopped flag", "err", err)
			http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
			return
		}
		c.bus.Publish()
	case req.Ticket > 0:
		slog.Info("console: per-ticket stop requested (not yet implemented; lands with Package 5)", "ticket_id", req.Ticket)
		http.Error(w, perTicketStopUnimplementedBody, http.StatusNotImplemented)
		return
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
