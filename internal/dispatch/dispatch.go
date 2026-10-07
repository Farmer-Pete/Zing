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
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"zing/internal/bus"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/proc"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
	"zing/internal/tracker"
)

// stateQueued is the state every intake insert uses (design section 6.8 step
// 3); every ticket starts there.
const stateQueued = "queued"

// The machine.toml job names claimTimeoutFor maps a pipeline state to
// (design section 6.8 step 6, PKG9-PLAN.md section 17.1): queued's own claim
// covers every one of classify's attempts, including its one automatic
// timeout retry (owner decision Q4); planning's job is "planning", building's
// job is "build", reviewing's job is "review" (ROUND runs up to seven lens
// runs in parallel under one job.review.timeout_minutes horizon, PKG9-PLAN.md
// section 6.2), judging's own claim takes the largest of "judge", "build",
// and "perimeter" (a fix step runs in every post-build state, design section
// 5.3) and a 10-minute floor; shipping's own claim takes the largest of
// "respond", "build", and "perimeter", the same reasoning with no floor of
// its own (a respond batch, not just a fix step, can also run inside
// "shipping"). Every other candidate state is a code-only handler and uses
// defaultCodeTimeout.
const (
	statePlanning  = "planning"
	stateBuilding  = "building"
	stateReviewing = "reviewing"
	stateJudging   = "judging"
	stateShipping  = "shipping"

	jobClassify  = "classify"
	jobPlanning  = "planning"
	jobBuild     = "build"
	jobReview    = "review"
	jobJudge     = "judge"
	jobPerimeter = "perimeter"
	jobRespond   = "respond"
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

// ErrConcurrentDrive is Tick's and Run's error when one of them is already
// driving this Dispatcher (design section 4.1): the two must never claim
// and launch at the same time, since both read and mutate d.inflight and
// d.stop. Exported, unlike the plan's own lowercase errConcurrentDrive, so
// a test in the external dispatch_test package can match it with errors.Is
// (a deviation noted in the implementation report).
var ErrConcurrentDrive = errors.New("dispatch: Tick or Run is already running on this dispatcher")

// alertCauseMaxBytes bounds the cause text a fail-closed alert quotes
// (design section 4.6): long enough for a useful message, short enough
// that one oversized error can never make an alert line unwieldy.
const alertCauseMaxBytes = 300

// runResult is one worker's outcome, sent on fill's caller-owned results
// channel (design section 4.2): Err is nil on success, or a *runError
// naming the ticket a worker's runAndCommit call failed on. The failed
// ticket id, when there is one, lives on the *runError itself (PR review
// fix D2): every reader (Tick, Run, finish) only ever consumes Err, and
// reportFirstError names the ticket through stopErr's own *runError, never
// through a field on this struct.
type runResult struct {
	Err error
}

// runError wraps a worker's runAndCommit error with the ticket id it ran
// against (design section 4.6), so reportFirstError and logStopAlert can
// name the failed ticket without Run or Tick threading it through
// separately. Unwrap keeps errors.Is(err, ErrFailClosed) working through the
// wrapper.
type runError struct {
	TicketID int64
	Err      error
}

func (e *runError) Error() string {
	return fmt.Sprintf("ticket %d: %v", e.TicketID, e.Err)
}

func (e *runError) Unwrap() error {
	return e.Err
}

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
	// Mode is the project's config.Intake.Mode value, copied verbatim by
	// ensureBindings (cmd/zing/serve.go) so this package need not import
	// internal/config for one comparison (PKG9-PLAN.md D29): intake skips a
	// binding whose Mode is intakeModeManual. Empty (a Binding literal that
	// predates D29, as every existing test's own still is) behaves as auto,
	// matching config's own default.
	Mode string
}

// intakeModeManual is config.IntakeModeManual copied as a plain string
// (PKG9-PLAN.md D29), the same "own copy of an unexported-to-us constant"
// pattern msgTypeUpdate and authorSystem above already use for store's
// values.
const intakeModeManual = "manual"

// Config is the dispatcher's run-time tuning (design section 6.8). Models
// and Floor (design section 4.4) are threaded straight into every job.Deps
// runAndCommit builds. Interval, MaxParallel, and Budget are the startup
// values only (#81): New copies them into the live Dispatcher.tune, and
// fill, Tick, Run, and runAndCommit read d.tune, under d.mu, from then on,
// so a console-driven SetTuning call changes what they see without a
// restart.
type Config struct {
	Interval    time.Duration     // the startup tick period; see Dispatcher.tune
	MaxParallel int               // the startup active-run guard; see Dispatcher.tune
	Owner       string            // this process's claim owner id, <hostname>-<pid>
	Models      map[string]string // alias -> exact model id (config.Models)
	Budget      time.Duration     // the startup agent budget; see Dispatcher.tune
	Floor       response.Severity // config.Review.Floor, parsed
	// Projects, Sandboxes, RequireSandbox, and Commands are PKG8-PLAN.md
	// section 10's own additions (Sandboxes replacing the single-profile
	// Sandbox, PKG9-PLAN.md section 4.3, 4.7), copied straight into every
	// job.Deps runAndCommit builds: Projects carries what building needs to
	// know about each store project; Sandboxes and RequireSandbox gate
	// every sandboxed job's run (design section 5.5); Commands runs a
	// build unit's test and lint re-runs.
	Projects       map[int64]job.Project
	Sandboxes      sandbox.Set
	RequireSandbox bool
	Commands       job.CommandRunner
	// HostCommands runs a host-kind scenario's check at judging, unsandboxed
	// (#49): serve wires job.NewHostCommandRunner(); selftest leaves it nil.
	HostCommands job.CommandRunner
	// DataDir is the resolved data directory (PKG9-PLAN.md section 4.3,
	// 7.3): the private temp root of every unsandboxed run lives under it.
	DataDir string
	// LensesParallel is config.Review.MaxLensesParallel, copied into every
	// job.Deps runAndCommit builds (PKG9-PLAN.md section 4.3, 6.2): the
	// bound ROUND's own semaphore uses.
	LensesParallel int
	// JudgeCodexHome is the resolved judge_codex_home (PKG9-PLAN.md section
	// 4.3, 4.5, D27), copied into every job.Deps runAndCommit builds:
	// runjob.go's own applySandbox step reads it for a job whose profile is
	// "judge".
	JudgeCodexHome string
	// MergeRule is config.Merge (PKG9-PLAN.md section 4.3, 8.8), copied
	// into every job.Deps runAndCommit builds: mergeDecision's own input
	// for the shipping handler's row 9 automatic merge gate.
	MergeRule job.MergeRule
	// ReviewBots is config.ReviewBots, copied into every job.Deps
	// runAndCommit builds: pollIdle's own input for nudging, then
	// escalating, a required review-bot check that has gone quiet.
	ReviewBots job.ReviewBotRule
	// Now is the clock Tick reads "the current instant" from for picking
	// ready candidates (PKG9-PLAN.md section 17.1): serve leaves it nil, so
	// New defaults it to time.Now; selftest injects a fake clock that
	// advances to a ticket's own next_poll_at whenever a tick finds nothing
	// due (section 17.1), so a babysit poll's backoff can be driven without
	// a real wall-clock wait.
	Now func() time.Time
	// ReclaimForeign makes fill's reconcile step reclaim claims held by any
	// other owner (design section 4.2, 6.3), running reclaimForeign before
	// ExpireClaims and scoping ExpireClaims to this process's own owner so
	// a live orphan's claim is never expired out from under reclaim. Only
	// serve sets it, and only after taking serve.lock (section 6.2): a
	// process without the lock cannot prove every other owner's claim is
	// really dead.
	ReclaimForeign bool
}

// Tuning is the dispatcher's live, console-changeable settings (#81):
// Dispatcher.tune starts as a copy of Config's own Interval, MaxParallel,
// and Budget, and SetTuning is the only way to change it afterward. fill's
// slot guard, Tick and Run's results-channel sizing, and runAndCommit's
// Budget all read it under d.mu instead of d.cfg.
type Tuning struct {
	MaxParallel int
	Interval    time.Duration
	Budget      time.Duration
}

