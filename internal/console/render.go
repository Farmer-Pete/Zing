package console

import (
	"bytes"
	"encoding/json"
	"fmt"

	"zing/internal/response"
	"zing/internal/store"
)

// msgTypeState is the message type a checkpoint transition writes (design
// section 7.1); its Body is empty and its from/to lives in the payload, so
// rendering it needs one extra decode step every other message type skips.
const msgTypeState = "state"

// messageView is what the thread fragment renders for one message row: the
// type and author as stored, and a display Body that is either the row's
// own Body or, for a state message, the decoded "from -> to (reason)" line.
type messageView struct {
	ID     int64
	Type   string
	Author string
	Body   string
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
// renders, decoding a state message's payload into its "from -> to" line.
func buildMessageViews(rows []store.MessageRow) []messageView {
	views := make([]messageView, 0, len(rows))
	for i := range rows {
		views = append(views, messageView{ID: rows[i].ID, Type: rows[i].Type, Author: rows[i].Author, Body: displayBody(&rows[i])})
	}
	return views
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
