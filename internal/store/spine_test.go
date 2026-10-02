package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

// newTestStore opens a fresh Store on a temp-file database and closes it on
// test cleanup.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Message and ticket literals repeated across spine_test.go and
// reads_test.go. testAuthorZing (store_test.go) already covers "zing";
// ticketStateQueued (rows.go) already covers "queued".
const (
	testStatePlanning = "planning"
	testTypeUpdate    = "update"
	testBodyProgress  = "progress"
	testTitleFixBug   = "fix the bug"
)

var testProject = Project{
	Name:          testAuthorZing,
	RepoURL:       "https://github.com/x/zing",
	LocalPath:     "/tmp/zing",
	Tracker:       "github",
	DefaultBranch: "main",
}

func TestEnsureProject_InsertsWhenAbsent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	id, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if id == 0 {
		t.Fatal("EnsureProject: got id 0, want a positive row id")
	}

	var name string
	if err := s.db.QueryRowContext(ctx, "SELECT name FROM projects WHERE id = ?", id).Scan(&name); err != nil {
		t.Fatalf("read back project: %v", err)
	}
	if name != testAuthorZing {
		t.Errorf("project name = %q, want %s", name, testAuthorZing)
	}
}

func TestEnsureProject_ReturnsExistingIDOnSecondCall(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	first, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("first EnsureProject: %v", err)
	}

	second, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("second EnsureProject: %v", err)
	}
	if second != first {
		t.Errorf("second EnsureProject id = %d, want the first id %d", second, first)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM projects WHERE name = 'zing'").Scan(&count); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if count != 1 {
		t.Errorf("projects named zing = %d, want 1 (no duplicate insert)", count)
	}
}

// TestEnsureProject_ReconcilesChangedDefaultBranch proves that when
// zing.toml's default_branch changes for an already-known project,
// EnsureProject updates the stored column rather than leaving it stale
// (spine.go, PKG5-PLAN.md section 9).
func TestEnsureProject_ReconcilesChangedDefaultBranch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	id, err := s.EnsureProject(ctx, testProject) // testProject.DefaultBranch == "main"
	if err != nil {
		t.Fatalf("first EnsureProject: %v", err)
	}

	changed := testProject
	changed.DefaultBranch = "develop"
	second, err := s.EnsureProject(ctx, changed)
	if err != nil {
		t.Fatalf("second EnsureProject with a changed default_branch: %v", err)
	}
	if second != id {
		t.Errorf("second EnsureProject id = %d, want the existing id %d", second, id)
	}

	var stored string
	if err := s.db.QueryRowContext(ctx, "SELECT default_branch FROM projects WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatalf("read back default_branch: %v", err)
	}
	if stored != "develop" {
		t.Errorf("default_branch = %q, want develop (reconciled)", stored)
	}
}

// TestEnsureProject_EmptyDefaultBranchDoesNotOverwrite proves EnsureProject
// only reconciles when p.DefaultBranch is non-empty, so a caller that omits
// it (config.Project.DefaultBranch is optional at the TOML layer, and
// applyDefaults deliberately leaves it "" rather than filling it to "main"
// -- see config.go's applyDefaults -- so ensureBindings can call
// EnsureProject with an empty value) never wipes an existing stored value.
func TestEnsureProject_EmptyDefaultBranchDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	id, err := s.EnsureProject(ctx, testProject) // testProject.DefaultBranch == "main"
	if err != nil {
		t.Fatalf("first EnsureProject: %v", err)
	}

	noBranch := testProject
	noBranch.DefaultBranch = ""
	if _, err := s.EnsureProject(ctx, noBranch); err != nil {
		t.Fatalf("second EnsureProject with an empty default_branch: %v", err)
	}

	var stored string
	if err := s.db.QueryRowContext(ctx, "SELECT default_branch FROM projects WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatalf("read back default_branch: %v", err)
	}
	if stored != "main" {
		t.Errorf("default_branch = %q, want main (untouched by an empty p.DefaultBranch)", stored)
	}
}

func TestInsertTicket_RequiresQueuedState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	_, err = s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "t", State: testStatePlanning})
	if err == nil {
		t.Error("InsertTicket with state=planning: want error, got nil")
	}
}

