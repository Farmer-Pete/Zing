package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"zing/internal/response"
)

// Message types, authors, and the question lifecycle states this file reads
// or writes, named once so commit.go carries identifiers rather than
// repeated literals.
const (
	msgTypeQuestion = "question"
	msgTypeAnswer   = "answer"
	msgTypeState    = "state"
	msgTypeResolved = "resolved"

	authorSystem = "system"
	authorYou    = "you"

	answerStateSent = "sent"

	questionStateOpen     = "open"
	questionStateAnswered = "answered"
	questionStateResolved = "resolved"
)

// errRunNeedsSession is returned when a HandlerCommit carries a Run but no
// Session: every run belongs to a session, so there is nothing to set its
// session_id to.
var errRunNeedsSession = errors.New("commit handler result: a run requires Session to be set")

// HandlerCommit describes what a job handler wants persisted for one ticket,
// and where the ticket goes next. CommitHandlerResult applies it in one
// fenced, atomic transaction (section 6.3).
type HandlerCommit struct {
	TicketID int64
	Owner    string
	Expires  time.Time // the exact lease the dispatcher claimed with (the fence)

	Next   string // "" keeps the state
	Reason string // required when Next != ""

	Waiting *string // nil clears waiting_on

	Session *SessionUpsert // nil, or create-or-update the run's session
	Runs    []Run          // run rows to insert (SessionID filled from Session after upsert)

	Messages         []Message // messages to insert (question, escalation, ...)
	AttachRunToMsgs  bool      // when true, every inserted message takes the id of the single inserted run
	ResolveQuestions []int64   // set each question's state to "resolved" and insert one "resolved" message per id
}

// SessionUpsert creates or updates the session a HandlerCommit's runs belong
// to. ID nil creates a new session; a non-nil ID updates the existing one.
type SessionUpsert struct {
	ID          *int64 // nil creates a new session; non-nil updates the existing one
	Job         string
	Runtime     string
	ExternalID  *string // set on create
	BumpResumes bool    // increment resumes on a resume commit
}

// CommitHandlerResult applies a validated handler commit under ownership
// fencing. It updates the ticket WHERE id=? AND claim_owner=? AND
// claim_expires_at=?. Zero rows affected means the lease was lost; it rolls
// back and returns applied=false, err=nil.
//
// Order inside the transaction (section 6.3): verify the fence; upsert the
// session and learn its id; insert the runs and learn their ids; insert the
// messages, attaching the single run's id when AttachRunToMsgs is set;
// resolve each ResolveQuestions id and insert its resolved message; write the
// state message; then apply Next, Waiting, and the claim clear in the one
// fenced ticket UPDATE that also serves as the final fence check.
func (s *Store) CommitHandlerResult(ctx context.Context, c HandlerCommit) (bool, error) {
	if c.AttachRunToMsgs && len(c.Runs) != 1 {
		return false, fmt.Errorf("commit handler result: AttachRunToMsgs requires exactly one run, got %d", len(c.Runs))
	}
	if c.Next != "" && c.Reason == "" {
		return false, fmt.Errorf("commit handler result: reason is required when transitioning to %s", c.Next)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("commit handler result: begin tx: %w", err)
	}
	defer rollback(tx)

	row := tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, c.TicketID)
	ticket, err := scanTicket(row)
	if err != nil {
		return false, fmt.Errorf("commit handler result: get ticket %d: %w", c.TicketID, err)
	}

	if !leaseMatches(ticket, c.Owner, c.Expires) {
		return false, nil // the lease was lost; the deferred rollback discards the read-only tx
	}

	var sessionID int64
	haveSession := false
	if c.Session != nil {
		sessionID, err = upsertSessionTx(ctx, tx, c.TicketID, *c.Session)
		if err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
		haveSession = true
	}

	runIDs := make([]int64, 0, len(c.Runs))
	for _, r := range c.Runs {
		if !haveSession {
			return false, errRunNeedsSession
		}
		var id int64
		id, err = insertRunTx(ctx, tx, sessionID, r)
		if err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
		runIDs = append(runIDs, id)
	}

	var attachRunID *int64
	if c.AttachRunToMsgs {
		attachRunID = &runIDs[0]
	}
	for _, m := range c.Messages {
		if attachRunID != nil {
			m.RunID = attachRunID
		}
		if err = s.insertMessageTx(ctx, tx, m); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	for _, qid := range c.ResolveQuestions {
		if err = resolveQuestionTx(ctx, tx, qid); err != nil {
			return false, fmt.Errorf("commit handler result: resolve question %d: %w", qid, err)
		}
		if err = s.insertMessageTx(ctx, tx, Message{
			TicketID: c.TicketID, ParentID: &qid, Type: msgTypeResolved, Author: authorSystem,
		}); err != nil {
			return false, fmt.Errorf("commit handler result: insert resolved message for question %d: %w", qid, err)
		}
	}

	if c.Next != "" {
		var payload []byte
		payload, err = json.Marshal(response.StatePayload{
			From: response.TicketState(ticket.State), To: response.TicketState(c.Next), Reason: c.Reason,
		})
		if err != nil {
			return false, fmt.Errorf("commit handler result: marshal state payload: %w", err)
		}
		if err = s.insertMessageTx(ctx, tx, Message{
			TicketID: c.TicketID, Type: msgTypeState, Author: authorSystem, Payload: payload,
		}); err != nil {
			return false, fmt.Errorf("commit handler result: insert state message: %w", err)
		}
	}

	var res sql.Result
	res, err = tx.ExecContext(ctx,
		`UPDATE tickets SET
			state = CASE WHEN ? <> '' THEN ? ELSE state END,
			waiting_on = ?,
			claim_owner = NULL,
			claim_expires_at = NULL
		 WHERE id = ? AND claim_owner = ? AND claim_expires_at = ?`,
		c.Next, c.Next, c.Waiting, c.TicketID, c.Owner, formatTime(c.Expires),
	)
	if err != nil {
		return false, fmt.Errorf("commit handler result: update ticket: %w", err)
	}
	var n int64
	n, err = res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("commit handler result: update ticket: %w", err)
	}
	if n == 0 {
		return false, nil // the lease changed out from under this commit; roll back
	}

	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("commit handler result: commit tx: %w", err)
	}
	return true, nil
}

