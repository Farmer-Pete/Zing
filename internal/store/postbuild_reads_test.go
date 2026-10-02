package store

import (
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/response"
)

// validFinding, validVerdict, and validRespond build the smallest artifact
// payload that passes its schema (internal/store/examples/artifacts/*.json
// carries the same shapes), so Findings, Verdicts, and RespondBatches tests
// can insert real, schema-valid rows through CommitHandlerResult rather than
// a hand-written JSON literal.
func validFinding(id string, round int) response.FindingArtifact {
	return response.FindingArtifact{
		Lens: response.LensTests, Severity: response.SeverityMinor,
		Location: "internal/cart/cart_test.go:10",
		Text:     "the new test does not assert the order total",
		Fix:      "assert order.Total == 0 for an empty cart",
		ID:       id, Round: round, SHA: strings.Repeat("a", 40),
		Lenses: []response.Lens{response.LensTests},
	}
}

func validVerdict(scenario string, round int) response.VerdictArtifact {
	return response.VerdictArtifact{
		Scenario: scenario, Result: response.ResultPass, Evidence: "go test ./...: PASS",
		Kind: response.ScenarioKindBehavior, Round: round, SHA: strings.Repeat("a", 40),
	}
}

func validRespond(batch int) response.RespondArtifact {
	return response.RespondArtifact{
		Threads: []response.ThreadAction{{ID: "t1", Action: response.ThreadVerbFix, Text: "renamed the variable"}},
		Batch:   batch, SHA: strings.Repeat("a", 40),
		Seen: []response.ThreadSeen{{TID: "t0123456789abcdef", LastComment: strings.Repeat("0", 64)}},
	}
}

func marshalArtifact(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	return b
}

// reserveAndCommitArtifacts reserves one run on ticketID and, in a single
// commit, writes artifacts, each carrying that run's id. It returns the
// run id every written row is scoped to.
func reserveAndCommitArtifacts(t *testing.T, s *Store, ticketID int64, artifactType string, payloads []json.RawMessage) int64 {
	t.Helper()
	ctx := t.Context()
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	artifacts := make([]Artifact, len(payloads))
	for i, p := range payloads {
		artifacts[i] = Artifact{Type: artifactType, RunID: &reserved.RunID, Payload: p}
	}

	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Artifacts: artifacts,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult: %v", err)
	}
	if !applied {
		t.Fatal("CommitHandlerResult: applied = false, want true")
	}
	return reserved.RunID
}

// TestFindingsOrder proves Findings returns every finding artifact of the
// ticket, decoded, ORDER BY artifacts.id (design section 4.2).
func TestFindingsOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	runID := reserveAndCommitArtifacts(t, s, ticketID, artifactTypeFinding, []json.RawMessage{
		marshalArtifact(t, validFinding("r1f1", 1)),
		marshalArtifact(t, validFinding("r1f2", 1)),
	})

	got, err := s.Findings(ctx, ticketID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Finding.ID != "r1f1" || got[1].Finding.ID != "r1f2" {
		t.Errorf("got ids = [%s, %s], want [r1f1, r1f2] (artifacts.id order)", got[0].Finding.ID, got[1].Finding.ID)
	}
	if got[0].ArtifactID <= 0 || got[1].ArtifactID <= got[0].ArtifactID {
		t.Errorf("ArtifactIDs = [%d, %d], want ascending and positive", got[0].ArtifactID, got[1].ArtifactID)
	}
	if got[0].RunID == nil || *got[0].RunID != runID {
		t.Errorf("got[0].RunID = %v, want %d", got[0].RunID, runID)
	}
}

