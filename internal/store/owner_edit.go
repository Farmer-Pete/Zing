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

// editPlanTaskTx edits or drops one task of the ticket's current (highest
// version) plan: it loads the plan, checks that its cohort has at least one
// sealed scenario, applies the edit or the drop, validates the result
// against artifacts/plan plus checkPlanStructure, and writes it back in
// place (the version is unchanged) guarded by the ticket's claim.
func (s *Store) editPlanTaskTx(ctx context.Context, tx *sql.Tx, req OwnerEditRequest) (response.OwnerEditEvent, error) {
	var id int64
	var runID sql.NullInt64
	var payload []byte
	err := tx.QueryRowContext(ctx,
		`SELECT id, run_id, payload FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		req.TicketID,
	).Scan(&id, &runID, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotFound, "this ticket has no plan")
	case err != nil:
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: load plan: %w", err)
	}

	var sealedCount int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ? AND sealed_at IS NOT NULL`,
		req.TicketID, runID,
	).Scan(&sealedCount); err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: count sealed scenarios: %w", err)
	}
	if sealedCount < 1 {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotSealed, "the plan is not sealed; answer the gate instead")
	}

	n, err := strconv.Atoi(req.Ref)
	if err != nil {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotFound, "no task "+req.Ref+" in the plan")
	}

	var plan response.Plan
	if err = json.Unmarshal(payload, &plan); err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: unmarshal plan: %w", err)
	}
	oldPayload := string(payload)

	found := false
	for _, t := range plan.Delivery.Tasks {
		if t.N == n {
			found = true
			break
		}
	}
	if !found {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotFound, fmt.Sprintf("no task %d in the plan", n))
	}

	switch req.Action {
	case OwnerEditActionEdit:
		for i := range plan.Delivery.Tasks {
			if plan.Delivery.Tasks[i].N != n {
				continue
			}
			if req.Text != nil {
				plan.Delivery.Tasks[i].Text = *req.Text
			}
			if req.Test != nil {
				plan.Delivery.Tasks[i].Test = *req.Test
			}
			if req.Demo != nil {
				plan.Delivery.Tasks[i].Demo = *req.Demo
			}
			break
		}
	case OwnerEditActionDrop:
		landed, lerr := landedTaskNumbers(ctx, tx, req.TicketID)
		if lerr != nil {
			return response.OwnerEditEvent{}, lerr
		}
		for _, m := range landed {
			if m >= n {
				return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeLanded, fmt.Sprintf("task %d has landed; only tasks after it can be dropped", m))
			}
		}
		plan = dropPlanTask(plan, n)
	}

	newPayload, err := json.Marshal(plan)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: marshal plan: %w", err)
	}
	if err = s.schemas.validate("artifacts", "plan", newPayload); err != nil {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeInvalid, err.Error())
	}
	if fault := checkPlanStructure(plan); fault != "" {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeInvalid, fault)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE artifacts SET payload = ? WHERE id = ? AND (SELECT claim_owner FROM tickets WHERE id = ?) IS NULL`,
		string(newPayload), id, req.TicketID,
	)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: update plan: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: update plan: %w", err)
	}
	if affected == 0 {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeClaimed, "ticket is claimed; edits are refused while a run holds it")
	}

	return response.OwnerEditEvent{
		Target: OwnerEditPlanTask, Ref: req.Ref, Action: req.Action,
		Old: oldPayload, New: string(newPayload),
	}, nil
}

// landedTaskNumbers returns the task_n of every build_report artifact of
// ticketID whose payload carries a commit_sha: the task numbers a drop must
// not reach at or past (editPlanTaskTx's landed refusal).
func landedTaskNumbers(ctx context.Context, tx *sql.Tx, ticketID int64) ([]int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT payload FROM artifacts WHERE ticket_id = ? AND type = 'build_report' AND json_extract(payload, '$.commit_sha') IS NOT NULL`,
		ticketID,
	)
	if err != nil {
		return nil, fmt.Errorf("owner edit: load landed tasks: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("owner edit: load landed tasks: %w", err)
		}
		var report response.BuildReport
		if err := json.Unmarshal(payload, &report); err != nil {
			return nil, fmt.Errorf("owner edit: decode build report: %w", err)
		}
		out = append(out, report.TaskN)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("owner edit: load landed tasks: %w", err)
	}
	return out, nil
}

// dropPlanTask returns p without task n: every later task's N drops by
// one, every file's task list loses n and shifts higher numbers down, and
// a file left with no task is removed. A file with no task list is kept.
// Pure: p is not modified.
func dropPlanTask(p response.Plan, n int) response.Plan {
	out := p

	tasks := make([]response.Task, 0, len(p.Delivery.Tasks))
	for _, t := range p.Delivery.Tasks {
		switch {
		case t.N == n:
			continue
		case t.N > n:
			t.N--
		}
		tasks = append(tasks, t)
	}
	out.Delivery.Tasks = tasks

	files := make([]response.FileChange, 0, len(p.Delivery.Files))
	for _, f := range p.Delivery.Files {
		if f.Task == "" {
			files = append(files, f)
			continue
		}
		var nums []string
		for field := range strings.FieldsSeq(f.Task) {
			m, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			switch {
			case m == n:
				continue
			case m > n:
				nums = append(nums, strconv.Itoa(m-1))
			default:
				nums = append(nums, strconv.Itoa(m))
			}
		}
		if len(nums) == 0 {
			continue
		}
		f.Task = strings.Join(nums, " ")
		files = append(files, f)
	}
	out.Delivery.Files = files

	return out
}

// checkPlanStructure reports the first structural fault the JSON Schema
// cannot see: tasks not numbered 1..len in order, or a file task list
// naming a number outside 1..len. Empty string means none.
func checkPlanStructure(p response.Plan) string {
	tasks := p.Delivery.Tasks
	for i, t := range tasks {
		if t.N != i+1 {
			return fmt.Sprintf("tasks must be numbered 1..%d in order; position %d has n %d", len(tasks), i+1, t.N)
		}
	}
	for _, f := range p.Delivery.Files {
		for field := range strings.FieldsSeq(f.Task) {
			m, err := strconv.Atoi(field)
			if err != nil || m < 1 || m > len(tasks) {
				return fmt.Sprintf("file %s names task %s, outside 1..%d", f.Path, field, len(tasks))
			}
		}
	}
	return ""
}

// editTicketBodyTx amends tickets.body: checkOwnerEditShape has already
// guaranteed req.Body is set and non-blank. It writes the new body back
// guarded by the ticket's claim; Ref stays empty, matching the request's own
// shape for this target.
func editTicketBodyTx(ctx context.Context, tx *sql.Tx, req OwnerEditRequest) (response.OwnerEditEvent, error) {
	var oldBody string
	err := tx.QueryRowContext(ctx, `SELECT body FROM tickets WHERE id = ?`, req.TicketID).Scan(&oldBody)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeNotFound, fmt.Sprintf("no ticket %d", req.TicketID))
	case err != nil:
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: load ticket %d: %w", req.TicketID, err)
	}

	newBody := *req.Body

	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET body = ? WHERE id = ? AND claim_owner IS NULL`,
		newBody, req.TicketID,
	)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: update ticket %d body: %w", req.TicketID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: update ticket %d body: %w", req.TicketID, err)
	}
	if n == 0 {
		return response.OwnerEditEvent{}, ownerEditErr(ownerEditCodeClaimed, "ticket is claimed; edits are refused while a run holds it")
	}

	return response.OwnerEditEvent{
		Target: OwnerEditTicketBody, Ref: "", Action: OwnerEditActionEdit,
		Old: oldBody, New: newBody,
	}, nil
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
