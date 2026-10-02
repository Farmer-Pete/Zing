package job

// prbody_test.go tests M3 task 5's pull request body renderer (design
// section 8.10, prbody.go): prBody, a pure function of the ticket id, the
// stored plan, the judge's selected final verdicts, the sealed scenario
// cohort, every build report, and every file event.

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// prTestSHA is a stand-in frozen head SHA: 40 lowercase hex digits, all
// VerdictArtifact.SHA's pattern requires. Its first seven characters,
// "abcdef1", are what every sha7 assertion below checks for.
const prTestSHA = "abcdef1234567890abcdef1234567890abcdef12"

// Path literals reused across this file's cases, pulled out as constants
// (matching reviewrules_test.go's own bareAGoPath convention) so goconst
// does not flag the repeats. bareAGoPath itself is reviewrules_test.go's.
const (
	bareBGoPath = "b.go"
	bareCGoPath = "c.go"
)

// -----------------------------------------------------------------------
// Pure: prBody
// -----------------------------------------------------------------------

// TestPRBodySections exercises every one of 8.10's seven fields together,
// against one plan with a goal, a demo, a declared file, and a deletion;
// one accepted extra; one landed build report with its own fence; and two
// scenarios, one behavior and one performance, each with a matching
// verdict.
func TestPRBodySections(t *testing.T) {
	t.Parallel()

	plan := response.Plan{
		Overview: response.Overview{
			Objective: "Ship widgets",
			Goals:     []string{"goal one", "goal two"},
		},
		Design: response.Design{
			Demo: response.Demo{Cmd: "make demo", Text: "widgets ship"},
		},
		Delivery: response.Delivery{
			Files: []response.FileChange{
				{Path: bareAGoPath, Action: response.FileActionCreate, Reason: "new file"},
			},
			Deletions: response.Deletions{
				Items: []response.Fence{
					{Path: bareBGoPath, Symbol: "Old", ExistedBecause: "existed because it was legacy"},
				},
			},
		},
	}

	scenarios := []response.Scenario{
		{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g1", When: "w1", Then: "t1"},
		{ID: "s2", Kind: response.ScenarioKindPerformance, Given: "g2", When: "w2", Then: "t2"},
	}
	verdicts := []response.VerdictArtifact{
		{Scenario: "s1", Result: response.ResultPass, Evidence: "ran scenario s1 and it passed", Kind: response.ScenarioKindBehavior, Round: 1, SHA: prTestSHA},
		{Scenario: "s2", Result: response.ResultFail, Evidence: "ran scenario s2 and it failed\nsecond line of detail", Kind: response.ScenarioKindPerformance, Round: 1, SHA: prTestSHA},
	}

	landedSHA := "1111111111111111111111111111111111111111"
	reports := []store.BuildReportRow{
		{
			ArtifactID: 1, RunID: 10,
			Report: response.BuildReport{
				TaskN:     1,
				Fences:    []response.Fence{{Path: bareCGoPath, Symbol: "Helper", ExistedBecause: "existed because it was shared"}},
				CommitSHA: &landedSHA,
			},
		},
	}

	acceptDecision := response.PerimeterAccept
	events := []store.FileEventRow{
		{
			ArtifactID: 2,
			File: response.FileArtifact{
				FileChange: response.FileChange{Path: "extra.go", Action: response.FileActionCreate, Reason: "builder's reason for extra"},
				Decision:   &acceptDecision,
			},
		},
	}

	got := prBody(42, plan, verdicts, scenarios, reports, events)

	want := orchestrator.PullRequest{
		Title:       "Ship widgets",
		What:        "Ship widgets\n\n- goal one\n- goal two",
		WorkingDemo: "`make demo`\n\nwidgets ship",
		Scenarios: "2 scenarios, judged on abcdef1: 1 passed, 1 failed.\n\n" +
			"| Scenario | Kind | Result |\n" +
			"| --- | --- | --- |\n" +
			"| s1 | behavior | pass |\n" +
			"| s2 | performance | fail |\n\n" +
			"Performance evidence:\n" +
			"- s2: ran scenario s2 and it failed",
		DeclaredFiles: "| Path | Action | Reason |\n" +
			"| --- | --- | --- |\n" +
			"| " + bareAGoPath + " | create | new file |\n" +
			"| extra.go | create | builder's reason for extra |",
		ChestertonsFence: "| Path | Symbol | Existed because |\n" +
			"| --- | --- | --- |\n" +
			"| " + bareBGoPath + " | Old | existed because it was legacy |\n" +
			"| " + bareCGoPath + " | Helper | existed because it was shared |",
		PlanLink: "The plan is ticket 42 in the Zing console.",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("prBody mismatch (-want +got):\n%s", diff)
	}
}

// TestPRBodyTitleCut drives 8.10's title rule: whitespace is collapsed
// first; at or under 256 runes the (collapsed) objective is untouched;
// past it, the title is cut at the last space at or before rune 253 and
// gets "..."; with no such space, the cut falls hard at rune 253.
func TestPRBodyTitleCut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		objective string
		want      string
	}{
		{
			name:      "whitespace collapses before the length check, so a short result is untouched",
			objective: "Ship   \t widgets\nfaster",
			want:      "Ship widgets faster",
		},
		{
			name:      "256 runes exactly is untouched",
			objective: strings.Repeat("a", 256),
			want:      strings.Repeat("a", 256),
		},
		{
			name:      "over 256 runes cuts at the last space at or before rune 253",
			objective: strings.Repeat("a", 250) + " " + strings.Repeat("b", 20),
			want:      strings.Repeat("a", 250) + "...",
		},
		{
			name:      "no space in the first 253 runes cuts hard at rune 253",
			objective: strings.Repeat("a", 300),
			want:      strings.Repeat("a", 253) + "...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := response.Plan{Overview: response.Overview{Objective: tc.objective, Goals: []string{"g"}}}
			got := prBody(1, plan, nil, nil, nil, nil)
			if got.Title != tc.want {
				t.Errorf("Title = %q, want %q", got.Title, tc.want)
			}
		})
	}
}

