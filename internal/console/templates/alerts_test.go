package templates

import (
	"strings"
	"testing"
)

// TestAlertLinesScrollRegionIsFocusable proves an accessibility fix: once
// .alert-lines gained max-height and overflow-y: auto (bug fix "the log
// banner never pushes the page down"), its only content (li.alert-line
// rows) had no focusable descendant, so a keyboard-only user had no way to
// focus the scroll region and reach rows past the visible cap. tabindex="0"
// on the ul itself makes it a stop in the tab order, so arrow and Page Down
// keys scroll it once focused.
func TestAlertLinesScrollRegionIsFocusable(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	lines := []AlertLine{{Time: "12:00:00", Level: "warn", Message: "disk space low"}}
	if err := Alerts(lines).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Alerts.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, `<ul class="alert-lines" tabindex="0">`) {
		t.Errorf("rendered alert strip's ul.alert-lines missing tabindex=\"0\": %s", got)
	}
}
