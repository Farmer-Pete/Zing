package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"zing/internal/response"
)

// Message types, authors, and the question lifecycle states this file reads
// or writes, named once so commit.go carries identifiers rather than
// repeated literals.
const (
	msgTypeQuestion   = "question"
	msgTypeAnswer     = "answer"
	msgTypeState      = "state"
	msgTypeResolved   = "resolved"
	msgTypeEscalation = "escalation"

	authorSystem = "system"
	authorYou    = "you"
	authorZing   = "zing"

	answerStateSent = "sent"

	questionStateOpen     = "open"
	questionStateAnswered = "answered"
	questionStateResolved = "resolved"

	waitingFlagQuestions = "questions"

	// nullDisplay is the "have" side of a same-to-same conflict message
	// (setKindTx, setBranchTx, setPRURLTx) when the column's current value
	// is NULL, so the three share one literal (goconst).
	nullDisplay = "null"
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
	// Sessions updates further existing sessions, each with ID set, after
	// Session (the review round terminalizes seven sessions in one commit,
	// design section 4.2). An entry with a nil ID is an error: "commit
	// handler result: extra session needs an id".
	Sessions []SessionUpsert
	// Runs is applied entry by entry: ID == 0 inserts a new row under
	// Session's session id (SessionID filled from Session after upsert);
	// ID > 0 updates that existing, ticket-owned run's outcome, exit_code,
	// and agent_seconds in place (design D13's Reserve placeholder,
	// terminalized here) and never touches its model column, which Reserve
	// already set.
	Runs []Run

	Messages []Message
	// AttachRunToMsgs requires exactly one Runs entry; every message whose
	// RunID is nil takes that run's id (inserted or updated), a message
	// whose RunID points at 0 is rejected, and any other non-nil RunID is
	// left as the handler set it.
	AttachRunToMsgs bool
	// SetKind sets tickets.kind to "bug" or "feature" when it is currently
	// NULL or already that same value; any other current value is a
	// conflict.
	SetKind *string
	// SetBranch sets tickets.branch when it is currently NULL or already
	// that same value; any other current value is a conflict. Applied after
	// SetKind.
	SetBranch *string
	// SetPRURL sets tickets.pr_url when it is currently NULL or already
	// that same value; any other current value is the conflict "pr url
	// conflict: have <x>, want <y>" (design section 4.2, 8.2). Applied after
	// SetBranch.
	SetPRURL *string
	// Poll sets all three poll columns together (design section 4.2, D8).
	// PollSchedule moves next_poll_at and poll_interval_s only, leaving
	// poll_fingerprint as it is (used when a poll's GitHub reads failed,
	// 8.3, so no fingerprint can be computed). ClearPoll sets all three to
	// NULL. At most one of Poll, PollSchedule, and ClearPoll may be set in
	// a single commit; more than one is the error "commit handler result:
	// at most one poll update per commit". All three are applied in the
	// same fenced ticket UPDATE that carries Next, Waiting, and the claim
	// clear.
	Poll         *PollUpdate
	PollSchedule *PollSchedule
	ClearPoll    bool
	// Artifacts is inserted after Runs and SetKind: TicketID is forced to
	// this commit's ticket, a non-nil RunID must belong to it, and
	// Version == 0 becomes one past that (ticket, type)'s current maximum.
	Artifacts        []Artifact
	ResolveQuestions []int64 // set each question's state to "resolved" and insert one "resolved" message per id
	// ResolveAll resolves every ticket question still "open" or "answered",
	// the abandon case where no individual id list applies.
	ResolveAll bool
	// Seal applies the section 4.5 cohort seal, after Artifacts: the
	// ticket's max-version plan artifact must match RunID and PlanVersion,
	// its scenario cohort (artifacts of type "scenario" carrying that
	// RunID) must count exactly ExpectedCount rows in [2,30], and sealing
	// every one of them at At must affect exactly ExpectedCount rows. Any
	// other outcome is a *SealMismatchError naming the failing stage, and
	// rolls the whole commit back (design D16).
	Seal *SealRequest
	// Escalation records one structured escalation and its linked question
	// in a single shape (design D10, section 6.7): a nil RunID is a cap
	// escalation that no run caused; a non-nil RunID ties both inserted
	// messages to the run that did.
	Escalation *EscalationCommit
	// TrackerEffect is carried, never applied, by this transaction: the
	// dispatcher runs it against the ticket's tracker only after a
	// successful commit (design D12).
	TrackerEffect *TrackerEffect
}

