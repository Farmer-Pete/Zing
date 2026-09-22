package response

import "testing"

// cleanPlan returns a Plan whose prose is deliberately unremarkable: no
// placeholder tokens, no vague qualifiers, no performance claims, and no
// text that echoes any scenario's Then. Tests mutate a copy of it to
// isolate the one condition under test.
func cleanPlan() Plan {
	return Plan{
		Overview: Overview{
			Objective: "Add a health check endpoint so a load balancer can tell when Zing is ready.",
			Context:   "cmd/zing/main.go builds the HTTP server and its routes in newMux.",
			Problem:   Problem{Text: "Nothing today answers a liveness probe."},
			Goals:     []string{"GET /healthz returns 200 when the process is running"},
			NonGoals:  []string{"Checking downstream dependencies is out of scope"},
		},
		Design: Design{
			Demo:  Demo{Cmd: "go run ./cmd/zing serve", Text: "Curling /healthz prints ok with a 200 status."},
			Shape: "newMux gains one more route, registered beside the existing index route.",
			Changes: []Change{
				{
					Path: testMainGo, Symbol: "newMux", Kind: ChangeKindModified,
					Callers: "main", Callees: "http.ListenAndServe", Before: "// none", After: "mux.HandleFunc(...)",
				},
			},
			Migrations: Migrations{None: true},
		},
		Delivery: Delivery{
			Files:     []FileChange{{Path: testMainGo, Action: FileActionModify, Reason: "register the route"}},
			Deletions: Deletions{None: true},
			Tests:     []TestCase{{Name: "TestHealthz", Seam: "newMux", Kind: TestKindIntegration, Asserts: "GET /healthz returns 200"}},
			Tasks:     []Task{{N: 1, Test: "TestHealthz", Demo: true, Text: "Add the handler and register it in newMux."}},
		},
		Review: Review{
			TrustRoot:    "none",
			Alternatives: []string{"Reuse the index handler: rejected, a probe should not share a path with a human page."},
			Risks:        []string{"A load balancer expecting a different body would need reconfiguring."},
		},
	}
}

func cleanScenarios() []Scenario {
	return []Scenario{
		{ID: "s1", Kind: ScenarioKindBehavior, Given: "the server is running", When: "a client sends GET /healthz", Then: "the response is 200 with body ok"},
		{ID: "s2", Kind: ScenarioKindNegative, Given: "the server is running", When: "a client sends POST /healthz", Then: "the response is 405"},
	}
}

