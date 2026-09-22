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

// claimGrace is the extra time a claim's expiry carries past the run
// deadline: checkpoint grace, not run time (design section 6.8 step 7).
const claimGrace = 5 * time.Minute

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
}

// Config is the dispatcher's run-time tuning (design section 6.8).
type Config struct {
	Interval    time.Duration // Run's tick period
	MaxParallel int           // the active-run guard (design section 6.8 step 4)
	Owner       string        // this process's claim owner id, <hostname>-<pid>
}

// Dispatcher ticks: reconcile, intake, count, pick, claim, run, commit
// (design section 6.8).
type Dispatcher struct {
	store    *store.Store
	tracker  tracker.Tracker
	bus      *bus.Broker
	machine  *machine.Machine
	reg      map[string]job.Handler
	rt       runtime.Runtime
	bindings []Binding
	cfg      Config
}

// New validates that every non-terminal state m.States.Order names has a
// handler in reg (job.Validate), so a missing handler fails at startup,
// never at a nil map read mid-tick, and returns a Dispatcher ready to tick.
//
// The plan's New signature (design section 6.8) does not list a
// runtime.Runtime parameter, but job.Deps carries one and a handler cannot
// run without it, so this implementation adds rt as an explicit trailing
// parameter. This is a deliberate deviation from the plan's listed
// signature, noted in the build report.
func New(
	s *store.Store, tr tracker.Tracker, b *bus.Broker, m *machine.Machine,
	reg map[string]job.Handler, bindings []Binding, cfg Config, rt runtime.Runtime,
) (*Dispatcher, error) {
	if err := job.Validate(m, reg); err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	return &Dispatcher{
		store: s, tracker: tr, bus: b, machine: m, reg: reg, rt: rt, bindings: bindings, cfg: cfg,
	}, nil
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

	// 6. Claim. expires is truncated to second precision because SQLite
	// round-trips the stored claim_expires_at TEXT column at second
	// precision (store's formatTime uses RFC 3339 with no fractional
	// seconds); CommitHandlerResult's fence compares the read-back value
	// against this same expires with time.Time.Equal, which is
	// nanosecond-sensitive, so an untruncated expires would never match.
	timeout := d.timeoutFor(ticket.State)
	expires := now.Add(timeout + claimGrace).UTC().Truncate(time.Second)
	claimed, err := d.store.Claim(ctx, ticket.ID, d.cfg.Owner, expires)
	if err != nil {
		return fmt.Errorf("dispatch: claim ticket %d: %w", ticket.ID, err)
	}
	if !claimed {
		return nil // another worker holds it
	}

	// 7. Run and commit.
	return d.runAndCommit(ctx, ticket, timeout, expires, now)
}

// Run ticks every cfg.Interval until ctx is done or the drain flag is set.
// It returns as soon as draining is observed, after the current tick
// finishes (design section 6.8).
func (d *Dispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := d.Tick(ctx); err != nil {
				return err
			}
			draining, _, err := d.store.Flags(ctx)
			if err != nil {
				return fmt.Errorf("dispatch: read flags: %w", err)
			}
			if draining {
				return nil
			}
		}
	}
}

// intake runs step 3: for each binding, ask the tracker for its tickets and
// insert every one the store does not already carry for that (project, ref)
// pair (design section 6.8 step 3).
func (d *Dispatcher) intake(ctx context.Context) error {
	for _, b := range d.bindings {
		tickets, err := d.tracker.Intake(ctx, b.TrackerProject, b.Rule)
		if err != nil {
			return fmt.Errorf("dispatch: intake %s: %w", b.TrackerProject, err)
		}
		for _, tk := range tickets {
			_, ok, err := d.store.TicketByRef(ctx, b.StoreProjectID, tk.Ref)
			if err != nil {
				return fmt.Errorf("dispatch: intake dedup check %s: %w", tk.Ref, err)
			}
			if ok {
				continue
			}
			if _, err := d.store.InsertTicket(ctx, store.Ticket{
				ProjectID: b.StoreProjectID, TrackerRef: tk.Ref, Title: tk.Title, Body: tk.Body, State: stateQueued,
			}); err != nil {
				return fmt.Errorf("dispatch: intake insert %s: %w", tk.Ref, err)
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
// deadline is now+timeout (not the later claim expiry), validate and apply
// its commit, and resolve one of three outcomes (design section 6.8 step
// 7): a handler error or an invalid commit releases the claim and leaves
// the ticket's state for a later retry; a lost lease or a commit error
// fails the dispatcher closed; a valid, applied commit publishes.
func (d *Dispatcher) runAndCommit(ctx context.Context, ticket store.Ticket, timeout time.Duration, expires, now time.Time) error {
	runCtx, cancel := context.WithDeadline(ctx, now.Add(timeout))
	defer cancel()

	deps := job.Deps{Store: d.store, Runtime: d.rt, Owner: d.cfg.Owner, Expires: expires}

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
		slog.Error("handler error", "ticket_id", ticket.ID, "state", ticket.State, "err", err)
		return d.releaseClaim(ctx, ticket.ID, expires)
	}

	applied, err := d.store.CommitHandlerResult(ctx, commit)
	if err != nil || !applied {
		slog.Error("lease lost", "ticket_id", ticket.ID, "applied", applied, "err", err)
		if setErr := d.store.SetStopped(ctx, true); setErr != nil {
			return fmt.Errorf("dispatch: set stopped after fail-closed on ticket %d: %w", ticket.ID, setErr)
		}
		if err != nil {
			return fmt.Errorf("%w: ticket %d: %w", ErrFailClosed, ticket.ID, err)
		}
		return fmt.Errorf("%w: ticket %d: the lease was lost", ErrFailClosed, ticket.ID)
	}

	d.bus.Publish()
	return nil
}

// releaseClaim clears ticket's claim with a fenced no-op commit: no state
// transition, no run, no message, and no wait change (waiting_on was
// already nil, since a candidate can only be picked while unwaited), so it
// carries only the ownership fence CommitHandlerResult always checks and
// always clears (design section 6.8 step 7, 6.3).
func (d *Dispatcher) releaseClaim(ctx context.Context, ticketID int64, expires time.Time) error {
	noop := store.HandlerCommit{TicketID: ticketID, Owner: d.cfg.Owner, Expires: expires}
	applied, err := d.store.CommitHandlerResult(ctx, noop)
	if err != nil {
		return fmt.Errorf("dispatch: release claim for ticket %d: %w", ticketID, err)
	}
	if !applied {
		return fmt.Errorf("dispatch: release claim for ticket %d: the lease was already lost", ticketID)
	}
	d.bus.Publish()
	return nil
}
