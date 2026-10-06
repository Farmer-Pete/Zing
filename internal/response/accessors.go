package response

import (
	"fmt"
	"slices"
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

// FileTaskChange is one file whose task list GrantFileTasks changed.
type FileTaskChange struct {
	Path string
	Old  string
	New  string
}

// FormatTaskList returns nums ascending, unique, joined by single spaces:
// the one canonical task-list format GrantFileTasks and the console's
// plan_file owner edit (store.normalizeTaskList) both write, so a granted
// file and a console-edited one can never drift onto different shapes.
func FormatTaskList(nums []int) string {
	nums = slices.Compact(slices.Sorted(slices.Values(nums)))
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, " ")
}

// GrantFileTasks returns p with g.Task added to the task list of every
// delivery file entry named in g.Paths whose list is non-empty and lacks
// it, plus one change per path that moved. Pure: p is not modified.
func GrantFileTasks(p Plan, g FileGrant) (Plan, []FileTaskChange) {
	out := p
	files := slices.Clone(p.Delivery.Files)
	var changes []FileTaskChange
	for i, f := range files {
		nums := FileTasks(f)
		named := slices.Contains(g.Paths, f.Path)
		mapped := len(nums) > 0
		has := slices.Contains(nums, g.Task)
		if !named || !mapped || has {
			continue
		}
		old := f.Task
		files[i].Task = FormatTaskList(append(nums, g.Task))
		if !slices.ContainsFunc(changes, func(c FileTaskChange) bool { return c.Path == f.Path }) {
			changes = append(changes, FileTaskChange{Path: f.Path, Old: old, New: files[i].Task})
		}
	}
	out.Delivery.Files = files
	return out, changes
}

// FileGrantOptionText is option d's text for g.
func FileGrantOptionText(g FileGrant) string {
	return fmt.Sprintf("Let task %d also change %s", g.Task, strings.Join(g.Paths, ", "))
}
