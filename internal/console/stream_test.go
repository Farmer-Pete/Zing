package console_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/response"
	"zing/internal/store"
)

// streamURL builds a GET /stream request url sending view, open, and
// project as the three navigation signals, the way a real @get('/stream')
// call does: JSON-encoded in the "datastar" query parameter (datastar
// skill, go-sdk.md: "Expects signals in URL.Query for GET").
func streamURL(base, view string, open, project int64) string {
	v := url.Values{}
	v.Set("datastar", fmt.Sprintf(`{"view":%q,"open":%d,"project":%d}`, view, open, project))
	return base + "/stream?" + v.Encode()
}

// openStream issues a GET /stream request for (view, open, project) and
// returns the response and a reader over its body, along with the cancel
// func that tears the request down. It does not read any frame yet.
func openStream(t *testing.T, base, view string, open, project int64) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL(base, view, open, project), http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Datastar-Request", "true") // what Datastar's @get sends (mw.go)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /stream: %v", err)
	}
	return resp, bufio.NewReader(resp.Body), cancel
}

// readInitialFrames reads the four frames one /stream connect always sends
// (design section 6.3: "always patch ... regions by id"; design section 6a,
// D8: "#alerts ... patched on every frame"), in the fixed order
// patchRegions writes them: #nav, then #main, then #rail, then #alerts.
func readInitialFrames(t *testing.T, r *bufio.Reader) (nav, main, rail, alerts string) {
	t.Helper()
	nav = readFrame(t, r)
	main = readFrame(t, r)
	rail = readFrame(t, r)
	alerts = readFrame(t, r)
	return nav, main, rail, alerts
}

// newReadTimeoutTestServer is newTestServerSandboxTracker (console_test.go)
// with the underlying http.Server's ReadTimeout set to readTimeout before
// Start, through newTestServerConfig's hook: the same field newServer
// (cmd/zing/serve.go) sets to 10s, only shorter, so a stream opened against
// this server hits the same net/http background-read deadline newServer's
// real stream traffic does, just sooner.
func newReadTimeoutTestServer(t *testing.T, s *store.Store, b *bus.Broker, readTimeout time.Duration) *httptest.Server {
	t.Helper()
	return newTestServerConfig(t, s, b, nil, newTestLogHandler(t), response.SeverityMinor, "", nil, "", func(c *http.Server) {
		c.ReadTimeout = readTimeout
	})
}

// TestStreamOutlivesServerReadTimeout pins the outcome the fix targets: a
// /stream connection stays open and keeps patching past the server's own
// ReadTimeout (200ms here, in place of newServer's 10s, so the test runs in
// under a second). It is not a regression test of handleStream's own
// SetReadDeadline(time.Time{}) call -- the test passes with or without that
// call, because this request is a bodyless GET and go1.27's net/http
// (server.go startBackgroundRead) already clears the read deadline before
// the handler runs -- so it does not reproduce the 19:25:57 cancel (H1
// remains unconfirmed).
func TestStreamOutlivesServerReadTimeout(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	b := bus.New()

	srv := newReadTimeoutTestServer(t, s, b, 200*time.Millisecond)

	resp, r, cancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	readInitialFrames(t, r)

	time.Sleep(500 * time.Millisecond) // past the 200ms ReadTimeout
	b.Publish()

	nav, main, rail, alerts := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav)
	assertExactSSEFraming(t, main)
	assertExactSSEFraming(t, rail)
	assertExactSSEFraming(t, alerts)
	if !strings.Contains(nav, `id="nav"`) {
		t.Errorf("post-ReadTimeout nav frame missing #nav; got:\n%s", nav)
	}
}

// TestStreamEndsCleanlyWhenAFrameFailsToBuild proves a frame that fails to
// build (here, a store read against a closed database) ends the stream with
// a clean EOF rather than a hang or a write error: the client's fetch sees a
// finished event it can reconnect on (design section's "the server half of
// the reconnect contract"). newConsoleTestStore's own t.Cleanup double-closes
// s and ignores that second error, so closing it early here is safe.
func TestStreamEndsCleanlyWhenAFrameFailsToBuild(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	readInitialFrames(t, r)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b.Publish()

	ch := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(r)
		ch <- err
	}()
	select {
	case err := <-ch:
		if err != nil {
			t.Errorf("io.ReadAll(body) after a frame failed to build: %v, want nil (a clean EOF)", err)
		}
	case <-time.After(frameTimeout):
		t.Fatal("stream did not end within frameTimeout after a frame failed to build")
	}
}

