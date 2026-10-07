package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	zing "zing"
	"zing/internal/config"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/store"
)

// The full queued-to-done ring this file used to drive through a real serve()
// (answering its planning and gate questions over a live HTTP server) moved
// to cmd/zing/selftest.go's selftestResumeE2E once task 14 wired serve's own
// runtime.Set and tracker to the two real runtimes and the real GitHub
// tracker: that ring only ever worked against the scripted Fake runtime and
// the fixture tracker (design D2), which production must never build, and
// selftestResumeE2E already proves the identical ring against its own Fake
// and Fixture, independent of serve(). The tests below only need serve() to
// come up and shut down cleanly; the one exception,
// TestServe_ClearsStaleDrainingAndStoppedFlagsAtStartup, drives only the
// dispatcher's code-only queued-to-planning transition.

// freeLoopbackPort asks the kernel for an unused loopback port by opening
// and immediately closing a listener on port 0, then reusing the port
// number it was assigned. This carries the ordinary test-only race of
// another process claiming the same port before serve binds it.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = ln.Close() }()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	return addr.Port
}

// openStoreWithRetry opens a second Store handle on dbPath, retrying on
// SQLITE_BUSY: store.Open runs its own migration check on every call, and
// that check can race serve's own first-ever Open (also migrating the same
// fresh file) closely enough that SQLite's busy_timeout pragma, which only
// applies inside a single connection's statements, does not cover it. A
// short retry loop is simpler than serializing test startup with serve's
// internals.
func openStoreWithRetry(ctx context.Context, t *testing.T, dbPath string) *store.Store {
	t.Helper()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("open store %s: %v (last error: %v)", dbPath, ctx.Err(), lastErr)
		default:
		}

		st, err := store.Open(ctx, dbPath)
		if err == nil {
			return st
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
}

// loopback is the console.bind address the tests below that need a working
// listener use.
const loopback = "127.0.0.1"

// testServeProjectName and testServeTracker name writeZingTOML's own config
// project (goconst): both this file and seed_test.go build a matching
// store.Project row for it directly, rather than repeating the literals.
const (
	testServeProjectName = "zing"
	testServeTracker     = "github"
	testServeStateQueued = "queued"
	// testServeTicketRef is seedQueuedTicketForServe's own tracker_ref
	// (goconst): every file that later looks for that one ticket by ref
	// shares this constant rather than repeating the literal.
	testServeTicketRef = "manual#1"
)

// zingTOMLOpts parameterizes writeZingTOML's console.bind, dispatch, and
// port fields, the ones the tests below vary; every other key is a fixed,
// valid one project (name "zing", repo "x/zing").
type zingTOMLOpts struct {
	Port            int
	IntervalSeconds int
	MaxParallel     int
	Bind            []string // nil or empty writes bind = [], an invalid config
	Self            bool     // true writes self = true on the one configured project
}

// newTestGitRepo git-inits a fresh temp directory and returns its path: task
// 8's serve wires one orchestrator.Orchestrator per configured project and
// resolves its git common dir at startup (GitCommonDir), so writeZingTOML's
// project path must be a real repository, not merely an existing directory.
// "git rev-parse --git-common-dir" succeeds against a freshly initialized
// repo with no commits, so init alone is enough.
func newTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := gitfixture.Git(t.Context(), dir, "init", "-q", dir); err != nil {
		t.Fatalf("git init %s: %v (%s)", dir, err, out)
	}
	return dir
}

// writeTestJudgeCodexHome creates a judge_codex_home folder, with an
// auth.json inside, under dataDir (PKG9-PLAN.md section 4.5, 7.3, D27): a
// sibling of dbPath, never under dataDir/tmp or dataDir/judge, so
// resolveJudgeCodexHome and checkJudgeCodexLogin both accept it.
func writeTestJudgeCodexHome(t *testing.T, dataDir string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "codex-judge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir judge_codex_home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write judge_codex_home/auth.json: %v", err)
	}
	return dir
}

// writeZingTOML writes a zing.toml built from opts to path, quoting each
// Bind entry into a TOML array (an empty or nil Bind writes bind = []). The
// one configured project's path is a fresh, real git repository
// (newTestGitRepo), since serve now resolves its git common dir at startup.
// judge_codex_home is set to a folder under path's own directory (every
// caller's dataDir, since cfgPath and dbPath always share one t.TempDir()),
// holding an auth.json, so checkJudgeCodexLogin passes and every existing
// serve() test here stays green now that machine.toml's own judge job
// needs it (PKG9-PLAN.md section 4.5, 7.3, D27).
func writeZingTOML(t *testing.T, path string, opts zingTOMLOpts) {
	t.Helper()

	bindItems := make([]string, len(opts.Bind))
	for i, b := range opts.Bind {
		bindItems[i] = strconv.Quote(b)
	}
	judgeCodexHome := writeTestJudgeCodexHome(t, filepath.Dir(path))

	selfLine := ""
	if opts.Self {
		selfLine = "self = true\n"
	}

	doc := fmt.Sprintf(`
user = "test-user"
github_token = "test-github-token"
claude_oauth_token = "test-claude-oauth-token"
judge_codex_home = %q

[console]
bind = [%s]
port = %d

[dispatch]
interval_seconds = %d
max_parallel = %d

[[projects]]
name = "zing"
repo = "x/zing"
path = %q
tracker = "github"
commands = { test = "go test ./...", lint = "golangci-lint run" }
%s`, judgeCodexHome, strings.Join(bindItems, ", "), opts.Port, opts.IntervalSeconds, opts.MaxParallel, newTestGitRepo(t), selfLine)

	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
}

// waitForTicketPastQueued polls a second store.Open on dbPath until the one
// fixture ticket has left state "queued", bounded so a dispatcher that never
// claims anything fails the test instead of hanging it.
func waitForTicketPastQueued(t *testing.T, dbPath string, serveDone <-chan error) store.Ticket {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	st := openStoreWithRetry(ctx, t, dbPath)
	defer func() { _ = st.Close() }()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal(`ticket did not leave state "queued" within the poll deadline (stale draining/stopped flags left set?)`)
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			tickets, err := st.ListAllTickets(ctx)
			if err != nil {
				t.Fatalf("ListAllTickets: %v", err)
			}
			if len(tickets) == 1 && tickets[0].State != testServeStateQueued {
				return tickets[0]
			}
		}
	}
}

