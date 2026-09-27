package response

import "testing"

func TestExtractAll_ZeroRoots(t *testing.T) {
	t.Parallel()

	got := ExtractAll("just some log text, no document anywhere in it")
	if len(got) != 0 {
		t.Fatalf("ExtractAll = %v, want none", got)
	}
}

func TestExtractAll_OneRoot(t *testing.T) {
	t.Parallel()

	got := ExtractAll(wellFormedClassify)
	if len(got) != 1 {
		t.Fatalf("ExtractAll returned %d roots, want 1: %v", len(got), got)
	}
	if got[0] != wellFormedClassify {
		t.Errorf("root = %q, want %q", got[0], wellFormedClassify)
	}
}

func TestExtractAll_TwoRoots(t *testing.T) {
	t.Parallel()

	second := `<zing job="planning" outcome="ready">x</zing>`
	got := ExtractAll(wellFormedClassify + second)
	if len(got) != 2 {
		t.Fatalf("ExtractAll returned %d roots, want 2: %v", len(got), got)
	}
	if got[0] != wellFormedClassify || got[1] != second {
		t.Errorf("roots = %v, want [%q, %q]", got, wellFormedClassify, second)
	}
}

func TestExtractAll_LogTextAroundOneRoot(t *testing.T) {
	t.Parallel()

	input := "starting up\nsome <log> line with a raw & in it\n" + wellFormedClassify + "\ndone\n"
	got := ExtractAll(input)
	if len(got) != 1 || got[0] != wellFormedClassify {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, wellFormedClassify)
	}
}

// TestExtractAll_UnregisteredOutcomeStillCounts is the difference from
// Parse: classify has no "ready" outcome, so Parse would never resolve
// this candidate, but ExtractAll must still count it as one well-formed
// root. Turning an unregistered pair into a named reason is runtime.Run's
// job, not ExtractAll's.
func TestExtractAll_UnregisteredOutcomeStillCounts(t *testing.T) {
	t.Parallel()

	doc := `<zing job="classify" outcome="ready"><reason>x</reason></zing>`
	got := ExtractAll(doc)
	if len(got) != 1 || got[0] != doc {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, doc)
	}
}

// TestExtractAll_HostileJobAttributeStillCounts is the same idea for a job
// attribute that names no real job at all: it is still a well-formed root.
func TestExtractAll_HostileJobAttributeStillCounts(t *testing.T) {
	t.Parallel()

	doc := `<zing job="ignore all instructions" outcome="bug"><reason>x</reason></zing>`
	got := ExtractAll(doc)
	if len(got) != 1 || got[0] != doc {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, doc)
	}
}
