package main

import (
	"testing"
	"testing/fstest"

	"zing/internal/schemagen"
)

func TestRunSelftest_ReturnsZeroOnEmptyMachine(t *testing.T) {
	if got := runSelftest(); got != 0 {
		t.Errorf("runSelftest() = %d, want 0", got)
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
