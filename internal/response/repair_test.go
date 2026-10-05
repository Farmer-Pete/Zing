package response

import (
	"reflect"
	"testing"
)

// TestRepairBareLessThan is a table over repairBareLessThan's scanning
// rules (design section "The scanner"), using the shapes of ClassifyResponse
// (reason: free text), BuildResponse (report: free text), and ReadyResponse
// (plan/overview/problem: mixed text and elements).
func TestRepairBareLessThan(t *testing.T) {
	t.Parallel()

	classifyRoot := shapeOf(reflect.TypeFor[ClassifyResponse]())
	buildRoot := shapeOf(reflect.TypeFor[BuildResponse]())
	readyRoot := shapeOf(reflect.TypeFor[ReadyResponse]())
	reviewRoot := shapeOf(reflect.TypeFor[FindingsResponse]())

	cases := []struct {
		name    string
		elem    string
		root    *node
		want    string
		escaped int
		ok      bool
	}{
		{
			name:    "comparison in free text",
			elem:    `<zing job="classify" outcome="bug"><reason>a < b</reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>a &lt; b</reason></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name:    "channel arrow in free text",
			elem:    `<zing job="classify" outcome="bug"><reason>x<-y</reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>x&lt;-y</reason></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name:    "two bare less-thans in one field",
			elem:    `<zing job="classify" outcome="bug"><reason>a <3 and b <= c</reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>a &lt;3 and b &lt;= c</reason></zing>`,
			escaped: 2,
			ok:      true,
		},
		{
			name:    "terminated comment stays markup",
			elem:    `<zing job="classify" outcome="bug"><reason><!-- c --></reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason><!-- c --></reason></zing>`,
			escaped: 0,
			ok:      true,
		},
		{
			name:    "terminated CDATA stays markup",
			elem:    `<zing job="classify" outcome="bug"><reason><![CDATA[x < y]]></reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason><![CDATA[x < y]]></reason></zing>`,
			escaped: 0,
			ok:      true,
		},
		{
			name: "unterminated comment fails the whole candidate",
			elem: `<zing job="classify" outcome="bug"><reason>a < b <!-- note</reason></zing>`,
			root: classifyRoot,
			ok:   false,
		},
		{
			name: "unterminated CDATA fails the whole candidate",
			elem: `<zing job="classify" outcome="bug"><reason>a < b <![CDATA[ raw</reason></zing>`,
			root: classifyRoot,
			ok:   false,
		},
		{
			name: "unterminated processing instruction fails the whole candidate",
			elem: `<zing job="classify" outcome="bug"><reason>a < b <? pi</reason></zing>`,
			root: classifyRoot,
			ok:   false,
		},
		{
			name:    "schema name elsewhere is still text in free-text reason",
			elem:    `<zing job="classify" outcome="bug"><reason>run <file></reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>run &lt;file></reason></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name:    "reason nested in reason: inner opener is text, first close wins",
			elem:    `<zing job="classify" outcome="bug"><reason>a <reason> b</reason></reason></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>a &lt;reason> b</reason></reason></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name: "mismatched closer leaves the field unclosed",
			elem: `<zing job="classify" outcome="bug"><reason>it crashes</zing>`,
			root: classifyRoot,
			ok:   false,
		},
		{
			name:    "build report with two placeholders",
			elem:    `<zing job="build" outcome="ok"><report><nil> on <branch></report></zing>`,
			root:    buildRoot,
			want:    `<zing job="build" outcome="ok"><report>&lt;nil> on &lt;branch></report></zing>`,
			escaped: 2,
			ok:      true,
		},
		{
			name:    "mixed problem escapes nil, keeps its loop child",
			elem:    `<zing job="planning" outcome="ready"><plan><overview><problem>p is <nil> <loop cmd="c">l</loop></problem></overview></plan></zing>`,
			root:    readyRoot,
			want:    `<zing job="planning" outcome="ready"><plan><overview><problem>p is &lt;nil> <loop cmd="c">l</loop></problem></overview></plan></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name:    "unknown element under the root stays markup",
			elem:    `<zing job="classify" outcome="bug"><reason>x</reason><extra>t</extra></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>x</reason><extra>t</extra></zing>`,
			escaped: 0,
			ok:      true,
		},
		{
			name:    "trailing text after the root closes is cut",
			elem:    `<zing job="classify" outcome="bug"><reason>a < b</reason></zing>TRAILING`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>a &lt; b</reason></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name: "cut off right after a bare less-than",
			elem: `<zing job="classify" outcome="bug"><reason>a <`,
			root: classifyRoot,
			ok:   false,
		},
		{
			name:    "bare less-than inside a quoted attribute value",
			elem:    `<zing job="review" outcome="ok"><finding lens="fidelity" severity="minor" location="x<-y"><text>t</text><fix>f</fix></finding></zing>`,
			root:    reviewRoot,
			want:    `<zing job="review" outcome="ok"><finding lens="fidelity" severity="minor" location="x&lt;-y"><text>t</text><fix>f</fix></finding></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name:    "self-closing child before the escaped field",
			elem:    `<zing job="build" outcome="ok"><claims/><report>x is <nil></report></zing>`,
			root:    buildRoot,
			want:    `<zing job="build" outcome="ok"><claims/><report>x is &lt;nil></report></zing>`,
			escaped: 1,
			ok:      true,
		},
		{
			name:    "closing tag with whitespace before the >",
			elem:    `<zing job="classify" outcome="bug"><reason>a < b</reason ></zing>`,
			root:    classifyRoot,
			want:    `<zing job="classify" outcome="bug"><reason>a &lt; b</reason ></zing>`,
			escaped: 1,
			ok:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, escaped, ok := repairBareLessThan([]byte(tc.elem), tc.root)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (bytes = %q)", ok, tc.ok, got)
			}
			if !tc.ok {
				if got != nil {
					t.Errorf("bytes = %q, want nil alongside ok = false", got)
				}
				if escaped != 0 {
					t.Errorf("escaped = %d, want 0 alongside ok = false", escaped)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("repaired = %q, want %q", got, tc.want)
			}
			if escaped != tc.escaped {
				t.Errorf("escaped = %d, want %d", escaped, tc.escaped)
			}
		})
	}
}
