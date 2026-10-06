package console_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// testMsgTypeReply and testDraftState are this file's own stand-ins for
// the "reply" message type and the "draft" lifecycle state (goconst):
// views.go's own msgTypeReply and draftMessageState live in package
// console, unreachable from this file's package console_test.
const (
	testMsgTypeReply = "reply"
	testDraftState   = "draft"
)

// seedExtraOpenQuestion inserts one more open "question" message directly
// via InsertMessage, for a test that needs several open questions on one
// already-claimed ticket: seedOpenQuestion's own Claim would return false
// on a ticket claimed once already (store.Claim only succeeds when
// claim_owner IS NULL), so it cannot simply be called a second time.
func seedExtraOpenQuestion(t *testing.T, s *store.Store, ticketID int64, key string) int64 {
	t.Helper()
	openState := testQuestionStateOpen
	payload := []byte(fmt.Sprintf(`{"key":%q,"kind":"question","state":"open","recommended":"a",`+
		`"options":[{"key":"a","text":"Plain hello"},{"key":"b","text":"hello, world"}]}`, key))
	id, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State:   &openState,
		Body:    key + "\n\nbody",
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(%s): %v", key, err)
	}
	return id
}

// replyDraftState returns the state of ticketID's reply draft against
// questionID, or "" when no such row exists.
func replyDraftState(t *testing.T, s *store.Store, ticketID, questionID int64) string {
	t.Helper()
	messages, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := range messages {
		m := &messages[i]
		if m.Type == testMsgTypeReply && m.ParentID != nil && *m.ParentID == questionID {
			if m.State == nil {
				return ""
			}
			return *m.State
		}
	}
	return ""
}

// TestDraft_SucceedsThenConflictsOnAClosedQuestion proves POST /draft's
// happy path (204, the draft row lands as author=you state=draft) and its
// 409 conflict path (a question already resolved is rejected, nothing
// written) (design section 6.7, 7.1).
func TestDraft_SucceedsThenConflictsOnAClosedQuestion(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	body := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":"a"}`, ticketID, questionID)
	resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", resp.StatusCode)
	}

	messages, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var found bool
	for i := range messages {
		m := &messages[i]
		if m.Type == "answer" && m.ParentID != nil && *m.ParentID == questionID {
			found = true
			if m.Author != "you" || m.State == nil || *m.State != testDraftState {
				t.Errorf("draft row = author=%s state=%v, want author=you state=draft", m.Author, m.State)
			}
		}
	}
	if !found {
		t.Fatal("no draft answer row found after POST /draft")
	}

	// Send the batch, which flips the question to "answered" (design
	// section 6.7) -- no longer "open" -- so a second draft against the
	// same question now hits SaveDraft's closed-question conflict.
	sendResp := doRequest(t, mutationRequest(t, srv, "/send", fmt.Sprintf(`{"ticket":%d,"questions":[%d]}`, ticketID, questionID)))
	_ = sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /send status = %d, want 200", sendResp.StatusCode)
	}

	resp2 := doRequest(t, mutationRequest(t, srv, "/draft", body))
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusConflict {
		respBody, readErr := io.ReadAll(resp2.Body)
		if readErr != nil {
			t.Fatalf("second POST /draft status = %d, want 409 (read body: %v)", resp2.StatusCode, readErr)
		}
		t.Fatalf("second POST /draft status = %d, want 409 (body: %s)", resp2.StatusCode, respBody)
	}
}

// TestDraft_EmptyTextClearsTheReplyDraft proves POST /draft's autosave
// contract (design section 6.7): a text draft against a question saves as
// 204, and a follow-up POST with text "" is also 204 and removes that
// question's draft reply row, rather than the 409 an empty text used to
// return.
func TestDraft_EmptyTextClearsTheReplyDraft(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	body := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"note"}`, ticketID, questionID)
	resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft (text) status = %d, want 204", resp.StatusCode)
	}

	messagesAfterSave, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	foundDraft := false
	for i := range messagesAfterSave {
		m := &messagesAfterSave[i]
		if m.Type == testMsgTypeReply && m.ParentID != nil && *m.ParentID == questionID {
			foundDraft = true
			break
		}
	}
	if !foundDraft {
		t.Fatalf("draft reply not found after POST /draft (text): the clear-side assertion below would pass vacuously")
	}

	emptyBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":""}`, ticketID, questionID)
	resp2 := doRequest(t, mutationRequest(t, srv, "/draft", emptyBody))
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft (empty text) status = %d, want 204", resp2.StatusCode)
	}

	messages, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := range messages {
		m := &messages[i]
		if m.Type == testMsgTypeReply && m.ParentID != nil && *m.ParentID == questionID {
			t.Fatalf("draft reply still present after an empty-text POST /draft: %+v", m)
		}
	}
}

