package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/store"
)

// --- #81: live tuning (max_parallel, interval, agent budget) ---------------

// TestValidateTuning proves ValidateTuning accepts a value at either bound
// and refuses one just outside it, with the exact message the console
// shows the owner (owner decision Q2), and refuses an unknown name.
func TestValidateTuning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value int
		want  string // the setting's Key on success, or "" if err is wanted
		err   string // non-empty: the exact TuningError message wanted
	}{
		{name: dispatch.TuneMaxParallel, value: 1, want: "dispatch.max_parallel"},
		{name: dispatch.TuneMaxParallel, value: 64, want: "dispatch.max_parallel"},
		{name: dispatch.TuneMaxParallel, value: 0, err: "max_parallel must be 1 to 64"},
		{name: dispatch.TuneMaxParallel, value: 65, err: "max_parallel must be 1 to 64"},
		{name: dispatch.TuneIntervalSeconds, value: 1, want: "dispatch.interval_seconds"},
		{name: dispatch.TuneIntervalSeconds, value: 86400, want: "dispatch.interval_seconds"},
		{name: dispatch.TuneIntervalSeconds, value: 0, err: "interval_seconds must be 1 to 86400"},
		{name: dispatch.TuneIntervalSeconds, value: 86401, err: "interval_seconds must be 1 to 86400"},
		{name: dispatch.TuneAgentMinutes, value: 1, want: "budget.agent_minutes_per_ticket"},
		{name: dispatch.TuneAgentMinutes, value: 525600, want: "budget.agent_minutes_per_ticket"},
		{name: dispatch.TuneAgentMinutes, value: 0, err: "agent_minutes_per_ticket must be 1 to 525600"},
		{name: dispatch.TuneAgentMinutes, value: 525601, err: "agent_minutes_per_ticket must be 1 to 525600"},
		{name: "foo", value: 1, err: `unknown setting "foo"`},
	}

	for _, c := range cases {
		s, err := dispatch.ValidateTuning(c.name, c.value)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("ValidateTuning(%q, %d): err = %v, want %q", c.name, c.value, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateTuning(%q, %d): err = %v, want nil", c.name, c.value, err)
			continue
		}
		if s.Key != c.want {
			t.Errorf("ValidateTuning(%q, %d).Key = %q, want %q", c.name, c.value, s.Key, c.want)
		}
	}
}

// TestSetTuning_NextFillUsesNewMaxParallel proves a SetTuning call that
// raises max_parallel is visible to the very next fill pass, without a
// restart (#81): three queued tickets on a blocking handler, MaxParallel
// starting at 1, SetTuning raises it to 2 before Tick ever runs, and Tick
// launches exactly 2 of the 3 tickets concurrently.
func TestSetTuning_NextFillUsesNewMaxParallel(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	seedQueuedTicket(t, s, "fake#1")
	seedQueuedTicket(t, s, "fake#2")
	seedQueuedTicket(t, s, "fake#3")

	started := make(chan int64, 3)
	release := make(chan struct{})
	reg := job.Registry()
	reg[testStateQueued] = &barrierHandler{started: started, release: release}

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner})

	if err := d.SetTuning(t.Context(), dispatch.TuneMaxParallel, 2, "peter"); err != nil {
		t.Fatalf("SetTuning: %v", err)
	}

	tickErrCh := make(chan error, 1)
	go func() { tickErrCh <- d.Tick(t.Context()) }()

	waitFor(t, started, "first handler to start")
	waitFor(t, started, "second handler to start")

	select {
	case id := <-started:
		t.Fatalf("a third handler started (id %d): want only 2 concurrent, the new max_parallel", id)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := waitFor(t, tickErrCh, "Tick to return"); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// TestSetTuning_NextDepsCarriesNewBudget proves a SetTuning call that
// changes the agent budget reaches the very next job.Deps runAndCommit
// builds (#81), without a restart.
func TestSetTuning_NextDepsCarriesNewBudget(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	ticketID := seedQueuedTicket(t, s, testFixtureRef)

	seedOwner := testSeedJudgingOwner
	seedExpires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, seedOwner, seedExpires)
	if err != nil || !claimed {
		t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: seedOwner, Expires: seedExpires, Next: testStateJudging, Reason: testSeedReason,
	})
	if err != nil || !applied {
		t.Fatalf("seed commit: applied=%v err=%v", applied, err)
	}

	spy := &spyHandler{next: testStateShipping, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateJudging] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{MaxParallel: 2, Owner: testOwner, Budget: testBudget})

	if err := d.SetTuning(t.Context(), dispatch.TuneAgentMinutes, 480, "peter"); err != nil {
		t.Fatalf("SetTuning: %v", err)
	}

	if err := d.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spy.Calls() != 1 {
		t.Fatalf("spy.Calls() = %d, want 1", spy.Calls())
	}
	if want := 480 * time.Minute; spy.Budget() != want {
		t.Errorf("Deps.Budget = %v, want %v", spy.Budget(), want)
	}
}

