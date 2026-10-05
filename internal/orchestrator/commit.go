package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"zing/internal/response"
)

// CoAuthorTrailer credits the Zing engine on every commit it makes. The git
// author and committer stay the host identity, so GitHub verifies the SSH
// signature against the owner's registered key and shows "Verified"; this
// trailer records that Zing did the work. Render always appends it, so no
// caller can omit it or set a different one.
const CoAuthorTrailer = "Co-Authored-By: Zing <zing@farmerpete.net>"

// gitAddSubcommand is "git add"'s own subcommand name, shared by
// CommitTask's and CommitMerge's own pathspecArgs calls so goconst sees one
// named constant instead of two repeated literals.
const gitAddSubcommand = "add"

// CommitMessage is one commit's content. The caller supplies the title, the
// function lines, and the fences; the model produces the last two in
// Package 8. Package 5 renders them and never parses git config.
type CommitMessage struct {
	Title     string           // the task's title, the subject line; non-empty, single line
	FuncLines []string         // one per changed function, among its callers and callees; each non-empty, single line
	Fences    []response.Fence // Chesterton's fence entries, reused from internal/response
}

// Render returns the full commit message, or an error if Title or any
// FuncLines entry is empty or spans more than one line, or if any Fences
// entry's Path, Symbol, or ExistedBecause is empty or spans more than one
// line (PKG5-PLAN.md section 8.3, worked example 12.1). Fences comes from
// Package 8's model output, so it is validated by the same single-line rule
// as Title and FuncLines rather than trusted: an empty or multiline field
// there would otherwise inject extra, model-controlled lines into the commit
// message. The layout is: subject, blank line, the func lines, a blank line
// and the fence lines when present, a blank line and CoAuthorTrailer.
func (m CommitMessage) Render() (string, error) {
	if err := validateSingleLine("title", m.Title); err != nil {
		return "", fmt.Errorf("orchestrator: render commit message: %w", err)
	}
	for i, fl := range m.FuncLines {
		if err := validateSingleLine(fmt.Sprintf("func line %d", i), fl); err != nil {
			return "", fmt.Errorf("orchestrator: render commit message: %w", err)
		}
	}
	for i, f := range m.Fences {
		if err := validateSingleLine(fmt.Sprintf("fence %d path", i), f.Path); err != nil {
			return "", fmt.Errorf("orchestrator: render commit message: %w", err)
		}
		if err := validateSingleLine(fmt.Sprintf("fence %d symbol", i), f.Symbol); err != nil {
			return "", fmt.Errorf("orchestrator: render commit message: %w", err)
		}
		if err := validateSingleLine(fmt.Sprintf("fence %d existed because", i), f.ExistedBecause); err != nil {
			return "", fmt.Errorf("orchestrator: render commit message: %w", err)
		}
	}

	var b strings.Builder
	b.WriteString(m.Title)
	b.WriteString("\n\n")
	b.WriteString(strings.Join(m.FuncLines, "\n"))

	if len(m.Fences) > 0 {
		b.WriteString("\n\n")
		lines := make([]string, len(m.Fences))
		for i, f := range m.Fences {
			lines[i] = fmt.Sprintf("Fence: %s %s, %s", f.Path, f.Symbol, f.ExistedBecause)
		}
		b.WriteString(strings.Join(lines, "\n"))
	}

	b.WriteString("\n\n")
	b.WriteString(CoAuthorTrailer)
	b.WriteString("\n")

	return b.String(), nil
}

// validateApprovedPaths rejects an approved slice CommitTask must never
// stage (PR review fix): empty, since an empty pathspec file makes
// "git add --pathspec-from-file=..." a no-op, which would leave CommitTask
// reporting success while committing nothing at all; or carrying an empty
// entry or one with a NUL byte, since NUL is the pathspec file's own
// record separator (writePathspecFile) and an empty or NUL-bearing path
// would corrupt the file or every entry after it. It runs before any
// staging, so a rejected call never touches git or the working tree.
func validateApprovedPaths(approved []string) error {
	if len(approved) == 0 {
		return errors.New("approved paths must not be empty")
	}
	for i, p := range approved {
		if p == "" {
			return fmt.Errorf("approved path %d must not be empty", i)
		}
		if strings.ContainsRune(p, 0) {
			return fmt.Errorf("approved path %d must not contain a NUL byte: %q", i, p)
		}
	}
	return nil
}

