// reviewing_session_limit_test.go tests task 5 of issue #45: tableCommit's
// capped-lens row, inserted right after the ctx.Err block, discards the
// whole review round with no commit whenever any lens hit the Claude
// session limit (design shape, owner decision Q3) -- ahead of the budget,
// sandbox, error-document, invalid-output and exec-failure rows, so a
// capped round never writes a "review round N failed" marker and never
// counts toward lensFailedTwiceWhat ("two review rounds in a row failed").
// It reuses reviewing_cap_test.go's own harness (reviewTicketReady,
// reviewScriptsFS, reviewScriptKey, pbClaim, pbGetTicket), package job.
package job

import (
	"strings"
	"testing"
	"time"

	"zing/internal/runtime"
)

// reviewCappedLensMessage is the Claude final message this file scripts one
// lens's turn with, well-formed enough for parseSessionLimit's ResetAt to
// actually parse, though tableCommit's capped row cares only that
// job.CappedUntil (here, claudeCapped) finds it true.
const reviewCappedLensMessage = "You've hit your session limit · resets 12:20pm (America/New_York)"

// reviewCappedExitKey is reviewScriptKey's own "review/1-LENS/1.xml" stem
// with the fake runtime's own ".exit1" suffix (fake.go's effectKey, task
// 1): it scripts lens's round-1 turn to exit 1 with
// reviewCappedLensMessage as its final message, read ahead of the same
// stem's ".xml" turn (Fake.Run checks N.exit1 before N.xml).
func reviewCappedExitKey(lens string) string {
	return strings.TrimSuffix(reviewScriptKey(lens, 1), ".xml") + ".exit1"
}

// TestReviewRound_CappedLensDiscardsRound proves tableCommit's capped-lens
// row (design goal "A capped review lens discards the whole round with no
// commit, so the round-failure count does not move", owner decision Q3,
// picked option a): two separate round attempts where lens "correctness"
// hits the session limit each return an error job.CappedUntil recognizes
// and an empty commit, so neither writes a "review round" marker and
// neither feeds lensFailedTwiceWhat's two-in-a-row escalation.
func TestReviewRound_CappedLensDiscardsRound(t *testing.T) {
	t.Parallel()
	const cappedLens = "correctness"

	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewCappedExitKey(cappedLens): reviewCappedLensMessage,
	})
	rt := runtime.NewFake(scripts)

	for attempt := 1; attempt <= 2; attempt++ {
		deps := pbClaim(t, s, rt, ticket.ID)
		ticketNow := pbGetTicket(t, s, ticket.ID)
		commit, err := (reviewingHandler{}).Run(t.Context(), ticketNow, deps)
		if err == nil {
			t.Fatalf("attempt %d: Run: err = nil, want a capped error", attempt)
		}
		if _, capped := CappedUntil(err); !capped {
			t.Fatalf("attempt %d: CappedUntil(%v) = (_, false), want true", attempt, err)
		}
		if commit.Escalation != nil {
			t.Errorf("attempt %d: commit.Escalation = %+v, want nil", attempt, commit.Escalation)
		}
		if len(commit.Messages) != 0 {
			t.Errorf("attempt %d: commit.Messages = %+v, want none", attempt, commit.Messages)
		}
		if commit.Waiting != nil {
			t.Errorf("attempt %d: commit.Waiting = %q, want nil", attempt, *commit.Waiting)
		}
		// tableCommit's capped row writes no commit at all: nothing to apply,
		// so the next attempt (simulating the dispatcher's free resume after
		// the reset) sees the same, round-less ticket state, once the claim
		// this attempt held is cleared (the dispatcher's own ParkRuns does
		// this in production; here, ExpireClaims stands in for it).
		if _, expireErr := s.ExpireClaims(t.Context(), time.Now().Add(24*time.Hour), ""); expireErr != nil {
			t.Fatalf("attempt %d: ExpireClaims: %v", attempt, expireErr)
		}
	}

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, reviewRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix(%q): %v", reviewRoundMarkerPrefix, err)
	}
	if len(markers) != 0 {
		t.Errorf("MarkersWithPrefix(%q) = %+v, want none (a capped round writes no round marker)", reviewRoundMarkerPrefix, markers)
	}

	messages, err := s.ListMessages(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range messages {
		if strings.HasPrefix(m.Body, "review round") {
			t.Errorf("message %+v starts with %q, want no round marker from a capped round", m, "review round")
		}
		if strings.Contains(m.Body, lensFailedTwiceWhat) {
			t.Errorf("message %+v contains %q, want a capped round never to count toward it", m, lensFailedTwiceWhat)
		}
	}
}
