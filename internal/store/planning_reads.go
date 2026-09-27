// planning_reads.go adds the read-only queries the planning state machine's
// entry decision and the gate's approve step consult (design section 4.5,
// 5.1, 5.3, 5.4): the current plan cohort and its seal progress, a run's
// scenario cohort, the answered-and-not-resolved rounds a tick resumes or
// resolves, a run's job and ticket, a ticket's project and spent agent time,
// the delivered-review count max_loops bounds, the invalid-output chain
// length D14 walks, whether a cap has already escalated once, and the
// pending/delivered marker pair a resume step checks before re-running.
// Every list here is deterministically ordered with an id tie-breaker,
// following reads.go and console_reads.go's own rule.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"zing/internal/response"
)

// msgTypeUpdate is the "update" message type: the system-authored markers
// section 5.3 writes (planreview pending/delivered, validation errors
// pending/delivered, response invalid, seal mismatch).
const msgTypeUpdate = "update"

// Cohort names the ticket's current plan cohort: the max-version "plan"
// artifact's version and the run that produced it (design section 4.5, 6.6
// step 1). RunID is nil for a legacy plan artifact stored before every
// artifact carried run_id.
type Cohort struct {
	PlanVersion int
	RunID       *int64
}

// CurrentCohort returns the ticket's current plan cohort: the max-version
// "plan" artifact's version and run id. ok is false, with no error, when the
// ticket has no plan artifact yet.
func (s *Store) CurrentCohort(ctx context.Context, ticketID int64) (Cohort, bool, error) {
	var version int
	var runID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT version, run_id FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		ticketID).Scan(&version, &runID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Cohort{}, false, nil
	case err != nil:
		return Cohort{}, false, fmt.Errorf("current cohort for ticket %d: %w", ticketID, err)
	}
	c := Cohort{PlanVersion: version}
	if runID.Valid {
		c.RunID = &runID.Int64
	}
	return c, true, nil
}

