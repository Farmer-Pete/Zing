// Package dispatch is the tick loop (design section 6.8): reconcile expired
// claims, intake new tickets, pick the next ready candidate by priority,
// claim it under a lease, run its state's handler, and apply the result in
// one fenced, atomic commit. It is the one place that decides what happens
// next; a job.Handler only proposes a commit and writes nothing itself.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"zing/internal/bus"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
	"zing/internal/tracker"
)

// stateQueued is the state every intake insert uses (design section 6.8 step
// 3); every ticket starts there.
const stateQueued = "queued"

// The two machine.toml job names the timeout lookup maps a pipeline state to
// (design section 6.8 step 6): planning's job is "planning", building's job
// is "build". Every other candidate state is a code-only handler and uses
// defaultCodeTimeout.
const (
	statePlanning = "planning"
	stateBuilding = "building"

	jobPlanning = "planning"
	jobBuild    = "build"
)

// defaultCodeTimeout is the claim/run timeout a code-only state's handler
// runs under (design section 6.8 step 6).
const defaultCodeTimeout = 5 * time.Minute

// The message type and author the seal-mismatch marker and every other
// system-authored "update" message this file writes share (design D16,
// section 4.5, 5.3). store.go's own msgTypeUpdate and authorSystem are
// unexported, so this package names its own copies of the same two closed
// values rather than reaching into store's internals.
const (
	msgTypeUpdate = "update"
	authorSystem  = "system"
)

// claimGrace is the extra time a claim's expiry carries past the run
// deadline: checkpoint grace, not run time (design section 6.8 step 7).
const claimGrace = 5 * time.Minute

// postHandlerWriteTimeout bounds the detached context every post-handler
// store write (the commit, the claim release, and the fail-closed
// SetStopped write) runs under: long enough for an ordinary write, short
// enough that a truly wedged store cannot hang the dispatcher forever
// (design section "dispatch" fix 4).
const postHandlerWriteTimeout = 30 * time.Second

// ErrFailClosed is the error Tick returns once a post-run commit comes back
// applied=false or errors: the runtime has already advanced its session, so
// the dispatcher must not re-drive it (Q-runtime, design section 6.8, 13).
// Tick has already called store.SetStopped(true) before returning it, so
// Run exits and a later Tick call returns immediately at the drain-or-stop
// check without touching the runtime again.
var ErrFailClosed = errors.New("dispatch: fail-closed: commit not applied cleanly")

// Binding pairs one store project with the tracker project intake reads for
// it and the assignee rule intake applies (design section 6.8). Bindings is
// a slice so one Dispatcher can serve many projects.
type Binding struct {
	StoreProjectID int64
	TrackerProject string
	Rule           tracker.IntakeRule
	// User is the configured user Zing acts for (cfg.User), named User
	// rather than Owner to stay distinct from Config.Owner, this process's
	// claim identity. intake names it in the pickup comment it posts for
	// every newly inserted ticket.
	User string
}

// Config is the dispatcher's run-time tuning (design section 6.8). Models,
// Budget, and Floor (design section 4.4) are threaded straight into every
// job.Deps runAndCommit builds; task 2 only threads them, nothing in this
// package reads them yet.
type Config struct {
	Interval    time.Duration     // Run's tick period
	MaxParallel int               // the active-run guard (design section 6.8 step 4)
	Owner       string            // this process's claim owner id, <hostname>-<pid>
	Models      map[string]string // alias -> exact model id (config.Models)
	Budget      time.Duration     // time.Duration(cfg.Budget.AgentMinutesPerTicket) * time.Minute
	Floor       response.Severity // config.Review.Floor, parsed
}

// Dispatcher ticks: reconcile, intake, count, pick, claim, run, commit
// (design section 6.8).
type Dispatcher struct {
	store    *store.Store
	tracker  tracker.Tracker
	bus      *bus.Broker
	machine  *machine.Machine
	reg      map[string]job.Handler
	rts      runtime.Set
	bindings []Binding
	cfg      Config
	drainCh  chan struct{}
}

// deferredMechanics names the three section 10 dispatcher mechanics this
// package defers, and their owner, for the one-line startup log New emits
// (the narrowing note, design section 0): a reader sees these are
// deliberate no-ops, not omissions.
const deferredMechanics = "dependency-blocking of tickets with unmerged depends_on children, " +
	"durable resume of an interrupted run, and max_resumes enforcement"

