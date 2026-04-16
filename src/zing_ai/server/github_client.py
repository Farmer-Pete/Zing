from __future__ import annotations

from datetime import datetime

import httpx

from zing_ai.server.models_external import GitHubPR


class GitHubAPIError(Exception):
    """Raised when the GitHub API returns a non-200 HTTP response."""


def _map_pr(pr: dict) -> GitHubPR:
    """Map a raw GitHub API PR dict to a :class:`GitHubPR` model."""
    raw_state = pr["state"]
    merged_at = pr.get("merged_at")
    if raw_state == "closed" and merged_at is None:
        state = "closed"
    elif merged_at is not None:
        state = "merged"
    else:
        state = "open"

    return GitHubPR(
        number=pr["number"],
        title=pr["title"],
        state=state,
        draft=pr["draft"],
        head_ref=pr["head"]["ref"],
        base_ref=pr["base"]["ref"],
        body=pr.get("body"),
        # `requested_reviewers` from GitHub mixes user objects ({"login": ...}) and
        # team objects ({"slug": ..., "name": ...}). v1 surfaces user reviewers only;
        # team-review requests don't yet drive an inbox item.
        requested_reviewers=[u["login"] for u in pr.get("requested_reviewers", []) if "login" in u],
        # TODO(v2): review_decision requires a follow-up GET /repos/{repo}/pulls/{n}/reviews
        # or may be available via the GraphQL API. Not present in REST list endpoint; set None.
        review_decision=None,
        mergeable_state=pr.get("mergeable_state") or "unknown",
        # TODO(v2): ci_status requires a follow-up GET /repos/{repo}/commits/{sha}/check-runs
        ci_status=None,
        url=pr["html_url"],
        updated_at=datetime.fromisoformat(pr["updated_at"].replace("Z", "+00:00")),
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
                raise GitHubAPIError(f"HTTP {r.status_code}: {r.text[:200]}")
            login = r.json()["login"]
            assert isinstance(login, str)
            self._username = login
        return self._username

    async def fetch_open_prs(self, repo: str) -> list[GitHubPR]:
        """Return all open pull requests for *repo* (``owner/name`` format).

        Raises :class:`GitHubAPIError` for non-200 HTTP responses.
        """
        r = await self._http.get(
            f"/repos/{repo}/pulls",
            params={"state": "open", "per_page": 100},
        )
        if r.status_code != 200:
            raise GitHubAPIError(f"HTTP {r.status_code}: {r.text[:200]}")
        return [_map_pr(pr) for pr in r.json()]
