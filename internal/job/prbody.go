// prbody.go renders the pull request PUBLISH opens (design section 8.2
// step 3, 8.10): prBody is a pure function of the ticket id, its stored
// plan, the judge's selected final verdicts over the sealed scenario
// cohort, every build report, and every file event. It does no I/O and
// reads no clock; the caller has already picked the verdict set to render
// (finalVerdicts, 8.2 step 2) before calling it.
package job

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// Title's own numbers (design 8.10): the title plus its " (#REF)" or
// " (REF)" suffix never exceeds prTitleRuneLimit runes; past that, the
// title is cut on a word boundary and gets "...". prEvidenceRuneCap caps a
// performance scenario's evidence line.
const (
	prTitleRuneLimit  = 256
	prEvidenceRuneCap = 300
)

// prBody renders the six-section PullRequest body (design 8.10): Title
// from the ticket's own title and ref; What from the plan's objective and
// goals; WorkingDemo from its demo; Scenarios from the cohort and its
// selected verdicts, never the scenarios' own given/when/then;
// DeclaredFiles from the plan's files plus every accepted extra;
// ChestertonsFence from the plan's deletions plus every landed build
// report's fences, deduplicated; PlanLink naming t.ID. The body itself
// renders through orchestrator.PullRequest.Body (Package 5).
func prBody(t store.Ticket, plan response.Plan, verdicts []response.VerdictArtifact, scenarios []response.Scenario, reports []store.BuildReportRow, events []store.FileEventRow) orchestrator.PullRequest {
	return orchestrator.PullRequest{
		Title:            prTitle(t.Title, t.TrackerRef),
		What:             prWhat(plan.Overview.Objective, plan.Overview.Goals),
		WorkingDemo:      prWorkingDemo(plan.Design.Demo),
		Scenarios:        prScenarios(scenarios, verdicts),
		DeclaredFiles:    prDeclaredFiles(plan, events),
		ChestertonsFence: prChestertonsFence(plan, reports),
		PlanLink:         fmt.Sprintf("The plan is ticket %d in the Zing console.", t.ID),
	}
}

// prTitle collapses title's whitespace and appends a suffix naming ref:
// " (#REF)" when ref is all ASCII digits, " (REF)" for any other
// non-empty ref, and no suffix for an empty ref. When the collapsed title
// plus suffix fits within prTitleRuneLimit runes, that is the result.
// Otherwise the title is cut at the last space in its first
// prTitleRuneLimit-len(suffix)-3 runes (a hard cut there if none), then
// "..." and the suffix are appended.
func prTitle(title, ref string) string {
	suffix := prTitleSuffix(ref)
	collapsed := collapseWhitespace(title)
	runes := []rune(collapsed)
	suffixRunes := len([]rune(suffix))
	budget := prTitleRuneLimit - suffixRunes
	if len(runes) <= budget {
		return collapsed + suffix
	}

	window := runes[:budget-3]
	cut := budget - 3
	for i, w := range slices.Backward(window) {
		if w == ' ' {
			cut = i
			break
		}
	}
	return string(runes[:cut]) + "..." + suffix
}

// prTitleSuffix is prTitle's own suffix rule: " (#REF)" for a ref that is
// all ASCII digits, " (REF)" for any other non-empty ref, "" for an empty
// ref.
func prTitleSuffix(ref string) string {
	switch {
	case ref == "":
		return ""
	case isAllASCIIDigits(ref):
		return " (#" + ref + ")"
	default:
		return " (" + ref + ")"
	}
}

// isAllASCIIDigits reports whether s is non-empty and every rune is an
// ASCII digit.
func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// prWhat is the objective, a blank line, then one "- <goal>" line per goal.
func prWhat(objective string, goals []string) string {
	lines := make([]string, len(goals))
	for i, g := range goals {
		lines[i] = "- " + g
	}
	return objective + "\n\n" + strings.Join(lines, "\n")
}

// prWorkingDemo is the demo command as an inline code span, a blank line,
// then the demo text.
func prWorkingDemo(demo response.Demo) string {
	return "`" + demo.Cmd + "`\n\n" + demo.Text
}