// New validates that every non-terminal state m.States.Order names has a
// handler in reg (job.Validate), so a missing handler fails at startup,
// never at a nil map read mid-tick, and returns a Dispatcher ready to tick.
//
// New's signature matches the plan (design section 6.8, 4.1 D2): rts is
// threaded into job.Deps.Runtimes on every handler call, so a handler
// resolves its job's runtime by the name machine.toml's job.runtime field
// gives it.
func New(
	s *store.Store, tr tracker.Tracker, b *bus.Broker, m *machine.Machine,
	reg map[string]job.Handler, bindings []Binding, cfg Config, rts runtime.Set,
) (*Dispatcher, error) {
	if err := job.Validate(m, reg); err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	slog.Info("deferred section 10 mechanics are explicit no-ops in this package",
		"mechanics", deferredMechanics, "owner", "Package 7")
	return &Dispatcher{
		store: s, tracker: tr, bus: b, machine: m, reg: reg, rts: rts, bindings: bindings, cfg: cfg,
		drainCh: make(chan struct{}, 1),
	}, nil
}

// postHandlerContext returns a detached, bounded context for a post-handler
// store write: detached with context.WithoutCancel so a cancelled handler
// context (the run deadline expiring, or the drain sequence force-cancelling
// the dispatcher's own context) cannot abort recording the commit, the
// claim release, or the fail-closed stopped flag -- the runtime may already
// have advanced past what a cancellation could undo, so these writes must
// still land. Bounded with a fixed timeout so a truly wedged store cannot
// hang the dispatcher forever now that cancellation no longer reaches it
// (design section "dispatch" fix 4). The caller must call the returned
// cancel to release the timer.
func postHandlerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), postHandlerWriteTimeout)
}

// NotifyDrain wakes a running Run promptly once draining has been set,
// rather than leaving it to notice on the next ticker fire (up to
// cfg.Interval, which can race a short drain deadline). The send is
// non-blocking, so repeated notifications before Run consumes one coalesce
// into a single wake.
func (d *Dispatcher) NotifyDrain() {
	select {
	case d.drainCh <- struct{}{}:
	default:
	}
}

// Tick runs one pass (design section 6.8): reconcile, drain-or-stop check,
// intake, the max-parallel count guard, pick, claim, and run-and-commit. It
// is the unit the tests drive directly, for determinism, rather than
// relying on Run's timer.
func (d *Dispatcher) Tick(ctx context.Context) error {
	now := time.Now()

	// 1. Reconcile. ExpireClaims logs "claim expired" per id itself
	// (store/spine.go), so Tick does not repeat that line.
	if _, err := d.store.ExpireClaims(ctx, now); err != nil {
		return fmt.Errorf("dispatch: reconcile: %w", err)
	}

	// 2. Drain or stop: return without starting work.
	draining, stopped, err := d.store.Flags(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: read flags: %w", err)
	}
	if draining || stopped {
		return nil
	}

	// 3. Intake.
	if intakeErr := d.intake(ctx); intakeErr != nil {
		return intakeErr
	}

	// 4. Count: the max-parallel guard.
	active, err := d.store.CountActiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: count active runs: %w", err)
	}
	if active >= d.cfg.MaxParallel {
		return nil
	}

	// 5. Pick.
	candidates, err := d.store.ListReadyCandidates(ctx, d.machine.States.Terminal)
	if err != nil {
		return fmt.Errorf("dispatch: list ready candidates: %w", err)
	}
	ordered := job.OrderCandidates(candidates, d.machine.States.Order)
	if len(ordered) == 0 {
		return nil
	}
	ticket := ordered[0]

	// 6. Claim. Claim and CommitHandlerResult each truncate their own
	// incoming expires to whole-second UTC precision at the store boundary
	// (section 6.3), so this same raw expires, handed to both Claim below
	// and to Deps.Expires / the eventual commit's Expires, fences correctly
	// without this caller truncating it itself. expires is computed from a
	// fresh time.Now() taken right here, after reconcile, intake, and count
	// (steps 1-4, which can each spend real wall time, notably a slow
	// intake) have already run, not the tick-start now: otherwise the
	// lease's actual coverage, measured from the moment it is really
	// claimed, would fall short of timeout + claimGrace by however long
	// those earlier steps took (design section "dispatch" fix 5, cubic P2).
	timeout := d.timeoutFor(ticket.State)
	expires := time.Now().Add(timeout + claimGrace)
	claimed, err := d.store.Claim(ctx, ticket.ID, d.cfg.Owner, expires)
	if err != nil {
		return fmt.Errorf("dispatch: claim ticket %d: %w", ticket.ID, err)
	}
	if !claimed {
		return nil // another worker holds it
	}

	// 7. Run and commit.
	return d.runAndCommit(ctx, ticket, timeout, expires)
}

