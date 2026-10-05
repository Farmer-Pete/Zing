package job

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// fixedTimeLayout mirrors store's own formatTime layout (unexported there):
// a fixed-width UTC timestamp these tests seed the claude_hold_until
// setting with directly, through the exported SetSettings, since this file
// lives in package job and cannot reach store's own test-only seed helper.
const sessionLimitTestTimeLayout = "2006-01-02T15:04:05Z"

// seedClaudeHoldSetting seeds the claude_hold_until setting directly
// through store's exported SetSettings (fixture setup, not the code under
// test).
func seedClaudeHoldSetting(t *testing.T, s *store.Store, until time.Time) {
	t.Helper()
	if err := s.SetSettings(t.Context(), "claude_hold_until", until.UTC().Truncate(time.Second).Format(sessionLimitTestTimeLayout)); err != nil {
		t.Fatalf("SetSettings(claude_hold_until): %v", err)
	}
}

// --- runJob's hold gate --------------------------------------------------

// TestRunJob_ClaudeHoldRefusesClaudeBeforeReserve proves the hold gate
// (design shape, "Hold"): with claude_hold_until 10 minutes after Deps.Now,
// runJob for planning (runtime claude) returns an error CappedUntil
// recognizes, errors.As finds a *HeldError inside it, and nothing was
// reserved.
func TestRunJob_ClaudeHoldRefusesClaudeBeforeReserve(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	now := time.Now()
	until := now.Add(10 * time.Minute).UTC().Truncate(time.Second)
	seedClaudeHoldSetting(t, s, until)

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	rec := &recordingReserve{fn: realReserve(s, owner, expires)}
	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAliasOpus: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: rec.Reserve,
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	_, err = runJob(t.Context(), d, ticket, jobPlanningName, store.SessionUpsert{Job: jobPlanningName, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobPlanning}, nil, nil, 0)
	if err == nil {
		t.Fatal("runJob: err = nil, want a HeldError")
	}
	resetAt, capped := CappedUntil(err)
	if !capped {
		t.Fatalf("CappedUntil(%v) = (_, false), want true", err)
	}
	if !resetAt.Equal(until) {
		t.Errorf("CappedUntil reset = %v, want %v", resetAt, until)
	}
	var held *HeldError
	if !errors.As(err, &held) { //nolint:modernize // see errKind's own comment
		t.Fatalf("errors.As(err, &HeldError) = false for err %v", err)
	}
	if rec.calls != 0 {
		t.Errorf("Reserve calls = %d, want 0", rec.calls)
	}
}

// TestRunJob_ClaudeHoldLetsCodexRun proves the hold gate only refuses a
// claude-runtime job (design shape, "Hold"): with the same hold, runJob for
// planreview (runtime codex) reserves a run.
func TestRunJob_ClaudeHoldLetsCodexRun(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	before, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket (before): %v", err)
	}

	now := time.Now()
	seedClaudeHoldSetting(t, s, now.Add(10*time.Minute))

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t),
		Models: map[string]string{testModelAlias: testModelExact, testRuntimeCodex: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	const jobPlanreviewName = string(response.JobPlanreview)
	_, err = runJob(t.Context(), d, ticket, jobPlanreviewName, store.SessionUpsert{Job: jobPlanreviewName, Runtime: testRuntimeCodex},
		runtime.RunRequest{Job: response.JobPlanreview}, nil, nil, 0)
	var held *HeldError
	if errors.As(err, &held) { //nolint:modernize // see errKind's own comment
		t.Fatalf("runJob(planreview): err = %v, want it not to be a HeldError", err)
	}

	after, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket (after): %v", err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("RunsForTicket count = %d, want %d (grew by 1)", len(after), len(before)+1)
	}
}

