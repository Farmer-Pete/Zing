package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Status is one of the four git status shapes a Change carries, closed to
// exactly these (PKG5-PLAN.md section 8.6). RevertPaths dispatches on this
// value to pick the right undo.
type Status int

const (
	// Modified is a tracked path changed since HEAD, staged or not.
	Modified Status = iota + 1
	// Added is a new path staged into the index but not yet committed.
	Added
	// Untracked is a new path git does not track at all.
	Untracked
	// Deleted is a tracked path removed since HEAD, staged or not.
	Deleted
)

// String names the status for log fields and test failure messages.
func (s Status) String() string {
	switch s {
	case Modified:
		return "modified"
	case Added:
		return "added"
	case Untracked:
		return "untracked"
	case Deleted:
		return "deleted"
	default:
		return "unknown"
	}
}

// Change is one path the worktree changed since HEAD, with its git status
// code, so a reverter knows how to undo it.
type Change struct {
	Path string
	Code Status
}

// Extra is one changed path that the plan did not declare.
type Extra struct {
	Path   string
	Marker string // "", markerTrustRoot, or markerStyleGuide
}

const (
	markerTrustRoot  = "trust root"
	markerStyleGuide = "style guide"
)

// literalPathspecEnv is the environment every pathspec-consuming git call in
// this file runs with, so a path read from --pathspec-from-file is never
// interpreted as pathspec magic or a wildcard, even one containing "*", "?",
// "[", or a leading ":".
var literalPathspecEnv = []string{"GIT_LITERAL_PATHSPECS=1"}

// conflictCodes are the git porcelain-v1 XY codes an unmerged path can carry.
// ChangedPaths treats every one of these as an error rather than guessing a
// revert for it; none is ever expected in a fresh ticket worktree.
var conflictCodes = map[string]bool{
	"DD": true, "AU": true, "UD": true, "UA": true,
	"DU": true, "AA": true, "UU": true,
}

// ChangedPaths returns every path the worktree changed since HEAD: staged,
// unstaged, and untracked. It runs "git -C <dir> -c status.renames=false
// status --porcelain=v1 -z --untracked-files=all --no-renames" and parses the
// NUL-separated records. With renames off, every record is a single path, so
// a rename surfaces as a Deleted old path plus an Added or Untracked new
// path, with no two-path record to parse. An unmerged/conflict code returns
// an error naming the path. The result is sorted by path.
func (o *Orchestrator) ChangedPaths(ctx context.Context, wt Worktree) ([]Change, error) {
	out, err := o.run.Output(ctx, wt.dir, "git", "-c", "status.renames=false",
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: changed paths: %w", err)
	}

	records := strings.Split(out, "\x00")
	changes := make([]Change, 0, len(records))
	for _, record := range records {
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("orchestrator: changed paths: malformed status record %q", record)
		}
		xy, p := record[:2], record[3:]

		code, err := statusFromXY(xy)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: changed paths: %s: %w", p, err)
		}
		changes = append(changes, Change{Path: p, Code: code})
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// statusFromXY classifies a git porcelain-v1 XY status code. "??" is
// Untracked. A conflict code (conflictCodes) is an error. Otherwise: an
// index status of "A" (checked first, since it can co-occur with a worktree
// "D" or "M") is Added; a "D" anywhere is Deleted; an "M" anywhere is
// Modified.
func statusFromXY(xy string) (Status, error) {
	switch {
	case xy == "??":
		return Untracked, nil
	case conflictCodes[xy]:
		return 0, fmt.Errorf("unmerged path (status %q)", xy)
	case xy[0] == 'A':
		return Added, nil
	case strings.Contains(xy, "D"):
		return Deleted, nil
	case strings.Contains(xy, "M"):
		return Modified, nil
	default:
		return 0, fmt.Errorf("unrecognized status %q", xy)
	}
}

