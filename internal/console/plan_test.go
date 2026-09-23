package console_test

import (
	"context"
	"strings"
	"testing"

	"zing/internal/console"
	"zing/internal/response"
)

// planChangePath, planMigrationFile, and planTestName are fixture string
// literals fixturePlan writes and this file's (and views_test.go's wiring
// test's) assertions read back, named once so goconst's repeated-literal
// guard has one definition to point at.
const (
	planChangePath    = "internal/console/plan.go"
	planMigrationFile = "0003_plan_index.sql"
	planTestName      = "TestRenderPlanEveryField"
)

// fixturePlan returns a response.Plan with every optional and nested field
// populated (design section 6.9, the plan renderer test's fixture): a
// bug-style Problem (Loop, Repro, ranked Hypotheses), a Change, a TypeDef
// with a Transition, a Migration, a Deletion, a Test, a Task, and a mermaid
// fence inside Shape, so TestRenderPlanEveryField exercises every branch
// the field-to-render map lists.
func fixturePlan() response.Plan {
	return response.Plan{
		Overview: response.Overview{
			Objective: "Ship a plan renderer that drops nothing.",
			Context:   "This lands in **internal/console**, the read side of the store.",
			Problem: response.Problem{
				Text: "The gate question shows a placeholder instead of the stored plan.",
				Loop: &response.Loop{
					Cmd:  "go test ./internal/console/... -run TestRenderPlan",
					Text: "fails: no plan renderer exists yet",
				},
				Repro: "1. seed a gate question\n2. open the thread\n3. the plan region stays a placeholder",
				Hypotheses: []response.Hypothesis{
					{Rank: 1, Cause: "no RenderPlan function exists", Prediction: "adding one replaces the placeholder"},
					{Rank: 2, Cause: "the gate context never calls it", Prediction: "wiring it in fixes the region"},
				},
			},
			Goals:    []string{"Render every Plan field", "Reuse the Task 5 markdown helper"},
			NonGoals: []string{"Recomputing files or tasks into rows of their own"},
		},
		Design: response.Design{
			Demo: response.Demo{
				Cmd:  "go test ./internal/console/... -run TestRenderPlanEveryField",
				Text: "the fixture plan renders every field",
			},
			Shape: "The renderer walks a Plan into HTML.\n\n```mermaid\ngraph TD\nPlan-->Overview\nPlan-->Design\n```",
			Changes: []response.Change{
				{
					Path: planChangePath, Symbol: "RenderPlan", Kind: response.ChangeKindNew,
					Callers: "views.go's threadComponent", Callees: "Render, templates.PlanView",
					Before: "", After: "func RenderPlan(plan response.Plan) (templ.Component, error)",
					SimpleCall: "console.RenderPlan(plan)",
				},
			},
			Types: []response.TypeDef{
				{
					Name: "RenderedPlan", File: "internal/console/templates/plan.go", Kind: response.ChangeKindNew,
					Fields: []response.Field{
						{Name: "Overview", Type: "RenderedOverview", Required: true, Default: "", Constraints: "none"},
					},
					Transitions: []response.Transition{
						{From: "unrendered", To: "rendered", When: "RenderPlan succeeds"},
					},
				},
			},
			Migrations: response.Migrations{
				Items: []response.Migration{
					{
						File:     planMigrationFile,
						Schema:   "CREATE INDEX idx_artifacts_plan ON artifacts(ticket_id, type);",
						Backfill: "none, index only",
						Locks:    "a brief index build on artifacts",
						Compat:   "additive, safe during a rolling deploy",
						Rollback: "DROP INDEX idx_artifacts_plan",
					},
				},
			},
		},
		Delivery: response.Delivery{
			Files: []response.FileChange{
				{Path: planChangePath, Action: response.FileActionCreate, Reason: "builds RenderedPlan from a stored Plan"},
				{Path: "internal/console/templates/plan.templ", Action: response.FileActionCreate, Reason: "renders RenderedPlan"},
			},
			Deletions: response.Deletions{
				Items: []response.Fence{
					{Path: "internal/console/templates/thread.templ", Symbol: "gateContext placeholder", ExistedBecause: "existed because Task 8 had not landed yet"},
				},
			},
			Tests: []response.TestCase{
				{Name: planTestName, Seam: "console.RenderPlan", Kind: response.TestKindUnit, Mocks: "", Asserts: "every field renders recognizably"},
			},
			Tasks: []response.Task{
				{N: 1, Test: planTestName, Demo: true, Text: "Render **every** Plan field."},
			},
		},
		Review: response.Review{
			TrustRoot:    "internal/console/render.go, the audited markdown boundary",
			Alternatives: []string{"Recompute rows from files/tasks instead of reading the stored plan"},
			Risks:        []string{"A future Plan field added with no matching render case"},
		},
	}
}