// Run ticks every cfg.Interval until ctx is done or the drain flag is set.
// It returns as soon as draining is observed, after the current tick
// finishes (design section 6.8). NotifyDrain wakes it promptly rather than
// leaving it to notice only on the next ticker fire.
func (d *Dispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.drainCh:
			draining, _, err := d.store.Flags(ctx)
			if err != nil {
				return ctxErrOr(ctx, fmt.Errorf("dispatch: read flags: %w", err))
			}
			if draining {
				return nil
			}
		case <-ticker.C:
			if err := d.Tick(ctx); err != nil {
				return ctxErrOr(ctx, err)
			}
			draining, _, err := d.store.Flags(ctx)
			if err != nil {
				return ctxErrOr(ctx, fmt.Errorf("dispatch: read flags: %w", err))
			}
			if draining {
				return nil
			}
		}
	}
}

// ctxErrOr returns ctx.Err() in place of err whenever ctx has already been
// canceled or has expired. A store call that straddles the moment ctx ends
// races database/sql's own context-driven teardown (it cancels the
// in-flight statement and, for a transaction, auto-rolls it back), so the
// error that surfaces is whichever side of that race lost -- for example
// "sql: transaction has already been committed or rolled back" or the
// driver's own "interrupted" -- never context.DeadlineExceeded or
// context.Canceled itself, even though ctx ending is what really caused the
// failure. Run's contract is to end because ctx is done; once it is, that
// is the reason to report, not an artifact of an operation ctx cut off
// mid-flight.
func ctxErrOr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// intake runs step 3: for each binding, ask the tracker for its tickets and
// insert every one the store does not already carry for that (project, ref)
// pair (design section 6.8 step 3).
func (d *Dispatcher) intake(ctx context.Context) error {
	for _, b := range d.bindings {
		tickets, err := d.tracker.Intake(ctx, b.TrackerProject, b.Rule)
		if err != nil {
			// A single project's tracker going unreachable must not stop
			// intake for every other project, nor fail the tick (PKG7-PLAN.md
			// D6/section 9: "intake error (serve)"). Package 6 left this
			// fatal; task 14 makes it resilient per-project instead.
			slog.Warn("intake error", "project", b.TrackerProject, "err", err)
			continue
		}
		for _, tk := range tickets {
			_, ok, err := d.store.TicketByRef(ctx, b.StoreProjectID, tk.Ref)
			if err != nil {
				return fmt.Errorf("dispatch: intake dedup check %s: %w", tk.Ref, err)
			}
			if ok {
				continue
			}
			newID, err := d.store.InsertTicket(ctx, store.Ticket{
				ProjectID: b.StoreProjectID, TrackerRef: tk.Ref, Title: tk.Title, Body: tk.Body, State: stateQueued,
			})
			if err != nil {
				return fmt.Errorf("dispatch: intake insert %s: %w", tk.Ref, err)
			}
			// The pickup comment is best-effort: it never fails the tick,
			// never rolls back the row just inserted, and is never retried.
			// The store is the source of truth; the tracker is a mirror, so
			// a lagging mirror is acceptable (design section 6.8 step 3,
			// plan section 6). The comment body is never logged.
			if cErr := d.tracker.Comment(ctx, b.TrackerProject, tk.Ref, tracker.PickupComment(b.User)); cErr != nil {
				slog.Warn("pickup comment failed", "ticket_id", newID, "project", b.TrackerProject, "ref", tk.Ref, "err", cErr)
			}
		}
	}
	return nil
}

// timeoutFor returns the claim/run timeout for state: the state's job
// timeout_minutes for planning and building, or defaultCodeTimeout for
// every other (code-only) state (design section 6.8 step 6).
func (d *Dispatcher) timeoutFor(state string) time.Duration {
	jobName, ok := jobNameForState(state)
	if !ok {
		return defaultCodeTimeout
	}
	j, ok := d.machine.Jobs[jobName]
	if !ok || j.TimeoutMinutes <= 0 {
		return defaultCodeTimeout
	}
	return time.Duration(j.TimeoutMinutes) * time.Minute
}

