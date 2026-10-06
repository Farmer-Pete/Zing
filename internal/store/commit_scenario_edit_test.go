package store

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"zing/internal/response"
)

// amendedCheck and amendedReason are the amended check and the judge's
// reason this file's ScenarioEdit fixtures repeat (goconst).
const (
	amendedCheck  = "go test ./amended"
	amendedReason = "the check bans files the then does not mention"
)

// scenarioSealedAt reads back ticketID's scenario id's sealed_at column, so
// TestCommitHandlerResultScenarioEdit can prove the ScenarioEdit step leaves
// it unchanged.
func scenarioSealedAt(t *testing.T, s *Store, ticketID int64, id string) string {
	t.Helper()
	var sealedAt string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT sealed_at FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND json_extract(payload, '$.id') = ?`,
		ticketID, id,
	).Scan(&sealedAt); err != nil {
		t.Fatalf("read sealed_at for scenario %s: %v", id, err)
	}
	return sealedAt
}

// TestCommitHandlerResultScenarioEdit proves HandlerCommit.ScenarioEdit's
// inline step (#57): a claimed ticket's commit updates the sealed scenario
// and writes one owner_edit event carrying the judge's reason, a commit
// whose lease has already been lost applies nothing, and a ScenarioEdit
// naming a scenario the ticket doesn't have fails the whole commit with
// nothing landed.
func TestCommitHandlerResultScenarioEdit(t *testing.T) {
	t.Parallel()

	t.Run("updates the scenario and writes one reasoned event", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID)
		sealedBefore := scenarioSealedAt(t, s, ticketID, "s1")

		owner, expires := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			ScenarioEdit: &ScenarioEdit{
				Ref: "s1", Kind: response.ScenarioKindNegative,
				Given: "a new given", When: "a new when", Then: "a new then",
				Check: amendedCheck, Reason: amendedReason,
			},
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if !applied {
			t.Fatal("applied = false, want true")
		}

		var sc response.Scenario
		if err := json.Unmarshal(readScenarioPayload(t, s, ticketID, "s1"), &sc); err != nil {
			t.Fatalf("unmarshal scenario s1: %v", err)
		}
		if sc.Kind != response.ScenarioKindNegative || sc.Given != "a new given" || sc.When != "a new when" ||
			sc.Then != "a new then" || sc.Check != amendedCheck {
			t.Errorf("scenario s1 = %+v, want the amended fields", sc)
		}
		if got := scenarioSealedAt(t, s, ticketID, "s1"); got != sealedBefore {
			t.Errorf("sealed_at = %q, want unchanged %q", got, sealedBefore)
		}

		events := ownerEditEvents(t, s, ticketID)
		if len(events) != 1 {
			t.Fatalf("owner_edit events = %d, want 1", len(events))
		}
		var ev response.OwnerEditEvent
		if err := json.Unmarshal(events[0].Payload, &ev); err != nil {
			t.Fatalf("unmarshal owner_edit event: %v", err)
		}
		if ev.Reason != amendedReason {
			t.Errorf("event reason = %q, want the judge's reason", ev.Reason)
		}
		if events[0].Body != response.OwnerEditLine(ev) {
			t.Errorf("event body = %q, want %q", events[0].Body, response.OwnerEditLine(ev))
		}
	})

	t.Run("a lost lease applies nothing", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID)
		before := readScenarioPayload(t, s, ticketID, "s1")

		owner, expires := claimForCommit(t, s, ticketID)
		// Move the lease's expiry out from under the commit below, directly
		// (not through Claim, which refuses to extend an already-claimed
		// ticket): the same race the fenced ticket UPDATE guards against.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tickets SET claim_expires_at = ? WHERE id = ?`,
			formatTime(expires.Add(time.Hour)), ticketID,
		); err != nil {
			t.Fatalf("move claim expiry: %v", err)
		}

		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			ScenarioEdit: &ScenarioEdit{
				Ref: "s1", Kind: response.ScenarioKindBehavior,
				Given: "g", When: "w", Then: "t", Check: "go test ./lost", Reason: "r",
			},
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if applied {
			t.Fatal("applied = true, want false (the lease had already moved)")
		}
		if after := readScenarioPayload(t, s, ticketID, "s1"); !bytes.Equal(after, before) {
			t.Errorf("payload = %s, want unchanged %s", after, before)
		}
		if n, countErr := s.CountEvents(ctx, ticketID, EventKindOwnerEdit, EventFilter{}); countErr != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0", n, countErr)
		}
	})

	t.Run("a missing scenario fails the whole commit", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		seedSealedScenario(t, s, ticketID)

		owner, expires := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			ScenarioEdit: &ScenarioEdit{
				Ref: "s9", Kind: response.ScenarioKindBehavior,
				Given: "g", When: "w", Then: "t", Check: "go test ./missing", Reason: "r",
			},
		})
		if err == nil {
			t.Fatal("CommitHandlerResult(missing scenario s9): err = nil, want a not-found error")
		}
		if applied {
			t.Error("applied = true, want false")
		}
		if n, countErr := s.CountEvents(ctx, ticketID, EventKindOwnerEdit, EventFilter{}); countErr != nil || n != 0 {
			t.Errorf("owner_edit events = %d (err %v), want 0 (the commit rolled back)", n, countErr)
		}
		// The claim itself is untouched: the fenced ticket UPDATE never ran.
		ticket, err := s.GetTicket(ctx, ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		if ticket.ClaimOwner == nil || *ticket.ClaimOwner != owner {
			t.Errorf("claim_owner = %v, want still %q", ticket.ClaimOwner, owner)
		}
	})
}

