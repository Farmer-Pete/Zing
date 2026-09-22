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

// fullPresence returns a presence set marking every field CheckPlan's
// Layer 2 checks gate on as present: <problem>, the first test's kind,
// and each of n scenarios' own id. It is the ordinary case for these
// tests, which hand-populate a Plan's fields directly with real values
// rather than parsing them from XML.
func fullPresence(n int) map[string]bool {
	m := map[string]bool{
		"plan/overview/problem":            true,
		"plan/delivery/tests/test[0]/kind": true,
	}
	for i := range n {
		m["scenarios/"+indexedName("scenario", i)+"/id"] = true
	}
	return m
}

// presenceWithout returns fullPresence(n) with key removed, for a test
// that needs everything present except the one field under test.
func presenceWithout(n int, key string) map[string]bool {
	m := fullPresence(n)
	delete(m, key)
	return m
}

func TestCheckPlan_PlaceholderAnywhereFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Design.Shape += " TODO: flesh this out."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
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

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	want := `plan/design/shape: placeholder "TBD" not allowed`
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_PlaceholdersComeFromChecklistsNotAHardcodedList(t *testing.T) {
	t.Parallel()

	custom := Checklists{
		Placeholders: []string{"FIXME_LATER"},
		Vague:        planChecklists.Vague,
		Units:        planChecklists.Units,
	}

	// A word in the custom list must be flagged.
	flagged := cleanPlan()
	flagged.Design.Shape += " FIXME_LATER: revisit this."
	errs := CheckPlan(flagged, cleanScenarios(), false, custom, fullPresence(2))
	want := `plan/design/shape: placeholder "FIXME_LATER" not allowed`
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan(custom checklist) = %v, want to contain %q", dumpErrs(errs), want)
	}

	// TODO is not in the custom list, so tuning the checklist actually
	// changed what is flagged, rather than the hardcoded default list
	// still being consulted underneath.
	notFlagged := cleanPlan()
	notFlagged.Design.Shape += " TODO: not in the custom list."
	errs = CheckPlan(notFlagged, cleanScenarios(), false, custom, fullPresence(2))
	if containsErr(errs, `plan/design/shape: placeholder "TODO" not allowed`) {
		t.Fatalf("CheckPlan(custom checklist) = %v, must not flag TODO: it is not in the custom placeholder list", dumpErrs(errs))
	}
}

func TestCheckPlan_VagueWordOutsideFenceFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Context += " This is a fast implementation."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	want := `plan/overview/context: vague word "fast"; give a concrete threshold`
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_VagueWordInsideFencePasses(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Design.Shape += "\n```go\n// this cache is fast on the happy path\n```\n"

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	if containsErr(errs, `plan/design/shape: vague word "fast"; give a concrete threshold`) {
		t.Fatalf("CheckPlan = %v, want no vague-word error: the word is inside a fenced code block", dumpErrs(errs))
	}
}

func TestCheckPlan_PerformanceWithoutMeasurementFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " Optimize the lookup path."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	want := "plan/delivery/tasks/task[0]: mentions performance without a measurement"
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_PerformanceWithMeasurementPasses(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " Optimize the lookup path to run under 50ms."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	if containsErr(errs, "plan/delivery/tasks/task[0]: mentions performance without a measurement") {
		t.Fatalf("CheckPlan = %v, want no performance error: 50ms is a measurement", dumpErrs(errs))
	}
}

func TestCheckPlan_PerformanceWithPercentMeasurementPasses(t *testing.T) {
	t.Parallel()

	// "%" is not a word character, so a trailing \b never matches right
	// after it: this is the exact case the plain \b boundary misses.
	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " Optimize until usage is under 50%."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	if containsErr(errs, "plan/delivery/tasks/task[0]: mentions performance without a measurement") {
		t.Fatalf("CheckPlan = %v, want no performance error: 50%% is a measurement", dumpErrs(errs))
	}
}

func TestCheckPlan_OptimisticDoesNotMatchPerformanceWord(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Delivery.Tasks[0].Text += " We are optimistic this will land cleanly."

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
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

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	if containsErr(errs, "plan/design/changes/change[0]/callers: mentions performance without a measurement") {
		t.Fatalf("CheckPlan = %v, want no performance error on a change field", dumpErrs(errs))
	}
}

