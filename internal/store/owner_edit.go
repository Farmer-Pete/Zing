// owner_edit.go: Store.OwnerEdit (#41), the one store method behind every
// owner console action that edits a sealed scenario's given/when/then/
// check, a sealed plan's task (edit or drop), a sealed plan's file's task
// list, or the ticket body. Each accepted edit runs in one transaction,
// validates the new payload against its JSON Schema (plus, for a plan, the
// structural 1..N check), is refused with *OwnerEditError while a run
// holds the ticket's claim, and writes exactly one owner_edit event
// holding the old and new text.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"zing/internal/response"
)

// The four OwnerEditRequest.Target values OwnerEdit accepts.
const (
	OwnerEditScenario   = "scenario"
	OwnerEditPlanTask   = "plan_task"
	OwnerEditPlanFile   = "plan_file"
	OwnerEditTicketBody = "ticket_body"
)

// The two OwnerEditRequest.Action values OwnerEdit accepts. Drop is legal
// only for OwnerEditPlanTask.
const (
	OwnerEditActionEdit = "edit"
	OwnerEditActionDrop = "drop"
)

// OwnerEditCode is OwnerEditError's Code: one of the named constants below.
// The console keys its HTTP status map by these, not by a copied string, so
// a renamed or added code fails to compile there instead of silently
// falling back to status 0.
type OwnerEditCode string

// OwnerEditError's Code values.
const (
	OwnerEditCodeBadRequest OwnerEditCode = "bad_request"
	OwnerEditCodeNotFound   OwnerEditCode = "not_found"
	OwnerEditCodeNotSealed  OwnerEditCode = "not_sealed"
	OwnerEditCodeLanded     OwnerEditCode = "landed"
	OwnerEditCodeClaimed    OwnerEditCode = "claimed"
	OwnerEditCodeInvalid    OwnerEditCode = "invalid"
)

// maxPlanTaskRef is the highest task number a plan may carry (response.
// Task.N's own jsonschema maximum), the upper bound OwnerEditRequest.Ref
// must respect for a plan_task edit before OwnerEdit ever loads the plan.
const maxPlanTaskRef = 12

// OwnerEditRequest is one owner edit: an edit to one scenario's given,
// when, then, or check, an edit or drop of one plan task, an edit of one
// plan file's task list, or an amendment to the ticket body (design
// section matching #41). Exactly the fields named for Target are
// meaningful; any other non-nil field is refused bad_request.
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

	// plan_file edit only. Must match planFileTasksPattern; every number
	// must name an existing task (checkPlanStructure). Stored normalized
	// ascending and unique.
	Tasks *string

	// ticket_body edit only.
	Body *string
}

// OwnerEditError is OwnerEdit's one refusal shape: Code picks the HTTP
// status the console maps it to, Reason is the one owner-facing sentence.
// A refusal changes nothing.
type OwnerEditError struct {
	Code   OwnerEditCode
	Reason string
}

func (e *OwnerEditError) Error() string { return e.Reason }

func ownerEditErr(code OwnerEditCode, reason string) *OwnerEditError {
	return &OwnerEditError{Code: code, Reason: reason}
}

// ownerEditClaimedReason is every claim-guarded write's one refusal
// sentence, shared by execClaimGuardedTx's callers so a claimed ticket
// reads the same regardless of which target it refused.
const ownerEditClaimedReason = "ticket is claimed; edits are refused while a run holds it"

// execClaimGuardedTx runs query guarded by "the ticket is not claimed" --
// query must itself AND its WHERE clause against
// "(SELECT claim_owner FROM tickets WHERE id = ?) IS NULL" or
// "claim_owner IS NULL" -- and returns a *OwnerEditError of
// OwnerEditCodeClaimed when it affects no rows. what names the target in a
// wrapped non-refusal error. nil means the write landed.
func execClaimGuardedTx(ctx context.Context, tx *sql.Tx, what, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("owner edit: update %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("owner edit: update %s: %w", what, err)
	}
	if n == 0 {
		return ownerEditErr(OwnerEditCodeClaimed, ownerEditClaimedReason)
	}
	return nil
}

