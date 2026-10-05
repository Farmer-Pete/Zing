package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/response"
)

// stopHookStdin builds the JSON a Claude Code Stop event sends on stdin.
// msg == nil omits last_assistant_message entirely, so StopHook sees it
// as unread.
func stopHookStdin(t *testing.T, msg *string) *strings.Reader {
	t.Helper()
	in := stopHookInput{SessionID: "s", LastAssistantMessage: msg}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal stop hook input: %v", err)
	}
	return strings.NewReader(string(b))
}

// readState reads back the state file as a stopHookState, failing the test
// on any error so callers can assert on the fields directly.
func readState(t *testing.T, path string) stopHookState {
	t.Helper()
	st, err := readStopHookState(path)
	if err != nil {
		t.Fatalf("readStopHookState(%q): %v", path, err)
	}
	return st
}

func TestStopHook_BlocksInvalidUntilCap(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "zing-stop-hook-s.json")
	text := `<zing job="classify" outcome="bug"><reason></reason></zing>`

	for i := 1; i <= maxStopBlocks; i++ {
		out, err := StopHook(stopHookStdin(t, &text), response.JobClassify, statePath)
		if err != nil {
			t.Fatalf("call %d: StopHook error = %v", i, err)
		}
		var decoded stopHookOutput
		if jerr := json.Unmarshal(out, &decoded); jerr != nil {
			t.Fatalf("call %d: out = %q, not valid JSON: %v", i, out, jerr)
		}
		if decoded.Decision != "block" {
			t.Errorf("call %d: Decision = %q, want block", i, decoded.Decision)
		}
		wantBlockPhrase := fmt.Sprintf("block %d of %d", i, maxStopBlocks)
		if !strings.Contains(decoded.Reason, wantBlockPhrase) {
			t.Errorf("call %d: Reason = %q, want it to contain %q", i, decoded.Reason, wantBlockPhrase)
		}
		if !strings.Contains(decoded.Reason, "zing document failed validation") {
			t.Errorf("call %d: Reason = %q, want it to contain %q", i, decoded.Reason, "zing document failed validation")
		}
		if !strings.Contains(decoded.Reason, "reason") {
			t.Errorf("call %d: Reason = %q, want it to contain the failing field name %q", i, decoded.Reason, "reason")
		}
	}

	out, err := StopHook(stopHookStdin(t, &text), response.JobClassify, statePath)
	if err != nil {
		t.Fatalf("call %d: StopHook error = %v", maxStopBlocks+1, err)
	}
	if out != nil {
		t.Errorf("call %d: out = %q, want nil once the cap is reached", maxStopBlocks+1, out)
	}

	st := readState(t, statePath)
	want := stopHookState{Events: maxStopBlocks + 1, Blocks: maxStopBlocks, Unread: 0}
	if st != want {
		t.Errorf("state = %+v, want %+v", st, want)
	}
}

func TestStopHook_AllowsValidDocument(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "zing-stop-hook-s.json")
	text := `<zing job="classify" outcome="bug"><reason>it crashes</reason></zing>`

	out, err := StopHook(stopHookStdin(t, &text), response.JobClassify, statePath)
	if err != nil {
		t.Fatalf("StopHook error = %v", err)
	}
	if out != nil {
		t.Errorf("out = %q, want nil for a valid document", out)
	}

	st := readState(t, statePath)
	want := stopHookState{Events: 1, Blocks: 0, Unread: 0}
	if st != want {
		t.Errorf("state = %+v, want %+v", st, want)
	}
}

func TestStopHook_UnreadWithoutLastMessage(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "zing-stop-hook-s.json")

	out, err := StopHook(stopHookStdin(t, nil), response.JobClassify, statePath)
	if err != nil {
		t.Fatalf("StopHook error = %v", err)
	}
	if out != nil {
		t.Errorf("out = %q, want nil when last_assistant_message is absent", out)
	}

	st := readState(t, statePath)
	want := stopHookState{Events: 1, Blocks: 0, Unread: 1}
	if st != want {
		t.Errorf("state = %+v, want %+v", st, want)
	}
}

// TestStopHook_BadInputAllows proves hook rule 3: stdin that is not JSON
// still counts the event and writes the state, but returns no output and
// the decode error, so a malformed Stop event never blocks the turn.
func TestStopHook_BadInputAllows(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "zing-stop-hook-s.json")

	out, err := StopHook(strings.NewReader("not json"), response.JobClassify, statePath)
	if err == nil {
		t.Fatal("StopHook error = nil, want a decode error for non-JSON stdin")
	}
	if out != nil {
		t.Errorf("out = %q, want nil for non-JSON stdin", out)
	}

	st := readState(t, statePath)
	want := stopHookState{Events: 1, Blocks: 0, Unread: 0}
	if st != want {
		t.Errorf("state = %+v, want %+v", st, want)
	}
}

func TestStopHook_CorruptStateAllows(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "zing-stop-hook-s.json")
	if err := os.WriteFile(statePath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("seed corrupt state file: %v", err)
	}
	text := `<zing job="classify" outcome="bug"><reason>it crashes</reason></zing>`

	out, err := StopHook(stopHookStdin(t, &text), response.JobClassify, statePath)
	if err == nil {
		t.Fatal("StopHook error = nil, want an error for a corrupt state file")
	}
	if out != nil {
		t.Errorf("out = %q, want nil when the state file is corrupt", out)
	}

	got, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatalf("re-read state file: %v", rerr)
	}
	if string(got) != "not-json" {
		t.Errorf("state file = %q, want it left unchanged", got)
	}
}
