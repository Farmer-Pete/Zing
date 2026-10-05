// stream_internal_test.go is a whitebox test for patchRegions and
// handleStream's MarkThreadRead branch (stream.go), the same package
// console, not console_test, precedent views_internal_test.go and
// rail_internal_test.go already set: the seam this test needs -- the
// console's own unexported ring (log.go's logRing) and the view name
// constants -- is cleanest reached directly, without exporting anything
// just for a test.
package console

import (
	"bufio"
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
	"time"

	"zing/internal/bus"
	"zing/internal/response"
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

// streamBeatFrameTimeout bounds every SSE read the heartbeat tests below
// make: long enough for a slow CI box, short enough that a hung stream
// fails the test instead of the suite.
const streamBeatFrameTimeout = 5 * time.Second

// openInternalStream issues a GET /stream request for (view, open) against
// base, carrying the Datastar-Request header handleStream's MarkThreadRead
// branch requires (mw.go's isDatastarSameSite), and returns the response
// and a buffered reader over its still-open body, without reading any
// frame yet. This is console_test.go:36's openStream, reimplemented here
// because this file is package console, not console_test, and so cannot
// call it directly.
func openInternalStream(t *testing.T, base, view string, open int64) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	v := url.Values{}
	v.Set("datastar", fmt.Sprintf(`{"view":%q,"open":%d,"project":0}`, view, open))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stream?"+v.Encode(), http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /stream: %v", err)
	}
	return resp, bufio.NewReader(resp.Body), cancel
}

// readStreamFrame reads one SSE frame from r, bounded by
// streamBeatFrameTimeout so a hung stream fails fast instead of hanging the
// test suite. This is console_test.go:382's readFrame pattern, reimplemented
// here because this file is package console, not console_test: datastar-go
// writes each event's data lines, then a blank line, then one extra blank
// line ("write double newlines to separate events", sse.go), so a frame's
// content is preceded by zero or more stray blank lines left over from the
// previous frame, skipped before accumulating starts.
func readStreamFrame(t *testing.T, r *bufio.Reader) string {
	t.Helper()

	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		started := false
		for {
			line, err := r.ReadString('\n')
			if !started {
				if line == "\n" && err == nil {
					continue // a stray separator blank line before the frame starts
				}
				started = true
			}
			sb.WriteString(line)
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
			if line == "\n" {
				ch <- result{sb.String(), nil}
				return
			}
		}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read SSE frame: %v", res.err)
		}
		return res.text
	case <-time.After(streamBeatFrameTimeout):
		t.Fatal("timed out waiting for an SSE frame")
		return ""
	}
}

// streamHeartbeatDemoTicket ensures the demo project and ticket exist
// (seed.go's EnsureProject plus ensureDemoTicket) and returns the ticket's
// id, the one fixture the three heartbeat tests below each open a thread
// stream on.
func streamHeartbeatDemoTicket(t *testing.T, c *console) int64 {
	t.Helper()
	projectID, err := c.store.EnsureProject(t.Context(), store.Project{
		Name: demoProjectName, RepoURL: demoProjectRepo, LocalPath: demoProjectPath, Tracker: "github",
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := ensureDemoTicket(t.Context(), c.store, projectID)
	if err != nil {
		t.Fatalf("ensureDemoTicket: %v", err)
	}
	return ticketID
}

// streamHeartbeatSeedGateQuestion seeds one open gate question ("Q5") on
// ticketID (seed.go's seedOneQuestion, with no Publish of its own) and
// returns the question message's id, read back through ListMessages the
// way a caller with no insert-result id to hand would.
func streamHeartbeatSeedGateQuestion(t *testing.T, c *console, ticketID int64) int64 {
	t.Helper()
	if err := seedOneQuestion(t.Context(), c.store, ticketID, "Q5", response.QuestionKindGate); err != nil {
		t.Fatalf("seedOneQuestion: %v", err)
	}
	msgs, err := c.store.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := range msgs {
		if msgs[i].Type == msgTypeQuestion {
			return msgs[i].ID
		}
	}
	t.Fatal("seeded gate question not found in ListMessages")
	return 0
}

// TestStreamHeartbeatRepatchesWithoutAWake is the ticket's own regression
// test (bug: "An open thread stops showing new questions, with no stale
// marker"): with no bus.Publish at all, a question committed for the open
// thread must still reach #main within a couple of heartbeat intervals,
// proving the beat -- not a wake the committer forgot to send -- is what
// repatches the page.
func TestStreamHeartbeatRepatchesWithoutAWake(t *testing.T) {
	c, _ := newStreamTestConsole(t)
	c.streamHeartbeat = 50 * time.Millisecond
	ticketID := streamHeartbeatDemoTicket(t, c)

	srv := streamTestServer(t, c, false)
	resp, r, cancel := openInternalStream(t, srv.URL, viewThread, ticketID)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	for range 4 { // the initial connect: #nav, #main, #rail, #alerts
		readStreamFrame(t, r)
	}

	questionID := streamHeartbeatSeedGateQuestion(t, c, ticketID)
	want := fmt.Sprintf(`id="question-%d"`, questionID)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no frame carrying %s arrived within 2s of a heartbeat-only commit (no bus.Publish)", want)
		}
		frame := readStreamFrame(t, r)
		if strings.Contains(frame, want) {
			return
		}
	}
}

