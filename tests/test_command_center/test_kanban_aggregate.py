"""Unit tests for the new KanbanView-based aggregate() function (Step 5)."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta

from tests.test_command_center.conftest import (
    make_issue as _make_issue,
)
from tests.test_command_center.conftest import (
    make_pr as _make_pr,
)
from tests.test_command_center.conftest import (
    make_session as _make_session,
)
from tests.test_command_center.conftest import (
    make_workflow_step as _make_workflow_step,
)
from zing_ai.server.command_center import aggregate
from zing_ai.server.models import SessionState, TextFinding
from zing_ai.server.models_external import KanbanCard, KanbanView

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

NOW = datetime.now(UTC)
RECENT = NOW - timedelta(days=3)
OLD = NOW - timedelta(days=10)  # outside the 7-day done window


def _agg(
    issues=None,
    prs=None,
    recent_prs=None,
    completed_issues=None,
    sessions=None,
    current_username="octocat",
) -> KanbanView:
    """Convenience wrapper for the new 6-arg aggregate() call."""
    return aggregate(
        issues=issues or [],
        prs=prs or [],
        recent_prs=recent_prs or [],
        completed_issues=completed_issues or [],
        sessions=sessions or [],
        current_username=current_username,
    )


def _all_cards(view: KanbanView) -> list[KanbanCard]:
    return view.todo + view.in_progress + view.needs_review + view.done


# ---------------------------------------------------------------------------
# Basic grouping
# ---------------------------------------------------------------------------


class TestAggregateEmptyInputs(unittest.TestCase):
    """aggregate() returns an empty KanbanView for empty inputs."""

    def test_empty_returns_kanban_view(self) -> None:
        view = _agg()
        self.assertIsInstance(view, KanbanView)
        self.assertEqual(view.todo, [])
        self.assertEqual(view.in_progress, [])
        self.assertEqual(view.needs_review, [])
        self.assertEqual(view.done, [])

    def test_empty_total_cards_is_zero(self) -> None:
        view = _agg()
        self.assertEqual(len(_all_cards(view)), 0)


class TestAggregateGrouping(unittest.TestCase):
    """Issues + PRs + sessions are grouped into KanbanCards correctly."""

    def test_issue_creates_one_card(self) -> None:
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "BAK-1")
        self.assertIs(cards[0].ticket, issue)

    def test_pr_linked_to_ticket_joins_card(self) -> None:
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=10, head_ref="BAK-1/feature")
        view = _agg(issues=[issue], prs=[pr])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].prs), 1)
        self.assertEqual(cards[0].prs[0].number, 10)

    def test_orphan_pr_becomes_its_own_card(self) -> None:
        pr = _make_pr(number=99, head_ref="feature/no-ticket")
        view = _agg(prs=[pr])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "pr-99")
        self.assertIsNone(cards[0].ticket)

    def test_session_with_ticket_id_joins_card(self) -> None:
        issue = _make_issue(identifier="BAK-1")
        session = _make_session(session_id="s1", ticket_id="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].sessions), 1)

    def test_standalone_session_excluded(self) -> None:
        """Sessions with no ticket_id must not produce a card."""
        session = _make_session(session_id="orphan", ticket_id=None)
        view = _agg(sessions=[session])
        self.assertEqual(len(_all_cards(view)), 0)

    def test_two_issues_produce_two_cards(self) -> None:
        i1 = _make_issue(identifier="BAK-1")
        i2 = _make_issue(identifier="BAK-2")
        view = _agg(issues=[i1, i2])
        self.assertEqual(len(_all_cards(view)), 2)

    def test_recent_pr_merged_joins_ticket_card(self) -> None:
        """A recently merged PR that references a ticket joins the ticket card."""
        issue = _make_issue(identifier="BAK-1")
        merged_pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="merged",
            updated_at=RECENT,
        )
        merged_pr.merged_at = RECENT
        view = _agg(issues=[issue], recent_prs=[merged_pr])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].prs), 1)

    def test_completed_issue_creates_card(self) -> None:
        """A completed issue (from completed_issues param) creates a card."""
        issue = _make_issue(
            identifier="BAK-2",
            state="Done",
            state_type="completed",
            updated_at=RECENT,
        )
        view = _agg(completed_issues=[issue])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "BAK-2")


# ---------------------------------------------------------------------------
# Column classification
# ---------------------------------------------------------------------------


class TestColumnClassification(unittest.TestCase):
    """Cards are placed in the correct Kanban column."""

    def test_todo_column_default(self) -> None:
        """An open issue with no sessions/PRs falls into todo."""
        issue = _make_issue(identifier="BAK-1", state_type="unstarted")
        view = _agg(issues=[issue])
        self.assertEqual(len(view.todo), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_in_progress_when_session_step_started(self) -> None:
        """A session with a STARTED step places the card in in_progress."""
        issue = _make_issue(identifier="BAK-1")
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[step])
        view = _agg(issues=[issue], sessions=[session])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.todo), 0)

    def test_in_progress_when_open_pr_no_reviewers(self) -> None:
        """An open PR with no requested_reviewers → in_progress."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=1, head_ref="BAK-1/feature", state="open", requested_reviewers=[])
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.in_progress), 1)

    def test_needs_review_when_pr_has_reviewers(self) -> None:
        """Open PR with requested_reviewers and non-APPROVED decision → needs_review."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            requested_reviewers=["octocat"],
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_needs_review_when_user_is_author_and_others_reviewing(self) -> None:
        """PR where user is author and others are reviewers → needs_review."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            requested_reviewers=["alice"],
            review_decision=None,
        )
        pr.author = "octocat"
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)

    def test_done_when_ticket_completed_recently(self) -> None:
        """Ticket with state_type='completed' updated within 7 days → done."""
        issue = _make_issue(
            identifier="BAK-1",
            state_type="completed",
            updated_at=RECENT,
        )
        view = _agg(issues=[issue])
        self.assertEqual(len(view.done), 1)
        self.assertEqual(len(view.todo), 0)

    def test_todo_when_ticket_completed_but_old(self) -> None:
        """Ticket with state_type='completed' updated > 7 days ago → todo (not done)."""
        issue = _make_issue(
            identifier="BAK-1",
            state_type="completed",
            updated_at=OLD,
        )
        view = _agg(issues=[issue])
        # Old completed ticket falls out of done window → goes to todo
        self.assertEqual(len(view.done), 0)
        self.assertEqual(len(view.todo), 1)

    def test_done_when_pr_merged_recently(self) -> None:
        """A card with a recently merged PR → done."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="merged",
            updated_at=RECENT,
        )
        pr.merged_at = RECENT
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.done), 1)

    def test_todo_when_pr_merged_old(self) -> None:
        """A merged PR older than 7 days doesn't count for done."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=5, head_ref="BAK-1/fix", state="merged", updated_at=OLD)
        pr.merged_at = OLD
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.done), 0)

    def test_done_when_merged_pr_approved_recently(self) -> None:
        """An APPROVED merged PR updated within 7 days → done."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="merged",
            updated_at=RECENT,
            review_decision="APPROVED",
        )
        pr.merged_at = RECENT
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.done), 1)


# ---------------------------------------------------------------------------
# Column priority rule
# ---------------------------------------------------------------------------


class TestColumnPriorityRule(unittest.TestCase):
    """In Progress > Needs Review > Done > To Do."""

    def test_in_progress_beats_needs_review(self) -> None:
        """A card with both a STARTED session and pending reviewers → in_progress."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            requested_reviewers=["octocat"],
        )
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[step])
        view = _agg(issues=[issue], prs=[pr], sessions=[session])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.needs_review), 0)

    def test_in_progress_beats_done(self) -> None:
        """A card with a STARTED session and a recently merged PR → in_progress."""
        issue = _make_issue(identifier="BAK-1", state_type="completed", updated_at=RECENT)
        pr = _make_pr(number=5, head_ref="BAK-1/fix", state="merged", updated_at=RECENT)
        pr.merged_at = RECENT
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[step])
        view = _agg(issues=[issue], prs=[pr], sessions=[session])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.done), 0)

    def test_needs_review_beats_done(self) -> None:
        """A card with pending reviews and a recently merged PR → needs_review."""
        issue = _make_issue(identifier="BAK-1", state_type="completed", updated_at=RECENT)
        pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="open",
            requested_reviewers=["octocat"],
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.done), 0)

    def test_done_beats_todo(self) -> None:
        """A recently completed ticket → done, not todo."""
        issue = _make_issue(
            identifier="BAK-1",
            state_type="completed",
            updated_at=RECENT,
        )
        view = _agg(issues=[issue])
        self.assertEqual(len(view.done), 1)
        self.assertEqual(len(view.todo), 0)


