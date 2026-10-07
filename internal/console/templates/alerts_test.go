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
	if err := Alerts(lines, "abc123def456", StopBanner{}).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Alerts.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, `<ul class="alert-lines" tabindex="0">`) {
		t.Errorf("rendered alert strip's ul.alert-lines missing tabindex=\"0\": %s", got)
	}
}

// TestAlertsCarriesBuildVersion proves #alerts renders the server's asset
// version as data-build, so console.js can compare it with the page's own
// data-build on every patched frame (#59).
func TestAlertsCarriesBuildVersion(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := Alerts(nil, "abc123def456", StopBanner{}).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Alerts.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, `data-build="abc123def456"`) {
		t.Errorf("rendered #alerts missing data-build=\"abc123def456\": %s", got)
	}

	open := strings.Index(got, `<div id="alerts"`)
	if open == -1 {
		t.Fatalf("rendered output missing <div id=\"alerts\" open tag: %s", got)
	}
	tagEnd := strings.Index(got[open:], ">")
	if tagEnd == -1 {
		t.Fatalf("rendered #alerts open tag never closes: %s", got)
	}
	bodyStart := open + tagEnd + 1
	bodyEnd := strings.LastIndex(got, "</div>")
	if bodyEnd == -1 || bodyEnd < bodyStart {
		t.Fatalf("rendered output missing closing </div> after #alerts open tag: %s", got)
	}
	body := got[bodyStart:bodyEnd]
	if strings.TrimSpace(body) != "" {
		t.Errorf("rendered #alerts body with nil lines not empty: %q", body)
	}
}

// TestAlertsRendersStopBanner proves #alerts renders the dispatcher-stopped
// banner (ticket #89) when stop.Show is true, with the Resume button
// disabled and the busy sentence shown exactly when ResumeDisabled is true,
// and renders no banner at all for a zero StopBanner.
func TestAlertsRendersStopBanner(t *testing.T) {
	t.Parallel()

	shown := StopBanner{
		Show:     true,
		Kind:     "fail-closed",
		Headline: "Dispatching stopped after fail-closed on ticket 42.",
		Cause:    "the lease was lost",
		Time:     "14:03:09",
		Note:     "Ticket 42 runs again once its claim expires.",
	}

	var sb strings.Builder
	if err := Alerts(nil, "abc123def456", shown).Render(t.Context(), &sb); err != nil {
		t.Fatalf("Alerts.Render: %v", err)
	}
	got := sb.String()
	for _, want := range []string{
		`class="dispatch-banner"`,
		`data-kind="fail-closed"`,
		"Dispatching stopped after fail-closed on ticket 42.",
		"the lease was lost",
		"14:03:09",
		"Ticket 42 runs again once its claim expires.",
		`class="dispatch-resume"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered #alerts missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "disabled") {
		t.Errorf("rendered #alerts carries disabled with ResumeDisabled false: %s", got)
	}

	var sbBusy strings.Builder
	busy := shown
	busy.ResumeDisabled = true
	busy.Busy = "1 runs are still finishing; resume once they are done"
	if err := Alerts(nil, "abc123def456", busy).Render(t.Context(), &sbBusy); err != nil {
		t.Fatalf("Alerts.Render: %v", err)
	}
	gotBusy := sbBusy.String()
	if !strings.Contains(gotBusy, "disabled") {
		t.Errorf("rendered #alerts missing disabled with ResumeDisabled true: %s", gotBusy)
	}
	if !strings.Contains(gotBusy, "1 runs are still finishing; resume once they are done") {
		t.Errorf("rendered #alerts missing busy text: %s", gotBusy)
	}

	var sbNone strings.Builder
	if err := Alerts(nil, "abc123def456", StopBanner{}).Render(t.Context(), &sbNone); err != nil {
		t.Fatalf("Alerts.Render: %v", err)
	}
	if strings.Contains(sbNone.String(), "dispatch-banner") {
		t.Errorf("rendered #alerts with a zero StopBanner carries a banner: %s", sbNone.String())
	}
}
