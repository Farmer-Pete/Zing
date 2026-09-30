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
func (c *console) navComponent(ctx context.Context) (templ.Component, error) {
	projects, err := c.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	items, err := c.store.InboxItems(ctx)
	if err != nil {
		return nil, err
	}
	return templates.Nav(projects, buildNavThreads(items), c.sandboxReason), nil
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
		return templates.Project(tickets), nil
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
// so displayBody can recognize them and render an owner-facing sentence
// instead of the raw bookkeeping body (F012, design section 9.2). console
// cannot import job's own unexported literals -- there is no shared
// package for the two to depend on -- so this copy and its originals must
// change together.
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
// loadFindings).
func buildThreadRows(ticket *store.Ticket, rows []store.MessageRow, plan *templates.RenderedPlan, scenarios []templates.ScenarioRow, findings []templates.FindingRow) ([]templates.ThreadRow, error) {
	rows = visibleRows(rows)

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
		question, err := buildThreadQuestion(ticket, &rows[i], messageCounts[rows[i].ID]+1, plan, scenarios, findings)
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

// buildThreadQuestion returns the detail a "question" message renders
// instead of its plain Body, or nil for every other type. messageCount is
// the question's own message (1) plus every reply, answer, followup, or
// resolved row that names it as a parent (buildThreadRows). plan,
// scenarios, and findings are the gate kind's three context regions (design
// section 6.9, 7, D8), set on q only when payload.Kind is gate. An
// unparseable payload falls back to nil (renders as a plain row) rather
// than failing the whole thread render, since the commit that wrote it
// already validated it against the messages/question schema; a markdown
// render failure, by contrast, is a real error (design section 6.10: Render
// can fail), and is returned rather than silently dropping the question's
// body.
func buildThreadQuestion(ticket *store.Ticket, m *store.MessageRow, messageCount int, plan *templates.RenderedPlan, scenarios []templates.ScenarioRow, findings []templates.FindingRow) (*templates.ThreadQuestion, error) {
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
	if payload.Kind == response.QuestionKindMerge && ticket != nil && ticket.PRURL != nil {
		q.PRURL = *ticket.PRURL
	}
	if payload.Kind == response.QuestionKindGate {
		q.Plan = plan
		q.Scenarios = scenarios
		q.Findings = findings
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
	default:
		return body
	}
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
