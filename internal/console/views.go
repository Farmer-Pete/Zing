// views.go builds the five views' per-view models from the store reads
// Task 2 added and renders them through the templ components in
// internal/console/templates (design section 6.5, Task 3). #nav's model
// (design section 6.3, 6.8, #106 bug 4) lives here too: it is not one of
// the five views, but it is built the same way, from store.LiveTickets.
package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"zing/internal/console/templates"
	"zing/internal/machine"
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
	return templates.Alerts(buildAlertLines(c.log.Warnings(alertsLimit)), assetVersion)
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

// navComponent builds the #nav region: every project, and every live
// ticket (design section 6.3, 6.8, #106 bug 4), built from
// store.LiveTickets so the sidebar shows a ticket the moment it stops
// being terminal, not only once it is blocking or unread. open is the
// thread view's own open ticket, 0 when none is open (bug fix: nav.templ's
// threadLink renders "selected" on this one row, so the sidebar's
// highlight is part of #nav's own HTML and survives every patch).
func (c *console) navComponent(ctx context.Context, open int64) (templ.Component, error) {
	projects, err := c.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	items, err := c.store.LiveTickets(ctx, c.terminalStates())
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].Ticket.ID
	}
	slog.DebugContext(ctx, "console: nav live tickets", "count", len(items), "ticket_ids", ids)
	now := time.Now()
	until, held, err := c.store.ClaudeHold(ctx)
	if err != nil {
		return nil, err
	}
	claudeHold := ""
	if held && until.After(now) {
		claudeHold = clockLabel(until)
	}
	return templates.Nav(projects, buildNavThreads(items, now), c.sandboxReason, claudeHold, open), nil
}

// terminalStates is the machine's terminal state list for InboxItems
// (design section 6.8's goal "InboxItems leaves out unread-only tickets
// whose state is in machine.States.Terminal"), nil when no machine is
// loaded (most console tests), which leaves the unread branch unfiltered,
// the same guard rail.go's buildPhaseRail already uses for a nil machine.
func (c *console) terminalStates() []string {
	if c.machine == nil {
		return nil
	}
	return c.machine.States.Terminal
}

// stateOrder is the machine's state order, for projectSections' live-ticket
// sort, nil when no machine is loaded, which leaves every ticket live and
// in issue-number order (the same nil-machine guard as terminalStates).
func (c *console) stateOrder() []string {
	if c.machine == nil {
		return nil
	}
	return c.machine.States.Order
}

// buildNavThreads turns LiveTickets into #nav's rows, preserving their
// issue-number order (design section 6.3, 6.8, #106 bug 5). now is
// compared against each item's ParkedUntil (#45): a past value is that
// ticket's own parked history, not a reason to show the badge again.
func buildNavThreads(items []store.LiveTicket, now time.Time) []templates.NavThread {
	out := make([]templates.NavThread, 0, len(items))
	for i := range items {
		th := templates.NavThread{
			Ticket:            items[i].Ticket,
			Unread:            items[i].Unread,
			OpenQuestionCount: items[i].OpenQuestionCount,
		}
		if items[i].Ticket.WaitingOn != nil {
			th.Blocking = true
			th.WaitingOn = *items[i].Ticket.WaitingOn
		}
		if items[i].ParkedUntil != nil && items[i].ParkedUntil.After(now) {
			th.ParkedUntil = clockLabel(*items[i].ParkedUntil)
		}
		out = append(out, th)
	}
	return out
}

// clockLabel formats t in time.Local as "3:04pm" (#45): the sidebar's own
// parked-badge and claude-hold clock, matching store.parkedMarkerBody's own
// clock format in the thread.
func clockLabel(t time.Time) string {
	return t.In(time.Local).Format("3:04pm")
}

