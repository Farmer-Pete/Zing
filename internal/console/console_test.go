package console_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
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

// testBindHost and testConsolePort are the console.New arguments every test
// in this package that does not itself exercise the mutation guard (mw.go,
// design section 6.14) passes: the guard only matters to the routes it
// wraps, so a GET-only test's httptest.NewServer port need not match
// testConsolePort. mw_test.go and answer_test.go build their own server on
// a reserved listener so their port does match.
const (
	testBindHost    = "127.0.0.1"
	testConsolePort = 7420
)

// testTrackerGitHub, testAuthorZing, testMsgTypeQuestion, and
// testWaitingQuestions round up the string literals this package's tests
// repeat three or more times: the tracker name every seeded project uses,
// the author every zing-authored message uses, the "question" message
// type, and the "questions" ticket.waiting_on value a seeded open question
// sets (store's own historical spelling, commit.go's waitingFlagQuestions).
const (
	testTrackerGitHub    = "github"
	testAuthorZing       = "zing"
	testMsgTypeQuestion  = "question"
	testWaitingQuestions = "questions"
)

var testProject = store.Project{
	Name: "acme", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: testTrackerGitHub,
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

// seedTicketIn inserts one queued ticket under proj and ref, returning its
// id. seedTicket (below) is the common case, one project reused across
// calls; this is the seam multi-project tests (Inbox grouping) use.
func seedTicketIn(t *testing.T, s *store.Store, proj store.Project, ref, title string) int64 {
	t.Helper()
	projectID, err := s.EnsureProject(t.Context(), proj)
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

// seedTicket inserts one queued ticket under testProject and ref, returning
// its id.
func seedTicket(t *testing.T, s *store.Store, ref, title string) int64 {
	t.Helper()
	return seedTicketIn(t, s, testProject, ref, title)
}

// seedStateMessage inserts one "state" message on ticketID, the same shape
// commit.go writes on every transition (design section 6.3).
func seedStateMessage(t *testing.T, s *store.Store, ticketID int64, from, to, reason string) int64 {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"from": from, "to": to, "reason": reason})
	if err != nil {
		t.Fatalf("marshal state payload: %v", err)
	}
	id, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: "state", Author: "system", Payload: payload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(state): %v", err)
	}
	return id
}

// seedUnreadUpdate inserts one unread "update" message authored by zing, the
// shape that makes a ticket unread without making it blocking (design
// section 6.8: unread is read_at IS NULL, author zing, type in question,
// followup, escalation, update).
func seedUnreadUpdate(t *testing.T, s *store.Store, ticketID int64, body string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: "update", Author: testAuthorZing, Body: body,
	})
	if err != nil {
		t.Fatalf("InsertMessage(update): %v", err)
	}
	return id
}

// seedOpenQuestion claims ticketID and commits one open "question" message
// with a two-option payload, the run-less shape a first-entry planning
// commit writes before a session exists (design section 6.3, section 6.6):
// a heading and body text in Body, and a validated QuestionPayload with
// options "a" and "b". It also sets the ticket's waiting_on to "questions",
// so a test can assert a resolved answer clears it. It returns the question
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

	waiting := testWaitingQuestions
	openState := "open"
	payload := []byte(`{"key":"Q1","kind":"question","state":"open","recommended":"a",` +
		`"options":[{"key":"a","text":"Plain hello"},{"key":"b","text":"hello, world"}]}`)

	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: &waiting,
		Messages: []store.Message{{
			TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing,
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
		if messages[i].Type == testMsgTypeQuestion {
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

func TestIndexRendersShellRegionsAndScript(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedOpenQuestion(t, s, ticketID) // blocking, so it shows in #nav's thread list

	srv := httptest.NewServer(console.New(s, bus.New(), testBindHost, testConsolePort))
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

	for _, want := range []string{
		`id="nav"`,
		`id="main"`,
		`id="rail"`,
		`id="stream-ctl"`,
		`data-signals="{view: 'inbox', open: 0, project: 0}"`,
		`data-init="@get('/stream')"`,
		`data-on:zing-nav="$view = evt.detail.view; $open = evt.detail.open; $project = evt.detail.project; @get('/stream')"`,
		"Add a hello endpoint",
		`<script type="module" src="/static/datastar.js">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("GET / body missing %q; got:\n%s", want, got)
		}
	}
}

func TestStaticServesDatastarBundle(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), testBindHost, testConsolePort))
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

// TestIndexReturns500WithGenericBodyOnStoreError proves a real store error
// on GET / never leaks its detail to the client: closing the store out from
// under a live server makes the store reads handleIndex depends on return a
// genuine error, and handleIndex must turn that into 500 with the fixed
// generic body, logging the detail server-side instead (design section
// "Console" fix 10).
func TestIndexReturns500WithGenericBodyOnStoreError(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), testBindHost, testConsolePort))
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

// TestNonStreamingRoutesSucceedUnderWriteDeadline proves the write-deadline
// middleware (design section 6.10, fix 2) does not break an ordinary, fast
// response on either of the two non-streaming routes it wraps in this file:
// GET / and GET /static/datastar.js. It does not test the deadline firing,
// only that its presence leaves a normal response intact; answer_test.go
// covers the same guard on the mutation routes.
func TestNonStreamingRoutesSucceedUnderWriteDeadline(t *testing.T) {
	s := newConsoleTestStore(t)

	srv := httptest.NewServer(console.New(s, bus.New(), testBindHost, testConsolePort))
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
}
