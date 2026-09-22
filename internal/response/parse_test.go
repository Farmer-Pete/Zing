package response

import (
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

	// unclosed element
	_, err := Parse([]byte(`<zing job="classify" outcome="bug"><reason>it crashes</zing>`))
	assertNoZing(t, err)
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
