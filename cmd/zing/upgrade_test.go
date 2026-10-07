package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zing/internal/config"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/store"
)

// fakeSteps is a test double for upgradeSteps that records every Build call
// and returns canned results for both Build and Selftest. By default, Build
// returns the sha it was given, and Selftest echoes back the last sha Build
// returned, so a built sha survives unmodified through to the version check
// unless a test overrides it.
type fakeSteps struct {
	mu         sync.Mutex
	buildCalls []string
	lastBuilt  string

	buildSHA string
	buildErr error

	// blockSHA and blockCh, when both set, make Build block until blockCh
	// is closed, but only for the matching sha.
	blockSHA string
	blockCh  chan struct{}
	// blockStarted, when set, is closed the instant Build enters its block
	// for blockSHA, so a test can wait for that instead of sleeping.
	blockStarted chan struct{}

	selftestVersion string
	selftestOutput  string
	selftestErr     error
	// selftestFunc, when set, replaces every other Selftest behavior.
	selftestFunc func() (version, output string, err error)
}

func (f *fakeSteps) Build(_ context.Context, sha, out string) (string, error) {
	f.mu.Lock()
	f.buildCalls = append(f.buildCalls, sha)
	block := f.blockSHA != "" && f.blockSHA == sha
	ch := f.blockCh
	started := f.blockStarted
	f.mu.Unlock()
	if block {
		if started != nil {
			close(started)
		}
		<-ch
	}
	if f.buildErr != nil {
		return "", f.buildErr
	}
	if err := os.WriteFile(out, []byte("new"), 0o755); err != nil {
		return "", err
	}
	built := f.buildSHA
	if built == "" {
		built = sha
	}
	f.mu.Lock()
	f.lastBuilt = built
	f.mu.Unlock()
	return built, nil
}

func (f *fakeSteps) Selftest(_ context.Context, _ string) (version, output string, err error) {
	if f.selftestFunc != nil {
		return f.selftestFunc()
	}
	if f.selftestErr != nil {
		return "", f.selftestOutput, f.selftestErr
	}
	if f.selftestVersion != "" {
		return f.selftestVersion, f.selftestOutput, nil
	}
	f.mu.Lock()
	v := f.lastBuilt
	f.mu.Unlock()
	return v, f.selftestOutput, nil
}

func (f *fakeSteps) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.buildCalls...)
}

// newTestUpgrader builds an upgrader whose DATA_DIR is a fresh t.TempDir,
// with bin/zing holding "old", exe set to that path, running set to
// 0123456789ab, and a fresh *fakeSteps as its steps. stop is a sync.Once
// closing the returned stopped channel, so a test can tell whether loop
// called it. gate is unbuffered, left open for the test to close.
func newTestUpgrader(t *testing.T) (u *upgrader, steps *fakeSteps, stopped chan struct{}) {
	t.Helper()

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	binPath := filepath.Join(dataDir, "bin", "zing")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("write bin/zing: %v", err)
	}

	st, err := store.Open(t.Context(), filepath.Join(dataDir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	steps = &fakeSteps{}
	stopped = make(chan struct{})
	var stopOnce sync.Once
	u = &upgrader{
		dataDir: dataDir,
		exe:     binPath,
		running: "0123456789ab",
		store:   st,
		steps:   steps,
		wake:    make(chan struct{}, 1),
		stop:    func() { stopOnce.Do(func() { close(stopped) }) },
		gate:    make(chan struct{}),
	}
	return u, steps, stopped
}

// waitForClose waits up to 5 s for ch to close, failing the test otherwise.
func waitForClose(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestRedactURLs proves redactURLs replaces a URL's userinfo with REDACTED
// and leaves text with no URL unchanged.
func TestRedactURLs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "https with user and token",
			in:   "https://user:token@github.com/x/zing.git",
			want: "https://REDACTED@github.com/x/zing.git",
		},
		{
			name: "ssh with git user",
			in:   "ssh://git@github.com/x/zing.git",
			want: "ssh://REDACTED@github.com/x/zing.git",
		},
		{
			name: "two URLs on one line",
			in:   "from https://a:b@h1/x to ssh://c@h2/y",
			want: "from https://REDACTED@h1/x to ssh://REDACTED@h2/y",
		},
		{
			name: "no URL",
			in:   "upgrade: build failed: exit status 1",
			want: "upgrade: build failed: exit status 1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := redactURLs(c.in); got != c.want {
				t.Errorf("redactURLs(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// seedTicketForUpgrade inserts one ticket directly into st, the way
// serve_test.go's seedQueuedTicketForServe seeds a ticket through the
// store, so tellOwner has somewhere to post.
func seedTicketForUpgrade(t *testing.T, st *store.Store) int64 {
	t.Helper()

	projectID, err := st.EnsureProject(t.Context(), store.Project{
		Name: "zing", RepoURL: "https://github.com/x/zing", Tracker: "github",
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := st.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "1", Title: "a ticket", State: "queued",
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID
}

// TestTellOwner_PostsOnlyForTicket proves tellOwner posts one redacted
// update message by author system when ticketID is above 0, and posts no
// message at all for ticket id 0.
func TestTellOwner_PostsOnlyForTicket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	ticketID := seedTicketForUpgrade(t, st)

	tellOwner(t.Context(), st, ticketID, "upgrade: build failed: https://u:tok@h/x")

	msgs, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ListMessages = %d messages, want 1", len(msgs))
	}
	if msgs[0].Type != "update" || msgs[0].Author != "system" {
		t.Errorf("message type/author = %s/%s, want update/system", msgs[0].Type, msgs[0].Author)
	}
	if strings.Contains(msgs[0].Body, "tok") {
		t.Errorf("message body %q still contains the secret", msgs[0].Body)
	}
	if !strings.Contains(msgs[0].Body, "REDACTED") {
		t.Errorf("message body %q does not contain REDACTED", msgs[0].Body)
	}

	tellOwner(t.Context(), st, 0, "upgrade: no ticket for this one")

	after, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListMessages after ticket_id 0 = %d messages, want still 1", len(after))
	}
}

// TestMatchesRunning proves the stamped-and-prefix rule matchesRunning
// applies to a version string.
func TestMatchesRunning(t *testing.T) {
	t.Parallel()

	sha := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name    string
		sha     string
		running string
		want    bool
	}{
		{"full sha matches", sha, "0123456789ab", true},
		{"devel is unstamped", sha, "devel", false},
		{"dirty suffix is unstamped", sha, "0123456789ab-dirty", false},
		{"too short a prefix", sha, "012345", false},
		{"different sha", sha, "fedcba987654", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := matchesRunning(c.sha, c.running); got != c.want {
				t.Errorf("matchesRunning(%q, %q) = %v, want %v", c.sha, c.running, got, c.want)
			}
		})
	}
}

