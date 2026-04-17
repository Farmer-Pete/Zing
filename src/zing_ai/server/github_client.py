from __future__ import annotations

import logging
from datetime import datetime

import httpx

from zing_ai.server.models_external import GitHubPR

logger = logging.getLogger(__name__)


class GitHubAPIError(Exception):
    """Raised when the GitHub API returns a non-200 HTTP response.

    Attributes:
        status_code: HTTP status code from the failed response.
    """

    def __init__(self, message: str, *, status_code: int | None = None) -> None:
        super().__init__(message)
        self.status_code = status_code


def _next_link(link_header: str | None) -> str | None:
    """Return the ``rel="next"`` URL from a GitHub ``Link`` header, if present.

    GitHub paginates with ``Link: <url>; rel="next", <url>; rel="last"``. We
    only care about the ``next`` target; when exhausted, the server omits
    ``rel="next"`` and we stop iterating.
    """
    if not link_header:
        return None
    for part in link_header.split(","):
        segments = [s.strip() for s in part.split(";")]
        if len(segments) < 2:
            continue
        url_segment, *rel_segments = segments
        is_next = any(rel == 'rel="next"' for rel in rel_segments)
        if is_next and url_segment.startswith("<") and url_segment.endswith(">"):
            return url_segment[1:-1]
    return None


def _map_pr(pr: dict, *, repo: str = "") -> GitHubPR:
    """Map a raw GitHub GraphQL PR node dict to a :class:`GitHubPR` model."""
    raw_state = pr["state"]
    if raw_state == "MERGED":
        state = "merged"
    elif raw_state == "CLOSED":
        state = "closed"
    else:
        state = "open"

    # reviewRequests nodes contain requestedReviewer objects; surface user logins only.
    review_requests = pr.get("reviewRequests") or {}
    reviewer_nodes = review_requests.get("nodes") or []
    requested_reviewers = [
        node["requestedReviewer"]["login"]
        for node in reviewer_nodes
        if node.get("requestedReviewer") and "login" in node["requestedReviewer"]
    ]

    # CI status lives at commits -> last commit -> statusCheckRollup -> state
    commits = pr.get("commits") or {}
    commit_nodes = commits.get("nodes") or []
    ci_status: str | None = None
    if commit_nodes:
        rollup = (commit_nodes[-1].get("commit") or {}).get("statusCheckRollup")
        if rollup:
            ci_status = rollup.get("state")

    # mergeable: GraphQL returns MERGEABLE / CONFLICTING / UNKNOWN
    raw_mergeable = pr.get("mergeable") or "UNKNOWN"
    mergeable_state = raw_mergeable.lower()

    author_obj = pr.get("author") or {}
    author = author_obj.get("login") or ""

    return GitHubPR(
        number=pr["number"],
        title=pr["title"],
        state=state,
        draft=pr["isDraft"],
        head_ref=pr["headRefName"],
        base_ref=pr["baseRefName"],
        body=pr.get("body"),
        author=author,
        repo=repo,
        requested_reviewers=requested_reviewers,
        review_decision=pr.get("reviewDecision"),
        mergeable_state=mergeable_state,
        ci_status=ci_status,
        url=pr["url"],
        updated_at=datetime.fromisoformat(pr["updatedAt"].replace("Z", "+00:00")),
    )


class GitHubClient:
    """Async HTTP client for the GitHub REST API."""

    def __init__(self, token: str) -> None:
        """Initialise with a GitHub personal access token."""
        self._http = httpx.AsyncClient(
            base_url="https://api.github.com",
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
            timeout=30.0,
        )
        self._username: str | None = None

    async def aclose(self) -> None:
        """Close the underlying HTTP client and release connections."""
        await self._http.aclose()

    async def fetch_current_user(self) -> str:
        """Return the authenticated GitHub username (cached after first call)."""
        if self._username is None:
            r = await self._http.get("/user")
            if r.status_code != 200:
                logger.warning("GitHub HTTP %s on /user: %s", r.status_code, r.text[:500])
                raise GitHubAPIError(f"HTTP {r.status_code}", status_code=r.status_code)
            login = r.json()["login"]
            assert isinstance(login, str)
            self._username = login
        return self._username

    async def fetch_writable_repos(self) -> list[str]:
        """Return ``owner/name`` strings for every repo the token can push to.

        Uses ``GET /user/repos`` with ``affiliation=owner,collaborator,organization_member``
        and filters to repos where ``permissions.push`` is true. Follows Link
        pagination (max 20 pages = 2000 repos).
        """
        results: list[str] = []
        next_url: str | None = "/user/repos"
        next_params: dict[str, str | int] | None = {
            "affiliation": "owner,collaborator,organization_member",
            "per_page": 100,
            "sort": "pushed",
        }
        MAX_PAGES = 20
        for _ in range(MAX_PAGES):
            if next_url is None:
                break
            r = await self._http.get(next_url, params=next_params)
            if r.status_code != 200:
                logger.warning("GitHub HTTP %s on %s: %s", r.status_code, next_url, r.text[:500])
                raise GitHubAPIError(f"HTTP {r.status_code}", status_code=r.status_code)
            for repo in r.json():
                perms = repo.get("permissions", {})
                if perms.get("push"):
                    results.append(repo["full_name"])
            next_url = _next_link(r.headers.get("Link"))
            next_params = None
        return results

    async def _graphql(self, query: str, variables: dict) -> dict:
        """Execute a GitHub GraphQL query and return the ``data`` payload.

        Raises :class:`GitHubAPIError` on HTTP errors or when the response
        contains a top-level ``errors`` array (HTTP 200 with GraphQL errors).
        """
        r = await self._http.post(
            "https://api.github.com/graphql",
            json={"query": query, "variables": variables},
        )
        if r.status_code != 200:
            logger.warning("GitHub GraphQL HTTP %s: %s", r.status_code, r.text[:500])
            raise GitHubAPIError(f"HTTP {r.status_code}", status_code=r.status_code)
        payload = r.json()
        if payload.get("errors"):
            msg = payload["errors"][0].get("message", "GraphQL error")
            logger.warning("GitHub GraphQL error: %s", msg)
            raise GitHubAPIError(msg)
        return payload.get("data") or {}

    async def fetch_open_prs(self, repo: str) -> list[GitHubPR]:
        """Return all open pull requests for *repo* (``owner/name`` format).

        Uses the GitHub GraphQL API to fetch PR data including author,
        reviewDecision, and CI status in a single query. Raises
        :class:`GitHubAPIError` for HTTP errors or GraphQL errors.
        """
        owner, _, name = repo.partition("/")
        query = """
        query($owner: String!, $repo: String!) {
          repository(owner: $owner, name: $repo) {
            pullRequests(states: OPEN, first: 100, orderBy: {field: UPDATED_AT, direction: DESC}) {
              nodes {
                number
                title
                state
                isDraft
                headRefName
                baseRefName
                body
                author { login }
                reviewDecision
                mergeable
                url
                updatedAt
                reviewRequests(first: 10) {
                  nodes {
                    requestedReviewer {
                      ... on User { login }
                    }
                  }
                }
                commits(last: 1) {
                  nodes {
                    commit {
                      statusCheckRollup { state }
                    }
                  }
                }
              }
            }
          }
        }
        """
        data = await self._graphql(query, {"owner": owner, "repo": name})
        repository = data.get("repository") or {}
        pull_requests = repository.get("pullRequests") or {}
        nodes = pull_requests.get("nodes") or []
        return [_map_pr(pr, repo=repo) for pr in nodes]
