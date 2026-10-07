package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zing/internal/store"
)

func TestDecideBoot(t *testing.T) {
	const (
		to7      = "0123456789abcdef"
		running7 = "0123456"
	)

	tests := []struct {
		name    string
		marker  upgradeMarker
		found   bool
		running string
		want    bootAction
	}{
		{
			name:    "missing",
			found:   false,
			running: running7,
			want:    bootNormal,
		},
		{
			name:    "pending match",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootWatch,
		},
		{
			name:    "pending mismatch",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: "fedcba9876543210",
			want:    bootDiscard,
		},
		{
			name:    "pending mismatch running devel",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: versionFallback,
			want:    bootDiscard,
		},
		{
			name:    "pending mismatch running dirty",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: "0123456" + versionDirtySuffix,
			want:    bootDiscard,
		},
		{
			name:    "pending mismatch running 6-char prefix",
			marker:  upgradeMarker{State: markerPending, ToSHA: to7},
			found:   true,
			running: "012345",
			want:    bootDiscard,
		},
		{
			name:    "attempted match",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootRollback,
		},
		{
			name:    "attempted mismatch",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: "fedcba9876543210",
			want:    bootDiscard,
		},
		{
			name:    "attempted mismatch running devel",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: versionFallback,
			want:    bootDiscard,
		},
		{
			name:    "attempted mismatch running dirty",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: "0123456" + versionDirtySuffix,
			want:    bootDiscard,
		},
		{
			name:    "attempted mismatch running 6-char prefix",
			marker:  upgradeMarker{State: markerAttempted, ToSHA: to7},
			found:   true,
			running: "012345",
			want:    bootDiscard,
		},
		{
			name:    "rolled_back match",
			marker:  upgradeMarker{State: markerRolledBack, ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootReport,
		},
		{
			name:    "rolled_back mismatch",
			marker:  upgradeMarker{State: markerRolledBack, ToSHA: to7},
			found:   true,
			running: "fedcba9876543210",
			want:    bootReport,
		},
		{
			name:    "bogus state",
			marker:  upgradeMarker{State: "bogus", ToSHA: to7},
			found:   true,
			running: running7,
			want:    bootDiscard,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideBoot(tt.marker, tt.found, tt.running); got != tt.want {
				t.Errorf("decideBoot(%+v, %v, %q) = %v, want %v", tt.marker, tt.found, tt.running, got, tt.want)
			}
		})
	}
}

func TestBootOutcome(t *testing.T) {
	for _, watching := range []bool{false, true} {
		for _, booted := range []bool{false, true} {
			for _, deadlinePassed := range []bool{false, true} {
				for _, signalled := range []bool{false, true} {
					var wantOutcome, wantCause string
					switch {
					case !watching || booted:
						wantOutcome, wantCause = outcomeNone, ""
					case signalled:
						wantOutcome, wantCause = outcomeRevert, ""
					case deadlinePassed:
						wantOutcome, wantCause = outcomeFailed, bootCauseDeadline
					default:
						wantOutcome, wantCause = outcomeFailed, bootCauseStopped
					}

					gotOutcome, gotCause := bootOutcome(watching, booted, deadlinePassed, signalled)
					if gotOutcome != wantOutcome || gotCause != wantCause {
						t.Errorf("bootOutcome(%v, %v, %v, %v) = (%v, %v), want (%v, %v)",
							watching, booted, deadlinePassed, signalled, gotOutcome, gotCause, wantOutcome, wantCause)
					}
				}
			}
		}
	}
}

func TestRollbackTarget(t *testing.T) {
	t.Parallel()

	m := upgradeMarker{FromSHA: "a1", ToSHA: "b2", TicketID: 7}
	got := rollbackTarget(m, "X")

	want := restartTarget{Binary: "X", Next: "", FromSHA: "b2", ToSHA: "a1", TicketID: 7}
	if got != want {
		t.Errorf("rollbackTarget(%+v, %q) = %+v, want %+v", m, "X", got, want)
	}
}

func TestRemoveMarker(t *testing.T) {
	t.Parallel()

	t.Run("removes existing marker", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, upgradeMarkerFile)
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if err := removeMarker(dir); err != nil {
			t.Fatalf("removeMarker: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("marker still exists after removeMarker")
		}
	})

	t.Run("missing marker is fine", func(t *testing.T) {
		t.Parallel()
		if err := removeMarker(t.TempDir()); err != nil {
			t.Errorf("removeMarker with no marker = %v, want nil", err)
		}
	})
}

