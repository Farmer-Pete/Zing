package response

import "testing"

// TestExampleFS_EveryExampleParsesAndValidates proves every embedded
// example under examples/*.xml is a document Parse and Validate accept, so
// cmd/zing's selftest (which runs the same two calls per example) can rely
// on ExampleFS holding nothing broken.
func TestExampleFS_EveryExampleParsesAndValidates(t *testing.T) {
	t.Parallel()

	files, err := ExampleFiles()
	if err != nil {
		t.Fatalf("ExampleFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("ExampleFiles returned none")
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := ExampleFS.ReadFile(name)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", name, err)
			}
			doc, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse(%s): %v", name, err)
			}
			if errs := Validate(doc, ValidateContext{Kind: KindFeature}); len(errs) != 0 {
				t.Fatalf("Validate(%s) = %v, want no errors", name, dumpErrs(errs))
			}
		})
	}
}

// wantedRegistryPairs is every (job, outcome) pair design section 7.1 names
// that task 4 requires an example for: each job-specific outcome once, plus
// one example each of the two universal outcomes (question, error), which
// share one response type across every job (design section 7.1).
var wantedRegistryPairs = []registryKey{
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

// TestExampleFS_CoversEveryRegistryPair proves the embedded examples reach
// every pair wantedRegistryPairs names, plus at least one document with
// outcome "question" and one with outcome "error" (the two outcomes shared
// by every job, design section 7.1), so selftest's parse-and-validate step
// actually exercises the whole registry, not just whichever files happen to
// be checked in.
func TestExampleFS_CoversEveryRegistryPair(t *testing.T) {
	t.Parallel()

	files, err := ExampleFiles()
	if err != nil {
		t.Fatalf("ExampleFiles: %v", err)
	}

	seen := make(map[registryKey]bool, len(files))
	var sawQuestion, sawError bool
	for _, name := range files {
		data, err := ExampleFS.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		doc, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse(%s): %v", name, err)
		}
		h := doc.Response.Header()
		seen[registryKey{h.Job, h.Outcome}] = true
		switch h.Outcome {
		case OutcomeQuestion:
			sawQuestion = true
		case OutcomeError:
			sawError = true
		}
	}

	for _, want := range wantedRegistryPairs {
		if !seen[want] {
			t.Errorf("no example for job=%s outcome=%s", want.Job, want.Outcome)
		}
	}
	if !sawQuestion {
		t.Error("no example with outcome=question")
	}
	if !sawError {
		t.Error("no example with outcome=error")
	}
}
