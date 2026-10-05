// owner_edit.go: POST /tickets/{id}/edit (#41), the one console route
// behind every owner edit to a sealed scenario, a sealed plan's task, or
// the ticket body: store.Store.OwnerEdit does the work, this file only
// decodes the request and maps a refusal to its HTTP status.
package console

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"zing/internal/store"
)

// maxOwnerEditBodyBytes bounds POST /tickets/{id}/edit's request body: an
// edit carries at most a few short fields, and ticket_body's own longest
// field is a ticket body, well under this (design section 6.7's 64 KiB cap
// for the composer is tighter still, but a plan's full payload can be
// larger than a draft ever is).
const maxOwnerEditBodyBytes = 256 << 10 // 256 KiB

// ownerEditBody is POST /tickets/{id}/edit's body: target and action are
// always present; every other field is meaningful only for the target
// store.OwnerEdit's own shape check allows it on (store.OwnerEditRequest).
type ownerEditBody struct {
	Target string  `json:"target"`
	Ref    string  `json:"ref"`
	Action string  `json:"action"`
	Given  *string `json:"given"`
	When   *string `json:"when"`
	Then   *string `json:"then"`
	Check  *string `json:"check"`
	Text   *string `json:"text"`
	Test   *string `json:"test"`
	Demo   *bool   `json:"demo"`
	Body   *string `json:"body"`
}

// request builds b's store.OwnerEditRequest for ticketID.
func (b ownerEditBody) request(ticketID int64) store.OwnerEditRequest {
	return store.OwnerEditRequest{
		TicketID: ticketID, Target: b.Target, Ref: b.Ref, Action: b.Action,
		Given: b.Given, When: b.When, Then: b.Then, Check: b.Check,
		Text: b.Text, Test: b.Test, Demo: b.Demo, Body: b.Body,
	}
}

// ownerEditStatus maps a *store.OwnerEditError's Code to the HTTP status
// handleOwnerEdit answers with.
var ownerEditStatus = map[string]int{
	"bad_request": http.StatusBadRequest,
	"not_found":   http.StatusNotFound,
	"not_sealed":  http.StatusConflict,
	"landed":      http.StatusConflict,
	"claimed":     http.StatusConflict,
	"invalid":     http.StatusUnprocessableEntity,
}

// handleOwnerEdit is POST /tickets/{id}/edit: the owner edits a sealed
// scenario, a sealed plan's task, or the ticket body (store.OwnerEdit).
// 204 on success; a refusal answers its status with the reason as body.
func (c *console) handleOwnerEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOwnerEditBodyBytes)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var body ownerEditBody
	if err = decodeStrict(r, &body); err != nil {
		writeDecodeError(w, err)
		return
	}

	err = c.store.OwnerEdit(r.Context(), body.request(id))
	var refusal *store.OwnerEditError
	switch {
	case errors.As(err, &refusal):
		http.Error(w, refusal.Reason, ownerEditStatus[refusal.Code])
		return
	case err != nil:
		slog.Error("console: owner edit", "ticket_id", id, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}
