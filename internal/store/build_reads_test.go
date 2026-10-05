package store

import (
	"fmt"
	"testing"
)

// buildReportPayload is a minimal, schema-valid "build_report" artifact
// payload (schemas/artifacts/build_report.json) for task n.
func buildReportPayload(n int) []byte {
	return []byte(fmt.Sprintf(
		`{"task_n":%d,"files_changed":["a.go"],"extras":[],"fences":[],"report":"did it","title":"Task %d"}`,
		n, n))
}

// filePayload is a minimal, schema-valid "file" artifact payload
// (schemas/artifacts/file.json) for taskN naming path with decision (empty
// for a proposed-but-undecided event).
func filePayload(path string, taskN int, decision string) []byte {
	body := fmt.Sprintf(`{"path":%q,"action":"create","reason":"r","trust_root":false,"style_guide":false,"task_n":%d`, path, taskN)
	if decision != "" {
		body += fmt.Sprintf(`,"decision":%q`, decision)
	}
	body += `}`
	return []byte(body)
}

// TestStoredPlan proves StoredPlan reads the ticket's max-version plan
// artifact, decoded (design section 4.2): ok is false with no plan yet, and
// a second, higher version wins over the first.
// TestStoredPlan's two subtests each open their own store rather than share
// one across t.Parallel() siblings: both seed a ticket under the same
// fixed testProject name, and EnsureProject's own check-then-insert is not
// safe for two goroutines racing on the same project row.
func TestStoredPlan(t *testing.T) {
	t.Parallel()

	t.Run("no plan yet", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		_, _, ok, err := s.StoredPlan(ctx, ticketID)
		if err != nil {
			t.Fatalf("StoredPlan: %v", err)
		}
		if ok {
			t.Error("StoredPlan with no plan artifact: ok = true, want false")
		}
	})

	t.Run("max version wins", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "2")
		insertPlanArtifact(t, s, ticketID, nil, 1)
		insertPlanArtifact(t, s, ticketID, nil, 2)

		plan, version, ok, err := s.StoredPlan(ctx, ticketID)
		if err != nil {
			t.Fatalf("StoredPlan: %v", err)
		}
		if !ok {
			t.Fatal("StoredPlan: ok = false, want true")
		}
		if version != 2 {
			t.Errorf("version = %d, want 2", version)
		}
		if plan.Overview.Objective != "Stop checkout from crashing on an empty cart." {
			t.Errorf("plan.Overview.Objective = %q, want the checked-in fixture's own text", plan.Overview.Objective)
		}
	})
}

// TestBuildReportsOrder proves BuildReports returns every build_report
// artifact of the ticket, decoded, ORDER BY artifacts.id (design section
// 4.2): insertion order, not task or run order.
func TestBuildReportsOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	n2, n1 := 2, 1
	r2, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &n2})
	if err != nil {
		t.Fatalf("Reserve task 2: %v", err)
	}
	r1, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &n1})
	if err != nil {
		t.Fatalf("Reserve task 1: %v", err)
	}

	// Inserted in reverse task order, so BuildReports's own order proves it
	// follows artifacts.id, not task_n.
	aFirst, err := s.InsertArtifact(ctx, Artifact{TicketID: ticketID, RunID: &r2.RunID, Type: testTypeBuildReport, Payload: buildReportPayload(2)})
	if err != nil {
		t.Fatalf("insert build_report for task 2: %v", err)
	}
	aSecond, err := s.InsertArtifact(ctx, Artifact{TicketID: ticketID, RunID: &r1.RunID, Type: testTypeBuildReport, Payload: buildReportPayload(1)})
	if err != nil {
		t.Fatalf("insert build_report for task 1: %v", err)
	}

	rows, err := s.BuildReports(ctx, ticketID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].ArtifactID != aFirst || rows[1].ArtifactID != aSecond {
		t.Errorf("ArtifactID order = [%d, %d], want [%d, %d] (insertion order)", rows[0].ArtifactID, rows[1].ArtifactID, aFirst, aSecond)
	}
	if rows[0].RunID != r2.RunID || rows[0].Report.TaskN != 2 {
		t.Errorf("rows[0] = (RunID=%d, TaskN=%d), want (%d, 2)", rows[0].RunID, rows[0].Report.TaskN, r2.RunID)
	}
	if rows[1].RunID != r1.RunID || rows[1].Report.TaskN != 1 {
		t.Errorf("rows[1] = (RunID=%d, TaskN=%d), want (%d, 1)", rows[1].RunID, rows[1].Report.TaskN, r1.RunID)
	}
}