// TestSetTuning_WritesValueWhoAndWhen proves SetTuning writes the value,
// the changer, and the current time (from Config.Now) under the setting's
// key and its two provenance keys (owner decision Q3), in the settings
// table.
func TestSetTuning_WritesValueWhoAndWhen(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	fixed := time.Date(2026, 10, 6, 14, 3, 0, 0, time.UTC)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner, Now: func() time.Time { return fixed }})

	if err := d.SetTuning(t.Context(), dispatch.TuneMaxParallel, 3, "peter"); err != nil {
		t.Fatalf("SetTuning: %v", err)
	}

	value, ok, err := s.GetSetting(t.Context(), "dispatch.max_parallel")
	if err != nil || !ok || value != "3" {
		t.Errorf("dispatch.max_parallel = %q, ok=%v, err=%v, want %q, true, nil", value, ok, err, "3")
	}
	by, ok, err := s.GetSetting(t.Context(), "dispatch.max_parallel.changed_by")
	if err != nil || !ok || by != "peter" {
		t.Errorf("dispatch.max_parallel.changed_by = %q, ok=%v, err=%v, want %q, true, nil", by, ok, err, "peter")
	}
	at, ok, err := s.GetSetting(t.Context(), "dispatch.max_parallel.changed_at")
	if err != nil || !ok || at != "2026-10-06T14:03:00Z" {
		t.Errorf("dispatch.max_parallel.changed_at = %q, ok=%v, err=%v, want %q, true, nil", at, ok, err, "2026-10-06T14:03:00Z")
	}
}

// TestSetTuning_RejectsOutOfRangeAndWritesNothing proves an out-of-range
// SetTuning call returns a *TuningError and writes nothing to the settings
// table: the value, the changer, and the time keys all stay unset.
func TestSetTuning_RejectsOutOfRangeAndWritesNothing(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner})

	for _, value := range []int{0, 65} {
		err := d.SetTuning(t.Context(), dispatch.TuneMaxParallel, value, "peter")
		if te, _ := errors.AsType[*dispatch.TuningError](err); te == nil {
			t.Fatalf("SetTuning(max_parallel, %d): err = %v, want a *dispatch.TuningError", value, err)
		}
	}

	for _, key := range []string{"dispatch.max_parallel", "dispatch.max_parallel.changed_by", "dispatch.max_parallel.changed_at"} {
		_, ok, err := s.GetSetting(t.Context(), key)
		if err != nil {
			t.Fatalf("GetSetting(%q): %v", key, err)
		}
		if ok {
			t.Errorf("GetSetting(%q): ok = true, want false (a refused SetTuning must write nothing)", key)
		}
	}
}

// TestSetTuning_StoreErrorLeavesTuningUnchanged proves that when the
// settings-table write fails, SetTuning returns that error untouched (not
// a *dispatch.TuningError) and leaves d.tune exactly as it started (#81):
// the store write happens before d.tune is ever touched.
func TestSetTuning_StoreErrorLeavesTuningUnchanged(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), nil, nil,
		dispatch.Config{MaxParallel: 1, Owner: testOwner})
	startTuning := d.CurrentTuning()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := d.SetTuning(t.Context(), dispatch.TuneMaxParallel, 2, "peter")
	if err == nil {
		t.Fatal("SetTuning after Close: err = nil, want an error")
	}
	if te, _ := errors.AsType[*dispatch.TuningError](err); te != nil {
		t.Fatalf("SetTuning after Close: err = %v (a *dispatch.TuningError), want a plain store error", err)
	}
	if got := d.CurrentTuning(); got != startTuning {
		t.Errorf("CurrentTuning() = %+v after a store error, want unchanged %+v", got, startTuning)
	}
}