// The three console-changeable setting names (owner decision Q1, Q2):
// SetTuning's and ValidateTuning's name argument, and TuningSetting.Name.
const (
	TuneMaxParallel     = "max_parallel"
	TuneIntervalSeconds = "interval_seconds"
	TuneAgentMinutes    = "agent_minutes_per_ticket"
)

// MaxParallelCeiling is the console's own upper bound for max_parallel
// (owner decision Q2): a zing.toml value above it still works at startup,
// but the console can never set max_parallel above it, so Run sizes its
// results buffer to at least this many slots regardless of the startup
// Config.MaxParallel (finish's own buffer-capacity guarantee).
const MaxParallelCeiling = 64

// TuningSetting is one row of TuningSettings: a console-changeable
// setting's name, its settings-table key, its console label, and its
// bounds (owner decision Q2).
type TuningSetting struct {
	Name  string
	Key   string
	Label string
	Min   int
	Max   int
}

// TuningSettings lists the three console-changeable dispatch settings in
// console display order. ValidateTuning, LoadTuning, and the console's
// Settings view all loop over it instead of naming the three settings
// separately.
var TuningSettings = []TuningSetting{
	{Name: TuneMaxParallel, Key: "dispatch.max_parallel", Label: "Max parallel tickets", Min: 1, Max: MaxParallelCeiling},
	{Name: TuneIntervalSeconds, Key: "dispatch.interval_seconds", Label: "Dispatch interval (seconds)", Min: 1, Max: 86400},
	{Name: TuneAgentMinutes, Key: "budget.agent_minutes_per_ticket", Label: "Agent minutes per ticket", Min: 1, Max: 525600},
}

// TuningError is ValidateTuning's and SetTuning's refusal: the console
// shows Msg to the owner as is, with no further wrapping.
type TuningError struct {
	Msg string
}

func (e *TuningError) Error() string { return e.Msg }

// ValidateTuning returns the TuningSettings entry for name when value is
// inside its bounds, or a *TuningError the console shows the owner as is.
func ValidateTuning(name string, value int) (TuningSetting, error) {
	for _, s := range TuningSettings {
		if s.Name != name {
			continue
		}
		if value < s.Min || value > s.Max {
			return TuningSetting{}, &TuningError{Msg: fmt.Sprintf("%s must be %d to %d", name, s.Min, s.Max)}
		}
		return s, nil
	}
	return TuningSetting{}, &TuningError{Msg: fmt.Sprintf("unknown setting %q", name)}
}

// withTuning returns t with name set to value, in that setting's unit.
// name is assumed already accepted by ValidateTuning; any other name
// leaves t unchanged.
func withTuning(t Tuning, name string, value int) Tuning {
	switch name {
	case TuneMaxParallel:
		t.MaxParallel = value
	case TuneIntervalSeconds:
		t.Interval = time.Duration(value) * time.Second
	case TuneAgentMinutes:
		t.Budget = time.Duration(value) * time.Minute
	}
	return t
}

// The two sources LoadTuning reports for each setting name: TuningSourceStore
// when a valid stored value won, TuningSourceToml when the zing.toml-derived
// base value won (#81).
const (
	TuningSourceStore = "store"
	TuningSourceToml  = "zing.toml"
)

// LoadTuning overlays each valid stored setting from the settings table on
// top of base, the zing.toml-derived startup values (#81, owner decision:
// "a stored value wins once it is set"). A setting whose key is unset,
// empty, not a decimal integer, or outside ValidateTuning's bounds keeps
// base's own value for that setting and is reported as coming from
// zing.toml; any other call returns an error and must not be used.
// sources maps every TuningSettings name to TuningSourceStore or
// TuningSourceToml.
func LoadTuning(ctx context.Context, st *store.Store, base Tuning) (Tuning, map[string]string, error) {
	out := base
	sources := make(map[string]string, len(TuningSettings))
	for _, s := range TuningSettings {
		sources[s.Name] = TuningSourceToml
		raw, ok, err := st.GetSetting(ctx, s.Key)
		if err != nil {
			return base, nil, fmt.Errorf("dispatch: load %s: %w", s.Key, err)
		}
		if !ok || raw == "" {
			continue
		}
		v, convErr := strconv.Atoi(raw)
		if convErr == nil {
			_, convErr = ValidateTuning(s.Name, v)
		}
		if convErr != nil {
			slog.Warn("dispatch: stored setting is invalid, using zing.toml",
				"key", s.Key, "stored", raw, "err", convErr)
			continue
		}
		out = withTuning(out, s.Name, v)
		sources[s.Name] = TuningSourceStore
	}
	return out, sources, nil
}

// Dispatcher ticks: reconcile, intake, count, pick, claim, run, commit
// (design section 6.8), now launching up to cfg.MaxParallel claimed
// tickets' handlers at once (design section 4.1, #45 D1) instead of running
// one ticket per tick to completion before returning.
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

	// mu guards inflight, stop, stopErr, firstErrorReported, and tune
	// (design section 4.1, #81): every read or write of these fields happens
	// under it, including a bare len(inflight).
	mu       sync.Mutex
	inflight map[int64]bool // ticket ids this process is running now
	stop     bool           // set once: no further claim or launch
	stopErr  error          // the first error that set stop; nil when a drain or cancel set it

	// tune is the live Tuning (#81): New copies Config's own Interval,
	// MaxParallel, and Budget into it, and SetTuning is the only way to
	// change it afterward. Guarded by mu above, like inflight.
	tune Tuning

	// tuneMu serializes SetTuning calls across the store write and the
	// d.tune update that follows it (#81), the same reason control.go's
	// logLevelMu exists: without it, two concurrent POST /settings calls
	// could write their two values to the store in one order and to d.tune
	// in the other. Taken before mu above, never after.
	tuneMu sync.Mutex

	// tuneCh wakes a running Run as soon as SetTuning changes
	// interval_seconds (#81), rather than leaving the new interval to apply
	// only on the ticker's next, still-old-interval fire. Capacity 1,
	// made in New; SetTuning sends to it non-blockingly, so repeated
	// interval changes before Run's select loop wakes coalesce into a
	// single wake, the same pattern NotifyDrain uses for drainCh.
	tuneCh chan struct{}

	// firstErrorReported guards reportFirstError's alert 1 (design section
	// 4.6): raised at most once per Dispatcher lifetime, whether the error
	// that stopped the dispatcher arrives in Run's main loop or while
	// finish drains the remaining in-flight workers.
	firstErrorReported bool

	// wg counts launched-but-not-yet-finished workers (design section
	// 4.1): one Add per launch in fill, one Done per worker return, Wait in
	// finish so Run (and Tick) never returns while a worker is still live.
	wg sync.WaitGroup

	// driving enforces one caller of Tick or Run at a time (design section
	// 4.1): both start with a CompareAndSwap and return ErrConcurrentDrive
	// on failure, since concurrent passes would race every read and write
	// of inflight and stop above.
	driving atomic.Bool

	// afterClaimForTest, when non-nil, is called by fill synchronously
	// right after a successful Claim for ticketID, before fill's own
	// stop-check-and-launch critical section (design section 4.2 step 5).
	// It exists only so a test can open a precise, otherwise sub-microsecond
	// race window -- call setStop, or NotifyDrain, or cancel ctx, from
	// another goroutine while this goroutine is paused here -- and is set
	// only through export_test.go's SetAfterClaimForTest, never in
	// production code.
	afterClaimForTest func(ticketID int64)

	// beforeClaimForTest, when non-nil, is called by fill synchronously
	// right before attempting Claim for ticketID, for each candidate in
	// pick order (design section 4.2 step 5). It exists only so a test can
	// block a later candidate's claim attempt in the same pass until a
	// concurrently running worker (launched for an earlier candidate in
	// that same pass) has reached a specific point -- most usefully,
	// stopErrRecordedForTest below firing -- making a race between a
	// worker's own setStop and fill's own return deterministic instead of
	// timing-dependent. Set only through export_test.go's
	// SetBeforeClaimForTest, never in production code.
	beforeClaimForTest func(ticketID int64)

	// stopErrRecordedForTest, when non-nil, is called by setStop
	// synchronously, right after it is the first call to record a non-nil
	// stopErr (design section 4.6), with that same error. It exists only so
	// a test can learn the exact moment reportFirstError's own eventual
	// description became fixed, without polling or sleeping. Set only
	// through export_test.go's SetStopErrRecordedForTest, never in
	// production code.
	stopErrRecordedForTest func(err error)
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	slog.Info("deferred section 10 mechanics are explicit no-ops in this package",
		"mechanics", deferredMechanics, "owner", "Package 7")
	return &Dispatcher{
		store: s, tracker: tr, bus: b, machine: m, reg: reg, rts: rts, bindings: bindings, cfg: cfg,
		tune:     Tuning{MaxParallel: cfg.MaxParallel, Interval: cfg.Interval, Budget: cfg.Budget},
		drainCh:  make(chan struct{}, 1),
		tuneCh:   make(chan struct{}, 1),
		inflight: make(map[int64]bool),
	}, nil
}

