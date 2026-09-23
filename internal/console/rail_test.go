// rail_test.go is Task 9's own verify-by: a rails render test over a real
// store and a real machine (design section 6.11, 12 row 9), plus POST
// /side's contract (design section 6.11, 7.1).
package console_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/response"
	"zing/internal/store"
)

// testWaitingGate is the "gate" waiting_on reason, one of the six
// question-backed reasons SendBatch clears (design section 6.7); rail_test
// uses it as any non-nil waiting_on to prove the phase rail's "waiting"
// pill, not because the gate kind is otherwise relevant here.
const testWaitingGate = "gate"

// advanceTicketToBuilding claims ticketID and commits one transition to
// "building" with waiting_on set and one session-and-run, through the same
// store.CommitHandlerResult seam commit.go's own producers use (design
// section 6.7, 6.11): a real fenced write, not a raw SQL poke, so this
// fixture is a row a real handler could have written. Every caller reads
// the session or run back through the store (SessionsForTicket,
// RunsForTicket) rather than a returned id, so this reports only the
// t.Fatalf failures above.
func advanceTicketToBuilding(t *testing.T, s *store.Store, ticketID int64, model string, agentSeconds int) {
	t.Helper()

	const owner = "rail-test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	waiting := testWaitingGate
	outcome := "ok"
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: string(response.TicketStateBuilding), Reason: "rail test advance",
		Waiting: &waiting,
		Session: &store.SessionUpsert{Job: string(response.TicketStateBuilding), Runtime: "fake"},
		Runs:    []store.Run{{Turn: 0, Model: &model, Outcome: &outcome, AgentSeconds: &agentSeconds}},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	sessions, err := s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("SessionsForTicket: no session found after CommitHandlerResult")
	}
}

// railScenarioPayload is a minimal, schema-valid scenario artifact payload
// (the same shape console_reads_test.go's own scenario fixture uses):
// InsertArtifact validates every payload against its schema, so rail_test's
// fixture must satisfy scenario.json, not just be well-formed JSON.
func railScenarioPayload(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`)
}

// railPlanPayload marshals plan_test.go's fixturePlan(), the one fully
// populated, schema-valid response.Plan this package already builds for the
// plan-renderer test, so rail_test's Plan artifact is a real row a planning
// commit could have written.
func railPlanPayload(t *testing.T) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(fixturePlan())
	if err != nil {
		t.Fatalf("marshal fixturePlan: %v", err)
	}
	return payload
}

// TestRail_PhaseArtifactsAndRun proves the three store-backed rail sections
// design section 6.11 names (Log is Task 10's, Side is covered by
// TestPostSide below): the phase rail marks done/now/upcoming and shows
// "waiting" when waiting_on is set; the artifacts rail shows a present
// artifact's version and an absent slot's "after <phase>" placeholder; the
// run rail shows the newest session's newest run's values and "-" for
// Worktree and Branch, which this package cannot supply (design section
// 6.11: "arrive with Package 5").
func TestRail_PhaseArtifactsAndRun(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "plan", Version: 1, Payload: railPlanPayload(t)}); err != nil {
		t.Fatalf("insert plan artifact: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "scenario", Version: 1, Payload: railScenarioPayload("s1")}); err != nil {
		t.Fatalf("insert scenario artifact: %v", err)
	}

	advanceTicketToBuilding(t, s, ticketID, "sonnet", 42)

	srv := httptest.NewServer(console.New(s, bus.New(), testMachine(t), testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail := readInitialFrames(t, r)

	// Phase: queued and planning are before "building" (done), building
	// itself is now with the waiting pill, reviewing/judging/shipping/done
	// are after (upcoming).
	for _, state := range []string{string(response.TicketStateQueued), string(response.TicketStatePlanning)} {
		want := `<span class="phase-dot phase-done" data-phase-state="` + state + `">`
		if !strings.Contains(rail, want) {
			t.Errorf("rail missing done phase dot for %s; want %q in:\n%s", state, want, rail)
		}
	}
	if !strings.Contains(rail, `<span class="phase-dot phase-now" data-phase-state="building">`) {
		t.Errorf("rail missing the now phase dot for building; got:\n%s", rail)
	}
	if !strings.Contains(rail, `<span class="pill pill-waiting">waiting</span>`) {
		t.Errorf("rail missing the waiting pill on the now row; got:\n%s", rail)
	}
	for _, state := range []string{
		string(response.TicketStateReviewing), string(response.TicketStateJudging),
		string(response.TicketStateShipping), string(response.TicketStateDone),
	} {
		want := `<span class="phase-dot phase-upcoming" data-phase-state="` + state + `">`
		if !strings.Contains(rail, want) {
			t.Errorf("rail missing upcoming phase dot for %s; want %q in:\n%s", state, want, rail)
		}
	}

	// Artifacts: Plan and Scenarios are present (v1); Decisions (planreview)
	// and Review report (finding), never seeded, show their "after <phase>"
	// placeholders.
	if !strings.Contains(rail, `<span class="artifact-label">Plan</span>`) ||
		!strings.Contains(rail, `<summary>v1</summary>`) {
		t.Errorf("rail missing a present Plan v1 slot; got:\n%s", rail)
	}
	if !strings.Contains(rail, `<span class="artifact-label">Decisions</span> <span class="empty">after planning</span>`) {
		t.Errorf(`rail missing Decisions' "after planning" placeholder; got:\n%s`, rail)
	}
	if !strings.Contains(rail, `<span class="artifact-label">Review report</span> <span class="empty">after reviewing</span>`) {
		t.Errorf(`rail missing Review report's "after reviewing" placeholder; got:\n%s`, rail)
	}

	// Run: the newest (only) session's newest (only) run.
	for _, want := range []string{
		"<dt>Model</dt><dd>sonnet</dd>",
		"<dt>Agent time</dt><dd>42s</dd>",
		"<dt>Attempts</dt><dd>1</dd>",
		"<dt>Worktree</dt><dd>-</dd>",
		"<dt>Branch</dt><dd>-</dd>",
	} {
		if !strings.Contains(rail, want) {
			t.Errorf("rail run section missing %q; got:\n%s", want, rail)
		}
	}
}

