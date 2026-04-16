from __future__ import annotations

import unittest
from datetime import UTC, datetime

import httpx
import respx

from zing_ai.server.github_client import GitHubAPIError, GitHubClient

_USER_RESPONSE = {"login": "octocat", "id": 1}

_PR_LIST_RESPONSE = [
    {
        "number": 42,
        "title": "Add feature X",
        "state": "open",
        "draft": False,
        "head": {"ref": "feature/x"},
        "base": {"ref": "main"},
        "body": "This adds feature X.",
        "requested_reviewers": [{"login": "alice"}, {"login": "bob"}],
        "mergeable_state": "clean",
        "merged_at": None,
        "html_url": "https://github.com/owner/repo/pull/42",
        "updated_at": "2026-03-10T12:00:00Z",
    },
    {
        "number": 43,
        "title": "Fix bug Y",
        "state": "open",
        "draft": True,
        "head": {"ref": "fix/bug-y"},
        "base": {"ref": "develop"},
        "body": None,
        "requested_reviewers": [],
        "mergeable_state": "unknown",
        "merged_at": None,
        "html_url": "https://github.com/owner/repo/pull/43",
        "updated_at": "2026-03-11T08:30:00Z",
    },
    {
        "number": 44,
        "title": "Update deps",
        "state": "open",
        "draft": False,
        "head": {"ref": "chore/deps"},
        "base": {"ref": "main"},
        "body": "Bump all the things.",
        "requested_reviewers": [{"login": "carol"}],
        "mergeable_state": None,
        "merged_at": None,
        "html_url": "https://github.com/owner/repo/pull/44",
        "updated_at": "2026-03-12T16:45:00+00:00",
    },
]


