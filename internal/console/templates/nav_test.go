package templates

import (
	"strings"
	"testing"

	"zing/internal/store"
)

// renderNav renders Nav(nil, threads, "", openTicketID) to a string,
// failing the test on a render error.
func renderNav(t *testing.T, threads []NavThread, openTicketID int64) string {
	t.Helper()
	var sb strings.Builder
	if err := Nav(nil, threads, "", openTicketID).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Nav.Render: %v", err)
	}
	return sb.String()
}

// TestNavThreadSelected proves the bug fix for the Threads sidebar losing
// its selected highlight once the thread view patches in: a server-rendered
// "selected" class on the open ticket's row survives every #nav patch (it
// is part of the HTML #nav renders, not a client-only focus ring that a
// morph can drop), so the open thread's row stays marked regardless of
// which SSE frame last touched #nav.
func TestNavThreadSelected(t *testing.T) {
	t.Parallel()

	threads := []NavThread{
		{Ticket: store.Ticket{ID: 5, Title: "Add a hello endpoint"}},
		{Ticket: store.Ticket{ID: 9, Title: "Fix the flaky test"}},
	}

	t.Run("the open ticket's row carries the selected class", func(t *testing.T) {
		t.Parallel()
		got := renderNav(t, threads, 5)
		if !strings.Contains(got, `class="nav-link nav-thread selected"`) {
			t.Errorf("rendered nav missing the selected row; got:\n%s", got)
		}
	})

	t.Run("every other row stays unselected", func(t *testing.T) {
		t.Parallel()
		got := renderNav(t, threads, 5)
		if !strings.Contains(got, `class="nav-link nav-thread"`) {
			t.Errorf("rendered nav missing the unselected row; got:\n%s", got)
		}
	})

	t.Run("no open ticket selects nothing", func(t *testing.T) {
		t.Parallel()
		got := renderNav(t, threads, 0)
		if strings.Contains(got, "selected") {
			t.Errorf("rendered nav selected a row with no ticket open; got:\n%s", got)
		}
	})
}

// TestZingNavExpr_KnownViewsUnchanged pins zingNavExpr's exact output for
// every view name a real caller passes today, so hardening it against an
// out-of-set view (below) cannot silently change what the current literal
// callers render.
func TestZingNavExpr_KnownViewsUnchanged(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	tests := []string{
		"",
		"unknown",
		`'});alert(document.cookie);({a:'`,
		"thread'", // a single added quote, the minimal JS-string-breakout attempt
	}
	for _, view := range tests {
		t.Run(view, func(t *testing.T) {
			t.Parallel()
			if got := zingNavExpr(view, 1, 2); got != "" {
				t.Errorf("zingNavExpr(%q, 1, 2) = %q, want \"\" for an out-of-set view", view, got)
			}
		})
	}
}