// scenarioRefPattern is OwnerEditRequest.Ref's required shape for a
// scenario target, matching response.Scenario.ID's own pattern.
var scenarioRefPattern = regexp.MustCompile(`^s\d+$`)

// planFileTasksPattern is OwnerEditRequest.Tasks' required shape for a
// plan_file edit: one or more task numbers (1 to 99), separated by single
// spaces. Whether each number names a task that actually exists on the
// plan is checked later, by checkPlanStructure.
var planFileTasksPattern = regexp.MustCompile(`^[1-9]\d?( [1-9]\d?)*$`)

// ownerEditFieldsByTarget names, for each target, exactly the
// OwnerEditRequest fields an edit may set.
var ownerEditFieldsByTarget = map[string]map[string]bool{
	OwnerEditScenario:   {"given": true, "when": true, "then": true, "check": true},
	OwnerEditPlanTask:   {"text": true, "test": true, "demo": true},
	OwnerEditPlanFile:   {"tasks": true},
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
	if req.Tasks != nil {
		names = append(names, "tasks")
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
		return ownerEditErr(OwnerEditCodeBadRequest, "ticket id must be at least 1")
	}

	allowed, ok := ownerEditFieldsByTarget[req.Target]
	if !ok {
		return ownerEditErr(OwnerEditCodeBadRequest, "target must be one of scenario, plan_task, plan_file, ticket_body")
	}

	switch req.Action {
	case OwnerEditActionEdit:
	case OwnerEditActionDrop:
		if req.Target != OwnerEditPlanTask {
			return ownerEditErr(OwnerEditCodeBadRequest, "drop is allowed only for plan_task")
		}
	default:
		return ownerEditErr(OwnerEditCodeBadRequest, "action must be edit or drop")
	}

	if err := checkOwnerEditRef(req.Target, req.Ref); err != nil {
		return err
	}

	names := ownerEditSetFields(req)
	for _, name := range names {
		if !allowed[name] {
			return ownerEditErr(OwnerEditCodeBadRequest, "field "+name+" is not allowed for target "+req.Target)
		}
	}
	if req.Action == OwnerEditActionEdit && len(names) == 0 {
		return ownerEditErr(OwnerEditCodeBadRequest, "an edit must set at least one field")
	}

	if req.Target == OwnerEditPlanFile && req.Tasks != nil && !planFileTasksPattern.MatchString(*req.Tasks) {
		return ownerEditErr(OwnerEditCodeBadRequest, "tasks must be task numbers separated by single spaces, such as 2 6")
	}

	blankBody := req.Body != nil && strings.TrimSpace(*req.Body) == ""
	if req.Target == OwnerEditTicketBody && blankBody {
		return ownerEditErr(OwnerEditCodeBadRequest, "body must not be blank")
	}

	return nil
}

// checkOwnerEditRef validates Ref's format for target: a scenario id
// (^s[0-9]+$), a plan task number (decimal, 1 to maxPlanTaskRef), a
// non-blank file path for plan_file, or, for ticket_body, the empty string.
func checkOwnerEditRef(target, ref string) *OwnerEditError {
	switch target {
	case OwnerEditScenario:
		if !scenarioRefPattern.MatchString(ref) {
			return ownerEditErr(OwnerEditCodeBadRequest, "ref must match ^s[0-9]+$")
		}
	case OwnerEditPlanTask:
		n, err := strconv.Atoi(ref)
		inRange := err == nil && n >= 1 && n <= maxPlanTaskRef
		if !inRange {
			return ownerEditErr(OwnerEditCodeBadRequest, fmt.Sprintf("ref must be a decimal from 1 to %d", maxPlanTaskRef))
		}
	case OwnerEditPlanFile:
		if strings.TrimSpace(ref) == "" {
			return ownerEditErr(OwnerEditCodeBadRequest, "ref must name a file path")
		}
	case OwnerEditTicketBody:
		if ref != "" {
			return ownerEditErr(OwnerEditCodeBadRequest, "ref must be empty for ticket_body")
		}
	}
	return nil
}

