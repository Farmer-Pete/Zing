// events.go renders typed event rows (#34 first step): one rule per
// event_kind, keyed the same way store.EventKinds names a schema, instead
// of updateLine's hand-written body prefixes.
package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"zing/internal/response"
	"zing/internal/store"
)

// eventRule renders one event kind's payload as the owner-facing sentence.
type eventRule func(payload json.RawMessage) (string, error)

// eventRules has one rule per event kind, keyed by messages.event_kind.
// TestEveryEventKindHasARenderRule fails when this disagrees with
// store.EventKinds().
var eventRules = map[string]eventRule{
	store.EventKindCheckRerun: checkRerunLine,
	store.EventKindOwnerEdit:  ownerEditLine,
}

// eventLine renders an event row through its kind's rule. If the kind has
// no rule (a row from a newer binary), it names the kind. If the payload
// won't decode, it says so and logs the ids.
func eventLine(m *store.MessageRow) string {
	kind := *m.EventKind
	rule, ok := eventRules[kind]
	if !ok {
		return "Unrecognized event " + kind + "."
	}
	line, err := rule(m.Payload)
	if err != nil {
		attrs := []any{"message_id", m.ID, "ticket_id", m.TicketID, "kind", kind, "err", err}
		if m.RunID != nil {
			attrs = append(attrs, "run_id", *m.RunID)
		}
		slog.Warn("console: event payload unreadable", attrs...)
		return "Unreadable " + kind + " event."
	}
	return line
}

// checkRerunLine renders a check_rerun event (store.EventKindCheckRerun):
// one re-run of a check on a sha.
func checkRerunLine(payload json.RawMessage) (string, error) {
	var e response.CheckRerunEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("decode check_rerun: %w", err)
	}
	if e.Check == "" || e.SHA == "" {
		return "", errors.New("decode check_rerun: missing check or sha")
	}
	return "Zing re-ran the " + string(e.Check) + " check on " + sha7(e.SHA) + ".", nil
}

// ownerEditLine renders an owner_edit event (store.EventKindOwnerEdit).
func ownerEditLine(payload json.RawMessage) (string, error) {
	var e response.OwnerEditEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("decode owner_edit: %w", err)
	}
	return response.OwnerEditLine(e), nil
}
