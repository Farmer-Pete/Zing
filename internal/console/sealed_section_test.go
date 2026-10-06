// sealed_section_test.go proves #75's post-gate "Sealed plan and scenarios"
// section, test-first: a sealed ticket in building, reviewing, judging,
// shipping or escalated shows the section with its owner-edit boxes, never
// at the gate itself and never outside those states, and an edit posted
// from the section's plan-file box lands in the stored plan.
package console_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// sealedSectionFilePath is the plan file every sealed-section fixture's
// plan carries, matching owner_edit_test.go's own TestOwnerEditRoute_SetsPlanFileTasks
// fixture path.
const sealedSectionFilePath = "internal/store/console_reads.go"

// sealedSectionHostCheck is the sealed host scenario's own check_cmd, named
// once so TestThreadSealedSection_PostGateStatesShowBoxes and
// TestThreadSealedSection_HiddenOutsidePostGateStates (a) seed the exact
// same fixture (PlanReview owner decision).
const sealedSectionHostCheck = "go test ./internal/console/"

// advanceTicketToState moves a queued ticketID to state through one claim
// and one store.CommitHandlerResult, the seam rail_test.go's
// advanceTicketToBuilding uses: InsertTicket accepts only queued
// (store/spine.go), so a fixture reaches building, done, or any other
// state the way a real handler does. The commit clears the claim; a test
// that needs it held calls s.Claim again afterwards. Going straight from
// queued skips checkGateApprovalTx, which fires only on planning to
// building or a seal.
func advanceTicketToState(t *testing.T, s *store.Store, ticketID int64, state string) {
	t.Helper()
	const owner = "sealed-section-test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: state, Reason: "sealed section test advance",
	})
	if err != nil || !applied {
		t.Fatalf("CommitHandlerResult(%s): applied=%v err=%v", state, applied, err)
	}
}

// seedSealedSectionFixture seeds a queued ticket carrying a run, a 2-task
// plan whose one file names task "2", and one sealed host scenario s1
// (check sealedSectionHostCheck), through the same real store writes every
// other console test uses, returning the ticket id.
func seedSealedSectionFixture(t *testing.T, s *store.Store) int64 {
	t.Helper()
	ticketID := seedTicket(t, s, "1", "fix the bug")
	runID := seedRun(t, s, ticketID)

	plan := planWithTasks(
		[]response.Task{
			{N: 1, Test: "T1", Demo: true, Text: "sealed section task one"},
			{N: 2, Test: "T2", Demo: false, Text: "sealed section task two"},
		},
		[]response.FileChange{
			{Path: sealedSectionFilePath, Action: response.FileActionModify, Task: "2", Reason: "r1"},
		},
	)
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, insErr := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: &runID, Type: testArtifactTypePlan, Version: 1, Payload: payload,
	}); insErr != nil {
		t.Fatalf("InsertArtifact(plan): %v", insErr)
	}
	seedSealedScenarioArtifact(t, s, ticketID, &runID, response.Scenario{
		ID: "s1", Kind: response.ScenarioKindHost, Check: sealedSectionHostCheck,
		Given: "g1", When: "w1", Then: "t1",
	})
	seedGateQuestionInState(t, s, ticketID, response.QuestionStateResolved, string(response.QuestionStateResolved))
	return ticketID
}

// sealedSectionHTML extracts the post-gate section's own fragment from a
// rendered #main frame: from id="sealed-section" up to sandboxRunBox's own
// class, the element thread.templ places right after it, so assertions run
// against only the section's own markup, not the rest of the page.
func sealedSectionHTML(t *testing.T, main string) (string, bool) {
	t.Helper()
	start := strings.Index(main, `id="sealed-section"`)
	if start < 0 {
		return "", false
	}
	end := strings.Index(main[start:], `class="sandbox-run-box"`)
	if end < 0 {
		t.Fatalf("sealedSectionHTML: found id=\"sealed-section\" but no following sandbox-run-box in:\n%s", main)
	}
	return main[start : start+end], true
}

