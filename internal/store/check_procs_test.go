package store

import (
	"errors"
	"testing"
	"time"
)

// Check-row fixture values shared by this file's tests.
const (
	testCheckKindTest = "test"
	testCheckKindLint = "lint"
	testCheckKindFix  = "fix"
	testCheckToken    = "77.000001"
	testForeignOwner  = "other-1"
)

// checkProcCount returns how many check_procs rows ticketID has.
func checkProcCount(t *testing.T, s *Store, ticketID int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM check_procs WHERE ticket_id = ?`, ticketID).Scan(&n); err != nil {
		t.Fatalf("count check_procs: %v", err)
	}
	return n
}

// claimedTicket seeds a queued ticket claimed by testForeignOwner until an
// hour from now.
func claimedTicket(t *testing.T, s *Store) (ticketID int64, expires time.Time) {
	t.Helper()
	_, ticketID = seedQueuedTicket(t, s, "1")
	expires = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if ok, err := s.Claim(t.Context(), ticketID, testForeignOwner, expires); err != nil || !ok {
		t.Fatalf("Claim = (%v, %v), want (true, nil)", ok, err)
	}
	return ticketID, expires
}

// TestRecordCheckStartAndClear proves a recorded CHECK command survives a
// clear naming another pgid and is gone after a clear naming its own.
func TestRecordCheckStartAndClear(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()

	gen, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 4242, testCheckToken, now, now)
	if err != nil {
		t.Fatalf("RecordCheckStart: %v", err)
	}
	if err := s.ClearCheckStart(ctx, ticketID, gen+1); err != nil {
		t.Fatalf("ClearCheckStart(wrong gen): %v", err)
	}
	if n := checkProcCount(t, s, ticketID); n != 1 {
		t.Fatalf("rows after a clear with the wrong gen = %d, want 1", n)
	}
	if err := s.ClearCheckStart(ctx, ticketID, gen); err != nil {
		t.Fatalf("ClearCheckStart: %v", err)
	}
	if n := checkProcCount(t, s, ticketID); n != 0 {
		t.Errorf("rows after a clear with the right gen = %d, want 0", n)
	}
}

// TestRecordCheckStartRecordsFix proves RecordCheckStart accepts kind fix
// (#131 added the fix command to CHECK, but migration 0006's CHECK
// constraint still refused anything but test and lint): the write succeeds,
// CheckProc reads the row back with Kind fix and the recorded pgid, and
// ClearCheckStart removes it.
func TestRecordCheckStartRecordsFix(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()

	gen, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindFix, 4242, testCheckToken, now, now)
	if err != nil {
		t.Fatalf("RecordCheckStart(fix): %v", err)
	}
	if gen <= 0 {
		t.Errorf("gen = %d, want > 0", gen)
	}
	c, ok, err := s.CheckProc(ctx, ticketID)
	if err != nil || !ok {
		t.Fatalf("CheckProc = (%+v, %v, %v), want a row", c, ok, err)
	}
	if c.Kind != testCheckKindFix || c.PGID != 4242 {
		t.Errorf("row = %+v, want kind fix, pgid 4242", c)
	}
	if err := s.ClearCheckStart(ctx, ticketID, gen); err != nil {
		t.Fatalf("ClearCheckStart: %v", err)
	}
	if n := checkProcCount(t, s, ticketID); n != 0 {
		t.Errorf("rows after clear = %d, want 0", n)
	}
}

// TestCheckClearsNeedTheRecordGeneration proves a replacement command with
// the same pgid and no start token is never cleared by a holder of the
// earlier record: pgid and a NULL proc_start
// cannot tell the two apart, so every clear and reclaim delete matches the
// record's own generation.
func TestCheckClearsNeedTheRecordGeneration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()

	first, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 4242, "", now, now)
	if err != nil {
		t.Fatalf("RecordCheckStart(first): %v", err)
	}
	seen, ok, err := s.CheckProc(ctx, ticketID)
	if err != nil || !ok || seen.Gen != first {
		t.Fatalf("CheckProc = (%+v, %v, %v), want the first record, gen %d", seen, ok, err, first)
	}
	second, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindLint, 4242, "", now, now)
	if err != nil {
		t.Fatalf("RecordCheckStart(replacement): %v", err)
	}
	if second == first {
		t.Fatalf("replacement gen = %d, want a new generation", second)
	}

	if err := s.ClearCheckStart(ctx, ticketID, first); err != nil {
		t.Fatalf("ClearCheckStart(first): %v", err)
	}
	if ok, err := s.ClearDeadCheck(ctx, ticketID, seen); err != nil || ok {
		t.Fatalf("ClearDeadCheck(first) = (%v, %v), want (false, nil)", ok, err)
	}
	if applied, err := s.ReclaimClaim(ctx, ticketID, testForeignOwner, expires, &seen); err != nil || applied {
		t.Fatalf("ReclaimClaim(first) = (%v, %v), want (false, nil)", applied, err)
	}
	if n := checkProcCount(t, s, ticketID); n != 1 {
		t.Fatalf("rows = %d, want the replacement kept", n)
	}
	if err := s.ClearCheckStart(ctx, ticketID, second); err != nil {
		t.Fatalf("ClearCheckStart(second): %v", err)
	}
	if n := checkProcCount(t, s, ticketID); n != 0 {
		t.Errorf("rows after clearing the replacement's own gen = %d, want 0", n)
	}
}

// TestRecordCheckStartReplaces proves the lint command's record replaces
// the test command's: one row per ticket.
func TestRecordCheckStartReplaces(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()

	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 4242, testCheckToken, now, now); err != nil {
		t.Fatalf("RecordCheckStart(test): %v", err)
	}
	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindLint, 4300, "", now, now); err != nil {
		t.Fatalf("RecordCheckStart(lint): %v", err)
	}
	if n := checkProcCount(t, s, ticketID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	c, ok, err := openCheckForTicket(ctx, s.db, ticketID)
	if err != nil || !ok {
		t.Fatalf("openCheckForTicket = (%v, %v)", ok, err)
	}
	if c.Kind != testCheckKindLint || c.PGID != 4300 || c.ProcStart != nil {
		t.Errorf("row = %+v, want the lint record with a NULL proc_start", c)
	}
}

// TestRecordCheckStartFencedOnClaim proves only the live claim's holder
// can record a CHECK command: a wrong owner or expiry
// writes nothing and returns ErrClaimLost, and a non-positive pgid is
// refused.
func TestRecordCheckStartFencedOnClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()

	if _, err := s.RecordCheckStart(ctx, ticketID, "someone-else", expires, testCheckKindTest, 4242, "", now, now); !errors.Is(err, ErrClaimLost) {
		t.Errorf("wrong owner: err = %v, want ErrClaimLost", err)
	}
	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires.Add(time.Second), testCheckKindTest, 4242, "", now, now); !errors.Is(err, ErrClaimLost) {
		t.Errorf("wrong expiry: err = %v, want ErrClaimLost", err)
	}
	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 0, "", now, now); err == nil {
		t.Error("pgid 0: want an error, got nil")
	}
	if n := checkProcCount(t, s, ticketID); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// TestForeignClaimsCarriesCheck proves ForeignClaims reports a claim's
// recorded CHECK command with every field parsed, and nil without one.
func TestForeignClaimsCarriesCheck(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)

	claims, err := s.ForeignClaims(ctx, "self")
	if err != nil || len(claims) != 1 {
		t.Fatalf("ForeignClaims = %+v, %v, want one claim", claims, err)
	}
	if claims[0].Check != nil {
		t.Errorf("Check = %+v without a row, want nil", claims[0].Check)
	}

	budgetStart := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	started := budgetStart.Add(3 * time.Minute)
	if _, err = s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindLint, 4242, testCheckToken, started, budgetStart); err != nil {
		t.Fatalf("RecordCheckStart: %v", err)
	}
	claims, err = s.ForeignClaims(ctx, "self")
	if err != nil || len(claims) != 1 || claims[0].Check == nil {
		t.Fatalf("ForeignClaims = %+v, %v, want one claim with a Check", claims, err)
	}
	c := claims[0].Check
	if c.Kind != testCheckKindLint || c.PGID != 4242 || c.ProcStart == nil || *c.ProcStart != testCheckToken ||
		!c.StartedAt.Equal(started) || !c.BudgetStartedAt.Equal(budgetStart) {
		t.Errorf("Check = %+v, want lint 4242 %s started %v budget %v", c, testCheckToken, started, budgetStart)
	}
	run := c.AsOpenRun()
	if run.Job != "build" || run.PGID == nil || *run.PGID != 4242 || run.StartedAt == nil || !run.StartedAt.Equal(budgetStart) {
		t.Errorf("AsOpenRun = %+v, want job build, pgid 4242, started at the budget start", run)
	}
}

// TestReclaimClaimDeletesCheckRow proves ReclaimClaim deletes the CHECK row
// it was given when the row still names that exact process, and that a
// failed fence leaves both the claim and the row.
func TestReclaimClaimDeletesCheckRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()
	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 4242, testCheckToken, now, now); err != nil {
		t.Fatalf("RecordCheckStart: %v", err)
	}
	claims, err := s.ForeignClaims(ctx, "self")
	if err != nil || len(claims) != 1 || claims[0].Check == nil {
		t.Fatalf("ForeignClaims = %+v, %v", claims, err)
	}
	seen := claims[0].Check

	applied, err := s.ReclaimClaim(ctx, ticketID, testForeignOwner, expires.Add(time.Second), seen)
	if err != nil || applied {
		t.Fatalf("ReclaimClaim with a stale fence = (%v, %v), want (false, nil)", applied, err)
	}
	if n := checkProcCount(t, s, ticketID); n != 1 {
		t.Fatalf("rows after a failed fence = %d, want 1", n)
	}

	applied, err = s.ReclaimClaim(ctx, ticketID, testForeignOwner, expires, seen)
	if err != nil || !applied {
		t.Fatalf("ReclaimClaim = (%v, %v), want (true, nil)", applied, err)
	}
	if n := checkProcCount(t, s, ticketID); n != 0 {
		t.Errorf("rows after the reclaim = %d, want 0", n)
	}
}

// TestReclaimClaimKeepsClaimWhenCheckRowReplaced proves the race between a
// read and a reclaim: the claim holder records a new CHECK command between
// the ForeignClaims read and ReclaimClaim, so the row no longer names the
// process reclaim judged gone. ReclaimClaim must keep the claim and the
// new row; a later pass judges the new process.
func TestReclaimClaimKeepsClaimWhenCheckRowReplaced(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()
	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 4242, testCheckToken, now, now); err != nil {
		t.Fatalf("RecordCheckStart(test): %v", err)
	}
	claims, err := s.ForeignClaims(ctx, "self")
	if err != nil || len(claims) != 1 || claims[0].Check == nil {
		t.Fatalf("ForeignClaims = %+v, %v", claims, err)
	}
	seen := claims[0].Check

	// The replacement: same pgid, different start token, and a new pgid.
	for _, replacement := range []struct {
		pgid  int
		token string
	}{{4242, "78.000002"}, {4300, testCheckToken}} {
		if _, recErr := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindLint, replacement.pgid, replacement.token, now, now); recErr != nil {
			t.Fatalf("RecordCheckStart(replacement): %v", recErr)
		}
		applied, reclaimErr := s.ReclaimClaim(ctx, ticketID, testForeignOwner, expires, seen)
		if reclaimErr != nil || applied {
			t.Fatalf("ReclaimClaim over replacement %+v = (%v, %v), want (false, nil)", replacement, applied, reclaimErr)
		}
		if n := checkProcCount(t, s, ticketID); n != 1 {
			t.Fatalf("rows = %d, want the replacement row kept", n)
		}
	}

	// A row that appears after a read that saw none is a replacement too.
	applied, err := s.ReclaimClaim(ctx, ticketID, testForeignOwner, expires, nil)
	if err != nil || applied {
		t.Fatalf("ReclaimClaim with no row seen but one present = (%v, %v), want (false, nil)", applied, err)
	}
	got, err := s.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner == nil || *got.ClaimOwner != testForeignOwner {
		t.Errorf("claim owner = %v, want the claim kept", got.ClaimOwner)
	}
}

// TestExpireClaimsKeepsClaimWithCheckRow proves ordinary expiry never
// clears a claim whose ticket still records a CHECK command:
// ExpiringChecks reports it for the dispatcher to judge, and
// only once ClearDeadCheck removes that exact row does ExpireClaims clear
// the claim.
func TestExpireClaimsKeepsClaimWithCheckRow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ticketID, expires := claimedTicket(t, s)
	now := time.Now()
	if _, err := s.RecordCheckStart(ctx, ticketID, testForeignOwner, expires, testCheckKindTest, 4242, testCheckToken, now, now); err != nil {
		t.Fatalf("RecordCheckStart: %v", err)
	}
	later := expires.Add(time.Minute)

	checks, err := s.ExpiringChecks(ctx, later, "")
	if err != nil || len(checks) != 1 || checks[0].TicketID != ticketID || checks[0].Check.PGID != 4242 {
		t.Fatalf("ExpiringChecks = %+v, %v, want the one recorded command", checks, err)
	}
	if ids, err := s.ExpireClaims(ctx, later, ""); err != nil || len(ids) != 0 {
		t.Fatalf("ExpireClaims with a CHECK row = %v, %v, want nothing cleared", ids, err)
	}

	stale := checks[0].Check
	stale.Gen++
	if ok, err := s.ClearDeadCheck(ctx, ticketID, stale); err != nil || ok {
		t.Fatalf("ClearDeadCheck with another identity = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.ClearDeadCheck(ctx, ticketID, checks[0].Check); err != nil || !ok {
		t.Fatalf("ClearDeadCheck = (%v, %v), want (true, nil)", ok, err)
	}
	if ids, err := s.ExpireClaims(ctx, later, ""); err != nil || len(ids) != 1 || ids[0] != ticketID {
		t.Errorf("ExpireClaims after the row is cleared = %v, %v, want [%d]", ids, err, ticketID)
	}
}