// TestRun_IntervalChangeResetsTicker proves a SetTuning call that changes
// interval_seconds wakes a running Run and resets its ticker at once (#81),
// rather than leaving the new interval to apply only once the old, much
// longer interval would next have fired.
func TestRun_IntervalChangeResetsTicker(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	seedQueuedTicket(t, s, testFixtureRef)

	spy := &spyHandler{next: testStatePlanning, reason: testSpyReason}
	reg := job.Registry()
	reg[testStateQueued] = spy

	d := newDispatcher(t, s, newFixtureTracker(t), bus.New(), fakeRuntime(t), reg, nil,
		dispatch.Config{Interval: time.Hour, MaxParallel: 1, Owner: testOwner})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- d.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	if got := spy.Calls(); got != 0 {
		t.Fatalf("spy.Calls() = %d after 100ms with a 1h interval, want 0", got)
	}

	if err := d.SetTuning(t.Context(), dispatch.TuneIntervalSeconds, 1, "peter"); err != nil {
		t.Fatalf("SetTuning: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for spy.Calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("spy.Calls() stayed 0 for 5s after SetTuning(interval_seconds, 1)")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if want := time.Second; d.CurrentTuning().Interval != want {
		t.Errorf("CurrentTuning().Interval = %v, want %v", d.CurrentTuning().Interval, want)
	}

	cancel()
	if err := waitFor(t, runErrCh, "Run to return after ctx cancel"); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
}

// TestLoadTuning_StoredValuesWinOverBase proves a valid stored value for
// every setting wins over base, the zing.toml-derived startup values
// (#81), and that sources names store for each one; and that when only
// one of the three is stored, sources names store for it and zing.toml
// for the other two.
func TestLoadTuning_StoredValuesWinOverBase(t *testing.T) {
	t.Parallel()

	base := dispatch.Tuning{MaxParallel: 2, Interval: 30 * time.Second, Budget: 240 * time.Minute}

	t.Run("all three stored", func(t *testing.T) {
		t.Parallel()
		s := newDispatchTestStore(t)
		if err := s.SetSettings(t.Context(),
			"dispatch.max_parallel", "3",
			"dispatch.interval_seconds", "10",
			"budget.agent_minutes_per_ticket", "480"); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}

		got, sources, err := dispatch.LoadTuning(t.Context(), s, base)
		if err != nil {
			t.Fatalf("LoadTuning: %v", err)
		}
		want := dispatch.Tuning{MaxParallel: 3, Interval: 10 * time.Second, Budget: 480 * time.Minute}
		if got != want {
			t.Errorf("LoadTuning() = %+v, want %+v", got, want)
		}
		for _, name := range []string{dispatch.TuneMaxParallel, dispatch.TuneIntervalSeconds, dispatch.TuneAgentMinutes} {
			if sources[name] != dispatch.TuningSourceStore {
				t.Errorf("sources[%q] = %q, want %q", name, sources[name], dispatch.TuningSourceStore)
			}
		}
	})

	t.Run("only max_parallel stored", func(t *testing.T) {
		t.Parallel()
		s := newDispatchTestStore(t)
		if err := s.SetSettings(t.Context(), "dispatch.max_parallel", "3"); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}

		got, sources, err := dispatch.LoadTuning(t.Context(), s, base)
		if err != nil {
			t.Fatalf("LoadTuning: %v", err)
		}
		want := dispatch.Tuning{MaxParallel: 3, Interval: base.Interval, Budget: base.Budget}
		if got != want {
			t.Errorf("LoadTuning() = %+v, want %+v", got, want)
		}
		if sources[dispatch.TuneMaxParallel] != dispatch.TuningSourceStore {
			t.Errorf("sources[max_parallel] = %q, want %q", sources[dispatch.TuneMaxParallel], dispatch.TuningSourceStore)
		}
		if sources[dispatch.TuneIntervalSeconds] != dispatch.TuningSourceToml {
			t.Errorf("sources[interval_seconds] = %q, want %q", sources[dispatch.TuneIntervalSeconds], dispatch.TuningSourceToml)
		}
		if sources[dispatch.TuneAgentMinutes] != dispatch.TuningSourceToml {
			t.Errorf("sources[agent_minutes_per_ticket] = %q, want %q", sources[dispatch.TuneAgentMinutes], dispatch.TuningSourceToml)
		}
	})
}

