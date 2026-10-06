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
// TestOwnerEditControls_TasksWhenEditable and
// TestOwnerEditControls_FileTasksWhenEditable: every templ.Component field
// PlanView unconditionally renders (ContextHTML, Problem.TextHTML,
// Design.ShapeHTML) is a non-nil stand-in, and Delivery.Tasks and
// Delivery.Files are the only fields these tests vary.
func minimalRenderedPlan(tasks []RenderedTask, files []response.FileChange, editable bool) RenderedPlan {
	return RenderedPlan{
		Overview: RenderedOverview{
			Objective:   "Ship it.",
			ContextHTML: textComponent(""),
			Problem:     RenderedProblem{TextHTML: textComponent("")},
		},
		Design: RenderedDesign{ShapeHTML: textComponent("")},
		Delivery: RenderedDelivery{
			Tasks: tasks,
			Files: files,
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
		if err := PlanView(minimalRenderedPlan(tasks, nil, true)).Render(t.Context(), &sb); err != nil {
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
		if err := PlanView(minimalRenderedPlan(tasks, nil, false)).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()
		if strings.Contains(got, "owner-edit-save") || strings.Contains(got, "owner-edit-drop") {
			t.Errorf("want no owner-edit controls when not editable; got:\n%s", got)
		}
	})
}

// TestOwnerEditControls_FileTasksWhenEditable proves filesTable's own
// per-file owner-edit box (#51, ownerEditFileForm) renders only when
// RenderedPlan.Editable is true: every render shows the Tasks header and
// each file's task list, the editable render carries one
// data-target="plan_file" box per file with data-ref set to the path and an
// input data-field="tasks" holding the file's task list, and the read-only
// render carries no data-target="plan_file" at all.
func TestOwnerEditControls_FileTasksWhenEditable(t *testing.T) {
	t.Parallel()
	files := []response.FileChange{
		{Path: "internal/one.go", Action: response.FileActionModify, Task: "1", Reason: "r1"},
		{Path: "internal/store/console_reads.go", Action: response.FileActionModify, Task: "6", Reason: "r2"},
	}

	t.Run("editable true shows a box per file", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := PlanView(minimalRenderedPlan(nil, files, true)).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()
		if !strings.Contains(got, "<th>Tasks</th>") {
			t.Errorf("want a Tasks header; got:\n%s", got)
		}
		for _, f := range files {
			if !strings.Contains(got, "<td>"+f.Task+"</td>") {
				t.Errorf("want the file's task list %q rendered; got:\n%s", f.Task, got)
			}
		}
		if n := strings.Count(got, `data-target="plan_file"`); n != len(files) {
			t.Errorf(`want %d data-target="plan_file" boxes; got %d in:\n%s`, len(files), n, got)
		}
		for _, f := range files {
			if !strings.Contains(got, `data-ref="`+f.Path+`"`) {
				t.Errorf("want a box with data-ref=%q; got:\n%s", f.Path, got)
			}
		}
		if !strings.Contains(got, `data-field="tasks" value="1"`) {
			t.Errorf(`want an input data-field="tasks" value="1"; got:\n%s`, got)
		}
	})

	t.Run("editable false shows none", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := PlanView(minimalRenderedPlan(nil, files, false)).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()
		if !strings.Contains(got, "<th>Tasks</th>") {
			t.Errorf("want a Tasks header; got:\n%s", got)
		}
		for _, f := range files {
			if !strings.Contains(got, "<td>"+f.Task+"</td>") {
				t.Errorf("want the file's task list %q rendered; got:\n%s", f.Task, got)
			}
		}
		if strings.Contains(got, `data-target="plan_file"`) {
			t.Errorf(`want no data-target="plan_file" box when not editable; got:\n%s`, got)
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
