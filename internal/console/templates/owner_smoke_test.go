package templates

import (
	"strings"
	"testing"
)

// TestPlanViewListsOwnerSmoke proves PlanView's Owner smoke block (design
// section 6.9's Delivery.OwnerSmoke): a plan with items renders an "Owner
// smoke" heading and every item between the Tasks and Review headings, and a
// plan with none renders no such heading or text.
func TestPlanViewListsOwnerSmoke(t *testing.T) {
	t.Parallel()

	t.Run("with items", func(t *testing.T) {
		t.Parallel()
		plan := minimalRenderedPlan(nil, nil, false)
		plan.Delivery.OwnerSmoke = []string{"Open the inbox and see the badge", "Click Abandon and see the confirm"}

		var sb strings.Builder
		if err := PlanView(plan).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()

		tasksAt := strings.Index(got, "<h3>Tasks</h3>")
		smokeAt := strings.Index(got, "<h3>Owner smoke</h3>")
		reviewAt := strings.Index(got, "<h2>Review</h2>")
		headingsFound := tasksAt != -1 && smokeAt != -1 && reviewAt != -1
		if !headingsFound {
			t.Fatalf("want Tasks, Owner smoke, and Review headings all present; got:\n%s", got)
		}
		if tasksAt >= smokeAt || smokeAt >= reviewAt {
			t.Errorf("want Owner smoke heading after Tasks and before Review; got tasksAt=%d smokeAt=%d reviewAt=%d in:\n%s", tasksAt, smokeAt, reviewAt, got)
		}
		for _, item := range plan.Delivery.OwnerSmoke {
			if !strings.Contains(got, item) {
				t.Errorf("want item %q present; got:\n%s", item, got)
			}
		}
	})

	t.Run("without items", func(t *testing.T) {
		t.Parallel()
		plan := minimalRenderedPlan(nil, nil, false)

		var sb strings.Builder
		if err := PlanView(plan).Render(t.Context(), &sb); err != nil {
			t.Fatalf("PlanView.Render: %v", err)
		}
		got := sb.String()

		if strings.Contains(got, "Owner smoke") {
			t.Errorf("want no Owner smoke heading when OwnerSmoke is nil; got:\n%s", got)
		}
	})
}
