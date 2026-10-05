// sandboxrun.go: POST /tickets/{id}/sandbox-run, the owner's "run a command
// in this ticket's sandbox" console action (split from #73). It runs one
// shell command in a ticket's existing worktree exactly as CHECK runs its
// lint and test commands, through job.TicketCommands (internal/job's own
// Commands value, shared with the dispatcher so the two can never drift).
// Only a loopback caller (requireLoopback) that also passes the console's
// usual same-origin guard may call it; results are never stored.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"zing/internal/job"
	"zing/internal/orchestrator"
)

// TicketRunner runs one shell command in a ticket's worktree exactly as
// CHECK runs it. job.TicketCommands is the real implementation; serve builds
// it with the same CommandRunner value the dispatcher's CHECK step uses.
type TicketRunner interface {
	Run(ctx context.Context, ticketID int64, shellCmd string) (job.TicketCommandResult, error)
}

// maxSandboxCmdBytes bounds the command text POST /tickets/{id}/sandbox-run
// accepts; the command runs exactly as sent, untrimmed, when it is within
// this bound.
const maxSandboxCmdBytes = 4096

// sandboxRunRequest is POST /tickets/{id}/sandbox-run's body.
type sandboxRunRequest struct {
	Cmd string `json:"cmd"`
}

// sandboxRunResponse is TicketCommandResult reshaped for the wire.
type sandboxRunResponse struct {
	Exit     int    `json:"exit"`
	TimedOut bool   `json:"timed_out"`
	Output   string `json:"output"`
	Total    int64  `json:"total"`
	Cut      bool   `json:"cut"`
}

// handleSandboxRun is POST /tickets/{id}/sandbox-run. It runs behind
// requireLoopback and the mutation guard's requireSameOrigin (New), so by
// the time this handler runs, the caller is already known to be loopback
// and same-origin; this handler's own checks follow from there, in the
// order the design lays out: a bad {id}, a bad body, an empty or
// too-long command, no runner configured, then the runner's own lookup and
// sandbox failures.
func (c *console) handleSandboxRun(w http.ResponseWriter, r *http.Request) {
	ticketID, ok := parsePositiveID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)
	var req sandboxRunRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		http.Error(w, "command is empty", http.StatusBadRequest)
		return
	}
	if len(req.Cmd) > maxSandboxCmdBytes {
		http.Error(w, fmt.Sprintf("command is longer than %d bytes", maxSandboxCmdBytes), http.StatusBadRequest)
		return
	}
	if c.run == nil {
		http.Error(w, "sandbox runs are not available", http.StatusServiceUnavailable)
		return
	}

	res, err := c.run.Run(r.Context(), ticketID, req.Cmd)
	switch {
	case errors.Is(err, job.ErrNoTicket):
		http.Error(w, "ticket not found", http.StatusNotFound)
		return
	case errors.Is(err, job.ErrNoProject):
		http.Error(w, fmt.Sprintf("ticket %d's project is not configured", ticketID), http.StatusConflict)
		return
	case errors.Is(err, orchestrator.ErrNoWorktree):
		http.Error(w, fmt.Sprintf("ticket %d has no worktree", ticketID), http.StatusConflict)
		return
	case errors.Is(err, job.ErrSandbox):
		http.Error(w, "build sandbox unavailable", http.StatusServiceUnavailable)
		return
	case err != nil && r.Context().Err() != nil:
		slog.Info("console: sandbox run canceled", "ticket_id", ticketID)
		return
	case err != nil:
		slog.Error("console: sandbox run", "ticket_id", ticketID, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeJSON)
	if err := json.NewEncoder(w).Encode(sandboxRunResponse{
		Exit: res.Exit, TimedOut: res.TimedOut, Output: res.Output, Total: res.Total, Cut: res.Cut,
	}); err != nil {
		slog.Error("console: write sandbox run response", "ticket_id", ticketID, "err", err)
	}
}

// requireLoopback refuses any request not from 127.0.0.1 or ::1 (owner
// decision: sandbox runs are local-owner-only), ahead of the same-origin
// guard.
func requireLoopback(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			http.Error(w, "sandbox runs are allowed from this machine only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// isLoopbackRemote reports whether remoteAddr ("host:port") is 127.0.0.1 or
// ::1, IPv4-mapped forms included. Any other address, including other
// 127.x addresses, is refused.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}