// rollback rolls tx back, ignoring the "already committed" case: every
// exported method here defers this immediately after a successful BeginTx,
// so it runs as a harmless no-op on the success path (after tx.Commit) and
// as the actual undo on every early return.
func rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Warn("rollback failed", "err", err)
	}
}

// leaseMatches reports whether t is currently held by owner with exactly
// expires as its claim expiry, the fence CommitHandlerResult checks before
// writing anything.
func leaseMatches(t Ticket, owner string, expires time.Time) bool {
	return t.ClaimOwner != nil && *t.ClaimOwner == owner &&
		t.ClaimExpiresAt != nil && t.ClaimExpiresAt.Equal(expires)
}

// upsertSessionTx creates su's session when su.ID is nil, or bumps its
// resumes counter in place when BumpResumes is set on an existing session,
// and returns the session's id either way.
func upsertSessionTx(ctx context.Context, tx *sql.Tx, ticketID int64, su SessionUpsert) (int64, error) {
	if su.ID != nil {
		if su.BumpResumes {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET resumes = resumes + 1 WHERE id = ?`, *su.ID); err != nil {
				return 0, fmt.Errorf("bump session resumes: %w", err)
			}
		}
		return *su.ID, nil
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (ticket_id, job, runtime, external_id) VALUES (?, ?, ?, ?)`,
		ticketID, su.Job, su.Runtime, su.ExternalID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert session: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert session: %w", err)
	}
	return id, nil
}

// insertRunTx inserts one runs row under sessionID and returns its id.
func insertRunTx(ctx context.Context, tx *sql.Tx, sessionID int64, r Run) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO runs (session_id, turn, lens, task_n, model, outcome, agent_seconds, exit_code)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, r.Turn, r.Lens, r.TaskN, r.Model, r.Outcome, r.AgentSeconds, r.ExitCode,
	)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	return id, nil
}

