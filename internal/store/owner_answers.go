// owner_answers.go gives job.specFor (internal/job/spec.go) the owner's
// answers to questions outside the planning conversation and the gate
// (design "Decisions and additions", OwnerAnswers selection): escalation
// answers, classify answers, and any other answered question D31's
// planning-conversation model does not already cover.
package store

import (
	"context"
	"fmt"
	"sort"

	"zing/internal/response"
)

// OwnerAnswer is one answered question outside the planning conversation
// and the gate, with the owner's sent rows on it (design shape).
type OwnerAnswer struct {
	Question   MessageRow
	Escalation bool
	Owner      []MessageRow
}

// OwnerAnswers returns every answered question on ticketID outside the
// planning conversation and the gate: each question with its sent owner
// answer and reply rows, question id order (design shape, "OwnerAnswers
// selection"). Escalation marks a question whose parent is an "escalation"
// message. A question with no owner rows is dropped.
func (s *Store) OwnerAnswers(ctx context.Context, ticketID int64) ([]OwnerAnswer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = ?
		 AND id NOT IN (`+planningQuestionsSQL+`) AND COALESCE(json_extract(payload, '$.kind'), '') != ?
		 ORDER BY id`,
		ticketID, msgTypeQuestion, ticketID, string(response.QuestionKindGate))
	if err != nil {
		return nil, fmt.Errorf("owner answers for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var questions []MessageRow
	for rows.Next() {
		m, scanErr := scanMessage(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("owner answers for ticket %d: %w", ticketID, scanErr)
		}
		questions = append(questions, m)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("owner answers for ticket %d: %w", ticketID, rowsErr)
	}
	if len(questions) == 0 {
		return nil, nil
	}

	qIDs := make([]int64, len(questions))
	for i := range questions {
		qIDs[i] = questions[i].ID
	}

	escalationIDs, err := escalationMessageIDs(ctx, s, ticketID, questions)
	if err != nil {
		return nil, fmt.Errorf("owner answers for ticket %d: %w", ticketID, err)
	}

	answers, err := messagesByParent(ctx, s, ticketID, msgTypeAnswer, qIDs)
	if err != nil {
		return nil, fmt.Errorf("owner answers for ticket %d: %w", ticketID, err)
	}
	replies, err := messagesByParent(ctx, s, ticketID, msgTypeReply, qIDs)
	if err != nil {
		return nil, fmt.Errorf("owner answers for ticket %d: %w", ticketID, err)
	}

	var out []OwnerAnswer
	for i := range questions {
		q := questions[i]
		owner := append([]MessageRow{}, answers[q.ID]...)
		owner = append(owner, replies[q.ID]...)
		if len(owner) == 0 {
			continue
		}
		sort.Slice(owner, func(i, j int) bool { return owner[i].ID < owner[j].ID })
		escalation := q.ParentID != nil && escalationIDs[*q.ParentID]
		out = append(out, OwnerAnswer{Question: q, Escalation: escalation, Owner: owner})
	}
	return out, nil
}

// escalationMessageIDs returns the ids, among questions' own parent_ids,
// that name a message of type "escalation" (design shape, "Escalation=true
// when the question's parent_id names a message of type escalation").
func escalationMessageIDs(ctx context.Context, s *Store, ticketID int64, questions []MessageRow) (map[int64]bool, error) {
	var parentIDs []int64
	for i := range questions {
		if questions[i].ParentID != nil {
			parentIDs = append(parentIDs, *questions[i].ParentID)
		}
	}
	escalationIDs := make(map[int64]bool)
	if len(parentIDs) == 0 {
		return escalationIDs, nil
	}

	inClause, idArgs := inClauseFor(parentIDs)
	args := append([]any{ticketID, msgTypeEscalation}, idArgs...)
	//nolint:gosec // G202: inClause is fixed text, no user input
	query := `SELECT id FROM messages WHERE ticket_id = ? AND type = ? AND id IN (` + inClause + `)`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("escalation message ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("escalation message ids: %w", scanErr)
		}
		escalationIDs[id] = true
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("escalation message ids: %w", rowsErr)
	}
	return escalationIDs, nil
}