// SetTuning validates value against name's bounds (ValidateTuning), writes
// it to the settings table together with the changer (by) and the current
// time in one transaction, and then updates the live Tuning fill, Tick,
// Run, and runAndCommit read (#81, owner decisions Q2, Q3). tuneMu holds
// the store write and the d.tune update together, so two concurrent
// SetTuning calls can never write the store in one order and d.tune in the
// other. The store write happens first: if it fails, d.tune is unchanged
// and the caller gets the error back untouched.
func (d *Dispatcher) SetTuning(ctx context.Context, name string, value int, by string) error {
	s, err := ValidateTuning(name, value)
	if err != nil {
		return err
	}
	d.tuneMu.Lock()
	defer d.tuneMu.Unlock()
	at := d.cfg.Now().UTC().Format(time.RFC3339)
	if err := d.store.SetSettings(ctx, s.Key, strconv.Itoa(value),
		s.Key+".changed_by", by, s.Key+".changed_at", at); err != nil {
		return fmt.Errorf("dispatch: set %s: %w", name, err)
	}
	d.mu.Lock()
	d.tune = withTuning(d.tune, name, value)
	d.mu.Unlock()
	if name == TuneIntervalSeconds {
		select {
		case d.tuneCh <- struct{}{}:
		default:
		}
	}
	slog.Info("dispatch: setting changed", "name", name, "value", value, "by", by)
	return nil
}

// CurrentTuning returns the live Tuning, read under d.mu like every other
// access to d.tune (#81): the console's Settings view reads it to show the
// owner the value actually in effect, not just what is stored.
func (d *Dispatcher) CurrentTuning() Tuning {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tune
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
// cfg.Interval, which can race a short drain deadline). It first calls
// setStop(nil) (design section 4.5): from that moment fill launches nothing
// more, whether or not Run's select loop has yet woken on drainCh, so a
// pass that is mid-claim when drain begins still stops at (or right after)
// its very next claim. The drainCh send itself stays non-blocking, so
// repeated notifications before Run consumes one coalesce into a single
// wake.
func (d *Dispatcher) NotifyDrain() {
	d.setStop(nil)
	select {
	case d.drainCh <- struct{}{}:
	default:
	}
}

// setStop records that no further claim or launch may happen (design
// section 4.1): stop is set unconditionally, and stopErr is set to err only
// while it is still nil, so the first non-nil error any caller reports
// wins and is never overwritten by a later one -- including a later nil
// from a drain or a context cancellation, which must never erase a real
// error already recorded. It returns whether this call was the first to
// set stop, though no caller in this package currently needs that signal.
func (d *Dispatcher) setStop(err error) bool {
	d.mu.Lock()
	first := !d.stop
	d.stop = true
	recorded := false
	if d.stopErr == nil {
		d.stopErr = err
		recorded = err != nil
	}
	hook := d.stopErrRecordedForTest
	d.mu.Unlock()

	if recorded && hook != nil {
		hook(err)
	}
	return first
}

// isStopped reports whether setStop has been called yet (design section
// 4.1), read under d.mu like every other access to stop.
func (d *Dispatcher) isStopped() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stop
}

// Tick runs one pass synchronously (design section 4.3): fill, then wait
// for every worker fill launched this pass to finish, then return their
// joined errors. It is the unit the tests drive directly, for determinism,
// rather than relying on Run's timer; cmd/zing/selftest.go and the e2e
// suites also drive it directly. Only one of Tick or Run may be driving
// this Dispatcher at a time (design section 4.1): a second call while one
// is already in flight returns ErrConcurrentDrive rather than racing fill's
// own claim-and-launch critical section.
func (d *Dispatcher) Tick(ctx context.Context) error {
	if !d.driving.CompareAndSwap(false, true) {
		return ErrConcurrentDrive
	}
	defer d.driving.Store(false)

	d.mu.Lock()
	slots := d.tune.MaxParallel
	d.mu.Unlock()
	results := make(chan runResult, slots)
	launched, fillErr := d.fill(ctx, results)
	if fillErr != nil {
		d.setStop(fillErr)
	}

	errs := make([]error, 0, launched+1)
	errs = append(errs, fillErr)
	for range launched {
		r := <-results
		errs = append(errs, r.Err)
	}

	err := errors.Join(errs...)
	if err != nil {
		// D3: a fail-closed (or any other) error raises both alerts even
		// from Tick, so the console's alerts strip shows them the same way
		// a real serve's Run would (design section 4.3, 4.6).
		d.reportFirstError()
		d.logStopAlert()
	}
	return err
}

// Run ticks every cfg.Interval until ctx is done, the drain flag is
// observed, or a fill pass or a launched worker reports an error (design
// section 4.4). Unlike Tick, Run never blocks the caller between passes:
// each tick's fill only launches workers and returns; Run's own select loop
// keeps receiving their results so a free slot is ready for the next
// ticker fire, and a worker's error stops future launches and begins the
// drain-then-return sequence immediately rather than waiting for the next
// tick to notice. Only one of Tick or Run may drive this Dispatcher at a
// time; see Tick's own doc comment.
func (d *Dispatcher) Run(ctx context.Context) error {
	if !d.driving.CompareAndSwap(false, true) {
		return ErrConcurrentDrive
	}
	defer d.driving.Store(false)

	results := make(chan runResult, max(MaxParallelCeiling, d.cfg.MaxParallel))
	interval := d.CurrentTuning().Interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.setStop(nil)
			return d.finish(ctx.Err(), results)

		case <-d.drainCh:
			draining, _, err := d.store.Flags(ctx)
			if err != nil {
				return d.stopOnPassError(ctx, fmt.Errorf("dispatch: read flags: %w", err), results)
			}
			if draining {
				d.setStop(nil)
				return d.finish(nil, results)
			}

		case <-d.tuneCh:
			if iv := d.CurrentTuning().Interval; iv != interval {
				slog.Info("dispatch: interval reset", "old", interval, "new", iv)
				interval = iv
				ticker.Reset(iv)
			}

		case r := <-results:
			if r.Err != nil {
				return d.finish(r.Err, results)
			}
			// A nil result only frees a slot; the next ticker fire refills
			// it (design section 4.4) -- this keeps one place, the ticker,
			// that starts passes.

		case <-ticker.C:
			draining, _, err := d.store.Flags(ctx)
			if err != nil {
				return d.stopOnPassError(ctx, fmt.Errorf("dispatch: read flags: %w", err), results)
			}
			if draining {
				d.setStop(nil)
				return d.finish(nil, results)
			}
			if _, fillErr := d.fill(ctx, results); fillErr != nil {
				return d.stopOnPassError(ctx, fillErr, results)
			}
		}
	}
}

