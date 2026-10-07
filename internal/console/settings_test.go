// settings_test.go is task 2's own test (#81): POST /settings reaches a
// real *dispatch.Dispatcher through console.WithTuner, so a live
// max_parallel or agent-budget change takes hold on the dispatcher's very
// next pass, with no restart, and every bound and nil-tuner case answers
// the owner's chosen status (owner decisions Q1, Q2, Q3).
package console_test

import (
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/console"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/response"
	zruntime "zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
	"zing/internal/tracker"
)

// settingsTestOwner is the claim owner every dispatcher built in this file
// uses (goconst): never read back by anything a test asserts on.
const settingsTestOwner = "settings-test-owner"

// settingsBarrierHandler overrides the queued state's handler (job.Registry
// otherwise registers classify there): it signals its own arrival on
// started, then blocks on release, so a test can observe exactly how many
// tickets a fill pass launched concurrently, the same technique
// internal/dispatch/dispatch_test.go's own barrierHandler uses, copied here
// since that type is private to package dispatch_test.
type settingsBarrierHandler struct {
	mu      sync.Mutex
	entries int
	release chan struct{}
}

func (h *settingsBarrierHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	h.mu.Lock()
	h.entries++
	h.mu.Unlock()
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return store.HandlerCommit{}, ctx.Err()
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}, nil
}

func (h *settingsBarrierHandler) Entries() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entries
}

// settingsBudgetHandler overrides the queued state's handler and records
// every job.Deps.Budget it is called with, then returns a fenced no-op
// commit (no Next), so a test can prove a live SetTuning budget change
// reaches the very next job.Deps the dispatcher builds.
type settingsBudgetHandler struct {
	mu      sync.Mutex
	budgets []time.Duration
}

func (h *settingsBudgetHandler) Run(_ context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	h.mu.Lock()
	h.budgets = append(h.budgets, d.Budget)
	h.mu.Unlock()
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}, nil
}

func (h *settingsBudgetHandler) Budgets() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.budgets...)
}

