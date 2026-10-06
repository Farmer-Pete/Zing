package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// cappedUntilSettingValue reads the raw claude_hold_until setting value, "",
// false when absent, the fixture setup and assertion helper this file's
// tests share.
func cappedUntilSettingValue(t *testing.T, s *Store) (value string, ok bool) {
	t.Helper()
	value, ok, err := s.GetSetting(t.Context(), claudeHoldSettingKey)
	if err != nil {
		t.Fatalf("GetSetting(%s): %v", claudeHoldSettingKey, err)
	}
	return value, ok
}

// seedClaudeHold seeds the claude_hold_until setting directly (fixture
// setup, not the code under test).
func seedClaudeHold(t *testing.T, s *Store, until time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(),
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		claudeHoldSettingKey, formatTime(until),
	); err != nil {
		t.Fatalf("seed claude hold: %v", err)
	}
}

// reserveOpenRun claims nothing itself: it reserves one run on a fresh
// session for job, returning the run id, so a test can set up several open
// runs on the same ticket (ParkRuns sweeps every open run of every session).
func reserveOpenRun(t *testing.T, s *Store, ticketID int64, owner string, expires time.Time, job string) int64 {
	t.Helper()
	reserved, err := s.Reserve(t.Context(), ticketID, owner, expires,
		SessionUpsert{Job: job, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve(%s): %v", job, err)
	}
	return reserved.RunID
}

// parkedMarkerBodies returns every "parked until" update message body on
// ticketID, in message id order.
func parkedMarkerBodies(t *testing.T, s *Store, ticketID int64) []string {
	t.Helper()
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var bodies []string
	for i := range msgs {
		if msgs[i].Type == msgTypeUpdate && strings.HasPrefix(msgs[i].Body, cappedParkedPrefix) {
			bodies = append(bodies, msgs[i].Body)
		}
	}
	return bodies
}

// TestParkedMarkerBody proves parkedMarkerBody's own two formatting rules
// (design shape): a named zone's own Location().String() is used as-is, and
// time.Local's own synthetic "Local" name falls back to the zone
// abbreviation (until.Format("MST")) instead.
func TestParkedMarkerBody(t *testing.T) {
	t.Parallel()

	namedZone := time.Date(2026, 10, 5, 12, 20, 0, 0, time.FixedZone("America/New_York", -4*60*60))
	if got, want := parkedMarkerBody(namedZone, []int64{1625}), "parked until 12:20pm America/New_York (run 1625): Claude session limit"; got != want {
		t.Errorf("parkedMarkerBody(named zone) = %q, want %q", got, want)
	}

	local := time.Date(2026, 10, 5, 12, 20, 0, 0, time.Local)
	want := "parked until 12:20pm " + local.Format("MST") + " (run 1625): Claude session limit"
	if got := parkedMarkerBody(local, []int64{1625}); got != want {
		t.Errorf("parkedMarkerBody(time.Local) = %q, want %q", got, want)
	}

	if got, want := parkedMarkerBody(namedZone, []int64{1625, 1626}), "parked until 12:20pm America/New_York (runs 1625, 1626): Claude session limit"; got != want {
		t.Errorf("parkedMarkerBody(two runs) = %q, want %q", got, want)
	}
}

// TestParkRuns_StampsOpenRunsAndHold proves ParkRuns' main path (design
// shape, #45): every open run on the ticket is stamped interrupted with
// capped_until, the claim clears, a later stored hold is left alone (the
// hold only ever rises), and exactly one "parked until" marker names both
// runs ascending. A second park with a later until, and at least one fresh
// open run, then raises the hold.
func TestParkRuns_StampsOpenRunsAndHold(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	runA := reserveOpenRun(t, s, ticketID, owner, expires, testStatePlanning)
	runB := reserveOpenRun(t, s, ticketID, owner, expires, testStateBuilding)

	until := time.Date(2026, 10, 5, 12, 20, 0, 0, time.UTC)
	laterHold := until.Add(time.Hour)
	seedClaudeHold(t, s, laterHold)

	res, err := s.ParkRuns(ctx, ticketID, owner, expires, until, "")
	if err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}
	if !res.Applied {
		t.Fatal("ParkRuns: Applied = false, want true")
	}
	if len(res.RunIDs) != 2 || res.RunIDs[0] != runA || res.RunIDs[1] != runB {
		t.Errorf("ParkRuns.RunIDs = %v, want [%d %d] ascending", res.RunIDs, runA, runB)
	}

	for _, runID := range []int64{runA, runB} {
		run, ok, rerr := s.SessionNewestRun(ctx, sessionIDForRun(t, s, runID))
		if rerr != nil {
			t.Fatalf("SessionNewestRun: %v", rerr)
		}
		if !ok {
			t.Fatalf("SessionNewestRun for run %d: ok = false", runID)
		}
		if run.Outcome == nil || *run.Outcome != testOutcomeError {
			t.Errorf("run %d Outcome = %v, want error", runID, run.Outcome)
		}
		if !run.Interrupted {
			t.Errorf("run %d Interrupted = false, want true", runID)
		}
		if run.CappedUntil == nil || !run.CappedUntil.Equal(until) {
			t.Errorf("run %d CappedUntil = %v, want %v", runID, run.CappedUntil, until)
		}
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner != nil || got.ClaimExpiresAt != nil {
		t.Errorf("ticket claim = (%v, %v), want cleared", got.ClaimOwner, got.ClaimExpiresAt)
	}

	holdValue, ok := cappedUntilSettingValue(t, s)
	if !ok || holdValue != formatTime(laterHold) {
		t.Errorf("claude_hold_until = (%q, %v), want (%q, true) -- the stored hold is later, so it stays", holdValue, ok, formatTime(laterHold))
	}

	bodies := parkedMarkerBodies(t, s, ticketID)
	if len(bodies) != 1 {
		t.Fatalf("parked markers = %v, want exactly one", bodies)
	}
	if !strings.HasPrefix(bodies[0], cappedParkedPrefix) || !strings.Contains(bodies[0], "(runs "+strconv.FormatInt(runA, 10)+", "+strconv.FormatInt(runB, 10)+")") {
		t.Errorf("parked marker = %q, want it to start with %q and name (runs %d, %d)", bodies[0], cappedParkedPrefix, runA, runB)
	}

	// A second park, with a fresh open run and a later until, raises the hold.
	owner2, expires2 := claimForCommit(t, s, ticketID)
	runC := reserveOpenRun(t, s, ticketID, owner2, expires2, testStateJudging)
	until2 := laterHold.Add(time.Hour)

	res2, err := s.ParkRuns(ctx, ticketID, owner2, expires2, until2, "")
	if err != nil {
		t.Fatalf("second ParkRuns: %v", err)
	}
	if !res2.Applied || len(res2.RunIDs) != 1 || res2.RunIDs[0] != runC {
		t.Errorf("second ParkRuns = %+v, want Applied true, RunIDs [%d]", res2, runC)
	}

	holdValue2, ok2 := cappedUntilSettingValue(t, s)
	if !ok2 || holdValue2 != formatTime(until2) {
		t.Errorf("claude_hold_until after second park = (%q, %v), want (%q, true)", holdValue2, ok2, formatTime(until2))
	}
	if len(parkedMarkerBodies(t, s, ticketID)) != 2 {
		t.Errorf("parked markers after second park = %v, want exactly two", parkedMarkerBodies(t, s, ticketID))
	}
}

// TestParkRuns_FinishTerminalizesGoodRunsFirst proves ParkRuns' finish
// argument (design shape, owner decision Q6): a run named in finish is
// terminalized by its own real outcome and keeps capped_until nil, while a
// different open run on the same ticket is still swept as capped, and
// finish's own run id is excluded from ParkRuns.RunIDs.
func TestParkRuns_FinishTerminalizesGoodRunsFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStateReviewing)
	owner, expires := claimForCommit(t, s, ticketID)

	goodRun := reserveOpenRun(t, s, ticketID, owner, expires, testStateReviewing)
	cappedRun := reserveOpenRun(t, s, ticketID, owner, expires, testStateReviewing)

	outcome := "ok"
	exitCode := 0
	agentSeconds := 5
	until := time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)

	res, err := s.ParkRuns(ctx, ticketID, owner, expires, until, "", Run{
		ID: goodRun, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds,
	})
	if err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}
	if !res.Applied {
		t.Fatal("ParkRuns: Applied = false, want true")
	}
	if len(res.RunIDs) != 1 || res.RunIDs[0] != cappedRun {
		t.Errorf("ParkRuns.RunIDs = %v, want [%d] (the good run is finished, not swept)", res.RunIDs, cappedRun)
	}

	good, ok, err := s.SessionNewestRun(ctx, sessionIDForRun(t, s, goodRun))
	if err != nil {
		t.Fatalf("SessionNewestRun(good): %v", err)
	}
	if !ok {
		t.Fatal("SessionNewestRun(good): ok = false")
	}
	if good.Outcome == nil || *good.Outcome != "ok" {
		t.Errorf("good run Outcome = %v, want ok", good.Outcome)
	}
	if good.Interrupted {
		t.Error("good run Interrupted = true, want false")
	}
	if good.CappedUntil != nil {
		t.Errorf("good run CappedUntil = %v, want nil", good.CappedUntil)
	}

	capped, ok, err := s.SessionNewestRun(ctx, sessionIDForRun(t, s, cappedRun))
	if err != nil {
		t.Fatalf("SessionNewestRun(capped): %v", err)
	}
	if !ok {
		t.Fatal("SessionNewestRun(capped): ok = false")
	}
	if !capped.Interrupted || capped.CappedUntil == nil || !capped.CappedUntil.Equal(until) {
		t.Errorf("capped run = %+v, want interrupted with capped_until %v", capped, until)
	}
}

