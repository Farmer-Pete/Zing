package console

import (
	"bytes"
	"log/slog"
	"net/http"

	"zing/internal/console/templates"
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

// genericServerErrorBody is what a real store error returns to the client:
// no error detail, since the underlying errors can carry database internals
// a browser has no business seeing. The detail goes to slog instead (design
// section 6.9: never err.Error() in the response).
const genericServerErrorBody = "internal error"