// renderPlanToString drives console.RenderPlan's returned templ.Component
// the same way a handler does (render_test.go's renderToString), so this
// exercises the real boundary.
func renderPlanToString(t *testing.T, plan response.Plan) string {
	t.Helper()
	comp, err := console.RenderPlan(plan)
	if err != nil {
		t.Fatalf("console.RenderPlan: %v", err)
	}
	if comp == nil {
		t.Fatal("console.RenderPlan: returned a nil templ.Component")
	}
	var buf strings.Builder
	if err := comp.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render(): %v", err)
	}
	return buf.String()
}

// TestRenderPlanEveryField proves the design section 6.9 field-to-render
// map: the four parts as <h2>, their children as <h3>, and recognizable
// output for every field, not only the headings.
func TestRenderPlanEveryField(t *testing.T) {
	got := renderPlanToString(t, fixturePlan())

	t.Run("four parts as h2", func(t *testing.T) {
		for _, want := range []string{"<h2>Overview</h2>", "<h2>Design</h2>", "<h2>Delivery</h2>", "<h2>Review</h2>"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("overview children as h3", func(t *testing.T) {
		for _, want := range []string{"<h3>Objective</h3>", "<h3>Context</h3>", "<h3>Problem</h3>", "<h3>Goals</h3>", "<h3>Non-goals</h3>"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("objective renders as plain text", func(t *testing.T) {
		if !strings.Contains(got, "Ship a plan renderer that drops nothing.") {
			t.Error("missing the objective text")
		}
	})

	t.Run("context renders through the markdown helper", func(t *testing.T) {
		if !strings.Contains(got, "<strong>internal/console</strong>") {
			t.Errorf("context did not render as markdown; got:\n%s", got)
		}
	})

	t.Run("problem text renders through the markdown helper", func(t *testing.T) {
		if !strings.Contains(got, "The gate question shows a placeholder instead of the stored plan.") {
			t.Error("missing the problem text")
		}
	})

	t.Run("bug loop renders as cmd plus text", func(t *testing.T) {
		if !strings.Contains(got, "go test ./internal/console/... -run TestRenderPlan") {
			t.Error("missing the loop cmd")
		}
		if !strings.Contains(got, "fails: no plan renderer exists yet") {
			t.Error("missing the loop text")
		}
	})

	t.Run("repro renders", func(t *testing.T) {
		if !strings.Contains(got, "seed a gate question") {
			t.Error("missing the repro text")
		}
	})

	t.Run("hypotheses render as a rank/cause/prediction table", func(t *testing.T) {
		for _, want := range []string{
			"<td>1</td>", "no RenderPlan function exists", "adding one replaces the placeholder",
			"<td>2</td>", "the gate context never calls it", "wiring it in fixes the region",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("hypotheses table missing %q", want)
			}
		}
	})

	t.Run("goals and non-goals render as lists", func(t *testing.T) {
		for _, want := range []string{
			"<li>Render every Plan field</li>", "<li>Reuse the Task 5 markdown helper</li>",
			"<li>Recomputing files or tasks into rows of their own</li>",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("design children as h3", func(t *testing.T) {
		for _, want := range []string{"<h3>Demo</h3>", "<h3>Shape</h3>", "<h3>Changes</h3>", "<h3>Types</h3>", "<h3>Migrations</h3>"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("design demo renders as cmd plus text", func(t *testing.T) {
		if !strings.Contains(got, "go test ./internal/console/... -run TestRenderPlanEveryField") {
			t.Error("missing the demo cmd")
		}
		if !strings.Contains(got, "the fixture plan renders every field") {
			t.Error("missing the demo text")
		}
	})

	t.Run("shape renders through the markdown helper with a mermaid block", func(t *testing.T) {
		if !strings.Contains(got, "<pre class=\"mermaid\">") {
			t.Errorf("shape did not draw its mermaid block; got:\n%s", got)
		}
		if !strings.Contains(got, "graph TD") {
			t.Error("mermaid block missing its source")
		}
		if !strings.Contains(got, "The renderer walks a Plan into HTML.") {
			t.Error("shape prose missing")
		}
	})

	t.Run("changes render as a table", func(t *testing.T) {
		for _, want := range []string{
			planChangePath, "RenderPlan", "new",
			"views.go&#39;s threadComponent", "Render, templates.PlanView",
			"func RenderPlan(plan response.Plan) (templ.Component, error)",
			"console.RenderPlan(plan)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("changes table missing %q; got:\n%s", want, got)
			}
		}
	})

	t.Run("types render name/file/kind, a fields table, and a transitions sub-table", func(t *testing.T) {
		for _, want := range []string{
			"RenderedPlan", "internal/console/templates/plan.go",
			"Overview", "RenderedOverview", "true",
			"unrendered", "rendered", "RenderPlan succeeds",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("types section missing %q", want)
			}
		}
	})

	t.Run("migrations render each field", func(t *testing.T) {
		for _, want := range []string{
			planMigrationFile,
			"CREATE INDEX idx_artifacts_plan ON artifacts(ticket_id, type);",
			"none, index only",
			"a brief index build on artifacts",
			"additive, safe during a rolling deploy",
			"DROP INDEX idx_artifacts_plan",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("migrations section missing %q", want)
			}
		}
	})

	t.Run("delivery children as h3", func(t *testing.T) {
		for _, want := range []string{"<h3>Files</h3>", "<h3>Deletions</h3>", "<h3>Tests</h3>", "<h3>Tasks</h3>"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("files render as a table", func(t *testing.T) {
		for _, want := range []string{
			planChangePath, "create", "builds RenderedPlan from a stored Plan",
			"internal/console/templates/plan.templ", "renders RenderedPlan",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("files table missing %q", want)
			}
		}
	})

	t.Run("deletions render as a table", func(t *testing.T) {
		for _, want := range []string{
			"internal/console/templates/thread.templ", "gateContext placeholder",
			"existed because Task 8 had not landed yet",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("deletions table missing %q", want)
			}
		}
	})

	t.Run("tests render as a table", func(t *testing.T) {
		for _, want := range []string{
			planTestName, "console.RenderPlan", "unit", "every field renders recognizably",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("tests table missing %q", want)
			}
		}
	})

	t.Run("tasks render as a table with text through the markdown helper", func(t *testing.T) {
		if !strings.Contains(got, "<td>1</td>") {
			t.Error("missing task n")
		}
		if !strings.Contains(got, planTestName) {
			t.Error("missing task test name")
		}
		if !strings.Contains(got, "<strong>every</strong>") {
			t.Errorf("task text did not render as markdown; got:\n%s", got)
		}
	})

	t.Run("review children as h3", func(t *testing.T) {
		for _, want := range []string{"<h3>Trust root</h3>", "<h3>Alternatives</h3>", "<h3>Risks</h3>"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("review fields render", func(t *testing.T) {
		if !strings.Contains(got, "internal/console/render.go, the audited markdown boundary") {
			t.Error("missing trust root")
		}
		if !strings.Contains(got, "<li>Recompute rows from files/tasks instead of reading the stored plan</li>") {
			t.Error("missing alternative")
		}
		if !strings.Contains(got, "<li>A future Plan field added with no matching render case</li>") {
			t.Error("missing risk")
		}
	})
}

// TestRenderPlanNoneLines proves a plan with Migrations.None and
// Deletions.None renders the "none" lines rather than an empty table
// (design section 6.9: "or a 'none' line").
func TestRenderPlanNoneLines(t *testing.T) {
	plan := fixturePlan()
	plan.Design.Migrations = response.Migrations{None: true}
	plan.Delivery.Deletions = response.Deletions{None: true}

	got := renderPlanToString(t, plan)

	if strings.Contains(got, planMigrationFile) {
		t.Error("Migrations.None: still rendered the fixture's migration")
	}
	if strings.Contains(got, "gateContext placeholder") {
		t.Error("Deletions.None: still rendered the fixture's deletion")
	}
	if !strings.Contains(got, "No migrations.") {
		t.Errorf("Migrations.None did not render a none line; got:\n%s", got)
	}
	if !strings.Contains(got, "Nothing deleted.") {
		t.Errorf("Deletions.None did not render a none line; got:\n%s", got)
	}
}
