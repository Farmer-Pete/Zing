// Package templates holds the console's templ components (design section
// 6.2: every console template is a .templ component, and templ generate
// turns each into a committed *_templ.go). It is a package of its own,
// rather than living inside internal/console, only because a .templ file's
// generated *_templ.go is Go source and Go compiles one package per
// directory; internal/console (server.go, handlers.go, views.go, stream.go)
// builds the view models this package renders and imports it to do so.
//
// This file holds the small view-model types the five views' components
// take as parameters: the presentation shape internal/console/views.go
// builds from store rows, not store rows themselves, so this package stays
// free of any question-lifecycle or payload-decoding logic of its own.
package templates

import (
	"github.com/a-h/templ"

	"zing/internal/store"
)

// InboxGroup is one project's cluster of inbox cards, the Inbox view's
// grouping unit (design section 6.5: "grouped by project, blocking first").
// Groups appear in the order their first item was encountered in
// store.InboxItems' own blocking-first, newest-first order, so that overall
// priority order survives the grouping.
type InboxGroup struct {
	ProjectName string
	Items       []store.InboxItem
}

// NavThread is one blocking-or-unread ticket's row in #nav's per-thread
// badge list (design section 6.3, 6.8): the badge shows the waiting flag
// when blocking, an unread dot otherwise.
type NavThread struct {
	Ticket            store.Ticket
	Blocking          bool
	WaitingOn         string // meaningful only when Blocking
	OpenQuestionCount int
}

// ThreadOption is one option chip a question group renders: a label plus
// the option key the picked chip's draft answer would carry (design section
// 6.6, 6.7). optionChips (thread.templ) numbers these 1..N for the
// keyboard's chip action.
type ThreadOption struct {
	Key, Text string
}

// ThreadItem is one row an item-kind question (perimeter, review) renders:
// one file or finding with its own accept/reject/drop/discuss controls
// (design section 6.6, 8: the closed set of four item decisions).
type ThreadItem struct {
	Ref, Text string
}

// ThreadQuestion is the detail a "question" message renders in place of a
// plain body: its key, title, and message count for the <details> summary
// (design section 6.6), its body and recommendation (pre-rendered through
// the Task 5 Render helper, so this package never imports html/template of
// its own), its kind (dispatching the control thread.templ renders: option
// chips for the four option kinds, item rows for the two item kinds), its
// options or items, a pill label for its lifecycle state, PRURL, the merge
// kind's minimal context (design section 12, Task 6 scope: merge's is
// already on the Ticket row, so it renders for real), and Plan, the gate
// kind's context (design section 6.9, Task 8): the ticket's stored plan
// artifact, pre-rendered by internal/console/plan.go, or nil when none is
// stored yet.
type ThreadQuestion struct {
	Key, Title      string
	Kind            string
	BodyHTML        templ.Component
	Recommended     string
	RecommendedHTML templ.Component // nil when Recommended is empty
	Options         []ThreadOption
	Items           []ThreadItem
	StateLabel      string
	MessageCount    int
	PRURL           string        // merge kind only; empty when the ticket has no PR link yet
	Plan            *RenderedPlan // gate kind only; nil when the ticket has no stored plan artifact yet
}

// ThreadRow is one message the read-only Thread view renders: a state
// separator (Type == "state"), or a plain row (update, escalation, or any
// other type) with Body as its already-decoded display text, or, when
// Question is non-nil, a read-only question group in place of Body (design
// section 6.6).
type ThreadRow struct {
	ID       int64
	Type     string
	Author   string
	Body     string
	Question *ThreadQuestion
}

// IsState reports whether this row is a state-transition separator, the one
// row type the Thread view centers rather than left-aligning (design
// section 6.6).
func (r ThreadRow) IsState() bool { return r.Type == "state" }

// PhaseDot is one state in the rail's Phase section (design section 6.11):
// machine.States.Order drawn as dots, each before, at, or after the
// ticket's own state. Waiting is only meaningful when Status is "now"
// (design section 6.11: "The now row shows waiting when the ticket's
// waiting_on is set").
type PhaseDot struct {
	State   string
	Status  string // "done", "now", or "upcoming"
	Waiting bool
}

// ArtifactSlot is one of the rail's seven labeled artifact slots (design
// section 6.11): Scenarios, Decisions, Plan, Design doc, Explainer, Review
// report, Judge verdict. Present is false when the ticket carries no
// artifact of that slot's type yet, and the rail shows "after AfterPhase"
// in its place. PayloadText is the present artifact's pretty-printed JSON
// payload, shown inline through a <details> disclosure when Present: design
// section 7.1's route table names no artifact-viewing endpoint, so "links
// to open it" (design section 6.11) is an in-page disclosure rather than a
// second page (console.buildArtifactsRail, rail.go).
type ArtifactSlot struct {
	Label       string
	Present     bool
	Version     int
	AfterPhase  string
	PayloadText string
}

// RunRail is the rail's Run section (design section 6.11): the newest
// session's newest run. Every field is pre-formatted by
// console.buildRunRail (rail.go), "-" standing in for a value this package
// cannot supply yet (Worktree, Branch -- design section 6.11: "arrive with
// Package 5") or that a fixture run left nil (Model, AgentSeconds).
type RunRail struct {
	Model, AgentTime, Attempts, Worktree, Branch string
}

// RailModel is the #rail region's full content (design section 6.11), built
// by console.buildRailModel from the open ticket's store reads plus
// machine.States.Order. Rail(nil) renders the same empty placeholder every
// non-thread or ticket-less /stream frame still needs (design section 6.3:
// "#rail ... is always patched, so leaving a thread clears the old rail").
type RailModel struct {
	Phase     []PhaseDot
	Artifacts []ArtifactSlot
	Run       RunRail
}
