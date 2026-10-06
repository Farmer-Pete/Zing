package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetTicket reads the ticket with id, or a wrapped sql.ErrNoRows if none exists.
func (s *Store) GetTicket(ctx context.Context, id int64) (Ticket, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, id)
	t, err := scanTicket(row)
	if err != nil {
		return Ticket{}, fmt.Errorf("get ticket %d: %w", id, err)
	}
	return t, nil
}

// TicketByRef finds the ticket for (projectID, ref), the intake dedup check.
// A missing ticket is not an error: ok is false and err is nil.
func (s *Store) TicketByRef(ctx context.Context, projectID int64, ref string) (Ticket, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ticketColumns+` FROM tickets WHERE project_id = ? AND tracker_ref = ?`, projectID, ref)
	t, err := scanTicket(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Ticket{}, false, nil
		}
		return Ticket{}, false, fmt.Errorf("ticket by ref %s: %w", ref, err)
	}
	return t, true, nil
}

// ListAllTickets returns every ticket, ordered by id, for the console.
func (s *Store) ListAllTickets(ctx context.Context) ([]Ticket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketColumns+` FROM tickets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list all tickets: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("list all tickets: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all tickets: %w", err)
	}
	return out, nil
}

// ListReadyCandidates returns every unclaimed, non-terminal ticket that is
// dispatchable at now (design section 4.2, D21/OQ7):
//
//	(waiting_on IS NULL AND (next_poll_at IS NULL OR next_poll_at <= now))
//	OR (waiting_on = 'merge' AND next_poll_at IS NOT NULL AND next_poll_at <= now)
//	OR (waiting_on IS NULL AND EXISTS an answered question of the ticket)
//
// and NOT parked: no run on the ticket's sessions has a capped_until still
// after now (#45, ParkRuns) -- a ticket a Claude session limit parked is not
// a dispatch candidate again until its reset passes. Ordered by id for a
// deterministic result. The first clause is the ordinary case: a ticket not
// waiting on anything is a candidate right away unless it carries a future
// poll schedule (shipping's own backoff, 8.3). The second clause is what
// keeps a ticket waiting on merge a candidate while its poll is due, so a
// merge on GitHub or a reopened loop is still seen (D21). The third clause
// lets an answered question skip a poll schedule that has not come due yet
// (merge's own question is answered through SendBatch, which already clears
// waiting_on itself, design section 6.7) -- without it, the owner's answer
// would otherwise wait out whatever backoff interval was in force when the
// question was asked. The store applies no priority order: tracker_ref is
// TEXT, so SQL would sort "fake#10" before "fake#2"; the dispatcher parses
// the numeric external id and orders candidates in Go (section 6.2).
func (s *Store) ListReadyCandidates(ctx context.Context, terminal []string, now time.Time) ([]Ticket, error) {
	nowStr := formatTime(now)
	query := `SELECT ` + ticketColumns + ` FROM tickets WHERE claim_owner IS NULL`
	args := make([]any, 0, len(terminal)+2)
	if len(terminal) > 0 {
		placeholders := make([]string, len(terminal))
		for i, state := range terminal {
			placeholders[i] = "?"
			args = append(args, state)
		}
		// The dynamic part is a fixed number of "?" placeholders, one per
		// terminal state; every value rides as a bind argument below, never
		// concatenated into the query text.
		query += ` AND state NOT IN (` + strings.Join(placeholders, ", ") + `)` //nolint:gosec // G202: placeholders only, values are bind args
	}
	query += ` AND (
		(waiting_on IS NULL AND (next_poll_at IS NULL OR next_poll_at <= ?))
		OR (waiting_on = 'merge' AND next_poll_at IS NOT NULL AND next_poll_at <= ?)
		OR (waiting_on IS NULL AND EXISTS (
			SELECT 1 FROM messages
			WHERE messages.ticket_id = tickets.id AND messages.type = ? AND messages.state = ?
		))
	) AND NOT EXISTS (
		SELECT 1 FROM runs r JOIN sessions s ON s.id = r.session_id
		WHERE s.ticket_id = tickets.id AND r.capped_until IS NOT NULL AND r.capped_until > ?
	) ORDER BY id`
	args = append(args, nowStr, nowStr, msgTypeQuestion, questionStateAnswered, nowStr)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list ready candidates: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("list ready candidates: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ready candidates: %w", err)
	}
	return out, nil
}

// ListMessages returns every message for ticketID, ordered by id ascending.
func (s *Store) ListMessages(ctx context.Context, ticketID int64) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? ORDER BY id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list messages for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list messages for ticket %d: %w", ticketID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// OpenSession returns the newest session for (ticketID, job), the resume
// lookup a planning (or building) handler makes to decide first-entry
// versus resume. ok is false, with no error, when no such session exists.
func (s *Store) OpenSession(ctx context.Context, ticketID int64, job string) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, ticket_id, job, runtime, external_id, resumes
		 FROM sessions WHERE ticket_id = ? AND job = ? ORDER BY id DESC LIMIT 1`,
		ticketID, job)

	var sess Session
	var externalID sql.NullString
	err := row.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("open session for ticket %d job %s: %w", ticketID, job, err)
	}
	if externalID.Valid {
		sess.ExternalID = &externalID.String
	}
	return sess, true, nil
}

