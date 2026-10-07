// rail_test.go is Task 9's own verify-by: a rails render test over a real
// store and a real machine (design section 6.11, 12 row 9), plus POST
// /side's contract (design section 6.11, 7.1).
package console_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/dispatch"
	"zing/internal/proc"
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
		Session: &store.SessionUpsert{Job: string(response.TicketStateBuilding), Runtime: testRuntimeFake},
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
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypePlan, Version: 1, Payload: railPlanPayload(t)}); err != nil {
		t.Fatalf("insert plan artifact: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypeScenario, Version: 1, Payload: railScenarioPayload("s1")}); err != nil {
		t.Fatalf("insert scenario artifact: %v", err)
	}

	advanceTicketToBuilding(t, s, ticketID, "sonnet", 42)

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

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

// TestRail_ScenariosVersionShowsPlanCohortNotRowCount proves a bug the owner
// found live (ticket 1): the Scenarios slot showed its own scenario
// artifact's Version, which store.InsertArtifact assigns per row -- "next
// past (ticket, type)'s current maximum" -- so it climbs by one for every
// scenario ever written across every planning turn, not once per cohort.
// Plan and Decisions show the cohort number instead, since planning writes
// exactly one row of each per turn, so the two drift apart: the owner saw
// "Scenarios v40" next to "Plan v5". The fix is the plan cohort version the
// newest scenario's own run_id belongs to (readyArtifacts writes a cohort's
// plan, claims, and scenario rows together under one run id), which this
// seeds at a small scale: cohort 1 (run 4, plan v1) writes scenario rows
// v1-v3; cohort 2 (run 6, plan v2) writes v4-v5. The newest scenario row's
// own version is 5, but it belongs to plan v2, so the rail must show
// "Scenarios v2", matching Plan, not "Scenarios v5".
func TestRail_ScenariosVersionShowsPlanCohortNotRowCount(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#2", "Scenario cohort version")

	// artifacts.run_id references a real runs row, so this needs two actual
	// runs, not two bare literals: advanceTicketToBuilding claims and commits
	// one session-and-run each call (the claim it releases on commit, so a
	// second call on the same ticket succeeds), and RunsForTicket reads their
	// ids back in insertion order.
	advanceTicketToBuilding(t, s, ticketID, "sonnet", 1)
	advanceTicketToBuilding(t, s, ticketID, "opus", 1)
	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("RunsForTicket returned %d runs, want 2", len(runs))
	}
	run1, run2 := runs[0].ID, runs[1].ID

	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypePlan, Version: 1, RunID: &run1, Payload: railPlanPayload(t)}); err != nil {
		t.Fatalf("insert plan v1: %v", err)
	}
	for i, v := range []int{1, 2, 3} {
		id := fmt.Sprintf("s%d", i+1)
		if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypeScenario, Version: v, RunID: &run1, Payload: railScenarioPayload(id)}); err != nil {
			t.Fatalf("insert scenario v%d: %v", v, err)
		}
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypePlan, Version: 2, RunID: &run2, Payload: railPlanPayload(t)}); err != nil {
		t.Fatalf("insert plan v2: %v", err)
	}
	for i, v := range []int{4, 5} {
		id := fmt.Sprintf("s%d", i+4)
		if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypeScenario, Version: v, RunID: &run2, Payload: railScenarioPayload(id)}); err != nil {
			t.Fatalf("insert scenario v%d: %v", v, err)
		}
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	if !strings.Contains(rail, `<span class="artifact-label">Plan</span>`) || !strings.Contains(rail, `<summary>v2</summary>`) {
		t.Fatalf("rail missing Plan v2 (test setup); got:\n%s", rail)
	}

	slot := regexp.MustCompile(`(?s)artifact-label">Scenarios</span>.*?</div>`).FindString(rail)
	if slot == "" {
		t.Fatalf("rail missing the Scenarios slot; got:\n%s", rail)
	}
	if !strings.Contains(slot, "<summary>v2</summary>") {
		t.Errorf("Scenarios slot = %q, want it to show v2 (the plan cohort its rows belong to), not its own row version", slot)
	}
}

