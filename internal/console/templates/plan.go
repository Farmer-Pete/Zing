// plan.go holds the plan renderer's view-model types (design section 6.9):
// the presentation shape internal/console/plan.go builds from a stored
// response.Plan, pre-rendering every markdown field through the Task 5
// Render helper so this package never imports html/template of its own --
// the same split views.go documents for ThreadQuestion's BodyHTML and
// RecommendedHTML.
package templates

import (
	"github.com/a-h/templ"

	"zing/internal/response"
)

// RenderedProblem is Overview.Problem (design section 6.9) with its
// markdown Text pre-rendered. Loop, Repro, and Hypotheses carry no markdown
// field of their own (bug only; Loop is nil for a feature).
type RenderedProblem struct {
	TextHTML   templ.Component
	Loop       *response.Loop
	Repro      string
	Hypotheses []response.Hypothesis
}

// RenderedOverview is Overview (design section 6.9) with its markdown
// Context pre-rendered.
type RenderedOverview struct {
	Objective   string
	ContextHTML templ.Component
	Problem     RenderedProblem
	Goals       []string
	NonGoals    []string
}

// RenderedDesign is Design (design section 6.9) with its markdown Shape
// pre-rendered. Changes, Types, and Migrations carry no markdown field of
// their own.
type RenderedDesign struct {
	Demo       response.Demo
	ShapeHTML  templ.Component
	Changes    []response.Change
	Types      []response.TypeDef
	Migrations response.Migrations
}

// RenderedTask is one Task with its markdown Text pre-rendered (its own
// doc tag in internal/response/types.go: "what to build, exactly,
// markdown").
type RenderedTask struct {
	response.Task
	TextHTML templ.Component
}

// RenderedDelivery is Delivery (design section 6.9) with each Task's Text
// pre-rendered. Files, Deletions, and Tests carry no markdown field of
// their own. DecidedFiles is design section 9.2's own addition: the newest
// FileEvents row per path that carries an owner decision (path, decision,
// task number, marker, builder reason, perimeter description), not part of
// the stored Plan itself.
type RenderedDelivery struct {
	Files        []response.FileChange
	Deletions    response.Deletions
	Tests        []response.TestCase
	Tasks        []RenderedTask
	DecidedFiles []response.FileArtifact
}

// RenderedPlan is a stored response.Plan (design section 6.9) with every
// markdown field pre-rendered: Overview.Context, Overview.Problem.Text,
// Design.Shape, and each Delivery.Tasks[n].Text. Review carries no markdown
// field of its own. internal/console/plan.go builds one from a
// store.GetArtifact(ticketID, "plan") payload; PlanView (plan.templ) is the
// only thing that reads it. TicketID and Editable (#41) are set only by
// internal/console/views.go's loadPlan, for the gate's own rendering of the
// current plan: RenderPlan (the artifacts rail) leaves both zero, so the
// rail's own copy of the plan stays read-only. Editable is true only when
// the plan's cohort has at least one sealed scenario (console.planSealed),
// matching store.OwnerEdit's own not_sealed check for a plan task. Claimed
// (#75) is true only when the ticket's ClaimOwner is non-nil, set by
// console.threadComponent: it renders every owner-edit box filesTable and
// tasksTable build disabled with the store's own claim sentence. The
// rail's RenderPlan leaves it false, same as Editable.
type RenderedPlan struct {
	Overview RenderedOverview
	Design   RenderedDesign
	Delivery RenderedDelivery
	Review   response.Review
	TicketID int64
	Editable bool
	Claimed  bool
}