// jobNameForState maps the two fake-runtime pipeline states to the
// machine.toml job name that names their timeout.
func jobNameForState(state string) (string, bool) {
	switch state {
	case statePlanning:
		return jobPlanning, true
	case stateBuilding:
		return jobBuild, true
	default:
		return "", false
	}
}

// runAndCommit is step 7: run ticket's handler under a context whose
// deadline is (a fresh time.Now(), taken here, right after the claim) +
// timeout, not the tick-start now (so time already spent on reconcile and
// intake earlier in this same Tick never eats into the handler's own
// budget) and not the later claim expiry, validate and apply its commit,
// and resolve one of five outcomes (design section 6.8 step 7, 4.5, D16):
// runtime.ErrCanceled leaves the claim in place for ExpireClaims to
// reconcile; job.ErrNoAction and a store.ErrSealMismatch commit each release
// the claim and continue, never stopping the dispatcher; any other handler
// error or invalid commit releases the claim and leaves the ticket's state
// for a later retry; a lost lease or any other commit error fails the
// dispatcher closed; a valid, applied commit runs its TrackerEffect, if any,
// and publishes. expires is the claim lease Claim was already called with
// (step 6), computed from a fresh post-intake time.Now() taken there (design
// section "dispatch" fix 5), and stays as-is here so it remains consistent
// with what was actually claimed. Every post-handler store write below (the
// commit, the release, and the fail-closed SetStopped) runs under a
// detached, bounded context (postHandlerContext), not ctx or runCtx
// directly, so a cancelled handler context cannot abort recording what the
// runtime already did (design section "dispatch" fix 4).
func (d *Dispatcher) runAndCommit(ctx context.Context, ticket store.Ticket, timeout time.Duration, expires time.Time) error {
	runCtx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
	defer cancel()

	deps := job.Deps{
		Store: d.store, Runtimes: d.rts, Machine: d.machine,
		Models: d.cfg.Models, Budget: d.cfg.Budget, Floor: d.cfg.Floor,
		Owner: d.cfg.Owner, Expires: expires,
		// Reserve closes over this tick's own owner and expires (the same
		// lease Claim above just took out), so a handler's runJob call never
		// sees either directly (design D13, section 4.4, 4.6).
		Reserve: func(reserveCtx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return d.store.Reserve(reserveCtx, ticketID, d.cfg.Owner, expires, su, seed)
		},
	}

	handler, ok := d.reg[ticket.State]
	if !ok {
		// job.Validate at New guarantees every non-terminal state has a
		// handler, and ListReadyCandidates excludes every terminal state, so
		// this is unreachable through a correctly constructed Dispatcher.
		// Treat it as the same recoverable condition as a handler error,
		// rather than panicking on what New already promised could not
		// happen.
		slog.Error("handler error", "ticket_id", ticket.ID, "state", ticket.State, "err", "no handler registered for this state")
		return d.releaseClaim(ctx, ticket.ID, expires)
	}

	commit, err := handler.Run(runCtx, ticket, deps)
	if err == nil {
		err = job.ValidateCommit(ticket, commit)
	}
	if err != nil {
		// runtime.ErrCanceled (design D13, section 4.5, 6.8): the parent
		// context was canceled (dispatcher shutdown), not a failure to
		// escalate. The claim is left in place -- releasing it would clear
		// claim_expires_at, and ExpireClaims reconciles only a lease that
		// has actually expired, so a release here would orphan the run
		// Reserve already wrote with a null outcome. Letting the lease
		// expire is what lets reconcile mark it error instead.
		if errors.Is(err, runtime.ErrCanceled) {
			slog.Warn("run canceled by shutdown", "ticket_id", ticket.ID)
			return nil
		}
		// job.ErrNoAction (design section 4.5, 5.1 step 8): the entry
		// decision found nothing to do this tick. The claim is released the
		// same way any other handler error's is, but never escalates to
		// fail-closed even when the release itself finds the lease already
		// gone: "no action" is not itself a runtime-desync risk.
		if errors.Is(err, job.ErrNoAction) {
			return d.releaseClaimNoStop(ctx, ticket.ID, expires, "claim released, no action")
		}
		// store.ErrClaimLost from a handler that never reached runJob's
		// Reserve (design section 4.6 step 7, section 6.8): the lease this
		// tick claimed was already gone before anything ran, so nothing was
		// reserved and no run needs reconciling. This is the same kind of
		// non-runtime-desync condition ErrNoAction is (job.ErrNoAction,
		// above): log and move on, never fail closed. A post-Reserve lease
		// loss (rt.Run already started or finished under a stale lease) is a
		// different case entirely and still falls through to the generic
		// releaseClaim below, which fails closed exactly as before.
		if errors.Is(err, store.ErrClaimLost) {
			slog.Warn("claim lost before reserve; nothing ran", "ticket_id", ticket.ID, "state", ticket.State)
			return nil
		}
		slog.Error("handler error", "ticket_id", ticket.ID, "state", ticket.State, "err", err)
		return d.releaseClaim(ctx, ticket.ID, expires)
	}

	commitCtx, cancelCommit := postHandlerContext(ctx)
	defer cancelCommit()
	applied, err := d.store.CommitHandlerResult(commitCtx, commit)
	if err != nil {
		// store.ErrSealMismatch (design D16, section 4.5, 6.6 branch 0): a
		// seal transaction mismatch is a TOCTOU race the gate's own pre-check
		// re-runs next tick, bounded at two attempts by branch 0's own
		// escalation. It releases the claim and writes a marker, but never
		// stops the dispatcher.
		var mismatch *store.SealMismatchError
		if errors.Is(err, store.ErrSealMismatch) && errors.As(err, &mismatch) {
			return d.releaseAfterSealMismatch(ctx, ticket.ID, commit, expires, mismatch)
		}
	}
	if err != nil || !applied {
		if err != nil {
			slog.Error("commit failed", "ticket_id", ticket.ID, "err", err)
		} else {
			slog.Error("lease lost", "ticket_id", ticket.ID)
		}
		// A canceled handler context (runCtx above, or ctx itself on the way
		// out) must not stop this flag write from landing: the runtime may
		// already have advanced past what a cancellation could undo, so the
		// stopped flag is the one thing that must still get through.
		stoppedCtx, cancelStopped := postHandlerContext(ctx)
		defer cancelStopped()
		if setErr := d.store.SetStopped(stoppedCtx, true); setErr != nil {
			return fmt.Errorf("dispatch: set stopped after fail-closed on ticket %d: %w", ticket.ID, setErr)
		}
		if err != nil {
			return fmt.Errorf("%w: ticket %d: %w", ErrFailClosed, ticket.ID, err)
		}
		return fmt.Errorf("%w: ticket %d: the lease was lost", ErrFailClosed, ticket.ID)
	}

	d.postCommitTrackerEffect(ctx, ticket, commit)
	d.bus.Publish()
	return nil
}

