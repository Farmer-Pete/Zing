// dispatchbanner_test.go: the stop banner's wiring (ticket #89, task 6):
// WithDispatch, stopBanner reached through a real GET /, and the fixture a
// real *dispatch.Dispatcher's own fail-closed stop renders through it.
package console_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
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
// always answers the status it is built with, and Resume (unused by this
// task's tests, exercised by the resume tests) answers resumeErr.
type fakeDispatch struct {
	status    zdispatch.StopStatus
	statusErr error
	resumeErr error
}

func (f *fakeDispatch) StopStatus(context.Context) (zdispatch.StopStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeDispatch) Resume(context.Context) error {
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
		srv := newTestServerDispatch(t, s, bus.New(), nil, newTestLogHandler(t), fd)

		body := getBody(t, srv.URL+"/")
		if !contains(body, "Dispatching is stopped by the owner.") {
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
		srv := newTestServerDispatch(t, s, bus.New(), nil, newTestLogHandler(t), fd)

		body := getBody(t, srv.URL+"/")
		for _, want := range []string{
			"Dispatching stopped after fail-closed on ticket 42.",
			"the lease was lost",
			"1 runs are still finishing; resume once they are done",
			`disabled`,
		} {
			if !contains(body, want) {
				t.Errorf("GET / body does not contain %q; got:\n%s", want, body)
			}
		}
	})

	t.Run("not_stopped", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		fd := &fakeDispatch{status: zdispatch.StopStatus{}}
		srv := newTestServerDispatch(t, s, bus.New(), nil, newTestLogHandler(t), fd)

		body := getBody(t, srv.URL+"/")
		if contains(body, `class="dispatch-banner"`) {
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

	srv := newTestServerDispatch(t, s, bus.New(), nil, newTestLogHandler(t), d)

	body := getBody(t, srv.URL+"/")
	for _, want := range []string{
		"Dispatching stopped after fail-closed on ticket",
		"the lease was lost",
		"runs again once its claim expires.",
		"dispatch-resume",
	} {
		if !contains(body, want) {
			t.Errorf("GET / body does not contain %q; got:\n%s", want, body)
		}
	}
	if !contains(body, strconv.FormatInt(ticketID, 10)) {
		t.Errorf("GET / body does not name ticket %d; got:\n%s", ticketID, body)
	}
	if contains(body, `class="dispatch-resume" disabled`) {
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

func contains(body, want string) bool {
	return strings.Contains(body, want)
}