// SealRequest names the cohort a Seal step must prove and seal: the run that
// produced it, the plan version it was planned against, the exact scenario
// count it must carry, and the instant to stamp every sealed row with
// (design D16, section 4.5).
type SealRequest struct {
	RunID         int64
	PlanVersion   int
	ExpectedCount int // 2..30
	At            time.Time
}

// EscalationCommit is one structured escalation plus its linked question
// (design D10, section 6.7). Body is "<code>: <what>", stored on both the
// escalation message and, with the standing question appended, the question
// message. Payload is the escalation's own validated shape; RunID is nil for
// a cap escalation no run caused.
type EscalationCommit struct {
	RunID   *int64
	Body    string
	Payload response.EscalationPayload
}

// TrackerEffect is a tracker comment a handler wants posted after its commit
// lands (design D12): Ref names the tracker issue, Notes is the comment
// text. CommitHandlerResult carries this value through unread; only the
// dispatcher, after a successful commit, resolves Ref and posts Notes.
type TrackerEffect struct {
	Ref, Notes string
}

// ErrSealMismatch is the sentinel every *SealMismatchError unwraps to, so a
// caller that only needs to know "was this a seal mismatch" can use
// errors.Is without also importing the typed shape (design D16).
var ErrSealMismatch = errors.New("store: seal invariant mismatch")

// SealMismatchError is Seal's typed failure (design D16): Stage names which
// of the three checks failed ("plan", "count", or "update"), Expected is the
// SealRequest's ExpectedCount, and Affected is the row count the "update"
// stage actually saw (zero for "plan" and "count", which fail before any
// row is touched).
type SealMismatchError struct {
	Stage    string
	Expected int
	Affected int
}

func (e *SealMismatchError) Error() string {
	return fmt.Sprintf("store: seal mismatch at stage %q: expected %d, affected %d", e.Stage, e.Expected, e.Affected)
}

