// rail_internal_test.go is a whitebox test for capLogEntries (rail.go),
// review fix 5 (package 4 re-review): "cap the merged, sorted tail to the
// ring capacity (keep the newest entries)". It lives in package console,
// not console_test, because capLogEntries is unexported and this property
// is cleanest proved directly against synthetic entries rather than through
// a real ring: buildLogRail's own merge can only be pushed past RingCapacity
// by a genuine race between its two non-atomic Tail() reads (see rail.go's
// buildLogRail doc comment), which a single-goroutine test cannot reproduce
// deterministically through the ring itself.
package console

import (
	"fmt"
	"testing"
	"time"
)

// TestCapLogEntries_KeepsNewestAndDropsOldestExcess proves a merge that
// somehow exceeded RingCapacity is trimmed back down to exactly
// RingCapacity entries, keeping the newest ones (the tail of an
// oldest-first slice) and dropping the oldest excess from the front.
func TestCapLogEntries_KeepsNewestAndDropsOldestExcess(t *testing.T) {
	const over = 37
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	entries := make([]LogEntry, 0, RingCapacity+over)
	for i := range RingCapacity + over {
		entries = append(entries, LogEntry{
			Time:    base.Add(time.Duration(i) * time.Second),
			Message: fmt.Sprintf("line-%d", i),
		})
	}

	got := capLogEntries(entries, RingCapacity)
	if len(got) != RingCapacity {
		t.Fatalf("capLogEntries returned %d entries, want exactly %d (RingCapacity)", len(got), RingCapacity)
	}

	wantFirst := fmt.Sprintf("line-%d", over)
	if got[0].Message != wantFirst {
		t.Errorf("capLogEntries kept an old entry: got[0].Message = %q, want %q (the oldest %d entries dropped)", got[0].Message, wantFirst, over)
	}

	wantLast := fmt.Sprintf("line-%d", RingCapacity+over-1)
	if got[len(got)-1].Message != wantLast {
		t.Errorf("capLogEntries dropped a newest entry: got[len-1].Message = %q, want %q", got[len(got)-1].Message, wantLast)
	}
}

// TestCapLogEntries_NoOpAtOrUnderCapacity proves a merge already at or under
// capacity comes back unchanged, not trimmed or reordered.
func TestCapLogEntries_NoOpAtOrUnderCapacity(t *testing.T) {
	entries := []LogEntry{{Message: "a"}, {Message: "b"}, {Message: "c"}}

	got := capLogEntries(entries, RingCapacity)
	if len(got) != len(entries) {
		t.Fatalf("capLogEntries under capacity returned %d entries, want %d unchanged", len(got), len(entries))
	}
	for i, e := range entries {
		if got[i].Message != e.Message {
			t.Errorf("capLogEntries under capacity reordered entries: got[%d].Message = %q, want %q", i, got[i].Message, e.Message)
		}
	}
}