// CohortSealState reports one scenario cohort's seal progress (design D16,
// section 4.5, 6.6 step 3): total is how many scenario artifacts the cohort
// carries, sealed is how many of those carry a non-null sealed_at, and
// commonAt is that instant, but only when sealed is at least one and every
// sealed row agrees on it -- nil when none are sealed, or when the sealed
// rows disagree, a state the seal transaction itself never produces but the
// gate's pre-check must still recognize as "not consistently sealed".
func (s *Store) CohortSealState(ctx context.Context, ticketID, runID int64) (total, sealed int, commonAt *time.Time, err error) {
	var distinct int
	var minSealed sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(sealed_at), COUNT(DISTINCT sealed_at), MIN(sealed_at)
		 FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ?`,
		ticketID, runID).Scan(&total, &sealed, &distinct, &minSealed)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("cohort seal state for ticket %d run %d: %w", ticketID, runID, err)
	}
	if sealed > 0 && distinct == 1 && minSealed.Valid {
		ts, perr := time.Parse(time.RFC3339, minSealed.String)
		if perr != nil {
			return 0, 0, nil, fmt.Errorf("cohort seal state for ticket %d run %d: parse sealed_at: %w", ticketID, runID, perr)
		}
		commonAt = &ts
	}
	return total, sealed, commonAt, nil
}

// ScenariosForRun returns runID's scenario cohort, ordered by artifacts.id
// (insertion order): the exact order zing scenarios prints and the gate
// renders (design section 7, 8). sealedOnly restricts it to the sealed rows,
// the judge run's own rule; the gate reads every row (sealedOnly false) so a
// pending cohort still renders.
func (s *Store) ScenariosForRun(ctx context.Context, ticketID, runID int64, sealedOnly bool) ([]Artifact, error) {
	query := `SELECT ` + artifactColumns + ` FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ?`
	if sealedOnly {
		query += ` AND sealed_at IS NOT NULL`
	}
	query += ` ORDER BY artifacts.id`

	rows, err := s.db.QueryContext(ctx, query, ticketID, runID)
	if err != nil {
		return nil, fmt.Errorf("scenarios for ticket %d run %d: %w", ticketID, runID, err)
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scenarios for ticket %d run %d: %w", ticketID, runID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scenarios for ticket %d run %d: %w", ticketID, runID, err)
	}
	return out, nil
}

// AllScenarios returns every scenario artifact on ticketID, across every run
// and version, ordered by artifacts.id: the console's fallback for a legacy,
// uncohorted ticket whose scenarios carry no run_id (design section 7).
func (s *Store) AllScenarios(ctx context.Context, ticketID int64) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE ticket_id = ? AND type = 'scenario' ORDER BY artifacts.id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("all scenarios for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("all scenarios for ticket %d: %w", ticketID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("all scenarios for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// Round is one answered planning interaction: a group of "question" messages
// plus its sent answers and replies (design section 4.5, 5.1 step 1).
// RunID/SessionID/Job are filled only when the group is keyed by a non-null
// run_id (the ordinary classify/planning/planreview/gate case, joined
// through runs -> sessions); ParentID is filled instead when the group has
// no run (an escalation-linked question, keyed by the escalation message's
// own id). A round may carry Replies with no Answers.
type Round struct {
	RunID, SessionID *int64
	ParentID         *int64
	Job              string
	Questions        []MessageRow
	Answers          []MessageRow
	Replies          []MessageRow
}

// roundKey groups AnsweredRounds' questions: ByRun true keys on a run id,
// false keys on a parent id (an escalation's own message id).
type roundKey struct {
	ByRun bool
	ID    int64
}

// roundKeyFor returns q's group key (design section 4.5): by run_id when
// non-null, else by parent_id (every escalation-linked question carries
// one). Neither case is reachable in a well-formed database, but a question
// carrying neither groups on its own id rather than panicking, becoming a
// single-question round of its own.
func roundKeyFor(q MessageRow) roundKey {
	switch {
	case q.RunID != nil:
		return roundKey{ByRun: true, ID: *q.RunID}
	case q.ParentID != nil:
		return roundKey{ByRun: false, ID: *q.ParentID}
	default:
		return roundKey{ByRun: false, ID: q.ID}
	}
}

// AnsweredRounds returns every group of answered-but-not-resolved "question"
// messages on ticketID, newest first by the group's newest question id
// (design section 4.5, 5.1 step 1). Every "question" message in state
// "answered" (never "resolved") is grouped by run_id when non-null, else by
// parent_id; a round's Answers and Replies are its group's sent answers and
// replies, matched by parent_id.
func (s *Store) AnsweredRounds(ctx context.Context, ticketID int64) ([]Round, error) {
	qRows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = ? AND state = ? ORDER BY id`,
		ticketID, msgTypeQuestion, questionStateAnswered)
	if err != nil {
		return nil, fmt.Errorf("answered rounds for ticket %d: %w", ticketID, err)
	}
	defer qRows.Close()

	groups := make(map[roundKey]*Round)
	var order []roundKey
	var questionIDs []int64

	for qRows.Next() {
		q, scanErr := scanMessage(qRows)
		if scanErr != nil {
			return nil, fmt.Errorf("answered rounds for ticket %d: %w", ticketID, scanErr)
		}
		key := roundKeyFor(q)
		g, ok := groups[key]
		if !ok {
			g = &Round{}
			if key.ByRun {
				id := key.ID
				g.RunID = &id
			} else {
				id := key.ID
				g.ParentID = &id
			}
			groups[key] = g
			order = append(order, key)
		}
		g.Questions = append(g.Questions, q)
		questionIDs = append(questionIDs, q.ID)
	}
	if rowsErr := qRows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("answered rounds for ticket %d: %w", ticketID, rowsErr)
	}
	if len(order) == 0 {
		return nil, nil
	}

	if fillErr := fillRunContexts(ctx, s, groups); fillErr != nil {
		return nil, fmt.Errorf("answered rounds for ticket %d: %w", ticketID, fillErr)
	}

	answers, err := messagesByParent(ctx, s, ticketID, msgTypeAnswer, answerStateSent, questionIDs)
	if err != nil {
		return nil, fmt.Errorf("answered rounds for ticket %d: %w", ticketID, err)
	}
	replies, err := messagesByParent(ctx, s, ticketID, msgTypeReply, answerStateSent, questionIDs)
	if err != nil {
		return nil, fmt.Errorf("answered rounds for ticket %d: %w", ticketID, err)
	}

	out := make([]Round, 0, len(order))
	for _, key := range order {
		g := groups[key]
		for i := range g.Questions {
			qID := g.Questions[i].ID
			g.Answers = append(g.Answers, answers[qID]...)
			g.Replies = append(g.Replies, replies[qID]...)
		}
		out = append(out, *g)
	}

	slices.SortFunc(out, func(a, b Round) int {
		an := a.Questions[len(a.Questions)-1].ID
		bn := b.Questions[len(b.Questions)-1].ID
		switch {
		case an > bn:
			return -1
		case an < bn:
			return 1
		default:
			return 0
		}
	})
	return out, nil
}