func TestCheckPlan_PlaceholderAnywhereFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Design.Shape += " TODO: flesh this out."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	want := `plan/design/shape: placeholder "TODO" not allowed`
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_PlaceholderInsideFencedCodeStillFails(t *testing.T) {
	t.Parallel()

	// The placeholder rule has no code-block exemption, unlike the vague
	// rule below.
	p := cleanPlan()
	p.Design.Shape += "\n```go\n// TBD: fill in\n```\n"

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	want := `plan/design/shape: placeholder "TBD" not allowed`
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_VagueWordOutsideFenceFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Context += " This is a fast implementation."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	want := `plan/overview/context: vague word "fast"; give a concrete threshold`
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_VagueWordInsideFencePasses(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Design.Shape += "\n```go\n// this cache is fast on the happy path\n```\n"

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	if containsErr(errs, `plan/design/shape: vague word "fast"; give a concrete threshold`) {
		t.Fatalf("CheckPlan = %v, want no vague-word error: the word is inside a fenced code block", dumpErrs(errs))
	}
}

func TestCheckPlan_PerformanceWithoutMeasurementFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " Optimize the lookup path."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	want := "plan/delivery/tasks/task[0]: mentions performance without a measurement"
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_PerformanceWithMeasurementPasses(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " Optimize the lookup path to run under 50ms."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	if containsErr(errs, "plan/delivery/tasks/task[0]: mentions performance without a measurement") {
		t.Fatalf("CheckPlan = %v, want no performance error: 50ms is a measurement", dumpErrs(errs))
	}
}

func TestCheckPlan_OptimisticDoesNotMatchPerformanceWord(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " We are optimistic this will land cleanly."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	if containsErr(errs, "plan/delivery/tasks/task[0]: mentions performance without a measurement") {
		t.Fatalf("CheckPlan = %v, want no performance error: \"optimistic\" is not \"optimize\"", dumpErrs(errs))
	}
}

func TestCheckPlan_PerformanceWordInChangeIsNotChecked(t *testing.T) {
	t.Parallel()

	// The performance-without-measurement rule reads only task prose, not
	// change prose (design section 6.6: "a task prose element, not a
	// change").
	p := cleanPlan()
	p.Design.Changes[0].Callers += " Optimize this later."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	if containsErr(errs, "plan/design/changes/change[0]/callers: mentions performance without a measurement") {
		t.Fatalf("CheckPlan = %v, want no performance error on a change field", dumpErrs(errs))
	}
}

func TestCheckPlan_ScenarioLeakFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Context += " the response is 200 with body ok"

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	want := "plan/overview/context: repeats scenario s1 then-text; the plan must not restate acceptance"
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_ScenarioLeakAcrossLineBreakFails(t *testing.T) {
	t.Parallel()

	// The Then text is split across a line break in the plan prose;
	// normalizing both sides (collapsing whitespace runs, including
	// newlines, to one space) must still catch the leak.
	p := cleanPlan()
	p.Overview.Context += "\nthe response is 200\nwith body ok"

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists)
	want := "plan/overview/context: repeats scenario s1 then-text; the plan must not restate acceptance"
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_NoLeakPasses(t *testing.T) {
	t.Parallel()

	errs := CheckPlan(cleanPlan(), cleanScenarios(), false, planChecklists)
	if containsErr(errs, "plan/overview/context: repeats scenario s1 then-text; the plan must not restate acceptance") {
		t.Fatalf("CheckPlan = %v, want no scenario-leak error on the clean plan", dumpErrs(errs))
	}
}

// bugShapePlan returns a clean plan whose bug-plan-shape parts (loop,
// repro, hypotheses, first-test-kind) are all missing or wrong: the same
// content used both to prove the bug rules fire under bug kind, and that
// they do NOT fire under feature kind.
func bugShapePlan() Plan {
	p := cleanPlan()
	p.Delivery.Tests[0].Kind = TestKindIntegration // not regression
	return p
}

func TestCheckPlan_BugPlanMissingShapeFailsOnlyUnderBugKind(t *testing.T) {
	t.Parallel()

	p := bugShapePlan()
	lists := planChecklists

	featureErrs := CheckPlan(p, cleanScenarios(), false, lists)
	for _, want := range []string{
		"plan/overview/problem/loop: bug plan needs a loop",
		"plan/overview/problem/repro: bug plan needs a repro",
		"plan/overview/problem/hypotheses: bug plan needs three to five hypotheses",
		"plan/delivery/tests/test[0]/kind: first test must be kind regression",
	} {
		if containsErr(featureErrs, want) {
			t.Errorf("CheckPlan(bug=false) = %v, must not contain %q: bug-shape rules are bug-only", dumpErrs(featureErrs), want)
		}
	}

	bugErrs := CheckPlan(p, cleanScenarios(), true, lists)
	for _, want := range []string{
		"plan/overview/problem/loop: bug plan needs a loop",
		"plan/overview/problem/repro: bug plan needs a repro",
		"plan/overview/problem/hypotheses: bug plan needs three to five hypotheses",
		"plan/delivery/tests/test[0]/kind: first test must be kind regression",
	} {
		if !containsErr(bugErrs, want) {
			t.Errorf("CheckPlan(bug=true) = %v, want to contain %q", dumpErrs(bugErrs), want)
		}
	}
}

func TestCheckPlan_FullBugPlanPasses(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Problem.Loop = &Loop{Cmd: "go test ./... -run TestBug", Text: "fails with a nil pointer dereference"}
	p.Overview.Problem.Repro = "call Foo(nil) directly; every field is load bearing"
	p.Overview.Problem.Hypotheses = []Hypothesis{
		{Rank: 1, Cause: "Foo dereferences its argument without a nil check", Prediction: "adding a nil guard makes the crash disappear"},
		{Rank: 2, Cause: "the caller passes nil when the cache misses", Prediction: "populating the cache first makes the crash disappear"},
		{Rank: 3, Cause: "a race leaves the pointer unset", Prediction: "serializing the two calls makes the crash disappear"},
	}
	p.Delivery.Tests[0].Kind = TestKindRegression

	errs := CheckPlan(p, cleanScenarios(), true, planChecklists)
	if len(errs) != 0 {
		t.Fatalf("CheckPlan = %v, want no errors: a full bug plan satisfies every bug-shape rule", dumpErrs(errs))
	}
}

func TestCheckPlan_CleanFeaturePlanPasses(t *testing.T) {
	t.Parallel()

	errs := CheckPlan(cleanPlan(), cleanScenarios(), false, planChecklists)
	if len(errs) != 0 {
		t.Fatalf("CheckPlan = %v, want no errors", dumpErrs(errs))
	}
}
