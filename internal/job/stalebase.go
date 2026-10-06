// stalebase.go lets the four ticket states that fetch a base (building,
// reviewing, judging, shipping) tell the owner on the ticket when a fetch
// fell back to the last fetched base, once per step and sha (#68
// follow-up).
package job

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"unicode/utf8"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// staleBaseSHA is the stale_base schema's own sha pattern.
var staleBaseSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The two skip causes staleBaseSkip returns.
const (
	staleBaseAlreadyNoted   = "already_noted"
	staleBasePayloadInvalid = "payload_invalid"
)

// withStaleBaseNote runs one tick of a handler and, when a base fetch in
// it fell back to the last fetched base, adds one stale_base event to the
// tick's commit: once per step and sha (#68 follow-up). A tick that
// errs, or a note that can't be built, leaves the commit unchanged.
func withStaleBaseNote(ctx context.Context, t store.Ticket, d Deps, step string, run func(context.Context, store.Ticket, Deps) (store.HandlerCommit, error)) (store.HandlerCommit, error) {
	proj, ok := d.Projects[t.ProjectID]
	if !ok || proj.Orch == nil {
		return run(ctx, t, d)
	}
	proj.Orch.TakeBaseFallback(t.ID) // an earlier fallback belongs to no step here
	c, err := run(ctx, t, d)
	f, stale := proj.Orch.TakeBaseFallback(t.ID)
	if !stale || err != nil || c.TicketID != t.ID {
		return c, err
	}
	skip := func(cause string, skipErr error) (store.HandlerCommit, error) {
		slog.Warn("stale base note skipped", "ticket_id", t.ID, "step", step, "sha", f.SHA, "reason", f.Reason, "skip", cause, "err", skipErr)
		return c, nil
	}
	rows, evErr := d.Store.Events(ctx, t.ID, store.EventKindStaleBase, store.EventFilter{SHA: f.SHA})
	if evErr != nil {
		return skip("events_read_failed", evErr)
	}
	switch cause := staleBaseSkip(f, rows, step); cause {
	case "":
	case staleBaseAlreadyNoted:
		slog.Info("stale base already noted", "ticket_id", t.ID, "step", step, "sha", f.SHA)
		return c, nil
	default:
		return skip(cause, nil)
	}
	msg, msgErr := store.NewEvent(t.ID, store.EventKindStaleBase, response.StaleBaseEvent{Step: step, Branch: f.Branch, SHA: f.SHA, Reason: f.Reason})
	if msgErr != nil {
		return skip("event_build_failed", msgErr)
	}
	c.Messages = append(c.Messages, msg)
	slog.Info("stale base noted", "ticket_id", t.ID, "step", step, "sha", f.SHA, "reason", f.Reason)
	return c, nil
}

// staleBaseSkip says why f should not become a stale_base event for
// step, or "" when it should. rows are the ticket's stale_base events
// for f.SHA. payload_invalid catches what the schema would reject at
// commit time (a SHA-256 sha, a branch over 255 characters), so the note
// is passed over instead of failing the tick. A row whose payload won't
// decode is passed over.
func staleBaseSkip(f orchestrator.BaseFallback, rows []store.MessageRow, step string) string {
	if !staleBaseSHA.MatchString(f.SHA) || f.Branch == "" || utf8.RuneCountInString(f.Branch) > 255 {
		return staleBasePayloadInvalid
	}
	for i := range rows {
		var ev response.StaleBaseEvent
		if json.Unmarshal(rows[i].Payload, &ev) != nil {
			continue
		}
		if ev.Step == step {
			return staleBaseAlreadyNoted
		}
	}
	return ""
}