// fillRunContexts fills every run-keyed group's Job and SessionID, joined
// through runs -> sessions, so AnsweredRounds' caller needs no separate
// lookup per round.
func fillRunContexts(ctx context.Context, s *Store, groups map[roundKey]*Round) error {
	for key, g := range groups {
		if !key.ByRun {
			continue
		}
		var job string
		var sessionID int64
		err := s.db.QueryRowContext(ctx,
			`SELECT s.job, s.id FROM runs r JOIN sessions s ON s.id = r.session_id WHERE r.id = ?`,
			key.ID).Scan(&job, &sessionID)
		if err != nil {
			return fmt.Errorf("run context for run %d: %w", key.ID, err)
		}
		g.Job = job
		g.SessionID = &sessionID
	}
	return nil
}

// messagesByParent returns every message of (typ, state) whose parent_id is
// one of parentIDs, bucketed by parent id, each bucket ordered by id. The
// dynamic part of the query is a fixed number of "?" placeholders, one per
// id; every value rides as a bind argument, never concatenated into the
// query text (the same pattern ListReadyCandidates uses, reads.go).
func messagesByParent(ctx context.Context, s *Store, ticketID int64, typ, state string, parentIDs []int64) (map[int64][]MessageRow, error) {
	out := make(map[int64][]MessageRow)
	if len(parentIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(parentIDs))
	args := make([]any, 0, len(parentIDs)+3)
	args = append(args, ticketID, typ, state)
	for i, id := range parentIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	inClause := `parent_id IN (` + strings.Join(placeholders, ", ") + `)`                                                                  //nolint:gosec // G202: placeholders only, values are bind args
	query := `SELECT ` + messageColumns + ` FROM messages WHERE ticket_id = ? AND type = ? AND state = ? AND ` + inClause + ` ORDER BY id` //nolint:gosec // G202: messageColumns and inClause are both fixed text, no user input

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("messages by parent (%s): %w", typ, err)
	}
	defer rows.Close()

	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("messages by parent (%s): %w", typ, err)
		}
		if m.ParentID != nil {
			out[*m.ParentID] = append(out[*m.ParentID], m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("messages by parent (%s): %w", typ, err)
	}
	return out, nil
}

// RunContext returns runID's job and ticket id, joined through its session
// (design section 4.5): the lookup that recovers which job and ticket a bare
// run id belongs to. An unknown run id returns an error wrapping
// sql.ErrNoRows.
func (s *Store) RunContext(ctx context.Context, runID int64) (job string, ticketID int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT s.job, s.ticket_id FROM runs r JOIN sessions s ON s.id = r.session_id WHERE r.id = ?`,
		runID).Scan(&job, &ticketID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, fmt.Errorf("run context for run %d: %w", runID, sql.ErrNoRows)
		}
		return "", 0, fmt.Errorf("run context for run %d: %w", runID, err)
	}
	return job, ticketID, nil
}

// ProjectForTicket returns ticketID's project: the workdir and tracker
// lookup runJob and the post-commit tracker effect both make (design section
// 4.6, 6.8).
func (s *Store) ProjectForTicket(ctx context.Context, ticketID int64) (Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.repo_url, p.local_path, p.tracker, p.default_branch
		 FROM projects p JOIN tickets t ON t.project_id = p.id WHERE t.id = ?`,
		ticketID)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, fmt.Errorf("project for ticket %d: %w", ticketID, sql.ErrNoRows)
		}
		return Project{}, fmt.Errorf("project for ticket %d: %w", ticketID, err)
	}
	return p, nil
}

// AgentSecondsForTicket returns the sum of agent_seconds over every run on
// ticketID's sessions (design section 4.6's budget check): 0 when the ticket
// has no terminalized runs yet.
func (s *Store) AgentSecondsForTicket(ctx context.Context, ticketID int64) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(r.agent_seconds), 0) FROM runs r JOIN sessions s ON s.id = r.session_id WHERE s.ticket_id = ?`,
		ticketID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("agent seconds for ticket %d: %w", ticketID, err)
	}
	return total, nil
}

// CountDeliveredReviews counts the ticket's "planreview v<V> delivered"
// markers (design section 5.1 step 7, 5.3): the delivered-cycle count
// max_loops bounds.
func (s *Store) CountDeliveredReviews(ctx context.Context, ticketID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ? AND body LIKE 'planreview v%delivered'`,
		ticketID, msgTypeUpdate).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count delivered reviews for ticket %d: %w", ticketID, err)
	}
	return n, nil
}

