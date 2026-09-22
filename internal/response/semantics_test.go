package response

import "testing"

func TestCheckNoneUnion_TrueWithItemsFails(t *testing.T) {
	t.Parallel()

	err := checkNoneUnion("plan/design/migrations", true, true, 2)
	if err == nil {
		t.Fatal("checkNoneUnion = nil, want an error: none=\"true\" with items present")
	}
	want := `plan/design/migrations: none="true" allows no items`
	if got := err.Error(); got != want {
		t.Errorf("checkNoneUnion = %q, want %q", got, want)
	}
}

func TestCheckNoneUnion_FalsePresentFails(t *testing.T) {
	t.Parallel()

	err := checkNoneUnion("plan/design/migrations", true, false, 0)
	if err == nil {
		t.Fatal("checkNoneUnion = nil, want an error: none=\"false\" is not a legal value")
	}
	want := `plan/design/migrations: none must be "true" when present`
	if got := err.Error(); got != want {
		t.Errorf("checkNoneUnion = %q, want %q", got, want)
	}
}

func TestCheckNoneUnion_AbsentAndEmptyFails(t *testing.T) {
	t.Parallel()

	err := checkNoneUnion("plan/delivery/deletions", false, false, 0)
	if err == nil {
		t.Fatal("checkNoneUnion = nil, want an error: neither none=\"true\" nor any items")
	}
	want := `plan/delivery/deletions: set none="true" or list one or more`
	if got := err.Error(); got != want {
		t.Errorf("checkNoneUnion = %q, want %q", got, want)
	}
}

func TestCheckNoneUnion_TrueEmptyPasses(t *testing.T) {
	t.Parallel()

	if err := checkNoneUnion("plan/design/migrations", true, true, 0); err != nil {
		t.Errorf("checkNoneUnion = %v, want nil", err)
	}
}

func TestCheckNoneUnion_AbsentWithItemsPasses(t *testing.T) {
	t.Parallel()

	if err := checkNoneUnion("plan/delivery/deletions", false, false, 3); err != nil {
		t.Errorf("checkNoneUnion = %v, want nil", err)
	}
}

// allKeysPresent returns a presence map marking every one of n children's
// own key attribute present, the ordinary case these direct
// checkChildrenDAG tests build (each Child literal already carries a real
// key).
func allKeysPresent(n int) map[string]bool {
	m := make(map[string]bool, n)
	for i := range n {
		m[indexedName("child", i)+"/key"] = true
	}
	return m
}

