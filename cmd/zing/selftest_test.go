package main

import (
	"strings"
	"testing"
	"testing/fstest"

	"zing/internal/response"
	"zing/internal/schemagen"
)

// TestRunSelftest_ReturnsZeroOnEmptyMachine proves the whole selftest suite
// passes end to end, schema and template checks through the design section
// 11 dispatcher-to-done e2e suite (selftestE2E) included: runSelftest
// prints "selftest: OK" and exits 0.
func TestRunSelftest_ReturnsZeroOnEmptyMachine(t *testing.T) {
	if got := runSelftest(); got != 0 {
		t.Errorf("runSelftest() = %d, want 0", got)
	}
}

// TestSelftestE2E_TicketReachesDoneWithOneQuestionAnswered isolates the
// section 11 end-to-end suite from the rest of selftest, so a failure here
// names the e2e path specifically rather than surfacing only as
// runSelftest's generic non-zero exit.
func TestSelftestE2E_TicketReachesDoneWithOneQuestionAnswered(t *testing.T) {
	if err := selftestE2E(t.Context()); err != nil {
		t.Errorf("selftestE2E() = %v, want nil", err)
	}
}

// TestSchemagenDiff_CatchesTamperedSchema proves the schema-drift check
// selftest runs (step 3 of section 6.9) actually detects a tampered
// committed schema, using an in-memory filesystem so the real committed
// files stay untouched.
func TestSchemagenDiff_CatchesTamperedSchema(t *testing.T) {
	generated, err := schemagen.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tampered := make(fstest.MapFS, len(generated))
	for relpath, b := range generated {
		tampered[relpath] = &fstest.MapFile{Data: b}
	}

	var tamperedPath string
	for relpath := range generated {
		tamperedPath = relpath
		break
	}
	tampered[tamperedPath] = &fstest.MapFile{Data: []byte("{}")}

	diffs, err := schemagen.Diff(tampered)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diffs) != 1 || diffs[0] != tamperedPath {
		t.Errorf("Diff(tampered) = %v, want [%s]", diffs, tamperedPath)
	}
}

// TestCheckResponseTemplates_RendersEveryRegisteredPair proves the pair
// list task 6 hardcodes (registeredPairs) actually renders end to end: an
// unregistered or misnamed pair here would make RenderTemplate error.
func TestCheckResponseTemplates_RendersEveryRegisteredPair(t *testing.T) {
	if err := checkResponseTemplates(); err != nil {
		t.Errorf("checkResponseTemplates() = %v, want nil", err)
	}
}

// TestCheckResponseExamples_RealExamplesPass proves the real embedded
// examples (internal/response/examples/*.xml) all parse and validate, the
// same check selftest itself runs.
func TestCheckResponseExamples_RealExamplesPass(t *testing.T) {
	if err := checkResponseExamples(response.ExampleFS); err != nil {
		t.Errorf("checkResponseExamples(response.ExampleFS) = %v, want nil", err)
	}
}

// TestCheckResponseExamples_CatchesTamperedExample proves the
// parse-and-validate step selftest runs over response.ExampleFS actually
// detects a broken example, using an in-memory copy so the real committed
// files stay untouched.
func TestCheckResponseExamples_CatchesTamperedExample(t *testing.T) {
	files, err := response.ExampleFiles()
	if err != nil {
		t.Fatalf("ExampleFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("ExampleFiles returned none")
	}

	tampered := make(fstest.MapFS, len(files))
	for _, name := range files {
		data, err := response.ExampleFS.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		tampered[name] = &fstest.MapFile{Data: data}
	}

	// A well-formed <zing> element that fails Validate: classify/bug
	// requires a non-empty <reason>, which this omits.
	tampered[files[0]] = &fstest.MapFile{Data: []byte(`<zing job="classify" outcome="bug"></zing>`)}

	gotErr := checkResponseExamples(tampered)
	if gotErr == nil {
		t.Fatal("checkResponseExamples(tampered) = nil, want an error")
	}
	// Tie the failure to the planted file and to the mechanism under test
	// (Validate rejecting the missing <reason>), so a regression that fails
	// for some other reason cannot quietly satisfy this test.
	if !strings.Contains(gotErr.Error(), files[0]) {
		t.Errorf("error %q does not name the tampered example %s", gotErr, files[0])
	}
	if !strings.Contains(gotErr.Error(), "reason: missing required element") {
		t.Errorf("error %q is not the expected missing-reason validation error", gotErr)
	}
}
