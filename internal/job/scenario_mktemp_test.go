package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// TestCheckScenarioShape_RejectsBareMktemp is a regression test for #245 s6
// and #244 s10: a sealed check that calls mktemp without a "$TMPDIR/"
// template ignores TMPDIR on macOS, writes to the per-user temp folder
// instead, and every write under it fails inside the sandbox -- either
// failing the check for no visible reason, or, as in #245 s6, comparing two
// empty files and passing having compared nothing. Each offending check is
// refused at planning time, pointing at the safe form.
func TestCheckScenarioShape_RejectsBareMktemp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		check string
	}{
		{"s6_judge", `d=$(mktemp -d); echo a > "$d/list1"; echo b > "$d/list2"; comm -3 "$d/list1" "$d/list2"`},
		{"plain", `mktemp`},
		{"backtick", "d=`mktemp -d`"},
		{"after_and", `cd x && mktemp -d`},
		{"t_flag", `mktemp -t foo`},
		{"p_flag", `mktemp -d -p "$TMPDIR" x.XXXXXX`},
		{"long_tmpdir", `mktemp --tmpdir x.XXXXXX`},
		{"long_tmpdir_value", `mktemp --tmpdir="$TMPDIR" x.XXXXXX`},
		{"default_expansion", `mktemp -d "${TMPDIR:-/tmp}/x-XXXXXX"`},
		{"other_dir", `mktemp -d "$HOME/x-XXXXXX"`},
		{"single_quoted", `mktemp -d '$TMPDIR/x-XXXXXX'`},
		{"second_call_bare", `d=$(mktemp -d "$TMPDIR/x-XXXXXX"); e=$(mktemp -d)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				{ID: "s1", Kind: response.ScenarioKindBehavior, Then: "compares two lists", Check: tc.check},
				cleanScenario,
			}
			errs := checkScenarioShape(scenarios)
			if len(errs) != 1 {
				t.Fatalf("checkScenarioShape(%q) = %+v, want exactly one error", tc.check, errs)
			}
			if errs[0].Path != "scenarios/scenario[0]/check" {
				t.Errorf("checkScenarioShape(%q) error Path = %q, want %q", tc.check, errs[0].Path, "scenarios/scenario[0]/check")
			}
			if errs[0].Msg != mktempCheckMsg {
				t.Errorf("checkScenarioShape(%q) error Msg = %q, want %q", tc.check, errs[0].Msg, mktempCheckMsg)
			}
		})
	}
}

// TestCheckScenarioShape_AllowsTmpdirMktemp checks that the mktemp rule only
// refuses a call without a "$TMPDIR/" template: the four accepted
// spellings, other long options, a quoted mention, a similarly named
// command, and a host check all pass.
func TestCheckScenarioShape_AllowsTmpdirMktemp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kind  response.ScenarioKind
		check string
	}{
		{"quoted", response.ScenarioKindBehavior, `d=$(mktemp -d "$TMPDIR/x-XXXXXX")`},
		{"unquoted", response.ScenarioKindBehavior, `d=$(mktemp -d $TMPDIR/x-XXXXXX)`},
		{"braced_quoted", response.ScenarioKindBehavior, `d=$(mktemp -d "${TMPDIR}/x-XXXXXX")`},
		{"braced", response.ScenarioKindBehavior, `d=$(mktemp -d ${TMPDIR}/x-XXXXXX)`},
		{"file_form", response.ScenarioKindBehavior, `f=$(mktemp "$TMPDIR/f-XXXXXX")`},
		{"long_directory", response.ScenarioKindBehavior, `mktemp --directory "$TMPDIR/x-XXXXXX"`},
		{"long_quiet", response.ScenarioKindBehavior, `mktemp --quiet -d "$TMPDIR/x-XXXXXX"`},
		{"quoted_mention", response.ScenarioKindBehavior, `grep -q 'mktemp -d' notes.txt`},
		{"similar_word", response.ScenarioKindBehavior, `mktempfoo`},
		{"host", response.ScenarioKindHost, `d=$(mktemp -d)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := response.Scenario{ID: "s1", Kind: tc.kind, Then: "compares two lists", Check: tc.check}
			for _, err := range checkScenarioRules(0, sc) {
				if err.Msg == mktempCheckMsg {
					t.Fatalf("checkScenarioRules(%q) flagged mktemp, want it allowed", tc.check)
				}
			}
		})
	}
}

// TestJudgeAmendment_RefusesBareMktemp is a regression test for #245 s6: a
// judge amendment whose check calls mktemp without a "$TMPDIR/" template
// goes through the same checkScenarioRules as a planned scenario, so it is
// refused before the owner ever sees it.
func TestJudgeAmendment_RefusesBareMktemp(t *testing.T) {
	t.Parallel()
	scenarios := []response.Scenario{
		{ID: "s1", Kind: response.ScenarioKindBehavior, Given: "g1", When: "w1", Then: "t1", Check: pbNoopShellCmd},
		{ID: "s2", Kind: response.ScenarioKindBehavior, Given: "g2", When: "w2", Then: "t2", Check: pbNoopShellCmd},
	}
	_, amended, refusal := judgeAmendment(scenarios, response.Amendment{
		Scenario: "s1", Given: "g1", When: "w1", Then: "t1",
		Check:  `d=$(mktemp -d); echo a > "$d/list1"; echo b > "$d/list2"; comm -3 "$d/list1" "$d/list2"`,
		Reason: "clears its state first",
	})
	if !strings.Contains(refusal, mktempCheckMsg) {
		t.Errorf("refusal = %q, want it to contain %q", refusal, mktempCheckMsg)
	}
	var zero response.Amendment
	if amended != zero {
		t.Errorf("amended = %+v, want the zero value", amended)
	}
}
