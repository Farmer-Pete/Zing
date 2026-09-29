package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

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

func (c sandboxedCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration) (int, error) {
	var execPrefix, env []string

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
	} else if c.RequireSandbox {
		return -1, ErrSandbox
	}

	return runShellCommand(ctx, dir, shellCmd, execPrefix, env, timeout)
}

// runShellCommand runs "/bin/sh -c shellCmd" (behind execPrefix, when set),
// in its own process group, the whole group killed after Wait returns on
// every path (design section 5.5, mirroring runtime.Claude.run's own
// process-group discipline), bounded by timeout.
func runShellCommand(ctx context.Context, dir, shellCmd string, execPrefix, extraEnv []string, timeout time.Duration) (int, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := shellCommandNameArgs(execPrefix, shellCmd)
	cmd := exec.CommandContext(runCtx, name, args...) //nolint:gosec // G204: shellCmd is an operator-configured project command (config.Project.Commands.Test/Lint), never model-influenced argv; execPrefix is the sandbox's own prefix
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("job: command runner: start: %w", err)
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