// TestVerdictsOrder proves Verdicts returns every judge verdict artifact of
// the ticket, decoded, ORDER BY artifacts.id (design section 4.2).
func TestVerdictsOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	runID := reserveAndCommitArtifacts(t, s, ticketID, artifactTypeVerdict, []json.RawMessage{
		marshalArtifact(t, validVerdict("s1", 1)),
		marshalArtifact(t, validVerdict("s2", 1)),
	})

	got, err := s.Verdicts(ctx, ticketID)
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Verdict.Scenario != "s1" || got[1].Verdict.Scenario != "s2" {
		t.Errorf("got scenarios = [%s, %s], want [s1, s2] (artifacts.id order)", got[0].Verdict.Scenario, got[1].Verdict.Scenario)
	}
	if got[1].ArtifactID <= got[0].ArtifactID {
		t.Errorf("ArtifactIDs = [%d, %d], want ascending", got[0].ArtifactID, got[1].ArtifactID)
	}
	if got[0].RunID == nil || *got[0].RunID != runID {
		t.Errorf("got[0].RunID = %v, want %d", got[0].RunID, runID)
	}
}

// TestRespondBatchesOrder proves RespondBatches returns every respond
// artifact of the ticket, decoded, ORDER BY artifacts.id (design section
// 4.2).
func TestRespondBatchesOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	runID := reserveAndCommitArtifacts(t, s, ticketID, artifactTypeRespond, []json.RawMessage{
		marshalArtifact(t, validRespond(1)),
		marshalArtifact(t, validRespond(2)),
	})

	got, err := s.RespondBatches(ctx, ticketID)
	if err != nil {
		t.Fatalf("RespondBatches: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Respond.Batch != 1 || got[1].Respond.Batch != 2 {
		t.Errorf("got batches = [%d, %d], want [1, 2] (artifacts.id order)", got[0].Respond.Batch, got[1].Respond.Batch)
	}
	if got[1].ArtifactID <= got[0].ArtifactID {
		t.Errorf("ArtifactIDs = [%d, %d], want ascending", got[0].ArtifactID, got[1].ArtifactID)
	}
	if got[0].RunID == nil || *got[0].RunID != runID {
		t.Errorf("got[0].RunID = %v, want %d", got[0].RunID, runID)
	}
}

// TestRunByID proves RunByID returns one run of any ticket, by its own id
// (design section 4.2): not scoped to a caller-known ticket, unlike every
// other run read in this package.
func TestRunByID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	reserved, err := s.Reserve(ctx, ticketID, owner, expires,
		SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelOpus48})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	run, err := s.RunByID(ctx, reserved.RunID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if run.ID != reserved.RunID {
		t.Errorf("run.ID = %d, want %d", run.ID, reserved.RunID)
	}
	if run.SessionID != reserved.SessionID {
		t.Errorf("run.SessionID = %d, want %d", run.SessionID, reserved.SessionID)
	}
	if run.Model == nil || *run.Model != testModelOpus48 {
		t.Errorf("run.Model = %v, want %q", run.Model, testModelOpus48)
	}

	if _, err := s.RunByID(ctx, reserved.RunID+1000); err == nil {
		t.Error("RunByID(unknown id): want an error, got nil")
	}
}

// TestMarkersWithPrefix proves MarkersWithPrefix matches a marker's first
// line by prefix, not by exact equality (design section 5.1, D18): a "fix
// requested ..." marker is returned for prefix "fix requested ", a
// differently-worded marker is not, and two matches come back in id order
// (oldest first).
func TestMarkersWithPrefix(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	insertUpdateMarker(t, s, ticketID, "fix requested findings after run 4\nfindings text")
	insertUpdateMarker(t, s, ticketID, "fix landed 1 sha aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	second := insertUpdateMarker(t, s, ticketID, "fix requested ci_log after run 9\nlog text")

	got, err := s.MarkersWithPrefix(ctx, ticketID, "fix requested ")
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (the landed marker excluded)", len(got))
	}
	if got[1].ID != second {
		t.Errorf("got[1].ID = %d, want %d (oldest first)", got[1].ID, second)
	}
	for _, m := range got {
		if m.Body == "fix landed 1 sha aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Errorf("got includes the landed marker: %q", m.Body)
		}
	}

	none, err := s.MarkersWithPrefix(ctx, ticketID, "respond batch ")
	if err != nil {
		t.Fatalf("MarkersWithPrefix (no match): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("len(none) = %d, want 0", len(none))
	}
}

