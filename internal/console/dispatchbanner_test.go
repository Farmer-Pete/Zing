// dispatchbanner_test.go: the stop banner's wiring (ticket #89, task 6):
// WithDispatch, stopBanner reached through a real GET /, and the fixture a
// real *dispatch.Dispatcher's own fail-closed stop renders through it.
package console_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zing/fixtures"
	"zing/internal/bus"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	zruntime "zing/internal/runtime"
	"zing/internal/store"
	"zing/internal/tracker"
)

// fakeDispatch is this file's own console.Dispatch double: StopStatus
// always answers the status it is built with, and Resume answers
// resumeErr and counts its own calls, so TestDispatchResume_CrossOriginRefused
// can prove a rejected request never reaches it (ticket #89, task 7).
type fakeDispatch struct {
	status      zdispatch.StopStatus
	statusErr   error
	resumeErr   error
	resumeCalls atomic.Int32
}

func (f *fakeDispatch) StopStatus(context.Context) (zdispatch.StopStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeDispatch) Resume(context.Context) error {
	f.resumeCalls.Add(1)
	return f.resumeErr
}

// consoleFailOnceHandler is this package's own failOnceHandler (mirroring
// internal/dispatch/resume_test.go's own): its first call self-steals this
// exact ticket's claim, the same fence-gone trick selfStealingHandler uses
// in internal/dispatch's own tests, so the dispatcher's own commit finds
// its fence already gone and fails closed; every later call instead
// returns a plain, successful transition commit.
type consoleFailOnceHandler struct {
	calls atomic.Int32
}

func (h *consoleFailOnceHandler) Run(ctx context.Context, t store.Ticket, d job.Deps) (store.HandlerCommit, error) {
	if h.calls.Add(1) == 1 {
		if _, err := d.Store.CommitHandlerResult(ctx, store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires}); err != nil {
			return store.HandlerCommit{}, err
		}
	}
	return store.HandlerCommit{TicketID: t.ID, Owner: d.Owner, Expires: d.Expires, Next: testPlanningLiteral, Reason: "test"}, nil
}

// newStoppedRealDispatcher builds a real *dispatch.Dispatcher over a fresh
// store and the real machine.toml, with "queued" replaced by a
// *consoleFailOnceHandler, seeds one queued ticket, and ticks it once: the
// handler self-steals its own claim, so the dispatcher's commit fails
// closed and d.Tick returns an error matching dispatch.ErrFailClosed. It
// returns the dispatcher (already stopped) and the failing ticket's id, so
// a test can wire it into a console through WithDispatch and assert on the
// banner it renders.
func newStoppedRealDispatcher(t *testing.T, s *store.Store) (d *zdispatch.Dispatcher, ticketID int64) {
	t.Helper()
	ctx := t.Context()

	m := testMachine(t)

	scripts, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("sub scripts fs: %v", err)
	}
	rt := zruntime.NewFake(scripts)
	rts, err := zruntime.NewSet(map[string]zruntime.Runtime{"claude": rt, "codex": rt, testRuntimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}

	reg := job.Registry()
	reg[testStateQueued] = &consoleFailOnceHandler{}

	d, err = zdispatch.New(s, tr, bus.New(), m, reg, nil,
		zdispatch.Config{MaxParallel: 1, Interval: time.Millisecond, Owner: "console-test"}, rts)
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}

	ticketID = seedTicket(t, s, "fake#dispatchbanner", "dispatch banner fixture")

	if tickErr := d.Tick(ctx); tickErr == nil || !errors.Is(tickErr, zdispatch.ErrFailClosed) {
		t.Fatalf("Tick: err = %v, want errors.Is(err, dispatch.ErrFailClosed)", tickErr)
	}

	return d, ticketID
}

// TestDispatchBanner_ShowsOnIndex proves GET / renders #alerts' stop
// banner from whatever console.Dispatch is wired through WithDispatch
// (ticket #89), for the owner-stop and the fail-closed-with-ticket shapes,
// and renders no banner at all when nothing is stopped.
func TestDispatchBanner_ShowsOnIndex(t *testing.T) {
	t.Parallel()

	t.Run("owner", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{status: zdispatch.StopStatus{Stopped: true, Kind: zdispatch.StopKindOwner}}
		srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

		body := getBody(t, srv.URL+"/")
		if !strings.Contains(body, "Dispatching is stopped by the owner.") {
			t.Errorf("GET / body does not contain the owner headline; got:\n%s", body)
		}
	})

	t.Run("fail_closed_with_ticket", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{status: zdispatch.StopStatus{
			Stopped: true, Kind: zdispatch.StopKindFailClosed, Cause: "dispatch: fail-closed: commit not applied cleanly: ticket 42: the lease was lost",
			TicketID: 42, HasTicket: true, InFlight: 1,
		}}
		srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

		body := getBody(t, srv.URL+"/")
		for _, want := range []string{
			"Dispatching stopped after fail-closed on ticket 42.",
			"the lease was lost",
			"1 runs are still finishing; resume once they are done",
			`class="dispatch-resume" disabled`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET / body does not contain %q; got:\n%s", want, body)
			}
		}
	})

	t.Run("not_stopped", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{status: zdispatch.StopStatus{}}
		srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

		body := getBody(t, srv.URL+"/")
		if strings.Contains(body, `class="dispatch-banner"`) {
			t.Errorf("GET / body contains a dispatch-banner section with nothing stopped; got:\n%s", body)
		}
	})
}

