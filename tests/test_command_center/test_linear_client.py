from __future__ import annotations

import unittest
from datetime import UTC, datetime

import httpx
import respx

from zing_ai.server.linear_client import LinearAPIError, LinearClient

_VIEWER_RESPONSE = {"data": {"viewer": {"id": "user-abc-123"}}}

_ISSUES_RESPONSE = {
    "data": {
        "issues": {
            "nodes": [
                {
                    "id": "issue-1",
                    "identifier": "BAK-1",
                    "title": "First issue",
                    "state": {"name": "In Progress"},
                    "assignee": {"name": "Alice"},
                    "team": {"name": "Backend"},
                    "url": "https://linear.app/issue/BAK-1",
                    "updatedAt": "2026-01-15T10:30:00.000Z",
                },
                {
                    "id": "issue-2",
                    "identifier": "BAK-2",
                    "title": "Second issue",
                    "state": {"name": "Todo"},
                    "assignee": None,
                    "team": {"name": "Backend"},
                    "url": "https://linear.app/issue/BAK-2",
                    "updatedAt": "2026-02-20T08:00:00+00:00",
                },
            ],
            "pageInfo": {"hasNextPage": False, "endCursor": None},
        }
    }
}


class TestLinearClient(unittest.IsolatedAsyncioTestCase):
    """Tests for LinearClient."""

    @respx.mock
    async def test_fetch_viewer_id_caches(self) -> None:
        """Second call to fetch_viewer_id must not hit the API again."""
        route = respx.post("https://api.linear.app/graphql").mock(
            return_value=httpx.Response(200, json=_VIEWER_RESPONSE)
        )
        client = LinearClient(api_key="test-key")
        try:
            first = await client.fetch_viewer_id()
            second = await client.fetch_viewer_id()
        finally:
            await client.aclose()

        self.assertEqual(first, "user-abc-123")
        self.assertEqual(second, "user-abc-123")
        # The route should only have been called once (caching)
        self.assertEqual(route.call_count, 1)

    @respx.mock
    async def test_fetch_my_open_issues(self) -> None:
        """fetch_my_open_issues returns a list of LinearIssue objects parsed from nodes."""
        respx.post("https://api.linear.app/graphql").mock(
            side_effect=[
                httpx.Response(200, json=_VIEWER_RESPONSE),
                httpx.Response(200, json=_ISSUES_RESPONSE),
            ]
        )
        client = LinearClient(api_key="test-key")
        try:
            issues = await client.fetch_my_open_issues()
        finally:
            await client.aclose()

        self.assertEqual(len(issues), 2)

        first = issues[0]
        self.assertEqual(first.id, "issue-1")
        self.assertEqual(first.identifier, "BAK-1")
        self.assertEqual(first.title, "First issue")
        self.assertEqual(first.state, "In Progress")
        self.assertEqual(first.assignee, "Alice")
        self.assertEqual(first.team, "Backend")
        self.assertEqual(first.url, "https://linear.app/issue/BAK-1")
        self.assertIsInstance(first.updated_at, datetime)
        self.assertEqual(
            first.updated_at,
            datetime(2026, 1, 15, 10, 30, 0, tzinfo=UTC),
        )

        second = issues[1]
        self.assertIsNone(second.assignee)
        self.assertEqual(second.identifier, "BAK-2")

    @respx.mock
    async def test_fetch_my_open_issues_handles_null_team(self) -> None:
        """Triage issues come back with team=null; the parser must not raise."""
        issues_response = {
            "data": {
                "issues": {
                    "nodes": [
                        {
                            "id": "issue-triage",
                            "identifier": "TRI-1",
                            "title": "Untriaged",
                            "state": {"name": "Triage"},
                            "assignee": {"name": "Alice"},
                            "team": None,
                            "url": "https://linear.app/issue/TRI-1",
                            "updatedAt": "2026-01-15T10:30:00Z",
                        }
                    ],
                    "pageInfo": {"hasNextPage": False, "endCursor": None},
                }
            }
        }
        respx.post("https://api.linear.app/graphql").mock(
            side_effect=[
                httpx.Response(200, json=_VIEWER_RESPONSE),
                httpx.Response(200, json=issues_response),
            ]
        )
        client = LinearClient(api_key="test-key")
        try:
            issues = await client.fetch_my_open_issues()
        finally:
            await client.aclose()

        self.assertEqual(len(issues), 1)
        self.assertIsNone(issues[0].team)

    @respx.mock
    async def test_fetch_my_open_issues_empty_nodes(self) -> None:
        """Empty ``nodes`` array must yield an empty list, not raise."""
        empty_response = {
            "data": {
                "issues": {
                    "nodes": [],
                    "pageInfo": {"hasNextPage": False, "endCursor": None},
                }
            }
        }
        respx.post("https://api.linear.app/graphql").mock(
            side_effect=[
                httpx.Response(200, json=_VIEWER_RESPONSE),
                httpx.Response(200, json=empty_response),
            ]
        )
        client = LinearClient(api_key="test-key")
        try:
            issues = await client.fetch_my_open_issues()
        finally:
            await client.aclose()

        self.assertEqual(issues, [])

    @respx.mock
    async def test_fetch_my_open_issues_paginates(self) -> None:
        """Multiple pages must be fetched and concatenated in order."""
        page_1 = {
            "data": {
                "issues": {
                    "nodes": [
                        {
                            "id": "p1-1",
                            "identifier": "BAK-10",
                            "title": "Page 1, issue 1",
                            "state": {"name": "In Progress"},
                            "assignee": {"name": "Alice"},
                            "team": {"name": "Backend"},
                            "url": "https://linear.app/issue/BAK-10",
                            "updatedAt": "2026-01-01T00:00:00Z",
                        }
                    ],
                    "pageInfo": {"hasNextPage": True, "endCursor": "cursor-1"},
                }
            }
        }
        page_2 = {
            "data": {
                "issues": {
                    "nodes": [
                        {
                            "id": "p2-1",
                            "identifier": "BAK-11",
                            "title": "Page 2, issue 1",
                            "state": {"name": "Todo"},
                            "assignee": {"name": "Bob"},
                            "team": {"name": "Backend"},
                            "url": "https://linear.app/issue/BAK-11",
                            "updatedAt": "2026-02-01T00:00:00Z",
                        }
                    ],
                    "pageInfo": {"hasNextPage": False, "endCursor": "cursor-2"},
                }
            }
        }
        respx.post("https://api.linear.app/graphql").mock(
            side_effect=[
                httpx.Response(200, json=_VIEWER_RESPONSE),
                httpx.Response(200, json=page_1),
                httpx.Response(200, json=page_2),
            ]
        )
        client = LinearClient(api_key="test-key")
        try:
            issues = await client.fetch_my_open_issues()
        finally:
            await client.aclose()

        self.assertEqual([i.identifier for i in issues], ["BAK-10", "BAK-11"])

    @respx.mock
    async def test_http_500_raises(self) -> None:
        """A non-200 HTTP response must raise LinearAPIError."""
        respx.post("https://api.linear.app/graphql").mock(
            return_value=httpx.Response(500, text="Internal Server Error")
        )
        client = LinearClient(api_key="test-key")
        try:
            with self.assertRaises(LinearAPIError) as ctx:
                await client.fetch_viewer_id()
        finally:
            await client.aclose()

        self.assertIn("HTTP 500", str(ctx.exception))

    @respx.mock
    async def test_http_200_with_errors_raises(self) -> None:
        """HTTP 200 with errors[] must raise LinearAPIError with the error message."""
        error_body = {"data": None, "errors": [{"message": "rate limited"}]}
        respx.post("https://api.linear.app/graphql").mock(
            return_value=httpx.Response(200, json=error_body)
        )
        client = LinearClient(api_key="test-key")
        try:
            with self.assertRaises(LinearAPIError) as ctx:
                await client.fetch_viewer_id()
        finally:
            await client.aclose()

        self.assertEqual(str(ctx.exception), "rate limited")

    @respx.mock
    async def test_aclose_closes_client(self) -> None:
        """aclose must close the underlying httpx.AsyncClient."""
        client = LinearClient(api_key="test-key")
        self.assertFalse(client._http.is_closed)
        await client.aclose()
        self.assertTrue(client._http.is_closed)


if __name__ == "__main__":
    unittest.main()
