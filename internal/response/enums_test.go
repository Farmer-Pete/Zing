package response

import (
	"slices"
	"testing"
)

// TestParseSeverity_AcceptsExactlyTheFourClosedValues proves ParseSeverity
// round-trips every valid Severity and errors on anything else, including a
// near-miss (design section 4.4).
func TestParseSeverity_AcceptsExactlyTheFourClosedValues(t *testing.T) {
	t.Parallel()

	for _, want := range []Severity{SeverityBlocker, SeverityMajor, SeverityMinor, SeverityNit} {
		got, err := ParseSeverity(string(want))
		if err != nil {
			t.Errorf("ParseSeverity(%q): %v, want nil", want, err)
		}
		if got != want {
			t.Errorf("ParseSeverity(%q) = %q, want %q", want, got, want)
		}
	}
}

func TestParseSeverity_RejectsAnUnknownValue(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", "Blocker", "critical", "blockers", " blocker"} {
		if _, err := ParseSeverity(bad); err == nil {
			t.Errorf("ParseSeverity(%q): want an error, got nil", bad)
		}
	}
}

// TestSeverity_Rank_OrdersLowestToHighest proves Rank gives blocker the
// highest rank and nit the lowest, so two severities compare with <=
// (design section 4.4, 6.5's "at-or-below floor").
func TestSeverity_Rank_OrdersLowestToHighest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sev  Severity
		want int
	}{
		{SeverityBlocker, 3},
		{SeverityMajor, 2},
		{SeverityMinor, 1},
		{SeverityNit, 0},
	}
	for _, tt := range tests {
		if got := tt.sev.Rank(); got != tt.want {
			t.Errorf("%s.Rank() = %d, want %d", tt.sev, got, tt.want)
		}
	}

	if SeverityNit.Rank() >= SeverityMinor.Rank() ||
		SeverityMinor.Rank() >= SeverityMajor.Rank() ||
		SeverityMajor.Rank() >= SeverityBlocker.Rank() {
		t.Error("Rank() must strictly increase nit < minor < major < blocker")
	}
}

// TestEscalationCode_Values_IncludesPackage8Codes proves the two codes
// Package 8 adds (design section 4.1), sandbox_unavailable and
// replan_unsupported, appear in EscalationCode's own Values().
func TestEscalationCode_Values_IncludesPackage8Codes(t *testing.T) {
	t.Parallel()

	values := EscalationCode("").Values()
	for _, want := range []EscalationCode{EscalationCodeSandboxUnavailable, EscalationCodeReplanUnsupported} {
		found := false
		for _, v := range values {
			if v == string(want) {
				found = true
			}
		}
		if !found {
			t.Errorf("EscalationCode.Values() = %v, want it to contain %q", values, want)
		}
	}
}

// TestEscalationOrigin_Values_IncludesPackage8Origins proves the three
// origins Package 8 adds (design section 4.1), build, perimeter, and fix,
// appear in EscalationOrigin's own Values().
func TestEscalationOrigin_Values_IncludesPackage8Origins(t *testing.T) {
	t.Parallel()

	values := EscalationOrigin("").Values()
	for _, want := range []EscalationOrigin{EscalationOriginBuild, EscalationOriginPerimeter, EscalationOriginFix} {
		found := false
		for _, v := range values {
			if v == string(want) {
				found = true
			}
		}
		if !found {
			t.Errorf("EscalationOrigin.Values() = %v, want it to contain %q", values, want)
		}
	}
}

// TestEscalationCode_Values_IncludesPRClosed proves pr_closed, Package 9's
// own code (design decision D13), appears in EscalationCode's own Values().
func TestEscalationCode_Values_IncludesPRClosed(t *testing.T) {
	t.Parallel()

	values := EscalationCode("").Values()
	if !slices.Contains(values, string(EscalationCodePRClosed)) {
		t.Errorf("EscalationCode.Values() = %v, want it to contain %q", values, EscalationCodePRClosed)
	}
}

// TestEscalationOrigin_Values_IncludesPackage9Origins proves the four
// origins Package 9 adds (design section 4.1), review, judge, shipping, and
// respond, appear in EscalationOrigin's own Values().
func TestEscalationOrigin_Values_IncludesPackage9Origins(t *testing.T) {
	t.Parallel()

	values := EscalationOrigin("").Values()
	want := []EscalationOrigin{
		EscalationOriginReview, EscalationOriginJudge, EscalationOriginShipping, EscalationOriginRespond,
	}
	for _, w := range want {
		found := false
		for _, v := range values {
			if v == string(w) {
				found = true
			}
		}
		if !found {
			t.Errorf("EscalationOrigin.Values() = %v, want it to contain %q", values, w)
		}
	}
}
