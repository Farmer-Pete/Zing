package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

const (
	argv0           = "zing"
	cmdServe        = "serve"
	cmdSelftest     = "selftest"
	cmdUnrecognized = "bogus"
)

func TestCommandName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no argument defaults to serve", []string{argv0}, cmdServe},
		{"explicit serve", []string{argv0, cmdServe}, cmdServe},
		{"selftest", []string{argv0, cmdSelftest}, cmdSelftest},
		{"unrecognized command passes through", []string{argv0, cmdUnrecognized}, cmdUnrecognized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := commandName(tt.args); got != tt.want {
				t.Errorf("commandName(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestDispatch_Selftest(t *testing.T) {
	t.Parallel()

	if got := dispatch([]string{argv0, cmdSelftest}); got != 0 {
		t.Errorf("dispatch(selftest) = %d, want 0", got)
	}
}

// TestDispatch_UnknownCommand cannot run in parallel: it swaps the process
// os.Stderr to capture dispatch's error message.
func TestDispatch_UnknownCommand(t *testing.T) {
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	got := dispatch([]string{argv0, cmdUnrecognized})

	if closeErr := w.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if got != 2 {
		t.Errorf("dispatch(bogus) = %d, want 2", got)
	}
	want := "zing: unknown command \"bogus\"\n"
	if string(out) != want {
		t.Errorf("stderr = %q, want %q", out, want)
	}
}
