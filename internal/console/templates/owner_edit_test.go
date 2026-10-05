package templates

import (
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// TestOwnerEditControls_SealedScenarioOnly proves scenariosSection's own
// owner-edit box (#41, ownerEditScenarioForm) renders for a sealed scenario
// and not for an unsealed one: rendering one of each produces exactly one
// element carrying data-target="scenario".
func TestOwnerEditControls_SealedScenarioOnly(t *testing.T) {
	t.Parallel()
	rows := []ScenarioRow{
		{ID: "s1", Kind: "happy", Given: "g1", When: "w1", Then: "t1", Check: "true", Sealed: true, TicketID: 7},
		{ID: "s2", Kind: "happy", Given: "g2", When: "w2", Then: "t2", Check: "", Sealed: false, TicketID: 7},
	}

	var sb strings.Builder
	if err := scenariosSection(rows).Render(t.Context(), &sb); err != nil {
		t.Fatalf("scenariosSection.Render: %v", err)
	}
	got := sb.String()

	if n := strings.Count(got, `data-target="scenario"`); n != 1 {
		t.Errorf(`want exactly one element with data-target="scenario"; got %d in:\n%s`, n, got)
	}
}

// minimalRenderedPlan is RenderedPlan's minimal valid fixture for
// TestOwnerEditControls_TasksWhenEditable: every templ.Component field
// PlanView unconditionally renders (ContextHTML, Problem.TextHTML,
// Design.ShapeHTML) is a non-nil stand-in, and Delivery.Tasks is the only
// field this test varies.
func minimalRenderedPlan(tasks []RenderedTask, editable bool) RenderedPlan {
	return RenderedPlan{
		Overview: RenderedOverview{
			Objective:   "Ship it.",
			ContextHTML: textComponent(""),
			Problem:     RenderedProblem{TextHTML: textComponent("")},
		},
		Design: RenderedDesign{ShapeHTML: textComponent("")},
		Delivery: RenderedDelivery{
			Tasks: tasks,
		},
		TicketID: 7,
		Editable: editable,
	}
}

// TestOwnerEditControls_TasksWhenEditable proves tasksTable's own
// per-task owner-edit box (#41, ownerEditTaskForm) renders only when
// RenderedPlan.Editable is true: every task row then carries both
// owner-edit-save and owner-edit-drop, and neither renders at all when
// Editable is false.
func TestOwnerEditControls_TasksWhenEditable(t *testing.T) {
	t.Parallel()
	tasks := []RenderedTask{
		{response.Task{N: 1, Test: "go test ./x", Demo: true, Text: "do x"}, textComponent("do x")},
		{response.Task{N: 2, Test: "go test ./y", Demo: false, Text: "do y"}, textComponent("do y")},
	}

	t.Run("editable true shows save and drop on every row", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := PlanView(minimalRenderedPlan(tasks, true)).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()
		if n := strings.Count(got, "owner-edit-save"); n != len(tasks) {
			t.Errorf("want %d owner-edit-save controls; got %d in:\n%s", len(tasks), n, got)
		}
		if n := strings.Count(got, "owner-edit-drop"); n != len(tasks) {
			t.Errorf("want %d owner-edit-drop controls; got %d in:\n%s", len(tasks), n, got)
		}
	})

	t.Run("editable false shows neither", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := PlanView(minimalRenderedPlan(tasks, false)).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()
		if strings.Contains(got, "owner-edit-save") || strings.Contains(got, "owner-edit-drop") {
			t.Errorf("want no owner-edit controls when not editable; got:\n%s", got)
		}
	})
}

// TestOwnerEditControls_TicketBodyEditor proves Thread renders
// ticketBodyEditor's own owner-edit box (#41) for the open ticket: a
// data-target="ticket_body" element whose textarea holds the ticket's
// stored body, behind the one leading "\n" every owner-edit textarea opens
// with (bug fix: the HTML parser drops exactly one leading newline right
// after a textarea's opening tag, so the prefix is there precisely so a
// stored value does not lose one of its own).
func TestOwnerEditControls_TicketBodyEditor(t *testing.T) {
	t.Parallel()
	ticket := &store.Ticket{ID: 7, Title: testHelloTicketTitle, Body: "B"}

	var sb strings.Builder
	if err := Thread(ticket, nil, WaitProgress{}, "").Render(t.Context(), &sb); err != nil {
		t.Fatalf("Thread.Render: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, `data-target="ticket_body"`) {
		t.Errorf(`want a data-target="ticket_body" element; got:\n%s`, got)
	}
	if !strings.Contains(got, "<textarea data-field=\"body\">\nB</textarea>") {
		t.Errorf("want the body textarea to open with a dropped newline then contain B; got:\n%s", got)
	}
}

// TestOwnerEditControls_TextareaPreservesLeadingNewline proves the same
// prefix keeps a stored value's own leading newline intact: a body of
// "\nX" renders as "\n\nX" in the raw HTML, so the parser's one-newline
// drop leaves exactly "\nX" -- the owner's stored text unchanged.
func TestOwnerEditControls_TextareaPreservesLeadingNewline(t *testing.T) {
	t.Parallel()
	ticket := &store.Ticket{ID: 7, Title: testHelloTicketTitle, Body: "\nX"}

	var sb strings.Builder
	if err := Thread(ticket, nil, WaitProgress{}, "").Render(t.Context(), &sb); err != nil {
		t.Fatalf("Thread.Render: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, "<textarea data-field=\"body\">\n\nX</textarea>") {
		t.Errorf("want the body textarea to double up the stored leading newline; got:\n%s", got)
	}
}