// Perimeter classifies changed paths against the declared allowlist. A path
// equal to a declared path is inside the perimeter and omitted; declared is
// an exact-path set, not globs. Every other path is an Extra. An extra that
// matches a trust-root pattern is marked "trust root"; one that matches a
// style-guide pattern is marked "style guide"; one that matches both is
// marked "trust root". The result is sorted by path. It is a pure function.
func Perimeter(changed []Change, declared, trustRoot, styleGuide []string) []Extra {
	declaredSet := make(map[string]struct{}, len(declared))
	for _, d := range declared {
		declaredSet[d] = struct{}{}
	}

	extras := make([]Extra, 0, len(changed))
	for _, c := range changed {
		if _, ok := declaredSet[c.Path]; ok {
			continue
		}

		marker := ""
		switch {
		case matchAny(trustRoot, c.Path):
			marker = markerTrustRoot
		case matchAny(styleGuide, c.Path):
			marker = markerStyleGuide
		}
		extras = append(extras, Extra{Path: c.Path, Marker: marker})
	}

	sort.Slice(extras, func(i, j int) bool { return extras[i].Path < extras[j].Path })
	return extras
}

func matchAny(patterns []string, p string) bool {
	for _, pattern := range patterns {
		if matchPattern(pattern, p) {
			return true
		}
	}
	return false
}

// matchPattern is the matcher Perimeter uses for the trust-root and
// style-guide sets (declared is always exact equality, never a pattern):
//   - a pattern with no "*" matches by exact equality
//   - a pattern ending in "/**" matches a path equal to the prefix or one
//     starting with the prefix + "/" (the only recursive form the design's
//     trust-root and style-guide sets use)
//   - any other pattern is matched with path.Match against the full path,
//     which cannot itself express "**", hence the special case above
func matchPattern(pattern, p string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == p
	}
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	ok, err := path.Match(pattern, p)
	return err == nil && ok
}

// RevertPaths undoes exactly the given changes, by status, so a rejected
// extra leaves no trace and no accepted file is touched. It groups the paths
// by how they undo: Modified and Deleted paths are restored with
// "git restore --staged --worktree"; Added (staged-new) paths are unstaged
// with "git restore --staged" and then removed from disk; Untracked paths
// are removed from disk directly, with no git call. Each restore reads its
// paths from a NUL-separated file via "--pathspec-from-file=<file>
// --pathspec-file-nul", run with GIT_LITERAL_PATHSPECS=1 in the environment,
// so a path is never interpreted as pathspec magic or a wildcard. It returns
// an error if any given path is still changed afterward (re-checked with
// ChangedPaths).
//
// On the Runner and GIT_LITERAL_PATHSPECS wiring: Runner (orchestrator.go,
// Task 1) is Run/Output with no per-call environment, and o.run may be any
// implementation a caller injects, not only execRunner. Rather than widen
// that shared interface for the two restore calls below, RevertPaths builds
// its own execRunner with extraEnv set to GIT_LITERAL_PATHSPECS=1 at the call
// site (execRunner's extraEnv field was added in Task 1 for exactly this).
// That runner, not o.run, runs the two restore commands; every path removal
// goes through os.Remove instead of a git call, so it carries no pathspec
// risk at all. This bypasses whatever Runner a caller wired into o.run for
// those two commands specifically. Every test here constructs the
// Orchestrator with execRunner (real git) as o.run already, since this is a
// real-git integration seam (PKG5-PLAN.md section 11), so the bypass changes
// nothing observable in this package. CommitTask (commit.go, a later task)
// needs the same GIT_LITERAL_PATHSPECS wiring for "git add"; if that task
// finds a cleaner shared spot for it, this is the function to reconcile it
// with.
func (o *Orchestrator) RevertPaths(ctx context.Context, wt Worktree, changes []Change) error {
	restoreBoth := make([]string, 0, len(changes))
	restoreStaged := make([]string, 0, len(changes))
	remove := make([]string, 0, len(changes))

	for _, c := range changes {
		switch c.Code {
		case Modified, Deleted:
			restoreBoth = append(restoreBoth, c.Path)
		case Added:
			restoreStaged = append(restoreStaged, c.Path)
			remove = append(remove, c.Path)
		case Untracked:
			remove = append(remove, c.Path)
		default:
			return fmt.Errorf("orchestrator: revert paths: %s: unrecognized status %v", c.Path, c.Code)
		}
	}

	o.log.Info("reverting extra paths", "branch", wt.branch, "count", len(changes))

	litRun := execRunner{extraEnv: literalPathspecEnv}

	if len(restoreBoth) > 0 {
		if err := runPathspecCommand(ctx, litRun, wt.dir, restoreBoth, "restore", "--staged", "--worktree"); err != nil {
			return fmt.Errorf("orchestrator: revert paths: %w", err)
		}
	}
	if len(restoreStaged) > 0 {
		if err := runPathspecCommand(ctx, litRun, wt.dir, restoreStaged, "restore", "--staged"); err != nil {
			return fmt.Errorf("orchestrator: revert paths: %w", err)
		}
	}
	for _, p := range remove {
		full := filepath.Join(wt.dir, p)
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("orchestrator: revert paths: remove %s: %w", full, err)
		}
	}

	if err := checkFullyReverted(ctx, o, wt, changes); err != nil {
		return err
	}

	o.log.Info("reverted extra paths", "branch", wt.branch, "count", len(changes))
	return nil
}

