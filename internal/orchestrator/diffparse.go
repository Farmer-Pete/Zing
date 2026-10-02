package orchestrator

import (
	"regexp"
	"strconv"
	"strings"
)

// LineRange is one inclusive range of line numbers in one file of a diff
// (design section 10.1, 6.3).
type LineRange struct{ From, To int }

// DiffIndex maps a path to the ranges a finding may point into (design
// section 6.3): a modified or added file's new-side hunk ranges, or a
// deleted file's old-side ranges. A path git reported as binary is present
// with no ranges, so Contains never matches it -- present but unreachable,
// not absent.
type DiffIndex map[string][]LineRange

// Contains reports whether line lies inside one of path's ranges.
func (ix DiffIndex) Contains(path string, line int) bool {
	for _, r := range ix[path] {
		if line >= r.From && line <= r.To {
			return true
		}
	}
	return false
}

// devNull is the placeholder git substitutes for the missing side of an
// added or a deleted file's "---"/"+++" line, or of a binary file's
// summary line.
const devNull = "/dev/null"

// hunkHeaderPattern matches a unified diff hunk header's line-number part,
// "@@ -a[,b] +c[,d] @@", and ignores any trailing context text git appends
// after the closing "@@". b and d, each optional, mean 1 when omitted --
// git's own shorthand for a one-line range.
var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// binaryDiffSummaryPattern matches git's one-line summary in place of a
// binary file's diff body: "Binary files a/<old> and b/<new> differ" (with
// "/dev/null" on either side for a binary file added or deleted).
var binaryDiffSummaryPattern = regexp.MustCompile(`^Binary files (\S+) and (\S+) differ$`)

// ParseDiff is pure: a recursive-descent line scan of git's unified diff
// (design section 10.1), reading each file's "diff --git", "---"/"+++",
// and "@@" lines in the single forward order git always emits them, with
// no lookahead past the current line.
//
// ParseDiff assumes --no-renames output (every caller in this package runs
// git diff that way, PKG9-PLAN.md section 10.1), so "diff --git a/<p>
// b/<p>" always names the same path on both sides except at an add or a
// delete, where one side is "/dev/null". The path recorded is read off the
// "---"/"+++" lines that follow "diff --git", not off that header line
// itself: a path can contain spaces, which a split of "diff --git a/<p>
// b/<p>" cannot disambiguate, while "--- a/<p>" and "+++ b/<p>" each carry
// exactly one path.
//
// "+++ /dev/null" marks a deletion, so that file's hunks record old-side
// ranges (from the "-a,b" half of each "@@" header) instead of new-side
// ones (from "+c,d"); a file that is not a deletion records the new-side
// ranges for "d > 0" only (a hunk that only removes lines touches no line
// on the new side, so it contributes no range). "Binary files ... differ"
// records the path with no range and no hunk to read. A hunk header that
// does not match hunkHeaderPattern is skipped, not fatal: the scan simply
// moves to the next line.
func ParseDiff(diff string) DiffIndex {
	idx := make(DiffIndex)

	var (
		path     string
		deletion bool
	)

	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path = ""
			deletion = false

		case strings.HasPrefix(line, "--- "):
			if old := strings.TrimPrefix(line, "--- "); old != devNull {
				path = strings.TrimPrefix(old, "a/")
			}

		case strings.HasPrefix(line, "+++ "):
			newSide := strings.TrimPrefix(line, "+++ ")
			if newSide == devNull {
				deletion = true
			} else {
				path = strings.TrimPrefix(newSide, "b/")
			}
			recordPath(idx, path)

		case strings.HasPrefix(line, "Binary files "):
			if p, ok := binaryDiffPath(line); ok {
				recordPath(idx, p)
			}

		case path != "" && strings.HasPrefix(line, "@@ "):
			addHunkRange(idx, path, deletion, line)
		}
	}

	return idx
}

// recordPath ensures path is present in idx, even with no ranges yet, so a
// file whose hunks all turn out to contribute nothing (or that has none)
// still shows up as "in the diff" rather than absent.
func recordPath(idx DiffIndex, path string) {
	if path == "" {
		return
	}
	if _, ok := idx[path]; !ok {
		idx[path] = nil
	}
}

// addHunkRange parses one "@@ -a[,b] +c[,d] @@" line and appends the range
// it describes to path's entry in idx: the old side when deletion is true,
// otherwise the new side, and only when that side's count is greater than
// zero.
func addHunkRange(idx DiffIndex, path string, deletion bool, line string) {
	m := hunkHeaderPattern.FindStringSubmatch(line)
	if m == nil {
		return
	}

	oldStart, oldCount := atoi(m[1]), atoiDefault(m[2], 1)
	newStart, newCount := atoi(m[3]), atoiDefault(m[4], 1)

	if deletion {
		if oldCount > 0 {
			idx[path] = append(idx[path], LineRange{From: oldStart, To: oldStart + oldCount - 1})
		}
		return
	}
	if newCount > 0 {
		idx[path] = append(idx[path], LineRange{From: newStart, To: newStart + newCount - 1})
	}
}

// binaryDiffPath extracts the path from a "Binary files a/<old> and b/<new>
// differ" line, preferring the new (b/) side and falling back to the old
// (a/) side when the new side is "/dev/null" (a binary file deleted).
func binaryDiffPath(line string) (string, bool) {
	m := binaryDiffSummaryPattern.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	for _, side := range []string{m[2], m[1]} {
		if side == devNull {
			continue
		}
		p := strings.TrimPrefix(strings.TrimPrefix(side, "a/"), "b/")
		return p, true
	}
	return "", false
}

// atoi parses s, a digit group hunkHeaderPattern already constrained to
// "\d+", as a non-negative int. It cannot fail given that pattern, but
// returns 0 rather than panicking if it somehow did.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// atoiDefault is atoi, but returns def for an empty s: hunkHeaderPattern's
// optional count group is empty exactly when the header omitted it, which
// git's own convention means 1.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	return atoi(s)
}