// TestRail_PlanArtifactRendersThroughPlanRendererNotRawJSON proves bug fix
// 16: the rail's Plan slot used to show its stored artifact as raw indented
// JSON (prettyPayload), the owner's locked-view complaint. It now renders
// through the same RenderPlan path the gate's own context region uses
// (views.go's loadPlan, TestThreadGateContextRendersStoredPlan), so the
// disclosure shows the plan's own headings and prose instead of braces and
// quoted field names. Every other slot (Scenarios here) is unaffected and
// still shows its raw payload.
func TestRail_PlanArtifactRendersThroughPlanRendererNotRawJSON(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypePlan, Version: 1, Payload: railPlanPayload(t)}); err != nil {
		t.Fatalf("insert plan artifact: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: testArtifactTypeScenario, Version: 1, Payload: railScenarioPayload("s1")}); err != nil {
		t.Fatalf("insert scenario artifact: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	if !strings.Contains(rail, "<h2>Overview</h2>") || !strings.Contains(rail, "Ship a plan renderer that drops nothing.") {
		t.Errorf("rail's Plan slot did not render through RenderPlan; got:\n%s", rail)
	}
	if strings.Contains(rail, `"overview"`) || strings.Contains(rail, `"objective"`) {
		t.Errorf("rail's Plan slot still shows raw JSON field names; got:\n%s", rail)
	}
	// The Scenarios slot, never given a RenderedHTML, still shows its raw
	// payload: the fix is scoped to the Plan slot alone.
	if !strings.Contains(rail, `class="artifact-payload"`) || !strings.Contains(rail, "behavior") {
		t.Errorf("rail's Scenarios slot lost its raw payload view; got:\n%s", rail)
	}
}

// TestRail_NoSessionRendersAllDashes proves a ticket with no session yet
// (every field this package cannot supply) renders every Run field as "-"
// rather than panicking on an empty SessionsForTicket result.
func TestRail_NoSessionRendersAllDashes(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

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

// TestRail_RunInterruptedShowsLabel proves #45 design section 9:
// buildRunRail (rail.go) reads the newest run's own store.Run.Interrupted,
// and rail.templ renders the word "interrupted" right after Model when it
// is set. The run is reserved, then interrupted for real through
// InterruptRuns (section 5.3), the same path a shutdown or a dead-serve
// reclaim drives in production, rather than set the column directly:
// CommitHandlerResult's own Runs field never writes Interrupted (D6, design
// section 7.2), only InterruptRuns and ReclaimClaim do.
func TestRail_RunInterruptedShowsLabel(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	advanceTicketToBuilding(t, s, ticketID, "sonnet", 42) // one terminal run, so a newest session already exists

	const owner = "rail-interrupt-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	if _, reserveErr := s.Reserve(t.Context(), ticketID, owner, expires,
		store.SessionUpsert{Job: string(response.TicketStateBuilding), Runtime: testRuntimeFake},
		store.RunSeed{Model: "opus-interrupted"},
	); reserveErr != nil {
		t.Fatalf("Reserve: %v", reserveErr)
	}
	applied, err := s.InterruptRuns(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	if !strings.Contains(rail, `<dd>opus-interrupted<span class="pill pill-interrupted">interrupted</span></dd>`) {
		t.Errorf("rail run section missing the interrupted label right after Model; got:\n%s", rail)
	}
}

// ---- Run rail's run list (#43 split, run evidence) ---------------------

// advanceTicketWithRuns is advanceTicketToBuilding generalized to any job,
// runtime and set of runs: it claims ticketID, then commits one transition
// to next with a brand-new session (job, runtime) and every entry of runs
// inserted under it, through the same store.CommitHandlerResult seam
// commit.go's own producers use. Calling it again on the same ticket
// creates a second session (store.upsertSessionTx inserts whenever
// SessionUpsert.ID is nil), which is the point: TestRail_RunListShowsEveryRun
// needs two sessions with different jobs and runtimes.
func advanceTicketWithRuns(t *testing.T, s *store.Store, ticketID int64, next, job, runtime string, runs []store.Run) {
	t.Helper()

	owner := fmt.Sprintf("rail-test-owner-%s-%s", job, runtime)
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	waiting := testWaitingGate
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: next, Reason: "rail test advance with runs",
		Waiting: &waiting,
		Session: &store.SessionUpsert{Job: job, Runtime: runtime},
		Runs:    runs,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
}

// runRowHTML returns the <li class="run-row" data-run-id="runID">...</li>
// element the rail's run list rendered for runID, failing the test if the
// rail holds none.
func runRowHTML(t *testing.T, rail string, runID int64) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)<li class="run-row" data-run-id="` + strconv.FormatInt(runID, 10) + `">.*?</li>`)
	row := pattern.FindString(rail)
	if row == "" {
		t.Fatalf("rail missing a run-row for run %d; got:\n%s", runID, rail)
	}
	return row
}

// runRowHref returns the href attribute of the <a class="class"> element
// inside row, failing the test if row holds none.
func runRowHref(t *testing.T, row, class string) string {
	t.Helper()
	pattern := regexp.MustCompile(`class="` + class + `" href="([^"]+)"`)
	m := pattern.FindStringSubmatch(row)
	if m == nil {
		t.Fatalf("row missing an href for class %q; got:\n%s", class, row)
	}
	return m[1]
}

// TestRail_RunEvidenceLinksServeStoredText is the design's demo test: it
// stores a run's final message and stderr path through
// Store.RecordRunEvidence, opens the ticket's thread stream, reads the run
// row's two hrefs out of the rail, and fetches both, proving that stored
// evidence reaches the owner through the rail rather than just the store.
func TestRail_RunEvidenceLinksServeStoredText(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	const stderrText = "boom: something failed\n"
	stderrPath := writeRunStderrFile(t, s, runID, stderrText)
	final := "the agent said this"
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{FinalMessage: &final, StderrPath: &stderrPath}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	row := runRowHTML(t, rail, runID)
	finalHref := runRowHref(t, row, "run-final")
	stderrHref := runRowHref(t, row, "run-stderr")

	finalResp, err := http.Get(srv.URL + finalHref) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s: %v", finalHref, err)
	}
	defer func() { _ = finalResp.Body.Close() }()
	if finalResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", finalHref, finalResp.StatusCode)
	}
	finalBody, err := io.ReadAll(finalResp.Body)
	if err != nil {
		t.Fatalf("read final body: %v", err)
	}
	if string(finalBody) != final {
		t.Errorf("final body = %q, want %q", finalBody, final)
	}

	stderrResp, err := http.Get(srv.URL + stderrHref) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s: %v", stderrHref, err)
	}
	defer func() { _ = stderrResp.Body.Close() }()
	if stderrResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", stderrHref, stderrResp.StatusCode)
	}
	stderrBody, err := io.ReadAll(stderrResp.Body)
	if err != nil {
		t.Fatalf("read stderr body: %v", err)
	}
	if string(stderrBody) != stderrText {
		t.Errorf("stderr body = %q, want %q", stderrBody, stderrText)
	}
}

