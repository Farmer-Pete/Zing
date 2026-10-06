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
// false when none matches. Both command and every deny entry are
// normalized the same way (normalizeSegment): whitespace collapsed to
// single spaces, and a leading "time" and VAR=value assignments stripped
// -- so an entry like "CGO_ENABLED=0 go test ./..." also blocks a plain
// "go test ./..." call, and "time go test ./... | tail -40" is still
// caught even though neither its own leading "time" nor a deny entry's
// own env prefix survives normalizing. A normalized entry with no shell
// operator is compared against each normalized segment of command (split
// on &&, ||, ;, |, and newline, each normalized on its own): a segment
// matches the entry on equality, or on a prefix followed by a space, so a
// targeted call like "go test -run TestX ./a" is not denied by the entry
// "go test ./...". A normalized entry that still holds a shell operator,
// such as "cd web && npm test", is never split; it is compared whole,
// against command's own normalization (not split into segments), by
// equality only: a targeted call through the same operator, such as "cd
// web && npm test -- foo.spec", stays allowed.
func DeniedCommand(command string, deny []string) (entry string, ok bool) {
	normCommand := NormalizeCommand(command)

	var normSegments []string
	for seg := range strings.SplitSeq(segmentSplitter.Replace(command), "\n") {
		if norm := NormalizeCommand(seg); norm != "" {
			normSegments = append(normSegments, norm)
		}
	}

	for _, d := range deny {
		normEntry := NormalizeCommand(d)
		if normEntry == "" {
			continue
		}
		if strings.ContainsAny(normEntry, "&|;") {
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

// NormalizeCommand strips a leading "time" and any leading VAR=value
// fields from seg, then joins what remains with single spaces. It never
// splits seg on a shell operator; a mid-string operator such as the "&&"
// in "cd web && npm test" survives as its own field. DeniedCommand applies
// it to every command segment and deny entry; Project.DenyCommands
// (internal/job/job.go) applies it to a project's own configured commands,
// so a configured command and a deny entry that differ only by one of
// these prefixes are recognized as the same duplicate.
func NormalizeCommand(seg string) string {
	fields := strings.Fields(seg)
	for len(fields) > 0 && (fields[0] == "time" || envAssignment.MatchString(fields[0])) {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

// segmentSplitter splits a command on &&, ||, ;, |, and newline. The
// two-character operators are tried first, so "||" is not split into two
// empty segments around a stray "|".
var segmentSplitter = strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n")

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
