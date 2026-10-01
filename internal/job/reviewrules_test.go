package job

// reviewrules_test.go tests task 8's pure finding rules (design section
// 6.3, 6.7, reviewrules.go): ParseLocation, FilterFindings, DedupFindings,
// splitByFloor, selectLenses, renderFixFindings, and (since job already
// imports orchestrator, and this is where the plan lists its test)
// orchestrator.CountNoun.

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"zing/internal/orchestrator"
	"zing/internal/response"
)

// Location and id literals reused across this file's cases, pulled out as
// constants (matching internal/orchestrator's own aGoPath/testOwner
// convention) so goconst does not flag the repeats. wordFinding pairs with
// job's own FixKindFindings ("findings") for the CountNoun cases.
const (
	bareAGoPath     = "a.go"
	nestedAGoLine12 = "internal/x/a.go:12"
	aGoLine12       = "a.go:12"
	aGoLine44       = "a.go:44"
	aGoLine1        = "a.go:1"
	findingID1      = "r1f1"
	findingID2      = "r1f2"
	findingID3      = "r1f3"
	findingID4      = "r1f4"
	wordFinding     = "finding"
)

// -----------------------------------------------------------------------
// Pure: ParseLocation
// -----------------------------------------------------------------------

func TestParseLocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		loc      string
		wantPath string
		wantLine int
		wantOK   bool
	}{
		{"a plain path:line", nestedAGoLine12, "internal/x/a.go", 12, true},
		{"surrounding spaces trimmed", "  a.go:5  ", bareAGoPath, 5, true},
		{"path.Clean normalizes a leading ./", "./a.go:5", bareAGoPath, 5, true},
		{"path.Clean resolves an internal ..", "sub/../x.go:3", "x.go", 3, true},
		{"no colon at all", bareAGoPath, "", 0, false},
		{"leading slash is refused", "/etc/passwd:1", "", 0, false},
		{"a ../ prefix is refused", "../secret.go:1", "", 0, false},
		{"line zero is refused", "a.go:0", "", 0, false},
		{"a leading zero on the line is refused", "a.go:012", "", 0, false},
		{"a non-digit line is refused", "a.go:x", "", 0, false},
		{"empty string", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, line, ok := ParseLocation(tc.loc)
			if ok != tc.wantOK {
				t.Fatalf("ParseLocation(%q) ok = %v, want %v", tc.loc, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if path != tc.wantPath || line != tc.wantLine {
				t.Errorf("ParseLocation(%q) = (%q, %d), want (%q, %d)", tc.loc, path, line, tc.wantPath, tc.wantLine)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: FilterFindings
// -----------------------------------------------------------------------

// reviewDiffIndex is 6.3's own worked example diff index: internal/x/a.go
// has ranges [10,15] and [43,45]; old.go (a deletion) has [1,20].
func reviewDiffIndex() orchestrator.DiffIndex {
	return orchestrator.DiffIndex{
		"internal/x/a.go": {{From: 10, To: 15}, {From: 43, To: 45}},
		"old.go":          {{From: 1, To: 20}},
	}
}

// TestFilterFindings drives 6.3's own worked-example table, plus the
// fidelity-only plan_ref rule.
func TestFilterFindings(t *testing.T) {
	t.Parallel()
	idx := reviewDiffIndex()

	cases := []struct {
		name string
		f    response.Finding
		want bool
	}{
		{"inside the first range: kept", response.Finding{Lens: response.LensCorrectness, Location: nestedAGoLine12, Text: "t", Fix: "f"}, true},
		{"just past the first range: dropped, outside the diff", response.Finding{Lens: response.LensCorrectness, Location: "internal/x/a.go:16", Text: "t", Fix: "f"}, false},
		{"inside the second range: kept", response.Finding{Lens: response.LensCorrectness, Location: "internal/x/a.go:44", Text: "t", Fix: "f"}, true},
		{"a deleted file's old-side range: kept", response.Finding{Lens: response.LensCorrectness, Location: "old.go:7", Text: "t", Fix: "f"}, true},
		{"a file not in the diff: dropped", response.Finding{Lens: response.LensCorrectness, Location: "internal/x/b.go:3", Text: "t", Fix: "f"}, false},
		{"a bad location, no colon: dropped", response.Finding{Lens: response.LensCorrectness, Location: bareAGoPath, Text: "t", Fix: "f"}, false},
		{"a bad location, absolute path: dropped", response.Finding{Lens: response.LensCorrectness, Location: "/etc/passwd:1", Text: "t", Fix: "f"}, false},
		{"fidelity with an empty plan_ref: dropped", response.Finding{Lens: response.LensFidelity, Location: nestedAGoLine12, PlanRef: "  ", Text: "t", Fix: "f"}, false},
		{"fidelity with a plan_ref: kept", response.Finding{Lens: response.LensFidelity, Location: nestedAGoLine12, PlanRef: "plan/delivery/tasks/task[3]", Text: "t", Fix: "f"}, true},
		{"a non-fidelity finding needs no plan_ref", response.Finding{Lens: response.LensSecurity, Location: nestedAGoLine12, Text: "t", Fix: "f"}, true},
	}

	findings := make([]response.Finding, len(cases))
	for i, tc := range cases {
		findings[i] = tc.f
	}

	survivors := FilterFindings(findings, idx)
	survived := make(map[response.Finding]bool, len(survivors))
	for _, s := range survivors {
		survived[s] = true
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := survived[tc.f]; got != tc.want {
				t.Errorf("FilterFindings: survived = %v, want %v for %+v", got, tc.want, tc.f)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: DedupFindings
// -----------------------------------------------------------------------

// reviewLensOrder mirrors machine.toml's jobs.review.lenses.
var reviewLensOrder = []response.Lens{
	response.LensSimplification, response.LensCorrectness, response.LensSecurity,
	response.LensFidelity, response.LensTests, response.LensQuality, response.LensObservability,
}

// TestDedupFindings drives 6.3's own worked example: correctness and
// security both report a.go:12 (merged, highest severity, lens-ordered
// text); tests alone reports a.go:44 (merged count 1 overall).
func TestDedupFindings(t *testing.T) {
	t.Parallel()
	findings := []response.Finding{
		{Lens: response.LensSecurity, Severity: response.SeverityMajor, Location: aGoLine12, Text: "unchecked input", Fix: "validate input"},
		{Lens: response.LensCorrectness, Severity: response.SeverityMinor, Location: aGoLine12, Text: "nil map write", Fix: "add nil check"},
		{Lens: response.LensTests, Severity: response.SeverityNit, Location: aGoLine44, Text: "missing test", Fix: "add test"},
	}

	got := DedupFindings(findings, reviewLensOrder)

	want := []response.FindingArtifact{
		{
			Lens: response.LensCorrectness, Severity: response.SeverityMajor, Location: aGoLine12,
			Text:   "[correctness] nil map write\n\n[security] unchecked input",
			Fix:    "[correctness] add nil check\n\n[security] validate input",
			Lenses: []response.Lens{response.LensCorrectness, response.LensSecurity},
		},
		{
			Lens: response.LensTests, Severity: response.SeverityNit, Location: aGoLine44,
			Text: "[tests] missing test", Fix: "[tests] add test",
			Lenses: []response.Lens{response.LensTests},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("DedupFindings mismatch (-want +got):\n%s", diff)
	}
}

// TestDedupFindingsPlanRef proves PlanRef carries the fidelity lens's own
// value, and stays empty when fidelity did not report that location.
func TestDedupFindingsPlanRef(t *testing.T) {
	t.Parallel()
	findings := []response.Finding{
		{Lens: response.LensFidelity, Severity: response.SeverityMajor, Location: aGoLine1, PlanRef: "plan/delivery/tasks/task[1]", Text: "drifted", Fix: "match the plan"},
		{Lens: response.LensCorrectness, Severity: response.SeverityMinor, Location: aGoLine1, Text: "also here", Fix: "fix it"},
		{Lens: response.LensQuality, Severity: response.SeverityNit, Location: "b.go:2", Text: "style", Fix: "reword"},
	}

	got := DedupFindings(findings, reviewLensOrder)
	if len(got) != 2 {
		t.Fatalf("DedupFindings: got %d rows, want 2", len(got))
	}
	if got[0].Location != aGoLine1 || got[0].PlanRef != "plan/delivery/tasks/task[1]" {
		t.Errorf("a.go:1 PlanRef = %q, want the fidelity lens's own value", got[0].PlanRef)
	}
	if got[1].Location != "b.go:2" || got[1].PlanRef != "" {
		t.Errorf("b.go:2 PlanRef = %q, want empty: fidelity did not report this location", got[1].PlanRef)
	}
}

// -----------------------------------------------------------------------
// Pure: splitByFloor
// -----------------------------------------------------------------------

func severityRow(id string, sev response.Severity) response.FindingArtifact {
	return response.FindingArtifact{ID: id, Severity: sev, Location: aGoLine1, Lens: response.LensCorrectness, Lenses: []response.Lens{response.LensCorrectness}, Text: "t", Fix: "f"}
}

// TestSplitByFloor drives 6.3's floor table across the three floors the
// plan names: minor (the default), nit, and blocker.
func TestSplitByFloor(t *testing.T) {
	t.Parallel()
	rows := []response.FindingArtifact{
		severityRow(findingID1, response.SeverityNit),
		severityRow(findingID2, response.SeverityMinor),
		severityRow(findingID3, response.SeverityMajor),
		severityRow(findingID4, response.SeverityBlocker),
	}

	idsOf := func(rows []response.FindingArtifact) []string {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return ids
	}

	cases := []struct {
		name          string
		floor         response.Severity
		wantAtOrBelow []string
		wantAbove     []string
	}{
		{"floor minor: nit and minor go to a fix run", response.SeverityMinor, []string{findingID1, findingID2}, []string{findingID3, findingID4}},
		{"floor nit: only nit goes to a fix run", response.SeverityNit, []string{findingID1}, []string{findingID2, findingID3, findingID4}},
		{"floor blocker: everything goes to a fix run", response.SeverityBlocker, []string{findingID1, findingID2, findingID3, findingID4}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			atOrBelow, above := splitByFloor(rows, tc.floor)

			if gotIDs := idsOf(atOrBelow); !equalStrings(gotIDs, tc.wantAtOrBelow) {
				t.Errorf("atOrBelow ids = %v, want %v", gotIDs, tc.wantAtOrBelow)
			}
			if gotIDs := idsOf(above); !equalStrings(gotIDs, tc.wantAbove) {
				t.Errorf("above ids = %v, want %v", gotIDs, tc.wantAbove)
			}
			for _, row := range atOrBelow {
				if row.Decision == nil || *row.Decision != response.FindingAccept {
					t.Errorf("atOrBelow row %s Decision = %v, want accept", row.ID, row.Decision)
				}
			}
			for _, row := range above {
				if row.Decision != nil {
					t.Errorf("above row %s Decision = %v, want nil", row.ID, *row.Decision)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------
// Pure: selectLenses
// -----------------------------------------------------------------------

// TestSelectLenses drives 6.7's own worked example: round 1 runs every
// lens; round 2 runs correctness, security, and fidelity (tests is out
// because its finding was dropped, quality is out because b.go did not
// change).
func TestSelectLenses(t *testing.T) {
	t.Parallel()
	t.Run("round 1 is all seven, in machine.toml order", func(t *testing.T) {
		t.Parallel()
		got := selectLenses(1, reviewLensOrder, nil, nil)
		if diff := cmp.Diff(reviewLensOrder, got); diff != "" {
			t.Errorf("selectLenses(1, ...) mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("round 2: 6.7's worked example", func(t *testing.T) {
		t.Parallel()
		accept := response.FindingAccept
		drop := response.FindingDrop
		prev := []response.FindingArtifact{
			{
				ID: findingID1, Location: aGoLine12, Decision: &accept,
				Lenses: []response.Lens{response.LensCorrectness, response.LensSecurity},
			},
			{
				ID: findingID2, Location: aGoLine44, Decision: &drop,
				Lenses: []response.Lens{response.LensTests},
			},
			{
				ID: findingID3, Location: "b.go:9", Decision: &accept,
				Lenses: []response.Lens{response.LensQuality},
			},
		}
		changed := []string{bareAGoPath, "a_test.go"}

		got := selectLenses(2, reviewLensOrder, prev, changed)
		want := []response.Lens{response.LensCorrectness, response.LensSecurity, response.LensFidelity}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("selectLenses(2, ...) mismatch (-want +got):\n%s", diff)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: renderFixFindings
// -----------------------------------------------------------------------

// TestRenderFixFindings proves the block shape, the blank-line join, that a
// non-accept row is skipped, and that ordering is numeric id order (r1f10
// sorts after r1f2, not before it as a lexical string sort would place it).
func TestRenderFixFindings(t *testing.T) {
	t.Parallel()
	accept := response.FindingAccept
	drop := response.FindingDrop

	rows := []response.FindingArtifact{
		{ID: "r1f10", Severity: response.SeverityNit, Lens: response.LensTests, Location: "c.go:5", Text: "t4", Fix: "f4", Decision: &accept},
		{ID: findingID3, Severity: response.SeverityBlocker, Lens: response.LensSecurity, Location: "b.go:1", Text: "sql injection", Fix: "parameterize", Decision: &accept},
		{ID: findingID2, Severity: response.SeverityMajor, Lens: response.LensCorrectness, Location: "a.go:20", Text: "dropped one", Fix: "n/a", Decision: &drop},
		{ID: findingID1, Severity: response.SeverityMinor, Lens: response.LensCorrectness, Location: aGoLine12, Text: "nil map write", Fix: "add nil check", Decision: &accept},
	}

	want := "r1f1 minor correctness a.go:12\nnil map write\nFix: add nil check" +
		"\n\n" +
		"r1f3 blocker security b.go:1\nsql injection\nFix: parameterize" +
		"\n\n" +
		"r1f10 nit tests c.go:5\nt4\nFix: f4"

	if got := renderFixFindings(rows); got != want {
		t.Errorf("renderFixFindings mismatch:\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

// TestRenderFixFindingsAllDropped proves an empty result when nothing was
// accepted.
func TestRenderFixFindingsAllDropped(t *testing.T) {
	t.Parallel()
	drop := response.FindingDrop
	rows := []response.FindingArtifact{
		{ID: findingID1, Severity: response.SeverityNit, Lens: response.LensTests, Location: aGoLine1, Text: "t", Fix: "f", Decision: &drop},
	}
	if got := renderFixFindings(rows); got != "" {
		t.Errorf("renderFixFindings = %q, want empty", got)
	}
}

// -----------------------------------------------------------------------
// Pure: orchestrator.CountNoun
// -----------------------------------------------------------------------

// TestCountNoun proves orchestrator.CountNoun pairs CountWord's spelling
// with the right singular or plural noun (design section 6.4's review
// question, which uses it for finding/findings).
func TestCountNoun(t *testing.T) {
	t.Parallel()
	many := string(FixKindFindings)
	cases := []struct {
		n    int
		want string
	}{
		{1, "one " + wordFinding},
		{2, "two " + many},
		{3, "three " + many},
		{10, "10 " + many},
		{0, "0 " + many},
	}
	for _, tc := range cases {
		if got := orchestrator.CountNoun(tc.n, wordFinding, many); got != tc.want {
			t.Errorf("CountNoun(%d, %q, %q) = %q, want %q", tc.n, wordFinding, many, got, tc.want)
		}
	}
}