func (e *SealMismatchError) Unwrap() error { return ErrSealMismatch }

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
// Order inside the transaction (section 6.3, 4.5): verify the fence; upsert
// the session and learn its id; update every further session Sessions
// names; insert or update the runs and learn their ids; set kind; set
// branch; set pr url; insert the artifacts; seal the cohort when Seal is
// set; record the escalation and its linked question when Escalation is
// set; resolve every question when ResolveAll is set; insert the messages,
// attaching the single run's id when AttachRunToMsgs is set; resolve each
// ResolveQuestions id and insert its resolved message; write the state
// message; then apply Next, Waiting, the three poll columns (Poll,
// PollSchedule, or ClearPoll, design section 4.2, D8), and the claim clear
// in the one fenced ticket UPDATE that also serves as the final fence
// check.
func (s *Store) CommitHandlerResult(ctx context.Context, c HandlerCommit) (bool, error) {
	if c.AttachRunToMsgs && len(c.Runs) != 1 {
		return false, errors.New("commit handler result: attach needs exactly one run")
	}
	if pollUpdateCount(c) > 1 {
		return false, errors.New("commit handler result: at most one poll update per commit")
	}
	if c.Poll != nil {
		if !isHex64Lower(c.Poll.Fingerprint) {
			return false, errors.New("commit handler result: poll fingerprint must be 64 lowercase hex characters")
		}
		if c.Poll.IntervalS < 30 || c.Poll.IntervalS > 300 {
			return false, errors.New("commit handler result: poll interval must be 30 to 300")
		}
	}
	if c.PollSchedule != nil && (c.PollSchedule.IntervalS < 30 || c.PollSchedule.IntervalS > 300) {
		return false, errors.New("commit handler result: poll interval must be 30 to 300")
	}
	if c.Next != "" && c.Reason == "" {
		return false, fmt.Errorf("commit handler result: reason is required when transitioning to %s", c.Next)
	}

	expires := truncateExpires(c.Expires)

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

	// No Go-side lease pre-check: the fenced UPDATE at the end of this
	// transaction (WHERE claim_owner = ? AND claim_expires_at = ?) is the
	// one authoritative fence. Zero rows affected there rolls everything in
	// this tx back and reports applied=false, so a lease already lost is
	// caught there rather than duplicated here.

	if c.Session != nil && c.Session.ID != nil {
		if err = verifySessionForTicket(ctx, tx, c.TicketID, *c.Session.ID); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
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

	for _, su := range c.Sessions {
		if su.ID == nil {
			return false, errors.New("commit handler result: extra session needs an id")
		}
		if err = verifySessionForTicket(ctx, tx, c.TicketID, *su.ID); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
		if _, err = upsertSessionTx(ctx, tx, c.TicketID, su); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	runIDs := make([]int64, 0, len(c.Runs))
	for _, r := range c.Runs {
		if r.ID > 0 {
			if err = updateRunTx(ctx, tx, r, c.TicketID); err != nil {
				return false, fmt.Errorf("commit handler result: %w", err)
			}
			runIDs = append(runIDs, r.ID)
			continue
		}
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

	if c.SetKind != nil {
		if err = setKindTx(ctx, tx, c.TicketID, *c.SetKind); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	if c.SetBranch != nil {
		if err = setBranchTx(ctx, tx, c.TicketID, *c.SetBranch); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	if c.SetPRURL != nil {
		if err = setPRURLTx(ctx, tx, c.TicketID, *c.SetPRURL); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	for _, a := range c.Artifacts {
		// Force every inserted artifact's ticket_id to c.TicketID, the same
		// rule commit.go already applies to Messages: a handler proposes
		// artifacts but never writes, so this commit boundary is what an
		// artifact can never be scoped away from.
		a.TicketID = c.TicketID
		if a.RunID != nil {
			var owned bool
			owned, err = runOwnedByTicketTx(ctx, tx, c.TicketID, *a.RunID)
			if err != nil {
				return false, fmt.Errorf("commit handler result: check artifact run %d: %w", *a.RunID, err)
			}
			if !owned {
				return false, fmt.Errorf("commit handler result: artifact run %d not owned by ticket %d", *a.RunID, c.TicketID)
			}
		}
		if _, err = s.insertArtifactTx(ctx, tx, a); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	if c.Seal != nil {
		if err = sealCohortTx(ctx, tx, c.TicketID, *c.Seal); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	if c.Escalation != nil {
		if err = s.escalateTx(ctx, tx, c.TicketID, *c.Escalation); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	if c.ResolveAll {
		if _, err = tx.ExecContext(ctx,
			`UPDATE messages SET state = ? WHERE ticket_id = ? AND type = ? AND state IN (?, ?)`,
			questionStateResolved, c.TicketID, msgTypeQuestion, questionStateOpen, questionStateAnswered,
		); err != nil {
			return false, fmt.Errorf("commit handler result: resolve all questions: %w", err)
		}
	}

	for _, m := range c.Messages {
		// Force every inserted message's ticket_id to c.TicketID: a handler
		// proposes messages but never writes, so this commit boundary, not
		// the handler, is what a message can never be scoped away from.
		m.TicketID = c.TicketID
		if attachRunID != nil {
			switch {
			case m.RunID == nil:
				m.RunID = attachRunID
			case *m.RunID == 0:
				return false, errors.New("commit handler result: message run id 0")
			default:
				// A message with an explicit non-zero RunID keeps it.
			}
		}
		if m.ParentID != nil {
			if err = verifyParentForTicket(ctx, tx, c.TicketID, *m.ParentID); err != nil {
				return false, fmt.Errorf("commit handler result: %w", err)
			}
		}
		if m.Type == msgTypeQuestion {
			if m.Payload, err = fillQuestionKeyTx(ctx, tx, c.TicketID, m.Payload); err != nil {
				return false, fmt.Errorf("commit handler result: %w", err)
			}
		}
		if err = s.insertMessageTx(ctx, tx, m); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	for _, qid := range c.ResolveQuestions {
		if err = verifyQuestionForTicket(ctx, tx, c.TicketID, qid); err != nil {
			return false, fmt.Errorf("commit handler result: resolve question %d: %w", qid, err)
		}
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

	// nextPollAt, pollIntervalS, and pollFingerprint default to the ticket's
	// own current values (read at the top of this transaction), so a commit
	// that sets none of Poll, PollSchedule, or ClearPoll leaves all three
	// poll columns untouched (design section 4.2, D8).
	nextPollAt := formatTimePtr(ticket.NextPollAt)
	pollIntervalS := ticket.PollIntervalS
	pollFingerprint := ticket.PollFingerprint
	switch {
	case c.ClearPoll:
		nextPollAt, pollIntervalS, pollFingerprint = nil, nil, nil
	case c.Poll != nil:
		at := formatTime(c.Poll.NextAt)
		iv := c.Poll.IntervalS
		fp := c.Poll.Fingerprint
		nextPollAt, pollIntervalS, pollFingerprint = &at, &iv, &fp
	case c.PollSchedule != nil:
		at := formatTime(c.PollSchedule.NextAt)
		iv := c.PollSchedule.IntervalS
		nextPollAt, pollIntervalS = &at, &iv
		// pollFingerprint stays the ticket's own current value: a
		// schedule-only commit never touches it (design section 4.2, 8.3).
	}

	var res sql.Result
	res, err = tx.ExecContext(ctx,
		`UPDATE tickets SET
			state = CASE WHEN ? <> '' THEN ? ELSE state END,
			waiting_on = ?,
			next_poll_at = ?,
			poll_interval_s = ?,
			poll_fingerprint = ?,
			claim_owner = NULL,
			claim_expires_at = NULL
		 WHERE id = ? AND claim_owner = ? AND claim_expires_at = ?`,
		c.Next, c.Next, c.Waiting, nextPollAt, pollIntervalS, pollFingerprint, c.TicketID, c.Owner, formatTime(expires),
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

// verifySessionForTicket errors unless sessionID exists and belongs to
// ticketID, the check a resume commit's Session.ID must pass before this
// transaction touches it (section 6.3: every write scoped to c.TicketID).
func verifySessionForTicket(ctx context.Context, tx *sql.Tx, ticketID, sessionID int64) error {
	var gotTicketID int64
	err := tx.QueryRowContext(ctx, `SELECT ticket_id FROM sessions WHERE id = ?`, sessionID).Scan(&gotTicketID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %d not found", sessionID)
		}
		return fmt.Errorf("get session %d: %w", sessionID, err)
	}
	if gotTicketID != ticketID {
		return fmt.Errorf("session %d belongs to ticket %d, not %d", sessionID, gotTicketID, ticketID)
	}
	return nil
}

// verifyQuestionForTicket errors unless questionID exists, is a "question"
// message, and belongs to ticketID, the check every ResolveQuestions id must
// pass before this transaction resolves it (section 6.3: every write scoped
// to c.TicketID).
func verifyQuestionForTicket(ctx context.Context, tx *sql.Tx, ticketID, questionID int64) error {
	var gotTicketID int64
	var gotType string
	err := tx.QueryRowContext(ctx,
		`SELECT ticket_id, type FROM messages WHERE id = ?`, questionID).Scan(&gotTicketID, &gotType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("question %d not found", questionID)
		}
		return fmt.Errorf("get question %d: %w", questionID, err)
	}
	if gotType != msgTypeQuestion {
		return fmt.Errorf("message %d is type %s, not question", questionID, gotType)
	}
	if gotTicketID != ticketID {
		return fmt.Errorf("question %d belongs to ticket %d, not %d", questionID, gotTicketID, ticketID)
	}
	return nil
}

// verifyParentForTicket errors unless parentID names a message that belongs
// to ticketID, the check every inserted message's ParentID must pass before
// this transaction inserts it (section 6.3: every write scoped to
// c.TicketID) -- otherwise a handler could link a message onto another
// ticket's thread.
func verifyParentForTicket(ctx context.Context, tx *sql.Tx, ticketID, parentID int64) error {
	var gotTicketID int64
	err := tx.QueryRowContext(ctx, `SELECT ticket_id FROM messages WHERE id = ?`, parentID).Scan(&gotTicketID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("parent message %d not found", parentID)
		}
		return fmt.Errorf("get parent message %d: %w", parentID, err)
	}
	if gotTicketID != ticketID {
		return fmt.Errorf("parent message %d belongs to ticket %d, not %d", parentID, gotTicketID, ticketID)
	}
	return nil
}

// upsertSessionTx creates su's session when su.ID is nil, or updates it in
// place when su.ID names an existing one, and returns the session's id
// either way. An update bumps the resumes counter when BumpResumes is set,
// and, when ExternalID is non-nil, fills external_id -- but only while it is
// still NULL, so a second terminalizing commit on the same session (the
// D13 case where Reserve created the session before the runtime call ran,
// and every later commit on it also carries RunResult.SessionID) can never
// clobber the id the first commit recorded. ExternalID pointing at "" is
// rejected outright (F035): an empty external_id never means anything, so
// the whole commit fails rather than storing one.
func upsertSessionTx(ctx context.Context, tx *sql.Tx, ticketID int64, su SessionUpsert) (int64, error) {
	if su.ExternalID != nil && *su.ExternalID == "" {
		return 0, errors.New("upsert session: external_id must not be empty")
	}

	if su.ID != nil {
		if su.BumpResumes {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET resumes = resumes + 1 WHERE id = ?`, *su.ID); err != nil {
				return 0, fmt.Errorf("bump session resumes: %w", err)
			}
		}
		if su.ExternalID != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET external_id = ? WHERE id = ? AND external_id IS NULL`,
				*su.ExternalID, *su.ID); err != nil {
				return 0, fmt.Errorf("set session external_id: %w", err)
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

// updateRunTx terminalizes an existing, ticket-owned run (design D13's
// Reserve placeholder): outcome, exit_code, and agent_seconds only. model is
// never written here -- Reserve already set it, and nothing after Reserve
// may change it. The WHERE clause is the same ticket-ownership subquery
// insertArtifactTx's RunID check reuses: session_id IN (SELECT id FROM
// sessions WHERE ticket_id = ?). Zero rows affected means r.ID does not name
// a run on one of ticketID's sessions.
func updateRunTx(ctx context.Context, tx *sql.Tx, r Run, ticketID int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET outcome = ?, exit_code = ?, agent_seconds = ?
		 WHERE id = ? AND session_id IN (SELECT id FROM sessions WHERE ticket_id = ?)`,
		r.Outcome, r.ExitCode, r.AgentSeconds, r.ID, ticketID,
	)
	if err != nil {
		return fmt.Errorf("update run %d: %w", r.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update run %d: %w", r.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("run %d not owned by ticket %d", r.ID, ticketID)
	}
	return nil
}

// runOwnedByTicketTx reports whether runID names a run on one of ticketID's
// sessions, the check every non-nil Artifact.RunID must pass before this
// transaction inserts it (section 6.3-style scoping, design section 4.5).
func runOwnedByTicketTx(ctx context.Context, tx *sql.Tx, ticketID, runID int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM runs WHERE id = ? AND session_id IN (SELECT id FROM sessions WHERE ticket_id = ?)`,
		runID, ticketID).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check run %d owned by ticket %d: %w", runID, ticketID, err)
	}
	return true, nil
}

// setKindTx sets tickets.kind to kind ("bug" or "feature") when it is
// currently NULL or already kind (design section 4.5's "null-to-value and
// same-to-same succeed"). The UPDATE's WHERE clause matches both cases, but
// modernc.org/sqlite may report zero rows affected for a same-to-same write
// that changes no bytes, so a zero-row result re-reads the live kind and
// only reports a conflict when it actually differs from kind.
func setKindTx(ctx context.Context, tx *sql.Tx, ticketID int64, kind string) error {
	if kind != "bug" && kind != "feature" {
		return fmt.Errorf("kind must be bug or feature, got %q", kind)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET kind = ? WHERE id = ? AND (kind IS NULL OR kind = ?)`,
		kind, ticketID, kind,
	)
	if err != nil {
		return fmt.Errorf("set kind: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set kind: %w", err)
	}
	if n > 0 {
		return nil
	}

	var have sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM tickets WHERE id = ?`, ticketID).Scan(&have); err != nil {
		return fmt.Errorf("read kind for conflict: %w", err)
	}
	if have.Valid && have.String == kind {
		return nil // same-to-same: the driver reported zero rows for a no-op write
	}
	haveStr := nullDisplay
	if have.Valid {
		haveStr = have.String
	}
	return fmt.Errorf("kind conflict: have %s, want %s", haveStr, kind)
}

// setBranchTx sets tickets.branch to branch when it is currently NULL or
// already that same value, mirroring setKindTx's same-to-same and conflict
// handling (design section 4.2): UPDATE tickets SET branch=? WHERE id=? AND
// (branch IS NULL OR branch=?); zero rows affected reads the live value to
// tell a true conflict from a driver that reports zero rows on a no-op
// write.
func setBranchTx(ctx context.Context, tx *sql.Tx, ticketID int64, branch string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET branch = ? WHERE id = ? AND (branch IS NULL OR branch = ?)`,
		branch, ticketID, branch,
	)
	if err != nil {
		return fmt.Errorf("set branch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set branch: %w", err)
	}
	if n > 0 {
		return nil
	}

	var have sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT branch FROM tickets WHERE id = ?`, ticketID).Scan(&have); err != nil {
		return fmt.Errorf("read branch for conflict: %w", err)
	}
	if have.Valid && have.String == branch {
		return nil // same-to-same: the driver reported zero rows for a no-op write
	}
	haveStr := nullDisplay
	if have.Valid {
		haveStr = have.String
	}
	return fmt.Errorf("branch conflict: have %s, want %s", haveStr, branch)
}

// setPRURLTx sets tickets.pr_url to url when it is currently NULL or
// already that same value, mirroring setKindTx's and setBranchTx's
// same-to-same and conflict handling (design section 4.2, 8.2).
func setPRURLTx(ctx context.Context, tx *sql.Tx, ticketID int64, url string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET pr_url = ? WHERE id = ? AND (pr_url IS NULL OR pr_url = ?)`,
		url, ticketID, url,
	)
	if err != nil {
		return fmt.Errorf("set pr url: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set pr url: %w", err)
	}
	if n > 0 {
		return nil
	}

	var have sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT pr_url FROM tickets WHERE id = ?`, ticketID).Scan(&have); err != nil {
		return fmt.Errorf("read pr url for conflict: %w", err)
	}
	if have.Valid && have.String == url {
		return nil // same-to-same: the driver reported zero rows for a no-op write
	}
	haveStr := nullDisplay
	if have.Valid {
		haveStr = have.String
	}
	return fmt.Errorf("pr url conflict: have %s, want %s", haveStr, url)
}

// pollUpdateCount reports how many of c's three poll-update fields are set,
// the input to CommitHandlerResult's "at most one poll update per commit"
// rule (design section 4.2, D8).
func pollUpdateCount(c HandlerCommit) int {
	n := 0
	if c.Poll != nil {
		n++
	}
	if c.PollSchedule != nil {
		n++
	}
	if c.ClearPoll {
		n++
	}
	return n
}

// isHex64Lower reports whether s is exactly 64 lowercase hexadecimal
// characters (design section 4.2's ^[0-9a-f]{64}$ rule for a poll
// fingerprint).
func isHex64Lower(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// insertArtifactTx is InsertArtifact (store.go), tx-scoped: it validates
// a.Payload against the same schema validator and inserts against tx instead
// of s.db, so a commit's artifacts share one transaction with the rest of
// the write, the way insertMessageTx mirrors InsertMessage. Version == 0
// becomes one past (a.TicketID, a.Type)'s current maximum, computed inside
// tx, so two zero-Version artifacts of the same type inserted by the same
// commit each see the prior insert; Version > 0 is stored exactly, and a
// whole-document type's unique-index collision (artifacts_whole_doc_uk)
// surfaces as "artifact <type> version <v> exists" rather than a raw
// constraint error.
func (s *Store) insertArtifactTx(ctx context.Context, tx *sql.Tx, a Artifact) (int64, error) {
	if a.Version < 0 {
		return 0, fmt.Errorf("insert artifact: version %d must not be negative", a.Version)
	}
	if err := s.schemas.validate("artifacts", a.Type, a.Payload); err != nil {
		return 0, err
	}

	version := a.Version
	if version == 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts WHERE ticket_id = ? AND type = ?`,
			a.TicketID, a.Type).Scan(&version); err != nil {
			return 0, fmt.Errorf("next artifact version: %w", err)
		}
	}

	var sealedAt *string
	if a.SealedAt != nil {
		formatted := a.SealedAt.UTC().Format(time.RFC3339)
		sealedAt = &formatted
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts (ticket_id, run_id, type, version, payload, sealed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.TicketID, a.RunID, a.Type, version, string(a.Payload), sealedAt,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, fmt.Errorf("artifact %s version %d exists", a.Type, version)
		}
		return 0, fmt.Errorf("insert artifact: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert artifact: %w", err)
	}
	return id, nil
}

// sealCohortTx applies req inside tx (design D16, section 4.5): (1) the
// ticket's max-version plan artifact must have req.RunID's version and run
// id, else a "plan" mismatch; (2) the scenario cohort that run id carries
// must count exactly req.ExpectedCount rows in [2,30], else a "count"
// mismatch; (3) sealing every still-unsealed row of that cohort at req.At
// must affect exactly req.ExpectedCount rows, else an "update" mismatch
// naming how many it actually affected. req.At is formatted the same way
// insertArtifactTx formats sealed_at: UTC RFC3339.
func sealCohortTx(ctx context.Context, tx *sql.Tx, ticketID int64, req SealRequest) error {
	var planVersion int
	var planRunID sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT version, run_id FROM artifacts WHERE ticket_id = ? AND type = 'plan' ORDER BY version DESC LIMIT 1`,
		ticketID).Scan(&planVersion, &planRunID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &SealMismatchError{Stage: "plan", Expected: req.ExpectedCount}
	case err != nil:
		return fmt.Errorf("seal: read max-version plan artifact: %w", err)
	}
	if planVersion != req.PlanVersion || !planRunID.Valid || planRunID.Int64 != req.RunID {
		return &SealMismatchError{Stage: "plan", Expected: req.ExpectedCount}
	}

	var count int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artifacts WHERE ticket_id = ? AND type = 'scenario' AND run_id = ?`,
		ticketID, req.RunID).Scan(&count); err != nil {
		return fmt.Errorf("seal: count scenario cohort: %w", err)
	}
	if count != req.ExpectedCount || count < 2 || count > 30 {
		return &SealMismatchError{Stage: "count", Expected: req.ExpectedCount}
	}

	sealedAt := req.At.UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		`UPDATE artifacts SET sealed_at = ? WHERE ticket_id = ? AND type = 'scenario' AND run_id = ? AND sealed_at IS NULL`,
		sealedAt, ticketID, req.RunID)
	if err != nil {
		return fmt.Errorf("seal: update sealed_at: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("seal: update sealed_at: %w", err)
	}
	if int(affected) != req.ExpectedCount {
		return &SealMismatchError{Stage: "update", Expected: req.ExpectedCount, Affected: int(affected)}
	}
	return nil
}

// nextQuestionKeyTx returns one past the count of every "question" message
// ticketID carries, of any lifecycle state (design section 6.7): the
// allocation an escalation's linked question, a gate question, and a
// planning question all share, so no two ever collide on the same Q<n>.
func nextQuestionKeyTx(ctx context.Context, tx *sql.Tx, ticketID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND type = ?`, ticketID, msgTypeQuestion,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("next question key: %w", err)
	}
	return n + 1, nil
}

// fillQuestionKeyTx allocates a Q<n> key for a "question" message payload
// that arrives with Key still empty (design section 4.5, 6.7: "gate and
// planning questions use the same Q<n> allocation" as an escalation's linked
// question, escalateTx above), so a handler that posts a gate or a planning
// batch question never computes the allocation itself. A payload that
// already carries a key is returned unchanged. The allocation and this
// message's insert share nextQuestionKeyTx's live COUNT, so two empty-key
// questions in the same commit -- inserted one at a time by
// CommitHandlerResult's Messages loop -- see Q1, then Q2, never a collision.
func fillQuestionKeyTx(ctx context.Context, tx *sql.Tx, ticketID int64, payload json.RawMessage) (json.RawMessage, error) {
	var qp response.QuestionPayload
	if err := json.Unmarshal(payload, &qp); err != nil {
		return nil, fmt.Errorf("fill question key: unmarshal payload: %w", err)
	}
	if qp.Key != "" {
		return payload, nil
	}

	n, err := nextQuestionKeyTx(ctx, tx, ticketID)
	if err != nil {
		return nil, fmt.Errorf("fill question key: %w", err)
	}
	qp.Key = fmt.Sprintf("Q%d", n)

	out, err := json.Marshal(qp)
	if err != nil {
		return nil, fmt.Errorf("fill question key: marshal payload: %w", err)
	}
	return out, nil
}

// escalateTx inserts ec's escalation message, then its linked question
// (design D10, section 6.7): the question is parented to the escalation's
// own id, carries the same run id, is recommended "b", and offers the fixed
// retry/back-to-planning/abandon choice. Both payloads are validated by
// insertMessageTx against their committed schemas.
func (s *Store) escalateTx(ctx context.Context, tx *sql.Tx, ticketID int64, ec EscalationCommit) error {
	// A non-nil RunID must name a run on one of this ticket's own sessions,
	// the same scoping every Artifact.RunID passes: the foreign key alone only
	// proves the run exists, so without this a RunID from another ticket would
	// link the escalation (and its question) through that ticket's session and
	// job (design section 4.5, 6.7).
	if ec.RunID != nil {
		owned, err := runOwnedByTicketTx(ctx, tx, ticketID, *ec.RunID)
		if err != nil {
			return fmt.Errorf("escalation: %w", err)
		}
		if !owned {
			return fmt.Errorf("escalation: run %d does not belong to ticket %d", *ec.RunID, ticketID)
		}
	}

	payload, err := json.Marshal(ec.Payload)
	if err != nil {
		return fmt.Errorf("escalation: marshal payload: %w", err)
	}
	if err = s.insertMessageTx(ctx, tx, Message{
		TicketID: ticketID, RunID: ec.RunID, Type: msgTypeEscalation, Author: authorZing, Body: ec.Body, Payload: payload,
	}); err != nil {
		return fmt.Errorf("escalation: insert escalation message: %w", err)
	}
	escID, err := lastInsertIDTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("escalation: %w", err)
	}

	n, err := nextQuestionKeyTx(ctx, tx, ticketID)
	if err != nil {
		return fmt.Errorf("escalation: %w", err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key:         fmt.Sprintf("Q%d", n),
		Kind:        response.QuestionKindQuestion,
		State:       response.QuestionStateOpen,
		Recommended: "b",
		Options: []response.Option{
			{Key: "a", Text: "Retry"},
			{Key: "b", Text: "Back to planning"},
			{Key: "c", Text: "Abandon"},
		},
	})
	if err != nil {
		return fmt.Errorf("escalation: marshal question payload: %w", err)
	}
	if err = s.insertMessageTx(ctx, tx, Message{
		TicketID: ticketID, RunID: ec.RunID, ParentID: &escID, Type: msgTypeQuestion, Author: authorZing,
		State: new(questionStateOpen), Body: ec.Body + "\n\nHow should Zing proceed?", Payload: qPayload,
	}); err != nil {
		return fmt.Errorf("escalation: insert question message: %w", err)
	}
	return nil
}

// isUniqueConstraintErr reports whether err came from a SQLite UNIQUE
// constraint violation, detected by message text since modernc.org/sqlite's
// error type carries no exported constraint name to switch on.
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// resolveQuestionTx sets one question message's lifecycle state to
// "resolved", but only from "answered": the WHERE clause requires the
// question's current state be "answered", so a still-open (or already
// resolved) question can never be resolved out from under itself. Zero rows
// affected means questionID did not satisfy that, and is reported as an
// error so the whole commit rolls back rather than silently no-op'ing.
func resolveQuestionTx(ctx context.Context, tx *sql.Tx, questionID int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE id = ? AND type = ? AND state = ?`,
		questionStateResolved, questionID, msgTypeQuestion, questionStateAnswered)
	if err != nil {
		return fmt.Errorf("update question state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update question state: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("question %d is not in answered state", questionID)
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
	openSiblings, err = countOpenBatchSiblings(ctx, tx, in.TicketID, q.RunID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: %w", err)
	}

	waitCleared := false
	if openSiblings == 0 {
		// Conditional on waiting_on = 'questions': the batch being fully
		// answered only ever clears the questions wait, and never some other
		// wait flag the ticket might (hypothetically) carry instead.
		// WaitCleared reports whether this update actually cleared
		// something, not just whether the batch was done.
		var res sql.Result
		res, err = tx.ExecContext(ctx,
			`UPDATE tickets SET waiting_on = NULL WHERE id = ? AND waiting_on = ?`, in.TicketID, waitingFlagQuestions)
		if err != nil {
			return AnswerResult{}, fmt.Errorf("answer question: clear wait: %w", err)
		}
		var n int64
		n, err = res.RowsAffected()
		if err != nil {
			return AnswerResult{}, fmt.Errorf("answer question: clear wait: %w", err)
		}
		waitCleared = n > 0
	}

	if err = tx.Commit(); err != nil {
		return AnswerResult{}, fmt.Errorf("answer question: commit tx: %w", err)
	}
	return AnswerResult{Accepted: true, WaitCleared: waitCleared}, nil
}

// countOpenBatchSiblings counts questions still "open" that share runID, the
// batch an answered question belongs to (section 6.3, section 6.6), scoped
// to ticketID so one ticket's batch can never be gated by another ticket's
// open question.
func countOpenBatchSiblings(ctx context.Context, tx *sql.Tx, ticketID int64, runID *int64) (int, error) {
	var n int
	var err error
	if runID == nil {
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE type = ? AND state = ? AND run_id IS NULL AND ticket_id = ?`,
			msgTypeQuestion, questionStateOpen, ticketID,
		).Scan(&n)
	} else {
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE type = ? AND state = ? AND run_id = ? AND ticket_id = ?`,
			msgTypeQuestion, questionStateOpen, *runID, ticketID,
		).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("count open batch siblings: %w", err)
	}
	return n, nil
}
