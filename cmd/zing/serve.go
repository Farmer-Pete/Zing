package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/config"
	"zing/internal/console"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/runtime"
	"zing/internal/store"
	"zing/internal/tracker"
)

// drainDeadline bounds how long serve waits for the dispatcher's Run
// goroutine to finish its current tick before force-cancelling it (design
// section 6.10). A tick that is still running past this deadline gets its
// context cancelled so the store is never closed under a live handler.
const drainDeadline = 30 * time.Second

// shutdownTimeout bounds srv.Shutdown, which by then only has to close idle
// connections and let SSE handlers unwind, since RegisterOnShutdown already
// cancelled their base context.
const shutdownTimeout = 10 * time.Second

// defaultDispatchInterval and defaultDispatchMaxParallel are the documented
// defaults for cfg.Dispatch.IntervalSeconds and cfg.Dispatch.MaxParallel
// (internal/config's applyDefaults uses the same two values). serve clamps
// to these here, rather than in internal/config, because applyDefaults only
// fires when a key is absent from zing.toml: an explicit non-positive value
// (interval_seconds = 0, max_parallel = -1) survives config.Load untouched
// and would otherwise panic time.Ticker (interval) or stall all processing
// (max_parallel, since Tick never claims once active >= max_parallel).
const (
	defaultDispatchInterval    = 30 * time.Second
	defaultDispatchMaxParallel = 2
)

// run wires the default paths and the signal-derived base context, then
// hands off to serve. It is the "serve" subcommand's entry point.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	dbPath, err := store.DefaultPath()
	if err != nil {
		return err
	}

	return serve(ctx, cfgPath, dbPath)
}

// serve starts the store, the dispatcher, and the console, and runs until
// ctx is cancelled (or the console listener fails), draining the dispatcher
// before it closes the store (design section 6.10).
func serve(ctx context.Context, cfgPath, dbPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	bindAddr, err := consoleBindAddr(cfg.Console)
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}

	logHandler, err := installLogHandler(ctx, st)
	if err != nil {
		_ = st.Close()
		return err
	}

	// Clear the persisted control flags a prior graceful stop may have left
	// set. Without this, the "draining" flag survives across a restart: the
	// HTTP listener below starts normally, but the dispatcher goroutine's
	// first Tick (and Run, right after it) sees draining still true and
	// exits immediately, so nothing is ever dispatched even though the
	// console comes up and serves normally (recovery-by-restart is the
	// intended path here per the plan's Q-runtime note, so "stopped" is
	// cleared too).
	if err = st.SetDraining(ctx, false); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: clear draining flag: %w", err)
	}
	if err = st.SetStopped(ctx, false); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: clear stopped flag: %w", err)
	}

	bindings, err := ensureBindings(ctx, st, cfg.Projects)
	if err != nil {
		_ = st.Close()
		return err
	}

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		_ = st.Close()
		return err
	}

	scripts, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: sub scripts fs: %w", err)
	}
	rt := runtime.NewFake(scripts)

	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		_ = st.Close()
		return err
	}

	b := bus.New()

	// dispCtx is deliberately not derived from ctx's cancellation: the
	// drain sequence below stops the dispatcher through the store's
	// draining flag first, and only cancels dispCtx as the timeout
	// backstop, so a signal must not cancel it on its own.
	dispCtx, cancelDisp := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDisp()

	d, err := zdispatch.New(st, tr, b, m, job.Registry(), bindings, zdispatch.Config{
		Interval:    dispatchInterval(cfg.Dispatch.IntervalSeconds),
		MaxParallel: dispatchMaxParallel(cfg.Dispatch.MaxParallel),
		Owner:       claimOwner(),
	}, rt)
	if err != nil {
		_ = st.Close()
		return err
	}

	// dispDone signals (by closing) once d.Run's goroutine has returned;
	// dispErr holds its return value, safe to read once dispDone has closed,
	// because the close happens-after the assignment in the same goroutine
	// and happens-before any receive of it (design section 6.10).
	dispDone := make(chan struct{})
	var dispErr error
	go func() {
		dispErr = d.Run(dispCtx)
		close(dispDone)
	}()

	srv := newServer(ctx, bindAddr, console.New(st, b, m, cfg.Console.Bind[0], cfg.Console.Port, logHandler))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("starting", "addr", bindAddr)

	// dispTriggered records whether the dispatcher's own goroutine is what
	// ended this select, as opposed to a normal signal (ctx.Done()) or an
	// HTTP listener failure (errCh). Only in that case does the dispatcher's
	// captured error become serve's return value below: on the other two
	// paths dispDone will also close during the drain that shutdown runs
	// next, but that is the expected, graceful join, not a failure to
	// report.
	var serveErr error
	var dispTriggered bool
	select {
	case serveErr = <-errCh:
	case <-ctx.Done():
	case <-dispDone:
		dispTriggered = true
	}

	return shutdown(ctx, st, srv, d, errCh, serveErr, dispTriggered, dispDone, func() error { return dispErr }, cancelDisp)
}

