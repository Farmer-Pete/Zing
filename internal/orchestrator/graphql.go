package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v92/github"
)

// graphQL is a small client over go-github's own request and response
// machinery, so every GraphQL call reuses c's token, base URL, user agent,
// and rate-limit handling instead of a second HTTP client or a new
// dependency (PKG9-PLAN.md section 10.4, D6).
type graphQL struct{ c *github.Client }

// gqlRequestBody is the POST body every GraphQL call sends.
type gqlRequestBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// gqlEnvelope is the shape of every GraphQL response: "errors" is non-empty
// only on failure, and "data" decodes into the caller's out on success.
type gqlEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// gqlError is one entry of a GraphQL response's "errors" array. "type"
// classifies the failure; GitHub sends NOT_FOUND for a stale or wrong id.
type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// do posts {"query": q, "variables": vars} to "graphql" through c.NewRequest
// and c.Do, so the token, base URL, user agent, and rate-limit handling are
// go-github's (PKG9-PLAN.md section 10.4). A non-empty "errors" array is an
// error: type NOT_FOUND wraps ErrGitHubNotFound; any other is
// "orchestrator: graphql <op>: <first message>". "data" decodes into out
// when out is non-nil.
func (g graphQL) do(ctx context.Context, op, q string, vars map[string]any, out any) error {
	req, err := g.c.NewRequest(ctx, http.MethodPost, "graphql", gqlRequestBody{Query: q, Variables: vars})
	if err != nil {
		return fmt.Errorf("orchestrator: graphql %s: build request: %w", op, err)
	}

	var env gqlEnvelope
	if _, err := g.c.Do(req, &env); err != nil {
		return classifyGitHubErr(err)
	}

	if len(env.Errors) > 0 {
		first := env.Errors[0]
		if first.Type == "NOT_FOUND" {
			return fmt.Errorf("%w: %s", ErrGitHubNotFound, first.Message)
		}
		return fmt.Errorf("orchestrator: graphql %s: %s", op, first.Message)
	}

	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("orchestrator: graphql %s: decode response: %w", op, err)
		}
	}
	return nil
}

// The six documents, exact (PKG9-PLAN.md section 10.4), checked against
// GitHub's published GraphQL schema (input and payload field names,
// connection and type shapes) before this file was written.
const (
	qZingThreads = `query ZingThreads($owner: String!, $repo: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line
          comments(last: 100) { nodes { id body createdAt updatedAt author { login } } }
        }
      }
    }
  }
}`

	qZingThreadComments = `query ZingThreadComments($thread: ID!, $after: String) {
  node(id: $thread) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { body author { login } } }
    }
  }
}`

	qZingReply = `mutation ZingReply($thread: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $thread, body: $body}) { comment { id } }
}`

	qZingResolve = `mutation ZingResolve($thread: ID!) {
  resolveReviewThread(input: {threadId: $thread}) { thread { id isResolved } }
}`

	qZingReady = `mutation ZingReady($pr: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $pr}) { pullRequest { isDraft } }
}`

	qZingDraft = `mutation ZingDraft($pr: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $pr}) { pullRequest { isDraft } }
}`
)

// maxThreadPages bounds ListThreads to 10 pages of 100 (1000 threads): an
// eleventh page is refused rather than fetched (PKG9-PLAN.md section 10.4).
const maxThreadPages = 10

// gqlVarThread is the "$thread" variable name the ZingThreadComments,
// ZingReply, and ZingResolve documents share (goconst: three call sites in
// github.go build its variables map with this key).
const gqlVarThread = "thread"

// gqlActor is a GraphQL Actor's login, embedded wherever the six documents
// ask for comments(...) { nodes { author { login } } }.
type gqlActor struct {
	Login string `json:"login"`
}

// login returns a's login, or "ghost" when GitHub reports no author
// (PKG9-PLAN.md section 10.3's ThreadComment.Author).
func (a *gqlActor) login() string {
	if a == nil {
		return "ghost"
	}
	return a.Login
}

// gqlThreadComment is one node of a ZingThreads or ZingThreadComments
// comments connection.
type gqlThreadComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Author    *gqlActor `json:"author"`
}

func (c gqlThreadComment) toThreadComment() ThreadComment {
	return ThreadComment{ID: c.ID, Author: c.Author.login(), Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// gqlPageInfo is a GraphQL connection's pageInfo, common to every paginated
// field the six documents read.
type gqlPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// gqlThreadNode is one node of ZingThreads' reviewThreads connection.
type gqlThreadNode struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       *int   `json:"line"` // null when GitHub reports no line
	Comments   struct {
		Nodes []gqlThreadComment `json:"nodes"`
	} `json:"comments"`
}

func (n gqlThreadNode) toThread() Thread {
	line := 0
	if n.Line != nil {
		line = *n.Line
	}
	comments := make([]ThreadComment, 0, len(n.Comments.Nodes))
	for _, c := range n.Comments.Nodes {
		comments = append(comments, c.toThreadComment())
	}
	return Thread{
		ID: n.ID, IsResolved: n.IsResolved, IsOutdated: n.IsOutdated, Path: n.Path, Line: line,
		Comments: comments,
	}
}

// zingThreadsData is ZingThreads' "data".
type zingThreadsData struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				PageInfo gqlPageInfo     `json:"pageInfo"`
				Nodes    []gqlThreadNode `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// zingThreadCommentsData is ZingThreadComments' "data".
type zingThreadCommentsData struct {
	Node struct {
		Comments struct {
			PageInfo gqlPageInfo        `json:"pageInfo"`
			Nodes    []gqlThreadComment `json:"nodes"`
		} `json:"comments"`
	} `json:"node"`
}

// zingResolveData is ZingResolve's "data".
type zingResolveData struct {
	ResolveReviewThread struct {
		Thread struct {
			IsResolved bool `json:"isResolved"`
		} `json:"thread"`
	} `json:"resolveReviewThread"`
}

// zingReadyData is ZingReady's "data".
type zingReadyData struct {
	MarkPullRequestReadyForReview struct {
		PullRequest struct {
			IsDraft bool `json:"isDraft"`
		} `json:"pullRequest"`
	} `json:"markPullRequestReadyForReview"`
}

// zingDraftData is ZingDraft's "data".
type zingDraftData struct {
	ConvertPullRequestToDraft struct {
		PullRequest struct {
			IsDraft bool `json:"isDraft"`
		} `json:"pullRequest"`
	} `json:"convertPullRequestToDraft"`
}
