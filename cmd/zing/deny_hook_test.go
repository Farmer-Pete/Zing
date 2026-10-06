package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// denyFlag is runDenyHook's repeated --deny flag name, named once so the
// tests below don't repeat the literal past goconst's threshold.
const denyFlag = "--deny"

// denyHookStdin builds the JSON a Claude Code PreToolUse event for a Bash
// call sends on stdin.
func denyHookStdin(toolName, command string) *bytes.Reader {
	b, err := json.Marshal(struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}{
		ToolName: toolName,
		ToolInput: struct {
			Command string `json:"command"`
		}{Command: command},
	})
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(b)
}

// TestDenyHook_BlocksTimedFullSuite proves runDenyHook stops the two
// full-suite calls (runs 1672 and 1993) that timed out build jobs: both
// ended in a plain "go test ./..." behind "time" and a pipe to tail,
// which neither matches the project's configured make test nor would be
// caught by a naive prefix check on the raw command.
func TestDenyHook_BlocksTimedFullSuite(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := runDenyHook(
		[]string{denyFlag, liveTestCmd},
		denyHookStdin("Bash", "time go test ./... | tail -40"),
		&stderr,
	)
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), liveTestCmd) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), liveTestCmd)
	}
	if !strings.Contains(stderr.String(), "CHECK") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "CHECK")
	}
}

func TestDenyHook_AllowsTargetedTest(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := runDenyHook(
		[]string{denyFlag, liveTestCmd},
		denyHookStdin("Bash", "go test -run TestX ./x"),
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestDenyHook_AllowsNonBashTool(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := runDenyHook(
		[]string{denyFlag, liveTestCmd},
		denyHookStdin("Read", liveTestCmd),
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestDenyHook_MalformedStdinAllows(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := runDenyHook(
		[]string{denyFlag, liveTestCmd},
		strings.NewReader("not json"),
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "deny-hook:") {
		t.Errorf("stderr = %q, want it to start with %q", stderr.String(), "deny-hook:")
	}
}

func TestDenyHook_NoDenyFlagIsUsageError(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := runDenyHook(nil, denyHookStdin("Bash", liveTestCmd), &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stderr.String() != denyHookUsage+"\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), denyHookUsage+"\n")
	}
}
