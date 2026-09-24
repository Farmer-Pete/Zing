// project.go implements "zing project add" (PKG5-PLAN.md section 10):
// discover a repository's default branch and its "ci" required check
// through the GitHub API, then append the project to zing.toml.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"zing/internal/config"
	"zing/internal/orchestrator"
)

const projectAddUsage = `usage: zing project add --name <n> --repo <owner/repo> --path <dir> --test "<cmd>" --lint "<cmd>" [--tracker github]`

// errMissingRequiredCICheck is the exact error PKG5-PLAN.md section 10 step
// 6 requires when the repository's default branch protection does not list
// "ci" among its required status checks. projectAdd returns it unwrapped, so
// its Error() text is exactly this string with no added prefix.
var errMissingRequiredCICheck = errors.New("branch protection missing required check ci")

// runProject dispatches "zing project"'s one subcommand, "add".
func runProject(args []string) int {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(os.Stderr, "usage: zing project add ...")
		return 2
	}
	return runProjectAdd(args[1:])
}

// runProjectAdd is the real "zing project add" entry point: it resolves the
// default config path, loads it once to authenticate the real GitHub
// client, then hands off to projectAdd, the testable core. projectAdd runs
// its own LoadForAdd as PKG5-PLAN.md section 10 step 1 specifies, so this
// outer load exists only to obtain cfg.GitHubToken for orchestrator.NewGitHub
// before projectAdd's own validation begins; a test calls projectAdd
// directly with a fake GitHub client and never needs a real token.
func runProjectAdd(args []string) int {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zing project add: %v\n", err)
		return 2
	}

	cfg, err := config.LoadForAdd(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zing project add: %v\n", err)
		return 1
	}

	gh, err := orchestrator.NewGitHub(cfg.GitHubToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zing project add: %v\n", err)
		return 1
	}

	if err := projectAdd(context.Background(), cfgPath, gh, args); err != nil {
		fmt.Fprintf(os.Stderr, "zing project add: %v\n", err)
		return 1
	}
	return 0
}

// projectAddFlags is the parsed and validated "zing project add" flag set.
type projectAddFlags struct {
	name, repo, path, test, lint, tracker string
}

// parseProjectAddFlags parses and validates the "add" subcommand's flags, in
// the order PKG5-PLAN.md section 10 step 2 lists: name, repo, path
// (absolute), test, lint required; tracker defaults to "github" and must be
// "github". It also rejects any trailing positional argument fs.Parse leaves
// unconsumed (a stray word after the flags, or a flag typo'd without its
// leading "-"), naming it in the error, rather than silently ignoring it.
func parseProjectAddFlags(args []string) (projectAddFlags, error) {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f projectAddFlags
	fs.StringVar(&f.name, "name", "", "project name")
	fs.StringVar(&f.repo, "repo", "", "owner/repo")
	fs.StringVar(&f.path, "path", "", "absolute local checkout path")
	fs.StringVar(&f.test, "test", "", "the project's test command")
	fs.StringVar(&f.lint, "lint", "", "the project's lint command")
	fs.StringVar(&f.tracker, "tracker", "github", "issue tracker (github only)")
	if err := fs.Parse(args); err != nil {
		return projectAddFlags{}, fmt.Errorf("%s: %w", projectAddUsage, err)
	}
	if fs.NArg() > 0 {
		return projectAddFlags{}, fmt.Errorf("zing project add: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	switch {
	case f.name == "":
		return projectAddFlags{}, errors.New("zing project add: --name is required")
	case f.repo == "":
		return projectAddFlags{}, errors.New("zing project add: --repo is required")
	case f.path == "":
		return projectAddFlags{}, errors.New("zing project add: --path is required")
	case !filepath.IsAbs(f.path):
		return projectAddFlags{}, fmt.Errorf("zing project add: --path must be absolute, got %q", f.path)
	case f.test == "":
		return projectAddFlags{}, errors.New("zing project add: --test is required")
	case f.lint == "":
		return projectAddFlags{}, errors.New("zing project add: --lint is required")
	case f.tracker != "github":
		return projectAddFlags{}, fmt.Errorf("zing project add: --tracker must be github, got %q", f.tracker)
	}
	return f, nil
}

// splitOwnerRepo splits "owner/repo" on its single "/" (PKG5-PLAN.md section
// 10 step 4). Zero or more than one slash is an error.
func splitOwnerRepo(repo string) (owner, name string, err error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("zing project add: --repo must be owner/repo, got %q", repo)
	}
	return parts[0], parts[1], nil
}

// projectAdd is the testable core of "zing project add", run in this exact
// order so the first error is deterministic (PKG5-PLAN.md section 10):
//  1. LoadForAdd from cfgPath: a missing github_token or user fails here;
//     zero existing projects is allowed.
//  2. Parse and validate the flags.
//  3. A project whose name already exists is a fatal error, with no write.
//  4. Split --repo into owner and repo on the single "/".
//  5. Discover the default branch through gh.RepoDefaultBranch; an empty
//     result is an error, since projectAdd must not trust an empty
//     DefaultBranch into the saved project (store.EnsureProject only
//     defaults an empty DefaultBranch to "main" for a project it is
//     inserting for the first time, not one already on record).
//  6. gh.RequiredChecks on that branch; a result missing "ci" fails with
//     errMissingRequiredCICheck and writes nothing.
//  7. Append the project, with the discovered default branch, and
//     config.Save.
func projectAdd(ctx context.Context, cfgPath string, gh orchestrator.GitHub, args []string) error {
	cfg, err := config.LoadForAdd(cfgPath)
	if err != nil {
		return err
	}

	f, err := parseProjectAddFlags(args)
	if err != nil {
		return err
	}

	if slices.ContainsFunc(cfg.Projects, func(p config.Project) bool { return p.Name == f.name }) {
		return fmt.Errorf("zing project add: project %q already exists", f.name)
	}

	owner, repo, err := splitOwnerRepo(f.repo)
	if err != nil {
		return err
	}

	defaultBranch, err := gh.RepoDefaultBranch(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("zing project add: repo default branch: %w", err)
	}
	if defaultBranch == "" {
		return errors.New("zing project add: repo default branch: GitHub returned an empty default branch")
	}

	checks, err := gh.RequiredChecks(ctx, owner, repo, defaultBranch)
	if err != nil {
		return fmt.Errorf("zing project add: required checks: %w", err)
	}
	if !slices.Contains(checks, "ci") {
		return errMissingRequiredCICheck
	}

	cfg.Projects = append(cfg.Projects, config.Project{
		Name:          f.name,
		Repo:          f.repo,
		Path:          f.path,
		Tracker:       f.tracker,
		DefaultBranch: defaultBranch,
		Commands: config.Commands{
			Test: f.test,
			Lint: f.lint,
		},
	})

	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("zing project add: %w", err)
	}
	return nil
}
