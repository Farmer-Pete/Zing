package console

import (
	"bytes"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/starfederation/datastar-go/datastar"

	"zing/internal/console/templates"
	"zing/internal/store"
)

// contentTypeHTML is the shell page's Content-Type; named once so goconst
// has nothing to flag.
const contentTypeHTML = "text/html; charset=utf-8"

// handleIndex serves the shell: the palette, the #nav/#main/#rail regions
// rendered once for the shell's default signals (view=inbox, open=0,
// project=0), the #stream-ctl bridge, and the script tag (design section
// 6.3, 7.1). The shell's own data-init on #stream-ctl calls GET /stream
// immediately after, whose first frame patches the same three regions
// again -- the same view-building code path this handler already used, so
// the two never drift.
func (c *console) handleIndex(w http.ResponseWriter, r *http.Request) {
	nav, err := c.navComponent(r.Context())
	if err != nil {
		slog.Error("console: build nav", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}
	main, err := c.mainComponent(r.Context(), viewInbox, 0, 0)
	if err != nil {
		slog.Error("console: build main", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	// Rendered into a buffer first, not straight to w, so a render failure
	// still reports 500 rather than sending a 200 with a half-written body.
	var buf bytes.Buffer
	if err := templates.Shell(nav, main, templates.Rail()).Render(r.Context(), &buf); err != nil {
		slog.Error("console: render shell", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeHTML)
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Error("console: write shell page", "err", err)
	}
}

// answerSignals is the shape POST /answer reads from the client's $answer
// signal: a chip's data-on:click sets $answer to exactly this nested object
// before @post('/answer') sends it (design section 6.9). Task 3 keeps this
// handler exactly as Package 3 built it; Tasks 6 and 7 rework it into
// /draft and /send.
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
// no error detail, since the underlying errors can carry database internals
// a browser has no business seeing. The detail goes to slog instead (design
// section 6.9: never err.Error() in the response).
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
