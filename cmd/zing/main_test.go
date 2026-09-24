package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	srv := newServer(t.Context(), mux)
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
	cmdValidate     = "validate"
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

// TestDispatch_Validate proves dispatch routes "validate" to runValidate
// with the arguments after the subcommand name, on top of validate_test.go's
// own direct coverage of runValidate's behavior.
func TestDispatch_Validate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "doc.xml")
	doc := `<zing job="classify" outcome="bug"><reason>it crashes</reason></zing>`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := dispatch([]string{argv0, cmdValidate, path}); got != 0 {
		t.Errorf("dispatch(validate, %s) = %d, want 0", path, got)
	}
}

// TestDispatch_UnknownCommand cannot run in parallel: it swaps the process
// os.Stderr to capture dispatch's error message.
// TestDispatch_BareInvocationDoesNotPanic proves the args[2:] slice-bounds
// bug is fixed: commandName defaults a bare `zing` invocation (len(args) ==
// 1) to "serve", so dispatch must not slice args[2:] unconditionally -- that
// panicked with "slice bounds out of range" since 2 > len(args). HOME is
// redirected to an empty temp dir so config.DefaultPath's ~/.zing/zing.toml
// is guaranteed absent; serve then fails fast on the missing config (exit 1)
// rather than opening any real database, so this test never touches
// ~/.zing/zing.db.
func TestDispatch_BareInvocationDoesNotPanic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got := dispatch([]string{argv0})
	if got != 1 {
		t.Errorf("dispatch(bare) = %d, want 1 (serve reached and failed fast on the missing config, not a panic)", got)
	}
}

// TestDispatch_ValidateWithNoExtraArgs covers "zing validate" with nothing
// after it, the other len(args) == 2 edge subArgs must also get right:
// args[2:] on a two-element slice is empty, not out of range, but this locks
// the behavior in alongside the bare-invocation fix so a future refactor of
// subArgs cannot regress it silently. runValidate(nil) prints its usage line
// and returns 2.
func TestDispatch_ValidateWithNoExtraArgs(t *testing.T) {
	t.Parallel()

	if got := dispatch([]string{argv0, cmdValidate}); got != 2 {
		t.Errorf("dispatch(validate, no args) = %d, want 2 (usage error)", got)
	}
}

// TestDispatch_Project proves dispatch routes "project" to runProject with
// the arguments after the subcommand name, mirroring TestDispatch_Validate.
// project_test.go already covers runProject/projectAdd's own behavior in
// depth, so "zing project" with no further arguments is enough here to
// exercise dispatch's own routing: runProject's usage-error path returns 2.
func TestDispatch_Project(t *testing.T) {
	t.Parallel()

	if got := dispatch([]string{argv0, "project"}); got != 2 {
		t.Errorf("dispatch(project, no args) = %d, want 2 (usage error)", got)
	}
}

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
