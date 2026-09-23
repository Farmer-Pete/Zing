package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"zing/internal/response"
	"zing/internal/store"
)

// msgTypeState is the message type a checkpoint transition writes (design
// section 7.1); its Body is empty and its from/to lives in the payload, so
// rendering it needs one extra decode step every other message type skips.
const msgTypeState = "state"

// msgTypeQuestion and questionStateOpen are the message type and lifecycle
// state a question block renders for (design section 6.9). messages.state is
// the canonical question lifecycle value (section 8); commit.go's
// package-private constants of the same name live in internal/store and are
// not visible here.
const (
	msgTypeQuestion   = "question"
	questionStateOpen = "open"
)

// messageOption is one chip a question block renders: the option key
// POST /answer's $answer signal carries and the button's label text.
type messageOption struct {
	Key, Text string
}

// questionView is the extra data an open question message renders instead
// of its plain Body: the heading and body text split from the stored Body
// (design section 6.6's Title-then-Body mapping) and the chips built from
// the stored QuestionPayload's options.
type questionView struct {
	Title, Body string
	Options     []messageOption
}

// messageView is what the thread fragment renders for one message row: the
// type and author as stored, and a display Body that is either the row's
// own Body or, for a state message, the decoded "from -> to (reason)" line.
// Question is non-nil only for a message whose lifecycle state is open, and
// the template renders its question block in place of Body for that row.
type messageView struct {
	ID       int64
	Type     string
	Author   string
	Body     string
	Question *questionView
}

// shellData is the template data for the "shell" template (templates/shell.gohtml).
type shellData struct {
	Tickets []store.Ticket
}

// threadData is the template data for the "threadFragment" template
// (templates/thread.gohtml). Ticket is nil when the requested id does not
// exist.
type threadData struct {
	Ticket   *store.Ticket
	Messages []messageView
}

// buildMessageViews turns store rows into the view the thread template
// renders, decoding a state message's payload into its "from -> to" line and
// an open question message's payload into its chips.
func buildMessageViews(rows []store.MessageRow) []messageView {
	views := make([]messageView, 0, len(rows))
	for i := range rows {
		views = append(views, messageView{
			ID: rows[i].ID, Type: rows[i].Type, Author: rows[i].Author,
			Body:     displayBody(&rows[i]),
			Question: buildQuestionView(&rows[i]),
		})
	}
	return views
}

// buildQuestionView returns the question block data for a message whose
// type is "question" and whose lifecycle state is open, or nil for every
// other message: an answered or resolved question falls back to its plain
// Body (design section 6.9). An unparseable payload also falls back to the
// plain Body rather than failing the whole thread render, since the
// commit that wrote it already validated it against the messages/question
// schema (section 6.6).
func buildQuestionView(m *store.MessageRow) *questionView {
	if m.Type != msgTypeQuestion || m.State == nil || *m.State != questionStateOpen {
		return nil
	}

	var payload response.QuestionPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil
	}

	title, body := splitQuestionBody(m.Body)
	options := make([]messageOption, 0, len(payload.Options))
	for _, o := range payload.Options {
		options = append(options, messageOption{Key: o.Key, Text: o.Text})
	}
	return &questionView{Title: title, Body: body, Options: options}
}

// splitQuestionBody splits a question message's Body into its heading (the
// first line) and the text after it, matching how a planning commit writes
// Title and Body together: title first as the heading, then a blank line,
// then the body (design section 6.6).
func splitQuestionBody(raw string) (title, body string) {
	title, rest, _ := strings.Cut(raw, "\n")
	return title, strings.TrimPrefix(rest, "\n")
}

// displayBody returns what the thread renders for one message: the row's
// own Body for every type except "state", whose Body is always empty
// (commit.go writes the transition into the payload, not the body).
func displayBody(m *store.MessageRow) string {
	if m.Type != msgTypeState || len(m.Payload) == 0 {
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

// renderShell renders the full "/" page, tickets included.
func renderShell(tickets []store.Ticket) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "shell", shellData{Tickets: tickets}); err != nil {
		return "", fmt.Errorf("console: render shell: %w", err)
	}
	return buf.String(), nil
}

// renderTicketsFragment renders the #tickets element alone, the payload
// GET /updates patches on connect and on every bus signal.
func renderTicketsFragment(tickets []store.Ticket) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "ticketsFragment", tickets); err != nil {
		return "", fmt.Errorf("console: render tickets fragment: %w", err)
	}
	return buf.String(), nil
}

// renderThreadFragment renders the #thread element alone, the payload
// GET /thread patches on connect and on every bus signal. ticket is nil when
// the requested id does not exist.
func renderThreadFragment(ticket *store.Ticket, messages []messageView) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "threadFragment", threadData{Ticket: ticket, Messages: messages}); err != nil {
		return "", fmt.Errorf("console: render thread fragment: %w", err)
	}
	return buf.String(), nil
}