// TestPRBodyScenariosHideText asserts the Scenarios field never carries a
// scenario's own Given, When, or Then text (design 8.10: "Never given,
// when, or then"), only its id, kind, and the judge's result.
func TestPRBodyScenariosHideText(t *testing.T) {
	t.Parallel()

	plan := response.Plan{Overview: response.Overview{Objective: "x", Goals: []string{"g"}}}
	scenarios := []response.Scenario{
		{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "GIVEN_SECRET_TEXT", When: "WHEN_SECRET_TEXT", Then: "THEN_SECRET_TEXT"},
	}
	verdicts := []response.VerdictArtifact{
		{Scenario: "s1", Result: response.ResultPass, Evidence: "evidence text", Kind: response.ScenarioKindBehavior, Round: 1, SHA: prTestSHA},
	}

	got := prBody(1, plan, verdicts, scenarios, nil, nil)

	for _, leak := range []string{"GIVEN_SECRET_TEXT", "WHEN_SECRET_TEXT", "THEN_SECRET_TEXT"} {
		if strings.Contains(got.Scenarios, leak) {
			t.Errorf("Scenarios leaked %q:\n%s", leak, got.Scenarios)
		}
	}
	if !strings.Contains(got.Scenarios, "s1") || !strings.Contains(got.Scenarios, "behavior") || !strings.Contains(got.Scenarios, "pass") {
		t.Errorf("Scenarios missing the id, kind, or result:\n%s", got.Scenarios)
	}
}