// TestUpgrade_RequestQueuesAndCarries proves Request queues the newest
// request while no target is set, wakes the loop once, and once a target is
// set writes to carry instead, leaving queued untouched. It runs with no
// goroutines.
func TestUpgrade_RequestQueuesAndCarries(t *testing.T) {
	t.Parallel()

	u, _, _ := newTestUpgrader(t)

	u.Request(1, "a")
	u.Request(2, "b")

	u.mu.Lock()
	if !u.hasQueued || u.queued != (upgradeRequest{TicketID: 2, SHA: "b"}) {
		u.mu.Unlock()
		t.Fatalf("queued = %+v, hasQueued = %v, want {2 b}, true", u.queued, u.hasQueued)
	}
	u.mu.Unlock()

	select {
	case <-u.wake:
	default:
		t.Fatalf("wake has no signal, want one")
	}
	select {
	case <-u.wake:
		t.Fatalf("wake had a second signal, want exactly one queued for two Requests")
	default:
	}

	rt, carry, hasCarry, ok := u.Target()
	if ok {
		t.Fatalf("Target = %+v, %+v, %v, %v before target is set, want ok false", rt, carry, hasCarry, ok)
	}

	u.mu.Lock()
	target := restartTarget{Binary: "bin/zing", ToSHA: "b"}
	u.target = &target
	u.mu.Unlock()

	u.Request(3, "c")
	u.Request(4, "d")

	u.mu.Lock()
	if !u.hasQueued || u.queued != (upgradeRequest{TicketID: 2, SHA: "b"}) {
		u.mu.Unlock()
		t.Fatalf("queued changed after target was set: queued = %+v, hasQueued = %v, want still {2 b}, true", u.queued, u.hasQueued)
	}
	u.mu.Unlock()

	rt, carry, hasCarry, ok = u.Target()
	if !ok {
		t.Fatalf("Target ok = %v, want true", ok)
	}
	if rt != target {
		t.Errorf("Target restartTarget = %+v, want %+v", rt, target)
	}
	if !hasCarry || carry != (upgradeRequest{TicketID: 4, SHA: "d"}) {
		t.Errorf("carry = %+v, hasCarry = %v, want {4 d}, true", carry, hasCarry)
	}
}