// releaseAfterSealMismatch is the D16 dispatcher rule (section 4.5, 6.6
// branch 0): log the mismatch at error with the cohort run id the commit's
// own Seal request named, write the "seal mismatch cohort <runID>" marker
// (author system), and release the claim through the same no-op-commit path
// releaseClaim uses, without ever calling SetStopped -- the gate's pre-check
// simply re-runs on the ticket's next tick.
func (d *Dispatcher) releaseAfterSealMismatch(
	ctx context.Context, ticketID int64, commit store.HandlerCommit, expires time.Time, mismatch *store.SealMismatchError,
) error {
	var cohortRunID int64
	if commit.Seal != nil {
		cohortRunID = commit.Seal.RunID
	}
	slog.Error("seal invariant mismatch",
		"ticket_id", ticketID, "cohort_run_id", cohortRunID,
		"stage", mismatch.Stage, "expected", mismatch.Expected, "affected", mismatch.Affected)

	marker := store.Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("seal mismatch cohort %d", cohortRunID),
	}
	return d.releaseClaimNoStop(ctx, ticketID, expires, "claim released after seal mismatch", marker)
}

// postCommitTrackerEffect runs commit.TrackerEffect, if any, only after
// CommitHandlerResult has already applied cleanly (design D12, section 4.5,
// 6.8): it resolves the binding for ticket.ProjectID the same way intake
// (above) resolves one for its pickup comment, builds the nothing_to_do
// comment body, and posts it best-effort -- a failure only warns, since the
// ticket's own state has already committed and must not be undone by a
// tracker-side failure.
func (d *Dispatcher) postCommitTrackerEffect(ctx context.Context, ticket store.Ticket, commit store.HandlerCommit) {
	if commit.TrackerEffect == nil {
		return
	}
	e := commit.TrackerEffect

	b, ok := d.bindingForProject(ticket.ProjectID)
	if !ok {
		slog.Warn("tracker comment failed", "ticket_id", ticket.ID, "ref", e.Ref, "err", "no binding for project")
		return
	}

	body := tracker.NothingToDoComment(b.User, e.Notes)
	// Unlike the store writes above (which detach with WithoutCancel so they
	// still land after a cancel), this comment is best-effort and the commit
	// has already succeeded, so it derives from ctx and is cancelled by a
	// shutdown -- still bounded by postHandlerWriteTimeout, but never able to
	// keep Run alive past the drain deadline on a blocked tracker.
	commentCtx, cancel := context.WithTimeout(ctx, postHandlerWriteTimeout)
	defer cancel()
	if err := d.tracker.Comment(commentCtx, b.TrackerProject, e.Ref, body); err != nil {
		slog.Warn("tracker comment failed", "ticket_id", ticket.ID, "ref", e.Ref, "err", err)
	}
}

