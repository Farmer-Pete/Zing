package response

import (
	"encoding/xml"
	"strings"
	"testing"
)

// run98Literal is the judge run 98 document, quoted verbatim from the
// owner's capture of the Codex session file (see the ticket's Q5). It is
// the regression input for the backtick loss: encoding/xml drops the code
// child elements inside why and tried when decoded straight into a string
// field (internal/response/types.go:41-43).
const run98Literal = `<zing job="judge" outcome="error"><error code="cannot_run"><what>Scenario s7, the full unfiltered test suite, could not be completed.</what><why><code>go test ./...</code> emitted successful results through <code>internal/gitfixture</code>, then produced no further output or completion after approximately 90 seconds of repeated waits. I interrupted it; it exited with status 1.</why><tried>Ran <code>go build ./...</code> (exit 0). Targeted checks for s1 through s6 produced the specified PASS/SKIP outcomes. <code>make lint</code> exited 0 with 0 issues. Ran <code>go test ./...</code> and waited through three 30-second intervals after its initial output.</tried></error></zing>`

const run98ExpectedWhy = "`go test ./...` emitted successful results through `internal/gitfixture`, then produced no further output or completion after approximately 90 seconds of repeated waits. I interrupted it; it exited with status 1."

const run98ExpectedTried = "Ran `go build ./...` (exit 0). Targeted checks for s1 through s6 produced the specified PASS/SKIP outcomes. `make lint` exited 0 with 0 issues. Ran `go test ./...` and waited through three 30-second intervals after its initial output."

// TestExtractAll_Keeps36InlineCode reproduces #36: ExtractAll followed by
// xml.Unmarshal into JudgeErrorResponse, exactly the seam
// runtime.parseFinalMessage uses, must keep every backtick span from run
// 98's why and tried.
func TestExtractAll_Keeps36InlineCode(t *testing.T) {
	t.Parallel()

	roots := ExtractAll(run98Literal)
	if len(roots) != 1 {
		t.Fatalf("ExtractAll roots = %d, want 1", len(roots))
	}

	var doc JudgeErrorResponse
	if err := xml.Unmarshal([]byte(roots[0]), &doc); err != nil {
		t.Fatalf("xml.Unmarshal: %v", err)
	}

	if doc.Error.Why != run98ExpectedWhy {
		t.Errorf("Why =\n%q\nwant\n%q", doc.Error.Why, run98ExpectedWhy)
	}
	if doc.Error.Tried != run98ExpectedTried {
		t.Errorf("Tried =\n%q\nwant\n%q", doc.Error.Tried, run98ExpectedTried)
	}
}