// TestParkRuns_FenceMissWritesNothing proves ParkRuns shares
// interruptClaimedRuns' own fence (design section 5.3): a wrong owner
// leaves the run, the claim, the settings row, and the messages untouched.
func TestParkRuns_FenceMissWritesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)
	runID := reserveOpenRun(t, s, ticketID, owner, expires, testStatePlanning)

	until := time.Now().Add(30 * time.Minute)
	res, err := s.ParkRuns(ctx, ticketID, testOwnerOther, expires, until, "")
	if err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}
	if res.Applied {
		t.Error("ParkRuns with the wrong owner: Applied = true, want false")
	}
	if len(res.RunIDs) != 0 {
		t.Errorf("ParkRuns with the wrong owner: RunIDs = %v, want empty", res.RunIDs)
	}

	run, ok, err := s.SessionNewestRun(ctx, sessionIDForRun(t, s, runID))
	if err != nil {
		t.Fatalf("SessionNewestRun: %v", err)
	}
	if !ok {
		t.Fatal("SessionNewestRun: ok = false, want true")
	}
	if run.Outcome != nil || run.Interrupted || run.CappedUntil != nil {
		t.Errorf("run = %+v, want untouched (nil outcome, not interrupted, no capped_until)", run)
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner == nil || *got.ClaimOwner != owner {
		t.Errorf("ticket claim owner = %v, want unchanged %s", got.ClaimOwner, owner)
	}

	if _, ok := cappedUntilSettingValue(t, s); ok {
		t.Error("claude_hold_until exists after a fence miss, want absent")
	}
	if len(parkedMarkerBodies(t, s, ticketID)) != 0 {
		t.Error("parked marker exists after a fence miss, want none")
	}
}

