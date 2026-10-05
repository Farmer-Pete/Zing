// stream.go: GET /stream, the one live SSE stream per tab (design section
// 6.3, Q6). It reads the three navigation signals, renders and patches
// #nav, #main, and #rail once on connect, then again every time the bus
// wakes it or its own heartbeat ticker fires, until the request context is
// done.
//
// bus.Subscribe's cancel deregisters the subscription but never closes the
// channel (Task 1 reconciliation against merged Package 3, design section
// 14: "the /stream loop must select on r.Context().Done() to exit; there is
// no closed-channel branch"), so the loop below selects on the request
// context alone; a !ok receive is never reachable and is not written.
//
// The heartbeat (bug fix, console.streamHeartbeat) re-renders and patches
// all four regions on a fixed interval with no bus wake at all, so a
// missed or swallowed wake -- the stream's own failure mode this fixes --
// heals within one interval instead of leaving the page stale with nothing
// on screen saying so. A zero streamHeartbeat (every test that does not
// itself exercise the beat) leaves the ticker channel nil, and a nil
// channel's select case never fires.
package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/starfederation/datastar-go/datastar"
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
	// Defense in depth only: for a bodyless GET such as this one, net/http
	// already clears the read deadline before the handler runs
	// (go1.27 server.go startBackgroundRead), so this is not confirmed as the
	// cause of the 19:25:57 cancel (H1 is unconfirmed). It guards against a
	// request shape where net/http does not clear it first.
	//
	// http.ErrNotSupported is not fatal here, matching withWriteDeadline's
	// own tolerance of it (server.go): it means w does not implement the
	// optional deadline interface at all, which a real connection's
	// ResponseWriter always does, so answering 500 here would kill an
	// otherwise-healthy stream over a capability its transport never had.
	if err := rc.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Error("console: stream: clear read deadline", "err", err)
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

	// Opening a thread is a fresh GET /stream (shell.templ's zing-nav), so
	// this connect is the one place it is marked read; a bus wake below never
	// marks, so a patch the owner did not cause leaves new messages unread.
	// GET /stream only passes the Host check (mw.go requireAllowedHost), so
	// the write also needs the same-site signal isDatastarSameSite shares
	// with the mutation guard (mw.go): a cross-site img or link cannot send
	// it. Any other request still renders but writes nothing. It runs before
	// Subscribe so this tab's own Publish does not wake it, and publishes
	// only when a row changed. A failure is logged and the stream still
	// renders.
	if sig.View == viewThread && sig.Open > 0 {
		if !isDatastarSameSite(r) {
			slog.Debug("console: stream: open not marked", "ticket_id", sig.Open, "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		} else {
			n, err := c.store.MarkThreadRead(r.Context(), sig.Open)
			switch {
			case err != nil:
				logStreamErr(r.Context(), slog.LevelError, "console: stream: mark thread read", err, "ticket_id", sig.Open, "view", sig.View)
			case n > 0:
				slog.Info("console: stream: marked thread read", "ticket_id", sig.Open, "marked", n)
				c.bus.Publish()
			default:
				slog.Debug("console: stream: thread already read", "ticket_id", sig.Open)
			}
		}
	}

	ch, cancel := c.bus.Subscribe()
	defer cancel()

	// A zero streamHeartbeat (every test that does not itself exercise the
	// beat) leaves beat nil, so its select case below never fires -- not a
	// missing bug fix's worth of real behavior, just no ticker to stop.
	var beat <-chan time.Time
	if c.streamHeartbeat > 0 {
		t := time.NewTicker(c.streamHeartbeat)
		defer t.Stop()
		beat = t.C
	}

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
		case <-beat:
			if !c.patchRegions(r.Context(), sse, sig) {
				return
			}
		}
	}
}

// patchRegions renders and patches #nav, #main, #rail, and #alerts for one
// frame, always all four (design section 6.3: "#rail ... is always
// patched, so leaving a thread clears the old rail"; design section 6a,
// D8: "#alerts ... patched on every frame"). It returns false -- ending the
// stream -- on a store read failure or on any PatchElementTempl error (a
// render error or a write error alike, folded together by the SDK, most
// often meaning the client has gone away). Every PatchElementTempl error is
// logged, with the frame's ids, before the stream ends (review fix, PR
// #16): the SDK folds a render error into the same return as an ordinary
// client-disconnect write error, so this was the only place that could
// still tell the two apart, and silently ending on a genuine render error
// left it undiagnosable.
//
// #alerts never logs at warn itself (its own render path only reads the
// ring; it never calls slog.Warn), so patching it cannot re-trigger onWarn
// and cascade into another wake. A patch failure here ends the stream, like
// every other region's, rather than looping (design section 6a, "risk:
// alerts onWarn re-entrancy").
//
// Every failure here is logged through logStreamErr, so a failure after the
// stream's own context is done -- the owner navigated or closed the tab --
// logs at Debug instead of its usual level, and so never reaches the
// #alerts banner.
func (c *console) patchRegions(ctx context.Context, sse *datastar.ServerSentEventGenerator, sig streamSignals) bool {
	// nav's own open ticket is sig.Open only in the thread view: every other
	// view's open is 0 in practice (console.js's reduceNav sets the whole
	// {view,open,project} triple together on every navigation), but a
	// stray/crafted sig.Open must not select a row the thread view did not
	// actually open.
	navOpen := int64(0)
	if sig.View == viewThread {
		navOpen = sig.Open
	}
	nav, err := c.navComponent(ctx, navOpen)
	if err != nil {
		logStreamErr(ctx, slog.LevelError, "console: stream: build nav", err)
		return false
	}
	if patchErr := sse.PatchElementTempl(nav); patchErr != nil {
		logStreamErr(ctx, slog.LevelWarn, "console: stream: patch nav", patchErr, "view", sig.View, "open", sig.Open)
		return false
	}

	main, err := c.mainComponent(ctx, sig.View, sig.Open, sig.Project)
	if err != nil {
		logStreamErr(ctx, slog.LevelError, "console: stream: build main", err, "view", sig.View)
		return false
	}
	if patchErr := sse.PatchElementTempl(main); patchErr != nil {
		logStreamErr(ctx, slog.LevelWarn, "console: stream: patch main", patchErr, "view", sig.View, "open", sig.Open)
		return false
	}

	rail, err := c.railComponent(ctx, sig.View, sig.Open)
	if err != nil {
		logStreamErr(ctx, slog.LevelError, "console: stream: build rail", err, "view", sig.View, "open", sig.Open)
		return false
	}
	if patchErr := sse.PatchElementTempl(rail); patchErr != nil {
		logStreamErr(ctx, slog.LevelWarn, "console: stream: patch rail", patchErr, "view", sig.View, "open", sig.Open)
		return false
	}

	if patchErr := sse.PatchElementTempl(c.alertsComponent()); patchErr != nil {
		logStreamErr(ctx, slog.LevelWarn, "console: stream: patch alerts", patchErr, "view", sig.View, "open", sig.Open)
		return false
	}
	return true
}

// logStreamErr logs a /stream failure at level, or at Debug once the
// stream's context is done: a cancelled stream means the owner navigated or
// closed the tab, not a fault, and #alerts shows every WARN-and-above record
// the ring holds (views.go's alertsComponent), so a cancellation must never
// reach it at WARN or ERROR.
func logStreamErr(ctx context.Context, level slog.Level, msg string, err error, args ...any) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, msg, append(args, "err", err)...)
}