# ---------------------------------------------------------------------------
# To Do sort order
# ---------------------------------------------------------------------------


class TestTodoSortOrder(unittest.TestCase):
    """To Do column: state_type bucket (other=0, backlog=1, triage=2), then priority asc."""

    def test_other_bucket_before_backlog(self) -> None:
        """'other' state_type (bucket 0) sorts before 'backlog' (bucket 1)."""
        unstarted = _make_issue(identifier="BAK-1", state_type="unstarted", priority=0)
        backlog = _make_issue(identifier="BAK-2", state_type="backlog", priority=0)
        view = _agg(issues=[unstarted, backlog])
        self.assertEqual(view.todo[0].key, "BAK-1")
        self.assertEqual(view.todo[1].key, "BAK-2")

    def test_backlog_before_triage(self) -> None:
        """'backlog' (bucket 1) sorts before 'triage' (bucket 2)."""
        triage = _make_issue(identifier="BAK-3", state_type="triage", priority=0)
        backlog = _make_issue(identifier="BAK-2", state_type="backlog", priority=0)
        view = _agg(issues=[triage, backlog])
        self.assertEqual(view.todo[0].key, "BAK-2")
        self.assertEqual(view.todo[1].key, "BAK-3")

    def test_priority_within_bucket_urgent_first(self) -> None:
        """Within same bucket, priority=1 (urgent) sorts before priority=4 (low)."""
        urgent = _make_issue(identifier="BAK-1", state_type="unstarted", priority=1)
        low = _make_issue(identifier="BAK-2", state_type="unstarted", priority=4)
        view = _agg(issues=[urgent, low])
        self.assertEqual(view.todo[0].key, "BAK-1")  # urgent first
        self.assertEqual(view.todo[1].key, "BAK-2")

    def test_no_priority_sorts_last_within_bucket(self) -> None:
        """priority=0 (no priority) sorts after priority=4 (low) within same bucket."""
        no_priority = _make_issue(identifier="BAK-1", state_type="unstarted", priority=0)
        low = _make_issue(identifier="BAK-2", state_type="unstarted", priority=4)
        view = _agg(issues=[no_priority, low])
        self.assertEqual(view.todo[0].key, "BAK-2")  # explicit priority first
        self.assertEqual(view.todo[1].key, "BAK-1")  # no-priority last


