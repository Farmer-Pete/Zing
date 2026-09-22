package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
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
		Interval:    time.Duration(cfg.Dispatch.IntervalSeconds) * time.Second,
		MaxParallel: cfg.Dispatch.MaxParallel,
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

	addr := net.JoinHostPort(cfg.Console.Bind[0], strconv.Itoa(cfg.Console.Port))
	srv := newServer(ctx, addr, console.New(st, b))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("starting", "addr", addr)

	var serveErr error
	select {
	case serveErr = <-errCh:
	case <-ctx.Done():
	}

	return shutdown(ctx, st, srv, errCh, serveErr, dispDone, func() error { return dispErr }, cancelDisp)
}

// shutdown runs the drain-then-close sequence (design section 6.10 step
// 10) through drainAndShutdown, then folds in the two things that sequence
// does not carry through its channel-of-struct{} and closure shape: the
// dispatcher's own returned error (logged, not returned: an expected
// consequence of a forced shutdown, not a serve failure) and the console
// listener's error from errCh. It returns the first real error among the
// console listener, Shutdown, and the store close.
func shutdown(
	ctx context.Context, st *store.Store, srv *http.Server,
	errCh <-chan error, serveErr error, dispDone <-chan struct{}, dispErr func() error, cancelDisp context.CancelFunc,
) error {
	err := drainAndShutdown(
		ctx, drainDeadline,
		func() error { return st.SetDraining(context.WithoutCancel(ctx), true) },
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
	if de := dispErr(); de != nil && !errors.Is(de, context.Canceled) {
		slog.Error("dispatcher stopped", "err", de)
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
