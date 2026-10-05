package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"zing/internal/runtime"
	"zing/internal/sandbox"
)

// sandboxedCommands is the real CommandRunner (design section 5.5): it runs
// /bin/sh -c <shellCmd>, wrapped in the same sandbox prefix and environment
// a build run itself gets, for the test and lint re-runs a build unit's
// CHECK step makes.
type sandboxedCommands struct {
	Sandbox        sandbox.Sandbox
	RequireSandbox bool
}

// NewCommandRunner returns the real CommandRunner, wrapping every command in
// sb when it is available; sb unavailable and requireSandbox true makes
// every call return ErrSandbox instead of running anything (design section
// 5.5). serve always passes requireSandbox true; selftest passes false with
// sandbox.Off().
func NewCommandRunner(sb sandbox.Sandbox, requireSandbox bool) CommandRunner {
	return sandboxedCommands{Sandbox: sb, RequireSandbox: requireSandbox}
}

func (c sandboxedCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio CommandIO) (int, error) {
	var execPrefix, env []string
	// Overwritten below with ParamsFor's own resolved worktree when the
	// sandbox is available, so the command runs in the same directory the
	// profile's WORKTREE rule names (task 16a: a dir reached through a
	// symlink otherwise fails every write with EPERM).
	workDir := dir

	if c.Sandbox.Available() {
		runDir, cleanup, err := c.Sandbox.NewRunDir()
		if err != nil {
			return -1, fmt.Errorf("job: command runner: sandbox run dir: %w", err)
		}
		defer cleanup()

		p, err := c.Sandbox.ParamsFor(dir, repoGit, runDir)
		if err != nil {
			return -1, fmt.Errorf("job: command runner: sandbox params: %w", err)
		}
		prefix, err := c.Sandbox.Prefix(p)
		if err != nil {
			return -1, fmt.Errorf("job: command runner: sandbox prefix: %w", err)
		}
		execPrefix = prefix
		env = c.Sandbox.Env(p, os.Getenv("PATH"))
		workDir = p.Worktree
	} else if c.RequireSandbox {
		return -1, ErrSandbox
	}

	return runShellCommand(ctx, workDir, shellCmd, execPrefix, env, timeout, cio)
}

// hostCommands is the CommandRunner for host-kind scenario checks at
// judging (design section 5): runShellCommand with no sandbox prefix, the
// filtered parent environment, and TMPDIR set to a fresh directory removed
// after the run.
type hostCommands struct{}

// NewHostCommandRunner returns the unsandboxed runner judging uses for
// host-kind scenario checks (Deps.HostCommands).
func NewHostCommandRunner() CommandRunner { return hostCommands{} }

func (hostCommands) Run(ctx context.Context, dir, _, shellCmd string, timeout time.Duration, cio CommandIO) (int, error) {
	tmp, err := os.MkdirTemp("", "zing-host-check-")
	if err != nil {
		return -1, fmt.Errorf("job: host command runner: temp dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			slog.Warn("host check temp dir removal failed", "dir", tmp, "error", rmErr)
		}
	}()
	return runShellCommand(ctx, dir, shellCmd, nil, []string{"TMPDIR=" + tmp}, timeout, cio)
}

