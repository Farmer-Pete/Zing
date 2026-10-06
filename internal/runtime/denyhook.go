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

// DeniedCommand reports the first deny entry that command matches; ok is
// false when none matches. Each deny entry is normalized to single spaces
// but never split or stripped itself. An entry with no shell operator is
// compared against each segment of command (split on &&, ||, ;, |, and
// newline, each stripped of a leading time and VAR=value and
// whitespace-collapsed): a segment matches the entry on equality, or on a
// prefix followed by a space, so a targeted call like "go test -run TestX
// ./a" is not denied by the entry "go test ./...". An entry that still
// holds a shell operator after normalizing, such as "cd web && npm test",
// is compared whole, against command's own normalized whitespace, by
// equality only: a targeted call through the same operator, such as "cd
// web && npm test -- foo.spec", stays allowed.
func DeniedCommand(command string, deny []string) (entry string, ok bool) {
	normCommand := strings.Join(strings.Fields(command), " ")

	var normSegments []string
	for _, seg := range splitCommandSegments(command) {
		if norm := normalizeSegment(seg); norm != "" {
			normSegments = append(normSegments, norm)
		}
	}

	for _, d := range deny {
		normEntry := strings.Join(strings.Fields(d), " ")
		if normEntry == "" {
			continue
		}
		if strings.ContainsAny(normEntry, "&|;\n") {
			if normCommand == normEntry {
				return d, true
			}
			continue
		}
		for _, normSeg := range normSegments {
			if normSeg == normEntry || strings.HasPrefix(normSeg, normEntry+" ") {
				return d, true
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

// segmentSplitter splits a command on &&, ||, ;, |, and newline. The
// two-character operators are tried first, so "||" is not split into two
// empty segments around a stray "|".
var segmentSplitter = strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n")

// splitCommandSegments splits command on &&, ||, ;, |, and newline.
func splitCommandSegments(command string) []string {
	return strings.Split(segmentSplitter.Replace(command), "\n")
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
