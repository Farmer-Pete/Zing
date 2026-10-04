package response

import (
	"strconv"
	"strings"
)

// Files returns p's delivered file changes (design section 6.11). p's zero
// value has a nil Delivery.Files, which this returns unchanged: a
// zero-length slice, never a panic.
func Files(p Plan) []FileChange {
	return p.Delivery.Files
}

// Fences returns p's deletions. It reads p.Delivery.Deletions.Items,
// hiding the $.delivery.deletions.deletions nesting the stored JSON shape
// carries (design section 6.11).
func Fences(p Plan) []Fence {
	return p.Delivery.Deletions.Items
}

// Tasks returns p's delivery tasks, in build order (design section 6.11).
func Tasks(p Plan) []Task {
	return p.Delivery.Tasks
}

// FileTasks parses f.Task ("1 3") into task numbers, in written order,
// duplicates kept. A token that is not a positive integer is skipped, so a
// malformed value Layer 1 already reported yields what it can; "" yields nil.
func FileTasks(f FileChange) []int {
	var out []int
	for tok := range strings.FieldsSeq(f.Task) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// TaskMapped reports whether p assigns files to tasks: true when at least
// one file names a task. A plan stored before the task attribute existed
// returns false, and the build keeps whole-plan scope for it.
func TaskMapped(p Plan) bool {
	for _, f := range p.Delivery.Files {
		if len(FileTasks(f)) > 0 {
			return true
		}
	}
	return false
}