// stopOnPassError ends Run after a store read or a fill pass failed. When
// ctx is already done, the failure is only the cancel cutting a statement
// off mid-flight (database/sql then reports its own rollback error, not
// the context's), so Run stops as for a cancel: no stop
// error, no alerts, and ctx.Err() as the result. Otherwise the error stops
// the dispatcher and raises both alerts (design section 4.6).
func (d *Dispatcher) stopOnPassError(ctx context.Context, err error, results <-chan runResult) error {
	if ctx.Err() != nil {
		d.setStop(nil)
		return d.finish(ctx.Err(), results)
	}
	d.setStop(err)
	return d.finish(err, results)
}

// finish is Run's (and Tick's own fail-closed path's) shutdown join (design
// section 4.4): it calls reportFirstError (alert 1) up front, before
// anything else, so every caller -- a worker error, a flags-read failure, or
// a fill error -- raises alert 1 before alert 2, matching Tick's behavior
// (reportFirstError is idempotent and a no-op when d.stopErr is nil). It
// then waits for every worker fill ever launched to call d.wg.Done(),
// draining results throughout so no worker ever blocks on a full channel,
// joining every non-nil result error into err and reporting it too (the
// same idempotent call, in case a worker error arrives only here). It
// returns only after every launched worker has actually returned, so Run
// (and the goroutine serve starts it in) never lets the store close under a
// live handler. Once the wait is over, alert 2 (design section 4.6) is
// logged if any error was ever reported during this Dispatcher's lifetime.
func (d *Dispatcher) finish(err error, results <-chan runResult) error {
	d.reportFirstError()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	waiting := true
	for waiting {
		select {
		case r := <-results:
			if r.Err != nil {
				d.reportFirstError()
				err = errors.Join(err, r.Err)
			}
		case <-done:
			waiting = false
		}
	}

	// Every worker sends its result before calling wg.Done (design section
	// 4.2 step 5), so by the time d.wg.Wait() above returned, every
	// in-flight worker's value is already sitting in results' buffer even
	// if Go's select happened to pick the done case first above. results'
	// capacity is max(MaxParallelCeiling, the startup max_parallel), at
	// least every console-reachable max_parallel (#81): a worker that still
	// finds it full blocks before wg.Done, so this drain is never skipped
	// while a send is still pending. Drain it now, without blocking.
	for {
		select {
		case r := <-results:
			if r.Err != nil {
				d.reportFirstError()
				err = errors.Join(err, r.Err)
			}
		default:
			if d.hasStopErr() {
				d.logStopAlert()
			}
			return err
		}
	}
}

// hasStopErr reports whether stopErr has ever been set to a non-nil error
// (design section 4.6): finish's own signal for whether alert 2 belongs on
// the way out, since a plain drain or ctx cancellation (stopErr left nil)
// must never raise it.
func (d *Dispatcher) hasStopErr() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopErr != nil
}

// reportFirstError logs alert 1 (design section 4.6) at most once per
// Dispatcher lifetime, naming d.stopErr -- the error saved by the first
// setStop call that carried one -- never whichever result happened to
// arrive first at a caller's own select. Both Tick and Run call it from
// every place an error can first become visible (a worker's result, or a
// fill error), so whichever happens first is what gets reported, matching
// setStop's own "first error wins" rule.
func (d *Dispatcher) reportFirstError() {
	d.mu.Lock()
	if d.firstErrorReported || d.stopErr == nil {
		d.mu.Unlock()
		return
	}
	d.firstErrorReported = true
	err := d.stopErr
	inFlight := len(d.inflight)
	d.mu.Unlock()

	kind, where, ticketID, hasTicket := alertKindWhere(err)
	cause := truncateCause(alertCause(err), alertCauseMaxBytes)
	msg := fmt.Sprintf("%s %s: %s. Finishing %d other run(s), then stopping.", kind, where, cause, inFlight)
	if hasTicket {
		slog.Error(msg, "ticket_id", ticketID, "in_flight", inFlight)
	} else {
		slog.Error(msg, "in_flight", inFlight)
	}
}

// logStopAlert logs alert 2 (design section 4.6): called once, after
// d.wg.Wait() has returned, whenever any error was ever reported during
// this Dispatcher's lifetime (hasStopErr above).
func (d *Dispatcher) logStopAlert() {
	d.mu.Lock()
	err := d.stopErr
	d.mu.Unlock()
	if err == nil {
		return
	}

	kind, where, ticketID, hasTicket := alertKindWhere(err)
	msg := fmt.Sprintf("dispatcher stopped after %s %s. Restart zing serve to resume.", kind, where)
	if hasTicket {
		slog.Error(msg, "ticket_id", ticketID)
	} else {
		slog.Error(msg)
	}
}

// alertKindWhere derives the two alerts' shared kind and where from err
// (design section 4.6): kind is "fail-closed" when err wraps ErrFailClosed,
// else "error"; where is "on ticket <id>" when err is a *runError, else "in
// a dispatcher pass" (a reconcile, flags, intake, list, or claim failure
// from fill itself).
func alertKindWhere(err error) (kind, where string, ticketID int64, hasTicket bool) {
	if re, ok := errors.AsType[*runError](err); ok {
		ticketID = re.TicketID
		hasTicket = true
		where = fmt.Sprintf("on ticket %d", re.TicketID)
	} else {
		where = "in a dispatcher pass"
	}
	if errors.Is(err, ErrFailClosed) {
		kind = "fail-closed"
	} else {
		kind = "error"
	}
	return kind, where, ticketID, hasTicket
}

// alertCause returns the text alert 1 quotes as "cause" (design section
// 4.6): a *runError's own underlying Err, never the wrapper's "ticket %d:"
// prefix (where already names the ticket), or err's own message when it
// carries no ticket at all.
func alertCause(err error) string {
	if re, ok := errors.AsType[*runError](err); ok {
		return re.Err.Error()
	}
	return err.Error()
}