func TestStreamPatchesAllFourRegionsOnConnect(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedOpenQuestion(t, s, ticketID)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	nav, main, rail, alerts := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav)
	assertExactSSEFraming(t, main)
	assertExactSSEFraming(t, rail)
	assertExactSSEFraming(t, alerts)

	if !strings.Contains(nav, `id="nav"`) {
		t.Errorf("initial nav frame missing #nav; got:\n%s", nav)
	}
	if !strings.Contains(nav, "Add a hello endpoint") {
		t.Errorf("initial nav frame missing the blocking ticket's title; got:\n%s", nav)
	}
	if !strings.Contains(main, `id="main"`) {
		t.Errorf("initial main frame missing #main; got:\n%s", main)
	}
	if !strings.Contains(rail, `id="rail"`) {
		t.Errorf("initial rail frame missing #rail; got:\n%s", rail)
	}
	if !strings.Contains(alerts, `id="alerts"`) {
		t.Errorf("initial alerts frame missing #alerts; got:\n%s", alerts)
	}
}

func TestStreamReRendersOnPublish(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Ticket one")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "recent", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	nav1, main1, rail1, alerts1 := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav1)
	assertExactSSEFraming(t, main1)
	assertExactSSEFraming(t, rail1)
	assertExactSSEFraming(t, alerts1)
	if !strings.Contains(main1, "Ticket one") {
		t.Errorf("initial main frame (view=recent) missing the seeded ticket; got:\n%s", main1)
	}

	seedTicket(t, s, "fake#2", "Ticket two")
	b.Publish()

	nav2, main2, rail2, alerts2 := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav2)
	assertExactSSEFraming(t, main2)
	assertExactSSEFraming(t, rail2)
	assertExactSSEFraming(t, alerts2)
	if !strings.Contains(main2, "Ticket two") {
		t.Errorf("post-publish main frame missing the newly seeded ticket; got:\n%s", main2)
	}
}

// TestStreamLeavingAThreadPatchesAnEmptyRail proves #rail is always
// patched (design section 6.3, 12): a connection opened on a thread view
// with a real open ticket sees the rail's real content (Task 9, design
// section 6.11), while a connection opened on any other view (or a thread
// with no open ticket) still sees the empty <aside id="rail"></aside> Task
// 3 introduced -- so navigating away from a thread clears whatever the rail
// held before, rather than stranding it.
func TestStreamLeavingAThreadPatchesAnEmptyRail(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	threadResp, threadR, threadCancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer threadCancel()
	defer func() { _ = threadResp.Body.Close() }()
	_, _, threadRail, _ := readInitialFrames(t, threadR)

	inboxResp, inboxR, inboxCancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer inboxCancel()
	defer func() { _ = inboxResp.Body.Close() }()
	_, _, inboxRail, _ := readInitialFrames(t, inboxR)

	if strings.Contains(threadRail, `<aside id="rail"></aside>`) {
		t.Errorf("view=thread rail frame is still the empty placeholder; got:\n%s", threadRail)
	}
	if !strings.Contains(threadRail, `class="rail-artifacts"`) {
		t.Errorf("view=thread rail frame missing its real content; got:\n%s", threadRail)
	}
	if !strings.Contains(inboxRail, `<aside id="rail"></aside>`) {
		t.Errorf("view=inbox rail frame is not the empty placeholder; got:\n%s", inboxRail)
	}
}

