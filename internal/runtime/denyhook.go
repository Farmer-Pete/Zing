package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// envAssignment matches a leading VAR=value assignment, stripped from a
// command segment the same way a leading "time" is, before matching.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// bashToolCheck is Claude Code's tool name for a Bash call, named so this
// file's own comparison doesn't add a third raw "Bash" literal next to
// claude.go's two (tasks 2, 7, out of scope here).
const bashToolCheck = "Bash"

// denyHookInput is the subset of a Claude Code PreToolUse event the hook
// reads.
type denyHookInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// DeniedCommand reports the first deny entry that a segment of command
// matches, after stripping a leading time and VAR=value assignments from
// each segment; ok is false when none matches.
func DeniedCommand(command string, deny []string) (entry string, ok bool) {
	normDeny := make([]string, len(deny))
	for i, d := range deny {
		normDeny[i] = normalizeFields(strings.Fields(d))
	}
	for _, seg := range splitCommandSegments(command) {
		fields := strings.Fields(seg)
		for len(fields) > 0 && (fields[0] == "time" || envAssignment.MatchString(fields[0])) {
			fields = fields[1:]
		}
		normSeg := normalizeFields(fields)
		if normSeg == "" {
			continue
		}
		for i, d := range normDeny {
			if d == "" {
				continue
			}
			if normSeg == d || strings.HasPrefix(normSeg, d+" ") {
				return deny[i], true
			}
		}
	}
	return "", false
}

// normalizeFields joins fields with single spaces.
func normalizeFields(fields []string) string {
	return strings.Join(fields, " ")
}

// splitCommandSegments splits command on &&, ||, ;, |, and newline, the
// two-character operators checked before the single-character ones so
// "||" is not split into two empty segments around a stray "|".
func splitCommandSegments(command string) []string {
	var segments []string
	var cur strings.Builder
	for i := 0; i < len(command); {
		if i+1 < len(command) && (command[i:i+2] == "&&" || command[i:i+2] == "||") {
			segments = append(segments, cur.String())
			cur.Reset()
			i += 2
			continue
		}
		switch c := command[i]; c {
		case ';', '|', '\n':
			segments = append(segments, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
		i++
	}
	segments = append(segments, cur.String())
	return segments
}

// DenyHook decides one PreToolUse event. reason is non-empty when the Bash
// command matches deny; err is a decode failure, which never blocks.
func DenyHook(stdin io.Reader, deny []string) (reason string, err error) {
	var in denyHookInput
	if derr := json.NewDecoder(stdin).Decode(&in); derr != nil {
		return "", fmt.Errorf("runtime: deny hook: decode input: %w", derr)
	}
	if in.ToolName != bashToolCheck {
		return "", nil
	}
	entry, ok := DeniedCommand(in.ToolInput.Command, deny)
	if !ok {
		return "", nil
	}
	return fmt.Sprintf(
		"zing: this command matches %q, which Zing denies in build runs. CHECK runs the project's full test and lint commands after you return; run the tests your change touches by name instead.",
		entry,
	), nil
}
