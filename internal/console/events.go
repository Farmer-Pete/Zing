// events.go renders typed event rows (#34 first step): one rule per
// event_kind, keyed the same way store.EventKinds names a schema, instead
// of updateLine's hand-written body prefixes.
package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/response"
	"zing/internal/store"
)

// eventRule renders one event kind's payload as the owner-facing sentence.
type eventRule func(payload json.RawMessage) (string, error)

// eventRules has one rule per event kind, keyed by messages.event_kind.
// TestEveryEventKindHasARenderRule fails when this disagrees with
// store.EventKinds().
var eventRules = map[string]eventRule{
	store.EventKindCheckRerun:       checkRerunLine,
	store.EventKindCheckRerunPassed: checkRerunPassedLine,
	store.EventKindOwnerEdit:        ownerEditLine,
	store.EventKindStaleBase:        staleBaseLine,
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

// checkRerunWhy explains each RerunReason in checkRerunLine's sentence.
var checkRerunWhy = map[response.RerunReason]string{
	response.RerunReasonFlaky: "it failed",
	response.RerunReasonInfra: "it was cancelled or never started",
	response.RerunReasonNoLog: "its log could not be read",
}

// checkRerunLine renders a check_rerun event (store.EventKindCheckRerun):
// one re-run of a GitHub check run on a sha, naming the workflow run and
// why it was re-run.
func checkRerunLine(payload json.RawMessage) (string, error) {
	var e response.CheckRerunEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("decode check_rerun: %w", err)
	}
	why, knownReason := checkRerunWhy[e.Reason]
	complete := e.Check != "" && e.SHA != "" && e.RunID != 0
	if !complete || !knownReason {
		return "", errors.New("decode check_rerun: missing check, sha, run_id, or reason")
	}
	return fmt.Sprintf("Zing re-ran the %s check on %s (workflow run %d) because %s.", e.Check, sha7(e.SHA), e.RunID, why), nil
}

// checkRerunPassedLine renders a check_rerun_passed event
// (store.EventKindCheckRerunPassed): a check that failed once and passed
// on re-run, likely flaky.
func checkRerunPassedLine(payload json.RawMessage) (string, error) {
	var e response.CheckRerunPassedEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("decode check_rerun_passed: %w", err)
	}
	if e.Check == "" || e.SHA == "" {
		return "", errors.New("decode check_rerun_passed: missing check or sha")
	}
	if len(e.Tests) == 0 {
		return e.Check + " failed once and passed on re-run (likely flaky).", nil
	}
	return e.Check + " failed once and passed on re-run (likely flaky): " + strings.Join(e.Tests, ", ") + ".", nil
}

// ownerEditLine renders an owner_edit event (store.EventKindOwnerEdit).
func ownerEditLine(payload json.RawMessage) (string, error) {
	var e response.OwnerEditEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("decode owner_edit: %w", err)
	}
	return response.OwnerEditLine(e), nil
}

// staleBaseLine renders a stale_base event (store.EventKindStaleBase): a
// step that used the last fetched base because the fetch failed.
func staleBaseLine(payload json.RawMessage) (string, error) {
	var e response.StaleBaseEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("decode stale_base: %w", err)
	}
	return response.StaleBaseLine(e), nil
}