// validateSingleLine rejects an empty value or one containing a line break,
// naming field in the error so Render's caller sees which part of the
// message was invalid.
func validateSingleLine(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("%s must be a single line", field)
	}
	return nil
}

// CommitTask writes a single signed commit of exactly the approved paths,
// and nothing else already sitting in the index (PKG5-PLAN.md section 8.3).
// approved is written once to a NUL-delimited pathspec file, and that same
// file is passed to both "git add --pathspec-from-file=<file>
// --pathspec-file-nul" and "git commit -S -F <msg>
// --pathspec-from-file=<file> --pathspec-file-nul", both run under
// GIT_LITERAL_PATHSPECS=1 (the same wiring perimeter.go's RevertPaths uses),
// so a path is never read as pathspec magic or a wildcard, "git add -A" is
// never run, and the commit itself is scoped to approved rather than
// whatever else a caller or a prior step left staged in the index. Steps:
// revalidate wt; record HEAD; stage approved; render and commit, both
// pathspec-scoped; verify with signedStatus. A git error at commit, an
// unsigned result, or an error running the verification itself all reset
// the branch to the recorded HEAD with "git reset --soft <head>" and return
// a plain "commit signing failed: ..." error, so no unsigned commit is ever
// left at HEAD.
func (o *Orchestrator) CommitTask(ctx context.Context, wt Worktree, approved []string, m CommitMessage) (sha string, err error) {
	if err = validateApprovedPaths(approved); err != nil {
		return "", fmt.Errorf("orchestrator: commit task: %w", err)
	}

	if err = o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: commit task: %w", err)
	}

	priorHead, err := o.run.Output(ctx, wt.dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit task: record head: %w", err)
	}
	priorHead = strings.TrimSpace(priorHead)

	pathspecFile, err := writePathspecFile(approved)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit task: %w", err)
	}
	defer func() { _ = os.Remove(pathspecFile) }()

	litRun := execRunner{extraEnv: literalPathspecEnv, drivers: wt.drivers}
	addArgs := pathspecArgs([]string{gitAddSubcommand}, pathspecFile)
	if out, addErr := litRun.Run(ctx, wt.dir, "git", addArgs...); addErr != nil {
		return "", fmt.Errorf("orchestrator: commit task: stage approved paths: %w: %s", addErr, strings.TrimSpace(out))
	}

	message, err := m.Render()
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit task: %w", err)
	}

	msgFile, err := writeCommitMessageFile(message)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit task: %w", err)
	}
	defer func() { _ = os.Remove(msgFile) }()

	o.log.Info("committing task", "branch", wt.branch, "title", m.Title, "approved_count", len(approved))

	commitArgs := pathspecArgs([]string{"commit", "-S", "-F", msgFile}, pathspecFile)
	commitOut, commitErr := litRun.Run(ctx, wt.dir, "git", commitArgs...)
	if commitErr != nil {
		return "", o.resetAfterUnsignedCommit(ctx, wt, priorHead,
			fmt.Sprintf("git commit -S: %v: %s", commitErr, strings.TrimSpace(commitOut)))
	}

	signed, verified, statusErr := o.signedStatus(ctx, wt.dir, "HEAD")
	if statusErr != nil {
		return "", o.resetAfterUnsignedCommit(ctx, wt, priorHead,
			fmt.Sprintf("verify signature: %v", statusErr))
	}
	if !signed {
		return "", o.resetAfterUnsignedCommit(ctx, wt, priorHead, "commit at HEAD carries no signature")
	}
	if !verified {
		o.log.Warn("commit signed but not locally verifiable (no gpg.ssh.allowedSignersFile on this host)",
			"branch", wt.branch)
	}

	newSHA, err := o.run.Output(ctx, wt.dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit task: read new sha: %w", err)
	}
	newSHA = strings.TrimSpace(newSHA)

	o.log.Info("committed task", "branch", wt.branch, "sha", newSHA, "verified", verified)

	return newSHA, nil
}

