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

// denyHookInput is the subset of a Claude Code PreToolUse event the hook
// reads.
type denyHookInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// denyPiece is one normalized, matchable piece of a deny entry, kept next
// to the original entry text so a match can report what the owner wrote
// even when the entry itself held several segments or a leading env
// assignment.
type denyPiece struct {
	normalized string
	entry      string
}

// DeniedCommand reports the first deny entry that a segment of command
// matches, after stripping a leading time and VAR=value assignments from
// each segment; ok is false when none matches. Each deny entry is split
// and stripped the same way, so an entry that is itself a compound command
// (such as "go vet ./... && go test ./...") or starts with an env
// assignment (such as "CGO_ENABLED=0 go test ./...") still matches the
// bare segment a build run tries to run.
func DeniedCommand(command string, deny []string) (entry string, ok bool) {
	var pieces []denyPiece
	for _, d := range deny {
		for _, seg := range splitCommandSegments(d) {
			if norm := normalizeSegment(seg); norm != "" {
				pieces = append(pieces, denyPiece{normalized: norm, entry: d})
			}
		}
	}
	for _, seg := range splitCommandSegments(command) {
		normSeg := normalizeSegment(seg)
		if normSeg == "" {
			continue
		}
		for _, p := range pieces {
			if normSeg == p.normalized || strings.HasPrefix(normSeg, p.normalized+" ") {
				return p.entry, true
			}
		}
	}
	return "", false
}

// strippablePrefix reports whether field is a leading "time" or a
// VAR=value assignment, the parts normalizeSegment drops before a segment
// or deny entry is compared.
func strippablePrefix(field string) bool {
	return field == "time" || envAssignment.MatchString(field)
}

// normalizeSegment strips any leading time and VAR=value fields from seg,
// then joins what remains with single spaces.
func normalizeSegment(seg string) string {
	fields := strings.Fields(seg)
	for len(fields) > 0 && strippablePrefix(fields[0]) {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

// commandOperator reports the two-character shell operator command starts
// with at i, or "" when there is none; checked before the single-character
// operators so "||" is not split into two empty segments around a stray
// "|".
func commandOperator(command string, i int) string {
	if i+1 >= len(command) {
		return ""
	}
	switch pair := command[i : i+2]; pair {
	case "&&", "||":
		return pair
	default:
		return ""
	}
}

// splitCommandSegments splits command on &&, ||, ;, |, and newline.
func splitCommandSegments(command string) []string {
	var segments []string
	var cur strings.Builder
	for i := 0; i < len(command); {
		if op := commandOperator(command, i); op != "" {
			segments = append(segments, cur.String())
			cur.Reset()
			i += len(op)
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
	if in.ToolName != claudeBashTool {
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
