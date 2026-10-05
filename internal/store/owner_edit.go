// owner_edit.go: Store.OwnerEdit (#41), the one store method behind every
// owner console action that edits a sealed scenario's given/when/then/
// check, a sealed plan's task (edit or drop), or the ticket body. Each
// accepted edit runs in one transaction, validates the new payload against
// its JSON Schema (plus, for a plan, the structural 1..N check), is
// refused with *OwnerEditError while a run holds the ticket's claim, and
// writes exactly one owner_edit event holding the old and new text.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"zing/internal/response"
)

// The three OwnerEditRequest.Target values OwnerEdit accepts.
const (
	OwnerEditScenario   = "scenario"
	OwnerEditPlanTask   = "plan_task"
	OwnerEditTicketBody = "ticket_body"
)

// The two OwnerEditRequest.Action values OwnerEdit accepts. Drop is legal
// only for OwnerEditPlanTask.
const (
	OwnerEditActionEdit = "edit"
	OwnerEditActionDrop = "drop"
)

// OwnerEditError's Code values.
const (
	ownerEditCodeBadRequest = "bad_request"
	ownerEditCodeNotFound   = "not_found"
	ownerEditCodeNotSealed  = "not_sealed"
	ownerEditCodeLanded     = "landed"
	ownerEditCodeClaimed    = "claimed"
	ownerEditCodeInvalid    = "invalid"
)

// maxPlanTaskRef is the highest task number a plan may carry (response.
// Task.N's own jsonschema maximum), the upper bound OwnerEditRequest.Ref
// must respect for a plan_task edit before OwnerEdit ever loads the plan.
const maxPlanTaskRef = 12

// OwnerEditRequest is one owner edit: an edit to one scenario's given,
// when, then, or check, an edit or drop of one plan task, or an amendment
// to the ticket body (design section matching #41). Exactly the fields
// named for Target are meaningful; any other non-nil field is refused
// bad_request.
type OwnerEditRequest struct {
	TicketID int64
	Target   string
	Ref      string
	Action   string

	// scenario edit only.
	Given *string
	When  *string
	Then  *string
	Check *string

	// plan_task edit only.
	Text *string
	Test *string
	Demo *bool

	// ticket_body edit only.
	Body *string
}

// OwnerEditError is OwnerEdit's one refusal shape: Code picks the HTTP
// status the console maps it to, Reason is the one owner-facing sentence.
// A refusal changes nothing.
type OwnerEditError struct {
	Code   string
	Reason string
}

func (e *OwnerEditError) Error() string { return e.Reason }

func ownerEditErr(code, reason string) *OwnerEditError {
	return &OwnerEditError{Code: code, Reason: reason}
}

// scenarioRefPattern is OwnerEditRequest.Ref's required shape for a
// scenario target, matching response.Scenario.ID's own pattern.
var scenarioRefPattern = regexp.MustCompile(`^s\d+$`)

// ownerEditFieldsByTarget names, for each target, exactly the
// OwnerEditRequest fields an edit may set.
var ownerEditFieldsByTarget = map[string]map[string]bool{
	OwnerEditScenario:   {"given": true, "when": true, "then": true, "check": true},
	OwnerEditPlanTask:   {"text": true, "test": true, "demo": true},
	OwnerEditTicketBody: {"body": true},
}

// ownerEditSetFields names every field req sets, in a fixed order, so a
// bad_request names the first one deterministically.
func ownerEditSetFields(req OwnerEditRequest) []string {
	var names []string
	if req.Given != nil {
		names = append(names, "given")
	}
	if req.When != nil {
		names = append(names, "when")
	}
	if req.Then != nil {
		names = append(names, "then")
	}
	if req.Check != nil {
		names = append(names, "check")
	}
	if req.Text != nil {
		names = append(names, "text")
	}
	if req.Test != nil {
		names = append(names, "test")
	}
	if req.Demo != nil {
		names = append(names, "demo")
	}
	if req.Body != nil {
		names = append(names, "body")
	}
	return names
}

