package store

import "testing"

// TestMarkersWithPrefix proves MarkersWithPrefix matches a marker's first
// line by prefix, not by exact equality (design section 5.1, D18): a "fix
// requested ..." marker is returned for prefix "fix requested ", a
// differently-worded marker is not, and two matches come back in id order
// (oldest first).
func TestMarkersWithPrefix(t *testing.T) {
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
func TestMaxRunID(t *testing.T) {
	s := newTestStore(t)

	t.Run("no runs yet", func(t *testing.T) {
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
func TestSessionAfter(t *testing.T) {
	s := newTestStore(t)

	t.Run("none for a ticket with no session", func(t *testing.T) {
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