// TestEscalateTxAmendedOptions proves escalateTx's amended branch (#57):
// an escalation whose payload carries an amendment offers "Accept the
// amended check", "Edit it" and "Abandon", recommends "a", and copies the
// amendment into the question payload; without one, the post-seal options
// are Retry and Abandon exactly as before.
func TestEscalateTxAmendedOptions(t *testing.T) {
	t.Parallel()

	t.Run("amended", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStateJudging)

		amendment := &response.Amendment{
			Scenario: "s1", Kind: response.ScenarioKindNegative,
			Given: "g2", When: "w2", Then: "t2", Check: amendedCheck,
			Reason: amendedReason,
		}
		payload := escalationTestPayload(response.EscalationCodeCannotRun, response.EscalationOriginJudge)
		payload.Amendment = amendment

		owner, expires := claimForCommit(t, s, ticketID)
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{Body: "cannot_run: s1's check is wrong as written", Payload: payload},
		})
		if err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}
		if !applied {
			t.Fatal("applied = false, want true")
		}

		qp := latestQuestionPayload(t, s, ticketID)
		wantOptions := []response.Option{
			{Key: "a", Text: escalationOptionAcceptAmendment},
			{Key: "b", Text: escalationOptionEditAmendment},
			{Key: "c", Text: escalationOptionAbandon},
		}
		if len(qp.Options) != len(wantOptions) {
			t.Fatalf("options = %+v, want %+v", qp.Options, wantOptions)
		}
		for i := range wantOptions {
			if qp.Options[i] != wantOptions[i] {
				t.Errorf("options[%d] = %+v, want %+v", i, qp.Options[i], wantOptions[i])
			}
		}
		if qp.Recommended != "a" {
			t.Errorf("recommended = %q, want %q", qp.Recommended, "a")
		}
		if qp.Amendment == nil || *qp.Amendment != *amendment {
			t.Errorf("question amendment = %+v, want %+v", qp.Amendment, amendment)
		}
	})

	t.Run("no amendment keeps the post-seal options", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		setTicketState(t, s, ticketID, testStateJudging)

		owner, expires := claimForCommit(t, s, ticketID)
		if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Escalation: &EscalationCommit{
				Body:    "cannot_run: plain",
				Payload: escalationTestPayload(response.EscalationCodeCannotRun, response.EscalationOriginJudge),
			},
		}); err != nil {
			t.Fatalf("CommitHandlerResult: %v", err)
		}

		qp := latestQuestionPayload(t, s, ticketID)
		if !equalOptions(qp.Options, wantEscalationOptionsPostSeal) {
			t.Errorf("options = %+v, want %+v", qp.Options, wantEscalationOptionsPostSeal)
		}
		if qp.Amendment != nil {
			t.Errorf("question amendment = %+v, want nil", qp.Amendment)
		}
	})
}

func equalOptions(a, b []response.Option) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