// TestMaxRunID proves MaxRunID returns the ticket's highest run id, or 0
// when it has none yet (design D18, section 5.1).
// TestMaxRunID's two subtests each open their own store rather than share
// one across t.Parallel() siblings: both seed a ticket under the same
// fixed testProject name, and EnsureProject's own check-then-insert is not
// safe for two goroutines racing on the same project row.
func TestMaxRunID(t *testing.T) {
	t.Parallel()

	t.Run("no runs yet", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		got, err := s.MaxRunID(ctx, ticketID)
		if err != nil {
			t.Fatalf("MaxRunID: %v", err)
		}
		if got != 0 {
			t.Errorf("MaxRunID = %d, want 0", got)
		}
	})

	t.Run("the highest run id across sessions", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "2")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		first, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve 1: %v", err)
		}
		second, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve 2: %v", err)
		}
		if second.RunID <= first.RunID {
			t.Fatalf("second.RunID = %d, want it greater than first.RunID = %d", second.RunID, first.RunID)
		}

		got, err := s.MaxRunID(ctx, ticketID)
		if err != nil {
			t.Fatalf("MaxRunID: %v", err)
		}
		if got != second.RunID {
			t.Errorf("MaxRunID = %d, want %d", got, second.RunID)
		}
	})
}