// TestUpgrade_PrepareBacksUpAndKeepsPrev proves a successful prepare builds,
// selftests, backs up, hard-links zing.prev, and returns the restart target,
// logging one step per stage.
func TestUpgrade_PrepareBacksUpAndKeepsPrev(t *testing.T) {
	// Not t.Parallel(): this test swaps the global slog default logger to
	// capture step logs, which would race with any other test logging
	// concurrently.

	u, steps, _ := newTestUpgrader(t)
	builtSHA := "fedcba9876540123456789abcdef012345678900"
	steps.buildSHA = builtSHA
	steps.selftestVersion = builtSHA
	steps.selftestOutput = "ok"

	var logs strings.Builder
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	ticketID := seedTicketForUpgrade(t, u.store)
	req := upgradeRequest{TicketID: ticketID, SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

	rt, err := u.prepare(t.Context(), req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	binary := filepath.Join(u.dataDir, "bin", "zing")
	if rt.Binary != binary || rt.Next != binary+".next" || rt.FromSHA != u.running || rt.ToSHA != builtSHA || rt.TicketID != ticketID {
		t.Fatalf("restartTarget = %+v", rt)
	}

	nextBytes, err := os.ReadFile(binary + ".next")
	if err != nil || string(nextBytes) != "new" {
		t.Errorf("zing.next = %q, %v, want \"new\"", nextBytes, err)
	}
	prevBytes, err := os.ReadFile(binary + ".prev")
	if err != nil || string(prevBytes) != "old" {
		t.Errorf("zing.prev = %q, %v, want \"old\"", prevBytes, err)
	}
	binBytes, err := os.ReadFile(binary)
	if err != nil || string(binBytes) != "old" {
		t.Errorf("bin/zing = %q, %v, want \"old\"", binBytes, err)
	}
	binInfo, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("stat bin/zing: %v", err)
	}
	prevInfo, err := os.Stat(binary + ".prev")
	if err != nil {
		t.Fatalf("stat zing.prev: %v", err)
	}
	if !os.SameFile(binInfo, prevInfo) {
		t.Errorf("zing.prev does not share bin/zing's inode")
	}

	backupPath := filepath.Join(u.dataDir, backupPrefix+sha12(builtSHA))
	backup, err := store.Open(t.Context(), backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = backup.Close() }()
	if err := backup.VerifyTables(t.Context()); err != nil {
		t.Errorf("VerifyTables on backup: %v", err)
	}

	logged := logs.String()
	for _, step := range []string{"install_check", "build", "selftest", "version_check", "backup", "keep_prev"} {
		if !strings.Contains(logged, "step="+step) {
			t.Errorf("logs missing step %s:\n%s", step, logged)
		}
	}
}

// TestUpgrade_IgnoresDevBuildBinary proves install_check refuses to run when
// the running executable is not DATA_DIR/bin/zing.
func TestUpgrade_IgnoresDevBuildBinary(t *testing.T) {
	t.Parallel()

	u, steps, _ := newTestUpgrader(t)
	u.exe = filepath.Join(t.TempDir(), "dev-build-of-zing")

	ticketID := seedTicketForUpgrade(t, u.store)
	req := upgradeRequest{TicketID: ticketID, SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

	_, err := u.prepare(t.Context(), req)
	if !errors.Is(err, errUpgradeNotRun) {
		t.Fatalf("prepare error = %v, want errUpgradeNotRun", err)
	}
	if len(steps.calls()) != 0 {
		t.Errorf("Build called %d times, want 0", len(steps.calls()))
	}

	msgs, err := u.store.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ListMessages = %d, want 1", len(msgs))
	}
	binary := filepath.Join(u.dataDir, "bin", "zing")
	want := "upgrade: not run: serve runs " + u.exe + ", not " + binary
	if msgs[0].Body != want {
		t.Errorf("message = %q, want %q", msgs[0].Body, want)
	}

	for _, suffix := range []string{".next", ".prev"} {
		if _, statErr := os.Stat(binary + suffix); !os.IsNotExist(statErr) {
			t.Errorf("%s exists, want absent", binary+suffix)
		}
	}
	matches, err := filepath.Glob(filepath.Join(u.dataDir, backupPrefix+"*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("backups = %v, want none", matches)
	}
}

// TestUpgrade_UnstampedBuildKeepsOldBinary proves version_check refuses an
// unstamped or dirty zing.next, removes it, posts the message, and leaves
// bin/zing and the backups untouched.
func TestUpgrade_UnstampedBuildKeepsOldBinary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		version string
	}{
		{"devel", "devel"},
		{"dirty", "fedcba987654-dirty"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			u, steps, _ := newTestUpgrader(t)
			builtSHA := "fedcba9876540123456789abcdef012345678900"
			steps.buildSHA = builtSHA
			steps.selftestVersion = c.version

			ticketID := seedTicketForUpgrade(t, u.store)
			req := upgradeRequest{TicketID: ticketID, SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

			_, err := u.prepare(t.Context(), req)
			wantErr := "upgrade: zing.next reports version " + c.version + ", not " + builtSHA
			if err == nil || err.Error() != wantErr {
				t.Fatalf("prepare error = %v, want %q", err, wantErr)
			}

			binary := filepath.Join(u.dataDir, "bin", "zing")
			if _, statErr := os.Stat(binary + ".next"); !os.IsNotExist(statErr) {
				t.Errorf("zing.next exists, want removed")
			}
			binBytes, err := os.ReadFile(binary)
			if err != nil || string(binBytes) != "old" {
				t.Errorf("bin/zing = %q, %v, want unchanged \"old\"", binBytes, err)
			}
			matches, err := filepath.Glob(filepath.Join(u.dataDir, backupPrefix+"*"))
			if err != nil {
				t.Fatalf("glob: %v", err)
			}
			if len(matches) != 0 {
				t.Errorf("backups = %v, want none", matches)
			}

			msgs, err := u.store.ListMessages(t.Context(), ticketID)
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			if len(msgs) != 1 || msgs[0].Body != wantErr {
				t.Fatalf("messages = %+v, want one message %q", msgs, wantErr)
			}
		})
	}
}

