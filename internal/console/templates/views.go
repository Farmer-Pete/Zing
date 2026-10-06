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

// FeedRow is one message the Feed view renders (design section 6.5):
// console.displayFeedMessages' own decode of its Body (bug fix, PR #16)
// plus BodyHTML, that decoded text markdown-rendered (bug fix: raw
// backticks showed literally in the Feed, the owner's locked-view
// complaint, design section 22.7).
type FeedRow struct {
	ID, TicketID int64
	Type, Author string
	BodyHTML     templ.Component
}

// InboxGroup is one project's cluster of inbox cards, the Inbox view's
// grouping unit (design section 6.5: "grouped by project, blocking first").
// Groups appear in the order their first item was encountered in
// store.InboxItems' own blocking-first, newest-first order, so that overall
// priority order survives the grouping.
type InboxGroup struct {
	ProjectName string
	Items       []store.InboxItem
}

// NavThread is one live ticket's row in #nav's per-ticket list (design
// section 6.3, 6.8, #106 bug 4): the row shows the blocking badge when
// Blocking, the unread badge when Unread, and otherwise its state pill.
type NavThread struct {
	Ticket            store.Ticket
	Blocking          bool
	Unread            bool   // threadLink reads it only when Blocking is false
	WaitingOn         string // meaningful only when Blocking
	OpenQuestionCount int

	// ParkedUntil is clockLabel of this ticket's own store.LiveTicket.
	// ParkedUntil (#45), such as "12:20pm", when that time is still after
	// now; empty otherwise. threadLink renders it as its own badge, after
	// Blocking but ahead of Unread and the state pill, so a parked ticket's
	// row names the one thing the owner cannot act on until the reset.
	ParkedUntil string
}

