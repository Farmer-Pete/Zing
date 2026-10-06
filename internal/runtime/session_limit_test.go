package runtime

import (
	"errors"
	"testing"
	"time"
)

// TestParseSessionLimit pins parseSessionLimit's grammar and next-instant
// rule (design section "Session-limit message grammar").
func TestParseSessionLimit(t *testing.T) {
	t.Parallel()

	now, err := time.Parse(time.RFC3339, "2026-10-05T15:05:00Z")
	if err != nil {
		t.Fatalf("parse now: %v", err)
	}

	parsedCases := []struct {
		name string
		msg  string
		want string
	}{
		{"hour and minute", sessionLimitPrefix + " · resets 12:20pm (America/New_York)", "2026-10-05T16:20:00Z"},
		{"bare hour am", sessionLimitPrefix + " · resets 5am (America/New_York)", "2026-10-06T09:00:00Z"},
		{"bare hour midnight", sessionLimitPrefix + " · resets 12am (UTC)", "2026-10-06T00:00:00Z"},
		{"equal to now rolls to next day", sessionLimitPrefix + " · resets 3:05pm (UTC)", "2026-10-06T15:05:00Z"},
		{"uppercase ampm", sessionLimitPrefix + " · resets 12:20PM (America/New_York)", "2026-10-05T16:20:00Z"},
	}
	for _, tc := range parsedCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sl, ok := parseSessionLimit(tc.msg, now)
			if !ok {
				t.Fatalf("parseSessionLimit(%q) ok = false, want true", tc.msg)
			}
			if !sl.Parsed {
				t.Errorf("Parsed = false, want true")
			}
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatalf("parse want: %v", err)
			}
			if !sl.ResetAt.Equal(want) {
				t.Errorf("ResetAt = %v, want %v", sl.ResetAt.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}

	fallbackCases := []struct {
		name string
		msg  string
	}{
		{"hour out of range", sessionLimitPrefix + " · resets 13:00pm (America/New_York)"},
		{"minute out of range", sessionLimitPrefix + " · resets 12:60pm (America/New_York)"},
		{"no zone", sessionLimitPrefix + " · resets 12:20pm"},
		{"unknown zone", sessionLimitPrefix + " · resets 12:20pm (Mars/Olympus)"},
		{"unparseable text", sessionLimitPrefix + " · resets soonish"},
	}
	for _, tc := range fallbackCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sl, ok := parseSessionLimit(tc.msg, now)
			if !ok {
				t.Fatalf("parseSessionLimit(%q) ok = false, want true", tc.msg)
			}
			if sl.Parsed {
				t.Errorf("Parsed = true, want false")
			}
			want := now.Add(sessionLimitFallback)
			if !sl.ResetAt.Equal(want) {
				t.Errorf("ResetAt = %v, want %v (now + 30m)", sl.ResetAt, want)
			}
		})
	}

	notSessionLimitCases := []struct {
		name string
		msg  string
	}{
		{"leading space", " " + sessionLimitPrefix + " · resets 12:20pm (America/New_York)"},
		{"leading newline", "\n" + sessionLimitPrefix + " · resets 12:20pm (America/New_York)"},
		{"weekly limit", "You've hit your weekly limit · resets 5am (UTC)"},
	}
	for _, tc := range notSessionLimitCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sl, ok := parseSessionLimit(tc.msg, now)
			if ok {
				t.Errorf("parseSessionLimit(%q) = %v, ok = true, want ok false", tc.msg, sl)
			}
		})
	}
}

// TestExitFailure_NonSessionLimitStaysExecError proves exitFailure only
// recognizes the session-limit prefix at the very first byte of the final
// message, and otherwise returns the plain ExecError every runtime
// returned before.
func TestExitFailure_NonSessionLimitStaysExecError(t *testing.T) {
	t.Parallel()

	now := time.Now()

	cases := []struct {
		name         string
		exitCode     int
		finalMessage string
	}{
		{"weekly limit message", 1, "You've hit your weekly limit · resets 5am (UTC)"},
		{"leading space before prefix", 1, " " + sessionLimitPrefix + " · resets 12:20pm (America/New_York)"},
		{"empty message", 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := exitFailure(tc.exitCode, tc.finalMessage, now)
			var execErr *ExecError
			if !errors.As(err, &execErr) {
				t.Fatalf("exitFailure(...) = %v, want *ExecError", err)
			}
			if execErr.ExitCode != tc.exitCode {
				t.Errorf("ExecError.ExitCode = %d, want %d", execErr.ExitCode, tc.exitCode)
			}
		})
	}

	sessionLimitMsg := sessionLimitPrefix + " · resets 12:20pm (America/New_York)"
	err := exitFailure(1, sessionLimitMsg, now)
	// errors.As, not the modernize-suggested errors.AsType: see exitCodeFrom
	// (errors.go) for why AsType's own discarded bool/error result trips
	// errcheck's check-blank here.
	var sl *SessionLimitError
	if !errors.As(err, &sl) { //nolint:modernize // see comment above
		t.Fatalf("exitFailure(...) = %v, want *SessionLimitError", err)
	}
}