// TestRail_RunListShowsEveryRun proves the rail's run list shows every run
// of the ticket across every session, newest first, each with its own
// session's job and runtime and the run's own model -- and that only the
// run carrying evidence gets final/stderr links.
func TestRail_RunListShowsEveryRun(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	// planningModel deliberately avoids the literal "sonnet": that string
	// already has two other non-call occurrences in this package
	// (TestBuildLogRail_TiesKeepRunAppendOrder's own model var, and
	// resume_e2e_test.go's model-alias map), and goconst (make lint) flags
	// a third.
	planningModel, judgeModel1, judgeModel2 := "sonnet-plan", "gpt-5", "gpt-5-mini"
	outcomeOK := "ok"
	secs5, secs7, secs9 := 5, 7, 9
	advanceTicketWithRuns(t, s, ticketID, string(response.TicketStatePlanning), "planning", "claude", []store.Run{
		{Turn: 0, Model: &planningModel, Outcome: &outcomeOK, AgentSeconds: &secs5},
	})
	advanceTicketWithRuns(t, s, ticketID, string(response.TicketStateJudging), "judge", "codex", []store.Run{
		{Turn: 0, Model: &judgeModel1, Outcome: &outcomeOK, AgentSeconds: &secs7},
		{Turn: 1, Model: &judgeModel2, Outcome: &outcomeOK, AgentSeconds: &secs9},
	})

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("RunsForTicket returned %d runs, want 3", len(runs))
	}
	planningRun, judgeRun1, judgeRun2 := runs[0], runs[1], runs[2]

	final := "judge's final word"
	stderrPath := writeRunStderrFile(t, s, judgeRun2.ID, "judge stderr")
	if err := s.RecordRunEvidence(t.Context(), judgeRun2.ID, store.RunEvidence{FinalMessage: &final, StderrPath: &stderrPath}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	ids := regexp.MustCompile(`data-run-id="(\d+)"`).FindAllStringSubmatch(rail, -1)
	if len(ids) != 3 {
		t.Fatalf("rail has %d run-row elements, want 3; got:\n%s", len(ids), rail)
	}
	wantOrder := []int64{judgeRun2.ID, judgeRun1.ID, planningRun.ID}
	for i, want := range wantOrder {
		got, convErr := strconv.ParseInt(ids[i][1], 10, 64)
		if convErr != nil {
			t.Fatalf("parse run id %q: %v", ids[i][1], convErr)
		}
		if got != want {
			t.Errorf("run-row[%d] id = %d, want %d (newest first)", i, got, want)
		}
	}

	planningRow := runRowHTML(t, rail, planningRun.ID)
	for _, want := range []string{
		`<span class="run-job">planning</span>`,
		`<span class="run-runtime">claude</span>`,
		`<span class="run-model">sonnet-plan</span>`,
	} {
		if !strings.Contains(planningRow, want) {
			t.Errorf("planning run row missing %q; got:\n%s", want, planningRow)
		}
	}
	if strings.Contains(planningRow, `href="/runs/`) {
		t.Errorf("planning run row should carry no evidence links; got:\n%s", planningRow)
	}

	judgeRow2 := runRowHTML(t, rail, judgeRun2.ID)
	for _, want := range []string{
		`<span class="run-job">judge</span>`,
		`<span class="run-runtime">codex</span>`,
		`<span class="run-model">gpt-5-mini</span>`,
		fmt.Sprintf(`href="/runs/%d/final"`, judgeRun2.ID),
		fmt.Sprintf(`href="/runs/%d/stderr"`, judgeRun2.ID),
	} {
		if !strings.Contains(judgeRow2, want) {
			t.Errorf("newest judge run row missing %q; got:\n%s", want, judgeRow2)
		}
	}

	judgeRow1 := runRowHTML(t, rail, judgeRun1.ID)
	if strings.Contains(judgeRow1, `href="/runs/`) {
		t.Errorf("older judge run row should carry no evidence links; got:\n%s", judgeRow1)
	}
}

// TestRail_RunListShowsTranscriptPath proves a run whose transcript_path is
// set renders that path as plain text, with no anchor around it: design's
// nongoal "Serving or rendering transcripts" -- transcripts live under
// ~/.claude/projects, outside the data directory.
func TestRail_RunListShowsTranscriptPath(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	const transcript = "/home/owner/.claude/projects/-work-zing/abc123.jsonl"
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{TranscriptPath: new(transcript)}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	row := runRowHTML(t, rail, runID)
	if !strings.Contains(row, `class="run-transcript"`) || !strings.Contains(row, transcript) {
		t.Errorf("run row missing the transcript path as text; got:\n%s", row)
	}
	if strings.Contains(row, "<a ") {
		t.Errorf("run row with only a transcript path should carry no anchor; got:\n%s", row)
	}
}

// TestRail_CodexTranscriptLinked proves a run whose transcript_path is this
// run's own Zing-written stdout file (the shape job.writeTranscriptFile
// produces) renders an anchor to GET /runs/{id}/transcript, unlike a
// Claude-style path outside the data directory, which TestRail_
// RunListShowsTranscriptPath keeps covering as plain text with no anchor.
func TestRail_CodexTranscriptLinked(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	path := writeRunTranscriptFile(t, s, runID, `{"type":"thread.started"}`+"\n")
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{TranscriptPath: &path}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	row := runRowHTML(t, rail, runID)
	href := runRowHref(t, row, "run-transcript")
	want := fmt.Sprintf("/runs/%d/transcript", runID)
	if href != want {
		t.Errorf("transcript href = %q, want %q", href, want)
	}
}

// railHTML opens ticketID's thread stream against srvURL, reads the
// initial frames, closes the response, and returns the rail HTML. Tests
// that need to read the rail more than once in a row use this instead of
// repeating the open/read/close sequence inline.
func railHTML(t *testing.T, srvURL string, ticketID int64) string {
	t.Helper()
	resp, r, cancel := openStream(t, srvURL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)
	return rail
}

// TestRail_RunListShowsRunningAndInterrupted proves a run that was reserved
// but never terminalized renders outcome "running" (runs.outcome is NULL),
// and that once InterruptRuns terminalizes it (#45: outcome becomes "error"
// and interrupted becomes true), the run list's own row shows the
// interrupted pill too, not just the old single-run Model field.
func TestRail_RunListShowsRunningAndInterrupted(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	const owner = "rail-running-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	reserved, err := s.Reserve(t.Context(), ticketID, owner, expires,
		store.SessionUpsert{Job: string(response.TicketStateBuilding), Runtime: testRuntimeFake},
		store.RunSeed{Model: "running-model"},
	)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	row := runRowHTML(t, railHTML(t, srv.URL, ticketID), reserved.RunID)
	if !strings.Contains(row, `<span class="run-outcome">running</span>`) {
		t.Errorf("run row missing outcome running; got:\n%s", row)
	}
	if strings.Contains(row, "pill-interrupted") {
		t.Errorf("run row should not show interrupted yet; got:\n%s", row)
	}

	applied, err := s.InterruptRuns(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("InterruptRuns: %v", err)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	row = runRowHTML(t, railHTML(t, srv.URL, ticketID), reserved.RunID)
	if !strings.Contains(row, `<span class="pill pill-interrupted">interrupted</span>`) {
		t.Errorf("run row missing the interrupted pill; got:\n%s", row)
	}
}

