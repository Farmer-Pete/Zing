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

	"github.com/a-h/templ"

	"zing/internal/console/templates"
	"zing/internal/response"
)

// RenderPlan renders a stored plan's every field (design section 6.9): the
// four parts (Overview, Design, Delivery, Review) as <h2>, their children
// as <h3>, a table for Changes, Types, Files, Deletions, Tests, and Tasks,
// and a "none" line when Migrations or Deletions carries none. It reads
// plan exactly as store.GetArtifact(ticketID, "plan") unmarshals it; it
// never recomputes files, tasks, or fences into rows of its own.
func RenderPlan(plan response.Plan) (templ.Component, error) {
	rendered, err := buildRenderedPlan(plan)
	if err != nil {
		return nil, err
	}
	return templates.PlanView(rendered), nil
}

// buildRenderedPlan pre-renders plan's four markdown fields and copies
// every other field across unchanged, so templates.PlanView reads plain
// data for everything but the prose.
func buildRenderedPlan(plan response.Plan) (templates.RenderedPlan, error) {
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
			Files:     plan.Delivery.Files,
			Deletions: plan.Delivery.Deletions,
			Tests:     plan.Delivery.Tests,
			Tasks:     tasks,
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
