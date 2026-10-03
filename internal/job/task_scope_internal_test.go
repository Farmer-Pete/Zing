package job

// task_scope_internal_test.go tests building.go's own unexported
// foreignTaskPaths (task 2) and declaredPaths/acceptedPaths task scoping
// (task 4) of "check each build task against its own files, not the whole
// plan": pure functions over a plan, events, and a changed path list,
// reached by no seam in building_test.go (package job_test).

import (
	"slices"
	"testing"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// testScopeGreetPath and testScopeHelperPath are this file's own file-path
// literals, named so goconst has nothing to flag across this file's many
// cases (pbHelloTxt, postbuild_test.go, already names "hello.txt").
const (
	testScopeGreetPath  = "greet.go"
	testScopeHelperPath = "helper.go"
)

func fileTask(path, task string) response.FileChange {
	return response.FileChange{Path: path, Task: task}
}

// TestForeignTaskPaths proves foreignTaskPaths' own ownership rule (design
// section "shape", rule 2): a path the plan assigns only to tasks other
// than taskN is foreign, named by its owners; everything else -- a path
// the unit owns, a shared path the unit is among the owners of, an extra
// the plan never declares, a plan with no task mapping, and a fix unit --
// is not.
func TestForeignTaskPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		plan    response.Plan
		taskN   int
		changed []string
		want    []string
	}{
		{
			name:    "own",
			plan:    response.Plan{Delivery: response.Delivery{Files: []response.FileChange{fileTask(pbHelloTxt, "1")}}},
			taskN:   1,
			changed: []string{pbHelloTxt},
			want:    nil,
		},
		{
			name: "other",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(pbHelloTxt, "1"), fileTask(testScopeGreetPath, "2"),
			}}},
			taskN:   1,
			changed: []string{testScopeGreetPath},
			want:    []string{"greet.go belongs to task 2, not task 1"},
		},
		{
			name: "shared",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(testScopeGreetPath, "1 2"),
			}}},
			taskN:   2,
			changed: []string{testScopeGreetPath},
			want:    nil,
		},
		{
			name: "two owners",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(testScopeHelperPath, "2 3"),
			}}},
			taskN:   1,
			changed: []string{testScopeHelperPath},
			want:    []string{"helper.go belongs to tasks 2 and 3, not task 1"},
		},
		{
			name: "three owners",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(testScopeHelperPath, "1 2 4"),
			}}},
			taskN:   3,
			changed: []string{testScopeHelperPath},
			want:    []string{"helper.go belongs to tasks 1, 2 and 4, not task 3"},
		},
		{
			name: "duplicate owners",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(testScopeHelperPath, "2"), fileTask(testScopeHelperPath, "2"),
			}}},
			taskN:   1,
			changed: []string{testScopeHelperPath},
			want:    []string{"helper.go belongs to task 2, not task 1"},
		},
		{
			name: "extra",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(pbHelloTxt, "1"),
			}}},
			taskN:   1,
			changed: []string{"extra1.go"},
			want:    nil,
		},
		{
			name: "legacy",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				{Path: testScopeGreetPath},
			}}},
			taskN:   1,
			changed: []string{testScopeGreetPath},
			want:    nil,
		},
		{
			name: "fix unit",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(pbHelloTxt, "1"), fileTask(testScopeGreetPath, "2"),
			}}},
			taskN:   0,
			changed: []string{testScopeGreetPath},
			want:    nil,
		},
		{
			name: "deleted",
			plan: response.Plan{Delivery: response.Delivery{Files: []response.FileChange{
				fileTask(pbHelloTxt, "1"), fileTask(testScopeGreetPath, "2"),
			}}},
			taskN:   1,
			changed: changedPathList([]orchestrator.Change{{Path: testScopeGreetPath, Code: orchestrator.Deleted}}),
			want:    []string{"greet.go belongs to task 2, not task 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := foreignTaskPaths(tc.plan, tc.taskN, tc.changed)
			if len(got) != len(tc.want) {
				t.Fatalf("foreignTaskPaths = %v, want %v", got, tc.want)
			}
			for i, e := range got {
				if e.Path != claimsFilesChangedPath {
					t.Errorf("errs[%d].Path = %q, want %q", i, e.Path, claimsFilesChangedPath)
				}
				if e.Msg != tc.want[i] {
					t.Errorf("errs[%d].Msg = %q, want %q", i, e.Msg, tc.want[i])
				}
			}
		})
	}
}

const testScopeExtraPath = "extra.go"

func acceptedExtraEvent(path string, taskN int) store.FileEventRow {
	accept := response.PerimeterAccept
	return store.FileEventRow{
		ArtifactID: 1,
		File: response.FileArtifact{
			FileChange: response.FileChange{Path: path},
			TaskN:      taskN,
			Decision:   &accept,
		},
	}
}

// TestDeclaredPathsScopesAcceptedExtras proves declaredPaths and
// acceptedPaths' own extraInScope rule (design rule 4): an accepted extra
// counts as declared, and is offered to the builder as accepted, only for
// the task the owner accepted it for -- unless the plan carries no task
// mapping at all (a plan stored before this change), which keeps
// whole-plan scope for every unit.
func TestDeclaredPathsScopesAcceptedExtras(t *testing.T) {
	t.Parallel()

	mapped := response.Plan{Delivery: response.Delivery{Files: []response.FileChange{fileTask(pbHelloTxt, "1")}}}
	events := []store.FileEventRow{acceptedExtraEvent(testScopeExtraPath, 1)}

	cases := []struct {
		name  string
		plan  response.Plan
		taskN int
		want  bool
	}{
		{"mapped task 1 (the accepting task)", mapped, 1, true},
		{"mapped fix unit", mapped, 0, true},
		{"mapped task 2 (a later task)", mapped, 2, false},
		{"unmapped task 2", response.Plan{Delivery: response.Delivery{Files: []response.FileChange{{Path: pbHelloTxt}}}}, 2, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := slices.Contains(declaredPaths(tc.plan, events, nil, tc.taskN), testScopeExtraPath); got != tc.want {
				t.Errorf("declaredPaths contains %q = %v, want %v", testScopeExtraPath, got, tc.want)
			}
			if got := slices.Contains(acceptedPaths(tc.plan, events, tc.taskN), testScopeExtraPath); got != tc.want {
				t.Errorf("acceptedPaths contains %q = %v, want %v", testScopeExtraPath, got, tc.want)
			}
		})
	}
}