// settingsTestDispatcher builds a real *dispatch.Dispatcher over a fresh
// store and the real machine.toml, with reg's handler substituted for the
// queued state (job.Registry() otherwise registers classify there), so a
// test can drive fill/Tick without exercising the real pipeline at all.
func settingsTestDispatcher(t *testing.T, s *store.Store, b *bus.Broker, cfg zdispatch.Config, queuedHandler job.Handler) *zdispatch.Dispatcher {
	t.Helper()

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	scriptsFS, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	rt := zruntime.NewFake(scriptsFS)
	rts, err := zruntime.NewSet(map[string]zruntime.Runtime{"claude": rt, "codex": rt, testRuntimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = queuedHandler

	if cfg.Owner == "" {
		cfg.Owner = settingsTestOwner
	}
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	cfg.Sandboxes = sandbox.OffSet()

	d, err := zdispatch.New(s, tr, b, m, reg, nil, cfg, rts)
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}
	return d
}

// settingsSeedQueuedTicket inserts one queued ticket under its own fresh
// project, so distinct tickets never collide on a tracker ref.
func settingsSeedQueuedTicket(t *testing.T, s *store.Store, ref string) int64 {
	t.Helper()
	ctx := t.Context()
	projectID, err := s.EnsureProject(ctx, store.Project{
		Name: ref, RepoURL: "https://example.invalid/" + ref, LocalPath: t.TempDir(), Tracker: testTrackerGitHub,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: "a ticket", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return id
}

// newMutationTestServerWithTuner copies newMutationTestServer (mw_test.go)
// and adds console.WithTuner(d), so POST /settings and the Settings view
// reach a real dispatcher.
func newMutationTestServerWithTuner(t *testing.T, s *store.Store, b *bus.Broker, d *zdispatch.Dispatcher) (srv *httptest.Server) {
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
		response.SeverityMinor, "", nil, "", nil, console.WithTuner(d))
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// waitForEntries polls (bounded by timeout, not a fixed sleep) until h has
// recorded at least want entries.
func waitForEntries(t *testing.T, h *settingsBarrierHandler, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if h.Entries() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("entries = %d after %s, want at least %d", h.Entries(), timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSettingsRoute_MaxParallelFillsNewSlots proves a POST /settings call
// that raises max_parallel is visible to the dispatcher's very next Tick,
// with no restart (#81): four queued tickets, max_parallel starting at 1,
// raised to 3 through the real console route before Tick ever runs, and
// Tick launches exactly 3 of the 4 tickets concurrently.
func TestSettingsRoute_MaxParallelFillsNewSlots(t *testing.T) {
	t.Parallel()

	s := newConsoleTestStore(t)
	refs := []string{"fake#1", "fake#2", "fake#3", "fake#4"}
	ticketIDs := make([]int64, 0, len(refs))
	for _, ref := range refs {
		ticketIDs = append(ticketIDs, settingsSeedQueuedTicket(t, s, ref))
	}

	h := &settingsBarrierHandler{release: make(chan struct{})}
	b := bus.New()
	d := settingsTestDispatcher(t, s, b, zdispatch.Config{MaxParallel: 1, Interval: time.Hour}, h)

	srv := newMutationTestServerWithTuner(t, s, b, d)

	req := mutationRequest(t, srv, "/settings", `{"name":"max_parallel","value":3}`)
	resp := doRequest(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /settings max_parallel=3 status = %d, want 204", resp.StatusCode)
	}

	tickErrCh := make(chan error, 1)
	go func() { tickErrCh <- d.Tick(t.Context()) }()

	waitForEntries(t, h, 3, 5*time.Second)
	time.Sleep(200 * time.Millisecond)
	if got := h.Entries(); got != 3 {
		t.Fatalf("entries = %d, want exactly 3 (the new max_parallel)", got)
	}

	// The ticket fill never reached is still queued and unclaimed.
	var untouched int64
	for _, id := range ticketIDs {
		ticket, err := s.GetTicket(t.Context(), id)
		if err != nil {
			t.Fatalf("GetTicket(%d): %v", id, err)
		}
		if ticket.ClaimOwner == nil && ticket.State == testStateQueued {
			// This ticket may or may not be one of the 3 launched (the
			// claim happens before the handler blocks, so a launched
			// ticket's ClaimOwner is non-nil). Only an unclaimed one
			// proves it was never picked.
			untouched = id
		}
	}
	if untouched == 0 {
		t.Fatal("no untouched queued ticket found; want exactly one of the 4 left unclaimed")
	}
	ticket, err := s.GetTicket(t.Context(), untouched)
	if err != nil {
		t.Fatalf("GetTicket(%d): %v", untouched, err)
	}
	if ticket.ClaimOwner != nil {
		t.Errorf("untouched ticket ClaimOwner = %v, want nil", *ticket.ClaimOwner)
	}
	if ticket.State != testStateQueued {
		t.Errorf("untouched ticket State = %q, want %q", ticket.State, testStateQueued)
	}

	close(h.release)
	if err := <-tickErrCh; err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// TestSettingsRoute_BudgetReachesNextDeps proves a POST /settings call that
// changes the agent budget reaches the very next job.Deps the dispatcher
// builds (#81), with no restart.
func TestSettingsRoute_BudgetReachesNextDeps(t *testing.T) {
	t.Parallel()

	s := newConsoleTestStore(t)
	settingsSeedQueuedTicket(t, s, "fake#1")

	h := &settingsBudgetHandler{}
	b := bus.New()
	d := settingsTestDispatcher(t, s, b, zdispatch.Config{MaxParallel: 1, Interval: time.Hour, Budget: 240 * time.Minute}, h)

	srv := newMutationTestServerWithTuner(t, s, b, d)

	req := mutationRequest(t, srv, "/settings", `{"name":"agent_minutes_per_ticket","value":480}`)
	resp := doRequest(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /settings agent_minutes_per_ticket=480 status = %d, want 204", resp.StatusCode)
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	budgets := h.Budgets()
	if len(budgets) != 1 {
		t.Fatalf("budgets recorded = %d, want exactly 1", len(budgets))
	}
	if want := 480 * time.Minute; budgets[0] != want {
		t.Errorf("Deps.Budget = %v, want %v", budgets[0], want)
	}
}

// TestSettingsRoute_RejectsOutOfRange proves every out-of-bounds or unknown
// POST /settings request answers 400 with the owner-facing message
// (owner decision Q2), writes nothing to the settings table, and leaves the
// dispatcher's live Tuning exactly as it started (#81): a refused POST must
// never partially apply. An extra field in the body also answers 400.
func TestSettingsRoute_RejectsOutOfRange(t *testing.T) {
	t.Parallel()

	s := newConsoleTestStore(t)
	b := bus.New()
	d := settingsTestDispatcher(t, s, b, zdispatch.Config{MaxParallel: 1, Interval: time.Hour}, &settingsBudgetHandler{})
	srv := newMutationTestServerWithTuner(t, s, b, d)
	startTuning := d.CurrentTuning()

	cases := []struct {
		name, body, wantMsg string
		keys                []string
	}{
		{
			name: "max_parallel too high", body: `{"name":"max_parallel","value":65}`,
			wantMsg: "max_parallel must be 1 to 64", keys: []string{"dispatch.max_parallel"},
		},
		{
			name: "interval_seconds too low", body: `{"name":"interval_seconds","value":0}`,
			wantMsg: "interval_seconds must be 1 to 86400", keys: []string{"dispatch.interval_seconds"},
		},
		{
			name: "agent_minutes_per_ticket too high", body: `{"name":"agent_minutes_per_ticket","value":525601}`,
			wantMsg: "agent_minutes_per_ticket must be 1 to 525600", keys: []string{"budget.agent_minutes_per_ticket"},
		},
		{
			name: "unknown name", body: `{"name":"foo","value":1}`,
			wantMsg: `unknown setting "foo"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := mutationRequest(t, srv, "/settings", c.body)
			resp := doRequest(t, req)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			got := readBody(t, resp)
			if strings.TrimSpace(got) != c.wantMsg {
				t.Errorf("body = %q, want %q", strings.TrimSpace(got), c.wantMsg)
			}
			for _, key := range c.keys {
				_, ok, err := s.GetSetting(t.Context(), key)
				if err != nil {
					t.Fatalf("GetSetting(%q): %v", key, err)
				}
				if ok {
					t.Errorf("GetSetting(%q): ok = true, want false (a refused POST must write nothing)", key)
				}
			}
			if got := d.CurrentTuning(); got != startTuning {
				t.Errorf("CurrentTuning() = %+v after a refused POST, want unchanged %+v", got, startTuning)
			}
		})
	}

	extraFieldReq := mutationRequest(t, srv, "/settings", `{"name":"max_parallel","value":2,"extra":1}`)
	extraFieldResp := doRequest(t, extraFieldReq)
	defer extraFieldResp.Body.Close()
	if extraFieldResp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an extra field", extraFieldResp.StatusCode)
	}
}

// TestSettingsRoute_NoTunerIs503 proves a console built with no
// console.WithTuner answers POST /settings 503, rather than panicking on a
// nil dispatcher (#81).
func TestSettingsRoute_NoTunerIs503(t *testing.T) {
	t.Parallel()

	s := newConsoleTestStore(t)
	srv, _ := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))

	req := mutationRequest(t, srv, "/settings", `{"name":"max_parallel","value":2}`)
	resp := doRequest(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	got := strings.TrimSpace(readBody(t, resp))
	const want = "dispatch settings are not available"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestSettingsView_ShowsLiveValuesAndSource proves the Settings view (task
// 5, owner decision Q1, Q4): the #main region for view=settings shows every
// tuning row with its live value, a source line, and a per-row message
// span carrying role alert and data-ignore-morph -- and, with no tuner
// wired, the "needs a running zing serve" line instead.
func TestSettingsView_ShowsLiveValuesAndSource(t *testing.T) {
	t.Parallel()

	s := newConsoleTestStore(t)
	b := bus.New()
	d := settingsTestDispatcher(t, s, b, zdispatch.Config{MaxParallel: 1, Interval: time.Hour}, &settingsBudgetHandler{})
	srv := newMutationTestServerWithTuner(t, s, b, d)

	if err := d.SetTuning(t.Context(), zdispatch.TuneMaxParallel, 3, "peter"); err != nil {
		t.Fatalf("SetTuning: %v", err)
	}

	resp, r, cancel := openStream(t, srv.URL, "settings", 0, 0)
	defer cancel()
	defer resp.Body.Close()
	_, main, _, _ := readInitialFrames(t, r)

	for _, name := range []string{"max_parallel", "interval_seconds", "agent_minutes_per_ticket"} {
		if !strings.Contains(main, `data-tuning-name="`+name+`"`) {
			t.Errorf("main missing tuning row for %q; got:\n%s", name, main)
		}
		msgID := "tuning-message-" + name
		if !strings.Contains(main, `id="`+msgID+`"`) || !strings.Contains(main, `role="alert"`) {
			t.Errorf("main missing alert message span for %q; got:\n%s", name, main)
		}
	}
	if !strings.Contains(main, `data-tuning-name="max_parallel"`) || !strings.Contains(main, `value="3"`) {
		t.Errorf("main missing max_parallel value 3; got:\n%s", main)
	}
	if !strings.Contains(main, "set by peter at") {
		t.Errorf("main missing \"set by peter at\" source line; got:\n%s", main)
	}
	if !strings.Contains(main, "data-ignore-morph") {
		t.Errorf("main missing data-ignore-morph on the message span; got:\n%s", main)
	}

	srvNoTuner, _ := newMutationTestServer(t, newConsoleTestStore(t), bus.New(), newTestLogHandler(t))
	resp2, r2, cancel2 := openStream(t, srvNoTuner.URL, "settings", 0, 0)
	defer cancel2()
	defer resp2.Body.Close()
	_, main2, _, _ := readInitialFrames(t, r2)
	if !strings.Contains(main2, "Dispatch settings need a running zing serve.") {
		t.Errorf("main (no tuner) missing the unavailable line; got:\n%s", main2)
	}
}