// truncateCause cuts s to at most maxBytes bytes, backing up to a valid
// rune boundary rather than splitting one (design section 4.6).
func truncateCause(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// fill runs one reconcile-intake-pick-claim-launch pass (design section
// 4.2): it replaces the body of the old Tick, except that step 7 (today's
// run-and-commit) now only launches a worker goroutine per claimed ticket,
// up to cfg.MaxParallel concurrently in flight for this process, instead of
// running the one picked ticket to completion inline. The caller owns
// results; fill only ever sends to it, through the worker goroutines it
// starts. It returns the number of workers launched this pass and the
// first error that ended it early (a reconcile, flags, intake, list, or
// claim failure) -- never a worker's own error, which always arrives later,
// on results.
func (d *Dispatcher) fill(ctx context.Context, results chan<- runResult) (int, error) {
	now := time.Now()

	// 1. Reconcile, in the order design section 4.2 step 1 and 6.3 give:
	// with a lock-holding serve reclaiming foreign claims itself, reclaim
	// runs first (it alone may clear a claim this process does not own),
	// then ExpireClaims is scoped to this process's own owner, so a live
	// orphan's claim already visited by reclaim is never also expired out
	// from under it. Without ReclaimForeign, ExpireClaims keeps today's
	// behavior of expiring every owner's claims (tests, selftest, a serve
	// without the lock).
	expireOwner := ""
	if d.cfg.ReclaimForeign {
		if err := d.reclaimForeign(ctx); err != nil {
			return 0, fmt.Errorf("dispatch: reclaim foreign: %w", err)
		}
		expireOwner = d.cfg.Owner
	}
	if err := d.clearDeadExpiringChecks(ctx, now, expireOwner); err != nil {
		return 0, fmt.Errorf("dispatch: reconcile: %w", err)
	}
	if _, err := d.store.ExpireClaims(ctx, now, expireOwner); err != nil {
		return 0, fmt.Errorf("dispatch: reconcile: %w", err)
	}

	// 2. Drain or stop: return without starting work.
	draining, stopped, err := d.store.Flags(ctx)
	if err != nil {
		return 0, fmt.Errorf("dispatch: read flags: %w", err)
	}
	if draining || stopped || d.isStopped() {
		return 0, nil
	}

	// 3. Intake.
	if intakeErr := d.intake(ctx); intakeErr != nil {
		return 0, intakeErr
	}

	// 4. Pick. cfg.Now() (defaulted to time.Now in New when serve leaves it
	// nil), not fill's own start-of-pass now above (which only bounds
	// ExpireClaims' own reconcile pass), is what ListReadyCandidates
	// compares next_poll_at against, so selftest's injected fake clock
	// governs candidacy the same way a real poll schedule would
	// (PKG9-PLAN.md section 17.1).
	candidates, err := d.store.ListReadyCandidates(ctx, d.machine.States.Terminal, d.cfg.Now())
	if err != nil {
		return 0, fmt.Errorf("dispatch: list ready candidates: %w", err)
	}
	ordered := job.OrderCandidates(candidates, d.machine.States.Order)

	// 5. Claim and launch every free slot, in priority order. A refused
	// claim (another worker, of this process or another, already holds the
	// ticket) moves on to the next candidate rather than ending the pass.
	launched := 0
	for i := range ordered {
		// Indexed, not "for _, ticket := range ordered": store.Ticket is
		// large enough that gocritic's rangeValCopy flags a per-iteration
		// value copy here, and every use below only ever needs a field or,
		// once, a copy to hand the worker goroutine (which must own one
		// regardless).
		id, state := ordered[i].ID, ordered[i].State

		d.mu.Lock()
		if d.stop || len(d.inflight) >= d.tune.MaxParallel {
			d.mu.Unlock()
			break
		}
		if d.inflight[id] {
			d.mu.Unlock()
			continue
		}
		d.mu.Unlock()

		// expires is computed from a fresh time.Now() per candidate, not
		// fill's own start-of-pass now: otherwise the lease's actual
		// coverage, measured from the moment it is really claimed, would
		// fall short of timeout + claimGrace by however long reconcile,
		// flags, intake, and list (and any earlier candidate in this same
		// loop) already spent (design section "dispatch" fix 5, cubic P2).
		timeout := d.claimTimeoutFor(state)
		expires := time.Now().Add(timeout + claimGrace)
		if d.beforeClaimForTest != nil {
			d.beforeClaimForTest(id)
		}
		claimed, err := d.store.Claim(ctx, id, d.cfg.Owner, expires)
		if err != nil {
			return launched, fmt.Errorf("dispatch: claim ticket %d: %w", id, err)
		}
		if !claimed {
			continue // another worker holds it
		}
		if d.afterClaimForTest != nil {
			d.afterClaimForTest(id)
		}

		// This is the launch linearization point (design section 4.2 step
		// 5): setStop also takes d.mu, so a stop set before this section
		// prevents the launch below, and a stop set after it finds the run
		// already launched (and drained like any other, by finish). ctx is
		// also checked here, not only d.stop (PR review fix D1): a ctx
		// cancellation that is not routed through setStop at all -- the
		// force-cancel at the drain deadline races this exact window too,
		// same as a stop -- must still release the claim through the
		// detached path below rather than launch a worker against an
		// already-cancelled context.
		d.mu.Lock()
		if d.stop || ctx.Err() != nil {
			d.mu.Unlock()
			if relErr := d.releaseClaimNoStop(ctx, id, expires, "claim released, dispatcher stopping"); relErr != nil {
				return launched, relErr
			}
			break
		}
		d.inflight[id] = true
		d.wg.Add(1)
		go d.worker(ctx, ordered[i], timeout, expires, results)
		d.mu.Unlock()
		launched++
	}

	return launched, nil
}

// worker runs one claimed ticket's handler to completion and reports the
// outcome on results (design section 4.2 step 5). A non-nil error stops the
// dispatcher (setStop) before anything else -- including before this
// worker's own inflight entry is cleared -- so a fill pass still claiming
// when this worker fails sees the stop at its very next candidate check, or
// right after its own claim (fill's own two stop checks), per the fail-
// closed guarantee (design D3, section 4.2).
func (d *Dispatcher) worker(ctx context.Context, ticket store.Ticket, timeout time.Duration, expires time.Time, results chan<- runResult) {
	err := d.runAndCommit(ctx, ticket, timeout, expires)
	if err != nil {
		err = &runError{TicketID: ticket.ID, Err: err}
		d.setStop(err)
	}

	// Send before leaving inflight: a finished worker whose result the
	// caller has not read yet still holds its slot, so fill can never
	// launch more than MaxParallel workers even while results sit unread.
	results <- runResult{Err: err}

	d.mu.Lock()
	delete(d.inflight, ticket.ID)
	d.mu.Unlock()
	d.wg.Done()
}

// reclaimForeign reclaims the claims of a dead serve (design section 6.3):
// for every ticket claimed by an owner other than cfg.Owner, it reclaims
// the claim once neither an open run of that ticket's sessions nor its
// recorded CHECK command (#55) still has a live process group, killing a
// group that has outlived its job's timeout plus claimGrace (unless its
// liveness could not be verified, in which case it is never killed). It
// runs inside fill's own reconcile step, before ExpireClaims, only when
// cfg.ReclaimForeign is set.
func (d *Dispatcher) reclaimForeign(ctx context.Context) error {
	claims, err := d.store.ForeignClaims(ctx, d.cfg.Owner)
	if err != nil {
		return fmt.Errorf("foreign claims: %w", err)
	}

	for _, c := range claims {
		anyLive := false
		for _, r := range c.Open {
			if d.evaluateOrphan(c.TicketID, r, orphanProcessAgent) {
				anyLive = true
			}
		}
		if c.Check != nil && d.evaluateOrphan(c.TicketID, c.Check.AsOpenRun(), orphanProcessCheck) {
			anyLive = true
		}
		if anyLive {
			continue
		}

		applied, err := d.store.ReclaimClaim(ctx, c.TicketID, c.Owner, c.Expires, c.Check)
		if err != nil {
			return fmt.Errorf("reclaim claim ticket %d: %w", c.TicketID, err)
		}
		if applied {
			slog.Warn("claim reclaimed from dead serve", "ticket_id", c.TicketID, "old_owner", c.Owner)
		}
	}
	return nil
}

// clearDeadExpiringChecks judges the CHECK command of every claim
// ExpireClaims(now, onlyOwner) is about to consider, by the same rules
// reclaimForeign applies: ExpireClaims skips a ticket
// that still records a CHECK command, so this deletes the row of a command
// judged gone -- only while the row still names that exact process -- and
// leaves a live one's claim held.
func (d *Dispatcher) clearDeadExpiringChecks(ctx context.Context, now time.Time, onlyOwner string) error {
	checks, err := d.store.ExpiringChecks(ctx, now, onlyOwner)
	if err != nil {
		return fmt.Errorf("expiring checks: %w", err)
	}
	for _, c := range checks {
		if d.evaluateOrphan(c.TicketID, c.Check.AsOpenRun(), orphanProcessCheck) {
			continue
		}
		cleared, err := d.store.ClearDeadCheck(ctx, c.TicketID, c.Check)
		if err != nil {
			return fmt.Errorf("clear dead check ticket %d: %w", c.TicketID, err)
		}
		slog.Info("expired claim's check command gone", "ticket_id", c.TicketID, "pgid", c.Check.PGID, "cleared", cleared)
	}
	return nil
}

// orphanProcessAgent and orphanProcessCheck name what evaluateOrphan is
// judging, for its logs: an agent run's process group, or a CHECK
// command's (#55).
const (
	orphanProcessAgent = "agent"
	orphanProcessCheck = "check"
)

// orphanLiveness classifies one open run's process group against its
// recorded identity (design section 6.3).
type orphanLiveness int

const (
	orphanDead orphanLiveness = iota
	orphanLive
	// orphanUnverifiedLive is a group GroupAlive reports alive with no
	// recorded start token to confirm it is really the run's own group
	// (design section 6.3): never killed, since Zing cannot tell it apart
	// from an unrelated group that happened to reuse the same id.
	orphanUnverifiedLive
)

// classifyOpenRun decides r's liveness (design section 6.3):
//   - PGID nil: dead (no agent was ever recorded for this run).
//   - ProcStart recorded and the live group's own StartToken matches it: live.
//   - ProcStart recorded and StartToken reports ErrNoProcess (the leader
//     exited but the group id cannot yet be reused while a descendant is
//     still in it): live.
//   - ProcStart recorded and the token differs (the pid was reused by an
//     unrelated process): dead.
//   - ProcStart nil (no token recorded, whichever platform or read failure
//     caused that): live but unverified when GroupAlive reports a member,
//     dead otherwise.
func classifyOpenRun(r store.OpenRun) orphanLiveness {
	if r.PGID == nil {
		return orphanDead
	}
	pgid := *r.PGID

	if r.ProcStart == nil {
		if proc.GroupAlive(pgid) {
			return orphanUnverifiedLive
		}
		return orphanDead
	}

	token, err := proc.StartToken(pgid)
	switch {
	case err == nil:
		if token == *r.ProcStart {
			return orphanLive
		}
		return orphanDead
	case errors.Is(err, proc.ErrNoProcess):
		// The leader (pid == pgid) has exited, but a process group id
		// cannot be reused while any member is alive -- so this alone does
		// not prove the group is empty, only that its own leader is gone.
		// GroupAlive settles it: a live member still in the group (a
		// descendant the leader spawned before exiting) means the group
		// is still the same incarnation and verified live; nothing left
		// means it is dead, reclaimable now rather than waiting for a
		// deadline that already passed the moment the leader exited.
		if proc.GroupAlive(pgid) {
			return orphanLive
		}
		return orphanDead
	default:
		// StartToken failed for a reason other than "no such process" (for
		// example ErrUnsupported, or a transient read failure) even though
		// a token was recorded at start: the plan names no rule for this
		// case. Falling back to the same unverified treatment the
		// no-token case gets, rather than assuming either live or dead, is
		// the smaller risk: a real orphan is never silently reclaimed, and
		// an unrelated group that reused the id is never killed either
		// (implementation report deviation).
		if proc.GroupAlive(pgid) {
			return orphanUnverifiedLive
		}
		return orphanDead
	}
}

// evaluateOrphan decides whether r still counts as live for this
// reclaimForeign pass (design section 6.3), killing its group when it has
// outlived its job's deadline and can be verified, and logging throughout
// so the console's alerts strip shows every step. process names what r
// stands for in those logs: orphanProcessAgent, or orphanProcessCheck for
// a CHECK command presented through store.OpenCheck.AsOpenRun.
func (d *Dispatcher) evaluateOrphan(ticketID int64, r store.OpenRun, process string) bool {
	class := classifyOpenRun(r)
	if class == orphanDead {
		return false
	}

	deadline := time.Now()
	if r.StartedAt != nil {
		deadline = r.StartedAt.Add(d.jobTimeoutOrDefault(r.Job) + claimGrace)
	}
	now := time.Now()
	if now.Before(deadline) {
		slog.Info("waiting for orphaned process of dead serve", "ticket_id", ticketID, "run_id", r.RunID, "pgid", *r.PGID, "process", process)
		return true
	}

	if class == orphanUnverifiedLive {
		slog.Warn("orphaned process unverified past deadline; reclaiming without kill", "ticket_id", ticketID, "run_id", r.RunID, "pgid", *r.PGID, "process", process)
		return false
	}

	if err := proc.KillGroup(*r.PGID); err != nil {
		slog.Error("kill orphaned process failed; retrying next tick", "ticket_id", ticketID, "run_id", r.RunID, "pgid", *r.PGID, "process", process, "err", err)
		return true
	}
	// SIGKILL is delivered, but the group may not have exited yet, so the
	// claim stays held this pass; a later pass reclaims it once the
	// liveness check finds the group gone (design section 6.3).
	slog.Warn("killed orphaned process of dead serve", "ticket_id", ticketID, "run_id", r.RunID, "pgid", *r.PGID, "process", process)
	return true
}

// intake runs step 3: for each binding, ask the tracker for its tickets and
// insert every one the store does not already carry for that (project, ref)
// pair (design section 6.8 step 3). A binding whose Mode is intakeModeManual
// is skipped entirely: manual intake (PKG9-PLAN.md D29) picks up issues only
// through POST /projects/{id}/pickup, never through this automatic poll.
func (d *Dispatcher) intake(ctx context.Context) error {
	for _, b := range d.bindings {
		if b.Mode == intakeModeManual {
			continue
		}
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
			if _, err := InsertAndAnnounce(ctx, d.store, d.tracker, b.StoreProjectID, b.TrackerProject, b.User, tk); err != nil {
				return err
			}
		}
	}
	return nil
}

