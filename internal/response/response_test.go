package response

import (
	"slices"
	"testing"
)

// valueser is implemented by every enum type in this package.
type valueser interface{ Values() []string }

func TestValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enum valueser
		want []string
	}{
		{"Job", Job(""), []string{
			"classify", "planning", "planreview", "build", "perimeter", "review", "judge", "respond", "side",
		}},
		{"TicketState", TicketState(""), []string{
			"queued", "planning", "building", "reviewing", "judging", "shipping", "done", "escalated", "abandoned",
		}},
		{"Outcome", Outcome(""), []string{
			"bug", "feature", "questions", "ready", "children", "nothing_to_do", "ok", "question", "error",
		}},
		{"ClaimKind", ClaimKind(""), []string{"code", "env"}},
		{"ClaimVerdict", ClaimVerdict(""), []string{"true", "false", "unchecked"}},
		{"ScenarioKind", ScenarioKind(""), []string{"behavior", "negative", "performance"}},
		{"ChangeKind", ChangeKind(""), []string{"new", "modified"}},
		{"FileAction", FileAction(""), []string{"create", "modify", "delete"}},
		{"TestKind", TestKind(""), []string{"integration", "unit", "regression", "e2e"}},
		{"Lens", Lens(""), []string{
			"problem", "simplification", "correctness", "security", "fidelity", "tests", "quality", "observability",
		}},
		{"Severity", Severity(""), []string{"blocker", "major", "minor", "nit"}},
		{"ErrorCode", ErrorCode(""), []string{"plan_gap", "cannot_run", "environment", "other"}},
		{"QuestionKind", QuestionKind(""), []string{"question", "gate", "split", "perimeter", "review", "merge"}},
		{"QuestionState", QuestionState(""), []string{"open", "answered", "resolved"}},
		{"TaskState", TaskState(""), []string{"pending", "running", "done", "failed"}},
		{"Decision", Decision(""), []string{"accept", "reject", "drop", "discuss"}},
		{"Result", Result(""), []string{"pass", "fail"}},
		{"ThreadVerb", ThreadVerb(""), []string{"fix", "reply", "addressed"}},
	}

	if len(tests) != 18 {
		t.Fatalf("18 enums documented in the plan, got %d test cases", len(tests))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.enum.Values()
			if !slices.Equal(got, tt.want) {
				t.Errorf("%s.Values() = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestConstantIdentifiers is a compile-time check: every constant referenced
// here must exist with exactly this name and this underlying value, per the
// plan's naming rule (type name + PascalCase value).
func TestConstantIdentifiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"JobClassify", string(JobClassify), "classify"},
		{"JobPlanreview", string(JobPlanreview), "planreview"},
		{"JobRespond", string(JobRespond), "respond"},
		{"TicketStateQueued", string(TicketStateQueued), "queued"},
		{"OutcomeNothingToDo", string(OutcomeNothingToDo), "nothing_to_do"},
		{"OutcomeError", string(OutcomeError), "error"},
		{"ClaimKindCode", string(ClaimKindCode), "code"},
		{"TestKindE2e", string(TestKindE2e), "e2e"},
		{"SeverityBlocker", string(SeverityBlocker), "blocker"},
		{"ResultPass", string(ResultPass), "pass"},
		{"DecisionAccept", string(DecisionAccept), "accept"},
		{"ThreadVerbFix", string(ThreadVerbFix), "fix"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