// resolveQuestionTx sets one question message's lifecycle state to
// "resolved".
func resolveQuestionTx(ctx context.Context, tx *sql.Tx, questionID int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE id = ? AND type = ?`,
		questionStateResolved, questionID, msgTypeQuestion)
	if err != nil {
		return fmt.Errorf("update question state: %w", err)
	}
	return nil
}

// insertMessageTx is InsertMessage (store.go), tx-scoped: it validates
// m.Payload against the same in-package schema validator and inserts
// against tx instead of s.db, so a commit's messages share one transaction
// with the rest of the write. The exported InsertMessage takes no *sql.Tx
// (section 6.3), so CommitHandlerResult and AnswerQuestion cannot call it
// directly.
func (s *Store) insertMessageTx(ctx context.Context, tx *sql.Tx, m Message) error {
	var payloadParam *string
	if messagePayloadTypes[m.Type] {
		if len(m.Payload) == 0 {
			return fmt.Errorf("message type %s requires a payload", m.Type)
		}
		if err := s.schemas.validate("messages", m.Type, m.Payload); err != nil {
			return err
		}
		text := string(m.Payload)
		payloadParam = &text
	} else if len(m.Payload) != 0 {
		return fmt.Errorf("message type %s takes no payload", m.Type)
	}

	_, err := tx.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, run_id, parent_id, type, author, state, body, payload, batch_id, read_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.TicketID, m.RunID, m.ParentID, m.Type, m.Author, m.State, m.Body, payloadParam, m.BatchID, formatTimePtr(m.ReadAt),
	)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// AnswerInput is one answer submission: the option key chosen for one
// question on one ticket.
type AnswerInput struct {
	TicketID   int64
	QuestionID int64
	Option     string
}

// AnswerResult reports whether AnswerQuestion accepted the answer, whether
// accepting it cleared the ticket's wait, and, on rejection, why.
type AnswerResult struct {
	Accepted    bool
	WaitCleared bool
	Conflict    string
}

// answerConflict is a rejected AnswerResult paired with a nil error, the
// shape every named-conflict return path in AnswerQuestion shares.
func answerConflict(reason string) (AnswerResult, error) {
	return AnswerResult{Conflict: reason}, nil
}

// answerQuestionPayload is the subset of QuestionPayload (response package)
// AnswerQuestion needs to validate an option against: whether the question
// carries any options at all, and which keys are legal.
type answerQuestionPayload struct {
	Options []struct {
		Key string `json:"key"`
	} `json:"options"`
}

// AnswerQuestion records one answer, flips the question, and clears the wait
// only when the batch is done (section 6.3). It checks, inside one
// transaction: the question exists, its ticket matches, it is still open,
// its payload carries at least one option, and the chosen option is one of
// them.
func (s *Store) AnswerQuestion(ctx context.Context, in AnswerInput) (AnswerResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: begin tx: %w", err)
	}
	defer rollback(tx)

	row := tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, in.QuestionID)
	q, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AnswerResult{}, fmt.Errorf("answer question: question %d: %w", in.QuestionID, sql.ErrNoRows)
		}
		return AnswerResult{}, fmt.Errorf("answer question: get question %d: %w", in.QuestionID, err)
	}
	if q.Type != msgTypeQuestion {
		return AnswerResult{}, fmt.Errorf("answer question: message %d is type %s, not question", in.QuestionID, q.Type)
	}

	if q.TicketID != in.TicketID {
		return answerConflict("wrong ticket")
	}

	switch {
	case q.State == nil:
		return AnswerResult{}, fmt.Errorf("answer question: question %d has no state", in.QuestionID)
	case *q.State == questionStateAnswered:
		return answerConflict("already answered")
	case *q.State == questionStateResolved:
		return answerConflict("question closed")
	case *q.State != questionStateOpen:
		return AnswerResult{}, fmt.Errorf("answer question: question %d has an unexpected state %q", in.QuestionID, *q.State)
	}

	var payload answerQuestionPayload
	err = json.Unmarshal(q.Payload, &payload)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: unmarshal question %d payload: %w", in.QuestionID, err)
	}
	if len(payload.Options) == 0 {
		return answerConflict("free-text not supported")
	}
	optionValid := false
	for _, opt := range payload.Options {
		if opt.Key == in.Option {
			optionValid = true
			break
		}
	}
	if !optionValid {
		return answerConflict("missing option")
	}

	var batchID int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(batch_id), 0) + 1 FROM messages WHERE ticket_id = ?`, in.TicketID).Scan(&batchID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: next batch id: %w", err)
	}

	var answerPayload []byte
	answerPayload, err = json.Marshal(response.AnswerPayload{Option: &in.Option})
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: marshal answer payload: %w", err)
	}
	err = s.insertMessageTx(ctx, tx, Message{
		TicketID: in.TicketID, ParentID: &in.QuestionID, Type: msgTypeAnswer, Author: authorYou,
		State: new(answerStateSent), Payload: answerPayload, BatchID: &batchID,
	})
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: insert answer message: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE id = ?`, questionStateAnswered, in.QuestionID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: mark question %d answered: %w", in.QuestionID, err)
	}

	var openSiblings int
	openSiblings, err = countOpenBatchSiblings(ctx, tx, q.RunID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: %w", err)
	}

	waitCleared := false
	if openSiblings == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE tickets SET waiting_on = NULL WHERE id = ?`, in.TicketID)
		if err != nil {
			return AnswerResult{}, fmt.Errorf("answer question: clear wait: %w", err)
		}
		waitCleared = true
	}

	if err = tx.Commit(); err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: commit tx: %w", err)
	}
	return AnswerResult{Accepted: true, WaitCleared: waitCleared}, nil
}

// countOpenBatchSiblings counts questions still "open" that share runID, the
// batch an answered question belongs to (section 6.3, section 6.6).
func countOpenBatchSiblings(ctx context.Context, tx *sql.Tx, runID *int64) (int, error) {
	var n int
	var err error
	if runID == nil {
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE type = ? AND state = ? AND run_id IS NULL`,
			msgTypeQuestion, questionStateOpen,
		).Scan(&n)
	} else {
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE type = ? AND state = ? AND run_id = ?`,
			msgTypeQuestion, questionStateOpen, *runID,
		).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("count open batch siblings: %w", err)
	}
	return n, nil
}
