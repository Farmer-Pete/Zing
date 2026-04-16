"""Tests for ExternalCache."""

from __future__ import annotations

import unittest
from datetime import datetime

from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.models_external import GitHubPR, LinearIssue


class TestExternalCacheDefaults(unittest.TestCase):
    """Default field values on a freshly instantiated ExternalCache."""

    def test_issues_defaults_to_empty_list(self) -> None:
        cache = ExternalCache()
        self.assertEqual(cache.issues, [])

    def test_prs_defaults_to_empty_list(self) -> None:
        cache = ExternalCache()
        self.assertEqual(cache.prs, [])

    def test_github_username_defaults_to_empty(self) -> None:
        cache = ExternalCache()
        self.assertEqual(cache.github_username, "")

    def test_last_polled_at_defaults_to_none(self) -> None:
        cache = ExternalCache()
        self.assertIsNone(cache.last_polled_at)

    def test_last_error_defaults_to_none(self) -> None:
        cache = ExternalCache()
        self.assertIsNone(cache.last_error)

    def test_two_instances_have_independent_lists(self) -> None:
        """field(default_factory=list) gives each instance its own list."""
        a = ExternalCache()
        b = ExternalCache()
        a.issues.append(
            LinearIssue(
                id="uuid-1",
                identifier="BAK-1",
                title="x",
                state="Open",
                assignee=None,
                team="BAK",
                url="https://linear.app/x/issue/BAK-1/x",
                updated_at=datetime.now(),
            )
        )
        self.assertEqual(len(a.issues), 1)
        self.assertEqual(len(b.issues), 0)


class TestExternalCacheMutation(unittest.TestCase):
    """Mutating fields keeps the correct types."""

    def test_mutate_issues(self) -> None:
        cache = ExternalCache()
        issue = LinearIssue(
            id="uuid-2",
            identifier="FRO-42",
            title="Add button",
            state="In Progress",
            assignee="alice",
            team="FRO",
            url="https://linear.app/x/issue/FRO-42/add-button",
            updated_at=datetime.now(),
        )
        cache.issues = [issue]
        self.assertEqual(len(cache.issues), 1)
        self.assertEqual(cache.issues[0].identifier, "FRO-42")

    def test_mutate_prs(self) -> None:
        cache = ExternalCache()
        pr = GitHubPR(
            number=7,
            title="Fix bug",
            state="open",
            draft=False,
            head_ref="bak-1/fix",
            base_ref="main",
            body=None,
            requested_reviewers=[],
            review_decision=None,
            mergeable_state="clean",
            ci_status=None,
            url="https://github.com/x/y/pull/7",
            updated_at=datetime.now(),
        )
        cache.prs = [pr]
        self.assertEqual(len(cache.prs), 1)
        self.assertEqual(cache.prs[0].number, 7)

    def test_mutate_scalar_fields(self) -> None:
        cache = ExternalCache()
        cache.github_username = "octocat"
        cache.last_polled_at = datetime(2026, 4, 16, 12, 0, 0)
        cache.last_error = "rate limited"
        self.assertEqual(cache.github_username, "octocat")
        self.assertEqual(cache.last_polled_at, datetime(2026, 4, 16, 12, 0, 0))
        self.assertEqual(cache.last_error, "rate limited")


if __name__ == "__main__":
    unittest.main()