// TestStreamOpeningAThreadMarksItRead proves opening a thread (a GET /stream
// with view=thread and open set to a real ticket, carrying Datastar-Request
// as Datastar's @get does) marks every unread message of that ticket read
// in the store before the first frame renders, so neither that thread's own
// nav frame nor a freshly opened inbox stream's nav frame still shows
// badge-unread (design section 6.8). The ticket stays live (queued,
// non-terminal), so #nav still lists its row and title (#106 bug 4); only
// the badge-unread clears.
func TestStreamOpeningAThreadMarksItRead(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Unread thread")
	seedUnreadUpdate(t, s, ticketID, "progress")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	threadResp, threadR, threadCancel := openStream(t, srv.URL, "thread", ticketID, 0)
	threadNav, _, _, _ := readInitialFrames(t, threadR)
	threadCancel()
	_ = threadResp.Body.Close()

	if strings.Contains(threadNav, "badge-unread") {
		t.Errorf("thread-open nav frame still shows badge-unread; got:\n%s", threadNav)
	}

	inboxResp, inboxR, inboxCancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer inboxCancel()
	defer func() { _ = inboxResp.Body.Close() }()
	inboxNav, _, _, _ := readInitialFrames(t, inboxR)

	// The ticket is still live (queued, non-terminal), so #nav still lists
	// it (design section 6.3, 6.8, #106 bug 4); it is the badge-unread, not
	// the row itself, that the mark-read clears.
	if strings.Contains(inboxNav, "badge-unread") {
		t.Errorf("post-open inbox nav frame still shows badge-unread; got:\n%s", inboxNav)
	}
	if !strings.Contains(inboxNav, "Unread thread") {
		t.Errorf("post-open inbox nav frame no longer lists the still-live ticket; got:\n%s", inboxNav)
	}
}

// TestStreamBusWakeDoesNotMarkRead proves a bus wake on an already-open
// stream never marks anything read: only the connect itself does (design
// section 6.8's "not on SSE patches the owner did not cause"). After the
// thread's own open marks its one message read, a second message that
// arrives and wakes the stream through the bus stays unread.
func TestStreamBusWakeDoesNotMarkRead(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Unread thread")
	firstID := seedUnreadUpdate(t, s, ticketID, "first")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	readInitialFrames(t, r) // the open-time mark: the seeded message is now read

	first, err := s.GetMessage(t.Context(), firstID)
	if err != nil {
		t.Fatalf("GetMessage(first): %v", err)
	}
	if first.ReadAt == nil {
		t.Error("the first message's ReadAt is still nil after opening the thread")
	}

	secondID := seedUnreadUpdate(t, s, ticketID, "second")
	b.Publish()
	readInitialFrames(t, r) // the wake's own re-render; it must not mark the new message

	second, err := s.GetMessage(t.Context(), secondID)
	if err != nil {
		t.Fatalf("GetMessage(second): %v", err)
	}
	if second.ReadAt != nil {
		t.Error("the second message's ReadAt is set after a bus wake; only the open itself should mark")
	}

	items, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}
	if !inboxHasTicket(items, ticketID) {
		t.Error("InboxItems no longer lists the ticket after a bus wake; the second message should still be unread")
	}
}

// TestStreamOpeningAReadThreadPublishesNothing proves the open-time mark
// publishes only when it actually marked a row: opening a thread with no
// unread messages costs an already-open stream no extra frame (design
// section 6.8's "an already-read thread ... costs no extra frames").
func TestStreamOpeningAReadThreadPublishesNothing(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Already read")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	respA, readerA, cancelA := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancelA()
	defer func() { _ = respA.Body.Close() }()
	readInitialFrames(t, readerA)

	threadResp, threadR, threadCancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer threadCancel()
	defer func() { _ = threadResp.Body.Close() }()
	readInitialFrames(t, threadR)

	expectNoMoreFrames(t, readerA, 300*time.Millisecond)
}

