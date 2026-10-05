// stream_internal_test.go is a whitebox test for patchRegions and
// handleStream's MarkThreadRead branch (stream.go), the same package
// console, not console_test, precedent views_internal_test.go and
// rail_internal_test.go already set: the seam this test needs -- the
// console's own unexported ring (log.go's logRing) and the view name
// constants -- is cleanest reached directly, without exporting anything
// just for a test.
package console

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/store"
)

// newInternalTestStore opens a fresh Store on a temp-file database, closed on
// test cleanup: the same pattern console_test.go's newConsoleTestStore uses,
// duplicated here because that helper lives in package console_test and this
// file needs package console's own unexported seams.
func newInternalTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// streamTestServer serves c.handleStream directly from an httptest.Server.
// When cancelBeforeHandle is true, the handler wraps the request context in
// one that is already cancelled before handleStream runs (modeling Datastar
// aborting the prior /stream fetch, or a closed tab), the deterministic way
// to drive the cancellation this test proves against, rather than racing a
// client-side cancel against the server's own first store read.
func streamTestServer(t *testing.T, c *console, cancelBeforeHandle bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cancelBeforeHandle {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		c.handleStream(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// doStreamRequest issues one GET /stream request for (view, open) against
// base, optionally carrying the Datastar-Request header handleStream's
// MarkThreadRead branch requires (mw.go's isDatastarSameSite), and returns
// the full response body read to EOF.
func doStreamRequest(t *testing.T, base, view string, open int64, datastarHeader bool) string {
	t.Helper()
	v := url.Values{}
	v.Set("datastar", fmt.Sprintf(`{"view":%q,"open":%d,"project":0}`, view, open))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/stream?"+v.Encode(), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if datastarHeader {
		req.Header.Set("Datastar-Request", "true")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// newCancelTestConsole builds a console and its log Handler for one
// cancel-logging subtest: a fresh store, a *slog.LevelVar at Debug (so the
// ring captures every record regardless of level, design section 6.12's
// "pass" gate), and the Handler installed as slog's default the way
// cmd/zing/serve.go installs the real one, restored on cleanup.
func newCancelTestConsole(t *testing.T) (*console, *Handler) {
	t.Helper()
	s := newInternalTestStore(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	h := NewHandler(io.Discard, lv, nil)
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &console{store: s, bus: bus.New(), log: h}, h
}

// assertNoStreamError asserts the console log handler holds no ERROR (or
// above) record and that alertsComponent would render no line, then asserts
// the ring holds a Debug entry for each of wantDebugMessages: the cancelled
// failure is still logged, just not loudly.
func assertNoStreamError(t *testing.T, h *Handler, body string, wantDebugMessages []string) {
	t.Helper()
	if strings.Contains(body, `id="nav"`) {
		t.Fatalf("response body carries a #nav frame; the store read did not observe the cancelled context:\n%s", body)
	}

	for _, e := range h.Warnings(RingCapacity) {
		if e.Level >= slog.LevelError {
			t.Errorf("ERROR record in ring: %q", e.Message)
		}
	}
	if lines := buildAlertLines(h.Warnings(alertsLimit)); len(lines) != 0 {
		t.Errorf("alertsComponent would render %d line(s), want none: %+v", len(lines), lines)
	}

	h.ring.mu.Lock()
	entries := append([]LogEntry(nil), h.ring.entries...)
	h.ring.mu.Unlock()

	for _, want := range wantDebugMessages {
		found := false
		for _, e := range entries {
			if e.Message == want && e.Level == slog.LevelDebug {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ring missing a Debug %q entry", want)
		}
	}
}

// TestStreamCancelLogsNoError is the regression test for the bug: a /stream
// whose request context is already done (the owner navigated, or closed the
// tab, and Datastar aborted the prior fetch) must log nothing at ERROR or
// above, in either the inbox view (patchRegions' own build-nav failure) or
// the thread view (handleStream's MarkThreadRead failure, then the same
// build-nav failure). Not parallel: it swaps slog's process-wide default
// logger (gate_test.go's own precedent for why that must stay sequential).
func TestStreamCancelLogsNoError(t *testing.T) {
	t.Run("inbox", func(t *testing.T) {
		c, h := newCancelTestConsole(t)
		srv := streamTestServer(t, c, true)

		body := doStreamRequest(t, srv.URL, viewInbox, 0, false)
		assertNoStreamError(t, h, body, []string{logMsgBuildNav})
	})

	t.Run("thread", func(t *testing.T) {
		c, h := newCancelTestConsole(t)
		srv := streamTestServer(t, c, true)

		body := doStreamRequest(t, srv.URL, viewThread, 1, true)
		assertNoStreamError(t, h, body, []string{"console: stream: mark thread read", logMsgBuildNav})
	})
}

// TestStreamStoreErrorStillLogsError proves the fix is scoped to a
// cancelled context: a store failure on a live request context (here, a
// closed store) still logs at ERROR, so a genuine fault still reaches the
// #alerts banner.
func TestStreamStoreErrorStillLogsError(t *testing.T) {
	s := newInternalTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	h := NewHandler(io.Discard, lv, nil)
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := &console{store: s, bus: bus.New(), log: h}
	srv := streamTestServer(t, c, false)

	doStreamRequest(t, srv.URL, viewInbox, 0, false)

	found := false
	for _, e := range h.Warnings(RingCapacity) {
		if e.Message == logMsgBuildNav && e.Level == slog.LevelError {
			found = true
		}
	}
	if !found {
		t.Errorf("ring missing an ERROR %q entry for a live-context store failure", logMsgBuildNav)
	}
}
