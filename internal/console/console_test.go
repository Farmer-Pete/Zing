package console_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/store"
)

// frameTimeout bounds every SSE read in this file: long enough for a slow
// CI box, short enough that a hung stream fails the test instead of the
// suite.
const frameTimeout = 5 * time.Second

var testProject = store.Project{
	Name: "zing", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: "github",
}

// newConsoleTestStore opens a fresh Store on a temp-file database, closed on
// test cleanup, the same pattern internal/dispatch's tests use.
func newConsoleTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedTicket inserts one queued ticket under ref and returns its id.
func seedTicket(t *testing.T, s *store.Store, ref, title string) int64 {
	t.Helper()
	projectID, err := s.EnsureProject(t.Context(), testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: title, State: "queued",
	})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	return id
}

// seedStateMessage inserts one "state" message on ticketID, the same shape
// commit.go writes on every transition (design section 6.3).
func seedStateMessage(t *testing.T, s *store.Store, ticketID int64, from, to, reason string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"from": from, "to": to, "reason": reason})
	if err != nil {
		t.Fatalf("marshal state payload: %v", err)
	}
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: "state", Author: "system", Payload: payload,
	}); err != nil {
		t.Fatalf("InsertMessage(state): %v", err)
	}
}

// seedOpenQuestion claims ticketID and commits one open "question" message
// with a two-option payload, the run-less shape a first-entry planning
// commit writes before a session exists (design section 6.3, section 6.6):
// a heading and body text in Body, and a validated QuestionPayload with
// options "a" and "b". It also sets the ticket's waiting_on to "questions",
// so a test can assert POST /answer clears it. It returns the question
// message's id.
func seedOpenQuestion(t *testing.T, s *store.Store, ticketID int64) int64 {
	t.Helper()

	const owner = "test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	waiting := "questions"
	openState := "open"
	payload := []byte(`{"key":"Q1","kind":"question","state":"open","recommended":"a",` +
		`"options":[{"key":"a","text":"Plain hello"},{"key":"b","text":"hello, world"}]}`)

	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: &waiting,
		Messages: []store.Message{{
			TicketID: ticketID, Type: "question", Author: "zing",
			State:   &openState,
			Body:    "How should the greeting read?\n\nPick the greeting style for GET /hello.",
			Payload: payload,
		}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	messages, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := range messages {
		if messages[i].Type == "question" {
			return messages[i].ID
		}
	}
	t.Fatal("seedOpenQuestion: no question message found after commit")
	return 0
}

// threadURL builds a GET /thread request url sending open as the $open
// signal, the way a real @get('/thread') call does: JSON-encoded in the
// "datastar" query parameter, since /thread is a single STABLE url and
// never carries a per-ticket "id" query parameter (design section 6.9,
// datastar skill go-sdk.md: "Expects signals in URL.Query for GET").
func threadURL(base string, open int64) string {
	v := url.Values{}
	v.Set("datastar", fmt.Sprintf(`{"open":%d}`, open))
	return base + "/thread?" + v.Encode()
}

// postAnswer POSTs body to base+"/answer" with the Content-Type and
// Datastar-Request headers a real Datastar @post('/answer') call always
// sends (design section "Console" fix 2: requireSameOrigin rejects a
// same-origin POST that lacks the Datastar-Request header, so every test
// that expects a real answer-handling response, not a 403, must set it).
func postAnswer(t *testing.T, base, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/answer", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST /answer request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /answer: %v", err)
	}
	return resp
}

// readFrame reads one SSE frame from r, bounded by frameTimeout so a hung
// stream fails fast instead of hanging the test suite. The datastar-go SDK
// writes each event's data lines, then a blank line, then one extra blank
// line ("write double newlines to separate events", sse.go), so a frame's
// content is preceded by zero or more stray blank lines left over from the
// previous frame; readFrame skips those before it starts accumulating.
func readFrame(t *testing.T, r *bufio.Reader) string {
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
	case <-time.After(frameTimeout):
		t.Fatal("timed out waiting for an SSE frame")
		return ""
	}
}