// waitForServing polls baseURL's GET / until it gets any HTTP response,
// bounded so a server that never comes up fails the test instead of hanging
// it. It also watches serveDone, so an early serve failure reports serve's
// own error instead of the opaque "never became reachable" timeout.
func waitForServing(t *testing.T, baseURL string, serveDone <-chan error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("server never became reachable within the poll deadline")
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", http.NoBody)
			if err != nil {
				t.Fatalf("build GET / request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				continue // not up yet
			}
			_ = resp.Body.Close()
			return
		}
	}
}

// cancelAndWaitForServe cancels ctx and asserts serve returns nil on
// serveDone within the drain window, the same shutdown assertion the
// end-to-end test above makes.
func cancelAndWaitForServe(t *testing.T, cancel context.CancelFunc, serveDone <-chan error) {
	t.Helper()

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within the drain window after ctx cancel")
	}
}

// TestServe_ClearsStaleDrainingAndStoppedFlagsAtStartup proves the fix for
// a stale "draining" flag surviving a prior graceful stop: serve, run
// against a database whose "draining" and "stopped" settings are both
// already true (exactly as a previous clean shutdown leaves them), must
// clear both before starting the dispatcher. Left unfixed, dispatch.Tick's
// very first flag read (design section 6.8 step 2) returns without ever
// claiming the ticket, and dispatch.Run exits on the same flag right after,
// so the HTTP listener comes up but nothing is ever dispatched; the ticket
// then never leaves "queued" and waitForTicketPastQueued below times out.
func TestServe_ClearsStaleDrainingAndStoppedFlagsAtStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback},
	})

	// Pre-migrate (avoids racing serve's own first store.Open against the
	// fresh file's WAL init), then mark the database draining and stopped,
	// simulating what a prior graceful stop leaves behind. It also seeds one
	// queued ticket directly (task 14: serve's own tracker is now the real
	// GitHub one, so this proof no longer waits on a real intake call
	// landing a ticket), under the project name writeZingTOML's config
	// names, so ensureBindings's own EnsureProject reconciles onto this row
	// at startup instead of inserting a second one.
	st, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("pre-migrate store.Open: %v", err)
	}
	if err := st.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining(true): %v", err)
	}
	if err := st.SetStopped(t.Context(), true); err != nil {
		t.Fatalf("SetStopped(true): %v", err)
	}
	seedQueuedTicketForServe(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("pre-migrate store.Close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, nil) }()

	waitForTicketPastQueued(t, dbPath, serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestServe_UpgraderRunsAlongsideShutdown proves serve's upgrader wiring
// (newUpgrader, go up.loop(ctx), close(up.gate), and the cancelServe-then-
// wait-on-upDone sequence before shutdown) does not change serve's ordinary
// shutdown behavior: with a non-nil su whose exe never matches
// DATA_DIR/bin/zing (so install_check would refuse any upgrade, exactly as
// in production before the owner sets self = true), serve still starts up,
// and a plain ctx cancellation still drains it within the usual deadline.
// Since no Request ever reaches the upgrader, up.Target never reports ok,
// so su.next must stay nil (review finding r2f2). The "upgrade: enabled"
// INFO log, captured off the real process os.Stderr (installLogHandler
// resets slog's default to write there, so a swapped slog default alone
// would never see it), proves newUpgrader really built a non-nil upgrader
// from su and serve really started its loop goroutine; without that wiring
// this log line would never appear, where su.next staying nil alone would
// not catch its removal (r3f2).
func TestServe_UpgraderRunsAlongsideShutdown(t *testing.T) {
	// Not t.Parallel(): this test swaps the process os.Stderr to capture
	// the "upgrade: enabled" log, which would race with any other test
	// logging concurrently.

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback}, Self: true,
	})

	su := &selfUpgrade{exe: filepath.Join(dir, "not-the-running-binary"), running: "0123456789ab"}

	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	var logs strings.Builder
	var copyErr error
	logsDone := make(chan struct{})
	go func() {
		_, copyErr = io.Copy(&logs, r)
		close(logsDone)
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, su) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)

	os.Stderr = origStderr
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	<-logsDone
	if copyErr != nil {
		t.Fatal(copyErr)
	}

	if su.next != nil {
		t.Errorf("su.next = %+v, want nil: no upgrade was ever requested", su.next)
	}
	got := logs.String()
	hasMsg := strings.Contains(got, `msg="upgrade: enabled"`)
	hasExe := strings.Contains(got, "exe="+su.exe)
	hasRunning := strings.Contains(got, "running="+su.running)
	if !hasMsg || !hasExe || !hasRunning {
		t.Errorf("logs = %q, want an \"upgrade: enabled\" line with exe=%s and running=%s (hasMsg=%v hasExe=%v hasRunning=%v): serve must have built a non-nil upgrader from su",
			got, su.exe, su.running, hasMsg, hasExe, hasRunning)
	}
}

