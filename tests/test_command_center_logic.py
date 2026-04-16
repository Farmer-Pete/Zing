"""Unit tests for command_center aggregation logic."""

from __future__ import annotations

import unittest
from datetime import datetime

from zing_ai.server.command_center import (
    AUDIT_STEP_NAMES,
    _parse_ticket_ids,
    aggregate,
)
from zing_ai.server.models_external import GitHubPR


def _make_pr(
    *,
    number: int = 1,
    title: str = "Title",
    head_ref: str = "feature",
    body: str | None = None,
) -> GitHubPR:
    """Build a GitHubPR with sensible defaults for ticket-parsing tests."""
    return GitHubPR(
        number=number,
        title=title,
        state="open",
        draft=False,
        head_ref=head_ref,
        base_ref="main",
        body=body,
        requested_reviewers=[],
        review_decision=None,
        mergeable_state="clean",
        ci_status=None,
        url=f"https://github.com/o/r/pull/{number}",
        updated_at=datetime(2026, 4, 16, 0, 0, 0),
    )


class TestAggregateEmptyInputs(unittest.TestCase):
    """The aggregate() entry point returns empty collections for empty inputs."""

    def test_aggregate_empty_inputs(self) -> None:
        inbox, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[],
            current_username="octocat",
        )
        self.assertEqual(inbox, [])
        self.assertEqual(hubs, [])


class TestAuditStepNames(unittest.TestCase):
    """AUDIT_STEP_NAMES covers the four audit types."""

    def test_audit_step_names_contents(self) -> None:
        self.assertEqual(
            AUDIT_STEP_NAMES,
            frozenset({"plan-audit", "build-audit", "pr-audit", "custom-audit"}),
        )


class TestParseTicketIds(unittest.TestCase):
    """_parse_ticket_ids extracts normalised ticket identifiers from PRs."""

    def test_parse_uppercase_branch(self) -> None:
        pr = _make_pr(head_ref="BAK-1179/feature")
        self.assertEqual(_parse_ticket_ids(pr), {"BAK-1179"})

    def test_parse_lowercase_branch(self) -> None:
        pr = _make_pr(head_ref="bak-1179/feature")
        self.assertEqual(_parse_ticket_ids(pr), {"BAK-1179"})

    def test_parse_body_closes_keyword(self) -> None:
        pr = _make_pr(body="Closes BAK-1179")
        self.assertEqual(_parse_ticket_ids(pr), {"BAK-1179"})

    def test_parse_multiple_tickets(self) -> None:
        pr = _make_pr(body="See FRO-892 and BAK-1179")
        self.assertEqual(_parse_ticket_ids(pr), {"FRO-892", "BAK-1179"})

    def test_parse_no_match(self) -> None:
        pr = _make_pr(title="No tickets here", head_ref="feature/cleanup", body=None)
        self.assertEqual(_parse_ticket_ids(pr), set())


if __name__ == "__main__":
    unittest.main()