// shutdown runs the drain-then-close sequence (design section 6.10 step
// 10) through drainAndShutdown, then folds in the things that sequence does
// not carry through its channel-of-struct{} and closure shape: the
// dispatcher's own returned error and the console listener's error from
// errCh. It returns the first real error among: the dispatcher (only when
// dispTriggered, i.e. the dispatcher's own goroutine, not a signal or an
// HTTP failure, is what ended serve's select), the console listener,
// Shutdown, and the store close. d is used only to wake Run promptly once
// draining is set (d.NotifyDrain, called from the setDraining closure
// below); drainAndShutdown itself stays decoupled from
// *dispatch.Dispatcher; so does shutdown_test.go, which drives it directly.
func shutdown(
	ctx context.Context, st *store.Store, srv *http.Server, d *zdispatch.Dispatcher,
	errCh <-chan error, serveErr error, dispTriggered bool, dispDone <-chan struct{}, dispErr func() error, cancelDisp context.CancelFunc,
) error {
	err := drainAndShutdown(
		ctx, drainDeadline,
		func() error {
			setErr := st.SetDraining(context.WithoutCancel(ctx), true)
			if setErr == nil {
				// Wake Run promptly rather than leaving it to notice only
				// on the next ticker fire, which can race a short drain
				// deadline (design section 6.10; dispatch.Dispatcher's own
				// NotifyDrain doc comment).
				d.NotifyDrain()
			}
			return setErr
		},
		dispDone,
		cancelDisp,
		func(parent context.Context) error {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), shutdownTimeout)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		},
		st.Close,
	)

	// dispDone is guaranteed closed by the time drainAndShutdown returns, so
	// dispErr() is safe to read here.
	de := dispErr()
	if de != nil && !errors.Is(de, context.Canceled) {
		slog.Error("dispatcher stopped", "err", de)
	}

	if serveErr == nil {
		serveErr = dispatchFailure(dispTriggered, de)
	}
	if serveErr == nil {
		serveErr = err
	}
	if serveErr == nil {
		if e := <-errCh; e != nil && !errors.Is(e, http.ErrServerClosed) {
			serveErr = e
		}
	}
	return serveErr
}

// dispatchFailure decides whether the dispatcher's own captured error (de)
// should become serve's return value. It fires only when dispTriggered is
// true, meaning the dispatcher's goroutine, rather than a signal or an HTTP
// listener failure, is what ended serve's main select: today a dispatcher
// error (for example dispatch.ErrFailClosed) stops all intake while HTTP
// keeps serving, and serve never reports it. A nil error, or
// context.Canceled (the expected result of the drain sequence's own forced
// cancel), never counts as a failure.
func dispatchFailure(dispTriggered bool, de error) error {
	if !dispTriggered || de == nil || errors.Is(de, context.Canceled) {
		return nil
	}
	return fmt.Errorf("dispatcher: %w", de)
}

