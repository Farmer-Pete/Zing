package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"zing/internal/response"
)

// CoAuthorTrailer credits the Zing engine on every commit it makes. The git
// author and committer stay the host identity, so GitHub verifies the SSH
// signature against the owner's registered key and shows "Verified"; this
// trailer records that Zing did the work. Render always appends it, so no
// caller can omit it or set a different one.
const CoAuthorTrailer = "Co-Authored-By: Zing <zing@farmerpete.net>"

// CommitMessage is one commit's content. The caller supplies the title, the
// function lines, and the fences; the model produces the last two in
// Package 8. Package 5 renders them and never parses git config.
type CommitMessage struct {
	Title     string           // the task's title, the subject line; non-empty, single line
	FuncLines []string         // one per changed function, among its callers and callees; each non-empty, single line
	Fences    []response.Fence // Chesterton's fence entries, reused from internal/response
}

// Render returns the full commit message, or an error if Title or any
// FuncLines entry is empty or spans more than one line (PKG5-PLAN.md section
// 8.3, worked example 12.1). The layout is: subject, blank line, the func
// lines, a blank line and the fence lines when present, a blank line and
// CoAuthorTrailer.
func (m CommitMessage) Render() (string, error) {
	if err := validateSingleLine("title", m.Title); err != nil {
		return "", fmt.Errorf("orchestrator: render commit message: %w", err)
	}
	for i, fl := range m.FuncLines {
		if err := validateSingleLine(fmt.Sprintf("func line %d", i), fl); err != nil {
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

// CommitTask writes a single signed commit of exactly the approved paths
// (PKG5-PLAN.md section 8.3). approved is staged with
// "git add --pathspec-from-file=<file> --pathspec-file-nul" under
// GIT_LITERAL_PATHSPECS=1, the same wiring perimeter.go's RevertPaths uses,
// so a path is never read as pathspec magic or a wildcard and "git add -A"
// is never run. Steps: revalidate wt; record HEAD; stage approved; render
// and commit with "git commit -S -F <file>"; verify with signedStatus. A git
// error at commit, an unsigned result, or an error running the verification
// itself all reset the branch to the recorded HEAD with
// "git reset --soft <head>" and return a plain "commit signing failed: ..."
// error, so no unsigned commit is ever left at HEAD.
func (o *Orchestrator) CommitTask(ctx context.Context, wt Worktree, approved []string, m CommitMessage) (sha string, err error) {
	if err = o.revalidate(ctx, wt); err != nil {
		return "", fmt.Errorf("orchestrator: commit task: %w", err)
	}

	priorHead, err := o.run.Output(ctx, wt.dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: commit task: record head: %w", err)
	}
	priorHead = strings.TrimSpace(priorHead)

	litRun := execRunner{extraEnv: literalPathspecEnv}
	if err = runPathspecCommand(ctx, litRun, wt.dir, approved, "add"); err != nil {
		return "", fmt.Errorf("orchestrator: commit task: stage approved paths: %w", err)
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

	commitOut, commitErr := o.run.Run(ctx, wt.dir, "git", "commit", "-S", "-F", msgFile)
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

// resetAfterUnsignedCommit resets wt.Dir back to priorHead with
// "git reset --soft" and returns a plain "commit signing failed: ..." error
// naming reason. A failure of the reset itself is logged, not returned,
// since the caller must still learn that signing failed.
func (o *Orchestrator) resetAfterUnsignedCommit(ctx context.Context, wt Worktree, priorHead, reason string) error {
	if out, resetErr := o.run.Run(ctx, wt.dir, "git", "reset", "--soft", priorHead); resetErr != nil {
		o.log.Error("commit signing failed and the reset also failed", "branch", wt.branch,
			"err", resetErr, "output", strings.TrimSpace(out))
	}
	return errors.New("commit signing failed: " + reason)
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
//   - "G" or "U": the signature verified locally; signed=true, verified=true.
//   - "N": git found no verifiable signature -- which is also what it
//     reports for a validly SSH-signed commit when allowedSignersFile is not
//     configured. Fall back to "git cat-file -p <rev>" and look for a
//     "gpgsig " header line: present means the commit is signed but this
//     host cannot verify it (signed=true, verified=false); absent means the
//     commit is genuinely unsigned (signed=false, verified=false).
//   - "B", "E", "X", "Y", "R": a real signing problem; signed=false.
func (o *Orchestrator) signedStatus(ctx context.Context, dir, rev string) (signed, verified bool, err error) {
	out, err := o.run.Output(ctx, dir, "git", "show", "--no-patch", "--format=%G?", rev)
	if err != nil {
		return false, false, fmt.Errorf("orchestrator: signed status: git show: %w", err)
	}

	switch code := strings.TrimSpace(out); code {
	case "G", "U":
		return true, true, nil
	case "N":
		return o.signedStatusFallback(ctx, dir, rev)
	case "B", "E", "X", "Y", "R":
		return false, false, nil
	default:
		return false, false, fmt.Errorf("orchestrator: signed status: unrecognized %%G? code %q", code)
	}
}

// signedStatusFallback is the presence check signedStatus falls back to when
// %G? reports "N": it looks for a "gpgsig " header in the raw commit object
// rather than trusting local verifiability, since a missing
// gpg.ssh.allowedSignersFile makes %G? report "N" even for a validly signed
// commit.
func (o *Orchestrator) signedStatusFallback(ctx context.Context, dir, rev string) (signed, verified bool, err error) {
	out, err := o.run.Output(ctx, dir, "git", "cat-file", "-p", rev)
	if err != nil {
		return false, false, fmt.Errorf("orchestrator: signed status: git cat-file: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "gpgsig ") {
			return true, false, nil
		}
	}
	return false, false, nil
}
