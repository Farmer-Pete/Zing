package job

import (
	"testing"

	"zing/internal/response"
)

// TestCheckScenarioShape_RejectsGovernanceFileRead is a regression test for
// #225: a sealed check that read the root CLAUDE.md or AGENTS.md from the
// working copy saw the default branch's version inside the judge's
// checkout (design D5), not the branch's own, and escalated twice as an
// environment fault. Each offending check is refused at planning time,
// pointing at git show HEAD:FILE.
func TestCheckScenarioShape_RejectsGovernanceFileRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kind  response.ScenarioKind
		check string
	}{
		{"bare_agents", response.ScenarioKindBehavior, `grep -q x AGENTS.md`},
		{"dot_slash_claude", response.ScenarioKindBehavior, `grep -q x ./CLAUDE.md`},
		{"redirect_less_than", response.ScenarioKindBehavior, `wc -l < AGENTS.md`},
		{"double_quoted", response.ScenarioKindBehavior, `cat "CLAUDE.md"`},
		{"and_and", response.ScenarioKindBehavior, `test -f AGENTS.md && go test ./x`},
		{"git_log", response.ScenarioKindBehavior, `git log -- AGENTS.md`},
		{"host", response.ScenarioKindHost, `grep -q x AGENTS.md`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				{ID: "s1", Kind: tc.kind, Then: "reads the file", Check: tc.check},
				cleanScenario,
			}
			errs := checkScenarioShape(scenarios)
			if len(errs) != 1 {
				t.Fatalf("checkScenarioShape(%q) = %+v, want exactly one error", tc.check, errs)
			}
			if errs[0].Path != "scenarios/scenario[0]/check" {
				t.Errorf("checkScenarioShape(%q) error Path = %q, want %q", tc.check, errs[0].Path, "scenarios/scenario[0]/check")
			}
			if errs[0].Msg != governanceFileCheckMsg {
				t.Errorf("checkScenarioShape(%q) error Msg = %q, want %q", tc.check, errs[0].Msg, governanceFileCheckMsg)
			}
		})
	}
}

// TestCheckScenarioShape_AllowsGovernanceFileFromCommit checks that the
// governance-file rule only matches the root CLAUDE.md or AGENTS.md named
// bare or as ./NAME: reading the committed file with git show HEAD:FILE, a
// nested path, or a similarly named file all pass.
func TestCheckScenarioShape_AllowsGovernanceFileFromCommit(t *testing.T) {
	t.Parallel()
	checks := []string{
		`git show HEAD:AGENTS.md | grep -q x`,
		`git show HEAD:./CLAUDE.md | grep -q x`,
		`grep -q x docs/AGENTS.md`,
		`grep -q x internal/x/CLAUDE.md`,
		`grep -q x MYAGENTS.md`,
		`grep -q x AGENTS.mdx`,
	}
	for _, check := range checks {
		t.Run(check, func(t *testing.T) {
			t.Parallel()
			sc := response.Scenario{ID: "s1", Kind: response.ScenarioKindBehavior, Then: "reads the file", Check: check}
			for _, err := range checkScenarioRules(0, sc) {
				if err.Msg == governanceFileCheckMsg {
					t.Fatalf("checkScenarioRules(%q) flagged a committed or nested governance-file read, want it allowed", check)
				}
			}
		})
	}
}
