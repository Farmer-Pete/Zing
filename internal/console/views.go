// views.go builds the five views' per-view models from the store reads
// Task 2 added and renders them through the templ components in
// internal/console/templates (design section 6.5, Task 3). #nav's model
// (design section 6.3, 6.8) lives here too: it is not one of the five
// views, but it is built the same way, from the same InboxItems read.
package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/a-h/templ"

	"zing/internal/console/templates"
	"zing/internal/response"
	"zing/internal/store"
)

// The five views' names, the closed set design section 8 names. keys.go
// (Task 4) becomes the keyboard map's source of truth, but /stream and
// views.go need the same five strings before that exists.
const (
	viewInbox   = "inbox"
	viewRecent  = "recent"
	viewFeed    = "feed"
	viewProject = "project"
	viewThread  = "thread"
)

// feedLimit is the message count views.go asks the feed for: the design's
// default clamp (design section 6.5: "capped at a clamped limit (1 to 200,
// default 200)"); store.FeedMessages clamps it again on its own, so this is
// belt and suspenders, not the only guard.
const feedLimit = 200

// alertsLimit caps the #alerts region (design section 6a, D8): "Warnings
// (alertsLimit = 20)". The ring itself (RingCapacity, log.go) remembers far
// more; the alerts strip is a live glance at the newest ones, not a full
// history -- the Log rail (rail.go's buildLogRail), filtered to one
// ticket's run_ids, is where the fuller history lives.
const alertsLimit = 20

// alertLineTimeFormat is the alerts region's per-line clock time (design
// section 6a: "the time (15:04:05)"), distinct from the Log rail's own
// millisecond-precision logLineTimeFormat (rail.go): an operator glancing
// at a live warning strip needs the second it happened, not sub-second
// precision.
const alertLineTimeFormat = "15:04:05"

// alertsComponent builds the #alerts region (design section 6a, D8): every
// WARN-and-above line the log Handler's ring currently holds, newest first.
// Unlike #main and #rail, it takes no navigation signals: it renders the
// same content on every frame regardless of which view or ticket is open,
// so patchRegions (stream.go) can call it without sig.
func (c *console) alertsComponent() templ.Component {
	return templates.Alerts(buildAlertLines(c.log.Warnings(alertsLimit)))
}

// buildAlertLines turns the handler's ring entries into the #alerts
// region's view model: a compact clock time, the level's own String, the
// message verbatim (LogEntry never holds a body; see log.go's FencedAttr
// discipline), and the ticket id when the entry carries one.
func buildAlertLines(entries []LogEntry) []templates.AlertLine {
	lines := make([]templates.AlertLine, 0, len(entries))
	for _, e := range entries {
		line := templates.AlertLine{
			Time:    e.Time.Format(alertLineTimeFormat),
			Level:   e.Level.String(),
			Message: e.Message,
		}
		if e.TicketID != nil {
			line.TicketID = *e.TicketID
			line.HasTicket = true
		}
		lines = append(lines, line)
	}
	return lines
}

// navComponent builds the #nav region: every project, and the per-thread
// blocking/unread badge list built from store.InboxItems, the same
// blocking-or-unread predicate section 6.8 defines (design section 6.3).
// open is the thread view's own open ticket, 0 when none is open (bug fix:
// nav.templ's threadLink renders "selected" on this one row, so the
// sidebar's highlight is part of #nav's own HTML and survives every patch).
func (c *console) navComponent(ctx context.Context, open int64) (templ.Component, error) {
	projects, err := c.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	items, err := c.store.InboxItems(ctx)
	if err != nil {
		return nil, err
	}
	return templates.Nav(projects, buildNavThreads(items), c.sandboxReason, open), nil
}

// buildNavThreads turns InboxItems into #nav's badge rows, preserving their
// blocking-first, newest-first order (design section 7.2).
func buildNavThreads(items []store.InboxItem) []templates.NavThread {
	out := make([]templates.NavThread, 0, len(items))
	for i := range items {
		th := templates.NavThread{
			Ticket:            items[i].Ticket,
			OpenQuestionCount: len(items[i].OpenQuestions),
		}
		if items[i].Ticket.WaitingOn != nil {
			th.Blocking = true
			th.WaitingOn = *items[i].Ticket.WaitingOn
		}
		out = append(out, th)
	}
	return out
}

// mainComponent builds the #main region for the current view (design
// section 6.3, 6.5): Inbox, Recent, Feed, and Project each read straight
// from their store method; Thread additionally reads the ticket and its
// messages. An unrecognized view falls back to Inbox, matching the shell's
// own data-signals default.
func (c *console) mainComponent(ctx context.Context, view string, open, project int64) (templ.Component, error) {
	switch view {
	case viewRecent:
		tickets, err := c.store.RecentTickets(ctx)
		if err != nil {
			return nil, err
		}
		return templates.Recent(tickets), nil
	case viewFeed:
		messages, err := c.store.FeedMessages(ctx, feedLimit)
		if err != nil {
			return nil, err
		}
		return templates.Feed(displayFeedMessages(messages)), nil
	case viewProject:
		tickets, err := c.store.TicketsByProject(ctx, project)
		if err != nil {
			return nil, err
		}
		return templates.Project(project, tickets), nil
	case viewThread:
		return c.threadComponent(ctx, open)
	default:
		return c.inboxComponent(ctx)
	}
}

// inboxComponent builds the Inbox view (design section 6.5): tickets that
// are blocking or have an unread message, grouped by project under
// headings, blocking first.
func (c *console) inboxComponent(ctx context.Context) (templ.Component, error) {
	items, err := c.store.InboxItems(ctx)
	if err != nil {
		return nil, err
	}
	return templates.Inbox(buildInboxGroups(items)), nil
}

// buildInboxGroups clusters InboxItems by project, under the heading of
// each project's first-encountered item, so the overall blocking-first,
// newest-first order (design section 7.2) survives the grouping (design
// section 6.5: "grouped by project, blocking first").
func buildInboxGroups(items []store.InboxItem) []templates.InboxGroup {
	var groups []templates.InboxGroup
	index := make(map[string]int, len(items))
	for n := range items {
		i, ok := index[items[n].ProjectName]
		if !ok {
			i = len(groups)
			index[items[n].ProjectName] = i
			groups = append(groups, templates.InboxGroup{ProjectName: items[n].ProjectName})
		}
		groups[i].Items = append(groups[i].Items, items[n])
	}
	return groups
}

// displayFeedMessages returns a copy of messages with each row's Body
// replaced by displayBody's decoding (code review fix, PR #16): the Feed
// view passed store.FeedMessages' raw rows straight to templates.Feed,
// which renders m.Body verbatim, so a state row (whose Body a commit
// leaves empty, the transition living in Payload instead) and a sent
// answer row (whose Body SaveDraft and SendBatch never set, the choice
// living in Payload instead) both rendered blank -- only the Thread view,
// through buildThreadRows, ever ran a row's Body through displayBody. The
// original rows are left untouched; displayBody reads from the copy still
// carrying the original Payload and Type, so decoding is unaffected by the
// Body overwrite.
func displayFeedMessages(messages []store.MessageRow) []store.MessageRow {
	out := make([]store.MessageRow, len(messages))
	for i := range messages {
		out[i] = messages[i]
		out[i].Body = displayBody(&messages[i])
	}
	return out
}

