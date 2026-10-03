//go:build darwin

package proc

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// startToken reads pid's process start time from the kernel through
// sysctl kern.proc.pid (design section 6.1): unix.SysctlKinfoProc returns a
// kinfo_proc struct whose ExternProc.P_starttime is the process's start
// time at microsecond resolution, an incarnation marker that is stable for
// the life of the process and differs across any two processes that ever
// held the same pid. The sysctl itself reports an undersized result for a
// pid with no live process, which SysctlKinfoProc turns into an error; that
// is ErrNoProcess here, whatever the underlying syscall error says.
// procStateZombie is SZOMB (<sys/proc.h>): a process that has exited but
// not yet been reaped by its parent (PR review fix A2). It still has a
// stable kinfo_proc entry, starttime included, but it is not alive -- a
// SIGKILL'd serve's own zombie, unreaped, must not block a stale lock's
// takeover forever, nor read as a live orphan for reclaim.
const procStateZombie = 5

func startToken(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("%w: pid %d: %v", ErrNoProcess, pid, err) //nolint:errorlint // ErrNoProcess is the sentinel this wraps; err's own type carries nothing a caller matches on
	}
	if kp.Proc.P_stat == procStateZombie {
		return "", fmt.Errorf("%w: pid %d: zombie", ErrNoProcess, pid) //nolint:errorlint // ErrNoProcess is the sentinel this wraps; err's own type carries nothing a caller matches on
	}
	return fmt.Sprintf("%d.%06d", kp.Proc.P_starttime.Sec, kp.Proc.P_starttime.Usec), nil
}
