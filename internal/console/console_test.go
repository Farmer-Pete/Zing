package console_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/thread?id="+strconv.FormatInt(ticketID, 10), http.NoBody)
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

	ctx, cancel := context.WithCancel(t.Context())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/thread?id="+strconv.FormatInt(ticketID, 10), http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /thread: %v", err)
	}

	r := bufio.NewReader(resp.Body)
	readFrame(t, r) // the initial patch, proving the stream is live

	baseline := runtime.NumGoroutine()

	cancel() // simulate the client navigating away or Datastar aborting the request
	_ = resp.Body.Close()

	// Poll, bounded, instead of a fixed sleep: the handler's goroutine exits
	// promptly once r.Context().Done() fires, so the count should settle back
	// near baseline well inside the deadline.
	deadline := time.Now().Add(frameTimeout)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Errorf("goroutine count did not settle after disconnect: got %d, baseline %d",
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
	if ct := resp.Header.Get("Content-Type"); ct != "text/javascript" {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/thread?id="+strconv.FormatInt(ticketID, 10), http.NoBody)
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
	// html/template treats data-on:click as a JS attribute (attrType strips
	// the "data-" prefix, and "on:click" then matches its "on" heuristic), so
	// it pads each substituted number with spaces as its JS-context escaper
	// does; match loosely around the ids instead of a fixed-spacing literal.
	wantChipA := regexp.MustCompile(
		`\$answer = \{ticket:\s*` + strconv.FormatInt(ticketID, 10) +
			`\s*,\s*question:\s*` + strconv.FormatInt(questionID, 10) + `\s*,\s*option: 'a'\}`)
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

	body := []byte(`{"answer":{"ticket":` + strconv.FormatInt(ticketID, 10) +
		`,"question":` + strconv.FormatInt(questionID, 10) + `,"option":"a"}}`)

	//nolint:noctx // a bare POST on a test server needs no deadline
	resp, err := http.Post(srv.URL+"/answer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first POST /answer: %v", err)
	}
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

	//nolint:noctx // a bare POST on a test server needs no deadline
	resp2, err := http.Post(srv.URL+"/answer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second POST /answer: %v", err)
	}
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
