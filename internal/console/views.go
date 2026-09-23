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
	return templates.Nav(projects, buildNavThreads(items)), nil
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
		threadRows, buildErr := buildThreadRows(&ticket, rows, plan)
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
	rendered, err := buildRenderedPlan(plan)
	if err != nil {
		return nil, fmt.Errorf("console: render plan artifact for ticket %d: %w", ticketID, err)
	}
	return &rendered, nil
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
// carries the merge kind's PR-link context, and plan the gate kind's
// (buildThreadQuestion); ticket may be nil only when the caller has no
// ticket at all (templates.Thread's own nil guard), never when rows is
// non-empty. plan is nil when the ticket has no stored plan artifact yet.
func buildThreadRows(ticket *store.Ticket, rows []store.MessageRow, plan *templates.RenderedPlan) ([]templates.ThreadRow, error) {
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
		question, err := buildThreadQuestion(ticket, &rows[i], messageCounts[rows[i].ID]+1, plan)
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
// resolved row that names it as a parent (buildThreadRows). plan is the
// gate kind's context (design section 6.9), set on q only when payload.Kind
// is gate. An unparseable payload falls back to nil (renders as a plain
// row) rather than failing the whole thread render, since the commit that
// wrote it already validated it against the messages/question schema; a
// markdown render failure, by contrast, is a real error (design section
// 6.10: Render can fail), and is returned rather than silently dropping the
// question's body.
func buildThreadQuestion(ticket *store.Ticket, m *store.MessageRow, messageCount int, plan *templates.RenderedPlan) (*templates.ThreadQuestion, error) {
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
	default:
		return m.Body
	}
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