// threadComponent builds the read-only Thread view for the open ticket:
// nil ticket and no rows when open is 0 or names no ticket (design section
// 6.6, carried over from Package 3's patchThread guard).
func (c *console) threadComponent(ctx context.Context, open int64) (templ.Component, error) {
	if open <= 0 {
		return templates.Thread(nil, nil), nil
	}
	ticket, err := c.store.GetTicket(ctx, open)
	switch {
	case err == nil:
		rows, listErr := c.store.ListMessages(ctx, open)
		if listErr != nil {
			return nil, listErr
		}
		plan, planErr := c.loadPlan(ctx, open)
		if planErr != nil {
			return nil, planErr
		}
		scenarios, scenariosErr := c.loadScenarios(ctx, open)
		if scenariosErr != nil {
			return nil, scenariosErr
		}
		findings, findingsErr := c.loadFindings(ctx, open)
		if findingsErr != nil {
			return nil, findingsErr
		}
		threadRows, buildErr := buildThreadRows(&ticket, rows, plan, scenarios, findings)
		if buildErr != nil {
			return nil, buildErr
		}
		return templates.Thread(&ticket, threadRows), nil
	case errors.Is(err, sql.ErrNoRows):
		return templates.Thread(nil, nil), nil
	default:
		return nil, err
	}
}

// planArtifactType is the artifacts.type literal a planning commit writes
// (internal/store/schemas/artifacts/plan.json), the type name
// store.GetArtifact(ticketID, "plan") in design section 6.9 names directly.
const planArtifactType = "plan"

// loadPlan reads ticketID's newest stored plan artifact (design section
// 6.9) and pre-renders it, for the gate kind's context region
// (thread.templ's gateContext). It returns nil, nil when the ticket has no
// plan artifact yet, the same "not there yet" shape store.GetArtifact
// itself returns (ok == false, err == nil).
func (c *console) loadPlan(ctx context.Context, ticketID int64) (*templates.RenderedPlan, error) {
	artifact, ok, err := c.store.GetArtifact(ctx, ticketID, planArtifactType)
	if err != nil {
		return nil, fmt.Errorf("console: load plan artifact for ticket %d: %w", ticketID, err)
	}
	if !ok {
		return nil, nil //nolint:nilnil // "no plan stored yet" is a legitimate result, not an error
	}
	var plan response.Plan
	if unmarshalErr := json.Unmarshal(artifact.Payload, &plan); unmarshalErr != nil {
		return nil, fmt.Errorf("console: unmarshal plan artifact for ticket %d: %w", ticketID, unmarshalErr)
	}
	events, err := c.store.FileEvents(ctx, ticketID)
	if err != nil {
		return nil, fmt.Errorf("console: load file events for ticket %d: %w", ticketID, err)
	}
	rendered, err := buildRenderedPlan(plan, events)
	if err != nil {
		return nil, fmt.Errorf("console: render plan artifact for ticket %d: %w", ticketID, err)
	}
	return &rendered, nil
}

// loadScenarios reads ticketID's current scenario cohort for the gate's
// context region (design section 7, D8, Task 11): CurrentCohort names the
// producing run; ScenariosForRun(*cohort.RunID, sealedOnly=false) reads it
// when there is one, so a still-pending (unsealed) cohort still renders.
// cohort.RunID == nil is a legacy plan artifact stored before every artifact
// carried run_id, so this falls back to AllScenarios (every scenario the
// ticket has ever carried, across every run) and logs the design section 9
// "legacy uncohorted scenarios rendered" debug line. Returns nil, nil, the
// same "nothing to show" shape scenariosSection's own empty-slice check
// renders as no table at all, when the ticket has no plan cohort yet.
func (c *console) loadScenarios(ctx context.Context, ticketID int64) ([]templates.ScenarioRow, error) {
	cohort, ok, err := c.store.CurrentCohort(ctx, ticketID)
	if err != nil {
		return nil, fmt.Errorf("console: current cohort for ticket %d: %w", ticketID, err)
	}
	if !ok {
		return nil, nil
	}

	var artifacts []store.Artifact
	if cohort.RunID != nil {
		artifacts, err = c.store.ScenariosForRun(ctx, ticketID, *cohort.RunID, false)
		if err != nil {
			return nil, fmt.Errorf("console: scenarios for ticket %d run %d: %w", ticketID, *cohort.RunID, err)
		}
	} else {
		artifacts, err = c.store.AllScenarios(ctx, ticketID)
		if err != nil {
			return nil, fmt.Errorf("console: all scenarios for ticket %d: %w", ticketID, err)
		}
		slog.Debug("legacy uncohorted scenarios rendered", "ticket_id", ticketID)
	}
	return buildScenarioRows(ticketID, artifacts)
}

// buildScenarioRows decodes every scenario artifact's stored
// response.Scenario payload into the gate context's view model, preserving
// artifacts' own order (the store's insertion order; see loadScenarios).
func buildScenarioRows(ticketID int64, artifacts []store.Artifact) ([]templates.ScenarioRow, error) {
	rows := make([]templates.ScenarioRow, 0, len(artifacts))
	for i := range artifacts {
		var sc response.Scenario
		if err := json.Unmarshal(artifacts[i].Payload, &sc); err != nil {
			return nil, fmt.Errorf("console: unmarshal scenario artifact %d for ticket %d: %w", i, ticketID, err)
		}
		rows = append(rows, templates.ScenarioRow{
			ID: sc.ID, Kind: string(sc.Kind), Given: sc.Given, When: sc.When, Then: sc.Then,
		})
	}
	return rows, nil
}

// planreviewFindingsPayload is the JSON shape a "planreview" artifact's
// Payload carries (internal/store/schemas/artifacts/planreview.json):
// mirrored here, rather than imported, from internal/job/planning.go's own
// identical, unexported planreviewArtifactPayload -- this package has no
// other reason to depend on internal/job, and the wire shape is the
// schema's, not that type's, to keep in sync.
type planreviewFindingsPayload struct {
	Findings []response.Finding `json:"findings"`
}