// TestParkRuns_NoOpenRunsOnlyReleasesClaim proves the empty-sweep path
// (design shape, the "HeldError with no open runs" edge case): a claimed
// ticket with no open runs still clears the claim and reports Applied true,
// but writes no marker and raises no hold, since nothing was actually
// parked.
func TestParkRuns_NoOpenRunsOnlyReleasesClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	until := time.Now().Add(30 * time.Minute)
	res, err := s.ParkRuns(ctx, ticketID, owner, expires, until, "")
	if err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}
	if !res.Applied {
		t.Fatal("ParkRuns: Applied = false, want true")
	}
	if len(res.RunIDs) != 0 {
		t.Errorf("ParkRuns.RunIDs = %v, want empty", res.RunIDs)
	}

	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner != nil || got.ClaimExpiresAt != nil {
		t.Errorf("ticket claim = (%v, %v), want cleared", got.ClaimOwner, got.ClaimExpiresAt)
	}

	if _, ok := cappedUntilSettingValue(t, s); ok {
		t.Error("claude_hold_until exists with no open runs, want absent")
	}
	if len(parkedMarkerBodies(t, s, ticketID)) != 0 {
		t.Error("parked marker exists with no open runs, want none")
	}
}

// TestClaudeHold_ReadsSetting proves ClaudeHold's own read (design shape):
// absent gives ok false, and after ParkRuns it returns the until instant
// (Equal), in time.Local.
func TestClaudeHold_ReadsSetting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	if _, ok, err := s.ClaudeHold(ctx); err != nil {
		t.Fatalf("ClaudeHold (absent): %v", err)
	} else if ok {
		t.Error("ClaudeHold (absent): ok = true, want false")
	}

	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)
	reserveOpenRun(t, s, ticketID, owner, expires, testStatePlanning)

	until := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
	if _, err := s.ParkRuns(ctx, ticketID, owner, expires, until, ""); err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}

	got, ok, err := s.ClaudeHold(ctx)
	if err != nil {
		t.Fatalf("ClaudeHold: %v", err)
	}
	if !ok {
		t.Fatal("ClaudeHold: ok = false, want true")
	}
	if !got.Equal(until) {
		t.Errorf("ClaudeHold = %v, want %v", got, until)
	}
	if got.Location() != time.Local {
		t.Errorf("ClaudeHold.Location() = %v, want time.Local", got.Location())
	}
}