// ConsecutiveInvalidOutputs walks job's runs (joined through sessions, and
// scoped to sessionID's session when non-nil) newest first by run id, and
// counts how many consecutive runs failed with the closed invalid-output
// marker (design D14, section 4.5, 5.4): outcome='error' and a "response
// invalid run <id>" update message naming that exact run. The walk stops --
// without counting the run it stopped on -- at the first run with any other
// terminal outcome, at the first run that has an escalation message
// attached (that run caused an escalation the owner has since resolved, the
// D14 chain boundary), or at the first error run with no matching marker (a
// different kind of failure). A run with a NULL outcome (still in flight, or
// crash-reconciled with none recorded) is skipped, not a stop. lastReason is
// the text after the first newline of the newest counted marker's body (""
// when n == 0).
func (s *Store) ConsecutiveInvalidOutputs(ctx context.Context, ticketID int64, job string, sessionID *int64) (n int, lastReason string, err error) {
	query := `SELECT r.id, r.outcome FROM runs r JOIN sessions s ON s.id = r.session_id
		WHERE s.ticket_id = ? AND s.job = ?`
	args := []any{ticketID, job}
	if sessionID != nil {
		query += ` AND r.session_id = ?`
		args = append(args, *sessionID)
	}
	query += ` ORDER BY r.id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, "", fmt.Errorf("consecutive invalid outputs for ticket %d job %s: %w", ticketID, job, err)
	}
	defer rows.Close()

	type runOutcome struct {
		id      int64
		outcome *string
	}
	var runs []runOutcome
	for rows.Next() {
		var id int64
		var outcome sql.NullString
		if scanErr := rows.Scan(&id, &outcome); scanErr != nil {
			return 0, "", fmt.Errorf("consecutive invalid outputs for ticket %d job %s: %w", ticketID, job, scanErr)
		}
		ro := runOutcome{id: id}
		if outcome.Valid {
			o := outcome.String
			ro.outcome = &o
		}
		runs = append(runs, ro)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return 0, "", fmt.Errorf("consecutive invalid outputs for ticket %d job %s: %w", ticketID, job, rowsErr)
	}
	if len(runs) == 0 {
		return 0, "", nil
	}

	escalated, err := runsWithEscalation(ctx, s, ticketID)
	if err != nil {
		return 0, "", fmt.Errorf("consecutive invalid outputs for ticket %d job %s: %w", ticketID, job, err)
	}
	reasons, err := invalidOutputReasons(ctx, s, ticketID)
	if err != nil {
		return 0, "", fmt.Errorf("consecutive invalid outputs for ticket %d job %s: %w", ticketID, job, err)
	}

	for _, r := range runs {
		if r.outcome == nil {
			continue
		}
		if escalated[r.id] {
			break
		}
		if *r.outcome != "error" {
			break
		}
		reason, ok := reasons[r.id]
		if !ok {
			break
		}
		n++
		if n == 1 {
			lastReason = reason
		}
	}
	return n, lastReason, nil
}

// runsWithEscalation returns the set of run ids that have at least one
// "escalation" message attached (messages.run_id = run.id AND
// type='escalation'), scoped to ticketID.
func runsWithEscalation(ctx context.Context, s *Store, ticketID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT run_id FROM messages WHERE ticket_id = ? AND type = ? AND run_id IS NOT NULL`,
		ticketID, msgTypeEscalation)
	if err != nil {
		return nil, fmt.Errorf("runs with escalation: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("runs with escalation: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs with escalation: %w", err)
	}
	return out, nil
}

// invalidOutputReasons parses every "response invalid run <id>" marker on
// ticketID (design section 5.3) into a run id -> reason map: the text after
// the marker's first newline, the closed Reason constant D14 stores there.
// A marker whose id text does not parse is ignored rather than failing the
// whole read.
func invalidOutputReasons(ctx context.Context, s *Store, ticketID int64) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT body FROM messages WHERE ticket_id = ? AND type = ? AND body LIKE 'response invalid run %'`,
		ticketID, msgTypeUpdate)
	if err != nil {
		return nil, fmt.Errorf("invalid output reasons: %w", err)
	}
	defer rows.Close()

	const prefix = "response invalid run "
	out := make(map[int64]string)
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("invalid output reasons: %w", err)
		}
		rest := strings.TrimPrefix(body, prefix)
		idText, reason, _ := strings.Cut(rest, "\n")
		id, perr := strconv.ParseInt(idText, 10, 64)
		if perr != nil {
			continue
		}
		out[id] = reason
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("invalid output reasons: %w", err)
	}
	return out, nil
}

// CountSealMismatches counts ticketID's "seal mismatch cohort <runID>"
// markers for runID (design D16, section 6.6 branch 0): the dispatcher
// writes one exact-body "update" message (author "system") each time a seal
// transaction mismatches (internal/dispatch's releaseAfterSealMismatch);
// the gate's approve pre-check escalates seal_failed once this reaches two,
// bounding the theoretical retry loop at two attempts.
func (s *Store) CountSealMismatches(ctx context.Context, ticketID, runID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ? AND body = ?`,
		ticketID, msgTypeUpdate, fmt.Sprintf("seal mismatch cohort %d", runID)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count seal mismatches for ticket %d run %d: %w", ticketID, runID, err)
	}
	return n, nil
}