// projectSections splits one project's tickets, already in issue-number
// order (store.TicketsByProject), into live and closed (design section
// 6.5, Task 3, #106 bug 4): closed is every ticket whose state is in
// terminal, in issue-number order; live is the rest, furthest-along state
// first by its index in order (a state missing from order sorts last,
// rank -1), issue-number order within a state (the sort is stable, so ties
// keep the input's own order).
func projectSections(tickets []store.Ticket, order, terminal []string) (live, closed []store.Ticket) {
	for i := range tickets {
		if slices.Contains(terminal, tickets[i].State) {
			closed = append(closed, tickets[i])
		} else {
			live = append(live, tickets[i])
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		return slices.Index(order, live[i].State) > slices.Index(order, live[j].State)
	})
	return live, closed
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
		rows, err := displayFeedMessages(messages)
		if err != nil {
			return nil, err
		}
		return templates.Feed(rows), nil
	case viewProject:
		if project == 0 {
			projects, err := c.store.ListProjects(ctx)
			if err != nil {
				return nil, err
			}
			if len(projects) > 0 {
				project = projects[0].ID
			}
			slog.DebugContext(ctx, "console: project view defaulted", "project_id", project)
		}
		tickets, err := c.store.TicketsByProject(ctx, project)
		if err != nil {
			return nil, err
		}
		live, closed := projectSections(tickets, c.stateOrder(), c.terminalStates())
		slog.DebugContext(ctx, "console: project sections", "project_id", project, "live", len(live), "closed", len(closed))
		return templates.Project(project, live, closed), nil
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
	items, err := c.store.InboxItems(ctx, c.terminalStates())
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

// displayFeedMessages builds the Feed view's own rows, each with its Body
// replaced by displayBody's decoding (code review fix, PR #16): the Feed
// view passed store.FeedMessages' raw rows straight to templates.Feed,
// which rendered m.Body verbatim, so a state row (whose Body a commit
// leaves empty, the transition living in Payload instead) and a sent
// answer row (whose Body SaveDraft and SendBatch never set, the choice
// living in Payload instead) both rendered blank -- only the Thread view,
// through buildThreadRows, ever ran a row's Body through displayBody.
// BodyHTML is markdown-rendered (bug fix: raw backticks showed literally in
// the Feed, the owner's locked-view complaint, design section 22.7), the
// same Render path the Thread view's own turns and bodies use.
func displayFeedMessages(messages []store.MessageRow) ([]templates.FeedRow, error) {
	out := make([]templates.FeedRow, len(messages))
	for i := range messages {
		bodyHTML, err := Render(displayBody(&messages[i]))
		if err != nil {
			return nil, fmt.Errorf("console: render feed message %d: %w", messages[i].ID, err)
		}
		out[i] = templates.FeedRow{
			ID: messages[i].ID, TicketID: messages[i].TicketID,
			Type: messages[i].Type, Author: messages[i].Author,
			BodyHTML: bodyHTML,
		}
	}
	return out, nil
}

// threadComponent builds the read-only Thread view for the open ticket:
// nil ticket and no rows when open is 0 or names no ticket (design section
// 6.6, carried over from Package 3's patchThread guard).
func (c *console) threadComponent(ctx context.Context, open int64) (templ.Component, error) {
	if open <= 0 {
		return templates.Thread(nil, nil, templates.WaitProgress{}, ""), nil
	}
	ticket, err := c.store.GetTicket(ctx, open)
	switch {
	case err == nil:
		rows, listErr := c.store.ListMessages(ctx, open)
		if listErr != nil {
			return nil, listErr
		}
		rows, detailErr := c.withEscalationDetails(ctx, open, rows)
		if detailErr != nil {
			return nil, detailErr
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
		conv, convErr := c.store.PlanningConversation(ctx, open)
		if convErr != nil {
			return nil, fmt.Errorf("console: planning conversation for ticket %d: %w", open, convErr)
		}
		agent := agentName(c.machine)
		threadRows, buildErr := buildThreadRows(&ticket, rows, plan, scenarios, findings, conv, agent)
		if buildErr != nil {
			return nil, buildErr
		}
		banner, bannerErr := c.threadBanner(ctx, open, rows, conv, agent)
		if bannerErr != nil {
			return nil, bannerErr
		}
		return templates.Thread(&ticket, threadRows, buildWaitProgress(&ticket, rows, conv), banner), nil
	case errors.Is(err, sql.ErrNoRows):
		return templates.Thread(nil, nil, templates.WaitProgress{}, ""), nil
	default:
		return nil, err
	}
}

// threadBanner computes the Thread view's own banner text, ahead of
// buildAgentStatus's own table (design section 22.7, 22.12.4): a gate
// approval in progress overrides it outright, whether or not the
// confirming turn has settled it yet, since the owner's attention belongs
// on the seal, not on which planning thread is or is not waiting on them.
// Every other ticket falls back to buildAgentStatus, which itself renders
// nothing once every planning thread is settled and no approval is
// running.
func (c *console) threadBanner(ctx context.Context, ticketID int64, rows []store.MessageRow, conv store.PlanningConversation, agent string) (string, error) {
	if gateApprovalInProgress(rows) {
		cohort, ok, err := c.store.CurrentCohort(ctx, ticketID)
		if err != nil {
			return "", fmt.Errorf("console: current cohort for ticket %d: %w", ticketID, err)
		}
		confirmed := false
		if ok {
			if _, confirmed, err = c.store.ConfirmedApprovalForVersion(ctx, ticketID, cohort.PlanVersion); err != nil {
				return "", fmt.Errorf("console: confirmed approval for ticket %d: %w", ticketID, err)
			}
		}
		if confirmed {
			return agent + " found no open questions. Sealing the scenarios.", nil
		}
		return "Checking with " + agent + " for open questions before sealing.", nil
	}
	if len(conv.Threads) > 0 {
		return buildAgentStatus(conv, agent), nil
	}
	return "", nil
}

// gateApprovalInProgress reports whether rows carries a gate-kind question
// currently in state "answered" whose newest sent answer picks option "a"
// (design section 22.12.1's "Gate approval in progress": the approval
// starts when the owner sends Approve and ends when the gate question is
// resolved, by the seal, a cancellation, or an escalation -- all of which
// move it out of "answered"). collectSentAnswers already gives each
// question's own newest sent answer (buildThreadRows' own use of it), so
// this reruns that same read rather than needing buildThreadRows' own
// internal map passed out.
func gateApprovalInProgress(rows []store.MessageRow) bool {
	visible := visibleRows(rows)
	sentAnswers := collectSentAnswers(visible)
	for i := range visible {
		m := &visible[i]
		if m.Type != msgTypeQuestion || m.State == nil || *m.State != msgStateAnswered {
			continue
		}
		var p response.QuestionPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue
		}
		if p.Kind != response.QuestionKindGate {
			continue
		}
		if ap, ok := sentAnswers[m.ID]; ok && ap.Option != nil && *ap.Option == "a" {
			return true
		}
	}
	return false
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
	rendered.TicketID = ticketID
	rendered.Editable, err = c.planSealed(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	return &rendered, nil
}

// planSealed reports whether ticketID's current plan cohort (CurrentCohort)
// has at least one sealed scenario (CohortSealState): loadPlan's own
// RenderedPlan.Editable, the gate context's gate for showing the plan
// task owner-edit controls (design section matching #41), matching
// store.OwnerEdit's own not_sealed refusal for a plan_task edit. False,
// with no error, when the ticket has no plan cohort yet, or its cohort's
// run_id is nil (a legacy plan artifact stored before every artifact
// carried run_id).
func (c *console) planSealed(ctx context.Context, ticketID int64) (bool, error) {
	cohort, ok, err := c.store.CurrentCohort(ctx, ticketID)
	if err != nil {
		return false, fmt.Errorf("console: current cohort for ticket %d: %w", ticketID, err)
	}
	if !ok || cohort.RunID == nil {
		return false, nil
	}
	_, sealed, _, err := c.store.CohortSealState(ctx, ticketID, *cohort.RunID)
	if err != nil {
		return false, fmt.Errorf("console: cohort seal state for ticket %d: %w", ticketID, err)
	}
	return sealed >= 1, nil
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
			Check: sc.Check, Sealed: artifacts[i].SealedAt != nil, TicketID: ticketID,
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

// gateCapMarker mirrors job/planning.go's own gateCapMarker exactly (issue
// #48 review P2): the fixed, version-scoped marker that file writes in the
// same commit that posts a gate at machine.toml's planreview max_loops cap.
// loadFindings reads this marker, not current config, so raising max_loops
// after a capped gate posts can never hide the floor findings that gate
// already asked the owner to review.
func gateCapMarker(version int) string {
	return fmt.Sprintf("gate cap reached plan v%d", version)
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
	if unmarshalErr := json.Unmarshal(artifact.Payload, &payload); unmarshalErr != nil {
		return nil, fmt.Errorf("console: unmarshal planreview artifact for ticket %d: %w", ticketID, unmarshalErr)
	}

	// capped reads job/planning.go's own gateCapMarker for this cohort's
	// exact plan version (issue #48 review P2), not current config: a gate
	// posted at the cap carries only at-or-below-floor findings (an
	// above-floor survivor escalates instead of posting a gate), so the
	// per-finding floor filter below must stop hiding them once that marker
	// is present -- they are exactly what the owner now decides on. Reading
	// the marker the gate's own commit wrote, rather than recomputing
	// CountDeliveredReviews against machine.toml, means raising max_loops
	// after the gate posts can never hide findings it already asked the
	// owner to review.
	_, capped, err := c.store.Marker(ctx, ticketID, gateCapMarker(cohort.PlanVersion))
	if err != nil {
		return nil, fmt.Errorf("console: gate cap marker for ticket %d: %w", ticketID, err)
	}

	rows := make([]templates.FindingRow, 0, len(payload.Findings))
	for _, f := range payload.Findings {
		if f.Severity.Rank() <= c.floor.Rank() && !capped {
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

// msgTypeFollowup mirrors the messages.type CHECK list's "followup" value
// (internal/store/migrations/0001_init.sql): D32's own owner-reopen row
// (plan section 22.12.2, "You reopened <key>."), not written by any job or
// store code yet on this branch. buildThreadRows and sentChildText already
// give it a home -- folded under its parent question, same as a resolved
// row -- so D31-6 needs no further placement work when it starts writing
// one.
const msgTypeFollowup = "followup"

// msgTypeResolved mirrors store's own unexported msgTypeResolved
// (internal/store/commit.go): a question's settling row (D31, section
// 22.3), author zing with the decision as its Body, or author system with
// an empty Body for the owner-abandoned and ResolveQuestions paths.
// sentChildText tells the two apart by author.
const msgTypeResolved = "resolved"

// authorYou, authorZing, and authorSystem mirror store's own unexported
// authorYou, authorZing, and authorSystem literals (internal/store/commit.go),
// the same package-local-copy pattern msgTypeReply above and
// demoBuildMarkerAuthor (seed.go) already use. buildThreadRows needs
// authorYou to single out the one unparented row this view still renders as
// a full message card -- a thread-level owner reply
// (design/threading-design.md (d)); sentChildText needs authorZing to tell
// the agent's own settling decision from the system's bare withdrawal (D31,
// section 22.3's resolved row); authorSystem is every marker's own author,
// used by the tests that build synthetic marker rows.
const (
	authorYou    = "you"
	authorZing   = "zing"
	authorSystem = "system"
)

// updateMarker* mirror the literal prefixes internal/job/planning.go and
// internal/job/building.go write into type="update" message bodies --
// planreviewPendingMarker and planreviewDeliveredMarker's "planreview v<N>
// pending"/"...delivered", validationErrorsPendingPrefix and
// validationErrorsDeliveredPrefix's "validation errors pending/delivered
// run <id>[...]", invalidOutputCommit's "response invalid run
// <id>\n<reason>", store.CountSealMismatches' "seal mismatch cohort <id>",
// and building.go's own markerClaimsOkFmt, markerClaimErrorsPendingFmt,
// markerClaimErrorsDeliveredFmt, markerPerimeterResolvedFmt, checkloop.go's
// markerCheckFailedPendingFmt and markerCheckFailedDeliveredFmt, the escalation
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
	updateMarkerCheckFailedPendingPrefix       = "check failed pending run "
	updateMarkerCheckFailedDeliveredPrefix     = "check failed delivered run "
	updateMarkerPerimeterResolvedPrefix        = "perimeter resolved run "
	updateMarkerRetryRequested                 = "retry requested"
	updateMarkerPerimeterQuestionDroppedPrefix = "perimeter question dropped run "
	updateMarkerReviewRoundPrefix              = "review round "
	updateMarkerReviewDiscussedPrefix          = "review discussed "
	updateMarkerReviewNotePrefix               = "review note "
)

// updateMarkerConversationPendingPrefix and updateMarkerConversationDeliveredPrefix
// mirror store's own unexported conversationPendingPrefix and
// conversationDeliveredPrefix (internal/store/conversation_reads.go, D31):
// "conversation pending run <R> batch <B>", which Reserve writes before a
// run carrying owner messages, and "conversation delivered run <R> batch
// <B>", which its commit writes once they land. Neither carries a word the
// owner would read as content -- they exist only so PlanningConversation can
// compute the delivery watermark -- so updateLine hides them outright
// (design/threading-design.md (d): "marker kind is bookkeeping -> hidden"),
// rather than showing an empty-feeling one-line divider for a run and batch
// number nobody asked to see.
const (
	updateMarkerConversationPendingPrefix   = "conversation pending run "
	updateMarkerConversationDeliveredPrefix = "conversation delivered run "
)

// updateMarkerGateConfirmedPrefix, updateMarkerGateApprovalCancelledPrefix,
// and updateMarkerSealRefusedPrefix mirror the D32 gate markers (design
// section 22.12.1, 22.12.3a): internal/job/planning.go's own
// confirmingMarkerBody ("gate confirmed run <R> plan v<V> gate <QID>
// answer <AID>") and its two cancellation call sites ("gate approval
// cancelled gate <QID> run <R>" when the agent cancels in the confirming
// turn, "gate approval cancelled gate <QID> batch <B>" when the owner's
// SendBatch reopens a thread instead, internal/store/console_writes.go),
// and internal/dispatch/dispatch.go's own seal-refused marker ("seal
// refused gate <QID>\n<reason>", two lines). Unlike every other
// updateMarker* pair above, these three are also parented to the gate
// question (parent_id = QID), so turnContent renders them inside that
// question's own turns, not only dividerLine's unparented fallback.
const (
	updateMarkerGateConfirmedPrefix         = "gate confirmed run "
	updateMarkerGateApprovalCancelledPrefix = "gate approval cancelled gate "
	updateMarkerSealRefusedPrefix           = "seal refused gate "
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
	updateMarkerJudgeHostPrefix                = "judge host "
	updateMarkerPrOpenedPrefix                 = "pr opened "
	updateMarkerCIWaitingPrefix                = "ci waiting "
	updateMarkerReviewersReRequestedPrefix     = "reviewers re-requested "
	updateMarkerPrReadyPrefix                  = "pr ready "
	updateMarkerPrDraftPrefix                  = "pr draft "
	updateMarkerThreadsBlockingPrefix          = "threads blocking "
	updateMarkerMergeAskedPrefix               = "merge asked "
	updateMarkerMergeHeldPrefix                = "merge held "
	updateMarkerMergeWithdrawnPrefix           = "merge withdrawn "
	updateMarkerMergeRetryPrefix               = "merge retry "
	updateMarkerMergeRefusedPrefix             = "merge refused "
	updateMarkerPrMergedPrefix                 = "pr merged "
	updateMarkerRespondBatchPrefix             = "respond batch "
	updateMarkerRespondCoverageFailedPrefix    = "respond coverage failed run "
	updateMarkerRespondCoverageDeliveredPrefix = "respond coverage delivered run "
	updateMarkerRespondAppliedPrefix           = "respond applied "
	updateMarkerFixRepliesPostedPrefix         = "fix replies posted "
)

// updateMarkerFixRequestedPrefix and updateMarkerFixLandedPrefix mirror
// internal/job/fix.go's own fixRequestedPrefix and fixLandedPrefix
// literals: "fix requested <kind> after run <R>" (fixRequestMessage) opens
// a fix unit -- kind one of findings, failure, ci_log, or threads
// (FixKind.Values) -- and "fix landed <mid> sha <sha>" (building.go's land
// and its adopted-commit twin) closes it once the unit's own commit lands.
// Neither had a console case until this task: design/threading-design.md's
// review found them unrecognized (a loose card today), and cmd/zing's own
// e2e second guard (verifySelftestMarkersAllRecognized) failed on the first
// real "fix requested failure after run <R>" marker the selftest pipeline
// wrote until these two cases were added.
const (
	updateMarkerFixRequestedPrefix = "fix requested "
	updateMarkerFixLandedPrefix    = "fix landed "
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
// summary shows (design section 6.6): "waiting on you" when open, "resolved"
// when resolved. An answered question (bug fix 9: it was badged "resuming"
// unconditionally, which read as the agent already being on its way back,
// even while the ticket still waited on other questions in the same round)
// splits on revisable, the same D30 flag buildThreadQuestion already
// computes: "answered · can change" while the round isn't done with it yet,
// "answered" once it's locked and nothing more can change here. A missing or
// unrecognized state renders as-is (or empty), rather than guessing.
func questionStateLabel(state *string, revisable bool) string {
	if state == nil {
		return ""
	}
	switch *state {
	case "open":
		return "waiting on you"
	case "answered":
		if revisable {
			return "answered · can change"
		}
		return "answered"
	case msgStateResolved:
		return "resolved"
	default:
		return *state
	}
}

// gateShowsPlan reports whether a question renders the plan, scenarios,
// and findings: only a gate still in play (bug fix: every gate, withdrawn,
// superseded, or rejected ones too, rendered the current plan, so a ticket
// with seven gates showed the same document seven times, and an old gate
// showed a plan it never gated). A resolved gate shows its verdict only;
// the plan stays in the rail's Plan artifact.
func gateShowsPlan(kind response.QuestionKind, state *string) bool {
	return kind == response.QuestionKindGate && (state == nil || *state != msgStateResolved)
}

// isWithdrawnGate reports whether a question is a gate that closed without
// an owner answer (bug fix: a gate withdrawn by a reopen showed "resolved",
// which reads as approved). An approved or rejected gate carries the
// owner's sent answer.
func isWithdrawnGate(kind response.QuestionKind, state *string, answered bool) bool {
	return kind == response.QuestionKindGate && state != nil && *state == msgStateResolved && !answered
}

// isSupersededGate reports whether a resolved gate closed because the
// confirming turn (D32, design section 22.12.3a) answered with a revised
// plan instead of "confirmed": Zing writes a "gate approval cancelled gate
// <QID> run <R>" marker (job/planning.go's cancelGateApproval) as a child of
// the gate question and resolves it, then a fresh gate follows. Without this
// check that gate would read "withdrawn" (isWithdrawnGate also matches it: a
// resolved gate the owner never answered), which reads as nobody having
// approved it -- the owner did approve it, and the agent's own answer is
// what cancelled that approval, so it reads "superseded" instead. children
// is the question's own child rows (buildThreadQuestion's own param, same
// one isWithdrawnGate's caller already has in scope).
func isSupersededGate(kind response.QuestionKind, state *string, children []store.MessageRow) bool {
	if kind != response.QuestionKindGate || state == nil || *state != msgStateResolved {
		return false
	}
	for i := range children {
		if children[i].Type == msgTypeUpdate && strings.HasPrefix(children[i].Body, updateMarkerGateApprovalCancelledPrefix) {
			return true
		}
	}
	return false
}

// gateVerdict reports the pill text for a gate that is resolved, answered,
// and neither withdrawn (isWithdrawnGate) nor superseded (isSupersededGate,
// D32): "approved" when the owner's sent answer picked the gate's "Approve"
// option -- job's own gateApprove then ran the seal pre-check (design
// section 6.6) -- "rejected" when it picked "Reject" -- job's own reject
// path resumes or restarts planning. It is bug fix: ticket 1's question 44,
// answer row 45 with payload option "b", used to fall through
// questionStateLabel's plain "resolved" pill, which does not say which way
// the gate went. The match is read off payload's own option text, not a
// hard-coded "a"/"b": job's own gateOptionApprove/gateOptionReject key
// letters (internal/job/planning.go) are unexported, so console cannot
// import them, and a hard-coded letter would silently stop matching if the
// gate's own option order or keys ever changed. ok is false for anything
// this doesn't cover (not a gate, not resolved, not answered, or an option
// whose text names neither), which leaves the caller's existing
// questionStateLabel result in place rather than guessing.
func gateVerdict(kind response.QuestionKind, state *string, answered bool, ap response.AnswerPayload, options []response.Option) (string, bool) {
	if kind != response.QuestionKindGate || state == nil || *state != msgStateResolved || !answered || ap.Option == nil {
		return "", false
	}
	for _, o := range options {
		if o.Key != *ap.Option {
			continue
		}
		switch o.Text {
		case "Approve":
			return "approved", true
		case "Reject":
			return "rejected", true
		}
	}
	return "", false
}

// msgStateResolved mirrors store's own unexported questionStateResolved
// (internal/store/commit.go), the same package-local-copy pattern
// msgStateOpen and msgStateAnswered (seed.go) already use: console cannot
// import store's unexported constants.
const msgStateResolved = "resolved"

// ticketStatePlanning mirrors store's own unexported ticketStatePlanning
// (internal/store/commit.go), the same package-local-copy pattern
// msgStateResolved above already uses: console cannot import store's
// unexported constants. D32's own "reopenable" test (design section
// 22.12.1) reads it directly: a planning question in state resolved is
// reopenable exactly while its ticket is still in this state, locked for
// good only once the seal commit moves the ticket to "building".
const ticketStatePlanning = "planning"

// hasOpenGateQuestion reports whether rows carries a gate-kind question
// still in state "open" (design section 22.12.2 step 3, 22.12.4): a
// settled, reopenable planning question's own reply-box placeholder names
// "and withdraw the gate" only while reopening would actually do that.
// rows is buildThreadRows' own visibleRows-filtered slice; a gate question
// is never itself a draft, so filtering changes nothing this scans for.
func hasOpenGateQuestion(rows []store.MessageRow) bool {
	for i := range rows {
		m := &rows[i]
		if m.Type != msgTypeQuestion || m.State == nil || *m.State != msgStateOpen {
			continue
		}
		var p response.QuestionPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue
		}
		if p.Kind == response.QuestionKindGate {
			return true
		}
	}
	return false
}

// agentFallback is the generic stand-in agentName returns when it has no
// real machine config to name, and the same generic name displayBody and
// MarkerRecognized pass to updateLine: neither reaches the thread view's
// own real agent name (console.agentName(c.machine), computed only in
// threadComponent), so a gate marker rendered through those two paths
// (the Feed view, and cmd/zing's marker-recognition guard) reads "The
// agent" rather than a specific name it has no way to know.
const agentFallback = "The agent"

// agentName returns the planning job's own display name (design section
// 22.7): its configured model with the first letter upper-cased ("fable"
// gives "Fable"), or agentFallback when m is nil (every test that does not
// exercise the rail, and every view built before a machine.toml loads) or
// the planning job carries no model.
func agentName(m *machine.Machine) string {
	if m == nil {
		return agentFallback
	}
	model := m.Jobs["planning"].Model
	if model == "" {
		return agentFallback
	}
	return strings.ToUpper(model[:1]) + model[1:]
}

// convThreadForQuestion finds questionID's own thread in conv, if any: nil
// when the question carries no planning run at all, which is how
// buildThreadQuestion tells a planning question (design section 22.1) apart
// from every other kind.
func convThreadForQuestion(conv store.PlanningConversation, questionID int64) *store.Thread {
	for i := range conv.Threads {
		if conv.Threads[i].Question.ID == questionID {
			return &conv.Threads[i]
		}
	}
	return nil
}

// planningPill computes a planning question's own summary pill (design
// section 22.7 item 1): "settled" once the agent has settled it; "with
// <agent>" when the in-flight run has already taken delivery of an owner
// message in this thread (a sent row whose batch is above the delivery
// watermark but at or below that run's own ThroughBatch); "queued" when an
// owner message is sent but no run has taken delivery of it yet; "your
// turn" otherwise -- nothing outstanding, so the ball is in the owner's
// court.
func planningPill(t store.Thread, conv store.PlanningConversation, agent string) string {
	if t.Settled {
		return "settled"
	}
	through := conv.Delivered
	inFlight := conv.InFlight != nil
	if inFlight {
		through = conv.InFlight.ThroughBatch
	}
	var withAgent, queued bool
	for i := range t.Turns {
		row := &t.Turns[i]
		if row.Author != authorYou || row.BatchID == nil || *row.BatchID <= conv.Delivered {
			continue
		}
		if inFlight && *row.BatchID <= through {
			withAgent = true
		} else {
			queued = true
		}
	}
	switch {
	case withAgent:
		return "with " + agent
	case queued:
		return "queued"
	default:
		return "your turn"
	}
}

// messageWord pluralizes "message" (design section 22.7's banner table).
func messageWord(n int) string {
	if n == 1 {
		return "1 message"
	}
	return strconv.Itoa(n) + " messages"
}

// buildAgentStatus computes the Thread view's banner for a ticket that
// carries at least one planning question (design section 22.7, agentStatus
// in place of waitProgressBanner): a run in flight reports it is working,
// plus how many owner messages are queued for its next turn (sent but
// beyond that run's own delivery); no run in flight but owner messages
// undelivered reports the agent gets them on its next turn; no run and
// nothing undelivered, but some thread still open, names the open threads;
// every thread settled renders no banner at all.
func buildAgentStatus(conv store.PlanningConversation, agent string) string {
	undelivered := conv.Undelivered()
	if conv.InFlight != nil {
		var queued int
		for i := range undelivered {
			if undelivered[i].BatchID != nil && *undelivered[i].BatchID > conv.InFlight.ThroughBatch {
				queued++
			}
		}
		text := agent + " is working."
		if queued > 0 {
			text += " " + messageWord(queued) + " queued for its next turn."
		}
		return text
	}
	if len(undelivered) > 0 {
		return agent + " gets your " + messageWord(len(undelivered)) + " on its next turn."
	}
	unsettled := conv.Unsettled()
	if len(unsettled) == 0 {
		return ""
	}
	keys := make([]string, 0, len(unsettled))
	for i := range unsettled {
		keys = append(keys, questionPayloadKeyFor(unsettled[i].Question))
	}
	return "Waiting on you: " + strings.Join(keys, ", ") + "."
}

// questionPayloadKeyFor decodes m's QuestionPayload and returns its Key,
// "" when the payload does not decode (unreachable for a real planning
// question row, every one validated against messages/question.json at
// insert; store.PlanningConversation's own questionPayloadKey is
// unexported, so this is console's copy of the same one-field decode).
func questionPayloadKeyFor(m store.MessageRow) string {
	var p response.QuestionPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return ""
	}
	return p.Key
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
// waitRoundKind maps ticket.WaitingOn to the response.QuestionKind its
// current round's questions share, mirroring store's own unexported
// kindForWaitReason (console_writes.go): console cannot import it, so this
// is a package-local copy of the same five-plus-one mapping (bug fix,
// buildWaitProgress below).
func waitRoundKind(waitingOn string) (response.QuestionKind, bool) {
	if waitingOn == waitReasonQuestions {
		return response.QuestionKindQuestion, true
	}
	switch response.QuestionKind(waitingOn) {
	case response.QuestionKindGate, response.QuestionKindSplit,
		response.QuestionKindPerimeter, response.QuestionKindReview, response.QuestionKindMerge:
		return response.QuestionKind(waitingOn), true
	default:
		return "", false // "error" and "children": not question-backed
	}
}

// buildWaitProgress counts the current round's questions (bug fix: "After a
// partial batch, Zing keeps the ticket waiting until every open question is
// answered, which is correct design, but nothing says so"): every "question"
// message whose own Kind matches ticket.WaitingOn's round and whose state is
// still open or already answered (a resolved or otherwise-closed question
// belongs to an earlier, already-cleared round, and never counts). Zero
// Total -- a ticket not currently question-blocked, or one whose
// waiting_on names a reason no question Kind backs ("error", "children") --
// renders no progress line at all (Thread, thread.templ). A planning
// question is skipped outright (design section 22.7): it shares its
// payload's Kind with an ordinary classify-round question, so without this
// exclusion a ticket whose waiting_on happens to be "questions" for a
// planning reason (design section 22.4) would double-count it against a
// round it was never part of. agentStatus (buildAgentStatus), not this
// count, is that question's own progress line.
func buildWaitProgress(ticket *store.Ticket, rows []store.MessageRow, conv store.PlanningConversation) templates.WaitProgress {
	if ticket == nil || ticket.WaitingOn == nil {
		return templates.WaitProgress{}
	}
	kind, ok := waitRoundKind(*ticket.WaitingOn)
	if !ok {
		return templates.WaitProgress{}
	}
	planningIDs := make(map[int64]bool, len(conv.Threads))
	for i := range conv.Threads {
		planningIDs[conv.Threads[i].Question.ID] = true
	}
	var progress templates.WaitProgress
	for i := range rows {
		m := &rows[i]
		if m.Type != msgTypeQuestion || m.State == nil {
			continue
		}
		if planningIDs[m.ID] {
			continue
		}
		if *m.State != msgStateOpen && *m.State != msgStateAnswered {
			continue
		}
		var payload response.QuestionPayload
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			continue
		}
		if payload.Kind != kind {
			continue
		}
		progress.Total++
		if *m.State == msgStateAnswered {
			progress.Answered++
		}
	}
	return progress
}

// buildThreadRows places every row by structure, not by type
// (design/threading-design.md (d), task D31-4a): a question or escalation
// row always opens its own thread -- the one case design section 6.6's
// interactive group applies to, and the only other case (escalation) that
// still renders a plain card -- whatever its own parent_id (an
// escalation-linked question's parent is the escalation it belongs to, and
// still gets its own group, unaffected by the rule below). Any other row
// that names a parent folds into that parent's thread instead of becoming a
// row of its own, whatever its type: the smell this task fixes is that a
// resolved row, an agent reply, or any future kind with an anchor in the
// database used to be thrown away by a renderer that only looked for reply
// and answer. The one row that still gets a full message card with no
// parent is a thread-level owner reply (reply/you); every other unparented
// row -- a state transition, a recognized or unrecognized "update" marker,
// or any other type this view does not otherwise expect loose -- renders as
// a one-line timeline divider instead, or nothing at all when updateLine
// reports the marker is pure bookkeeping. No row is ever dropped silently:
// a parented row is accounted for under its parent's MessageCount even when
// sentChildText has nothing to say about its type, and an unparented row
// always becomes either a divider or the one allowed card -- never an empty
// top-level row. conv is the ticket's whole planning conversation (design
// section 22.1, 22.3): a question whose id names one of conv.Threads is a
// planning question, rendered through buildThreadQuestion's conversation
// branch (turns in conversation order, the agentName-labeled pill, no
// AnsweredHTML); every other question kind is unaffected. agent is that
// conversation's own display name (views.go's agentName), used for a
// planning question's "with <agent>" pill and "Settled by <agent>" line.
func buildThreadRows(ticket *store.Ticket, rows []store.MessageRow, plan *templates.RenderedPlan, scenarios []templates.ScenarioRow, findings []templates.FindingRow, conv store.PlanningConversation, agent string) ([]templates.ThreadRow, error) {
	drafts := collectQuestionDrafts(rows)
	rows = visibleRows(rows)
	sentAnswers := collectSentAnswers(rows)

	// messageCounts holds, per parent message id, how many other messages in
	// this ticket name it as their parent (design section 6.6: the <details>
	// summary shows "the message count"). sentChildren holds those same rows
	// themselves, in message order, for buildThreadQuestion's own SentReplies
	// formatting (bug fix 10, extended by D31-4a to every type, not just
	// reply and answer: "whatever its type" is this function's own rule, so
	// the children map it builds from must not filter on type either). Both
	// are precomputed once over every row rather than per question, so this
	// stays O(n) instead of O(n*questions).
	messageCounts := make(map[int64]int, len(rows))
	sentChildren := make(map[int64][]store.MessageRow, len(rows))
	for i := range rows {
		if rows[i].ParentID == nil {
			continue
		}
		messageCounts[*rows[i].ParentID]++
		sentChildren[*rows[i].ParentID] = append(sentChildren[*rows[i].ParentID], rows[i])
	}

	// gateOpen feeds a settled, reopenable planning question's own reply-box
	// placeholder (design section 22.12.4): "...and withdraw the gate" only
	// while reopening would actually do that (22.12.2 step 3), which is
	// exactly when the ticket still carries an open gate question. Computed
	// once here, the same precomputed-over-every-row pattern messageCounts
	// and sentChildren already use, rather than rescanning rows per question.
	gateOpen := hasOpenGateQuestion(rows)

	out := make([]templates.ThreadRow, 0, len(rows))
	for i := range rows {
		row := &rows[i]

		// A question or escalation row always opens its own thread, whatever
		// its own parent_id: type decides before parent_id gets a turn. The
		// one exception (bug fix, ticket 1's duplicate card: escalation 48,
		// parent_id NULL, and question 49, parent_id 48, whose body repeats
		// the escalation's own summary text): an escalation with a question
		// child renders no row of its own -- the question already carries
		// that text, so the escalation would otherwise stack a second,
		// identical card directly above it. An escalation with no question
		// child yet (the owner has not been asked) keeps today's own card.
		if row.Type == msgTypeEscalation && escalationHasQuestionChild(sentChildren[row.ID]) {
			continue
		}
		if row.Type == msgTypeQuestion || row.Type == msgTypeEscalation {
			convThread := convThreadForQuestion(conv, row.ID)
			question, err := buildThreadQuestion(ticket, row, messageCounts[row.ID]+1, plan, scenarios, findings, drafts, sentAnswers, sentChildren[row.ID], convThread, conv, agent, gateOpen)
			if err != nil {
				return nil, err
			}
			out = append(out, templates.ThreadRow{
				ID: row.ID, Type: row.Type, Author: row.Author,
				Body:     displayBody(row),
				Question: question,
			})
			continue
		}

		// Any other row naming a parent was already folded into that
		// parent's own thread above (sentChildren) and never gets a row of
		// its own here.
		if row.ParentID != nil {
			continue
		}

		// The one unparented row that still gets a full message card: a
		// thread-level owner reply.
		if row.Type == msgTypeReply && row.Author == authorYou {
			out = append(out, templates.ThreadRow{
				ID: row.ID, Type: row.Type, Author: row.Author,
				Body: displayBody(row),
			})
			continue
		}

		line, show := dividerLine(row, agent)
		if !show {
			continue
		}
		out = append(out, templates.ThreadRow{
			ID: row.ID, Type: row.Type, Author: row.Author,
			Body: line, Divider: true,
		})
	}
	return out, nil
}

// escalationHasQuestionChild reports whether children (buildThreadRows' own
// precomputed sentChildren[row.ID]) names a question among them: an
// escalation always gets a question child when the owner is actually asked
// about it (design/threading-design.md's "an escalation-linked question's
// parent is the escalation it belongs to"), and that question's own Body
// repeats the escalation's summary text, so the escalation itself would
// otherwise render as a duplicate card directly above it (bug fix, ticket
// 1's escalation 48 / question 49).
func escalationHasQuestionChild(children []store.MessageRow) bool {
	for i := range children {
		if children[i].Type == msgTypeQuestion {
			return true
		}
	}
	return false
}

// dividerLine computes an unparented, non-root row's one-line timeline text
// (design/threading-design.md (d)): an "update" marker goes through
// updateLine, whose own second return tells buildThreadRows whether to show
// it at all; every other type (chiefly "state", but defensively any other
// type this view does not expect loose, such as a stray "resolved" or
// "answer" row with no parent) goes through displayBody and is always
// shown, the same unconditional rendering a state separator already gets
// today.
func dividerLine(m *store.MessageRow, agent string) (string, bool) {
	if m.Type == msgTypeUpdate {
		return updateLine(m, agent)
	}
	return displayBody(m), true
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

// pickedAnswerText formats a planning question's own owner pick as a turn
// (design section 22.7 item 5): "picked <chip number>. <option text>",
// the same chip number optionChips renders, or "picked <key>" when the key
// names no option (defensive, mirrors sentAnswerText's own fallback). An
// item-kind answer -- never written against a planning question in
// practice, since a planning question's payload carries options, not items
// (design section 22.1) -- falls back to sentAnswerText's own ref:decision
// formatting, unprefixed.
func pickedAnswerText(payload response.AnswerPayload, options []templates.ThreadOption) string {
	if payload.Option != nil {
		for i, o := range options {
			if o.Key == *payload.Option {
				return "picked " + strconv.Itoa(i+1) + ". " + o.Text
			}
		}
		return "picked " + *payload.Option
	}
	return sentAnswerText(payload, options)
}

// turnContent returns one child row's own turn label and markdown text, for
// buildTurns below (bug fix 10, extended by D31 and D31-4a, and by D31-5's
// fix for "duplicate Answered: plus You:"): a plain reply's own Body --
// the owner's typed text ("You"), or D31's own agent reply (agent) -- an
// answer's payload run through pickedAnswerText, but only inside a
// planning question (planning true): every other kind's AnsweredHTML
// already shows the very same pick, so rendering it here too was the
// duplicate bug. A resolved row is the settling decision when the agent
// wrote one (ok false: SettledLabel/SettledHTML show it instead, not a
// turn) or a bare acknowledgement for the owner-abandoned and
// ResolveQuestions paths, which post with no Body of their own
// ("Resolved."). A followup row (D32, section 22.12.2: the owner reopening
// an already-settled thread, not written by any job or store code yet on
// this branch) names itself, with no separate author label. Any other
// type, or an answer whose payload fails to decode, reports ok false,
// which buildTurns skips rather than adding a blank line -- the row is
// still accounted for in the question's own MessageCount (buildThreadRows),
// just with nothing of its own to show as a turn.
func turnContent(m *store.MessageRow, options []templates.ThreadOption, key, agent string, planning bool) (author, text string, ok bool) {
	switch m.Type {
	case msgTypeReply:
		if m.Author == authorYou {
			return "You", m.Body, true
		}
		return agent, m.Body, true
	case msgTypeAnswer:
		if !planning {
			return "", "", false
		}
		var ap response.AnswerPayload
		if err := json.Unmarshal(m.Payload, &ap); err != nil {
			return "", "", false
		}
		return "You", pickedAnswerText(ap, options), true
	case msgTypeResolved:
		if m.Author == authorZing {
			return "", "", false // the settling decision: SettledLabel/SettledHTML, not a turn
		}
		return "", "Resolved.", true
	case msgTypeFollowup:
		return "", "You reopened " + key + ".", true
	case msgTypeUpdate:
		// A D32 gate marker (updateMarkerGateConfirmedPrefix,
		// updateMarkerGateApprovalCancelledPrefix,
		// updateMarkerSealRefusedPrefix) is parented to the gate question
		// (design section 22.12.1's "Placement" rule), so it reaches this
		// case rather than dividerLine's unparented fallback; updateLine's
		// own second return still hides a bookkeeping marker outright
		// (nothing today parents one of those here, but the fallback costs
		// nothing) rather than showing an empty turn.
		line, show := updateLine(m, agent)
		if !show {
			return "", "", false
		}
		return "", line, true
	default:
		return "", "", false
	}
}

// mergeThreadOrder reorders children (buildThreadRows' own generic
// parent_id fold, which includes every child row regardless of type or
// author) to match turns' own order (store.PlanningConversation's turn
// order, design section 22.3), inserting any row turns has no place for --
// a system-authored resolved row ResolveQuestions or withdraw writes
// (internal/store/commit.go), which store's agentRowsByQuestion (author
// zing only) never reads back, so threadTurns never sees it -- by id: each
// such row goes in just before the first turn whose id is greater, or at
// the end if none is greater (live console bug, ticket 1: a settle's
// "Resolved." row must render in its chronological place, not always
// last). It never changes which rows belong to the question -- only their
// display order -- so a row store.PlanningConversation does not recognize
// is never dropped. This is the one place that placement happens; nothing
// else re-derives it.
func mergeThreadOrder(children, turns []store.MessageRow) []store.MessageRow {
	if len(turns) == 0 {
		return children
	}
	inTurns := make(map[int64]bool, len(turns))
	for i := range turns {
		inTurns[turns[i].ID] = true
	}
	out := append([]store.MessageRow(nil), turns...)
	for i := range children {
		c := children[i]
		if inTurns[c.ID] {
			continue
		}
		pos := len(out)
		for j := range out {
			if out[j].ID > c.ID {
				pos = j
				break
			}
		}
		out = append(out, store.MessageRow{})
		copy(out[pos+1:], out[pos:])
		out[pos] = c
	}
	return out
}

// buildTurns assembles a question's displayed Turns (design section 22.7):
// convThread's own turn order when this is a planning question (design
// section 22.1) -- reordering children, never changing which rows belong to
// it, mergeThreadOrder -- otherwise children in their own (message) order.
// Each turn's text is rendered through Render (bug fix: raw backticks
// showed literally), and an owner turn past the delivery watermark is
// tagged Queued -- meaningful only for a planning question; every other
// kind's Queued is always false, since store.PlanningConversation never
// classifies a non-planning row's batch.
func buildTurns(children []store.MessageRow, convThread *store.Thread, conv store.PlanningConversation, options []templates.ThreadOption, key, agent string) ([]templates.Turn, error) {
	ordered := children
	planning := convThread != nil
	if planning {
		ordered = mergeThreadOrder(children, convThread.Turns)
	}
	turns := make([]templates.Turn, 0, len(ordered))
	for i := range ordered {
		m := &ordered[i]
		author, text, ok := turnContent(m, options, key, agent, planning)
		if !ok {
			continue
		}
		bodyHTML, err := Render(text)
		if err != nil {
			return nil, fmt.Errorf("console: render question %s turn %d: %w", key, m.ID, err)
		}
		queued := planning && m.Author == authorYou && m.BatchID != nil && *m.BatchID > conv.Delivered
		turns = append(turns, templates.Turn{Author: author, BodyHTML: bodyHTML, Queued: queued})
	}
	return turns, nil
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
// returned rather than silently dropping the question's body. children is
// this question's own sent reply and answer rows, in message order (bug fix
// 10), formatted into ThreadQuestion.Turns below (buildTurns) rather than
// left for buildThreadRows to render a second time as standalone rows.
// convThread is this question's own entry in the ticket's planning
// conversation, nil for every kind but a planning question (design section
// 22.1); conv carries that conversation's delivery watermark and in-flight
// run, for convThread's own pill and Queued tags; agent is the planning
// job's own display name (views.go's agentName).
func buildThreadQuestion(ticket *store.Ticket, m *store.MessageRow, messageCount int, plan *templates.RenderedPlan, scenarios []templates.ScenarioRow, findings []templates.FindingRow, drafts map[int64]questionDraft, sentAnswers map[int64]response.AnswerPayload, children []store.MessageRow, convThread *store.Thread, conv store.PlanningConversation, agent string, gateOpen bool) (*templates.ThreadQuestion, error) {
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

	options := make([]templates.ThreadOption, 0, len(payload.Options))
	for _, o := range payload.Options {
		textHTML, optErr := RenderInline(o.Text)
		if optErr != nil {
			return nil, fmt.Errorf("console: render question %d option %s: %w", m.ID, o.Key, optErr)
		}
		options = append(options, templates.ThreadOption{Key: o.Key, Text: o.Text, TextHTML: textHTML})
	}
	items := make([]templates.ThreadItem, 0, len(payload.Items))
	for _, it := range payload.Items {
		items = append(items, templates.ThreadItem{Ref: it.Ref, Text: it.Text})
	}

	// RecommendedHTML maps a leading option key to its own chip number and
	// text before rendering (bug fix: "Recommended: a:" against chips
	// numbered 1 to 4, design section 22.7 item 3), now that options is
	// built -- templates.RecommendedDisplayText needs it to look the key up.
	var recommendedHTML templ.Component
	if payload.Recommended != "" {
		recommendedHTML, err = Render(templates.RecommendedDisplayText(payload.Recommended, options))
		if err != nil {
			return nil, fmt.Errorf("console: render question %d recommendation: %w", m.ID, err)
		}
	}

	// planning is this question's own test for "is this a planning
	// question" (design section 22.1): convThread is set only when
	// buildThreadRows found this question's id among the ticket's planning
	// conversation. A planning question's lifecycle is D31's own (open,
	// resolved; D31-5 renders through planningPill and Turns/SettledHTML);
	// every other kind keeps the D30 answered/resolved rendering unchanged.
	planning := convThread != nil
	var interactive, revisable bool
	var stateLabel string
	switch {
	case planning:
		interactive = !convThread.Settled
		stateLabel = planningPill(*convThread, conv, agent)
	default:
		revisable = m.State != nil && *m.State == msgStateAnswered && ticketStillWaitingOnQuestions(ticket)
		// Interactive is true for a still-open question (code review fix,
		// PR #16: questionGroup (thread.templ) used to render option chips,
		// item rows, and the free reply input for every question regardless
		// of state, so an answered or resolved question -- whose draft
		// SaveDraft would refuse anyway (openQuestionForTicketTx) -- still
		// looked editable), and, as of D30, also for a question already
		// answered while the ticket still waits on this round: the owner
		// can still revise a pick before Zing resumes the agent with it
		// (console_writes.go's questionDraftableTx carries the same rule
		// server-side; this is the rendering half).
		interactive = (m.State != nil && *m.State == msgStateOpen) || revisable
		stateLabel = questionStateLabel(m.State, revisable)
	}
	gateAnswer, answered := sentAnswers[m.ID]
	switch {
	case isSupersededGate(payload.Kind, m.State, children):
		stateLabel = "superseded"
	case isWithdrawnGate(payload.Kind, m.State, answered):
		stateLabel = "withdrawn"
	default:
		if verdict, ok := gateVerdict(payload.Kind, m.State, answered, gateAnswer, payload.Options); ok {
			stateLabel = verdict
		}
	}

	q := &templates.ThreadQuestion{
		Key: payload.Key, Title: title, Kind: string(payload.Kind),
		BodyHTML: bodyHTML, Recommended: payload.Recommended, RecommendedHTML: recommendedHTML,
		Options: options, Items: items, StateLabel: stateLabel,
		MessageCount: messageCount,
		Interactive:  interactive,
		Revisable:    revisable,
	}

	// ReopenPlaceholder (D32, design section 22.12.2, 22.12.4): a settled
	// planning thread keeps its reply box, as a reopen box, for as long as
	// the ticket is still reopenable -- state "planning", not yet sealed
	// into "building". 22.7 item 6's "no reply box when settled" now applies
	// only once the thread is locked for good (questionGroup, thread.templ,
	// gates this on Interactive||ReopenPlaceholder!=""). The gate clause
	// names what reopening would actually withdraw (22.12.2 step 3): nothing
	// when no gate question is currently open.
	if planning && convThread.Settled && ticket != nil && ticket.State == ticketStatePlanning {
		placeholder := "Write to reopen " + payload.Key
		if gateOpen {
			placeholder += " and withdraw the gate"
		}
		q.ReopenPlaceholder = placeholder
	}

	turns, err := buildTurns(children, convThread, conv, options, payload.Key, agent)
	if err != nil {
		return nil, err
	}
	q.Turns = turns

	// SettledLabel/SettledHTML (design section 22.7 item 6, extended by
	// D32/22.12.4): a planning question's own closing line, once the agent
	// has settled it with a decision at least once. A settle with no
	// decision text (the owner-abandoned path, section 22.3's
	// resolved/system row) renders no closing line here -- that row is
	// instead an ordinary, unlabeled turn ("Resolved.",
	// turnContent/mergeThreadOrder above), so a settled thread is never left
	// with an empty one. store.PlanningConversation keeps Decision at the
	// newest settle's text even after a reopen moves the question back to
	// open (conversation_reads.go), so a reopened-but-not-yet-resettled
	// thread still shows it, labeled "Earlier decision" instead of "Settled
	// by <agent>" until the agent settles it again.
	if planning && convThread.Decision != "" {
		decisionHTML, decErr := Render(convThread.Decision)
		if decErr != nil {
			return nil, fmt.Errorf("console: render question %d decision: %w", m.ID, decErr)
		}
		if convThread.Settled {
			q.SettledLabel = "Settled by " + agent
		} else {
			q.SettledLabel = "Earlier decision"
		}
		q.SettledHTML = decisionHTML
	}

	// AnsweredHTML (bug fix): a closed, non-planning question's locked
	// note, shown instead of its now-hidden controls, only for
	// state=answered and no longer revisable -- not resolved, a later
	// terminal state a reader has already moved past, and not a still-
	// revisable one, which keeps its live controls (Revisable above)
	// rather than locking. A planning question never reaches this: D31
	// never sets state=answered (section 22.3), so AnsweredHTML and
	// SettledHTML are mutually exclusive in practice.
	if !planning && m.State != nil && *m.State == msgStateAnswered && !revisable {
		if ap, ok := sentAnswers[m.ID]; ok {
			if txt := sentAnswerText(ap, options); txt != "" {
				answeredHTML, ansErr := Render(txt)
				if ansErr != nil {
					return nil, fmt.Errorf("console: render question %d answered text: %w", m.ID, ansErr)
				}
				q.AnsweredHTML = answeredHTML
			}
		}
	}
	if payload.Kind == response.QuestionKindMerge && ticket != nil && ticket.PRURL != nil {
		q.PRURL = *ticket.PRURL
	}
	if gateShowsPlan(payload.Kind, m.State) {
		q.Plan = plan
		q.Scenarios = scenarios
		q.Findings = findings
	}
	if payload.Amendment != nil {
		a := payload.Amendment
		q.Amendment = &templates.ScenarioRow{
			ID: a.Scenario, Kind: string(a.Kind), Given: a.Given, When: a.When, Then: a.Then,
			Check: a.Check, Sealed: true, TicketID: m.TicketID,
		}
	}
	if draft, ok := drafts[m.ID]; ok {
		q.DraftReply = draft.Reply
		if draft.Answer.Option != nil {
			q.DraftOption = *draft.Answer.Option
		}
		q.DraftItems = draft.Answer.Items
	}
	// A revisable question with no unsent draft yet still shows its last
	// sent pick, chip-picked the same way an unsent draft would, so the
	// owner sees what they are revising rather than a group that looks
	// freshly blank (D30).
	if revisable && q.DraftOption == "" && len(q.DraftItems) == 0 {
		if ap, ok := sentAnswers[m.ID]; ok {
			if ap.Option != nil {
				q.DraftOption = *ap.Option
			}
			if len(ap.Items) > 0 {
				q.DraftItems = ap.Items
			}
		}
	}
	// PickedOption/PickedItems (bug fix: "options vanish once locked", item
	// 4): once a question is no longer interactive, optionChips and
	// itemRows still need to show what was actually picked, every kind
	// alike -- not just a still-revisable one (DraftOption's own fallback
	// above).
	if !interactive {
		if ap, ok := sentAnswers[m.ID]; ok {
			if ap.Option != nil {
				q.PickedOption = *ap.Option
			}
			if len(ap.Items) > 0 {
				q.PickedItems = ap.Items
			}
		}
	}
	return q, nil
}

// ticketStillWaitingOnQuestions reports whether ticket's own waiting_on is
// still "questions" (D30, mirrors console_writes.go's questionDraftableTx):
// false for a nil ticket (never happens for a real question render, but
// buildThreadQuestion's own ticket param is itself nilable) or any other
// wait reason, including none.
func ticketStillWaitingOnQuestions(ticket *store.Ticket) bool {
	return ticket != nil && ticket.WaitingOn != nil && *ticket.WaitingOn == waitReasonQuestions
}

// splitQuestionBody splits a question message's Body into its heading (the
// first line) and the text after it, matching how a planning commit writes
// Title and Body together: title first as the heading, then a blank line,
// then the body (design section 6.6).
func splitQuestionBody(raw string) (title, body string) {
	title, rest, _ := strings.Cut(raw, "\n")
	return title, strings.TrimPrefix(rest, "\n")
}

// withEscalationDetails rewrites, in place, the Body of every escalation's
// own question row with detail read at render time from the data the
// escalation points at (ticket 70, design section "Build the detail at read
// time"): a plan review loops_exhausted question gets the planreview
// artifact's remaining findings. rows is returned unchanged -- not even
// reordered -- except for those Body rewrites, so threadComponent's later
// buildThreadRows call sees the same rows it always did, with richer
// question bodies. Only a store read failure is returned; every other
// escalation whose payload, marker, or artifact is missing or unparseable
// just keeps its stored body (buildThreadQuestion's own
// "unparseable payload renders as a plain row" rule, extended here).
func (c *console) withEscalationDetails(ctx context.Context, ticketID int64, rows []store.MessageRow) ([]store.MessageRow, error) {
	var runs []store.Run
	var sessions []store.Session
	runsLoaded := false

	for i := range rows {
		esc := rows[i]
		if esc.Type != msgTypeEscalation {
			continue
		}
		var ep response.EscalationPayload
		if err := json.Unmarshal(esc.Payload, &ep); err != nil {
			continue
		}

		var detail string
		switch {
		case ep.Code == string(response.EscalationCodeLoopsExhausted) && ep.Origin == string(response.EscalationOriginCapLoops):
			d, err := c.loopsExhaustedDetail(ctx, ticketID, esc.ID, rows)
			if err != nil {
				return nil, err
			}
			detail = d
		case ep.Code == string(response.EscalationCodeResponseInvalid) && esc.RunID != nil:
			if !runsLoaded {
				var err error
				runs, err = c.store.RunsForTicket(ctx, ticketID)
				if err != nil {
					return nil, err
				}
				sessions, err = c.store.SessionsForTicket(ctx, ticketID)
				if err != nil {
					return nil, err
				}
				runsLoaded = true
			}
			detail = c.responseInvalidDetailFor(ctx, ticketID, esc.ID, *esc.RunID, rows, runs, sessions)
		}
		if detail == "" {
			continue
		}

		for j := range rows {
			q := rows[j]
			if q.Type != msgTypeQuestion || q.ParentID == nil || *q.ParentID != esc.ID {
				continue
			}
			title, body := splitQuestionBody(q.Body)
			rows[j].Body = title + "\n\n" + detail + "\n\n" + body
		}
	}
	return rows, nil
}

// loopsExhaustedDetail reads the plan review findings a cap_loops
// loops_exhausted escalation (escalationID) never itself carries (design
// H1): the version comes from the newest "planreview vN pending" marker
// stored before the escalation, and the findings from the planreview
// artifact at that version. It returns "" with no error when the marker is
// missing or unparseable, or the artifact at that version does not exist --
// each logged once at Debug rather than failing the render.
func (c *console) loopsExhaustedDetail(ctx context.Context, ticketID, escalationID int64, rows []store.MessageRow) (string, error) {
	version, ok := latestPlanreviewPendingVersion(rows, escalationID)
	if !ok {
		slog.DebugContext(ctx, "console: escalation detail source unavailable",
			"ticket_id", ticketID, "escalation_id", escalationID, "reason", "marker")
		return "", nil
	}

	artifact, found, err := c.store.PlanReviewAt(ctx, ticketID, version)
	if err != nil {
		return "", err
	}
	if !found {
		slog.DebugContext(ctx, "console: escalation detail source unavailable",
			"ticket_id", ticketID, "escalation_id", escalationID, "reason", "artifact", "plan_version", version)
		return "", nil
	}
	var payload struct {
		Findings []response.Finding `json:"findings"`
	}
	if err := json.Unmarshal(artifact.Payload, &payload); err != nil {
		slog.DebugContext(ctx, "console: escalation detail source unavailable",
			"ticket_id", ticketID, "escalation_id", escalationID, "reason", "artifact", "plan_version", version)
		return "", nil //nolint:nilerr // an unparseable artifact leaves the question body unchanged, not an error
	}
	return findingsDetail(version, payload.Findings), nil
}

// latestPlanreviewPendingVersion returns the version named by the newest
// "planreview vN pending" update row whose id is below beforeID -- rows is
// ordered by id ascending (store.ListMessages), so the last match as this
// scans in order is the newest one -- and false when there is none, or its
// version fails strconv.Atoi.
func latestPlanreviewPendingVersion(rows []store.MessageRow, beforeID int64) (int, bool) {
	var body string
	var found bool
	for i := range rows {
		if rows[i].Type != msgTypeUpdate || rows[i].ID >= beforeID {
			continue
		}
		if !strings.HasPrefix(rows[i].Body, updateMarkerPlanreviewPrefix) || !strings.HasSuffix(rows[i].Body, updateMarkerPlanreviewPendingSuffix) {
			continue
		}
		body, found = rows[i].Body, true
	}
	if !found {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(body, updateMarkerPlanreviewPrefix), updateMarkerPlanreviewPendingSuffix))
	if err != nil {
		return 0, false
	}
	return v, true
}

// findingsDetail renders a plan review's own remaining findings as a
// markdown list, one item per finding ("- SEVERITY at `LOCATION`: TEXT"),
// under a heading naming version. It returns "" for no findings, so a
// caller can treat that as "no detail" the same way it treats a missing
// marker or artifact.
func findingsDetail(version int, findings []response.Finding) string {
	if len(findings) == 0 {
		return ""
	}
	lines := make([]string, 0, len(findings)+1)
	lines = append(lines, fmt.Sprintf("Remaining findings from the plan review of v%d:", version))
	for _, f := range findings {
		text := strings.Join(strings.Fields(f.Text), " ")
		lines = append(lines, fmt.Sprintf("- %s at `%s`: %s", f.Severity, f.Location, text))
	}
	return strings.Join(lines, "\n")
}

// responseInvalidDetailFor reads the job, lens and validator errors a
// response_invalid escalation's own why never carries (design H2): the
// run's own session names the job, the run itself carries the lens when
// one applies, and the validator's errors sit on that run's own "response
// invalid run <id>" marker (invalidMarkerBody, internal/job/planning.go).
// It returns "" with no error when the marker, the run, or its session is
// missing -- each logged once at Debug rather than failing the render, the
// same fallback loopsExhaustedDetail uses for its own missing sources.
func (c *console) responseInvalidDetailFor(
	ctx context.Context, ticketID, escalationID, runID int64, rows []store.MessageRow, runs []store.Run, sessions []store.Session,
) string {
	marker, ok := findResponseInvalidMarker(rows, runID)
	if !ok {
		slog.DebugContext(ctx, "console: escalation detail source unavailable",
			"ticket_id", ticketID, "escalation_id", escalationID, "reason", "marker", "run_id", runID)
		return ""
	}

	run, ok := findRun(runs, runID)
	if !ok {
		slog.DebugContext(ctx, "console: escalation detail source unavailable",
			"ticket_id", ticketID, "escalation_id", escalationID, "reason", "run", "run_id", runID)
		return ""
	}

	session, ok := findSession(sessions, run.SessionID)
	if !ok {
		slog.DebugContext(ctx, "console: escalation detail source unavailable",
			"ticket_id", ticketID, "escalation_id", escalationID, "reason", "session", "run_id", runID)
		return ""
	}

	return responseInvalidDetail(session.Job, run.Lens, runID, marker)
}

// findResponseInvalidMarker returns the body of the update row among rows
// whose own first line is exactly "response invalid run <runID>"
// (invalidMarkerBody), matching on the full first line rather than a bare
// prefix so run 9's marker is never mistaken for run 91's.
func findResponseInvalidMarker(rows []store.MessageRow, runID int64) (string, bool) {
	want := updateMarkerResponseInvalidPrefix + strconv.FormatInt(runID, 10)
	for i := range rows {
		if rows[i].Type != msgTypeUpdate {
			continue
		}
		first, _, _ := strings.Cut(rows[i].Body, "\n")
		if first == want {
			return rows[i].Body, true
		}
	}
	return "", false
}

// findRun returns the run in runs whose ID is runID.
func findRun(runs []store.Run, runID int64) (store.Run, bool) {
	for _, r := range runs {
		if r.ID == runID {
			return r, true
		}
	}
	return store.Run{}, false
}

// findSession returns the session in sessions whose ID is sessionID.
func findSession(sessions []store.Session, sessionID int64) (store.Session, bool) {
	for _, s := range sessions {
		if s.ID == sessionID {
			return s, true
		}
	}
	return store.Session{}, false
}

// responseInvalidDetail renders a response_invalid escalation's own job,
// lens, run id, and the run's own "response invalid run <id>" marker body
// (invalidMarkerBody, internal/job/planning.go) into the detail
// withEscalationDetails appends to the question body (design H2). marker's
// first line (the marker's own run id) is discarded in favor of the
// caller's own authoritative runID; its second line is the closed reason,
// shown only when there are no validator errors. The validator's errors --
// model text that can hold anything, including a markdown fence of its own
// -- sit in a fenced code block one backtick longer than the longest
// backtick run they contain, so they always render as inert text rather
// than breaking out of the fence early.
func responseInvalidDetail(job string, lens *string, runID int64, marker string) string {
	_, rest, _ := strings.Cut(marker, "\n")
	reason, errs, hasErrs := strings.Cut(rest, "\n")

	head := "Job: " + job
	if lens != nil {
		head += ", lens " + *lens
	}
	head += fmt.Sprintf(". Run %d.", runID)

	if !hasErrs || errs == "" {
		return head + "\n\nReason: " + reason + "."
	}

	fence := backtickFence(errs)
	return head + "\n\nValidator errors:\n\n" + fence + "\n" + errs + "\n" + fence
}

// backtickFence returns a markdown code-fence delimiter for text: backticks
// one longer than the longest run of consecutive backticks text contains,
// and never fewer than 3 (CommonMark's own minimum fence length).
func backtickFence(text string) string {
	longest, cur := 0, 0
	for _, r := range text {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	n := max(longest+1, 3)
	return strings.Repeat("`", n)
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
		// The Feed view (displayFeedMessages) has no notion of a hidden row --
		// every ticket's every message is a line there -- so a marker
		// updateLine hides from the Thread view (buildThreadRows' own
		// dividerLine, which checks the second return value) still shows its
		// raw body here, the same defensive fallback an unrecognized marker
		// already gets. agentFallback, not a real machine config, names the
		// agent in a gate marker's sentence: the Feed view has no ticket-
		// specific agent to read (console.agentName(c.machine) is computed
		// only in threadComponent), and this function's own signature, called
		// directly by many existing tests, stays single-argument rather than
		// threading one through for the sake of three marker shapes.
		if line, ok := updateLine(m, agentFallback); ok {
			return line
		}
		return m.Body
	default:
		return m.Body
	}
}

// MarkerRecognized reports whether updateLine actually registers m's own
// body, for cmd/zing's e2e second guard (design/threading-design.md: "it
// renders every ticket and asserts that updateLine recognized every update
// row the run wrote. That catches a new marker that someone writes but
// never registers, which the hand-kept table cannot."). A non-"update" row
// has no marker to recognize, so it always reports true. updateLine's own
// registered cases always rewrite the body into either an owner-facing
// sentence or nothing (a deliberately hidden marker, shown == false); only
// its unregistered default case echoes the raw body back unchanged with
// shown == true, so that one shape -- a marker whose prefix updateLine's
// switch does not list at all -- is what this reports false for, without
// this function re-listing every updateMarker* prefix a second time.
func MarkerRecognized(m store.MessageRow) bool {
	if m.Type != msgTypeUpdate {
		return true
	}
	if m.EventKind != nil {
		_, ok := eventRules[*m.EventKind]
		return ok
	}
	// agentFallback: see displayBody's own identical call, same reasoning --
	// cmd/zing's selftest guard has no per-ticket agent name to pass, and
	// recognition never depends on which name a gate marker's sentence
	// carries, only on whether it differs from the raw body.
	line, shown := updateLine(&m, agentFallback)
	return !shown || line != m.Body
}

// updateLine recognizes the known planning-bookkeeping markers a type=
// "update" message's Body carries (F012: the owner should never read
// "planreview v3 pending" or a raw response.PathError line) and returns an
// owner-facing sentence in its place. The second return reports whether the
// Thread view should show this row at all (design/threading-design.md (d),
// task D31-4a): false only for the two D31 conversation markers, pure
// request/acknowledgement bookkeeping with no sentence worth showing (they
// exist only so PlanningConversation can compute its delivery watermark);
// every other known marker, and a body that matches none of the known
// updateMarker* prefixes -- not a marker this view recognizes -- both come
// back true, the same defensive fallback stateLine, escalationLine, and
// answerLine already use for a payload they cannot decode. Every true case
// renders as a one-line timeline divider (buildThreadRows), not the full
// card this function's callers used to feed into. A typed event row
// (EventKind set) renders through its kind's rule in events.go and is
// always shown.
func updateLine(m *store.MessageRow, agent string) (string, bool) {
	if m.EventKind != nil {
		return eventLine(m), true
	}
	body := m.Body
	switch {
	case strings.HasPrefix(body, updateMarkerConversationPendingPrefix):
		return "", false
	case strings.HasPrefix(body, updateMarkerConversationDeliveredPrefix):
		return "", false
	case strings.HasPrefix(body, updateMarkerGateConfirmedPrefix):
		return agent + " confirmed nothing is open.", true
	case strings.HasPrefix(body, updateMarkerGateApprovalCancelledPrefix):
		return gateApprovalCancelledLine(body, agent), true
	case strings.HasPrefix(body, updateMarkerSealRefusedPrefix):
		return sealRefusedLine(body), true
	case strings.HasPrefix(body, updateMarkerPlanreviewPrefix) && strings.HasSuffix(body, updateMarkerPlanreviewPendingSuffix):
		return "Plan review found only minor findings. Planning resumes automatically to address them.", true
	case strings.HasPrefix(body, updateMarkerPlanreviewPrefix) && strings.HasSuffix(body, updateMarkerPlanreviewDeliveredSuffix):
		return "Planning resumed with the review findings.", true
	case strings.HasPrefix(body, updateMarkerValidationPendingPrefix):
		return validationErrorsLine(body), true
	case strings.HasPrefix(body, updateMarkerValidationDeliveredPrefix):
		return "The agent received the check results.", true
	case strings.HasPrefix(body, updateMarkerResponseInvalidPrefix):
		return responseInvalidLine(body), true
	case strings.HasPrefix(body, updateMarkerSealMismatchPrefix):
		return "The scenario set changed before approval. Zing re-reads it on the next tick.", true
	case strings.HasPrefix(body, updateMarkerClaimsOkPrefix):
		return "Claims checked for run " + strings.TrimPrefix(body, updateMarkerClaimsOkPrefix) + ".", true
	case strings.HasPrefix(body, updateMarkerClaimErrorsPendingPrefix):
		return claimErrorsPendingLine(body), true
	case strings.HasPrefix(body, updateMarkerClaimErrorsDeliveredPrefix):
		return "Claim errors sent back to run " + strings.TrimPrefix(body, updateMarkerClaimErrorsDeliveredPrefix) + ".", true
	case strings.HasPrefix(body, updateMarkerCheckFailedPendingPrefix):
		return checkFailedPendingLine(body), true
	case strings.HasPrefix(body, updateMarkerCheckFailedDeliveredPrefix):
		return "Test and lint output sent back to run " + strings.TrimPrefix(body, updateMarkerCheckFailedDeliveredPrefix) + ".", true
	case strings.HasPrefix(body, updateMarkerPerimeterResolvedPrefix):
		return "Perimeter decided for run " + strings.TrimPrefix(body, updateMarkerPerimeterResolvedPrefix) + ".", true
	case body == updateMarkerRetryRequested:
		return "Retry requested.", true
	case strings.HasPrefix(body, updateMarkerPerimeterQuestionDroppedPrefix):
		return "Perimeter question dropped for run " + strings.TrimPrefix(body, updateMarkerPerimeterQuestionDroppedPrefix) + ".", true
	case isReviewMarker(body):
		if line, ok := reviewUpdateLine(body); ok {
			return line, true
		}
		return body, true
	case isJudgeMarker(body):
		if line, ok := judgeUpdateLine(body); ok {
			return line, true
		}
		return body, true
	case isShippingMarker(body):
		if line, ok := shippingUpdateLine(body); ok {
			return line, true
		}
		return body, true
	case isRespondMarker(body):
		if line, ok := respondUpdateLine(body); ok {
			return line, true
		}
		return body, true
	case isFixMarker(body):
		if line, ok := fixUpdateLine(body); ok {
			return line, true
		}
		return body, true
	default:
		return body, true
	}
}

// gateApprovalCancelledLine renders a "gate approval cancelled gate <QID>
// run <R>" or "gate approval cancelled gate <QID> batch <B>" marker (design
// section 22.12.1, 22.12.2 step 3, 22.12.3): the confirming turn itself
// cancels an approval by returning questions, ready, or error (" run <R>",
// internal/job/planning.go), and the owner cancels one by reopening a
// settled thread instead (" batch <B>", internal/store/console_writes.go).
// Both share the same prefix up to the question id, so the word right
// after it -- "run" or "batch" -- is what tells the two apart; a shape
// that matches neither (a future marker, or a malformed one) falls back to
// the agent's own wording, the safer of the two to guess wrong toward,
// since it names an actor instead of silently crediting the owner for
// something they did not do.
func gateApprovalCancelledLine(body, agent string) string {
	rest := strings.TrimPrefix(body, updateMarkerGateApprovalCancelledPrefix)
	if strings.Contains(rest, " batch ") {
		return "You reopened a thread; the approval is cancelled."
	}
	return agent + " found open questions; the approval is cancelled."
}

// sealRefusedLine renders a "seal refused gate <QID>\n<reason>" marker
// (design section 22.12.3a, dispatch.go's releaseAfterSealMismatch-style
// write): the reason is the seal invariant's own failure text (for
// example "answer 9 is not the approval of gate question 8"), kept as the
// marker's own second line rather than reparsed here, since dispatch.go
// already composed it from the exact *SealRefusedError the store returned.
func sealRefusedLine(body string) string {
	_, reason, _ := strings.Cut(body, "\n")
	return "Zing did not seal: " + reason + "."
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
// "judge round ", "judge coverage failed/delivered run ", "judge check ", or
// "judge host " prefixes (design section 5.1, 7.1, 7.2, 7.5-7.7; host
// scenario kind, #137): judgeUpdateLine's own exact-shape parse runs behind,
// the same two-step prefix-then-parse pattern isReviewMarker and
// reviewUpdateLine already use.
func isJudgeMarker(body string) bool {
	return strings.HasPrefix(body, updateMarkerJudgeRoundPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeCoverageFailedPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeCoverageDeliveredPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeCheckPrefix) ||
		strings.HasPrefix(body, updateMarkerJudgeHostPrefix)
}

// judgeUpdateLine renders one of judging.go's own marker shapes (design
// section 5.1) as an owner-facing sentence: the five "judge round <n> ..."
// shapes (started, verdicts, passed, failed, retry; judgeRoundLine),
// "judge coverage failed/delivered run <rid>" (7.2 step 5), "judge check
// <n> <scenario_id> exit <code>" (7.5 step 4; judgeCheckLine), and "judge
// host <n> <scenario_id> exit <code> cmd <hash>" (host scenario kind, #137;
// judgeHostCheckLine). ok is false when body's prefix matched but the rest
// of its shape did not, the same defensive fallback reviewUpdateLine's own
// default case uses.
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
	case strings.HasPrefix(first, updateMarkerJudgeHostPrefix):
		return judgeHostCheckLine(first)
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

// judgeHostCheckLine renders "judge host <n> <scenario_id> exit <code> cmd
// <hash>" (host scenario kind, #137) as its own sentence: the round number
// and the command's hash play no part in it, the same elision judgeCheckLine
// gives the round number.
func judgeHostCheckLine(first string) (string, bool) {
	fields := strings.Fields(strings.TrimPrefix(first, updateMarkerJudgeHostPrefix))
	isHostShape := len(fields) == 6 && fields[2] == "exit" && fields[4] == "cmd"
	if !isHostShape {
		return "", false
	}
	scenarioID, code := fields[1], fields[3]
	return "Host check for " + scenarioID + " exited " + code + ".", true
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
// ", "threads blocking ", or "merge asked/held/withdrawn/retry/refused "
// prefixes (design section 5.1, 8.2-8.9): shippingUpdateLine's own
// exact-shape parse runs behind, the same two-step pattern isReviewMarker
// uses.
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
		strings.HasPrefix(body, updateMarkerMergeRetryPrefix) ||
		strings.HasPrefix(body, updateMarkerMergeRefusedPrefix) ||
		strings.HasPrefix(body, updateMarkerPrMergedPrefix)
}

// shippingUpdateLine renders one of shipping.go's own marker shapes (design
// section 5.1) as an owner-facing sentence: "pr opened <number>" (8.2 step
// 6), "ci waiting <names>" (8.4), "reviewers re-requested <sha>" with its
// own logins line (9.4), "pr ready/draft <sha>" (8.5 rows 3 and 8, 8.9),
// "threads blocking <tids>" (8.5 row 6a), "merge asked/held/withdrawn
// <sha>", "merge retry <sha>" (the automatic retry after a "Base branch
// was modified" refusal), and "merge refused <sha>" with its own reason
// line (8.8), and "pr merged <sha>" (8.8). ok is false when body's prefix
// matched but the rest of its shape did not, the same defensive fallback
// reviewUpdateLine's own default case uses.
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
	case strings.HasPrefix(first, updateMarkerMergeRetryPrefix):
		return "Main moved during the merge; Zing checks the pull request again in 10 seconds.", true
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

// isFixMarker reports whether body carries job/fix.go's own "fix requested "
// or "fix landed " prefix: fixUpdateLine's own exact-shape parse runs
// behind, the same two-step prefix-then-parse pattern isReviewMarker uses.
func isFixMarker(body string) bool {
	return strings.HasPrefix(body, updateMarkerFixRequestedPrefix) || strings.HasPrefix(body, updateMarkerFixLandedPrefix)
}

// fixUpdateLine renders one of job/fix.go's own two marker shapes as an
// owner-facing sentence: "fix requested <kind> after run <R>" (opening a
// fix unit, its own text kept below the header) and "fix landed <mid> sha
// <sha>" (closing one). ok is false when body's prefix matched but the rest
// of its shape did not, the same defensive fallback reviewUpdateLine's own
// default case uses.
func fixUpdateLine(body string) (string, bool) {
	first, rest, hasRest := strings.Cut(body, "\n")
	switch {
	case strings.HasPrefix(first, updateMarkerFixRequestedPrefix):
		return fixRequestedLine(first, rest, hasRest)
	case strings.HasPrefix(first, updateMarkerFixLandedPrefix):
		return fixLandedLine(first)
	default:
		return "", false
	}
}

// fixRequestedLine renders "fix requested <kind> after run <R>"'s own first
// line (job/fix.go's fixRequestMessage): kind is read generically, rather
// than hardcoded to FixKind's four values, the same elision reviewRoundLine
// and judgeRoundLine already use for a round or run number. The marker's
// own text (lines 2..) is kept below the header, the same
// claimErrorsPendingLine pattern.
func fixRequestedLine(first, rest string, hasRest bool) (string, bool) {
	tail := strings.TrimPrefix(first, updateMarkerFixRequestedPrefix)
	kind, after, ok := strings.Cut(tail, " ")
	if !ok || !strings.HasPrefix(after, "after run ") {
		return "", false
	}
	runID := strings.TrimPrefix(after, "after run ")
	header := "Fix requested after run " + runID + " (" + kind + "):"
	if hasRest {
		return header + "\n" + rest, true
	}
	return header, true
}

// fixLandedLine renders "fix landed <mid> sha <sha>" (building.go's land
// and its adopted-commit twin): the request's own message id plays no part
// in the sentence, the same elision judgeRoundLine's "verdicts" case uses
// for the run id.
func fixLandedLine(first string) (string, bool) {
	fields := strings.Fields(strings.TrimPrefix(first, updateMarkerFixLandedPrefix))
	if len(fields) != 3 || fields[1] != "sha" {
		return "", false
	}
	return "Fix landed at " + sha7(fields[2]) + ".", true
}

// claimErrorsPendingLine renders a "claim errors pending run <rid>" body's
// first line as one owner-facing sentence, then the marker's own error
// lines unchanged (design section 9.2): unlike validationErrorsLine's own
// "Field <path>: <message>" rewrite, a claim error's own path (for example
// "claims/files_changed: observed [a.go], claimed [a.go, b.go]") is
// already the owner-facing shape CheckBuildClaims produces.
func claimErrorsPendingLine(body string) string {
	first, rest, hasRest := strings.Cut(body, "\n")
	rid := strings.TrimPrefix(first, updateMarkerClaimErrorsPendingPrefix)
	header := "Claim check failed for run " + rid + ":"
	if !hasRest {
		return header
	}
	return header + "\n" + rest
}

// checkFailedPendingLine renders a "check failed pending run <rid>" body's
// first line as one owner-facing sentence, then the failing commands'
// sections unchanged (#55), mirroring claimErrorsPendingLine.
func checkFailedPendingLine(body string) string {
	first, rest, hasRest := strings.Cut(body, "\n")
	rid := strings.TrimPrefix(first, updateMarkerCheckFailedPendingPrefix)
	header := "Test or lint failed for run " + rid + ":"
	if !hasRest {
		return header
	}
	return header + "\n" + rest
}

// responseInvalidLine renders a "response invalid run <rid>\n<reason>[\n
// <errors>]" marker (invalidMarkerBody, internal/job/planning.go): one
// sentence naming the run and the closed reason, then the validator's
// errors unchanged, one per line. A marker with no reason line keeps the
// reason clause out. It never claims a retry: invalidOutputCommit
// (internal/job/planning.go) escalates response_invalid instead of
// retrying once this is the second consecutive invalid run. It never
// asserts a link exists: recordRunEvidence (internal/job/runjob.go) only
// stores a final message when the run actually produced one, so an empty
// Codex -o file or empty Claude stdout leaves no link to point at. It is a
// pure renderer and logs nothing.
func responseInvalidLine(body string) string {
	first, rest, _ := strings.Cut(body, "\n")
	rid := strings.TrimPrefix(first, updateMarkerResponseInvalidPrefix)
	reason, errs, hasErrs := strings.Cut(rest, "\n")
	head := "Run " + rid + "'s response could not be used"
	if reason != "" {
		head += ": " + reason
	}
	head += ". If a final message was kept, it is linked under Runs in the side panel."
	if !hasErrs || errs == "" {
		return head
	}
	return head + "\n" + errs
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
	lines := []string{"The agent's last response did not pass Zing's checks. Zing is asking the agent to fix it."}
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