// OwnerEdit applies one owner edit to a sealed scenario, a sealed plan's
// task, a sealed plan's file's task list, or the ticket body, in one
// transaction, and records an owner_edit event holding the old and new
// text. It refuses while a run holds the ticket's claim. A refusal returns
// *OwnerEditError and changes nothing.
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
	case OwnerEditPlanFile:
		ev, err = s.editPlanFileTx(ctx, tx, req)
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

	slog.InfoContext(ctx, "owner edit applied", "ticket_id", req.TicketID, "target", req.Target, "ref", req.Ref, "action", req.Action)
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
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeNotFound, "no scenario "+req.Ref+" on this ticket")
	case err != nil:
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: load scenario %s: %w", req.Ref, err)
	}
	if !sealedAt.Valid {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeNotSealed, "scenario "+req.Ref+" is not sealed; answer the gate instead")
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
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeInvalid, err.Error())
	}

	if err := execClaimGuardedTx(ctx, tx, "scenario "+req.Ref,
		`UPDATE artifacts SET payload = ? WHERE id = ? AND (SELECT claim_owner FROM tickets WHERE id = ?) IS NULL`,
		string(newPayload), id, req.TicketID,
	); err != nil {
		return response.OwnerEditEvent{}, err
	}

	return response.OwnerEditEvent{
		Target: OwnerEditScenario, Ref: req.Ref, Action: OwnerEditActionEdit,
		Old: oldPayload, New: string(newPayload),
	}, nil
}