// drainAndShutdown runs the section 6.10 drain-then-close sequence, decoupled
// from *store.Store and *http.Server so it can be driven directly in a test:
// it marks the system draining, waits for the dispatcher to signal dispDone,
// bounded by drainDeadline; if dispDone has not closed by then, it logs
// "drain timed out", calls forceDisp to cancel the dispatcher's own context,
// and waits again without a bound, because the store must never close while
// a handler may still be running. Only once dispDone has closed does it call
// shutdown and, last, closeStore. It returns the first error from shutdown or
// closeStore; a setDraining error is logged, not returned, since drain and
// join must proceed regardless (design section 6.10).
func drainAndShutdown(
	ctx context.Context,
	drainDeadline time.Duration,
	setDraining func() error,
	dispDone <-chan struct{},
	forceDisp context.CancelFunc,
	shutdown func(context.Context) error,
	closeStore func() error,
) error {
	if err := setDraining(); err != nil {
		slog.Error("set draining", "err", err)
	}

	select {
	case <-dispDone:
	case <-time.After(drainDeadline):
		slog.Error("drain timed out")
		forceDisp()
		<-dispDone
	}

	var err error
	if shutErr := shutdown(ctx); shutErr != nil {
		err = shutErr
	}
	if closeErr := closeStore(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

// installLogHandler builds the Task 5 slog.Handler (internal/console/log.go,
// design section 6.12), seeds its LevelVar from settings.log_level, and
// installs it as slog's process-wide default, so every slog call from here
// on -- this package's own and every other package's -- goes through the
// one handler console.New's Task 10 log argument wires into POST /loglevel,
// POST /debug, and the rail's Log tail. Writing to os.Stderr, the same sink
// slog's own factory default uses, preserves this process's existing log
// output shape; only the level gate, the per-ticket debug override, and the
// ring are new. A missing or unrecognized stored level (a hand-edited
// settings row, or a fresh database before migrations seed it -- store.Open
// always runs them first, so this is defensive, not an expected path)
// defaults to info and is logged once, rather than failing serve over a bad
// setting.
func installLogHandler(ctx context.Context, st *store.Store) (*console.Handler, error) {
	lv := new(slog.LevelVar)
	h := console.NewHandler(os.Stderr, lv)
	slog.SetDefault(slog.New(h))

	stored, ok, err := st.GetSetting(ctx, "log_level")
	if err != nil {
		return nil, fmt.Errorf("serve: get log_level setting: %w", err)
	}
	level, known := console.ParseLogLevel(stored)
	if !ok || !known {
		slog.Warn("settings.log_level missing or unrecognized, defaulting to info", "stored", stored)
		level = slog.LevelInfo
	}
	lv.Set(level)

	return h, nil
}

// consoleBindAddr validates cfg.Console.Bind and returns the address to
// listen on: host from the first entry, joined with cfg.Console.Port. An
// empty Bind list would otherwise panic net.JoinHostPort(cfg.Console.Bind[0],
// ...) below, so that case is rejected here instead. This skeleton binds one
// loopback address; a configured Bind longer than one entry (for example the
// tailnet address a later package adds) is not an error, but only its first
// entry is used, so the rest are logged rather than silently ignored.
func consoleBindAddr(cfg config.Console) (string, error) {
	if len(cfg.Bind) == 0 {
		return "", errors.New("zing.toml: console.bind: must have at least one address")
	}
	if len(cfg.Bind) > 1 {
		slog.Warn("console.bind has more than one address; only the first is bound, the rest are deferred to a later package",
			"bind", cfg.Bind, "using", cfg.Bind[0])
	}
	return net.JoinHostPort(cfg.Bind[0], strconv.Itoa(cfg.Port)), nil
}

// maxDispatchIntervalSeconds is the largest interval_seconds value that
// time.Duration(seconds) * time.Second cannot overflow an int64 nanosecond
// count (cubic P1): dispatchInterval passes its result straight to
// time.NewTicker inside the dispatcher's goroutine, so a value above this
// would wrap around to a bogus (often negative) duration and panic it.
const maxDispatchIntervalSeconds = math.MaxInt64 / int64(time.Second)

// dispatchInterval returns the dispatcher's tick interval for a configured
// dispatch.interval_seconds, clamping a non-positive value (zero or
// negative, whether from an explicit zing.toml entry or an unset field) and
// a value large enough to overflow a time.Duration
// (maxDispatchIntervalSeconds) to defaultDispatchInterval. Passed straight
// through to time.Ticker, either an out-of-range value would otherwise
// panic it.
func dispatchInterval(seconds int) time.Duration {
	switch {
	case seconds <= 0:
		slog.Warn("dispatch.interval_seconds is not positive, using the default",
			"interval_seconds", seconds, "default_seconds", int(defaultDispatchInterval.Seconds()))
		return defaultDispatchInterval
	case int64(seconds) > maxDispatchIntervalSeconds:
		slog.Warn("dispatch.interval_seconds overflows a time.Duration, using the default",
			"interval_seconds", seconds, "default_seconds", int(defaultDispatchInterval.Seconds()))
		return defaultDispatchInterval
	default:
		return time.Duration(seconds) * time.Second
	}
}

// dispatchMaxParallel returns the dispatcher's max-parallel guard for a
// configured dispatch.max_parallel, clamping a non-positive value to
// defaultDispatchMaxParallel. Tick's max-parallel guard (design section 6.8
// step 4) is "active >= max_parallel"; a non-positive value would make that
// guard true before any ticket is ever claimed, stalling all processing.
func dispatchMaxParallel(n int) int {
	if n <= 0 {
		slog.Warn("dispatch.max_parallel is not positive, using the default",
			"max_parallel", n, "default", defaultDispatchMaxParallel)
		return defaultDispatchMaxParallel
	}
	return n
}

// ensureBindings ensures a store project for every configured project and
// returns the dispatch.Binding each one needs for intake (design section
// 6.10 step 3).
func ensureBindings(ctx context.Context, st *store.Store, projects []config.Project) ([]zdispatch.Binding, error) {
	bindings := make([]zdispatch.Binding, 0, len(projects))
	for _, p := range projects {
		id, err := st.EnsureProject(ctx, store.Project{
			Name:      p.Name,
			RepoURL:   p.Repo,
			LocalPath: p.Path,
			Tracker:   p.Tracker,
		})
		if err != nil {
			return nil, fmt.Errorf("ensure project %s: %w", p.Name, err)
		}
		bindings = append(bindings, zdispatch.Binding{
			StoreProjectID: id,
			TrackerProject: p.Name,
			Rule:           tracker.IntakeRule{Assignee: p.Intake.AssignedTo},
		})
	}
	return bindings, nil
}

// claimOwner returns this process's claim owner id, <hostname>-<pid>
// (design section 7.2).
func claimOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// newServer builds the HTTP server with its timeouts and graceful-drain
// wiring. drainCtx carries ctx's values (none, today) but never its
// cancellation, so a caller's ctx (the signal-derived base context) cannot
// tear down in-flight requests out from under Shutdown; only Shutdown's own
// RegisterOnShutdown callback cancels it.
//
// Shutdown closes listeners and waits for handlers, but it does not cancel
// request contexts on its own. Every request context here derives from a
// drain context that is cancelled when Shutdown begins, so long-lived SSE
// handlers that select on r.Context().Done() exit instead of holding
// Shutdown until its deadline.
//
// WriteTimeout stays unset on purpose: SSE handlers hold the connection
// open. Non-streaming routes should bound writes with http.ResponseController
// instead.
func newServer(ctx context.Context, addr string, handler http.Handler) *http.Server {
	drainCtx, drain := context.WithCancel(context.WithoutCancel(ctx))
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return drainCtx },
	}
	srv.RegisterOnShutdown(drain)
	return srv
}
