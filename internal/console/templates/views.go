// Package templates holds the console's templ components (design section
// 6.2: every console template is a .templ component, and templ generate
// turns each into a committed *_templ.go). It is a package of its own,
// rather than living inside internal/console, only because a .templ file's
// generated *_templ.go is Go source and Go compiles one package per
// directory; internal/console (server.go, handlers.go, views.go, stream.go)
// builds the view models this package renders and imports it to do so.
//
// This file holds the small view-model types the components take as
// parameters: the five views, #nav's per-thread badge list (NavThread), and
// the rail (PhaseDot, ArtifactSlot, RunRail, LogLine, LogRail, RailModel).
// Each is the presentation shape internal/console/views.go builds from
// store rows, not a store row itself, so this package stays free of any
// question-lifecycle or payload-decoding logic of its own.
package templates

import (
	"github.com/a-h/templ"

	"zing/internal/response"
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

// ScenarioRow is one scenario in the gate's context region's scenarios table
// (design section 7, D8, Task 11): the current cohort's scenario set, in
// the store's own insertion order, decoded straight from its stored
// response.Scenario payload by internal/console/views.go's loadScenarios.
type ScenarioRow struct {
	ID, Kind, Given, When, Then string
}

// FindingRow is one above-floor plan-review finding in the gate's context
// region's findings table (design section 7, D8, Task 11):
// internal/console/views.go's loadFindings has already dropped every
// finding at or below the configured review.floor, so every row here is one
// the owner, not the planning loop's own floor split (job/planning.go), must
// decide.
type FindingRow struct {
	Lens, Severity, Location, Text, Fix string
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

	// Interactive reports whether this question is still open and should
	// render its active controls -- option chips, item-decision rows, and
	// the free reply input (design section 6.6, 6.7; code review fix, PR
	// #16). An answered or resolved question renders read-only: its
	// context region and lifecycle pill still show, but console.SaveDraft
	// already refuses a draft against a closed question (openQuestionForTicketTx),
	// so a control that let a visitor try anyway was misleading, not just
	// inert. As of D30, Interactive is also true for a question already
	// answered while the ticket still waits on this round (Revisable
	// below): the owner can still change their pick before Zing resumes the
	// agent with it.
	Interactive bool

	// Revisable is D30's own narrower flag: true only for a question
	// state=answered whose ticket still waits on this round. questionGroup
	// shows its revise note ("Answered. You can change this until Zing
	// resumes the agent.") only then, distinguishing it from an ordinary
	// still-open (never answered) question, which Interactive alone cannot.
	Revisable bool

	PRURL string        // merge kind only; empty when the ticket has no PR link yet
	Plan  *RenderedPlan // gate kind only; nil when the ticket has no stored plan artifact yet

	// Scenarios and Findings are the gate kind's other two context regions
	// (design section 7, D8, Task 11), rendered before Plan inside
	// gateContext (thread.templ): the current scenario cohort and the
	// above-floor plan-review findings, both nil (rendering no table at
	// all, scenarios.templ's and findings.templ's own "no empty table"
	// rule) when the ticket has no cohort, or nothing to show at that
	// region.
	Scenarios []ScenarioRow
	Findings  []FindingRow

	// DraftReply, DraftOption, and DraftItems are the ticket's own
	// in-progress, unsent draft against this question, if any (bug fix: the
	// owner typed a reply, it saved, but the thread never rendered it back,
	// so it looked lost). freeReply (thread.templ) renders DraftReply as
	// the reply box's starting value; optionChips renders the DraftOption
	// chip picked; itemRows renders each DraftItems ref picked. Every field
	// is the zero value when this question carries no draft.
	DraftReply  string
	DraftOption string
	DraftItems  map[string]response.Decision

	// AnsweredText is a closed, state=answered question's own sent answer,
	// plainly formatted (console.sentAnswerText), empty otherwise (bug fix:
	// an answered question's controls disappeared with nothing to show in
	// their place, so the group looked inert rather than closed and
	// decided). questionGroup renders it as a locked note instead of
	// optionChips/itemRows/freeReply when !Interactive.
	AnsweredText string
}

// HasDraft reports whether this question carries any unsent draft -- a
// reply, an option pick, or at least one item pick (bug fix): Thread's own
// banner (thread.templ's draftBanner) shows only when some question in the
// ticket does.
func (q *ThreadQuestion) HasDraft() bool {
	return q.DraftReply != "" || q.DraftOption != "" || len(q.DraftItems) > 0
}

// WaitProgress is the current round's answered-vs-total count (bug fix:
// "After a partial batch, Zing keeps the ticket waiting until every open
// question is answered, which is correct design, but nothing says so").
// console.buildWaitProgress computes it from the ticket's own waiting_on and
// its questions' states; Total == 0 means nothing to show (Thread,
// thread.templ) -- the ticket is not currently question-blocked.
type WaitProgress struct {
	Answered, Total int
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
// Package 5") or that a fixture run left nil (Model, AgentTime).
type RunRail struct {
	Model, AgentTime, Attempts, Worktree, Branch string
}

// LogLine is one entry the rail's Log section renders (design section 6.11,
// 6.12): one row of console.LogEntry from the ring buffer, pre-formatted by
// console.buildLogRail (rail.go) so this package never imports log/slog of
// its own.
type LogLine struct {
	Time, Level, Message string
}

// LogRail is the rail's Log section (design section 6.11, 6.12): the
// current runtime log level and whether the open ticket's per-ticket debug
// override is on (both read fresh on every render, for the small level
// select and debug toggle POST /loglevel and /debug back), and Lines, the
// ring buffer's entries for the open ticket's own run_ids, oldest first
// (design section 6.11: "filtered by the open ticket's run_ids").
type LogRail struct {
	Level string
	Debug bool
	Lines []LogLine

	// StartedAt is the server's own start time, pre-formatted HH:MM (bug
	// fix, console.buildLogRail): Lines is empty both when the ring
	// genuinely holds nothing yet and right after a `zing serve` restart,
	// since the ring lives in memory and a restart always starts it empty.
	// logRail's empty state names StartedAt so the second case reads as
	// "quiet since the restart", not "broken".
	StartedAt string
}

// AlertLine is one row the #alerts region renders (design section 6a, D8):
// one console.LogEntry from the ring buffer's WARN-and-above slice,
// pre-formatted by console.buildAlertLines (views.go) so this package
// never imports log/slog of its own, matching LogLine's own separation.
// TicketID is meaningful only when HasTicket is true, the same shape
// LogEntry's own *int64 TicketID collapses to once console has decided
// there is one to show.
type AlertLine struct {
	Time, Level, Message string
	TicketID             int64
	HasTicket            bool
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
	Log       LogRail
}