// TestStreamGateQuestionWakePatchesMain is the ticket's "Done when" test
// (scope's own Find-first question: "Does a new question on the open
// thread publish a bus wake that patches #main?"): with the heartbeat off
// (streamHeartbeat 0), a gate question committed for the open ticket,
// followed by one bus.Publish, still produces a #main frame carrying it --
// the server-side wake path reading the code already ruled out as the
// fault, kept here as a guard against it regressing.
func TestStreamGateQuestionWakePatchesMain(t *testing.T) {
	c, _ := newStreamTestConsole(t)
	ticketID := streamHeartbeatDemoTicket(t, c)

	srv := streamTestServer(t, c, false)
	resp, r, cancel := openInternalStream(t, srv.URL, viewThread, ticketID)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	for range 4 {
		readStreamFrame(t, r)
	}

	questionID := streamHeartbeatSeedGateQuestion(t, c, ticketID)
	c.bus.Publish()
	want := fmt.Sprintf(`id="question-%d"`, questionID)

	deadline := time.Now().Add(streamBeatFrameTimeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no frame carrying %s arrived after a gate question commit plus bus.Publish", want)
		}
		frame := readStreamFrame(t, r)
		if strings.Contains(frame, want) {
			return
		}
	}
}

// TestStreamHeartbeatDoesNotMarkRead proves the beat case shares
// patchRegions, not the connect-time MarkThreadRead branch (handleStream
// runs that branch once, before the loop, never inside it): after connect,
// an unread message inserted on the open ticket stays unread through at
// least three heartbeat-only re-renders.
func TestStreamHeartbeatDoesNotMarkRead(t *testing.T) {
	c, _ := newStreamTestConsole(t)
	c.streamHeartbeat = 50 * time.Millisecond
	ticketID := streamHeartbeatDemoTicket(t, c)

	srv := streamTestServer(t, c, false)
	resp, r, cancel := openInternalStream(t, srv.URL, viewThread, ticketID)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	for range 4 {
		readStreamFrame(t, r)
	}

	msgID, err := c.store.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: "zing", Body: "progress",
	})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	for range 3 * 4 { // at least three beats' worth of four-region frames
		readStreamFrame(t, r)
	}

	msg, err := c.store.GetMessage(t.Context(), msgID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.ReadAt != nil {
		t.Error("an unread message was marked read by a heartbeat-only re-render")
	}
}

// TestStreamEndsWhenAFrameWriteTimesOut is the regression test for the
// wedged-write hypothesis (stream.go's prior SetWriteDeadline(time.Time{}),
// no deadline at all): with streamWriteTimeout already in the past (1ns),
// armFrameWriteDeadline must fail the very first region write, and
// handleStream must end the stream rather than hold the connection open
// with nothing left to send. A client reading the body to its end must see
// that end -- EOF or a read error, either way the read returns -- within a
// few seconds, and the body it did get must carry no #main frame.
func TestStreamEndsWhenAFrameWriteTimesOut(t *testing.T) {
	c, _ := newStreamTestConsole(t)
	c.streamWriteTimeout = time.Nanosecond
	ticketID := streamHeartbeatDemoTicket(t, c)

	srv := streamTestServer(t, c, false)

	// doErr and status report the request's own round trip -- did /stream
	// even open -- separately from readErr, the body read's own end (EOF or
	// a read error, either is the timed-out deadline's doing). Folding them
	// into one error, as an earlier version of this test did, let a failed
	// Do (never reaching the server at all) pass silently: an empty body
	// has no #main frame either, but for the wrong reason.
	type result struct {
		doErr   error
		status  int
		body    string
		readErr error
	}
	ch := make(chan result, 1)
	go func() {
		v := url.Values{}
		v.Set("datastar", fmt.Sprintf(`{"view":%q,"open":%d,"project":0}`, viewThread, ticketID))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/stream?"+v.Encode(), http.NoBody)
		if err != nil {
			ch <- result{doErr: err}
			return
		}
		req.Header.Set("Datastar-Request", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- result{doErr: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		ch <- result{status: resp.StatusCode, body: string(body), readErr: err}
	}()

	select {
	case res := <-ch:
		if res.doErr != nil {
			t.Fatalf("GET /stream: %v", res.doErr)
		}
		if res.status != http.StatusOK {
			t.Fatalf("GET /stream: status = %d, want %d", res.status, http.StatusOK)
		}
		if strings.Contains(res.body, `id="main"`) {
			t.Fatalf("body carries a #main frame despite a write deadline already in the past:\n%s", res.body)
		}
		// res.readErr (EOF or a read error) is not checked further: either
		// one means the stream ended, which is what a timed-out write
		// deadline should do.
	case <-time.After(streamBeatFrameTimeout):
		t.Fatal("reading /stream's body did not end within 5s of a timed-out write deadline")
	}
}
