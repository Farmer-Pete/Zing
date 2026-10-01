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