class TestGitHubClient(unittest.IsolatedAsyncioTestCase):
    """Tests for GitHubClient."""

    @respx.mock
    async def test_fetch_current_user_caches(self) -> None:
        """Second call to fetch_current_user must not hit the API again."""
        route = respx.get("https://api.github.com/user").mock(
            return_value=httpx.Response(200, json=_USER_RESPONSE)
        )
        client = GitHubClient(token="ghp_test")
        try:
            first = await client.fetch_current_user()
            second = await client.fetch_current_user()
        finally:
            await client.aclose()

        self.assertEqual(first, "octocat")
        self.assertEqual(second, "octocat")
        self.assertEqual(route.call_count, 1)

    @respx.mock
    async def test_fetch_open_prs(self) -> None:
        """fetch_open_prs returns 3 GitHubPR objects with all fields populated correctly."""
        respx.get("https://api.github.com/repos/owner/repo/pulls").mock(
            return_value=httpx.Response(200, json=_PR_LIST_RESPONSE)
        )
        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_open_prs("owner/repo")
        finally:
            await client.aclose()

        self.assertEqual(len(prs), 3)

        first = prs[0]
        self.assertEqual(first.number, 42)
        self.assertEqual(first.title, "Add feature X")
        self.assertEqual(first.state, "open")
        self.assertFalse(first.draft)
        self.assertEqual(first.head_ref, "feature/x")
        self.assertEqual(first.base_ref, "main")
        self.assertEqual(first.body, "This adds feature X.")
        self.assertEqual(first.requested_reviewers, ["alice", "bob"])
        self.assertIsNone(first.review_decision)
        self.assertEqual(first.mergeable_state, "clean")
        self.assertIsNone(first.ci_status)
        self.assertEqual(first.url, "https://github.com/owner/repo/pull/42")
        self.assertEqual(first.updated_at, datetime(2026, 3, 10, 12, 0, 0, tzinfo=UTC))

        second = prs[1]
        self.assertTrue(second.draft)
        self.assertIsNone(second.body)
        self.assertEqual(second.requested_reviewers, [])
        self.assertEqual(second.mergeable_state, "unknown")

        third = prs[2]
        self.assertEqual(third.requested_reviewers, ["carol"])
        # mergeable_state is None in raw response — should default to "unknown"
        self.assertEqual(third.mergeable_state, "unknown")
        self.assertEqual(third.updated_at, datetime(2026, 3, 12, 16, 45, 0, tzinfo=UTC))

    @respx.mock
    async def test_fetch_open_prs_filters_team_reviewers(self) -> None:
        """Team reviewer objects (no `login` key) must not raise KeyError."""
        pr_with_team_reviewer = {
            "number": 99,
            "title": "Mixed reviewers",
            "state": "open",
            "draft": False,
            "head_ref": "feature/teams",
            "head": {"ref": "feature/teams"},
            "base": {"ref": "main"},
            "body": None,
            "requested_reviewers": [
                {"login": "alice"},
                {"slug": "backend-team", "name": "Backend"},  # team object — no login
                {"login": "bob"},
            ],
            "mergeable_state": "clean",
            "html_url": "https://github.com/owner/repo/pull/99",
            "updated_at": "2026-03-15T09:00:00Z",
        }
        respx.get("https://api.github.com/repos/owner/repo/pulls").mock(
            return_value=httpx.Response(200, json=[pr_with_team_reviewer])
        )
        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_open_prs("owner/repo")
        finally:
            await client.aclose()

        self.assertEqual(len(prs), 1)
        # Team object dropped; only user logins survive.
        self.assertEqual(prs[0].requested_reviewers, ["alice", "bob"])

    @respx.mock
    async def test_fetch_open_prs_follows_link_header_pagination(self) -> None:
        """>100 open PRs must paginate via the Link header, not truncate."""
        page_1_pr = {
            "number": 1,
            "title": "Page 1",
            "state": "open",
            "draft": False,
            "head": {"ref": "feature/1"},
            "base": {"ref": "main"},
            "body": None,
            "requested_reviewers": [],
            "mergeable_state": "clean",
            "merged_at": None,
            "html_url": "https://github.com/owner/repo/pull/1",
            "updated_at": "2026-03-10T12:00:00Z",
        }
        page_2_pr = {
            **page_1_pr,
            "number": 2,
            "title": "Page 2",
            "html_url": "https://github.com/owner/repo/pull/2",
        }

        next_url = "https://api.github.com/repos/owner/repo/pulls?state=open&per_page=100&page=2"
        respx.get("https://api.github.com/repos/owner/repo/pulls").mock(
            return_value=httpx.Response(
                200,
                json=[page_1_pr],
                headers={"Link": f'<{next_url}>; rel="next"'},
            )
        )
        respx.get(next_url).mock(
            return_value=httpx.Response(200, json=[page_2_pr])
            # No Link header -> iteration stops.
        )

        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_open_prs("owner/repo")
        finally:
            await client.aclose()

        self.assertEqual([p.number for p in prs], [1, 2])

    @respx.mock
    async def test_http_404_raises(self) -> None:
        """A 404 response must raise GitHubAPIError."""
        respx.get("https://api.github.com/repos/owner/missing/pulls").mock(
            return_value=httpx.Response(404, json={"message": "Not Found"})
        )
        client = GitHubClient(token="ghp_test")
        try:
            with self.assertRaises(GitHubAPIError) as ctx:
                await client.fetch_open_prs("owner/missing")
        finally:
            await client.aclose()

        self.assertIn("HTTP 404", str(ctx.exception))

    @respx.mock
    async def test_http_401_raises(self) -> None:
        """A 401 response on /user must raise GitHubAPIError."""
        respx.get("https://api.github.com/user").mock(
            return_value=httpx.Response(401, json={"message": "Bad credentials"})
        )
        client = GitHubClient(token="ghp_bad")
        try:
            with self.assertRaises(GitHubAPIError) as ctx:
                await client.fetch_current_user()
        finally:
            await client.aclose()

        self.assertIn("HTTP 401", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
