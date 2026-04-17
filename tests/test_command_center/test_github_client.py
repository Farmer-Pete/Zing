from __future__ import annotations

import unittest
from datetime import UTC, datetime

import httpx
import respx

from zing_ai.server.github_client import GitHubAPIError, GitHubClient

_USER_RESPONSE = {"login": "octocat", "id": 1}


# GraphQL response shape — matches the query in GitHubClient.fetch_open_prs
def _gql_response(nodes: list[dict]) -> dict:
    """Wrap PR nodes in the GraphQL envelope returned by the GitHub API."""
    return {
        "data": {
            "repository": {
                "pullRequests": {
                    "nodes": nodes,
                }
            }
        }
    }


def _make_pr_node(
    *,
    number: int,
    title: str,
    state: str = "OPEN",
    is_draft: bool = False,
    head_ref: str = "feature/x",
    base_ref: str = "main",
    body: str | None = None,
    author_login: str = "octocat",
    review_decision: str | None = None,
    mergeable: str = "MERGEABLE",
    url: str = "https://github.com/owner/repo/pull/1",
    updated_at: str = "2026-03-10T12:00:00Z",
    reviewer_logins: list[str] | None = None,
    ci_status: str | None = None,
) -> dict:
    """Build a minimal GraphQL PR node dict."""
    reviewer_nodes = [{"requestedReviewer": {"login": login}} for login in (reviewer_logins or [])]
    commit_node: dict = {"commit": {}}
    if ci_status is not None:
        commit_node = {"commit": {"statusCheckRollup": {"state": ci_status}}}
    return {
        "number": number,
        "title": title,
        "state": state,
        "isDraft": is_draft,
        "headRefName": head_ref,
        "baseRefName": base_ref,
        "body": body,
        "author": {"login": author_login},
        "reviewDecision": review_decision,
        "mergeable": mergeable,
        "url": url,
        "updatedAt": updated_at,
        "reviewRequests": {"nodes": reviewer_nodes},
        "commits": {"nodes": [commit_node]},
    }