// checkOwnerEditShape validates req's shape before OwnerEdit ever opens a
// transaction: the target, the action, Ref's format for that target, and
// that only the fields allowed for that target are set.
func checkOwnerEditShape(req OwnerEditRequest) *OwnerEditError {
	if req.TicketID < 1 {
		return ownerEditErr(ownerEditCodeBadRequest, "ticket id must be at least 1")
	}

	allowed, ok := ownerEditFieldsByTarget[req.Target]
	if !ok {
		return ownerEditErr(ownerEditCodeBadRequest, "target must be one of scenario, plan_task, ticket_body")
	}

	switch req.Action {
	case OwnerEditActionEdit:
	case OwnerEditActionDrop:
		if req.Target != OwnerEditPlanTask {
			return ownerEditErr(ownerEditCodeBadRequest, "drop is allowed only for plan_task")
		}
	default:
		return ownerEditErr(ownerEditCodeBadRequest, "action must be edit or drop")
	}

	if err := checkOwnerEditRef(req.Target, req.Ref); err != nil {
		return err
	}

	names := ownerEditSetFields(req)
	for _, name := range names {
		if !allowed[name] {
			return ownerEditErr(ownerEditCodeBadRequest, "field "+name+" is not allowed for target "+req.Target)
		}
	}
	if req.Action == OwnerEditActionEdit && len(names) == 0 {
		return ownerEditErr(ownerEditCodeBadRequest, "an edit must set at least one field")
	}

	if req.Target == OwnerEditTicketBody && req.Body != nil && strings.TrimSpace(*req.Body) == "" {
		return ownerEditErr(ownerEditCodeBadRequest, "body must not be blank")
	}

	return nil
}

// checkOwnerEditRef validates Ref's format for target: a scenario id
// (^s[0-9]+$), a plan task number (decimal, 1 to maxPlanTaskRef), or, for
// ticket_body, the empty string.
func checkOwnerEditRef(target, ref string) *OwnerEditError {
	switch target {
	case OwnerEditScenario:
		if !scenarioRefPattern.MatchString(ref) {
			return ownerEditErr(ownerEditCodeBadRequest, "ref must match ^s[0-9]+$")
		}
	case OwnerEditPlanTask:
		n, err := strconv.Atoi(ref)
		if err != nil || n < 1 || n > maxPlanTaskRef {
			return ownerEditErr(ownerEditCodeBadRequest, fmt.Sprintf("ref must be a decimal from 1 to %d", maxPlanTaskRef))
		}
	case OwnerEditTicketBody:
		if ref != "" {
			return ownerEditErr(ownerEditCodeBadRequest, "ref must be empty for ticket_body")
		}
	}
	return nil
}

// OwnerEdit applies one owner edit to a sealed scenario, a sealed plan's
// task, or the ticket body, in one transaction, and records an owner_edit
// event holding the old and new text. It refuses while a run holds the
// ticket's claim. A refusal returns *OwnerEditError and changes nothing.
func (s *Store) OwnerEdit(ctx context.Context, req OwnerEditRequest) error {
	if err := checkOwnerEditShape(req); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("owner edit: begin: %w", err)
	}
	defer rollback(tx)

	var ev response.OwnerEditEvent
	switch req.Target {
	case OwnerEditScenario:
		ev, err = s.editScenarioTx(ctx, tx, req)
	case OwnerEditPlanTask:
		ev, err = s.editPlanTaskTx(ctx, tx, req)
	case OwnerEditTicketBody:
		ev, err = editTicketBodyTx(ctx, tx, req)
	}
	if err != nil {
		return err
	}

	if insErr := s.insertOwnerEditEventTx(ctx, tx, req.TicketID, ev); insErr != nil {
		return insErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("owner edit: commit: %w", commitErr)
	}

	slog.Info("owner edit applied", "ticket_id", req.TicketID, "target", req.Target, "ref", req.Ref, "action", req.Action)
	return nil
}

