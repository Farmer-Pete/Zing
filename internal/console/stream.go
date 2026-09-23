// stream.go: GET /stream, the one live SSE stream per tab (design section
// 6.3, Q6). It reads the three navigation signals, renders and patches
// #nav, #main, and #rail once on connect, then again every time the bus
// wakes it, until the request context is done.
//
// bus.Subscribe's cancel deregisters the subscription but never closes the
// channel (Task 1 reconciliation against merged Package 3, design section
// 14: "the /stream loop must select on r.Context().Done() to exit; there is
// no closed-channel branch"), so the loop below selects on the request
// context alone; a !ok receive is never reachable and is not written.
package console

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"zing/internal/console/templates"
)

// streamSignals is the shape GET /stream reads from the client (design
// section 6.3, 6.4): the three navigation signals the shell's body declares
// with their defaults, so even the very first request already carries real
// state.
type streamSignals struct {
	View    string `json:"view"`
	Open    int64  `json:"open"`
	Project int64  `json:"project"`
}

// handleStream is GET /stream (design section 6.3, steps 1-6): clear the
// write deadline, read the signals, open the SSE generator, subscribe to
// the bus, patch once, then loop on the request context and the bus
// channel until the client disconnects.
func (c *console) handleStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	var sig streamSignals
	if err := datastar.ReadSignals(r, &sig); err != nil {
		slog.Error("console: read stream signals", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sse := datastar.NewSSE(w, r)
	ch, cancel := c.bus.Subscribe()
	defer cancel()

	if !c.patchRegions(r.Context(), sse, sig) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !c.patchRegions(r.Context(), sse, sig) {
				return
			}
		}
	}
}

// patchRegions renders and patches #nav, #main, and #rail for one frame,
// always all three (design section 6.3: "#rail ... is always patched, so
// leaving a thread clears the old rail"). It returns false -- ending the
// stream -- on a store read failure or on any PatchElementTempl error (a
// render error or a write error alike, folded together by the SDK, meaning
// the client has gone away).
func (c *console) patchRegions(ctx context.Context, sse *datastar.ServerSentEventGenerator, sig streamSignals) bool {
	nav, err := c.navComponent(ctx)
	if err != nil {
		slog.Error("console: stream: build nav", "err", err)
		return false
	}
	if patchErr := sse.PatchElementTempl(nav); patchErr != nil {
		return false
	}

	main, err := c.mainComponent(ctx, sig.View, sig.Open, sig.Project)
	if err != nil {
		slog.Error("console: stream: build main", "view", sig.View, "err", err)
		return false
	}
	if patchErr := sse.PatchElementTempl(main); patchErr != nil {
		return false
	}

	return sse.PatchElementTempl(templates.Rail()) == nil
}