// loadFindings reads ticketID's above-floor plan-review findings for the
// gate's context region (design section 7, D8, Task 11): the "planreview"
// artifact stored at the current cohort's exact plan version
// (PlanReviewAt), filtered to findings whose severity ranks strictly above
// c.floor. job/planning.go's own floor split (planReviewOkCommit) keeps
// every finding at or below the floor in its own resume loop, so only the
// findings a human, not that loop, must decide belong here. Returns nil,
// nil -- scenariosSection's and findingsSection's own "no empty table" rule
// -- when the ticket has no cohort yet, or no planreview artifact stored at
// its version.
func (c *console) loadFindings(ctx context.Context, ticketID int64) ([]templates.FindingRow, error) {
	cohort, ok, err := c.store.CurrentCohort(ctx, ticketID)
	if err != nil {
		return nil, fmt.Errorf("console: current cohort for ticket %d: %w", ticketID, err)
	}
	if !ok {
		return nil, nil
	}

	artifact, ok, err := c.store.PlanReviewAt(ctx, ticketID, cohort.PlanVersion)
	if err != nil {
		return nil, fmt.Errorf("console: planreview at version %d for ticket %d: %w", cohort.PlanVersion, ticketID, err)
	}
	if !ok {
		return nil, nil
	}

	var payload planreviewFindingsPayload
	if err := json.Unmarshal(artifact.Payload, &payload); err != nil {
		return nil, fmt.Errorf("console: unmarshal planreview artifact for ticket %d: %w", ticketID, err)
	}

	rows := make([]templates.FindingRow, 0, len(payload.Findings))
	for _, f := range payload.Findings {
		if f.Severity.Rank() <= c.floor.Rank() {
			continue
		}
		rows = append(rows, templates.FindingRow{
			Lens: string(f.Lens), Severity: string(f.Severity), Location: f.Location, Text: f.Text, Fix: f.Fix,
		})
	}
	return rows, nil
}

// msgTypeState, msgTypeQuestion, msgTypeEscalation, and msgTypeAnswer name
// the message types the read-only Thread view renders specially (design
// section 6.6): a state message as a centered separator, a question message
// as a read-only group, an escalation message (whose Body a commit may
// leave empty, like state) decoded into one line, and an answer message
// (whose Body SaveDraft and SendBatch never set, the choice living in
// Payload instead) decoded into its chosen option or item decisions. Every
// other type (update, followup, resolved, reply) is a plain row using its
// own Body.
const (
	msgTypeState      = "state"
	msgTypeQuestion   = "question"
	msgTypeEscalation = "escalation"
	msgTypeAnswer     = "answer"
)

// msgTypeUpdate mirrors job.msgTypeUpdate (internal/job/planning.go), the
// same package-local-copy pattern as the msgType* block above: job's own
// constant is unexported, and there is no shared package to import it from,
// so this copy and job's must change together.
const msgTypeUpdate = "update"

// msgTypeReply mirrors store's own unexported msgTypeReply
// (internal/store/console_writes.go), the same package-local-copy pattern
// draftMessageState below uses for store's "draft" state literal:
// collectQuestionDrafts needs to recognize a draft reply row the same way
// SaveDraft's own insertReplyDraftTx writes one.
const msgTypeReply = "reply"

// updateMarker* mirror the literal prefixes internal/job/planning.go and
// internal/job/building.go write into type="update" message bodies --
// planreviewPendingMarker and planreviewDeliveredMarker's "planreview v<N>
// pending"/"...delivered", validationErrorsPendingPrefix and
// validationErrorsDeliveredPrefix's "validation errors pending/delivered
// run <id>[...]", invalidOutputCommit's "response invalid run
// <id>\n<reason>", store.CountSealMismatches' "seal mismatch cohort <id>",
// and building.go's own markerClaimsOkFmt, markerClaimErrorsPendingFmt,
// markerClaimErrorsDeliveredFmt, markerPerimeterResolvedFmt, the escalation
// resolution's "retry requested", and markerPerimeterQuestionDroppedFmt --
// and reviewing.go's own "review round <n> done/asked/failed/void", "review
// discussed <id>", and "review note <id>" -- so displayBody can recognize
// them and render an owner-facing sentence instead of the raw bookkeeping
// body (F012, design section 9.2). console cannot import job's own
// unexported literals -- there is no shared package for the two to depend
// on -- so this copy and its originals must change together.
const (
	updateMarkerPlanreviewPrefix               = "planreview v"
	updateMarkerPlanreviewPendingSuffix        = " pending"
	updateMarkerPlanreviewDeliveredSuffix      = " delivered"
	updateMarkerValidationPendingPrefix        = "validation errors pending run "
	updateMarkerValidationDeliveredPrefix      = "validation errors delivered run "
	updateMarkerResponseInvalidPrefix          = "response invalid run "
	updateMarkerSealMismatchPrefix             = "seal mismatch cohort "
	updateMarkerClaimsOkPrefix                 = "claims ok run "
	updateMarkerClaimErrorsPendingPrefix       = "claim errors pending run "
	updateMarkerClaimErrorsDeliveredPrefix     = "claim errors delivered run "
	updateMarkerPerimeterResolvedPrefix        = "perimeter resolved run "
	updateMarkerRetryRequested                 = "retry requested"
	updateMarkerPerimeterQuestionDroppedPrefix = "perimeter question dropped run "
	updateMarkerReviewRoundPrefix              = "review round "
	updateMarkerReviewDiscussedPrefix          = "review discussed "
	updateMarkerReviewNotePrefix               = "review note "
)

// updateMarker* mirror the literal prefixes design section 5.1's table
// names for judging.go, shipping.go, and respond.go (sections 7, 8, 9):
// judging.go already writes the four "judge round <n> ..." shapes
// (judgeRoundStartedLine, judgeRoundRetryLine, judgeRoundFailedLine,
// judgeRoundVerdictsLine, and the EVALUATE "passed" shape), "judge coverage
// failed/delivered run <rid>" (judgeCoverageFailedFmt,
// judgeCoverageDeliveredFmt), and "judge check <n> <id> exit <code>"
// (judgeCheckLine). shipping.go and respond.go do not write their marker
// shapes yet (M4 tasks 4-8), so this copy and those originals must change
// together once they do, the same constraint the block above already
// states for planning.go, building.go, and reviewing.go.
const (
	updateMarkerJudgeRoundPrefix               = "judge round "
	updateMarkerJudgeCoverageFailedPrefix      = "judge coverage failed run "
	updateMarkerJudgeCoverageDeliveredPrefix   = "judge coverage delivered run "
	updateMarkerJudgeCheckPrefix               = "judge check "
	updateMarkerPrOpenedPrefix                 = "pr opened "
	updateMarkerCIWaitingPrefix                = "ci waiting "
	updateMarkerReviewersReRequestedPrefix     = "reviewers re-requested "
	updateMarkerPrReadyPrefix                  = "pr ready "
	updateMarkerPrDraftPrefix                  = "pr draft "
	updateMarkerThreadsBlockingPrefix          = "threads blocking "
	updateMarkerMergeAskedPrefix               = "merge asked "
	updateMarkerMergeHeldPrefix                = "merge held "
	updateMarkerMergeWithdrawnPrefix           = "merge withdrawn "
	updateMarkerMergeRefusedPrefix             = "merge refused "
	updateMarkerPrMergedPrefix                 = "pr merged "
	updateMarkerRespondBatchPrefix             = "respond batch "
	updateMarkerRespondCoverageFailedPrefix    = "respond coverage failed run "
	updateMarkerRespondCoverageDeliveredPrefix = "respond coverage delivered run "
	updateMarkerRespondAppliedPrefix           = "respond applied "
	updateMarkerFixRepliesPostedPrefix         = "fix replies posted "
)

// draftMessageState mirrors store's own unexported draft-state literal
// (store.DraftInput's SaveDraft writes state="draft", console_writes.go);
// this package needs its own copy of that one literal to recognize an
// unsent draft row, the same way it already copies the message-type
// literals above rather than importing package store's unexported
// constants.
const draftMessageState = "draft"

