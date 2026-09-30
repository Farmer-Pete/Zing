package job

import (
	"fmt"
	stdpath "path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"zing/internal/orchestrator"
	"zing/internal/response"
)

// locationPattern is 6.3's location grammar, applied after trimming spaces:
// "<path>:<line>", where line has no leading zero and is never "0".
var locationPattern = regexp.MustCompile(`^(.+):([1-9]\d*)$`)

// ParseLocation parses one finding's Location (design section 6.3): after
// trimming spaces, locationPattern splits path from line; path is cleaned
// with path.Clean and must not start with "/" or "../". Anything else --
// no colon, a non-digit or zero-leading line, an absolute or escaping path
// -- returns ok = false.
func ParseLocation(loc string) (path string, line int, ok bool) {
	m := locationPattern.FindStringSubmatch(strings.TrimSpace(loc))
	if m == nil {
		return "", 0, false
	}

	cleaned := stdpath.Clean(m[1])
	if strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") {
		return "", 0, false
	}

	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return cleaned, n, true
}

// FilterFindings applies 6.3's diff filter and fidelity check to one
// round's raw lens findings: a finding with a bad Location, a Location
// outside idx (its path not in the diff, or its line outside every range
// the diff records for that path), or -- for a fidelity finding only -- an
// empty PlanRef after trimming, is dropped. Order among survivors follows
// findings.
func FilterFindings(findings []response.Finding, idx orchestrator.DiffIndex) []response.Finding {
	survivors := make([]response.Finding, 0, len(findings))
	for _, f := range findings {
		path, line, ok := ParseLocation(f.Location)
		if !ok || !idx.Contains(path, line) {
			continue
		}
		if f.Lens == response.LensFidelity && strings.TrimSpace(f.PlanRef) == "" {
			continue
		}
		survivors = append(survivors, f)
	}
	return survivors
}

// DedupFindings merges FilterFindings' survivors that share the same
// (path, line) into one row per location (design section 6.3): Severity is
// the highest of the group by Rank; Lenses lists every reporting lens, in
// lensOrder (machine.toml's jobs.review.lenses); Lens is the first of them;
// Text and Fix are each lens's own text (or fix) as "[<lens>] <text>",
// blank-line joined, in lens order; PlanRef is the fidelity lens's, or
// empty when fidelity did not report this location; Location is
// "<path>:<line>", normalized. The result is ordered by (path, line) --
// ascending path, then ascending line -- so the caller can assign r<n>f<k>
// ids by walking it in order (6.2). A finding with a Location
// ParseLocation cannot parse is skipped: FilterFindings already removes
// those from any input built the way 6.2 and 6.6 build it, so this is
// defense in depth, not a path a caller should rely on.
func DedupFindings(findings []response.Finding, lensOrder []response.Lens) []response.FindingArtifact {
	type key struct {
		path string
		line int
	}
	type group struct {
		key   key
		items []response.Finding
	}

	groups := make(map[key]*group, len(findings))
	order := make([]key, 0, len(findings))
	for _, f := range findings {
		path, line, ok := ParseLocation(f.Location)
		if !ok {
			continue
		}
		k := key{path, line}
		g, exists := groups[k]
		if !exists {
			g = &group{key: k}
			groups[k] = g
			order = append(order, k)
		}
		g.items = append(g.items, f)
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].path != order[j].path {
			return order[i].path < order[j].path
		}
		return order[i].line < order[j].line
	})

	lensRank := make(map[response.Lens]int, len(lensOrder))
	for i, l := range lensOrder {
		lensRank[l] = i
	}

	merged := make([]response.FindingArtifact, 0, len(order))
	for _, k := range order {
		items := append([]response.Finding(nil), groups[k].items...)
		sort.SliceStable(items, func(i, j int) bool {
			return lensRank[items[i].Lens] < lensRank[items[j].Lens]
		})

		row := response.FindingArtifact{Location: fmt.Sprintf("%s:%d", k.path, k.line)}
		var texts, fixes []string
		best := items[0].Severity
		for _, it := range items {
			if it.Severity.Rank() > best.Rank() {
				best = it.Severity
			}
			row.Lenses = append(row.Lenses, it.Lens)
			texts = append(texts, fmt.Sprintf("[%s] %s", it.Lens, it.Text))
			fixes = append(fixes, fmt.Sprintf("[%s] %s", it.Lens, it.Fix))
			if it.Lens == response.LensFidelity {
				row.PlanRef = it.PlanRef
			}
		}
		row.Severity = best
		row.Lens = row.Lenses[0]
		row.Text = strings.Join(texts, "\n\n")
		row.Fix = strings.Join(fixes, "\n\n")
		merged = append(merged, row)
	}
	return merged
}

