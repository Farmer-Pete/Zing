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
	f.mu.Unlock()
	if block {
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
	t.Parallel()

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
	close(u.gate)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	loopDone := make(chan struct{})
	go func() { u.loop(ctx); close(loopDone) }()

	u.Request(1, shaA)
	// Give the loop time to start building A and block inside it before B
	// and C are queued behind it.
	time.Sleep(50 * time.Millisecond)
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
	if !hasCarry || carry.TicketID != 4 {
		t.Errorf("carry = %+v, hasCarry = %v, want ticket 4, true", carry, hasCarry)
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

// TestUpgrade_LoopCancelledAfterPrepareWritesNoMarker proves that when ctx
// ends while prepare is finishing successfully, loop discards the build
// without a marker, a target, a stop call, or any message.
func TestUpgrade_LoopCancelledAfterPrepareWritesNoMarker(t *testing.T) {
	t.Parallel()

	u, steps, stopped := newTestUpgrader(t)
	close(u.gate)

	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	steps.selftestFunc = func() (string, string, error) {
		cancel()
		return sha[:12], "", nil
	}

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