// logLine calls console.Handler.Handle directly with a hand-built
// slog.Record so the entry lands at an exact, caller-chosen Time -- the
// equal-Time collision TestBuildLogRail_TiesKeepRunAppendOrder needs, which
// logging through a *slog.Logger cannot arrange since that stamps Time from
// slog's own clock at the call site.
func logLine(t *testing.T, h *console.Handler, at time.Time, msg string, runID int64) {
	t.Helper()
	r := slog.NewRecord(at, slog.LevelInfo, msg, 0)
	r.AddAttrs(slog.Int64("run_id", runID))
	if err := h.Handle(t.Context(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// TestBuildLogRail_TiesKeepRunAppendOrder proves buildLogRail's merge across
// runs breaks an equal-Time tie by run order then append order
// (rail.go's slices.SortStableFunc), not the unspecified order a plain,
// non-stable sort would allow.
func TestBuildLogRail_TiesKeepRunAppendOrder(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	const owner = "rail-log-tiebreak-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	waiting := testWaitingGate
	model := "sonnet"
	outcome := "ok"
	agentSeconds := 1
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: string(response.TicketStateBuilding), Reason: "rail log-tiebreak test",
		Waiting: &waiting,
		Session: &store.SessionUpsert{Job: string(response.TicketStateBuilding), Runtime: testRuntimeFake},
		Runs: []store.Run{
			{Turn: 0, Model: &model, Outcome: &outcome, AgentSeconds: &agentSeconds},
			{Turn: 1, Model: &model, Outcome: &outcome, AgentSeconds: &agentSeconds},
		},
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("RunsForTicket = %d runs, want 2", len(runs))
	}

	// Both runs log at the same seven instants, an overlapping window real
	// concurrent runs produce, with run 0's whole block appended before run
	// 1's (buildLogRail's own per-run append order). This specific shape --
	// seven shared instants, not two or three -- is load-bearing: a plain
	// slices.SortFunc leaves a short tied run undisturbed (its introsort
	// falls back to insertion sort, or its already-sorted-run detection
	// short-circuits, for anything much smaller than this), so a shorter
	// repro would pass against the very bug this test exists to catch.
	logHandler := newTestLogHandler(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const tiedInstants = 7
	for i := range tiedInstants {
		at := base.Add(time.Duration(i) * time.Second)
		logLine(t, logHandler, at, fmt.Sprintf("run0-line-%d", i), runs[0].ID)
	}
	for i := range tiedInstants {
		at := base.Add(time.Duration(i) * time.Second)
		logLine(t, logHandler, at, fmt.Sprintf("run1-line-%d", i), runs[1].ID)
	}

	srv := newTestServer(t, s, bus.New(), testMachine(t), logHandler)

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, _, rail, _ := readInitialFrames(t, r)

	for i := range tiedInstants {
		run0Msg := fmt.Sprintf("run0-line-%d", i)
		run1Msg := fmt.Sprintf("run1-line-%d", i)
		i0 := strings.Index(rail, run0Msg)
		i1 := strings.Index(rail, run1Msg)
		if i0 < 0 || i1 < 0 {
			t.Fatalf("rail missing %q or %q; got:\n%s", run0Msg, run1Msg, rail)
		}
		if i0 > i1 {
			t.Errorf("rail rendered %q before %q for an equal-Time tie, want run/append order (run 0 before run 1); got:\n%s", run1Msg, run0Msg, rail)
		}
	}
}

// TestPostSide_ReturnsTheFixedReplyAndWritesNoMessage proves POST /side's
// full contract (design section 6.11, 7.1): it answers with the one fixed
// inert reply rendered into the rail's #side-reply element, behind the
// mutation guard like every other state-changing route, and it writes no
// message row -- the side agent is Package 10's job, not this one's.
func TestPostSide_ReturnsTheFixedReplyAndWritesNoMessage(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestBuildLogRail_EmptyStateNamesServerStart proves the bug fix for the
// Log rail reading "No log lines yet." after every `zing serve` restart
// (log.go's ring lives in memory, so a restart always starts it empty,
// which looked broken rather than merely quiet): with no log entries at
// all, the empty state instead names when the server started, computed
// from console.New's own startedAt (server.go), not from the ring.
func TestBuildLogRail_EmptyStateNamesServerStart(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	_, _, rail, _ := readInitialFrames(t, r)

	if strings.Contains(rail, "No log lines yet.") {
		t.Errorf("rail log section still renders the old, unexplained empty state; got:\n%s", rail)
	}
	want := regexp.MustCompile(`No log lines since Zing started at \d{2}:\d{2}\.`)
	if !want.MatchString(rail) {
		t.Errorf("rail log section missing the server-start empty state; got:\n%s", rail)
	}
}

// ---- GET /runs/{id}/{kind} (#43 split, run evidence) -------------------

// oneSeededRun advances ticketID to building once (advanceTicketToBuilding)
// and returns the id of the one run that call writes, failing the test if
// RunsForTicket does not report exactly one more run than before.
func oneSeededRun(t *testing.T, s *store.Store, ticketID int64, model string) int64 {
	t.Helper()
	before, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket (before): %v", err)
	}
	advanceTicketToBuilding(t, s, ticketID, model, 1)
	after, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket (after): %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("RunsForTicket returned %d runs, want %d (one more than before)", len(after), len(before)+1)
	}
	return after[len(after)-1].ID
}

// writeRunStderrFile writes data under s.Dir()/runs/run-<runID>-stderr.log,
// the same path shape job/runjob.go's writeStderrFile produces, and returns
// it.
func writeRunStderrFile(t *testing.T, s *store.Store, runID int64, data string) string {
	t.Helper()
	dir := filepath.Join(s.Dir(), "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, store.StderrFileName(runID))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// writeRunTranscriptFile writes data under
// s.Dir()/runs/run-<runID>-stdout.jsonl, the same path shape
// job/runjob.go's writeTranscriptFile produces, and returns it.
func writeRunTranscriptFile(t *testing.T, s *store.Store, runID int64, data string) string {
	t.Helper()
	dir := filepath.Join(s.Dir(), "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, store.StdoutFileName(runID))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestRunFile_ServesFinalMessage proves GET /runs/{id}/final serves a
// stored final message as plain text, with the headers that keep a browser
// from sniffing it as HTML.
func TestRunFile_ServesFinalMessage(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	final := "the agent said <b>this</b>"
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{FinalMessage: &final}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/final", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/final: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if nos := resp.Header.Get("X-Content-Type-Options"); nos != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nos)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != final {
		t.Errorf("body = %q, want %q", body, final)
	}
}

// TestRunFile_ServesStderrFromDataDir proves GET /runs/{id}/stderr serves
// the stderr file a run's recorded stderr_path names, when it resolves
// inside the store's own data directory.
func TestRunFile_ServesStderrFromDataDir(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	const stderrText = "boom: something failed\n"
	path := writeRunStderrFile(t, s, runID, stderrText)
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{StderrPath: &path}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/stderr", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/stderr: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != stderrText {
		t.Errorf("body = %q, want %q", body, stderrText)
	}
}

// TestRunFile_RefusesPathOutsideDataDir proves GET /runs/{id}/stderr 403s,
// and never leaks the file's text, when the recorded stderr_path resolves
// outside the store's data directory (a shape this route should never see
// in production, but must refuse rather than trust).
func TestRunFile_RefusesPathOutsideDataDir(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	const secretText = "outside secret\n"
	outside := filepath.Join(t.TempDir(), "outside-stderr.log")
	if err := os.WriteFile(outside, []byte(secretText), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{StderrPath: &outside}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/stderr", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/stderr: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), secretText) || strings.Contains(string(body), "outside secret") {
		t.Errorf("response body leaked the file's text: %q", body)
	}
	if !strings.Contains(string(body), "outside the data directory") {
		t.Errorf("body = %q, want it to mention the data directory", body)
	}
}

// TestRunFile_RefusesSymlinkEscapingRunsDir proves GET /runs/{id}/stderr
// 403s, and never leaks the file's text, when the recorded stderr_path
// names a symlink that lives inside <store.Dir()>/runs but resolves to a
// file outside it. filepath.Rel alone cannot catch this: the path string
// itself sits under runs/, only EvalSymlinks shows where it really points.
func TestRunFile_RefusesSymlinkEscapingRunsDir(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	const secretText = "symlink secret\n"
	target := filepath.Join(t.TempDir(), "target-stderr.log")
	if err := os.WriteFile(target, []byte(secretText), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	runsDir := filepath.Join(s.Dir(), "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(runsDir, store.StderrFileName(runID))
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{StderrPath: &link}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/stderr", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/stderr: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), secretText) || strings.Contains(string(body), "symlink secret") {
		t.Errorf("response body leaked the file's text: %q", body)
	}
}

// TestRunFile_RefusesNonStderrFileInsideDataDir proves GET /runs/{id}/stderr
// 403s when stderr_path names a real file inside the data directory that
// is not a run's stderr log, such as the store's own database file. The
// route serves only run-<id>-stderr.log directly inside <store.Dir()>/runs,
// never anything else the data directory holds. The file genuinely sits
// inside the data directory, so the body must not claim it is outside.
func TestRunFile_RefusesNonStderrFileInsideDataDir(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	dbPath := filepath.Join(s.Dir(), "zing.db")
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{StderrPath: &dbPath}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/stderr", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/stderr: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "outside the data directory") {
		t.Errorf("body = %q, zing.db sits inside the data directory, should not say otherwise", body)
	}
}

// TestRunFile_RefusesAnotherRunsStderrFile proves GET /runs/{id}/stderr 403s,
// and never leaks the file's text, when stderr_path names a real stderr
// file inside <store.Dir()>/runs that belongs to a different run. The exact
// per-run file name check (not just "inside runs/") is what catches this.
func TestRunFile_RefusesAnotherRunsStderrFile(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")
	otherRunID := oneSeededRun(t, s, ticketID, "sonnet")

	const otherSecret = "another run's stderr\n"
	otherPath := writeRunStderrFile(t, s, otherRunID, otherSecret)
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{StderrPath: &otherPath}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/stderr", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/stderr: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), otherSecret) || strings.Contains(string(body), "another run's stderr") {
		t.Errorf("response body leaked the file's text: %q", body)
	}
}

// TestRunFile_ServesTranscriptFromDataDir proves GET /runs/{id}/transcript
// serves the stdout file a run's recorded transcript_path names, the same
// way GET /runs/{id}/stderr already serves the stderr file.
func TestRunFile_ServesTranscriptFromDataDir(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	runID := oneSeededRun(t, s, ticketID, "sonnet")

	const transcriptText = `{"type":"thread.started"}` + "\n" + `{"type":"error","message":"boom"}` + "\n"
	path := writeRunTranscriptFile(t, s, runID, transcriptText)
	if err := s.RecordRunEvidence(t.Context(), runID, store.RunEvidence{TranscriptPath: &path}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/transcript", srv.URL, runID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/transcript: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if nos := resp.Header.Get("X-Content-Type-Options"); nos != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nos)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != transcriptText {
		t.Errorf("body = %q, want %q", body, transcriptText)
	}
}

// TestRunFile_RefusesTranscriptOutsideRunsDir proves GET
// /runs/{id}/transcript 403s on a Claude-style transcript path outside the
// data directory, and 404s with a transcript-specific message when the run
// kept no transcript at all.
func TestRunFile_RefusesTranscriptOutsideRunsDir(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	outsideRunID := oneSeededRun(t, s, ticketID, "sonnet")
	noTranscriptRunID := oneSeededRun(t, s, ticketID, "sonnet")

	const secretText = "claude rollout secret\n"
	outside := filepath.Join(t.TempDir(), "abc123.jsonl")
	if err := os.WriteFile(outside, []byte(secretText), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.RecordRunEvidence(t.Context(), outsideRunID, store.RunEvidence{TranscriptPath: &outside}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	resp, err := http.Get(fmt.Sprintf("%s/runs/%d/transcript", srv.URL, outsideRunID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/transcript: %v", outsideRunID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), secretText) || strings.Contains(string(body), "claude rollout secret") {
		t.Errorf("response body leaked the file's text: %q", body)
	}
	if !strings.Contains(string(body), "outside the data directory") {
		t.Errorf("body = %q, want it to mention the data directory", body)
	}

	resp2, err := http.Get(fmt.Sprintf("%s/runs/%d/transcript", srv.URL, noTranscriptRunID)) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /runs/%d/transcript: %v", noTranscriptRunID, err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp2.StatusCode)
	}
	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body2), "this run kept no transcript") {
		t.Errorf("body = %q, want it to say this run kept no transcript", body2)
	}
}

// TestRunFile_MissingCases proves the route's various 404 shapes: a run
// with no evidence at all for any kind, a run whose stderr file has
// since been deleted, a run id that names no run, an unrecognized kind,
// and an id that does not even parse as a positive int64.
func TestRunFile_MissingCases(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	noEvidenceRunID := oneSeededRun(t, s, ticketID, "sonnet")
	goneRunID := oneSeededRun(t, s, ticketID, "opus")
	dirRunID := oneSeededRun(t, s, ticketID, "opus")

	goneStderr := writeRunStderrFile(t, s, goneRunID, "will be deleted")
	if err := s.RecordRunEvidence(t.Context(), goneRunID, store.RunEvidence{StderrPath: &goneStderr}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}
	if err := os.Remove(goneStderr); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	runsDir := filepath.Join(s.Dir(), "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dirStderr := filepath.Join(runsDir, store.StderrFileName(dirRunID))
	if err := os.Mkdir(dirStderr, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := s.RecordRunEvidence(t.Context(), dirRunID, store.RunEvidence{StderrPath: &dirStderr}); err != nil {
		t.Fatalf("RecordRunEvidence: %v", err)
	}

	unknownRunID := goneRunID + 1_000_000

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	tests := []struct {
		name       string
		path       string
		wantInBody string
	}{
		{"no final message", fmt.Sprintf("/runs/%d/final", noEvidenceRunID), "this run kept no final message"},
		{"no stderr", fmt.Sprintf("/runs/%d/stderr", noEvidenceRunID), "this run wrote no stderr"},
		{"stderr file gone", fmt.Sprintf("/runs/%d/stderr", goneRunID), "stderr file is gone"},
		{"stderr path is a directory", fmt.Sprintf("/runs/%d/stderr", dirRunID), "stderr file is gone"},
		{"unknown run", fmt.Sprintf("/runs/%d/final", unknownRunID), "no such run"},
		{"no transcript", fmt.Sprintf("/runs/%d/transcript", noEvidenceRunID), "this run kept no transcript"},
		{"unrecognized kind", fmt.Sprintf("/runs/%d/bogus", noEvidenceRunID), "404 page not found"},
		{"id does not parse", "/runs/abc/final", "404 page not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(srv.URL + tt.path) //nolint:noctx // a bare GET on a test server needs no deadline
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want 404", tt.path, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("GET %s: read body: %v", tt.path, err)
			}
			if !strings.Contains(string(body), tt.wantInBody) {
				t.Errorf("GET %s body = %q, want it to contain %q", tt.path, body, tt.wantInBody)
			}
		})
	}
}

// runFileLogRecords parses buf as one JSON object per line (slog's own
// JSONHandler shape), so TestRunFile_LogsOutcomes can assert on individual
// fields rather than substrings.
func runFileLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for line := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		recs = append(recs, m)
	}
	return recs
}

// findRunFileLogRecord returns the first record in recs whose "msg" field
// equals msg, failing the test if there is none.
func findRunFileLogRecord(t *testing.T, recs []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, r := range recs {
		if r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no log record with msg %q among %d records", msg, len(recs))
	return nil
}

// runFileLogInt64 reads key from rec as the int64 a JSON number decodes to
// (encoding/json gives float64), failing the test if key is absent or not
// a number.
func runFileLogInt64(t *testing.T, rec map[string]any, key string) int64 {
	t.Helper()
	v, ok := rec[key].(float64)
	if !ok {
		t.Fatalf("field %q = %v (%T), want a number", key, rec[key], rec[key])
	}
	return int64(v)
}

// TestRunFile_LogsOutcomes proves the route's three outcome log lines
// (served, not found, outside the data dir) name the run and ticket but
// never carry the evidence text itself. Not t.Parallel: it swaps slog's
// process-wide default to capture the records.
func TestRunFile_LogsOutcomes(t *testing.T) {
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	r1 := oneSeededRun(t, s, ticketID, "sonnet") // served
	r2 := oneSeededRun(t, s, ticketID, "sonnet") // not found
	r3 := oneSeededRun(t, s, ticketID, "sonnet") // outside the data dir

	const finalMsg = "final secret"
	const stderrSecret = "stderr secret"
	stderrPath := writeRunStderrFile(t, s, r1, stderrSecret)
	if err := s.RecordRunEvidence(t.Context(), r1, store.RunEvidence{FinalMessage: new(finalMsg), StderrPath: &stderrPath}); err != nil {
		t.Fatalf("RecordRunEvidence r1: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "r3-stderr.log")
	if err := os.WriteFile(outside, []byte(stderrSecret), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.RecordRunEvidence(t.Context(), r3, store.RunEvidence{StderrPath: &outside}); err != nil {
		t.Fatalf("RecordRunEvidence r3: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	getBody := func(path string) {
		resp, err := http.Get(srv.URL + path) //nolint:noctx // a bare GET on a test server needs no deadline
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("drain body for %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}
	getBody(fmt.Sprintf("/runs/%d/final", r1))
	getBody(fmt.Sprintf("/runs/%d/final", r2))
	getBody(fmt.Sprintf("/runs/%d/stderr", r3))

	recs := runFileLogRecords(t, &logBuf)

	served := findRunFileLogRecord(t, recs, "run file served")
	if served["level"] != "INFO" {
		t.Errorf("served level = %v, want INFO", served["level"])
	}
	if got := runFileLogInt64(t, served, "ticket_id"); got != ticketID {
		t.Errorf("served ticket_id = %d, want %d", got, ticketID)
	}
	if got := runFileLogInt64(t, served, "run_id"); got != r1 {
		t.Errorf("served run_id = %d, want %d", got, r1)
	}
	if served["kind"] != "final" {
		t.Errorf("served kind = %v, want final", served["kind"])
	}
	if got := runFileLogInt64(t, served, "bytes"); got != int64(len(finalMsg)) {
		t.Errorf("served bytes = %d, want %d", got, len(finalMsg))
	}

	notFound := findRunFileLogRecord(t, recs, "run file not found")
	if notFound["level"] != "INFO" {
		t.Errorf("not found level = %v, want INFO", notFound["level"])
	}
	if got := runFileLogInt64(t, notFound, "ticket_id"); got != ticketID {
		t.Errorf("not found ticket_id = %d, want %d", got, ticketID)
	}
	if got := runFileLogInt64(t, notFound, "run_id"); got != r2 {
		t.Errorf("not found run_id = %d, want %d", got, r2)
	}
	if notFound["kind"] != "final" {
		t.Errorf("not found kind = %v, want final", notFound["kind"])
	}
	if notFound["reason"] != "no_final_message" {
		t.Errorf("not found reason = %v, want no_final_message", notFound["reason"])
	}

	outsideRec := findRunFileLogRecord(t, recs, "run file outside data dir")
	if outsideRec["level"] != "WARN" {
		t.Errorf("outside level = %v, want WARN", outsideRec["level"])
	}
	if got := runFileLogInt64(t, outsideRec, "ticket_id"); got != ticketID {
		t.Errorf("outside ticket_id = %d, want %d", got, ticketID)
	}
	if got := runFileLogInt64(t, outsideRec, "run_id"); got != r3 {
		t.Errorf("outside run_id = %d, want %d", got, r3)
	}
	if outsideRec["kind"] != "stderr" {
		t.Errorf("outside kind = %v, want stderr", outsideRec["kind"])
	}
	if outsideRec["path"] != outside {
		t.Errorf("outside path = %v, want %q", outsideRec["path"], outside)
	}

	if strings.Contains(logBuf.String(), finalMsg) || strings.Contains(logBuf.String(), stderrSecret) {
		t.Errorf("log buffer leaked evidence text:\n%s", logBuf.String())
	}
}

// ---- Stall line (ticket "Say on each ticket why it is not moving", split
// from #79) -----------------------------------------------------------

// TestRail_StallOwnerWaitAndLastRan proves the rail's stall section shows
// the "waiting on the owner" reason, naming the waiting_on flag, and the
// "last ran" line formatted from the newest run's recorded StartedAt. This
// task (Task 3) wires no SlotSource, so decideStall's running, claim_dead,
// and slot reasons cannot fire here; only waiting-on-owner is exercised.
func TestRail_StallOwnerWaitAndLastRan(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	advanceTicketToBuilding(t, s, ticketID, "sonnet", 42)

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("RunsForTicket returned %d runs, want 1", len(runs))
	}
	startedAt := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	if err := s.RecordRunStart(t.Context(), runs[0].ID, 0, "", startedAt, ""); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))
	rail := railHTML(t, srv.URL, ticketID)

	if !strings.Contains(rail, "waiting on the owner (gate)") {
		t.Errorf("rail missing the owner-wait stall line; got:\n%s", rail)
	}
	if !strings.Contains(rail, "last ran 2026-01-02 03:04 UTC") {
		t.Errorf("rail missing the last-ran line; got:\n%s", rail)
	}
}

// TestRail_StallCIWaiting proves the rail's stall section shows the CI
// reason, with the whole-minutes count and the check names, once the ticket
// is in shipping and carries a "ci waiting ..." system update marker newer
// than its newest run's start (owner decision Q1).
func TestRail_StallCIWaiting(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	advanceTicketToState(t, s, ticketID, string(response.TicketStateShipping))

	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeUpdate, Author: "system", Body: "ci waiting ci,lint",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))
	rail := railHTML(t, srv.URL, ticketID)

	if !strings.Contains(rail, `data-stall-reason="ci"`) {
		t.Errorf("rail missing data-stall-reason=\"ci\"; got:\n%s", rail)
	}
	if !strings.Contains(rail, "CI waiting 0 minutes for ci, lint") {
		t.Errorf("rail missing the CI waiting line; got:\n%s", rail)
	}
}

// TestRail_StallNoSlotSource proves a console with a machine but no
// SlotSource still renders the stall section's shell -- class="rail-stall"
// and the "never ran" last-ran line -- but shows no stall-reason element,
// since a ready queued ticket's only possible reason (a full run-slot
// table) needs a SlotSource this console does not have (Task 4).
func TestRail_StallNoSlotSource(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))
	rail := railHTML(t, srv.URL, ticketID)

	if !strings.Contains(rail, `class="rail-stall"`) {
		t.Errorf("rail missing the rail-stall section; got:\n%s", rail)
	}
	if !strings.Contains(rail, "never ran") {
		t.Errorf("rail missing the never-ran line; got:\n%s", rail)
	}
	if strings.Contains(rail, `class="stall-reason"`) {
		t.Errorf("rail should show no stall-reason with no SlotSource; got:\n%s", rail)
	}
}

// TestRail_StallHiddenWhenTerminal proves a terminal ticket (abandoned)
// renders no rail-stall section at all, matching buildStallRail's own
// terminal-state guard.
func TestRail_StallHiddenWhenTerminal(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")
	if err := s.AbandonTicket(t.Context(), ticketID, "rail stall test"); err != nil {
		t.Fatalf("AbandonTicket: %v", err)
	}

	srv := newTestServer(t, s, bus.New(), testMachine(t), newTestLogHandler(t))
	rail := railHTML(t, srv.URL, ticketID)

	if strings.Contains(rail, "rail-stall") {
		t.Errorf("rail should hide the stall section for a terminal ticket; got:\n%s", rail)
	}
}

// fakeSlots is a console.SlotSource a test can seed with a fixed snapshot,
// standing in for a real dispatch.Dispatcher (owner decision Q5): Slots
// always returns the exact dispatch.SlotSnapshot the test constructed it
// with.
type fakeSlots dispatch.SlotSnapshot

func (f fakeSlots) Slots() dispatch.SlotSnapshot { return dispatch.SlotSnapshot(f) }

// newStallTestServer is newTestServerConfig's own listener-reservation and
// placeholder-swap recipe (console_test.go), narrowed to what the stall
// tests need: a real machine (testMachine) so the rail renders a stall
// section at all, and slots wired through console.WithSlots so decideStall's
// running, claim_dead, and slot reasons can fire.
func newStallTestServer(t *testing.T, s *store.Store, b *bus.Broker, slots console.SlotSource) *httptest.Server {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", testBindHost+":0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}

	handler := console.New(s, b, testMachine(t), testBindHosts, addr.Port, newTestLogHandler(t), nil, testPushToken,
		response.SeverityMinor, "", nil, "", nil, console.WithSlots(slots))
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestRail_StallSlotLine proves the rail's stall section shows the "waiting
// for a free run slot" reason, naming the tickets that hold the slots, for
// a ready (unclaimed, non-waiting) ticket once every slot a SlotSource
// reports is busy (owner decision Q5; "Done when" bullet 1).
func TestRail_StallSlotLine(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	slots := fakeSlots{Owner: "this-serve", Inflight: []int64{11, 12}, MaxParallel: 2}
	srv := newStallTestServer(t, s, bus.New(), slots)
	rail := railHTML(t, srv.URL, ticketID)

	if !strings.Contains(rail, `data-stall-reason="slot"`) {
		t.Errorf("rail missing data-stall-reason=\"slot\"; got:\n%s", rail)
	}
	if !strings.Contains(rail, "waiting for a free run slot; slots held by tickets 11, 12") {
		t.Errorf("rail missing the slot-wait line; got:\n%s", rail)
	}
}

// TestRail_StallDeadClaim proves the rail's stall section shows the
// dead-claim reason for a ticket claimed by an owner other than the
// SlotSource's own, when that ticket carries no open runs at all -- so
// ClaimAlive, still always false until Task 5 wires ClaimProcessesAlive,
// correctly picks the "next dispatch pass reclaims it" text rather than the
// "leftover agent process is still running" one.
func TestRail_StallDeadClaim(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, "dead-serve-1", expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	slots := fakeSlots{Owner: "this-serve", Inflight: nil, MaxParallel: 2}
	srv := newStallTestServer(t, s, bus.New(), slots)
	rail := railHTML(t, srv.URL, ticketID)

	if !strings.Contains(rail, `data-stall-reason="claim_dead"`) {
		t.Errorf("rail missing data-stall-reason=\"claim_dead\"; got:\n%s", rail)
	}
	if !strings.Contains(rail, "claim held by a process that is no longer alive (dead-serve-1); the next dispatch pass reclaims it") {
		t.Errorf("rail missing the dead-claim line; got:\n%s", rail)
	}
}

// TestRail_StallDeadClaimOrphanRunning proves the dead-claim reason picks
// the "leftover agent process is still running" text, not "the next
// dispatch pass reclaims it", when store.ForeignClaims' matching entry's
// open run still has a live process group (owner decision Q2,
// dispatch.ClaimProcessesAlive).
func TestRail_StallDeadClaimOrphanRunning(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Add a hello endpoint")

	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start sh: %v", err)
	}
	pgid := cmd.Process.Pid
	token, err := proc.StartToken(pgid)
	if err != nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
		t.Skipf("proc.StartToken unsupported on this platform: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort teardown
		_ = cmd.Wait()                           //nolint:errcheck // best-effort teardown
	})

	const foreignOwner = "dead-serve-2"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, foreignOwner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	reserved, err := s.Reserve(t.Context(), ticketID, foreignOwner, expires,
		store.SessionUpsert{Job: string(response.TicketStateBuilding), Runtime: testRuntimeFake},
		store.RunSeed{Model: "test-model"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := s.RecordRunStart(t.Context(), reserved.RunID, pgid, token, time.Now(), ""); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	slots := fakeSlots{Owner: "this-serve", Inflight: nil, MaxParallel: 2}
	srv := newStallTestServer(t, s, bus.New(), slots)
	rail := railHTML(t, srv.URL, ticketID)

	if !strings.Contains(rail, `data-stall-reason="claim_dead"`) {
		t.Errorf("rail missing data-stall-reason=\"claim_dead\"; got:\n%s", rail)
	}
	if !strings.Contains(rail, "claim held by a process that is no longer alive (dead-serve-2); its leftover agent process is still running") {
		t.Errorf("rail missing the live-orphan dead-claim line; got:\n%s", rail)
	}
}
