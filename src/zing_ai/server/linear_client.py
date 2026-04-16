from __future__ import annotations

from datetime import datetime

import httpx

from zing_ai.server.models_external import LinearIssue


class LinearAPIError(Exception):
    """Raised when the Linear API returns an error (HTTP non-200 or errors[] in body)."""


_VIEWER_QUERY = "{ viewer { id } }"

_ISSUES_QUERY = """
query MyOpenIssues($viewerId: String!) {
  issues(filter: {
    assignee: { id: { eq: $viewerId } }
    state: { type: { nin: ["completed", "canceled"] } }
  }) { nodes { id identifier title state { name } assignee { name } team { name } url updatedAt } }
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
            raise LinearAPIError(f"HTTP {resp.status_code}: {resp.text[:200]}")
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
        """Return all open issues assigned to the authenticated user."""
        viewer_id = await self.fetch_viewer_id()
        data = await self._post(_ISSUES_QUERY, {"viewerId": viewer_id})
        issues: list[LinearIssue] = []
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
                    assignee=node["assignee"]["name"] if node.get("assignee") else None,
                    team=node["team"]["name"] if node.get("team") else None,
                    url=node["url"],
                    updated_at=datetime.fromisoformat(updated_raw),
                )
            )
        return issues
