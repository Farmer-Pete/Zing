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
// console.js's postDraft and sendBatch (Task 4) were already written
// against these three routes' JSON contract, ahead of this task building
// them. No key or client caller posts /read today (ticket #44 removed the
// bare x binding that used to); the route stays for a future caller.
package console

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// store.DraftInput (design section 6.7). Text is a pointer so decodeStrict's
// strict decode can tell an omitted "text" key, which names no mode at all
// and is malformed when the request also names a question, apart from an
// explicit "text":"", which clears that question's reply draft (review fix).
// Base is the reply text the tab last saw saved (ticket #43): nil means the
// caller sent none and SaveDraft keeps overwriting unconditionally; a
// non-nil value that no longer matches what is stored becomes a 409
// "changed in another tab" instead of a silent overwrite. It shares Text's
// 8000-rune cap, since it is itself a previous save's text.
type draftRequest struct {
	Ticket   int64             `json:"ticket"`
	Question *int64            `json:"question"`
	Option   *string           `json:"option"`
	Item     *draftItemRequest `json:"item"`
	Text     *string           `json:"text"`
	Base     *string           `json:"base"`
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
	var text string
	if req.Text != nil {
		text = *req.Text
	}
	if len([]rune(text)) > maxDraftTextLen {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Base != nil && len([]rune(*req.Base)) > maxDraftTextLen {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A question draft naming none of option, item, or text (text omitted,
	// not an explicit "") names no mode at all: SaveDraft's draftModeCount
	// would otherwise read it as an empty-text clear (review fix).
	if req.Question != nil && req.Option == nil && req.Item == nil && req.Text == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// A thread reply (Question nil) reaches SaveDraft's insertReplyDraftTx
	// with no further existence check on Ticket (unlike an option or item
	// answer, which validates Ticket against the named question's own row);
	// a nonexistent ticket id there fails the messages.ticket_id foreign
	// key, an untyped *sqlite.Error SaveDraft returns as-is, so it fell
	// through to the generic 500 branch below instead of a 4xx (review fix,
	// PR #16). Checking existence here, for every draft mode, keeps that
	// check in the one console-owned lookup already used elsewhere
	// (views.go's threadComponent) rather than adding a store-side error
	// type this package would need a new seam to detect.
	if _, err := c.store.GetTicket(r.Context(), req.Ticket); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "ticket not found", http.StatusConflict)
			return
		}
		slog.Error("console: save draft: get ticket", "ticket_id", req.Ticket, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	in := store.DraftInput{TicketID: req.Ticket, QuestionID: req.Question, Option: req.Option, Text: text, Base: req.Base}
	if req.Item != nil {
		in.Item = &store.ItemDecision{Ref: req.Item.Ref, Decision: req.Item.Decision}
	}

	_, err := c.store.SaveDraft(r.Context(), in)
	if err != nil {
		if conflictErr, ok := errors.AsType[*store.ConflictError](err); ok {
			writeConflict(w, conflictErr)
			return
		}
		slog.Error("console: save draft", "ticket_id", req.Ticket, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeConflict writes ce as handleDraft's 409 (ticket #43): a conflict
// with no Current (every reason but "changed in another tab") stays the
// existing plain-text body, since console.js's other conflict handling
// (draftConflictMessage) only reads the status and the text. A conflict
// that does carry Current -- the stale-base case -- answers a JSON body
// instead, {"reason":..., "current":...}, so the client can learn the
// stored text to show under the box and to record as its next base,
// without a second round trip. The response header is already written by
// the time Encode could fail, so there is nothing left to tell the client;
// the error is logged and otherwise ignored (review fix, simplification).
func writeConflict(w http.ResponseWriter, ce *store.ConflictError) {
	if ce.Current == nil {
		http.Error(w, ce.Reason, http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	if err := json.NewEncoder(w).Encode(struct {
		Reason  string `json:"reason"`
		Current string `json:"current"`
	}{Reason: ce.Reason, Current: *ce.Current}); err != nil {
		slog.Error("console: write draft conflict", "reason", ce.Reason, "err", err)
	}
}

// maxSendQuestions bounds sendRequest.Questions (ticket #43: Cmd+Enter must
// name what it sends). 50 is comfortably above any one ticket's open
// question count in practice; the cap exists to keep a malformed or
// malicious body from asking SendBatchOnly to build an arbitrarily large
// set, not to express a real limit the console would ever approach.
const maxSendQuestions = 50

// sendRequest is POST /send's body (design section 6.4, ticket #43:
// sendBatch now posts {ticket, questions}, having collected the ids itself
// from what the page shows -- never a blanket "send everything drafted").
// Questions is required and non-empty: a body naming none would otherwise
// read as "send nothing's own scope", which SendBatchOnly has no way to
// tell apart from "send every draft on the ticket", the exact bug this
// ticket closes.
type sendRequest struct {
	Ticket    int64   `json:"ticket"`
	Questions []int64 `json:"questions"`
}

// handleSend is POST /send (design section 6.7, 7.1; ticket #43): send only
// the drafts on req.Questions. 400 on a malformed body, a non-positive
// ticket, an absent or empty Questions ("questions required"), more than
// maxSendQuestions ids, or any id at or below 0 (both "bad request"). 409
// when SendBatchOnly reports Empty (nothing drafted on those questions) or
// a typed conflict, 200 with a plain result line and a bus publish on
// success.
//
// The success response carries a body (bug fix: Cmd+Enter sent the batch,
// but console.js's postJSON ignored a 204's empty body, so the console
// showed nothing and the owner could not tell whether the chord had done
// anything), so it is 200 rather than 204, which forbids one. "Nothing to
// send." (the Empty branch) reads the same whether a question answers
// vanished after SendBatchOnly's own revalidation discarded them as stale.
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
	if len(req.Questions) == 0 {
		http.Error(w, "questions required", http.StatusBadRequest)
		return
	}
	if len(req.Questions) > maxSendQuestions {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, id := range req.Questions {
		if id <= 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	}

	result, err := c.store.SendBatchOnly(r.Context(), req.Ticket, req.Questions)
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
		http.Error(w, "Nothing to send. Pick an option or type a reply first.", http.StatusConflict)
		return
	}

	c.bus.Publish()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, sendResultText(result)); err != nil {
		slog.Error("console: write send result", "ticket_id", req.Ticket, "err", err)
	}
}

// sendResultText is handleSend's success body (bug fix, extended by design
// section 22.7): "Sent 1 message." singular, "Sent N messages." plural,
// read off BatchResult.Sent -- always > 0 here, since handleSend already
// returned on result.Empty above -- plus, when the batch also discarded a
// draft against a question that closed out from under it (SendBatch's own
// revalidation), " 1 not sent: its question closed first." or " N not
// sent: its question closed first.", so the owner learns what happened to
// it rather than finding it silently gone.
func sendResultText(result store.BatchResult) string {
	text := "Sent 1 message."
	if result.Sent != 1 {
		text = fmt.Sprintf("Sent %d messages.", result.Sent)
	}
	switch {
	case result.Discarded == 1:
		text += " 1 not sent: its question closed first."
	case result.Discarded > 1:
		text += fmt.Sprintf(" %d not sent: its question closed first.", result.Discarded)
	}
	return text
}

// readRequest is POST /read's body (design section 6.4, 6.8): {message}.
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
//
// The trailing-data check is a second Decode into a throwaway value, not
// dec.More() (cubic review fix, PR #16): More only peeks the next
// non-whitespace byte and treats ']' or '}' as "end of the enclosing
// array/object", so trailing garbage that happens to start with one of
// those bytes -- a body like `{"ticket":1}}`, say -- read as "no more
// input" and slipped through unrejected. A second Decode call returns
// io.EOF only when nothing but whitespace remains after the first value;
// any other outcome, a parse error or a second value alike, means there
// was another token, which is exactly "trailing data" (verified against
// both known-good and known-bad bodies before landing this fix).
func decodeStrict(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
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
