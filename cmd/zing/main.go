// Command zing starts the Zing web server.
package main

import (
	"context"
	"errors"
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
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
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
// Shutdown closes listeners and waits for handlers, but net/http never cancels
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
