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
	Kind   *string `json:"kind"`
	Text   *string `json:"text"`
	Test   *string `json:"test"`
	Demo   *bool   `json:"demo"`
	Body   *string `json:"body"`

	// AnswerQuestion answers an amended escalation's option b ("Edit it",
	// #57) in the same transaction as this scenario edit.
	AnswerQuestion *int64 `json:"answer_question"`
}

// request builds b's store.OwnerEditRequest for ticketID.
func (b ownerEditBody) request(ticketID int64) store.OwnerEditRequest {
	return store.OwnerEditRequest{
		TicketID: ticketID, Target: b.Target, Ref: b.Ref, Action: b.Action,
		Given: b.Given, When: b.When, Then: b.Then, Check: b.Check, Kind: b.Kind,
		Text: b.Text, Test: b.Test, Demo: b.Demo, Body: b.Body,
		AnswerQuestion: b.AnswerQuestion,
	}
}

// ownerEditStatus maps a *store.OwnerEditError's Code to the HTTP status
// handleOwnerEdit answers with, keyed by store's own exported constants so a
// renamed or added code fails to compile here instead of silently falling
// back to status 0.
var ownerEditStatus = map[store.OwnerEditCode]int{
	store.OwnerEditCodeBadRequest:    http.StatusBadRequest,
	store.OwnerEditCodeNotFound:      http.StatusNotFound,
	store.OwnerEditCodeNotSealed:     http.StatusConflict,
	store.OwnerEditCodeLanded:        http.StatusConflict,
	store.OwnerEditCodeClaimed:       http.StatusConflict,
	store.OwnerEditCodeInvalid:       http.StatusUnprocessableEntity,
	store.OwnerEditCodeAnswerRefused: http.StatusConflict,
}

// ownerEditSandboxCmdOnlyReason is handleOwnerEdit's refusal when a remote,
// non-loopback caller sets check, test, or kind: check and test both become
// shell commands CHECK and the build later run in the ticket's sandbox, and
// kind host makes a scenario's check run on this machine outside any sandbox
// at all (#57, c1 point 4) -- the same owner-decided local-only boundary
// sandboxrun.go's requireLoopback draws around POST /tickets/{id}/sandbox-run.
// Every other field (given, when, then, text, demo, body) carries no such
// risk and stays open to any same-origin caller.
const ownerEditSandboxCmdOnlyReason = "editing a check or test command is allowed from this machine only"

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

	// kind host makes a check run outside any sandbox, so kind joins check
	// and test under the same local-only boundary.
	if (body.Check != nil || body.Test != nil || body.Kind != nil) && !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, ownerEditSandboxCmdOnlyReason, http.StatusForbidden)
		return
	}

	err = c.store.OwnerEdit(r.Context(), body.request(id))
	var refusal *store.OwnerEditError
	switch {
	case errors.As(err, &refusal):
		slog.Info("console: owner edit refused", "ticket_id", id, "target", body.Target, "ref", body.Ref, "action", body.Action, "code", refusal.Code)
		http.Error(w, refusal.Reason, ownerEditStatus[refusal.Code])
		return
	case err != nil:
		slog.Error("console: owner edit", "ticket_id", id, "target", body.Target, "ref", body.Ref, "action", body.Action, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}
