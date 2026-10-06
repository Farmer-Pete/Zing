// pickup.go: POST /projects/{id}/pickup (PKG9-PLAN.md D29), sat behind the
// same mutation guard as every other state-changing route (mw.go). Manual
// intake: the owner picks up one issue by number, for a project in either
// mode. The handler validates n, calls Tracker.Issue, refuses with D29's
// four exact messages (an issue that is closed, a pull request, missing, or
// already a ticket in this project), and otherwise inserts the ticket and
// posts the pickup comment through dispatch.InsertAndAnnounce, the same
// shared step intake itself uses (design section 6.8 step 3), so the two
// paths can never drift.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"zing/internal/dispatch"
	"zing/internal/tracker"
)

// ticketStateAbandoned is the store's own terminal "abandoned" state
// (store.AbandonTicket's own ticketStateAbandoned, unexported there): the
// one console package copy liveHolder, handlePickup, and later routes
// check tickets.state against.
const ticketStateAbandoned = "abandoned"

// pickupRequest is POST /projects/{id}/pickup's body: {"n": <issue number>}.
type pickupRequest struct {
	N int `json:"n"`
}

// pickupResponse is POST /projects/{id}/pickup's 200 body: the issue number
// the owner asked for and the id of the ticket it became, which
// console.js's pickupIssue links to.
type pickupResponse struct {
	N        int   `json:"n"`
	TicketID int64 `json:"ticket_id"`
}