// visibleRows drops every unsent draft row (design section 6.6, 6.7, code
// review fix 2): a draft answer or reply belongs to the composer queue, not
// the read-only Thread view, until POST /send flips its state to sent.
func visibleRows(rows []store.MessageRow) []store.MessageRow {
	out := make([]store.MessageRow, 0, len(rows))
	for i := range rows {
		if rows[i].State != nil && *rows[i].State == draftMessageState {
			continue
		}
		out = append(out, rows[i])
	}
	return out
}

// questionStateLabel maps messages.state to the pill label the mock's group
// summary shows (design section 6.6): "resolved" when resolved, "waiting on
// you" when open, "resuming" when answered. A missing or unrecognized state
// renders as-is (or empty), rather than guessing.
func questionStateLabel(state *string) string {
	if state == nil {
		return ""
	}
	switch *state {
	case "open":
		return "waiting on you"
	case "answered":
		return "resuming"
	case "resolved":
		return "resolved"
	default:
		return *state
	}
}

// buildThreadRows turns store rows into the Thread view's rows (design
// section 6.6): a question message becomes an interactive group dispatching
// on its payload's Kind (Task 6), every other type a plain row. ticket
// carries the merge kind's PR-link context, and plan, scenarios, and
// findings the gate kind's three context regions (buildThreadQuestion);
// ticket may be nil only when the caller has no ticket at all
// (templates.Thread's own nil guard), never when rows is non-empty. plan is
// nil, and scenarios and findings are both nil, when the ticket carries
// nothing yet for that region (views.go's loadPlan, loadScenarios,
// loadFindings). Drafts are collected from the unfiltered rows, before
// visibleRows drops them (bug fix: a draft answers or replies to a
// question, which this function still needs to find below, even though the
// draft row itself never becomes its own ThreadRow).
func buildThreadRows(ticket *store.Ticket, rows []store.MessageRow, plan *templates.RenderedPlan, scenarios []templates.ScenarioRow, findings []templates.FindingRow) ([]templates.ThreadRow, error) {
	drafts := collectQuestionDrafts(rows)
	rows = visibleRows(rows)
	sentAnswers := collectSentAnswers(rows)

	// messageCounts holds, per question message id, how many other messages
	// in this ticket name it as their parent (design section 6.6: the
	// <details> summary shows "the message count"). Precomputed once over
	// every row rather than per question, so counting stays O(n) instead of
	// O(n*questions).
	messageCounts := make(map[int64]int, len(rows))
	for i := range rows {
		if rows[i].ParentID != nil {
			messageCounts[*rows[i].ParentID]++
		}
	}

	out := make([]templates.ThreadRow, 0, len(rows))
	for i := range rows {
		question, err := buildThreadQuestion(ticket, &rows[i], messageCounts[rows[i].ID]+1, plan, scenarios, findings, drafts, sentAnswers)
		if err != nil {
			return nil, err
		}
		out = append(out, templates.ThreadRow{
			ID: rows[i].ID, Type: rows[i].Type, Author: rows[i].Author,
			Body:     displayBody(&rows[i]),
			Question: question,
		})
	}
	return out, nil
}

// collectSentAnswers scans rows (already visibleRows-filtered, so every
// "answer" row left is sent, never a draft) for each question's own sent
// answer, keyed by the question's message id (bug fix, questionGroup's
// locked note: "render an answered question's controls as disabled or
// locked with its answer shown"). A payload that fails to decode is
// skipped, the same defensive choice collectQuestionDrafts and
// buildThreadQuestion both make for a payload they cannot parse.
func collectSentAnswers(rows []store.MessageRow) map[int64]response.AnswerPayload {
	out := make(map[int64]response.AnswerPayload)
	for i := range rows {
		m := &rows[i]
		if m.Type != msgTypeAnswer || m.ParentID == nil {
			continue
		}
		var ap response.AnswerPayload
		if err := json.Unmarshal(m.Payload, &ap); err != nil {
			continue
		}
		out[*m.ParentID] = ap
	}
	return out
}

// sentAnswerText formats a question's own sent answer plainly (bug fix):
// the option's text when payload.Option names one of q's own options (its
// bare key as a fallback, for a payload that is well-formed but, through
// some future drift, no longer matches), or "ref: decision" pairs, ref
// order, for an item answer. Empty when payload carries neither, which
// questionGroup (thread.templ) treats as "nothing to show" rather than an
// empty locked note.
func sentAnswerText(payload response.AnswerPayload, options []templates.ThreadOption) string {
	if payload.Option != nil {
		for _, o := range options {
			if o.Key == *payload.Option {
				return o.Text
			}
		}
		return *payload.Option
	}
	if len(payload.Items) == 0 {
		return ""
	}
	refs := make([]string, 0, len(payload.Items))
	for ref := range payload.Items {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, ref+": "+string(payload.Items[ref]))
	}
	return strings.Join(parts, ", ")
}

// questionDraft is one question's in-progress, unsent draft (bug fix): at
// most one draft reply and at most one draft answer can exist per question
// at a time (SaveDraft's insertReplyDraftTx and upsertOptionDraftTx/
// upsertItemDraftTx both update a question's existing draft row in place
// rather than inserting a second one), so collectQuestionDrafts needs no
// slice, just these two optional values.
type questionDraft struct {
	Reply  string
	Answer response.AnswerPayload
}

// collectQuestionDrafts scans every row this ticket carries -- before
// visibleRows drops the draft ones -- for an unsent (state="draft") reply
// or answer that targets a question (ParentID != nil), keyed by that
// question's own message id (bug fix: freeReply, optionChips, and itemRows
// need this to render a saved-but-unsent draft back instead of leaving the
// thread looking like it swallowed it). A thread-level reply draft
// (ParentID == nil) is skipped: there is no composer surface for one
// (freeReply only ever renders inside a question's own group), so it has
// nowhere to render back to. An answer draft whose payload fails to decode
// is skipped rather than erroring the whole thread render, the same
// defensive choice buildThreadQuestion already makes for a question's own
// unparseable payload.
func collectQuestionDrafts(rows []store.MessageRow) map[int64]questionDraft {
	drafts := make(map[int64]questionDraft)
	for i := range rows {
		m := &rows[i]
		if m.State == nil || *m.State != draftMessageState || m.ParentID == nil {
			continue
		}
		qid := *m.ParentID
		switch m.Type {
		case msgTypeReply:
			d := drafts[qid]
			d.Reply = m.Body
			drafts[qid] = d
		case msgTypeAnswer:
			var ap response.AnswerPayload
			if err := json.Unmarshal(m.Payload, &ap); err != nil {
				continue
			}
			d := drafts[qid]
			d.Answer = ap
			drafts[qid] = d
		}
	}
	return drafts
}

