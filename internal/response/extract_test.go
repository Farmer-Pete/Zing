package response

import (
	"strings"
	"testing"
	"time"
)

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

// TestExtractAll_ManyUnclosedStartsIsBoundedAndFast proves the candidate cap
// (maxRootCandidates): an output packed with unclosed "<zing" starts, each of
// which would otherwise make wellFormedRootExtent scan to EOF, returns no
// roots quickly rather than doing O(n^2) work (PR #23 review). The caller
// then reports its ordinary no-zing-element failure.
func TestExtractAll_ManyUnclosedStartsIsBoundedAndFast(t *testing.T) {
	t.Parallel()

	// Far more unclosed starts than the cap, so an uncapped scan would parse
	// every one of them to EOF. 2000 keeps the capped work (64 scans of a
	// 16 KiB input) well under the guard even under -race on a slow runner;
	// 20000 tripped the 5s guard in CI while still finishing.
	hostile := strings.Repeat("<zing >\n", 2000)

	done := make(chan []string, 1)
	go func() { done <- ExtractAll(hostile) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("ExtractAll returned %d roots, want 0 (no well-formed document)", len(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExtractAll did not return within 5s; the candidate scan is not bounded")
	}
}

// TestExtractAll_RepairsBareLessThan is ExtractAll's half of the ticket's
// live build/#69 shape: a bare <nil> inside <report> breaks the strict
// scan, so the repair pass must still return exactly one root, with the
// placeholder escaped and kept as text.
func TestExtractAll_RepairsBareLessThan(t *testing.T) {
	t.Parallel()

	broken := `<zing job="build" outcome="ok"><report>x is <nil></report></zing>`
	want := `<zing job="build" outcome="ok"><report>x is &lt;nil></report></zing>`
	input := "log line\n" + broken + "\ndone"

	got := ExtractAll(input)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, want)
	}
}

// TestExtractAll_StrictRootSkipsRepair guards goal 2 for ExtractAll: once
// the strict pass finds a well-formed root, the repair pass never runs, so
// a broken candidate earlier in the text contributes nothing and the valid
// root comes back unchanged.
func TestExtractAll_StrictRootSkipsRepair(t *testing.T) {
	t.Parallel()

	broken := `<zing job="build" outcome="ok"><report>x is <nil></report></zing>`
	input := broken + "\n" + wellFormedClassify

	got := ExtractAll(input)
	if len(got) != 1 || got[0] != wellFormedClassify {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, wellFormedClassify)
	}
}

// TestExtractAll_UnregisteredPairIsNotRepaired mirrors
// TestParse_UnregisteredPairIsNotRepaired: ExtractAll's repair pass also
// needs the (job, outcome) pair's shape, so an unregistered pair gets no
// root even though the strict pass also failed on the bare <.
func TestExtractAll_UnregisteredPairIsNotRepaired(t *testing.T) {
	t.Parallel()

	doc := `<zing job="classify" outcome="ready"><reason>a < b</reason></zing>`
	got := ExtractAll(doc)
	if len(got) != 0 {
		t.Fatalf("ExtractAll = %v, want zero roots", got)
	}
}
