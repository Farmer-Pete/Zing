// Package proc answers two questions about another process from the
// outside, from only its pid or process group id: which incarnation of
// that pid is this (StartToken), and is its process group still alive
// (GroupAlive, KillGroup). #45's dead-serve reclaim (design section 6.3)
// needs both to tell a live orphaned agent process from a dead one whose
// pid or process group id has since been reused by something unrelated.
package proc

import (
	"errors"
	"syscall"
)

// ErrUnsupported is StartToken's error on a platform with no
// implementation (design section 6.1): start_darwin.go and start_linux.go
// cover every platform this repository's CI builds for; start_other.go
// returns this on any other, so the package still builds everywhere.
var ErrUnsupported = errors.New("proc: start token unsupported on this platform")

// ErrNoProcess is StartToken's error when pid names no live process: the
// process already exited (and, on a system that reaps it, no longer has an
// entry to read), so it has no start time to report.
var ErrNoProcess = errors.New("proc: no such process")

// StartToken returns an opaque string that identifies pid's process
// incarnation (design section 6.1): two calls naming the same running
// process return equal tokens, and a pid later reused by an unrelated
// process returns a different one. Implemented per platform:
// start_darwin.go reads the kernel's own recorded process start time
// through sysctl; start_linux.go reads /proc/<pid>/stat's own starttime
// field; start_other.go returns ErrUnsupported.
func StartToken(pid int) (string, error) {
	return startToken(pid)
}

// GroupAlive reports whether any process is still in process group pgid
// (design section 6.1, 6.3): kill(-pgid, 0) sends no signal, it only checks
// for a receiver. ESRCH (no such process group) means the group is gone;
// nil (we may signal it) or EPERM (a process is there, just not ours to
// signal) both mean at least one process in it is still alive.
func GroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// KillGroup sends SIGKILL to every process in group pgid (design section
// 6.1, 6.3). ESRCH (the group is already gone) is not an error: a caller
// may retry KillGroup every pass until the group is actually gone, and that
// retry must not itself start failing once it succeeds.
func KillGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
