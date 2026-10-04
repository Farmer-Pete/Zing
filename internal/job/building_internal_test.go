package job

// building_internal_test.go tests building.go's own unexported pure
// functions that no seam in building_test.go (package job_test) can reach --
// the same reason planning_internal_test.go lives in package job instead of
// alongside it.

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"zing/internal/orchestrator"
	"zing/internal/response"
)

// TestUnitTitle proves the six-step commit-subject rule (design section
// 4.3): trim a leading marker, cut at an early sentence end, prefix "Task
// <n>: ", cut a long subject at a word boundary rather than mid-word, trim
// trailing punctuation, and fix a stray unbalanced backtick.
func TestUnitTitle(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			if got := unitTitle(tc.taskN, tc.text); got != tc.want {
				t.Errorf("unitTitle(%d, %q) = %q, want %q", tc.taskN, tc.text, got, tc.want)
			}
		})
	}
}

// pGoPath is the plan path shared by TestFuncLines' wrap subtests.
const pGoPath = "p.go"

// TestFuncLines proves design section 6.7's LAND helper: one line per
// plan.Design.Changes entry whose Path is in approved, in plan order, each
// "<symbol> (<path>): called by <callers>; calls <callees>", with every
// whitespace run in callers and callees collapsed to one space and the
// line wrapped on spaces into entries of at most funcLineWidth runes,
// continuation entries indented two spaces, never splitting a word.
func TestFuncLines(t *testing.T) {
	t.Parallel()
	t.Run("formats and filters by approved path, in plan order", func(t *testing.T) {
		t.Parallel()
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

	t.Run("wraps a long line at 72 runes", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("caller ", 40)
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: pGoPath, Symbol: "F", Callers: long, Callees: "c"},
		}}}
		got := funcLines(plan, []string{pGoPath})
		if len(got) == 0 {
			t.Fatalf("funcLines = %v, want at least one line", got)
		}
		wantWords := strings.Fields("F (p.go): called by " + collapseWhitespace(long) + "; calls c")
		gotWords := make([]string, 0, len(wantWords))
		for i, line := range got {
			if n := len([]rune(line)); n > 72 {
				t.Errorf("funcLines[%d] has %d runes, want at most 72", i, n)
			}
			if i > 0 && !strings.HasPrefix(line, "  ") {
				t.Errorf("funcLines[%d] = %q, want a two-space indent", i, line)
			}
			gotWords = append(gotWords, strings.Fields(line)...)
		}
		if !reflect.DeepEqual(gotWords, wantWords) {
			t.Errorf("funcLines words = %v, want %v", gotWords, wantWords)
		}
	})

	t.Run("keeps an overlong word whole", func(t *testing.T) {
		t.Parallel()
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: pGoPath, Symbol: "F", Callers: "a", Callees: "b " + strings.Repeat("x", 100) + " c"},
		}}}
		got := funcLines(plan, []string{pGoPath})
		overlong := "  " + strings.Repeat("x", 100)
		found := false
		for _, line := range got {
			if line == overlong {
				found = true
				continue
			}
			if n := len([]rune(line)); n > 72 {
				t.Errorf("funcLines entry %q has %d runes, want at most 72", line, n)
			}
		}
		if !found {
			t.Errorf("funcLines = %v, want an entry %q", got, overlong)
		}
		wantWords := strings.Fields("F (p.go): called by a; calls b " + strings.Repeat("x", 100) + " c")
		gotWords := make([]string, 0, len(wantWords))
		for _, line := range got {
			gotWords = append(gotWords, strings.Fields(line)...)
		}
		if !reflect.DeepEqual(gotWords, wantWords) {
			t.Errorf("funcLines words = %v, want %v", gotWords, wantWords)
		}
	})

	t.Run("forces a word that doesn't fit the continuation room onto its own entry", func(t *testing.T) {
		t.Parallel()
		word71 := strings.Repeat("x", 71)
		word70 := strings.Repeat("y", 70)
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: pGoPath, Symbol: "F", Callers: "c", Callees: "b " + word71 + " end71"},
			{Path: pGoPath, Symbol: "F", Callers: "c", Callees: "b " + word70 + " end70"},
		}}}
		got := funcLines(plan, []string{pGoPath, pGoPath})
		want71 := "  " + word71
		want70 := "  " + word70
		if !slices.Contains(got, want71) {
			t.Errorf("funcLines = %v, want an entry %q (73 runes: a 71-rune word doesn't fit the 70-rune room left after the two-space indent, so it stays whole and the entry runs past funcLineWidth)", got, want71)
		}
		if !slices.Contains(got, want70) {
			t.Errorf("funcLines = %v, want an entry %q (72 runes: a 70-rune word exactly fills the room left after the two-space indent)", got, want70)
		}
	})

	t.Run("wraps without splitting a word", func(t *testing.T) {
		t.Parallel()
		callees := strings.Repeat("abcdefghi ", 20)
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: pGoPath, Symbol: "F", Callers: "c", Callees: callees},
		}}}
		got := funcLines(plan, []string{pGoPath})

		line := "F (p.go): called by c; calls " + collapseWhitespace(callees)
		wantWords := strings.Fields(line)
		wantSet := make(map[string]bool, len(wantWords))
		for _, w := range wantWords {
			wantSet[w] = true
		}

		msg, err := orchestrator.CommitMessage{Title: "t", FuncLines: got}.Render()
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		// The body is "title\n\n" + func lines joined by "\n" + "\n\n" +
		// the trailer (commit.go's Render), with no fences here, so the
		// middle "\n\n"-delimited section is exactly the func lines.
		parts := strings.Split(msg, "\n\n")
		if len(parts) < 2 {
			t.Fatalf("Render() = %q, want a func-line section", msg)
		}
		renderedLines := strings.Split(parts[1], "\n")
		if !reflect.DeepEqual(renderedLines, got) {
			t.Errorf("rendered func lines = %v, want %v", renderedLines, got)
		}

		gotWords := make([]string, 0, len(wantWords))
		for i, l := range renderedLines {
			if n := len([]rune(l)); n > 72 {
				t.Errorf("rendered func line %d = %q has %d runes, want at most 72", i, l, n)
			}
			if i > 0 && !strings.HasPrefix(l, "  ") {
				t.Errorf("rendered func line %d = %q, want a two-space indent", i, l)
			}
			for w := range strings.FieldsSeq(l) {
				if !wantSet[w] {
					t.Fatalf("word %q in func line is not a word of the input line", w)
				}
				gotWords = append(gotWords, w)
			}
		}
		if !reflect.DeepEqual(gotWords, wantWords) {
			t.Errorf("funcLines words = %v, want %v", gotWords, wantWords)
		}
	})

	t.Run("leaves a short line's symbol and path whitespace untouched", func(t *testing.T) {
		t.Parallel()
		const spacedPath = "p  a.go"
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: spacedPath, Symbol: "F", Callers: "c", Callees: "d"},
		}}}
		got := funcLines(plan, []string{spacedPath})
		want := []string{"F (p  a.go): called by c; calls d"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("funcLines = %v, want %v", got, want)
		}
	})

	t.Run("empty approved set produces no lines", func(t *testing.T) {
		t.Parallel()
		plan := response.Plan{Design: response.Design{Changes: []response.Change{
			{Path: "greet.go", Symbol: "Greet", Callers: "main", Callees: "fmt"},
		}}}
		if got := funcLines(plan, nil); len(got) != 0 {
			t.Errorf("funcLines = %v, want none", got)
		}
	})
}
