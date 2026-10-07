package job

import (
	"testing"

	"zing/internal/response"
)

// cleanScenario is the second scenario every TestCheckScenarioShape_RejectsRmForce
// cohort pairs with its offending scenario, only to satisfy checkScenarioShape's
// own minimum count of 2; its check never trips any rule.
var cleanScenario = response.Scenario{
	ID: "s2", Kind: response.ScenarioKindBehavior, Then: "builds cleanly",
	Check: `d=$(mktemp -d "$TMPDIR/s2-XXXXXX") && go test ./x`,
}

// TestCheckScenarioShape_RejectsRmForce is a regression test for #94: a
// sealed check that cleared its state with rm -f or rm -rf made the Codex
// judge refuse the command ("rm -f style commands are not permitted. Use a
// safer approach"), and the escalation gave no cause. Each offending check
// is refused at planning time, pointing at a $TMPDIR mktemp.
func TestCheckScenarioShape_RejectsRmForce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kind  response.ScenarioKind
		check string
	}{
		{"semicolon_tail", response.ScenarioKindBehavior, `rm -f "$TMPDIR/s2.json"; go test ./x`},
		{"rf", response.ScenarioKindBehavior, `rm -rf d`},
		{"fr", response.ScenarioKindBehavior, `rm -fr d`},
		{"and_capital_flag", response.ScenarioKindBehavior, `cd x && rm -Rf y`},
		{"command_substitution", response.ScenarioKindBehavior, `d=$(rm -f x)`},
		{"host", response.ScenarioKindHost, `rm -f x`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				{ID: "s1", Kind: tc.kind, Then: "clears its state", Check: tc.check},
				cleanScenario,
			}
			errs := checkScenarioShape(scenarios)
			if len(errs) != 1 {
				t.Fatalf("checkScenarioShape(%q) = %+v, want exactly one error", tc.check, errs)
			}
			if errs[0].Path != "scenarios/scenario[0]/check" {
				t.Errorf("checkScenarioShape(%q) error Path = %q, want %q", tc.check, errs[0].Path, "scenarios/scenario[0]/check")
			}
			if errs[0].Msg != rmForceCheckMsg {
				t.Errorf("checkScenarioShape(%q) error Msg = %q, want %q", tc.check, errs[0].Msg, rmForceCheckMsg)
			}
		})
	}
}

// TestCheckScenarioShape_AllowsNonForceRm checks that the rm -f rule only
// matches rm used as a command word with a flag cluster holding f: a
// $TMPDIR mktemp replacement, a similarly named command, a quoted occurrence, or
// an rm without -f all pass.
func TestCheckScenarioShape_AllowsNonForceRm(t *testing.T) {
	t.Parallel()
	checks := []string{
		`d=$(mktemp -d "$TMPDIR/s2-XXXXXX") && go test ./x`,
		`form -f x`,
		`grep -q 'rm -f' notes.txt`,
		`rm -r d`,
		`rm d`,
	}
	for _, check := range checks {
		t.Run(check, func(t *testing.T) {
			t.Parallel()
			sc := response.Scenario{ID: "s1", Kind: response.ScenarioKindBehavior, Then: "clears its state", Check: check}
			for _, err := range checkScenarioRules(0, sc) {
				if err.Msg == rmForceCheckMsg {
					t.Fatalf("checkScenarioRules(%q) flagged rm -f/-rf, want it allowed", check)
				}
			}
		})
	}
}