// InsertAndAnnounce inserts a new queued ticket for tk under
// storeProjectID and posts the pickup comment, exactly once, matching
// intake's own insert step above (design section 6.8 step 3). It is
// exported so POST /projects/{id}/pickup's manual-intake handler
// (internal/console/pickup.go, PKG9-PLAN.md D29) reuses this same step
// instead of duplicating it: "it inserts the ticket exactly as intake does
// ... and posts the same pickup comment." The comment post is best-effort,
// like intake's own: a failure only warns, the inserted row is never rolled
// back, and the post is never retried (the store is the source of truth;
// the tracker is a mirror).
func InsertAndAnnounce(ctx context.Context, st *store.Store, tr tracker.Tracker, storeProjectID int64, trackerProject, user string, tk tracker.Ticket) (int64, error) {
	newID, err := st.InsertTicket(ctx, store.Ticket{
		ProjectID: storeProjectID, TrackerRef: tk.Ref, Title: tk.Title, Body: tk.Body, State: stateQueued,
	})
	if err != nil {
		return 0, fmt.Errorf("dispatch: insert ticket %s: %w", tk.Ref, err)
	}
	// The comment body is never logged (repo rule: never log a secret).
	if cErr := tr.Comment(ctx, trackerProject, tk.Ref, tracker.PickupComment(user)); cErr != nil {
		slog.Warn("pickup comment failed", "ticket_id", newID, "project", trackerProject, "ref", tk.Ref, "err", cErr)
	}
	return newID, nil
}

// claimTimeoutFor returns the claim/run timeout for state (design section
// 6.8 step 6, PKG9-PLAN.md section 17.1): queued takes
// max(defaultCodeTimeout, (1+classify's timeout_retries)*classify's
// timeout_minutes), so the lease covers classify's one automatic timeout
// retry (owner decision Q4; 0 when there is no classify job, leaving just
// the defaultCodeTimeout floor); planning and building each take one job's
// own timeout_minutes (jobTimeoutOrDefault's own defaultCodeTimeout fallback
// when that job is missing or carries no positive timeout_minutes);
// reviewing takes the largest of the review, build, and perimeter job
// timeouts, like shipping, since a review fix unit's build run and CHECK run
// inside it (#55); judging takes the largest of the judge, build, and
// perimeter job timeouts and a 10-minute floor
// (judgingMinClaimTimeout) -- CHECK's own command re-runs and a fix step
// (design section 5.3) can each run inside "judging", so its own claim
// must outlast all three -- never falling back to defaultCodeTimeout even
// when every one of those jobs is misconfigured at 0. Every other
// (code-only) state uses defaultCodeTimeout.
func (d *Dispatcher) claimTimeoutFor(state string) time.Duration {
	switch state {
	case stateQueued:
		// classify runs here, and machine.toml timeout_retries gives it up
		// to one more full attempt, so the lease covers every attempt.
		// Multiplied in int minutes, not as two time.Duration values,
		// so durationcheck does not read this as a units bug.
		classify := d.machine.Jobs[jobClassify]
		attempts := 1 + classify.TimeoutRetries
		return max(defaultCodeTimeout, time.Duration(attempts*classify.TimeoutMinutes)*time.Minute)
	case statePlanning:
		return d.jobTimeoutOrDefault(jobPlanning)
	case stateBuilding:
		return d.jobTimeoutOrDefault(jobBuild)
	case stateReviewing:
		// A review fix unit runs a build run and CHECK inside
		// "reviewing", so its claim must outlast both (#55 plan D9).
		return max(d.jobTimeoutMinutes(jobReview), d.jobTimeoutMinutes(jobBuild), d.jobTimeoutMinutes(jobPerimeter))
	case stateJudging:
		return max(d.jobTimeoutMinutes(jobJudge), d.jobTimeoutMinutes(jobBuild), d.jobTimeoutMinutes(jobPerimeter), judgingMinClaimTimeout)
	case stateShipping:
		return max(d.jobTimeoutMinutes(jobRespond), d.jobTimeoutMinutes(jobBuild), d.jobTimeoutMinutes(jobPerimeter))
	default:
		return defaultCodeTimeout
	}
}

