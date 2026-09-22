package response

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update the committed golden templates in internal/response/testdata/render")

const renderGoldenDir = "testdata/render"

func goldenPath(job Job, outcome Outcome) string {
	return filepath.Join(renderGoldenDir, string(job)+"-"+string(outcome)+".xml")
}

// TestRenderTemplate_MatchesGolden is the golden test: run with -update to
// (re)write internal/response/testdata/render from RenderTemplate(); without
// it, every registered pair's rendered bytes must equal its committed golden
// exactly. It enumerates the same registry task 1 built (registry.go), so a
// pair added there gets a golden here without this test changing.
func TestRenderTemplate_MatchesGolden(t *testing.T) {
	if *update {
		for key := range registry {
			out, err := RenderTemplate(key.Job, key.Outcome)
			if err != nil {
				t.Fatalf("RenderTemplate(%s, %s): %v", key.Job, key.Outcome, err)
			}
			path := goldenPath(key.Job, key.Outcome)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir for %s: %v", path, err)
			}
			if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}

	for key := range registry {
		t.Run(string(key.Job)+"/"+string(key.Outcome), func(t *testing.T) {
			got, err := RenderTemplate(key.Job, key.Outcome)
			if err != nil {
				t.Fatalf("RenderTemplate(%s, %s): %v", key.Job, key.Outcome, err)
			}
			path := goldenPath(key.Job, key.Outcome)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run go test -run TestRenderTemplate_MatchesGolden -update)", path, err)
			}
			if !bytes.Equal([]byte(got), want) {
				t.Errorf("%s: rendered output differs from golden; run with -update\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}

	// Bidirectional: fail if a committed golden exists with no registered
	// pair to render it (an orphan golden left behind by a removed pair).
	walkErr := filepath.WalkDir(renderGoldenDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(renderGoldenDir, path)
		if err != nil {
			return err
		}
		job, outcome, ok := parseGoldenName(rel)
		if !ok {
			t.Errorf("%s: golden filename does not parse as <job>-<outcome>.xml", rel)
			return nil
		}
		if _, ok := registry[registryKey{job, outcome}]; !ok {
			t.Errorf("%s: golden file has no registered (job, outcome) pair to render it", rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", renderGoldenDir, walkErr)
	}
}

func parseGoldenName(name string) (job Job, outcome Outcome, ok bool) {
	name = strings.TrimSuffix(name, ".xml")
	before, after, found := strings.Cut(name, "-")
	if !found {
		return "", "", false
	}
	return Job(before), Outcome(after), true
}

// TestRenderTemplate_UnknownPair asserts an unregistered pair returns the
// registry's own "no response for job <job> outcome <outcome>" error
// (design section 7.2), not a renderer-invented string.
func TestRenderTemplate_UnknownPair(t *testing.T) {
	t.Parallel()

	_, gotErr := RenderTemplate(JobClassify, OutcomeReady)
	if gotErr == nil {
		t.Fatal("RenderTemplate(classify, ready) = nil error, want an error")
	}

	_, wantErr := Lookup(JobClassify, OutcomeReady)
	if wantErr == nil {
		t.Fatal("Lookup(classify, ready) = nil error, want an error (test setup broken)")
	}

	if gotErr.Error() != wantErr.Error() {
		t.Errorf("RenderTemplate error = %q, want Lookup's own error %q", gotErr.Error(), wantErr.Error())
	}
}

// TestRenderTemplate_SharedTypeDiffersOnlyInOutcome proves ClassifyResponse,
// registered for both (classify, bug) and (classify, feature), renders the
// same template for each pair except for the root outcome attribute (design
// section 6.8: "A shared type such as ClassifyResponse renders once per
// pair, differing only in the root outcome").
func TestRenderTemplate_SharedTypeDiffersOnlyInOutcome(t *testing.T) {
	t.Parallel()

	bug, err := RenderTemplate(JobClassify, OutcomeBug)
	if err != nil {
		t.Fatalf("RenderTemplate(classify, bug): %v", err)
	}
	feature, err := RenderTemplate(JobClassify, OutcomeFeature)
	if err != nil {
		t.Fatalf("RenderTemplate(classify, feature): %v", err)
	}

	bugAsFeature := strings.Replace(bug, `outcome="bug"`, `outcome="feature"`, 1)
	if bugAsFeature != feature {
		t.Errorf("classify templates differ by more than the root outcome attribute:\n--- bug (outcome swapped) ---\n%s\n--- feature ---\n%s", bugAsFeature, feature)
	}
	if bug == feature {
		t.Error("classify bug and feature templates are byte-identical; the root outcome attribute should differ")
	}
}

// TestRenderTemplate_NoneUnionShowsBothForms asserts Migrations and
// Deletions each render two example lines: the none="true" form and the
// populated-list form (design section 6.8).
func TestRenderTemplate_NoneUnionShowsBothForms(t *testing.T) {
	t.Parallel()

	out, err := RenderTemplate(JobPlanning, OutcomeReady)
	if err != nil {
		t.Fatalf("RenderTemplate(planning, ready): %v", err)
	}

	for _, name := range []string{"migrations", "deletions"} {
		noneForm := "<" + name + ` none="true"/>`
		if !strings.Contains(out, noneForm) {
			t.Errorf("planning/ready template missing the none-union none form %q", noneForm)
		}
		listForm := "<" + name + ">"
		if !strings.Contains(out, listForm) {
			t.Errorf("planning/ready template missing the none-union list form %q", listForm)
		}
	}
}

// TestRenderTemplate_QuestionOptionsSpecialNote asserts Question.Options
// renders the hardcoded "none, or two to four" note rather than a generic
// slice cardinality note (design section 6.8; Question.Options'
// jsonschema tag carries only maxItems=4, no minItems, so the generic
// mapping would say "at most 4").
func TestRenderTemplate_QuestionOptionsSpecialNote(t *testing.T) {
	t.Parallel()

	out, err := RenderTemplate(JobClassify, OutcomeQuestion)
	if err != nil {
		t.Fatalf("RenderTemplate(classify, question): %v", err)
	}
	if !strings.Contains(out, "none, or two to four") {
		t.Errorf("classify/question template missing the Options special note; got:\n%s", out)
	}
	if strings.Contains(out, "at most 4") {
		t.Errorf("classify/question template used the generic slice note instead of the special one; got:\n%s", out)
	}
}

// TestEscape_EscapesLessThanAndAmpersand proves the renderer's escaping
// rule (design section 6.8: "any literal < or & in a value is escaped")
// independently of the golden files, since no committed doc tag happens to
// contain either character, so the golden bytes alone would never catch a
// regression here.
func TestEscape_EscapesLessThanAndAmpersand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"a < b", "a &lt; b"},
		{"cmd && run", "cmd &amp;&amp; run"},
		{`nothing to escape here`, `nothing to escape here`},
		{`quotes "stay" as-is`, `quotes "stay" as-is`},
	}
	for _, tt := range tests {
		if got := escape(tt.in); got != tt.want {
			t.Errorf("escape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRenderTemplate_EachFieldShowsItsType spot-checks the field-type piece
// (design section 6.8's first comment piece) for a representative field of
// each named kind: string, int, bool, an enum, and a struct.
func TestRenderTemplate_EachFieldShowsItsType(t *testing.T) {
	t.Parallel()

	out, err := RenderTemplate(JobPlanning, OutcomeReady)
	if err != nil {
		t.Fatalf("RenderTemplate(planning, ready): %v", err)
	}

	tests := []struct {
		name string
		want string
	}{
		{"string (Overview.Objective)", "<objective>...</objective> <!-- string,"},
		{"int (Hypothesis.Rank)", `rank: int`},
		{"bool (Task.Demo)", `demo: bool`},
		{"enum (Scenario.Kind)", "kind: ScenarioKind, one of: behavior | negative | performance"},
		{"struct (Design.Demo)", `<demo cmd="...">...</demo> <!-- Demo;`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(out, tt.want) {
				t.Errorf("planning/ready template missing %q", tt.want)
			}
		})
	}
}

// TestRenderTemplate_EndsWithOneTrailingNewline pins the exact
// end-of-output whitespace design section 6.8 requires.
func TestRenderTemplate_EndsWithOneTrailingNewline(t *testing.T) {
	t.Parallel()

	out, err := RenderTemplate(JobClassify, OutcomeBug)
	if err != nil {
		t.Fatalf("RenderTemplate(classify, bug): %v", err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("output does not end with a newline")
	}
	if strings.HasSuffix(out, "\n\n") {
		t.Fatal("output ends with more than one trailing newline")
	}
}