// sseFrameRe is the exact shape one datastar-go PatchElements frame takes
// (no selector, mode, namespace, or event id set, matching every patch this
// console sends): "event: datastar-patch-elements\n", one or more
// "data: elements <line>\n" rows (one per line of the rendered fragment),
// then the blank line readFrame stops at (datastar-go's sse.go: each event
// ends with a "double newline", of which the first belongs to the last data
// line and the second is this trailing blank line).
var sseFrameRe = regexp.MustCompile(`^event: datastar-patch-elements\n(?:data: elements[^\n]*\n)+\n$`)

// assertExactSSEFraming fails the test unless frame is exactly one
// datastar-patch-elements event in the SDK's own wire shape, not just a
// loose substring match (design section 11, "console").
func assertExactSSEFraming(t *testing.T, frame string) {
	t.Helper()
	if !sseFrameRe.MatchString(frame) {
		t.Errorf("SSE frame framing mismatch (want \"event: datastar-patch-elements\\n\" "+
			"then one or more \"data: elements ...\\n\" lines then a blank line); got:\n%q", frame)
	}
}

func TestIndexRendersTicketListAndScript(t *testing.T) {
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/") //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	if !strings.Contains(got, "Add a hello endpoint") {
		t.Errorf("GET / body missing the ticket title; got:\n%s", got)
	}
	if !strings.Contains(got, `id="tickets"`) {
		t.Errorf("GET / body missing #tickets; got:\n%s", got)
	}
	if !strings.Contains(got, `<script type="module" src="/static/datastar.js">`) {
		t.Errorf("GET / body missing the datastar script tag; got:\n%s", got)
	}
	if !strings.Contains(got, `<meta name="viewport" content="width=device-width, initial-scale=1">`) {
		t.Errorf("GET / body missing the responsive viewport meta tag; got:\n%s", got)
	}
}

func TestUpdatesStreamsInitialFrameAndOnPublish(t *testing.T) {
	s := newConsoleTestStore(t)
	seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/updates", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /updates: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	r := bufio.NewReader(resp.Body)
	first := readFrame(t, r)
	if !strings.Contains(first, "datastar-patch-elements") {
		t.Errorf("initial /updates frame missing datastar-patch-elements; got:\n%s", first)
	}
	if !strings.Contains(first, `id="tickets"`) {
		t.Errorf("initial /updates frame missing #tickets; got:\n%s", first)
	}
	if !strings.Contains(first, "Add a hello endpoint") {
		t.Errorf("initial /updates frame missing the ticket title; got:\n%s", first)
	}

	seedTicket(t, s, "fake#2", "Second ticket")
	b.Publish()

	second := readFrame(t, r)
	if !strings.Contains(second, "datastar-patch-elements") {
		t.Errorf("second /updates frame missing datastar-patch-elements; got:\n%s", second)
	}
	if !strings.Contains(second, "Second ticket") {
		t.Errorf("second /updates frame missing the new ticket; got:\n%s", second)
	}
}

func TestThreadStreamPatchesTicketMessages(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedStateMessage(t, s, ticketID, "queued", "planning", "picked up")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, threadURL(srv.URL, ticketID), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /thread: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	r := bufio.NewReader(resp.Body)
	first := readFrame(t, r)
	if !strings.Contains(first, "datastar-patch-elements") {
		t.Errorf("initial /thread frame missing datastar-patch-elements; got:\n%s", first)
	}
	if !strings.Contains(first, `id="thread"`) {
		t.Errorf("initial /thread frame missing #thread; got:\n%s", first)
	}
	if !strings.Contains(first, "state") || !strings.Contains(first, "queued") || !strings.Contains(first, "planning") {
		t.Errorf("initial /thread frame missing the state message; got:\n%s", first)
	}
}