// SessionState classifies LatestSession's newest session for (ticketID, job)
// against maxResumes (design D17, section 4.5).
type SessionState int

const (
	SessionNone      SessionState = iota // no session exists yet
	SessionIdless                        // newest session has external_id NULL
	SessionOpen                          // external_id set and resumes < maxResumes
	SessionExhausted                     // external_id set and resumes >= maxResumes
)

// LatestSession returns the newest session for (ticketID, job), by id, and
// classifies it against maxResumes (design D17): SessionNone when none
// exists; SessionIdless when its external_id is still NULL (a first turn
// that never got far enough for the runtime to echo one back, so step 3
// treats it the same as none); SessionOpen when it has an external_id and
// fewer than maxResumes resumes; SessionExhausted when it has an external_id
// and resumes is at or past maxResumes. A ticket with several sessions for
// the same job (one per fresh entry, section 5.1) is judged by its highest
// id, never by which older one happens to still be open.
//
// A scanned external_id of "" is impossible in a healthy database (F035:
// migration 0003's triggers forbid it, and upsertSessionTx rejects it before
// any commit can write one), so classifySession treats it as a store error
// rather than silently classifying it as idless, open, or exhausted.
func (s *Store) LatestSession(ctx context.Context, ticketID int64, job string, maxResumes int) (Session, SessionState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, ticket_id, job, runtime, external_id, resumes
		 FROM sessions WHERE ticket_id = ? AND job = ? ORDER BY id DESC LIMIT 1`,
		ticketID, job)

	var sess Session
	var externalID sql.NullString
	err := row.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, SessionNone, nil
		}
		return Session{}, SessionNone, fmt.Errorf("latest session for ticket %d job %s: %w", ticketID, job, err)
	}
	if externalID.Valid {
		sess.ExternalID = &externalID.String
	}

	state, err := classifySession(sess, maxResumes)
	if err != nil {
		return Session{}, SessionNone, err
	}
	return sess, state, nil
}

// SessionNewestRun returns the newest run (by turn) of sessionID: design
// section 7.4's own "latest run" for an answered-round resume (building.go's
// resumeBuildRound, judging.go's resumeAnswered, respond.go's
// resumeRespondAnswered), each of which already has the session by id
// (SessionByID) and needs only its newest run's Interrupted flag to decide
// resumeCharge. ok is false, with no error, when the session has no runs
// yet (never expected in practice: a round's own session always has at
// least the run that asked the question).
func (s *Store) SessionNewestRun(ctx context.Context, sessionID int64) (Run, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE session_id = ? ORDER BY turn DESC LIMIT 1`, sessionID)
	r, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, false, nil
		}
		return Run{}, false, fmt.Errorf("newest run for session %d: %w", sessionID, err)
	}
	return r, true, nil
}

