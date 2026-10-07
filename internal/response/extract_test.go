package response

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// brokenBuildReportNil is a build/ok document whose report holds a single
// bare < placeholder, the live run 283 shape: the strict scan rejects it,
// and the repair pass must turn it into exactly one well-formed root.
const brokenBuildReportNil = `<zing job="build" outcome="ok"><report>x is <nil></report></zing>`

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

	got := ExtractAll(classifyReadyDoc)
	if len(got) != 1 || got[0] != classifyReadyDoc {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, classifyReadyDoc)
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

	want := `<zing job="build" outcome="ok"><report>x is &lt;nil></report></zing>`
	input := "log line\n" + brokenBuildReportNil + "\ndone"

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

	input := brokenBuildReportNil + "\n" + wellFormedClassify

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

// TestExtractAll_StrictRootWithUnknownChildSkipsRepair is ExtractAll's half
// of TestParse_StrictDocumentKeepsItsBytes: the input holds no bare <, so
// the strict pass finds it well formed and flattens reason's <code> child
// to a backtick. A repair-first ExtractAll would instead escape <code>'s
// opening < as text and fail to match its closing </code> against the
// open reason frame, changing the root a different way.
func TestExtractAll_StrictRootWithUnknownChildSkipsRepair(t *testing.T) {
	t.Parallel()

	in := `<zing job="classify" outcome="bug"><reason>use <code>x</code></reason></zing>`
	want := "<zing job=\"classify\" outcome=\"bug\"><reason>use `x`</reason></zing>"
	got := ExtractAll(in)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ExtractAll = %v, want exactly [%q]", got, want)
	}
}

// TestExtractStrict_HitsCapReturnsNilTrue pins extractStrict's own half of
// the `|| capped` short-circuit: a well-formed root before the flood of
// unclosed starts would otherwise show up in roots, but past
// maxRootCandidates candidates extractStrict returns (nil, true), dropping
// that root rather than returning whatever partial roots it had found. That
// is also why ExtractAll checks capped at all: without it, ExtractAll would
// fall through to the repair pass and redo the identical, already-bounded
// scan for no benefit, since extractRepaired hits the same cap on the same
// candidate list.
func TestExtractStrict_HitsCapReturnsNilTrue(t *testing.T) {
	t.Parallel()

	hostile := wellFormedClassify + strings.Repeat("<zing >\n", 2000)
	roots, _, capped := extractStrict([]byte(hostile))
	if !capped {
		t.Fatal("capped = false, want true past maxRootCandidates")
	}
	if roots != nil {
		t.Errorf("roots = %v, want nil (the earlier well-formed root must not survive the cap)", roots)
	}
}

// TestExtractAll_RepairLogsEscapedCount is TestRepairLogsEscapedCount's
// ExtractAll half: the runtime calls ExtractAll before it ever calls
// Parse, so extractRepaired's own logRepair call needs its own coverage.
// Not parallel: it swaps slog's global default.
func TestExtractAll_RepairLogsEscapedCount(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var infoBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if got := ExtractAll(brokenBuildReportNil); len(got) != 1 {
		t.Fatalf("ExtractAll = %v, want one root", got)
	}
	got := infoBuf.String()
	if strings.Count(got, "repaired bare < in zing document") != 1 {
		t.Errorf("info log = %q, want exactly one repair record", got)
	}
	if !strings.Contains(got, "escaped=1") || !strings.Contains(got, "roots=1") {
		t.Errorf("info log = %q, want escaped=1 and roots=1", got)
	}

	var warnBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&warnBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if got := ExtractAll(brokenBuildReportNil); len(got) != 1 {
		t.Fatalf("ExtractAll = %v, want one root", got)
	}
	if warnBuf.Len() != 0 {
		t.Errorf("warn-level log = %q, want nothing written", warnBuf.String())
	}
}

// TestExtractAll_RepairLogsRootsOverOne guards the roots count in the
// repair log record: a message holding two repairable candidates gives
// ExtractAll two roots,
// which runtime.parseFinalMessage then rejects outright as
// reasonMultipleZingDocs. The log record must say roots=2, not read like a
// single clean repair, so an operator matching repair records to runs by
// timestamp (the plan's workaround until issue #94) does not mistake this
// for a successful one.
func TestExtractAll_RepairLogsRootsOverOne(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	b2 := `<zing job="build" outcome="ok"><report>y is <branch></report></zing>`

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if got := ExtractAll(brokenBuildReportNil + "\n" + b2); len(got) != 2 {
		t.Fatalf("ExtractAll = %v, want two roots", got)
	}
	got := buf.String()
	if !strings.Contains(got, "roots=2") {
		t.Errorf("info log = %q, want roots=2", got)
	}
	if !strings.Contains(got, "escaped=2") {
		t.Errorf("info log = %q, want escaped=2 (one < repaired in each root)", got)
	}
}