// prScenarios renders the cohort's summary line, its result table in
// cohort order, and, when any performance scenario is in the cohort, a
// "Performance evidence:" list naming each performance verdict's evidence
// (first line only, cut to prEvidenceRuneCap runes). It never reads a
// scenario's Given, When, or Then: only ID, Kind, and the matching
// verdict's Result and Evidence.
func prScenarios(scenarios []response.Scenario, verdicts []response.VerdictArtifact) string {
	byScenario := make(map[string]response.VerdictArtifact, len(verdicts))
	for _, v := range verdicts {
		byScenario[v.Scenario] = v
	}

	rows := make([][]string, 0, len(scenarios))
	var sha string
	var passed, failed int
	var evidence []string
	for _, sc := range scenarios {
		v := byScenario[sc.ID]
		if sha == "" {
			sha = v.SHA
		}

		result := "fail"
		if v.Result == response.ResultPass {
			result = "pass"
			passed++
		} else {
			failed++
		}
		rows = append(rows, []string{sc.ID, string(sc.Kind), result})

		if sc.Kind == response.ScenarioKindPerformance {
			line := firstLine(v.Evidence)
			evidence = append(evidence, "- "+sc.ID+": "+cutRunes(line, prEvidenceRuneCap))
		}
	}

	sha7 := sha
	if len(sha7) > 7 {
		sha7 = sha7[:7]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d scenarios, judged on %s: %d passed, %d failed.", len(scenarios), sha7, passed, failed)
	b.WriteString("\n\n")
	b.WriteString(markdownTable([]string{"Scenario", "Kind", "Result"}, rows))
	if len(evidence) > 0 {
		b.WriteString("\n\nPerformance evidence:\n")
		b.WriteString(strings.Join(evidence, "\n"))
	}
	return b.String()
}

// prDeclaredFiles is the plan's own files, then every accepted extra (the
// newest FileEvents row per path whose decision is accept), each with the
// builder's reason.
func prDeclaredFiles(plan response.Plan, events []store.FileEventRow) string {
	files := response.Files(plan)
	extras := acceptedFileRows(events)

	rows := make([][]string, 0, len(files)+len(extras))
	for _, f := range files {
		rows = append(rows, []string{f.Path, string(f.Action), f.Reason})
	}
	for _, row := range extras {
		rows = append(rows, []string{row.File.Path, string(row.File.Action), row.File.Reason})
	}
	return markdownTable([]string{"Path", "Action", "Reason"}, rows)
}

// acceptedFileRows returns, sorted by path, every FileEvents row whose
// newest decision is accept: the extras prDeclaredFiles lists after the
// plan's own files.
func acceptedFileRows(events []store.FileEventRow) []store.FileEventRow {
	byPath := newestFileEventPerPath(events)
	out := make([]store.FileEventRow, 0, len(byPath))
	for _, row := range byPath {
		if row.File.Decision != nil && *row.File.Decision == response.PerimeterAccept {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File.Path < out[j].File.Path })
	return out
}

// prChestertonsFence is the plan's own deletions, then every landed build
// report's fences in report order, deduplicated by (path, symbol) with the
// plan's own row, or the earliest report's, winning a repeat.
func prChestertonsFence(plan response.Plan, reports []store.BuildReportRow) string {
	type fenceKey struct{ path, symbol string }
	seen := make(map[fenceKey]bool)
	var fences []response.Fence
	add := func(f response.Fence) {
		key := fenceKey{f.Path, f.Symbol}
		if seen[key] {
			return
		}
		seen[key] = true
		fences = append(fences, f)
	}

	for _, f := range response.Fences(plan) {
		add(f)
	}
	for i := range reports {
		r := &reports[i]
		if r.Report.CommitSHA == nil {
			continue
		}
		for _, f := range r.Report.Fences {
			add(f)
		}
	}

	rows := make([][]string, len(fences))
	for i, f := range fences {
		rows[i] = []string{f.Path, f.Symbol, f.ExistedBecause}
	}
	return markdownTable([]string{"Path", "Symbol", "Existed because"}, rows)
}

// firstLine returns s up to its first newline, or all of s when it has
// none.
func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

// markdownTable renders a pipe table: headers, a "---" separator row, then
// rows in order, every cell passed through escapeCell (design 8.10: "|"
// escaped, whitespace runs collapsed to one space). An empty rows renders
// the header and separator alone.
func markdownTable(headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| ")
	b.WriteString(strings.Join(headers, " | "))
	b.WriteString(" |\n|")
	for range headers {
		b.WriteString(" --- |")
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = escapeCell(c)
		}
		b.WriteString("\n| ")
		b.WriteString(strings.Join(cells, " | "))
		b.WriteString(" |")
	}
	return b.String()
}

// escapeCell collapses a table cell's whitespace runs to one space, then
// escapes "|" as "\|" (design 8.10).
func escapeCell(s string) string {
	return strings.ReplaceAll(collapseWhitespace(s), "|", `\|`)
}
