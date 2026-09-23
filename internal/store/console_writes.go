// console_writes.go: the console's write surface (design section 6.7, 6.8):
// SaveDraft and SendBatch, the batched composer's two halves, and MarkRead.
// This file lives in package store, not internal/console, because the
// composer's writes need insertMessageTx and schemaSet.validate, both
// unexported (design section 14 reconciliation: "the console is in package
// store so it can transact"). SendBatch generalizes AnswerQuestion
// (commit.go) from one answer to a whole batch and reuses its validation
// order; it never calls the non-tx InsertMessage.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"zing/internal/response"
)

// msgTypeReply and draftState round out commit.go's message-type and
// lifecycle constants with the two values this file adds: a free-text
// "reply" row, and the "draft" state every composer write starts in before
// SendBatch flips it to answerStateSent (commit.go).
const (
	msgTypeReply = "reply"
	draftState   = "draft"
)

// ConflictError is SaveDraft's and SendBatch's one error shape for a
// semantic rejection the caller can safely show verbatim (design section
// 6.7): a closed question, a wrong ticket, a bad option or item ref, an
// ambiguous or empty draft, or (SendBatch only) an empty batch. The console
// handler maps it to 409; every other error maps to 500 with a generic body,
// matching AnswerQuestion's own answerConflict/AnswerResult.Conflict split.
type ConflictError struct{ Reason string }

func (e *ConflictError) Error() string { return e.Reason }

// conflict builds a *ConflictError as a plain error, the one shape every
// named-conflict return path in this file shares (commit.go's
// answerConflict does the same for AnswerQuestion's AnswerResult).
func conflict(reason string) error { return &ConflictError{Reason: reason} }

// ItemDecision is one ref-to-decision pick for an item-kind question
// (perimeter, review): SaveDraft merges it into the draft's
// AnswerPayload.Items map one call at a time (design section 6.7).
type ItemDecision struct {
	Ref      string
	Decision response.Decision
}

// DraftInput is one SaveDraft call: exactly one of Option, Item, or Text is
// set (design section 6.7). QuestionID is nil only for a thread reply; an
// option or item answer always targets a question.
type DraftInput struct {
	TicketID   int64
	QuestionID *int64
	Option     *string
	Item       *ItemDecision
	Text       string
}

// DraftResult reports the draft row SaveDraft wrote or updated. Replaced is
// true only when this call changed an existing draft's stored value; a
// fresh draft, and a repeat of the exact same option or item value, both
// report false (design section 6.7: "idempotent on a repeat with the same
// value").
type DraftResult struct {
	MessageID int64
	Replaced  bool
}

// BatchResult reports what SendBatch did: how many drafts it flipped to
// sent, how many it discarded as stale (their question closed or was
// deleted, or the exact option/item they drafted stopped being valid,
// between the draft and the send; PR review fix), the batch_id it
// allocated, whether it cleared the ticket's wait, and whether there was
// nothing left to send at all -- either no drafts existed, or every one of
// them turned out stale (design section 6.7).
type BatchResult struct {
	Sent        int
	Discarded   int
	BatchID     int64
	WaitCleared bool
	Empty       bool
}

// draftModeCount counts how many of Option, Item, and Text are set on in,
// the check SaveDraft makes before anything else: more than one is
// "ambiguous draft mode", and zero (Option and Item unset, Text empty) is
// "empty text" (design section 6.7's own two names for this one check).
func draftModeCount(in DraftInput) int {
	n := 0
	if in.Option != nil {
		n++
	}
	if in.Item != nil {
		n++
	}
	if in.Text != "" {
		n++
	}
	return n
}