// ThreadOption is one option chip a question group renders: a label plus
// the option key the picked chip's draft answer would carry (design section
// 6.6, 6.7). optionChips (thread.templ) numbers these 1..N for the
// keyboard's chip action.
type ThreadOption struct {
	Key, Text string

	// TextHTML is Text run through console.RenderInline (bug fix: raw
	// backticks in option chips, design section 22.7's owner-reported
	// locked-view complaint): optionChips renders this, inline-safe markdown,
	// in place of the plain Text string a <button> used to show verbatim.
	TextHTML templ.Component
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
// Check, Sealed, and TicketID (#41) feed scenariosSection's own owner-edit
// box, rendered only when Sealed: Check is the scenario's check_cmd, Sealed
// is whether the artifact's sealed_at is set, and TicketID is the owning
// ticket, the box's data-ticket attribute.
type ScenarioRow struct {
	ID, Kind, Given, When, Then, Check string
	Sealed                             bool
	TicketID                           int64
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

	// ReopenPlaceholder is D32's own reply-box placeholder for a settled,
	// still-reopenable planning question (design section 22.12.2, 22.12.4):
	// "Write to reopen Q1", with "and withdraw the gate" appended while a
	// gate question is currently open. Empty for every other question --
	// still open (Interactive's own box renders instead, with the default
	// placeholder), not a planning question, or a planning question already
	// locked for good because the ticket left "planning" -- so questionGroup
	// (thread.templ) treats a non-empty value as "show the reply box anyway,
	// with this placeholder" alongside Interactive, never instead of it.
	ReopenPlaceholder string

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
	// the reply box's starting value. Every field is the zero value when
	// this question carries no draft. DraftOption and DraftItems feed
	// optionChips/itemRows only while Interactive (console.effectivePickedOption,
	// effectivePickedItems); PickedOption and PickedItems below are what
	// those same controls render once the question is locked.
	DraftReply  string
	DraftOption string
	DraftItems  map[string]response.Decision

	// PickedOption and PickedItems are this question's own sent answer,
	// decoded once (console.collectSentAnswers) and kept separate from
	// DraftOption/DraftItems (bug fix: "options vanish once locked" --
	// optionChips and itemRows used to render only while Interactive, so a
	// settled or answered question showed no trace of what was picked).
	// They are set only once the question is no longer Interactive, so
	// HasDraft (the draft banner's own guard) never sees a sent answer as
	// an unsent draft. console.effectivePickedOption/effectivePickedItems
	// are the one place that reads them, falling back to Draft* while the
	// question is still open.
	PickedOption string
	PickedItems  map[string]response.Decision

	// AnsweredHTML is a closed, state=answered question's own sent answer,
	// pre-rendered as markdown (bug fix: raw backticks showed literally;
	// design section 22.7 bug-fix table), nil otherwise. questionGroup
	// renders it as a locked note instead of optionChips/itemRows/freeReply
	// when !Interactive and this question carries no SettledHTML (a
	// planning question's own, differently worded locked note).
	AnsweredHTML templ.Component

	// Turns is every sent (never draft) reply or answer naming this
	// question as its parent, each pre-rendered as markdown and labeled by
	// who wrote it (bug fix 10, extended by D31-5): each used to also get
	// its own standalone ThreadRow, so a typed reply like "Explain these
	// three options in more detail" showed up as a "reply you" card at the
	// bottom of the thread, detached from the question it was actually
	// about. For a planning question (design section 22.1, 22.7) this is
	// the whole conversation in turn order -- owner picks and texts, the
	// agent's own replies, each tagged Queued when the owner sent it but no
	// run has taken delivery of it yet -- built from
	// store.PlanningConversation; for every other kind it is each sent
	// reply (never an answer -- AnsweredHTML already shows the pick, so
	// repeating it here was the "duplicate Answered: plus You:" bug), in
	// message order, always labeled "You".
	Turns []Turn

	// SettledLabel and SettledHTML are a settled planning question's own
	// closing line (design section 22.7 item 6): "Settled by <agent>:" and
	// the agent's decision, pre-rendered as markdown. Both are zero for
	// every other kind, and for a planning question the agent settled with
	// no decision text to show (the owner-abandoned path, design section
	// 22.3's resolved/system row): that row renders as an ordinary,
	// unlabeled Turn ("Resolved.") instead, so a settled thread is never
	// left with an empty closing line.
	SettledLabel string
	SettledHTML  templ.Component
}

// TicketActions is the thread view's action bar (#65, design section 6.6's
// owner actions): console.actionsFor builds it from one ticket's own state,
// claim, and (for an abandoned ticket) whether a live successor exists at
// its ref.
type TicketActions struct {
	// Abandon reports whether the Abandon button renders at all: exactly
	// store.CanAbandon(ticket.State).
	Abandon bool

	// Restart reports whether the Restart from planning button renders:
	// wherever Abandon does, and also on an abandoned ticket while no live
	// ticket holds its ref.
	Restart bool

	// Held reports whether a run currently holds the ticket (claim_owner is
	// set): both buttons render disabled, with AbandonClaimedReason as the
	// note, while this is true.
	Held bool

	// Ref is the ticket's issue ref with any -abandoned-K suffix stripped
	// (store.SplitAttemptRef), the action bar's data-ref attribute and the
	// confirm dialog's own wording.
	Ref string
}

// Turn is one line of a question's own conversation (design section 22.7):
// an owner pick or text, the agent's reply, or -- unlabeled, Author "" --
// a bare system note such as "Resolved." (design/threading-design.md (d)'s
// fallback for a row placement put inside a question but that carries no
// turn of its own, bug 14). BodyHTML is always markdown-rendered, even for
// a plain reply, so a backtick in it never shows raw (bug fix). Queued is
// meaningful only for a planning question's own owner turns: true when the
// owner sent it but no run has yet taken delivery of it (design section
// 22.3, 22.7 item 5).
type Turn struct {
	Author   string
	BodyHTML templ.Component
	Queued   bool
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

// ThreadRow is one message the read-only Thread view renders: a read-only
// question group in place of Body when Question is non-nil (a "question" or
// "escalation" row always opens its own thread this way, design section
// 6.6); a one-line timeline divider when Divider is true -- a state
// transition, or any other unparented row this view does not otherwise
// expect, including a recognized or unrecognized "update" marker
// (design/threading-design.md (d), task D31-4a: "placement by structure,
// not by type" -- any row with a parent_id renders inside its parent's
// thread instead of reaching here at all, so the only rows that ever become
// a ThreadRow of their own are a question, an escalation, a divider, or the
// one allowed card below); or a plain message card -- only ever an
// unparented owner reply, buildThreadRows' sole exception to the divider
// default.
type ThreadRow struct {
	ID       int64
	Type     string
	Author   string
	Body     string
	Divider  bool
	Question *ThreadQuestion
}

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
// second page (console.buildArtifactsRail, rail.go). RenderedHTML is set
// only for the Plan slot (bug fix 16: the rail showed the plan artifact as
// raw indented JSON): console.buildArtifactsRail runs it through the same
// RenderPlan the gate's own context region uses, and artifactsRail
// (rail.templ) prefers it over PayloadText when it is non-nil.
type ArtifactSlot struct {
	Label        string
	Present      bool
	Version      int
	AfterPhase   string
	PayloadText  string
	RenderedHTML templ.Component
}

// RunRail is the rail's Run section (design section 6.11): the newest
// session's newest run. Every field is pre-formatted by
// console.buildRunRail (rail.go), "-" standing in for a value this package
// cannot supply yet (Worktree, Branch -- design section 6.11: "arrive with
// Package 5") or that a fixture run left nil (Model, AgentTime). Interrupted
// is the newest run's own store.Run.Interrupted (#45 design section 9):
// rail.templ renders the word "interrupted" right after Model when true.
// That is only while the interrupted run is still the ticket's newest run --
// until its own free resume (section 7.4) starts: the resume's own run row
// carries Interrupted=false (migration 0005's own default) and, once it
// exists, is the newest run in its place, so the pill is gone from then on.
type RunRail struct {
	Model, AgentTime, Attempts, Worktree, Branch string
	Interrupted                                  bool

	// Runs lists every run of the open ticket, newest (greatest id) first
	// (console.runRows, rail.go; #43 split). nil or empty renders "No runs
	// yet." (rail.templ's runList).
	Runs []RunRow
}

// RunRow is one row of the rail's Run list (design section 6.11, #43
// split): one runs table row joined with its session's job and runtime.
// Every field is pre-formatted by console.runRows (rail.go). Job and
// Runtime read "-" when the row's session is missing from the sessions
// read that built it; Model reads "-" when runs.model is NULL; Outcome
// reads "running" when runs.outcome is NULL; AgentTime reads "-" when
// runs.agent_seconds is NULL. FinalURL and StderrURL are "" unless the run
// kept that evidence (RecordRunEvidence, migration 0007), in which case
// rail.templ renders a link to GET /runs/{id}/{kind}. Transcript is the
// stored transcript path, shown as plain text when TranscriptURL is "".
// TranscriptURL is set only when the stored path is this run's own
// Zing-written stdout file (store.StdoutFileName), in which case
// rail.templ renders a link to GET /runs/{id}/transcript instead; a
// Claude transcript lives outside the data directory and the console never
// serves it, so it keeps rendering as plain text.
type RunRow struct {
	ID            int64
	Job           string
	Runtime       string
	Model         string
	Outcome       string
	Interrupted   bool
	AgentTime     string
	FinalURL      string
	StderrURL     string
	Transcript    string
	TranscriptURL string
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
