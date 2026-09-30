// plan.go builds a RenderedPlan from a stored response.Plan (design section
// 6.9) and renders it. It pre-renders every markdown field -- Overview.
// Context, Overview.Problem.Text, Design.Shape, and each Delivery.Tasks[n].
// Text -- through Render (render.go), the same Task 5 helper
// buildThreadQuestion (views.go) already uses for a question's body and
// recommendation, so internal/console/templates never imports
// html/template of its own and there is exactly one markdown path.
package console

import (
	"fmt"
	"sort"

	"github.com/a-h/templ"

	"zing/internal/console/templates"
	"zing/internal/response"
	"zing/internal/store"
)

// RenderPlan renders a stored plan's every field (design section 6.9): the
// four parts (Overview, Design, Delivery, Review) as <h2>, their children
// as <h3>, a table for Changes, Types, Files, Deletions, Tests, and Tasks,
// and a "none" line when Migrations or Deletions carries none. It reads
// plan exactly as store.GetArtifact(ticketID, "plan") unmarshals it; it
// never recomputes files, tasks, or fences into rows of its own. events is
// the ticket's FileEvents (design section 9.2): the Declared files section
// gains a "Decided during build" sub-list, fed by the newest row per path
// that carries a decision.
func RenderPlan(plan response.Plan, events []store.FileEventRow) (templ.Component, error) {
	rendered, err := buildRenderedPlan(plan, events)
	if err != nil {
		return nil, err
	}
	return templates.PlanView(rendered), nil
}

// decidedFiles reduces events to the newest file artifact per path that
// carries a decision (design section 9.2), sorted by path: an owner's
// perimeter accept or reject, the same reduction internal/job's building
// handler applies to decide what is declared, read here only to display.
func decidedFiles(events []store.FileEventRow) []response.FileArtifact {
	newest := make(map[string]response.FileArtifact, len(events))
	for _, e := range events {
		newest[e.File.Path] = e.File
	}
	out := make([]response.FileArtifact, 0, len(newest))
	for _, fa := range newest {
		if fa.Decision != nil {
			out = append(out, fa)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// buildRenderedPlan pre-renders plan's four markdown fields and copies
// every other field across unchanged, so templates.PlanView reads plain
// data for everything but the prose. events feeds Delivery.DecidedFiles
// (design section 9.2).
func buildRenderedPlan(plan response.Plan, events []store.FileEventRow) (templates.RenderedPlan, error) {
	contextHTML, err := Render(plan.Overview.Context)
	if err != nil {
		return templates.RenderedPlan{}, fmt.Errorf("console: render plan overview context: %w", err)
	}
	problemTextHTML, err := Render(plan.Overview.Problem.Text)
	if err != nil {
		return templates.RenderedPlan{}, fmt.Errorf("console: render plan problem text: %w", err)
	}
	shapeHTML, err := Render(plan.Design.Shape)
	if err != nil {
		return templates.RenderedPlan{}, fmt.Errorf("console: render plan design shape: %w", err)
	}
	tasks, err := buildRenderedTasks(plan.Delivery.Tasks)
	if err != nil {
		return templates.RenderedPlan{}, err
	}

	return templates.RenderedPlan{
		Overview: templates.RenderedOverview{
			Objective:   plan.Overview.Objective,
			ContextHTML: contextHTML,
			Problem: templates.RenderedProblem{
				TextHTML:   problemTextHTML,
				Loop:       plan.Overview.Problem.Loop,
				Repro:      plan.Overview.Problem.Repro,
				Hypotheses: plan.Overview.Problem.Hypotheses,
			},
			Goals:    plan.Overview.Goals,
			NonGoals: plan.Overview.NonGoals,
		},
		Design: templates.RenderedDesign{
			Demo:       plan.Design.Demo,
			ShapeHTML:  shapeHTML,
			Changes:    plan.Design.Changes,
			Types:      plan.Design.Types,
			Migrations: plan.Design.Migrations,
		},
		Delivery: templates.RenderedDelivery{
			Files:        plan.Delivery.Files,
			Deletions:    plan.Delivery.Deletions,
			Tests:        plan.Delivery.Tests,
			Tasks:        tasks,
			DecidedFiles: decidedFiles(events),
		},
		Review: plan.Review,
	}, nil
}

// buildRenderedTasks pre-renders each task's markdown Text (its own doc
// tag: "what to build, exactly, markdown").
func buildRenderedTasks(tasks []response.Task) ([]templates.RenderedTask, error) {
	out := make([]templates.RenderedTask, 0, len(tasks))
	for _, t := range tasks {
		textHTML, err := Render(t.Text)
		if err != nil {
			return nil, fmt.Errorf("console: render plan task %d text: %w", t.N, err)
		}
		out = append(out, templates.RenderedTask{Task: t, TextHTML: textHTML})
	}
	return out, nil
}
