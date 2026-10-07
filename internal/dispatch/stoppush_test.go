package dispatch_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/notify"
	"zing/internal/store"
)

// pushPostCounter is a mutex-guarded recorder of every POST a fake push
// service's httptest handler received, for a test whose dispatcher goroutine
// may still be sending while the test polls (the same syncBuffer-style
// guard resume_test.go's own log buffer uses).
type pushPostCounter struct {
	mu    sync.Mutex
	posts []recordedStopPush
}

// recordedStopPush is one POST stopPostHandler observed: the body and the
// headers the plan's shape section names.
type recordedStopPush struct {
	body    []byte
	headers http.Header
}

func (p *pushPostCounter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.posts)
}

func (p *pushPostCounter) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		p.mu.Lock()
		p.posts = append(p.posts, recordedStopPush{body: body, headers: r.Header.Clone()})
		p.mu.Unlock()
		w.WriteHeader(status)
	}
}

// countLinesWithAll reports how many lines of logged contain every one of
// want, so a test can tell a log line's own attributes apart from another
// line's (e.g. Resume's "dispatcher resumed from the console" line also
// carries kind=fail-closed, and a bare strings.Contains over the whole
// buffer cannot tell that attribute apart from the stop push line's own).
func countLinesWithAll(logged string, want ...string) int {
	count := 0
	for line := range strings.SplitSeq(logged, "\n") {
		matched := true
		for _, w := range want {
			if !strings.Contains(line, w) {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

// stopPushTestSubscription stores one fresh, valid push_subscriptions row
// pointed at endpoint, so notify.WebPush.Send has somewhere to POST.
func stopPushTestSubscription(t *testing.T, s *store.Store, endpoint string) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ua key: %v", err)
	}
	auth := make([]byte, 16)
	if _, readErr := rand.Read(auth); readErr != nil {
		t.Fatalf("random auth: %v", readErr)
	}
	keysJSON, err := json.Marshal(map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		"auth":   base64.RawURLEncoding.EncodeToString(auth),
	})
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
		Endpoint: endpoint, KeysJSON: keysJSON,
	}); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}
}

// TestRun_StopPushesOnceAndResumeDoesNot proves a fail-closed stop sends
// exactly one encrypted push, and that Resume's own re-dispatch sends none
// (design section "shape" rules, owner decision Q2): setStop's recorded
// branch, not Resume or a later successful tick, is the only place that
// calls notifyStop.
func TestRun_StopPushesOnceAndResumeDoesNot(t *testing.T) {
	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	rec := &pushPostCounter{}
	srv := httptest.NewServer(rec.handler(http.StatusCreated))
	defer srv.Close()
	stopPushTestSubscription(t, s, srv.URL)

	logBuf := &syncBuffer{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	reg := job.Registry()
	reg[testStateQueued] = &failOnceHandler{}
	reg[testStatePlanning] = noActionHandler{}

	push := notify.New(s)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner, Notifier: push})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after the fail-closed commit")
	waitUntil(t, func() bool { return rec.count() == 1 }, "exactly one push POST")

	if err := d.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitUntil(t, func() bool {
		return getTicket(t, s, ticketID).State == testStatePlanning
	}, "the ticket to dispatch again and reach planning")

	cancel()
	runErr := waitFor(t, runErrCh, "Run to return")
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run err = %v, want nil or context.Canceled", runErr)
	}

	if got := rec.count(); got != 1 {
		t.Fatalf("got %d push POSTs, want exactly 1 (none on Resume)", got)
	}
	post := rec.posts[0]
	if got := post.headers.Get("Content-Encoding"); got != "aes128gcm" {
		t.Errorf("Content-Encoding = %q, want aes128gcm", got)
	}
	if authz := post.headers.Get("Authorization"); !strings.HasPrefix(authz, "vapid t=") {
		t.Errorf("Authorization = %q, want a %q prefix", authz, "vapid t=")
	}
	if bytes.Contains(post.body, []byte("fail-closed")) {
		t.Errorf("push body contains plaintext %q, want it encrypted", "fail-closed")
	}

	logged := logBuf.String()
	wantSent := "dispatch: stop push sent"
	wantKind := "kind=" + dispatch.StopKindFailClosed
	if got := countLinesWithAll(logged, wantSent, wantKind); got != 1 {
		t.Errorf("lines with both %q and %q = %d, want exactly 1 (log: %s)", wantSent, wantKind, got, logged)
	}
	if got := countLinesWithAll(logged, wantSent, "ticket_id="); got != 1 {
		t.Errorf("lines with both %q and ticket_id= = %d, want exactly 1 (log: %s)", wantSent, got, logged)
	}
}

