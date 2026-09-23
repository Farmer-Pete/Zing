// answer.go: POST /draft, POST /send, POST /read (design section 6.7, 6.8),
// the batched composer's HTTP surface. Each wraps a store.console_writes.go
// method and maps a *store.ConflictError to 409. POST /send and POST /read
// publish on success, waking the live /stream; POST /draft does not (review
// fix, package 4 re-review): a draft renders nothing server-side -- Thread,
// Feed, Inbox, and Nav all filter draft rows out, and no server region shows
// a picked/answered chip state -- so publishing on a draft only woke the
// stream to re-render identical HTML, and the resulting morph stripped the
// client-only ".picked" highlight and collapsed the open question group
// after every pick. This supersedes Package 3's POST /answer (server.go,
// handlers.go), which answered one option at a time with no draft stage;
// console.js's postDraft, sendBatch, and markRead (Task 4) were already
// written against these three routes' JSON contract, ahead of this task
// building them.
package console

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"zing/internal/response"
	"zing/internal/store"
)

// maxDraftBodyBytes bounds the request body of every JSON-decoding POST
// route in this package: /draft, /send, /read (this file), /stop (stop.go),
// and /loglevel, /debug (control.go) (design section 6.7: "The body cap is
// 64 KiB"). Wrapping r.Body in http.MaxBytesReader before decoding keeps an
// oversized body from being buffered in full before any validation runs,
// and lets the handler tell "too large" (413) apart from "malformed" (400).
const maxDraftBodyBytes = 64 << 10 // 64 KiB

// maxDraftTextLen is the longest a free-text draft body may be (design
// section 6.7: "Text longer than 8000 characters returns 400"), counted in
// runes so a multi-byte character counts once.
const maxDraftTextLen = 8000

// draftItemRequest is the wire shape of a DraftInput.Item.
type draftItemRequest struct {
	Ref      string            `json:"ref"`
	Decision response.Decision `json:"decision"`
}

// draftRequest is POST /draft's body: console.js's postDraft already sends
// {ticket, question, text} for a free reply (design section 6.4); option
// and item extend that same shape for a chip or item-decision draft.
// Exactly one of Option, Item, or Text is meaningful, matching
// store.DraftInput (design section 6.7).
type draftRequest struct {
	Ticket   int64             `json:"ticket"`
	Question *int64            `json:"question"`
	Option   *string           `json:"option"`
	Item     *draftItemRequest `json:"item"`
	Text     string            `json:"text"`
}

// handleDraft is POST /draft (design section 6.7, 7.1): decode the body
// strictly and bounded, translate it into a store.DraftInput, call
// SaveDraft, and report the outcome. 413 over the body cap, 400 on
// malformed or unknown-field JSON or an over-length Text, 409 on a typed
// *store.ConflictError (SaveDraft's semantic checks), 204 on success. It
// deliberately does not call c.bus.Publish (review fix, package 4
// re-review): a draft changes nothing any server-rendered region shows, so
// waking /stream would only cost every open tab an identical re-render --
// one whose morph strips the client-only ".picked" highlight and collapses
// the open question group, degrading multi-item picking. POST /send below
// still publishes once the batch is actually sent.
func (c *console) handleDraft(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)

	var req draftRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Ticket <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len([]rune(req.Text)) > maxDraftTextLen {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	in := store.DraftInput{TicketID: req.Ticket, QuestionID: req.Question, Option: req.Option, Text: req.Text}
	if req.Item != nil {
		in.Item = &store.ItemDecision{Ref: req.Item.Ref, Decision: req.Item.Decision}
	}

	_, err := c.store.SaveDraft(r.Context(), in)
	if err != nil {
		if conflictErr, ok := errors.AsType[*store.ConflictError](err); ok {
			http.Error(w, conflictErr.Reason, http.StatusConflict)
			return
		}
		slog.Error("console: save draft", "ticket_id", req.Ticket, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// sendRequest is POST /send's body (design section 6.4: sendBatch posts
// {ticket}).
type sendRequest struct {
	Ticket int64 `json:"ticket"`
}

// handleSend is POST /send (design section 6.7, 7.1): send the ticket's
// drafted batch. 400 on a malformed body or non-positive ticket, 409 when
// SendBatch reports Empty (nothing drafted) or a typed conflict, 204 and a
// bus publish on success.
func (c *console) handleSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)

	var req sendRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Ticket <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	result, err := c.store.SendBatch(r.Context(), req.Ticket)
	if err != nil {
		if conflictErr, ok := errors.AsType[*store.ConflictError](err); ok {
			http.Error(w, conflictErr.Reason, http.StatusConflict)
			return
		}
		slog.Error("console: send batch", "ticket_id", req.Ticket, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	if result.Empty {
		http.Error(w, "nothing to send", http.StatusConflict)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}

// readRequest is POST /read's body (design section 6.4: markRead posts
// {message}).
type readRequest struct {
	Message int64 `json:"message"`
}

// handleRead is POST /read (design section 6.8, 7.1): mark one message
// read. 400 on a malformed body or non-positive message id, 500 with a
// generic body on a store error (for example a message id that names no
// row), 204 and a bus publish on success.
func (c *console) handleRead(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBodyBytes)

	var req readRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Message <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := c.store.MarkRead(r.Context(), req.Message); err != nil {
		slog.Error("console: mark read", "message_id", req.Message, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}

// decodeStrict decodes r's JSON body into dst, rejecting unknown fields and
// trailing data (design section 6.7: "decodes strictly"). The caller must
// already have wrapped r.Body in http.MaxBytesReader.
func decodeStrict(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errTrailingData
	}
	return nil
}

// errTrailingData is decodeStrict's own error for a body that decodes fine
// but keeps going, the "no trailing data" half of "decodes strictly".
var errTrailingData = errors.New("trailing data after JSON body")

// writeDecodeError maps a decodeStrict failure to the design section 6.7
// status split: 413 when http.MaxBytesReader tripped the body cap, 400 for
// every other decode failure (malformed or unknown-field JSON).
func writeDecodeError(w http.ResponseWriter, err error) {
	if maxBytesErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		_ = maxBytesErr // matched only to detect the body-cap case; its fields add nothing to the fixed message below
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "bad request", http.StatusBadRequest)
}
