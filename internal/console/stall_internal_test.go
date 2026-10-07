// stall_internal_test.go is a whitebox unit test for decideStall and
// lastRanText (stall.go), Task 2 of "Say on each ticket why it is not
// moving" (split from #79, issue #93). It lives in package console, not
// console_test, because stallInput, stallReason, and both functions are
// unexported and the decision is cleanest proved directly against
// synthetic stallInput values rather than through a real store, dispatcher,
// or running server.
package console

import (
	"testing"
	"time"

	"zing/internal/dispatch"
	"zing/internal/response"
	"zing/internal/store"
)

func TestDecideStall(t *testing.T) {
	t.Parallel()

	ciTime := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		in         stallInput
		wantReason stallReason
		wantText   string
	}{
		{
			name: "running_now",
			in: stallInput{
				Ticket: store.Ticket{ID: 11, WaitingOn: new("gate")},
				Slots:  &dispatch.SlotSnapshot{Owner: "this-serve", Inflight: []int64{11, 12}, MaxParallel: 2},
				Now:    ciTime,
			},
			wantReason: stallRunning,
			wantText:   "running now",
		},
		{
			name: "owner_wait_beats_ci",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping), WaitingOn: new("merge")},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci,lint"},
				Now:      ciTime,
			},
			wantReason: stallOwner,
			wantText:   "waiting on the owner (merge)",
		},
		{
			name: "dead_claim_reclaim_next",
			in: stallInput{
				Ticket:     store.Ticket{ID: 1, ClaimOwner: new("dead-serve-1")},
				Slots:      &dispatch.SlotSnapshot{Owner: "this-serve", MaxParallel: 2},
				ClaimAlive: false,
				Now:        ciTime,
			},
			wantReason: stallClaimDead,
			wantText:   "claim held by a process that is no longer alive (dead-serve-1); the next dispatch pass reclaims it",
		},
		{
			name: "dead_claim_orphan_running",
			in: stallInput{
				Ticket:     store.Ticket{ID: 1, ClaimOwner: new("dead-serve-2")},
				Slots:      &dispatch.SlotSnapshot{Owner: "this-serve", MaxParallel: 2},
				ClaimAlive: true,
				Now:        ciTime,
			},
			wantReason: stallClaimDead,
			wantText:   "claim held by a process that is no longer alive (dead-serve-2); its leftover agent process is still running",
		},
		{
			name: "dead_claim_beats_ci",
			in: stallInput{
				Ticket:     store.Ticket{ID: 1, State: string(response.TicketStateShipping), ClaimOwner: new("dead-serve-1")},
				Slots:      &dispatch.SlotSnapshot{Owner: "this-serve", MaxParallel: 2},
				ClaimAlive: false,
				CIMarker:   &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci"},
				Now:        ciTime,
			},
			wantReason: stallClaimDead,
			wantText:   "claim held by a process that is no longer alive (dead-serve-1); the next dispatch pass reclaims it",
		},
		{
			name: "ci_waiting_minutes",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci,lint"},
				LastRan:  new(time.Date(2026, 1, 2, 9, 30, 0, 0, time.UTC)),
				Now:      time.Date(2026, 1, 2, 10, 7, 59, 0, time.UTC),
			},
			wantReason: stallCI,
			wantText:   "CI waiting 7 minutes for ci, lint",
		},
		{
			name: "ci_one_minute",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci"},
				Now:      ciTime.Add(time.Minute),
			},
			wantReason: stallCI,
			wantText:   "CI waiting 1 minute for ci",
		},
		{
			name: "ci_future_marker_clamps_zero",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci"},
				Now:      ciTime.Add(-time.Minute),
			},
			wantReason: stallCI,
			wantText:   "CI waiting 0 minutes for ci",
		},
		{
			name: "ci_marker_older_than_newest_run",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci"},
				LastRan:  new(ciTime.Add(time.Minute)),
				Now:      ciTime.Add(2 * time.Minute),
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "ci_marker_equal_to_newest_run",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci"},
				LastRan:  new(ciTime),
				Now:      ciTime.Add(time.Minute),
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "ci_marker_names_none",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting "},
				Now:      ciTime.Add(time.Minute),
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "ci_marker_nil_created_at",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateShipping)},
				CIMarker: &store.MessageRow{Body: "ci waiting ci"},
				Now:      ciTime.Add(time.Minute),
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "ci_not_shipping",
			in: stallInput{
				Ticket:   store.Ticket{ID: 1, State: string(response.TicketStateBuilding)},
				CIMarker: &store.MessageRow{CreatedAt: new(ciTime), Body: "ci waiting ci"},
				Now:      ciTime.Add(time.Minute),
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "slot_full",
			in: stallInput{
				Ticket:    store.Ticket{ID: 1},
				Slots:     &dispatch.SlotSnapshot{Owner: "this-serve", Inflight: []int64{11, 12}, MaxParallel: 2},
				Candidate: true,
				Now:       ciTime,
			},
			wantReason: stallSlot,
			wantText:   "waiting for a free run slot; slots held by tickets 11, 12",
		},
		{
			name: "slot_free",
			in: stallInput{
				Ticket:    store.Ticket{ID: 1},
				Slots:     &dispatch.SlotSnapshot{Owner: "this-serve", Inflight: []int64{11}, MaxParallel: 2},
				Candidate: true,
				Now:       ciTime,
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "not_candidate",
			in: stallInput{
				Ticket:    store.Ticket{ID: 1},
				Slots:     &dispatch.SlotSnapshot{Owner: "this-serve", Inflight: []int64{11, 12}, MaxParallel: 2},
				Candidate: false,
				Now:       ciTime,
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "no_slot_source",
			in: stallInput{
				Ticket:    store.Ticket{ID: 1},
				Slots:     nil,
				Candidate: true,
				Now:       ciTime,
			},
			wantReason: stallNone,
			wantText:   "",
		},
		{
			name: "own_claim_not_inflight",
			in: stallInput{
				Ticket: store.Ticket{ID: 1, ClaimOwner: new("this-serve")},
				Slots:  &dispatch.SlotSnapshot{Owner: "this-serve", MaxParallel: 2},
				Now:    ciTime,
			},
			wantReason: stallNone,
			wantText:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotReason, gotText := decideStall(tc.in)
			if gotReason != tc.wantReason || gotText != tc.wantText {
				t.Fatalf("decideStall() = (%q, %q), want (%q, %q)", gotReason, gotText, tc.wantReason, tc.wantText)
			}
		})
	}
}

func TestLastRanText(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := lastRanText(nil); got != "never ran" {
			t.Fatalf("lastRanText(nil) = %q, want %q", got, "never ran")
		}
	})

	t.Run("utc", func(t *testing.T) {
		t.Parallel()
		ts := time.Date(2026, 1, 2, 3, 4, 59, 0, time.UTC)
		want := "last ran 2026-01-02 03:04 UTC"
		if got := lastRanText(&ts); got != want {
			t.Fatalf("lastRanText(%v) = %q, want %q", ts, got, want)
		}
	})

	t.Run("converts_to_utc", func(t *testing.T) {
		t.Parallel()
		loc := time.FixedZone("UTC+2", 2*60*60)
		ts := time.Date(2026, 1, 2, 5, 4, 59, 0, loc)
		want := "last ran 2026-01-02 03:04 UTC"
		if got := lastRanText(&ts); got != want {
			t.Fatalf("lastRanText(%v) = %q, want %q", ts, got, want)
		}
	})
}