// TestRun_StopPushFailureOnlyWarns proves a failed send only logs a warning
// and never touches dispatching or Resume (design section "shape" rules: "A
// send failure only logs a warning and never affects dispatching"). The log
// is read only after Run has returned, so finish's own notifyWG.Wait() has
// already joined the send goroutine and this read cannot race its write.
func TestRun_StopPushFailureOnlyWarns(t *testing.T) {
	s := newDispatchTestStore(t)
	seedQueuedTicket(t, s, testFixtureRef)

	rec := &pushPostCounter{}
	srv := httptest.NewServer(rec.handler(http.StatusInternalServerError))
	defer srv.Close()
	stopPushTestSubscription(t, s, srv.URL)

	logBuf := &syncBuffer{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	reg := job.Registry()
	reg[testStateQueued] = &failOnceHandler{}
	reg[testStatePlanning] = noActionHandler{}

	push := notify.New(s)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner, Notifier: push})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	waitUntil(t, func() bool { return dispatch.IsStoppedForTest(d) }, "dispatcher to park after the fail-closed commit")

	if err := d.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitUntil(t, func() bool {
		return getTicket(t, s, 1).State == testStatePlanning
	}, "the ticket to dispatch again and reach planning")

	cancel()
	runErr := waitFor(t, runErrCh, "Run to return")
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run err = %v, want nil or context.Canceled", runErr)
	}

	logged := logBuf.String()
	wantFailed := "dispatch: stop push failed"
	wantKind := "kind=" + dispatch.StopKindFailClosed
	if got := countLinesWithAll(logged, wantFailed, wantKind); got != 1 {
		t.Errorf("lines with both %q and %q = %d, want exactly 1 (log: %s)", wantFailed, wantKind, got, logged)
	}
	if strings.Contains(logged, srv.URL) {
		t.Errorf("log contains the push endpoint URL %q, want it never logged", srv.URL)
	}
}

// TestResume_OwnerStopSendsNoPush proves an owner stop (the store's stopped
// flag alone, with no recorded stopErr) never calls notifyStop (design
// section "shape" rules, owner decision Q2): only setStop's recorded branch
// -- a non-nil error -- triggers a push, and an owner's POST /stop never
// gives setStop one.
func TestResume_OwnerStopSendsNoPush(t *testing.T) {
	s := newDispatchTestStore(t)
	if err := s.SetStopped(t.Context(), true); err != nil {
		t.Fatalf("SetStopped: %v", err)
	}

	rec := &pushPostCounter{}
	srv := httptest.NewServer(rec.handler(http.StatusCreated))
	defer srv.Close()
	stopPushTestSubscription(t, s, srv.URL)

	push := notify.New(s)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{MaxParallel: 1, Interval: 5 * time.Millisecond, Owner: testOwner, Notifier: push})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	if err := d.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	cancel()
	runErr := waitFor(t, runErrCh, "Run to return")
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run err = %v, want nil or context.Canceled", runErr)
	}

	if got := rec.count(); got != 0 {
		t.Errorf("got %d push POSTs, want 0 for an owner stop", got)
	}
}
