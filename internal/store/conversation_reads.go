// conversation_reads.go gives D31's planning-conversation model its reads
// (design section 22): the one SQL fragment that tells a planning question
// apart from every other question kind, the delivery watermark and the
// in-flight run's own batch, and the turn order a thread renders in. Every
// later read, and SendBatch (console_writes.go, task D31-3), share the one
// planningQuestionsSQL fragment, so no two ever classify a question
// differently.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"zing/internal/response"
)

// planningQuestionsSQL is the ids of ticket ?'s planning questions (design
// section 22.1): a "question" message with no parent, asked by a run on a
// "planning" job session, whose payload kind is "question". That excludes
// the gate (kind "gate"), every escalation-linked question (parent_id is
// the escalation's own id), classify questions (session job "classify"),
// and plan-review questions (session job "planreview").
const planningQuestionsSQL = `SELECT q.id FROM messages q
JOIN runs r ON r.id = q.run_id
JOIN sessions s ON s.id = r.session_id
WHERE q.ticket_id = ? AND q.type = 'question' AND q.parent_id IS NULL
  AND s.job = 'planning' AND json_extract(q.payload, '$.kind') = 'question'`

// conversationPendingPrefix and conversationDeliveredPrefix are the two
// system markers D31's delivery watermark reads and Reserve writes (design
// section 22.3): "conversation pending run <R> batch <B>" while a run
// carrying owner messages is in flight, "conversation delivered run <R>
// batch <B>" once its commit lands.
const (
	conversationPendingPrefix   = "conversation pending run "
	conversationDeliveredPrefix = "conversation delivered run "
)

// PlanningConversation is one ticket's planning threads (D31, section
// 22.3): every planning question, the delivery watermark W (the newest
// "conversation delivered" marker's batch, or 0 when there is none), and
// the in-flight run's own batch, when a planning run is reserved with no
// outcome yet.
type PlanningConversation struct {
	Threads   []Thread
	Delivered int64 // W
	InFlight  *InFlightRun
}

// Thread is one planning question's whole conversation (design section
// 22.3, 22.5): the question itself, its turns in display order (sent owner
// rows, agent replies, and the settling decision row, interleaved by the
// run that answered them), whether it is settled, and the decision text
// when it is.
type Thread struct {
	Question MessageRow
	Turns    []MessageRow
	Settled  bool
	Decision string // body of the zing-authored resolved row, "" when none
}

// InFlightRun is the newest planning run with no outcome yet (design
// section 22.3): RunID, and ThroughBatch, the batch it will have delivered
// once it commits -- the "conversation pending" marker's own batch, or the
// watermark W when it carries none because it received no owner message.
type InFlightRun struct {
	RunID        int64
	ThroughBatch int64
}

// conversationMarker is one parsed "conversation <pending|delivered> run
// <R> batch <B>" message body: RunID and Batch, plus the message's own id
// so a caller can order several the way they were written.
type conversationMarker struct {
	ID    int64
	RunID int64
	Batch int64
}

