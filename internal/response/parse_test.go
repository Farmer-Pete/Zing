package response

import (
	"strings"
	"testing"
)

const (
	wantReason         = "it crashes"
	wellFormedClassify = `<zing job="classify" outcome="bug"><reason>` + wantReason + `</reason></zing>`
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
	// well-formed but names an unregistered pair. Both are failures for
	// extraction purposes, so the exact string still wins.
	input := `<zing job="classify" outcome="bug"><reason>oops` +
		`<zing job="classify" outcome="ready"><reason>x</reason></zing>`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
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
