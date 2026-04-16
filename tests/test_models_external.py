from __future__ import annotations

import unittest
from datetime import datetime

from pydantic import ValidationError

from zing_ai.server.models_external import (
    GitHubPR,
    Hub,
    InboxItem,
    LinearIssue,
)


class TestSignalKey(unittest.TestCase):
    """Tests for Hub.signal_key property."""

    def test_hyphen_identifier(self) -> None:
        hub = _make_hub("BAK-1179")
        self.assertEqual(hub.signal_key, "bak_1179")

    def test_pr_style_id(self) -> None:
        hub = _make_hub("pr-150")
        self.assertEqual(hub.signal_key, "pr_150")

    def test_spaces_in_id(self) -> None:
        hub = _make_hub("Big Fun Ticket")
        self.assertEqual(hub.signal_key, "big_fun_ticket")

    def test_mixed_hyphens_and_spaces(self) -> None:
        hub = _make_hub("MY-TICKET 42")
        self.assertEqual(hub.signal_key, "my_ticket_42")

    def test_already_lowercase(self) -> None:
        hub = _make_hub("session-abc123")
        self.assertEqual(hub.signal_key, "session_abc123")


class TestHubIdValidation(unittest.TestCase):
    """Hub.id must start with a letter and contain only safe chars."""

    def test_empty_string_rejected(self) -> None:
        with self.assertRaises(ValidationError):
            _make_hub("")

    def test_all_numeric_rejected(self) -> None:
        # JS dot-notation can't access keys that start with a digit.
        with self.assertRaises(ValidationError):
            _make_hub("12345")

    def test_leading_hyphen_rejected(self) -> None:
        with self.assertRaises(ValidationError):
            _make_hub("-BAK-1")

    def test_special_chars_rejected(self) -> None:
        # Slash / ampersand / paren would produce malformed Datastar keys.
        for bad in ("BAK/1", "BAK&1", "BAK(1)"):
            with self.assertRaises(ValidationError):
                _make_hub(bad)

    def test_consecutive_hyphens_allowed(self) -> None:
        # Collision with a natural "pr__orphan" is possible but rare; allow it.
        hub = _make_hub("pr--orphan")
        self.assertEqual(hub.signal_key, "pr__orphan")


class TestLinearIssueModel(unittest.TestCase):
    """Tests for LinearIssue construction and JSON roundtrip."""

    def _sample(self) -> LinearIssue:
        return LinearIssue(
            id="uuid-1234",
            identifier="BAK-1179",
            title="Fix the thing",
            state="In Progress",
            assignee="alice",
            team="Backend",
            url="https://linear.app/issue/BAK-1179",
            updated_at=datetime(2026, 4, 1, 12, 0, 0),
        )

    def test_construction(self) -> None:
        issue = self._sample()
        self.assertEqual(issue.identifier, "BAK-1179")
        self.assertEqual(issue.team, "Backend")
        self.assertIsNone(
            LinearIssue(
                id="x",
                identifier="FRO-1",
                title="t",
                state="Todo",
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


class TestHubModel(unittest.TestCase):
    """Tests for Hub construction and JSON roundtrip."""

    def _sample(self) -> Hub:
        return Hub(
            id="BAK-1179",
            kind="ticket",
            title="Fix the thing",
            team="Backend",
            assignee="alice",
            urgency="hot",
        )

    def test_construction(self) -> None:
        hub = self._sample()
        self.assertEqual(hub.id, "BAK-1179")
        self.assertEqual(hub.kind, "ticket")
        self.assertEqual(hub.urgency, "hot")

    def test_defaults(self) -> None:
        hub = self._sample()
        self.assertEqual(hub.prs, [])
        self.assertEqual(hub.sessions, [])
        self.assertEqual(hub.audits, [])
        self.assertIsNone(hub.linear_issue)

    def test_json_roundtrip(self) -> None:
        hub = self._sample()
        restored = Hub.model_validate_json(hub.model_dump_json())
        self.assertEqual(restored, hub)


class TestInboxItemModel(unittest.TestCase):
    """Tests for InboxItem construction and JSON roundtrip."""

    def _sample(self) -> InboxItem:
        return InboxItem(
            priority="high",
            action_text="Review PR",
            detail_text="PR #150 is waiting for your review.",
            hub_id="BAK-1179",
            hub_label="BAK-1179",
            time_waiting="2 hours",
            target_url="https://github.com/org/repo/pull/150",
        )

    def test_construction(self) -> None:
        item = self._sample()
        self.assertEqual(item.priority, "high")
        self.assertEqual(item.hub_label, "BAK-1179")

    def test_null_detail(self) -> None:
        item = self._sample().model_copy(update={"detail_text": None})
        self.assertIsNone(item.detail_text)

    def test_json_roundtrip(self) -> None:
        item = self._sample()
        restored = InboxItem.model_validate_json(item.model_dump_json())
        self.assertEqual(restored, item)


def _make_hub(hub_id: str) -> Hub:
    """Helper to create a minimal Hub with the given id."""
    return Hub(
        id=hub_id,
        kind="ticket",
        title="Test",
        team=None,
        assignee=None,
        urgency="active",
    )


if __name__ == "__main__":
    unittest.main()
