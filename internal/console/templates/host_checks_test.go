package templates

import (
	"strings"
	"testing"
)

// TestGateHostChecksSection proves scenariosSection renders the "Runs on
// your machine at judging" list inline (design change to scenariosSection,
// Task 5): one row per host scenario, in the slice's own order, with the id
// and the check verbatim and HTML-escaped in a pre/code block, and a
// behavior-kind row's check left out of that section.
func TestGateHostChecksSection(t *testing.T) {
	t.Parallel()
	rows := []ScenarioRow{
		{ID: "s1", Kind: "host", Given: "g1", When: "w1", Then: "t1", Check: `cd sub && go test ./internal/sandbox/...`},
		{ID: "s2", Kind: "behavior", Given: "g2", When: "w2", Then: "t2", Check: "go test ./behaviorpkg"},
		{ID: "s3", Kind: "host", Given: "g3", When: "w3", Then: "t3", Check: "go test ./hostpkg"},
	}

	var sb strings.Builder
	if err := scenariosSection(rows).Render(t.Context(), &sb); err != nil {
		t.Fatalf("scenariosSection.Render: %v", err)
	}
	got := sb.String()

	if !strings.Contains(got, "Runs on your machine at judging") {
		t.Fatalf("want the host-checks heading; got:\n%s", got)
	}
	if !strings.Contains(got, `<td class="host-check-id">s1</td>`) {
		t.Errorf(`want a host-check-id cell for s1; got:\n%s`, got)
	}
	const wantCode = `<pre><code class="host-check">cd sub &amp;&amp; go test ./internal/sandbox/...</code></pre>`
	if !strings.Contains(got, wantCode) {
		t.Errorf("want the escaped check command %q; got:\n%s", wantCode, got)
	}

	start := strings.Index(got, "Runs on your machine at judging")
	end := strings.Index(got[start:], "</section>")
	if start < 0 || end < 0 {
		t.Fatalf("want a host-checks section; got:\n%s", got)
	}
	section := got[start : start+end]
	if strings.Contains(section, "go test ./behaviorpkg") {
		t.Errorf("want the behavior scenario's check left out of the host-checks section; got:\n%s", section)
	}

	s1 := strings.Index(got, "s1")
	s3 := strings.Index(got, "s3")
	if s1 < 0 || s3 < 0 || s1 > s3 {
		t.Errorf("want s1 before s3 in sealed order; got:\n%s", got)
	}
}

// TestGateHostChecksSection_AbsentWithoutHost proves scenariosSection
// renders no host-checks section, and still renders the scenarios table,
// when the cohort has no host scenario.
func TestGateHostChecksSection_AbsentWithoutHost(t *testing.T) {
	t.Parallel()
	rows := []ScenarioRow{
		{ID: "s1", Kind: "behavior", Given: "g1", When: "w1", Then: "t1", Check: "go test ./behaviorpkg"},
		{ID: "s2", Kind: "negative", Given: "g2", When: "w2", Then: "t2", Check: "go test ./negativepkg"},
	}

	var sb strings.Builder
	if err := scenariosSection(rows).Render(t.Context(), &sb); err != nil {
		t.Fatalf("scenariosSection.Render: %v", err)
	}
	got := sb.String()

	if strings.Contains(got, "Runs on your machine at judging") {
		t.Errorf("want no host-checks heading; got:\n%s", got)
	}
	if strings.Contains(got, "host-check") {
		t.Errorf("want no host-check class; got:\n%s", got)
	}
	if !strings.Contains(got, `<table class="scenarios">`) {
		t.Errorf("want the scenarios table to still render; got:\n%s", got)
	}
}
