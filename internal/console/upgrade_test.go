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
	"time"

	"zing/internal/bus"
	"zing/internal/console"
	zdispatch "zing/internal/dispatch"
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

// TestSettings_UpgradeButton proves the Settings view's Upgrade now row
// (task 10, Q6): it appears, with its button and alert message span, only
// when an upgrader is wired through console.WithUpgrader.
func TestSettings_UpgradeButton(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	up := &fakeUpgrader{}
	srv := newUpgradeTestServer(t, s, bus.New(), console.WithUpgrader(up))

	resp, r, cancel := openStream(t, srv.URL, "settings", 0, 0)
	defer cancel()
	defer resp.Body.Close()
	_, main, _, _ := readInitialFrames(t, r)
	if !strings.Contains(main, `class="upgrade-now"`) {
		t.Errorf("main missing upgrade-now button; got:\n%s", main)
	}
	if !strings.Contains(main, `id="upgrade-message" role="alert" data-ignore-morph`) {
		t.Errorf("main missing upgrade-message alert span; got:\n%s", main)
	}

	srvNoUpgrader := newUpgradeTestServer(t, newConsoleTestStore(t), bus.New())
	resp2, r2, cancel2 := openStream(t, srvNoUpgrader.URL, "settings", 0, 0)
	defer cancel2()
	defer resp2.Body.Close()
	_, main2, _, _ := readInitialFrames(t, r2)
	if strings.Contains(main2, "upgrade-now") || strings.Contains(main2, "upgrade-message") {
		t.Errorf("main (no upgrader) should not show the Upgrade now row; got:\n%s", main2)
	}

	// A live serve always wires a tuner (views.go's c.tuner != nil branch);
	// prove the Upgrade now row renders there too, alongside a tuning row.
	s3 := newConsoleTestStore(t)
	d := settingsTestDispatcher(t, s3, bus.New(), zdispatch.Config{MaxParallel: 1, Interval: time.Hour}, &settingsBudgetHandler{})
	up3 := &fakeUpgrader{}
	srv3 := newUpgradeTestServer(t, s3, bus.New(), console.WithTuner(d), console.WithUpgrader(up3))
	resp3, r3, cancel3 := openStream(t, srv3.URL, "settings", 0, 0)
	defer cancel3()
	defer resp3.Body.Close()
	_, main3, _, _ := readInitialFrames(t, r3)
	if !strings.Contains(main3, `data-tuning-name="max_parallel"`) {
		t.Errorf("main (with tuner) missing a tuning row; got:\n%s", main3)
	}
	if !strings.Contains(main3, `class="upgrade-now"`) {
		t.Errorf("main (with tuner) missing upgrade-now button; got:\n%s", main3)
	}
}