// TestUpgrade_AlreadyRunningSkipsUpgrade proves that when the built sha
// already matches the running version, prepare stops after build, removes
// zing.next, posts no message, and never runs selftest or any later step.
func TestUpgrade_AlreadyRunningSkipsUpgrade(t *testing.T) {
	t.Parallel()

	u, steps, _ := newTestUpgrader(t)
	// 40 hex characters, starting with u.running ("0123456789ab").
	builtSHA := u.running + "cdefabcdefabcdefabcdefabcdef"
	if len(builtSHA) != 40 {
		t.Fatalf("builtSHA length = %d, want 40", len(builtSHA))
	}
	steps.buildSHA = builtSHA
	steps.selftestErr = errors.New("selftest must not run")

	ticketID := seedTicketForUpgrade(t, u.store)
	req := upgradeRequest{TicketID: ticketID, SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}

	_, err := u.prepare(t.Context(), req)
	if !errors.Is(err, errUpgradeNotRun) {
		t.Fatalf("prepare error = %v, want errUpgradeNotRun", err)
	}
	if calls := steps.calls(); len(calls) != 1 {
		t.Fatalf("Build called %d times, want 1", len(calls))
	}

	binary := filepath.Join(u.dataDir, "bin", "zing")
	if _, statErr := os.Stat(binary + ".next"); !os.IsNotExist(statErr) {
		t.Errorf("zing.next exists, want removed")
	}
	if _, statErr := os.Stat(binary + ".prev"); !os.IsNotExist(statErr) {
		t.Errorf("zing.prev exists, want absent")
	}
	matches, err := filepath.Glob(filepath.Join(u.dataDir, backupPrefix+"*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("backups = %v, want none", matches)
	}
	msgs, err := u.store.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages = %+v, want none", msgs)
	}
}

// TestUpgrade_LoopBuildsNewestQueuedSHA proves a build superseded mid-flight
// is discarded for the newest queued request without returning to loop's
// wake wait, that loop builds exactly the shas that were ever started, and
// that a request arriving after stop lands in carry.
func TestUpgrade_LoopBuildsNewestQueuedSHA(t *testing.T) {
	t.Parallel()

	shaA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC := "cccccccccccccccccccccccccccccccccccccccc"[:40]
	shaD := "dddddddddddddddddddddddddddddddddddddddd"

	u, steps, stopped := newTestUpgrader(t)
	steps.blockSHA = shaA
	steps.blockCh = make(chan struct{})
	steps.blockStarted = make(chan struct{})
	close(u.gate)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	loopDone := make(chan struct{})
	go func() { u.loop(ctx); close(loopDone) }()

	u.Request(1, shaA)
	// Wait for the loop to actually enter Build for A and block inside it,
	// rather than assuming it has by a fixed deadline, before B and C are
	// queued behind it.
	waitForClose(t, steps.blockStarted, "Build(A) to start")
	u.Request(2, shaB)
	u.Request(3, shaC)

	close(steps.blockCh)
	waitForClose(t, stopped, "stop")

	u.Request(4, shaD)

	if got, want := steps.calls(), []string{shaA, shaC}; !slices.Equal(got, want) {
		t.Fatalf("Build calls = %v, want %v", got, want)
	}

	rt, carry, hasCarry, ok := u.Target()
	if !ok {
		t.Fatalf("Target ok = %v, want true", ok)
	}
	if rt.ToSHA != shaC {
		t.Errorf("Target.ToSHA = %q, want %q", rt.ToSHA, shaC)
	}
	if !hasCarry || carry.TicketID != 4 || carry.SHA != shaD {
		t.Errorf("carry = %+v, hasCarry = %v, want ticket 4, sha %q, true", carry, hasCarry, shaD)
	}

	m, found, err := loadUpgradeMarker(u.dataDir)
	if err != nil || !found {
		t.Fatalf("loadUpgradeMarker: %v, found %v", err, found)
	}
	if m.ToSHA != rt.ToSHA || m.State != markerPending {
		t.Errorf("marker = %+v, want to_sha %q, state pending", m, rt.ToSHA)
	}

	cancel()
	waitForClose(t, loopDone, "loop")
}