// guardBootSHA is a stand-in commit sha, used as both the running build's
// version and the marker's to_sha so matchesRunning agrees they match.
const guardBootSHA = "0123456789abcdef0123456789abcdef01234567"

// writeGuardBootBinary resolves dir's data directory (macOS temp dirs sit
// behind a symlink), creates DATA_DIR/bin, and writes exe (and, with prev
// non-empty, DATA_DIR/bin/zing.prev) with the given contents. It returns
// the resolved data dir and the exe path.
func writeGuardBootBinary(t *testing.T, dir, exeContent, prevContent string) (resolved, exe string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	binDir := filepath.Join(resolved, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	exe = filepath.Join(binDir, "zing")
	if err := os.WriteFile(exe, []byte(exeContent), 0o700); err != nil {
		t.Fatalf("write %s: %v", exe, err)
	}
	if prevContent != "" {
		if err := os.WriteFile(exe+".prev", []byte(prevContent), 0o700); err != nil {
			t.Fatalf("write %s.prev: %v", exe, err)
		}
	}
	return resolved, exe
}

func TestGuardBoot_RollsBackAttempted(t *testing.T) {
	t.Parallel()

	t.Run("rolls back and releases the lock", func(t *testing.T) {
		t.Parallel()
		resolved, exe := writeGuardBootBinary(t, t.TempDir(), "new", "old")
		marker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 5, State: markerAttempted}
		if err := saveUpgradeMarker(resolved, marker); err != nil {
			t.Fatalf("saveUpgradeMarker: %v", err)
		}

		action, m, target, err := guardBoot(resolved, exe, guardBootSHA)
		if err != nil {
			t.Fatalf("guardBoot: %v", err)
		}
		if action != bootRollback {
			t.Errorf("action = %v, want bootRollback", action)
		}
		if target.Next != "" {
			t.Errorf("target.Next = %q, want empty", target.Next)
		}

		su := &selfUpgrade{boot: action, marker: m}
		if su.boot != bootRollback {
			t.Errorf("su.boot = %v, want bootRollback", su.boot)
		}

		got, err := os.ReadFile(exe)
		if err != nil {
			t.Fatalf("read exe: %v", err)
		}
		if string(got) != "old" {
			t.Errorf("exe contents = %q, want %q", got, "old")
		}
		if _, statErr := os.Stat(exe + ".prev"); !os.IsNotExist(statErr) {
			t.Errorf("zing.prev still exists after rollback")
		}

		saved, found, err := loadUpgradeMarker(resolved)
		if err != nil || !found {
			t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, err)
		}
		if saved.State != markerRolledBack {
			t.Errorf("marker state = %q, want %q", saved.State, markerRolledBack)
		}
		if su.marker.State != markerRolledBack {
			t.Errorf("su.marker.State = %q, want %q", su.marker.State, markerRolledBack)
		}

		lock, err := acquireServeLock(resolved)
		if err != nil {
			t.Fatalf("acquireServeLock after guardBoot: %v", err)
		}
		lock.release()
	})

	t.Run("no_prev", func(t *testing.T) {
		t.Parallel()
		resolved, exe := writeGuardBootBinary(t, t.TempDir(), "new", "")
		marker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 5, State: markerAttempted}
		if err := saveUpgradeMarker(resolved, marker); err != nil {
			t.Fatalf("saveUpgradeMarker: %v", err)
		}

		action, m, _, err := guardBoot(resolved, exe, guardBootSHA)
		if err != nil {
			t.Fatalf("guardBoot: %v", err)
		}
		if action != bootNormal {
			t.Errorf("action = %v, want bootNormal", action)
		}
		if m.State != markerAttempted {
			t.Errorf("m.State = %q, want %q (the marker as read, before the no-op removal)", m.State, markerAttempted)
		}
		if _, found, loadErr := loadUpgradeMarker(resolved); loadErr != nil || found {
			t.Errorf("marker found=%v err=%v, want gone", found, loadErr)
		}
	})
}

