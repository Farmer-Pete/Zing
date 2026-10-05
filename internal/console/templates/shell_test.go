package templates

import (
	"strings"
	"testing"
)

// TestMainScrollsWideContentInsteadOfThePage proves bug fix 17: the owner
// reported a horizontal scrollbar under the whole thread whenever it held a
// wide plan table or a long code line. #main itself clips horizontally
// (overflow-x: hidden), and each table and pre instead gets its own
// scrollable box (display: block plus overflow-x: auto, since neither
// shrinks to its container on its own), so wide content scrolls inside
// itself rather than widening the page.
func TestMainScrollsWideContentInsteadOfThePage(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := Shell(emptyBodyHTML, emptyBodyHTML, emptyBodyHTML, emptyBodyHTML).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Shell.Render: %v", err)
	}
	got := sb.String()
	for _, want := range []string{
		"#main { padding: 1.5rem; overflow-y: auto; overflow-x: hidden; }",
		"#main table { display: block; max-width: 100%; overflow-x: auto; }",
		"#main pre { max-width: 100%; overflow-x: auto; }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered shell missing the wide-content scroll fix %q", want)
		}
	}
}

// TestShellRendersSandboxRunDialog proves the sandbox-run result dialog
// (split from #73) renders after .layout's closing tag, so it sits outside
// #main and every other region /stream re-renders.
func TestShellRendersSandboxRunDialog(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := Shell(emptyBodyHTML, emptyBodyHTML, emptyBodyHTML, emptyBodyHTML).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Shell.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, `<dialog id="sandbox-run-result"`) {
		t.Errorf("rendered shell missing the sandbox-run-result dialog; got:\n%s", got)
	}
	layoutClose := strings.Index(got, "</div>")
	dialogIdx := strings.Index(got, `<dialog id="sandbox-run-result"`)
	if layoutClose < 0 || dialogIdx < 0 || dialogIdx < layoutClose {
		t.Errorf("rendered shell's sandbox-run-result dialog is not after .layout's close; got:\n%s", got)
	}
}

// TestAlertStripHasBoundedHeight proves the alert strip bug fix: #alerts
// sits above .layout in normal flow and holds up to alertsLimit (20)
// lines, so an unbounded .alert-lines pushed the whole page down with
// every new warning. The rule now caps its height and scrolls instead,
// and an empty strip still takes no space.
func TestAlertStripHasBoundedHeight(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := Shell(emptyBodyHTML, emptyBodyHTML, emptyBodyHTML, emptyBodyHTML).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Shell.Render: %v", err)
	}
	got := sb.String()
	start := strings.Index(got, ".alert-lines {")
	if start < 0 {
		t.Fatalf("rendered shell has no .alert-lines rule")
	}
	end := strings.Index(got[start:], "}")
	if end < 0 {
		t.Fatalf("rendered shell's .alert-lines rule is unterminated")
	}
	rule := got[start : start+end]
	for _, want := range []string{"max-height: 5.5rem;", "overflow-y: auto;"} {
		if !strings.Contains(rule, want) {
			t.Errorf("rendered shell's .alert-lines rule missing %q", want)
		}
	}
	if !strings.Contains(got, "#alerts:empty { display: none; }") {
		t.Errorf("rendered shell missing the empty-strip rule")
	}
}

// TestColumnsScrollOnTheirOwn proves the sidebar-stays-visible fix: the page
// itself must never scroll vertically, so #nav, #main and #rail's existing
// overflow-y: auto rules can take effect on a fixed-height track instead of
// a track that grows to fit a long thread.
func TestColumnsScrollOnTheirOwn(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := Shell(emptyBodyHTML, emptyBodyHTML, emptyBodyHTML, emptyBodyHTML).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Shell.Render: %v", err)
	}
	got := sb.String()

	bodyStart := strings.Index(got, "body {")
	if bodyStart < 0 {
		t.Fatalf("rendered shell has no body rule")
	}
	bodyEnd := strings.Index(got[bodyStart:], "}")
	if bodyEnd < 0 {
		t.Fatalf("rendered shell's body rule is unterminated")
	}
	bodyRule := got[bodyStart : bodyStart+bodyEnd]
	for _, want := range []string{"height: 100vh;", "display: flex;", "flex-direction: column;"} {
		if !strings.Contains(bodyRule, want) {
			t.Errorf("rendered shell's body rule missing %q, got %q", want, bodyRule)
		}
	}

	layoutStart := strings.Index(got, ".layout {")
	if layoutStart < 0 {
		t.Fatalf("rendered shell has no .layout rule")
	}
	layoutEnd := strings.Index(got[layoutStart:], "}")
	if layoutEnd < 0 {
		t.Fatalf("rendered shell's .layout rule is unterminated")
	}
	layoutRule := got[layoutStart : layoutStart+layoutEnd]
	for _, want := range []string{"flex: 1;", "min-height: 0;", "grid-template-rows: minmax(0, 1fr);"} {
		if !strings.Contains(layoutRule, want) {
			t.Errorf("rendered shell's .layout rule missing %q, got %q", want, layoutRule)
		}
	}
	if strings.Contains(layoutRule, "min-height: 100vh") {
		t.Errorf("rendered shell's .layout rule still has min-height: 100vh")
	}

	if !strings.Contains(got, "#alerts { flex: none; }") {
		t.Errorf("rendered shell missing #alerts { flex: none; }")
	}

	for _, sel := range []string{"#nav {", "#rail {"} {
		start := strings.Index(got, sel)
		if start < 0 {
			t.Fatalf("rendered shell has no %s rule", sel)
		}
		end := strings.Index(got[start:], "}")
		if end < 0 {
			t.Fatalf("rendered shell's %s rule is unterminated", sel)
		}
		rule := got[start : start+end]
		if !strings.Contains(rule, "overflow-y: auto;") {
			t.Errorf("rendered shell's %s rule missing %q, got %q", sel, "overflow-y: auto;", rule)
		}
	}
}