// TestFlattenInline tables flattenInline's rewrite rules against several
// registered shapes: a judge error's why (free text with two code
// elements), a planning goal (free text with one non-code inline
// element), an option's chardata (mixed text with a code element nested
// in a b element), a self-closing inline tag, a document with no inline
// tag, and an unregistered header.
func TestFlattenInline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		elem string
		want string
		n    int
	}{
		{
			name: "code in why becomes backticks, what and tried keep their order and bytes",
			elem: `<zing job="judge" outcome="error"><error code="cannot_run"><what>w</what><why>run <code>go test ./...</code> now</why><tried>t</tried></error></zing>`,
			want: "<zing job=\"judge\" outcome=\"error\"><error code=\"cannot_run\"><what>w</what><why>run `go test ./...` now</why><tried>t</tried></error></zing>",
			n:    2,
		},
		{
			name: "em in a planning goal becomes its text",
			elem: `<zing job="planning" outcome="ready"><claims><claim kind="code" verdict="true" evidence="x.go:1">c</claim></claims><scenarios><scenario id="s1" kind="scripted" check="go test"><given>g</given><when>w</when><then>t</then></scenario><scenario id="s2" kind="scripted" check="go test"><given>g</given><when>w</when><then>t</then></scenario></scenarios><plan><overview><objective>o</objective><context>c</context><problem>p</problem><goals><goal>keep <em>this</em> working</goal></goals><nongoals><nongoal>n</nongoal></nongoals></overview><design><demo cmd="x">d</demo><shape>s</shape></design><delivery><files><file path="a" action="modify">r</file></files><deletions none="true"></deletions><tests><test name="t1" seam="s" kind="unit">a</test></tests><tasks><task n="1" test="t1">build it</task></tasks></delivery><review><trust_root>none</trust_root><alternatives><alternative>alt</alternative></alternatives><risks><risk>risk</risk></risks></review></plan></zing>`,
			want: `<zing job="planning" outcome="ready"><claims><claim kind="code" verdict="true" evidence="x.go:1">c</claim></claims><scenarios><scenario id="s1" kind="scripted" check="go test"><given>g</given><when>w</when><then>t</then></scenario><scenario id="s2" kind="scripted" check="go test"><given>g</given><when>w</when><then>t</then></scenario></scenarios><plan><overview><objective>o</objective><context>c</context><problem>p</problem><goals><goal>keep this working</goal></goals><nongoals><nongoal>n</nongoal></nongoals></overview><design><demo cmd="x">d</demo><shape>s</shape></design><delivery><files><file path="a" action="modify">r</file></files><deletions none="true"></deletions><tests><test name="t1" seam="s" kind="unit">a</test></tests><tasks><task n="1" test="t1">build it</task></tasks></delivery><review><trust_root>none</trust_root><alternatives><alternative>alt</alternative></alternatives><risks><risk>risk</risk></risks></review></plan></zing>`,
			n:    2,
		},
		{
			name: "code nested in b in an option's chardata becomes backticks",
			elem: `<zing job="build" outcome="question"><question key="q1"><title>t</title><body>b</body><option key="a">plain</option><option key="b">run <b><code>go test</code></b> now</option><recommended>a</recommended></question><progress>p</progress></zing>`,
			want: `<zing job="build" outcome="question"><question key="q1"><title>t</title><body>b</body><option key="a">plain</option><option key="b">run ` + "`go test` now</option><recommended>a</recommended></question><progress>p</progress></zing>",
			n:    4,
		},
		{
			name: "inline element with attributes loses the tag and keeps its text",
			elem: `<zing job="classify" outcome="bug"><reason>use <code class="x">go test</code> here</reason></zing>`,
			want: "<zing job=\"classify\" outcome=\"bug\"><reason>use `go test` here</reason></zing>",
			n:    2,
		},
		{
			name: "self-closing br is removed",
			elem: `<zing job="classify" outcome="bug"><reason>a<br/>b</reason></zing>`,
			want: `<zing job="classify" outcome="bug"><reason>ab</reason></zing>`,
			n:    1,
		},
		{
			name: "self-closing code is removed with no backtick",
			elem: `<zing job="classify" outcome="bug"><reason>use <code/>x</reason></zing>`,
			want: `<zing job="classify" outcome="bug"><reason>use x</reason></zing>`,
			n:    1,
		},
		{
			name: "no inline tag returns the same bytes",
			elem: `<zing job="classify" outcome="bug"><reason>plain text</reason></zing>`,
			want: `<zing job="classify" outcome="bug"><reason>plain text</reason></zing>`,
			n:    0,
		},
		{
			name: "unknown element under the root is skipped, code child included",
			elem: `<zing job="classify" outcome="bug"><reason>plain text</reason><extra><code>y</code></extra></zing>`,
			want: `<zing job="classify" outcome="bug"><reason>plain text</reason><extra><code>y</code></extra></zing>`,
			n:    0,
		},
		{
			name: "unregistered header returns count 0",
			elem: `<zing job="classify" outcome="nope"><reason>plain</reason></zing>`,
			want: `<zing job="classify" outcome="nope"><reason>plain</reason></zing>`,
			n:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, n := flattenInline([]byte(tc.elem))
			if n != tc.n {
				t.Errorf("rewritten = %d, want %d", n, tc.n)
			}
			if string(out) != tc.want {
				t.Errorf("out =\n%q\nwant\n%q", out, tc.want)
			}
			if tc.n == 0 && string(out) != tc.elem {
				t.Errorf("out = %q, want the input unchanged: %q", out, tc.elem)
			}
		})
	}
}

// TestParse_Keeps36InlineCode is the same regression through Parse, the
// seam the fake runtime and the Stop hook path use.
func TestParse_Keeps36InlineCode(t *testing.T) {
	t.Parallel()

	doc, err := Parse([]byte(run98Literal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	resp, ok := doc.Response.(*JudgeErrorResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *JudgeErrorResponse", doc.Response)
	}

	if resp.Error.Why != run98ExpectedWhy {
		t.Errorf("Why =\n%q\nwant\n%q", resp.Error.Why, run98ExpectedWhy)
	}
	if resp.Error.Tried != run98ExpectedTried {
		t.Errorf("Tried =\n%q\nwant\n%q", resp.Error.Tried, run98ExpectedTried)
	}
	if strings.Contains(string(doc.Elem), "<code>") {
		t.Errorf("Elem still holds a code tag: %s", doc.Elem)
	}
}
