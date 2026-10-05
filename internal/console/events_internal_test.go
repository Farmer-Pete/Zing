// events_internal_test.go is a whitebox test for eventRules and eventLine
// (events.go), the same rail_internal_test.go precedent as
// views_internal_test.go: eventLine's fallbacks are cleanest proved
// directly against a synthetic store.MessageRow.
package console

import (
	"testing"

	"zing/internal/store"
)

// TestEveryEventKindHasARenderRule proves eventRules agrees with
// store.EventKinds() exactly: every schema has a rule, and every rule has
// a schema. This is the test the ticket asked for -- "a kind with no
// render rule fails a test".
func TestEveryEventKindHasARenderRule(t *testing.T) {
	t.Parallel()
	kinds := store.EventKinds()
	if len(kinds) == 0 {
		t.Fatal("store.EventKinds() returned no kinds")
	}
	known := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		known[kind] = true
		if _, ok := eventRules[kind]; !ok {
			t.Errorf("event kind %s has no render rule in internal/console/events.go", kind)
		}
	}
	for kind := range eventRules {
		if !known[kind] {
			t.Errorf("render rule %s has no schema in internal/store/schemas/events", kind)
		}
	}
}

// TestEventLineFallbacks proves eventLine's two defensive fallbacks -- an
// unknown kind, and a known kind whose payload won't decode -- alongside
// the checkRerunLine rule's own happy path, and that updateLine always
// shows an event row.
func TestEventLineFallbacks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind string
		body string
		want string
	}{
		{"unknown kind", "not_a_kind", "{}", "Unrecognized event not_a_kind."},
		{"undecodable payload", store.EventKindCheckRerun, "{}", "Unreadable check_rerun event."},
		{
			"check_rerun happy path", store.EventKindCheckRerun,
			`{"check":"Hooks and tests","sha":"cccccccccccccccccccccccccccccccccccccccc","run_id":37145043448,"check_run_id":5001,"reason":"flaky"}`,
			"Zing re-ran the Hooks and tests check on ccccccc (workflow run 37145043448) because it failed.",
		},
		{
			"check_rerun_passed happy path", store.EventKindCheckRerunPassed,
			`{"check":"Hooks and tests","sha":"cccccccccccccccccccccccccccccccccccccccc","tests":["TestA"]}`,
			"Hooks and tests failed once and passed on re-run (likely flaky): TestA.",
		},
		{
			"owner_edit plan_task drop happy path", store.EventKindOwnerEdit,
			`{"target":"plan_task","ref":"2","action":"drop","old":"old plan","new":"new plan"}`,
			"Owner dropped plan task 2; later tasks moved up one.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind := tc.kind
			row := store.MessageRow{Message: store.Message{ //nolint:modernize // keyed on purpose
				Type: msgTypeUpdate, Author: authorSystem, EventKind: &kind, Payload: []byte(tc.body),
			}}
			if got := eventLine(&row); got != tc.want {
				t.Errorf("eventLine(%+v) = %q, want %q", row, got, tc.want)
			}
			line, shown := updateLine(&row, agentFallback)
			if !shown {
				t.Errorf("updateLine(%+v) shown = false, want true", row)
			}
			if line != tc.want {
				t.Errorf("updateLine(%+v) = %q, want %q", row, line, tc.want)
			}
		})
	}
}
