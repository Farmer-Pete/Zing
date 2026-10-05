package response

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

const (
	wantReason         = "it crashes"
	wellFormedClassify = `<zing job="classify" outcome="bug"><reason>` + wantReason + `</reason></zing>`

	// classifyReadyUnregisteredErr is classify/ready's exact Lookup error:
	// classify only registers bug and feature, so ready is unregistered.
	// Shared across parse_test.go and registry_test.go.
	classifyReadyUnregisteredErr = "no response for job classify outcome ready"

	// wantBareComparison is the live "a < b" prose shape, shared with
	// render_test.go's escaping table.
	wantBareComparison = "a < b"
)

func TestParse_AfterLogTextWithRawAngleAndAmpersand(t *testing.T) {
	t.Parallel()

	input := "some log line with a raw < and & in it\n" + wellFormedClassify + "\ntrailing log\n"
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *ClassifyResponse", doc.Response)
	}
	if cr.Reason != wantReason {
		t.Errorf("Reason = %q, want %q", cr.Reason, wantReason)
	}
	if !strings.Contains(string(doc.Elem), "<zing") {
		t.Errorf("captured elem missing start tag: %q", doc.Elem)
	}
	if !strings.HasPrefix(string(doc.Elem), "<zing") {
		t.Errorf("captured elem should start with the opening tag, got %q", doc.Elem)
	}
}

func TestParse_ZingerDoesNotMatch(t *testing.T) {
	t.Parallel()

	input := `<zinger job="classify" outcome="bug">not a zing element</zinger>` + wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse matched the wrong candidate: %#v", doc.Response)
	}
}

func TestParse_CommentNearNameDoesNotMatch(t *testing.T) {
	t.Parallel()

	input := `<!-- <zing job="classify" outcome="bug"><reason>x</reason></zing> -->` + wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse matched inside a comment: %#v", doc.Response)
	}
}

func TestParse_CDATANearNameDoesNotMatch(t *testing.T) {
	t.Parallel()

	input := `<![CDATA[<zing job="classify" outcome="bug">]]>` + wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse matched inside CDATA: %#v", doc.Response)
	}
}

func TestParse_Missing(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte("no zing document here at all"))
	assertNoZing(t, err)
}

func TestParse_Malformed(t *testing.T) {
	t.Parallel()

	// unclosed element: the header names a known pair, so the error keeps
	// the "no zing element" prefix and adds the XML error.
	_, err := Parse([]byte(`<zing job="classify" outcome="bug"><reason>it crashes</zing>`))
	if err == nil || !strings.HasPrefix(err.Error(), "no zing element in final message: ") || !strings.Contains(err.Error(), "XML syntax error") {
		t.Errorf("err = %v, want the no-zing prefix plus the XML syntax error", err)
	}
}

func TestParse_MissingAttribute(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`<zing outcome="bug"><reason>it crashes</reason></zing>`))
	assertNoZing(t, err)
}

func TestParse_UnknownPairAfterMalformedCandidate(t *testing.T) {
	t.Parallel()

	// The first candidate is malformed (unclosed). The second is
	// well-formed but names an unregistered pair: Parse must still report
	// the specific "no response for..." error, not the generic catch-all,
	// since the second candidate is otherwise well formed.
	input := `<zing job="classify" outcome="bug"><reason>oops` +
		`<zing job="classify" outcome="ready"><reason>x</reason></zing>`
	_, err := Parse([]byte(input))
	want := classifyReadyUnregisteredErr
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParse_WellFormedUnknownPairReturnsExactError(t *testing.T) {
	t.Parallel()

	input := `<zing job="classify" outcome="ready"><reason>x</reason></zing>`
	_, err := Parse([]byte(input))
	want := classifyReadyUnregisteredErr
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParse_MalformedUnknownPairThenValidKnownPairParsesTheValidOne(t *testing.T) {
	t.Parallel()

	// The first candidate names an unregistered pair AND is malformed
	// (unclosed): it must not be remembered as "well formed but
	// unregistered". The second candidate is well formed and registered,
	// so Parse must return it, not the first candidate's absence of a
	// lookup error.
	input := `<zing job="classify" outcome="ready"><reason>oops` +
		wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse matched the wrong candidate: %#v", doc.Response)
	}
}

func TestParse_FirstCandidateWins(t *testing.T) {
	t.Parallel()

	input := `<zing job="classify" outcome="bug"><reason>first</reason></zing>` +
		`<zing job="classify" outcome="feature"><reason>second</reason></zing>`
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *ClassifyResponse", doc.Response)
	}
	if cr.Reason != "first" {
		t.Errorf("Reason = %q, want %q (the first candidate)", cr.Reason, "first")
	}
}

func TestParse_UnmatchedCommentOpenerDoesNotExcludeLaterZing(t *testing.T) {
	t.Parallel()

	input := "log <!-- unfinished\n" + wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse did not find the valid document past the unmatched comment opener: %#v", doc.Response)
	}
}