// TestRail_NoSessionRendersAllDashes proves a ticket with no session yet
// (every field this package cannot supply) renders every Run field as "-"
// rather than panicking on an empty SessionsForTicket result.
func TestRail_NoSessionRendersAllDashes(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := httptest.NewServer(console.New(s, bus.New(), testMachine(t), testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken))
	defer srv.Close()

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail := readInitialFrames(t, r)

	for _, want := range []string{
		"<dt>Model</dt><dd>-</dd>",
		"<dt>Agent time</dt><dd>-</dd>",
		"<dt>Attempts</dt><dd>-</dd>",
		"<dt>Worktree</dt><dd>-</dd>",
		"<dt>Branch</dt><dd>-</dd>",
	} {
		if !strings.Contains(rail, want) {
			t.Errorf("rail run section missing %q; got:\n%s", want, rail)
		}
	}
}

// TestPostSide_ReturnsTheFixedReplyAndWritesNoMessage proves POST /side's
// full contract (design section 6.11, 7.1): it answers with the one fixed
// inert reply rendered into the rail's #side-reply element, behind the
// mutation guard like every other state-changing route, and it writes no
// message row -- the side agent is Package 10's job, not this one's.
func TestPostSide_ReturnsTheFixedReplyAndWritesNoMessage(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	before, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages (before): %v", err)
	}

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))
	req := mutationRequest(t, srv, "/side", `{"ticket":`+strconv.FormatInt(ticketID, 10)+`,"text":"what should I ask?"}`)
	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /side status = %d, want 200", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST /side body: %v", err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "The side agent arrives in Package 10.") {
		t.Errorf("POST /side body missing the fixed reply; got:\n%s", body)
	}
	if !strings.Contains(body, `id="side-reply"`) {
		t.Errorf("POST /side body missing #side-reply; got:\n%s", body)
	}

	after, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages (after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("POST /side wrote %d message row(s), want 0 (before=%d, after=%d)", len(after)-len(before), len(before), len(after))
	}
}

// TestPostSide_RejectsCrossOrigin proves /side sits behind the same
// mutation guard as every other state-changing route (design section 6.11:
// "Apply the mutation middleware to /side" is this package's own hard
// constraint, not a design section quote).
func TestPostSide_RejectsCrossOrigin(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))
	req := mutationRequest(t, srv, "/side", `{"ticket":`+strconv.FormatInt(ticketID, 10)+`,"text":"hi"}`)
	req.Header.Set("Origin", "http://evil.example")
	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /side cross-origin status = %d, want 403", resp.StatusCode)
	}
}