// TestUpgrade_SelftestFailsKeepsOldBinary drives a failing selftest through
// loop and proves the failure never sets a target, never writes a marker,
// and never calls stop.
func TestUpgrade_SelftestFailsKeepsOldBinary(t *testing.T) {
	t.Parallel()

	u, steps, stopped := newTestUpgrader(t)
	steps.selftestErr = errors.New("boom")
	close(u.gate)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go u.loop(ctx)

	ticketID := seedTicketForUpgrade(t, u.store)
	sha := "fedcba9876540123456789abcdef012345678900"
	u.Request(ticketID, sha)

	deadline := time.Now().Add(5 * time.Second)
	var msgs []store.MessageRow
	for time.Now().Before(deadline) {
		var err error
		msgs, err = u.store.ListMessages(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		found := false
		for _, m := range msgs {
			if strings.Contains(m.Body, "boom") {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatalf("no message posted within deadline")
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Body, "boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("messages = %+v, want one containing boom", msgs)
	}

	select {
	case <-stopped:
		t.Fatalf("stop was called, want it never called")
	default:
	}

	binary := filepath.Join(u.dataDir, "bin", "zing")
	if _, err := os.Stat(filepath.Join(u.dataDir, upgradeMarkerFile)); !os.IsNotExist(err) {
		t.Errorf("upgrade.json exists, want absent")
	}
	if _, _, _, ok := u.Target(); ok {
		t.Errorf("Target ok = true, want false")
	}
	binBytes, err := os.ReadFile(binary)
	if err != nil || string(binBytes) != "old" {
		t.Errorf("bin/zing = %q, %v, want unchanged \"old\"", binBytes, err)
	}
	if _, err := os.Stat(binary + ".next"); !os.IsNotExist(err) {
		t.Errorf("zing.next exists, want absent")
	}
}

// TestUpgrade_LoopMarkerWriteFailsKeepsOldBinary drives a failing
// saveUpgradeMarker through loop (by putting a directory where upgrade.json
// would be renamed) and proves the failure removes zing.next, posts a
// message starting with "upgrade: write upgrade.json:", and never sets a
// target or calls stop.
func TestUpgrade_LoopMarkerWriteFailsKeepsOldBinary(t *testing.T) {
	t.Parallel()

	u, steps, stopped := newTestUpgrader(t)
	builtSHA := "fedcba9876540123456789abcdef012345678900"
	steps.buildSHA = builtSHA
	steps.selftestVersion = builtSHA
	close(u.gate)

	if err := os.MkdirAll(filepath.Join(u.dataDir, upgradeMarkerFile), 0o755); err != nil {
		t.Fatalf("mkdir upgrade.json: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go u.loop(ctx)

	ticketID := seedTicketForUpgrade(t, u.store)
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	u.Request(ticketID, sha)

	deadline := time.Now().Add(5 * time.Second)
	var msgs []store.MessageRow
	for time.Now().Before(deadline) {
		var err error
		msgs, err = u.store.ListMessages(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if len(msgs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Body, "upgrade: write upgrade.json:") {
		t.Fatalf("messages = %+v, want one starting with %q", msgs, "upgrade: write upgrade.json:")
	}

	select {
	case <-stopped:
		t.Fatalf("stop was called, want it never called")
	default:
	}
	if _, _, _, ok := u.Target(); ok {
		t.Errorf("Target ok = true, want false")
	}
	binary := filepath.Join(u.dataDir, "bin", "zing")
	if _, err := os.Stat(binary + ".next"); !os.IsNotExist(err) {
		t.Errorf("zing.next exists, want absent")
	}
}

// TestUpgrade_LoopCancelledAfterPrepareWritesNoMarker proves that when ctx
// ends in the window right after prepare has already finished successfully
// but before runQueued's ctx.Err check runs, loop discards the build
// without a marker, a target, a stop call, or any message, even though
// prepare's own work (the backup and zing.prev) went ahead and happened.
func TestUpgrade_LoopCancelledAfterPrepareWritesNoMarker(t *testing.T) {
	t.Parallel()

	u, steps, stopped := newTestUpgrader(t)
	close(u.gate)

	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	builtSHA := "fedcba9876540123456789abcdef012345678900"
	steps.buildSHA = builtSHA
	steps.selftestVersion = builtSHA

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	u.testPostPrepare = cancel

	loopDone := make(chan struct{})
	go func() { u.loop(ctx); close(loopDone) }()

	ticketID := seedTicketForUpgrade(t, u.store)
	u.Request(ticketID, sha)

	waitForClose(t, loopDone, "loop")

	select {
	case <-stopped:
		t.Fatalf("stop was called, want it never called")
	default:
	}

	binary := filepath.Join(u.dataDir, "bin", "zing")
	if _, err := os.Stat(filepath.Join(u.dataDir, upgradeMarkerFile)); !os.IsNotExist(err) {
		t.Errorf("upgrade.json exists, want absent")
	}
	if _, err := os.Stat(binary + ".next"); !os.IsNotExist(err) {
		t.Errorf("zing.next exists, want absent")
	}
	if _, _, _, ok := u.Target(); ok {
		t.Errorf("Target ok = true, want false")
	}
	msgs, err := u.store.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages = %+v, want none", msgs)
	}

	// prepare itself must have succeeded before the post-prepare cancel, so
	// this proves runQueued's ctx.Err check (not prepare's own failure
	// path) is what discarded the build.
	if _, err := os.Stat(binary + ".prev"); err != nil {
		t.Errorf("zing.prev missing, want prepare to have succeeded: %v", err)
	}
	backupPath := filepath.Join(u.dataDir, backupPrefix+sha12(builtSHA))
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("backup missing, want prepare to have succeeded: %v", err)
	}
}

// TestRestartAfterServe proves restartAfterServe swaps rt.Next over
// rt.Binary and execs rt.Binary only when rt is non-nil and ctx is still
// live, and that a cancelled ctx or an exec error are both reported without
// touching the files further than described.
func TestRestartAfterServe(t *testing.T) {
	t.Parallel()

	newFakeExec := func(err error) (execFunc, *[]string, *string) {
		var calls []string
		var argv0 string
		return func(a0 string, argv, envv []string) error {
			argv0 = a0
			calls = append(calls, argv...)
			calls = append(calls, envv...)
			return err
		}, &calls, &argv0
	}

	t.Run("nil target", func(t *testing.T) {
		t.Parallel()
		exec, calls, _ := newFakeExec(nil)
		if err := restartAfterServe(t.Context(), nil, []string{"zing"}, nil, exec); err != nil {
			t.Fatalf("restartAfterServe: %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("exec called with nil target, want no call")
		}
	})

	t.Run("cancelled ctx", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		binary := filepath.Join(dir, "zing")
		next := binary + ".next"
		if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
			t.Fatalf("write zing.next: %v", err)
		}
		if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
			t.Fatalf("write zing: %v", err)
		}
		rt := &restartTarget{Binary: binary, Next: next, FromSHA: "0123456789ab", ToSHA: "fedcba987654"}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		exec, calls, _ := newFakeExec(nil)

		if err := restartAfterServe(ctx, rt, []string{"zing"}, nil, exec); err != nil {
			t.Fatalf("restartAfterServe: %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("exec called with cancelled ctx, want no call")
		}
		if _, err := os.Stat(next); err != nil {
			t.Errorf("zing.next must still exist: %v", err)
		}
	})

	t.Run("live ctx execs binary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		binary := filepath.Join(dir, "zing")
		next := binary + ".next"
		if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
			t.Fatalf("write zing.next: %v", err)
		}
		if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
			t.Fatalf("write zing: %v", err)
		}
		rt := &restartTarget{Binary: binary, Next: next, FromSHA: "0123456789ab", ToSHA: "fedcba987654"}

		exec, calls, argv0 := newFakeExec(nil)
		if err := restartAfterServe(t.Context(), rt, []string{"zing", "serve"}, []string{"A=1"}, exec); err != nil {
			t.Fatalf("restartAfterServe: %v", err)
		}
		if *argv0 != binary {
			t.Errorf("exec argv0 = %q, want %q", *argv0, binary)
		}
		if want := []string{"zing", "serve", "A=1"}; !slices.Equal(*calls, want) {
			t.Errorf("exec argv+env = %v, want %v", *calls, want)
		}
		got, err := os.ReadFile(binary)
		if err != nil || string(got) != "new" {
			t.Errorf("binary = %q, %v, want \"new\"", got, err)
		}
		if _, err := os.Stat(next); !os.IsNotExist(err) {
			t.Errorf("zing.next exists after swap, want removed")
		}
	})

	t.Run("exec error is wrapped", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		binary := filepath.Join(dir, "zing")
		next := binary + ".next"
		if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
			t.Fatalf("write zing.next: %v", err)
		}
		if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
			t.Fatalf("write zing: %v", err)
		}
		rt := &restartTarget{Binary: binary, Next: next}

		wantErr := errors.New("exec failed")
		exec, _, _ := newFakeExec(wantErr)
		err := restartAfterServe(t.Context(), rt, nil, nil, exec)
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("restartAfterServe error = %v, want wrapping %v", err, wantErr)
		}
	})
}

