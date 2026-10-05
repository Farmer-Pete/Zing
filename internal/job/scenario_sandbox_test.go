package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// scenario0CheckPath, okThen, and okCheck are shared across this test's
// table so goconst doesn't see the sandbox-probe literals repeated per
// case; every case below pairs a check under test with a second,
// unremarkable scenario to satisfy checkScenarioShape's own 2-scenario
// minimum.
const (
	scenario0CheckPath = "scenarios/scenario[0]/check"
	okThen             = "passes"
	okCheck            = `go test ./internal/job -run TestX`
)

// TestCheckScenarioShape_RejectsHostSandboxProbes is a regression test for
// #78: the sealed checks ran Zing's own darwin sandbox probes
// (sandbox-exec, internal/sandbox), which cannot start inside the seatbelt
// the judge and CHECK already run under, so every probe skipped and
// "passed" while a real failure on the host hid behind it. Such a check is
// refused at planning time.
func TestCheckScenarioShape_RejectsHostSandboxProbes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		scenarios []response.Scenario
		wantPaths []string
		wantMsgs  []string
	}{
		{
			name: "sandbox-exec",
			scenarios: []response.Scenario{
				{ID: "s1", Then: "denies the write", Check: `sandbox-exec -p '(version 1)' true`},
				{ID: "s2", Then: okThen, Check: okCheck},
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{hostSandboxCheckMsg},
		},
		{
			name: "internal/sandbox package",
			scenarios: []response.Scenario{
				{ID: "s1", Then: "denies the push", Check: `go test ./internal/sandbox -run 'Test(Build|Judge)DeniesGitPush' -count=1`},
				{ID: "s2", Then: okThen, Check: okCheck},
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{hostSandboxCheckMsg},
		},
		{
			name: "both strings in one check still gives one error",
			scenarios: []response.Scenario{
				{ID: "s1", Then: "denies the push", Check: `sandbox-exec -p '(version 1)' go test ./internal/sandbox -run TestX`},
				{ID: "s2", Then: okThen, Check: okCheck},
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{hostSandboxCheckMsg},
		},
		{
			name: "ordinary check and empty check give none",
			scenarios: []response.Scenario{
				{ID: "s1", Then: okThen, Check: okCheck},
				{ID: "s2", Then: okThen, Check: ``},
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			// go test ./internal/sandboxes also matches on the
			// "internal/sandbox" substring; the plan accepts that
			// false positive rather than parsing the package path.
			name: "similarly named package also matches",
			scenarios: []response.Scenario{
				{ID: "s1", Then: okThen, Check: `go test ./internal/sandboxes -run TestX`},
				{ID: "s2", Then: okThen, Check: okCheck},
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{hostSandboxCheckMsg},
		},
		{
			name: "/tmp/ and internal/sandbox both match, /tmp first",
			scenarios: []response.Scenario{
				{ID: "s1", Then: okThen, Check: `sh -c 'go build -o /tmp/zing-s1 ./cmd/zing && go test ./internal/sandbox -run TestX'`},
				{ID: "s2", Then: okThen, Check: okCheck},
			},
			wantPaths: []string{scenario0CheckPath, scenario0CheckPath},
			// The /tmp/ message comes first; both are matched by
			// substring below.
			wantMsgs: []string{"$TMPDIR", hostSandboxCheckMsg},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errs := checkScenarioShape(tt.scenarios)
			if len(errs) != len(tt.wantPaths) {
				t.Fatalf("checkScenarioShape = %+v, want %d error(s) at %v", errs, len(tt.wantPaths), tt.wantPaths)
			}
			for i, path := range tt.wantPaths {
				if errs[i].Path != path {
					t.Errorf("error %d path = %q, want %q", i, errs[i].Path, path)
				}
			}
			for i, wantMsg := range tt.wantMsgs {
				if !strings.Contains(errs[i].Msg, wantMsg) {
					t.Errorf("error %d msg = %q, want it to mention %q", i, errs[i].Msg, wantMsg)
				}
			}
		})
	}
}
