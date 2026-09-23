// control_test.go is Task 10's own verify-by: control.go's POST /loglevel
// and POST /debug over a real HTTP server and a real store (design section
// 6.12, 7.1, 12 row 10), plus the Log rail's tail wired to the ring
// (design section 6.11).
package console_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
)

// TestLogLevel_ChangesLiveAndWritesSetting proves POST /loglevel (design
// section 6.12, 7.1): a valid level changes the running handler's LevelVar
// at once and persists to settings.log_level in the same call, 204 on
// success.
func TestLogLevel_ChangesLiveAndWritesSetting(t *testing.T) {
	s := newConsoleTestStore(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	log := console.NewHandler(&bytes.Buffer{}, lv)
	srv, _ := newMutationTestServer(t, s, bus.New(), log)

	resp := doRequest(t, mutationRequest(t, srv, "/loglevel", `{"level":"debug"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /loglevel status = %d, want 204", resp.StatusCode)
	}

	if lv.Level() != slog.LevelDebug {
		t.Errorf("LevelVar.Level() = %v, want debug", lv.Level())
	}

	stored, ok, err := s.GetSetting(t.Context(), "log_level")
	if err != nil {
		t.Fatalf("GetSetting(log_level): %v", err)
	}
	if !ok || stored != "debug" {
		t.Errorf("GetSetting(log_level) = (%q, %v), want (debug, true)", stored, ok)
	}
}

// TestLogLevel_RejectsAnUnknownLevel proves POST /loglevel's closed set
// (design section 6.12: "debug/info/warn/error"): a level outside it is
// rejected with 400, and neither the LevelVar nor the setting changes.
func TestLogLevel_RejectsAnUnknownLevel(t *testing.T) {
	s := newConsoleTestStore(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	log := console.NewHandler(&bytes.Buffer{}, lv)
	srv, _ := newMutationTestServer(t, s, bus.New(), log)

	resp := doRequest(t, mutationRequest(t, srv, "/loglevel", `{"level":"trace"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /loglevel with an unknown level status = %d, want 400", resp.StatusCode)
	}
	if lv.Level() != slog.LevelInfo {
		t.Errorf("LevelVar.Level() = %v, want it unchanged (info)", lv.Level())
	}

	stored, ok, err := s.GetSetting(t.Context(), "log_level")
	if err != nil {
		t.Fatalf("GetSetting(log_level): %v", err)
	}
	if !ok || stored != "info" {
		t.Errorf("GetSetting(log_level) = (%q, %v), want the seeded default (info, true) unchanged", stored, ok)
	}
}

// TestDebug_TogglesPerTicketDebugSet proves POST /debug (design section
// 6.12, 7.1): the first call turns a ticket's debug override on -- a
// debug-level line carrying that ticket_id, which the level alone would
// gate out, is now emitted -- and the second call turns it back off.
func TestDebug_TogglesPerTicketDebugSet(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	buf := &bytes.Buffer{}
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo) // above debug, so an undebugged ticket's debug line is gated out
	log := console.NewHandler(buf, lv)
	srv, _ := newMutationTestServer(t, s, bus.New(), log)
	logger := slog.New(log)

	logger.Debug("not yet debugged", "ticket_id", ticketID)
	if buf.Len() != 0 {
		t.Fatalf("before POST /debug: sink = %q, want empty", buf.String())
	}

	body := `{"ticket":` + strconv.FormatInt(ticketID, 10) + `}`
	resp := doRequest(t, mutationRequest(t, srv, "/debug", body))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first POST /debug status = %d, want 204", resp.StatusCode)
	}

	logger.Debug("now debugged", "ticket_id", ticketID)
	if !strings.Contains(buf.String(), "now debugged") {
		t.Errorf("after first POST /debug: sink = %q, want the debug line emitted", buf.String())
	}

	buf.Reset()
	resp2 := doRequest(t, mutationRequest(t, srv, "/debug", body))
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second POST /debug status = %d, want 204", resp2.StatusCode)
	}

	logger.Debug("debugged again after toggling off", "ticket_id", ticketID)
	if buf.Len() != 0 {
		t.Errorf("after second POST /debug (toggled off): sink = %q, want empty", buf.String())
	}
}

// TestDebug_RejectsMalformedBody proves POST /debug's transport-layer check
// (design section 7.1: 400 on a bad body), matching every other mutation
// handler's decodeStrict use.
func TestDebug_RejectsMalformedBody(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	resp := doRequest(t, mutationRequest(t, srv, "/debug", `{"ticket":0}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /debug with ticket=0 status = %d, want 400", resp.StatusCode)
	}
}

// TestLogTail_FiltersByOpenTicketRunIDs proves the Log rail (design section
// 6.11): the ring buffer's entries for the open ticket's own run_ids, and
// only those -- a line carrying a different ticket's run_id must not leak
// into this ticket's rail.
func TestLogTail_FiltersByOpenTicketRunIDs(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketA := seedTicket(t, s, "fake#1", "Ticket A")
	ticketB := seedTicket(t, s, "fake#2", "Ticket B")
	advanceTicketToBuilding(t, s, ticketA, "sonnet", 1)
	advanceTicketToBuilding(t, s, ticketB, "sonnet", 1)

	runsA, err := s.RunsForTicket(t.Context(), ticketA)
	if err != nil || len(runsA) != 1 {
		t.Fatalf("RunsForTicket(A) = %v, %v, want exactly one run", runsA, err)
	}
	runsB, err := s.RunsForTicket(t.Context(), ticketB)
	if err != nil || len(runsB) != 1 {
		t.Fatalf("RunsForTicket(B) = %v, %v, want exactly one run", runsB, err)
	}

	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	log := console.NewHandler(&bytes.Buffer{}, lv)
	logger := slog.New(log)
	logger.Info("line belonging to ticket A", "run_id", runsA[0].ID)
	logger.Info("line belonging to ticket B", "run_id", runsB[0].ID)

	srv := newTestServer(t, s, bus.New(), testMachine(t), log)

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketA, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail := readInitialFrames(t, r)

	if !strings.Contains(rail, "line belonging to ticket A") {
		t.Errorf("rail missing ticket A's own log line; got:\n%s", rail)
	}
	if strings.Contains(rail, "line belonging to ticket B") {
		t.Errorf("rail leaked ticket B's log line into ticket A's rail; got:\n%s", rail)
	}
}

// TestFencedAttrThroughTheWiredHandler proves a fenced value still never
// logs raw once routed through the handler console.New actually wires in
// (design section 6.12, §16): the Task 5 coverage (log_test.go) proves this
// against a bare Handler; this proves the wiring did not change it.
func TestFencedAttrThroughTheWiredHandler(t *testing.T) {
	buf := &bytes.Buffer{}
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	log := console.NewHandler(buf, lv)

	const secret = "sk-super-secret-token-do-not-log"
	slog.New(log).Debug("body received", console.FencedAttr("body", secret))

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("sink = %q, must never contain the raw fenced value", out)
	}
	if !strings.Contains(out, "len="+strconv.Itoa(len(secret))) || !strings.Contains(out, "sha256=") {
		t.Errorf("sink = %q, want the fenced value's length and sha256", out)
	}
}
