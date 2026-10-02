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
func startToken(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("%w: pid %d: %v", ErrNoProcess, pid, err) //nolint:errorlint // ErrNoProcess is the sentinel this wraps; err's own type carries nothing a caller matches on
	}
	return fmt.Sprintf("%d.%06d", kp.Proc.P_starttime.Sec, kp.Proc.P_starttime.Usec), nil
}
