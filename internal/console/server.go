// Package console serves the console (design section 6.9): a ticket list
// and one ticket's messages, live over two Server-Sent Events streams, plus
// the open-question block and POST /answer for recording a chosen option.
package console

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"

	"zing/internal/bus"
	"zing/internal/store"
)

// contentTypeJS is the MIME type the vendored Datastar bundle is served
// with; named once so goconst has nothing to flag.
const contentTypeJS = "text/javascript"

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
	mux.HandleFunc("GET /{$}", c.handleIndex)
	mux.HandleFunc("GET /updates", c.handleUpdates)
	mux.HandleFunc("GET /thread", c.handleThread)
	mux.HandleFunc("POST /answer", c.handleAnswer)
	mux.HandleFunc("GET /static/datastar.js", handleStatic)
	return mux
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
