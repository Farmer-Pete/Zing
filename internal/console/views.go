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
		return templates.Feed(messages), nil
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
		return templates.Thread(&ticket, buildThreadRows(rows)), nil
	case errors.Is(err, sql.ErrNoRows):
		return templates.Thread(nil, nil), nil
	default:
		return nil, err
	}
}

// msgTypeState, msgTypeQuestion, and msgTypeEscalation name the message
// types the read-only Thread view renders specially (design section 6.6): a
// state message as a centered separator, a question message as a read-only
// group, and an escalation message (whose Body a commit may leave empty,
// like state) decoded into one line. Every other type (update, answer,
// followup, resolved, reply) is a plain row using its own Body.
const (
	msgTypeState      = "state"
	msgTypeQuestion   = "question"
	msgTypeEscalation = "escalation"
)

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

// buildThreadRows turns store rows into the read-only Thread view's rows
// (design section 6.6, Task 3 scope): a question message becomes a
// read-only group (title, body, recommendation, options), never the
// interactive chips or composer Tasks 6 and 7 add.
func buildThreadRows(rows []store.MessageRow) []templates.ThreadRow {
	out := make([]templates.ThreadRow, 0, len(rows))
	for i := range rows {
		out = append(out, templates.ThreadRow{
			ID: rows[i].ID, Type: rows[i].Type, Author: rows[i].Author,
			Body:     displayBody(&rows[i]),
			Question: buildThreadQuestion(&rows[i]),
		})
	}
	return out
}

// buildThreadQuestion returns the read-only detail a "question" message
// renders instead of its plain Body, or nil for every other type. An
// unparseable payload falls back to the plain Body rather than failing the
// whole thread render, since the commit that wrote it already validated it
// against the messages/question schema.
func buildThreadQuestion(m *store.MessageRow) *templates.ThreadQuestion {
	if m.Type != msgTypeQuestion {
		return nil
	}
	var payload response.QuestionPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil
	}
	title, body := splitQuestionBody(m.Body)
	options := make([]templates.ThreadOption, 0, len(payload.Options))
	for _, o := range payload.Options {
		options = append(options, templates.ThreadOption{Key: o.Key, Text: o.Text})
	}
	return &templates.ThreadQuestion{
		Title: title, Body: body, Recommended: payload.Recommended,
		Options: options, StateLabel: questionStateLabel(m.State),
	}
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
// leaves empty, the transition or the report living in Payload instead),
// or the row's own Body for every other type.
func displayBody(m *store.MessageRow) string {
	switch m.Type {
	case msgTypeState:
		return stateLine(m)
	case msgTypeEscalation:
		return escalationLine(m)
	default:
		return m.Body
	}
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
