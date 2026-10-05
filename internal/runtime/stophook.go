package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"zing/internal/response"
)

// maxStopBlocks caps how many times one run's Stop hook blocks the end
// of a turn; past it the turn ends and the runtime's own parse decides.
const maxStopBlocks = 3

// stopHookState is the hook's counters for one run, kept in a state file
// named for the Claude Code session id so parallel runs on a shared host
// TMPDIR do not collide.
type stopHookState struct {
	Events int `json:"events"`
	Blocks int `json:"blocks"`
	Unread int `json:"unread"`
}

// stopHookInput is the subset of a Claude Code Stop event's JSON stdin
// the hook reads. SessionID rides along for the record; no rule reads it.
type stopHookInput struct {
	SessionID            string  `json:"session_id"`
	LastAssistantMessage *string `json:"last_assistant_message"`
}

// stopHookOutput is the hook's stdout when it blocks the end of a turn.
type stopHookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// StopHook decides one Claude Code Stop event for job (rules in order:
// state, input, unread, valid, cap, block). out is the bytes for stdout:
// nil lets the turn end; a block decision keeps it going. err is for
// stderr only; it never turns into a block.
func StopHook(stdin io.Reader, job response.Job, statePath string) (out []byte, err error) {
	st, err := readStopHookState(statePath)
	if err != nil {
		return nil, err
	}
	st.Events++
	var in stopHookInput
	if derr := json.NewDecoder(stdin).Decode(&in); derr != nil {
		return nil, errors.Join(fmt.Errorf("runtime: stop hook: decode input: %w", derr), writeStopHookState(statePath, st))
	}
	if in.LastAssistantMessage == nil {
		st.Unread++
		return nil, writeStopHookState(statePath, st)
	}
	_, _, perr := parseFinalMessage(*in.LastAssistantMessage, job)
	if perr == nil || st.Blocks >= maxStopBlocks {
		return nil, writeStopHookState(statePath, st)
	}
	st.Blocks++
	if werr := writeStopHookState(statePath, st); werr != nil {
		return nil, werr
	}
	return json.Marshal(stopHookOutput{Decision: "block", Reason: stopHookReason(perr, st.Blocks)})
}

// readStopHookState reads the state file at path. A missing file gives the
// zero state and no error, so the first Stop event of a run needs nothing
// written in advance. A file that fails to decode gives the zero state and
// the decode error, so the hook never silently resets a run's counters.
func readStopHookState(path string) (stopHookState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return stopHookState{}, nil
	}
	if err != nil {
		return stopHookState{}, err
	}
	var st stopHookState
	if err := json.Unmarshal(b, &st); err != nil {
		return stopHookState{}, err
	}
	return st, nil
}

// writeStopHookState writes st to path by writing a temp file and renaming
// it over path, so a reader never sees a partially written state file.
func writeStopHookState(path string, st stopHookState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stopHookReason renders the Stop hook's block reason: which block out of
// maxStopBlocks this is, the closed Reason (and Detail, when present) from
// err, and the instruction to fix and end the turn again.
func stopHookReason(err error, block int) string {
	reason, detail := err.Error(), ""
	if ioe, ok := errors.AsType[*InvalidOutputError](err); ok {
		reason, detail = ioe.Reason, ioe.Detail
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Zing could not accept your final message (block %d of %d): %s.\n", block, maxStopBlocks, reason)
	if detail != "" {
		b.WriteString(detail + "\n")
	}
	b.WriteString("Fix every error and end your turn again with the whole corrected zing document as your final message.")
	return b.String()
}