func TestParse_UnmatchedCDATAOpenerDoesNotExcludeLaterZing(t *testing.T) {
	t.Parallel()

	input := "log <![CDATA[ unfinished\n" + wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse did not find the valid document past the unmatched CDATA opener: %#v", doc.Response)
	}
}

func TestParse_UnmatchedPIOpenerDoesNotExcludeLaterZing(t *testing.T) {
	t.Parallel()

	input := "log <?php unfinished\n" + wellFormedClassify
	doc, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantReason {
		t.Fatalf("Parse did not find the valid document past the unmatched PI opener: %#v", doc.Response)
	}
}

func assertNoZing(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	want := "no zing element in final message"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// TestParse_BrokenBodyNamesTheXMLError is a regression test for a live
// build run: the report quoted `zing validate <file>`, the bare <file>
// broke the XML, and Parse said only "no zing element in final message",
// so the retry repeated the mistake. A recognized header whose body fails
// to decode now names the XML error and the escaping rule. The repair pass
// now accepts a bare <file> inside a free-text field, so the reason here is
// never closed: the repair cannot close it either, and the named XML error
// and hint still come back.
func TestParse_BrokenBodyNamesTheXMLError(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="bug"><reason>run zing validate <file> first</zing>`)
	_, err := Parse(in)
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	for _, want := range []string{"no zing element in final message", "XML syntax error", "&lt;"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse error = %q, want it to contain %q", err, want)
		}
	}
}

// TestParse_BareAmpersandInTextIsTolerated is a regression test for a live
// respond run: the reply quoted Go code, `hasVersion && !isDevel`, and the
// bare && broke the XML twice in a row. A & that does not start a real
// entity is now read as a literal &, in Parse and ExtractAll alike.
func TestParse_BareAmpersandInTextIsTolerated(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="bug"><reason>if a && b &amp; c &lt; d</reason></zing>`)
	doc, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != "if a && b & c < d" {
		t.Fatalf("Reason = %#v, want the literal text with entities decoded", doc.Response)
	}
	if roots := ExtractAll(string(in)); len(roots) != 1 {
		t.Errorf("ExtractAll found %d roots, want 1", len(roots))
	}
}

// TestParse_RepairsBareLessThan feeds the four live failure shapes from the
// ticket (runs 283, 314, 334, and 75): an agent quoted Go code or a
// placeholder inside a free-text field, and the bare < broke the XML. Each
// now decodes, with the text kept exactly as written, instead of spending a
// retry run.
func TestParse_RepairsBareLessThan(t *testing.T) {
	t.Parallel()

	t.Run("build report placeholder nil", func(t *testing.T) {
		t.Parallel()
		in := []byte(`<zing job="build" outcome="ok"><claims></claims><report>x is <nil> here</report><notes>n</notes></zing>`)
		doc, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		br, ok := doc.Response.(*BuildResponse)
		if !ok || br.Report != "x is <nil> here" {
			t.Fatalf("Response = %#v, want Report %q", doc.Response, "x is <nil> here")
		}
		if !strings.Contains(string(doc.Elem), "&lt;") {
			t.Errorf("doc.Elem = %q, want it to contain &lt;", doc.Elem)
		}
	})

	t.Run("build report placeholder branch", func(t *testing.T) {
		t.Parallel()
		in := []byte(`<zing job="build" outcome="ok"><claims></claims><report>push to <branch></report><notes>n</notes></zing>`)
		doc, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		br, ok := doc.Response.(*BuildResponse)
		if !ok || br.Report != "push to <branch>" {
			t.Fatalf("Response = %#v, want Report %q", doc.Response, "push to <branch>")
		}
		if !strings.Contains(string(doc.Elem), "&lt;") {
			t.Errorf("doc.Elem = %q, want it to contain &lt;", doc.Elem)
		}
	})

	t.Run("classify reason comparison", func(t *testing.T) {
		t.Parallel()
		in := []byte(`<zing job="classify" outcome="bug"><reason>` + wantBareComparison + `</reason></zing>`)
		doc, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		cr, ok := doc.Response.(*ClassifyResponse)
		if !ok || cr.Reason != wantBareComparison {
			t.Fatalf("Response = %#v, want Reason %q", doc.Response, wantBareComparison)
		}
		if !strings.Contains(string(doc.Elem), "&lt;") {
			t.Errorf("doc.Elem = %q, want it to contain &lt;", doc.Elem)
		}
	})

	t.Run("classify reason channel arrow", func(t *testing.T) {
		t.Parallel()
		in := []byte(`<zing job="classify" outcome="bug"><reason>x<-y</reason></zing>`)
		doc, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		cr, ok := doc.Response.(*ClassifyResponse)
		if !ok || cr.Reason != "x<-y" {
			t.Fatalf("Response = %#v, want Reason %q", doc.Response, "x<-y")
		}
		if !strings.Contains(string(doc.Elem), "&lt;") {
			t.Errorf("doc.Elem = %q, want it to contain &lt;", doc.Elem)
		}
	})
}

