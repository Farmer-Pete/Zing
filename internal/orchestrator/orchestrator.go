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
	"sort"
	"strings"
)

// Project is the git and GitHub identity of one repository the orchestrator
// serves. It is derived from a store.Project and the config, not read from
// the store here.
type Project struct {
	Owner         string // "Farmer-Pete"; non-empty
	Repo          string // "Zing"; non-empty
	LocalPath     string // absolute path to the main checkout; must be absolute
	DefaultBranch string // "main"; non-empty
	// BuildWritableRoots names every path a sandboxed build run can write
	// outside the worktree itself (PKG8-PLAN.md section 15, task 8): the
	// sandbox cache root and the login's mds cache folder. Each entry must
	// be absolute; checkSigningPrograms' own disallowed-roots list
	// (worktree.go) always includes these alongside o.proj.LocalPath and
	// the Claude Code transcripts folder, so a git signing program that
	// resolves into a build's own writable cache is refused exactly like
	// one resolving into the worktree.
	BuildWritableRoots []string
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
// holds additional environment variables appended to the scrubbed process
// environment (see command and scrubGitLocationEnv) for every command this
// Runner runs -- a caller adds GIT_LITERAL_PATHSPECS=1 through it for the
// pathspec-consuming git calls in commit.go and perimeter.go. drivers names
// the filter drivers (PKG8-PLAN.md section 7.2) command overrides to empty
// on every git call this Runner makes, so a worktree-content-touching
// command never runs an owner-configured clean, smudge, or process filter; a
// caller building a runner scoped to one Worktree sets it from that
// Worktree's own drivers field. The zero value runs with the scrubbed
// process environment, no additions, and no driver overrides (still hooks-
// and fsmonitor-disabled, since command applies that unconditionally).
type execRunner struct {
	extraEnv []string
	drivers  []string
}

// hardenedGitArgs returns args with the hardening prefix in front
// (PKG8-PLAN.md section 7.2): core.hooksPath and core.fsmonitor are always
// disabled, so no repo-local git hook and no filesystem monitor ever runs on
// the orchestrator's behalf; then, for each name in drivers (sorted, so the
// result is deterministic regardless of call order), the four settings that
// make that filter driver a no-op are added. A driver name reaches git as
// one argv element per "-c filter.<name>.<key>=<value>", never through a
// shell, so a driver name cannot inject an option or another setting.
func hardenedGitArgs(drivers []string, args ...string) []string {
	sorted := append([]string(nil), drivers...)
	sort.Strings(sorted)

	prefix := make([]string, 0, 4+4*len(sorted)+len(args))
	prefix = append(prefix, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false")
	for _, name := range sorted {
		prefix = append(prefix,
			"-c", "filter."+name+".clean=",
			"-c", "filter."+name+".smudge=",
			"-c", "filter."+name+".process=",
			"-c", "filter."+name+".required=false",
		)
	}
	return append(prefix, args...)
}

// isExitCode reports whether err is (or wraps) an *exec.ExitError whose exit
// code is code -- used to distinguish "the command ran and found nothing"
// (git config --get and --get-regexp exit 1 for no match; git diff --no-
// index exits 1 when the compared paths differ, the expected case here)
// from a real failure.
func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

// gitLocationEnv names the environment variables that redirect where git
// finds its repository, index, and object store. Zing always runs git
// against an explicit working directory (cmd.Dir), so these are scrubbed
// from every git child's environment: if the process inherits them -- most
// commonly when Zing runs from inside a git hook, which exports GIT_DIR and
// its siblings -- they would override cmd.Dir and send every git command at
// the wrong repository. GIT_LITERAL_PATHSPECS (added via extraEnv) is not in
// this list and is preserved.
var gitLocationEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_COMMON_DIR",
	"GIT_PREFIX",
	"GIT_NAMESPACE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_CEILING_DIRECTORIES",
}

// scrubGitLocationEnv returns env with every gitLocationEnv assignment
// removed, so an inherited GIT_DIR (or sibling) cannot override cmd.Dir.
func scrubGitLocationEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, key := range gitLocationEnv {
			if strings.HasPrefix(kv, key+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// localeEnv forces git's messages into the C locale so any output this
// package matches on (for example the "is not a working tree" text
// RemoveWorktree tolerates) is not translated by an inherited LANG or
// LC_MESSAGES. It is appended after the inherited environment, so it wins.
var localeEnv = []string{"LC_ALL=C", "LANG=C"}

// command builds the *exec.Cmd for one git (or other) invocation. It always
// sets cmd.Env to the scrubbed process environment, the C locale, and
// extraEnv, so no git child ever inherits a repository-redirecting GIT_*
// variable and its messages never depend on the host locale; cmd.Dir is the
// single source of truth for which repository the command acts on. When
// name is "git", args is first passed through hardenedGitArgs(r.drivers,
// ...), so every git call this Runner makes -- whatever the caller asked
// for -- carries the hooks/fsmonitor/driver-override prefix; a non-git
// command (there are none in production, but tests exercise this) is left
// alone.
func (r execRunner) command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	if name == "git" {
		args = hardenedGitArgs(r.drivers, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	env := scrubGitLocationEnv(os.Environ())
	env = append(env, localeEnv...)
	env = append(env, r.extraEnv...)
	cmd.Env = env
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
// gh and run are also required (PR review fix): either nil would otherwise
// build an Orchestrator that panics the first time it calls a GitHub or git
// method, rather than failing here at construction.
func New(proj Project, gh GitHub, run Runner, log *slog.Logger) (*Orchestrator, error) {
	if err := validateProject(proj); err != nil {
		return nil, err
	}
	if gh == nil {
		return nil, errors.New("orchestrator: github client must not be nil")
	}
	if run == nil {
		return nil, errors.New("orchestrator: runner must not be nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{proj: proj, gh: gh, run: run, log: log}, nil
}

// NewRunner returns the real Runner: os/exec commands, hooks and fsmonitor
// always disabled, and no filter driver overrides of its own -- a method
// scoped to one Worktree builds its own runner carrying that Worktree's
// drivers instead of using this one directly for a content-touching
// command. cmd/zing's serve and selftest pass this to New.
func NewRunner() Runner {
	return execRunner{}
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
	for _, root := range proj.BuildWritableRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("orchestrator: project build writable root must be absolute: %q", root)
		}
	}
	return nil
}