// TestDraft_OmittedTextAgainstAQuestionIsMalformed proves POST /draft tells
// an omitted "text" key apart from an explicit "text":"" (review fix): a
// question draft naming none of option, item, or text names no mode at all,
// so it is 400, while the explicit-empty-string case
// (TestDraft_EmptyTextClearsTheReplyDraft) must keep returning 204 and
// clearing the draft.
func TestDraft_OmittedTextAgainstAQuestionIsMalformed(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	body := fmt.Sprintf(`{"ticket":%d,"question":%d}`, ticketID, questionID)
	resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatalf("status = %d, want 400 (read body: %v)", resp.StatusCode, readErr)
		}
		t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, respBody)
	}
}

// TestDraft_RejectsMalformedAndOversizedBodies proves the transport-layer
// checks POST /draft runs before SaveDraft ever sees the body (design
// section 6.7): malformed JSON and an unknown field are 400, and a body
// padded well past the 64 KiB cap is 413.
func TestDraft_RejectsMalformedAndOversizedBodies(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	t.Run("malformed JSON", func(t *testing.T) {
		t.Parallel()
		resp := doRequest(t, mutationRequest(t, srv, "/draft", `{not json`))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`{"ticket":%d,"text":"hi","bogus":true}`, ticketID)
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("text over 8000 characters", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`{"ticket":%d,"text":%q}`, ticketID, strings.Repeat("x", 8001))
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("base over 8000 characters", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`{"ticket":%d,"text":"hi","base":%q}`, ticketID, strings.Repeat("x", 8001))
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("base of exactly 8000 characters is not rejected for its length", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`{"ticket":%d,"text":"hi","base":%q}`, ticketID, strings.Repeat("x", 8000))
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusBadRequest {
			t.Errorf("status = %d, want anything but 400 (length alone must not reject it)", resp.StatusCode)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		t.Parallel()
		filler := strings.Repeat("x", 80<<10) // past the 64 KiB cap
		body := fmt.Sprintf(`{"ticket":%d,"text":%q}`, ticketID, filler)
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
	})
}

// TestDraft_ThreadReplyAgainstMissingTicketReturns409 proves a thread-reply
// draft (no question, design section 6.7) against a ticket id that names no
// row is a 409, not a 500 (review fix, PR #16): SaveDraft's insertReplyDraftTx
// path had no existence check of its own, so it fell through to the
// messages.ticket_id foreign key and an untyped store error the handler
// mapped to 500.
func TestDraft_ThreadReplyAgainstMissingTicketReturns409(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	const missingTicketID = 999999
	body := fmt.Sprintf(`{"ticket":%d,"text":"hello"}`, missingTicketID)
	resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /draft against a missing ticket: status = %d, want 409", resp.StatusCode)
	}
}

