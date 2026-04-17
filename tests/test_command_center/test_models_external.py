from __future__ import annotations

import unittest
from datetime import datetime

from zing_ai.server.models_external import (
    GitHubPR,
    LinearIssue,
)


class TestLinearIssueModel(unittest.TestCase):
    """Tests for LinearIssue construction and JSON roundtrip."""

    def _sample(self) -> LinearIssue:
        return LinearIssue(
            id="uuid-1234",
            identifier="BAK-1179",
            title="Fix the thing",
            state="In Progress",
            state_type="started",
            priority=2,
            assignee="alice",
            team="Backend",
            url="https://linear.app/issue/BAK-1179",
            updated_at=datetime(2026, 4, 1, 12, 0, 0),
        )

    def test_construction(self) -> None:
        issue = self._sample()
        self.assertEqual(issue.identifier, "BAK-1179")
        self.assertEqual(issue.team, "Backend")
        self.assertEqual(issue.state_type, "started")
        self.assertEqual(issue.priority, 2)
        self.assertIsNone(
            LinearIssue(
                id="x",
                identifier="FRO-1",
                title="t",
                state="Todo",
                state_type="unstarted",
                assignee=None,
                team="Frontend",
                url="https://example.com",
                updated_at=datetime(2026, 1, 1),
            ).assignee
        )

    def test_json_roundtrip(self) -> None:
        issue = self._sample()
        restored = LinearIssue.model_validate_json(issue.model_dump_json())
        self.assertEqual(restored, issue)


class TestGitHubPRModel(unittest.TestCase):
    """Tests for GitHubPR construction and JSON roundtrip."""

    def _sample(self) -> GitHubPR:
        return GitHubPR(
            number=150,
            title="Add dashboard",
            state="open",
            draft=False,
            head_ref="feature/dashboard",
            base_ref="main",
            body="This PR adds the dashboard.",
            requested_reviewers=["bob", "carol"],
            review_decision="REVIEW_REQUIRED",
            mergeable_state="clean",
            ci_status="success",
            url="https://github.com/org/repo/pull/150",
            updated_at=datetime(2026, 4, 15, 9, 30, 0),
        )

    def test_construction(self) -> None:
        pr = self._sample()
        self.assertEqual(pr.number, 150)
        self.assertEqual(pr.state, "open")
        self.assertFalse(pr.draft)

    def test_null_fields(self) -> None:
        pr = self._sample()
        pr2 = pr.model_copy(update={"body": None, "ci_status": None, "review_decision": None})
        self.assertIsNone(pr2.body)
        self.assertIsNone(pr2.ci_status)
        self.assertIsNone(pr2.review_decision)

    def test_json_roundtrip(self) -> None:
        pr = self._sample()
        restored = GitHubPR.model_validate_json(pr.model_dump_json())
        self.assertEqual(restored, pr)


if __name__ == "__main__":
    unittest.main()