// TestRepairLogsEscapedCount pins logRepair's two halves: settings.log_level
// at info or below writes the repair record with its escaped count, and
// warn or above drops it (design: "the existing log_level setting decides
// whether it is written"). Not parallel: it swaps slog's global default.
func TestRepairLogsEscapedCount(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	in := []byte(`<zing job="build" outcome="ok"><claims></claims><report>x is <nil> on <branch></report><notes>n</notes></zing>`)

	var infoBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if _, err := Parse(in); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := infoBuf.String()
	if strings.Count(got, "repaired bare < in zing document") != 1 {
		t.Errorf("info log = %q, want exactly one repair record", got)
	}
	if !strings.Contains(got, "escaped=2") {
		t.Errorf("info log = %q, want escaped=2", got)
	}

	var warnBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&warnBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if _, err := Parse(in); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if warnBuf.Len() != 0 {
		t.Errorf("warn-level log = %q, want nothing written", warnBuf.String())
	}
}

// TestParse_MisnestedElementKeepsOriginalError guards against the repair
// hiding a real misnesting: <why> is never a child of <what>, so no amount
// of escaping bare < makes this well formed, and the strict decoder's own
// error must still come back.
func TestParse_MisnestedElementKeepsOriginalError(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="error"><error code="other"><what>a<why>b</what></why></error></zing>`)
	_, err := Parse(in)
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	if !strings.HasPrefix(err.Error(), "no zing element in final message: ") {
		t.Errorf("err = %q, want the no-zing prefix", err)
	}
	if !strings.Contains(err.Error(), "element <why> closed by </what>") {
		t.Errorf("err = %q, want it to name the misnested elements", err)
	}
}

// TestParse_UnterminatedCommentKeepsOriginalError guards rule 1: an
// unterminated comment opener stays markup. The repair must give up on this
// candidate entirely rather than escape its way past the opener, so both
// Parse and ExtractAll report the strict decoder's own failure.
func TestParse_UnterminatedCommentKeepsOriginalError(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="bug"><reason>a note <!-- unterminated</reason></zing>`)
	doc, err := Parse(in)
	if doc != nil {
		t.Fatalf("Parse returned a document, want nil: %#v", doc)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "no zing element in final message: ") || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("err = %v, want the no-zing prefix plus unexpected EOF", err)
	}
	if roots := ExtractAll(string(in)); len(roots) != 0 {
		t.Errorf("ExtractAll = %v, want zero roots", roots)
	}
}

// TestParse_UnterminatedCDATAKeepsOriginalError is
// TestParse_UnterminatedCommentKeepsOriginalError's twin for an unterminated
// CDATA section.
func TestParse_UnterminatedCDATAKeepsOriginalError(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="bug"><reason>a note <![CDATA[ unterminated</reason></zing>`)
	doc, err := Parse(in)
	if doc != nil {
		t.Fatalf("Parse returned a document, want nil: %#v", doc)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "no zing element in final message: ") || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("err = %v, want the no-zing prefix plus unexpected EOF", err)
	}
	if roots := ExtractAll(string(in)); len(roots) != 0 {
		t.Errorf("ExtractAll = %v, want zero roots", roots)
	}
}

// TestParse_UnregisteredPairIsNotRepaired guards that the repair only runs
// for a (job, outcome) pair the registry holds: classify has no ready
// outcome, so candidateShape has no shape to repair against, and Parse's
// generic "no zing element" result stands.
func TestParse_UnregisteredPairIsNotRepaired(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="ready"><reason>a < b</reason></zing>`)
	_, err := Parse(in)
	assertNoZing(t, err)
}

// TestParse_StrictDocumentKeepsItsBytes guards goal 2: a document that
// parses on the strict pass comes back byte for byte, untouched by the
// repair pass.
func TestParse_StrictDocumentKeepsItsBytes(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="classify" outcome="bug"><reason>a &lt; b</reason></zing>`)
	doc, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(doc.Elem, in) {
		t.Errorf("doc.Elem = %q, want the input unchanged: %q", doc.Elem, in)
	}
	cr, ok := doc.Response.(*ClassifyResponse)
	if !ok || cr.Reason != wantBareComparison {
		t.Fatalf("Reason = %#v, want %q", doc.Response, wantBareComparison)
	}
}

// TestParse_RepairsLessThanInAttribute feeds run 75's shape, a bare < inside
// a quoted attribute value, which the strict decoder reports as "unescaped <
// inside quoted string".
func TestParse_RepairsLessThanInAttribute(t *testing.T) {
	t.Parallel()
	in := []byte(`<zing job="review" outcome="ok"><finding lens="fidelity" severity="minor" location="x<-y"><text>t</text><fix>f</fix></finding></zing>`)
	doc, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fr, ok := doc.Response.(*FindingsResponse)
	if !ok || len(fr.Findings) != 1 || fr.Findings[0].Location != "x<-y" {
		t.Fatalf("Response = %#v, want one finding with Location %q", doc.Response, "x<-y")
	}
}