// TestDispatchBanner_RealDispatcherStopShowsBanner proves the banner comes
// from a real *dispatch.Dispatcher's own fail-closed stop, not only from a
// fake double (ticket #89's own "GET / shows the banner with the reason
// and ticket" verify-by).
func TestDispatchBanner_RealDispatcherStopShowsBanner(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	d, ticketID := newStoppedRealDispatcher(t, s)

	srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), d)

	body := getBody(t, srv.URL+"/")
	for _, want := range []string{
		fmt.Sprintf("Dispatching stopped after fail-closed on ticket %d.", ticketID),
		"the lease was lost",
		fmt.Sprintf("Ticket %d runs again once its claim expires.", ticketID),
		"dispatch-resume",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET / body does not contain %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="dispatch-resume" disabled`) {
		t.Errorf("GET / resume button is disabled with no run in flight; got:\n%s", body)
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestDispatchResume_Responses proves POST /dispatch/resume's four response
// shapes (ticket #89, task 7): 204 on success, 409 with the dispatcher's
// own *dispatch.ResumeRefusal sentence verbatim, 500 with the generic body
// on any other error, and 503 when no dispatcher is wired through
// WithDispatch at all.
func TestDispatchResume_Responses(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{}
		srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

		resp := doRequest(t, mutationRequest(t, srv, "/dispatch/resume", "{}"))
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
		if len(b) != 0 {
			t.Errorf("body = %q, want empty", b)
		}
	})

	t.Run("refusal", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{resumeErr: &zdispatch.ResumeRefusal{Reason: "the dispatcher is not stopped"}}
		srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

		resp := doRequest(t, mutationRequest(t, srv, "/dispatch/resume", "{}"))
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("status = %d, want 409", resp.StatusCode)
		}
		if got := strings.TrimSpace(string(b)); got != "the dispatcher is not stopped" {
			t.Errorf("body = %q, want %q", got, "the dispatcher is not stopped")
		}
	})

	t.Run("store_error", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{resumeErr: errors.New("boom")}
		srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

		resp := doRequest(t, mutationRequest(t, srv, "/dispatch/resume", "{}"))
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", resp.StatusCode)
		}
		if got := strings.TrimSpace(string(b)); got != "internal error" {
			t.Errorf("body = %q, want %q", got, "internal error")
		}
	})

	t.Run("no_dispatch", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

		resp := doRequest(t, mutationRequest(t, srv, "/dispatch/resume", "{}"))
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", resp.StatusCode)
		}
		if got := strings.TrimSpace(string(b)); got != "the dispatcher is not running in this process" {
			t.Errorf("body = %q, want %q", got, "the dispatcher is not running in this process")
		}
	})
}

// TestDispatchResume_CrossOriginRefused proves POST /dispatch/resume sits
// behind the same mutation guard every other state-changing route does
// (ticket #89, task 7): a cross-origin request never reaches
// Dispatch.Resume at all.
func TestDispatchResume_CrossOriginRefused(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	fd := &fakeDispatch{}
	srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), fd)

	req := mutationRequest(t, srv, "/dispatch/resume", "{}")
	req.Header.Set("Origin", "http://evil.example")
	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if got := fd.resumeCalls.Load(); got != 0 {
		t.Errorf("Resume calls = %d, want 0", got)
	}
}

// TestDispatchResume_RealDispatcherResumesAndDispatches is the plan's own
// demo (ticket #89, task 7): a real dispatcher fails closed on a ticket,
// GET / shows the banner naming that ticket, a same-origin POST
// /dispatch/resume answers 204, and the next Tick moves the ticket to
// planning with the banner gone -- all without a new Run or a serve
// restart.
func TestDispatchResume_RealDispatcherResumesAndDispatches(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	d, ticketID := newStoppedRealDispatcher(t, s)

	srv := newTestServerDispatch(t, s, bus.New(), newTestLogHandler(t), d)

	body := getBody(t, srv.URL+"/")
	wantHeadline := fmt.Sprintf("Dispatching stopped after fail-closed on ticket %d.", ticketID)
	if !strings.Contains(body, wantHeadline) {
		t.Errorf("GET / body does not contain %q before resume; got:\n%s", wantHeadline, body)
	}

	resp := doRequest(t, mutationRequest(t, srv, "/dispatch/resume", "{}"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /dispatch/resume status = %d, want 204", resp.StatusCode)
	}

	_, stopped, err := s.Flags(t.Context())
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if stopped {
		t.Error("Flags: stopped = true, want false after resume")
	}

	if tickErr := d.Tick(t.Context()); tickErr != nil {
		t.Fatalf("Tick after resume: %v", tickErr)
	}

	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.State != testPlanningLiteral {
		t.Errorf("ticket state = %q, want %q", ticket.State, testPlanningLiteral)
	}

	body = getBody(t, srv.URL+"/")
	if strings.Contains(body, `class="dispatch-banner"`) {
		t.Errorf("GET / body still contains a dispatch-banner after resume; got:\n%s", body)
	}
}
