//go:build linux

package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// starttimeFieldsAfterComm is how many whitespace-separated fields lie
// between /proc/<pid>/stat's state field (field 3, the first field after
// the comm field's closing paren) and its starttime field (field 22):
// 22 - 3 = 19, so starttime is index 19 once the fields are split starting
// from state.
const starttimeFieldsAfterComm = 19

// startToken reads pid's process start time from /proc/<pid>/stat (design
// section 6.1): field 22, starttime, in clock ticks since boot -- an
// incarnation marker the kernel never reuses for a pid's later process
// while it can still answer at all. The comm field (field 2) is
// parenthesized and may itself contain spaces or parens, so this finds the
// stat line's *last* ')' and splits only what follows it, rather than
// splitting the whole line on whitespace and risking a shifted field index.
// A missing file (the process already exited, and the kernel no longer has
// an entry for it) wraps ErrNoProcess.
func startToken(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", fmt.Errorf("%w: pid %d: %v", ErrNoProcess, pid, err) //nolint:errorlint // ErrNoProcess is the sentinel this wraps; err's own type carries nothing a caller matches on
	}

	line := string(data)
	closeParen := strings.LastIndexByte(line, ')')
	if closeParen < 0 || closeParen+2 > len(line) {
		return "", fmt.Errorf("proc: start token: pid %d: malformed /proc/%d/stat", pid, pid)
	}

	fields := strings.Fields(line[closeParen+2:])
	if len(fields) == 0 {
		return "", fmt.Errorf("proc: start token: pid %d: malformed /proc/%d/stat", pid, pid)
	}
	// fields[0] is the state field (field 3): "Z" is a zombie, a process
	// already exited but not yet reaped by its parent (PR review fix A2).
	// It still has a stable starttime, but it is not alive -- a SIGKILL'd
	// serve's own zombie, unreaped, must not block a stale lock's takeover
	// forever, nor read as a live orphan for reclaim.
	if fields[0] == "Z" {
		return "", fmt.Errorf("%w: pid %d: zombie", ErrNoProcess, pid) //nolint:errorlint // ErrNoProcess is the sentinel this wraps; err's own type carries nothing a caller matches on
	}
	if len(fields) <= starttimeFieldsAfterComm {
		return "", fmt.Errorf("proc: start token: pid %d: too few fields in /proc/%d/stat", pid, pid)
	}

	starttime := fields[starttimeFieldsAfterComm]
	if _, err := strconv.ParseUint(starttime, 10, 64); err != nil {
		return "", fmt.Errorf("proc: start token: pid %d: starttime field %q: %w", pid, starttime, err)
	}
	return starttime, nil
}
