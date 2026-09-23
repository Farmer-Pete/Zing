package console_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /stream: %v", err)
	}
	return resp, bufio.NewReader(resp.Body), cancel
}

// readInitialFrames reads the three frames one /stream connect always sends
// (design section 6.3: "always patch three regions by id"), in the fixed
// order patchRegions writes them: #nav, then #main, then #rail.
func readInitialFrames(t *testing.T, r *bufio.Reader) (nav, main, rail string) {
	t.Helper()
	nav = readFrame(t, r)
	main = readFrame(t, r)
	rail = readFrame(t, r)
	return nav, main, rail
}

func TestStreamPatchesAllThreeRegionsOnConnect(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedOpenQuestion(t, s, ticketID)

	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

	resp, r, cancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	nav, main, rail := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav)
	assertExactSSEFraming(t, main)
	assertExactSSEFraming(t, rail)

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
}

func TestStreamReRendersOnPublish(t *testing.T) {
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Ticket one")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b, nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

	resp, r, cancel := openStream(t, srv.URL, "recent", 0, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	nav1, main1, rail1 := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav1)
	assertExactSSEFraming(t, main1)
	assertExactSSEFraming(t, rail1)
	if !strings.Contains(main1, "Ticket one") {
		t.Errorf("initial main frame (view=recent) missing the seeded ticket; got:\n%s", main1)
	}

	seedTicket(t, s, "fake#2", "Ticket two")
	b.Publish()

	nav2, main2, rail2 := readInitialFrames(t, r)
	assertExactSSEFraming(t, nav2)
	assertExactSSEFraming(t, main2)
	assertExactSSEFraming(t, rail2)
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
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

	threadResp, threadR, threadCancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer threadCancel()
	defer func() { _ = threadResp.Body.Close() }()
	_, _, threadRail := readInitialFrames(t, threadR)

	inboxResp, inboxR, inboxCancel := openStream(t, srv.URL, "inbox", 0, 0)
	defer inboxCancel()
	defer func() { _ = inboxResp.Body.Close() }()
	_, _, inboxRail := readInitialFrames(t, inboxR)

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

// TestStreamDisconnectUnsubscribesWithNoGoroutineLeak proves the loop exits
// on r.Context().Done() alone (Task 1 reconciliation, design section 14:
// bus.Subscribe's cancel does not close the channel) and that its deferred
// cancel actually deregisters the subscriber: a client that goes away lets
// the handler goroutine exit, and a publish afterward reaches a fresh
// subscriber promptly, proving the departed one is really gone rather than
// still holding a slot.
func TestStreamDisconnectUnsubscribesWithNoGoroutineLeak(t *testing.T) {
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b, nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

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
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b, nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

	resp1, r1, cancel1 := openStream(t, srv.URL, "inbox", 0, 0)
	nav1, main1, rail1 := readInitialFrames(t, r1)
	assertExactSSEFraming(t, nav1)
	assertExactSSEFraming(t, main1)
	assertExactSSEFraming(t, rail1)

	resp2, r2, cancel2 := openStream(t, srv.URL, "recent", 0, 0)
	defer cancel2()
	defer func() { _ = resp2.Body.Close() }()
	nav2, main2, rail2 := readInitialFrames(t, r2)
	assertExactSSEFraming(t, nav2)
	assertExactSSEFraming(t, main2)
	assertExactSSEFraming(t, rail2)

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
	nav3, main3, rail3 := readInitialFrames(t, r2)
	assertExactSSEFraming(t, nav3)
	assertExactSSEFraming(t, main3)
	assertExactSSEFraming(t, rail3)
}

func TestStreamRejectsMalformedSignalsWith400(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

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
