from __future__ import annotations

import asyncio
import logging
from datetime import UTC, datetime, timedelta

import httpx

from zing_ai.server.models_external import CICheck, GitHubPR

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

    # CI status lives at commits -> last commit -> statusCheckRollup
    commits = pr.get("commits") or {}
    commit_nodes = commits.get("nodes") or []
    ci_status: str | None = None
    ci_checks: list[CICheck] = []
    if commit_nodes:
        rollup = (commit_nodes[-1].get("commit") or {}).get("statusCheckRollup")
        if rollup:
            rollup_state = rollup.get("state")
            ci_status = rollup_state.lower() if rollup_state else None

            # Parse individual check runs / status contexts
            contexts = rollup.get("contexts") or {}
            for ctx in contexts.get("nodes") or []:
                name = ctx.get("name") or ctx.get("context")
                if not name:
                    continue  # skip entries with no name
                url = ctx.get("detailsUrl") or ctx.get("targetUrl")

                if "status" in ctx:
                    # CheckRun: has status + conclusion
                    status = (ctx.get("status") or "").lower()
                    conclusion = ctx.get("conclusion")
                    if conclusion:
                        conclusion = conclusion.lower()
                else:
                    # StatusContext: has state only, which is the final result
                    raw = (ctx.get("state") or "").lower()
                    status = "completed" if raw in ("success", "failure", "error") else raw
                    conclusion = raw if raw in ("success", "failure", "error") else None

                ci_checks.append(CICheck(name=name, status=status, conclusion=conclusion, url=url))

    # mergeable: GraphQL returns MERGEABLE / CONFLICTING / UNKNOWN
    raw_mergeable = pr.get("mergeable") or "UNKNOWN"
    mergeable_state = raw_mergeable.lower()

    author_obj = pr.get("author") or {}
    author = author_obj.get("login") or ""

    # mergedAt is present for merged PRs; parse to datetime if available.
    merged_at: datetime | None = None
    raw_merged_at = pr.get("mergedAt")
    if raw_merged_at:
        merged_at = datetime.fromisoformat(raw_merged_at.replace("Z", "+00:00"))

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
        ci_checks=ci_checks,
        url=pr["url"],
        updated_at=datetime.fromisoformat(pr["updatedAt"].replace("Z", "+00:00")),
        merged_at=merged_at,
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
                      statusCheckRollup {
                        state
                        contexts(first: 50) {
                          nodes {
                            ... on CheckRun {
                              name
                              status
                              conclusion
                              detailsUrl
                            }
                            ... on StatusContext {
                              context
                              state
                              targetUrl
                            }
                          }
                        }
                      }
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

    async def fetch_recent_prs(
        self,
        repos: list[str],
        username: str,
        since_days: int = 7,
    ) -> list[GitHubPR]:
        """Return recently merged PRs from *repos* authored or reviewed by *username*.

        For each repo in *repos* (``owner/name`` format), runs a GraphQL query for
        merged PRs ordered by last updated. Results are filtered client-side to keep
        only PRs where:

        - ``merged_at`` is within the last *since_days* days, AND
        - the PR was authored by *username* OR *username* appears in ``reviewRequests``.

        Deduplication is by ``(repo, number)`` key.
        """
        cutoff = datetime.now(UTC) - timedelta(days=since_days)
        seen: set[tuple[str, int]] = set()
        results: list[GitHubPR] = []

        query = """
        query($owner: String!, $repo: String!) {
          repository(owner: $owner, name: $repo) {
            pullRequests(
              states: [MERGED], first: 50,
              orderBy: {field: UPDATED_AT, direction: DESC}
            ) {
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
                mergedAt
                url
                updatedAt
                reviewRequests(first: 10) {
                  nodes {
                    requestedReviewer {
                      ... on User { login }
                    }
                  }
                }
                reviews(first: 20) {
                  nodes {
                    author { login }
                  }
                }
                commits(last: 1) {
                  nodes {
                    commit {
                      statusCheckRollup {
                        state
                        contexts(first: 50) {
                          nodes {
                            ... on CheckRun {
                              name
                              status
                              conclusion
                              detailsUrl
                            }
                            ... on StatusContext {
                              context
                              state
                              targetUrl
                            }
                          }
                        }
                      }
                    }
                  }
                }
              }
            }
          }
        }
        """

        async def _fetch_one_repo(repo_str: str) -> list[tuple[GitHubPR, dict]]:
            """Fetch merged PRs for one repo, returning (pr, node) pairs."""
            owner, _, name = repo_str.partition("/")
            try:
                data = await self._graphql(query, {"owner": owner, "repo": name})
            except GitHubAPIError:
                logger.warning("fetch_recent_prs: skipping repo %s due to API error", repo_str)
                return []
            repository = data.get("repository") or {}
            pull_requests = repository.get("pullRequests") or {}
            nodes = pull_requests.get("nodes") or []
            return [(_map_pr(node, repo=repo_str), node) for node in nodes]

        # Fetch all repos concurrently.
        repo_results = await asyncio.gather(
            *(_fetch_one_repo(r) for r in repos), return_exceptions=True
        )

        for repo, result in zip(repos, repo_results, strict=True):
            if isinstance(result, BaseException):
                logger.warning("fetch_recent_prs: skipping repo %s: %s", repo, result)
                continue
            for pr, node in result:
                # Skip if outside the time window.
                if pr.merged_at is None or pr.merged_at < cutoff:
                    continue

                # Skip if user is not the author and did not review.
                # For merged PRs, requested_reviewers is cleared by GitHub,
                # so we check the reviews list instead.
                is_author = pr.author == username
                reviews_data = node.get("reviews") or {}
                review_authors = {
                    r.get("author", {}).get("login", "") for r in (reviews_data.get("nodes") or [])
                }
                is_reviewer = username in review_authors
                if not is_author and not is_reviewer:
                    continue

                key = (repo, pr.number)
                if key in seen:
                    continue
                seen.add(key)
                results.append(pr)

        return results
