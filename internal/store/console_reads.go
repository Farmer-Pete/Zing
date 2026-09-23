// console_reads.go adds the read helpers the console needs: projects, the
// inbox (with open-question summaries), a ticket's project, the recency
// list, the message feed, artifacts, sessions, runs, and settings. Every
// list is deterministically ordered with an id tie-breaker (design section
// 7.2).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QuestionSummary is one open question's inbox-row summary: the key, a
// title, a compact one-line preview, and the question message's own time
// (design section 6.5). Title is the first line of the question message's
// body, the same rule the thread view's group summary uses (section 6.6).
// Preview is that same title, truncated to a fixed length so a long
// question still renders as one short line in the compact inbox card; when
// the title already fits, Preview equals Title.
type QuestionSummary struct {
	Key     string
	Title   string
	Preview string
	Time    time.Time
}

// InboxItem is one ticket's inbox card: the ticket, its project's name, its
// open questions in message-id order, and the display-only time of its
// newest message (design section 7.2).
type InboxItem struct {
	Ticket        Ticket
	ProjectName   string
	OpenQuestions []QuestionSummary
	NewestAt      *time.Time // nil when the ticket has no message
}

// questionPreviewMaxRunes bounds QuestionSummary.Preview to one short line.
const questionPreviewMaxRunes = 80

// questionTitle returns the first line of a question message's body, the
// title shown in both the thread group summary (section 6.6) and the inbox
// row (section 6.5).
func questionTitle(body string) string {
	title, _, _ := strings.Cut(body, "\n")
	return title
}

// questionPreview truncates title to questionPreviewMaxRunes, appending an
// ellipsis when it cuts the text short, for the inbox's compact row.
func questionPreview(title string) string {
	r := []rune(title)
	if len(r) <= questionPreviewMaxRunes {
		return title
	}
	return string(r[:questionPreviewMaxRunes]) + "…"
}

// questionKeyPayload is the one field this file needs out of a question
// message's payload (response.QuestionPayload.Key, response package),
// unmarshaled directly the way commit.go's answerQuestionPayload does, so
// this file does not import internal/response for a single field.
type questionKeyPayload struct {
	Key string `json:"key"`
}

// unreadMessageWhere is the unread predicate (design section 6.8): a
// message is unread when read_at is unset, its author is zing, and its
// type is one of the four thread-visible Zing message types (never state,
// resolved, or side).
const unreadMessageWhere = `read_at IS NULL AND author = 'zing' AND type IN ('question','followup','escalation','update')`

// projectColumns is the projects column list, in table-declaration order.
const projectColumns = `id, name, repo_url, local_path, tracker, default_branch`

// scanProject scans one row of projectColumns into a Project. Every column
// is NOT NULL (migrations/0001_init.sql), so no nullable handling is
// needed, unlike scanTicket and scanMessage.
func scanProject(rs rowScanner) (Project, error) {
	var p Project
	if err := rs.Scan(&p.ID, &p.Name, &p.RepoURL, &p.LocalPath, &p.Tracker, &p.DefaultBranch); err != nil {
		return Project{}, err
	}
	return p, nil
}

// ListProjects returns every project, ordered by name then id.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return out, nil
}

// inboxQuery is InboxItems' single joined read (design section 6.5: "one
// joined read, no N+1"). One row per (ticket, open question) pair, a
// question's columns NULL when the ticket has none (the LEFT JOIN); a
// correlated MAX(id) subquery supplies the newest-message time and the
// ordering key, per section 7.2's "never by timestamp ... id is the only
// correct sort". Rows are grouped back into InboxItems in Go, by ticket id
// change; the ORDER BY guarantees one ticket's rows are contiguous.
// inboxQuery's two "newest message" correlated subqueries (the display
// time and the sort key) both exclude state=draft rows (code review fix 2):
// an unsent draft is only visible in the composer queue, so it must not
// count as a ticket's newest message for the inbox's display time or its
// blocking-first, newest-first sort, even though it is a real row with the
// greatest id. Bound as "?" rather than inlined, so this stays an ordinary
// parameterized literal rather than string-built SQL; InboxItems passes
// draftState (console_writes.go) for both placeholders, in the order they
// appear here.
const inboxQuery = `
SELECT
	t.id, t.project_id, t.tracker_ref, t.title, t.body, t.kind, t.state, t.waiting_on,
	t.parent_ticket_id, t.branch, t.pr_url, t.claim_owner, t.claim_expires_at,
	p.name,
	(SELECT created_at FROM messages nm WHERE nm.ticket_id = t.id AND (nm.state IS NULL OR nm.state != ?) ORDER BY nm.id DESC LIMIT 1),
	q.id, q.body, q.payload, q.created_at
FROM tickets t
JOIN projects p ON p.id = t.project_id
LEFT JOIN messages q ON q.ticket_id = t.id AND q.type = 'question' AND q.state = 'open'
WHERE t.waiting_on IS NOT NULL
   OR EXISTS (SELECT 1 FROM messages um WHERE um.ticket_id = t.id AND ` + unreadMessageWhere + `)
ORDER BY
	(t.waiting_on IS NOT NULL) DESC,
	(SELECT MAX(mm.id) FROM messages mm WHERE mm.ticket_id = t.id AND (mm.state IS NULL OR mm.state != ?)) DESC,
	t.id,
	q.id
`