// checkFullyReverted re-runs ChangedPaths and errors, naming every offending
// path, if any of changes is still present in the result.
func checkFullyReverted(ctx context.Context, o *Orchestrator, wt Worktree, changes []Change) error {
	remaining, err := o.ChangedPaths(ctx, wt)
	if err != nil {
		return fmt.Errorf("orchestrator: revert paths: recheck: %w", err)
	}

	stillChanged := make(map[string]bool, len(remaining))
	for _, c := range remaining {
		stillChanged[c.Path] = true
	}

	leftover := make([]string, 0, len(changes))
	for _, c := range changes {
		if stillChanged[c.Path] {
			leftover = append(leftover, c.Path)
		}
	}
	if len(leftover) > 0 {
		sort.Strings(leftover)
		return fmt.Errorf("orchestrator: revert paths: still changed after revert: %s", strings.Join(leftover, ", "))
	}
	return nil
}

// runPathspecCommand runs "git <args...> --pathspec-from-file=<file>
// --pathspec-file-nul" in dir through run, with paths written NUL-separated
// to a temp file that is removed afterward.
func runPathspecCommand(ctx context.Context, run Runner, dir string, paths []string, args ...string) error {
	file, err := writePathspecFile(paths)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file) }()

	full := make([]string, 0, len(args)+2)
	full = append(full, args...)
	full = append(full, "--pathspec-from-file="+file, "--pathspec-file-nul")

	if out, runErr := run.Run(ctx, dir, "git", full...); runErr != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), runErr, strings.TrimSpace(out))
	}
	return nil
}

// writePathspecFile writes paths NUL-separated to a fresh temp file and
// returns its path. The caller removes it when done.
func writePathspecFile(paths []string) (string, error) {
	f, err := os.CreateTemp("", "zing-pathspec-*")
	if err != nil {
		return "", fmt.Errorf("create pathspec file: %w", err)
	}
	name := f.Name()

	for _, p := range paths {
		if _, writeErr := f.WriteString(p + "\x00"); writeErr != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return "", fmt.Errorf("write pathspec file: %w", writeErr)
		}
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close pathspec file: %w", closeErr)
	}
	return name, nil
}

var smallNumberWords = map[int]string{
	1: "one", 2: "two", 3: "three", 4: "four", 5: "five",
	6: "six", 7: "seven", 8: "eight", 9: "nine",
}

func countWord(n int) string {
	if w, ok := smallNumberWords[n]; ok {
		return w
	}
	return strconv.Itoa(n)
}

// PerimeterNotice is the message the build loop resumes the agent with after
// a revert (PKG5-PLAN.md section 12.3). It names each reverted path and its
// marker, states that the change was reverted, and gives the agent two
// options: repair any declared file that depended on the reverted change, or
// return a question to the owner. It is a pure function.
func PerimeterNotice(reverted []Extra) string {
	noun := "file"
	if len(reverted) != 1 {
		noun = "files"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You changed %s %s outside the set the plan declared. "+
		"They were reverted, so your task is not committed yet.\n\n", countWord(len(reverted)), noun)

	for _, e := range reverted {
		if e.Marker == "" {
			fmt.Fprintf(&b, "- %s\n", e.Path)
		} else {
			fmt.Fprintf(&b, "- %s (%s)\n", e.Path, e.Marker)
		}
	}

	b.WriteString("\nRepair any declared file that depended on these reverts so the task builds " +
		"within its declared files. If you need one of these files in the plan, return a question " +
		"to the owner and say which file and why.\n")

	return b.String()
}
