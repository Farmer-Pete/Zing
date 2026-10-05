package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/runtime"
)

// fakeClaudeStopHookScript stands in for the real claude binary: it reads
// its Stop hook out of its own --settings argument and runs it against a
// sequence of fixtures (cmd/zing/testdata/fake_claude_stop_hook.sh).
const fakeClaudeStopHookScript = "testdata/fake_claude_stop_hook.sh"

// stopHookFixtureInput is the Claude Code Stop event JSON the fake claude
// feeds the real hook on stdin.
type stopHookFixtureInput struct {
	SessionID            string `json:"session_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestStopHook_FakeClaudeBlocksThenAccepts drives the real `zing validate
// --hook` path -- this very test binary, invoked under ZING_TEST_MAIN=1 --
// from a fake claude CLI that extracts its Stop hook straight out of its
// own --settings argument, exactly the shape Claude.Command builds. The
// first turn's final message fails validation (a misspelled closing tag):
// the hook blocks it. The second turn's final message is valid: the hook
// lets it through, and the fake claude sends it on as the CLI's own
// result, all inside this one Run call, with no retry run.
func TestStopHook_FakeClaudeBlocksThenAccepts(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("fake_claude_stop_hook.sh requires a POSIX shell; skipped on windows")
	}

	dir := t.TempDir()
	tmpDir := t.TempDir()

	const brokenDoc = `<zing job="classify" outcome="bug"><reason>broken</reasn></zing>`
	const validDoc = `<zing job="classify" outcome="bug"><reason>ok</reason></zing>`

	writeJSONFile(t, filepath.Join(dir, "hook_in_1.json"), stopHookFixtureInput{SessionID: "s", LastAssistantMessage: brokenDoc})
	writeJSONFile(t, filepath.Join(dir, "hook_in_2.json"), stopHookFixtureInput{SessionID: "s", LastAssistantMessage: validDoc})

	writeJSONFile(t, filepath.Join(dir, "result_2.json"), map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": validDoc, "session_id": "unused", "num_turns": 1, "duration_ms": 1,
	})

	zingBin := os.Args[0] // this test binary, running under ZING_TEST_MAIN=1
	req := runtime.RunRequest{
		Job:    response.JobClassify,
		Model:  "test-model",
		Prompt: "the assembled prompt",
		Env: []string{
			"FAKE_CLAUDE_DIR=" + dir,
			"TMPDIR=" + tmpDir,
			"ZING_TEST_MAIN=1",
		},
	}
	c := runtime.NewClaude(fakeClaudeStopHookScript, "test-oauth-token").WithStopHook(zingBin)

	res, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	hookOut1, rerr := os.ReadFile(filepath.Join(dir, "hook_out_1"))
	if rerr != nil {
		t.Fatalf("read hook_out_1: %v", rerr)
	}
	if !strings.Contains(string(hookOut1), `"decision":"block"`) {
		t.Errorf("hook_out_1 = %q, want it to contain %q", hookOut1, `"decision":"block"`)
	}
	if !strings.Contains(string(hookOut1), "no zing element") {
		t.Errorf("hook_out_1 = %q, want it to contain %q", hookOut1, "no zing element")
	}

	hookOut2, rerr := os.ReadFile(filepath.Join(dir, "hook_out_2"))
	if rerr != nil {
		t.Fatalf("read hook_out_2: %v", rerr)
	}
	if len(hookOut2) != 0 {
		t.Errorf("hook_out_2 = %q, want empty", hookOut2)
	}

	cr, ok := res.Response.(*response.ClassifyResponse)
	if !ok {
		t.Fatalf("Response type = %T, want *ClassifyResponse", res.Response)
	}
	if cr.Reason != "ok" {
		t.Errorf("Reason = %q, want %q", cr.Reason, "ok")
	}

	if res.StopHookEvents != 2 {
		t.Errorf("StopHookEvents = %d, want 2", res.StopHookEvents)
	}
	if res.StopHookBlocks != 1 {
		t.Errorf("StopHookBlocks = %d, want 1", res.StopHookBlocks)
	}

	statePath := filepath.Join(tmpDir, "zing-stop-hook-"+res.SessionID+".json")
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("state file %s still exists after Run, want it removed", statePath)
	}
}
