package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// HeadSHA returns git rev-parse HEAD. It revalidates wt first, like every
// method in this file.
func (o *Orchestrator) HeadSHA(ctx context.Context, wt Worktree) (string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: head sha: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: head sha: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Diff returns the branch's diff at sha against its merge base with the
// base (design section 10.1):
//
//	base := git merge-base <baseRev> <sha>
//	git diff --no-ext-diff --no-textconv --no-color --no-renames -U3 <base> <sha>
//
// Diff fetches the base itself, right after revalidate, so baseRev is
// refs/zing/base/<default> as of this call, or the local default branch
// when the fetch has never worked.
//
// --no-ext-diff and --no-textconv keep a configured external diff or
// textconv driver from ever running or reshaping the text a lens reads (the
// same reasoning as Hunk in perimeter.go); --no-renames keeps ParseDiff's
// assumption that "diff --git a/<p> b/<p>" always names one path, not two.
func (o *Orchestrator) Diff(ctx context.Context, wt Worktree, sha string) (string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: diff: %w", err)
	}

	if _, _, err := o.fetchBase(ctx, wt.ticketID()); err != nil {
		return "", fmt.Errorf("orchestrator: diff: %w", err)
	}

	base, err := o.baseRev(ctx, wt.ticketID())
	if err != nil {
		return "", fmt.Errorf("orchestrator: diff: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	mergeBase, err := run.Output(ctx, wt.dir, "git", "merge-base", base, sha)
	if err != nil {
		return "", fmt.Errorf("orchestrator: diff: merge-base: %w", err)
	}

	out, err := run.Output(ctx, wt.dir, "git", "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "-U3", strings.TrimSpace(mergeBase), sha)
	if err != nil {
		return "", fmt.Errorf("orchestrator: diff: %w", err)
	}
	return out, nil
}

// ChangedFilesBetween returns the paths changed between two commits,
// sorted: git diff --name-only --no-renames -z <from> <to>.
func (o *Orchestrator) ChangedFilesBetween(ctx context.Context, wt Worktree, from, to string) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: changed files between: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "diff", "--name-only", "--no-renames", "-z", from, to)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: changed files between: %w", err)
	}

	paths := make([]string, 0)
	for p := range strings.SplitSeq(out, "\x00") {
		if p == "" {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// ChangedFilesSinceBase is ChangedFilesBetween(merge-base(baseRev, to), to).
func (o *Orchestrator) ChangedFilesSinceBase(ctx context.Context, wt Worktree, to string) ([]string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: changed files since base: %w", err)
	}

	base, err := o.baseRev(ctx, wt.ticketID())
	if err != nil {
		return nil, fmt.Errorf("orchestrator: changed files since base: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	mergeBase, err := run.Output(ctx, wt.dir, "git", "merge-base", base, to)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: changed files since base: merge-base: %w", err)
	}

	return o.ChangedFilesBetween(ctx, wt, strings.TrimSpace(mergeBase), to)
}

// IsAncestor runs git merge-base --is-ancestor a b; exit 1 is false, nil.
func (o *Orchestrator) IsAncestor(ctx context.Context, wt Worktree, a, b string) (bool, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return false, fmt.Errorf("orchestrator: is ancestor: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	if _, err := run.Output(ctx, wt.dir, "git", "merge-base", "--is-ancestor", a, b); err != nil {
		if isExitCode1(err) {
			return false, nil
		}
		return false, fmt.Errorf("orchestrator: is ancestor: %w", err)
	}
	return true, nil
}