// InboxItems returns every ticket that is blocking (waiting_on IS NOT NULL)
// or has an unread message, blocking first, then by the ticket's greatest
// SENT message id descending, then ticket id (design section 7.2; "sent"
// per code review fix 2 -- see inboxQuery). unreadMessageWhere's own
// author='zing' clause already excludes a draft from the unread check
// itself, since every draft SaveDraft or SendBatch writes is author="you"
// (console_writes.go); only the two newest-message subqueries need the
// explicit exclusion inboxQuery adds. Each item carries its open questions,
// ordered by message id, and the display-only time of its newest message.
func (s *Store) InboxItems(ctx context.Context) ([]InboxItem, error) {
	rows, err := s.db.QueryContext(ctx, inboxQuery, draftState, draftState)
	if err != nil {
		return nil, fmt.Errorf("inbox items: %w", err)
	}
	defer rows.Close()

	var out []InboxItem
	for rows.Next() {
		var t Ticket
		var kind, waitingOn, branch, prURL, claimOwner, claimExpiresAt sql.NullString
		var parentTicketID sql.NullInt64
		var projectName string
		var newestAt sql.NullString
		var qID sql.NullInt64
		var qBody, qPayload, qCreatedAt sql.NullString

		if err := rows.Scan(
			&t.ID, &t.ProjectID, &t.TrackerRef, &t.Title, &t.Body,
			&kind, &t.State, &waitingOn, &parentTicketID,
			&branch, &prURL, &claimOwner, &claimExpiresAt,
			&projectName, &newestAt,
			&qID, &qBody, &qPayload, &qCreatedAt,
		); err != nil {
			return nil, fmt.Errorf("inbox items: scan: %w", err)
		}

		if kind.Valid {
			t.Kind = &kind.String
		}
		if waitingOn.Valid {
			t.WaitingOn = &waitingOn.String
		}
		if parentTicketID.Valid {
			t.ParentTicketID = &parentTicketID.Int64
		}
		if branch.Valid {
			t.Branch = &branch.String
		}
		if prURL.Valid {
			t.PRURL = &prURL.String
		}
		if claimOwner.Valid {
			t.ClaimOwner = &claimOwner.String
		}
		if claimExpiresAt.Valid {
			ts, perr := time.Parse(time.RFC3339, claimExpiresAt.String)
			if perr != nil {
				return nil, fmt.Errorf("inbox items: parse claim_expires_at: %w", perr)
			}
			t.ClaimExpiresAt = &ts
		}

		if len(out) == 0 || out[len(out)-1].Ticket.ID != t.ID {
			item := InboxItem{Ticket: t, ProjectName: projectName}
			if newestAt.Valid {
				ts, perr := time.Parse(time.RFC3339Nano, newestAt.String)
				if perr != nil {
					return nil, fmt.Errorf("inbox items: parse newest_at: %w", perr)
				}
				item.NewestAt = &ts
			}
			out = append(out, item)
		}

		if !qID.Valid {
			continue
		}
		var key questionKeyPayload
		if perr := json.Unmarshal([]byte(qPayload.String), &key); perr != nil {
			return nil, fmt.Errorf("inbox items: unmarshal question %d payload: %w", qID.Int64, perr)
		}
		createdAt, perr := time.Parse(time.RFC3339Nano, qCreatedAt.String)
		if perr != nil {
			return nil, fmt.Errorf("inbox items: parse question %d created_at: %w", qID.Int64, perr)
		}
		title := questionTitle(qBody.String)
		cur := &out[len(out)-1]
		cur.OpenQuestions = append(cur.OpenQuestions, QuestionSummary{
			Key: key.Key, Title: title, Preview: questionPreview(title), Time: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inbox items: %w", err)
	}
	return out, nil
}

// TicketsByProject returns one project's tickets, ordered by tracker_ref
// then id.
func (s *Store) TicketsByProject(ctx context.Context, projectID int64) ([]Ticket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ticketColumns+` FROM tickets WHERE project_id = ? ORDER BY tracker_ref, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("tickets by project %d: %w", projectID, err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("tickets by project %d: %w", projectID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tickets by project %d: %w", projectID, err)
	}
	return out, nil
}

// RecentTickets returns every ticket ordered by its greatest SENT message id
// descending, then ticket id; a ticket with no sent message sorts last, by
// ticket id (design section 7.2). The MAX(id) subquery yields NULL for a
// ticket with no sent message, and SQLite sorts NULL last in a DESC
// ordering, which is exactly this rule. The subquery excludes state='draft'
// rows (code review fix, PR #16: Inbox and Feed already exclude a draft
// from their own newest-message ordering, design section 7.2's "sent" rule
// -- see inboxQuery and FeedMessages -- but Recent's own MAX(id) subquery
// had not been updated to match, so saving a draft reordered Recent even
// though the draft itself never renders anywhere).
func (s *Store) RecentTickets(ctx context.Context) ([]Ticket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ticketColumns+` FROM tickets t
		 ORDER BY (SELECT MAX(id) FROM messages WHERE ticket_id = t.id AND (state IS NULL OR state != ?)) DESC, t.id`,
		draftState)
	if err != nil {
		return nil, fmt.Errorf("recent tickets: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("recent tickets: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent tickets: %w", err)
	}
	return out, nil
}

// clampFeedLimit clamps limit into [1, 200] (design section 7.2).
func clampFeedLimit(limit int) int {
	switch {
	case limit < 1:
		return 1
	case limit > 200:
		return 200
	default:
		return limit
	}
}

// FeedMessages returns the newest SENT messages across every ticket
// (excluding state=draft: an unsent draft belongs to the composer queue,
// not the Feed, code review fix 2), newest first by id, capped at limit
// clamped to 1..200.
func (s *Store) FeedMessages(ctx context.Context, limit int) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE state IS NULL OR state != ? ORDER BY id DESC LIMIT ?`,
		draftState, clampFeedLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("feed messages: %w", err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("feed messages: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feed messages: %w", err)
	}
	return out, nil
}

// artifactColumns is the artifacts column list this file reads, matching
// Artifact's field order (store.go). id is excluded: Artifact is the
// insert shape (like Message), and carries no id field, so a read here
// returns the same struct a write takes, unlike a message read, which
// wraps Message in MessageRow to add one.
const artifactColumns = `ticket_id, run_id, type, version, payload, sealed_at`

// scanArtifact scans one row of artifactColumns into an Artifact.
func scanArtifact(rs rowScanner) (Artifact, error) {
	var a Artifact
	var runID sql.NullInt64
	var payload string
	var sealedAt sql.NullString

	if err := rs.Scan(&a.TicketID, &runID, &a.Type, &a.Version, &payload, &sealedAt); err != nil {
		return Artifact{}, err
	}
	if runID.Valid {
		a.RunID = &runID.Int64
	}
	a.Payload = json.RawMessage(payload)
	if sealedAt.Valid {
		ts, err := time.Parse(time.RFC3339, sealedAt.String)
		if err != nil {
			return Artifact{}, fmt.Errorf("parse sealed_at: %w", err)
		}
		a.SealedAt = &ts
	}
	return a, nil
}

// GetArtifact returns the greatest-version artifact of typ on ticketID. ok
// is false, with no error, when none exists.
func (s *Store) GetArtifact(ctx context.Context, ticketID int64, typ string) (Artifact, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE ticket_id = ? AND type = ? ORDER BY version DESC LIMIT 1`,
		ticketID, typ)
	a, err := scanArtifact(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, fmt.Errorf("get artifact %s for ticket %d: %w", typ, ticketID, err)
	}
	return a, true, nil
}

// ListArtifacts returns every artifact on ticketID, ordered by type, then
// version descending, then id.
func (s *Store) ListArtifacts(ctx context.Context, ticketID int64) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE ticket_id = ? ORDER BY type, version DESC, id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("list artifacts for ticket %d: %w", ticketID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// scanSession scans one row of (id, ticket_id, job, runtime, external_id,
// resumes), OpenSession's column order (reads.go), into a Session.
func scanSession(rs rowScanner) (Session, error) {
	var sess Session
	var externalID sql.NullString
	if err := rs.Scan(&sess.ID, &sess.TicketID, &sess.Job, &sess.Runtime, &externalID, &sess.Resumes); err != nil {
		return Session{}, err
	}
	if externalID.Valid {
		sess.ExternalID = &externalID.String
	}
	return sess, nil
}

// SessionsForTicket returns every session on ticketID, ordered by id.
func (s *Store) SessionsForTicket(ctx context.Context, ticketID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ticket_id, job, runtime, external_id, resumes FROM sessions WHERE ticket_id = ? ORDER BY id`,
		ticketID)
	if err != nil {
		return nil, fmt.Errorf("sessions for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("sessions for ticket %d: %w", ticketID, err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessions for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// RunsForTicket returns every run on ticketID's sessions, ordered by id.
// runs has no ticket_id column; the join through sessions is what scopes
// it to one ticket.
func (s *Store) RunsForTicket(ctx context.Context, ticketID int64) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.session_id, r.turn, r.lens, r.task_n, r.model, r.outcome, r.agent_seconds, r.exit_code
		 FROM runs r JOIN sessions s ON s.id = r.session_id
		 WHERE s.ticket_id = ? ORDER BY r.id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("runs for ticket %d: %w", ticketID, err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("runs for ticket %d: %w", ticketID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs for ticket %d: %w", ticketID, err)
	}
	return out, nil
}

// GetSetting reads one settings row. ok is false, with no error, when key
// does not exist; ok is true with an empty value when the row exists but
// is NULL (migrations/0001_init.sql seeds a few settings that way).
func (s *Store) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	var raw sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get setting %s: %w", key, err)
	}
	return raw.String, true, nil
}