// resetUnsignedCommitTimeout bounds the detached reset resetAfterUnsignedCommit
// runs, so a cleanup that can no longer inherit the caller's context still
// completes in bounded time rather than hanging forever.
const resetUnsignedCommitTimeout = 30 * time.Second

// resetAfterUnsignedCommit resets wt.Dir back to priorHead with
// "git reset --soft" and returns a plain "commit signing failed: ..." error
// naming reason. The reset runs under a detached context
// (context.WithoutCancel(ctx), bounded by resetUnsignedCommitTimeout) rather
// than ctx itself: ctx may already be cancelled or past its deadline by the
// time signing fails, and an unsigned commit left at HEAD because the
// cleanup couldn't run is worse than a reset that outlives the caller's own
// context. A failure of the reset itself is logged, not returned, since the
// caller must still learn that signing failed.
func (o *Orchestrator) resetAfterUnsignedCommit(ctx context.Context, wt Worktree, priorHead, reason string) error {
	resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetUnsignedCommitTimeout)
	defer cancel()
	if out, resetErr := o.run.Run(resetCtx, wt.dir, "git", "reset", "--soft", priorHead); resetErr != nil {
		o.log.Error("commit signing failed and the reset also failed", "branch", wt.branch,
			"err", resetErr, "output", strings.TrimSpace(out))
	}
	return errors.New("commit signing failed: " + reason)
}

// CommitMerge finishes the merge in progress as one signed merge commit:
// git add -u (every already-tracked change, which marks resolved conflicts
// and ordinary deletions as resolved), then git add -A scoped to the
// subset of MergeSidePaths whose working-tree file still exists
// (mergeSidePathsPresent) -- the extra pass "git add -u" alone would miss,
// a merge-side path an agent's bash access turned untracked, for example
// with "git rm --cached", leaving it out of the commit entirely even
// though mergeCheck's own outside-the-merge gate had allowed it. The
// explicit pass is scoped to paths that still exist because naming an
// absent one -- a path a resolved merge removed entirely, for example a
// file the ticket deleted and the base left alone, which "git add -u"
// above already staged while the path was still tracked -- makes git
// refuse the whole call with "pathspec '<p>' did not match any files";
// the pass is skipped outright when no side path still exists. Then
// git commit -S -F <msg> with no pathspec, because git refuses a partial
// commit during a merge, which is why CommitTask cannot do this. wantBaseSHA
// is the request's own base sha; a MERGE_HEAD that does not match it is
// refused before anything is staged, so a merge session that rewrote
// MERGE_HEAD (deliberately or not) never produces a commit whose second
// parent is not the base the request named. It returns an error, touching
// nothing, when no merge is in progress or MERGE_HEAD does not match
// wantBaseSHA. A commit error, an unsigned result, or a failed verification
// resets to the prior HEAD and puts MERGE_HEAD back
// (resetAfterUnsignedMerge), so the resolved tree stays a merge in
// progress the next tick can commit again.
func (o *Orchestrator) CommitMerge(ctx context.Context, wt Worktree, m CommitMessage, wantBaseSHA string) (string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: %w", err)
	}
	inProgress, err := o.mergeInProgress(ctx, wt)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: %w", err)
	}
	if !inProgress {
		return "", errors.New("orchestrator: commit merge: no merge in progress")
	}
	mergeHead, err := o.run.Output(ctx, wt.dir, "git", "rev-parse", "MERGE_HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: read MERGE_HEAD: %w", err)
	}
	mergeHead = strings.TrimSpace(mergeHead)
	if mergeHead != wantBaseSHA {
		return "", fmt.Errorf("orchestrator: commit merge: MERGE_HEAD is %s, want %s", mergeHead, wantBaseSHA)
	}
	priorHead, err := o.run.Output(ctx, wt.dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: record head: %w", err)
	}
	priorHead = strings.TrimSpace(priorHead)

	sidePaths, err := o.MergeSidePaths(ctx, wt)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: %w", err)
	}

	message, err := m.Render()
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: %w", err)
	}
	msgFile, err := writeCommitMessageFile(message)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: %w", err)
	}
	defer func() { _ = os.Remove(msgFile) }()

	run := execRunner{drivers: wt.drivers}
	if out, addErr := run.Run(ctx, wt.dir, "git", "add", "-u"); addErr != nil {
		return "", fmt.Errorf("orchestrator: commit merge: stage: %w: %s", addErr, strings.TrimSpace(out))
	}
	present, err := mergeSidePathsPresent(wt.dir, sidePaths)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: %w", err)
	}
	if len(present) > 0 {
		pathspecFile, pathspecErr := writePathspecFile(present)
		if pathspecErr != nil {
			return "", fmt.Errorf("orchestrator: commit merge: %w", pathspecErr)
		}
		defer func() { _ = os.Remove(pathspecFile) }()
		litRun := execRunner{extraEnv: literalPathspecEnv, drivers: wt.drivers}
		addArgs := pathspecArgs([]string{gitAddSubcommand, "-A"}, pathspecFile)
		if out, addErr := litRun.Run(ctx, wt.dir, "git", addArgs...); addErr != nil {
			return "", fmt.Errorf("orchestrator: commit merge: stage: %w: %s", addErr, strings.TrimSpace(out))
		}
	}
	o.log.Info("committing merge", "branch", wt.branch, "merge_head", mergeHead)
	if out, commitErr := run.Run(ctx, wt.dir, "git", "commit", "-S", "-F", msgFile); commitErr != nil {
		return "", o.resetAfterUnsignedMerge(ctx, wt, priorHead, mergeHead,
			fmt.Sprintf("git commit -S: %v: %s", commitErr, strings.TrimSpace(out)))
	}
	signed, verified, statusErr := o.signedStatus(ctx, wt.dir, "HEAD")
	if statusErr != nil {
		return "", o.resetAfterUnsignedMerge(ctx, wt, priorHead, mergeHead, fmt.Sprintf("verify signature: %v", statusErr))
	}
	if !signed {
		return "", o.resetAfterUnsignedMerge(ctx, wt, priorHead, mergeHead, "commit at HEAD carries no signature")
	}
	if !verified {
		o.log.Warn("commit signed but not locally verifiable (no gpg.ssh.allowedSignersFile on this host)",
			"branch", wt.branch)
	}
	newSHA, err := o.run.Output(ctx, wt.dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit merge: read new sha: %w", err)
	}
	newSHA = strings.TrimSpace(newSHA)
	o.log.Info("committed merge", "branch", wt.branch, "sha", newSHA, "verified", verified)
	return newSHA, nil
}

