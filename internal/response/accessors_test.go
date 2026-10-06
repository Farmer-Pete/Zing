package response

import (
	"bytes"
	"encoding/json"
	"testing"
)

// mustJSON marshals v for a round-trip byte comparison, per this package's
// standard-library-only test convention (no testify, no go-cmp).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestFiles_EmptyPlanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := Files(Plan{}); len(got) != 0 {
		t.Errorf("Files(Plan{}) = %v, want empty", got)
	}
}

func TestFences_EmptyPlanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := Fences(Plan{}); len(got) != 0 {
		t.Errorf("Fences(Plan{}) = %v, want empty", got)
	}
}

func TestTasks_EmptyPlanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := Tasks(Plan{}); len(got) != 0 {
		t.Errorf("Tasks(Plan{}) = %v, want empty", got)
	}
}

func TestFiles_ReturnsDeliveryFiles(t *testing.T) {
	t.Parallel()
	p := cleanPlan()
	got, want := Files(p), p.Delivery.Files
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("Files(p) = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

// TestFences_ReturnsDeletionItems proves Fences reads
// p.Delivery.Deletions.Items, hiding the $.delivery.deletions.deletions
// nesting design section 6.11 calls out.
func TestFences_ReturnsDeletionItems(t *testing.T) {
	t.Parallel()
	p := cleanPlan()
	p.Delivery.Deletions = Deletions{
		Items: []Fence{{Path: testMainGo, Symbol: "oldHandler", ExistedBecause: "existed because it predated the rewrite"}},
	}
	got, want := Fences(p), p.Delivery.Deletions.Items
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("Fences(p) = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestTasks_ReturnsDeliveryTasks(t *testing.T) {
	t.Parallel()
	p := cleanPlan()
	got, want := Tasks(p), p.Delivery.Tasks
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Errorf("Tasks(p) = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestFileTasks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		task string
		want []int
	}{
		{"single", "1", []int{1}},
		{"multiple", "1 3", []int{1, 3}},
		{"empty", "", nil},
		{"skipsMalformed", "1 x 0", []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FileTasks(FileChange{Task: tt.task})
			if !bytes.Equal(mustJSON(t, got), mustJSON(t, tt.want)) {
				t.Errorf("FileTasks(FileChange{Task: %q}) = %v, want %v", tt.task, got, tt.want)
			}
		})
	}
}

// testTasks2And6 is GrantFileTasks' own "2 6" result literal (goconst):
// TestGrantFileTasks and TestOwnerEditLine share it across this package's
// test files.
const testTasks2And6 = "2 6"

// testTasks6And3 and testTasks2And3And6 are TestGrantFileTasks' own
// "insertsBetween" case literals (goconst), its own task field and the
// change it asserts against.
const (
	testTasks6And3     = "6 3"
	testTasks2And3And6 = "2 3 6"
)

// TestGrantFileTasks proves GrantFileTasks' task-list arithmetic (design
// plan #51, rule 3): a new task number is inserted in ascending, unique
// order, a file already carrying it is left unchanged, an empty task list
// stays empty, a path with no entry is ignored, and two entries for one
// path both change while reporting a single FileTaskChange. The input
// plan's Files slice is unchanged afterwards.
func TestGrantFileTasks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		task        string
		want        string
		wantChanges []FileTaskChange
	}{
		{"appendsAscending", "6", testTasks2And6, []FileTaskChange{{Path: testFileA, Old: "6", New: testTasks2And6}}},
		{"insertsBetween", testTasks6And3, testTasks2And3And6, []FileTaskChange{{Path: testFileA, Old: testTasks6And3, New: testTasks2And3And6}}},
		{"noChangeWhenAlreadyPresent", testTasks2And6, testTasks2And6, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := Plan{Delivery: Delivery{Files: []FileChange{{Path: testFileA, Task: tt.task}}}}
			got, changes := GrantFileTasks(p, FileGrant{Task: 2, Paths: []string{testFileA}})
			if got.Delivery.Files[0].Task != tt.want {
				t.Errorf("Task = %q, want %q", got.Delivery.Files[0].Task, tt.want)
			}
			if !bytes.Equal(mustJSON(t, changes), mustJSON(t, tt.wantChanges)) {
				t.Errorf("changes = %+v, want %+v", changes, tt.wantChanges)
			}
		})
	}

	t.Run("emptyTaskStaysEmpty", func(t *testing.T) {
		t.Parallel()
		p := Plan{Delivery: Delivery{Files: []FileChange{{Path: testFileA, Task: ""}}}}
		got, changes := GrantFileTasks(p, FileGrant{Task: 2, Paths: []string{testFileA}})
		if got.Delivery.Files[0].Task != "" {
			t.Errorf("Task = %q, want empty", got.Delivery.Files[0].Task)
		}
		if len(changes) != 0 {
			t.Errorf("changes = %+v, want none", changes)
		}
	})

	t.Run("missingPathIsIgnored", func(t *testing.T) {
		t.Parallel()
		p := Plan{Delivery: Delivery{Files: []FileChange{{Path: testFileA, Task: "6"}}}}
		got, changes := GrantFileTasks(p, FileGrant{Task: 2, Paths: []string{testFileB}})
		if got.Delivery.Files[0].Task != "6" {
			t.Errorf("Task = %q, want unchanged 6", got.Delivery.Files[0].Task)
		}
		if len(changes) != 0 {
			t.Errorf("changes = %+v, want none", changes)
		}
	})

	t.Run("twoEntriesForOnePathBothChangeWithOneFileTaskChange", func(t *testing.T) {
		t.Parallel()
		p := Plan{Delivery: Delivery{Files: []FileChange{
			{Path: testFileA, Task: "6", Reason: "reason one"},
			{Path: testFileA, Task: testTasks6And3, Reason: "reason two"},
		}}}
		got, changes := GrantFileTasks(p, FileGrant{Task: 2, Paths: []string{testFileA}})
		if got.Delivery.Files[0].Task != testTasks2And6 {
			t.Errorf("Files[0].Task = %q, want %q", got.Delivery.Files[0].Task, testTasks2And6)
		}
		if got.Delivery.Files[1].Task != testTasks2And3And6 {
			t.Errorf("Files[1].Task = %q, want %q", got.Delivery.Files[1].Task, testTasks2And3And6)
		}
		if len(changes) != 1 {
			t.Fatalf("changes = %+v, want exactly 1", changes)
		}
		if changes[0] != (FileTaskChange{Path: testFileA, Old: "6", New: testTasks2And6}) {
			t.Errorf("changes[0] = %+v, want {a.go 6 2 6}", changes[0])
		}
	})

	t.Run("inputPlanUnchanged", func(t *testing.T) {
		t.Parallel()
		p := Plan{Delivery: Delivery{Files: []FileChange{{Path: testFileA, Task: "6"}}}}
		GrantFileTasks(p, FileGrant{Task: 2, Paths: []string{testFileA}})
		if p.Delivery.Files[0].Task != "6" {
			t.Errorf("input plan's Task mutated to %q, want unchanged 6", p.Delivery.Files[0].Task)
		}
	})
}

func TestTaskMapped(t *testing.T) {
	t.Parallel()
	t.Run("false when every file's Task is empty", func(t *testing.T) {
		t.Parallel()
		p := Plan{Delivery: Delivery{Files: []FileChange{{Path: "a.go"}, {Path: "b.go"}}}}
		if TaskMapped(p) {
			t.Errorf("TaskMapped(p) = true, want false")
		}
	})
	t.Run("true when one file has a task", func(t *testing.T) {
		t.Parallel()
		p := Plan{Delivery: Delivery{Files: []FileChange{{Path: "a.go"}, {Path: "b.go", Task: "2"}}}}
		if !TaskMapped(p) {
			t.Errorf("TaskMapped(p) = false, want true")
		}
	})
}
