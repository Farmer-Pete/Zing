package store

import (
	"testing"
	"time"
)

// TestFormatTimeFixedWidth proves formatTime (design DD1) always renders 20
// characters ending "Z", for a sub-second time (truncated away) and for a
// time given in a non-UTC zone (converted to UTC first).
func TestFormatTimeFixedWidth(t *testing.T) {
	t.Parallel()

	subSecond := time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC)
	got := formatTime(subSecond)
	want := "2026-03-04T05:06:07Z"
	if got != want {
		t.Errorf("formatTime(sub-second) = %q, want %q", got, want)
	}
	if len(got) != 20 {
		t.Errorf("formatTime(sub-second) length = %d, want 20", len(got))
	}
	if got[len(got)-1] != 'Z' {
		t.Errorf("formatTime(sub-second) = %q, want it to end in Z", got)
	}

	tz := time.FixedZone("UTC-7", -7*60*60)
	nonUTC := time.Date(2026, 3, 4, 5, 6, 7, 0, tz)
	got = formatTime(nonUTC)
	want = "2026-03-04T12:06:07Z"
	if got != want {
		t.Errorf("formatTime(non-UTC zone) = %q, want %q", got, want)
	}
	if len(got) != 20 {
		t.Errorf("formatTime(non-UTC zone) length = %d, want 20", len(got))
	}
	if got[len(got)-1] != 'Z' {
		t.Errorf("formatTime(non-UTC zone) = %q, want it to end in Z", got)
	}
}
