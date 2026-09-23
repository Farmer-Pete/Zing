package console

import (
	"encoding/json"
	"strings"

	"zing/internal/console/templates"
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

// buildMessageViews turns store rows into the view the thread template
// renders, decoding a state message's payload into its "from -> to" line and
// an open question message's payload into its chips.
func buildMessageViews(rows []store.MessageRow) []templates.MessageView {
	views := make([]templates.MessageView, 0, len(rows))
	for i := range rows {
		views = append(views, templates.MessageView{
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
func buildQuestionView(m *store.MessageRow) *templates.QuestionView {
	if m.Type != msgTypeQuestion || m.State == nil || *m.State != questionStateOpen {
		return nil
	}

	var payload response.QuestionPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil
	}

	title, body := splitQuestionBody(m.Body)
	options := make([]templates.MessageOption, 0, len(payload.Options))
	for _, o := range payload.Options {
		options = append(options, templates.MessageOption{Key: o.Key, Text: o.Text})
	}
	return &templates.QuestionView{Title: title, Body: body, Options: options}
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
