package job

// reviewrules_test.go tests task 8's pure finding rules (design section
// 6.3, 6.7, reviewrules.go): ParseLocation, FilterFindings, DedupFindings,
// splitByFloor, selectLenses, renderFixFindings, and (since job already
// imports orchestrator, and this is where the plan lists its test)
// orchestrator.CountNoun. It also tests FilterFindings' commit-text drop
// rule (ticket #10): a quality finding about pushed commit text can never
// be fixed by a fix run, so it is dropped and logged rather than kept.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/store"
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
	findingID10     = "r1f10"
	findingIDRound2 = "r2f1"
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
		{"a quality finding about a cut commit message: dropped", response.Finding{Lens: response.LensQuality, Location: nestedAGoLine12, Text: "commit message is cut mid-word", Fix: "f"}, false},
		{"a quality finding about commit authorship: dropped", response.Finding{Lens: response.LensQuality, Location: nestedAGoLine12, Text: "Co-Authored-By trailer names the wrong author", Fix: "f"}, false},
		{"a correctness finding mentioning the commit message builder: kept", response.Finding{Lens: response.LensCorrectness, Location: nestedAGoLine12, Text: "the commit message builder skips fences", Fix: "f"}, true},
		{"a quality finding unrelated to commit text: kept", response.Finding{Lens: response.LensQuality, Location: nestedAGoLine12, Text: "name tmp is not a plain word", Fix: "f"}, true},
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