// TestStreamOpenWithoutDatastarHeaderMarksNothing proves the open-time mark
// is gated behind the same Datastar-Request signal the mutation guard
// trusts (design section 6.8's goal 2): a GET /stream that lacks that
// header, or that carries a cross-site Sec-Fetch-Site, still renders but
// marks nothing, because GET /stream itself sits behind only the
// Host-allowlist guard.
func TestStreamOpenWithoutDatastarHeaderMarksNothing(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Unread thread")
	seedUnreadUpdate(t, s, ticketID, "progress")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	// No Datastar-Request header at all.
	ctx1, cancel1 := context.WithCancel(t.Context())
	defer cancel1()
	req1, err := http.NewRequestWithContext(ctx1, http.MethodGet, streamURL(srv.URL, "thread", ticketID, 0), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp1.Body.Close() }()
	readInitialFrames(t, bufio.NewReader(resp1.Body))

	items, err := s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}
	if !inboxHasTicket(items, ticketID) {
		t.Error("InboxItems no longer lists the ticket after an open with no Datastar-Request header")
	}

	// Datastar-Request set, but Sec-Fetch-Site is cross-site.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	req2, err := http.NewRequestWithContext(ctx2, http.MethodGet, streamURL(srv.URL, "thread", ticketID, 0), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req2.Header.Set("Datastar-Request", "true")
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	readInitialFrames(t, bufio.NewReader(resp2.Body))

	items, err = s.InboxItems(t.Context(), nil)
	if err != nil {
		t.Fatalf("InboxItems: %v", err)
	}
	if !inboxHasTicket(items, ticketID) {
		t.Error("InboxItems no longer lists the ticket after an open with Sec-Fetch-Site: cross-site")
	}
}

// TestStreamTerminalTicketNeverShowsUnread proves a ticket in a terminal
// state never shows "unread" (design section 6.8's goal "a ticket in a
// terminal state never shows unread"), even carrying unread messages and
// even though nothing ever opened its thread: navComponent and
// inboxComponent pass the real machine's terminal list (machine.toml's
// "done", "escalated", "abandoned") to InboxItems, so a done ticket drops
// out of both #nav's badge list and the Inbox view.
func TestStreamTerminalTicketNeverShowsUnread(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "done#1", "Finished ticket")
	seedUnreadUpdate(t, s, ticketID, "first")
	seedUnreadUpdate(t, s, ticketID, "second")

	const owner = "test-owner"
	expires := time.Now().Add(10 * time.Minute)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: string(response.TicketStateDone), Reason: "test: finished",
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	nav, main, _, _ := readInitialFrames(t, r)

	if strings.Contains(nav, "Finished ticket") {
		t.Errorf("nav frame still lists the done ticket; got:\n%s", nav)
	}
	if strings.Contains(main, "Finished ticket") {
		t.Errorf("inbox main frame still lists the done ticket; got:\n%s", main)
	}
}

// inboxHasTicket reports whether items contains ticketID.
func inboxHasTicket(items []store.InboxItem, ticketID int64) bool {
	for i := range items {
		if items[i].Ticket.ID == ticketID {
			return true
		}
	}
	return false
}