// TestDraft_RejectsTrailingDataAfterTheJSONBody proves decodeStrict rejects
// a body that decodes cleanly but keeps going, including trailing bytes
// that dec.More() alone missed (cubic review fix, PR #16): a stray '}' or
// ']' right after a complete top-level value makes More() report "no more
// input" (it treats those bytes as closing an enclosing array/object,
// which does not exist at the top level), so a body like `{"ticket":1}}`
// used to decode as valid.
func TestDraft_RejectsTrailingDataAfterTheJSONBody(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	for _, tc := range []struct {
		name string
		body string
	}{
		{"trailing brace", fmt.Sprintf(`{"ticket":%d,"text":"hi"}}`, ticketID)},
		{"trailing bracket", fmt.Sprintf(`{"ticket":%d,"text":"hi"}]`, ticketID)},
		{"second JSON value", fmt.Sprintf(`{"ticket":%d,"text":"hi"}{"ticket":%d,"text":"hi"}`, ticketID, ticketID)},
		{"trailing garbage", fmt.Sprintf(`{"ticket":%d,"text":"hi"} garbage`, ticketID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := doRequest(t, mutationRequest(t, srv, "/draft", tc.body))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestSend_SucceedsThenConflictsWhenEmpty proves POST /send's happy path
// (200, drafts flip to sent, a plain "Sent N answer(s)." body) and that
// sending again with nothing left drafted is 409 Empty with a "Nothing to
// send." body (design section 6.7, 7.1; bug fix: Cmd+Enter sent the batch,
// but the console showed nothing, so the owner thought it had done
// nothing -- POST /send's 204 carried no way to say what happened).
func TestSend_SucceedsThenConflictsWhenEmpty(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":"a"}`, ticketID, questionID)
	draftResp := doRequest(t, mutationRequest(t, srv, "/draft", draftBody))
	_ = draftResp.Body.Close()
	if draftResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", draftResp.StatusCode)
	}

	sendBody := fmt.Sprintf(`{"ticket":%d,"questions":[%d]}`, ticketID, questionID)
	sendResp := doRequest(t, mutationRequest(t, srv, "/send", sendBody))
	sendRespBody, err := io.ReadAll(sendResp.Body)
	_ = sendResp.Body.Close()
	if err != nil {
		t.Fatalf("read first POST /send body: %v", err)
	}
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("first POST /send status = %d, want 200", sendResp.StatusCode)
	}
	if got := string(sendRespBody); got != "Sent 1 message." {
		t.Errorf("first POST /send body = %q, want %q", got, "Sent 1 message.")
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil (wait cleared)", *ticket.WaitingOn)
	}

	sendResp2 := doRequest(t, mutationRequest(t, srv, "/send", sendBody))
	sendRespBody2, err := io.ReadAll(sendResp2.Body)
	_ = sendResp2.Body.Close()
	if err != nil {
		t.Fatalf("read second POST /send body: %v", err)
	}
	if sendResp2.StatusCode != http.StatusConflict {
		t.Fatalf("second POST /send status = %d, want 409 (nothing left to send)", sendResp2.StatusCode)
	}
	if got := strings.TrimSpace(string(sendRespBody2)); got != "Nothing to send. Pick an option or type a reply first." {
		t.Errorf("second POST /send body = %q, want %q", got, "Nothing to send. Pick an option or type a reply first.")
	}
}

// TestSend_RequiresQuestions proves POST /send no longer sends every draft
// on the ticket (ticket #43, cause 1 of the Cmd+Enter bug): a body naming
// no questions, whether the key is absent or an empty array, answers 400
// "questions required" rather than falling back to the old send-everything
// behavior, and the drafted reply stays a draft either way.
func TestSend_RequiresQuestions(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"a reply"}`, ticketID, questionID)
	draftResp := doRequest(t, mutationRequest(t, srv, "/draft", draftBody))
	_ = draftResp.Body.Close()
	if draftResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", draftResp.StatusCode)
	}

	for _, sendBody := range []string{
		fmt.Sprintf(`{"ticket":%d}`, ticketID),
		fmt.Sprintf(`{"ticket":%d,"questions":[]}`, ticketID),
	} {
		resp := doRequest(t, mutationRequest(t, srv, "/send", sendBody))
		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read POST /send %s body: %v", sendBody, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /send %s status = %d, want 400", sendBody, resp.StatusCode)
		}
		if got := strings.TrimSpace(string(respBody)); got != "questions required" {
			t.Errorf("POST /send %s body = %q, want %q", sendBody, got, "questions required")
		}
	}

	if got := replyDraftState(t, s, ticketID, questionID); got != testDraftState {
		t.Errorf("reply state after rejected sends = %q, want %q", got, testDraftState)
	}
}

// TestSend_OnlyListedQuestionsSend proves a scoped POST /send leaves an
// unlisted question's draft untouched over HTTP (ticket #43): with drafts
// on three questions, sending only Q1's id sends exactly that one, and Q2
// and Q3 stay drafted. A repeat send naming only Q1 again, now already
// sent, is 409 Empty.
func TestSend_OnlyListedQuestionsSend(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	q1 := seedOpenQuestion(t, s, ticketID)
	q2 := seedExtraOpenQuestion(t, s, ticketID, "Q2")
	q3 := seedExtraOpenQuestion(t, s, ticketID, "Q3")

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	for _, q := range []int64{q1, q2, q3} {
		draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"a reply"}`, ticketID, q)
		draftResp := doRequest(t, mutationRequest(t, srv, "/draft", draftBody))
		_ = draftResp.Body.Close()
		if draftResp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST /draft(q=%d) status = %d, want 204", q, draftResp.StatusCode)
		}
	}

	sendResp := doRequest(t, mutationRequest(t, srv, "/send", fmt.Sprintf(`{"ticket":%d,"questions":[%d]}`, ticketID, q1)))
	sendRespBody, err := io.ReadAll(sendResp.Body)
	_ = sendResp.Body.Close()
	if err != nil {
		t.Fatalf("read POST /send body: %v", err)
	}
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /send status = %d, want 200", sendResp.StatusCode)
	}
	if got := string(sendRespBody); got != "Sent 1 message." {
		t.Errorf("POST /send body = %q, want %q", got, "Sent 1 message.")
	}

	for _, q := range []int64{q2, q3} {
		if got := replyDraftState(t, s, ticketID, q); got != testDraftState {
			t.Errorf("reply state for q=%d = %q, want %q", q, got, testDraftState)
		}
	}

	sendResp2 := doRequest(t, mutationRequest(t, srv, "/send", fmt.Sprintf(`{"ticket":%d,"questions":[%d,%d]}`, ticketID, q1, q1)))
	sendRespBody2, err := io.ReadAll(sendResp2.Body)
	_ = sendResp2.Body.Close()
	if err != nil {
		t.Fatalf("read second POST /send body: %v", err)
	}
	if sendResp2.StatusCode != http.StatusConflict {
		t.Fatalf("second POST /send status = %d, want 409 (nothing left to send on q1)", sendResp2.StatusCode)
	}
	if got := strings.TrimSpace(string(sendRespBody2)); got != "Nothing to send. Pick an option or type a reply first." {
		t.Errorf("second POST /send body = %q, want %q", got, "Nothing to send. Pick an option or type a reply first.")
	}
}

