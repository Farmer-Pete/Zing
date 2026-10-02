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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"zing/internal/dispatch"
	"zing/internal/tracker"
)

// pickupRequest is POST /projects/{id}/pickup's body: {"n": <issue number>}.
type pickupRequest struct {
	N int `json:"n"`
}

// handlePickup is POST /projects/{id}/pickup (design section 6.14-style
// guard, PKG9-PLAN.md D29): 400 on a malformed {id}, a malformed body, or a
// non-positive n; 404 when {id} names no configured project; 409 with D29's
// exact message on one of the four refusals; 204 and a bus publish on
// success.
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
	if alreadyTicketed {
		http.Error(w, fmt.Sprintf("issue #%d is already ticket %d", req.N, existingTicket.ID), http.StatusConflict)
		return
	}

	tk, err := c.tracker.Issue(r.Context(), trackerProject, ref)
	switch {
	case errors.Is(err, tracker.ErrIssueNotFound):
		http.Error(w, fmt.Sprintf("issue #%d not found", req.N), http.StatusConflict)
		return
	case errors.Is(err, tracker.ErrIssueClosed):
		http.Error(w, fmt.Sprintf("issue #%d is closed", req.N), http.StatusConflict)
		return
	case errors.Is(err, tracker.ErrIssueIsPullRequest):
		http.Error(w, fmt.Sprintf("#%d is a pull request, not an issue", req.N), http.StatusConflict)
		return
	case err != nil:
		slog.Error("console: pickup: issue", "project_id", projectID, "ref", ref, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	if _, err := dispatch.InsertAndAnnounce(r.Context(), c.store, c.tracker, projectID, trackerProject, c.user, tk); err != nil {
		slog.Error("console: pickup: insert ticket", "project_id", projectID, "ref", ref, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
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
