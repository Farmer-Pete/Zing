package console

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"zing/internal/console/templates"
	"zing/internal/store"
)

// contentTypeHTML is the shell page's Content-Type; named once so goconst
// has nothing to flag.
const contentTypeHTML = "text/html; charset=utf-8"

// handleIndex serves the shell: the server-rendered ticket list, the empty
// thread region, the client signal, and the script tag (design section 6.9).
func (c *console) handleIndex(w http.ResponseWriter, r *http.Request) {
	tickets, err := c.store.ListAllTickets(r.Context())
	if err != nil {
		slog.Error("console: list tickets", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	// Rendered into a buffer first, not straight to w, so a render failure
	// (impossible for Shell's typed, error-free parameters today, but kept
	// symmetric with every other error path here) still reports 500 rather
	// than sending a 200 with a half-written body.
	var buf bytes.Buffer
	if err := templates.Shell(tickets).Render(r.Context(), &buf); err != nil {
		slog.Error("console: render shell", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeHTML)
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Error("console: write shell page", "err", err)
	}
}

// handleUpdates is the list stream: it patches #tickets once on connect and
// again every time the bus wakes it, until the client disconnects (design
// section 6.9).
func (c *console) handleUpdates(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sse := datastar.NewSSE(w, r)
	ch, cancel := c.bus.Subscribe()
	defer cancel()

	if !c.patchTickets(r.Context(), sse) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !c.patchTickets(r.Context(), sse) {
				return
			}
		}
	}
}

// patchTickets renders the current ticket list and patches it into #tickets.
// It returns false when the stream should end: a store read failed, or the
// patch itself failed (a render error or a write error alike; PatchElementTempl
// folds the two together), which datastar-go treats as the client having
// gone away.
func (c *console) patchTickets(ctx context.Context, sse *datastar.ServerSentEventGenerator) bool {
	tickets, err := c.store.ListAllTickets(ctx)
	if err != nil {
		slog.Error("console: list tickets", "err", err)
		return false
	}
	return sse.PatchElementTempl(templates.TicketsFragment(tickets)) == nil
}

// threadSignals is the shape GET /thread reads from the client's $open
// signal: the id of the open ticket, 0 meaning none (design section 6.9).
// For a GET, Datastar sends signals JSON-encoded in the "datastar" query
// parameter (datastar skill, go-sdk.md), so ReadSignals decodes from there
// rather than from a query id.
type threadSignals struct {
	Open int64 `json:"open"`
}

// handleThread is the thread stream: it patches #thread once on connect and
// again on every bus wake, until the client disconnects or Datastar cancels
// the previous request for a newly opened ticket (design section 6.9). The
// route itself is a STABLE url, "/thread", never "/thread?id=<n>": Datastar
// keys requestCancellation (default "auto") by method and url, so a
// per-ticket url would not cancel the prior stream when $open switches from
// one ticket to another, leaving a stale stream that could patch #thread
// with the wrong ticket's content. A constant url means every $open change
// aborts the prior fetch first; cancellation runs the deferred cancel below,
// so no subscriber leaks.
func (c *console) handleThread(w http.ResponseWriter, r *http.Request) {
	var sig threadSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		slog.Error("console: read thread signals", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sse := datastar.NewSSE(w, r)
	ch, cancel := c.bus.Subscribe()
	defer cancel()

	if !c.patchThread(r.Context(), sse, sig.Open) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !c.patchThread(r.Context(), sse, sig.Open) {
				return
			}
		}
	}
}

// patchThread renders ticketID's messages and patches them into #thread. A
// non-positive ticketID (open == 0: no ticket is open, or the signal was
// absent) renders an empty thread rather than looking anything up or
// erroring, matching the design's "id<=0 means render nothing" guard (design
// section 6.9). A missing ticket ID (positive, but no such row) renders a
// "not found" fragment rather than ending the stream, since a
// not-yet-committed id is a client-timing issue, not a stream failure. Any
// other store error ends the stream, matching patchTickets.
func (c *console) patchThread(ctx context.Context, sse *datastar.ServerSentEventGenerator, ticketID int64) bool {
	if ticketID <= 0 {
		return c.patchThreadFragment(sse, nil, nil)
	}
	ticket, err := c.store.GetTicket(ctx, ticketID)
	switch {
	case err == nil:
		messages, listErr := c.store.ListMessages(ctx, ticketID)
		if listErr != nil {
			slog.Error("console: list messages", "ticket_id", ticketID, "err", listErr)
			return false
		}
		return c.patchThreadFragment(sse, &ticket, buildMessageViews(messages))
	case errors.Is(err, sql.ErrNoRows):
		return c.patchThreadFragment(sse, nil, nil)
	default:
		slog.Error("console: get ticket", "ticket_id", ticketID, "err", err)
		return false
	}
}

// patchThreadFragment renders and patches #thread for one (ticket,
// messages) pair, returning false when the patch itself fails (a render
// error or a write error alike; PatchElementTempl folds the two together).
func (c *console) patchThreadFragment(sse *datastar.ServerSentEventGenerator, ticket *store.Ticket, messages []templates.MessageView) bool {
	return sse.PatchElementTempl(templates.ThreadFragment(ticket, messages)) == nil
}

// answerSignals is the shape POST /answer reads from the client's $answer
// signal: a chip's data-on:click sets $answer to exactly this nested object
// before @post('/answer') sends it (design section 6.9).
type answerSignals struct {
	Answer struct {
		Ticket   int64  `json:"ticket"`
		Question int64  `json:"question"`
		Option   string `json:"option"`
	} `json:"answer"`
}

// optionPattern is the one legal shape for a chosen option key: a single
// lowercase letter, matching the option keys commit.go's QuestionPayload
// mapping and AnswerQuestion's schema both use (design section 6.3, 6.6).
var optionPattern = regexp.MustCompile(`^[a-z]$`)

// genericServerErrorBody is what a real store error returns to the client:
// no error detail, since AnswerQuestion's underlying errors can carry
// database internals a browser has no business seeing. The detail goes to
// slog instead (design section 6.9: never err.Error() in the response).
const genericServerErrorBody = "internal error"

// maxAnswerBodyBytes bounds POST /answer's request body: the $answer signal
// is a handful of small fields, so a few KB is comfortable headroom, and
// wrapping r.Body in http.MaxBytesReader before ReadSignals keeps an
// oversized body from being buffered in full before validation ever runs.
const maxAnswerBodyBytes = 8 << 10 // 8 KiB

// handleAnswer validates $answer, records it through store.AnswerQuestion,
// and reports the outcome: 400 on a malformed signal (a non-positive ticket
// or question id, or an option that is not a single lowercase letter), 204
// and a bus publish when the store accepts it, 409 with the named conflict
// reason when the store rejects it (a safe, intended message; no publish,
// since nothing changed), and 500 with a generic body on any other store
// error, logged server-side with the detail (design section 6.9, 6.3).
func (c *console) handleAnswer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAnswerBodyBytes)

	var sig answerSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		slog.Error("console: read answer signals", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if sig.Answer.Ticket <= 0 || sig.Answer.Question <= 0 || !optionPattern.MatchString(sig.Answer.Option) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	result, err := c.store.AnswerQuestion(r.Context(), store.AnswerInput{
		TicketID:   sig.Answer.Ticket,
		QuestionID: sig.Answer.Question,
		Option:     sig.Answer.Option,
	})
	if err != nil {
		slog.Error("console: answer question", "ticket_id", sig.Answer.Ticket, "question_id", sig.Answer.Question, "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	if !result.Accepted {
		http.Error(w, result.Conflict, http.StatusConflict)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}
