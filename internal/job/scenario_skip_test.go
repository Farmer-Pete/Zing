package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// TestCheckScenarioShape_ExpectedSkip is a regression test for the judge
// treating an expected skip as unobserved (#129 s5): a check that greps
// the "--- SKIP:" line asserts the skip itself, so it is exempt from the
// host-sandbox rule (#78) and satisfies the new rule that a then expecting
// a skip needs a check that proves the skip happened.
func TestCheckScenarioShape_ExpectedSkip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		scenario0 response.Scenario
		wantPaths []string
		wantMsgs  []string
	}{
		{
			name: "sandbox_probe_skip_grepped",
			scenario0: response.Scenario{
				ID:    "s1",
				Then:  `the probe skips with "set ZING_LIVE_CLI=1 ..."`,
				Check: `go test ./internal/sandbox -run TestProbeTLSWithoutSecurityServer -v -count=1 | grep -q -- '--- SKIP: TestProbeTLSWithoutSecurityServer'`,
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			name: "bare_go_test_rejected",
			scenario0: response.Scenario{
				ID:    "s1",
				Then:  "the live test skips",
				Check: `go test ./internal/runtime -run TestLive -count=1`,
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{expectedSkipCheckMsg},
		},
		{
			name: "grepped_outside_sandbox_accepted",
			scenario0: response.Scenario{
				ID:    "s1",
				Then:  "the live test skips",
				Check: `go test ./internal/runtime -run TestLive -v -count=1 | grep -q -- '--- SKIP: TestLive'`,
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			name: "no_check_accepted",
			scenario0: response.Scenario{
				ID:    "s1",
				Then:  "skipped",
				Check: ``,
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			name: "no_longer_skips_flagged",
			scenario0: response.Scenario{
				ID:    "s1",
				Then:  "the test no longer skips",
				Check: okCheck,
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{expectedSkipCheckMsg},
		},
		{
			name: "sandbox_probe_bare_both_errors",
			scenario0: response.Scenario{
				ID:    "s1",
				Then:  "the probe skips",
				Check: `go test ./internal/sandbox -run TestX`,
			},
			wantPaths: []string{scenario0CheckPath, scenario0CheckPath},
			wantMsgs:  []string{hostSandboxCheckMsg, expectedSkipCheckMsg},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scenarios := []response.Scenario{
				tt.scenario0,
				{ID: "s2", Then: okThen, Check: okCheck},
			}
			errs := checkScenarioShape(scenarios)
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
