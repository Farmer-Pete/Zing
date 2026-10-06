package job

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"zing/internal/response"
)

// TestCheckScenarioShape_RejectsProseGrep is a regression test for one of
// three #86 sealed-check failures: a check grepped a phrase of more than
// one word with grep -F or grep -q straight against a prompt file's prose,
// which prompts/planreview.md hard-wraps at about 72 columns, so the phrase
// spanned a line break and the grep never matched. Such a check is refused
// at planning time.
func TestCheckScenarioShape_RejectsProseGrep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		check string
	}{
		{name: "grep -qF against a .md file", check: `grep -qF 'two words' prompts/x.md`},
		{name: "grep -F against a .md file outside prompts/", check: `grep -F 'a b' docs/y.md`},
		{name: "double-quoted phrase against a prompts/ file", check: `grep -qF "two words" prompts/x.md`},
		{name: "grep -q without F against a .md file", check: `grep -q 'two words' docs/y.md`},
		{name: "grep -F against a non-.md prompts/ file", check: `grep -F 'two words' prompts/x.txt`},
		{name: "tr -d newline escape does not join", check: `tr -d '\n' < prompts/x.md | grep -qF 'two words'`},
		{name: "tr -d space class does not join", check: `tr -d '[:space:]' < prompts/x.md | grep -qF 'two words'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				{ID: "s1", Then: okThen, Check: tt.check},
				{ID: "s2", Then: okThen, Check: okCheck},
			}
			errs := checkScenarioShape(scenarios)
			if len(errs) != 1 || errs[0].Path != scenario0CheckPath || !strings.Contains(errs[0].Msg, "[:space:]") {
				t.Fatalf("checkScenarioShape = %+v, want one error on %s naming [:space:]", errs, scenario0CheckPath)
			}
		})
	}
}

// TestCheckScenarioShape_AllowsJoinedProseGrep covers the forms the rule
// above must not flag: the phrase's lines joined first (either tr form),
// a single-word phrase (which can't straddle a line break), and a
// multi-word phrase against a file the rule doesn't treat as prose.
func TestCheckScenarioShape_AllowsJoinedProseGrep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		check string
	}{
		{name: "tr -s space class join", check: `tr -s '[:space:]' ' ' < prompts/x.md | grep -qF 'two words'`},
		{name: "tr newline join", check: `tr '\n' ' ' < prompts/x.md | grep -qF 'two words'`},
		{name: "single word phrase over prose", check: `grep -qF 'word' prompts/x.md`},
		{name: "multi-word phrase over non-prose file", check: `grep -qF 'two words' main.go`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				{ID: "s1", Then: okThen, Check: tt.check},
				{ID: "s2", Then: okThen, Check: okCheck},
			}
			errs := checkScenarioShape(scenarios)
			if len(errs) != 0 {
				t.Fatalf("checkScenarioShape = %+v, want no errors", errs)
			}
		})
	}
}

// TestCheckScenarioShape_RejectsUnquotedGlob is a regression test for
// another of the three #86 failures: a check's unquoted glob,
// --include=*.go, made zsh (the judge agent's login shell) abort with "no
// matches found" before grep ran; a leading "!" then turned that abort
// into exit 0, a false pass. Such a check is refused at planning time.
func TestCheckScenarioShape_RejectsUnquotedGlob(t *testing.T) {
	t.Parallel()
	scenarios := []response.Scenario{
		{ID: "s1", Then: noMatchThen, Check: `! grep -rn foo . --include=*.go`},
		{ID: "s2", Then: okThen, Check: okCheck},
	}
	errs := checkScenarioShape(scenarios)
	if len(errs) != 1 || errs[0].Path != scenario0CheckPath || !strings.Contains(errs[0].Msg, "--include=*.go") {
		t.Fatalf("checkScenarioShape = %+v, want one error on %s naming --include=*.go", errs, scenario0CheckPath)
	}

	noneScenarios := []response.Scenario{
		{ID: "s1", Then: noMatchThen, Check: `! grep -rn foo . --include='*.go'`},
		{ID: "s2", Then: okThen, Check: `echo $?`},
		{ID: "s3", Then: okThen, Check: `go test -run 'TestX$' ./internal/job`},
	}
	if errs := checkScenarioShape(noneScenarios); len(errs) != 0 {
		t.Fatalf("checkScenarioShape = %+v, want no errors", errs)
	}
}

// TestCheckScenarioShape_UnquotedGlobKeepsQuotedSpace is a regression test
// for review finding r1f7: a shell word that opens a quote holding a
// space, such as grep "a b"*.go, must be reported whole. Before the fix,
// wordStart (and the word's own end) moved on every space, quoted or not,
// so the reported word was the mangled b"*.go instead of "a b"*.go.
func TestCheckScenarioShape_UnquotedGlobKeepsQuotedSpace(t *testing.T) {
	t.Parallel()
	scenarios := []response.Scenario{
		{ID: "s1", Then: okThen, Check: `grep "a b"*.go main.go`},
		{ID: "s2", Then: okThen, Check: okCheck},
	}
	errs := checkScenarioShape(scenarios)
	if len(errs) != 1 || errs[0].Path != scenario0CheckPath {
		t.Fatalf("checkScenarioShape = %+v, want one error on %s", errs, scenario0CheckPath)
	}
	if !strings.Contains(errs[0].Msg, `"a b"*.go`) {
		t.Fatalf("checkScenarioShape Msg = %q, want it to name the whole word \"a b\"*.go", errs[0].Msg)
	}
}

// TestCheckScenarioShape_UnquotedGlobIgnoresParamExpansion is a regression
// test for review thread t7c41ccb4b641b4ba: unquotedGlob flagged the "?" in
// "${VAR:?msg}" as an unquoted glob, a false positive, because the rune
// just before that "?" is ":", not "$". A bare glob character inside a
// "${...}" parameter expansion is not a filename glob at all, so it must
// not be flagged, while a real unquoted glob outside any "${...}" still
// must be.
func TestCheckScenarioShape_UnquotedGlobIgnoresParamExpansion(t *testing.T) {
	t.Parallel()
	scenarios := []response.Scenario{
		{ID: "s1", Then: okThen, Check: `VAR=${VAR:?msg}`},
		{ID: "s2", Then: okThen, Check: `echo ${a:-${b:?c}}`},
		{ID: "s3", Then: okThen, Check: okCheck},
	}
	if errs := checkScenarioShape(scenarios); len(errs) != 0 {
		t.Fatalf("checkScenarioShape = %+v, want no errors", errs)
	}

	withGlob := []response.Scenario{
		{ID: "s1", Then: noMatchThen, Check: `: "${VAR:?msg}"; grep -rn foo . --include=*.go`},
		{ID: "s2", Then: okThen, Check: okCheck},
	}
	errs := checkScenarioShape(withGlob)
	if len(errs) != 1 || errs[0].Path != scenario0CheckPath || !strings.Contains(errs[0].Msg, "--include=*.go") {
		t.Fatalf("checkScenarioShape = %+v, want one error on %s naming --include=*.go", errs, scenario0CheckPath)
	}
}

// TestPromptsNameCheckSafeForms proves planning-bug.md, planning-feature.md,
// and the tests lens all name the two safe forms that checkScenarioShape's
// rules above require: a quoted include glob, and a tr join ahead of a
// multi-word grep over prose. A planner reading any of these three prompts
// should see the exact form that passes the rule, not just that the rule
// exists.
func TestPromptsNameCheckSafeForms(t *testing.T) {
	t.Parallel()
	collapse := regexp.MustCompile(`\s+`)
	paths := []string{
		"lenses/tests.md",
		"planning-bug.md",
		"planning-feature.md",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			full := filepath.Join("..", "..", "prompts", p)
			raw, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("read %s: %v", full, err)
			}
			text := collapse.ReplaceAllString(string(raw), " ")
			if !strings.Contains(text, `tr -s '[:space:]' ' '`) {
				t.Errorf("%s does not name the safe join form tr -s '[:space:]' ' '", full)
			}
			if !strings.Contains(text, `--include='*.go'`) {
				t.Errorf("%s does not name the safe quoted glob --include='*.go'", full)
			}
		})
	}
}