// parseConversationMarkerBody extracts <R> and <B> from a body of the
// shape "<prefix><R> batch <B>" (design section 22.3). ok is false for a
// body that does not start with prefix, or whose tail does not parse,
// rather than failing the whole read over one malformed marker.
func parseConversationMarkerBody(prefix, body string) (runID, batch int64, ok bool) {
	rest := strings.TrimPrefix(body, prefix)
	if rest == body {
		return 0, 0, false
	}
	runText, batchText, found := strings.Cut(rest, " batch ")
	if !found {
		return 0, 0, false
	}
	r, err := strconv.ParseInt(runText, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.ParseInt(batchText, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return r, b, true
}

// deliveredMarkers returns every "conversation delivered run <R> batch <B>"
// message on ticketID, oldest first by id -- the same order B grows in,
// since a delivered marker is only ever written once its run's own batch
// exceeds the watermark that came before it (design section 22.3, 22.4).
func (s *Store) deliveredMarkers(ctx context.Context, ticketID int64) ([]conversationMarker, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, body FROM messages WHERE ticket_id = ? AND type = ? AND author = ? AND body LIKE ? ORDER BY id`,
		ticketID, msgTypeUpdate, authorSystem, conversationDeliveredPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("delivered markers for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []conversationMarker
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, fmt.Errorf("delivered markers for ticket %d: %w", ticketID, err)
		}
		runID, batch, ok := parseConversationMarkerBody(conversationDeliveredPrefix, body)
		if !ok {
			continue // a malformed marker body is skipped, not a failed read
		}
		out = append(out, conversationMarker{ID: id, RunID: runID, Batch: batch})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delivered markers for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// inFlightPlanningRun returns the newest planning run with no outcome yet,
// and the batch it will have delivered once it commits (design section
// 22.3): the run's own "conversation pending" marker, or w when it carries
// none. ok is false, with no error, when no planning run is in flight.
func (s *Store) inFlightPlanningRun(ctx context.Context, ticketID, w int64) (run InFlightRun, ok bool, err error) {
	var runID int64
	err = s.db.QueryRowContext(ctx,
		`SELECT r.id FROM runs r JOIN sessions s ON s.id = r.session_id
		 WHERE s.ticket_id = ? AND s.job = 'planning' AND r.outcome IS NULL
		 ORDER BY r.id DESC LIMIT 1`, ticketID).Scan(&runID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return InFlightRun{}, false, nil
	case err != nil:
		return InFlightRun{}, false, fmt.Errorf("in-flight planning run for ticket %d: %w", ticketID, err)
	}

	through := w
	var body string
	err = s.db.QueryRowContext(ctx,
		`SELECT body FROM messages WHERE ticket_id = ? AND type = ? AND author = ? AND body LIKE ? ORDER BY id DESC LIMIT 1`,
		ticketID, msgTypeUpdate, authorSystem, conversationPendingPrefix+strconv.FormatInt(runID, 10)+" batch %",
	).Scan(&body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No pending marker: the run received no owner message, so it will
		// deliver nothing past w (design section 22.3).
	case err != nil:
		return InFlightRun{}, false, fmt.Errorf("pending marker for run %d: %w", runID, err)
	default:
		if _, b, parsed := parseConversationMarkerBody(conversationPendingPrefix, body); parsed {
			through = b
		}
	}
	return InFlightRun{RunID: runID, ThroughBatch: through}, true, nil
}

// planningQuestionRows returns every planning question of ticketID, in
// question id order (design section 22.1, 22.3).
func (s *Store) planningQuestionRows(ctx context.Context, ticketID int64) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE id IN (`+planningQuestionsSQL+`) ORDER BY id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("planning questions for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("planning questions for ticket %d: %w", ticketID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("planning questions for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// inClauseFor returns len(ids) "?" placeholders, comma-joined, and ids as
// bind args: the IN clause every bucketed-by-parent read below shares.
func inClauseFor(ids []int64) (clause string, args []any) {
	placeholders := make([]string, len(ids))
	args = make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ", "), args
}

// ownerRowsByQuestion returns every sent owner message (answer or reply,
// author you) whose parent is one of qIDs, bucketed by parent id, each
// bucket ordered (batch_id, id) ascending (design section 22.3's turn
// order).
func ownerRowsByQuestion(ctx context.Context, s *Store, ticketID int64, qIDs []int64) (map[int64][]MessageRow, error) {
	out := make(map[int64][]MessageRow)
	if len(qIDs) == 0 {
		return out, nil
	}
	inClause, idArgs := inClauseFor(qIDs)
	args := append([]any{ticketID, authorYou, msgTypeAnswer, msgTypeReply, answerStateSent}, idArgs...)
	query := `SELECT ` + messageColumns + ` FROM messages WHERE ticket_id = ? AND author = ? AND type IN (?, ?) AND state = ? AND parent_id IN (` + inClause + `) ORDER BY batch_id, id` //nolint:gosec // G202: messageColumns and inClause are both fixed text, no user input

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("owner rows by question: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("owner rows by question: %w", err)
		}
		if m.ParentID != nil {
			out[*m.ParentID] = append(out[*m.ParentID], m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("owner rows by question: %w", err)
	}
	return out, nil
}

// agentRowsByQuestion returns every zing-authored reply and resolved
// message (D31's agent reply and settling-decision rows) whose parent is
// one of qIDs, bucketed by parent id, each bucket ordered by id (design
// section 22.3's turn order: "that run's agent rows by id").
func agentRowsByQuestion(ctx context.Context, s *Store, ticketID int64, qIDs []int64) (map[int64][]MessageRow, error) {
	out := make(map[int64][]MessageRow)
	if len(qIDs) == 0 {
		return out, nil
	}
	inClause, idArgs := inClauseFor(qIDs)
	args := append([]any{ticketID, authorZing, msgTypeReply, msgTypeResolved}, idArgs...)
	query := `SELECT ` + messageColumns + ` FROM messages WHERE ticket_id = ? AND author = ? AND type IN (?, ?) AND parent_id IN (` + inClause + `) ORDER BY id` //nolint:gosec // G202: messageColumns and inClause are both fixed text, no user input

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("agent rows by question: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("agent rows by question: %w", err)
		}
		if m.ParentID != nil {
			out[*m.ParentID] = append(out[*m.ParentID], m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent rows by question: %w", err)
	}
	return out, nil
}

// threadTurns interleaves ownerRows and agentRows into one planning
// question's displayed turn order (design section 22.3): an owner row with
// batch b belongs to the first delivered marker whose batch is at least b,
// and is shown before that marker's run's own agent rows; within one run,
// owner rows come by (batch_id, id) -- the order they already arrive in --
// then that run's agent rows by id; an owner row past every marker's batch
// is undelivered and comes last. markers is assumed sorted ascending by
// Batch (deliveredMarkers returns it in that order, since B only grows).
// ownerRows must already be sorted (batch_id, id) ascending and agentRows
// by id ascending.
func threadTurns(markers []conversationMarker, ownerRows, agentRows []MessageRow) []MessageRow {
	type group struct {
		ownerRows []MessageRow
		agentRows []MessageRow
	}
	groups := make(map[int64]*group)
	seen := make(map[int64]bool)
	var order []int64
	getGroup := func(runID int64) *group {
		g, ok := groups[runID]
		if !ok {
			g = &group{}
			groups[runID] = g
		}
		if !seen[runID] {
			seen[runID] = true
			order = append(order, runID)
		}
		return g
	}

	// Establish every marker's run group up front, in marker (ascending
	// batch) order, so a run's group appears in chronological order even
	// when its own agent rows arrive empty-handed (it settled a thread with
	// no new owner message, design section 22.8).
	for _, m := range markers {
		getGroup(m.RunID)
	}

	var undelivered []MessageRow
	for i := range ownerRows {
		row := ownerRows[i]
		var b int64
		if row.BatchID != nil {
			b = *row.BatchID
		}
		assigned := false
		for _, m := range markers {
			if m.Batch >= b {
				getGroup(m.RunID).ownerRows = append(getGroup(m.RunID).ownerRows, row)
				assigned = true
				break
			}
		}
		if !assigned {
			undelivered = append(undelivered, row)
		}
	}

	for i := range agentRows {
		row := agentRows[i]
		if row.RunID == nil {
			continue // unreachable: every agent reply and resolved row carries its settling run's id
		}
		getGroup(*row.RunID).agentRows = append(getGroup(*row.RunID).agentRows, row)
	}

	// A run referenced only by an agent row (no delivered marker, because
	// it settled without receiving a new owner message) was appended to
	// order out of chronological place above; run ids only grow over a
	// session's life, so sorting order ascending recovers it.
	slices.Sort(order)

	var turns []MessageRow
	for _, runID := range order {
		g := groups[runID]
		turns = append(turns, g.ownerRows...)
		turns = append(turns, g.agentRows...)
	}
	turns = append(turns, undelivered...)
	return turns
}

// PlanningConversation returns ticketID's whole planning conversation
// (design section 22.3): every planning question with its turns in display
// order, the delivery watermark, and the in-flight run's own batch, when
// one is reserved.
func (s *Store) PlanningConversation(ctx context.Context, ticketID int64) (PlanningConversation, error) {
	markers, err := s.deliveredMarkers(ctx, ticketID)
	if err != nil {
		return PlanningConversation{}, fmt.Errorf("planning conversation for ticket %d: %w", ticketID, err)
	}
	var w int64
	if n := len(markers); n > 0 {
		w = markers[n-1].Batch
	}

	run, ok, err := s.inFlightPlanningRun(ctx, ticketID, w)
	if err != nil {
		return PlanningConversation{}, fmt.Errorf("planning conversation for ticket %d: %w", ticketID, err)
	}
	var inFlight *InFlightRun
	if ok {
		inFlight = &run
	}

	questions, err := s.planningQuestionRows(ctx, ticketID)
	if err != nil {
		return PlanningConversation{}, fmt.Errorf("planning conversation for ticket %d: %w", ticketID, err)
	}
	if len(questions) == 0 {
		return PlanningConversation{Delivered: w, InFlight: inFlight}, nil
	}

	qIDs := make([]int64, len(questions))
	for i := range questions {
		qIDs[i] = questions[i].ID
	}

	ownerByQ, err := ownerRowsByQuestion(ctx, s, ticketID, qIDs)
	if err != nil {
		return PlanningConversation{}, fmt.Errorf("planning conversation for ticket %d: %w", ticketID, err)
	}
	agentByQ, err := agentRowsByQuestion(ctx, s, ticketID, qIDs)
	if err != nil {
		return PlanningConversation{}, fmt.Errorf("planning conversation for ticket %d: %w", ticketID, err)
	}

	threads := make([]Thread, len(questions))
	for i := range questions {
		q := questions[i]
		decision := ""
		agentRows := agentByQ[q.ID]
		for j := range agentRows {
			if agentRows[j].Type == msgTypeResolved {
				decision = agentRows[j].Body
				break
			}
		}
		threads[i] = Thread{
			Question: q,
			Turns:    threadTurns(markers, ownerByQ[q.ID], agentByQ[q.ID]),
			Settled:  q.State != nil && *q.State == questionStateResolved,
			Decision: decision,
		}
	}

	return PlanningConversation{Threads: threads, Delivered: w, InFlight: inFlight}, nil
}

// Undelivered returns every sent owner row, across every unsettled thread,
// whose batch is above the watermark (design section 22.3, 22.4): the set
// a resume must carry, in (batch_id, id) order.
func (c PlanningConversation) Undelivered() []MessageRow {
	var out []MessageRow
	for i := range c.Threads {
		t := &c.Threads[i]
		if t.Settled {
			continue
		}
		for j := range t.Turns {
			row := t.Turns[j]
			if row.Author == authorYou && row.BatchID != nil && *row.BatchID > c.Delivered {
				out = append(out, row)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		bi, bj := *out[i].BatchID, *out[j].BatchID
		if bi != bj {
			return bi < bj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Unsettled returns every thread not yet settled, question id order.
func (c PlanningConversation) Unsettled() []Thread {
	var out []Thread
	for i := range c.Threads {
		if !c.Threads[i].Settled {
			out = append(out, c.Threads[i])
		}
	}
	return out
}

// ThreadByKey returns the thread whose question carries key (such as
// "Q7"), ok false when no planning question of this conversation does.
func (c PlanningConversation) ThreadByKey(key string) (Thread, bool) {
	for i := range c.Threads {
		if questionPayloadKey(c.Threads[i].Question) == key {
			return c.Threads[i], true
		}
	}
	return Thread{}, false
}

// questionPayloadKey decodes q's QuestionPayload and returns its Key, ""
// when the payload does not decode (unreachable for a real question row,
// every one validated against messages/question.json at insert).
func questionPayloadKey(q MessageRow) string {
	var p response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &p); err != nil {
		return ""
	}
	return p.Key
}
