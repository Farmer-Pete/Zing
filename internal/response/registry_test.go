package response

import (
	"slices"
	"strings"
	"testing"
)

func TestLookup_RegisteredPairs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		job     Job
		outcome Outcome
	}{
		{JobClassify, OutcomeBug},
		{JobClassify, OutcomeFeature},
		{JobPlanning, OutcomeQuestions},
		{JobPlanning, OutcomeReady},
		{JobPlanning, OutcomeChildren},
		{JobPlanning, OutcomeNothingToDo},
		{JobPlanreview, OutcomeOk},
		{JobBuild, OutcomeOk},
		{JobPerimeter, OutcomeOk},
		{JobReview, OutcomeOk},
		{JobJudge, OutcomeOk},
		{JobRespond, OutcomeOk},
		{JobSide, OutcomeOk},
	}
	for _, tt := range tests {
		t.Run(string(tt.job)+"/"+string(tt.outcome), func(t *testing.T) {
			t.Parallel()
			r, err := Lookup(tt.job, tt.outcome)
			if err != nil {
				t.Fatalf("Lookup(%s, %s) = %v, want no error", tt.job, tt.outcome, err)
			}
			if r == nil {
				t.Fatal("Lookup returned a nil Response")
			}
		})
	}
}

func TestLookup_UniversalOutcomes(t *testing.T) {
	t.Parallel()

	for _, j := range Job("").Values() {
		job := Job(j)
		t.Run(string(job)+"/question", func(t *testing.T) {
			t.Parallel()
			r, err := Lookup(job, OutcomeQuestion)
			if err != nil {
				t.Fatalf("Lookup(%s, question) = %v, want no error", job, err)
			}
			if _, ok := r.(*QuestionResponse); !ok {
				t.Errorf("Lookup(%s, question) type = %T, want *QuestionResponse", job, r)
			}
		})
		t.Run(string(job)+"/error", func(t *testing.T) {
			t.Parallel()
			r, err := Lookup(job, OutcomeError)
			if err != nil {
				t.Fatalf("Lookup(%s, error) = %v, want no error", job, err)
			}
			if _, ok := r.(*ErrorResponse); !ok {
				t.Errorf("Lookup(%s, error) type = %T, want *ErrorResponse", job, r)
			}
		})
	}
}

func TestLookup_UnknownPair(t *testing.T) {
	t.Parallel()

	_, err := Lookup(JobClassify, OutcomeReady)
	if err == nil {
		t.Fatal("Lookup(classify, ready) = nil error, want an error")
	}
	want := classifyReadyUnregisteredErr
	if got := err.Error(); got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
}

// TestRegisteredPairs_MatchesPlanClosedSet is the independent, hand-typed
// reader of the registry's contents design section 7.1 calls for: it
// builds the closed set from the plan's table itself, not from anything
// buildRegistry shares, so a pair that silently stops resolving (or one
// that should not be there) is still caught even though RegisteredPairs is
// derived from the same registry map Lookup uses.
func TestRegisteredPairs_MatchesPlanClosedSet(t *testing.T) {
	t.Parallel()

	named := []RegisteredPair{
		{JobClassify, OutcomeBug},
		{JobClassify, OutcomeFeature},
		{JobPlanning, OutcomeQuestions},
		{JobPlanning, OutcomeReady},
		{JobPlanning, OutcomeChildren},
		{JobPlanning, OutcomeNothingToDo},
		{JobPlanreview, OutcomeOk},
		{JobBuild, OutcomeOk},
		{JobPerimeter, OutcomeOk},
		{JobReview, OutcomeOk},
		{JobJudge, OutcomeOk},
		{JobRespond, OutcomeOk},
		{JobSide, OutcomeOk},
	}
	want := make(map[RegisteredPair]bool, len(named)+len(Job("").Values())*2)
	for _, p := range named {
		want[p] = true
	}
	for _, j := range Job("").Values() {
		job := Job(j)
		want[RegisteredPair{job, OutcomeQuestion}] = true
		want[RegisteredPair{job, OutcomeError}] = true
	}

	got := RegisteredPairs()
	if len(got) != len(want) {
		t.Fatalf("RegisteredPairs() has %d pairs, want %d", len(got), len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("RegisteredPairs() contains unexpected pair %+v", p)
		}
	}
	for p := range want {
		if !slices.Contains(got, p) {
			t.Errorf("RegisteredPairs() is missing %+v", p)
		}
	}
}

// TestRegisteredPairs_Sorted proves the enumeration is deterministic, so
// two callers (selftest and a future one) always see the same order.
func TestRegisteredPairs_Sorted(t *testing.T) {
	t.Parallel()

	got := RegisteredPairs()
	sorted := slices.Clone(got)
	slices.SortFunc(sorted, func(a, b RegisteredPair) int {
		if c := strings.Compare(string(a.Job), string(b.Job)); c != 0 {
			return c
		}
		return strings.Compare(string(a.Outcome), string(b.Outcome))
	})
	if !slices.Equal(got, sorted) {
		t.Errorf("RegisteredPairs() = %v, want sorted order %v", got, sorted)
	}
}

// TestLookup_FreshPointer proves each call returns its own pointer, not a
// shared one that callers could corrupt for each other.
func TestLookup_FreshPointer(t *testing.T) {
	t.Parallel()

	a, err := Lookup(JobClassify, OutcomeBug)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Lookup(JobClassify, OutcomeBug)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("Lookup returned the same pointer on two calls")
	}
}
