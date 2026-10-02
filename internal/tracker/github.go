package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"
)

// proposedLabel marks an issue Zing filed on itself (FileTicket). It carries
// no assignee, so the Intake assignee gate excludes it until a human assigns
// one (decision Q16, PKG6-PLAN.md section 4.2).
const proposedLabel = "zing:proposed"

// ghHTTPTimeout bounds every call the real client makes, mirroring
// internal/orchestrator's ghClient (PKG5-PLAN.md section 6).
const ghHTTPTimeout = 30 * time.Second

// perPage is the page size for every paginated GitHub list call.
const perPage = 100

// errNoRepos and errEmptyProjectName are NewGitHub's own construction
// errors, not go-github failures, so they carry no %w cause to wrap.
var (
	errNoRepos          = errors.New("tracker: no repos configured")
	errEmptyProjectName = errors.New("tracker: project name must not be empty")
)

// repo is one repository's owner and name, e.g. {"Farmer-Pete", "Zing"}.
type repo struct{ owner, name string }

// GitHubTracker implements Tracker against go-github v92. repos maps a
// project NAME (the value passed to every Tracker method) to its repo, so
// one tracker serves many projects with one client and one token. login is
// the authenticated user's login, read once by authLogin and cached here
// (design section 10.5): every project shares the one token, so one cached
// login serves all of them.
type GitHubTracker struct {
	c     *github.Client
	repos map[string]repo

	mu    sync.Mutex
	login string
}

// Guarantee the concrete type satisfies the interface without returning the
// interface from the constructor: the repo's ireturn linter allow-lists only
// orchestrator.GitHub, so NewGitHub returns *GitHubTracker and this line
// proves that still satisfies Tracker.
var _ Tracker = (*GitHubTracker)(nil)

// NewGitHub builds the real github tracker. token authenticates the client;
// an empty token is an error (github.NewClient returns "token must not be
// empty"). repos maps each project name to its "owner/name" string; it must
// be non-empty, and every key and every parsed owner/name half must be
// non-empty, else construction returns a project-specific error. It returns
// the concrete *GitHubTracker, not the Tracker interface.
func NewGitHub(token string, repos map[string]string) (*GitHubTracker, error) {
	if len(repos) == 0 {
		return nil, errNoRepos
	}

	parsed := make(map[string]repo, len(repos))
	for project, s := range repos {
		if project == "" {
			return nil, errEmptyProjectName
		}
		r, err := parseRepo(s)
		if err != nil {
			return nil, fmt.Errorf("tracker: project %q: %w", project, err)
		}
		parsed[project] = r
	}

	c, err := github.NewClient(github.WithAuthToken(token), github.WithTimeout(ghHTTPTimeout))
	if err != nil {
		return nil, fmt.Errorf("tracker: new github client: %w", err)
	}

	return &GitHubTracker{c: c, repos: parsed}, nil
}

// parseRepo splits s on its single "/" into a repo. It rejects a missing
// slash, more than one slash, or an empty half.
func parseRepo(s string) (repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return repo{}, fmt.Errorf("tracker: bad repo %q, want owner/name", s)
	}
	return repo{owner: owner, name: name}, nil
}

// repoFor looks up project's repo. A miss means an unknown project, checked
// before any HTTP call by every Tracker method below.
func (g *GitHubTracker) repoFor(project string) (repo, error) {
	r, ok := g.repos[project]
	if !ok {
		return repo{}, fmt.Errorf("tracker: unknown project %q", project)
	}
	return r, nil
}