// TestUpgrade_LoopWaitsForGate proves loop never builds anything before its
// gate is closed, even with a request already queued.
func TestUpgrade_LoopWaitsForGate(t *testing.T) {
	t.Parallel()

	u, steps, _ := newTestUpgrader(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go u.loop(ctx)

	u.Request(1, "0000000000000000000000000000000000000000")

	time.Sleep(100 * time.Millisecond)
	if n := len(steps.calls()); n != 0 {
		t.Fatalf("Build called %d times before gate opened, want 0", n)
	}

	close(u.gate)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(steps.calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(steps.calls()); n != 1 {
		t.Fatalf("Build called %d times after gate opened, want 1", n)
	}
}

// TestUpgrade_MergeBuildsSelftestsSwapsAndExecs is the working demo for this
// part: a merge request drives the loop through prepare, which builds,
// selftests, backs up, and keeps zing.prev, then stops, and restartAfterServe
// completes the swap and execs the new binary with the same argv and env.
func TestUpgrade_MergeBuildsSelftestsSwapsAndExecs(t *testing.T) {
	t.Parallel()

	u, steps, stopped := newTestUpgrader(t)
	sha := "fedcba9876540123456789abcdef012345678900"
	steps.buildSHA = sha
	steps.selftestVersion = sha[:12]
	close(u.gate)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go u.loop(ctx)

	ticketID := seedTicketForUpgrade(t, u.store)
	u.Request(ticketID, sha)

	waitForClose(t, stopped, "stop")

	rt, _, _, ok := u.Target()
	if !ok {
		t.Fatalf("Target ok = %v, want true", ok)
	}

	binary := filepath.Join(u.dataDir, "bin", "zing")
	if rt.Binary != binary || rt.Next != binary+".next" {
		t.Fatalf("restartTarget = %+v", rt)
	}

	var argv0 string
	var gotArgv, gotEnv []string
	exec := func(a0 string, argv, envv []string) error {
		argv0, gotArgv, gotEnv = a0, argv, envv
		return nil
	}

	if err := restartAfterServe(t.Context(), &rt, []string{"zing", "serve"}, []string{"A=1"}, exec); err != nil {
		t.Fatalf("restartAfterServe: %v", err)
	}

	if argv0 != binary {
		t.Errorf("exec argv0 = %q, want %q", argv0, binary)
	}
	if want := []string{"zing", "serve"}; !slices.Equal(gotArgv, want) {
		t.Errorf("exec argv = %v, want %v", gotArgv, want)
	}
	if want := []string{"A=1"}; !slices.Equal(gotEnv, want) {
		t.Errorf("exec env = %v, want %v", gotEnv, want)
	}

	binBytes, err := os.ReadFile(binary)
	if err != nil || string(binBytes) != "new" {
		t.Errorf("bin/zing = %q, %v, want \"new\"", binBytes, err)
	}
	prevBytes, err := os.ReadFile(binary + ".prev")
	if err != nil || string(prevBytes) != "old" {
		t.Errorf("zing.prev = %q, %v, want \"old\"", prevBytes, err)
	}
	if _, statErr := os.Stat(binary + ".next"); !os.IsNotExist(statErr) {
		t.Errorf("zing.next exists after swap, want removed")
	}

	backupPath := filepath.Join(u.dataDir, backupPrefix+sha12(sha))
	if _, statErr := os.Stat(backupPath); statErr != nil {
		t.Errorf("backup %s missing: %v", backupPath, statErr)
	}

	m, found, err := loadUpgradeMarker(u.dataDir)
	if err != nil || !found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, err)
	}
	if m.State != markerPending || m.ToSHA != sha || m.TicketID != ticketID {
		t.Errorf("marker = %+v, want pending, to_sha %s, ticket_id %d", m, sha, ticketID)
	}
}

