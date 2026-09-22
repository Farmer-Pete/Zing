// Command zing starts the Zing web server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// page is the placeholder index until templates and the Datastar bundle land.
const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Zing</title>
</head>
<body>
  <h1>Zing</h1>
</body>
</html>
`

func main() {
	os.Exit(dispatch(os.Args))
}

// commandName returns the subcommand named in os.Args-shaped args, defaulting
// to "serve" when none is given.
func commandName(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return "serve"
}

// dispatch runs the subcommand named in os.Args-shaped args and returns the
// process exit code.
func dispatch(args []string) int {
	switch cmd := commandName(args); cmd {
	case "selftest":
		return runSelftest()
	case "serve":
		if err := run(); err != nil {
			slog.Error("server stopped", "err", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "zing: unknown command %q\n", cmd)
		return 2
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ZING_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := newServer(addr, newMux())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("starting", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", index)
	return mux
}

// newServer builds the HTTP server with its timeouts and graceful-drain wiring.
//
// Shutdown closes listeners and waits for handlers, but it does not cancel
// request contexts on its own. Every request context here derives from a drain
// context that is cancelled when Shutdown begins, so long-lived SSE handlers that
// select on r.Context().Done() exit instead of holding Shutdown until its deadline.
//
// WriteTimeout stays unset on purpose: SSE handlers hold the connection open.
// Non-streaming routes should bound writes with http.ResponseController instead.
func newServer(addr string, handler http.Handler) *http.Server {
	drainCtx, drain := context.WithCancel(context.Background())
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

func index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write([]byte(page)); err != nil {
		slog.Error("write index", "err", err)
	}
}