func TestGuardBoot_MarksPendingAttempted(t *testing.T) {
	resolved, exe := writeGuardBootBinary(t, t.TempDir(), "zing", "")
	marker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 9, State: markerPending}
	if err := saveUpgradeMarker(resolved, marker); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	action, m, _, err := guardBoot(resolved, exe, guardBootSHA)
	if err != nil {
		t.Fatalf("guardBoot: %v", err)
	}
	if action != bootWatch {
		t.Errorf("action = %v, want bootWatch", action)
	}

	su := &selfUpgrade{boot: action, marker: m}
	su.booted.Store(false)
	su.deadlinePassed.Store(false)
	if su.marker.State != markerAttempted {
		t.Errorf("su.marker.State = %q, want %q", su.marker.State, markerAttempted)
	}
	if su.booted.Load() || su.deadlinePassed.Load() {
		t.Errorf("su.booted / su.deadlinePassed should start false")
	}

	saved, found, err := loadUpgradeMarker(resolved)
	if err != nil || !found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, err)
	}
	if saved.State != markerAttempted {
		t.Errorf("marker state = %q, want %q", saved.State, markerAttempted)
	}

	t.Run("mismatch_discards", func(t *testing.T) {
		resolvedMismatch, exeMismatch := writeGuardBootBinary(t, t.TempDir(), "zing", "")
		pendingMarker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 9, State: markerPending}
		if saveErr := saveUpgradeMarker(resolvedMismatch, pendingMarker); saveErr != nil {
			t.Fatalf("saveUpgradeMarker: %v", saveErr)
		}

		action, m, _, guardErr := guardBoot(resolvedMismatch, exeMismatch, "fedcba9876543210")
		if guardErr != nil {
			t.Fatalf("guardBoot: %v", guardErr)
		}
		if action != bootDiscard {
			t.Errorf("action = %v, want bootDiscard", action)
		}
		if m.State != markerPending {
			t.Errorf("m.State = %q, want %q (the marker as read, before the discard)", m.State, markerPending)
		}
		if _, found, loadErr := loadUpgradeMarker(resolvedMismatch); loadErr != nil || found {
			t.Errorf("marker found=%v err=%v, want gone", found, loadErr)
		}
	})

	t.Run("missing_data_dir", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")

		action, _, _, guardErr := guardBoot(missing, exe, guardBootSHA)
		if guardErr != nil {
			t.Fatalf("guardBoot: %v", guardErr)
		}
		if action != bootNormal {
			t.Errorf("action = %v, want bootNormal", action)
		}
		if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
			t.Errorf("missing data dir was created")
		}
	})

	t.Run("exe_outside_data_dir", func(t *testing.T) {
		prevDefault := slog.Default()
		var buf bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prevDefault) })

		resolvedOutside, realExe := writeGuardBootBinary(t, t.TempDir(), "zing", "old")
		attemptedMarker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 9, State: markerAttempted}
		if saveErr := saveUpgradeMarker(resolvedOutside, attemptedMarker); saveErr != nil {
			t.Fatalf("saveUpgradeMarker: %v", saveErr)
		}
		before, readErr := os.ReadFile(filepath.Join(resolvedOutside, upgradeMarkerFile))
		if readErr != nil {
			t.Fatalf("read upgrade.json: %v", readErr)
		}
		prevBefore, readErr := os.ReadFile(realExe + ".prev")
		if readErr != nil {
			t.Fatalf("read zing.prev: %v", readErr)
		}

		outsideDir := t.TempDir()
		outsideExe := filepath.Join(outsideDir, "zing")
		if writeErr := os.WriteFile(outsideExe, []byte("outside"), 0o700); writeErr != nil {
			t.Fatalf("write outside exe: %v", writeErr)
		}

		action, m, _, guardErr := guardBoot(resolvedOutside, outsideExe, guardBootSHA)
		if guardErr != nil {
			t.Fatalf("guardBoot: %v", guardErr)
		}
		if action != bootNormal {
			t.Errorf("action = %v, want bootNormal", action)
		}
		if m != (upgradeMarker{}) {
			t.Errorf("m = %+v, want the zero value (the marker is never read for this action)", m)
		}

		after, readErr := os.ReadFile(filepath.Join(resolvedOutside, upgradeMarkerFile))
		if readErr != nil {
			t.Fatalf("read upgrade.json after: %v", readErr)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("upgrade.json changed: before=%q after=%q", before, after)
		}
		prevAfter, readErr := os.ReadFile(realExe + ".prev")
		if readErr != nil {
			t.Fatalf("read zing.prev after: %v", readErr)
		}
		if !bytes.Equal(prevBefore, prevAfter) {
			t.Errorf("zing.prev changed: before=%q after=%q", prevBefore, prevAfter)
		}

		for _, name := range []string{serveLockFilename, serveLockGuardFilename} {
			if _, statErr := os.Stat(filepath.Join(resolvedOutside, name)); !os.IsNotExist(statErr) {
				t.Errorf("%s was created in the data dir", name)
			}
		}

		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("log lines = %d, want 1: %q", len(lines), buf.String())
		}
		line := lines[0]
		wantBinary := filepath.Join(resolvedOutside, "bin", "zing")
		for _, want := range []string{
			"level=WARN",
			"upgrade: boot guard skipped: serve runs " + outsideExe,
			wantBinary,
		} {
			if !strings.Contains(line, want) {
				t.Errorf("log line %q missing %q", line, want)
			}
		}
	})
}