// TestStreamOpeningAMissingTicketMarksNothing proves an open id that names
// no ticket renders the empty thread view, marks nothing, and publishes
// nothing (design section 6.8's goal 3): MarkThreadRead's UPDATE matches no
// row, so it returns 0 and the open-time publish never fires.
func TestStreamOpeningAMissingTicketMarksNothing(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	otherID := seedTicket(t, s, "fake#1", "Some other ticket")
	otherMsgID := seedUnreadUpdate(t, s, otherID, "progress")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	respA, readerA, cancelA := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancelA()
	defer func() { _ = respA.Body.Close() }()
	readInitialFrames(t, readerA)

	threadResp, threadR, threadCancel := openStream(t, srv.URL, "thread", 999999, 0)
	defer threadCancel()
	defer func() { _ = threadResp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, threadR)
	if !strings.Contains(main, "Select a ticket.") {
		t.Errorf("missing-ticket thread open main frame is not the empty thread view; got:\n%s", main)
	}

	otherMsg, err := s.GetMessage(t.Context(), otherMsgID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if otherMsg.ReadAt != nil {
		t.Error("the unrelated ticket's message was marked read by an open naming a missing ticket")
	}

	expectNoMoreFrames(t, readerA, 300*time.Millisecond)
}

// TestStreamDisconnectUnsubscribesWithNoGoroutineLeak proves the loop exits
// on r.Context().Done() alone (Task 1 reconciliation, design section 14:
// bus.Subscribe's cancel does not close the channel) and that its deferred
// cancel actually deregisters the subscriber: a client that goes away lets
// the handler goroutine exit, and a publish afterward reaches a fresh
// subscriber promptly, proving the departed one is really gone rather than
// still holding a slot.
func TestStreamDisconnectUnsubscribesWithNoGoroutineLeak(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	// The baseline is taken before the stream opens, not after: a count
	// taken while the handler is already alive can never show that
	// handler's own exit bringing the count back down, so it would prove
	// nothing about the disconnect below.
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL(srv.URL, "inbox", 0, 0), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}

	r := bufio.NewReader(resp.Body)
	readInitialFrames(t, r) // the initial patch, proving the stream is live

	cancel() // simulate the client navigating away or Datastar aborting the request
	_ = resp.Body.Close()

	// Poll, bounded, instead of a fixed sleep: this is how the test
	// synchronizes on the handler's observable unsubscribe (the bus itself
	// exposes no subscriber count to read directly), rather than sleeping a
	// fixed guess and hoping the handler was done by then.
	deadline := time.Now().Add(frameTimeout)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Errorf("goroutine count did not settle back to the pre-stream baseline: got %d, baseline %d",
				runtime.NumGoroutine(), baseline)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A publish after disconnect must not panic or block on the departed
	// subscriber; a second, fresh subscriber must still see it promptly.
	ch, unsub := b.Subscribe()
	defer unsub()
	b.Publish()
	select {
	case <-ch:
	case <-time.After(frameTimeout):
		t.Fatal("a fresh subscriber saw no publish after the prior client disconnected")
	}
}

// TestStreamRapidReopenLeavesOneSubscriber models Datastar's
// requestCancellation default: a rapid navigation change aborts the prior
// /stream fetch on #stream-ctl before the next one starts (design section
// 6.3). It opens a second stream while the first is still live, then
// cancels the first's context (the abort) and proves that specific
// handler returns, leaving the second as the one live subscriber: a
// publish reaches it, and the goroutine count settles back to "only the
// second stream's handler still running" rather than "both still running."
func TestStreamRapidReopenLeavesOneSubscriber(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := newTestServer(t, s, b, nil, newTestLogHandler(t))

	resp1, r1, cancel1 := openStream(t, srv.URL, "inbox", 0, 0)
	nav1, main1, rail1, alerts1 := readInitialFrames(t, r1)
	assertExactSSEFraming(t, nav1)
	assertExactSSEFraming(t, main1)
	assertExactSSEFraming(t, rail1)
	assertExactSSEFraming(t, alerts1)

	resp2, r2, cancel2 := openStream(t, srv.URL, "recent", 0, 0)
	defer cancel2()
	defer func() { _ = resp2.Body.Close() }()
	nav2, main2, rail2, alerts2 := readInitialFrames(t, r2)
	assertExactSSEFraming(t, nav2)
	assertExactSSEFraming(t, main2)
	assertExactSSEFraming(t, rail2)
	assertExactSSEFraming(t, alerts2)

	// The count with both streams live and confirmed (each has delivered
	// its initial frames) is the reference point for the cancellation
	// below: an absolute baseline taken before either connection opened
	// would also count each connection's own client-transport goroutines,
	// per-connection overhead rather than evidence of a leaked handler.
	afterBoth := runtime.NumGoroutine()

	cancel1()
	_ = resp1.Body.Close()

	deadline := time.Now().Add(frameTimeout)
	for runtime.NumGoroutine() >= afterBoth {
		if time.Now().After(deadline) {
			t.Fatalf("the first stream's handler did not return after cancellation: goroutines = %d, want < %d (the count with both streams live)",
				runtime.NumGoroutine(), afterBoth)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The second stream is the one live subscriber left: a publish must
	// still reach it promptly, framed exactly like any other patch set.
	b.Publish()
	nav3, main3, rail3, alerts3 := readInitialFrames(t, r2)
	assertExactSSEFraming(t, nav3)
	assertExactSSEFraming(t, main3)
	assertExactSSEFraming(t, rail3)
	assertExactSSEFraming(t, alerts3)
}

func TestStreamRejectsMalformedSignalsWith400(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	//nolint:noctx // a bare GET on a test server needs no deadline
	resp, err := http.Get(srv.URL + "/stream?datastar=" + url.QueryEscape("{not valid json"))
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /stream with malformed signals: status = %d, want 400", resp.StatusCode)
	}
}

// noCascadeWait bounds how long TestStreamAlertsPatchesOnceOnAWarningNoCascade
// waits to prove absence, not presence: a real cascade (onWarn firing again
// from inside the very patch it caused) would re-publish synchronously, in
// the same call stack that already delivered the expected patch set, so it
// would arrive within milliseconds, not merely be slow. This bound is short
// on purpose, to keep the test fast; it would only mask a cascade that
// somehow took longer than this to occur, which the mechanism here (a
// synchronous bus.Publish inside Handle) does not produce.
const noCascadeWait = 300 * time.Millisecond

// newTestLogHandlerWithBus builds a console.Handler wired the same way
// cmd/zing's installLogHandler wires the real one (design section 6a):
// onWarn is b.Publish, so a WARN-or-above record wakes every open /stream
// subscriber through the same bus the console itself publishes store
// changes on.
func newTestLogHandlerWithBus(t *testing.T, b *bus.Broker) *console.Handler {
	t.Helper()
	return console.NewHandler(io.Discard, new(slog.LevelVar), b.Publish)
}

// expectNoMoreFrames fails the test if another SSE frame arrives on r
// within wait. It reads in its own goroutine, exactly like readFrame,
// skipping the same stray leading blank separator line readFrame's own
// doc comment explains (datastar-go's sse.go writes a double newline
// between events, so one blank line is always left over from the prior
// frame and must not itself be mistaken for a new one). A genuine absence
// just lets that goroutine block until the test's deferred cancel tears
// the connection down.
func expectNoMoreFrames(t *testing.T, r *bufio.Reader, wait time.Duration) {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		var sb strings.Builder
		started := false
		for {
			line, err := r.ReadString('\n')
			if !started {
				if line == "\n" && err == nil {
					continue // the stray separator blank line before any real frame
				}
				started = true
			}
			sb.WriteString(line)
			if err != nil {
				return
			}
			if line == "\n" {
				ch <- sb.String()
				return
			}
		}
	}()
	select {
	case frame := <-ch:
		t.Errorf("unexpected extra SSE frame (a cascade): %q", frame)
	case <-time.After(wait):
	}
}