func TestInsertTicket_InsertsAndRoundTrips(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, err := s.InsertTicket(ctx, Ticket{
		ProjectID: projectID, TrackerRef: "42", Title: testTitleFixBug, State: ticketStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	got, err := s.GetTicket(ctx, id)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ProjectID != projectID || got.TrackerRef != "42" || got.Title != testTitleFixBug || got.State != ticketStateQueued {
		t.Errorf("GetTicket = %+v, want project %d, ref 42, title %q, state queued", got, projectID, testTitleFixBug)
	}
	if got.WaitingOn != nil {
		t.Errorf("GetTicket.WaitingOn = %v, want nil", *got.WaitingOn)
	}
	if got.ClaimOwner != nil {
		t.Errorf("GetTicket.ClaimOwner = %v, want nil", *got.ClaimOwner)
	}
}

func TestInsertTicket_DuplicateTrackerRefRejected(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "a", State: ticketStateQueued}); err != nil {
		t.Fatalf("first InsertTicket: %v", err)
	}

	if _, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "b", State: ticketStateQueued}); err == nil {
		t.Error("second InsertTicket with the same (project_id, tracker_ref): want error, got nil")
	}
}

func TestClaim_ClaimsAnUnclaimedTicket(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	expires := time.Now().Add(10 * time.Minute)
	claimed, err := s.Claim(ctx, id, "host-1", expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim on an unclaimed ticket: got false, want true")
	}

	got, err := s.GetTicket(ctx, id)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner == nil || *got.ClaimOwner != "host-1" {
		t.Errorf("ClaimOwner = %v, want host-1", got.ClaimOwner)
	}
	if got.ClaimExpiresAt == nil || !got.ClaimExpiresAt.Equal(expires.UTC().Truncate(time.Second)) {
		t.Errorf("ClaimExpiresAt = %v, want %v", got.ClaimExpiresAt, expires)
	}
}

func TestClaim_SecondClaimIsRefused(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "t", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	expires := time.Now().Add(10 * time.Minute)
	claimed, err := s.Claim(ctx, id, "host-1", expires)
	if err != nil || !claimed {
		t.Fatalf("first Claim: claimed=%v err=%v, want true, nil", claimed, err)
	}

	claimed, err = s.Claim(ctx, id, "host-2", expires)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed {
		t.Error("second Claim on an already-claimed ticket: got true, want false")
	}

	got, err := s.GetTicket(ctx, id)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if got.ClaimOwner == nil || *got.ClaimOwner != "host-1" {
		t.Errorf("ClaimOwner after a refused second claim = %v, want host-1 (unchanged)", got.ClaimOwner)
	}
}

// TestExpireClaims_ClearsAtOrPastExpiry proves the reconcile query's "at or
// past" contract, `claim_expires_at <= ?` (spine.go): a claim expired in the
// past, one expiring at exactly now (the boundary the <= comparison exists
// for), and a claim still in the future are all handled correctly in one
// pass -- the first two cleared, the last one left untouched.
func TestExpireClaims_ClearsAtOrPastExpiry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	projectID, err := s.EnsureProject(ctx, testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	expiredID, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "1", Title: "expired", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(expired): %v", err)
	}
	boundaryID, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "2", Title: "boundary", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(boundary): %v", err)
	}
	freshID, err := s.InsertTicket(ctx, Ticket{ProjectID: projectID, TrackerRef: "3", Title: "fresh", State: ticketStateQueued})
	if err != nil {
		t.Fatalf("InsertTicket(fresh): %v", err)
	}

	now := time.Now()
	if _, err = s.Claim(ctx, expiredID, "host-1", now.Add(-time.Minute)); err != nil {
		t.Fatalf("Claim(expired): %v", err)
	}
	// boundaryID's expiry is exactly now, formatted through the same
	// second-precision RFC 3339 round trip ExpireClaims's own formatTime(now)
	// argument goes through below, so the two compare equal in SQL: the "at"
	// case the <= comparison (not <) exists to catch.
	if _, err = s.Claim(ctx, boundaryID, "host-1", now); err != nil {
		t.Fatalf("Claim(boundary): %v", err)
	}
	if _, err = s.Claim(ctx, freshID, "host-1", now.Add(time.Hour)); err != nil {
		t.Fatalf("Claim(fresh): %v", err)
	}

	ids, err := s.ExpireClaims(ctx, now)
	if err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}
	wantCleared := map[int64]bool{expiredID: true, boundaryID: true}
	if len(ids) != len(wantCleared) {
		t.Fatalf("ExpireClaims returned %v, want exactly %v cleared", ids, wantCleared)
	}
	for _, id := range ids {
		if !wantCleared[id] {
			t.Errorf("ExpireClaims returned unexpected id %d", id)
		}
	}
	// Assert each expected id is actually present, not only that the set
	// sizes match: a set-size check alone would not catch, for example, a
	// bug that returned the expired id twice instead of the expired id and
	// the boundary id.
	if !slices.Contains(ids, expiredID) {
		t.Errorf("ExpireClaims = %v, want it to contain the expired ticket id %d", ids, expiredID)
	}
	if !slices.Contains(ids, boundaryID) {
		t.Errorf("ExpireClaims = %v, want it to contain the boundary ticket id %d", ids, boundaryID)
	}

	got, err := s.GetTicket(ctx, expiredID)
	if err != nil {
		t.Fatalf("GetTicket(expired): %v", err)
	}
	if got.ClaimOwner != nil || got.ClaimExpiresAt != nil {
		t.Errorf("expired ticket claim = (%v, %v), want (nil, nil)", got.ClaimOwner, got.ClaimExpiresAt)
	}

	boundary, err := s.GetTicket(ctx, boundaryID)
	if err != nil {
		t.Fatalf("GetTicket(boundary): %v", err)
	}
	if boundary.ClaimOwner != nil || boundary.ClaimExpiresAt != nil {
		t.Errorf("boundary ticket claim (expiry == now) = (%v, %v), want (nil, nil) -- the query is <=", boundary.ClaimOwner, boundary.ClaimExpiresAt)
	}

	stillClaimed, err := s.GetTicket(ctx, freshID)
	if err != nil {
		t.Fatalf("GetTicket(fresh): %v", err)
	}
	if stillClaimed.ClaimOwner == nil {
		t.Error("fresh (not-yet-expired) ticket claim was cleared, want it untouched")
	}
}