// mergeSidePathsPresent filters paths (MergeSidePaths' own result) to the
// ones whose working-tree file still exists, checked through
// os.OpenRoot(dir) so a path never escapes it. CommitMerge's own explicit
// "git add -A" pass needs this: naming a path that exists in neither the
// working tree nor the index makes git refuse the whole call with
// "pathspec '<p>' did not match any files", and a resolved merge can
// legitimately leave such a path behind, for example a file one side
// deleted and the other left untouched.
func mergeSidePathsPresent(dir string, paths []string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	present := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, statErr := root.Lstat(p); statErr == nil {
			present = append(present, p)
		} else if !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("%s: %w", p, statErr)
		}
	}
	return present, nil
}

// resetAfterUnsignedMerge is resetAfterUnsignedCommit plus
// "git update-ref MERGE_HEAD <mergeHead>" under the same detached,
// bounded context: git reset clears MERGE_HEAD, and without it the
// resolved tree would stop being a merge. A failed update-ref is logged
// WARN ("merge head restore failed"), never returned; the returned error
// is resetAfterUnsignedCommit's own "commit signing failed: ..." error.
//
// When "git commit -S" itself failed outright -- a pre-commit hook or a
// signing error that never produced a commit -- HEAD is still priorHead
// and MERGE_HEAD still resolves to mergeHead: nothing moved, so there is
// nothing to reset and no MERGE_HEAD to restore. Only an unsigned or
// unverifiable commit, which did move HEAD, needs the soft reset and the
// MERGE_HEAD restore.
func (o *Orchestrator) resetAfterUnsignedMerge(ctx context.Context, wt Worktree, priorHead, mergeHead, reason string) error {
	checkCtx, checkCancel := context.WithTimeout(context.WithoutCancel(ctx), resetUnsignedCommitTimeout)
	defer checkCancel()
	headNow, headErr := o.run.Output(checkCtx, wt.dir, "git", "rev-parse", "HEAD")
	mergeHeadNow, mergeHeadErr := o.run.Output(checkCtx, wt.dir, "git", "rev-parse", "-q", "--verify", "MERGE_HEAD")
	headUnmoved := headErr == nil && strings.TrimSpace(headNow) == priorHead
	mergeHeadIntact := mergeHeadErr == nil && strings.TrimSpace(mergeHeadNow) == mergeHead
	if headUnmoved && mergeHeadIntact {
		return errors.New("commit signing failed: " + reason)
	}

	err := o.resetAfterUnsignedCommit(ctx, wt, priorHead, reason)

	resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetUnsignedCommitTimeout)
	defer cancel()
	if out, updateErr := o.run.Run(resetCtx, wt.dir, "git", "update-ref", "MERGE_HEAD", mergeHead); updateErr != nil {
		o.log.Warn("merge head restore failed", "branch", wt.branch,
			"err", updateErr, "output", strings.TrimSpace(out))
	}
	return err
}