// jobTimeoutOrDefault returns name's own timeout_minutes, or
// defaultCodeTimeout when machine.toml names no such job or gives it no
// positive timeout_minutes (claimTimeoutFor's own single-job states).
func (d *Dispatcher) jobTimeoutOrDefault(name string) time.Duration {
	j, ok := d.machine.Jobs[name]
	if !ok || j.TimeoutMinutes <= 0 {
		return defaultCodeTimeout
	}
	return time.Duration(j.TimeoutMinutes) * time.Minute
}

// jobTimeoutMinutes returns name's own timeout_minutes as a Duration, or 0
// when machine.toml names no such job or gives it no positive
// timeout_minutes: claimTimeoutFor's own judging row folds this into a
// max() alongside judgingMinClaimTimeout, so a misconfigured job
// contributes nothing rather than defaultCodeTimeout's own 5 minutes.
func (d *Dispatcher) jobTimeoutMinutes(name string) time.Duration {
	j, ok := d.machine.Jobs[name]
	if !ok || j.TimeoutMinutes <= 0 {
		return 0
	}
	return time.Duration(j.TimeoutMinutes) * time.Minute
}

// judgingMinClaimTimeout is claimTimeoutFor's own floor for "judging"
// (design section 17.1's table: "max(judge, build, perimeter, 10)").
const judgingMinClaimTimeout = 10 * time.Minute

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

	d.mu.Lock()
	budget := d.tune.Budget
	d.mu.Unlock()

	deps := job.Deps{
		Store: d.store, Runtimes: d.rts, Machine: d.machine,
		Models: d.cfg.Models, Budget: budget, Floor: d.cfg.Floor,
		Owner: d.cfg.Owner, Expires: expires, Now: d.cfg.Now,
		Projects: d.cfg.Projects, Sandboxes: d.cfg.Sandboxes, RequireSandbox: d.cfg.RequireSandbox, Commands: d.cfg.Commands,
		HostCommands: d.cfg.HostCommands,
		DataDir:      d.cfg.DataDir, LensesParallel: d.cfg.LensesParallel, JudgeCodexHome: d.cfg.JudgeCodexHome,
		MergeRule: d.cfg.MergeRule, ReviewBots: d.cfg.ReviewBots,
		// Tracker is the dispatcher itself: PostPRLink and PostDone (below)
		// already give it job.ShipTracker's own two methods, over its own
		// tracker and bindings (PKG9-PLAN.md section 8.6, 17.1).
		Tracker: d,
		// Splitter is the dispatcher too: FileSplitChild and CloseSplitParent
		// (split.go), the same split-variant pair job.SplitTracker names.
		Splitter: d,
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

	// Shutdown interrupt (design section 7.2): ctx is this Dispatcher's own
	// long-lived context (serve's dispCtx), not runCtx, so this is true
	// only when the *dispatcher* was told to stop -- the drain sequence's
	// force-cancel at the drain deadline -- not when runCtx's own deadline
	// merely expired while ctx is still live (that keeps the handler-error
	// path below, exactly as before #45). Whatever the handler returned
	// (runtime.ErrCanceled, a plain context.Canceled, some other wrapped
	// error, or even nil with a commit) is irrelevant here: the run is
	// interrupted either way, any commit is discarded, and InterruptRuns
	// recognizes the running session as cut off mid-turn so the next tick
	// resumes it instead of redriving a runtime that already advanced.
	if ctx.Err() != nil {
		return d.recordShutdownInterrupt(ctx, ticket, expires)
	}

	if err == nil {
		err = job.ValidateCommit(ticket, commit)
	}
	if err != nil {
		// job.Capped (design shape, "Park write"): a Claude session limit
		// hit, or a hold refusal for a lens that reserved a run before
		// another lens in the same round hit the cap. This is checked ahead
		// of runtime.ErrCanceled because a capped error is never that: the
		// run is parked, not left for ExpireClaims to reconcile.
		if capped, ok := job.Capped(err); ok {
			return d.parkCapped(ctx, ticket, expires, capped)
		}
		// runtime.ErrCanceled (design D13, section 4.5, 6.8): runCtx's own
		// deadline expired (ctx itself is still live, or the branch above
		// would already have returned), not a failure to escalate. The
		// claim is left in place -- releasing it would clear
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

		// store.ErrSealRefused (D32, design section 22.12.3a): the seal
		// invariant's own check failed -- no approval, a stale or cancelled
		// one, or an owner message after it. It releases the claim and
		// writes a marker naming the reason, exactly like a mismatch, but
		// never escalates: the next tick re-reads the store, which already
		// reflects whatever made the check fail (a resolved gate question
		// after an owner send, for instance).
		var refused *store.SealRefusedError
		if errors.Is(err, store.ErrSealRefused) && errors.As(err, &refused) {
			return d.releaseAfterSealRefused(ctx, ticket.ID, commit, expires, refused)
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

// recordShutdownInterrupt runs runAndCommit's own shutdown-interrupt path
// (design section 7.2): it terminalizes every open run of ticket's
// sessions as interrupted and clears the claim in one fenced write
// (store.InterruptRuns), under a detached, bounded context so the
// dispatcher's own cancellation cannot abort the write that must still
// land. It never returns a non-nil error: a write failure only logs (the
// claim then simply expires later, and ExpireClaims or a later reclaim
// pass reconciles it the ordinary way), since a shutdown already in
// progress must not itself fail closed.
func (d *Dispatcher) recordShutdownInterrupt(ctx context.Context, ticket store.Ticket, expires time.Time) error {
	postCtx, cancel := postHandlerContext(ctx)
	defer cancel()

	applied, err := d.store.InterruptRuns(postCtx, ticket.ID, d.cfg.Owner, expires)
	if err != nil {
		slog.Error("record shutdown interrupt failed", "ticket_id", ticket.ID, "err", err)
		return nil
	}
	if !applied {
		slog.Warn("claim already lost", "ticket_id", ticket.ID)
		return nil
	}
	d.bus.Publish()
	return nil
}

// parkCapped records a Claude session limit (or a hold refusal) for ticket
// (design shape, "Park write"): store.ParkRuns terminalizes capped.Finish (a
// capped review round's own already-finished lens runs, owner decision Q6)
// by their real outcome, then every run of the ticket still open as
// interrupted with capped_until = capped.Until, raises the claude_hold_until
// setting when that is later, writes one "parked until" marker when any
// run was actually swept, writes one further "discarded review round"
// marker in the same transaction when capped.Round is non-zero (a capped
// review round, job.Capped) and something was actually parked or finished
// -- so a reviewing ticket held only by some other ticket's hold, with
// every lens refused before Reserve and nothing of its own to park, never
// floods the thread with a marker for a round that never ran -- and clears
// the claim, all under a detached, bounded context so a cancelled handler
// context cannot abort a write that must still land. That marker is scoped
// to review's own capped round alone, never for a plain capped run of some
// other job, so reviewingHandler's own cappedRoundNote cannot mistake an
// unrelated job's park for a discarded review round. Like
// recordShutdownInterrupt, it never returns a non-nil error: a write
// failure is logged and the claim is left to expire for ExpireClaims to
// reconcile.
func (d *Dispatcher) parkCapped(ctx context.Context, ticket store.Ticket, expires time.Time, capped job.CappedInfo) error {
	postCtx, cancel := postHandlerContext(ctx)
	defer cancel()

	discardMarker := ""
	if capped.Round != 0 {
		discardMarker = job.CappedRoundDiscardedMarker(capped.Round)
	}
	res, err := d.store.ParkRuns(postCtx, ticket.ID, d.cfg.Owner, expires, capped.Until, discardMarker, capped.Finish...)
	resetAt := capped.Until.UTC().Format(time.RFC3339)
	switch {
	case err != nil:
		slog.Error("claude session limit park failed", "ticket_id", ticket.ID, "reset_at", resetAt, "err", err)
		return nil
	case !res.Applied:
		slog.Warn("claude session limit park skipped: claim already lost", "ticket_id", ticket.ID, "reset_at", resetAt)
		return nil
	case len(res.RunIDs) == 0 && len(capped.Finish) == 0:
		// Nothing was actually parked or finished: every lens (or the lone
		// job) was refused before Reserve, with no run of its own to sweep.
		// This repeats on every tick for a held, unparked ticket until the
		// reset, so it logs at DEBUG rather than flooding WARN.
		slog.Debug("claim released, claude held", "ticket_id", ticket.ID, "reset_at", resetAt)
	default:
		finishedIDs := make([]int64, len(capped.Finish))
		for i, r := range capped.Finish {
			finishedIDs[i] = r.ID
		}
		slog.Warn("claude session limit park", "ticket_id", ticket.ID, "run_ids", res.RunIDs,
			"finished_run_ids", finishedIDs, "reset_at", resetAt, "round", capped.Round)
	}

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

// releaseAfterSealRefused is D32's own dispatcher rule (design section
// 22.12.3a): log the refusal at warn with the gate question id commit's own
// GateApproval named (0 when the commit carried none at all, the "no gate
// approval check" case -- a caller bug, since every real seal-shaped commit
// sets it), write the "seal refused gate <QID>" marker (first line) then
// the reason (second line), and release the claim through the same
// no-escalate path -- the next tick re-reads the store, which already
// reflects whatever made the check fail. The marker is parented to the gate
// question, exactly as releaseAfterSealMismatch's own marker is, only when
// GateApproval named one; with none, it is still written, unparented,
// rather than silently dropped.
func (d *Dispatcher) releaseAfterSealRefused(
	ctx context.Context, ticketID int64, commit store.HandlerCommit, expires time.Time, refused *store.SealRefusedError,
) error {
	var qid int64
	var parentID *int64
	if commit.GateApproval != nil {
		qid = commit.GateApproval.QuestionID
		parentID = &qid
	}
	slog.Warn("seal refused", "ticket_id", ticketID, "question_id", qid, "reason", refused.Reason)

	marker := store.Message{
		TicketID: ticketID, ParentID: parentID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("seal refused gate %d\n%s", qid, refused.Reason),
	}
	return d.releaseClaimNoStop(ctx, ticketID, expires, "claim released after seal refused", marker)
}

// postCommitTrackerEffect runs commit.TrackerEffect, if any, only after
// CommitHandlerResult has already applied cleanly (design D12, section 4.5,
// 6.8): it resolves the binding for ticket.ProjectID the same way intake
// (above) resolves one for its pickup comment, builds the comment body
// e.Kind names, and posts it best-effort -- a failure only warns, since the
// ticket's own state has already committed and must not be undone by a
// tracker-side failure. An unrecognized Kind posts nothing: every kind this
// dispatcher knows is named below, and guessing at an unknown one risks
// posting the wrong comment under the ticket's own name. PUBLISH's PR-link
// comment and DONE's done comment never reach here -- PostPRLink and
// PostDone (below) post those before their own commits (PKG9-PLAN.md
// section 8.2, 8.6, 11).
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

	var body string
	switch e.Kind {
	case store.TrackerEffectKindNothingToDo:
		body = tracker.NothingToDoComment(b.User, e.Notes)
	default:
		slog.Warn("tracker comment skipped", "ticket_id", ticket.ID, "ref", e.Ref, "kind", e.Kind, "err", "unrecognized tracker effect kind")
		return
	}

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

// shipMarkerFmt is the hidden marker PostPRLink and PostDone each search
// for, then post, scoped to one ticket by its own store id (PKG9-PLAN.md
// section 8.2 step 5, 8.6 step 1, 11): "pr" before the draft PR's link
// comment, "done" before the done comment.
const shipMarkerFmt = "<!-- zing:%s t%d -->"

// postMarkedOnce is PostPRLink and PostDone's shared shape (PKG9-PLAN.md
// section 11): resolve the ticket this (projectID, ref) pair names, so the
// hidden marker is scoped to its own store id (neither method is handed the
// ticket id directly), search the issue's comments for that marker through
// CommentContains, which counts only one from the tracker's own
// authenticated login (design 10.5, so a spoofed marker from anyone else
// never suppresses the real post), and post bodyFor's comment, the marker
// appended after a blank line, only when no such comment exists yet. It
// returns the resolved binding so PostDone can reuse it for Close without a
// second lookup.
func (d *Dispatcher) postMarkedOnce(ctx context.Context, projectID int64, ref, kind string, bodyFor func(Binding) string) (Binding, error) {
	b, ok := d.bindingForProject(projectID)
	if !ok {
		return Binding{}, fmt.Errorf("dispatch: no tracker binding for project %d", projectID)
	}
	t, found, err := d.store.TicketByRef(ctx, projectID, ref)
	if err != nil {
		return Binding{}, fmt.Errorf("dispatch: ticket by ref: %w", err)
	}
	if !found {
		return Binding{}, fmt.Errorf("dispatch: no ticket for project %d ref %q", projectID, ref)
	}

	marker := fmt.Sprintf(shipMarkerFmt, kind, t.ID)
	already, err := d.tracker.CommentContains(ctx, b.TrackerProject, ref, marker)
	if err != nil {
		return Binding{}, fmt.Errorf("dispatch: comment contains: %w", err)
	}
	if already {
		return b, nil
	}

	body := bodyFor(b) + "\n\n" + marker
	if err := d.tracker.Comment(ctx, b.TrackerProject, ref, body); err != nil {
		return Binding{}, fmt.Errorf("dispatch: post comment: %w", err)
	}
	return b, nil
}

// Dispatcher satisfies job.ShipTracker through PostPRLink and PostDone
// below, so Deps.Tracker (runAndCommit) can carry *Dispatcher directly.
var _ job.ShipTracker = (*Dispatcher)(nil)

// PostPRLink implements job.ShipTracker's PostPRLink (PKG9-PLAN.md section
// 8.2 step 5, section 11): posts tracker.PRComment at most once per ticket,
// guarded by postMarkedOnce's hidden marker.
func (d *Dispatcher) PostPRLink(ctx context.Context, projectID int64, ref, prURL string) error {
	_, err := d.postMarkedOnce(ctx, projectID, ref, "pr", func(b Binding) string {
		return tracker.PRComment(b.User, prURL)
	})
	return err
}

// PostDone implements job.ShipTracker's PostDone (PKG9-PLAN.md section 8.6
// step 1, section 11): posts tracker.DoneComment the same marked-once way
// PostPRLink posts the PR link, then closes the tracker issue. Closing an
// already-closed issue succeeds (design 10.5), so a crash between the two
// calls, or a retried tick, never fails on the second one -- PostDone always
// calls Close, even when the comment step itself was a skip.
func (d *Dispatcher) PostDone(ctx context.Context, projectID int64, ref, prURL, mergeSHA string) error {
	b, err := d.postMarkedOnce(ctx, projectID, ref, "done", func(b Binding) string {
		return tracker.DoneComment(b.User, prURL, mergeSHA)
	})
	if err != nil {
		return err
	}
	if err := d.tracker.Close(ctx, b.TrackerProject, ref); err != nil {
		return fmt.Errorf("dispatch: close issue: %w", err)
	}
	return nil
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
