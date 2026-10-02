package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"zing/internal/proc"
)

// serveLockFilename and serveLockGuardFilename are the two files a data
// directory carries for #45's one-serve-per-data-directory rule (design D7,
// section 6.2): serveLockFilename is the lock itself, proving which process
// is the live "zing serve" against this data directory; serveLockGuardFilename
// is never removed and exists only to serialize starters through an OS
// flock, so two processes starting at once can never interleave the
// stale-takeover steps below.
const (
	serveLockFilename      = "serve.lock"
	serveLockGuardFilename = "serve.lock.guard"
)

// serveLockMaxAttempts bounds acquireServeLockAs's own retry loop (design
// section 6.2): one plain attempt, and one retry after removing a lock
// proven stale. A second EEXIST after that retry cannot happen under the
// guard's flock unless something outside Zing created the file.
const serveLockMaxAttempts = 2

// serveLock holds a lock file proving this process is the one live "zing
// serve" against a data directory (design D7, section 6.2). Only serve
// takes one; selftest uses its own temp directory, and validate, scenarios,
// project add, and version never touch the data directory at all.
type serveLock struct {
	path  string
	pid   int
	token string
}

// acquireServeLock acquires <dataDir>/serve.lock for this process (design
// section 6.2), right after config.Load and host resolution and before
// store.Open, so a second serve against the same data directory never opens
// the store or runs the startup sweeps.
func acquireServeLock(dataDir string) (*serveLock, error) {
	pid := os.Getpid()
	token, err := proc.StartToken(pid)
	if err != nil {
		// ErrUnsupported (no platform implementation) is the only error
		// expected for this process's own, definitely-alive pid; whatever
		// the cause, an empty token falls back to the plain PID-liveness
		// check every reader of this lock already has (serveLockHolderAlive
		// below, and reclaimForeign's own classifyOpenRun).
		token = ""
	}
	return acquireServeLockAs(dataDir, pid, token)
}

// acquireServeLockAs is acquireServeLock's own body, parameterized on pid
// and token so a test can acquire as two distinct fake identities without
// forking a real process (design section 10 item 4).
func acquireServeLockAs(dataDir string, pid int, token string) (*serveLock, error) {
	guard, err := lockServeLockGuard(dataDir)
	if err != nil {
		return nil, err
	}
	defer guard.release()

	path := filepath.Join(dataDir, serveLockFilename)
	for range serveLockMaxAttempts {
		linked, err := linkServeLock(dataDir, path, pid, token)
		if err != nil {
			return nil, err
		}
		if linked {
			return &serveLock{path: path, pid: pid, token: token}, nil
		}

		// EEXIST: read and parse the existing file. A file that fails to
		// parse is stale -- it cannot be a half-written lock, because
		// linkServeLock only ever makes path visible through os.Link from
		// a fully-written temp file, which either fails outright or
		// creates path with its complete content in one step.
		holderPID, holderToken, parseErr := readServeLock(path)
		if parseErr == nil && serveLockHolderAlive(holderPID, holderToken) {
			return nil, fmt.Errorf("serve: another zing serve is running (pid %d); stop it before starting a new one", holderPID)
		}

		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return nil, fmt.Errorf("serve: remove stale lock %s: %w", path, rmErr)
		}
		slog.Warn("stale serve lock taken over", "old_pid", holderPID)
	}
	return nil, fmt.Errorf("serve: could not take %s", path)
}

// release removes the lock file only if it still parses with our own pid
// and start token (design section 6.2): a lock this process no longer owns
// must never be removed out from under whoever owns it now.
func (l *serveLock) release() {
	pid, token, err := readServeLock(l.path)
	if err != nil || pid != l.pid || token != l.token {
		return
	}
	if rmErr := os.Remove(l.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		slog.Error("release serve lock failed", "path", l.path, "err", rmErr)
	}
}

// serveLockGuard holds the OS-level flock that serializes every starter
// against one data directory (design section 6.2): while it is held, no
// other starter can read, remove, or link serve.lock, so the stale-takeover
// steps in acquireServeLockAs can never interleave across processes. A
// running serve does not hold this flock; only serve.lock itself marks a
// running serve. The kernel drops the flock if the holding process dies, so
// the guard file is never removed.
type serveLockGuard struct {
	f *os.File
}