// bindingForProject returns the Binding whose StoreProjectID matches
// storeProjectID, the same lookup intake's pickup comment (above) already
// makes implicitly by iterating d.bindings one binding at a time.
func (d *Dispatcher) bindingForProject(storeProjectID int64) (Binding, bool) {
	for _, b := range d.bindings {
		if b.StoreProjectID == storeProjectID {
			return b, true
		}
	}
	return Binding{}, false
}

// releaseClaim clears ticket's claim with a fenced no-op commit: no state
// transition, no run, no message, and no wait change (waiting_on was
// already nil, since a candidate can only be picked while unwaited), so it
// carries only the ownership fence CommitHandlerResult always checks and
// always clears (design section 6.8 step 7, 6.3).
//
// This runs after a handler error or an invalid commit, either of which can
// follow a handler that already drove the runtime forward (a real turn ran
// before the handler failed). So a release error, or the release itself
// finding the lease already gone, gets the same fail-closed treatment as a
// failed post-run commit (runAndCommit above): the dispatcher must not
// assume the release succeeded and keep ticking as if this ticket's claim
// were cleanly freed, since the runtime may already be ahead of what this
// process still believes.
func (d *Dispatcher) releaseClaim(ctx context.Context, ticketID int64, expires time.Time) error {
	noop := store.HandlerCommit{TicketID: ticketID, Owner: d.cfg.Owner, Expires: expires}
	commitCtx, cancelCommit := postHandlerContext(ctx)
	defer cancelCommit()
	applied, err := d.store.CommitHandlerResult(commitCtx, noop)
	if err == nil && applied {
		d.bus.Publish()
		return nil
	}

	if err != nil {
		slog.Error("release claim failed", "ticket_id", ticketID, "err", err)
	} else {
		slog.Error("release claim: lease already lost", "ticket_id", ticketID)
	}
	stoppedCtx, cancelStopped := postHandlerContext(ctx)
	defer cancelStopped()
	if setErr := d.store.SetStopped(stoppedCtx, true); setErr != nil {
		return fmt.Errorf("dispatch: set stopped after fail-closed releasing claim for ticket %d: %w", ticketID, setErr)
	}
	if err != nil {
		return fmt.Errorf("%w: ticket %d: release claim: %w", ErrFailClosed, ticketID, err)
	}
	return fmt.Errorf("%w: ticket %d: release claim: the lease was already lost", ErrFailClosed, ticketID)
}

// releaseClaimNoStop releases ticketID's claim with the same fenced no-op
// commit releaseClaim uses (optionally carrying msgs, such as the
// seal-mismatch marker), but never escalates to fail-closed: unlike an
// ordinary handler error, job.ErrNoAction and a seal-transaction mismatch
// are not themselves signs the runtime has moved ahead of what this process
// believes, so a lease already gone (applied=false) or a write error only
// warns, and the dispatcher keeps ticking (design section 4.5, D16).
// successLog names the event to log at warn when the release actually
// applies.
func (d *Dispatcher) releaseClaimNoStop(ctx context.Context, ticketID int64, expires time.Time, successLog string, msgs ...store.Message) error {
	noop := store.HandlerCommit{TicketID: ticketID, Owner: d.cfg.Owner, Expires: expires, Messages: msgs}
	commitCtx, cancelCommit := postHandlerContext(ctx)
	defer cancelCommit()
	applied, err := d.store.CommitHandlerResult(commitCtx, noop)
	switch {
	case err != nil:
		slog.Warn("claim release failed", "ticket_id", ticketID, "err", err)
	case !applied:
		slog.Warn("claim already lost", "ticket_id", ticketID)
	default:
		slog.Warn(successLog, "ticket_id", ticketID)
		d.bus.Publish()
	}
	return nil
}
