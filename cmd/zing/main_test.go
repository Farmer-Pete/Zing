package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIndex(t *testing.T) {
	t.Parallel()

	mux := newMux()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if !strings.Contains(rec.Body.String(), "<h1>Zing</h1>") {
		t.Errorf("body missing heading: %q", rec.Body.String())
	}
}

// TestShutdownCancelsRequests proves the drain wiring in newServer: a handler
// blocked on its request context must be released when Shutdown begins.
func TestShutdownCancelsRequests(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-r.Context().Done()
		close(released)
	})

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(ln.Addr().String(), mux)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/stream", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Logf("drain body: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Logf("close body: %v", err)
		}
	})

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("handler was not released by Shutdown")
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned %v, want http.ErrServerClosed", err)
	}
}