func TestCheckPlan_ScenarioLeakFails(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Context += " the response is 200 with body ok"

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
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

	errs := CheckPlan(p, cleanScenarios(), false, planChecklists, fullPresence(2))
	want := "plan/overview/context: repeats scenario s1 then-text; the plan must not restate acceptance"
	if !containsErr(errs, want) {
		t.Fatalf("CheckPlan = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckPlan_NoLeakPasses(t *testing.T) {
	t.Parallel()

	errs := CheckPlan(cleanPlan(), cleanScenarios(), false, planChecklists, fullPresence(2))
	if containsErr(errs, "plan/overview/context: repeats scenario s1 then-text; the plan must not restate acceptance") {
		t.Fatalf("CheckPlan = %v, want no scenario-leak error on the clean plan", dumpErrs(errs))
	}
}

// TestCheckPlan_ScenarioLeakSkippedWhenIDNotPresent proves the
// scenario-leak check does not read a scenario's zero-value ID (Layer 1
// already reports scenarios/scenario[0]/id missing) to build its message:
// when that scenario's id was never in the document, the check is skipped
// for it entirely, not run with a blank id interpolated.
func TestCheckPlan_ScenarioLeakSkippedWhenIDNotPresent(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Context += " the response is 200 with body ok"

	scenarios := []Scenario{
		{Kind: ScenarioKindBehavior, Given: "g", When: "w", Then: "the response is 200 with body ok"}, // id missing
		{ID: "s2", Kind: ScenarioKindNegative, Given: "g2", When: "w2", Then: "the response is 405"},
	}
	present := presenceWithout(2, "scenarios/scenario[0]/id")

	errs := CheckPlan(p, scenarios, false, planChecklists, present)
	if containsErr(errs, "plan/overview/context: repeats scenario  then-text; the plan must not restate acceptance") {
		t.Fatalf("CheckPlan = %v, must not report a scenario leak off a scenario whose id is absent", dumpErrs(errs))
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

// The four design section 6.6 bug-plan-shape messages, shared by every
// test below that checks for their presence or absence.
const (
	bugShapeLoopErr       = "plan/overview/problem/loop: bug plan needs a loop"
	bugShapeReproErr      = "plan/overview/problem/repro: bug plan needs a repro"
	bugShapeHypothesesErr = "plan/overview/problem/hypotheses: bug plan needs three to five hypotheses"
	bugShapeTestKindErr   = "plan/delivery/tests/test[0]/kind: first test must be kind regression"

	// testBugLoopCmd is the Loop.Cmd a bug-shape-valid plan uses, shared
	// with validate_test.go's planXMLBugValidFirstTest.
	testBugLoopCmd = "go test ./... -run TestBug"
)

func TestCheckPlan_BugPlanMissingShapeFailsOnlyUnderBugKind(t *testing.T) {
	t.Parallel()

	p := bugShapePlan()
	lists := planChecklists
	bugShapeErrs := []string{bugShapeLoopErr, bugShapeReproErr, bugShapeHypothesesErr, bugShapeTestKindErr}

	featureErrs := CheckPlan(p, cleanScenarios(), false, lists, fullPresence(2))
	for _, want := range bugShapeErrs {
		if containsErr(featureErrs, want) {
			t.Errorf("CheckPlan(bug=false) = %v, must not contain %q: bug-shape rules are bug-only", dumpErrs(featureErrs), want)
		}
	}

	bugErrs := CheckPlan(p, cleanScenarios(), true, lists, fullPresence(2))
	for _, want := range bugShapeErrs {
		if !containsErr(bugErrs, want) {
			t.Errorf("CheckPlan(bug=true) = %v, want to contain %q", dumpErrs(bugErrs), want)
		}
	}
}

// TestCheckPlan_ProblemNotPresentSkipsProblemChecksButKeepsTestKindCheck
// proves problemPresent=false suppresses the loop/repro/hypotheses checks
// (which would otherwise fire off Problem's zero value) while the
// first-test-kind check, which does not read Problem at all, still runs
// (design MAJOR finding 2).
func TestCheckPlan_ProblemNotPresentSkipsProblemChecksButKeepsTestKindCheck(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Problem = Problem{} // as if <problem> never decoded
	p.Delivery.Tests[0].Kind = TestKindIntegration

	errs := CheckPlan(p, cleanScenarios(), true, planChecklists, presenceWithout(2, "plan/overview/problem"))
	for _, notWant := range []string{bugShapeLoopErr, bugShapeReproErr, bugShapeHypothesesErr} {
		if containsErr(errs, notWant) {
			t.Errorf("CheckPlan(problemPresent=false) = %v, must not contain %q", dumpErrs(errs), notWant)
		}
	}
	if !containsErr(errs, bugShapeTestKindErr) {
		t.Errorf("CheckPlan(problemPresent=false) = %v, want to contain %q: it does not depend on problem", dumpErrs(errs), bugShapeTestKindErr)
	}
}

// TestCheckPlan_FirstTestKindNotPresentSkipsTestKindCheck proves
// firstTestKindPresent=false suppresses the first-test-kind check even
// though Tests[0].Kind's zero value ("") is not TestKindRegression, since
// that zero value means the attribute never decoded (Layer 1 already
// reports plan/delivery/tests/test[0]/kind missing).
func TestCheckPlan_FirstTestKindNotPresentSkipsTestKindCheck(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Problem.Loop = &Loop{Cmd: testBugLoopCmd, Text: "fails"}
	p.Overview.Problem.Repro = "steps"
	p.Overview.Problem.Hypotheses = []Hypothesis{
		{Rank: 1, Cause: "c1", Prediction: "p1"},
		{Rank: 2, Cause: "c2", Prediction: "p2"},
		{Rank: 3, Cause: "c3", Prediction: "p3"},
	}
	p.Delivery.Tests[0].Kind = "" // as if kind never decoded

	errs := CheckPlan(p, cleanScenarios(), true, planChecklists, presenceWithout(2, "plan/delivery/tests/test[0]/kind"))
	if containsErr(errs, bugShapeTestKindErr) {
		t.Fatalf("CheckPlan(firstTestKindPresent=false) = %v, must not contain %q", dumpErrs(errs), bugShapeTestKindErr)
	}
}

// TestCheckPlan_FirstTestKindPresentButWrongStillFlags proves the
// presence gate only suppresses the check when kind is truly absent: a
// present, non-regression kind still draws the error.
func TestCheckPlan_FirstTestKindPresentButWrongStillFlags(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Problem.Loop = &Loop{Cmd: testBugLoopCmd, Text: "fails"}
	p.Overview.Problem.Repro = "steps"
	p.Overview.Problem.Hypotheses = []Hypothesis{
		{Rank: 1, Cause: "c1", Prediction: "p1"},
		{Rank: 2, Cause: "c2", Prediction: "p2"},
		{Rank: 3, Cause: "c3", Prediction: "p3"},
	}
	p.Delivery.Tests[0].Kind = TestKindUnit

	errs := CheckPlan(p, cleanScenarios(), true, planChecklists, fullPresence(2))
	if !containsErr(errs, bugShapeTestKindErr) {
		t.Fatalf("CheckPlan(firstTestKindPresent=true) = %v, want to contain %q", dumpErrs(errs), bugShapeTestKindErr)
	}
}

func TestCheckPlan_FullBugPlanPasses(t *testing.T) {
	t.Parallel()

	p := cleanPlan()
	p.Overview.Problem.Loop = &Loop{Cmd: testBugLoopCmd, Text: "fails with a nil pointer dereference"}
	p.Overview.Problem.Repro = "call Foo(nil) directly; every field is load bearing"
	p.Overview.Problem.Hypotheses = []Hypothesis{
		{Rank: 1, Cause: "Foo dereferences its argument without a nil check", Prediction: "adding a nil guard makes the crash disappear"},
		{Rank: 2, Cause: "the caller passes nil when the cache misses", Prediction: "populating the cache first makes the crash disappear"},
		{Rank: 3, Cause: "a race leaves the pointer unset", Prediction: "serializing the two calls makes the crash disappear"},
	}
	p.Delivery.Tests[0].Kind = TestKindRegression

	errs := CheckPlan(p, cleanScenarios(), true, planChecklists, fullPresence(2))
	if len(errs) != 0 {
		t.Fatalf("CheckPlan = %v, want no errors: a full bug plan satisfies every bug-shape rule", dumpErrs(errs))
	}
}

func TestCheckPlan_CleanFeaturePlanPasses(t *testing.T) {
	t.Parallel()

	errs := CheckPlan(cleanPlan(), cleanScenarios(), false, planChecklists, fullPresence(2))
	if len(errs) != 0 {
		t.Fatalf("CheckPlan = %v, want no errors", dumpErrs(errs))
	}
}