// TestThreadSealedSection_PostGateStatesShowBoxes proves the done-when
// test: a sealed ticket in building, reviewing, judging, shipping or
// escalated shows the collapsed sealed section with its scenario and
// plan-file owner-edit boxes, and shows no host-check list, no
// answer_question wiring, and no claim note while unclaimed.
func TestThreadSealedSection_PostGateStatesShowBoxes(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"building", "reviewing", "judging", "shipping", "escalated"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := newConsoleTestStore(t)
			ticketID := seedSealedSectionFixture(t, s)
			advanceTicketToState(t, s, ticketID, state)

			srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
			resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
			defer cancel()
			defer func() { _ = resp.Body.Close() }()
			_, main, _, _ := readInitialFrames(t, r)

			section, ok := sealedSectionHTML(t, main)
			if !ok {
				t.Fatalf("state %s: no sealed section in main frame:\n%s", state, main)
			}
			if !strings.Contains(main, "Sealed plan and scenarios") {
				t.Error(`main frame missing "Sealed plan and scenarios"`)
			}
			if strings.Contains(section, `id="sealed-section" class="sealed-section" data-preserve-attr="open" open`) {
				t.Error("sealed-section rendered open, want collapsed by default")
			}

			if n := strings.Count(section, `data-target="scenario"`); n != 1 {
				t.Errorf(`data-target="scenario" count = %d, want 1`, n)
			}
			if n := strings.Count(section, `data-target="plan_file"`); n != 1 {
				t.Errorf(`data-target="plan_file" count = %d, want 1`, n)
			}
			if n := strings.Count(section, "owner-edit-drop"); n != 2 {
				t.Errorf(`owner-edit-drop count = %d, want 2`, n)
			}
			if strings.Contains(main, "Runs on your machine at judging") {
				t.Error(`main frame contains "Runs on your machine at judging", want the host-check list left out post-gate`)
			}
			if strings.Contains(section, "data-answer-question") {
				t.Error("sealed section contains data-answer-question, want none")
			}
			if strings.Contains(section, "owner-edit-claim-note") {
				t.Error("sealed section contains owner-edit-claim-note while unclaimed, want none")
			}
		})
	}
}

// TestThreadSealedSection_EditPlanFileTasks proves the done-when test: an
// edit posted from the sealed section's plan-file box writes the
// owner_edit event and updates the stored plan.
func TestThreadSealedSection_EditPlanFileTasks(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedSealedSectionFixture(t, s)
	advanceTicketToState(t, s, ticketID, "building")

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	_, main, _, _ := readInitialFrames(t, r)
	cancel()
	_ = resp.Body.Close()

	section, ok := sealedSectionHTML(t, main)
	if !ok {
		t.Fatalf("no sealed section in main frame:\n%s", main)
	}
	if !strings.Contains(section, `data-ref="`+sealedSectionFilePath+`"`) {
		t.Fatalf("sealed section missing plan file box for %s:\n%s", sealedSectionFilePath, section)
	}

	editResp := doRequest(t, mutationRequest(t, srv, ownerEditPath(ticketID),
		`{"target":"plan_file","ref":"`+sealedSectionFilePath+`","action":"edit","tasks":"1 2"}`))
	defer func() { _ = editResp.Body.Close() }()
	if editResp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204; body = %q", editResp.StatusCode, readBody(t, editResp))
	}

	got, version, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	if version != 1 {
		t.Errorf("version = %d, want unchanged 1", version)
	}
	if gotTask := fileTaskByPath(got.Delivery.Files, sealedSectionFilePath); gotTask != "1 2" {
		t.Errorf("file %s task = %q, want %q", sealedSectionFilePath, gotTask, "1 2")
	}

	n, err := s.CountEvents(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents(owner_edit): %v", err)
	}
	if n != 1 {
		t.Errorf("owner_edit events = %d, want 1", n)
	}
}