// SessionByID returns the session with id, classified against maxResumes
// the same way LatestSession classifies its own newest session (review
// F045): a build or perimeter round resumes round.SessionID directly (plan
// section 6.2), not necessarily the ticket's newest session for the job,
// since an older round's own session need not still be the newest one on
// the ticket by the time the owner answers it. err wraps sql.ErrNoRows when
// id names no session.
func (s *Store) SessionByID(ctx context.Context, id int64, maxResumes int) (Session, SessionState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, ticket_id, job, runtime, external_id, resumes FROM sessions WHERE id = ?`, id)

	var sess Session
	var externalID sql.NullString
	err := row.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes)
	if err != nil {
		return Session{}, SessionNone, fmt.Errorf("session %d: %w", id, err)
	}
	if externalID.Valid {
		sess.ExternalID = &externalID.String
	}

	state, err := classifySession(sess, maxResumes)
	if err != nil {
		return Session{}, SessionNone, err
	}
	return sess, state, nil
}

// classifySession is the state classification LatestSession and SessionByID
// both apply to an already-scanned session row (design D17): SessionIdless
// when ExternalID is still nil (a first turn that never got far enough for
// the runtime to echo one back), SessionExhausted when Resumes is at or
// past maxResumes, SessionOpen otherwise. It also carries the empty-
// external-id case, a store error rather than a silent misclassification
// (see the doc above LatestSession).
func classifySession(sess Session, maxResumes int) (SessionState, error) {
	switch {
	case sess.ExternalID != nil && *sess.ExternalID == "":
		return SessionNone, fmt.Errorf("store: session %d has an empty external_id", sess.ID)
	case sess.ExternalID == nil:
		return SessionIdless, nil
	case sess.Resumes >= maxResumes:
		return SessionExhausted, nil
	default:
		return SessionOpen, nil
	}
}

// QuestionsByState returns every "question" message on ticketID whose
// messages.state equals state (the canonical question lifecycle value,
// section 8), ordered by id.
func (s *Store) QuestionsByState(ctx context.Context, ticketID int64, state string) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = 'question' AND state = ? ORDER BY id`,
		ticketID, state)
	if err != nil {
		return nil, fmt.Errorf("questions by state %s for ticket %d: %w", state, ticketID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("questions by state %s for ticket %d: %w", state, ticketID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("questions by state %s for ticket %d: %w", state, ticketID, err)
	}
	return out, nil
}

// runColumns is the runs column list, in table-declaration order, including
// migration 0005's four interrupt/identity columns (design section 5.1) and
// migration 0009's capped_until (#45).
const runColumns = `id, session_id, turn, lens, task_n, model, outcome, agent_seconds, exit_code, interrupted, pgid, proc_start, started_at, capped_until`

// scanRun scans one row of runColumns, in that order, into a Run.
func scanRun(rs rowScanner) (Run, error) {
	var r Run
	var lens, model, outcome sql.NullString
	var taskN, agentSeconds, exitCode, pgid sql.NullInt64
	var interrupted int
	var procStart, startedAt, cappedUntil sql.NullString

	if err := rs.Scan(
		&r.ID, &r.SessionID, &r.Turn, &lens, &taskN, &model, &outcome, &agentSeconds, &exitCode,
		&interrupted, &pgid, &procStart, &startedAt, &cappedUntil,
	); err != nil {
		return Run{}, err
	}
	if lens.Valid {
		r.Lens = &lens.String
	}
	if taskN.Valid {
		n := int(taskN.Int64)
		r.TaskN = &n
	}
	if model.Valid {
		r.Model = &model.String
	}
	if outcome.Valid {
		r.Outcome = &outcome.String
	}
	if agentSeconds.Valid {
		n := int(agentSeconds.Int64)
		r.AgentSeconds = &n
	}
	if exitCode.Valid {
		n := int(exitCode.Int64)
		r.ExitCode = &n
	}
	r.Interrupted = interrupted != 0
	if pgid.Valid {
		n := int(pgid.Int64)
		r.PGID = &n
	}
	if procStart.Valid {
		r.ProcStart = &procStart.String
	}
	if startedAt.Valid {
		ts, err := time.Parse(fixedTimeLayout, startedAt.String)
		if err != nil {
			return Run{}, fmt.Errorf("parse started_at: %w", err)
		}
		r.StartedAt = &ts
	}
	if cappedUntil.Valid {
		ts, err := time.Parse(fixedTimeLayout, cappedUntil.String)
		if err != nil {
			return Run{}, fmt.Errorf("parse capped_until: %w", err)
		}
		r.CappedUntil = &ts
	}
	return r, nil
}

// FirstRun returns the session's turn-0 run: the row with the lowest turn
// for sessionID (design section 6.6). ok is false, with no error, when the
// session has no runs yet.
func (s *Store) FirstRun(ctx context.Context, sessionID int64) (Run, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE session_id = ? ORDER BY turn ASC LIMIT 1`, sessionID)
	r, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, false, nil
		}
		return Run{}, false, fmt.Errorf("first run for session %d: %w", sessionID, err)
	}
	return r, true, nil
}

// QuestionsByRun returns every "question" message attached to runID whose
// messages.state equals state (the canonical question lifecycle value,
// section 8), ordered by id. Planning's resume reads this to scope its
// answered batch to the session's turn-0 run, rather than to every answered
// question on the ticket (section 6.6).
func (s *Store) QuestionsByRun(ctx context.Context, runID int64, state string) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE run_id = ? AND type = 'question' AND state = ? ORDER BY id`,
		runID, state)
	if err != nil {
		return nil, fmt.Errorf("questions by run %d state %s: %w", runID, state, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("questions by run %d state %s: %w", runID, state, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("questions by run %d state %s: %w", runID, state, err)
	}
	return out, nil
}

// OpenRunIDs returns the set of every run whose outcome is still NULL (D13,
// section 4.5): still reserved, not yet terminalized by a commit or by
// ExpireClaims's own reconcile. cmd/zing's removeStaleStderrFiles reads this
// so a run's own stderr file (runjob.go's writeStderrFile) is never removed
// while its run is still open, whatever its age. A Store method because
// Store.db is unexported (store.go).
func (s *Store) OpenRunIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE outcome IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("open run ids: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("open run ids: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("open run ids: %w", err)
	}
	return out, nil
}

// SplitChildRow is one child ticket already filed under a split's parent
// (#74): the tickets row's id, split_key, tracker_ref, and state.
type SplitChildRow struct {
	TicketID int64
	Key      string
	Ref      string
	State    string
}

// SplitChildren returns every child ticket already filed under parentID,
// ordered by id: the tickets whose parent_ticket_id is parentID and whose
// split_key is not NULL. A parent with no filed children returns an empty
// slice and a nil error.
func (s *Store) SplitChildren(ctx context.Context, parentID int64) ([]SplitChildRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, split_key, tracker_ref, state FROM tickets WHERE parent_ticket_id = ? AND split_key IS NOT NULL ORDER BY id`,
		parentID)
	if err != nil {
		return nil, fmt.Errorf("split children for ticket %d: %w", parentID, err)
	}
	defer rows.Close()

	out := []SplitChildRow{}
	for rows.Next() {
		var row SplitChildRow
		if err := rows.Scan(&row.TicketID, &row.Key, &row.Ref, &row.State); err != nil {
			return nil, fmt.Errorf("split children for ticket %d: %w", parentID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("split children for ticket %d: %w", parentID, err)
	}
	return out, nil
}

// GetMessage reads the message with id, or a wrapped sql.ErrNoRows if none exists.
func (s *Store) GetMessage(ctx context.Context, id int64) (MessageRow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id)
	m, err := scanMessage(row)
	if err != nil {
		return MessageRow{}, fmt.Errorf("get message %d: %w", id, err)
	}
	return m, nil
}