// SaveDraft upserts one draft for a ticket (design section 6.7). It handles
// exactly one answer mode: an option answer keys by (ticket, question) and
// replaces the earlier draft; an item answer merges one ref->decision into
// the (ticket, question) draft's items map; a free reply on a question is a
// reply row with parent_id=question; a free reply to the thread is a reply
// row with no parent. All drafts are author="you", state="draft".
func (s *Store) SaveDraft(ctx context.Context, in DraftInput) (result DraftResult, err error) {
	// Named returns so one deferred call logs every branch's outcome
	// (CLAUDE.md: "log every major branch with the ids"), without a log
	// line at each of SaveDraft's many early returns.
	defer func() {
		logSaveDraftOutcome(ctx, in, result, err)
	}()

	switch n := draftModeCount(in); {
	case n > 1:
		return DraftResult{}, conflict("ambiguous draft mode")
	case n == 0:
		return DraftResult{}, conflict("empty text")
	}
	if in.QuestionID == nil && (in.Option != nil || in.Item != nil) {
		return DraftResult{}, conflict("question required")
	}
	if in.Item != nil && !validDecision(in.Item.Decision) {
		return DraftResult{}, conflict("invalid decision")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DraftResult{}, fmt.Errorf("save draft: begin tx: %w", err)
	}
	defer rollback(tx)

	if in.QuestionID == nil {
		result, err = s.insertReplyDraftTx(ctx, tx, in.TicketID, nil, in.Text)
	} else {
		var payload response.QuestionPayload
		payload, err = openQuestionForTicketTx(ctx, tx, *in.QuestionID, in.TicketID)
		if err != nil {
			return DraftResult{}, err
		}

		switch {
		case in.Option != nil:
			if !validOption(payload, *in.Option) {
				return DraftResult{}, conflict("missing option")
			}
			result, err = s.upsertOptionDraftTx(ctx, tx, in.TicketID, *in.QuestionID, *in.Option)
		case in.Item != nil:
			if !validItemRef(payload, in.Item.Ref) {
				return DraftResult{}, conflict("missing item")
			}
			result, err = s.upsertItemDraftTx(ctx, tx, in.TicketID, *in.QuestionID, *in.Item)
		default:
			result, err = s.insertReplyDraftTx(ctx, tx, in.TicketID, in.QuestionID, in.Text)
		}
	}
	if err != nil {
		return DraftResult{}, err
	}

	if err = tx.Commit(); err != nil {
		return DraftResult{}, fmt.Errorf("save draft: commit tx: %w", err)
	}
	return result, nil
}

// logSaveDraftOutcome logs SaveDraft's major branches with their ids
// (CLAUDE.md: "log every major branch with the ids"): a successful save
// reports the row it wrote or updated, and a named conflict reports its
// reason. An unexpected (non-conflict) error is left to the caller: the
// console handler already logs it (internal/console/answer.go), so logging
// it again here would violate "log or return, never both". question_id logs
// as 0 for a thread reply, which carries no question.
func logSaveDraftOutcome(ctx context.Context, in DraftInput, result DraftResult, err error) {
	var questionID int64
	if in.QuestionID != nil {
		questionID = *in.QuestionID
	}
	if err != nil {
		if ce, ok := errors.AsType[*ConflictError](err); ok {
			slog.InfoContext(ctx, "save draft conflict",
				"ticket_id", in.TicketID, "question_id", questionID, "reason", ce.Reason)
		}
		return
	}
	slog.InfoContext(ctx, "draft saved",
		"ticket_id", in.TicketID, "question_id", questionID, "message_id", result.MessageID, "replaced", result.Replaced)
}

// openQuestionForTicketTx reads questionID and returns its parsed payload,
// after checking every question-targeted conflict SaveDraft and SendBatch's
// re-validation share (design section 6.7): the question exists, is a
// "question" message, belongs to ticketID, and is still open. A missing
// question is reported as a ConflictError ("question not found"), unlike
// AnswerQuestion's raw sql.ErrNoRows, because a stale or mistyped question
// id arriving from a client is an ordinary 409, not a 500 (design section
// 6.7's own conflict list names it alongside the others).
func openQuestionForTicketTx(ctx context.Context, tx *sql.Tx, questionID, ticketID int64) (response.QuestionPayload, error) {
	q, err := getMessageTx(ctx, tx, questionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return response.QuestionPayload{}, conflict("question not found")
		}
		return response.QuestionPayload{}, fmt.Errorf("save draft: get question %d: %w", questionID, err)
	}
	if q.Type != msgTypeQuestion {
		return response.QuestionPayload{}, conflict("question not found")
	}
	if q.TicketID != ticketID {
		return response.QuestionPayload{}, conflict("wrong ticket")
	}
	if q.State == nil || *q.State != questionStateOpen {
		return response.QuestionPayload{}, conflict("question closed")
	}

	var payload response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &payload); err != nil {
		return response.QuestionPayload{}, fmt.Errorf("save draft: unmarshal question %d payload: %w", questionID, err)
	}
	return payload, nil
}

// lastInsertIDTx returns the id insertMessageTx (commit.go) just gave its
// row. insertMessageTx itself reports only an error, not an id -- every
// existing caller (AnswerQuestion, CommitHandlerResult) never needs one back
// -- so a draft insert, which does (DraftResult.MessageID), reads it right
// after with SQLite's own last_insert_rowid(), scoped to tx's one
// connection, rather than widening insertMessageTx's signature for every
// caller.
func lastInsertIDTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&id); err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// getMessageTx is GetMessage (reads.go), tx-scoped: SaveDraft and SendBatch
// read a question inside their own transaction, so its state cannot change
// out from under the validation they base a write on.
func getMessageTx(ctx context.Context, tx *sql.Tx, id int64) (MessageRow, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id)
	m, err := scanMessage(row)
	if err != nil {
		return MessageRow{}, err
	}
	return m, nil
}

