package console

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/starfederation/datastar-go/datastar"

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
		http.Error(w, "list tickets: "+err.Error(), http.StatusInternalServerError)
		return
	}

	page, err := renderShell(tickets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeHTML)
	if _, err := w.Write([]byte(page)); err != nil {
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
// patch itself failed, which datastar-go treats as the client having gone
// away.
func (c *console) patchTickets(ctx context.Context, sse *datastar.ServerSentEventGenerator) bool {
	tickets, err := c.store.ListAllTickets(ctx)
	if err != nil {
		slog.Error("console: list tickets", "err", err)
		return false
	}
	fragment, err := renderTicketsFragment(tickets)
	if err != nil {
		slog.Error("console: render tickets fragment", "err", err)
		return false
	}
	return sse.PatchElements(fragment) == nil
}

// handleThread is the thread stream for one ticket: it patches #thread once
// on connect and again on every bus wake, until the client disconnects or
// Datastar cancels the previous request for a newly opened ticket (design
// section 6.9). Cancellation runs the deferred cancel, so no subscriber
// leaks.
func (c *console) handleThread(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "id must be an integer", http.StatusBadRequest)
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

	if !c.patchThread(r.Context(), sse, id) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !c.patchThread(r.Context(), sse, id) {
				return
			}
		}
	}
}

// patchThread renders ticketID's messages and patches them into #thread. A
// missing ticket renders a "not found" fragment rather than ending the
// stream, since a not-yet-committed id is a client-timing issue, not a
// stream failure. Any other store error ends the stream, matching
// patchTickets.
func (c *console) patchThread(ctx context.Context, sse *datastar.ServerSentEventGenerator, ticketID int64) bool {
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
// messages) pair, returning false when the patch itself fails.
func (c *console) patchThreadFragment(sse *datastar.ServerSentEventGenerator, ticket *store.Ticket, messages []messageView) bool {
	fragment, err := renderThreadFragment(ticket, messages)
	if err != nil {
		slog.Error("console: render thread fragment", "err", err)
		return false
	}
	return sse.PatchElements(fragment) == nil
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

// handleAnswer reads $answer, records it through store.AnswerQuestion, and
// reports the outcome: 204 and a bus publish when it is accepted, 409 with
// the named conflict reason when it is rejected (no publish, since nothing
// changed), and 500 on any other error (design section 6.9, section 6.3).
func (c *console) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var sig answerSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result, err := c.store.AnswerQuestion(r.Context(), store.AnswerInput{
		TicketID:   sig.Answer.Ticket,
		QuestionID: sig.Answer.Question,
		Option:     sig.Answer.Option,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !result.Accepted {
		http.Error(w, result.Conflict, http.StatusConflict)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}
