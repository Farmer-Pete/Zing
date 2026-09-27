package response

import "testing"

// TestResolvesInPlan_RepeatedElementItselfResolves proves a finding located
// at a repeated element itself -- "plan/delivery/tasks/task[0]", the exact
// example Finding.Location's own doc tag gives -- resolves, so a review whose
// findings all target tasks is not silently treated as clean (design section
// 6.5; PR #23 review). A path a stored plan does not carry (including an
// out-of-range index or a hallucinated child) still does not resolve.
func TestResolvesInPlan_RepeatedElementItselfResolves(t *testing.T) {
	t.Parallel()

	xml := []byte(planXML())
	cases := []struct {
		name     string
		location string
		want     bool
	}{
		{"the plan root", "plan", true},
		{"the sole task element itself", "plan/delivery/tasks/task[0]", true},
		{"the sole file element itself", "plan/delivery/files/file[0]", true},
		{"a repeated leaf element itself", "plan/overview/goals/goal[0]", true},
		{"a scalar element", "plan/overview/objective", true},
		{"a task attribute", "plan/delivery/tasks/task[0]/n", true},
		{"an out-of-range task index", "plan/delivery/tasks/task[9]", false},
		{"a hallucinated child", "plan/delivery/nonexistent", false},
		{"a path outside the plan root", "cohort/scenarios/scenario[0]", false},
		{"the empty string", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolvesInPlan(xml, c.location); got != c.want {
				t.Errorf("ResolvesInPlan(%q) = %v, want %v", c.location, got, c.want)
			}
		})
	}
}
