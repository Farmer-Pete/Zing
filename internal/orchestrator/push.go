package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Push pushes wt's branch to origin (PKG5-PLAN.md section 8.4). It first
// revalidates wt (8.2): the branch must match the zing/ form, must differ
// from the default branch, and must be the branch actually checked out in
// wt.Dir. That rejects a default or a forged non-zing branch before any git
// command runs. It then verifies every commit in
// <default_branch>..<branch> is signed, using the same signedStatus (8.3)
// CommitTask verifies with: a single unsigned commit aborts the push before
// anything is pushed. Only then does it run
// "git -C <dir> push -u origin refs/heads/<branch>:refs/heads/<branch>", an
// explicit same-name refspec that cannot be reinterpreted as a target on the
// default branch.
func (o *Orchestrator) Push(ctx context.Context, wt Worktree) error {
	if err := o.revalidate(ctx, wt); err != nil {
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

	o.log.Info("pushing branch", "branch", wt.branch, "commits", len(shas))

	refspec := "refs/heads/" + wt.branch + ":refs/heads/" + wt.branch
	if out, runErr := o.run.Run(ctx, wt.dir, "git", "push", "-u", "origin", refspec); runErr != nil {
		return fmt.Errorf("orchestrator: push: git push: %w: %s", runErr, strings.TrimSpace(out))
	}

	o.log.Info("pushed branch", "branch", wt.branch)

	return nil
}

// unpushedShas returns every commit sha in <default_branch>..<branch>, read
// with "git log -z --format=%H" and split on NUL, trimmed, with empty
// fields dropped.
func (o *Orchestrator) unpushedShas(ctx context.Context, wt Worktree) ([]string, error) {
	out, err := o.run.Output(ctx, wt.dir, "git", "log", "-z", "--format=%H", o.proj.DefaultBranch+".."+wt.branch)
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", o.proj.DefaultBranch, wt.branch, err)
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
	DeclaredFiles    string // pre-rendered markdown table
	ChestertonsFence string
	PlanLink         string // a URL or console link
}

// prBodySections is the fixed, ordered shape Body renders: six "## "
// headings, each carrying the PullRequest field it draws from.
var prBodySections = []struct {
	heading string
	field   func(PullRequest) string
}{
	{"What", func(pr PullRequest) string { return pr.What }},
	{"Working demo", func(pr PullRequest) string { return pr.WorkingDemo }},
	{"Scenarios", func(pr PullRequest) string { return pr.Scenarios }},
	{"Declared files", func(pr PullRequest) string { return pr.DeclaredFiles }},
	{"Chesterton's fence", func(pr PullRequest) string { return pr.ChestertonsFence }},
	{"Plan", func(pr PullRequest) string { return pr.PlanLink }},
}

// Body renders the six sections of section 13 in order: What, Working demo,
// Scenarios, Declared files, Chesterton's fence, Plan. An empty section
// renders its heading followed by "None." so the shape is stable. An empty
// Title is an error.
func (pr PullRequest) Body() (string, error) {
	if pr.Title == "" {
		return "", errors.New("orchestrator: pull request body: title must not be empty")
	}

	var b strings.Builder
	for i, section := range prBodySections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## ")
		b.WriteString(section.heading)
		b.WriteString("\n\n")

		content := section.field(pr)
		if content == "" {
			content = "None."
		}
		b.WriteString(content)
	}
	b.WriteString("\n")

	return b.String(), nil
}

// OpenDraftPR pushes wt's branch, then creates a draft pull request from
// wt.Branch into the default branch through go-github, and returns the PR
// URL and number (PKG5-PLAN.md section 8.4). It is idempotent under retry:
// if CreateDraftPR fails, it asks GitHub for an existing open PR whose head
// is wt.Branch (FindPRByHead) and returns that one when present, so a lost
// response does not open a duplicate; otherwise it returns the create
// error. It sets nothing on the store; the caller records branch and
// pr_url.
func (o *Orchestrator) OpenDraftPR(ctx context.Context, wt Worktree, pr PullRequest) (url string, number int, err error) {
	if err = o.Push(ctx, wt); err != nil {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: %w", err)
	}

	body, err := pr.Body()
	if err != nil {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: %w", err)
	}

	prURL, prNumber, createErr := o.gh.CreateDraftPR(ctx, o.proj.Owner, o.proj.Repo, wt.branch, o.proj.DefaultBranch, pr.Title, body)
	if createErr == nil {
		o.log.Info("opened draft pr", "branch", wt.branch, "url", prURL, "number", prNumber)
		return prURL, prNumber, nil
	}

	o.log.Warn("create draft pr failed, checking for an existing pr with this head", "branch", wt.branch, "err", createErr)

	existingURL, existingNumber, ok, findErr := o.gh.FindPRByHead(ctx, o.proj.Owner, o.proj.Repo, wt.branch)
	if findErr != nil {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: create draft pr: %w (and find existing pr also failed: %w)", createErr, findErr)
	}
	if !ok {
		return "", 0, fmt.Errorf("orchestrator: open draft pr: create draft pr: %w", createErr)
	}

	o.log.Info("found existing pr after create failure", "branch", wt.branch, "url", existingURL, "number", existingNumber)

	return existingURL, existingNumber, nil
}
