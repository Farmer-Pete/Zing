package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// pushRetryBackoff is the wait after each failed git push GitHub answered
// with a 5xx: 3 retries, so 4 attempts in all. New clones this into every
// Orchestrator's own pushBackoff field; package tests shorten that clone
// rather than mutating this package variable.
var pushRetryBackoff = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

// github5xxPattern matches a git push GitHub refused with a server error
// (owner decision Q2): a status phrase, or the code right after "HTTP " or
// "error: ", never a bare 502 inside a SHA or a branch name.
var github5xxPattern = regexp.MustCompile(`Internal Server Error|Bad Gateway|Service Unavailable|Gateway Timeout|(?:HTTP |error: )50[0234]\b`)

// isGitHub5xx reports whether out, git push's combined output, shows a
// GitHub 5xx.
func isGitHub5xx(out string) bool {
	return github5xxPattern.MatchString(out)
}

// Push pushes wt's branch to origin (PKG5-PLAN.md section 8.4). It first
// revalidates wt (8.2): the branch must match the zing/ form, must differ
// from the default branch, and must be the branch actually checked out in
// wt.Dir. That rejects a default or a forged non-zing branch before any git
// command runs. It then fetches the base itself, right after revalidate,
// so refs/zing/base/<default> is current before unpushedShas reads it. It
// then verifies every commit in <base>..<branch> is signed, using the same
// signedStatus (8.3) CommitTask verifies with: a single unsigned commit
// aborts the push before anything is pushed. Only then does it run
// "git -C <dir> push origin refs/heads/<branch>:refs/heads/<branch>", an
// explicit same-name refspec that cannot be reinterpreted as a target on the
// default branch, followed by "git config --local branch.<b>.remote/.merge" to record the
// upstream (PR review fix C3; design section 8's own inventory in
// commonlock.go).
func (o *Orchestrator) Push(ctx context.Context, wt Worktree) error {
	if err := o.revalidate(ctx, wt); err != nil {
		return fmt.Errorf("orchestrator: push: %w", err)
	}

	if _, _, err := o.fetchBase(ctx, wt.ticketID); err != nil {
		return fmt.Errorf("orchestrator: push: %w", err)
	}

	shas, err := o.unpushedShas(ctx, wt)
	if err != nil {
		return fmt.Errorf("orchestrator: push: %w", err)
	}
	for _, sha := range shas {
		signed, _, statusErr := o.signedStatus(ctx, wt.dir, sha)
		if statusErr != nil {
			return fmt.Errorf("orchestrator: push: verify commit %s signed: %w", sha, statusErr)
		}
		if !signed {
			return fmt.Errorf("orchestrator: push: commit %s on branch %q is not signed", sha, wt.branch)
		}
	}

	o.log.Info("pushing branch", "ticket_id", wt.ticketID, "branch", wt.branch, "commits", len(shas))

	// The push itself (no -u) writes only refs/remotes/origin/<branch>: a
	// ref update, which git-safe object/ref writes already make safe
	// without commonMu (design section 8's own "Scope of the guarantee").
	// It runs outside the lock (PR review fix C3), so a slow or stalled
	// remote never blocks every other ticket's shared git writes in this
	// repository for the whole network round trip.
	refspec := "refs/heads/" + wt.branch + ":refs/heads/" + wt.branch
	attempts, pushErr := o.pushOrigin(ctx, wt, refspec)
	if pushErr != nil {
		return pushErr
	}

	// Only the upstream config writes touch the shared config, so only
	// they need commonMu. They write branch.<b>.remote and .merge
	// directly rather than through git branch --set-upstream-to, which
	// needs refs/remotes/origin/<b> and so fails when origin has no fetch
	// refspec, even though the push above already published the branch.
	for _, kv := range [][2]string{
		{"branch." + wt.branch + ".remote", "origin"},
		{"branch." + wt.branch + ".merge", "refs/heads/" + wt.branch},
	} {
		if out, runErr := o.runCommon(ctx, o.run, wt.dir, "config", "--local", kv[0], kv[1]); runErr != nil {
			return fmt.Errorf("orchestrator: push: git config %s: %w: %s", kv[0], runErr, strings.TrimSpace(out))
		}
	}

	o.log.Info("pushed branch", "ticket_id", wt.ticketID, "branch", wt.branch, "attempts", attempts)

	return nil
}

// pushOrigin runs git push, retrying a GitHub 5xx after each o.pushBackoff
// wait (owner decision Q1: the wait runs inside this call, which blocks the
// calling tick). Any other failure returns at once. attempts counts every
// push run, including the final one, whether it succeeded or not.
func (o *Orchestrator) pushOrigin(ctx context.Context, wt Worktree, refspec string) (attempts int, err error) {
	for attempt := 1; ; attempt++ {
		out, runErr := o.run.Run(ctx, wt.dir, "git", "push", "origin", refspec)
		if runErr == nil {
			return attempt, nil
		}

		server := isGitHub5xx(out)
		retry := server && attempt <= len(o.pushBackoff)
		o.log.Warn("git push failed", "ticket_id", wt.ticketID, "branch", wt.branch, "attempt", attempt, "github_5xx", server, "retry", retry)
		if !retry {
			return attempt, fmt.Errorf("orchestrator: push: git push (attempts: %d): %w: %s", attempt, runErr, strings.TrimSpace(out))
		}

		select {
		case <-ctx.Done():
			return attempt, fmt.Errorf("orchestrator: push: git push (attempts: %d): waiting to retry: %w", attempt, ctx.Err())
		case <-time.After(o.pushBackoff[attempt-1]):
		}
	}
}