func TestThreadStreamUnsubscribesOnDisconnect(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b))
	defer srv.Close()

	// The baseline is taken before the stream opens, not after: a count
	// taken while the handler is already alive can never show that
	// handler's own exit bringing the count back down, so it would prove
	// nothing about the disconnect below.
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(t.Context())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, threadURL(srv.URL, ticketID), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /thread: %v", err)
	}

	r := bufio.NewReader(resp.Body)
	readFrame(t, r) // the initial patch, proving the stream is live

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

// TestThreadStreamRapidReopenLeavesOneSubscriber models Datastar's
// requestCancellation default: a rapid $open replacement aborts the prior
// /thread fetch on the same element before the next one starts (design
// section 6.9). It opens a second stream for the same ticket while the
// first is still live, then cancels the first's context (the abort) and
// proves that specific handler returns, leaving the second as the one live
// subscriber: a publish reaches it, and the goroutine count settles back to
// "only the second stream's handler still running" rather than "both
// still running."
func TestThreadStreamRapidReopenLeavesOneSubscriber(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b))
	defer srv.Close()

	ctx1, cancel1 := context.WithCancel(t.Context())
	req1, err := http.NewRequestWithContext(ctx1, http.MethodGet, threadURL(srv.URL, ticketID), http.NoBody)
	if err != nil {
		t.Fatalf("new request 1: %v", err)
	}
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("GET /thread (1): %v", err)
	}
	r1 := bufio.NewReader(resp1.Body)
	assertExactSSEFraming(t, readFrame(t, r1))

	// The second stream opens for the same ticket while the first is still
	// live, the way a fresh /thread fetch briefly overlaps the request it is
	// about to cancel.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	req2, err := http.NewRequestWithContext(ctx2, http.MethodGet, threadURL(srv.URL, ticketID), http.NoBody)
	if err != nil {
		t.Fatalf("new request 2: %v", err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET /thread (2): %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	r2 := bufio.NewReader(resp2.Body)
	assertExactSSEFraming(t, readFrame(t, r2))

	// The count with both streams live and confirmed (each has delivered
	// its initial frame) is the reference point for the cancellation
	// below: an absolute baseline taken before either connection opened
	// would also count each connection's own client-transport goroutines
	// (its persistConn readLoop and writeLoop), which are per-connection
	// overhead, not evidence of a leaked stream handler. Comparing against
	// this "both open" count isolates exactly what cancelling the first
	// connection should remove.
	afterBoth := runtime.NumGoroutine()

	// Cancel the first request, Datastar's own abort of the prior fetch,
	// and prove that specific handler returns rather than lingering
	// alongside the second: the count must drop below afterBoth once its
	// handler (and that connection's own transport goroutines) exit.
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
	// still reach it promptly, framed exactly like any other patch.
	b.Publish()
	assertExactSSEFraming(t, readFrame(t, r2))
}

// TestThreadStreamSwitchingTicketAbortsPriorStream proves the stable-url fix
// (design section 6.9, "Console" fix 3) holds across a genuine ticket
// switch, not just a same-ticket reopen: /thread is one constant url for
// every ticket, so Datastar's requestCancellation (keyed by method and url
// alone, never by the $open signal value) aborts ticket A's in-flight fetch
// the moment $open changes to ticket B, even though the two are different
// tickets. This test drives that same abort explicitly at the HTTP layer and
// proves the server side of it: ticket A's handler returns once its
// connection is cancelled, and ticket B's stream, opened on the same
// "/thread" url with a different $open value, is the one live subscriber
// left, patched with ticket B's own content.
func TestThreadStreamSwitchingTicketAbortsPriorStream(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketA := seedTicket(t, s, "fake#1", "Ticket A")
	ticketB := seedTicket(t, s, "fake#2", "Ticket B")
	b := bus.New()

	srv := httptest.NewServer(console.New(s, b))
	defer srv.Close()

	ctx1, cancel1 := context.WithCancel(t.Context())
	req1, err := http.NewRequestWithContext(ctx1, http.MethodGet, threadURL(srv.URL, ticketA), http.NoBody)
	if err != nil {
		t.Fatalf("new request (ticket A): %v", err)
	}
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("GET /thread (ticket A): %v", err)
	}
	r1 := bufio.NewReader(resp1.Body)
	assertExactSSEFraming(t, readFrame(t, r1))

	// The second stream opens for a DIFFERENT ticket, still on the same
	// "/thread" url, while the first is still live -- the way a real $open
	// change from ticket A to ticket B briefly overlaps the fetch it is
	// about to cancel.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	req2, err := http.NewRequestWithContext(ctx2, http.MethodGet, threadURL(srv.URL, ticketB), http.NoBody)
	if err != nil {
		t.Fatalf("new request (ticket B): %v", err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET /thread (ticket B): %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	r2 := bufio.NewReader(resp2.Body)
	frame2 := readFrame(t, r2)
	assertExactSSEFraming(t, frame2)
	if !strings.Contains(frame2, "Ticket B") {
		t.Errorf("ticket B stream's initial frame missing its own title; got:\n%s", frame2)
	}

	afterBoth := runtime.NumGoroutine()

	// Cancel the first request, the abort a real $open change to ticket B
	// would trigger client-side, and prove that specific handler (ticket
	// A's) returns rather than lingering alongside ticket B's.
	cancel1()
	_ = resp1.Body.Close()

	deadline := time.Now().Add(frameTimeout)
	for runtime.NumGoroutine() >= afterBoth {
		if time.Now().After(deadline) {
			t.Fatalf("ticket A's stream handler did not return after cancellation: goroutines = %d, want < %d (the count with both streams live)",
				runtime.NumGoroutine(), afterBoth)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Ticket B's stream is the one live subscriber left: a publish must
	// reach it, framed with ticket B's own content, never a stale patch left
	// over from the cancelled ticket-A stream.
	b.Publish()
	frame3 := readFrame(t, r2)
	assertExactSSEFraming(t, frame3)
	if !strings.Contains(frame3, "Ticket B") {
		t.Errorf("post-publish frame on the surviving (ticket B) stream missing its title; got:\n%s", frame3)
	}
}

func TestStaticServesDatastarBundle(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/datastar.js") //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /static/datastar.js: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/datastar.js status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != testContentTypeJS {
		t.Errorf("Content-Type = %q, want %s", ct, testContentTypeJS)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		t.Error("GET /static/datastar.js returned an empty body")
	}
}

func TestThreadStreamRendersOpenQuestionBlock(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, threadURL(srv.URL, ticketID), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /thread: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	r := bufio.NewReader(resp.Body)
	frame := readFrame(t, r)

	qBlock := `id="q-` + strconv.FormatInt(questionID, 10) + `"`
	if !strings.Contains(frame, qBlock) {
		t.Errorf("thread frame missing the question block %s; got:\n%s", qBlock, frame)
	}
	if !strings.Contains(frame, "How should the greeting read?") {
		t.Errorf("thread frame missing the question title; got:\n%s", frame)
	}
	if !strings.Contains(frame, "Plain hello") || !strings.Contains(frame, "hello, world") {
		t.Errorf("thread frame missing both option chips; got:\n%s", frame)
	}
	// templ (Package 4) builds this attribute value through a Go expression
	// (fmt.Sprintf, in templates/thread.templ), and every Go-expression
	// attribute value passes through templ's runtime.EscapeString, which is
	// plain html.EscapeString: it HTML-entity-encodes the single quotes
	// around the option key ('a' becomes &#39;a&#39;), unlike a static
	// template literal (the Send button below), which passes through
	// untouched. A browser decodes the entity back to a literal quote while
	// parsing the attribute into the DOM, so Datastar reads the same
	// $answer expression either way; match both spellings rather than
	// assume one escaping style.
	wantChipA := regexp.MustCompile(
		`\$answer = \{ticket:\s*` + strconv.FormatInt(ticketID, 10) +
			`\s*,\s*question:\s*` + strconv.FormatInt(questionID, 10) + `\s*,\s*option: (?:'a'|&#39;a&#39;)\}`)
	if !wantChipA.MatchString(frame) {
		t.Errorf("thread frame missing option a's click signal matching %s; got:\n%s", wantChipA, frame)
	}
	if !strings.Contains(frame, "@post('/answer')") || !strings.Contains(frame, ">Send<") {
		t.Errorf("thread frame missing the Send button; got:\n%s", frame)
	}
}

func TestAnswerAcceptsThenConflictsOnRepeat(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	body := `{"answer":{"ticket":` + strconv.FormatInt(ticketID, 10) +
		`,"question":` + strconv.FormatInt(questionID, 10) + `,"option":"a"}}`

	resp := postAnswer(t, srv.URL, body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first POST /answer status = %d, want 204", resp.StatusCode)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket WaitingOn = %q, want nil (wait cleared)", *ticket.WaitingOn)
	}

	answered, err := s.GetMessage(t.Context(), questionID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if answered.State == nil || *answered.State != "answered" {
		t.Errorf("question state after the first answer = %v, want \"answered\"", answered.State)
	}

	resp2 := postAnswer(t, srv.URL, body)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second POST /answer status = %d, want 409", resp2.StatusCode)
	}
	respBody, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("read second response body: %v", err)
	}
	if !strings.Contains(string(respBody), "already answered") {
		t.Errorf("second POST /answer body = %q, want it to contain %q", respBody, "already answered")
	}

	stillAnswered, err := s.GetMessage(t.Context(), questionID)
	if err != nil {
		t.Fatalf("GetMessage after repeat: %v", err)
	}
	if stillAnswered.State == nil || *stillAnswered.State != "answered" {
		t.Errorf("question state after the repeat = %v, want unchanged \"answered\"", stillAnswered.State)
	}
}

// TestAnswerRejectsInvalidInputWith400 proves handleAnswer validates
// $answer before it ever reaches the store: a non-positive ticket or
// question id, or an option that is not a single lowercase letter, all
// return 400 (design section "Console" fix 1) without needing any seeded
// question, since validation runs first.
func TestAnswerRejectsInvalidInputWith400(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	cases := []struct {
		name, body string
	}{
		{"zero ticket", `{"answer":{"ticket":0,"question":1,"option":"a"}}`},
		{"negative ticket", `{"answer":{"ticket":-1,"question":1,"option":"a"}}`},
		{"zero question", `{"answer":{"ticket":1,"question":0,"option":"a"}}`},
		{"negative question", `{"answer":{"ticket":1,"question":-1,"option":"a"}}`},
		{"empty option", `{"answer":{"ticket":1,"question":1,"option":""}}`},
		{"uppercase option", `{"answer":{"ticket":1,"question":1,"option":"A"}}`},
		{"multi-letter option", `{"answer":{"ticket":1,"question":1,"option":"ab"}}`},
		{"digit option", `{"answer":{"ticket":1,"question":1,"option":"1"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postAnswer(t, srv.URL, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("POST /answer(%s) status = %d, want 400", tc.body, resp.StatusCode)
			}
		})
	}
}

// TestAnswerReturns500WithGenericBodyOnStoreError proves a real store error
// (not a named conflict) never leaks its detail to the client: a question
// id that passes validation but names no real message makes
// store.AnswerQuestion return a genuine error (a wrapped sql.ErrNoRows,
// not an AnswerResult conflict), and handleAnswer must turn that into 500
// with the fixed generic body, logging the detail server-side instead
// (design section "Console" fix 1).
func TestAnswerReturns500WithGenericBodyOnStoreError(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	const missingQuestionID = 999999
	body := fmt.Sprintf(`{"answer":{"ticket":%d,"question":%d,"option":"a"}}`, ticketID, missingQuestionID)

	resp := postAnswer(t, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST /answer status = %d, want 500", resp.StatusCode)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	got := strings.TrimSpace(string(respBody))
	if got != "internal error" {
		t.Errorf(`POST /answer body = %q, want exactly "internal error"`, got)
	}
	if strings.Contains(got, "sql") || strings.Contains(got, strconv.Itoa(missingQuestionID)) ||
		strings.Contains(got, "answer question") {
		t.Errorf("POST /answer body leaked store error detail: %q", got)
	}
}

// TestIndexReturns500WithGenericBodyOnStoreError proves a real store error
// on GET / never leaks its detail to the client: closing the store out from
// under a live server makes ListAllTickets return a genuine error, and
// handleIndex must turn that into 500 with the fixed generic body, logging
// the detail server-side instead (design section "Console" fix 10).
func TestIndexReturns500WithGenericBodyOnStoreError(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	//nolint:noctx // a bare GET on a test server needs no deadline
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET / after closing the store: status = %d, want 500", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := strings.TrimSpace(string(body))
	if got != "internal error" {
		t.Errorf(`GET / body = %q, want exactly "internal error"`, got)
	}
}

// TestThreadRejectsInvalidIDWith400 proves handleThread validates the id
// query parameter -- non-integer and non-positive alike -- before it ever
// opens the SSE stream (design section "Console" fix 11).
// TestThreadOpenZeroOrNegativeRendersEmptyThread proves patchThread's
// id<=0 guard (design section 6.9, "Console" fix 3): a $open signal of 0 (no
// ticket open, including an absent signal, which ReadSignals leaves at its
// zero value) or a negative value opens the stream normally and patches an
// empty #thread, rather than erroring -- a stale or unset signal is a normal
// client state, not a bad request.
func TestThreadOpenZeroOrNegativeRendersEmptyThread(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	cases := []struct {
		name string
		open int64
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, threadURL(srv.URL, tc.open), http.NoBody)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /thread(open=%d): %v", tc.open, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /thread(open=%d) status = %d, want 200", tc.open, resp.StatusCode)
			}

			r := bufio.NewReader(resp.Body)
			frame := readFrame(t, r)
			assertExactSSEFraming(t, frame)
			if !strings.Contains(frame, `id="thread"`) {
				t.Errorf("GET /thread(open=%d) frame missing #thread; got:\n%s", tc.open, frame)
			}
		})
	}
}

// TestThreadRejectsMalformedSignalsWith400 proves handleThread validates the
// datastar-encoded $open signal before it ever opens the SSE stream (design
// section 6.9, "Console" fix 3): a "datastar" query parameter that is not
// valid JSON fails ReadSignals and returns 400.
func TestThreadRejectsMalformedSignalsWith400(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	//nolint:noctx // a bare GET on a test server needs no deadline
	resp, err := http.Get(srv.URL + "/thread?datastar=" + url.QueryEscape("{not valid json"))
	if err != nil {
		t.Fatalf("GET /thread: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /thread with malformed signals: status = %d, want 400", resp.StatusCode)
	}
}

// TestAnswerRejectsOversizedBodyWith400 proves POST /answer wraps r.Body in
// http.MaxBytesReader before ReadSignals (design section "Console" fix 12):
// a body padded well past the limit with an otherwise-ignored field is
// rejected with 400 before it can reach the store, rather than decoding in
// full and failing later (which would surface as 500 for these
// nonexistent ids, not 400).
func TestAnswerRejectsOversizedBodyWith400(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	filler := strings.Repeat("x", 16<<10) // far past the console's body size limit
	body := fmt.Sprintf(`{"answer":{"ticket":1,"question":999999,"option":"a"},"filler":%q}`, filler)

	resp := postAnswer(t, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /answer with an oversized body: status = %d, want 400 (rejected before it could reach the store)", resp.StatusCode)
	}
}

// TestAnswerCSRFGuard proves the same-origin guard on POST /answer (design
// section "Console" fix 2, CWE-352): a cross-site request -- whether flagged
// by a Sec-Fetch-Site value other than "same-origin" or "none", or by the
// absence of the Datastar-Request header every real Datastar backend action
// sends -- is rejected with 403 before it ever reaches the store, leaving
// the question untouched; a normal same-origin Datastar POST still succeeds
// with 204. Each rejected case seeds its own ticket and question, since a
// wrongly accepted case would answer the question and corrupt a later
// case's expectations.
func TestAnswerCSRFGuard(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	postWithHeaders := func(t *testing.T, body string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/answer", strings.NewReader(body))
		if err != nil {
			t.Fatalf("build POST /answer request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /answer: %v", err)
		}
		return resp
	}

	rejected := []struct {
		name    string
		headers map[string]string
	}{
		{"cross-site Sec-Fetch-Site", map[string]string{"Sec-Fetch-Site": "cross-site", "Datastar-Request": "true"}},
		{"same-site Sec-Fetch-Site is not same-origin", map[string]string{"Sec-Fetch-Site": "same-site", "Datastar-Request": "true"}},
		{"missing Datastar-Request header", map[string]string{}},
	}
	for i, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			ticketID := seedTicket(t, s, fmt.Sprintf("csrf-reject-%d", i), "CSRF guard fixture")
			questionID := seedOpenQuestion(t, s, ticketID)
			body := fmt.Sprintf(`{"answer":{"ticket":%d,"question":%d,"option":"a"}}`, ticketID, questionID)

			resp := postWithHeaders(t, body, tc.headers)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST /answer status = %d, want 403", resp.StatusCode)
			}

			ticket, err := s.GetTicket(t.Context(), ticketID)
			if err != nil {
				t.Fatalf("GetTicket: %v", err)
			}
			if ticket.WaitingOn == nil || *ticket.WaitingOn != "questions" {
				t.Errorf("ticket.WaitingOn after a rejected cross-site POST = %v, want unchanged \"questions\" (nothing answered)", ticket.WaitingOn)
			}
		})
	}

	t.Run("same-origin Datastar POST still succeeds", func(t *testing.T) {
		ticketID := seedTicket(t, s, "csrf-accept", "CSRF guard fixture (accepted)")
		questionID := seedOpenQuestion(t, s, ticketID)
		body := fmt.Sprintf(`{"answer":{"ticket":%d,"question":%d,"option":"a"}}`, ticketID, questionID)

		resp := postAnswer(t, srv.URL, body)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("same-origin POST /answer status = %d, want 204", resp.StatusCode)
		}
	})
}

// TestNonStreamingRoutesSucceedUnderWriteDeadline proves the write-deadline
// middleware (design section 6.10, fix 2) does not break an ordinary, fast
// response on any of the three non-streaming routes it wraps: GET /,
// POST /answer, and GET /static/datastar.js. It does not test the deadline
// firing, only that its presence leaves a normal response intact.
func TestNonStreamingRoutesSucceedUnderWriteDeadline(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	//nolint:noctx // a bare GET on a test server needs no deadline
	indexResp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = indexResp.Body.Close()
	if indexResp.StatusCode != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", indexResp.StatusCode)
	}

	//nolint:noctx // a bare GET on a test server needs no deadline
	staticResp, err := http.Get(srv.URL + "/static/datastar.js")
	if err != nil {
		t.Fatalf("GET /static/datastar.js: %v", err)
	}
	_ = staticResp.Body.Close()
	if staticResp.StatusCode != http.StatusOK {
		t.Errorf("GET /static/datastar.js status = %d, want 200", staticResp.StatusCode)
	}

	body := fmt.Sprintf(`{"answer":{"ticket":%d,"question":%d,"option":"a"}}`, ticketID, questionID)
	answerResp := postAnswer(t, srv.URL, body)
	_ = answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /answer status = %d, want 204", answerResp.StatusCode)
	}
}