// TestUpgradeHandoff proves upgradeHandoff returns nil for a nil upgrader
// or one with no target yet, and otherwise saves the carry into upgrade.json
// (or just logs a WARN when that write fails) and returns the target.
func TestUpgradeHandoff(t *testing.T) {
	t.Parallel()

	if rt := upgradeHandoff(t.TempDir(), nil); rt != nil {
		t.Errorf("upgradeHandoff(nil) = %+v, want nil", rt)
	}

	u, _, _ := newTestUpgrader(t)
	if rt := upgradeHandoff(u.dataDir, u); rt != nil {
		t.Errorf("upgradeHandoff with no target = %+v, want nil", rt)
	}

	target := restartTarget{Binary: "bin/zing", FromSHA: "0123456789ab", ToSHA: "fedcba987654", TicketID: 7}
	marker := upgradeMarker{FromSHA: target.FromSHA, ToSHA: target.ToSHA, TicketID: target.TicketID, State: markerPending}
	if err := saveUpgradeMarker(u.dataDir, marker); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}
	u.mu.Lock()
	u.target = &target
	u.carry, u.hasCarry = upgradeRequest{TicketID: 9, SHA: "abcdef0123456789abcdef0123456789abcdef01"}, true
	u.mu.Unlock()

	rt := upgradeHandoff(u.dataDir, u)
	if rt == nil || *rt != target {
		t.Fatalf("upgradeHandoff = %+v, want %+v", rt, target)
	}
	m, found, err := loadUpgradeMarker(u.dataDir)
	if err != nil || !found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, err)
	}
	if !m.HasNext || m.NextSHA != "abcdef0123456789abcdef0123456789abcdef01" || m.NextTicketID != 9 {
		t.Errorf("marker = %+v, want the carry recorded", m)
	}

	// A missing upgrade.json makes saveCarry fail; upgradeHandoff must still
	// return the target, since the swap has to happen regardless.
	u2, _, _ := newTestUpgrader(t)
	u2.mu.Lock()
	u2.target = &target
	u2.carry, u2.hasCarry = upgradeRequest{TicketID: 9, SHA: "abc"}, true
	u2.mu.Unlock()
	rt2 := upgradeHandoff(u2.dataDir, u2)
	if rt2 == nil || *rt2 != target {
		t.Fatalf("upgradeHandoff with failing saveCarry = %+v, want %+v", rt2, target)
	}
}