func lockServeLockGuard(dataDir string) (*serveLockGuard, error) {
	path := filepath.Join(dataDir, serveLockGuardFilename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("serve: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("serve: lock %s: %w", path, err)
	}
	return &serveLockGuard{f: f}, nil
}

func (g *serveLockGuard) release() {
	if err := syscall.Flock(int(g.f.Fd()), syscall.LOCK_UN); err != nil {
		slog.Warn("unlock serve.lock.guard failed", "err", err)
	}
	_ = g.f.Close()
}

// linkServeLock writes path's complete content to a per-pid temp file,
// fsyncs and closes it, then links it into path (design section 6.2): a
// hard link either fails (most commonly EEXIST, when path already names a
// file) or creates path with the complete content in one step, so path is
// never visible half-written. The temp file is removed after the link
// attempt either way. linked is false, err nil, only on EEXIST; any other
// failure is returned as a real error.
func linkServeLock(dataDir, path string, pid int, token string) (linked bool, err error) {
	tmp := filepath.Join(dataDir, fmt.Sprintf("%s.%d.tmp", serveLockFilename, pid))
	content := fmt.Sprintf("pid=%d\nstart=%s\n", pid, token)

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false, fmt.Errorf("serve: write %s: %w", tmp, err)
	}
	if _, writeErr := f.WriteString(content); writeErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return false, fmt.Errorf("serve: write %s: %w", tmp, writeErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return false, fmt.Errorf("serve: sync %s: %w", tmp, syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("serve: close %s: %w", tmp, closeErr)
	}

	linkErr := os.Link(tmp, path)
	_ = os.Remove(tmp)
	switch {
	case linkErr == nil:
		return true, nil
	case errors.Is(linkErr, os.ErrExist):
		return false, nil
	default:
		return false, fmt.Errorf("serve: link %s: %w", path, linkErr)
	}
}

// readServeLock reads path in one os.ReadFile call and parses it (design
// section 6.2). Split from parseServeLock below so a caller that already
// has the bytes from its own single read -- a test proving the file is
// never visible half-written included -- parses that exact read instead of
// racing a second one against a concurrent writer.
func readServeLock(path string) (pid int, token string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", fmt.Errorf("read %s: %w", path, err)
	}
	return parseServeLock(data)
}

// parseServeLock parses a serve.lock file's two-line content
// ("pid=<decimal>\nstart=<token>\n", token possibly empty). Any other shape
// is reported as an error: a lock file linkServeLock wrote is always
// exactly this shape, so bytes that fail to parse are stale by
// construction, never a writer still in progress (linkServeLock only ever
// makes path visible through os.Link from a fully-written, fsynced temp
// file).
func parseServeLock(data []byte) (pid int, token string, err error) {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		return 0, "", fmt.Errorf("serve.lock: want 2 lines, got %d", len(lines))
	}
	pidStr, ok := strings.CutPrefix(lines[0], "pid=")
	if !ok {
		return 0, "", fmt.Errorf("serve.lock: first line %q missing pid= prefix", lines[0])
	}
	pid, err = strconv.Atoi(pidStr)
	if err != nil {
		return 0, "", fmt.Errorf("serve.lock: parse pid %q: %w", pidStr, err)
	}
	token, ok = strings.CutPrefix(lines[1], "start=")
	if !ok {
		return 0, "", fmt.Errorf("serve.lock: second line %q missing start= prefix", lines[1])
	}
	return pid, token, nil
}

// serveLockHolderAlive reports whether the recorded (pid, token) names a
// live process (design section 6.2 step 3): when token is set, only an
// exact proc.StartToken match counts (a pid whose process exited, even if
// later reused, must not be mistaken for the same incarnation); when token
// is empty (an unsupported platform, or a start-time read failure), it
// falls back to a plain signal-0 liveness check.
func serveLockHolderAlive(pid int, token string) bool {
	if token != "" {
		got, err := proc.StartToken(pid)
		return err == nil && got == token
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