// buildThreadQuestion returns the detail a "question" message renders
// instead of its plain Body, or nil for every other type. messageCount is
// the question's own message (1) plus every reply, answer, followup, or
// resolved row that names it as a parent (buildThreadRows). plan,
// scenarios, and findings are the gate kind's three context regions (design
// section 6.9, 7, D8), set on q only when payload.Kind is gate. drafts is
// collectQuestionDrafts' own map, keyed by this question's id, carrying its
// in-progress draft reply and/or answer, if any (bug fix). An unparseable
// payload falls back to nil (renders as a plain row) rather than failing
// the whole thread render, since the commit that wrote it already validated
// it against the messages/question schema; a markdown render failure, by
// contrast, is a real error (design section 6.10: Render can fail), and is
// returned rather than silently dropping the question's body.
func buildThreadQuestion(ticket *store.Ticket, m *store.MessageRow, messageCount int, plan *templates.RenderedPlan, scenarios []templates.ScenarioRow, findings []templates.FindingRow, drafts map[int64]questionDraft, sentAnswers map[int64]response.AnswerPayload) (*templates.ThreadQuestion, error) {
	if m.Type != msgTypeQuestion {
		return nil, nil //nolint:nilnil // "no question" is a legitimate result, not an error
	}
	var payload response.QuestionPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil, nil //nolint:nilnil,nilerr // an unparseable payload renders as a plain row, not an error
	}

	title, body := splitQuestionBody(m.Body)
	bodyHTML, err := Render(body)
	if err != nil {
		return nil, fmt.Errorf("console: render question %d body: %w", m.ID, err)
	}

	var recommendedHTML templ.Component
	if payload.Recommended != "" {
		recommendedHTML, err = Render(payload.Recommended)
		if err != nil {
			return nil, fmt.Errorf("console: render question %d recommendation: %w", m.ID, err)
		}
	}

	options := make([]templates.ThreadOption, 0, len(payload.Options))
	for _, o := range payload.Options {
		options = append(options, templates.ThreadOption{Key: o.Key, Text: o.Text})
	}
	items := make([]templates.ThreadItem, 0, len(payload.Items))
	for _, it := range payload.Items {
		items = append(items, templates.ThreadItem{Ref: it.Ref, Text: it.Text})
	}

	q := &templates.ThreadQuestion{
		Key: payload.Key, Title: title, Kind: string(payload.Kind),
		BodyHTML: bodyHTML, Recommended: payload.Recommended, RecommendedHTML: recommendedHTML,
		Options: options, Items: items, StateLabel: questionStateLabel(m.State),
		MessageCount: messageCount,
		// Interactive is true only for a still-open question (code review
		// fix, PR #16): questionGroup (thread.templ) used to render option
		// chips, item rows, and the free reply input for every question
		// regardless of state, so an answered or resolved question -- whose
		// draft SaveDraft would refuse anyway (openQuestionForTicketTx) --
		// still looked editable.
		Interactive: m.State != nil && *m.State == msgStateOpen,
	}
	// AnsweredText (bug fix): a closed question's locked note, shown instead
	// of its now-hidden controls, only for state=answered -- not resolved, a
	// later terminal state a reader has already moved past.
	if m.State != nil && *m.State == msgStateAnswered {
		if ap, ok := sentAnswers[m.ID]; ok {
			q.AnsweredText = sentAnswerText(ap, options)
		}
	}
	if payload.Kind == response.QuestionKindMerge && ticket != nil && ticket.PRURL != nil {
		q.PRURL = *ticket.PRURL
	}
	if payload.Kind == response.QuestionKindGate {
		q.Plan = plan
		q.Scenarios = scenarios
		q.Findings = findings
	}
	if draft, ok := drafts[m.ID]; ok {
		q.DraftReply = draft.Reply
		if draft.Answer.Option != nil {
			q.DraftOption = *draft.Answer.Option
		}
		q.DraftItems = draft.Answer.Items
	}
	return q, nil
}

// splitQuestionBody splits a question message's Body into its heading (the
// first line) and the text after it, matching how a planning commit writes
// Title and Body together: title first as the heading, then a blank line,
// then the body (design section 6.6).
func splitQuestionBody(raw string) (title, body string) {
	title, rest, _ := strings.Cut(raw, "\n")
	return title, strings.TrimPrefix(rest, "\n")
}

// displayBody returns what the Thread view renders for one message: a
// decoded system line for "state" and "escalation" (whose Body a commit
// leaves empty, the transition or the report living in Payload instead), a
// decoded choice for "answer" (whose Body SaveDraft and SendBatch never
// set, code review fix 2), or the row's own Body for every other type. A
// row reaching here is always sent, never a draft: buildThreadRows already
// filters state=draft rows out via visibleRows before this runs.
func displayBody(m *store.MessageRow) string {
	switch m.Type {
	case msgTypeState:
		return stateLine(m)
	case msgTypeEscalation:
		return escalationLine(m)
	case msgTypeAnswer:
		return answerLine(m)
	case msgTypeUpdate:
		return updateLine(m)
	default:
		return m.Body
	}
}

// updateLine recognizes the known planning-bookkeeping markers a type=
// "update" message's Body carries (F012: the owner should never read
// "planreview v3 pending" or a raw response.PathError line) and returns an
// owner-facing sentence instead. A body that matches none of the known
// updateMarker* prefixes is not a marker this view knows about, so it falls
// through unchanged, the same defensive fallback stateLine, escalationLine,
// and answerLine already use for a payload they cannot decode.
func updateLine(m *store.MessageRow) string {
	body := m.Body
	switch {
	case strings.HasPrefix(body, updateMarkerPlanreviewPrefix) && strings.HasSuffix(body, updateMarkerPlanreviewPendingSuffix):
		return "Plan review found only minor findings. Planning resumes automatically to address them."
	case strings.HasPrefix(body, updateMarkerPlanreviewPrefix) && strings.HasSuffix(body, updateMarkerPlanreviewDeliveredSuffix):
		return "Planning resumed with the review findings."
	case strings.HasPrefix(body, updateMarkerValidationPendingPrefix):
		return validationErrorsLine(body)
	case strings.HasPrefix(body, updateMarkerValidationDeliveredPrefix):
		return "The agent received the check results."
	case strings.HasPrefix(body, updateMarkerResponseInvalidPrefix):
		return "The agent's last response could not be used. Zing retries once."
	case strings.HasPrefix(body, updateMarkerSealMismatchPrefix):
		return "The scenario set changed before approval. Zing re-reads it on the next tick."
	case strings.HasPrefix(body, updateMarkerClaimsOkPrefix):
		return "Claims checked for run " + strings.TrimPrefix(body, updateMarkerClaimsOkPrefix) + "."
	case strings.HasPrefix(body, updateMarkerClaimErrorsPendingPrefix):
		return claimErrorsPendingLine(body)
	case strings.HasPrefix(body, updateMarkerClaimErrorsDeliveredPrefix):
		return "Claim errors sent back to run " + strings.TrimPrefix(body, updateMarkerClaimErrorsDeliveredPrefix) + "."
	case strings.HasPrefix(body, updateMarkerPerimeterResolvedPrefix):
		return "Perimeter decided for run " + strings.TrimPrefix(body, updateMarkerPerimeterResolvedPrefix) + "."
	case body == updateMarkerRetryRequested:
		return "Retry requested."
	case strings.HasPrefix(body, updateMarkerPerimeterQuestionDroppedPrefix):
		return "Perimeter question dropped for run " + strings.TrimPrefix(body, updateMarkerPerimeterQuestionDroppedPrefix) + "."
	case isReviewMarker(body):
		if line, ok := reviewUpdateLine(body); ok {
			return line
		}
		return body
	case isJudgeMarker(body):
		if line, ok := judgeUpdateLine(body); ok {
			return line
		}
		return body
	case isShippingMarker(body):
		if line, ok := shippingUpdateLine(body); ok {
			return line
		}
		return body
	case isRespondMarker(body):
		if line, ok := respondUpdateLine(body); ok {
			return line
		}
		return body
	default:
		return body
	}
}