// syncLogBuffer is a thread-safe io.Writer, so a test can poll the text an
// os.Pipe-captured os.Stderr has accumulated so far while serve is still
// running, instead of waiting for the pipe to close.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestServe_WatchBootRecordsBootedOK proves serve's boot-watch wiring end to
// end: with su.boot set to bootWatch, startUpgrader leaves the upgrader's
// gate shut, the watch goroutine started once the listeners are up polls
// GET / until it answers 200, and onBooted then removes upgrade.json and
// opens the gate with the marker's carried request already queued. su.exe
// is deliberately not DATA_DIR/bin/zing, so prepare's own install_check
// refuses the carried request and logs "upgrade: not run" -- since no
// fakeSteps can be injected into serve's own upgrader, that refusal log
// line, captured off the real process os.Stderr the same way
// TestServe_UpgraderRunsAlongsideShutdown does (installLogHandler resets
// slog's default to write there), is what proves the carried request
// really reached the now-open loop.
func TestServe_WatchBootRecordsBootedOK(t *testing.T) {
	// Not t.Parallel(): swaps the process os.Stderr to capture the
	// "upgrade: not run" log, which would race with any other test logging
	// concurrently.

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback}, Self: true,
	})

	marker := upgradeMarker{
		FromSHA: "0123456789ab", ToSHA: "fedcba9876543210fedcba9876543210fedcba9",
		TicketID: 0, State: markerAttempted,
		HasNext: true, NextSHA: strings.Repeat("0", 40), NextTicketID: 0,
	}
	if err := saveUpgradeMarker(dir, marker); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	su := &selfUpgrade{
		exe: filepath.Join(dir, "not-the-running-binary"), running: "0123456789ab",
		boot: bootWatch, marker: marker,
	}

	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	logs := &syncLogBuffer{}
	var copyErr error
	logsDone := make(chan struct{})
	go func() {
		_, copyErr = io.Copy(logs, r)
		close(logsDone)
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, su) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, found, loadErr := loadUpgradeMarker(dir)
		if loadErr != nil {
			t.Fatalf("loadUpgradeMarker: %v", loadErr)
		}
		if !found && strings.Contains(logs.String(), "upgrade: not run") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker_gone=%v logs=%q: want upgrade.json gone and an \"upgrade: not run\" log line within 5s", !found, logs.String())
		}
		select {
		case err := <-serveDone:
			t.Fatalf("serve exited early: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancelAndWaitForServe(t, cancel, serveDone)

	os.Stderr = origStderr
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	<-logsDone
	if copyErr != nil {
		t.Fatal(copyErr)
	}
}

// TestServe_ReportBootClosesRolledBack proves serve's report-boot wiring
// (review finding r1f1): with su.boot set to bootReport and su.marker set
// to a rolled_back marker, serve calls closeUpgrade right after dataDir
// resolves, which removes upgrade.json, before it ever reaches the
// listeners.
func TestServe_ReportBootClosesRolledBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback},
	})

	marker := upgradeMarker{
		FromSHA: "0123456789ab", ToSHA: "fedcba9876543210fedcba9876543210fedcba9",
		TicketID: 0, State: markerRolledBack,
	}
	if err := saveUpgradeMarker(dir, marker); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	su := &selfUpgrade{boot: bootReport, marker: marker}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, su) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)

	if _, found, loadErr := loadUpgradeMarker(dir); loadErr != nil || found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v, want gone", found, loadErr)
	}

	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestServe_UpgraderStopsOnDispatcherExitWithoutCtxCancel proves that serve's
// cancelServe-then-wait-on-upDone sequence (right before shutdown, serve.go)
// really ends the running upgrader loop goroutine even when the parent ctx
// is never cancelled. The drain here is triggered by the dispatcher's own
// goroutine ending on its own, through the store's "draining" flag, which
// does not touch ctx at all (waitForShutdownTrigger's dispDone case). Left
// unfixed (review finding r4f1: deleting the cancelServe()/<-upDone call
// at serve.go), up.loop would keep waiting on its own derived ctx, which
// stays live, and serve would hang forever on <-upDone instead of
// returning once shutdown finishes.
func TestServe_UpgraderStopsOnDispatcherExitWithoutCtxCancel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback}, Self: true,
	})

	su := &selfUpgrade{exe: filepath.Join(dir, "not-the-running-binary"), running: "0123456789ab"}

	// t.Context() is never cancelled by this test: the only way serve ends
	// below is the dispatcher's own exit, not a signal or parent cancel.
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(t.Context(), cfgPath, dbPath, false, su) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)

	st, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetDraining(t.Context(), true); err != nil {
		t.Fatalf("SetDraining(true): %v", err)
	}

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within the drain window after the dispatcher's own exit; the upgrader loop likely never saw its ctx end")
	}
}

// seedQueuedTicketForServe inserts one queued ticket directly into st, under
// a project named "zing" (writeZingTOML's own config project name), so a
// test can prove the dispatcher really claims and advances it without
// depending on a real tracker's Intake landing one first.
func seedQueuedTicketForServe(t *testing.T, st *store.Store) {
	t.Helper()

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: testServeProjectName, RepoURL: "https://github.com/x/zing", Tracker: testServeTracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testServeTicketRef, Title: "a ticket", State: testServeStateQueued,
	}); err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
}

