package response

import "testing"

// testOwnerEditActionEdit is this file's own "edit" literal (goconst):
// store.OwnerEditActionEdit lives in internal/store, which this package
// must not import.
const testOwnerEditActionEdit = "edit"

// TestOwnerEditLine proves the exact sentence OwnerEditLine renders for
// each of the four target/action combinations the design names.
func TestOwnerEditLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		e    OwnerEditEvent
		want string
	}{
		{"scenario edit", OwnerEditEvent{Target: "scenario", Ref: "s3", Action: testOwnerEditActionEdit}, "Owner edited scenario s3."},
		{"plan_task edit", OwnerEditEvent{Target: "plan_task", Ref: "2", Action: testOwnerEditActionEdit}, "Owner edited plan task 2."},
		{
			"plan_task drop",
			OwnerEditEvent{Target: "plan_task", Ref: "2", Action: "drop"},
			"Owner dropped plan task 2; later tasks moved up one.",
		},
		{
			"ticket_body edit",
			OwnerEditEvent{Target: "ticket_body", Ref: "", Action: testOwnerEditActionEdit},
			"Owner amended the ticket body.",
		},
		{
			"plan_file edit",
			OwnerEditEvent{Target: "plan_file", Ref: testFileA, Action: testOwnerEditActionEdit, Old: "6", New: testTasks2And6},
			"Owner set the tasks for a.go to 2 6 (was 6).",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := OwnerEditLine(tc.e); got != tc.want {
				t.Errorf("OwnerEditLine(%+v) = %q, want %q", tc.e, got, tc.want)
			}
		})
	}
}