// isReviewMarker reports whether body carries one of reviewing.go's own
// "review round ", "review discussed ", or "review note " prefixes (design
// section 5.1, 6.2-6.6): the gate reviewUpdateLine's own exact-shape parse
// runs behind, the same two-step prefix-then-parse pattern
// claimErrorsPendingLine and validationErrorsLine already use.
func isReviewMarker(body string) bool {
	return strings.HasPrefix(body, updateMarkerReviewRoundPrefix) ||
		strings.HasPrefix(body, updateMarkerReviewDiscussedPrefix) ||
		strings.HasPrefix(body, updateMarkerReviewNotePrefix)
}

// reviewUpdateLine renders one of reviewing.go's own six review marker
// shapes (design section 5.1) as an owner-facing sentence: the four "review
// round <n> ..." round markers (done, asked, failed, void), "review
// discussed <id>" (6.6 step 4), and "review note <id>" (6.5 step 4, D24).
// ok is false when body's prefix matched but the rest of its shape did not
// (a future marker shape this view does not know yet), the same defensive
// fallback updateLine's other cases use.
func reviewUpdateLine(body string) (string, bool) {
	first, rest, hasRest := strings.Cut(body, "\n")
	switch {
	case strings.HasPrefix(first, updateMarkerReviewDiscussedPrefix):
		id := strings.TrimPrefix(first, updateMarkerReviewDiscussedPrefix)
		return "Finding " + id + " discussed with the lens.", true
	case strings.HasPrefix(first, updateMarkerReviewNotePrefix):
		id := strings.TrimPrefix(first, updateMarkerReviewNotePrefix)
		header := "Owner's note on " + id + ":"
		if hasRest {
			return header + "\n" + rest, true
		}
		return header, true
	case strings.HasPrefix(first, updateMarkerReviewRoundPrefix):
		return reviewRoundLine(first, rest, hasRest)
	default:
		return "", false
	}
}

// reviewRoundLine renders one "review round <n> done/asked/failed/void"
// marker's first line (design section 5.1, 6.2, 6.2a): n is read
// generically, rather than hardcoded, since a ticket can run any number of
// rounds. "done"'s own second line (the "kept N dropped N merged N" summary)
// is kept under the header sentence, the same claimErrorsPendingLine
// pattern.
func reviewRoundLine(first, rest string, hasRest bool) (string, bool) {
	tail := strings.TrimPrefix(first, updateMarkerReviewRoundPrefix)
	n, after, ok := strings.Cut(tail, " ")
	if !ok {
		return "", false
	}
	switch {
	case strings.HasPrefix(after, "done sha "):
		header := "Review round " + n + " finished."
		if hasRest {
			return header + "\n" + rest, true
		}
		return header, true
	case after == "asked":
		return "Review round " + n + " is waiting on a lens question.", true
	case after == "failed":
		return "Review round " + n + " failed. Zing retries the round.", true
	case after == "void":
		return "Review round " + n + " restarted: the branch moved during the round.", true
	default:
		return "", false
	}
}

