// upgrade_test.go is task 9's own test (#109 part 2, Q6): POST /upgrade
// reaches a wired Upgrader through console.WithUpgrader and answers
// "upgrade started", or 503 with no upgrader wired at all.
package console_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/response"
	"zing/internal/store"
)

// fakeUpgrader records every Request call, for handleUpgrade's tests.
type fakeUpgrader struct {
	mu       sync.Mutex
	calls    int
	ticketID int64
	sha      string
}

func (f *fakeUpgrader) Request(ticketID int64, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.ticketID = ticketID
	f.sha = sha
}

func (f *fakeUpgrader) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newUpgradeTestServer builds a console.New handler over opts, copying
// newMutationTestServerWithTunerAndUser's (settings_test.go) shape so this
// file's tests can wire console.WithUpgrader, or leave it off entirely.
func newUpgradeTestServer(t *testing.T, s *store.Store, b *bus.Broker, opts ...console.Option) (srv *httptest.Server) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	port := addr.Port

	handler := console.New(s, b, nil, []string{testBindHost}, port, newTestLogHandler(t), nil, testPushToken,
		response.SeverityMinor, "", nil, "", nil, opts...)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestHandleUpgrade_RequestsUpgrade proves POST /upgrade, with an upgrader
// wired through console.WithUpgrader: a body of {} calls Request(0, "")
// exactly once and answers 202 "upgrade started", and a body carrying an
// unknown field is rejected 400 with no further call.
func TestHandleUpgrade_RequestsUpgrade(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	up := &fakeUpgrader{}
	srv := newUpgradeTestServer(t, s, bus.New(), console.WithUpgrader(up))

	resp := doRequest(t, mutationRequest(t, srv, "/upgrade", `{}`))
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /upgrade status = %d, want 202", resp.StatusCode)
	}
	if !strings.Contains(string(body), "upgrade started") {
		t.Errorf("body = %q, want it to contain %q", body, "upgrade started")
	}
	if got := up.Calls(); got != 1 {
		t.Fatalf("Calls() = %d, want 1", got)
	}
	if up.ticketID != 0 || up.sha != "" {
		t.Errorf("Request called with (%d, %q), want (0, \"\")", up.ticketID, up.sha)
	}

	resp2 := doRequest(t, mutationRequest(t, srv, "/upgrade", `{"x":1}`))
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /upgrade (unknown field) status = %d, want 400", resp2.StatusCode)
	}
	if got := up.Calls(); got != 1 {
		t.Fatalf("Calls() after rejected body = %d, want still 1", got)
	}
}

// TestHandleUpgrade_UnavailableWithoutSelfProject proves POST /upgrade
// answers 503 with no upgrader wired at all (no project in zing.toml sets
// self = true).
func TestHandleUpgrade_UnavailableWithoutSelfProject(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := newUpgradeTestServer(t, s, bus.New())

	resp := doRequest(t, mutationRequest(t, srv, "/upgrade", `{}`))
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST /upgrade status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), "no project in zing.toml sets self = true") {
		t.Errorf("body = %q, want it to contain %q", body, "no project in zing.toml sets self = true")
	}
}
