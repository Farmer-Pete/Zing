package store

import (
	"database/sql"
	"errors"
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

func TestInsertTicket_RequiresQueuedState(t *testing.T) {
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

func TestExpireClaims_NoExpiredClaimsReturnsEmpty(t *testing.T) {
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
