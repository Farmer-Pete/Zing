package job

// building_internal_test.go tests building.go's own unexported pure
// functions that no seam in building_test.go (package job_test) can reach --
// the same reason planning_internal_test.go lives in package job instead of
// alongside it.

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// TestUnitTitle proves the commit-subject rule (design section 4.3): "Task
// <n>: " plus the first line of the task text with leading "#", "*", "-",
// and spaces trimmed, cut to 72 runes.
func TestUnitTitle(t *testing.T) {
	cases := []struct {
		name  string
		taskN int
		text  string
		want  string
	}{
		{
			name:  "trims a markdown heading prefix",
			taskN: 1,
			text:  "# Add the greeting file\nmore task detail on later lines",
			want:  "Task 1: Add the greeting file",
		},
		{
			name:  "trims a bullet prefix",
			taskN: 2,
			text:  "- Add the greet package with a Greet function",
			want:  "Task 2: Add the greet package with a Greet function",
		},
		{
			name:  "trims a star prefix and repeated leading spaces",
			taskN: 3,
			text:  "**  Add greet_test.go",
			want:  "Task 3: Add greet_test.go",
		},
		{
			name:  "cuts the whole subject to 72 runes",
			taskN: 4,
			text:  strings.Repeat("x", 100),
			// "Task 4: " is 8 runes; 72-8 = 64 runes of the task text survive.
			want: "Task 4: " + strings.Repeat("x", 64),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitTitle(tc.taskN, tc.text); got != tc.want {
				t.Errorf("unitTitle(%d, %q) = %q, want %q", tc.taskN, tc.text, got, tc.want)
			}
		})
	}
}

// TestFuncLines proves design section 6.7's LAND helper: one line per
// plan.Design.Changes entry whose Path is in approved, in plan order, each
// "<symbol> (<path>): called by <callers>; calls <callees>", with every
// whitespace run in callers and callees collapsed to one space and the
// whole line cut to 160 runes.
func TestFuncLines(t *testing.T) {
	t.Run("formats and filters by approved path, in plan order", func(t *testing.T) {
		const (
			greetPath = "greet.go"
			greetTest = "greet_test.go"
			greetSym  = "Greet"
		)
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: greetPath, Symbol: greetSym, Callers: "main.go   \n  index", Callees: "fmt.Println"},
			{Path: "unapproved.go", Symbol: "Unused", Callers: "x", Callees: "y"},
			{Path: greetTest, Symbol: "TestGreet", Callers: "go test", Callees: greetSym},
		}}}
		got := funcLines(plan, []string{greetTest, greetPath})
		want := []string{
			"Greet (greet.go): called by main.go index; calls fmt.Println",
			"TestGreet (greet_test.go): called by go test; calls Greet",
		}
		if len(got) != len(want) {
			t.Fatalf("funcLines = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("funcLines[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("cuts a long line to 160 runes", func(t *testing.T) {
		long := strings.Repeat("caller ", 40)
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: "p.go", Symbol: "F", Callers: long, Callees: "c"},
		}}}
		got := funcLines(plan, []string{"p.go"})
		if len(got) != 1 {
			t.Fatalf("funcLines = %v, want exactly one line", got)
		}
		if n := len([]rune(got[0])); n > 160 {
			t.Errorf("funcLines[0] has %d runes, want at most 160", n)
		}
	})

	t.Run("empty approved set produces no lines", func(t *testing.T) {
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: "greet.go", Symbol: "Greet", Callers: "main", Callees: "fmt"},
		}}}
		if got := funcLines(plan, nil); len(got) != 0 {
			t.Errorf("funcLines = %v, want none", got)
		}
	})
}