// validOption reports whether key is one of q's option keys.
func validOption(q response.QuestionPayload, key string) bool {
	for _, o := range q.Options {
		if o.Key == key {
			return true
		}
	}
	return false
}

// validItemRef reports whether ref is one of q's item refs.
func validItemRef(q response.QuestionPayload, ref string) bool {
	for _, it := range q.Items {
		if it.Ref == ref {
			return true
		}
	}
	return false
}

// validDecision reports whether d is one of the design section 8 closed set
// of item decisions. SaveDraft checks this itself, rather than leaving a
// bad value to fail later at the messages/answer schema (schema.go), so an
// invalid decision reports a clean 409 conflict, not a raw schema error.
func validDecision(d response.Decision) bool {
	switch d {
	case response.DecisionAccept, response.DecisionReject, response.DecisionDrop, response.DecisionDiscuss:
		return true
	default:
		return false
	}
}

// findDraftAnswerTx returns the ticket's existing draft "answer" row for
// questionID, if any: at most one can exist, since upsertOptionDraftTx and
// upsertItemDraftTx always update it in place rather than inserting a
// second one.
func findDraftAnswerTx(ctx context.Context, tx *sql.Tx, ticketID, questionID int64) (id int64, payload json.RawMessage, found bool, err error) {
	row := tx.QueryRowContext(ctx,
		`SELECT id, payload FROM messages WHERE ticket_id = ? AND parent_id = ? AND type = ? AND state = ?`,
		ticketID, questionID, msgTypeAnswer, draftState)
	var payloadStr sql.NullString
	if err = row.Scan(&id, &payloadStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("find draft answer: %w", err)
	}
	if payloadStr.Valid {
		payload = json.RawMessage(payloadStr.String)
	}
	return id, payload, true, nil
}

// upsertOptionDraftTx writes questionID's draft answer as {"option": option},
// replacing whatever draft answer (option or items) was there before (design
// section 6.7: "An option answer ... replaces the earlier draft"). Replaced
// reports whether an existing draft already held this exact option.
func (s *Store) upsertOptionDraftTx(ctx context.Context, tx *sql.Tx, ticketID, questionID int64, option string) (DraftResult, error) {
	existingID, existingPayload, found, err := findDraftAnswerTx(ctx, tx, ticketID, questionID)
	if err != nil {
		return DraftResult{}, err
	}

	newPayload, err := json.Marshal(response.AnswerPayload{Option: &option})
	if err != nil {
		return DraftResult{}, fmt.Errorf("save draft: marshal option answer: %w", err)
	}

	if !found {
		if insErr := s.insertMessageTx(ctx, tx, Message{
			TicketID: ticketID, ParentID: &questionID, Type: msgTypeAnswer, Author: authorYou,
			State: new(draftState), Payload: newPayload,
		}); insErr != nil {
			return DraftResult{}, fmt.Errorf("save draft: insert option answer: %w", insErr)
		}
		id, idErr := lastInsertIDTx(ctx, tx)
		if idErr != nil {
			return DraftResult{}, fmt.Errorf("save draft: %w", idErr)
		}
		return DraftResult{MessageID: id}, nil
	}

	var old response.AnswerPayload
	if decodeErr := json.Unmarshal(existingPayload, &old); decodeErr != nil {
		return DraftResult{}, fmt.Errorf("save draft: decode existing option answer: %w", decodeErr)
	}
	same := old.Option != nil && *old.Option == option && len(old.Items) == 0

	if err := updateDraftPayloadTx(ctx, tx, s.schemas, existingID, newPayload); err != nil {
		return DraftResult{}, err
	}
	return DraftResult{MessageID: existingID, Replaced: !same}, nil
}

