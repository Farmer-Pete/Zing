// Package orchestrator drives every git operation and GitHub write Zing
// performs on behalf of a ticket: one worktree per ticket, one signed commit
// per task, a push to the ticket branch, and a draft pull request. Agents
// never run git; the orchestrator does (PKG5-PLAN.md sections 2, 11-13).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// Project is the git and GitHub identity of one repository the orchestrator
// serves. It is derived from a store.Project and the config, not read from
// the store here.
type Project struct {
	Owner         string // "Farmer-Pete"; non-empty
	Repo          string // "Zing"; non-empty
	LocalPath     string // absolute path to the main checkout; must be absolute
	DefaultBranch string // "main"; non-empty
}

// Runner runs an external command in a working directory. Run returns
// combined stdout and stderr, for commands whose output is only for a log or
// an error. Output returns stdout alone, for commands whose stdout is parsed
// (rev-parse, the %G? checks, status). execRunner is the real implementation;
// the git tests run it against a temp repo.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (combined string, err error)
	Output(ctx context.Context, dir, name string, args ...string) (stdout string, err error)
}

// execRunner is the real Runner, running commands with os/exec. extraEnv
// holds additional environment variables appended to the inherited process
// environment (os.Environ()) for every command this Runner runs -- needed
// later so a caller can add GIT_LITERAL_PATHSPECS=1 for the pathspec-
// consuming git calls in commit.go and perimeter.go. The zero value runs
// with the inherited environment unchanged.
type execRunner struct {
	extraEnv []string
}

func (r execRunner) command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(r.extraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.extraEnv...)
	}
	return cmd
}

// Run runs name with args in dir and returns combined stdout and stderr. On
// a non-zero exit it returns that combined output and the *exec.ExitError
// unwrapped, so a caller can wrap it with %w and its own operation context.
func (r execRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := r.command(ctx, dir, name, args...).CombinedOutput()
	return string(out), err
}

// Output runs name with args in dir and returns stdout alone. On a non-zero
// exit the returned error carries the process's stderr, trimmed, appended
// after the underlying *exec.ExitError so a caller that only logs err still
// sees why the command failed.
func (r execRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := r.command(ctx, dir, name, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return string(out), fmt.Errorf("%w: %s", err, trimTrailingNewline(exitErr.Stderr))
		}
		return string(out), err
	}
	return string(out), nil
}

func trimTrailingNewline(b []byte) string {
	s := string(b)
	for s != "" && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// Orchestrator owns git and every GitHub write for one project.
type Orchestrator struct {
	proj Project
	gh   GitHub
	run  Runner
	log  *slog.Logger
}

// New validates proj (non-empty Owner, Repo, DefaultBranch; absolute
// LocalPath) and returns the orchestrator. An invalid Project is an error.
func New(proj Project, gh GitHub, run Runner, log *slog.Logger) (*Orchestrator, error) {
	if err := validateProject(proj); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{proj: proj, gh: gh, run: run, log: log}, nil
}

func validateProject(proj Project) error {
	switch {
	case proj.Owner == "":
		return errors.New("orchestrator: project owner must not be empty")
	case proj.Repo == "":
		return errors.New("orchestrator: project repo must not be empty")
	case proj.DefaultBranch == "":
		return errors.New("orchestrator: project default branch must not be empty")
	case !filepath.IsAbs(proj.LocalPath):
		return fmt.Errorf("orchestrator: project local path must be absolute: %q", proj.LocalPath)
	}
	return nil
}