// splitByFloor partitions dedup rows by 6.3's floor rule: at or below floor
// (Severity.Rank() <= floor.Rank()) get Decision accept and route straight
// to a fix run; above the floor keep Decision nil and route to the review
// question (6.4). Order within each bucket follows rows.
func splitByFloor(rows []response.FindingArtifact, floor response.Severity) (atOrBelow, above []response.FindingArtifact) {
	for i := range rows {
		if rows[i].Severity.Rank() <= floor.Rank() {
			row := rows[i]
			accept := response.FindingAccept
			row.Decision = &accept
			atOrBelow = append(atOrBelow, row)
		} else {
			above = append(above, rows[i])
		}
	}
	return atOrBelow, above
}

// selectLenses is 6.7's re-review lens selection. Round 1 always runs the
// whole set, in allLenses' order. For round >= 2, a lens runs when it is
// fidelity, or when some row of prev (the previous round's rows, newest per
// ID) has Decision accept, lists the lens in Lenses, and has a path
// (ParseLocation on its Location) in changed. The result keeps allLenses'
// order.
func selectLenses(round int, allLenses []response.Lens, prev []response.FindingArtifact, changed []string) []response.Lens {
	if round <= 1 {
		return allLenses
	}

	changedSet := make(map[string]struct{}, len(changed))
	for _, p := range changed {
		changedSet[p] = struct{}{}
	}

	runs := make(map[response.Lens]struct{})
	for i := range prev {
		row := &prev[i]
		if row.Decision == nil || *row.Decision != response.FindingAccept {
			continue
		}
		path, _, ok := ParseLocation(row.Location)
		if !ok {
			continue
		}
		if _, changed := changedSet[path]; !changed {
			continue
		}
		for _, l := range row.Lenses {
			runs[l] = struct{}{}
		}
	}

	selected := make([]response.Lens, 0, len(allLenses))
	for _, l := range allLenses {
		if l == response.LensFidelity {
			selected = append(selected, l)
			continue
		}
		if _, ok := runs[l]; ok {
			selected = append(selected, l)
		}
	}
	return selected
}

// findingIDPattern parses a finding id's round and per-round sequence
// number k, so callers can sort in true numeric id order rather than
// lexical string order ("r1f10" would otherwise sort before "r1f2").
var findingIDPattern = regexp.MustCompile(`^r([1-9]\d*)[fh]([1-9]\d*)$`)

// findingIDKey returns id's (round, k) for numeric sorting. An id that does
// not match the closed r<round>f<k>/r<round>h<k> shape sorts last (ok =
// false): the rows these functions are handed are always store-validated
// against that schema pattern, so this is defense in depth, not a path a
// caller should rely on.
func findingIDKey(id string) (round, k int, ok bool) {
	m := findingIDPattern.FindStringSubmatch(id)
	if m == nil {
		return 0, 0, false
	}
	round, roundErr := strconv.Atoi(m[1])
	k, kErr := strconv.Atoi(m[2])
	if roundErr != nil || kErr != nil {
		return 0, 0, false
	}
	return round, k, true
}

// sortByID returns rows sorted by numeric id order (findingIDKey), stable
// so two rows that somehow shared an id keep their input order. It does
// not mutate rows.
func sortByID(rows []response.FindingArtifact) []response.FindingArtifact {
	sorted := append([]response.FindingArtifact(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, ki, oki := findingIDKey(sorted[i].ID)
		rj, kj, okj := findingIDKey(sorted[j].ID)
		if oki != okj {
			return oki
		}
		if ri != rj {
			return ri < rj
		}
		return ki < kj
	})
	return sorted
}

// findingBlock renders row as 6.3's fix-text block: "<id> <severity> <lens>
// <path>:<line>", then row's own text, then "Fix: <fix>", each on its own
// line. It carries no decision filter of its own -- renderFixFindings adds
// that -- so DISCUSS (6.6) can reuse it to render a whole group's findings
// regardless of decision.
func findingBlock(row response.FindingArtifact) string {
	return fmt.Sprintf("%s %s %s %s\n%s\nFix: %s", row.ID, row.Severity, row.Lens, row.Location, row.Text, row.Fix)
}

// renderFixFindings is 6.3's fix text: one findingBlock per finding with
// Decision accept, in ID order, blocks separated by a blank line. The
// fenced result is what the fix prompt's findings input carries.
func renderFixFindings(rows []response.FindingArtifact) string {
	sorted := sortByID(rows)
	blocks := make([]string, 0, len(sorted))
	for i := range sorted {
		row := &sorted[i]
		if row.Decision == nil || *row.Decision != response.FindingAccept {
			continue
		}
		blocks = append(blocks, findingBlock(*row))
	}
	return strings.Join(blocks, "\n\n")
}