// editScenarioTx applies a scenario edit (OwnerEditPlanTask and
// OwnerEditTicketBody never reach here): it loads the sealed scenario
// named by req.Ref, sets every non-nil field, validates the result against
// artifacts/scenario, and writes it back guarded by the ticket's claim.
func (s *Store) editScenarioTx(ctx context.Context, tx *sql.Tx, req OwnerEditRequest) (response.OwnerEditEvent, error) {
	var id int64
	var payload []byte
	var sealedAt sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT id, payload, sealed_at FROM artifacts
		 WHERE ticket_id = ? AND type = 'scenario' AND json_extract(payload, '$.id') = ?
		 ORDER BY id DESC LIMIT 1`,
		req.TicketID, req.Ref,
	).Scan(&id, &payload, &sealedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotFound, "no scenario "+req.Ref+" on this ticket")
	case err != nil:
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: load scenario %s: %w", req.Ref, err)
	}
	if !sealedAt.Valid {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotSealed, "scenario "+req.Ref+" is not sealed; answer the gate instead")
	}

	var sc response.Scenario
	if err = json.Unmarshal(payload, &sc); err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: unmarshal scenario %s: %w", req.Ref, err)
	}
	oldPayload := string(payload)

	if req.Given != nil {
		sc.Given = *req.Given
	}
	if req.When != nil {
		sc.When = *req.When
	}
	if req.Then != nil {
		sc.Then = *req.Then
	}
	if req.Check != nil {
		sc.Check = *req.Check
	}

	newPayload, err := json.Marshal(sc)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: marshal scenario %s: %w", req.Ref, err)
	}
	if err = s.schemas.validate("artifacts", "scenario", newPayload); err != nil {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeInvalid, err.Error())
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE artifacts SET payload = ? WHERE id = ? AND (SELECT claim_owner FROM tickets WHERE id = ?) IS NULL`,
		string(newPayload), id, req.TicketID,
	)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: update scenario %s: %w", req.Ref, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: update scenario %s: %w", req.Ref, err)
	}
	if n == 0 {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeClaimed, "ticket is claimed; edits are refused while a run holds it")
	}

	return response.OwnerEditEvent{
		Target: OwnerEditScenario, Ref: req.Ref, Action: OwnerEditActionEdit,
		Old: oldPayload, New: string(newPayload),
	}, nil
}

// editPlanTaskTx edits or drops one task of the ticket's current plan.
// Task 2 implements this; for now it refuses every request.
func (s *Store) editPlanTaskTx(_ context.Context, _ *sql.Tx, _ OwnerEditRequest) (response.OwnerEditEvent, error) { //nolint:unparam // result 0 is always the zero value until task 2 implements this
	return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeBadRequest, "not supported yet")
}

// editTicketBodyTx amends tickets.body. Task 3 implements this; for now it
// refuses every request.
func editTicketBodyTx(_ context.Context, _ *sql.Tx, _ OwnerEditRequest) (response.OwnerEditEvent, error) { //nolint:unparam // result 0 is always the zero value until task 3 implements this
	return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeBadRequest, "not supported yet")
}

// insertOwnerEditEventTx builds the owner_edit event message for ev and
// inserts it inside tx through insertMessageTx, which validates the
// payload against events/owner_edit. Body is set to
// response.OwnerEditLine(ev), so the feed renders a line even if a future
// kind's rule cannot decode the payload.
func (s *Store) insertOwnerEditEventTx(ctx context.Context, tx *sql.Tx, ticketID int64, ev response.OwnerEditEvent) error {
	msg, err := NewEvent(ticketID, EventKindOwnerEdit, ev)
	if err != nil {
		return fmt.Errorf("owner edit: build event: %w", err)
	}
	msg.Body = response.OwnerEditLine(ev)

	if err = s.insertMessageTx(ctx, tx, msg); err != nil {
		return fmt.Errorf("owner edit: insert event: %w", err)
	}
	return nil
}
