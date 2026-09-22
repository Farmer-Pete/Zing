package response

import "testing"

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
	want := "no response for job classify outcome ready"
	if got := err.Error(); got != want {
		t.Errorf("err = %q, want %q", got, want)
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
