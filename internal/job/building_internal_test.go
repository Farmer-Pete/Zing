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

// TestUnitTitle proves the six-step commit-subject rule (design section
// 4.3): trim a leading marker, cut at an early sentence end, prefix "Task
// <n>: ", cut a long subject at a word boundary rather than mid-word, trim
// trailing punctuation, and fix a stray unbalanced backtick.
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
			name:  "falls back to the bare subject when the cut window holds no space past the prefix",
			taskN: 4,
			text:  strings.Repeat("x", 100),
			// The subject has no space past "Task 4: " itself within the
			// first 72 runes, so the cut lands on the prefix's own trailing
			// space and step 5 trims the rest away.
			want: "Task 4",
		},
		{
			name:  "the worked example (design section 4.3)",
			taskN: 1,
			text:  "Add the greeting file hello.txt so the project's test command, `test -f hello.txt`, passes.",
			want:  "Task 1: Add the greeting file hello.txt so the project's test command",
		},
		{
			name:  "keeps the text before an early sentence end",
			taskN: 5,
			text:  "Add hello.txt. Then wire it into main so the server responds on port 8080 with the greeting",
			want:  "Task 5: Add hello.txt",
		},
		{
			name:  "falls back to the bare subject for an empty first line",
			taskN: 6,
			text:  "",
			want:  "Task 6",
		},
		{
			name:  "removes an odd trailing backtick the cut leaves unbalanced",
			taskN: 7,
			text:  "Enable the `feature flag without a matching close and then keep going with filler words here",
			want:  "Task 7: Enable the feature flag without a matching close and then keep",
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