// TestNewUpgrader covers newUpgrader's own rules: nil, nil with no usable
// self project, findSelfProject's own error even with a nil su, the missing
// binding error, and the fields it fills in for the one usable case.
func TestNewUpgrader(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dataDir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	oneSelf := []config.Project{{Name: "zing", Self: true}}
	bindingFor := func(names ...string) []zdispatch.Binding {
		bindings := make([]zdispatch.Binding, len(names))
		for i, name := range names {
			bindings[i] = zdispatch.Binding{StoreProjectID: int64(i + 1), TrackerProject: name}
		}
		return bindings
	}
	su := &selfUpgrade{exe: "/data/bin/zing", running: "0123456789ab"}

	t.Run("nil su with one self project", func(t *testing.T) {
		t.Parallel()
		up, err := newUpgrader(oneSelf, bindingFor("zing"), nil, nil, dataDir, st, nil)
		if up != nil || err != nil {
			t.Fatalf("newUpgrader = %v, %v, want nil, nil", up, err)
		}
	})

	t.Run("no self project", func(t *testing.T) {
		t.Parallel()
		up, err := newUpgrader([]config.Project{{Name: "zing"}}, bindingFor("zing"), nil, su, dataDir, st, nil)
		if up != nil || err != nil {
			t.Fatalf("newUpgrader = %v, %v, want nil, nil", up, err)
		}
	})

	t.Run("two self projects", func(t *testing.T) {
		t.Parallel()
		two := []config.Project{{Name: "a", Self: true}, {Name: "b", Self: true}}
		for _, testSu := range []*selfUpgrade{nil, su} {
			up, err := newUpgrader(two, bindingFor("a", "b"), nil, testSu, dataDir, st, nil)
			if up != nil || err == nil || err.Error() != "zing.toml: only one project may set self = true" {
				t.Fatalf("newUpgrader(su=%v) = %v, %v, want nil, the only-one-self error", testSu, up, err)
			}
		}
	})

	t.Run("missing binding", func(t *testing.T) {
		t.Parallel()
		up, err := newUpgrader(oneSelf, nil, nil, su, dataDir, st, nil)
		if up != nil || err == nil || err.Error() != "serve: self project zing: no store binding" {
			t.Fatalf("newUpgrader = %v, %v, want nil, the no-binding error", up, err)
		}
	})

	t.Run("one self project", func(t *testing.T) {
		t.Parallel()
		jobProjects := map[int64]job.Project{1: {RepoGit: "/data/repos/zing/.git"}}
		var stopped bool
		stop := func() { stopped = true }

		up, err := newUpgrader(oneSelf, bindingFor("zing"), jobProjects, su, dataDir, st, stop)
		if err != nil {
			t.Fatalf("newUpgrader: %v", err)
		}
		if up == nil {
			t.Fatal("newUpgrader returned nil upgrader")
		}
		if up.exe != su.exe || up.running != su.running {
			t.Errorf("exe/running = %q/%q, want %q/%q", up.exe, up.running, su.exe, su.running)
		}
		if up.dataDir != dataDir || up.store != st {
			t.Errorf("dataDir/store not copied through")
		}
		steps, ok := up.steps.(gitGoSteps)
		if !ok {
			t.Fatalf("steps = %T, want gitGoSteps", up.steps)
		}
		wantSteps := gitGoSteps{repoGit: "/data/repos/zing/.git", defaultBranch: "main", tmpRoot: filepath.Join(dataDir, "tmp")}
		if steps != wantSteps {
			t.Errorf("steps = %+v, want %+v", steps, wantSteps)
		}
		if cap(up.wake) != 1 {
			t.Errorf("wake capacity = %d, want 1", cap(up.wake))
		}
		if up.stop == nil {
			t.Fatal("stop not set")
		}
		up.stop()
		if !stopped {
			t.Error("up.stop() did not call the given stop func")
		}
	})
}
