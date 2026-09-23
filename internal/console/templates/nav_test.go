package templates

import "testing"

// TestZingNavExpr_KnownViewsUnchanged pins zingNavExpr's exact output for
// every view name a real caller passes today, so hardening it against an
// out-of-set view (below) cannot silently change what the current literal
// callers render.
func TestZingNavExpr_KnownViewsUnchanged(t *testing.T) {
	tests := []struct {
		view          string
		open, project int64
		want          string
	}{
		{"inbox", 0, 0, `document.getElementById('stream-ctl').dispatchEvent(new CustomEvent('zing-nav',{detail:{view:'inbox',open:0,project:0}}))`},
		{"recent", 0, 0, `document.getElementById('stream-ctl').dispatchEvent(new CustomEvent('zing-nav',{detail:{view:'recent',open:0,project:0}}))`},
		{"feed", 0, 0, `document.getElementById('stream-ctl').dispatchEvent(new CustomEvent('zing-nav',{detail:{view:'feed',open:0,project:0}}))`},
		{"project", 0, 42, `document.getElementById('stream-ctl').dispatchEvent(new CustomEvent('zing-nav',{detail:{view:'project',open:0,project:42}}))`},
		{"thread", 7, 0, `document.getElementById('stream-ctl').dispatchEvent(new CustomEvent('zing-nav',{detail:{view:'thread',open:7,project:0}}))`},
	}
	for _, tt := range tests {
		t.Run(tt.view, func(t *testing.T) {
			if got := zingNavExpr(tt.view, tt.open, tt.project); got != tt.want {
				t.Errorf("zingNavExpr(%q, %d, %d) = %q, want %q", tt.view, tt.open, tt.project, got, tt.want)
			}
		})
	}
}

// TestZingNavExpr_RejectsOutOfSetView proves a view name outside
// validNavViews never reaches the single-quoted JS string zingNavExpr
// builds: a future non-literal caller (a view threaded through from
// somewhere other than this file's own hardcoded call sites) cannot break
// out of the JS string literal or inject script, since the whole
// expression comes back empty instead.
func TestZingNavExpr_RejectsOutOfSetView(t *testing.T) {
	tests := []string{
		"",
		"unknown",
		`'});alert(document.cookie);({a:'`,
		"thread'", // a single added quote, the minimal JS-string-breakout attempt
	}
	for _, view := range tests {
		t.Run(view, func(t *testing.T) {
			if got := zingNavExpr(view, 1, 2); got != "" {
				t.Errorf("zingNavExpr(%q, 1, 2) = %q, want \"\" for an out-of-set view", view, got)
			}
		})
	}
}
