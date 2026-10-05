// stream_internal_test.go is a whitebox test for patchRegions and
// handleStream's MarkThreadRead branch (stream.go), the same package
// console, not console_test, precedent views_internal_test.go and
// rail_internal_test.go already set: the seam this test needs -- the
// console's own unexported ring (log.go's logRing) and the view name
// constants -- is cleanest reached directly, without exporting anything
// just for a test.
package console

import (
	"bytes"
	"context"
	"errors"
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

// newTestLogHandler builds a Handler at Debug (so the ring captures every
// record regardless of level, design section 6.12's "pass" gate), its text
// sink writing to w, and installs it as slog's default the way
// cmd/zing/serve.go installs the real one, restored on cleanup.
func newTestLogHandler(t *testing.T, w io.Writer) *Handler {
	t.Helper()
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	h := NewHandler(w, lv, nil)
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// newStreamTestConsole builds a console and its log Handler for a /stream
// test: a fresh store, closed on test cleanup, plus newTestLogHandler's
// Handler, installed as slog's default.
func newStreamTestConsole(t *testing.T) (*console, *Handler) {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := newTestLogHandler(t, io.Discard)
	return &console{store: s, bus: bus.New(), log: h}, h
}

// ringEntries returns a locked snapshot of h's ring, the same copy-under-
// mutex every direct ring read in this file needs.
func ringEntries(h *Handler) []LogEntry {
	h.ring.mu.Lock()
	defer h.ring.mu.Unlock()
	return append([]LogEntry(nil), h.ring.entries...)
}

// TestStreamCancelLogsNoError is the regression test for the bug: a /stream
// whose request context is already done (the owner navigated, or closed the
// tab, and Datastar aborted the prior fetch) must log nothing at ERROR or
// above, in either the inbox view (patchRegions' own build-nav failure) or
// the thread view (handleStream's MarkThreadRead failure, then the same
// build-nav failure). Not parallel: it swaps slog's process-wide default
// logger (gate_test.go's own precedent for why that must stay sequential).
func TestStreamCancelLogsNoError(t *testing.T) {
	cases := []struct {
		name           string
		view           string
		open           int64
		datastarHeader bool
		wantDebug      []string
	}{
		{
			name:      "inbox",
			view:      viewInbox,
			open:      0,
			wantDebug: []string{"console: stream: build nav"},
		},
		{
			name:           "thread",
			view:           viewThread,
			open:           1,
			datastarHeader: true,
			wantDebug:      []string{"console: stream: mark thread read", "console: stream: build nav"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, h := newStreamTestConsole(t)
			srv := streamTestServer(t, c, true)

			body := doStreamRequest(t, srv.URL, tc.view, tc.open, tc.datastarHeader)

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

			entries := ringEntries(h)
			for _, want := range tc.wantDebug {
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
		})
	}
}

// TestStreamStoreErrorStillLogsError proves the fix is scoped to a
// cancelled context: a store failure on a live request context (here, a
// closed store) still logs at ERROR, so a genuine fault still reaches the
// #alerts banner.
func TestStreamStoreErrorStillLogsError(t *testing.T) {
	c, h := newStreamTestConsole(t)
	if err := c.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	srv := streamTestServer(t, c, false)

	doStreamRequest(t, srv.URL, viewInbox, 0, false)

	const wantMsg = "console: stream: build nav"
	found := false
	for _, e := range h.Warnings(RingCapacity) {
		if e.Message == wantMsg && e.Level == slog.LevelError {
			found = true
		}
	}
	if !found {
		t.Errorf("ring missing an ERROR %q entry for a live-context store failure", wantMsg)
	}
}

// TestLogStreamErr is a direct whitebox table test of logStreamErr's two
// independent triggers for dropping to Debug: errors.Is(err,
// context.Canceled), and ctx.Err() != nil, each on its own. The two cases in
// TestStreamCancelLogsNoError always hit both triggers together (a store
// error wrapping context.Canceled under an already-cancelled context), so
// neither proves the ctx.Err() branch matters on its own; that branch is the
// only thing that drops a patch-write failure -- never itself
// context.Canceled -- from WARN to Debug once the stream's context is done.
// Not parallel: it swaps slog's process-wide default logger.
func TestLogStreamErr(t *testing.T) {
	cases := []struct {
		name      string
		cancelled bool
		err       error
		level     slog.Level
		want      slog.Level
	}{
		{
			name:      "cancelled ctx, non-Canceled error logs at Debug",
			cancelled: true,
			err:       errors.New("write: broken pipe"),
			level:     slog.LevelWarn,
			want:      slog.LevelDebug,
		},
		{
			name:      "live ctx, wrapped context.Canceled logs at Debug",
			cancelled: false,
			err:       fmt.Errorf("inbox items: %w", context.Canceled),
			level:     slog.LevelError,
			want:      slog.LevelDebug,
		},
		{
			name:      "live ctx, plain error keeps its level",
			cancelled: false,
			err:       errors.New("boom"),
			level:     slog.LevelWarn,
			want:      slog.LevelWarn,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sink bytes.Buffer
			h := newTestLogHandler(t, &sink)

			ctx := t.Context()
			if tc.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			const msg = "console: stream: test case"
			logStreamErr(ctx, tc.level, msg, tc.err)

			entries := ringEntries(h)

			var got *LogEntry
			for i := range entries {
				if entries[i].Message == msg {
					got = &entries[i]
				}
			}
			if got == nil {
				t.Fatalf("ring missing entry %q", msg)
			}
			if got.Level != tc.want {
				t.Errorf("ring entry level = %v, want %v", got.Level, tc.want)
			}

			if !strings.Contains(sink.String(), tc.err.Error()) {
				t.Errorf("sink output missing err %q:\n%s", tc.err.Error(), sink.String())
			}
		})
	}
}