// TestSend_RejectsOversizedOrInvalidQuestionList proves handleSend's two
// cheap checks ahead of the store (ticket #43): a questions list over
// maxSendQuestions (50) ids, or one containing an id at or below 0, both
// answer 400 "bad request" and leave the draft untouched, while a list of
// exactly 50 ids that includes the drafted question still sends.
func TestSend_RejectsOversizedOrInvalidQuestionList(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	q1 := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"a reply"}`, ticketID, q1)
	draftResp := doRequest(t, mutationRequest(t, srv, "/draft", draftBody))
	_ = draftResp.Body.Close()
	if draftResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", draftResp.StatusCode)
	}

	// 51 ids, none of them q1, so this also proves the cap is enforced
	// before any question is looked up.
	oversizedIDs := make([]string, 51)
	for i := range oversizedIDs {
		oversizedIDs[i] = strconv.FormatInt(q1+int64(i)+1000, 10)
	}
	oversizedBody := fmt.Sprintf(`{"ticket":%d,"questions":[%s]}`, ticketID, strings.Join(oversizedIDs, ","))
	oversizedResp := doRequest(t, mutationRequest(t, srv, "/send", oversizedBody))
	oversizedRespBody, err := io.ReadAll(oversizedResp.Body)
	_ = oversizedResp.Body.Close()
	if err != nil {
		t.Fatalf("read 51-id POST /send body: %v", err)
	}
	if oversizedResp.StatusCode != http.StatusBadRequest {
		t.Errorf("51 ids: status = %d, want 400", oversizedResp.StatusCode)
	}
	if got := strings.TrimSpace(string(oversizedRespBody)); got != "bad request" {
		t.Errorf("51 ids: body = %q, want %q", got, "bad request")
	}

	zeroBody := fmt.Sprintf(`{"ticket":%d,"questions":[%d,0]}`, ticketID, q1)
	zeroResp := doRequest(t, mutationRequest(t, srv, "/send", zeroBody))
	zeroRespBody, err := io.ReadAll(zeroResp.Body)
	_ = zeroResp.Body.Close()
	if err != nil {
		t.Fatalf("read id-0 POST /send body: %v", err)
	}
	if zeroResp.StatusCode != http.StatusBadRequest {
		t.Errorf("id 0: status = %d, want 400", zeroResp.StatusCode)
	}
	if got := strings.TrimSpace(string(zeroRespBody)); got != "bad request" {
		t.Errorf("id 0: body = %q, want %q", got, "bad request")
	}

	if got := replyDraftState(t, s, ticketID, q1); got != testDraftState {
		t.Errorf("reply state after rejected sends = %q, want %q", got, testDraftState)
	}

	exactly50 := make([]string, 50)
	exactly50[0] = strconv.FormatInt(q1, 10)
	for i := 1; i < 50; i++ {
		exactly50[i] = strconv.FormatInt(q1+int64(i)+1000, 10)
	}
	okBody := fmt.Sprintf(`{"ticket":%d,"questions":[%s]}`, ticketID, strings.Join(exactly50, ","))
	okResp := doRequest(t, mutationRequest(t, srv, "/send", okBody))
	_ = okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Errorf("50 ids including q1: status = %d, want 200", okResp.StatusCode)
	}
}

// TestSend_RefusesAmendmentAcceptFromNonLoopback proves POST /send holds
// Accept on an amended escalation to the same loopback-only boundary
// handleOwnerEdit already holds a check, test, or kind edit to (#57, r1f9
// triage): a non-loopback send of a drafted "a" (Accept) is refused 409 with
// SendBatchOnly's own reason, the question stays open, and the scenario is
// unchanged. The same send, from loopback, succeeds and answers "a".
func TestSend_RefusesAmendmentAcceptFromNonLoopback(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	seedSealedScenarioArtifact(t, s, ticketID, nil, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindBehavior, Check: testOldCheck, Given: "g", When: "w", Then: "t",
	})
	questionID := seedAmendedQuestion(t, s, ticketID, "s1")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":"a"}`, ticketID, questionID)
	draftResp := doRequest(t, mutationRequest(t, srv, "/draft", draftBody))
	_ = draftResp.Body.Close()
	if draftResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", draftResp.StatusCode)
	}

	authority := strings.TrimPrefix(srv.URL, "http://")
	sendBody := fmt.Sprintf(`{"ticket":%d,"questions":[%d]}`, ticketID, questionID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/send", strings.NewReader(sendBody))
	if err != nil {
		t.Fatalf("build POST /send request: %v", err)
	}
	req.Host = authority
	req.RemoteAddr = testNonLoopbackRemoteAddr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", srv.URL)

	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("non-loopback send status = %d, want 409; body = %q", rec.Code, rec.Body.String())
	}
	wantReason := "accepting a judge amendment is allowed from this machine only"
	if got := strings.TrimSpace(rec.Body.String()); got != wantReason {
		t.Errorf("non-loopback send body = %q, want %q", got, wantReason)
	}

	got, err := s.GetMessage(t.Context(), questionID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.State == nil || *got.State != testQuestionStateOpen {
		t.Errorf("question state after non-loopback send = %v, want unchanged open", got.State)
	}
	if got := readScenarioByID(t, s, ticketID, "s1").Check; got != testOldCheck {
		t.Errorf("s1 check after non-loopback send = %q, want unchanged %q", got, testOldCheck)
	}

	loopbackResp := doRequest(t, mutationRequest(t, srv, "/send", sendBody))
	_ = loopbackResp.Body.Close()
	if loopbackResp.StatusCode != http.StatusOK {
		t.Fatalf("loopback send status = %d, want 200", loopbackResp.StatusCode)
	}
	got, err = s.GetMessage(t.Context(), questionID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.State == nil || *got.State != testQuestionStateAnswered {
		t.Errorf("question state after loopback send = %v, want answered", got.State)
	}
}

// TestDraft_StaleBaseConflictsOverHTTP proves POST /draft's base check over
// HTTP (ticket #43, cause 1 of the two-tabs bug): tab A's save with base ""
// succeeds, tab B's save of different text with the same stale base "" is
// 409 with a JSON body naming tab A's text as current, and a third save
// that catches up to that text as its base succeeds.
func TestDraft_StaleBaseConflictsOverHTTP(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	bodyA := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"from tab A","base":""}`, ticketID, questionID)
	respA := doRequest(t, mutationRequest(t, srv, "/draft", bodyA))
	_ = respA.Body.Close()
	if respA.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft (tab A) status = %d, want 204", respA.StatusCode)
	}

	bodyB := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"from tab B","base":""}`, ticketID, questionID)
	respB := doRequest(t, mutationRequest(t, srv, "/draft", bodyB))
	respBBody, err := io.ReadAll(respB.Body)
	_ = respB.Body.Close()
	if err != nil {
		t.Fatalf("read POST /draft (tab B) body: %v", err)
	}
	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("POST /draft (tab B) status = %d, want 409 (body: %s)", respB.StatusCode, respBBody)
	}
	if ct := respB.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("POST /draft (tab B) Content-Type = %q, want application/json", ct)
	}
	var conflict struct {
		Reason  string `json:"reason"`
		Current string `json:"current"`
	}
	if err := json.Unmarshal(respBBody, &conflict); err != nil {
		t.Fatalf("unmarshal conflict body %q: %v", respBBody, err)
	}
	if conflict.Reason != "changed in another tab" {
		t.Errorf("conflict reason = %q, want %q", conflict.Reason, "changed in another tab")
	}
	if conflict.Current != "from tab A" {
		t.Errorf("conflict current = %q, want %q", conflict.Current, "from tab A")
	}

	bodyC := fmt.Sprintf(`{"ticket":%d,"question":%d,"text":"from tab A, caught up","base":"from tab A"}`, ticketID, questionID)
	respC := doRequest(t, mutationRequest(t, srv, "/draft", bodyC))
	_ = respC.Body.Close()
	if respC.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft (caught up) status = %d, want 204", respC.StatusCode)
	}
}

