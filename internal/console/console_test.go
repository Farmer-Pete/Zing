package console_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
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
	Name: "acme", RepoURL: "https://github.com/x/zing", LocalPath: "/tmp/zing", Tracker: "github",
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
		TicketID: ticketID, Type: "update", Author: "zing", Body: body,
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

func TestIndexRendersShellRegionsAndScript(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedOpenQuestion(t, s, ticketID) // blocking, so it shows in #nav's thread list

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
// under a live server makes the store reads handleIndex depends on return a
// genuine error, and handleIndex must turn that into 500 with the fixed
// generic body, logging the detail server-side instead (design section
// "Console" fix 10).
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