// TestLoadTuning_UnsetOrInvalidKeepsBase proves an unset, empty, or
// invalid stored value keeps base's own value for that setting and is
// reported as coming from zing.toml, and that an invalid stored value
// logs one warning naming the key and the stored text (#81). Not
// t.Parallel at the top level: the invalid-value subtests swap slog's
// process-wide default, which t.Parallel forbids for the whole ancestor
// chain. Its other subtests, which touch no global state, are parallel on
// their own.
func TestLoadTuning_UnsetOrInvalidKeepsBase(t *testing.T) { //nolint:tparallel // the invalid-value subtests swap slog's process-wide default, so the parent itself cannot call Parallel
	base := dispatch.Tuning{MaxParallel: 2, Interval: 30 * time.Second, Budget: 240 * time.Minute}

	t.Run("nothing stored", func(t *testing.T) {
		t.Parallel()
		s := newDispatchTestStore(t)

		got, sources, err := dispatch.LoadTuning(t.Context(), s, base)
		if err != nil {
			t.Fatalf("LoadTuning: %v", err)
		}
		if got != base {
			t.Errorf("LoadTuning() = %+v, want base %+v", got, base)
		}
		for _, name := range []string{dispatch.TuneMaxParallel, dispatch.TuneIntervalSeconds, dispatch.TuneAgentMinutes} {
			if sources[name] != dispatch.TuningSourceToml {
				t.Errorf("sources[%q] = %q, want %q", name, sources[name], dispatch.TuningSourceToml)
			}
		}
	})

	for _, stored := range []string{"abc", "0"} {
		t.Run("invalid dispatch.max_parallel "+stored, func(t *testing.T) {
			s := newDispatchTestStore(t)
			if err := s.SetSettings(t.Context(), "dispatch.max_parallel", stored); err != nil {
				t.Fatalf("SetSettings: %v", err)
			}

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got, sources, err := dispatch.LoadTuning(t.Context(), s, base)
			if err != nil {
				t.Fatalf("LoadTuning: %v", err)
			}
			if got.MaxParallel != base.MaxParallel {
				t.Errorf("MaxParallel = %d, want base %d", got.MaxParallel, base.MaxParallel)
			}
			if sources[dispatch.TuneMaxParallel] != dispatch.TuningSourceToml {
				t.Errorf("sources[max_parallel] = %q, want %q", sources[dispatch.TuneMaxParallel], dispatch.TuningSourceToml)
			}

			log := buf.String()
			if !strings.Contains(log, "key=dispatch.max_parallel") || !strings.Contains(log, "stored="+stored) {
				t.Errorf("log = %q, want one level=WARN record with key=dispatch.max_parallel stored=%s", log, stored)
			}
			if got := strings.Count(log, "level=WARN"); got != 1 {
				t.Errorf("level=WARN count = %d, want exactly 1; log = %q", got, log)
			}
		})
	}

	t.Run("empty dispatch.interval_seconds", func(t *testing.T) {
		t.Parallel()
		s := newDispatchTestStore(t)
		if err := s.SetSettings(t.Context(), "dispatch.interval_seconds", ""); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}

		got, sources, err := dispatch.LoadTuning(t.Context(), s, base)
		if err != nil {
			t.Fatalf("LoadTuning: %v", err)
		}
		if got.Interval != base.Interval {
			t.Errorf("Interval = %v, want base %v", got.Interval, base.Interval)
		}
		if sources[dispatch.TuneIntervalSeconds] != dispatch.TuningSourceToml {
			t.Errorf("sources[interval_seconds] = %q, want %q", sources[dispatch.TuneIntervalSeconds], dispatch.TuningSourceToml)
		}
	})
}