// unpushedShas returns every commit sha in <base>..<branch>, read with
// "git log -z --format=%H" and split on NUL, trimmed, with empty fields
// dropped. base is baseRev: refs/zing/base/<default>, kept current by
// fetchBase, or the local default branch while that ref does not exist
// yet.
func (o *Orchestrator) unpushedShas(ctx context.Context, wt Worktree) ([]string, error) {
	base, err := o.baseRev(ctx, wt.ticketID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: unpushed shas: %w", err)
	}

	out, err := o.run.Output(ctx, wt.dir, "git", "log", "-z", "--format=%H", base+".."+wt.branch)
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", base, wt.branch, err)
	}

	var shas []string
	for rec := range strings.SplitSeq(out, "\x00") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		shas = append(shas, rec)
	}
	return shas, nil
}

// PullRequest is the content of a draft pull request (PKG5-PLAN.md section
// 8.4). The caller fills every field; Package 8 and 9 fill them from the
// plan.
type PullRequest struct {
	Title            string // non-empty
	What             string // markdown body of "## What"
	WorkingDemo      string
	Scenarios        string
	AcceptedFindings string // markdown body of "## Accepted review findings"; empty leaves the section out
	DeclaredFiles    string // pre-rendered markdown table
	ChestertonsFence string
	PlanLink         string // a URL or console link
}

// prBodySections is the fixed, ordered shape Body renders: seven "## "
// headings, each carrying the PullRequest field it draws from. An optional
// section with empty content is left out, heading and all.
var prBodySections = []struct {
	heading  string
	field    func(PullRequest) string
	optional bool
}{
	{"What", func(pr PullRequest) string { return pr.What }, false},
	{"Working demo", func(pr PullRequest) string { return pr.WorkingDemo }, false},
	{"Scenarios", func(pr PullRequest) string { return pr.Scenarios }, false},
	{"Accepted review findings", func(pr PullRequest) string { return pr.AcceptedFindings }, true},
	{"Declared files", func(pr PullRequest) string { return pr.DeclaredFiles }, false},
	{"Chesterton's fence", func(pr PullRequest) string { return pr.ChestertonsFence }, false},
	{"Plan", func(pr PullRequest) string { return pr.PlanLink }, false},
}

// Body renders the sections in order (What, Working demo, Scenarios,
// Accepted review findings, Declared files, Chesterton's fence, Plan). An
// empty section renders its heading followed by "None.", except Accepted
// review findings, which is left out when empty. An empty Title is an
// error.
func (pr PullRequest) Body() (string, error) {
	if pr.Title == "" {
		return "", errors.New("orchestrator: pull request body: title must not be empty")
	}

	var b strings.Builder
	for _, section := range prBodySections {
		content := section.field(pr)
		if content == "" && section.optional {
			continue
		}
		if b.Len() != 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## ")
		b.WriteString(section.heading)
		b.WriteString("\n\n")

		if content == "" {
			content = "None."
		}
		b.WriteString(content)
	}
	b.WriteString("\n")

	return b.String(), nil
}

// OpenDraftPR renders and validates pr's body, pushes wt's branch, then
// creates a draft pull request from wt.Branch into the default branch
// through go-github, and returns the PR URL and number (PKG5-PLAN.md
// section 8.4). Body is rendered before Push runs, so an invalid PullRequest
// (an empty Title, in particular) errors before the remote branch is ever
// updated, rather than after. It is idempotent under retry: if CreateDraftPR
// fails, it asks GitHub for an existing open PR whose head is wt.Branch and
// whose base is the default branch (FindPRByHead) and returns that one when
// present, so a lost response does not open a duplicate; otherwise it
// returns the create error. It sets nothing on the store; the caller
// records branch and pr_url.
func (o *Orchestrator) OpenDraftPR(ctx context.Context, wt Worktree, pr PullRequest) (url string, number int, err error) {
	body, err := pr.Body()
	if err != nil {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: %w", err)
	}

	if err = o.Push(ctx, wt); err != nil {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: %w", err)
	}

	prURL, prNumber, createErr := o.gh.CreateDraftPR(ctx, o.proj.Owner, o.proj.Repo, wt.branch, o.proj.DefaultBranch, pr.Title, body)
	if createErr == nil {
		o.log.Info("opened draft pr", "branch", wt.branch, "url", prURL, "number", prNumber)
		return prURL, prNumber, nil
	}

	o.log.Warn("create draft pr failed, checking for an existing pr with this head", "branch", wt.branch, "err", createErr)

	existingURL, existingNumber, ok, findErr := o.gh.FindPRByHead(ctx, o.proj.Owner, o.proj.Repo, wt.branch, o.proj.DefaultBranch)
	if findErr != nil {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: create draft pr: %w (and find existing pr also failed: %w)", createErr, findErr)
	}
	if !ok {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: create draft pr: %w", createErr)
	}

	o.log.Info("found existing pr after create failure", "branch", wt.branch, "url", existingURL, "number", existingNumber)

	return existingURL, existingNumber, nil
}