// pathspecArgs appends "--pathspec-from-file=<file> --pathspec-file-nul" to
// args, the two flags every pathspec-scoped git call in this file and
// perimeter.go needs to read its paths from file rather than argv.
func pathspecArgs(args []string, file string) []string {
	return append(args, "--pathspec-from-file="+file, "--pathspec-file-nul")
}

// writeCommitMessageFile writes message to a fresh temp file and returns its
// path; the caller removes it when done. Mirrors perimeter.go's
// writePathspecFile, but for a single message body rather than NUL-separated
// paths, so it stays a separate helper.
func writeCommitMessageFile(message string) (string, error) {
	f, err := os.CreateTemp("", "zing-commit-msg-*")
	if err != nil {
		return "", fmt.Errorf("create commit message file: %w", err)
	}
	name := f.Name()

	if _, writeErr := f.WriteString(message); writeErr != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write commit message file: %w", writeErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close commit message file: %w", closeErr)
	}
	return name, nil
}

// signedStatus reports whether the commit at rev carries a valid signature,
// robustly across hosts that have not configured
// gpg.ssh.allowedSignersFile (PKG5-PLAN.md section 8.3). It reads
// "git show --no-patch --format=%G? <rev>", trimmed:
//   - "G": a good, locally verified signature; signed=true, verified=true.
//   - "U": a good signature from a key of unknown validity -- the signature
//     itself checks out, but this host cannot vouch for the key, so it is
//     signed=true but not verified=true (over-trusting "U" as fully
//     verified would accept a signature from an untrusted key).
//   - "N", "E", "X", "Y": git found no verifiable signature ("N"), could not
//     check one at all ("E"), or found one past expiry ("X") or signed by a
//     since-expired key ("Y") -- every one of which is also what a validly
//     SSH-signed commit reports when allowedSignersFile is not configured.
//     Fall back to "git cat-file -p <rev>" and look for a "gpgsig " header:
//     present means the commit is signed but this host cannot verify it
//     (signed=true, verified=false); absent means the commit is genuinely
//     unsigned (signed=false, verified=false).
//   - "B" or "R": a real signing problem (a bad signature, or a good
//     signature from a revoked key); signed=false, verified=false.
func (o *Orchestrator) signedStatus(ctx context.Context, dir, rev string) (signed, verified bool, err error) {
	out, err := o.run.Output(ctx, dir, "git", "show", "--no-patch", "--format=%G?", rev)
	if err != nil {
		return false, false, fmt.Errorf("orchestrator: signed status: git show: %w", err)
	}

	switch code := strings.TrimSpace(out); code {
	case "G":
		return true, true, nil
	case "U":
		return true, false, nil
	case "N", "E", "X", "Y":
		return o.signedStatusFallback(ctx, dir, rev)
	case "B", "R":
		return false, false, nil
	default:
		return false, false, fmt.Errorf("orchestrator: signed status: unrecognized %%G? code %q", code)
	}
}

