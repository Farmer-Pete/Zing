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
		{JobPlanning, OutcomeReplies},
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
			// Planning's own "question" spelling is overwritten to carry
			// Conversation too (design section 22.2, D31); every other job
			// keeps the plain universal QuestionResponse.
			if job == JobPlanning {
				if _, ok := r.(*PlanningQuestionsResponse); !ok {
					t.Errorf("Lookup(planning, question) type = %T, want *PlanningQuestionsResponse", r)
				}
				return
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
		{JobPlanning, OutcomeReplies},
		{JobPlanning, OutcomeConfirmed},
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

// TestPlanningQuestionOutcomesCarryReplies proves planning's two question
// spellings, questions and question, both decode to *PlanningQuestionsResponse
// (design section 22.2, D31), while every other job's question outcome
// still decodes to the plain *QuestionResponse.
func TestPlanningQuestionOutcomesCarryReplies(t *testing.T) {
	t.Parallel()

	for _, outcome := range []Outcome{OutcomeQuestions, OutcomeQuestion} {
		r, err := Lookup(JobPlanning, outcome)
		if err != nil {
			t.Fatalf("Lookup(planning, %s) = %v, want no error", outcome, err)
		}
		if _, ok := r.(*PlanningQuestionsResponse); !ok {
			t.Errorf("Lookup(planning, %s) type = %T, want *PlanningQuestionsResponse", outcome, r)
		}
	}

	r, err := Lookup(JobClassify, OutcomeQuestion)
	if err != nil {
		t.Fatalf("Lookup(classify, question) = %v, want no error", err)
	}
	if _, ok := r.(*QuestionResponse); !ok {
		t.Errorf("Lookup(classify, question) type = %T, want *QuestionResponse", r)
	}
}

// TestRepliesOutcomeIsPlanningOnly proves replies, the new outcome D31
// adds, resolves only for planning (to *RepliesResponse) and is not an
// outcome of any other job.
func TestRepliesOutcomeIsPlanningOnly(t *testing.T) {
	t.Parallel()

	r, err := Lookup(JobPlanning, OutcomeReplies)
	if err != nil {
		t.Fatalf("Lookup(planning, replies) = %v, want no error", err)
	}
	if _, ok := r.(*RepliesResponse); !ok {
		t.Errorf("Lookup(planning, replies) type = %T, want *RepliesResponse", r)
	}

	_, err = Lookup(JobBuild, OutcomeReplies)
	if err == nil {
		t.Fatal("Lookup(build, replies) = nil error, want an error: replies is not an outcome of build")
	}
	want := "no response for job build outcome replies"
	if got := err.Error(); got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
}

// TestConfirmedIsPlanningOnly proves confirmed, D32's own confirming-turn
// outcome, resolves only for planning (to *ConfirmedResponse) and is not an
// outcome of any other job.
func TestConfirmedIsPlanningOnly(t *testing.T) {
	t.Parallel()

	r, err := Lookup(JobPlanning, OutcomeConfirmed)
	if err != nil {
		t.Fatalf("Lookup(planning, confirmed) = %v, want no error", err)
	}
	if _, ok := r.(*ConfirmedResponse); !ok {
		t.Errorf("Lookup(planning, confirmed) type = %T, want *ConfirmedResponse", r)
	}

	_, err = Lookup(JobBuild, OutcomeConfirmed)
	if err == nil {
		t.Fatal("Lookup(build, confirmed) = nil error, want an error: confirmed is not an outcome of build")
	}
	want := "no response for job build outcome confirmed"
	if got := err.Error(); got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
}