# ---------------------------------------------------------------------------
# Audit step attachment
# ---------------------------------------------------------------------------


class TestAuditStepAttachment(unittest.TestCase):
    """Cards with READY audit steps and actionable findings get audit_steps set."""

    def test_ready_audit_step_with_findings_attached(self) -> None:
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[audit_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].audit_steps), 1)
        self.assertEqual(cards[0].audit_steps[0].step_name, "build-audit")

    def test_completed_audit_step_not_attached(self) -> None:
        """Only READY audit steps with findings are attached, not COMPLETED."""
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.COMPLETED,
            findings=[finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[audit_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards[0].audit_steps), 0)

    def test_evaluation_only_findings_not_attached(self) -> None:
        """Evaluation findings are informational — don't attach as actionable."""
        from zing_ai.server.models import CriterionRating, EvaluationFinding, Rating

        eval_finding = EvaluationFinding(
            title="Audit complete",
            criteria=[CriterionRating(name="crit", rating=Rating.STRONG, justification="n/a")],
        )
        audit_step = _make_workflow_step(
            step_name="plan-audit",
            state=SessionState.READY,
            findings=[eval_finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[audit_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards[0].audit_steps), 0)

    def test_multiple_audit_types_all_attached(self) -> None:
        """plan-audit and build-audit both get surfaced in audit_steps."""
        finding = TextFinding(title="Issue found")
        plan_step = _make_workflow_step(
            step_name="plan-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        build_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[plan_step, build_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards[0].audit_steps), 2)


# ---------------------------------------------------------------------------
# Filtering
# ---------------------------------------------------------------------------


class TestFiltering(unittest.TestCase):
    """Cards without at least a ticket or a PR are excluded."""

    def test_card_with_only_session_excluded(self) -> None:
        """A session attached to a ticket card — but the ticket isn't in issues.
        The card is excluded because it has no ticket and no PR.
        """
        # Session has ticket_id but no issue is provided → session is skipped
        session = _make_session(session_id="s1", ticket_id="BAK-99")
        view = _agg(sessions=[session])
        self.assertEqual(len(_all_cards(view)), 0)

    def test_orphan_pr_card_included(self) -> None:
        """Orphan PR cards (no ticket) are included since they have a PR."""
        pr = _make_pr(number=7, head_ref="feature/misc", state="open")
        view = _agg(prs=[pr])
        self.assertEqual(len(_all_cards(view)), 1)


# ---------------------------------------------------------------------------
# Backward compat — legacy 4-arg call still works
# ---------------------------------------------------------------------------


class TestLegacyBackwardCompat(unittest.TestCase):
    """The old 4-arg aggregate() call still returns a Hub-based tuple."""

    def test_legacy_keyword_call_returns_tuple(self) -> None:
        """aggregate(issues=..., prs=..., sessions=..., current_username=...) returns Hub tuple."""

        result = aggregate(
            issues=[],
            prs=[],
            sessions=[],
            current_username="octocat",
        )
        # Returns (inbox_items, hubs) tuple
        self.assertIsInstance(result, tuple)
        inbox, hubs = result
        self.assertIsInstance(inbox, list)
        self.assertIsInstance(hubs, list)

    def test_legacy_positional_call_returns_tuple(self) -> None:
        """aggregate(issues, prs, sessions, username) positional call returns Hub tuple."""
        result = aggregate([], [], [], "octocat")
        self.assertIsInstance(result, tuple)


if __name__ == "__main__":
    unittest.main()