// TestFileEventsOrder proves FileEvents returns every file artifact of the
// ticket, decoded, ORDER BY artifacts.id (design section 4.2): every
// append-only event, oldest first, so a caller can reduce to "newest row
// per path wins" itself.
func TestFileEventsOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	n := 1
	r, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &n})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	aProposed, err := s.InsertArtifact(ctx, Artifact{TicketID: ticketID, RunID: &r.RunID, Type: "file", Payload: filePayload("a.go", 1, "")})
	if err != nil {
		t.Fatalf("insert proposed file event: %v", err)
	}
	aDecided, err := s.InsertArtifact(ctx, Artifact{TicketID: ticketID, RunID: nil, Type: "file", Payload: filePayload("a.go", 1, "accept")})
	if err != nil {
		t.Fatalf("insert decided file event: %v", err)
	}

	rows, err := s.FileEvents(ctx, ticketID)
	if err != nil {
		t.Fatalf("FileEvents: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].ArtifactID != aProposed || rows[1].ArtifactID != aDecided {
		t.Errorf("ArtifactID order = [%d, %d], want [%d, %d] (insertion order)", rows[0].ArtifactID, rows[1].ArtifactID, aProposed, aDecided)
	}
	if rows[0].RunID == nil || *rows[0].RunID != r.RunID {
		t.Errorf("rows[0].RunID = %v, want %d", rows[0].RunID, r.RunID)
	}
	if rows[0].File.Decision != nil {
		t.Errorf("rows[0].File.Decision = %v, want nil (proposed, not yet decided)", rows[0].File.Decision)
	}
	if rows[1].RunID != nil {
		t.Errorf("rows[1].RunID = %v, want nil (the perimeter decision's own row carries none here)", rows[1].RunID)
	}
	if rows[1].File.Decision == nil || string(*rows[1].File.Decision) != "accept" {
		t.Errorf("rows[1].File.Decision = %v, want accept", rows[1].File.Decision)
	}
}