// TestDraft_DoesNotPublishWhileSendDoes proves handleDraft's review fix
// (answer.go's doc comment): a POST /draft that saves cleanly must not wake
// the bus, since a draft renders nothing server-side and the resulting
// /stream re-render would strip the client-only ".picked" highlight and
// collapse the open question group after every pick. POST /send against the
// same ticket, by contrast, still publishes once the batch actually sends.
func TestDraft_DoesNotPublishWhileSendDoes(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	questionID := seedOpenQuestion(t, s, ticketID)

	b := bus.New()
	ch, cancel := b.Subscribe()
	defer cancel()
	srv, _ := newMutationTestServer(t, s, b, newTestLogHandler(t))

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":"a"}`, ticketID, questionID)
	draftResp := doRequest(t, mutationRequest(t, srv, "/draft", draftBody))
	_ = draftResp.Body.Close()
	if draftResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", draftResp.StatusCode)
	}

	select {
	case <-ch:
		t.Fatal("POST /draft woke a bus subscriber, want no publish")
	default:
	}

	sendResp := doRequest(t, mutationRequest(t, srv, "/send", fmt.Sprintf(`{"ticket":%d,"questions":[%d]}`, ticketID, questionID)))
	_ = sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /send status = %d, want 200", sendResp.StatusCode)
	}

	select {
	case <-ch:
	default:
		t.Fatal("POST /send did not wake a bus subscriber, want a publish")
	}
}

// TestRead_MarksOneMessageRead proves POST /read sets read_at on the named
// message and publishes (design section 6.8, 7.1).
func TestRead_MarksOneMessageRead(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	msgID := seedUnreadUpdate(t, s, ticketID, "progress")

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	body := fmt.Sprintf(`{"message":%d}`, msgID)
	resp := doRequest(t, mutationRequest(t, srv, "/read", body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /read status = %d, want 204", resp.StatusCode)
	}

	m, err := s.GetMessage(t.Context(), msgID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.ReadAt == nil {
		t.Error("ReadAt after POST /read is still nil")
	}
}