// upsertItemDraftTx merges one ref->decision entry into questionID's draft
// answer items map, creating the draft answer row on the first pick (design
// section 6.7). Replaced reports whether ref already carried this exact
// decision.
func (s *Store) upsertItemDraftTx(ctx context.Context, tx *sql.Tx, ticketID, questionID int64, item ItemDecision) (DraftResult, error) {
	existingID, existingPayload, found, err := findDraftAnswerTx(ctx, tx, ticketID, questionID)
	if err != nil {
		return DraftResult{}, err
	}

	var payload response.AnswerPayload
	if found {
		if decodeErr := json.Unmarshal(existingPayload, &payload); decodeErr != nil {
			return DraftResult{}, fmt.Errorf("save draft: decode existing item answer: %w", decodeErr)
		}
	}
	same := found && payload.Items != nil && payload.Items[item.Ref] == item.Decision
	if payload.Items == nil {
		payload.Items = make(map[string]response.Decision, 1)
	}
	payload.Items[item.Ref] = item.Decision

	newPayload, err := json.Marshal(payload)
	if err != nil {
		return DraftResult{}, fmt.Errorf("save draft: marshal item answer: %w", err)
	}

	if !found {
		if insErr := s.insertMessageTx(ctx, tx, Message{
			TicketID: ticketID, ParentID: &questionID, Type: msgTypeAnswer, Author: authorYou,
			State: new(draftState), Payload: newPayload,
		}); insErr != nil {
			return DraftResult{}, fmt.Errorf("save draft: insert item answer: %w", insErr)
		}
		id, idErr := lastInsertIDTx(ctx, tx)
		if idErr != nil {
			return DraftResult{}, fmt.Errorf("save draft: %w", idErr)
		}
		return DraftResult{MessageID: id}, nil
	}

	if err := updateDraftPayloadTx(ctx, tx, s.schemas, existingID, newPayload); err != nil {
		return DraftResult{}, err
	}
	return DraftResult{MessageID: existingID, Replaced: !same}, nil
}

