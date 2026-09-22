package response

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"
)

// BuildObservation is what the orchestrator actually observed after a build
// task, injected by the caller so CheckBuildClaims stays a pure function
// (design decision Q3). Package 2 defines this type beside the wire types;
// it never appears on the wire itself.
type BuildObservation struct {
	FilesChanged []string
	TestExit     int
	LintExit     int
}

// CheckBuildClaims compares a build's claims against what was actually
// observed (design section 6.5). BuildClaims carries no tests_added field
// (decision Q-b), so this checks only test_exit, lint_exit, and
// files_changed, in that order (design section 6.5).
func CheckBuildClaims(c BuildClaims, o BuildObservation) []*PathError {
	var errs []*PathError
	if o.TestExit != c.TestExit {
		errs = append(errs, &PathError{
			Path: "claims/test_exit",
			Msg:  fmt.Sprintf("observed %d, claimed %d", o.TestExit, c.TestExit),
		})
	}
	if o.LintExit != c.LintExit {
		errs = append(errs, &PathError{
			Path: "claims/lint_exit",
			Msg:  fmt.Sprintf("observed %d, claimed %d", o.LintExit, c.LintExit),
		})
	}
	if !sameSet(o.FilesChanged, c.FilesChanged) {
		errs = append(errs, &PathError{
			Path: "claims/files_changed",
			Msg:  fmt.Sprintf("observed %s, claimed %s", formatSet(o.FilesChanged), formatSet(c.FilesChanged)),
		})
	}
	return errs
}

// sortedUnique returns items deduplicated and sorted, the display form
// CheckBuildClaims uses for a files_changed mismatch.
func sortedUnique(items []string) []string {
	set := make(map[string]struct{}, len(items))
	for _, it := range items {
		set[it] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for it := range set {
		out = append(out, it)
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	return slices.Equal(sortedUnique(a), sortedUnique(b))
}

func formatSet(items []string) string {
	return "[" + strings.Join(sortedUnique(items), ", ") + "]"
}

// claimPath builds the element path for one field of one indexed claim,
// e.g. claimPath(0, "evidence") -> "claims/claim[0]/evidence".
func claimPath(i int, field string) string {
	return joinPath(joinPath("claims", indexedName("claim", i)), field)
}

// CheckCodeClaims checks every Kind == "code" claim's evidence path against
// fsys (design section 6.5). Evidence is "<path>:<line>" for a code claim;
// only the path before the first ':' is checked. The path must resolve to a
// file: a code claim cites a file and a line, so a directory (which has no
// line) does not satisfy it and draws the same "no such file" error.
func CheckCodeClaims(claims []Claim, fsys fs.FS) []*PathError {
	var errs []*PathError
	for i, c := range claims {
		if c.Kind != ClaimKindCode {
			continue
		}
		path, _, _ := strings.Cut(c.Evidence, ":")
		info, err := fs.Stat(fsys, path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			errs = append(errs, &PathError{Path: claimPath(i, "evidence"), Msg: "no such file " + path})
		case err != nil:
			// A non-missing inspection failure (permission, I/O) is a real
			// error, not a claim that the path is absent; report it as itself.
			errs = append(errs, &PathError{Path: claimPath(i, "evidence"), Msg: "cannot inspect " + path + ": " + err.Error()})
		case info.IsDir():
			errs = append(errs, &PathError{Path: claimPath(i, "evidence"), Msg: "no such file " + path})
		}
	}
	return errs
}

// CheckNothingToDoClaims enforces the nothing_to_do rule (design section
// 6.4): every code claim must be verdict false, since a nothing_to_do
// outcome asserts there is nothing left to build. Env claims are not
// checked; only a code claim can carry a false-positive "this already
// exists" verdict.
func CheckNothingToDoClaims(claims []Claim) []*PathError {
	var errs []*PathError
	for i, c := range claims {
		if c.Kind != ClaimKindCode {
			continue
		}
		if c.Verdict != ClaimVerdictFalse {
			errs = append(errs, &PathError{
				Path: claimPath(i, "verdict"),
				Msg:  "nothing_to_do needs every code claim false",
			})
		}
	}
	return errs
}

// CheckCoverage compares a plan's scenario ids against a judge's verdicts:
// every scenario needs exactly one verdict (design section 6.5). These
// messages carry no element path (a verdict is judged outside any single
// document), so Path is empty; read the message from Msg, not Error().
func CheckCoverage(scenarioIDs []string, verdicts []Verdict) []*PathError {
	var errs []*PathError

	counts := make(map[string]int, len(verdicts))
	for _, v := range verdicts {
		counts[v.Scenario]++
	}

	known := make(map[string]bool, len(scenarioIDs))
	for _, id := range scenarioIDs {
		known[id] = true
		switch counts[id] {
		case 0:
			errs = append(errs, &PathError{Msg: "missing verdict for scenario " + id})
		case 1:
			// covered
		default:
			errs = append(errs, &PathError{Msg: "duplicate verdict for scenario " + id})
		}
	}

	for _, v := range verdicts {
		if !known[v.Scenario] {
			errs = append(errs, &PathError{Msg: "verdict for unknown scenario " + v.Scenario})
		}
	}

	return errs
}