// signedStatusFallback is the presence check signedStatus falls back to when
// %G? cannot report a locally verifiable result: it looks for a "gpgsig "
// header in the raw commit object rather than trusting local verifiability,
// since a missing gpg.ssh.allowedSignersFile makes %G? report a code such as
// "N" even for a validly signed commit. It scans only the header block of
// "git cat-file -p <rev>" -- the lines up to and not including the first
// blank line that separates the commit's headers from its message -- and
// stops there, so a commit message whose title or body happens to start
// with "gpgsig " (an attacker- or model-controlled string) is never
// misread as a signature header.
func (o *Orchestrator) signedStatusFallback(ctx context.Context, dir, rev string) (signed, verified bool, err error) {
	out, err := o.run.Output(ctx, dir, "git", "cat-file", "-p", rev)
	if err != nil {
		return false, false, fmt.Errorf("orchestrator: signed status: git cat-file: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "gpgsig ") {
			return true, false, nil
		}
	}
	return false, false, nil
}

// SignedStatus is the exported form of signedStatus for one commit of wt:
// it reports whether sha carries a valid signature, without the local-
// verifiability distinction signedStatus itself makes (see signedStatus's
// own doc for why "not locally verifiable" is not the same as "unsigned").
// It first calls revalidate (review F007), the same guard every sibling
// content method opens with; the unexported signedStatus does not repeat it,
// since CommitTask (its other caller) has already revalidated wt.
func (o *Orchestrator) SignedStatus(ctx context.Context, wt Worktree, sha string) (signed bool, err error) {
	if err = o.revalidate(ctx, wt); err != nil {
		return false, fmt.Errorf("orchestrator: signed status: %w", err)
	}
	signed, _, err = o.signedStatus(ctx, wt.dir, sha)
	return signed, err
}

// commitStatusFromLetter maps one "git diff-tree --name-status" status
// letter to Change's closed Status set (PKG8-PLAN.md section 7.3, the same
// mapping statusFromXY applies to "git status" codes in perimeter.go): "A"
// is Added; "M" and "T" (typechange) are Modified; "D" is Deleted. Any other
// letter -- "C" (copy) and "R" (rename) cannot appear here, since CommitChanges
// runs diff-tree with --no-renames -- is an error.
func commitStatusFromLetter(letter string) (Status, error) {
	switch letter {
	case "A":
		return Added, nil
	case "M", "T":
		return Modified, nil
	case "D":
		return Deleted, nil
	default:
		return 0, fmt.Errorf("unrecognized diff-tree status %q", letter)
	}
}

// CommitChanges returns what one commit changed, sorted by path:
//
//	git diff-tree --no-commit-id --name-status -r --no-renames -z <sha>
//
// The -z form pairs a NUL-terminated status letter with a NUL-terminated
// path, repeating; commitStatusFromLetter maps the letter to Change's
// closed set. Any other status letter is an error, naming the path. It
// first calls revalidate (review F007): every sibling content method
// (ChangedPaths, Hunk, BranchCommits, CommitTask) already opens with this
// guard, so a wrong, stale, or tampered worktree is never read here either.
func (o *Orchestrator) CommitChanges(ctx context.Context, wt Worktree, sha string) ([]Change, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return nil, fmt.Errorf("orchestrator: commit changes: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "diff-tree", "--no-commit-id", "--name-status", "-r", "--no-renames", "-z", sha)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: commit changes: %w", err)
	}

	records := strings.Split(out, "\x00")
	changes := make([]Change, 0, len(records)/2)
	for i := 0; i+1 < len(records); i += 2 {
		letter, path := records[i], records[i+1]
		if letter == "" && path == "" {
			continue
		}
		code, err := commitStatusFromLetter(letter)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: commit changes: %s: %w", path, err)
		}
		changes = append(changes, Change{Path: path, Code: code})
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// CommitSubject returns the first line of the commit message:
// git show -s --format=%s <sha>. It first calls revalidate (review F007),
// the same guard every sibling content method opens with.
func (o *Orchestrator) CommitSubject(ctx context.Context, wt Worktree, sha string) (string, error) {
	if err := o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: commit subject: %w", err)
	}

	run := execRunner{drivers: wt.drivers}
	out, err := run.Output(ctx, wt.dir, "git", "show", "-s", "--format=%s", sha)
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit subject: %w", err)
	}
	return strings.TrimSpace(out), nil
}