// countingExec is an execFunc fake that counts its calls and records the
// last argv0, so tests can assert "exactly one exec call" as well as which
// binary it named.
type countingExec struct {
	calls int
	argv0 string
}

func (ce *countingExec) exec(argv0 string, _, _ []string) error {
	ce.calls++
	ce.argv0 = argv0
	return nil
}

func TestBootAndServe_RollbackExecsBeforeConfig(t *testing.T) {
	resolved, exe := writeGuardBootBinary(t, t.TempDir(), "new", "old")
	marker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 5, State: markerAttempted}
	if err := saveUpgradeMarker(resolved, marker); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	dbPath := filepath.Join(resolved, "zing.db")
	cfgPath := filepath.Join(resolved, "does-not-exist.toml")
	su := &selfUpgrade{exe: exe, running: guardBootSHA[:12]}
	ce := &countingExec{}

	err := bootAndServe(t.Context(), cfgPath, dbPath, false, su, ce.exec)
	if err != nil {
		t.Fatalf("bootAndServe: %v", err)
	}
	if ce.calls != 1 {
		t.Errorf("exec calls = %d, want 1", ce.calls)
	}
	if ce.argv0 != exe {
		t.Errorf("exec argv0 = %q, want %q", ce.argv0, exe)
	}

	got, readErr := os.ReadFile(exe)
	if readErr != nil {
		t.Fatalf("read exe: %v", readErr)
	}
	if string(got) != "old" {
		t.Errorf("exe contents = %q, want %q", got, "old")
	}

	saved, found, loadErr := loadUpgradeMarker(resolved)
	if loadErr != nil || !found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, loadErr)
	}
	if saved.State != markerRolledBack {
		t.Errorf("marker state = %q, want %q", saved.State, markerRolledBack)
	}
}

