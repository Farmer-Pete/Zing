"""Tests for the standup message generator."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta

from tests.test_command_center.conftest import make_issue, make_pr
from zing_ai.server.command_center import generate_standup
from zing_ai.server.models_external import KanbanCard, KanbanView


class TestGenerateStandup(unittest.TestCase):
    """Tests for generate_standup()."""

    def _now(self) -> datetime:
        return datetime.now(UTC)

    def test_empty_board(self) -> None:
        view = KanbanView()
        msg = generate_standup(view, "alice")
        assert "nothing to report" in msg
        assert "nothing planned" in msg
        assert "None" in msg

    def test_done_yesterday_merged_pr(self) -> None:
        """A merged PR from yesterday appears in the done section."""
        now = self._now()
        yesterday = now - timedelta(hours=12)
        pr = make_pr(
            number=1,
            title="Add login page",
            state="merged",
            author="alice",
            merged_at=yesterday,
            updated_at=yesterday,
        )
        card = KanbanCard(key="pr-org/repo-1", prs=[pr], done_group="completed")
        view = KanbanView(done=[card])
        msg = generate_standup(view, "alice")
        assert "Merged [Add login page]" in msg
        assert "#1" not in msg  # PR number should NOT be in link text

    def test_done_ticket_completed(self) -> None:
        """A completed ticket appears with 'Completed' label."""
        now = self._now()
        yesterday = now - timedelta(hours=12)
        issue = make_issue(
            identifier="BAK-42",
            title="Fix auth bug",
            state_type="completed",
            updated_at=yesterday,
        )
        card = KanbanCard(key="BAK-42", ticket=issue, done_group="completed")
        view = KanbanView(done=[card])
        msg = generate_standup(view, "alice")
        assert "Completed [Fix auth bug]" in msg
        assert "BAK-42" not in msg.split("[Fix auth bug]")[0].split("\n")[-1]

    def test_ready_to_merge_appears_on_today_plate(self) -> None:
        """A ready-to-merge card lives under 'on my plate today', not yesterday's recap."""
        now = self._now()
        pr = make_pr(
            number=3,
            title="Approved feature",
            state="open",
            author="alice",
            updated_at=now - timedelta(hours=4),
        )
        card = KanbanCard(key="pr-org/repo-3", prs=[pr], done_group="ready_to_merge")
        view = KanbanView(done=[card])
        msg = generate_standup(view, "alice")
        done_section = msg.split("**What got done yesterday:**")[1].split("**")[0]
        today_section = msg.split("**What's on my plate today:**")[1].split("**")[0]
        assert "Merge in [Approved feature]" in today_section
        assert "Approved feature" not in done_section

    def test_in_progress_new_work(self) -> None:
        """A card in progress with no feedback shows 'Work on'."""
        issue = make_issue(identifier="BAK-10", title="Build dashboard")
        card = KanbanCard(key="BAK-10", ticket=issue)
        view = KanbanView(in_progress=[card])
        msg = generate_standup(view, "alice")
        assert "Work on [Build dashboard]" in msg

    def test_in_progress_with_feedback(self) -> None:
        """A card with unaddressed feedback shows 'Resolve comments on'."""
        pr = make_pr(
            number=5,
            title="Add search",
            author="alice",
            state="open",
            reviewers=["bob"],
            reviewer_states={"bob": "CHANGES_REQUESTED"},
            requested_reviewers=[],
        )
        issue = make_issue(identifier="BAK-20", title="Add search")
        card = KanbanCard(key="BAK-20", ticket=issue, prs=[pr])
        view = KanbanView(in_progress=[card])
        msg = generate_standup(view, "alice")
        assert "Resolve comments on [Add search]" in msg

    def test_blockers_waiting_on_others(self) -> None:
        """PRs in needs_review with review_group='others' appear as blockers."""
        now = self._now()
        pr = make_pr(
            number=7,
            title="Refactor API",
            author="alice",
            requested_reviewers=["bob"],
            updated_at=now - timedelta(hours=6),
        )
        card = KanbanCard(
            key="pr-org/repo-7",
            prs=[pr],
            review_group="others",
        )
        view = KanbanView(needs_review=[card])
        msg = generate_standup(view, "alice")
        assert "Need a review on [Refactor API]" in msg

    def test_needs_review_also_in_done(self) -> None:
        """Cards waiting on review also appear in done as 'Worked on'."""
        now = self._now()
        issue = make_issue(
            identifier="BAK-50",
            title="Refactor API",
            updated_at=now - timedelta(hours=6),
        )
        pr = make_pr(
            number=7,
            title="Refactor API",
            author="alice",
            requested_reviewers=["bob"],
            updated_at=now - timedelta(hours=6),
        )
        card = KanbanCard(
            key="BAK-50",
            ticket=issue,
            prs=[pr],
            review_group="others",
        )
        view = KanbanView(needs_review=[card])
        msg = generate_standup(view, "alice")
        assert "Worked on [Refactor API]" in msg
        assert "Need a review on [Refactor API]" in msg

    def test_blockers_mine_excluded(self) -> None:
        """PRs where I'm the reviewer (mine_passing) are NOT blockers."""
        pr = make_pr(
            number=8,
            title="Someone else PR",
            author="bob",
            requested_reviewers=["alice"],
        )
        card = KanbanCard(
            key="pr-org/repo-8",
            prs=[pr],
            review_group="mine_passing",
        )
        view = KanbanView(needs_review=[card])
        msg = generate_standup(view, "alice")
        assert "Someone else PR" not in msg
        assert "None" in msg.split("**Blockers:**")[1]

    def test_reviewed_prs_appear_in_done(self) -> None:
        """PRs the user reviewed (not authored) appear under 'what got done' as 'Reviewed ...'."""
        now = self._now()
        pr = make_pr(
            number=99,
            title="Someone else work",
            state="merged",
            author="bob",
            reviewers=["alice"],
            merged_at=now - timedelta(hours=6),
            updated_at=now - timedelta(hours=6),
        )
        card = KanbanCard(key="pr-org/repo-99", prs=[pr], done_group="completed")
        view = KanbanView(done=[card])
        msg = generate_standup(view, "alice")
        done_section = msg.split("**What got done yesterday:**")[1].split("**")[0]
        assert "Reviewed [Someone else work]" in done_section
        # And it must not be mislabelled as the user's own merge.
        assert "Merged [Someone else work]" not in msg

    def test_reviewed_pr_outside_window_excluded(self) -> None:
        """A review PR last touched before yesterday is not surfaced."""
        now = self._now()
        old = now - timedelta(days=5)
        pr = make_pr(
            number=42,
            title="Stale review",
            state="merged",
            author="bob",
            reviewers=["alice"],
            merged_at=old,
            updated_at=old,
        )
        card = KanbanCard(key="pr-org/repo-42", prs=[pr], done_group="completed")
        view = KanbanView(done=[card])
        msg = generate_standup(view, "alice")
        assert "Stale review" not in msg

    def test_reviewed_pr_in_needs_review(self) -> None:
        """A review on a PR still sitting in needs_review is surfaced under done."""
        now = self._now()
        pr = make_pr(
            number=11,
            title="Bob open PR",
            state="open",
            author="bob",
            reviewers=["alice"],
            requested_reviewers=[],
            updated_at=now - timedelta(hours=4),
        )
        card = KanbanCard(key="pr-org/repo-11", prs=[pr], review_group="mine_passing")
        view = KanbanView(needs_review=[card])
        msg = generate_standup(view, "alice")
        assert "Reviewed [Bob open PR]" in msg

    def test_no_pr_numbers_in_output(self) -> None:
        """PR and ticket numbers should not appear as visible text in the message."""
        now = self._now()
        pr = make_pr(
            number=1234,
            title="Big feature",
            state="merged",
            author="alice",
            merged_at=now - timedelta(hours=6),
            updated_at=now - timedelta(hours=6),
        )
        card = KanbanCard(key="pr-org/repo-1234", prs=[pr], done_group="completed")
        view = KanbanView(done=[card])
        msg = generate_standup(view, "alice")
        # The number can appear in the URL (inside parens) but NOT in the display text
        lines = msg.split("\n")
        for line in lines:
            if "Big feature" in line:
                # Extract the link text part [...]
                import re

                link_texts = re.findall(r"\[([^\]]+)\]", line)
                for text in link_texts:
                    assert "1234" not in text
