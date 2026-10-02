package job

// judgerules_test.go tests M2 task 5's pure judge rules (design section
// 7.4, judgerules.go): CheckCoverage, applyCheckExit, JudgePasses,
// scrubScenarioText, and renderFixFailures.

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"zing/internal/response"
)

// testJudgeSHA is a stand-in frozen head SHA: a valid 40 lowercase hex
// digits, which is all VerdictArtifact.SHA's pattern requires.
const testJudgeSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// farewellThen is a scenario's "then" text reused across the scrub cases
// below: long enough to clear the 12-rune floor, and a literal the judge
// might plausibly quote back in its evidence.
const farewellThen = "the greeting ends with a farewell line"

// -----------------------------------------------------------------------
// Pure: CheckCoverage
// -----------------------------------------------------------------------

func TestCheckCoverage(t *testing.T) {
	t.Parallel()

	t.Run("7.4's worked example", func(t *testing.T) {
		t.Parallel()
		scenarios := []response.Scenario{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}
		verdicts := []response.Verdict{
			{Scenario: "s1", Result: response.ResultPass, Evidence: "ok"},
			{Scenario: "s1", Result: response.ResultFail, Evidence: "ok"},
			{Scenario: "s4", Result: response.ResultPass, Evidence: "ok"},
		}

		got := CheckCoverage(scenarios, verdicts)

		want := []string{
			"duplicate verdict for scenario s1",
			"verdict for unknown scenario s4",
			"missing verdict for scenario s2",
			"missing verdict for scenario s3",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("CheckCoverage mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("evidence empty after trimming", func(t *testing.T) {
		t.Parallel()
		scenarios := []response.Scenario{{ID: "s1"}, {ID: "s2"}}
		verdicts := []response.Verdict{
			{Scenario: "s1", Result: response.ResultPass, Evidence: "   "},
			{Scenario: "s2", Result: response.ResultPass, Evidence: "fine"},
		}

		got := CheckCoverage(scenarios, verdicts)

		want := []string{"empty evidence for scenario s1"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("CheckCoverage mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("one verdict per scenario, every evidence non-empty, is clean", func(t *testing.T) {
		t.Parallel()
		scenarios := []response.Scenario{{ID: "s1"}, {ID: "s2"}}
		verdicts := []response.Verdict{
			{Scenario: "s1", Result: response.ResultPass, Evidence: "fine"},
			{Scenario: "s2", Result: response.ResultFail, Evidence: "also fine"},
		}

		got := CheckCoverage(scenarios, verdicts)

		if len(got) != 0 {
			t.Errorf("CheckCoverage = %v, want no errors", got)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: applyCheckExit
// -----------------------------------------------------------------------

func TestApplyCheckExit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		exit       int
		wantResult response.Result
		wantNote   string
	}{
		{"exit 0 is pass", 0, response.ResultPass, "Zing re-ran the check command: exit 0."},
		{"exit 1 is fail", 1, response.ResultFail, "Zing re-ran the check command: exit 1."},
		{"exit -1 is a timeout and fails", -1, response.ResultFail, "Zing re-ran the check command: timed out after 10m."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			judged := response.VerdictArtifact{
				Scenario: "s2", Result: response.ResultPass, Evidence: "ran go test ./greet -run TestFarewell, saw PASS",
				Kind:  response.ScenarioKindBehavior,
				Round: 1,
				SHA:   testJudgeSHA,
			}

			got := applyCheckExit(judged, tc.exit)

			wantExit := tc.exit
			want := response.VerdictArtifact{
				Scenario:  "s2",
				Result:    tc.wantResult,
				Evidence:  judged.Evidence + "\n\n" + tc.wantNote,
				Kind:      response.ScenarioKindBehavior,
				Round:     1,
				SHA:       testJudgeSHA,
				CheckExit: &wantExit,
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("applyCheckExit mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: JudgePasses
// -----------------------------------------------------------------------

// TestJudgePasses drives 7.4's own table: a failing performance row never
// blocks the pass, and a failing behavior or negative row always does.
func TestJudgePasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		final        []response.VerdictArtifact
		wantPass     bool
		wantFailures []response.VerdictArtifact
	}{
		{
			name: "a failing performance row never blocks the pass",
			final: []response.VerdictArtifact{
				{Scenario: "s1", Result: response.ResultPass, Kind: response.ScenarioKindBehavior},
				{Scenario: "s2", Result: response.ResultPass, Kind: response.ScenarioKindNegative},
				{Scenario: "s3", Result: response.ResultFail, Kind: response.ScenarioKindPerformance},
			},
			wantPass:     true,
			wantFailures: nil,
		},
		{
			name: "a failing negative row fails the round",
			final: []response.VerdictArtifact{
				{Scenario: "s1", Result: response.ResultPass, Kind: response.ScenarioKindBehavior},
				{Scenario: "s2", Result: response.ResultFail, Kind: response.ScenarioKindNegative},
				{Scenario: "s3", Result: response.ResultPass, Kind: response.ScenarioKindPerformance},
			},
			wantPass: false,
			wantFailures: []response.VerdictArtifact{
				{Scenario: "s2", Result: response.ResultFail, Kind: response.ScenarioKindNegative},
			},
		},
		{
			name: "every performance row failing still passes",
			final: []response.VerdictArtifact{
				{Scenario: "s1", Result: response.ResultFail, Kind: response.ScenarioKindPerformance},
				{Scenario: "s2", Result: response.ResultFail, Kind: response.ScenarioKindPerformance},
				{Scenario: "s3", Result: response.ResultPass, Kind: response.ScenarioKindBehavior},
			},
			wantPass:     true,
			wantFailures: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotPass, gotFailures := JudgePasses(tc.final)
			if gotPass != tc.wantPass {
				t.Errorf("JudgePasses(...) pass = %v, want %v", gotPass, tc.wantPass)
			}
			if diff := cmp.Diff(tc.wantFailures, gotFailures); diff != "" {
				t.Errorf("JudgePasses(...) failures mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: scrubScenarioText
// -----------------------------------------------------------------------

func TestScrubScenarioText(t *testing.T) {
	t.Parallel()

	t.Run("7.4's worked example", func(t *testing.T) {
		t.Parallel()
		sc := response.Scenario{Then: farewellThen}
		evidence := "ran ./greeter; expected " + farewellThen + ", got none"

		got := scrubScenarioText(evidence, sc)

		want := "ran ./greeter; expected [scenario text removed], got none"
		if got != want {
			t.Errorf("scrubScenarioText(...) = %q, want %q", got, want)
		}
	})

	t.Run("a short field left alone", func(t *testing.T) {
		t.Parallel()
		sc := response.Scenario{When: "run it"} // 6 runes, under the 12-rune floor
		evidence := "steps: run it then check the log"

		got := scrubScenarioText(evidence, sc)

		if got != evidence {
			t.Errorf("scrubScenarioText(...) = %q, want the evidence unchanged: %q", got, evidence)
		}
	})

	t.Run("longest first, so a shorter field inside a longer one is not matched on its own", func(t *testing.T) {
		t.Parallel()
		sc := response.Scenario{
			Given: "the greeting ends with a farewell", // contained in Then below
			Then:  farewellThen,
		}
		evidence := "observed: " + farewellThen + " as printed"

		got := scrubScenarioText(evidence, sc)

		want := "observed: [scenario text removed] as printed"
		if got != want {
			t.Errorf("scrubScenarioText(...) = %q, want %q", got, want)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: renderFixFailures
// -----------------------------------------------------------------------

// TestRenderFixFailures proves cohort order (not failures' own order) and
// that each block's evidence is scrubbed against its own scenario.
func TestRenderFixFailures(t *testing.T) {
	t.Parallel()
	scenarios := []response.Scenario{
		{ID: "s1"},
		{ID: "s2", Then: farewellThen},
		{ID: "s3"},
	}
	failures := []response.VerdictArtifact{
		{Scenario: "s3", Evidence: "timeout observed"},
		{Scenario: "s2", Evidence: "expected " + farewellThen + ", got none"},
		{Scenario: "s1", Evidence: "off by one"},
	}

	got := renderFixFailures(failures, scenarios)

	want := "s1: off by one\n\ns2: expected [scenario text removed], got none\n\ns3: timeout observed"
	if got != want {
		t.Errorf("renderFixFailures(...) = %q, want %q", got, want)
	}
}