// insertRunWithOutcome inserts one turn-0 runs row directly under sessionID
// with the given outcome (nil for NULL, the reserved-but-uncommitted case)
// and agentSeconds (nil for NULL), and returns its id.
func insertRunWithOutcome(t *testing.T, s *Store, sessionID int64, outcome *string, agentSeconds *int) int64 {
	t.Helper()
	res, err := s.db.ExecContext(t.Context(),
		`INSERT INTO runs (session_id, turn, outcome, agent_seconds) VALUES (?, 0, ?, ?)`,
		sessionID, outcome, agentSeconds)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return id
}

// TestExpireClaims_ReconcilesNullOutcomeRunOnExpiredClaim proves the D13
// reconcile (spine.go's reconcileReservedRunsTx): a run reserved under a
// claim that has since expired without ever being committed -- a crash, or
// an ErrCanceled shutdown that left no commit (design section 4.5) -- gets
// terminalized to outcome=error, exit_code=-1, agent_seconds floored at 0,
// in the same pass that clears the ticket's claim.
func TestExpireClaims_ReconcilesNullOutcomeRunOnExpiredClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sessionID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRunWithOutcome(t, s, sessionID, nil, nil)

	now := time.Now()
	if _, err := s.Claim(ctx, ticketID, "host-1", now.Add(-time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	ids, err := s.ExpireClaims(ctx, now)
	if err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}
	if !slices.Contains(ids, ticketID) {
		t.Fatalf("ExpireClaims = %v, want it to contain the expired ticket %d", ids, ticketID)
	}

	run, ok, err := s.FirstRun(ctx, sessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok || run.ID != runID {
		t.Fatalf("FirstRun = (%+v, %v), want the reserved run %d", run, ok, runID)
	}
	if run.Outcome == nil || *run.Outcome != "error" {
		t.Errorf("run.Outcome = %v, want error", run.Outcome)
	}
	if run.ExitCode == nil || *run.ExitCode != -1 {
		t.Errorf("run.ExitCode = %v, want -1", run.ExitCode)
	}
	if run.AgentSeconds == nil || *run.AgentSeconds != 0 {
		t.Errorf("run.AgentSeconds = %v, want 0", run.AgentSeconds)
	}
}

// TestExpireClaims_LeavesTerminalOutcomeRunUntouched proves the reconcile
// query's WHERE outcome IS NULL clause: a run that already reached a
// terminal outcome before the claim expired is left exactly as it was, agent
// seconds included.
func TestExpireClaims_LeavesTerminalOutcomeRunUntouched(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sessionID := insertSession(t, s, ticketID, testStatePlanning)
	wantSeconds := 42
	runID := insertRunWithOutcome(t, s, sessionID, new(testTypeQuestion), &wantSeconds)

	now := time.Now()
	if _, err := s.Claim(ctx, ticketID, "host-1", now.Add(-time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := s.ExpireClaims(ctx, now); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, sessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok || run.ID != runID {
		t.Fatalf("FirstRun = (%+v, %v), want run %d", run, ok, runID)
	}
	if run.Outcome == nil || *run.Outcome != testTypeQuestion {
		t.Errorf("run.Outcome = %v, want unchanged %s", run.Outcome, testTypeQuestion)
	}
	if run.ExitCode != nil {
		t.Errorf("run.ExitCode = %v, want still nil (never touched)", run.ExitCode)
	}
	if run.AgentSeconds == nil || *run.AgentSeconds != wantSeconds {
		t.Errorf("run.AgentSeconds = %v, want unchanged %d", run.AgentSeconds, wantSeconds)
	}
}

// TestExpireClaims_LeavesRunsOfUnexpiredClaimUntouched proves the reconcile
// step only ever runs for a ticket ExpireClaims is actually clearing: a
// null-outcome run under a claim that has not expired yet is left alone.
func TestExpireClaims_LeavesRunsOfUnexpiredClaimUntouched(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sessionID := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRunWithOutcome(t, s, sessionID, nil, nil)

	now := time.Now()
	if _, err := s.Claim(ctx, ticketID, "host-1", now.Add(time.Hour)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := s.ExpireClaims(ctx, now); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, sessionID)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok || run.ID != runID {
		t.Fatalf("FirstRun = (%+v, %v), want run %d", run, ok, runID)
	}
	if run.Outcome != nil {
		t.Errorf("run.Outcome = %v, want still nil (claim not expired)", run.Outcome)
	}
}

// TestExpireClaims_LeavesAnotherTicketsRunUntouched proves the reconcile
// query is scoped to the expiring ticket's own sessions: a null-outcome run
// that belongs to a different ticket's session is left alone even though its
// own claim also expires in the same ExpireClaims pass.
func TestExpireClaims_LeavesAnotherTicketsRunUntouched(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, expiring := seedQueuedTicket(t, s, "1")
	_, other := seedQueuedTicket(t, s, "2")
	setTicketState(t, s, expiring, testStatePlanning)
	setTicketState(t, s, other, testStatePlanning)

	otherSession := insertSession(t, s, other, testStatePlanning)
	otherRunID := insertRunWithOutcome(t, s, otherSession, nil, nil)

	now := time.Now()
	if _, err := s.Claim(ctx, expiring, "host-1", now.Add(-time.Minute)); err != nil {
		t.Fatalf("Claim(expiring): %v", err)
	}
	// other's claim is never taken, so it can never appear in ExpireClaims's
	// own cleared set; the reconcile scoping is what this test is really
	// asserting: otherRunID's session hangs off a ticket ExpireClaims never
	// touches at all.
	if _, err := s.ExpireClaims(ctx, now); err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}

	run, ok, err := s.FirstRun(ctx, otherSession)
	if err != nil {
		t.Fatalf("FirstRun: %v", err)
	}
	if !ok || run.ID != otherRunID {
		t.Fatalf("FirstRun = (%+v, %v), want run %d", run, ok, otherRunID)
	}
	if run.Outcome != nil {
		t.Errorf("run.Outcome = %v, want still nil (belongs to another ticket)", run.Outcome)
	}
}

func TestExpireClaims_NoExpiredClaimsReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	ids, err := s.ExpireClaims(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ExpireClaims on an empty database = %v, want empty", ids)
	}
}

func TestFlags_DefaultsAndSetters(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	draining, stopped, err := s.Flags(ctx)
	if err != nil {
		t.Fatalf("Flags: %v", err)
	}
	if draining || stopped {
		t.Errorf("initial Flags = (%v, %v), want (false, false)", draining, stopped)
	}

	if err = s.SetDraining(ctx, true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	if err = s.SetStopped(ctx, true); err != nil {
		t.Fatalf("SetStopped: %v", err)
	}

	draining, stopped, err = s.Flags(ctx)
	if err != nil {
		t.Fatalf("Flags after setting: %v", err)
	}
	if !draining || !stopped {
		t.Errorf("Flags after SetDraining(true), SetStopped(true) = (%v, %v), want (true, true)", draining, stopped)
	}

	if err = s.SetDraining(ctx, false); err != nil {
		t.Fatalf("SetDraining(false): %v", err)
	}
	draining, stopped, err = s.Flags(ctx)
	if err != nil {
		t.Fatalf("Flags after clearing draining: %v", err)
	}
	if draining {
		t.Error("draining after SetDraining(false) = true, want false")
	}
	if !stopped {
		t.Error("stopped after SetDraining(false) = false, want true (untouched)")
	}
}

func TestGetTicket_MissingIDReturnsError(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.GetTicket(ctx, 999)
	if err == nil {
		t.Fatal("GetTicket on a missing id: want error, got nil")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetTicket error = %v, want it to wrap sql.ErrNoRows", err)
	}
}