// updateDraftPayloadTx validates payload against the messages/answer schema
// (the same validator insertMessageTx uses) and writes it onto an existing
// draft row, id. Every draft-answer update goes through this one place, so
// an upsert can never write a payload insertMessageTx's own INSERT path
// would have rejected.
func updateDraftPayloadTx(ctx context.Context, tx *sql.Tx, schemas *schemaSet, id int64, payload []byte) error {
	if err := schemas.validate("messages", msgTypeAnswer, payload); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET payload = ? WHERE id = ?`, string(payload), id); err != nil {
		return fmt.Errorf("save draft: update answer: %w", err)
	}
	return nil
}

// findDraftReplyTx returns the ticket's existing draft "reply" row for
// parentID (nil for a thread reply, a question id for a question-targeted
// reply), if any: at most one can exist, since insertReplyDraftTx always
// updates it in place rather than inserting a second one. parentID needs its
// own IS NULL branch because SQL's parent_id = ? never matches a NULL
// column.
func findDraftReplyTx(ctx context.Context, tx *sql.Tx, ticketID int64, parentID *int64) (id int64, body string, found bool, err error) {
	var row *sql.Row
	if parentID == nil {
		row = tx.QueryRowContext(ctx,
			`SELECT id, body FROM messages WHERE ticket_id = ? AND parent_id IS NULL AND type = ? AND state = ?`,
			ticketID, msgTypeReply, draftState)
	} else {
		row = tx.QueryRowContext(ctx,
			`SELECT id, body FROM messages WHERE ticket_id = ? AND parent_id = ? AND type = ? AND state = ?`,
			ticketID, *parentID, msgTypeReply, draftState)
	}
	if err = row.Scan(&id, &body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", false, nil
		}
		return 0, "", false, fmt.Errorf("find draft reply: %w", err)
	}
	return id, body, true, nil
}

// insertReplyDraftTx upserts a draft "reply" row: parentID nil for a thread
// reply, or the question id for a question-targeted free reply (design
// section 6.7). It updates one existing draft reply row in place rather than
// always inserting, so a repeated Enter on the same free-text reply (the
// composer resubmits the whole draft on every keystroke commit) never
// accumulates duplicate rows that would all send. A reply carries no payload
// (messagePayloadTypes, store.go), only Body.
func (s *Store) insertReplyDraftTx(ctx context.Context, tx *sql.Tx, ticketID int64, parentID *int64, text string) (DraftResult, error) {
	existingID, existingBody, found, err := findDraftReplyTx(ctx, tx, ticketID, parentID)
	if err != nil {
		return DraftResult{}, err
	}

	if found {
		if _, updErr := tx.ExecContext(ctx, `UPDATE messages SET body = ? WHERE id = ?`, text, existingID); updErr != nil {
			return DraftResult{}, fmt.Errorf("save draft: update reply: %w", updErr)
		}
		return DraftResult{MessageID: existingID, Replaced: existingBody != text}, nil
	}

	if insErr := s.insertMessageTx(ctx, tx, Message{
		TicketID: ticketID, ParentID: parentID, Type: msgTypeReply, Author: authorYou,
		State: new(draftState), Body: text,
	}); insErr != nil {
		return DraftResult{}, fmt.Errorf("save draft: insert reply: %w", insErr)
	}
	id, err := lastInsertIDTx(ctx, tx)
	if err != nil {
		return DraftResult{}, fmt.Errorf("save draft: %w", err)
	}
	return DraftResult{MessageID: id}, nil
}

// kindForWaitReason maps a ticket.waiting_on value back to the question
// kind that set it, defined only over the six question-backed reasons
// (design section 6.7): "error" and "children" are not question-backed and
// have no kind, so SendBatch's wait-clearing step never looks either up.
func kindForWaitReason(reason string) (response.QuestionKind, bool) {
	if reason == waitingFlagQuestions {
		return response.QuestionKindQuestion, true
	}
	switch response.QuestionKind(reason) {
	case response.QuestionKindGate, response.QuestionKindSplit,
		response.QuestionKindPerimeter, response.QuestionKindReview, response.QuestionKindMerge:
		return response.QuestionKind(reason), true
	default:
		return "", false
	}
}

// SendBatch flips every draft for ticketID to sent under one batch_id, marks
// answered questions, and clears the matching question-backed wait when
// none of that kind is open (design section 6.7). It runs inside one BEGIN
// IMMEDIATE transaction (store.Open sets _txlock=immediate on the store's
// one connection), so it takes the write lock before it reads and
// max(batch_id)+1 is never racy.
func (s *Store) SendBatch(ctx context.Context, ticketID int64) (result BatchResult, err error) {
	// Named returns so one deferred call logs every branch's outcome
	// (CLAUDE.md: "log every major branch with the ids"), without a log
	// line at each of SendBatch's early returns.
	defer func() {
		logSendBatchOutcome(ctx, ticketID, result, err)
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchResult{}, fmt.Errorf("send batch: begin tx: %w", err)
	}
	defer rollback(tx)

	drafts, err := loadDraftsTx(ctx, tx, ticketID)
	if err != nil {
		return BatchResult{}, err
	}
	if len(drafts) == 0 {
		return BatchResult{Empty: true}, nil
	}

	valid, stale, questions, err := revalidateBatchTx(ctx, tx, ticketID, drafts)
	if err != nil {
		return BatchResult{}, err
	}
	if len(stale) > 0 {
		if delErr := deleteStaleDraftsTx(ctx, tx, stale); delErr != nil {
			return BatchResult{}, delErr
		}
	}
	if len(valid) == 0 {
		// Every draft in the batch turned out stale (PR review fix): discard
		// them (the DELETE above) and still reconcile the wait. Without this,
		// a ticket whose questions closed under a stale draft stays blocked
		// with waiting_on set and no run left to resume it (PR review fix).
		waitCleared, clearErr := clearMatchingWaitTx(ctx, tx, ticketID)
		if clearErr != nil {
			return BatchResult{}, clearErr
		}
		if err = tx.Commit(); err != nil {
			return BatchResult{}, fmt.Errorf("send batch: commit tx: %w", err)
		}
		return BatchResult{Discarded: len(stale), Empty: true, WaitCleared: waitCleared}, nil
	}

	var batchID int64
	if err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(batch_id), 0) + 1 FROM messages WHERE ticket_id = ?`, ticketID,
	).Scan(&batchID); err != nil {
		return BatchResult{}, fmt.Errorf("send batch: next batch id: %w", err)
	}

	for i := range valid {
		if _, err = tx.ExecContext(ctx,
			`UPDATE messages SET state = ?, batch_id = ? WHERE id = ?`, answerStateSent, batchID, valid[i].ID,
		); err != nil {
			return BatchResult{}, fmt.Errorf("send batch: flip draft %d to sent: %w", valid[i].ID, err)
		}
	}

	if markErr := markAnsweredQuestionsTx(ctx, tx, valid, questions); markErr != nil {
		return BatchResult{}, markErr
	}

	waitCleared, err := clearMatchingWaitTx(ctx, tx, ticketID)
	if err != nil {
		return BatchResult{}, err
	}

	if err = tx.Commit(); err != nil {
		return BatchResult{}, fmt.Errorf("send batch: commit tx: %w", err)
	}
	return BatchResult{Sent: len(valid), Discarded: len(stale), BatchID: batchID, WaitCleared: waitCleared}, nil
}

