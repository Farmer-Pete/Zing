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
	// msgTypeFollowup is D32's own reopen turn (design section 22.12.2): an
	// owner row, parented to the planning question it reopens, body
	// "reopened". It carries no payload (messagePayloadTypes).
	msgTypeFollowup = "followup"

	authorSystem = "system"
	authorYou    = "you"
	authorZing   = "zing"

	answerStateSent = "sent"

	questionStateOpen     = "open"
	questionStateAnswered = "answered"
	questionStateResolved = "resolved"

	waitingFlagQuestions = "questions"
	// waitingFlagGate mirrors job's own waitingFlagGate (planning.go, the
	// same string response.QuestionKindGate carries): D32's own fence
	// widening (design section 22.12.2) needs to recognize it here too.
	waitingFlagGate = string(response.QuestionKindGate)

	// nullDisplay is the "have" side of a same-to-same conflict message
	// (setKindTx, setBranchTx, setPRURLTx) when the column's current value
	// is NULL, so the three share one literal (goconst).
	nullDisplay = "null"

	// ticketStatePlanning and ticketStateBuilding are the two ticket.state
	// values checkGateApprovalTx's own seal invariant compares against (D32,
	// design section 22.12.3a): this package otherwise treats state as an
	// opaque string owned by internal/job (job.statePlanning,
	// job.stateBuilding), but the invariant itself -- "a commit that moves a
	// ticket from planning to building" -- is store's own to enforce.
	ticketStatePlanning = "planning"
	ticketStateBuilding = "building"

	// gateApproveOptionKey is the gate question's own "Approve" option key
	// (design D8, section 6.6; job.gateOptionApprove's same literal, mirrored
	// here because this package cannot import job): checkGateApprovalTx's
	// own re-check that AID actually picked approve, not reject.
	gateApproveOptionKey = "a"
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
	// WithdrawQuestions resolves each named question, open or answered,
	// inserting one "resolved" message per id, in this commit's own
	// transaction (design section 4.2, M4, D13's merge question): unlike
	// ResolveQuestions, which requires "answered" and errors on anything
	// else, an already-resolved question here is success with no second
	// message (a retried commit, after the owner's Merge now already ran
	// once, converges rather than erroring), and withdrawing a still-open
	// question (POLL finds a loop reopened while a merge question sits
	// unanswered) is exactly what the name is for. An id that is not a
	// question of this commit's own ticket is the error "question <id> is
	// not a question of ticket <t>".
	WithdrawQuestions []int64
	// Seal applies the section 4.5 cohort seal, after Artifacts: the
	// ticket's max-version plan artifact must match RunID and PlanVersion,
	// its scenario cohort (artifacts of type "scenario" carrying that
	// RunID) must count exactly ExpectedCount rows in [2,30], and sealing
	// every one of them at At must affect exactly ExpectedCount rows. Any
	// other outcome is a *SealMismatchError naming the failing stage, and
	// rolls the whole commit back (design D16).
	Seal *SealRequest
	// GateApproval binds a seal to one confirmed gate approval (D32, design
	// section 22.12.3a): required whenever Seal is set, or Next is
	// "building" while the ticket is still in "planning". CommitHandlerResult
	// re-verifies it against fresh reads inside this same transaction
	// (checkGateApprovalTx) before ever calling sealCohortTx or applying
	// Next, and a failing check is a *SealRefusedError that rolls the whole
	// commit back.
	GateApproval *GateApproval
	// Escalation records one structured escalation and its linked question
	// in a single shape (design D10, section 6.7): a nil RunID is a cap
	// escalation that no run caused; a non-nil RunID ties both inserted
	// messages to the run that did.
	Escalation *EscalationCommit
	// TrackerEffect is carried, never applied, by this transaction: the
	// dispatcher runs it against the ticket's tracker only after a
	// successful commit (design D12).
	TrackerEffect *TrackerEffect
	// Conversation applies a planning turn's thread effects (D31, section
	// 22.3): settling the threads this turn's response decided, and the
	// fence that keeps a message sent during the run from being stranded
	// behind a Waiting this commit would otherwise set. Applied right after
	// Messages and before ResolveQuestions.
	Conversation *ConversationCommit
}