// TestSessionAfter proves SessionAfter returns the newest session of job
// whose turn-0 run id is greater than afterRunID (design D18, section 5.1):
// none for a ticket with no matching session, the one session found when
// there is exactly one, the newest of several after the watermark, a
// session started before the watermark ignored entirely, and the three
// SessionState classifications.
// TestSessionAfter's four subtests each open their own store rather than
// share one across t.Parallel() siblings: each seeds a ticket under the
// same fixed testProject name, and EnsureProject's own check-then-insert is
// not safe for two goroutines racing on the same project row.
func TestSessionAfter(t *testing.T) {
	t.Parallel()

	t.Run("none for a ticket with no session", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "1")
		_, _, _, ok, err := s.SessionAfter(ctx, ticketID, testJobBuild, 0, 3)
		if err != nil {
			t.Fatalf("SessionAfter: %v", err)
		}
		if ok {
			t.Error("SessionAfter with no session: ok = true, want false")
		}
	})

	t.Run("one session after the watermark", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "2")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		before, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (before watermark): %v", err)
		}

		after, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (after watermark): %v", err)
		}

		sess, state, newest, ok, err := s.SessionAfter(ctx, ticketID, testJobBuild, before.RunID, 3)
		if err != nil {
			t.Fatalf("SessionAfter: %v", err)
		}
		if !ok {
			t.Fatal("SessionAfter: ok = false, want true")
		}
		if sess.ID != after.SessionID {
			t.Errorf("sess.ID = %d, want %d (the session started after the watermark)", sess.ID, after.SessionID)
		}
		if newest.ID != after.RunID {
			t.Errorf("newest.ID = %d, want %d", newest.ID, after.RunID)
		}
		if state != SessionIdless {
			t.Errorf("state = %v, want SessionIdless (external_id still NULL)", state)
		}
	})

	t.Run("the newest of several sessions after the watermark; an earlier one is ignored", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "3")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		before, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (before watermark): %v", err)
		}
		_, err = s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (older, after watermark): %v", err)
		}
		newestSess, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (newest, after watermark): %v", err)
		}

		sess, _, _, ok, err := s.SessionAfter(ctx, ticketID, testJobBuild, before.RunID, 3)
		if err != nil {
			t.Fatalf("SessionAfter: %v", err)
		}
		if !ok {
			t.Fatal("SessionAfter: ok = false, want true")
		}
		if sess.ID != newestSess.SessionID {
			t.Errorf("sess.ID = %d, want %d (the newest session after the watermark)", sess.ID, newestSess.SessionID)
		}

		// A watermark at or past every session's own turn-0 run: none found.
		_, _, _, ok, err = s.SessionAfter(ctx, ticketID, testJobBuild, newestSess.RunID, 3)
		if err != nil {
			t.Fatalf("SessionAfter (watermark at the newest run): %v", err)
		}
		if ok {
			t.Error("SessionAfter with the watermark at the newest run: ok = true, want false")
		}
	})

	t.Run("open and exhausted states", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		_, ticketID := seedQueuedTicket(t, s, "4")
		setTicketState(t, s, ticketID, testStatePlanning)
		owner, expires := claimForCommit(t, s, ticketID)

		before, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (before watermark): %v", err)
		}
		after, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
		if err != nil {
			t.Fatalf("Reserve (after watermark): %v", err)
		}
		external := "ext-after"
		applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
			TicketID: ticketID, Owner: owner, Expires: expires,
			Session: &SessionUpsert{ID: &after.SessionID, ExternalID: &external},
		})
		if err != nil || !applied {
			t.Fatalf("fill external_id: applied=%v err=%v", applied, err)
		}

		_, state, _, ok, err := s.SessionAfter(ctx, ticketID, testJobBuild, before.RunID, 3)
		if err != nil || !ok {
			t.Fatalf("SessionAfter: ok=%v err=%v", ok, err)
		}
		if state != SessionOpen {
			t.Errorf("state = %v, want SessionOpen", state)
		}

		owner, expires = claimForCommit(t, s, ticketID)
		for range 3 {
			_, err = s.Reserve(ctx, ticketID, owner, expires,
				SessionUpsert{ID: &after.SessionID, BumpResumes: true}, RunSeed{Model: testModelClaudeX})
			if err != nil {
				t.Fatalf("resume Reserve: %v", err)
			}
		}

		_, state, _, ok, err = s.SessionAfter(ctx, ticketID, testJobBuild, before.RunID, 3)
		if err != nil || !ok {
			t.Fatalf("SessionAfter after 3 resumes: ok=%v err=%v", ok, err)
		}
		if state != SessionExhausted {
			t.Errorf("state after 3 resumes (max 3) = %v, want SessionExhausted", state)
		}
	})
}

// TestSessionRunIDs proves SessionRunIDs returns a session's own run ids, in
// turn order (design section 5.3): the identity a fix unit's build reports
// are scoped against.
func TestSessionRunIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	owner, expires := claimForCommit(t, s, ticketID)

	first, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	external := "ext-1"
	applied, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Session: &SessionUpsert{ID: &first.SessionID, ExternalID: &external},
	})
	if err != nil || !applied {
		t.Fatalf("fill external_id: applied=%v err=%v", applied, err)
	}

	owner, expires = claimForCommit(t, s, ticketID)
	second, err := s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{ID: &first.SessionID, BumpResumes: true}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve resume: %v", err)
	}

	// A second, unrelated session must not leak into the first's own list.
	_, err = s.Reserve(ctx, ticketID, owner, expires, SessionUpsert{Job: testJobBuild, Runtime: testRuntimeFake}, RunSeed{Model: testModelClaudeX})
	if err != nil {
		t.Fatalf("Reserve (other session): %v", err)
	}

	got, err := s.SessionRunIDs(ctx, first.SessionID)
	if err != nil {
		t.Fatalf("SessionRunIDs: %v", err)
	}
	if len(got) != 2 || got[0] != first.RunID || got[1] != second.RunID {
		t.Errorf("SessionRunIDs = %v, want [%d, %d]", got, first.RunID, second.RunID)
	}
}