// logSendBatchOutcome logs SendBatch's major branches with their ids
// (CLAUDE.md: "log every major branch with the ids"): a successful send
// reports batch_id, how many drafts it sent, how many it discarded as stale,
// and whether it cleared the ticket's wait; an empty batch (nothing to send,
// possibly because every draft turned out stale) and a named conflict each
// get their own line. An unexpected (non-conflict) error is left to the
// caller: the console handler already logs it (internal/console/answer.go),
// so logging it again here would violate "log or return, never both".
func logSendBatchOutcome(ctx context.Context, ticketID int64, result BatchResult, err error) {
	if err != nil {
		if ce, ok := errors.AsType[*ConflictError](err); ok {
			slog.InfoContext(ctx, "send batch conflict", "ticket_id", ticketID, "reason", ce.Reason)
		}
		return
	}
	if result.Empty {
		slog.InfoContext(ctx, "send batch empty", "ticket_id", ticketID, "discarded", result.Discarded)
		return
	}
	slog.InfoContext(ctx, "batch sent",
		"ticket_id", ticketID, "batch_id", result.BatchID, "sent", result.Sent,
		"discarded", result.Discarded, "wait_cleared", result.WaitCleared)
}

// loadDraftsTx returns every draft answer or reply row for ticketID, in id
// order, the batch SendBatch sends.
func loadDraftsTx(ctx context.Context, tx *sql.Tx, ticketID int64) ([]MessageRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type IN (?, ?) AND state = ? ORDER BY id`,
		ticketID, msgTypeAnswer, msgTypeReply, draftState)
	if err != nil {
		return nil, fmt.Errorf("send batch: load drafts: %w", err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("send batch: load drafts: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("send batch: load drafts: %w", err)
	}
	return out, nil
}

// revalidateBatchTx re-checks every draft against its current question
// exactly as SaveDraft did when it was written (design section 6.7:
// "re-validates the whole batch against current questions"), splitting
// drafts into valid (send them) and stale (discard them) rather than
// rejecting the whole call on the first conflict it finds (PR review fix): a
// draft saved against a question that later closes -- or is deleted, or
// stops accepting the exact option/item the draft picked -- used to fail
// openQuestionForTicketTx's re-check with a *ConflictError, which SendBatch
// then let roll its entire transaction back, wedging every other draft on
// the ticket (including perfectly valid ones on other questions) behind a
// 409 that repeated forever, since the same stale draft failed the same
// re-check on every retry. Now any *ConflictError this function's own
// re-checks raise (question not found, wrong ticket, question closed,
// missing option, missing item) marks only the one draft that triggered it,
// and every other draft on the same now-stale question, as stale; the caller
// deletes those rows and sends the rest. A decode failure on a draft's own
// payload is not stale in this sense -- it is a hard error that still aborts
// the whole SendBatch call (returned, not appended to stale), since
// SaveDraft's own writers never produce a payload that fails to parse, so
// one here points at a bug or corruption this function should not paper
// over by silently discarding evidence of it.
//
// questions returned is keyed by id and holds only the questions a *valid*
// draft still touches, for markAnsweredQuestionsTx to reuse; a question
// backing only stale drafts is left out, since there is nothing left to mark
// answered against it.
func revalidateBatchTx(ctx context.Context, tx *sql.Tx, ticketID int64, drafts []MessageRow) (valid, stale []MessageRow, questions map[int64]response.QuestionPayload, err error) {
	questions = make(map[int64]response.QuestionPayload)
	staleQuestions := make(map[int64]bool)

	for i := range drafts {
		d := drafts[i]
		if d.ParentID == nil {
			valid = append(valid, d) // a thread reply targets no question
			continue
		}
		qid := *d.ParentID

		if staleQuestions[qid] {
			stale = append(stale, d)
			continue
		}
		payload, seen := questions[qid]
		if !seen {
			var openErr error
			payload, openErr = openQuestionForTicketTx(ctx, tx, qid, ticketID)
			if openErr != nil {
				if ce, isConflict := errors.AsType[*ConflictError](openErr); isConflict {
					slog.DebugContext(ctx, "send batch: question stale",
						"ticket_id", ticketID, "question_id", qid, "reason", ce.Reason)
					staleQuestions[qid] = true
					stale = append(stale, d)
					continue
				}
				return nil, nil, nil, openErr
			}
			questions[qid] = payload
		}

		if d.Type != msgTypeAnswer {
			valid = append(valid, d) // a question-targeted reply needs no further check
			continue
		}
		var ap response.AnswerPayload
		if unmarshalErr := json.Unmarshal(d.Payload, &ap); unmarshalErr != nil {
			return nil, nil, nil, fmt.Errorf("send batch: unmarshal draft %d: %w", d.ID, unmarshalErr)
		}
		optionStale := ap.Option != nil && !validOption(payload, *ap.Option)
		itemStale := false
		for ref := range ap.Items {
			if !validItemRef(payload, ref) {
				itemStale = true
				break
			}
		}
		if optionStale || itemStale {
			stale = append(stale, d)
			continue
		}
		valid = append(valid, d)
	}
	return valid, stale, questions, nil
}

// deleteStaleDraftsTx removes every row in stale from the messages table
// outright (PR review fix): a stale draft answered a question that is no
// longer open (or no longer exists), or picked an option/item that stopped
// being valid, so there is nothing left for it to mean. It is discarded, not
// flipped to sent or any other state.
func deleteStaleDraftsTx(ctx context.Context, tx *sql.Tx, stale []MessageRow) error {
	for i := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, stale[i].ID); err != nil {
			return fmt.Errorf("send batch: discard stale draft %d: %w", stale[i].ID, err)
		}
	}
	return nil
}

// sentItemDecisionsTx returns every already-sent item decision for question
// qid, merged ref->decision, across every "sent" answer row that targets it
// (parent_id = qid), not just the one this batch just sent: an item-kind
// question decided across two or more separate SendBatch calls needs every
// prior send's decisions counted for markAnsweredQuestionsTx to see it as
// complete.
func sentItemDecisionsTx(ctx context.Context, tx *sql.Tx, qid int64) (map[string]response.Decision, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT payload FROM messages WHERE parent_id = ? AND type = ? AND state = ?`,
		qid, msgTypeAnswer, answerStateSent)
	if err != nil {
		return nil, fmt.Errorf("send batch: list sent item decisions for question %d: %w", qid, err)
	}
	defer rows.Close()

	items := make(map[string]response.Decision)
	for rows.Next() {
		var payload sql.NullString
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("send batch: list sent item decisions for question %d: %w", qid, err)
		}
		if !payload.Valid {
			continue
		}
		var ap response.AnswerPayload
		if err := json.Unmarshal([]byte(payload.String), &ap); err != nil {
			return nil, fmt.Errorf("send batch: unmarshal sent answer for question %d: %w", qid, err)
		}
		maps.Copy(items, ap.Items)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("send batch: list sent item decisions for question %d: %w", qid, err)
	}
	return items, nil
}