// TestRunJob_ClaudeHoldPastLetsClaudeRun proves a hold already in the past
// is no hold at all (design shape, "Hold"): with claude_hold_until 1 minute
// before Deps.Now, runJob for planning reserves a run.
func TestRunJob_ClaudeHoldPastLetsClaudeRun(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)
	owner, expires := claimRunJobTicket(t, s, ticketID)

	before, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket (before): %v", err)
	}

	now := time.Now()
	seedClaudeHoldSetting(t, s, now.Add(-time.Minute))

	fake := runtime.NewFake(fstest.MapFS{})
	set, err := runtime.NewSet(map[string]runtime.Runtime{testRuntimeClaude: fake, testRuntimeCodex: fake, runtimeFake: fake})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}

	d := Deps{
		Store: s, Runtimes: set, Machine: runJobTestMachine(t), Models: map[string]string{testModelAliasOpus: testModelExact},
		Budget: time.Hour, Owner: owner, Expires: expires, Reserve: realReserve(s, owner, expires),
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	_, err = runJob(t.Context(), d, ticket, jobPlanningName, store.SessionUpsert{Job: jobPlanningName, Runtime: testRuntimeClaude},
		runtime.RunRequest{Job: response.JobPlanning}, nil, nil, 0)
	var held *HeldError
	if errors.As(err, &held) { //nolint:modernize // see errKind's own comment
		t.Fatalf("runJob(planning): err = %v, want it not to be a HeldError", err)
	}

	after, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket (after): %v", err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("RunsForTicket count = %d, want %d (grew by 1)", len(after), len(before)+1)
	}
}

// --- routeFailure's passthrough ------------------------------------------

// TestRouteFailure_PassesCappedThrough proves routeFailure treats a capped
// error exactly as it already treats runtime.ErrCanceled (design shape,
// "shape" diagram's routeFailure box): an empty commit, ok true, and the
// same error back, for a wrapped SessionLimitError and for a HeldError.
func TestRouteFailure_PassesCappedThrough(t *testing.T) {
	t.Parallel()
	ticket := store.Ticket{ID: 1}
	d := Deps{}
	rr := runResult{}

	sessionLimitErr := fmt.Errorf("wrap: %w", &runtime.SessionLimitError{ResetAt: time.Now().Add(time.Hour), Parsed: true})
	commit, ok, err := routeFailure(ticket, d, rr, sessionLimitErr, 0, nil, nil, response.EscalationOriginClassify)
	if !ok {
		t.Fatal("routeFailure(wrapped SessionLimitError): ok = false, want true")
	}
	if !errors.Is(err, sessionLimitErr) {
		t.Errorf("routeFailure(wrapped SessionLimitError): err = %v, want %v", err, sessionLimitErr)
	}
	if !reflect.DeepEqual(commit, store.HandlerCommit{}) {
		t.Errorf("routeFailure(wrapped SessionLimitError): commit = %+v, want the zero value", commit)
	}

	heldErr := &HeldError{Until: time.Now().Add(10 * time.Minute)}
	commit2, ok2, err2 := routeFailure(ticket, d, rr, heldErr, 0, nil, nil, response.EscalationOriginClassify)
	if !ok2 {
		t.Fatal("routeFailure(HeldError): ok = false, want true")
	}
	if !errors.Is(err2, heldErr) {
		t.Errorf("routeFailure(HeldError): err = %v, want %v", err2, heldErr)
	}
	if !reflect.DeepEqual(commit2, store.HandlerCommit{}) {
		t.Errorf("routeFailure(HeldError): commit = %+v, want the zero value", commit2)
	}
}

// --- CappedUntil -----------------------------------------------------------

// TestCappedUntil proves CappedUntil's own classification (design shape,
// "CappedUntil"): a %w-wrapped SessionLimitError gives its ResetAt, a
// HeldError gives its Until, and neither an ExecError, runtime.ErrCanceled,
// nor nil is capped.
func TestCappedUntil(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 10, 5, 16, 20, 0, 0, time.UTC)
	wrapped := fmt.Errorf("wrap: %w", &runtime.SessionLimitError{ResetAt: resetAt, Parsed: true})
	got, ok := CappedUntil(wrapped)
	if !ok || !got.Equal(resetAt) {
		t.Errorf("CappedUntil(wrapped SessionLimitError) = (%v, %v), want (%v, true)", got, ok, resetAt)
	}

	until := time.Date(2026, 10, 5, 17, 0, 0, 0, time.UTC)
	got2, ok2 := CappedUntil(&HeldError{Until: until})
	if !ok2 || !got2.Equal(until) {
		t.Errorf("CappedUntil(HeldError) = (%v, %v), want (%v, true)", got2, ok2, until)
	}

	for _, err := range []error{&runtime.ExecError{ExitCode: 1}, runtime.ErrCanceled, nil} {
		if _, ok := CappedUntil(err); ok {
			t.Errorf("CappedUntil(%v) = (_, true), want false", err)
		}
	}
}
