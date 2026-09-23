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
// sent, the batch_id it allocated, whether it cleared the ticket's wait, and
// whether there was nothing to send at all (design section 6.7).
type BatchResult struct {
	Sent        int
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
func (s *Store) SaveDraft(ctx context.Context, in DraftInput) (DraftResult, error) {
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

	var result DraftResult
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

// insertReplyDraftTx inserts a draft "reply" row: parentID nil for a thread
// reply, or the question id for a question-targeted free reply (design
// section 6.7). A reply carries no payload (messagePayloadTypes, store.go),
// only Body.
func (s *Store) insertReplyDraftTx(ctx context.Context, tx *sql.Tx, ticketID int64, parentID *int64, text string) (DraftResult, error) {
	if err := s.insertMessageTx(ctx, tx, Message{
		TicketID: ticketID, ParentID: parentID, Type: msgTypeReply, Author: authorYou,
		State: new(draftState), Body: text,
	}); err != nil {
		return DraftResult{}, fmt.Errorf("save draft: insert reply: %w", err)
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
func (s *Store) SendBatch(ctx context.Context, ticketID int64) (BatchResult, error) {
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

	questions, err := revalidateBatchTx(ctx, tx, ticketID, drafts)
	if err != nil {
		return BatchResult{}, err
	}

	var batchID int64
	if err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(batch_id), 0) + 1 FROM messages WHERE ticket_id = ?`, ticketID,
	).Scan(&batchID); err != nil {
		return BatchResult{}, fmt.Errorf("send batch: next batch id: %w", err)
	}

	for i := range drafts {
		if _, err = tx.ExecContext(ctx,
			`UPDATE messages SET state = ?, batch_id = ? WHERE id = ?`, answerStateSent, batchID, drafts[i].ID,
		); err != nil {
			return BatchResult{}, fmt.Errorf("send batch: flip draft %d to sent: %w", drafts[i].ID, err)
		}
	}

	if markErr := markAnsweredQuestionsTx(ctx, tx, drafts, questions); markErr != nil {
		return BatchResult{}, markErr
	}

	waitCleared, err := clearMatchingWaitTx(ctx, tx, ticketID)
	if err != nil {
		return BatchResult{}, err
	}

	if err = tx.Commit(); err != nil {
		return BatchResult{}, fmt.Errorf("send batch: commit tx: %w", err)
	}
	return BatchResult{Sent: len(drafts), BatchID: batchID, WaitCleared: waitCleared}, nil
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

// revalidateBatchTx re-checks every draft against its question exactly as
// SaveDraft did when it was written (design section 6.7: "re-validates the
// whole batch against current questions"), so a question that closed, or an
// option or item ref that stopped being valid, between the draft and the
// send is caught here rather than sent. It returns every distinct question
// the batch touches, keyed by id, for markAnsweredQuestionsTx to reuse.
func revalidateBatchTx(ctx context.Context, tx *sql.Tx, ticketID int64, drafts []MessageRow) (map[int64]response.QuestionPayload, error) {
	questions := make(map[int64]response.QuestionPayload)
	for i := range drafts {
		d := &drafts[i]
		if d.ParentID == nil {
			continue // a thread reply targets no question
		}
		qid := *d.ParentID
		payload, ok := questions[qid]
		if !ok {
			var err error
			payload, err = openQuestionForTicketTx(ctx, tx, qid, ticketID)
			if err != nil {
				return nil, err
			}
			questions[qid] = payload
		}
		if d.Type != msgTypeAnswer {
			continue // a question-targeted reply needs no further check
		}
		var ap response.AnswerPayload
		if err := json.Unmarshal(d.Payload, &ap); err != nil {
			return nil, fmt.Errorf("send batch: unmarshal draft %d: %w", d.ID, err)
		}
		if ap.Option != nil && !validOption(payload, *ap.Option) {
			return nil, conflict("missing option")
		}
		for ref := range ap.Items {
			if !validItemRef(payload, ref) {
				return nil, conflict("missing item")
			}
		}
	}
	return questions, nil
}

// markAnsweredQuestionsTx flips a question to "answered" when the batch just
// sent gives it a sent option answer, a complete sent item answer (every
// item ref in the question's payload has a decision), or a sent reply
// (design section 6.7). An incomplete item answer sends its draft like any
// other but leaves the question open.
func markAnsweredQuestionsTx(ctx context.Context, tx *sql.Tx, drafts []MessageRow, questions map[int64]response.QuestionPayload) error {
	for qid, payload := range questions {
		answered := false
		items := make(map[string]response.Decision)
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