// loadSealedPlanTx loads the ticket's newest plan artifact and refuses it
// unless its cohort (the run that produced it) has at least one sealed
// scenario.
func loadSealedPlanTx(ctx context.Context, tx *sql.Tx, ticketID int64) (id int64, payload []byte, plan response.Plan, err error) {
	var runID sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT id, run_id, payload FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		ticketID,
	).Scan(&id, &runID, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil, response.Plan{}, ownerEditErr(OwnerEditCodeNotFound, "this ticket has no plan")
	case err != nil:
		return 0, nil, response.Plan{}, fmt.Errorf("owner edit: load plan: %w", err)
	}

	var sealedCount int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ? AND sealed_at IS NOT NULL`,
		ticketID, runID,
	).Scan(&sealedCount); err != nil {
		return 0, nil, response.Plan{}, fmt.Errorf("owner edit: count sealed scenarios: %w", err)
	}
	if sealedCount < 1 {
		return 0, nil, response.Plan{}, ownerEditErr(OwnerEditCodeNotSealed, "the plan is not sealed; answer the gate instead")
	}

	if err = json.Unmarshal(payload, &plan); err != nil {
		return 0, nil, response.Plan{}, fmt.Errorf("owner edit: unmarshal plan: %w", err)
	}
	return id, payload, plan, nil
}

// editPlanTaskTx edits or drops one task of the ticket's current (highest
// version) plan: it loads the plan, checks that its cohort has at least one
// sealed scenario, applies the edit or the drop, validates the result
// against artifacts/plan plus checkPlanStructure, and writes it back in
// place (the version is unchanged) guarded by the ticket's claim.
func (s *Store) editPlanTaskTx(ctx context.Context, tx *sql.Tx, req OwnerEditRequest) (response.OwnerEditEvent, error) {
	id, payload, plan, err := loadSealedPlanTx(ctx, tx, req.TicketID)
	if err != nil {
		return response.OwnerEditEvent{}, err
	}

	// checkOwnerEditRef has already confirmed req.Ref parses as a decimal
	// from 1 to maxPlanTaskRef, so the error is unreachable here.
	n, _ := strconv.Atoi(req.Ref) //nolint:errcheck // unreachable, see above

	oldPayload := string(payload)

	idx := slices.IndexFunc(plan.Delivery.Tasks, func(t response.Task) bool { return t.N == n })
	if idx == -1 {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeNotFound, fmt.Sprintf("no task %d in the plan", n))
	}

	switch req.Action {
	case OwnerEditActionEdit:
		if req.Text != nil {
			plan.Delivery.Tasks[idx].Text = *req.Text
		}
		if req.Test != nil {
			plan.Delivery.Tasks[idx].Test = *req.Test
		}
		if req.Demo != nil {
			plan.Delivery.Tasks[idx].Demo = *req.Demo
		}
	case OwnerEditActionDrop:
		var maxLanded sql.NullInt64
		if err = tx.QueryRowContext(ctx,
			`SELECT MAX(json_extract(payload, '$.task_n')) FROM artifacts
			 WHERE ticket_id = ? AND type = 'build_report' AND json_extract(payload, '$.commit_sha') IS NOT NULL`,
			req.TicketID,
		).Scan(&maxLanded); err != nil {
			return response.OwnerEditEvent{}, fmt.Errorf("owner edit: load landed tasks: %w", err)
		}
		if maxLanded.Valid && int(maxLanded.Int64) >= n {
			return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeLanded, fmt.Sprintf("task %d has landed; only tasks after it can be dropped", maxLanded.Int64))
		}
		plan = dropPlanTask(plan, n)
	}

	newPayload, err := json.Marshal(plan)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: marshal plan: %w", err)
	}
	if err = s.schemas.validate("artifacts", "plan", newPayload); err != nil {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeInvalid, err.Error())
	}
	if fault := checkPlanStructure(plan); fault != "" {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeInvalid, fault)
	}

	if err := execClaimGuardedTx(ctx, tx, "plan",
		`UPDATE artifacts SET payload = ? WHERE id = ? AND (SELECT claim_owner FROM tickets WHERE id = ?) IS NULL`,
		string(newPayload), id, req.TicketID,
	); err != nil {
		return response.OwnerEditEvent{}, err
	}

	return response.OwnerEditEvent{
		Target: OwnerEditPlanTask, Ref: req.Ref, Action: req.Action,
		Old: oldPayload, New: string(newPayload),
	}, nil
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
			namesTask := err == nil && m >= 1 && m <= len(tasks)
			if !namesTask {
				return fmt.Sprintf("file %s names task %s, outside 1..%d", f.Path, field, len(tasks))
			}
		}
	}
	return ""
}

// editPlanFileTx sets the task list of every delivery file entry whose path
// is req.Ref, on the ticket's newest sealed, task-mapped plan. It writes
// the whole plan back in place (the version is unchanged) guarded by the
// ticket's claim.
func (s *Store) editPlanFileTx(ctx context.Context, tx *sql.Tx, req OwnerEditRequest) (response.OwnerEditEvent, error) {
	id, _, plan, err := loadSealedPlanTx(ctx, tx, req.TicketID)
	if err != nil {
		return response.OwnerEditEvent{}, err
	}
	if !response.TaskMapped(plan) {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeInvalid, "this plan's files name no tasks; a task list cannot be set")
	}

	newTasks := normalizeTaskList(*req.Tasks)
	old, found := "", false
	for i, f := range plan.Delivery.Files {
		if f.Path != req.Ref {
			continue
		}
		if !found {
			old, found = f.Task, true
		}
		plan.Delivery.Files[i].Task = newTasks
	}
	if !found {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeNotFound, "no file "+req.Ref+" in the plan")
	}

	newPayload, err := json.Marshal(plan)
	if err != nil {
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: marshal plan: %w", err)
	}
	if err = s.schemas.validate("artifacts", "plan", newPayload); err != nil {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeInvalid, err.Error())
	}
	if fault := checkPlanStructure(plan); fault != "" {
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeInvalid, fault)
	}

	if err := execClaimGuardedTx(ctx, tx, "plan",
		`UPDATE artifacts SET payload = ? WHERE id = ? AND (SELECT claim_owner FROM tickets WHERE id = ?) IS NULL`,
		string(newPayload), id, req.TicketID,
	); err != nil {
		return response.OwnerEditEvent{}, err
	}

	return response.OwnerEditEvent{
		Target: OwnerEditPlanFile, Ref: req.Ref, Action: OwnerEditActionEdit,
		Old: old, New: newTasks,
	}, nil
}

// normalizeTaskList returns s's task numbers ascending, unique, joined by
// single spaces (response.FormatTaskList, the same format GrantFileTasks
// writes). checkOwnerEditShape has already matched s against
// planFileTasksPattern, so every field is a valid number.
func normalizeTaskList(s string) string {
	var nums []int
	for field := range strings.FieldsSeq(s) {
		n, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		nums = append(nums, n)
	}
	return response.FormatTaskList(nums)
}

// grantPlanFilesTx applies a HandlerCommit's GrantFiles to the ticket's
// newest plan in place and writes one owner_edit event per changed file
// (plan #51). It needs no claim guard: CommitHandlerResult's own fenced
// UPDATE, at the end of the same transaction, is what proves the handler
// still holds the claim. It writes no event and no UPDATE when
// GrantFileTasks reports no change, or when the ticket has no plan at all
// (sql.ErrNoRows): a commit's GrantFiles must never refuse an otherwise
// valid commit over a grant that no longer applies.
func (s *Store) grantPlanFilesTx(ctx context.Context, tx *sql.Tx, ticketID int64, g response.FileGrant) error {
	var id int64
	var payload []byte
	err := tx.QueryRowContext(ctx,
		`SELECT id, payload FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		ticketID).Scan(&id, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("grant plan files: load plan: %w", err)
	}
	var plan response.Plan
	if err = json.Unmarshal(payload, &plan); err != nil {
		return fmt.Errorf("grant plan files: unmarshal plan: %w", err)
	}
	granted, changes := response.GrantFileTasks(plan, g)
	if len(changes) == 0 {
		slog.DebugContext(ctx, "file grant no change", "ticket_id", ticketID, "task_n", g.Task, "paths", g.Paths)
		return nil
	}
	newPayload, err := json.Marshal(granted)
	if err != nil {
		return fmt.Errorf("grant plan files: marshal plan: %w", err)
	}
	if err = s.schemas.validate("artifacts", "plan", newPayload); err != nil {
		return fmt.Errorf("grant plan files: %w", err)
	}
	if fault := checkPlanStructure(granted); fault != "" {
		return fmt.Errorf("grant plan files: %s", fault)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE artifacts SET payload = ? WHERE id = ?`, string(newPayload), id); err != nil {
		return fmt.Errorf("grant plan files: update plan: %w", err)
	}
	for _, ch := range changes {
		ev := response.OwnerEditEvent{Target: OwnerEditPlanFile, Ref: ch.Path, Action: OwnerEditActionEdit, Old: ch.Old, New: ch.New}
		if err := s.insertOwnerEditEventTx(ctx, tx, ticketID, ev); err != nil {
			return err
		}
	}
	slog.InfoContext(ctx, "file grant applied", "ticket_id", ticketID, "task_n", g.Task, "paths", g.Paths, "changed", len(changes))
	return nil
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
		return response.OwnerEditEvent{}, ownerEditErr(OwnerEditCodeNotFound, fmt.Sprintf("no ticket %d", req.TicketID))
	case err != nil:
		return response.OwnerEditEvent{}, fmt.Errorf("owner edit: load ticket %d: %w", req.TicketID, err)
	}

	newBody := *req.Body

	if err := execClaimGuardedTx(ctx, tx, fmt.Sprintf("ticket %d body", req.TicketID),
		`UPDATE tickets SET body = ? WHERE id = ? AND claim_owner IS NULL`,
		newBody, req.TicketID,
	); err != nil {
		return response.OwnerEditEvent{}, err
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