func TestCheckChildrenDAG_DuplicateKey(t *testing.T) {
	t.Parallel()

	children := []Child{
		{Key: "c1", Title: "t", Body: "b"},
		{Key: "c1", Title: "t", Body: "b"},
	}
	errs := checkChildrenDAG(children, allKeysPresent(len(children)))
	want := "child[1]/key: duplicate key c1"
	if !containsErr(errs, want) {
		t.Fatalf("checkChildrenDAG = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckChildrenDAG_UnknownDep(t *testing.T) {
	t.Parallel()

	children := []Child{
		{Key: "c1", Title: "t", Body: "b", DependsOn: []string{"c9"}},
	}
	errs := checkChildrenDAG(children, allKeysPresent(len(children)))
	want := "child[0]/depends_on: unknown key c9"
	if !containsErr(errs, want) {
		t.Fatalf("checkChildrenDAG = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckChildrenDAG_SelfDependency(t *testing.T) {
	t.Parallel()

	children := []Child{
		{Key: "c1", Title: "t", Body: "b", DependsOn: []string{"c1"}},
	}
	errs := checkChildrenDAG(children, allKeysPresent(len(children)))
	want := "child[0]/depends_on: self-dependency"
	if !containsErr(errs, want) {
		t.Fatalf("checkChildrenDAG = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckChildrenDAG_Cycle(t *testing.T) {
	t.Parallel()

	// c1 -> c2 -> c1: a 2-node cycle, declared in that order.
	children := []Child{
		{Key: "c1", Title: "t", Body: "b", DependsOn: []string{"c2"}},
		{Key: "c2", Title: "t", Body: "b", DependsOn: []string{"c1"}},
	}
	errs := checkChildrenDAG(children, allKeysPresent(len(children)))
	want := "children: dependency cycle c1 -> c2 -> c1"
	if !containsErr(errs, want) {
		t.Fatalf("checkChildrenDAG = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckChildrenDAG_CyclePathIsDeterministic(t *testing.T) {
	t.Parallel()

	// A longer cycle through three nodes, declared in a fixed order, plus
	// an unrelated acyclic node, so the DFS has real choices to make and
	// still must always report the same path.
	children := []Child{
		{Key: "c1", Title: "t", Body: "b", DependsOn: []string{"c2"}},
		{Key: "c2", Title: "t", Body: "b", DependsOn: []string{"c3"}},
		{Key: "c3", Title: "t", Body: "b", DependsOn: []string{"c1"}},
		{Key: "c4", Title: "t", Body: "b"},
	}
	want := "children: dependency cycle c1 -> c2 -> c3 -> c1"
	present := allKeysPresent(len(children))
	for range 20 {
		errs := checkChildrenDAG(children, present)
		if !containsErr(errs, want) {
			t.Fatalf("checkChildrenDAG = %v, want to contain %q", dumpErrs(errs), want)
		}
	}
}

func TestCheckChildrenDAG_NoIssuesPasses(t *testing.T) {
	t.Parallel()

	children := []Child{
		{Key: "c1", Title: "t", Body: "b"},
		{Key: "c2", Title: "t", Body: "b", DependsOn: []string{"c1"}},
	}
	errs := checkChildrenDAG(children, allKeysPresent(len(children)))
	if len(errs) != 0 {
		t.Fatalf("checkChildrenDAG = %v, want no errors", dumpErrs(errs))
	}
}

// TestCheckChildrenDAG_MissingKeySkipsDuplicateAndDependencyChecks proves
// a child whose key never decoded (present says so) does not contribute
// its zero-value Key to the duplicate-key check, and its own depends_on is
// not checked either, so a zero-value depends_on entry cannot spuriously
// "self-depend" on a zero-value key.
func TestCheckChildrenDAG_MissingKeySkipsDuplicateAndDependencyChecks(t *testing.T) {
	t.Parallel()

	children := []Child{
		{Title: "t", Body: "b", DependsOn: []string{""}},
		{Title: "t", Body: "b"},
	}
	errs := checkChildrenDAG(children, map[string]bool{})
	if len(errs) != 0 {
		t.Fatalf("checkChildrenDAG = %v, want no errors: neither child's key is present", dumpErrs(errs))
	}
}

func questionsWithOptionCounts(counts ...int) []Question {
	qs := make([]Question, len(counts))
	for i, n := range counts {
		opts := make([]Option, n)
		for j := range opts {
			opts[j] = Option{Key: string(rune('a' + j)), Text: "opt"}
		}
		qs[i] = Question{Key: "q1", Title: "t", Body: "b", Options: opts, Recommended: "r"}
	}
	return qs
}

func TestCheckQuestionCardinality_OneOptionFails(t *testing.T) {
	t.Parallel()

	errs := checkQuestionCardinality(questionsWithOptionCounts(1))
	want := "question[0]/options: give none, or two to four"
	if !containsErr(errs, want) {
		t.Fatalf("checkQuestionCardinality = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckQuestionCardinality_ZeroPasses(t *testing.T) {
	t.Parallel()

	errs := checkQuestionCardinality(questionsWithOptionCounts(0))
	if len(errs) != 0 {
		t.Fatalf("checkQuestionCardinality = %v, want no errors", dumpErrs(errs))
	}
}

func TestCheckQuestionCardinality_ThreePasses(t *testing.T) {
	t.Parallel()

	errs := checkQuestionCardinality(questionsWithOptionCounts(3))
	if len(errs) != 0 {
		t.Fatalf("checkQuestionCardinality = %v, want no errors", dumpErrs(errs))
	}
}

// TestCheckQuestionCardinality_FiveOptionsNotFlaggedHere proves this
// function no longer duplicates Layer 1's own maxItems=4 error: a count
// over four is Layer 1's job alone (design section 6.6).
func TestCheckQuestionCardinality_FiveOptionsNotFlaggedHere(t *testing.T) {
	t.Parallel()

	errs := checkQuestionCardinality(questionsWithOptionCounts(5))
	if len(errs) != 0 {
		t.Fatalf("checkQuestionCardinality = %v, want no errors: maxItems=4 is Layer 1's own check", dumpErrs(errs))
	}
}