// TestThreadSealedSection_HiddenOutsidePostGateStates proves showSealedSection
// never fires outside its states: an open gate still shows its own boxes
// (and the host-check list, proving atGate stayed true there) with no
// sealed section on the page; a sealed cohort in done gets no section; and
// an unsealed cohort in building gets no section either.
func TestThreadSealedSection_HiddenOutsidePostGateStates(t *testing.T) {
	t.Parallel()

	t.Run("open gate keeps its own boxes, no sealed section", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		ticketID := seedTicket(t, s, "1", "fix the bug")
		runID := seedRun(t, s, ticketID)
		seedPlanArtifact(t, s, ticketID, &runID, 1)
		seedSealedScenarioArtifact(t, s, ticketID, &runID, response.Scenario{
			ID: "s1", Kind: response.ScenarioKindHost, Check: sealedSectionHostCheck,
			Given: "g1", When: "w1", Then: "t1",
		})
		seedGateQuestion(t, s, ticketID)
		advanceTicketToState(t, s, ticketID, "planning")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if _, ok := sealedSectionHTML(t, main); ok {
			t.Fatalf(`main frame contains id="sealed-section", want none:\n%s`, main)
		}

		gate := findGroup(t, splitQuestionGroups(t, main), gateQuestionTitle)
		if n := strings.Count(gate, `data-target="scenario"`); n != 1 {
			t.Errorf(`gate group data-target="scenario" count = %d, want 1`, n)
		}
		if !strings.Contains(gate, "Runs on your machine at judging") {
			t.Error(`gate group missing "Runs on your machine at judging"`)
		}
	})

	t.Run("done ticket gets no sealed section", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		ticketID := seedSealedSectionFixture(t, s)
		advanceTicketToState(t, s, ticketID, "done")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if _, ok := sealedSectionHTML(t, main); ok {
			t.Fatalf(`main frame contains id="sealed-section", want none:\n%s`, main)
		}
	})

	t.Run("unsealed cohort in building gets no sealed section", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		ticketID := seedTicket(t, s, "1", "fix the bug")
		runID := seedRun(t, s, ticketID)
		seedPlanArtifact(t, s, ticketID, &runID, 1)
		seedScenarioArtifact(t, s, ticketID, &runID, 1, response.Scenario{
			ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g", When: "w", Then: "t",
		})
		advanceTicketToState(t, s, ticketID, "building")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if _, ok := sealedSectionHTML(t, main); ok {
			t.Fatalf(`main frame contains id="sealed-section", want none:\n%s`, main)
		}
	})
}

// assertDataFieldsDisabled fails the test unless every data-field control's
// opening tag in html -- every textarea, input and select an owner-edit box
// renders -- carries the disabled attribute (r1f2: the Save/Drop and
// claim-note checks alone leave every other control unchecked).
func assertDataFieldsDisabled(t *testing.T, html string) {
	t.Helper()
	parts := strings.Split(html, `data-field="`)
	if len(parts) < 2 {
		t.Fatalf("assertDataFieldsDisabled: no data-field controls found in:\n%s", html)
	}
	for _, p := range parts[1:] {
		end := strings.IndexByte(p, '>')
		if end < 0 {
			t.Fatalf("assertDataFieldsDisabled: no closing '>' after data-field in:\n%s", html)
		}
		if tag := p[:end]; !strings.Contains(tag, "disabled") {
			t.Errorf("data-field control not disabled: data-field=\"%s\"", tag)
		}
	}
}

// TestThreadSealedSection_ClaimedRendersDisabled proves #75's Q2: while a
// run holds the ticket's claim, every owner-edit box on the page -- the
// ticket body, the sealed section's scenario and plan-file boxes, and the
// plan task box -- renders its Save and Drop controls disabled and shows
// the store's own claim sentence. advanceTicketToState's own commit clears
// the claim, so the claim is taken again afterwards.
func TestThreadSealedSection_ClaimedRendersDisabled(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedSealedSectionFixture(t, s)
	advanceTicketToState(t, s, ticketID, "building")

	claimed, err := s.Claim(t.Context(), ticketID, "runner-1", time.Now().Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)

	saveCount := strings.Count(main, `class="owner-edit-save"`)
	saveDisabledCount := strings.Count(main, `class="owner-edit-save" disabled`)
	if saveCount < 4 {
		t.Errorf(`owner-edit-save count = %d, want at least 4 (ticket body, scenario, task, file); main:\n%s`, saveCount, main)
	}
	if saveDisabledCount != saveCount {
		t.Errorf("owner-edit-save disabled count = %d, want %d (all of them)", saveDisabledCount, saveCount)
	}

	dropCount := strings.Count(main, `class="owner-edit-drop"`)
	dropDisabledCount := strings.Count(main, `class="owner-edit-drop" disabled`)
	if dropDisabledCount != dropCount {
		t.Errorf("owner-edit-drop disabled count = %d, want %d (all of them)", dropDisabledCount, dropCount)
	}

	noteCount := strings.Count(main, store.OwnerEditClaimedReason)
	if noteCount != saveCount {
		t.Errorf("claim sentence count = %d, want %d (one per owner-edit-save box)", noteCount, saveCount)
	}

	assertDataFieldsDisabled(t, main)
}