// TestUnitSession proves UnitSession finds the newest "build" session whose
// first run matches taskN, taskN 0 matching a fix unit's NULL task_n
// (design section 4.2), under the same SessionState rules as LatestSession.
// TestUnitSession's three subtests each open their own store rather than
// share one across t.Parallel() siblings: each seeds a ticket under the
// same fixed testProject name, and EnsureProject's own check-then-insert is
// not safe for two goroutines racing on the same project row.
func TestUnitSession(t *testing.T) {
	t.Parallel()

	t.Run("none for a task that never ran", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		_, _, _, ok, err := s.UnitSession(ctx, ticketID, 1, 3)
		if err != nil {
			t.Fatalf("UnitSession: %v", err)
		}
		if ok {
			t.Error("UnitSession for a task that never ran: ok = true, want false")
		}
	})

	t.Run("finds a task unit, not a fix or a different task", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "2")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		fixN := 0
		if _, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &fixN}); err != nil {
			t.Fatalf("Reserve fix unit: %v", err)
		}
		task2 := 2
		if _, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &task2}); err != nil {
			t.Fatalf("Reserve task 2: %v", err)
		}
		task1 := 1
		r1, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &task1})
		if err != nil {
			t.Fatalf("Reserve task 1: %v", err)
		}

		sess, state, newest, ok, err := s.UnitSession(ctx, ticketID, 1, 3)
		if err != nil {
			t.Fatalf("UnitSession: %v", err)
		}
		if !ok {
			t.Fatal("UnitSession: ok = false, want true")
		}
		if sess.ID != r1.SessionID {
			t.Errorf("sess.ID = %d, want %d (task 1's own session)", sess.ID, r1.SessionID)
		}
		if state != SessionIdless {
			t.Errorf("state = %v, want SessionIdless (external_id still NULL)", state)
		}
		if newest.ID != r1.RunID {
			t.Errorf("newest.ID = %d, want %d", newest.ID, r1.RunID)
		}

		fixSess, _, _, ok, err := s.UnitSession(ctx, ticketID, 0, 3)
		if err != nil {
			t.Fatalf("UnitSession(taskN=0): %v", err)
		}
		if !ok {
			t.Fatal("UnitSession(taskN=0): ok = false, want true (the fix unit)")
		}
		if fixSess.ID == sess.ID {
			t.Error("UnitSession(taskN=0) returned task 1's own session, want the fix unit's")
		}
	})

	t.Run("open and exhausted states", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "3")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		task1 := 1
		first, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX, TaskN: &task1})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		external := "ext-1"
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Session: &SessionUpsert{ID: &first.SessionID, ExternalID: &external},
		})
		if err != nil || !applied {
			t.Fatalf("fill external_id: applied=%v err=%v", applied, err)
		}

		_, state, _, ok, err := s.UnitSession(ctx, ticketID, 1, 3)
		if err != nil || !ok {
			t.Fatalf("UnitSession: ok=%v err=%v", ok, err)
		}
		if state != SessionOpen {
			t.Errorf("state = %v, want SessionOpen", state)
		}

		// CommitHandlerResult above already released the claim; Reserve
		// itself never does (design section 4.5), so one more claim covers
		// all three resumes below (reserveInput's own pattern).
		owner, expires = claimForCommit(t, s, ticketID)
		for range 3 {
			_, err = s.Reserve(ctx, ticketID, owner, expires,
				SessionUpsert{ID: &first.SessionID, BumpResumes: true}, RunSeed{Model: testModelClaudeX})
			if err != nil {
				t.Fatalf("resume Reserve: %v", err)
			}
		}

		_, state, _, ok, err = s.UnitSession(ctx, ticketID, 1, 3)
		if err != nil || !ok {
			t.Fatalf("UnitSession after 3 resumes: ok=%v err=%v", ok, err)
		}
		if state != SessionExhausted {
			t.Errorf("state after 3 resumes (max 3) = %v, want SessionExhausted", state)
		}
	})
}

// TestMarkerMatchesFirstLineExactly proves Marker matches a marker's first
// line by exact equality, not by prefix (design section 4.2): "claims ok
// run 4" does not match a row "claims ok run 42".
func TestMarkerMatchesFirstLineExactly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	insertUpdateMarker(t, s, ticketID, "claims ok run 42")

	_, ok, err := s.Marker(ctx, ticketID, "claims ok run 4")
	if err != nil {
		t.Fatalf("Marker: %v", err)
	}
	if ok {
		t.Error(`Marker("claims ok run 4") matched "claims ok run 42", want no match`)
	}

	insertUpdateMarker(t, s, ticketID, "claims ok run 4")

	got, ok, err := s.Marker(ctx, ticketID, "claims ok run 4")
	if err != nil {
		t.Fatalf("Marker: %v", err)
	}
	if !ok {
		t.Fatal(`Marker("claims ok run 4"): ok = false, want true`)
	}
	if got.Body != "claims ok run 4" {
		t.Errorf("got.Body = %q, want %q", got.Body, "claims ok run 4")
	}
}

// TestBuildReportLegacyExitFieldsValidate proves a build_report stored
// before #55, which still carries test_exit and lint_exit, passes the
// schema: both fields stay optional properties, so a report written by an
// older binary is never rejected.
func TestBuildReportLegacyExitFieldsValidate(t *testing.T) {
	t.Parallel()
	schemas, err := loadSchemas()
	if err != nil {
		t.Fatalf("loadSchemas: %v", err)
	}
	legacy := `{"task_n":1,"files_changed":["a.go"],"test_exit":0,"lint_exit":0,"extras":[],"fences":[],"report":"did it","title":"Task 1"}`
	if err := schemas.validate(testTableArtifacts, testTypeBuildReport, []byte(legacy)); err != nil {
		t.Errorf("validate legacy build_report: %v", err)
	}
	if err := schemas.validate(testTableArtifacts, testTypeBuildReport, buildReportPayload(1)); err != nil {
		t.Errorf("validate current build_report: %v", err)
	}
}
