//go:build linux

package proc

import (
	"errors"
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
// section 6.1): field 22, starttime, in clock ticks since boot, prefixed
// with the current boot's own btime (PR review fix B2) so the token also
// differs across a reboot that reuses the pid -- starttime alone is tick-
// resolution and resets every boot, so a pid reused after a reboot could
// otherwise land on the same tick value a stale recording named. The comm
// field (field 2) is parenthesized and may itself contain spaces or
// parens, so this finds the stat line's *last* ')' and splits only what
// follows it, rather than splitting the whole line on whitespace and
// risking a shifted field index. A missing file (the process already
// exited, and the kernel no longer has an entry for it) wraps
// ErrNoProcess.
func startToken(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// Only a missing entry means the process is gone (PR review fix
		// B3): a permission or I/O failure reading an otherwise-live
		// process's own /proc/<pid>/stat must not be mistaken for
		// ErrNoProcess, or a caller (serveLockHolderAlive, reclaim's own
		// classifyOpenRun) would treat an unreadable live process as
		// exited.
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("proc: start token: pid %d: read /proc/%d/stat: %w", pid, pid, err)
		}
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
	if _, perr := strconv.ParseUint(starttime, 10, 64); perr != nil {
		return "", fmt.Errorf("proc: start token: pid %d: starttime field %q: %w", pid, starttime, perr)
	}

	// starttime alone is ticks since boot, so it resets every reboot: a pid
	// reused after a reboot can land on the same tick value a stale
	// recording named, which would make a dead serve's lock, or a dead
	// orphan's process group, read as alive (PR review fix B2). Prefixing
	// the current boot's own btime (seconds since the epoch, from
	// /proc/stat, which does not reset within one boot) makes the token
	// differ across any two boots even when the tick value coincides.
	btime, err := bootTime()
	if err != nil {
		return "", fmt.Errorf("proc: start token: pid %d: %w", pid, err)
	}
	return fmt.Sprintf("%d:%s", btime, starttime), nil
}

// bootTime reads /proc/stat's own "btime" line: the current boot's start
// time, in whole seconds since the epoch, stable for the life of the boot.
func bootTime() (uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	for line := range strings.Lines(string(data)) {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		btime, perr := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("parse btime %q: %w", rest, perr)
		}
		return btime, nil
	}
	return 0, errors.New("no btime line in /proc/stat")
}
