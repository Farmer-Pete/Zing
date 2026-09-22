package lens

import (
	"testing"
	"testing/fstest"

	zing "zing"
)

const (
	lensesDir   = "prompts/lenses"
	problemLens = "problem"
)

func TestLoad_EightRealLensesParse(t *testing.T) {
	t.Parallel()

	lenses, err := Load(zing.Assets, lensesDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(lenses) != 8 {
		t.Fatalf("len(lenses) = %d, want 8", len(lenses))
	}

	wantNames := []string{
		"correctness", "fidelity", "observability", problemLens,
		"quality", "security", "simplification", "tests",
	}
	for i, want := range wantNames {
		if lenses[i].Name != want {
			t.Errorf("lenses[%d].Name = %q, want %q (must be sorted)", i, lenses[i].Name, want)
		}
	}

	for _, l := range lenses {
		if l.Plan == "" {
			t.Errorf("lens %s: Plan is empty", l.Name)
		}
		if l.Name == problemLens {
			if l.Code != "" {
				t.Errorf("lens problem: Code = %q, want empty", l.Code)
			}
			continue
		}
		if l.Code == "" {
			t.Errorf("lens %s: Code is empty", l.Name)
		}
	}
}

func TestLoad_ProblemPlanSectionBodyIsExact(t *testing.T) {
	t.Parallel()

	lenses, err := Load(zing.Assets, lensesDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var problem *Lens
	for i := range lenses {
		if lenses[i].Name == problemLens {
			problem = &lenses[i]
		}
	}
	if problem == nil {
		t.Fatal("no problem lens found")
	}

	const wantFirstLine = "Ask, in this order:"
	if got := firstLine(problem.Plan); got != wantFirstLine {
		t.Errorf("problem.Plan first line = %q, want %q", got, wantFirstLine)
	}
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}

func TestParseLens_Fixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string // exact error; empty means Load must succeed
	}{
		{
			name:    "missing plan section",
			content: "## In code\nsome content\n",
			want:    `lens fixture: missing "## In a plan" section`,
		},
		{
			name:    "missing code section",
			content: "## In a plan\nsome content\n",
			want:    `lens fixture: missing "## In code" section`,
		},
		{
			name:    "duplicate heading",
			content: "## In a plan\na\n## In code\nb\n## In a plan\nc\n",
			want:    `lens fixture: duplicate "## In a plan" section`,
		},
		{
			name:    "preamble text before first heading",
			content: "some preamble\n## In a plan\na\n## In code\nb\n",
			want:    `lens fixture: content before first section`,
		},
		{
			name:    "reversed section order parses fine",
			content: "## In code\nb\n## In a plan\na\n",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{
				lensesDir + "/fixture.md": &fstest.MapFile{Data: []byte(tt.content)},
			}
			_, err := Load(fsys, lensesDir)

			if tt.want == "" {
				if err != nil {
					t.Errorf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error %q", tt.want)
			}
			if err.Error() != tt.want {
				t.Errorf("Load() = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

// TestLoad_EmptyDirIsAnError proves Load returns an error, not a nil error
// with zero lenses, when dir holds no *.md files.
func TestLoad_EmptyDirIsAnError(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		lensesDir + "/.keep": &fstest.MapFile{Data: []byte("")},
	}
	_, err := Load(fsys, lensesDir)
	want := "lens: no lens files in " + lensesDir
	if err == nil || err.Error() != want {
		t.Errorf("Load(empty dir) = %v, want %q", err, want)
	}
}

func TestParseLens_LeadingBlankLinesAllowedBeforeFirstHeading(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		lensesDir + "/fixture.md": &fstest.MapFile{Data: []byte("\n\n## In a plan\na\n## In code\nb\n")},
	}
	lenses, err := Load(fsys, lensesDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(lenses) != 1 || lenses[0].Plan != "a" || lenses[0].Code != "b" {
		t.Errorf("lenses = %+v, want one lens with Plan=a Code=b", lenses)
	}
}