// Intake returns the open, non-pull-request issues assigned to
// rule.Assignee, paginated to completion. rule.Assignee must name a real
// user: go-github tags IssueListByRepoOptions.Assignee
// `url:"assignee,omitempty"`, so an empty value omits the query parameter
// entirely and GitHub would return every open issue, including unassigned
// zing:proposed ones -- defeating the self-filed exclusion, which relies
// entirely on this server-side filter (self-filed issues carry no assignee,
// so a named assignee filter drops them by construction; once a human
// assigns one it is meant to be picked up, so no separate label check runs
// here). "none" and "*" are GitHub's own special filter values and are
// rejected for the same reason.
func (g *GitHubTracker) Intake(ctx context.Context, project string, rule IntakeRule) ([]Ticket, error) {
	r, err := g.repoFor(project)
	if err != nil {
		return nil, err
	}
	if rule.Assignee == "" || rule.Assignee == "none" || rule.Assignee == "*" {
		return nil, fmt.Errorf("tracker: intake needs a named assignee, got %q", rule.Assignee)
	}

	opts := &github.IssueListByRepoOptions{State: "open", Assignee: rule.Assignee}
	opts.ListOptions.PerPage = perPage

	var tickets []Ticket
	for {
		issues, resp, err := g.c.Issues.ListByRepo(ctx, r.owner, r.name, opts)
		if err != nil {
			return nil, fmt.Errorf("tracker: intake: %w", err)
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			tickets = append(tickets, Ticket{
				Ref:   strconv.Itoa(issue.GetNumber()),
				Title: issue.GetTitle(),
				Body:  issue.GetBody(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}

	return tickets, nil
}

// canonicalRef parses ref as the decimal issue number github Intake emits.
// It requires n > 0 and strconv.Itoa(n) == ref, rejecting "+1", "0", "-3",
// leading zeros, and surrounding whitespace, none of which round-trips to
// the decimal form Intake produces.
func canonicalRef(ref string) (int, error) {
	n, err := strconv.Atoi(ref)
	if err != nil || n <= 0 || strconv.Itoa(n) != ref {
		return 0, fmt.Errorf("tracker: bad ref %q, want a positive decimal issue number", ref)
	}
	return n, nil
}

// Fetch returns the single ticket ref within project.
func (g *GitHubTracker) Fetch(ctx context.Context, project, ref string) (Ticket, error) {
	r, err := g.repoFor(project)
	if err != nil {
		return Ticket{}, err
	}
	n, err := canonicalRef(ref)
	if err != nil {
		return Ticket{}, err
	}

	issue, _, err := g.c.Issues.Get(ctx, r.owner, r.name, n)
	if err != nil {
		return Ticket{}, fmt.Errorf("tracker: fetch: %w", err)
	}
	return Ticket{Ref: strconv.Itoa(issue.GetNumber()), Title: issue.GetTitle(), Body: issue.GetBody()}, nil
}

// Issue returns the single issue ref within project (PKG9-PLAN.md D29,
// manual intake's POST /projects/{id}/pickup): a 404 becomes
// ErrIssueNotFound, a pull request becomes ErrIssueIsPullRequest (checked
// before the state, since GitHub's own IsPullRequest is the more specific
// fact), and a closed issue becomes ErrIssueClosed. Every other error wraps
// the go-github failure, matching Fetch's own "tracker: issue:" prefix
// convention.
func (g *GitHubTracker) Issue(ctx context.Context, project, ref string) (Ticket, error) {
	r, err := g.repoFor(project)
	if err != nil {
		return Ticket{}, err
	}
	n, err := canonicalRef(ref)
	if err != nil {
		return Ticket{}, err
	}

	issue, resp, err := g.c.Issues.Get(ctx, r.owner, r.name, n)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return Ticket{}, ErrIssueNotFound
		}
		return Ticket{}, fmt.Errorf("tracker: issue: %w", err)
	}
	if issue.IsPullRequest() {
		return Ticket{}, ErrIssueIsPullRequest
	}
	if issue.GetState() == "closed" {
		return Ticket{}, ErrIssueClosed
	}
	return Ticket{Ref: strconv.Itoa(issue.GetNumber()), Title: issue.GetTitle(), Body: issue.GetBody()}, nil
}

// Comment posts body against ref within project. body is never logged: it
// may echo fenced or untrusted text, and this method logs nothing itself.
func (g *GitHubTracker) Comment(ctx context.Context, project, ref, body string) error {
	r, err := g.repoFor(project)
	if err != nil {
		return err
	}
	n, err := canonicalRef(ref)
	if err != nil {
		return err
	}

	if _, _, err := g.c.Issues.CreateComment(ctx, r.owner, r.name, n, github.IssueCommentRequest{Body: body}); err != nil {
		return fmt.Errorf("tracker: comment: %w", err)
	}
	return nil
}

// prefixHash turns each ref into "#<ref>" for GitHub auto-linking, leaving a
// ref that already starts with "#" as-is.
func prefixHash(refs []string) []string {
	out := make([]string, len(refs))
	for i, ref := range refs {
		if strings.HasPrefix(ref, "#") {
			out[i] = ref
			continue
		}
		out[i] = "#" + ref
	}
	return out
}

// FileTicket creates t within project as an unassigned issue labelled
// zing:proposed, and returns its new ref, the decimal issue number.
func (g *GitHubTracker) FileTicket(ctx context.Context, project string, t NewTicket) (string, error) {
	r, err := g.repoFor(project)
	if err != nil {
		return "", err
	}

	body := t.Body
	if len(t.DependsOn) > 0 {
		body += "\n\nDepends on " + strings.Join(prefixHash(t.DependsOn), ", ")
	}

	created, _, err := g.c.Issues.Create(ctx, r.owner, r.name, github.CreateIssueRequest{
		Title:  t.Title,
		Body:   new(body),
		Labels: []string{proposedLabel},
	})
	if err != nil {
		return "", fmt.Errorf("tracker: file ticket: %w", err)
	}
	return strconv.Itoa(created.GetNumber()), nil
}

// Collaborators returns project's collaborator logins, paginated like
// Intake. An empty login is skipped.
func (g *GitHubTracker) Collaborators(ctx context.Context, project string) ([]string, error) {
	r, err := g.repoFor(project)
	if err != nil {
		return nil, err
	}

	opts := &github.ListCollaboratorsOptions{}
	opts.PerPage = perPage

	var logins []string
	for {
		users, resp, err := g.c.Repositories.ListCollaborators(ctx, r.owner, r.name, opts)
		if err != nil {
			return nil, fmt.Errorf("tracker: collaborators: %w", err)
		}
		for _, u := range users {
			if login := u.GetLogin(); login != "" {
				logins = append(logins, login)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return logins, nil
}

// Close closes ref within project with state reason "completed" (design
// section 10.5, PKG9-PLAN.md section 8.6): GitHub succeeds the same call
// against an issue that is already closed, so a repeated DONE tick after a
// crash closes it again for free.
func (g *GitHubTracker) Close(ctx context.Context, project, ref string) error {
	r, err := g.repoFor(project)
	if err != nil {
		return err
	}
	n, err := canonicalRef(ref)
	if err != nil {
		return err
	}

	state, reason := "closed", "completed"
	if _, _, err := g.c.Issues.Update(ctx, r.owner, r.name, n, github.UpdateIssueRequest{State: &state, StateReason: &reason}); err != nil {
		return fmt.Errorf("tracker: close: %w", err)
	}
	return nil
}

// authLogin returns the tracker's own authenticated login, read once with
// Users.Get(ctx, "") and cached on the GitHubTracker (design section 10.5).
// An error here is never cached, so the next call -- and so the next tick --
// tries again.
func (g *GitHubTracker) authLogin(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.login != "" {
		return g.login, nil
	}
	u, _, err := g.c.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("tracker: viewer: %w", err)
	}
	g.login = u.GetLogin()
	return g.login, nil
}

// CommentContains reports whether ref already carries a comment containing
// needle, authored by the tracker's own authenticated login, reading every
// page of the issue's comments (design section 10.5): a comment from any
// other account never counts, so a spoofed marker cannot suppress a comment
// Zing must post at most once (PKG9-PLAN.md section 8.2, 8.6, 11).
func (g *GitHubTracker) CommentContains(ctx context.Context, project, ref, needle string) (bool, error) {
	r, err := g.repoFor(project)
	if err != nil {
		return false, err
	}
	n, err := canonicalRef(ref)
	if err != nil {
		return false, err
	}
	login, err := g.authLogin(ctx)
	if err != nil {
		return false, err
	}

	opts := &github.IssueListCommentsOptions{}
	opts.PerPage = perPage
	for {
		comments, resp, err := g.c.Issues.ListComments(ctx, r.owner, r.name, n, opts)
		if err != nil {
			return false, fmt.Errorf("tracker: comment contains: %w", err)
		}
		for _, c := range comments {
			if c.GetUser().GetLogin() != login {
				continue
			}
			if strings.Contains(c.GetBody(), needle) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, nil
}