// TestFilterFindingsLogsCommitTextDrop proves a dropped commit-text finding
// is logged with its lens, location, and the matched phrase, never the
// finding's own text. Not parallel: it calls slog.SetDefault to capture a
// log line, which swaps the process-wide default logger.
func TestFilterFindingsLogsCommitTextDrop(t *testing.T) {
	idx := reviewDiffIndex()

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	findings := []response.Finding{
		{Lens: response.LensQuality, Location: nestedAGoLine12, Text: "Co-Authored-By trailer names the wrong author", Fix: "f"},
	}

	got := FilterFindings(findings, idx)
	if len(got) != 0 {
		t.Fatalf("FilterFindings = %+v, want it dropped", got)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "review finding about pushed commit text dropped") {
		t.Errorf("log = %q, want it to contain the drop message", logged)
	}
	if !strings.Contains(logged, "lens=quality") {
		t.Errorf("log = %q, want lens=quality", logged)
	}
	if !strings.Contains(logged, "location="+nestedAGoLine12) {
		t.Errorf("log = %q, want location=%s", logged, nestedAGoLine12)
	}
	if !strings.Contains(logged, "phrase=co-authored-by") {
		t.Errorf("log = %q, want phrase=co-authored-by", logged)
	}
	if strings.Contains(logged, "trailer names the wrong author") {
		t.Errorf("log = %q, must not contain the finding text", logged)
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
		{ID: findingID10, Severity: response.SeverityNit, Lens: response.LensTests, Location: "c.go:5", Text: "t4", Fix: "f4", Decision: &accept},
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

// -----------------------------------------------------------------------
// Pure: droppedFindings
// -----------------------------------------------------------------------

// TestDroppedFindings proves the round cutoff, the held exclusion, and that
// a later row (TRIAGE's own decided row over ROUND's undecided one, or a
// decision later changed) wins over an earlier one for the same id (ticket
// 56).
func TestDroppedFindings(t *testing.T) {
	t.Parallel()
	drop := response.FindingDrop
	accept := response.FindingAccept

	rows := []store.FindingRow{
		{ArtifactID: 1, Finding: response.FindingArtifact{ID: findingID1, Location: aGoLine1, Round: 1}},
		{ArtifactID: 2, Finding: response.FindingArtifact{ID: findingID2, Location: aGoLine12, Round: 1, Decision: &drop}},
		{ArtifactID: 3, Finding: response.FindingArtifact{ID: "r1h1", Location: aGoLine44, Round: 1, Held: true, Decision: &drop}},
		{ArtifactID: 4, Finding: response.FindingArtifact{ID: findingIDRound2, Location: bareAGoPath + ":2", Round: 2, Decision: &drop}},
		{ArtifactID: 5, Finding: response.FindingArtifact{ID: findingID1, Location: aGoLine1, Round: 1, Decision: &drop}},
		{ArtifactID: 6, Finding: response.FindingArtifact{ID: findingID2, Location: aGoLine12, Round: 1, Decision: &accept}},
	}

	idsOf := func(rows []response.FindingArtifact) []string {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return ids
	}

	cases := []struct {
		name  string
		round int
		want  []string
	}{
		{"round 2: only r1f1 (r1f2's newest row accepted, r1h1 held, r2f1 not yet before round 2)", 2, []string{findingID1}},
		{"round 3: r1f1 then r2f1, id order", 3, []string{findingID1, findingIDRound2}},
		{"round 1: nothing is before round 1", 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := idsOf(droppedFindings(rows, tc.round))
			if !equalStrings(got, tc.want) {
				t.Errorf("droppedFindings(rows, %d) ids = %v, want %v", tc.round, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Pure: splitRepeated
// -----------------------------------------------------------------------

// TestSplitRepeated drives the worked example from the plan: a dropped row
// blocks its own normalized location regardless of the merged row's own
// severity (ticket 56, Q1); a dropped row whose file changed blocks
// nothing; a dropped row whose own SHA comparison failed (absent from
// changedBySHA) blocks nothing either (fail open, Q3); a merged row at a
// different line is untouched.
func TestSplitRepeated(t *testing.T) {
	t.Parallel()

	idsOf := func(rows []response.FindingArtifact) []string {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		return ids
	}

	t.Run("blocks only an unchanged location, fails open on an unresolved sha", func(t *testing.T) {
		t.Parallel()
		dropped := []response.FindingArtifact{
			{ID: findingID1, Location: greetGoLine5, SHA: "A"},
			{ID: findingID2, Location: "a.go:3", SHA: "A"},
			{ID: findingID3, Location: "c.go:7", SHA: "B"},
		}
		changedBySHA := map[string]map[string]bool{
			"A": {bareAGoPath: true},
		}
		merged := []response.FindingArtifact{
			{ID: findingIDRound2, Severity: response.SeverityBlocker, Location: "./greet.go:5"},
			{ID: "r2f2", Severity: response.SeverityMajor, Location: "a.go:3"},
			{ID: "r2f3", Severity: response.SeverityMajor, Location: "c.go:7"},
			{ID: "r2f4", Severity: response.SeverityMajor, Location: "greet.go:6"},
		}

		kept, repeats := splitRepeated(merged, dropped, changedBySHA)

		wantKept := []string{"r2f2", "r2f3", "r2f4"}
		if got := idsOf(kept); !equalStrings(got, wantKept) {
			t.Errorf("kept ids = %v, want %v", got, wantKept)
		}

		if len(repeats) != 1 {
			t.Fatalf("repeats = %+v, want exactly one", repeats)
		}
		if repeats[0].DroppedID != findingID1 || repeats[0].Location != greetGoLine5 {
			t.Errorf("repeats[0] = %+v, want {DroppedID: r1f1, Location: %s}", repeats[0], greetGoLine5)
		}
	})

	t.Run("lowest id wins when two dropped rows share a location", func(t *testing.T) {
		t.Parallel()
		dropped := []response.FindingArtifact{
			{ID: findingID10, Location: greetGoLine5, SHA: "A"},
			{ID: findingID2, Location: greetGoLine5, SHA: "A"},
		}
		changedBySHA := map[string]map[string]bool{
			"A": {},
		}
		merged := []response.FindingArtifact{
			{ID: findingIDRound2, Severity: response.SeverityMajor, Location: greetGoLine5},
		}

		kept, repeats := splitRepeated(merged, dropped, changedBySHA)

		if len(kept) != 0 {
			t.Errorf("kept = %+v, want none", kept)
		}
		if len(repeats) != 1 || repeats[0].DroppedID != findingID2 {
			t.Errorf("repeats = %+v, want exactly one with DroppedID %s", repeats, findingID2)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: renderDroppedInput
// -----------------------------------------------------------------------

// TestRenderDroppedInput proves the dropped findings input's own
// rendering (ticket 56): rows sort by id regardless of input order, a
// newline in Text collapses onto one line, Text beyond
// acceptedAtCapTextRunes is cut with "..." appended, and an empty dropped
// gives ok false with the zero NamedInput.
func TestRenderDroppedInput(t *testing.T) {
	t.Parallel()

	t.Run("two rows render in id order", func(t *testing.T) {
		t.Parallel()
		dropped := []response.FindingArtifact{
			{ID: findingID10, Location: aGoLine1, Text: "second by id"},
			{ID: findingID2, Location: aGoLine12, Text: "first by id"},
		}
		in, ok := renderDroppedInput(dropped)
		if !ok {
			t.Fatal("renderDroppedInput(dropped) ok = false, want true")
		}
		want := "- " + findingID2 + " " + aGoLine12 + " first by id\n" +
			"- r1f10 " + aGoLine1 + " second by id"
		if in.Text != want {
			t.Errorf("renderDroppedInput(dropped).Text = %q, want %q", in.Text, want)
		}
		if in.Label != droppedFindingsLabel {
			t.Errorf("renderDroppedInput(dropped).Label = %q, want %q", in.Label, droppedFindingsLabel)
		}
		if !in.Untrusted {
			t.Error("renderDroppedInput(dropped).Untrusted = false, want true")
		}
	})

	t.Run("a newline in text renders on one line", func(t *testing.T) {
		t.Parallel()
		in, ok := renderDroppedInput([]response.FindingArtifact{
			{ID: findingID1, Location: aGoLine1, Text: "first line\nsecond line"},
		})
		if !ok {
			t.Fatal("renderDroppedInput ok = false, want true")
		}
		want := "- " + findingID1 + " " + aGoLine1 + " first line second line"
		if in.Text != want {
			t.Errorf("renderDroppedInput(...).Text = %q, want %q", in.Text, want)
		}
	})

	t.Run("text beyond the cap is cut with an ellipsis", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("x", 250)
		in, ok := renderDroppedInput([]response.FindingArtifact{
			{ID: findingID1, Location: aGoLine1, Text: long},
		})
		if !ok {
			t.Fatal("renderDroppedInput ok = false, want true")
		}
		want := "- " + findingID1 + " " + aGoLine1 + " " + strings.Repeat("x", acceptedAtCapTextRunes) + "..."
		if in.Text != want {
			t.Errorf("renderDroppedInput(...).Text = %q, want %q", in.Text, want)
		}
	})

	t.Run("nil gives ok false", func(t *testing.T) {
		t.Parallel()
		in, ok := renderDroppedInput(nil)
		if ok {
			t.Errorf("renderDroppedInput(nil) ok = true, want false")
		}
		if in != (prompt.NamedInput{}) {
			t.Errorf("renderDroppedInput(nil) input = %+v, want the zero NamedInput", in)
		}
	})
}

// -----------------------------------------------------------------------
// Pure: renderAcceptedPerimeterInput
// -----------------------------------------------------------------------

// acceptedFileEvent builds a FileEventRow for a path decided at the file
// perimeter, for renderAcceptedPerimeterInput's cases below.
func acceptedFileEvent(artifactID int64, path, reason string, decision response.PerimeterDecision) store.FileEventRow {
	return store.FileEventRow{
		ArtifactID: artifactID,
		File: response.FileArtifact{
			FileChange: response.FileChange{Path: path, Action: response.FileActionCreate, Reason: reason},
			Decision:   &decision,
		},
	}
}

// perimeterQuestionMessage builds a MessageRow carrying a perimeter
// question payload naming items, for renderAcceptedPerimeterInput's cases
// below.
func perimeterQuestionMessage(t *testing.T, id int64, key string, kind response.QuestionKind, refs ...string) store.MessageRow {
	t.Helper()
	items := make([]response.Item, len(refs))
	for i, ref := range refs {
		items[i] = response.Item{Ref: ref, Text: "Builder: why"}
	}
	payload, err := json.Marshal(response.QuestionPayload{
		Key: key, Kind: kind, State: response.QuestionStateAnswered,
		Recommended: "Decide each file", Options: []response.Option{}, Items: items,
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	return store.MessageRow{ID: id, Type: msgTypeQuestion, Payload: payload}
}

// TestRenderAcceptedPerimeterInput proves the accepted perimeter files
// input's own rendering (ticket "the fidelity lens knows which out-of-plan
// files the owner accepted at the perimeter"): only a path whose newest
// file event is accepted appears, each line carries the perimeter
// question's key when one named the path, the reason is collapsed and cut
// at acceptedAtCapTextRunes, and a path with no accepted event, or no
// events at all, gives ok false.
func TestRenderAcceptedPerimeterInput(t *testing.T) {
	t.Parallel()

	t.Run("no events gives ok false", func(t *testing.T) {
		t.Parallel()
		in, ok := renderAcceptedPerimeterInput(nil, nil)
		if ok {
			t.Errorf("renderAcceptedPerimeterInput(nil, nil) ok = true, want false")
		}
		if in != (prompt.NamedInput{}) {
			t.Errorf("renderAcceptedPerimeterInput(nil, nil) input = %+v, want the zero NamedInput", in)
		}
	})

	t.Run("only a rejected path gives ok false", func(t *testing.T) {
		t.Parallel()
		events := []store.FileEventRow{acceptedFileEvent(1, "a.go", "why", response.PerimeterReject)}
		in, ok := renderAcceptedPerimeterInput(events, nil)
		if ok {
			t.Errorf("renderAcceptedPerimeterInput ok = true, want false")
		}
		if in != (prompt.NamedInput{}) {
			t.Errorf("renderAcceptedPerimeterInput input = %+v, want the zero NamedInput", in)
		}
	})

	t.Run("accepted then rejected for the same path gives ok false", func(t *testing.T) {
		t.Parallel()
		events := []store.FileEventRow{
			acceptedFileEvent(1, "a.go", "why", response.PerimeterAccept),
			acceptedFileEvent(2, "a.go", "why", response.PerimeterReject),
		}
		in, ok := renderAcceptedPerimeterInput(events, nil)
		if ok {
			t.Errorf("renderAcceptedPerimeterInput ok = true, want false")
		}
		if in != (prompt.NamedInput{}) {
			t.Errorf("renderAcceptedPerimeterInput input = %+v, want the zero NamedInput", in)
		}
	})

	t.Run("accepted with no question naming it", func(t *testing.T) {
		t.Parallel()
		events := []store.FileEventRow{acceptedFileEvent(1, "a.go", "why", response.PerimeterAccept)}
		in, ok := renderAcceptedPerimeterInput(events, nil)
		if !ok {
			t.Fatal("renderAcceptedPerimeterInput ok = false, want true")
		}
		want := "- a.go accepted: why"
		if in.Text != want {
			t.Errorf("renderAcceptedPerimeterInput(...).Text = %q, want %q", in.Text, want)
		}
		if in.Label != acceptedPerimeterLabel {
			t.Errorf("renderAcceptedPerimeterInput(...).Label = %q, want %q", in.Label, acceptedPerimeterLabel)
		}
		if !in.Untrusted {
			t.Error("renderAcceptedPerimeterInput(...).Untrusted = false, want true")
		}
	})

	t.Run("two accepted paths sort by path and carry the newest question's key", func(t *testing.T) {
		t.Parallel()
		events := []store.FileEventRow{
			acceptedFileEvent(1, "b.go", "why b", response.PerimeterAccept),
			acceptedFileEvent(2, "a.go", "why a", response.PerimeterAccept),
		}
		messages := []store.MessageRow{
			perimeterQuestionMessage(t, 1, "Q3", response.QuestionKindPerimeter, "a.go", "b.go"),
			perimeterQuestionMessage(t, 2, "Q9", response.QuestionKindPerimeter, "b.go"),
		}
		in, ok := renderAcceptedPerimeterInput(events, messages)
		if !ok {
			t.Fatal("renderAcceptedPerimeterInput ok = false, want true")
		}
		want := "- a.go accepted at Q3: why a\n" +
			"- b.go accepted at Q9: why b"
		if in.Text != want {
			t.Errorf("renderAcceptedPerimeterInput(...).Text = %q, want %q", in.Text, want)
		}
		if in.Label != acceptedPerimeterLabel {
			t.Errorf("renderAcceptedPerimeterInput(...).Label = %q, want %q", in.Label, acceptedPerimeterLabel)
		}
		if !in.Untrusted {
			t.Error("renderAcceptedPerimeterInput(...).Untrusted = false, want true")
		}
	})

	t.Run("a long reason with an embedded newline is collapsed and cut", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("x", 150) + "\n" + strings.Repeat("y", 150)
		events := []store.FileEventRow{acceptedFileEvent(1, "a.go", long, response.PerimeterAccept)}
		in, ok := renderAcceptedPerimeterInput(events, nil)
		if !ok {
			t.Fatal("renderAcceptedPerimeterInput ok = false, want true")
		}
		want := "- a.go accepted: " + strings.Repeat("x", 150) + " " + strings.Repeat("y", 49) + "..."
		if in.Text != want {
			t.Errorf("renderAcceptedPerimeterInput(...).Text = %q, want %q", in.Text, want)
		}
	})

	t.Run("a non-perimeter question naming the path is ignored", func(t *testing.T) {
		t.Parallel()
		events := []store.FileEventRow{acceptedFileEvent(1, "a.go", "why", response.PerimeterAccept)}
		messages := []store.MessageRow{
			perimeterQuestionMessage(t, 1, "Q1", response.QuestionKindQuestion, "a.go"),
		}
		in, ok := renderAcceptedPerimeterInput(events, messages)
		if !ok {
			t.Fatal("renderAcceptedPerimeterInput ok = false, want true")
		}
		want := "- a.go accepted: why"
		if in.Text != want {
			t.Errorf("renderAcceptedPerimeterInput(...).Text = %q, want %q", in.Text, want)
		}
	})
}