// TestServe_ClampsInvalidDispatchConfig proves that an explicit, invalid
// dispatch.interval_seconds and dispatch.max_parallel (both 0: present in
// the file, so internal/config's applyDefaults leaves them as the literal
// zeros instead of substituting its own defaults) do not reach
// dispatch.New unclamped. Left unfixed, time.Duration(0) passed to
// dispatch.Config.Interval panics time.NewTicker inside the dispatcher's
// goroutine the moment serve starts it, crashing the process; this test
// instead expects serve to come up and shut down cleanly.
func TestServe_ClampsInvalidDispatchConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 0, MaxParallel: 0, Bind: []string{loopback},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, nil) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestServe_StoredTuningWinsOverZingTOMLAfterRestart proves the wiring
// around dispatch.LoadTuning and console.WithTuner in runServe (#81, "after
// a serve restart, the stored values win over zing.toml"): a settings-table
// row stored before serve starts, under a different zing.toml max_parallel
// and budget, must be what the Settings view and the live dispatcher show,
// not zing.toml's own values. Passing the zing.toml-derived values straight
// into zdispatch.Config instead of LoadTuning's result, or dropping
// console.WithTuner(d) from console.New, would still start serve cleanly
// and leave this test as the only thing that catches it.
func TestServe_StoredTuningWinsOverZingTOMLAfterRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback},
	})

	st, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("pre-migrate store.Open: %v", err)
	}
	setErr := st.SetSettings(t.Context(),
		"dispatch.max_parallel", "3", "dispatch.max_parallel.changed_by", "peter", "dispatch.max_parallel.changed_at", "2026-10-06T14:03:00Z",
		"budget.agent_minutes_per_ticket", "480", "budget.agent_minutes_per_ticket.changed_by", "peter", "budget.agent_minutes_per_ticket.changed_at", "2026-10-06T14:03:00Z",
	)
	if setErr != nil {
		t.Fatalf("SetSettings: %v", setErr)
	}
	closeErr := st.Close()
	if closeErr != nil {
		t.Fatalf("pre-migrate store.Close: %v", closeErr)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, nil) }()

	baseURL := fmt.Sprintf("http://%s:%d", loopback, port)
	waitForServing(t, baseURL, serveDone)

	v := url.Values{}
	v.Set("datastar", `{"view":"settings","open":0,"project":0}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/stream?"+v.Encode(), http.NoBody)
	if err != nil {
		t.Fatalf("new /stream request: %v", err)
	}
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// /stream always sends three frames on connect, in order: #nav, then
	// #main, then #rail (readInitialSelftestFrames, cmd/zing/selftest.go).
	// This test only needs #main, so it reads with the same
	// readSelftestFrame readInitialSelftestFrames itself calls, skipping
	// the other two, rather than adding a second caller that discards
	// readInitialSelftestFrames' own nav and rail results.
	streamReader := bufio.NewReader(resp.Body)
	if _, navErr := readSelftestFrame(streamReader); navErr != nil {
		t.Fatalf("read nav frame: %v", navErr)
	}
	main, mainErr := readSelftestFrame(streamReader)
	if mainErr != nil {
		t.Fatalf("read main frame: %v", mainErr)
	}
	if _, railErr := readSelftestFrame(streamReader); railErr != nil {
		t.Fatalf("read rail frame: %v", railErr)
	}
	if !strings.Contains(main, `data-tuning-name="max_parallel"`) || !strings.Contains(main, `value="3"`) {
		t.Errorf("settings frame missing max_parallel value 3 (stored, not zing.toml's 1); got:\n%s", main)
	}
	if !strings.Contains(main, `value="480"`) {
		t.Errorf("settings frame missing agent_minutes_per_ticket value 480 (stored, not zing.toml's default); got:\n%s", main)
	}

	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestServe_ErrorsOnEmptyConsoleBind proves that an explicit, empty
// console.bind ("bind = []", present in the file so applyDefaults does not
// substitute its own default) fails serve with a clear error instead of
// panicking net.JoinHostPort(cfg.Console.Bind[0], ...) on an out-of-range
// index. The error must surface before store.Open, so no database file is
// created.
func TestServe_ErrorsOnEmptyConsoleBind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: freeLoopbackPort(t), IntervalSeconds: 1, MaxParallel: 1, Bind: nil,
	})

	err := serve(t.Context(), cfgPath, dbPath, false, nil)
	if err == nil {
		t.Fatal("serve returned nil, want an error for an empty console.bind")
	}
	if !strings.Contains(err.Error(), "console.bind") {
		t.Errorf("serve error = %q, want it to mention console.bind", err.Error())
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat dbPath after the error = %v, want os.ErrNotExist (serve must fail before store.Open)", statErr)
	}
}

// TestServe_BindsEveryLiteralAddressAndSkipsAnUnresolvedTailscaleEntry
// proves multi-bind (design section 6.14, Task 11): serve binds every
// literal console.bind entry (not only the first, unlike the single-bind
// skeleton TestConsoleBindAddr used to cover), and a "tailscale" entry that
// cannot resolve in this sandboxed test environment (no tailscale CLI, no
// matching interface) is skipped rather than failing serve.
func TestServe_BindsEveryLiteralAddressAndSkipsAnUnresolvedTailscaleEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback, bindTokenTailscale},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, cfgPath, dbPath, false, nil) }()

	waitForServing(t, fmt.Sprintf("http://127.0.0.1:%d", port), serveDone)
	cancelAndWaitForServe(t, cancel, serveDone)
}

// TestResolveBindHosts_EmptyTokensResolvesToNoHosts and the config-level
// wildcard/allowed_hosts checks cover the rest of multi-bind resolution
// directly (bind_test.go, internal/config/config_test.go); serve's own
// "no address resolved" error path is covered by
// TestServe_ErrorsOnEmptyConsoleBind above.

// TestDispatchInterval covers dispatchInterval directly: a positive
// interval_seconds converts straight to seconds, zero or negative clamps to
// defaultDispatchInterval, and a value large enough to overflow a
// time.Duration also clamps to defaultDispatchInterval instead of wrapping
// around and panicking time.NewTicker (cubic P1).
func TestDispatchInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "positive", seconds: 5, want: 5 * time.Second},
		{name: "zero clamps to default", seconds: 0, want: defaultDispatchInterval},
		{name: "negative clamps to default", seconds: -1, want: defaultDispatchInterval},
		{
			name:    "just under the overflow bound converts straight through",
			seconds: int(maxDispatchIntervalSeconds), want: time.Duration(maxDispatchIntervalSeconds) * time.Second,
		},
		{
			name:    "huge value overflowing a time.Duration clamps to default",
			seconds: math.MaxInt64, want: defaultDispatchInterval,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dispatchInterval(tc.seconds); got != tc.want {
				t.Errorf("dispatchInterval(%d) = %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}

// TestReviewBotChecks covers reviewBotChecks directly: it converts every
// config.ReviewBotCheck entry into job.ReviewBotRule's own check list,
// field by field, in order, and a nil or empty input converts to an empty
// (non-nil) slice rather than nil, so a caller can always range over the
// result.
func TestReviewBotChecks(t *testing.T) {
	t.Parallel()

	t.Run("nil converts to an empty slice", func(t *testing.T) {
		t.Parallel()
		got := reviewBotChecks(nil)
		if got == nil || len(got) != 0 {
			t.Errorf("reviewBotChecks(nil) = %+v, want a non-nil empty slice", got)
		}
	})

	t.Run("every entry converts field by field, in order", func(t *testing.T) {
		t.Parallel()
		in := []config.ReviewBotCheck{
			{Check: "CodeRabbit", Trigger: "@coderabbitai review"},
			{Check: "Other Bot", Trigger: "@otherbot review"},
		}
		want := []job.ReviewBotCheck{
			{Check: "CodeRabbit", Trigger: "@coderabbitai review"},
			{Check: "Other Bot", Trigger: "@otherbot review"},
		}
		got := reviewBotChecks(in)
		if len(got) != len(want) {
			t.Fatalf("reviewBotChecks(%+v) = %+v, want %+v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("reviewBotChecks(%+v)[%d] = %+v, want %+v", in, i, got[i], want[i])
			}
		}
	})
}

// TestDispatchMaxParallel covers dispatchMaxParallel directly: a positive
// value passes through, and zero or negative clamps to
// defaultDispatchMaxParallel.
func TestDispatchMaxParallel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "positive", n: 3, want: 3},
		{name: "zero clamps to default", n: 0, want: defaultDispatchMaxParallel},
		{name: "negative clamps to default", n: -1, want: defaultDispatchMaxParallel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dispatchMaxParallel(tc.n); got != tc.want {
				t.Errorf("dispatchMaxParallel(%d) = %d, want %d", tc.n, got, tc.want)
			}
		})
	}
}

// TestFindSelfProject covers findSelfProject directly (#109 part 1, Q2):
// zero self projects reports ok false with no error, exactly one reports its
// index, and two is the exact startup error, since serve can upgrade only
// one project.
func TestFindSelfProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		projects []config.Project
		wantIdx  int
		wantOK   bool
		wantErr  string
	}{
		{name: "none self", projects: []config.Project{{Name: "a"}, {Name: "b"}}, wantIdx: -1, wantOK: false},
		{name: "one self at index 1", projects: []config.Project{{Name: "a"}, {Name: "b", Self: true}}, wantIdx: 1, wantOK: true},
		{
			name:     "two self",
			projects: []config.Project{{Name: "a", Self: true}, {Name: "b", Self: true}},
			wantIdx:  -1, wantOK: false,
			wantErr: "zing.toml: only one project may set self = true",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx, ok, err := findSelfProject(tc.projects)
			if idx != tc.wantIdx || ok != tc.wantOK {
				t.Errorf("findSelfProject(...) = (%d, %v), want (%d, %v)", idx, ok, tc.wantIdx, tc.wantOK)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestDispatchFailure covers dispatchFailure directly (the decision
// shutdown makes about whether the dispatcher's own error becomes serve's
// return value): it fires only when dispTriggered is true and de is a real,
// non-context.Canceled error, in which case the returned error wraps de.
func TestDispatchFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	tests := []struct {
		name          string
		dispTriggered bool
		de            error
		wantNil       bool
	}{
		{name: "not triggered by the dispatcher", dispTriggered: false, de: boom, wantNil: true},
		{name: "triggered, no error", dispTriggered: true, de: nil, wantNil: true},
		{name: "triggered, context canceled", dispTriggered: true, de: context.Canceled, wantNil: true},
		{name: "triggered, real error", dispTriggered: true, de: boom, wantNil: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dispatchFailure(tc.dispTriggered, tc.de)
			if tc.wantNil {
				if got != nil {
					t.Errorf("dispatchFailure(%v, %v) = %v, want nil", tc.dispTriggered, tc.de, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("dispatchFailure(%v, %v) = nil, want a wrapped error", tc.dispTriggered, tc.de)
			}
			if !errors.Is(got, boom) {
				t.Errorf("dispatchFailure(%v, %v) = %v, want it to wrap %v", tc.dispTriggered, tc.de, got, boom)
			}
		})
	}
}

// TestServeRequiresClaudeOAuthToken proves checkClaudeOAuthToken's own gate
// (PKG9-PLAN.md section 4.5, D26): the real, checked-in machine.toml names
// several claude-runtime jobs, and "build" sorts first among them, so an
// empty token is refused with the exact error naming it; a machine with no
// claude-runtime job at all (every M1 job pointed at codex here) needs no
// token and returns nil.
func TestServeRequiresClaudeOAuthToken(t *testing.T) {
	t.Parallel()

	t.Run("a claude job and no key", func(t *testing.T) {
		t.Parallel()
		m, err := machine.Load(zing.Assets, "machine.toml")
		if err != nil {
			t.Fatalf("machine.Load: %v", err)
		}
		err = checkClaudeOAuthToken(m, "")
		if err == nil {
			t.Fatal("checkClaudeOAuthToken: want an error, got nil")
		}
		want := "serve: zing.toml: missing required key claude_oauth_token (machine.toml job build uses the claude runtime)"
		if err.Error() != want {
			t.Errorf("checkClaudeOAuthToken() = %q, want %q", err.Error(), want)
		}
	})

	t.Run("a claude job and a key", func(t *testing.T) {
		t.Parallel()
		m, err := machine.Load(zing.Assets, "machine.toml")
		if err != nil {
			t.Fatalf("machine.Load: %v", err)
		}
		if err := checkClaudeOAuthToken(m, "a-token"); err != nil {
			t.Errorf("checkClaudeOAuthToken() = %v, want nil", err)
		}
	})

	t.Run("no claude job and no key", func(t *testing.T) {
		t.Parallel()
		m := &machine.Machine{Jobs: map[string]machine.Job{
			"judge": {Runtime: "codex"},
		}}
		if err := checkClaudeOAuthToken(m, ""); err != nil {
			t.Errorf("checkClaudeOAuthToken() = %v, want nil (no job uses the claude runtime)", err)
		}
	})
}

// ---- judge_codex_home (PKG9-PLAN.md section 4.5, 7.3, D27; M2 task 2) -----

// resolvedTestDataDir returns dir's own symlink-resolved form, the same
// way resolveJudgeCodexHome resolves dataDir before comparing: macOS's own
// /var -> /private/var means t.TempDir() itself needs this before a test
// builds its own "want" error text around it.
func resolvedTestDataDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return resolved
}

// TestJudgeCodexHomeEqualsDataDir proves judge_codex_home resolving to
// DATA_DIR itself is refused with the exact error text (section 4.5's own
// table).
func TestJudgeCodexHomeEqualsDataDir(t *testing.T) {
	t.Parallel()

	dataDir := resolvedTestDataDir(t, t.TempDir())
	_, err := resolveJudgeCodexHome(dataDir, dataDir)
	if err == nil {
		t.Fatal("resolveJudgeCodexHome: want an error, got nil")
	}
	want := fmt.Sprintf("serve: judge_codex_home %s must be a folder inside the data directory %s, not the data directory itself", dataDir, dataDir)
	if err.Error() != want {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", err.Error(), want)
	}
}

// TestJudgeCodexHomeOutsideDataDir proves a judge_codex_home outside
// DATA_DIR entirely is refused with the exact error text.
func TestJudgeCodexHomeOutsideDataDir(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	rawDataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(rawDataDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dataDir := resolvedTestDataDir(t, rawDataDir)
	rawOutside := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(rawOutside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := resolvedTestDataDir(t, rawOutside)

	_, err := resolveJudgeCodexHome(dataDir, rawOutside)
	if err == nil {
		t.Fatal("resolveJudgeCodexHome: want an error, got nil")
	}
	want := fmt.Sprintf("serve: judge_codex_home %s must be inside the data directory %s", outside, dataDir)
	if err.Error() != want {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", err.Error(), want)
	}
}

// TestJudgeCodexHomePrefixSibling proves a judge_codex_home that merely
// shares DATA_DIR's own name as a string prefix (".zing-other" beside
// ".zing") is still refused, because filepath.Rel, not a string-prefix
// check, is what decides this (section 4.5's own worked example).
func TestJudgeCodexHomePrefixSibling(t *testing.T) {
	t.Parallel()

	base := resolvedTestDataDir(t, t.TempDir())
	dataDir := filepath.Join(base, ".zing")
	sibling := filepath.Join(base, ".zing-other")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := resolveJudgeCodexHome(dataDir, sibling)
	if err == nil {
		t.Fatal("resolveJudgeCodexHome: want an error, got nil")
	}
	want := fmt.Sprintf("serve: judge_codex_home %s must be inside the data directory %s", sibling, dataDir)
	if err.Error() != want {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", err.Error(), want)
	}
}

// TestJudgeCodexHomeSymlinkOutside proves a judge_codex_home reached
// through a symlink that really points outside DATA_DIR is refused: the
// check runs against the kernel-resolved path, not the one zing.toml wrote
// (mirroring internal/sandbox's own TestDeniesDataDirThroughSymlink).
func TestJudgeCodexHomeSymlinkOutside(t *testing.T) {
	t.Parallel()

	base := resolvedTestDataDir(t, t.TempDir())
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dataDir, "codex-judge-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := resolveJudgeCodexHome(dataDir, link)
	if err == nil {
		t.Fatal("resolveJudgeCodexHome: want an error, got nil")
	}
	want := fmt.Sprintf("serve: judge_codex_home %s must be inside the data directory %s", outside, dataDir)
	if err.Error() != want {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", err.Error(), want)
	}
}

// TestJudgeCodexHomeUnderTmp proves a judge_codex_home under
// <DATA_DIR>/tmp is refused: removeStartupTempRoots clears that folder
// whole at startup.
func TestJudgeCodexHomeUnderTmp(t *testing.T) {
	t.Parallel()

	dataDir := resolvedTestDataDir(t, t.TempDir())
	// The "tmp" folder itself must exist for evalSymlinksAllowMissing to
	// resolve underTmp's own parent; codex-judge, the folder
	// judge_codex_home would actually name, is deliberately left
	// uncreated, matching the "owner has not logged in yet" case.
	if err := os.MkdirAll(filepath.Join(dataDir, "tmp"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	underTmp := filepath.Join(dataDir, "tmp", "codex-judge")

	_, err := resolveJudgeCodexHome(dataDir, underTmp)
	if err == nil {
		t.Fatal("resolveJudgeCodexHome: want an error, got nil")
	}
	want := fmt.Sprintf("serve: judge_codex_home %s must not be inside %s, which serve clears at startup", underTmp, filepath.Join(dataDir, "tmp"))
	if err.Error() != want {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", err.Error(), want)
	}
}

// TestJudgeCodexHomeUnderJudge proves a judge_codex_home under
// <DATA_DIR>/judge is refused the same way: that is the per-run scenarios
// folder serve also clears at startup (section 7.3).
func TestJudgeCodexHomeUnderJudge(t *testing.T) {
	t.Parallel()

	dataDir := resolvedTestDataDir(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(dataDir, "judge"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	underJudge := filepath.Join(dataDir, "judge", "codex-judge")

	_, err := resolveJudgeCodexHome(dataDir, underJudge)
	if err == nil {
		t.Fatal("resolveJudgeCodexHome: want an error, got nil")
	}
	want := fmt.Sprintf("serve: judge_codex_home %s must not be inside %s, which serve clears at startup", underJudge, filepath.Join(dataDir, "judge"))
	if err.Error() != want {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", err.Error(), want)
	}
}

// TestServeSetsJudgeCodexHome proves a valid judge_codex_home -- a real
// folder inside DATA_DIR, not under tmp or judge -- resolves cleanly to
// its own symlink-resolved path, the value serve threads into
// dispatch.Config.JudgeCodexHome.
func TestServeSetsJudgeCodexHome(t *testing.T) {
	t.Parallel()

	dataDir := resolvedTestDataDir(t, t.TempDir())
	judgeCodexHome := filepath.Join(dataDir, "codex-judge")
	if err := os.MkdirAll(judgeCodexHome, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := resolveJudgeCodexHome(dataDir, judgeCodexHome)
	if err != nil {
		t.Fatalf("resolveJudgeCodexHome: %v", err)
	}
	if got != judgeCodexHome {
		t.Errorf("resolveJudgeCodexHome() = %q, want %q", got, judgeCodexHome)
	}
}

// TestServeRequiresJudgeCodexLogin proves checkJudgeCodexLogin requires
// <judge_codex_home>/auth.json to exist when jobs.judge's runtime is
// codex, with the exact error naming the missing file and the login
// command to run; a job named "judge" with another runtime, or no judge
// job at all, needs no login.
func TestServeRequiresJudgeCodexLogin(t *testing.T) {
	t.Parallel()

	t.Run("not logged in", func(t *testing.T) {
		t.Parallel()
		m := &machine.Machine{Jobs: map[string]machine.Job{
			"judge": {Runtime: runtimeNameCodex},
		}}
		judgeCodexHome := t.TempDir()

		err := checkJudgeCodexLogin(m, judgeCodexHome)
		if err == nil {
			t.Fatal("checkJudgeCodexLogin: want an error, got nil")
		}
		want := fmt.Sprintf("serve: judge Codex is not logged in: %s not found; run CODEX_HOME=%s codex login",
			filepath.Join(judgeCodexHome, "auth.json"), judgeCodexHome)
		if err.Error() != want {
			t.Errorf("checkJudgeCodexLogin() = %q, want %q", err.Error(), want)
		}
	})

	t.Run("a claude judge needs no codex login", func(t *testing.T) {
		t.Parallel()
		m, err := machine.Load(zing.Assets, "machine.toml")
		if err != nil {
			t.Fatalf("machine.Load: %v", err)
		}
		judgeCodexHome := t.TempDir()
		if err := checkJudgeCodexLogin(m, judgeCodexHome); err != nil {
			t.Errorf("checkJudgeCodexLogin() = %v, want nil (judge runtime is claude)", err)
		}
	})

	t.Run("logged in", func(t *testing.T) {
		t.Parallel()
		m, err := machine.Load(zing.Assets, "machine.toml")
		if err != nil {
			t.Fatalf("machine.Load: %v", err)
		}
		judgeCodexHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(judgeCodexHome, "auth.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write auth.json: %v", err)
		}
		if err := checkJudgeCodexLogin(m, judgeCodexHome); err != nil {
			t.Errorf("checkJudgeCodexLogin() = %v, want nil", err)
		}
	})

	t.Run("no judge job needs no login", func(t *testing.T) {
		t.Parallel()
		m := &machine.Machine{Jobs: map[string]machine.Job{
			"build": {Runtime: runtimeNameClaude},
		}}
		if err := checkJudgeCodexLogin(m, filepath.Join(t.TempDir(), "never-created")); err != nil {
			t.Errorf("checkJudgeCodexLogin() = %v, want nil (no judge job at all)", err)
		}
	})
}

// TestServeRemovesTempRootAtStartup proves removeStartupTempRoots removes
// <dataDir>/tmp/ whole (PKG9-PLAN.md section 7.3): a stale run directory
// left behind by a process that died mid-run is gone afterward, and a
// missing directory is not an error.
func TestServeRemovesTempRootAtStartup(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	stale := filepath.Join(dataDir, "tmp", "run", "deadbeefdeadbeef", "tmp")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", stale, err)
	}

	if err := removeStartupTempRoots(dataDir); err != nil {
		t.Fatalf("removeStartupTempRoots: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tmp")); !os.IsNotExist(err) {
		t.Errorf("<dataDir>/tmp still exists after removeStartupTempRoots (stat err = %v)", err)
	}

	// A second call, against a directory that no longer has a tmp/ subtree,
	// must not error.
	if err := removeStartupTempRoots(dataDir); err != nil {
		t.Errorf("removeStartupTempRoots (already removed): %v", err)
	}
}

// TestServeRemovesJudgeDirAtStartup proves removeStartupJudgeDir removes
// <dataDir>/judge/ whole (PKG9-PLAN.md section 7.3, D19): a stale run's own
// scenarios folder, left behind by a process that died mid-run, is gone
// afterward, and a missing directory is not an error.
func TestServeRemovesJudgeDirAtStartup(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	stale := filepath.Join(dataDir, "judge", "17")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", stale, err)
	}
	if err := os.WriteFile(filepath.Join(stale, "scenarios.xml"), []byte("<scenario/>\n"), 0o600); err != nil {
		t.Fatalf("write scenarios.xml: %v", err)
	}

	if err := removeStartupJudgeDir(dataDir); err != nil {
		t.Fatalf("removeStartupJudgeDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "judge")); !os.IsNotExist(err) {
		t.Errorf("<dataDir>/judge still exists after removeStartupJudgeDir (stat err = %v)", err)
	}

	// A second call, against a directory that no longer has a judge/
	// subtree, must not error.
	if err := removeStartupJudgeDir(dataDir); err != nil {
		t.Errorf("removeStartupJudgeDir (already removed): %v", err)
	}
}

// writeStderrFixture writes <dir>/run-<runID>-stderr.log (writeStderrFile's
// own name, runjob.go) and backdates its mtime to mtime, the shape
// TestServeRemovesStaleStderrFilesAtStartup needs to plant both a stale and
// a fresh file without waiting on a real run.
func writeStderrFixture(t *testing.T, dir string, runID int64, mtime time.Time) string {
	t.Helper()

	path := filepath.Join(dir, fmt.Sprintf("run-%d-stderr.log", runID))
	if err := os.WriteFile(path, []byte("stderr\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

// TestServeRemovesStaleStderrFilesAtStartup proves removeStaleStderrFiles
// (ticket #8's retention fix): a finished run's stderr file older than
// stderrRetention is removed, a fresh one is kept,
// and an old file is kept when its own run is still open (runs.outcome IS
// NULL), whatever its age. A file outside the run-<id>-stderr.log shape is
// left alone no matter how old it is.
func TestServeRemovesStaleStderrFilesAtStartup(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dataDir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: testServeProjectName, RepoURL: "https://github.com/x/zing", Tracker: testServeTracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	ticketA, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "manual#1", Title: "a", State: testServeStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket (A): %v", err)
	}
	ticketB, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "manual#2", Title: "b", State: testServeStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket (B): %v", err)
	}

	const ownerA, ownerB = "owner-a", "owner-b"
	now := time.Now()
	leaseExpires := now.Add(10 * time.Minute)
	var claimed bool
	claimed, err = st.Claim(t.Context(), ticketA, ownerA, leaseExpires)
	if err != nil || !claimed {
		t.Fatalf("claim A: claimed=%v err=%v", claimed, err)
	}
	claimed, err = st.Claim(t.Context(), ticketB, ownerB, leaseExpires)
	if err != nil || !claimed {
		t.Fatalf("claim B: claimed=%v err=%v", claimed, err)
	}

	runA, err := st.Reserve(t.Context(), ticketA, ownerA, leaseExpires,
		store.SessionUpsert{Job: "planning", Runtime: runtimeNameFake}, store.RunSeed{Model: "fake-model"})
	if err != nil {
		t.Fatalf("reserve A: %v", err)
	}
	runB, err := st.Reserve(t.Context(), ticketB, ownerB, leaseExpires,
		store.SessionUpsert{Job: "planning", Runtime: runtimeNameFake}, store.RunSeed{Model: "fake-model"})
	if err != nil {
		t.Fatalf("reserve B: %v", err)
	}

	// Expiring only ownerA's claim finishes run A (reconcileReservedRunsTx
	// terminalizes it) while leaving run B's claim, and its run, open.
	if _, err := st.ExpireClaims(t.Context(), now.Add(time.Hour), ownerA); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}

	runsDir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runsDir, err)
	}

	staleFinished := writeStderrFixture(t, runsDir, runA.RunID, now.Add(-20*24*time.Hour))
	staleOpen := writeStderrFixture(t, runsDir, runB.RunID, now.Add(-20*24*time.Hour))
	fresh := writeStderrFixture(t, runsDir, 999999, now.Add(-1*24*time.Hour))
	notes := filepath.Join(runsDir, "notes.txt")
	if err := os.WriteFile(notes, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write %s: %v", notes, err)
	}
	if err := os.Chtimes(notes, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour)); err != nil {
		t.Fatalf("chtimes %s: %v", notes, err)
	}
	// A directory that happens to match the run-<id>-stderr.log name shape
	// must be left alone: only entry.Type().IsRegular() files are removed.
	staleDir := filepath.Join(runsDir, "run-5-stderr.log")
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", staleDir, err)
	}
	if err := os.Chtimes(staleDir, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour)); err != nil {
		t.Fatalf("chtimes %s: %v", staleDir, err)
	}

	n := removeStaleStderrFiles(t.Context(), st, dataDir, now)
	if n != 1 {
		t.Errorf("removeStaleStderrFiles = %d, want 1", n)
	}
	if _, err := os.Stat(staleFinished); !os.IsNotExist(err) {
		t.Errorf("stale finished run's stderr file still exists (stat err = %v)", err)
	}
	for _, path := range []string{staleOpen, fresh, notes, staleDir} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed, want kept: %v", path, err)
		}
	}

	// A second call removes nothing more.
	if n := removeStaleStderrFiles(t.Context(), st, dataDir, now); n != 0 {
		t.Errorf("removeStaleStderrFiles (second call) = %d, want 0", n)
	}

	// A dataDir with no runs/ directory at all is not an error.
	if n := removeStaleStderrFiles(t.Context(), st, t.TempDir(), now); n != 0 {
		t.Errorf("removeStaleStderrFiles (no runs dir) = %d, want 0", n)
	}
}

// TestServeRemovesStaleStderrFilesAtStartup_OpenRunIDsFails proves that when
// store.Store.OpenRunIDs fails, removeStaleStderrFiles deletes nothing and
// returns 0: a failed open-run query must not be treated as an empty open
// set, or every old stderr file, including those of runs still in progress,
// would be removed.
func TestServeRemovesStaleStderrFilesAtStartup_OpenRunIDsFails(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dataDir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Closing the store before the sweep runs makes its next query fail,
	// standing in for any open-run query error.
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	runsDir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runsDir, err)
	}
	now := time.Now()
	stale := writeStderrFixture(t, runsDir, 1, now.Add(-20*24*time.Hour))

	if n := removeStaleStderrFiles(t.Context(), st, dataDir, now); n != 0 {
		t.Errorf("removeStaleStderrFiles = %d, want 0", n)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("%s was removed, want kept: %v", stale, err)
	}
}

// TestServeRemovesStaleStderrFilesAtStartup_UnparseableRunID proves that a
// name matching the run-<id>-stderr.log shape whose id fails to parse as an
// integer can name no open run, so it is treated as finished and removed on
// age alone.
func TestServeRemovesStaleStderrFilesAtStartup_UnparseableRunID(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dataDir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runsDir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runsDir, err)
	}
	now := time.Now()
	unparseable := filepath.Join(runsDir, "run-abc-stderr.log")
	if err := os.WriteFile(unparseable, []byte("stderr\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", unparseable, err)
	}
	if err := os.Chtimes(unparseable, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour)); err != nil {
		t.Fatalf("chtimes %s: %v", unparseable, err)
	}

	if n := removeStaleStderrFiles(t.Context(), st, dataDir, now); n != 1 {
		t.Errorf("removeStaleStderrFiles = %d, want 1", n)
	}
	if _, err := os.Stat(unparseable); !os.IsNotExist(err) {
		t.Errorf("%s still exists, want removed (stat err = %v)", unparseable, err)
	}
}

// TestServeRemovesStaleStderrFilesAtStartup_RemoveFailureIsNotCounted proves
// that a failed os.Remove is logged and skipped, not counted: n++ only ever
// follows a successful Remove.
func TestServeRemovesStaleStderrFilesAtStartup_RemoveFailureIsNotCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so unlink would still succeed")
	}
	t.Parallel()

	dataDir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dataDir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runsDir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", runsDir, err)
	}
	now := time.Now()
	staleA := writeStderrFixture(t, runsDir, 1, now.Add(-20*24*time.Hour))
	staleB := writeStderrFixture(t, runsDir, 2, now.Add(-20*24*time.Hour))

	// A read-only runs directory lets entries be listed and stat'd but
	// makes every unlink inside it fail with EACCES.
	if err := os.Chmod(runsDir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", runsDir, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(runsDir, 0o700); err != nil {
			t.Logf("restore runs dir permissions: %v", err)
		}
	})

	if n := removeStaleStderrFiles(t.Context(), st, dataDir, now); n != 0 {
		t.Errorf("removeStaleStderrFiles = %d, want 0", n)
	}
	for _, path := range []string{staleA, staleB} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed, want kept: %v", path, err)
		}
	}
}

// TestResolvePushToken_StableAcrossARestartUnlessExplicitlyConfigured
// proves the design section 6.13 precedence rule end to end against a real
// store: with no explicit console.push_token, the first call generates and
// persists a token that a second call (simulating a restart, with the same
// empty configured value) reuses unchanged; an explicit configured value on
// a later call always wins and overrides what was persisted, "rotating it
// in zing.toml" the way the design names.
func TestResolvePushToken_StableAcrossARestartUnlessExplicitlyConfigured(t *testing.T) {
	t.Parallel()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	first, err := resolvePushToken(t.Context(), st, "")
	if err != nil {
		t.Fatalf("resolvePushToken (first, generated): %v", err)
	}
	if first == "" {
		t.Fatal("resolvePushToken (first) = \"\", want a generated token")
	}

	// A second call with no explicit config, simulating a restart: must
	// reuse the persisted token, not generate a new one.
	second, err := resolvePushToken(t.Context(), st, "")
	if err != nil {
		t.Fatalf("resolvePushToken (second, after restart): %v", err)
	}
	if second != first {
		t.Errorf("resolvePushToken (after restart) = %q, want the same persisted token (%q)", second, first)
	}

	// An explicit config value always wins and rotates the effective token.
	explicit, err := resolvePushToken(t.Context(), st, "my-explicit-token")
	if err != nil {
		t.Fatalf("resolvePushToken (explicit): %v", err)
	}
	if explicit != "my-explicit-token" {
		t.Errorf("resolvePushToken (explicit) = %q, want my-explicit-token", explicit)
	}
}

// --- #45 milestone 5: one serve per data directory -------------------------

// TestServe_SecondServeRefused proves design D7 end to end: a second serve
// against the same data directory while a first one is still up is
// refused, naming the first serve's pid, and does not disturb the first
// serve at all.
func TestServe_SecondServeRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zing.toml")
	dbPath := filepath.Join(dir, "zing.db")

	port := freeLoopbackPort(t)
	writeZingTOML(t, cfgPath, zingTOMLOpts{
		Port: port, IntervalSeconds: 1, MaxParallel: 1, Bind: []string{loopback},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	firstDone := make(chan error, 1)
	go func() { firstDone <- serve(ctx, cfgPath, dbPath, false, nil) }()

	waitForServing(t, fmt.Sprintf("http://%s:%d", loopback, port), firstDone)

	secondCtx, secondCancel := context.WithCancel(t.Context())
	defer secondCancel()
	err := serve(secondCtx, cfgPath, dbPath, false, nil)
	if err == nil {
		t.Fatal("second serve against the same data directory: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "another zing serve is running") {
		t.Errorf("second serve err = %q, want it to name the live holder", err.Error())
	}

	// The first serve must be untouched by the refused second attempt.
	waitForServing(t, fmt.Sprintf("http://%s:%d", loopback, port), firstDone)
	cancelAndWaitForServe(t, cancel, firstDone)
}