// HasEscalation reports whether an "escalation" message exists for ticketID
// whose JSON payload has origin == origin and session_id == sessionID (using
// SQLite json_extract): the "already escalated this session once" check that
// keeps a cap escalation from repeating (design D17, section 5.1 step 3).
func (s *Store) HasEscalation(ctx context.Context, ticketID int64, origin string, sessionID int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM messages
		 WHERE ticket_id = ? AND type = ?
		   AND json_extract(payload, '$.origin') = ?
		   AND json_extract(payload, '$.session_id') = ?
		 LIMIT 1`,
		ticketID, msgTypeEscalation, origin, sessionID).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("has escalation for ticket %d origin %s: %w", ticketID, origin, err)
	}
	return true, nil
}

// PlanReviewAt returns the "planreview" artifact stored at exactly version
// on ticketID (design section 4.5, 5.1 steps 6 and 7): ok is false, with no
// error, when no planreview artifact exists at that exact version yet --
// entry step 6's "no planreview artifact at cohort.PlanVersion" test, and
// step 7's read of the artifact whose findings a live pending marker names.
func (s *Store) PlanReviewAt(ctx context.Context, ticketID int64, version int) (Artifact, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE ticket_id = ? AND type = 'planreview' AND version = ?`,
		ticketID, version)
	a, err := scanArtifact(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, fmt.Errorf("planreview at version %d for ticket %d: %w", version, ticketID, err)
	}
	return a, true, nil
}

// LiveMarker returns the newest "update" message whose body starts with
// pending, provided no "update" message whose body EQUALS delivered plus
// that pending's own tail carries a greater id (design section 5.1 steps 5
// and 7): ok is false when there is no pending marker, or when a later
// delivered marker for the SAME run has already closed it out.
//
// The tail is whatever follows the pending prefix on the pending body's
// first line: "" for planreview markers (prefix and body coincide), " run
// <id>" for validation-errors markers (design section 5.1's discriminator
// lives in the suffix). Matching delivered+tail by exact equality, rather
// than delivered+"%" by LIKE, is what keeps an older "delivered run A" from
// closing a newer "pending run B": the two runs' tails never compare equal.
func (s *Store) LiveMarker(ctx context.Context, ticketID int64, pending, delivered string) (MessageRow, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ticket_id = ? AND type = ? AND body LIKE ? ORDER BY id DESC LIMIT 1`,
		ticketID, msgTypeUpdate, pending+"%")
	m, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MessageRow{}, false, nil
		}
		return MessageRow{}, false, fmt.Errorf("live marker for ticket %d: %w", ticketID, err)
	}

	firstLine, _, _ := strings.Cut(m.Body, "\n")
	tail := strings.TrimPrefix(firstLine, pending)
	deliveredTarget := delivered + tail

	var laterDelivered int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ? AND body = ? AND id > ?`,
		ticketID, msgTypeUpdate, deliveredTarget, m.ID).Scan(&laterDelivered)
	if err != nil {
		return MessageRow{}, false, fmt.Errorf("live marker for ticket %d: %w", ticketID, err)
	}
	if laterDelivered > 0 {
		return MessageRow{}, false, nil
	}
	return m, true, nil
}

// EscalationByID reads the escalation message with id and decodes its
// EscalationPayload (design section 4.5, 6.7 Resolve): the entry step 1(b)
// read that turns an escalation-linked question's own parent id (the
// escalation message's id) into the Code, What, Why, Tried, SessionID, and
// Origin the owner's answered round resolves. A wrapped sql.ErrNoRows for an
// unknown id, exactly as GetMessage's own error.
func (s *Store) EscalationByID(ctx context.Context, id int64) (MessageRow, response.EscalationPayload, error) {
	m, err := s.GetMessage(ctx, id)
	if err != nil {
		return MessageRow{}, response.EscalationPayload{}, fmt.Errorf("escalation %d: %w", id, err)
	}
	var payload response.EscalationPayload
	if unmarshalErr := json.Unmarshal(m.Payload, &payload); unmarshalErr != nil {
		return MessageRow{}, response.EscalationPayload{}, fmt.Errorf("escalation %d: unmarshal payload: %w", id, unmarshalErr)
	}
	return m, payload, nil
}