// ConversationCommit is one planning turn's thread effects (design section
// 22.3): ThroughBatch is the delivery watermark after this run -- the
// newest owner batch the run received, or W when it received none -- and
// Settle names the threads this turn's response settled.
type ConversationCommit struct {
	ThroughBatch int64
	Settle       []SettleQuestion
}

// SettleQuestion is one thread a planning response settled (design section
// 22.3): QuestionID must name an open or answered planning question of the
// commit's own ticket, and Decision is the one-sentence decision text
// stored on the inserted "resolved" row.
type SettleQuestion struct {
	QuestionID int64
	Decision   string
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
// text, and Kind picks which comment builder the dispatcher's
// postCommitTrackerEffect applies to Notes before posting. CommitHandlerResult
// carries this value through unread; only the dispatcher, after a successful
// commit, resolves Ref and posts the built comment. PUBLISH's PR-link
// comment and DONE's done comment are posted before their own commits
// instead (PKG9-PLAN.md section 8.2, 8.6, 11) and never go through
// TrackerEffect.
type TrackerEffect struct {
	Kind       string
	Ref, Notes string
}

// TrackerEffectKindNothingToDo is TrackerEffect.Kind's value for design
// section 6.8's nothing_to_do row: the dispatcher responds to it with
// tracker.NothingToDoComment. An empty or unrecognized Kind posts nothing
// rather than guessing which comment to send.
const TrackerEffectKindNothingToDo = "nothing_to_do"

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

// GateApproval binds a seal to one confirmed gate approval (D32, design
// section 22.12.3a): QuestionID is the gate question (QID), AnswerID is its
// approving answer row (AID), ApproveBatch is AID's own batch_id (BA), and
// PlanVersion is the cohort version the approval covers.
type GateApproval struct {
	QuestionID   int64
	AnswerID     int64
	ApproveBatch int64
	PlanVersion  int
}

// ErrSealRefused is the sentinel every *SealRefusedError unwraps to (D32,
// design section 22.12.3a), so a caller that only needs to know "was this
// seal refused" can use errors.Is without also importing the typed shape.
var ErrSealRefused = errors.New("store: seal refused")

// SealRefusedError is the seal invariant's own typed failure (D32, design
// section 22.12.3a): Reason is one of the invariant's six fixed texts.
// Error() renders "seal refused: <reason>", the exact prefix design section
// 22.12.3a and the dispatcher's own marker (updateLine) share.
type SealRefusedError struct{ Reason string }

func (e *SealRefusedError) Error() string { return "seal refused: " + e.Reason }

func (e *SealRefusedError) Unwrap() error { return ErrSealRefused }

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
// attaching the single run's id when AttachRunToMsgs is set; apply
// Conversation when set (settle threads, and fence Waiting against a late
// owner message, design section 22.3); resolve each ResolveQuestions id
// and insert its resolved message; write the state message; then apply
// Next, Waiting (as the Conversation fence may have rewritten it), the
// three poll columns (Poll,
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

	// conversationRunID is "the commit's run" a settled thread's decision
	// row is attached to (design section 22.3): the same run attachRunID
	// above derives from, but read regardless of AttachRunToMsgs, since a
	// planning commit's Conversation stands on its own.
	var conversationRunID *int64
	if len(runIDs) > 0 {
		conversationRunID = &runIDs[0]
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

	if err = checkGateApprovalTx(ctx, tx, c.TicketID, ticket.State, c); err != nil {
		return false, fmt.Errorf("commit handler result: %w", err)
	}

	if c.Seal != nil {
		if err = sealCohortTx(ctx, tx, c.TicketID, *c.Seal); err != nil {
			return false, fmt.Errorf("commit handler result: %w", err)
		}
	}

	if c.Escalation != nil {
		if err = s.escalateTx(ctx, tx, c.TicketID, ticket.State, *c.Escalation); err != nil {
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

	for i := range c.Messages {
		m := c.Messages[i]
		// Force every inserted message's ticket_id to c.TicketID: a handler
		// proposes messages but never writes, so this commit boundary, not
		// the handler, is what a message can never be scoped away from.
		// m is a copy, not &c.Messages[i]: the caller's slice must never be
		// mutated by this commit, even if the transaction rolls back.
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

	// waitingOn is what the final ticket UPDATE below actually writes:
	// c.Waiting, unless applyConversationTx's fence clears it (design
	// section 22.3 step 3).
	waitingOn := c.Waiting
	if c.Conversation != nil {
		waitingOn, err = applyConversationTx(ctx, tx, s, c.TicketID, *c.Conversation, c.Waiting, c.Escalation, conversationRunID)
		if err != nil {
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

	for _, qid := range c.WithdrawQuestions {
		if err = s.withdrawQuestionTx(ctx, tx, c.TicketID, qid); err != nil {
			return false, fmt.Errorf("commit handler result: withdraw question %d: %w", qid, err)
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
		c.Next, c.Next, waitingOn, nextPollAt, pollIntervalS, pollFingerprint, c.TicketID, c.Owner, formatTime(expires),
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

// checkGateApprovalTx enforces the seal invariant (D32, design section
// 22.12.3a): every commit that seals a cohort (c.Seal set) or moves a
// ticket from "planning" to "building" (c.Next == "building" while
// ticketState, read at the top of this transaction, is still "planning")
// must carry a c.GateApproval the checks below -- run fresh, inside tx,
// against the same transaction the seal or transition itself commits in --
// accept. Any other commit (ticketState already "building", or moving
// somewhere else entirely) needs no check and returns nil at once. Each
// failing check returns a *SealRefusedError{Reason}, in the six-step order
// the design names; the first failure wins.
func checkGateApprovalTx(ctx context.Context, tx *sql.Tx, ticketID int64, ticketState string, c HandlerCommit) error {
	if c.Seal == nil && (c.Next != ticketStateBuilding || ticketState != ticketStatePlanning) {
		return nil
	}
	ga := c.GateApproval
	if ga == nil {
		return &SealRefusedError{Reason: "no gate approval check"}
	}

	notTheApproval := &SealRefusedError{
		Reason: fmt.Sprintf("answer %d is not the approval of gate question %d", ga.AnswerID, ga.QuestionID),
	}

	// Step 2: QID names a gate question of this ticket, and AID is the
	// newest sent answer on it, picking "a".
	var gotTicketID int64
	var gotKind sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT ticket_id, json_extract(payload, '$.kind') FROM messages WHERE id = ? AND type = ?`,
		ga.QuestionID, msgTypeQuestion,
	).Scan(&gotTicketID, &gotKind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return notTheApproval
	case err != nil:
		return fmt.Errorf("check gate approval: get question %d: %w", ga.QuestionID, err)
	case gotTicketID != ticketID, !gotKind.Valid, gotKind.String != string(response.QuestionKindGate):
		return notTheApproval
	}

	var newestAID int64
	var payload sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id, payload FROM messages WHERE parent_id = ? AND type = ? AND state = ? ORDER BY id DESC LIMIT 1`,
		ga.QuestionID, msgTypeAnswer, answerStateSent,
	).Scan(&newestAID, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return notTheApproval
	case err != nil:
		return fmt.Errorf("check gate approval: newest answer for question %d: %w", ga.QuestionID, err)
	case newestAID != ga.AnswerID, !payload.Valid:
		return notTheApproval
	}
	var ap response.AnswerPayload
	if jsonErr := json.Unmarshal([]byte(payload.String), &ap); jsonErr != nil || ap.Option == nil || *ap.Option != gateApproveOptionKey {
		return notTheApproval
	}

	// Step 3: a confirming marker names this exact QID, AID, and
	// PlanVersion (the run id is wildcarded: any confirming run qualifies).
	confirmPattern := fmt.Sprintf("gate confirmed run %% plan v%d gate %d answer %d", ga.PlanVersion, ga.QuestionID, ga.AnswerID)
	var confirmID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE parent_id = ? AND type = ? AND author = ? AND body LIKE ? ORDER BY id DESC LIMIT 1`,
		ga.QuestionID, msgTypeUpdate, authorSystem, confirmPattern,
	).Scan(&confirmID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &SealRefusedError{Reason: fmt.Sprintf(
			"no confirmation for gate question %d answer %d plan v%d", ga.QuestionID, ga.AnswerID, ga.PlanVersion)}
	case err != nil:
		return fmt.Errorf("check gate approval: find confirming marker: %w", err)
	}

	// Step 4: no cancellation marker for QID has a greater id than the
	// confirming marker step 3 just found.
	cancelPattern := fmt.Sprintf("gate approval cancelled gate %d %%", ga.QuestionID)
	var cancelID sql.NullInt64
	if err = tx.QueryRowContext(ctx,
		`SELECT MAX(id) FROM messages WHERE parent_id = ? AND type = ? AND author = ? AND body LIKE ?`,
		ga.QuestionID, msgTypeUpdate, authorSystem, cancelPattern,
	).Scan(&cancelID); err != nil {
		return fmt.Errorf("check gate approval: find cancellation marker: %w", err)
	}
	if cancelID.Valid && cancelID.Int64 > confirmID {
		return &SealRefusedError{Reason: fmt.Sprintf("approval of gate question %d was cancelled", ga.QuestionID)}
	}

	// Step 5: the owner fence. Any sent owner row (answer, reply, or
	// followup) on a planning question of this ticket, with a batch above
	// BA, refuses the seal: nothing the owner sent after Approve may be
	// sealed past.
	var violatingBatch int64
	err = tx.QueryRowContext(ctx,
		`SELECT m.batch_id FROM messages m
		 WHERE m.ticket_id = ? AND m.author = ? AND m.state = ? AND m.type IN (?, ?, ?)
		   AND m.batch_id > ? AND m.parent_id IN (`+planningQuestionsSQL+`)
		 ORDER BY m.batch_id DESC LIMIT 1`,
		ticketID, authorYou, answerStateSent, msgTypeAnswer, msgTypeReply, msgTypeFollowup,
		ga.ApproveBatch, ticketID,
	).Scan(&violatingBatch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No owner row above BA: the fence holds.
	case err != nil:
		return fmt.Errorf("check gate approval: find later owner message: %w", err)
	default:
		return &SealRefusedError{Reason: fmt.Sprintf(
			"owner wrote after approval (batch %d > %d)", violatingBatch, ga.ApproveBatch)}
	}

	// Step 6: a seal in this same commit must target the approved version.
	if c.Seal != nil && c.Seal.PlanVersion != ga.PlanVersion {
		return &SealRefusedError{Reason: fmt.Sprintf(
			"seal is for plan v%d, approval is for v%d", c.Seal.PlanVersion, ga.PlanVersion)}
	}

	return nil
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

// escalationOptionRetry, escalationOptionBackToPlanning, and
// escalationOptionAbandon are the three escalation option texts
// escalationOptionsFor assembles (goconst: each repeats across commit.go
// and commit_test.go's own want fixtures). The option keys stay literal
// "a"/"b"/"c" at each call site: those are the stable ids this plan's own
// point is that every caller must keep meaning the same thing by them, not
// a value worth hiding behind a name.
const (
	escalationOptionRetry          = "Retry"
	escalationOptionBackToPlanning = "Back to planning"
	escalationOptionAbandon        = "Abandon"
)

// escalationOptionsFor picks the question's options and recommendation
// (#47): post-seal, back to planning cannot run (replanUnsupportedEscalation
// is the only thing it does there), so it's dropped and Retry is always
// recommended. In planning all three options work; the recommendation
// follows the code. Abandon always keeps its "c" key, in both branches: it
// is never renumbered to "b" just because back to planning is missing.
func escalationOptionsFor(ticketState, code string) (options []response.Option, recommended string) {
	if ticketState != ticketStatePlanning {
		return []response.Option{
			{Key: "a", Text: escalationOptionRetry},
			{Key: "c", Text: escalationOptionAbandon},
		}, "a"
	}
	recommended = "a"
	if escalationBackToPlanningCodes[code] {
		recommended = "b"
	}
	return []response.Option{
		{Key: "a", Text: escalationOptionRetry},
		{Key: "b", Text: escalationOptionBackToPlanning},
		{Key: "c", Text: escalationOptionAbandon},
	}, recommended
}

// escalationBackToPlanningCodes is the planning-stage table's own two
// exceptions (#47 item 2): every other code recommends Retry.
var escalationBackToPlanningCodes = map[string]bool{
	string(response.EscalationCodeSplitUnsupported):          true,
	string(response.EscalationCodeNothingToDoWithTrueClaims): true,
}

// escalateTx inserts ec's escalation message, then its linked question
// (design D10, section 6.7): the question is parented to the escalation's
// own id, carries the same run id, and offers whichever options and
// recommendation escalationOptionsFor picks for ticketState and the
// escalation's own code (#47 item 2: post-seal, back to planning cannot
// run, so it is dropped). Both payloads are validated by insertMessageTx
// against their committed schemas.
func (s *Store) escalateTx(ctx context.Context, tx *sql.Tx, ticketID int64, ticketState string, ec EscalationCommit) error {
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

	options, recommended := escalationOptionsFor(ticketState, ec.Payload.Code)
	qPayload, err := json.Marshal(response.QuestionPayload{
		Key:         fmt.Sprintf("Q%d", n),
		Kind:        response.QuestionKindQuestion,
		State:       response.QuestionStateOpen,
		Recommended: recommended,
		Options:     options,
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

// applyConversationTx applies a HandlerCommit's Conversation (design
// section 22.3): each Settle entry resolves its question with a decision
// row, unless a late owner message defers it; then, when waiting would
// have the commit wait on questions with no escalation, and any planning
// question (settled by this call or not) still carries a late owner
// message, the wait is skipped, so a message sent during the run is never
// stranded behind it. It returns the waiting_on value the caller should
// actually write: waiting unchanged, unless the fence clears it.
func applyConversationTx(ctx context.Context, tx *sql.Tx, s *Store, ticketID int64, conv ConversationCommit, waiting *string, escalation *EscalationCommit, runID *int64) (*string, error) {
	for _, settle := range conv.Settle {
		if err := verifyOpenPlanningQuestionTx(ctx, tx, ticketID, settle.QuestionID); err != nil {
			return nil, err
		}
		late, err := lateOwnerMessageTx(ctx, tx, settle.QuestionID, conv.ThroughBatch)
		if err != nil {
			return nil, err
		}
		if late {
			slog.Info("settle deferred", "ticket_id", ticketID, "question_id", settle.QuestionID, "through_batch", conv.ThroughBatch)
			continue
		}
		if _, err = tx.ExecContext(ctx,
			`UPDATE messages SET state = ?, read_at = COALESCE(read_at, ?) WHERE id = ?`,
			questionStateResolved, formatTime(time.Now()), settle.QuestionID,
		); err != nil {
			return nil, fmt.Errorf("settle question %d: %w", settle.QuestionID, err)
		}
		if err = s.insertMessageTx(ctx, tx, Message{
			TicketID: ticketID, ParentID: &settle.QuestionID, Type: msgTypeResolved, Author: authorZing,
			RunID: runID, Body: settle.Decision,
		}); err != nil {
			return nil, fmt.Errorf("settle question %d: insert decision: %w", settle.QuestionID, err)
		}
	}

	out := waiting
	// D32 widens this fence to "gate" (design section 22.12.2): a
	// plan-review run can be in flight when the owner reopens a thread,
	// and its own clean commit would post a fresh gate while that thread
	// is still open. When that happens, the gate question this same
	// commit just inserted is withdrawn too, in this same transaction, so
	// the owner never sees a gate behind an open thread.
	if waiting != nil && escalation == nil && (*waiting == waitingFlagQuestions || *waiting == waitingFlagGate) {
		anyLate, err := anyUnsettledPlanningQuestionHasLateMessageTx(ctx, tx, ticketID, conv.ThroughBatch)
		if err != nil {
			return nil, err
		}
		if anyLate {
			if *waiting == waitingFlagGate {
				if err := withdrawJustPostedGateQuestionTx(ctx, tx, s, ticketID); err != nil {
					return nil, err
				}
			}
			slog.Info("planning wait skipped", "ticket_id", ticketID, "through_batch", conv.ThroughBatch)
			out = nil
		}
	}
	return out, nil
}

// withdrawJustPostedGateQuestionTx finds the ticket's own newest open gate
// question -- the one this same commit just inserted, posted by a
// plan-review run that finished while the owner was reopening a thread
// elsewhere (design section 22.12.2) -- and withdraws it (withdrawQuestionTx):
// a fresh gate behind an open thread is pointless, since the owner's own
// reopen already withdrew the fence's own planning wait.
func withdrawJustPostedGateQuestionTx(ctx context.Context, tx *sql.Tx, s *Store, ticketID int64) error {
	var gateQID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE ticket_id = ? AND type = ? AND json_extract(payload, '$.kind') = ? AND state = ? ORDER BY id DESC LIMIT 1`,
		ticketID, msgTypeQuestion, string(response.QuestionKindGate), questionStateOpen,
	).Scan(&gateQID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil // unreachable in practice: a Waiting == "gate" commit always just posted one
	case err != nil:
		return fmt.Errorf("find gate question to withdraw: %w", err)
	}
	if err := s.withdrawQuestionTx(ctx, tx, ticketID, gateQID); err != nil {
		return fmt.Errorf("withdraw gate question %d: %w", gateQID, err)
	}
	return nil
}

// verifyOpenPlanningQuestionTx errors unless questionID names an open or
// answered planning question (planningQuestionsSQL, conversation_reads.go)
// of ticketID: the check every Conversation.Settle entry must pass before
// this transaction settles it (design section 22.3).
func verifyOpenPlanningQuestionTx(ctx context.Context, tx *sql.Tx, ticketID, questionID int64) error {
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM messages m WHERE m.id = ? AND m.state IN (?, ?) AND m.id IN (`+planningQuestionsSQL+`)`,
		questionID, questionStateOpen, questionStateAnswered, ticketID,
	).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("settle question %d: not an open planning question of ticket %d", questionID, ticketID)
	case err != nil:
		return fmt.Errorf("settle question %d: %w", questionID, err)
	}
	return nil
}

// lateOwnerMessageTx reports whether questionID carries a sent owner
// message (answer or reply) with a batch above throughBatch (design section
// 22.3): the fixed point a settle was asked for, but the owner has since
// written past it.
func lateOwnerMessageTx(ctx context.Context, tx *sql.Tx, questionID, throughBatch int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE parent_id = ? AND author = ? AND type IN (?, ?) AND state = ? AND batch_id > ? LIMIT 1`,
		questionID, authorYou, msgTypeAnswer, msgTypeReply, answerStateSent, throughBatch,
	).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("late owner message for question %d: %w", questionID, err)
	}
	return true, nil
}

// anyUnsettledPlanningQuestionHasLateMessageTx reports whether ticketID
// carries at least one planning question still open or answered -- after
// whatever this commit's own Settle entries just resolved -- with a sent
// owner message above throughBatch (design section 22.3 step 3): the fence
// that keeps a questions wait from stranding a message the run never saw.
func anyUnsettledPlanningQuestionHasLateMessageTx(ctx context.Context, tx *sql.Tx, ticketID, throughBatch int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM messages m WHERE m.author = ? AND m.type IN (?, ?) AND m.state = ? AND m.batch_id > ?
		   AND m.parent_id IN (`+planningQuestionsSQL+` AND q.state IN (?, ?)) LIMIT 1`,
		authorYou, msgTypeAnswer, msgTypeReply, answerStateSent, throughBatch,
		ticketID, questionStateOpen, questionStateAnswered,
	).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("unsettled planning questions with late messages for ticket %d: %w", ticketID, err)
	}
	return true, nil
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

// withdrawQuestionTx is HandlerCommit.WithdrawQuestions' own per-id step
// (design section 4.2, M4): questionID must name a "question" message of
// ticketID, else "question <id> is not a question of ticket <t>"; already
// "resolved" is a no-op success with no second message (an idempotent
// retry after a crash, or after the owner's own answer already resolved
// it concurrently -- TestWithdrawRacesOwnerAnswer); "open" or "answered"
// both move to "resolved" with one inserted "resolved" message, unlike
// resolveQuestionTx, which only ever accepts "answered".
func (s *Store) withdrawQuestionTx(ctx context.Context, tx *sql.Tx, ticketID, questionID int64) error {
	var gotTicketID int64
	var gotType, gotState string
	err := tx.QueryRowContext(ctx,
		`SELECT ticket_id, type, state FROM messages WHERE id = ?`, questionID).Scan(&gotTicketID, &gotType, &gotState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("question %d is not a question of ticket %d", questionID, ticketID)
		}
		return fmt.Errorf("get question %d: %w", questionID, err)
	}
	if gotType != msgTypeQuestion || gotTicketID != ticketID {
		return fmt.Errorf("question %d is not a question of ticket %d", questionID, ticketID)
	}
	if gotState == questionStateResolved {
		return nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE id = ? AND type = ? AND state IN (?, ?)`,
		questionStateResolved, questionID, msgTypeQuestion, questionStateOpen, questionStateAnswered)
	if err != nil {
		return fmt.Errorf("withdraw question %d: %w", questionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("withdraw question %d: %w", questionID, err)
	}
	if n == 0 {
		// The state read above was "open" or "answered", but moved again
		// (resolved by a concurrent write) before this UPDATE's own WHERE
		// clause ran -- the same idempotent success as finding it already
		// resolved, not an error to roll the commit back over.
		return nil
	}

	return s.insertMessageTx(ctx, tx, Message{
		TicketID: ticketID, ParentID: &questionID, Type: msgTypeResolved, Author: authorSystem,
	})
}

// insertMessageTx is InsertMessage (store.go), tx-scoped: it validates
// m.Payload against the same in-package schema validator and inserts
// against tx instead of s.db, so a commit's messages share one transaction
// with the rest of the write. The exported InsertMessage takes no *sql.Tx
// (section 6.3), so CommitHandlerResult and AnswerQuestion cannot call it
// directly.
func (s *Store) insertMessageTx(ctx context.Context, tx *sql.Tx, m Message) error {
	payloadParam, err := messagePayloadParam(s.schemas, m)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, run_id, parent_id, type, author, state, body, payload, batch_id, read_at, event_kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.TicketID, m.RunID, m.ParentID, m.Type, m.Author, m.State, m.Body, payloadParam, m.BatchID, formatTimePtr(m.ReadAt), m.EventKind,
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