func TestBootAndServe_GuardRunsBeforeConfig(t *testing.T) {
	resolved, exe := writeGuardBootBinary(t, t.TempDir(), "zing", "")
	marker := upgradeMarker{FromSHA: "from-sha", ToSHA: guardBootSHA, TicketID: 9, State: markerPending}
	if err := saveUpgradeMarker(resolved, marker); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	dbPath := filepath.Join(resolved, "zing.db")
	cfgPath := filepath.Join(resolved, "zing.toml")
	if err := os.WriteFile(cfgPath, []byte("not = [toml"), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
	su := &selfUpgrade{exe: exe, running: guardBootSHA[:12]}
	ce := &countingExec{}

	err := bootAndServe(t.Context(), cfgPath, dbPath, false, su, ce.exec)
	if err == nil {
		t.Fatalf("bootAndServe: got nil error, want config.Load's decode error")
	}
	if ce.calls != 0 {
		t.Errorf("exec calls = %d, want 0", ce.calls)
	}

	saved, found, loadErr := loadUpgradeMarker(resolved)
	if loadErr != nil || !found {
		t.Fatalf("loadUpgradeMarker: found=%v err=%v", found, loadErr)
	}
	if saved.State != markerAttempted {
		t.Errorf("marker state = %q, want %q (the guard ran before zing.toml was read)", saved.State, markerAttempted)
	}
}

func TestWatchBoot_Returns200(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ok := watchBoot(t.Context(), srv.URL, 10*time.Millisecond)
	if !ok {
		t.Fatalf("watchBoot returned false, want true")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("GETs = %d, want 3", got)
	}
}

func TestWatchBoot_StopsOnCancel(t *testing.T) {
	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	ok := watchBoot(ctx, srv.URL, 10*time.Millisecond)
	if ok {
		t.Fatalf("watchBoot returned true, want false")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second+50*time.Millisecond {
		t.Errorf("watchBoot took %s, want within 1s of the cancel", elapsed)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), buf.String())
	}
	line := lines[0]
	for _, want := range []string{"level=WARN", "upgrade: boot watch gave up", "last_status=503"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing %q", line, want)
		}
	}
	m := regexp.MustCompile(`polls=(\d+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("log line %q missing polls=N", line)
	}
	if n, convErr := strconv.Atoi(m[1]); convErr != nil || n < 1 {
		t.Errorf("polls = %q, want a number at least 1", m[1])
	}
}

// openUpgradeBootStore opens a fresh store under t.TempDir and closes it on
// cleanup, for closeUpgrade's tests.
func openUpgradeBootStore(t *testing.T) (st *store.Store, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func TestCloseUpgrade_BootedOK(t *testing.T) {
	t.Parallel()

	st, dir := openUpgradeBootStore(t)
	ticketID := seedTicketForUpgrade(t, st)

	m := upgradeMarker{
		FromSHA: "fedcba9876543210", ToSHA: "0123456789abcdef0123456789abcdef01234567",
		TicketID: ticketID, State: markerAttempted,
		HasNext: true, NextSHA: "deadbeef", NextTicketID: 99,
	}
	if err := saveUpgradeMarker(dir, m); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	var gotTicketID int64
	var gotSHA string
	var calls int
	request := func(ticketID int64, sha string) {
		calls++
		gotTicketID, gotSHA = ticketID, sha
	}

	closeUpgrade(t.Context(), st, dir, m, closeBootedOK, request)

	msgs, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ListMessages = %d messages, want 1", len(msgs))
	}
	wantBody := "booted_ok " + sha12(m.ToSHA)
	if msgs[0].Body != wantBody || msgs[0].Type != "update" || msgs[0].Author != "system" {
		t.Errorf("message = %+v, want body %q, type update, author system", msgs[0], wantBody)
	}

	if _, found, loadErr := loadUpgradeMarker(dir); loadErr != nil || found {
		t.Errorf("marker found=%v err=%v, want gone", found, loadErr)
	}

	if calls != 1 {
		t.Fatalf("request calls = %d, want 1", calls)
	}
	if gotTicketID != m.NextTicketID || gotSHA != m.NextSHA {
		t.Errorf("request called with (%d, %q), want (%d, %q)", gotTicketID, gotSHA, m.NextTicketID, m.NextSHA)
	}

	t.Run("ticket_zero", func(t *testing.T) {
		t.Parallel()

		st, dir := openUpgradeBootStore(t)
		m := upgradeMarker{FromSHA: "fedcba9876543210", ToSHA: "0123456789abcdef0123456789abcdef01234567"}
		if err := saveUpgradeMarker(dir, m); err != nil {
			t.Fatalf("saveUpgradeMarker: %v", err)
		}

		closeUpgrade(t.Context(), st, dir, m, closeBootedOK, nil)

		if _, found, loadErr := loadUpgradeMarker(dir); loadErr != nil || found {
			t.Errorf("marker found=%v err=%v, want gone", found, loadErr)
		}
	})
}

func TestCloseUpgrade_RolledBack(t *testing.T) {
	t.Parallel()

	st, dir := openUpgradeBootStore(t)
	ticketID := seedTicketForUpgrade(t, st)

	m := upgradeMarker{
		FromSHA: "fedcba9876543210fedcba9876543210fedcba9", ToSHA: "0123456789abcdef0123456789abcdef01234567",
		TicketID: ticketID, State: markerRolledBack,
		HasNext: true, NextSHA: "deadbeef", NextTicketID: 99,
	}
	if err := saveUpgradeMarker(dir, m); err != nil {
		t.Fatalf("saveUpgradeMarker: %v", err)
	}

	calls := 0
	request := func(int64, string) { calls++ }

	closeUpgrade(t.Context(), st, dir, m, markerRolledBack, request)

	msgs, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ListMessages = %d messages, want 1", len(msgs))
	}
	wantBody := "upgrade: " + sha12(m.ToSHA) + " rolled back to " + sha12(m.FromSHA) + ": it did not answer 200 within 60 s"
	if msgs[0].Body != wantBody || msgs[0].Type != "update" || msgs[0].Author != "system" {
		t.Errorf("message = %+v, want body %q, type update, author system", msgs[0], wantBody)
	}

	if _, found, loadErr := loadUpgradeMarker(dir); loadErr != nil || found {
		t.Errorf("marker found=%v err=%v, want gone", found, loadErr)
	}

	if calls != 0 {
		t.Errorf("request calls = %d, want 0", calls)
	}
}
