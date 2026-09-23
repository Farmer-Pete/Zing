package console_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"zing/internal/bus"
)

// TestDraft_SucceedsThenConflictsOnAClosedQuestion proves POST /draft's
// happy path (204, the draft row lands as author=you state=draft) and its
// 409 conflict path (a question already resolved is rejected, nothing
// written) (design section 6.7, 7.1).
func TestDraft_SucceedsThenConflictsOnAClosedQuestion(t *testing.T) {
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
			if m.Author != "you" || m.State == nil || *m.State != "draft" {
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
	sendResp := doRequest(t, mutationRequest(t, srv, "/send", fmt.Sprintf(`{"ticket":%d}`, ticketID)))
	_ = sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /send status = %d, want 204", sendResp.StatusCode)
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

// TestDraft_RejectsMalformedAndOversizedBodies proves the transport-layer
// checks POST /draft runs before SaveDraft ever sees the body (design
// section 6.7): malformed JSON and an unknown field are 400, and a body
// padded well past the 64 KiB cap is 413.
func TestDraft_RejectsMalformedAndOversizedBodies(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	t.Run("malformed JSON", func(t *testing.T) {
		resp := doRequest(t, mutationRequest(t, srv, "/draft", `{not json`))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		body := fmt.Sprintf(`{"ticket":%d,"text":"hi","bogus":true}`, ticketID)
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("text over 8000 characters", func(t *testing.T) {
		body := fmt.Sprintf(`{"ticket":%d,"text":%q}`, ticketID, strings.Repeat("x", 8001))
		resp := doRequest(t, mutationRequest(t, srv, "/draft", body))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
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
			resp := doRequest(t, mutationRequest(t, srv, "/draft", tc.body))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestSend_SucceedsThenConflictsWhenEmpty proves POST /send's happy path
// (204, drafts flip to sent) and that sending again with nothing left
// drafted is 409 Empty (design section 6.7, 7.1).
func TestSend_SucceedsThenConflictsWhenEmpty(t *testing.T) {
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

	sendBody := fmt.Sprintf(`{"ticket":%d}`, ticketID)
	sendResp := doRequest(t, mutationRequest(t, srv, "/send", sendBody))
	_ = sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusNoContent {
		t.Fatalf("first POST /send status = %d, want 204", sendResp.StatusCode)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %q, want nil (wait cleared)", *ticket.WaitingOn)
	}

	sendResp2 := doRequest(t, mutationRequest(t, srv, "/send", sendBody))
	defer func() { _ = sendResp2.Body.Close() }()
	if sendResp2.StatusCode != http.StatusConflict {
		t.Fatalf("second POST /send status = %d, want 409 (nothing left to send)", sendResp2.StatusCode)
	}
}

// TestDraft_DoesNotPublishWhileSendDoes proves handleDraft's review fix
// (answer.go's doc comment): a POST /draft that saves cleanly must not wake
// the bus, since a draft renders nothing server-side and the resulting
// /stream re-render would strip the client-only ".picked" highlight and
// collapse the open question group after every pick. POST /send against the
// same ticket, by contrast, still publishes once the batch actually sends.
func TestDraft_DoesNotPublishWhileSendDoes(t *testing.T) {
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

	sendResp := doRequest(t, mutationRequest(t, srv, "/send", fmt.Sprintf(`{"ticket":%d}`, ticketID)))
	_ = sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /send status = %d, want 204", sendResp.StatusCode)
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
