// owner_smoke_test.go proves the Owner smoke checks section (#101): a
// ticket that reached done with reason merged, and whose stored plan
// carries owner_smoke items, shows them as unchecked, unsaved checkboxes;
// every other ticket shows none.
package console_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// advanceTicketWithReason moves ticketID to state the same way
// advanceTicketToState does, but with a caller-given reason instead of the
// fixed "sealed section test advance" string, so a test can reach done
// with reason merged, or any other reason, from queued.
func advanceTicketWithReason(t *testing.T, s *store.Store, ticketID int64, state, reason string) {
	t.Helper()
	const owner = "owner-smoke-test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: state, Reason: reason,
	})
	if err != nil || !applied {
		t.Fatalf("CommitHandlerResult(%s, %s): applied=%v err=%v", state, reason, applied, err)
	}
}

// seedOwnerSmokeFixture seeds a queued ticket carrying a run and a plan
// whose Delivery.OwnerSmoke is items, the same way
// seedSealedSectionFixture inserts its plan artifact.
func seedOwnerSmokeFixture(t *testing.T, s *store.Store, items []string) int64 {
	t.Helper()
	ticketID := seedTicket(t, s, "1", "fix the bug")
	runID := seedRun(t, s, ticketID)

	plan := planWithTasks(
		[]response.Task{{N: 1, Test: "T1", Demo: true, Text: "owner smoke task"}},
		[]response.FileChange{{Path: "internal/store/console_reads.go", Action: response.FileActionModify, Task: "1", Reason: "r1"}},
	)
	plan.Delivery.OwnerSmoke = items
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, insErr := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: &runID, Type: testArtifactTypePlan, Version: 1, Payload: payload,
	}); insErr != nil {
		t.Fatalf("InsertArtifact(plan): %v", insErr)
	}
	return ticketID
}

// ownerSmokeHTML extracts the Owner smoke checks section's own fragment
// from a rendered #main frame: from id="owner-smoke" up to its closing
// </section>, so assertions run against only the section's own markup.
func ownerSmokeHTML(t *testing.T, main string) (string, bool) {
	t.Helper()
	start := strings.Index(main, `id="owner-smoke"`)
	if start < 0 {
		return "", false
	}
	end := strings.Index(main[start:], "</section>")
	if end < 0 {
		t.Fatalf(`ownerSmokeHTML: found id="owner-smoke" but no closing </section> in:\n%s`, main)
	}
	return main[start : start+end], true
}

// TestThreadOwnerSmoke proves the done-when test: a merged done ticket
// whose stored plan carries owner_smoke items shows them as unchecked,
// unsaved checkboxes under "Owner smoke checks"; a merged done ticket with
// no items, a done ticket that did not merge, and a not-yet-done ticket
// all show none.
func TestThreadOwnerSmoke(t *testing.T) {
	t.Parallel()

	t.Run("done_with_items", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		items := []string{"Open the inbox and see the badge", "Click Abandon and see the confirm"}
		ticketID := seedOwnerSmokeFixture(t, s, items)
		advanceTicketWithReason(t, s, ticketID, "done", "merged")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if !strings.Contains(main, "Owner smoke checks") {
			t.Fatalf(`main frame missing "Owner smoke checks":\n%s`, main)
		}
		section, ok := ownerSmokeHTML(t, main)
		if !ok {
			t.Fatalf(`main frame missing id="owner-smoke":\n%s`, main)
		}
		for _, item := range items {
			if !strings.Contains(section, item) {
				t.Errorf("owner smoke section missing item %q:\n%s", item, section)
			}
		}
		if n := strings.Count(section, `type="checkbox"`); n != 2 {
			t.Errorf(`type="checkbox" count = %d, want 2`, n)
		}
		for _, forbidden := range []string{"checked", "data-on", "<form"} {
			if strings.Contains(section, forbidden) {
				t.Errorf("owner smoke section contains %q, want none:\n%s", forbidden, section)
			}
		}
	})

	t.Run("done_without_items", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		ticketID := seedOwnerSmokeFixture(t, s, nil)
		advanceTicketWithReason(t, s, ticketID, "done", "merged")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if strings.Contains(main, "Owner smoke checks") {
			t.Errorf(`main frame contains "Owner smoke checks", want none:\n%s`, main)
		}
		if _, ok := ownerSmokeHTML(t, main); ok {
			t.Errorf(`main frame contains id="owner-smoke", want none:\n%s`, main)
		}
	})

	t.Run("done_not_merged", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		items := []string{"Open the inbox and see the badge", "Click Abandon and see the confirm"}
		ticketID := seedOwnerSmokeFixture(t, s, items)
		advanceTicketWithReason(t, s, ticketID, "done", "nothing to do")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if strings.Contains(main, "Owner smoke checks") {
			t.Errorf(`main frame contains "Owner smoke checks", want none:\n%s`, main)
		}
		if _, ok := ownerSmokeHTML(t, main); ok {
			t.Errorf(`main frame contains id="owner-smoke", want none:\n%s`, main)
		}
	})

	t.Run("shipping_with_items", func(t *testing.T) {
		t.Parallel()
		s := newConsoleTestStore(t)
		items := []string{"Open the inbox and see the badge", "Click Abandon and see the confirm"}
		ticketID := seedOwnerSmokeFixture(t, s, items)
		advanceTicketWithReason(t, s, ticketID, "shipping", "merge pending")

		srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
		resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		_, main, _, _ := readInitialFrames(t, r)

		if strings.Contains(main, "Owner smoke checks") {
			t.Errorf(`main frame contains "Owner smoke checks", want none:\n%s`, main)
		}
		if _, ok := ownerSmokeHTML(t, main); ok {
			t.Errorf(`main frame contains id="owner-smoke", want none:\n%s`, main)
		}
	})
}
