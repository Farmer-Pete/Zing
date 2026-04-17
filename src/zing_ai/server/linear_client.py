from __future__ import annotations

import logging
from datetime import UTC, datetime, timedelta

import httpx

from zing_ai.server.models_external import LinearIssue

logger = logging.getLogger(__name__)


class LinearAPIError(Exception):
    """Raised when the Linear API returns an error (HTTP non-200 or errors[] in body).

    Attributes:
        status_code: HTTP status code (None for GraphQL errors on HTTP 200).
    """

    def __init__(self, message: str, *, status_code: int | None = None) -> None:
        super().__init__(message)
        self.status_code = status_code


_VIEWER_QUERY = "{ viewer { id } }"

# Linear's default page size is 50 and the maximum is 250. Requesting 250 plus
# paginating via `pageInfo.endCursor` means even large workloads (hundreds of
# open tickets across teams) surface in full, rather than silently truncating.
_ISSUES_PAGE_SIZE = 250

_ISSUES_QUERY = """
query MyOpenIssues($viewerId: ID!, $first: Int!, $after: String) {
  issues(
    first: $first,
    after: $after,
    filter: {
      assignee: { id: { eq: $viewerId } }
      state: { type: { nin: ["completed", "canceled"] } }
    }
  ) {
    nodes {
      id identifier title priority url updatedAt
      state { name type }
      assignee { name }
      team { name }
    }
    pageInfo { hasNextPage endCursor }
  }
}
"""

_COMPLETED_ISSUES_QUERY = """
query MyCompletedIssues($viewerId: ID!, $first: Int!, $after: String, $since: DateTimeComparators) {
  issues(
    first: $first,
    after: $after,
    filter: {
      assignee: { id: { eq: $viewerId } }
      state: { type: { eq: "completed" } }
      updatedAt: $since
    }
  ) {
    nodes {
      id identifier title priority url updatedAt
      state { name type }
      assignee { name }
      team { name }
    }
    pageInfo { hasNextPage endCursor }
  }
}
"""


class LinearClient:
    """Async HTTP client for the Linear GraphQL API."""

    def __init__(self, api_key: str) -> None:
        """Initialise with a Linear personal API key."""
        self._api_key = api_key
        self._viewer_id: str | None = None
        # Single AsyncClient reused across calls (connection pooling).
        self._http = httpx.AsyncClient(
            base_url="https://api.linear.app",
            headers={"Authorization": api_key, "Content-Type": "application/json"},
            timeout=30.0,
        )

    async def aclose(self) -> None:
        """Close the underlying HTTP client and release connections."""
        await self._http.aclose()

    async def _post(self, query: str, variables: dict | None = None) -> dict:
        """Send a GraphQL POST request and return the ``data`` object.

        Raises :class:`LinearAPIError` for both non-200 HTTP responses and
        HTTP 200 responses that carry a non-empty ``errors`` array (Linear's
        convention for rate-limits, auth failures, etc.).
        """
        resp = await self._http.post(
            "/graphql",
            json={"query": query, "variables": variables or {}},
        )
        if resp.status_code != 200:
            logger.warning("Linear HTTP %s response body: %s", resp.status_code, resp.text[:500])
            raise LinearAPIError(f"HTTP {resp.status_code}", status_code=resp.status_code)
        body = resp.json()
        # Linear returns errors[] on HTTP 200 for rate limits, auth failures, etc.
        if body.get("errors"):
            msg = body["errors"][0].get("message", "Unknown Linear error")
            raise LinearAPIError(msg)
        return body["data"]

    async def fetch_viewer_id(self) -> str:
        """Return the authenticated user's Linear viewer ID (cached after first call)."""
        if self._viewer_id is None:
            data = await self._post(_VIEWER_QUERY)
            viewer_id = data["viewer"]["id"]
            assert isinstance(viewer_id, str)
            self._viewer_id = viewer_id
        return self._viewer_id

    async def fetch_my_open_issues(self) -> list[LinearIssue]:
        """Return all open issues assigned to the authenticated user.

        Pages through Linear's cursor-based pagination so workloads larger
        than a single page (default 50, max 250) surface completely rather
        than silently truncating.
        """
        viewer_id = await self.fetch_viewer_id()
        issues: list[LinearIssue] = []
        after: str | None = None
        while True:
            data = await self._post(
                _ISSUES_QUERY,
                {"viewerId": viewer_id, "first": _ISSUES_PAGE_SIZE, "after": after},
            )
            for node in data["issues"]["nodes"]:
                updated_raw: str = node["updatedAt"]
                # datetime.fromisoformat requires +00:00 not Z (Python < 3.11 compat)
                if updated_raw.endswith("Z"):
                    updated_raw = updated_raw[:-1] + "+00:00"
                issues.append(
                    LinearIssue(
                        id=node["id"],
                        identifier=node["identifier"],
                        title=node["title"],
                        state=node["state"]["name"],
                        state_type=node["state"]["type"],
                        priority=node.get("priority", 0),
                        assignee=node["assignee"]["name"] if node.get("assignee") else None,
                        team=node["team"]["name"] if node.get("team") else None,
                        url=node["url"],
                        updated_at=datetime.fromisoformat(updated_raw),
                    )
                )
            page_info = data["issues"].get("pageInfo", {})
            if not page_info.get("hasNextPage"):
                break
            after = page_info.get("endCursor")
            if not after:
                # Defensive: hasNextPage=True but no cursor shouldn't happen
                # per Linear's schema; bail out rather than loop forever.
                break
        return issues

    async def fetch_completed_issues(self) -> list[LinearIssue]:
        """Return issues assigned to the authenticated user that were completed in the last 7 days.

        Powers the Done column of the Kanban board.  Pages through cursor-based
        pagination identically to :meth:`fetch_my_open_issues`.
        """
        viewer_id = await self.fetch_viewer_id()
        since = datetime.now(tz=UTC) - timedelta(days=7)
        since_filter = {"gte": since.strftime("%Y-%m-%dT%H:%M:%S.000Z")}
        issues: list[LinearIssue] = []
        after: str | None = None
        while True:
            data = await self._post(
                _COMPLETED_ISSUES_QUERY,
                {
                    "viewerId": viewer_id,
                    "first": _ISSUES_PAGE_SIZE,
                    "after": after,
                    "since": since_filter,
                },
            )
            for node in data["issues"]["nodes"]:
                updated_raw: str = node["updatedAt"]
                if updated_raw.endswith("Z"):
                    updated_raw = updated_raw[:-1] + "+00:00"
                issues.append(
                    LinearIssue(
                        id=node["id"],
                        identifier=node["identifier"],
                        title=node["title"],
                        state=node["state"]["name"],
                        state_type=node["state"]["type"],
                        priority=node.get("priority", 0),
                        assignee=node["assignee"]["name"] if node.get("assignee") else None,
                        team=node["team"]["name"] if node.get("team") else None,
                        url=node["url"],
                        updated_at=datetime.fromisoformat(updated_raw),
                    )
                )
            page_info = data["issues"].get("pageInfo", {})
            if not page_info.get("hasNextPage"):
                break
            after = page_info.get("endCursor")
            if not after:
                break
        return issues
