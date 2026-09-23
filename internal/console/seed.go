// seed.go: the minimal per-kind fixture builder (design section 6.15).
// SeedQuestionFixtures inserts one open question of each of the six kinds
// on a ticket, through the store's validated inserts, so a seeded row is a
// row a real producer could have written. Task 6's own question-kinds test
// uses it directly; Task 12's SeedDemo (not built here) reuses it for the
// demo project and ticket.
package console

import (
	"context"
	"encoding/json"
	"fmt"

	"zing/internal/response"
	"zing/internal/store"
)

// seedQuestionKinds is the design section 8 closed set of question kinds,
// in the order SeedQuestionFixtures inserts them; each kind's fixture key is
// "Q" plus its 1-based index here ("Q1".."Q6").
var seedQuestionKinds = []response.QuestionKind{
	response.QuestionKindQuestion,
	response.QuestionKindGate,
	response.QuestionKindSplit,
	response.QuestionKindMerge,
	response.QuestionKindPerimeter,
	response.QuestionKindReview,
}

// msgStateOpen is the messages.state (and QuestionPayload.State) value
// every seeded question carries: SeedQuestionFixtures seeds only open
// questions, matching "inserts ... one open question of each of the six
// kinds" (design section 6.15).
const msgStateOpen = "open"

// SeedQuestionFixtures inserts one open question of each of the six kinds
// (design section 8) on ticketID, each with just enough payload to render
// and compose: an option kind (question, gate, split, merge) gets two
// options and a recommended option key; an item kind (perimeter, review)
// gets two or three items, each with a ref and text (design section 6.15).
// Every insert goes through store.InsertMessage, which validates the
// payload against the messages/question schema, the same validation a real
// planning, gate, split, perimeter, review, or merge producer's commit
// would satisfy.
//
// It is idempotent: it first lists ticketID's existing question messages
// and skips any fixture key ("Q1".."Q6", one per kind) already present, so
// calling it twice on the same ticket inserts nothing the second time.
func SeedQuestionFixtures(ctx context.Context, s *store.Store, ticketID int64) error {
	existing, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("seed question fixtures: list messages: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for i := range existing {
		if existing[i].Type != msgTypeQuestion || len(existing[i].Payload) == 0 {
			continue
		}
		var p response.QuestionPayload
		if json.Unmarshal(existing[i].Payload, &p) == nil {
			have[p.Key] = true
		}
	}

	for i, kind := range seedQuestionKinds {
		key := fmt.Sprintf("Q%d", i+1)
		if have[key] {
			continue
		}
		if err := seedOneQuestion(ctx, s, ticketID, key, kind); err != nil {
			return fmt.Errorf("seed question fixtures: %s %s: %w", key, kind, err)
		}
	}
	return nil
}

// seedOneQuestion inserts one open "question" message of kind on ticketID,
// through store.InsertMessage (design section 6.15: "through the store's
// validated inserts").
func seedOneQuestion(ctx context.Context, s *store.Store, ticketID int64, key string, kind response.QuestionKind) error {
	title, body := seedQuestionText(kind)
	payload, err := seedQuestionPayload(key, kind)
	if err != nil {
		return err
	}
	openState := msgStateOpen
	_, err = s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, Type: msgTypeQuestion, Author: "zing",
		State:   &openState,
		Body:    title + "\n\n" + body,
		Payload: payload,
	})
	return err
}

// seedQuestionText returns the title (the <details> summary line) and body
// (the question's prose) fixture text for kind: the shape a planning commit
// writes together in one Body field, title first, then a blank line, then
// the body (splitQuestionBody, views.go).
func seedQuestionText(kind response.QuestionKind) (title, body string) {
	switch kind {
	case response.QuestionKindQuestion:
		return "How should the greeting read?", "Pick the greeting style for GET /hello."
	case response.QuestionKindGate:
		return "Approve the plan?", "Review the plan, scenarios, and decisions, then approve or reject it."
	case response.QuestionKindSplit:
		return "Split this ticket?", "The ticket looks large enough to split into independently buildable children."
	case response.QuestionKindMerge:
		return "Merge the PR?", "The build passed review and judging; approve the merge or hold it."
	case response.QuestionKindPerimeter:
		return "Confirm the file perimeter", "These are the files the plan expects to touch."
	case response.QuestionKindReview:
		return "Triage the review findings", "Decide each finding before the build continues."
	default:
		return "", ""
	}
}

// seedQuestionPayload builds and marshals the QuestionPayload fixture for
// kind (design section 6.15): an option kind gets two options and a
// recommended option key; an item kind gets two or three items, each with a
// ref and text, and an empty (not nil) Options slice, since the
// messages/question schema requires the "options" property present even
// when it is empty.
func seedQuestionPayload(key string, kind response.QuestionKind) (json.RawMessage, error) {
	payload := response.QuestionPayload{
		Key: key, Kind: kind, State: response.QuestionStateOpen,
		Options: []response.Option{},
	}

	switch kind {
	case response.QuestionKindQuestion:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Plain hello"},
			{Key: "b", Text: "hello, world"},
		}
	case response.QuestionKindGate:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Approve the plan"},
			{Key: "b", Text: "Reject the plan"},
		}
	case response.QuestionKindSplit:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Split into the proposed children"},
			{Key: "b", Text: "Keep this as one ticket"},
		}
	case response.QuestionKindMerge:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Merge the PR"},
			{Key: "b", Text: "Hold the PR"},
		}
	case response.QuestionKindPerimeter:
		payload.Recommended = "Accept every file in the perimeter"
		payload.Items = []response.Item{
			{Ref: "internal/hello/handler.go", Text: "new HTTP handler for GET /hello"},
			{Ref: "internal/hello/handler_test.go", Text: "test for the new handler"},
			{Ref: "cmd/zing/main.go", Text: "wires the new route"},
		}
	case response.QuestionKindReview:
		payload.Recommended = "Accept every finding"
		payload.Items = []response.Item{
			{Ref: "F1", Text: "missing error wrap on the store call"},
			{Ref: "F2", Text: "handler does not close the response body"},
		}
	default:
		return nil, fmt.Errorf("seed question fixtures: unknown question kind %q", kind)
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("seed question fixtures: marshal %s payload: %w", kind, err)
	}
	return b, nil
}
