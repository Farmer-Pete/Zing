// Package console serves the console (design section 6.3): the shell page,
// one live GET /stream per tab that patches the #nav, #main, and #rail
// regions, and POST /answer for recording a chosen option. Every page and
// fragment renders through the templ components in internal/console/templates
// (design section 6.2); there is no html/template use in this package.
package console

import (
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"zing/internal/bus"
	"zing/internal/store"
)

// contentTypeJS and contentTypeJSON are the MIME types the vendored and
// authored static assets are served with; named once so goconst has
// nothing to flag.
const (
	contentTypeJS   = "text/javascript"
	contentTypeJSON = "application/json"
)

// nonStreamWriteDeadline bounds how long one of the non-streaming routes
// has to finish writing its response (design section 6.10). It must never
// apply to the SSE routes, which hold the connection open far past any such
// bound on purpose (datastar skill, "Long-lived streams").
const nonStreamWriteDeadline = 5 * time.Second

// The five public static assets (design section 5, 12): the vendored
// Datastar bundle (Package 3), the vendored mermaid.js (static/ASSETS.md
// records its source, version, and digest), and three assets authored in
// this repo (console.js, keyboard.mjs, keys.json are Task 1 skeletons or
// placeholders; Task 4 fills them in for real). Each is embedded by its
// own exact path, never as a directory tree, so the mux below can register
// an explicit allowlist: console.test.js, package.json, and ASSETS.md have
// no route and so 404, the same as any other unlisted path under /static/.
//
//go:embed static/datastar.js
var datastarJS []byte

//go:embed static/mermaid.js
var mermaidJS []byte

//go:embed static/console.js
var consoleJS []byte

//go:embed static/keyboard.mjs
var keyboardMJS []byte

//go:embed static/keys.json
var keysJSON []byte

// console holds the read access every handler needs: the store to render
// from and the bus every SSE stream subscribes to for its wake-up signal.
type console struct {
	store *store.Store
	bus   *bus.Broker
}

// New builds the console and returns it as an http.Handler:
//
//	GET  /                     the shell page
//	GET  /stream                the one live SSE stream per tab (design section 6.3)
//	POST /answer                record the chosen option for an open question
//	GET  /static/datastar.js    the vendored Datastar bundle
//	GET  /static/mermaid.js     the vendored mermaid bundle
//	GET  /static/console.js     the console's DOM wiring (Task 1 skeleton)
//	GET  /static/keyboard.mjs   the console's pure keyboard logic (Task 1 skeleton)
//	GET  /static/keys.json      the keyboard binding table (Task 1 placeholder)
//
// /static/ is an explicit allowlist of exactly those five assets (design
// section 5, 12): every other path, including console.test.js, package.json,
// and ASSETS.md, has no registered route and so 404s from the mux itself.
//
// The returned handler is a *http.ServeMux, plain HTTP/1.1, with no timeouts
// of its own; cmd/zing wraps it in an http.Server with the drain-aware
// BaseContext and shutdown sequence (design section 6.14, cmd/zing/serve.go).
func New(st *store.Store, b *bus.Broker) http.Handler {
	c := &console{store: st, bus: b}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", withWriteDeadline(c.handleIndex))
	mux.HandleFunc("GET /stream", c.handleStream) // streaming: no write deadline
	mux.HandleFunc("POST /answer", withWriteDeadline(requireSameOrigin(c.handleAnswer)))
	mux.HandleFunc("GET /static/datastar.js", withWriteDeadline(staticAsset(datastarJS, contentTypeJS)))
	mux.HandleFunc("GET /static/mermaid.js", withWriteDeadline(staticAsset(mermaidJS, contentTypeJS)))
	mux.HandleFunc("GET /static/console.js", withWriteDeadline(staticAsset(consoleJS, contentTypeJS)))
	mux.HandleFunc("GET /static/keyboard.mjs", withWriteDeadline(staticAsset(keyboardMJS, contentTypeJS)))
	mux.HandleFunc("GET /static/keys.json", withWriteDeadline(staticAsset(keysJSON, contentTypeJSON)))
	return mux
}

// withWriteDeadline wraps a non-streaming handler with a per-request write
// deadline set through http.ResponseController, so a stalled write cannot
// hang a connection open indefinitely (design section 6.10). It must wrap
// only the non-streaming routes: New (above) is the one place that knows
// which routes stream and which do not, so /stream never gets wrapped
// here, matching its own SetWriteDeadline(time.Time{}) call that clears any
// deadline before it starts writing.
//
// http.ErrNotSupported is not fatal here, unlike in the streaming handler: it
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

// datastarRequestHeader is the header every Datastar backend action sends
// (datastar skill, attributes.md: "All backend actions send a
// Datastar-Request: true header"). A plain cross-site form POST cannot set a
// custom header without triggering a CORS preflight, so requiring it here
// rules out that attack shape even when Sec-Fetch-Site is absent.
const datastarRequestHeader = "Datastar-Request"

// requireSameOrigin guards a mutating route against a cross-site request
// forgery (CWE-352, design section "Console" fix 2): a cross-site fetch sent
// as text/plain under no-cors mode still reaches datastar.ReadSignals, since
// it decodes whatever body arrived without checking its declared content
// type, so without this guard a hostile page could POST a crafted body to
// /answer using the visitor's own session and silently answer an open
// question. The check rejects the request with 403 unless both hold: the
// Sec-Fetch-Site header, when the browser sends one, is "same-origin" or
// "none" (a same-origin fetch, or a request with no meaningful origin, such
// as a curl call or an older browser); and the Datastar-Request header is
// present and "true", which the SDK always sets on every backend action but
// a simple cross-origin form POST cannot set without a CORS preflight the
// browser would block first.
func requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		if r.Header.Get(datastarRequestHeader) != "true" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// staticAsset returns a handler serving one embedded static asset with a
// fixed content type. Every asset served this way is either vendored and
// reviewed once, not fetched at runtime (datastar.js, mermaid.js; design
// section 0, dependency set, and static/ASSETS.md), or authored in this
// repo (console.js, keyboard.mjs, keys.json).
func staticAsset(body []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if _, err := w.Write(body); err != nil {
			slog.Error("console: write static asset", "content_type", contentType, "err", err)
		}
	}
}