// TestRecordCappedResume_OncePerPark proves RecordCappedResume's own
// ordering rule (design shape): the first call after a park writes the
// marker and returns true, the second returns false (already recorded for
// that park), a later park re-arms it, and no park at all returns false.
func TestRecordCappedResume_OncePerPark(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	if written, err := s.RecordCappedResume(ctx, ticketID, 999); err != nil {
		t.Fatalf("RecordCappedResume with no park: %v", err)
	} else if written {
		t.Error("RecordCappedResume with no park: written = true, want false")
	}

	owner, expires := claimForCommit(t, s, ticketID)
	runA := reserveOpenRun(t, s, ticketID, owner, expires, testStatePlanning)
	until := time.Now().Add(20 * time.Minute)
	if _, err := s.ParkRuns(ctx, ticketID, owner, expires, until, ""); err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}

	written, err := s.RecordCappedResume(ctx, ticketID, runA)
	if err != nil {
		t.Fatalf("RecordCappedResume (first after park): %v", err)
	}
	if !written {
		t.Error("RecordCappedResume (first after park): written = false, want true")
	}

	written, err = s.RecordCappedResume(ctx, ticketID, runA)
	if err != nil {
		t.Fatalf("RecordCappedResume (second after same park): %v", err)
	}
	if written {
		t.Error("RecordCappedResume (second after same park): written = true, want false")
	}

	owner2, expires2 := claimForCommit(t, s, ticketID)
	runB := reserveOpenRun(t, s, ticketID, owner2, expires2, testStateBuilding)
	if _, parkErr := s.ParkRuns(ctx, ticketID, owner2, expires2, time.Now().Add(20*time.Minute), ""); parkErr != nil {
		t.Fatalf("second ParkRuns: %v", parkErr)
	}

	written, err = s.RecordCappedResume(ctx, ticketID, runB)
	if err != nil {
		t.Fatalf("RecordCappedResume (after second park): %v", err)
	}
	if !written {
		t.Error("RecordCappedResume (after second park): written = false, want true")
	}
}

// sessionIDForRun reads a run's session id directly, a fixture-only lookup
// since this package's own read helpers index runs by session, not the
// other way around.
func sessionIDForRun(t *testing.T, s *Store, runID int64) int64 {
	t.Helper()
	var sessionID int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT session_id FROM runs WHERE id = ?`, runID).Scan(&sessionID); err != nil {
		t.Fatalf("session id for run %d: %v", runID, err)
	}
	return sessionID
}
