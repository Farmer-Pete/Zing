package job

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"zing/internal/response"
)

// CheckCoverage is 7.4's coverage check, applied to one judge run's raw
// verdicts against the sealed cohort. It walks verdicts in document order,
// each one checked in this precedence: an id outside the cohort gives
// "verdict for unknown scenario <id>"; an id already seen in this walk
// gives "duplicate verdict for scenario <id>"; evidence empty after
// trimming gives "empty evidence for scenario <id>". It then walks
// scenarios in cohort order and gives "missing verdict for scenario <id>"
// for every one that never appeared in verdicts. The errors are returned
// in that order: document-order checks first, then the missing checks.
func CheckCoverage(scenarios []response.Scenario, verdicts []response.Verdict) []string {
	cohort := make(map[string]struct{}, len(scenarios))
	for _, sc := range scenarios {
		cohort[sc.ID] = struct{}{}
	}

	var errs []string
	seen := make(map[string]struct{}, len(verdicts))
	for _, v := range verdicts {
		_, known := cohort[v.Scenario]
		_, duplicate := seen[v.Scenario]
		switch {
		case !known:
			errs = append(errs, "verdict for unknown scenario "+v.Scenario)
		case duplicate:
			errs = append(errs, "duplicate verdict for scenario "+v.Scenario)
		case strings.TrimSpace(v.Evidence) == "":
			errs = append(errs, "empty evidence for scenario "+v.Scenario)
		}
		seen[v.Scenario] = struct{}{}
	}

	for _, sc := range scenarios {
		if _, ok := seen[sc.ID]; !ok {
			errs = append(errs, "missing verdict for scenario "+sc.ID)
		}
	}
	return errs
}

// applyCheckExit is 7.4's check override: judged is the judge's own stored
// verdict row for one scenario, and exit is CHECK's re-run exit code (-1
// for a command-runner timeout). The returned row keeps judged's scenario,
// kind, round, and sha; Result is pass only when exit is 0; Evidence is
// judged's own evidence plus a blank line and the re-run note; CheckExit
// holds exit.
func applyCheckExit(judged response.VerdictArtifact, exit int) response.VerdictArtifact {
	note := fmt.Sprintf("Zing re-ran the check command: exit %d.", exit)
	if exit == -1 {
		note = "Zing re-ran the check command: timed out after 10m."
	}

	row := judged
	row.Evidence = judged.Evidence + "\n\n" + note
	if exit == 0 {
		row.Result = response.ResultPass
	} else {
		row.Result = response.ResultFail
	}
	row.CheckExit = &exit
	return row
}

// JudgePasses is 7.4's pass rule. final is the newest verdict row per
// scenario of the round (the caller dedups before calling this). The
// round passes when every behavior and negative row is Result pass; a
// performance row never counts toward the pass, failing or not. failures
// lists every behavior or negative row whose Result is not pass, in
// final's order.
func JudgePasses(final []response.VerdictArtifact) (pass bool, failures []response.VerdictArtifact) {
	pass = true
	for _, row := range final {
		if row.Kind == response.ScenarioKindPerformance {
			continue
		}
		if row.Result != response.ResultPass {
			pass = false
			failures = append(failures, row)
		}
	}
	return pass, failures
}

// scrubScenarioText is 7.4's scenario text scrub. Every occurrence in
// evidence of sc's Given, When, Then, and Check, each trimmed and 12
// runes or longer, is replaced with "[scenario text removed]", longest
// field first -- so a shorter field that is itself a substring of a
// longer one is consumed with it rather than matched on its own leftover
// piece. Replacement is case-sensitive. A field under 12 runes after
// trimming is left alone, even when it appears in evidence verbatim. The
// fix run reads this scrubbed text, never the judge's own quoting of the
// scenario.
func scrubScenarioText(evidence string, sc response.Scenario) string {
	const minScrubRunes = 12

	fields := []string{sc.Given, sc.When, sc.Then, sc.Check}
	candidates := make([]string, 0, len(fields))
	for _, f := range fields {
		trimmed := strings.TrimSpace(f)
		if utf8.RuneCountInString(trimmed) >= minScrubRunes {
			candidates = append(candidates, trimmed)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return utf8.RuneCountInString(candidates[i]) > utf8.RuneCountInString(candidates[j])
	})

	scrubbed := evidence
	for _, c := range candidates {
		scrubbed = strings.ReplaceAll(scrubbed, c, "[scenario text removed]")
	}
	return scrubbed
}

// renderFixFailures is 7.4's failure text: one block per failure in cohort
// order (scenarios' own order, not failures'), "<scenario_id>: <scrubbed
// evidence>", blocks separated by a blank line. Only each failure's
// scenario id and evidence are read; a failure for a scenario outside
// scenarios is skipped (CheckCoverage already refuses an unknown id, so
// this is defense in depth, not a path a caller should rely on).
func renderFixFailures(failures []response.VerdictArtifact, scenarios []response.Scenario) string {
	byScenario := make(map[string]response.VerdictArtifact, len(failures))
	for _, f := range failures {
		byScenario[f.Scenario] = f
	}

	blocks := make([]string, 0, len(failures))
	for _, sc := range scenarios {
		f, ok := byScenario[sc.ID]
		if !ok {
			continue
		}
		blocks = append(blocks, fmt.Sprintf("%s: %s", sc.ID, scrubScenarioText(f.Evidence, sc)))
	}
	return strings.Join(blocks, "\n\n")
}
