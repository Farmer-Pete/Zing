// stop_test.go is fix 3's own verify-by: POST /stop (design section 6.11,
// 7.1), the s/S keyboard keys' backend, which had no registered route at
// all before this fix (a 404, keyboard.mjs's dispatch table notwithstanding).
package console_test

import (
	"net/http"
	"strconv"
	"testing"

	"zing/internal/bus"
)

// TestStop_AllSetsStoppedFlagAndReturnsNoContent proves POST /stop
// {"all": true} sets the store's persisted "stopped" flag and answers 204
// (design section 6.11, 7.1).
func TestStop_AllSetsStoppedFlagAndReturnsNoContent(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	_, stoppedBefore, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags (before): %v", err)
	}
	if stoppedBefore {
		t.Fatal("stopped flag already set before POST /stop, want false")
	}

	resp := doRequest(t, mutationRequest(t, srv, "/stop", `{"all":true}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /stop {all:true} status = %d, want 204", resp.StatusCode)
	}

	_, stoppedAfter, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags (after): %v", err)
	}
	if !stoppedAfter {
		t.Error("stopped flag after POST /stop {all:true} = false, want true")
	}
}

// TestStop_TicketLogsAndReturnsNoContent proves POST /stop {"ticket": N}
// answers 204 without touching the "stopped" flag: Package 4 has no
// per-ticket stop column (that lands with Package 5's orchestrator), so the
// handler only logs the request rather than inventing one.
func TestStop_TicketLogsAndReturnsNoContent(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	body := `{"ticket":` + strconv.FormatInt(ticketID, 10) + `}`
	resp := doRequest(t, mutationRequest(t, srv, "/stop", body))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /stop {ticket:N} status = %d, want 204", resp.StatusCode)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("stopped flag set by a per-ticket POST /stop, want it untouched (false)")
	}
}

// TestStop_RejectsBodyNamingNeither proves a body with neither all=true nor
// a positive ticket is rejected with 400 (design section 6.11: "Reject a
// body naming neither").
func TestStop_RejectsBodyNamingNeither(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	resp := doRequest(t, mutationRequest(t, srv, "/stop", `{}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /stop {} status = %d, want 400", resp.StatusCode)
	}
}

// TestStop_RejectsMalformedBody proves a malformed JSON body is rejected
// with 400, matching every other mutation handler's decodeStrict use.
func TestStop_RejectsMalformedBody(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	resp := doRequest(t, mutationRequest(t, srv, "/stop", `{not json`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /stop with a malformed body status = %d, want 400", resp.StatusCode)
	}
}