// TestPRBodyPerformanceEvidence asserts the "Performance evidence:" block
// appears only when the cohort has a performance scenario, lists one line
// per performance verdict (first line of evidence only, cut to 300
// runes), and is absent when the cohort has none.
func TestPRBodyPerformanceEvidence(t *testing.T) {
	t.Parallel()

	plan := response.Plan{Overview: response.Overview{Objective: "x", Goals: []string{"g"}}}

	t.Run("no performance scenario, no block", func(t *testing.T) {
		t.Parallel()
		scenarios := []response.Scenario{{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g", When: "w", Then: "t"}}
		verdicts := []response.VerdictArtifact{
			{Scenario: "s1", Result: response.ResultPass, Evidence: "e", Kind: response.ScenarioKindBehavior, Round: 1, SHA: prTestSHA},
		}
		got := prBody(1, plan, verdicts, scenarios, nil, nil)
		if strings.Contains(got.Scenarios, "Performance evidence:") {
			t.Errorf("Scenarios has a performance block with no performance scenario:\n%s", got.Scenarios)
		}
	})

	t.Run("a performance scenario's evidence is cut to its first line and 300 runes", func(t *testing.T) {
		t.Parallel()
		longSecondLine := strings.Repeat("y", 400)
		firstLine := strings.Repeat("x", 320)
		scenarios := []response.Scenario{{ID: "s9", Kind: response.ScenarioKindPerformance, Given: "g", When: "w", Then: "t"}}
		verdicts := []response.VerdictArtifact{
			{Scenario: "s9", Result: response.ResultFail, Evidence: firstLine + "\n" + longSecondLine, Kind: response.ScenarioKindPerformance, Round: 1, SHA: prTestSHA},
		}
		got := prBody(1, plan, verdicts, scenarios, nil, nil)
		wantLine := "- s9: " + firstLine[:300]
		if !strings.Contains(got.Scenarios, "Performance evidence:\n"+wantLine) {
			t.Errorf("Scenarios missing the cut evidence line:\n%s", got.Scenarios)
		}
		if strings.Contains(got.Scenarios, longSecondLine) {
			t.Errorf("Scenarios leaked the evidence's second line:\n%s", got.Scenarios)
		}
	})
}

// TestPRBodyFencesDeduped asserts ChestertonsFence lists the plan's own
// deletions first, then every landed build report's fences, with a
// (path, symbol) repeat across either source dropped in favor of the
// plan's row, and an unlanded report's fences never appearing at all.
func TestPRBodyFencesDeduped(t *testing.T) {
	t.Parallel()

	plan := response.Plan{
		Overview: response.Overview{Objective: "x", Goals: []string{"g"}},
		Delivery: response.Delivery{
			Deletions: response.Deletions{
				Items: []response.Fence{
					{Path: bareAGoPath, Symbol: "Foo", ExistedBecause: "existed because of the plan's own reason"},
				},
			},
		},
	}

	landedSHA := "2222222222222222222222222222222222222222"
	reports := []store.BuildReportRow{
		{
			ArtifactID: 1,
			Report: response.BuildReport{
				Fences: []response.Fence{
					{Path: bareAGoPath, Symbol: "Foo", ExistedBecause: "existed because this report's text must be dropped"},
					{Path: bareBGoPath, Symbol: "Bar", ExistedBecause: "existed because of something else"},
				},
				CommitSHA: &landedSHA,
			},
		},
		{
			ArtifactID: 2,
			Report: response.BuildReport{
				Fences: []response.Fence{
					{Path: bareCGoPath, Symbol: "Baz", ExistedBecause: "existed because it never landed"},
				},
				CommitSHA: nil, // unlanded: never reported
			},
		},
	}

	got := prBody(1, plan, nil, nil, reports, nil)

	want := "| Path | Symbol | Existed because |\n" +
		"| --- | --- | --- |\n" +
		"| " + bareAGoPath + " | Foo | existed because of the plan's own reason |\n" +
		"| " + bareBGoPath + " | Bar | existed because of something else |"

	if got.ChestertonsFence != want {
		t.Errorf("ChestertonsFence mismatch:\ngot:  %q\nwant: %q", got.ChestertonsFence, want)
	}
	if strings.Contains(got.ChestertonsFence, bareCGoPath) {
		t.Errorf("ChestertonsFence leaked an unlanded report's fence:\n%s", got.ChestertonsFence)
	}
}

// TestPRBodyEscapesPipes asserts every table cell has "|" escaped as "\|"
// and its whitespace runs collapsed to one space, in both the declared
// files and Chesterton's fence tables (design 8.10).
func TestPRBodyEscapesPipes(t *testing.T) {
	t.Parallel()

	plan := response.Plan{
		Overview: response.Overview{Objective: "x", Goals: []string{"g"}},
		Delivery: response.Delivery{
			Files: []response.FileChange{
				{Path: bareAGoPath, Action: response.FileActionModify, Reason: "needs  this\tfile | for   the perimeter"},
			},
			Deletions: response.Deletions{
				Items: []response.Fence{
					{Path: bareBGoPath, Symbol: "Old", ExistedBecause: "existed  because\tof | a   removed  flag"},
				},
			},
		},
	}

	got := prBody(1, plan, nil, nil, nil, nil)

	if !strings.Contains(got.DeclaredFiles, "| "+bareAGoPath+` | modify | needs this file \| for the perimeter |`) {
		t.Errorf("DeclaredFiles did not escape and collapse its cell:\n%s", got.DeclaredFiles)
	}
	if !strings.Contains(got.ChestertonsFence, "| "+bareBGoPath+` | Old | existed because of \| a removed flag |`) {
		t.Errorf("ChestertonsFence did not escape and collapse its cell:\n%s", got.ChestertonsFence)
	}
}