// runShellCommand runs "/bin/sh -c shellCmd" (behind execPrefix, when set),
// in its own process group, the whole group killed after Wait returns on
// every path (design section 5.5, mirroring runtime.Claude.run's own
// process-group discipline), bounded by timeout. The environment is
// runtime.FilteredEnv(extraEnv) (review F051), not os.Environ() plus
// extraEnv: the plan's own section 5.5 says the command runner uses the
// same filtered environment an agent run does, so an inherited token or key
// in this process's own environment cannot reach agent-written test code.
// cio.Out, when set, is both stdout and stderr: the same writer value, so
// os/exec hands the child one pipe for fd 1 and fd 2 and the bytes arrive
// in the order the child wrote them (#55 plan D2). cio.OnStart, when set,
// runs once right after Start with the group leader's pid.
func runShellCommand(ctx context.Context, dir, shellCmd string, execPrefix, extraEnv []string, timeout time.Duration, cio CommandIO) (int, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := shellCommandNameArgs(execPrefix, shellCmd)
	cmd := exec.CommandContext(runCtx, name, args...) //nolint:gosec // G204: shellCmd is an operator-configured project command (config.Project.Commands.Test/Lint), never model-influenced argv; execPrefix is the sandbox's own prefix
	cmd.Dir = dir
	cmd.Env = runtime.FilteredEnv(extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	if cio.Out != nil {
		cmd.Stdout = cio.Out
		cmd.Stderr = cio.Out
	}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("job: command runner: start: %w", err)
	}
	if cio.OnStart != nil {
		cio.OnStart(cmd.Process.Pid)
	}

	waitErr := cmd.Wait()
	killCommandGroup(cmd)

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return -1, ErrCommandTimeout
	case ctx.Err() != nil:
		return -1, fmt.Errorf("job: command runner: %w", ctx.Err())
	case waitErr == nil:
		return 0, nil
	default:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) { //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags (matches internal/runtime's own comment)
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("job: command runner: wait: %w", waitErr)
	}
}

// shellCommandNameArgs mirrors runtime.Claude's own commandNameArgs (design
// section 4.4): with no execPrefix, "/bin/sh -c shellCmd" unchanged; with
// one, execPrefix[0] as name and execPrefix[1:] plus "/bin/sh -c shellCmd"
// as args.
func shellCommandNameArgs(execPrefix []string, shellCmd string) (name string, args []string) {
	shArgs := []string{"-c", shellCmd}
	if len(execPrefix) == 0 {
		return "/bin/sh", shArgs
	}
	args = make([]string, 0, len(execPrefix)-1+1+len(shArgs))
	args = append(args, execPrefix[1:]...)
	args = append(args, "/bin/sh")
	args = append(args, shArgs...)
	return execPrefix[0], args
}

// killCommandGroup sends SIGKILL to cmd's whole process group once Wait has
// already returned, on every path (design section 5.5), the same discipline
// runtime.Claude's own killProcessGroup applies to an agent run. ESRCH (no
// such process: the group is already gone) is not logged; any other failure
// is, since it means a descendant may still be running past the point the
// caller believes the command is done.
func killCommandGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("kill command process group", "pid", cmd.Process.Pid, "error", err)
	}
}

// checkOutputCap is how many trailing bytes of one CHECK command's output
// Zing keeps and sends back to the builder (#55 plan D2).
const checkOutputCap = 16 << 10

// tailBuffer is an io.Writer that keeps only the last limit bytes written
// to it, in a fixed ring of limit bytes, and counts every byte. It never
// holds more than limit bytes, even during one write far larger than
// limit. It locks, so it stays safe if the child's pipe copier outlives
// Wait (cmd.WaitDelay).
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte // the ring; len(buf) <= limit
	next  int    // where the next byte goes once buf is full
	total int64
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit, buf: make([]byte, 0, limit)}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	if len(p) > b.limit {
		p = p[len(p)-b.limit:]
	}
	if room := b.limit - len(b.buf); room > 0 {
		k := min(room, len(p))
		b.buf = append(b.buf, p[:k]...)
		p = p[k:]
	}
	for len(p) > 0 { // the ring is full: overwrite the oldest bytes
		k := copy(b.buf[b.next:], p)
		b.next = (b.next + k) % b.limit
		p = p[k:]
	}
	return n, nil
}

// Tail returns the last limit bytes written. When bytes were cut and the
// cut landed inside a UTF-8 rune, the rune's leftover continuation bytes
// (at most 3) are dropped; any other invalid UTF-8 becomes U+FFFD.
func (b *tailBuffer) Tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var sb strings.Builder
	sb.Grow(len(b.buf))
	sb.Write(b.buf[b.next:])
	sb.Write(b.buf[:b.next])
	s := sb.String()
	if b.total > int64(len(s)) {
		for i := 0; i < utf8.UTFMax-1 && s != "" && !utf8.RuneStart(s[0]); i++ {
			s = s[1:]
		}
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// Total is how many bytes were written, kept or not.
func (b *tailBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}
