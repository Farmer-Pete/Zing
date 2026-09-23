// Package console serves the console (design section 6.9): a ticket list
// and one ticket's messages, live over two Server-Sent Events streams, plus
// the open-question block and POST /answer for recording a chosen option.
package console

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"zing/internal/bus"
	"zing/internal/store"
)

// contentTypeJS is the MIME type the vendored Datastar bundle is served
// with; named once so goconst has nothing to flag.
const contentTypeJS = "text/javascript"

// nonStreamWriteDeadline bounds how long one of the non-streaming routes
// has to finish writing its response (design section 6.10). It must never
// apply to the SSE routes, which hold the connection open far past any such
// bound on purpose (datastar skill, "Long-lived streams").
const nonStreamWriteDeadline = 5 * time.Second

//go:embed static/datastar.js
var datastarJS []byte

//go:embed templates/*.gohtml
var templatesFS embed.FS

// tmpl holds every named template in templates/*.gohtml, parsed once at
// package init. html/template escapes every dynamic value it renders, so no
// message body or ticket title can inject markup into the page.
var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.gohtml"))

// console holds the read access every handler needs: the store to render
// from and the bus every SSE stream subscribes to for its wake-up signal.
type console struct {
	store *store.Store
	bus   *bus.Broker
}

// New builds the console and returns it as an http.Handler:
//
//	GET  /                   the shell page
//	GET  /updates             the ticket-list SSE stream
//	GET  /thread?id=<n>       one ticket's message-thread SSE stream
//	POST /answer              record the chosen option for an open question
//	GET  /static/datastar.js  the vendored Datastar bundle
//
// The returned handler is a *http.ServeMux, plain HTTP/1.1, with no timeouts
// of its own; cmd/zing wraps it in an http.Server with the drain-aware
// BaseContext and shutdown sequence (Task 5c, design section 6.10).
func New(st *store.Store, b *bus.Broker) http.Handler {
	c := &console{store: st, bus: b}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", withWriteDeadline(c.handleIndex))
	mux.HandleFunc("GET /updates", c.handleUpdates) // streaming: no write deadline
	mux.HandleFunc("GET /thread", c.handleThread)   // streaming: no write deadline
	mux.HandleFunc("POST /answer", withWriteDeadline(c.handleAnswer))
	mux.HandleFunc("GET /static/datastar.js", withWriteDeadline(handleStatic))
	return mux
}

// withWriteDeadline wraps a non-streaming handler with a per-request write
// deadline set through http.ResponseController, so a stalled write cannot
// hang a connection open indefinitely (design section 6.10). It must wrap
// only the non-streaming routes: New (above) is the one place that knows
// which routes stream and which do not, so the SSE handlers never get
// wrapped here, matching the two handlers' own SetWriteDeadline(time.Time{})
// call that clears any deadline before they start writing.
//
// http.ErrNotSupported is not fatal here, unlike in the SSE handlers: it
// means w does not implement the optional deadline interface at all, which
// on a real connection never happens (net/http's own ResponseWriter always
// does) and only arises when a handler is driven directly against an
// httptest.ResponseRecorder with no real connection behind it, such as
// cmd/zing's in-process selftest e2e POST /answer (design section 11,
// "cmd/zing" fix 7). A request driven that way has nothing to time out
// against in the first place, so skipping the deadline and serving the
// request anyway is correct, not a silent downgrade of real traffic.
func withWriteDeadline(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(nonStreamWriteDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		next(w, r)
	}
}

// handleStatic serves the embedded, vendored Datastar bundle. It is not
// fetched at runtime; the file is reviewed and copied into static/ once
// (design section 0, dependency set).
func handleStatic(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", contentTypeJS)
	if _, err := w.Write(datastarJS); err != nil {
		slog.Error("console: write static bundle", "err", err)
	}
}
