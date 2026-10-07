package templates

import (
	"strings"
	"testing"

	"zing/internal/store"
)

// testStatePlanning is TestNavThreadBadgeByKind's own "planning" ticket
// state literal, named once so its three subtests' fixtures don't repeat
// the bare string (goconst).
const testStatePlanning = "planning"

// renderNav renders Nav(nil, threads, "", "", openTicketID) to a string,
// failing the test on a render error.
func renderNav(t *testing.T, threads []NavThread, openTicketID int64) string {
	t.Helper()
	return renderNavWithHold(t, threads, "", openTicketID)
}

// renderNavWithHold renders Nav(nil, threads, "", claudeHold, openTicketID)
// to a string, failing the test on a render error.
func renderNavWithHold(t *testing.T, threads []NavThread, claudeHold string, openTicketID int64) string {
	t.Helper()
	var sb strings.Builder
	if err := Nav(nil, threads, "", claudeHold, openTicketID).Render(t.Context(), &sb); err != nil {
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
		{Ticket: store.Ticket{ID: 5, Title: testHelloTicketTitle}},
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

// TestNavThreadBadgeByKind proves threadLink's three-way badge: blocking
// renders the waiting badge, unread-but-not-blocking renders the unread
// badge, and a quiet row renders its state pill instead (design section
// 6.8, #106 bug 4).
func TestNavThreadBadgeByKind(t *testing.T) {
	t.Parallel()

	t.Run("blocking renders the blocking badge", func(t *testing.T) {
		t.Parallel()
		th := NavThread{
			Ticket:    store.Ticket{ID: 1, State: testStatePlanning},
			Blocking:  true,
			Unread:    true,
			WaitingOn: "questions",
		}
		got := renderNav(t, []NavThread{th}, 0)
		if !strings.Contains(got, "badge-blocking") {
			t.Errorf("rendered nav missing badge-blocking for a blocking thread; got:\n%s", got)
		}
		if strings.Contains(got, "badge-unread") {
			t.Errorf("rendered nav shows badge-unread for a blocking thread; got:\n%s", got)
		}
		if strings.Contains(got, `<span class="pill">planning</span>`) {
			t.Errorf("rendered nav shows the state pill for a blocking thread; got:\n%s", got)
		}
	})

	t.Run("unread, non-blocking renders the unread badge", func(t *testing.T) {
		t.Parallel()
		th := NavThread{
			Ticket: store.Ticket{ID: 2, State: testStatePlanning},
			Unread: true,
		}
		got := renderNav(t, []NavThread{th}, 0)
		if !strings.Contains(got, "badge-unread") {
			t.Errorf("rendered nav missing badge-unread for an unread thread; got:\n%s", got)
		}
		if strings.Contains(got, `<span class="pill">planning</span>`) {
			t.Errorf("rendered nav shows the state pill for an unread thread; got:\n%s", got)
		}
	})

	t.Run("quiet renders the state pill, no badge", func(t *testing.T) {
		t.Parallel()
		th := NavThread{
			Ticket: store.Ticket{ID: 3, State: testStatePlanning},
		}
		got := renderNav(t, []NavThread{th}, 0)
		if !strings.Contains(got, `<span class="pill">planning</span>`) {
			t.Errorf("rendered nav missing the state pill for a quiet thread; got:\n%s", got)
		}
		if strings.Contains(got, "badge-blocking") || strings.Contains(got, "badge-unread") {
			t.Errorf("rendered nav shows a badge for a quiet thread; got:\n%s", got)
		}
	})
}

// TestNav_HasSettingsLink proves the Settings nav link (#81, owner
// decision Q1) renders after Feed, with a click expression that carries
// view settings.
func TestNav_HasSettingsLink(t *testing.T) {
	t.Parallel()

	got := renderNav(t, nil, 0)
	feedIdx := strings.Index(got, `>Feed<`)
	settingsIdx := strings.Index(got, `>Settings<`)
	if feedIdx == -1 || settingsIdx == -1 {
		t.Fatalf("rendered nav missing Feed or Settings link; got:\n%s", got)
	}
	if settingsIdx < feedIdx {
		t.Errorf("Settings link rendered before Feed; got:\n%s", got)
	}
	want := `data-on:click__prevent="document.getElementById(&#39;stream-ctl&#39;).dispatchEvent(new CustomEvent(&#39;zing-nav&#39;,{detail:{view:&#39;settings&#39;,open:0,project:0}}))"`
	if !strings.Contains(got, want) {
		t.Errorf("rendered nav missing the Settings click expression %q; got:\n%s", want, got)
	}
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

// TestNav_ParkedBadgeAndClaudeHold proves #45's two sidebar markers: a
// non-blocking parked thread's own badge, and the nav-wide claude-hold
// status line, each rendered only while there is something to show.
func TestNav_ParkedBadgeAndClaudeHold(t *testing.T) {
	t.Parallel()

	t.Run("a non-blocking parked thread shows the parked badge, and claudeHold shows the status line", func(t *testing.T) {
		t.Parallel()
		th := NavThread{
			Ticket:      store.Ticket{ID: 1, State: testStatePlanning},
			ParkedUntil: "12:20pm",
		}
		got := renderNavWithHold(t, []NavThread{th}, "12:20pm", 0)
		if !strings.Contains(got, `<span class="badge badge-parked">parked until 12:20pm</span>`) {
			t.Errorf("rendered nav missing the parked badge; got:\n%s", got)
		}
		if !strings.Contains(got, "claude: capped until 12:20pm") {
			t.Errorf("rendered nav missing the claude-hold status line; got:\n%s", got)
		}
	})

	t.Run("empty ParkedUntil and claudeHold render neither", func(t *testing.T) {
		t.Parallel()
		th := NavThread{Ticket: store.Ticket{ID: 2, State: testStatePlanning}}
		got := renderNavWithHold(t, []NavThread{th}, "", 0)
		if strings.Contains(got, "badge-parked") {
			t.Errorf("rendered nav shows a parked badge with ParkedUntil empty; got:\n%s", got)
		}
		if strings.Contains(got, "claude: capped until") {
			t.Errorf("rendered nav shows the claude-hold status line with claudeHold empty; got:\n%s", got)
		}
	})

	t.Run("a blocking thread with ParkedUntil set shows the blocking badge, not the parked badge", func(t *testing.T) {
		t.Parallel()
		th := NavThread{
			Ticket:      store.Ticket{ID: 3, State: testStatePlanning},
			Blocking:    true,
			WaitingOn:   "questions",
			ParkedUntil: "12:20pm",
		}
		got := renderNav(t, []NavThread{th}, 0)
		if !strings.Contains(got, "badge-blocking") {
			t.Errorf("rendered nav missing badge-blocking for a blocking, parked thread; got:\n%s", got)
		}
		if strings.Contains(got, "badge-parked") {
			t.Errorf("rendered nav shows badge-parked for a blocking thread; got:\n%s", got)
		}
	})
}
