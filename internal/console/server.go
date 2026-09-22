// Package console serves the SILENT-RING console (design section 6.9): a
// ticket list and one ticket's messages, live over two Server-Sent Events
// streams. It shows work; it takes no decisions here. The question-and-answer
// block and POST /answer are Task 6, built on top of this package later.
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

// New builds the silent-ring console and returns it as an http.Handler:
//
//	GET  /                   the shell page
//	GET  /updates             the ticket-list SSE stream
//	GET  /thread?id=<n>       one ticket's message-thread SSE stream
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