// markAnsweredQuestionsTx flips a question to "answered" when the batch just
// sent gives it a sent option answer, a complete sent item answer (every
// item ref in the question's payload has a decision, counting decisions
// already sent in an earlier SendBatch call alongside this batch's own), or
// a sent reply (design section 6.7). An incomplete item answer sends its
// draft like any other but leaves the question open.
func markAnsweredQuestionsTx(ctx context.Context, tx *sql.Tx, drafts []MessageRow, questions map[int64]response.QuestionPayload) error {
	for qid, payload := range questions {
		answered := false
		items := make(map[string]response.Decision)
		if len(payload.Items) > 0 {
			// An item-kind question can be decided piecemeal across more
			// than one SendBatch call, so completeness has to count every
			// item ref ever sent for it, not only the ones in this batch.
			sent, err := sentItemDecisionsTx(ctx, tx, qid)
			if err != nil {
				return err
			}
			maps.Copy(items, sent)
		}
		for i := range drafts {
			d := &drafts[i]
			if d.ParentID == nil || *d.ParentID != qid {
				continue
			}
			switch d.Type {
			case msgTypeReply:
				answered = true
			case msgTypeAnswer:
				var ap response.AnswerPayload
				if err := json.Unmarshal(d.Payload, &ap); err != nil {
					return fmt.Errorf("send batch: unmarshal draft %d: %w", d.ID, err)
				}
				if ap.Option != nil {
					answered = true
				}
				maps.Copy(items, ap.Items)
			}
		}
		if !answered && len(payload.Items) > 0 && len(items) == len(payload.Items) {
			complete := true
			for _, it := range payload.Items {
				if _, ok := items[it.Ref]; !ok {
					complete = false
					break
				}
			}
			answered = complete
		}
		if !answered {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET state = ? WHERE id = ? AND type = ? AND state = ?`,
			questionStateAnswered, qid, msgTypeQuestion, questionStateOpen,
		); err != nil {
			return fmt.Errorf("send batch: mark question %d answered: %w", qid, err)
		}
	}
	return nil
}

// clearMatchingWaitTx clears ticketID's waiting_on, and reports true, only
// when it currently names one of the six question-backed reasons and no
// "open" question of that reason's kind remains (design section 6.7). It
// never touches "error" or "children", which are not question-backed.
func clearMatchingWaitTx(ctx context.Context, tx *sql.Tx, ticketID int64) (bool, error) {
	var waitingOn sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT waiting_on FROM tickets WHERE id = ?`, ticketID).Scan(&waitingOn); err != nil {
		return false, fmt.Errorf("send batch: get ticket %d waiting_on: %w", ticketID, err)
	}
	if !waitingOn.Valid {
		return false, nil
	}
	kind, ok := kindForWaitReason(waitingOn.String)
	if !ok {
		return false, nil // error or children: not question-backed, never cleared here
	}

	stillOpen, err := openQuestionOfKindExistsTx(ctx, tx, ticketID, kind)
	if err != nil {
		return false, err
	}
	if stillOpen {
		return false, nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET waiting_on = NULL WHERE id = ? AND waiting_on = ?`, ticketID, waitingOn.String)
	if err != nil {
		return false, fmt.Errorf("send batch: clear wait: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("send batch: clear wait: %w", err)
	}
	return n > 0, nil
}

// openQuestionOfKindExistsTx reports whether ticketID has any open question
// whose payload Kind equals kind. Kind is not its own column, so this reads
// every open question and decodes each payload rather than filtering in SQL.
func openQuestionOfKindExistsTx(ctx context.Context, tx *sql.Tx, ticketID int64, kind response.QuestionKind) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT payload FROM messages WHERE ticket_id = ? AND type = ? AND state = ?`,
		ticketID, msgTypeQuestion, questionStateOpen)
	if err != nil {
		return false, fmt.Errorf("send batch: list open questions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var payload sql.NullString
		if err := rows.Scan(&payload); err != nil {
			return false, fmt.Errorf("send batch: list open questions: %w", err)
		}
		if !payload.Valid {
			continue
		}
		var qp response.QuestionPayload
		if err := json.Unmarshal([]byte(payload.String), &qp); err != nil {
			return false, fmt.Errorf("send batch: unmarshal open question payload: %w", err)
		}
		if qp.Kind == kind {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("send batch: list open questions: %w", err)
	}
	return false, nil
}

// SetSettings writes one or more settings rows in a single transaction
// (design section 7.2: "a variadic key-value set in one transaction, for
// the atomic VAPID pair and the log level"). kvs alternates key, value,
// key, value...; an odd count is rejected before any write. Each pair
// upserts: an existing key (every row migrations/0001_init.sql seeds,
// including log_level) is updated in place, and a key with no row yet (the
// VAPID pair Task 11 adds) is inserted, so one call covers both shapes.
func (s *Store) SetSettings(ctx context.Context, kvs ...string) error {
	if len(kvs)%2 != 0 {
		return fmt.Errorf("set settings: odd number of key/value arguments (%d)", len(kvs))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set settings: begin tx: %w", err)
	}
	defer rollback(tx)

	for i := 0; i+1 < len(kvs); i += 2 {
		key, value := kvs[i], kvs[i+1]
		if _, execErr := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, value,
		); execErr != nil {
			return fmt.Errorf("set settings: %s: %w", key, execErr)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set settings: commit tx: %w", err)
	}
	return nil
}

// PushSubscription is one push_subscriptions row to upsert (design section
// 6.13): Endpoint is the subscription's https URL, validated by the console
// handler (push.go), not here, because the endpoint is its own column and a
// keys_json schema cannot see it; KeysJSON is the serialized {p256dh, auth}
// object, validated here against push_subscriptions/keys.
type PushSubscription struct {
	Endpoint string
	KeysJSON []byte
}

// UpsertPushSubscription validates sub.KeysJSON against the
// push_subscriptions/keys schema, then inserts or replaces sub by its
// unique Endpoint (design section 6.13: "It replaces by endpoint, so a
// re-subscribe is idempotent"), migrations/0001_init.sql's own UNIQUE
// constraint on push_subscriptions.endpoint backing the upsert.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub PushSubscription) error {
	if err := s.schemas.validate("push_subscriptions", "keys", sub.KeysJSON); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, keys_json) VALUES (?, ?)
		 ON CONFLICT(endpoint) DO UPDATE SET keys_json = excluded.keys_json`,
		sub.Endpoint, string(sub.KeysJSON),
	); err != nil {
		return fmt.Errorf("upsert push subscription: %w", err)
	}
	return nil
}

// MarkRead sets messageID's read_at to now (design section 6.8).
func (s *Store) MarkRead(ctx context.Context, messageID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET read_at = ? WHERE id = ?`, formatTime(time.Now()), messageID)
	if err != nil {
		return fmt.Errorf("mark read: message %d: %w", messageID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark read: message %d: %w", messageID, err)
	}
	if n == 0 {
		return fmt.Errorf("mark read: message %d: %w", messageID, sql.ErrNoRows)
	}
	return nil
}