// handlePickup is POST /projects/{id}/pickup (design section 6.14-style
// guard, PKG9-PLAN.md D29): 400 on a malformed {id}, a malformed body, or a
// non-positive n; 404 when {id} names no configured project; 409 with D29's
// exact message on one of the four refusals; 200 with a pickupResponse JSON
// body and a bus publish on success.
func (c *console) handlePickup(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parsePositiveID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)
	var req pickupRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.N <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	trackerProject, found, err := c.projectName(r.Context(), projectID)
	if err != nil {
		slog.Error("console: pickup: list projects", "project_id", projectID, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	ref := strconv.Itoa(req.N)

	// Checked before the tracker call, both because it is the more specific
	// fact when a ticket already exists (regardless of the issue's current
	// GitHub state) and because it costs no network round trip.
	existingTicket, alreadyTicketed, ticketErr := c.store.TicketByRef(r.Context(), projectID, ref)
	if ticketErr != nil {
		slog.Error("console: pickup: ticket by ref", "project_id", projectID, "ref", ref, "err", ticketErr)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	if alreadyTicketed && existingTicket.State != ticketStateAbandoned {
		http.Error(w, fmt.Sprintf("issue #%d is already ticket %d", req.N, existingTicket.ID), http.StatusConflict)
		return
	}

	tk, err := c.fetchIssue(r.Context(), trackerProject, ref)
	if refusal, ok := errors.AsType[*actionRefusal](err); ok {
		http.Error(w, refusal.Reason, refusal.Status)
		return
	}
	if err != nil {
		slog.Error("console: pickup: issue", "project_id", projectID, "ref", ref, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	newID, retiredRef, err := c.insertFresh(r.Context(), projectID, trackerProject, tk)
	if refusal, ok := errors.AsType[*actionRefusal](err); ok {
		http.Error(w, refusal.Reason, refusal.Status)
		return
	}
	if err != nil {
		slog.Error("console: pickup: insert ticket", "project_id", projectID, "ref", ref, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	if retiredRef != "" {
		slog.Info("console: pickup retired abandoned ticket",
			"old_ticket_id", existingTicket.ID, "new_ticket_id", newID, "new_ref", retiredRef, "project_id", projectID, "ref", ref)
	}

	c.bus.Publish()
	w.Header().Set("Content-Type", contentTypeJSON)
	if err := json.NewEncoder(w).Encode(pickupResponse{N: req.N, TicketID: newID}); err != nil {
		slog.Error("console: write pickup response", "project_id", projectID, "ticket_id", newID, "err", err)
	}
}

// fetchIssue reads ref from the tracker, or refuses with pickup's exact
// messages: missing, closed, or a pull request is a 409 *actionRefusal, and
// a nil tracker is a 503 one. Any other error is returned wrapped.
func (c *console) fetchIssue(ctx context.Context, trackerProject, ref string) (tracker.Ticket, error) {
	if c.tracker == nil {
		return tracker.Ticket{}, &actionRefusal{Status: http.StatusServiceUnavailable, Reason: "the tracker is not available"}
	}
	tk, err := c.tracker.Issue(ctx, trackerProject, ref)
	switch {
	case errors.Is(err, tracker.ErrIssueNotFound):
		return tracker.Ticket{}, &actionRefusal{Status: http.StatusConflict, Reason: fmt.Sprintf("issue #%s not found", ref)}
	case errors.Is(err, tracker.ErrIssueClosed):
		return tracker.Ticket{}, &actionRefusal{Status: http.StatusConflict, Reason: fmt.Sprintf("issue #%s is closed", ref)}
	case errors.Is(err, tracker.ErrIssueIsPullRequest):
		return tracker.Ticket{}, &actionRefusal{Status: http.StatusConflict, Reason: fmt.Sprintf("#%s is a pull request, not an issue", ref)}
	case err != nil:
		return tracker.Ticket{}, fmt.Errorf("console: issue %s: %w", ref, err)
	}
	return tk, nil
}

// insertFresh retires an abandoned ticket holding tk.Ref, then inserts the
// new queued ticket and posts the pickup comment exactly as intake does.
// retiredRef is the old ticket's new ref, or "" when nothing held tk.Ref.
// Retire and insert are two commits, so a live ticket can take tk.Ref in
// between (auto intake, or a second click). When retire reports ErrRefLive
// or the insert fails, liveHolder re-reads the ref; a live holder turns the
// failure into a 409 naming that ticket.
func (c *console) insertFresh(ctx context.Context, projectID int64, trackerProject string, tk tracker.Ticket) (newID int64, retiredRef string, err error) {
	retiredRef, err = c.store.RetireAbandonedRef(ctx, projectID, tk.Ref)
	if err != nil {
		return 0, "", c.liveHolder(ctx, projectID, tk.Ref, fmt.Errorf("console: retire ref %s: %w", tk.Ref, err))
	}
	newID, err = dispatch.InsertAndAnnounce(ctx, c.store, c.tracker, projectID, trackerProject, c.user, tk)
	if err != nil {
		return 0, retiredRef, c.liveHolder(ctx, projectID, tk.Ref, err)
	}
	return newID, retiredRef, nil
}

// liveHolder returns a 409 *actionRefusal when a non-abandoned ticket now
// holds ref, and cause otherwise (including when the re-read itself fails).
func (c *console) liveHolder(ctx context.Context, projectID int64, ref string, cause error) error {
	cur, found, err := c.store.TicketByRef(ctx, projectID, ref)
	if err != nil || !found || cur.State == ticketStateAbandoned {
		return cause
	}
	return &actionRefusal{Status: http.StatusConflict, Reason: fmt.Sprintf("issue #%s is already ticket %d", ref, cur.ID)}
}

// projectName returns the Name of the store project id -- the
// TrackerProject every Tracker call needs, the same value ensureBindings
// (cmd/zing/serve.go) already builds each dispatch.Binding.TrackerProject
// from (a project's own Name).
func (c *console) projectName(ctx context.Context, id int64) (name string, found bool, err error) {
	projects, err := c.store.ListProjects(ctx)
	if err != nil {
		return "", false, err
	}
	for _, p := range projects {
		if p.ID == id {
			return p.Name, true, nil
		}
	}
	return "", false, nil
}

// parsePositiveID parses s (an r.PathValue) as a positive int64.
func parsePositiveID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