_PR_NODES = [
    _make_pr_node(
        number=42,
        title="Add feature X",
        body="This adds feature X.",
        author_login="alice",
        reviewer_logins=["alice", "bob"],
        review_decision=None,
        mergeable="MERGEABLE",
        ci_status=None,
        url="https://github.com/owner/repo/pull/42",
        updated_at="2026-03-10T12:00:00Z",
    ),
    _make_pr_node(
        number=43,
        title="Fix bug Y",
        is_draft=True,
        base_ref="develop",
        head_ref="fix/bug-y",
        body=None,
        mergeable="UNKNOWN",
        url="https://github.com/owner/repo/pull/43",
        updated_at="2026-03-11T08:30:00Z",
    ),
    _make_pr_node(
        number=44,
        title="Update deps",
        head_ref="chore/deps",
        body="Bump all the things.",
        reviewer_logins=["carol"],
        mergeable="UNKNOWN",
        url="https://github.com/owner/repo/pull/44",
        updated_at="2026-03-12T16:45:00+00:00",
    ),
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
        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(200, json=_gql_response(_PR_NODES))
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
        self.assertEqual(first.author, "alice")
        self.assertEqual(first.repo, "owner/repo")
        self.assertEqual(first.requested_reviewers, ["alice", "bob"])
        self.assertIsNone(first.review_decision)
        self.assertEqual(first.mergeable_state, "mergeable")
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
        self.assertEqual(third.mergeable_state, "unknown")
        self.assertEqual(third.updated_at, datetime(2026, 3, 12, 16, 45, 0, tzinfo=UTC))

    @respx.mock
    async def test_fetch_open_prs_populates_author_review_decision_ci_status(self) -> None:
        """GraphQL fields author, reviewDecision, and ci_status are mapped correctly."""
        node = _make_pr_node(
            number=10,
            title="Reviewed PR",
            author_login="bob",
            review_decision="APPROVED",
            ci_status="SUCCESS",
            url="https://github.com/owner/repo/pull/10",
        )
        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(200, json=_gql_response([node]))
        )
        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_open_prs("owner/repo")
        finally:
            await client.aclose()

        self.assertEqual(len(prs), 1)
        pr = prs[0]
        self.assertEqual(pr.author, "bob")
        self.assertEqual(pr.review_decision, "APPROVED")
        self.assertEqual(pr.ci_status, "SUCCESS")

    @respx.mock
    async def test_fetch_open_prs_filters_team_reviewers(self) -> None:
        """Team reviewer objects (no `login` key) must not appear in requested_reviewers."""
        # GraphQL returns only User fragments with login; team fragments don't have login.
        node: dict = _make_pr_node(
            number=99,
            title="Mixed reviewers",
            head_ref="feature/teams",
            url="https://github.com/owner/repo/pull/99",
            updated_at="2026-03-15T09:00:00Z",
        )
        # Inject a mix: two users and one team (requestedReviewer without login)
        node["reviewRequests"] = {
            "nodes": [
                {"requestedReviewer": {"login": "alice"}},
                {"requestedReviewer": {}},  # team — no login key
                {"requestedReviewer": {"login": "bob"}},
            ]
        }
        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(200, json=_gql_response([node]))
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
    async def test_http_404_raises(self) -> None:
        """A 404 HTTP response to the GraphQL endpoint must raise GitHubAPIError."""
        respx.post("https://api.github.com/graphql").mock(
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
    async def test_graphql_errors_raises(self) -> None:
        """A GraphQL 200 response with top-level errors must raise GitHubAPIError."""
        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(
                200,
                json={"errors": [{"message": "Could not resolve to a Repository"}]},
            )
        )
        client = GitHubClient(token="ghp_test")
        try:
            with self.assertRaises(GitHubAPIError) as ctx:
                await client.fetch_open_prs("owner/nonexistent")
        finally:
            await client.aclose()

        self.assertIn("Could not resolve to a Repository", str(ctx.exception))

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

    # ------------------------------------------------------------------ #
    # fetch_recent_prs                                                     #
    # ------------------------------------------------------------------ #

    def _merged_gql_response(self, nodes: list[dict]) -> dict:
        """Wrap PR nodes in the GraphQL envelope for merged PRs."""
        return {
            "data": {
                "repository": {
                    "pullRequests": {
                        "nodes": nodes,
                    }
                }
            }
        }

    @respx.mock
    async def test_fetch_recent_prs_basic(self) -> None:
        """fetch_recent_prs returns merged PRs authored by the username within the window."""
        # Use a mergedAt 3 days ago (well within a 7-day window).
        merged_at = "2026-04-14T10:00:00Z"
        node = _make_pr_node(
            number=10,
            title="Merged feature",
            state="MERGED",
            author_login="alice",
            updated_at=merged_at,
        )
        node["mergedAt"] = merged_at

        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(200, json=self._merged_gql_response([node]))
        )
        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_recent_prs(["owner/repo"], username="alice")
        finally:
            await client.aclose()

        self.assertEqual(len(prs), 1)
        self.assertEqual(prs[0].number, 10)
        self.assertEqual(prs[0].state, "merged")
        self.assertEqual(prs[0].author, "alice")
        self.assertEqual(prs[0].repo, "owner/repo")
        self.assertIsNotNone(prs[0].merged_at)

    @respx.mock
    async def test_fetch_recent_prs_filters_by_date(self) -> None:
        """fetch_recent_prs excludes PRs whose mergedAt is older than since_days."""
        old_merged_at = "2026-04-01T10:00:00Z"  # 16 days ago — outside 7-day window
        recent_merged_at = "2026-04-14T10:00:00Z"  # 3 days ago — inside window

        old_node = _make_pr_node(
            number=1,
            title="Old merged PR",
            state="MERGED",
            author_login="alice",
            updated_at=old_merged_at,
        )
        old_node["mergedAt"] = old_merged_at

        recent_node = _make_pr_node(
            number=2,
            title="Recent merged PR",
            state="MERGED",
            author_login="alice",
            updated_at=recent_merged_at,
        )
        recent_node["mergedAt"] = recent_merged_at

        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(
                200, json=self._merged_gql_response([old_node, recent_node])
            )
        )
        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_recent_prs(["owner/repo"], username="alice", since_days=7)
        finally:
            await client.aclose()

        self.assertEqual(len(prs), 1)
        self.assertEqual(prs[0].number, 2)

    @respx.mock
    async def test_fetch_recent_prs_filters_by_author_or_reviewer(self) -> None:
        """fetch_recent_prs keeps PRs where user is author OR reviewer; excludes others."""
        merged_at = "2026-04-14T10:00:00Z"

        # PR authored by alice — should be included for username "alice"
        author_node = _make_pr_node(
            number=1,
            title="Alice's PR",
            state="MERGED",
            author_login="alice",
            updated_at=merged_at,
        )
        author_node["mergedAt"] = merged_at

        # PR authored by bob with alice as reviewer — should be included
        reviewer_node = _make_pr_node(
            number=2,
            title="Bob's PR with alice as reviewer",
            state="MERGED",
            author_login="bob",
            reviewer_logins=["alice"],
            updated_at=merged_at,
        )
        reviewer_node["mergedAt"] = merged_at
        # Add reviews (submitted reviews) since the code now checks reviews, not reviewRequests
        reviewer_node["reviews"] = {"nodes": [{"author": {"login": "alice"}}]}

        # PR authored by carol with no involvement from alice — should be excluded
        unrelated_node = _make_pr_node(
            number=3,
            title="Carol's PR",
            state="MERGED",
            author_login="carol",
            updated_at=merged_at,
        )
        unrelated_node["mergedAt"] = merged_at

        respx.post("https://api.github.com/graphql").mock(
            return_value=httpx.Response(
                200,
                json=self._merged_gql_response([author_node, reviewer_node, unrelated_node]),
            )
        )
        client = GitHubClient(token="ghp_test")
        try:
            prs = await client.fetch_recent_prs(["owner/repo"], username="alice")
        finally:
            await client.aclose()

        numbers = {pr.number for pr in prs}
        self.assertIn(1, numbers)  # author match
        self.assertIn(2, numbers)  # reviewer match
        self.assertNotIn(3, numbers)  # no involvement — excluded


if __name__ == "__main__":
    unittest.main()