// sha7 returns the first seven characters of a 40-character lowercase hex
// sha, the short form design section 5.1's console sentences show (for
// example "Judge round <n> started on <sha7>."). A shorter input is
// returned unchanged rather than panicking on a slice out of range; only a
// malformed marker or a test fixture would be shorter than seven
// characters, since every real sha this view reads is 40 hex characters
// (section 5.1).
func sha7(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// isJudgeMarker reports whether body carries one of judging.go's own
// "judge round ", "judge coverage failed/delivered run ", or "judge check "
// prefixes (design section 5.1, 7.1, 7.2, 7.5-7.7): judgeUpdateLine's own
// exact-shape parse runs behind, the same two-step prefix-then-parse
// pattern isReviewMarker and reviewUpdateLine already use.
func isJudgeMarker(body string) bool {
	return strings.HasPrefix(body, updateMarkerJudgeRoundPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeCoverageFailedPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeCoverageDeliveredPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeCheckPrefix)
}

// judgeUpdateLine renders one of judging.go's own marker shapes (design
// section 5.1) as an owner-facing sentence: the five "judge round <n> ..."
// shapes (started, verdicts, passed, failed, retry; judgeRoundLine),
// "judge coverage failed/delivered run <rid>" (7.2 step 5), and "judge
// check <n> <scenario_id> exit <code>" (7.5 step 4; judgeCheckLine). ok is
// false when body's prefix matched but the rest of its shape did not, the
// same defensive fallback reviewUpdateLine's own default case uses.
func judgeUpdateLine(body string) (string, bool) {
	first, rest, hasRest := strings.Cut(body, "\n")
	switch {
	case strings.HasPrefix(first, updateMarkerJudgeCoverageFailedPrefix):
		rid := strings.TrimPrefix(first, updateMarkerJudgeCoverageFailedPrefix)
		header := "Judge verdicts incomplete for run " + rid + ":"
		if hasRest {
			return header + "\n" + rest, true
		}
		return header, true
	case strings.HasPrefix(first, updateMarkerJudgeCoverageDeliveredPrefix):
		rid := strings.TrimPrefix(first, updateMarkerJudgeCoverageDeliveredPrefix)
		return "Coverage errors sent back to run " + rid + ".", true
	case strings.HasPrefix(first, updateMarkerJudgeCheckPrefix):
		return judgeCheckLine(first)
	case strings.HasPrefix(first, updateMarkerJudgeRoundPrefix):
		return judgeRoundLine(first, rest, hasRest)
	default:
		return "", false
	}
}

// judgeCheckLine renders "judge check <n> <scenario_id> exit <code>" (7.5
// step 4) as its own sentence: the round number n plays no part in it, the
// same elision judgeRoundLine's "verdicts" case uses for the run id.
func judgeCheckLine(first string) (string, bool) {
	fields := strings.Fields(strings.TrimPrefix(first, updateMarkerJudgeCheckPrefix))
	if len(fields) != 4 || fields[2] != "exit" {
		return "", false
	}
	scenarioID, code := fields[1], fields[3]
	return "Check for " + scenarioID + " exited " + code + ".", true
}

// judgeRoundLine renders one "judge round <n> started/verdicts/passed/
// failed/retry" marker's first line (design section 5.1, 7.2, 7.6): n is
// read generically, rather than hardcoded, the same reviewRoundLine
// pattern.
func judgeRoundLine(first, rest string, hasRest bool) (string, bool) {
	tail := strings.TrimPrefix(first, updateMarkerJudgeRoundPrefix)
	n, after, ok := strings.Cut(tail, " ")
	if !ok {
		return "", false
	}
	switch {
	case strings.HasPrefix(after, "started sha "):
		sha, _, _ := strings.Cut(strings.TrimPrefix(after, "started sha "), " ")
		return "Judge round " + n + " started on " + sha7(sha) + ".", true
	case strings.HasPrefix(after, "verdicts run "):
		return "Judge round " + n + " returned its verdicts.", true
	case after == "passed":
		return "Judge round " + n + " passed.", true
	case after == "failed":
		header := "Judge round " + n + " failed:"
		if hasRest {
			return header + " " + rest + ".", true
		}
		return header + ".", true
	case strings.HasPrefix(after, "retry after run "):
		return "Judge round " + n + " restarted.", true
	default:
		return "", false
	}
}

// isShippingMarker reports whether body carries one of shipping.go's own
// "pr opened/ready/draft/merged ", "ci waiting ", "reviewers re-requested
// ", "threads blocking ", or "merge asked/held/withdrawn/refused " prefixes
// (design section 5.1, 8.2-8.9): shippingUpdateLine's own exact-shape parse
// runs behind, the same two-step pattern isReviewMarker uses.
func isShippingMarker(body string) bool {
	return strings.HasPrefix(body, updateMarkerPrOpenedPrefix) ||
		strings.HasPrefix(body, updateMarkerCIWaitingPrefix) ||
		strings.HasPrefix(body, updateMarkerReviewersReRequestedPrefix) ||
		strings.HasPrefix(body, updateMarkerPrReadyPrefix) ||
		strings.HasPrefix(body, updateMarkerPrDraftPrefix) ||
		strings.HasPrefix(body, updateMarkerThreadsBlockingPrefix) ||
		strings.HasPrefix(body, updateMarkerMergeAskedPrefix) ||
		strings.HasPrefix(body, updateMarkerMergeHeldPrefix) ||
		strings.HasPrefix(body, updateMarkerMergeWithdrawnPrefix) ||
		strings.HasPrefix(body, updateMarkerMergeRefusedPrefix) ||
		strings.HasPrefix(body, updateMarkerPrMergedPrefix)
}

// shippingUpdateLine renders one of shipping.go's own marker shapes (design
// section 5.1) as an owner-facing sentence: "pr opened <number>" (8.2 step
// 6), "ci waiting <names>" (8.4), "reviewers re-requested <sha>" with its
// own logins line (9.4), "pr ready/draft <sha>" (8.5 rows 3 and 8, 8.9),
// "threads blocking <tids>" (8.5 row 6a), "merge asked/held/withdrawn
// <sha>" and "merge refused <sha>" with its own reason line (8.8), and "pr
// merged <sha>" (8.8). ok is false when body's prefix matched but the rest
// of its shape did not, the same defensive fallback reviewUpdateLine's own
// default case uses.
func shippingUpdateLine(body string) (string, bool) {
	first, rest, hasRest := strings.Cut(body, "\n")
	switch {
	case strings.HasPrefix(first, updateMarkerPrOpenedPrefix):
		number := strings.TrimPrefix(first, updateMarkerPrOpenedPrefix)
		return "Draft pull request #" + number + " opened.", true
	case strings.HasPrefix(first, updateMarkerCIWaitingPrefix):
		names := strings.TrimPrefix(first, updateMarkerCIWaitingPrefix)
		return "CI is waiting for " + names + ".", true
	case strings.HasPrefix(first, updateMarkerReviewersReRequestedPrefix):
		logins := ""
		if hasRest {
			logins = rest
		}
		return "Review re-requested from " + logins + ".", true
	case strings.HasPrefix(first, updateMarkerPrReadyPrefix):
		sha := strings.TrimPrefix(first, updateMarkerPrReadyPrefix)
		return "Pull request marked ready at " + sha7(sha) + ".", true
	case strings.HasPrefix(first, updateMarkerPrDraftPrefix):
		sha := strings.TrimPrefix(first, updateMarkerPrDraftPrefix)
		return "Pull request moved back to draft at " + sha7(sha) + ".", true
	case strings.HasPrefix(first, updateMarkerThreadsBlockingPrefix):
		tids := strings.TrimPrefix(first, updateMarkerThreadsBlockingPrefix)
		return "Review threads Zing cannot read are blocking the merge: " + tids + ".", true
	case strings.HasPrefix(first, updateMarkerMergeAskedPrefix):
		sha := strings.TrimPrefix(first, updateMarkerMergeAskedPrefix)
		return "Asked whether to merge " + sha7(sha) + ".", true
	case strings.HasPrefix(first, updateMarkerMergeHeldPrefix):
		sha := strings.TrimPrefix(first, updateMarkerMergeHeldPrefix)
		return "Merge held at " + sha7(sha) + ".", true
	case strings.HasPrefix(first, updateMarkerMergeWithdrawnPrefix):
		return "The merge question was withdrawn; the loop reopened.", true
	case strings.HasPrefix(first, updateMarkerMergeRefusedPrefix):
		header := "Merge refused:"
		if hasRest {
			return header + " " + rest, true
		}
		return header, true
	case strings.HasPrefix(first, updateMarkerPrMergedPrefix):
		sha := strings.TrimPrefix(first, updateMarkerPrMergedPrefix)
		return "Pull request merged at " + sha7(sha) + ".", true
	default:
		return "", false
	}
}

// isRespondMarker reports whether body carries one of respond.go's own
// "respond batch ", "respond coverage failed/delivered run ", "respond
// applied ", or "fix replies posted " prefixes (design section 5.1,
// 9.2-9.4, 5.6): respondUpdateLine's own exact-shape parse runs behind, the
// same two-step pattern isReviewMarker uses.
func isRespondMarker(body string) bool {
	return strings.HasPrefix(body, updateMarkerRespondBatchPrefix) ||
		strings.HasPrefix(body, updateMarkerRespondCoverageFailedPrefix) ||
		strings.HasPrefix(body, updateMarkerRespondCoverageDeliveredPrefix) ||
		strings.HasPrefix(body, updateMarkerRespondAppliedPrefix) ||
		strings.HasPrefix(body, updateMarkerFixRepliesPostedPrefix)
}

// respondUpdateLine renders one of respond.go's own marker shapes (design
// section 5.1) as an owner-facing sentence: the four "respond batch <n>
// ..." shapes (started, stale, skipped, retry; respondBatchLine), "respond
// coverage failed/delivered run <rid>" (9.2), "respond applied <aid>" with
// its own replied/fixing/skipped counts line (9.3 step 4;
// respondAppliedLine), and "fix replies posted <aid>" (9.4). ok is false
// when body's prefix matched but the rest of its shape did not, the same
// defensive fallback reviewUpdateLine's own default case uses.
func respondUpdateLine(body string) (string, bool) {
	first, rest, hasRest := strings.Cut(body, "\n")
	switch {
	case strings.HasPrefix(first, updateMarkerRespondCoverageFailedPrefix):
		rid := strings.TrimPrefix(first, updateMarkerRespondCoverageFailedPrefix)
		header := "Thread actions incomplete for run " + rid + ":"
		if hasRest {
			return header + "\n" + rest, true
		}
		return header, true
	case strings.HasPrefix(first, updateMarkerRespondCoverageDeliveredPrefix):
		rid := strings.TrimPrefix(first, updateMarkerRespondCoverageDeliveredPrefix)
		return "Thread errors sent back to run " + rid + ".", true
	case strings.HasPrefix(first, updateMarkerRespondAppliedPrefix):
		return respondAppliedLine(rest, hasRest)
	case strings.HasPrefix(first, updateMarkerFixRepliesPostedPrefix):
		return "Replied to the fixed threads.", true
	case strings.HasPrefix(first, updateMarkerRespondBatchPrefix):
		return respondBatchLine(first, rest, hasRest)
	default:
		return "", false
	}
}

// respondAppliedLine renders "respond applied <aid>" (design section 5.1,
// 9.3 step 4) from its own line 2, "replied <r> fixing <f> skipped <s>",
// the source of the counts the sentence reports; an optional line 3 ("fix
// request after run <R>") plays no part in it. The batch id plays no part
// in it either, the same elision judgeRoundLine's "verdicts" case uses for
// the run id.
func respondAppliedLine(rest string, hasRest bool) (string, bool) {
	if !hasRest {
		return "", false
	}
	line2, _, _ := strings.Cut(rest, "\n")
	fields := strings.Fields(line2)
	if len(fields) != 6 || fields[0] != "replied" || fields[2] != "fixing" || fields[4] != "skipped" {
		return "", false
	}
	return "Replied to " + fields[1] + " threads; " + fields[3] + " go to a fix run.", true
}

// respondBatchLine renders one "respond batch <n> started/stale/skipped/
// retry" marker's first line (design section 5.1, 8.5 row 5, 9.2, 9.3,
// 5.6): n plays no part in any of the four sentences, the same elision
// judgeRoundLine's "verdicts" and "retry" cases use.
func respondBatchLine(first, rest string, hasRest bool) (string, bool) {
	tail := strings.TrimPrefix(first, updateMarkerRespondBatchPrefix)
	_, after, ok := strings.Cut(tail, " ")
	if !ok {
		return "", false
	}
	switch {
	case strings.HasPrefix(after, "started sha "):
		return respondBatchStartedLine(rest, hasRest)
	case after == "stale":
		return "Review threads changed; Zing will read them again.", true
	case after == "skipped":
		return "Review threads were resolved before Zing answered.", true
	case strings.HasPrefix(after, "retry sha "):
		return "Answering the review threads again.", true
	default:
		return "", false
	}
}

// respondBatchStartedLine counts the tids on a "respond batch <n> started
// sha <sha> after run <R>" marker's own line 2 (design section 5.1, 8.5 row
// 5): an empty line 2 is zero threads, not one, so the count comes from
// strings.Split only when the line is non-empty.
func respondBatchStartedLine(rest string, hasRest bool) (string, bool) {
	if !hasRest {
		return "", false
	}
	tidLine, _, _ := strings.Cut(rest, "\n")
	tidLine = strings.TrimSpace(tidLine)
	count := 0
	if tidLine != "" {
		count = len(strings.Split(tidLine, ","))
	}
	return fmt.Sprintf("Answering %d review threads.", count), true
}

// claimErrorsPendingLine renders a "claim errors pending run <rid>" body's
// first line as one owner-facing sentence, then the marker's own error
// lines unchanged (design section 9.2): unlike validationErrorsLine's own
// "Field <path>: <message>" rewrite, a claim error's own path (for example
// "claims/test_exit: observed 1, want 0") is already the owner-facing
// shape CheckBuildClaims and CheckCommandsPassed produce.
func claimErrorsPendingLine(body string) string {
	first, rest, hasRest := strings.Cut(body, "\n")
	rid := strings.TrimPrefix(first, updateMarkerClaimErrorsPendingPrefix)
	header := "Claim check failed for run " + rid + ":"
	if !hasRest {
		return header
	}
	return header + "\n" + rest
}

// validationErrorsLine renders a "validation errors pending run <id>"
// body's first line as one owner-facing sentence, then one line per
// response.PathError the run reported (formatReadyErrors,
// internal/job/planning.go: each already its own Error() shape, "path:
// msg"), rewritten as "Field <path>: <message>" by cutting on the first
// ": " -- a PathError's message never contains that substring, since it is
// generated from a small fixed set of English reasons (checkReady,
// checkScenarioShape), not user- or model-supplied text.
func validationErrorsLine(body string) string {
	_, rest, hasErrors := strings.Cut(body, "\n")
	lines := []string{"The plan did not pass its final checks. Zing is asking the agent to revise it."}
	if !hasErrors {
		return lines[0]
	}
	for line := range strings.SplitSeq(rest, "\n") {
		if line == "" {
			continue
		}
		path, msg, ok := strings.Cut(line, ": ")
		if !ok {
			lines = append(lines, line)
			continue
		}
		lines = append(lines, "Field "+path+": "+msg)
	}
	return strings.Join(lines, "\n")
}

// answerLine decodes a sent "answer" message's payload into the text its
// Body never carries (design section 6.6, 6.7, code review fix 2): the
// chosen option's key, or its item ref-to-decision picks joined into one
// line, ref order sorted so the rendered line is deterministic regardless
// of map iteration order. An unparseable or empty payload falls back to the
// (empty) Body rather than erroring, matching stateLine's and
// escalationLine's own defensive fallback.
func answerLine(m *store.MessageRow) string {
	if len(m.Payload) == 0 {
		return m.Body
	}
	var ap response.AnswerPayload
	if err := json.Unmarshal(m.Payload, &ap); err != nil {
		return m.Body
	}
	if ap.Option != nil {
		return "Option: " + *ap.Option
	}
	if len(ap.Items) == 0 {
		return m.Body
	}
	refs := make([]string, 0, len(ap.Items))
	for ref := range ap.Items {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, ref+": "+string(ap.Items[ref]))
	}
	return strings.Join(parts, ", ")
}

// stateLine decodes a "state" message's payload into its "from -> to
// (reason)" line (design section 6.6, ported from Package 3's render.go).
func stateLine(m *store.MessageRow) string {
	if len(m.Payload) == 0 {
		return m.Body
	}
	var sp response.StatePayload
	if err := json.Unmarshal(m.Payload, &sp); err != nil {
		return m.Body
	}
	line := string(sp.From) + " -> " + string(sp.To)
	if sp.Reason != "" {
		line += " (" + sp.Reason + ")"
	}
	return line
}

// escalationLine decodes an "escalation" message's payload into one summary
// line when its Body is empty; a producer that does write a Body wins over
// the decode.
func escalationLine(m *store.MessageRow) string {
	if m.Body != "" {
		return m.Body
	}
	if len(m.Payload) == 0 {
		return m.Body
	}
	var ep response.EscalationPayload
	if err := json.Unmarshal(m.Payload, &ep); err != nil {
		return m.Body
	}
	line := ep.Code + ": " + ep.What
	if ep.Why != "" {
		line += " - " + ep.Why
	}
	return line
}