// TestStreamAlertsPatchesOnceOnAWarningNoCascade is the plan's own
// no-cascade test (design section 6a, 8, 14: "a single warning produces one
// stream frame, not a cascade"): logging one WARN wakes the one open
// /stream subscriber through onWarn -> bus.Publish, the very next patch set
// carries that warning in #alerts, and nothing further arrives afterward.
// A cascade would mean the alerts render itself logged at warn (it does
// not) or a patch failure's own slog.Warn re-triggered the wake loop on a
// healthy connection (it should not, since every patch here succeeds).
func TestStreamAlertsPatchesOnceOnAWarningNoCascade(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	b := bus.New()
	log := newTestLogHandlerWithBus(t, b)

	srv := newTestServer(t, s, b, nil, log)

	resp, r, cancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	_, _, _, alerts1 := readInitialFrames(t, r)
	assertExactSSEFraming(t, alerts1)
	if strings.Contains(alerts1, "a live warning reaches the alerts view") {
		t.Fatalf("initial alerts frame already carries the warning; got:\n%s", alerts1)
	}

	slog.New(log).Warn("a live warning reaches the alerts view")

	nav2, main2, rail2, alerts2 := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav2)
	assertExactSSEFraming(t, main2)
	assertExactSSEFraming(t, rail2)
	assertExactSSEFraming(t, alerts2)
	if !strings.Contains(alerts2, "a live warning reaches the alerts view") {
		t.Errorf("post-warning alerts frame missing the warning; got:\n%s", alerts2)
	}
	if !strings.Contains(alerts2, "WARN") {
		t.Errorf("post-warning alerts frame missing its level; got:\n%s", alerts2)
	}

	expectNoMoreFrames(t, r, noCascadeWait)
}
