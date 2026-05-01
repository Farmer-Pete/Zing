"""Tests for the attention queue logic."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta

from zing_ai.server.attention import AttentionItem, build_attention_queue
from zing_ai.server.models import (
    ClaudeCodeSession,
    Notification,
    SessionState,
    TextFinding,
    WorkflowStep,
    ZingSession,
)


def _make_zing_session(
    session_id: str = "sess-1",
    title: str = "Test Session",
    ticket_id: str | None = "TKT-1",
    steps: list[WorkflowStep] | None = None,
) -> ZingSession:
    return ZingSession(
        session_id=session_id,
        title=title,
        ticket_id=ticket_id,
        steps=steps or [],
    )


def _make_step(
    step_name: str,
    state: SessionState,
    created_at: datetime,
    findings: list | None = None,
) -> WorkflowStep:
    return WorkflowStep(
        step_name=step_name,
        sequence=0,
        state=state,
        created_at=created_at,
        findings=findings or [],
    )


def _make_claude_session(
    session_id: str = "cc-1",
    title: str = "Claude Session",
    ticket_id: str | None = None,
    notifications: list[Notification] | None = None,
) -> ClaudeCodeSession:
    return ClaudeCodeSession(
        session_id=session_id,
        title=title,
        ticket_id=ticket_id,
        notifications=notifications or [],
    )


def _make_notification(
    body: str = "What should I do?",
    created_at: datetime | None = None,
    answered_at: datetime | None = None,
) -> Notification:
    return Notification(
        title="Question",
        body=body,
        created_at=created_at or datetime(2026, 1, 1, 12, 0, 0),
        answered_at=answered_at,
    )


class TestZingSessionFindings(unittest.TestCase):
    """ZingSession with a READY step produces a findings AttentionItem."""

    def test_ready_audit_step_produces_findings_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step_created = datetime(2026, 1, 1, 12, 0, 0)
        finding = TextFinding(title="Issue found")
        step = _make_step("build-audit", SessionState.READY, step_created, [finding])
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert len(items) == 1
        item = items[0]
        assert item.action_type == "findings"
        assert item.session_id == "sess-1"
        assert item.ticket_id == "TKT-1"
        assert item.title == "Test Session"
        assert item.step_name == "build-audit"
        assert item.finding_count == 1
        assert item.wait_seconds == 600
        assert item.card_key == "sess-1"

    def test_plan_step_produces_questions_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 5, 0)
        step_created = datetime(2026, 1, 1, 12, 0, 0)
        step = _make_step("plan", SessionState.READY, step_created)
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert len(items) == 1
        assert items[0].action_type == "questions"

    def test_plan_audit_step_produces_findings_not_questions(self) -> None:
        now = datetime(2026, 1, 1, 12, 5, 0)
        step_created = datetime(2026, 1, 1, 12, 0, 0)
        step = _make_step("plan-audit", SessionState.READY, step_created)
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert len(items) == 1
        assert items[0].action_type == "findings"


class TestClaudeCodeSessionAttach(unittest.TestCase):
    """ClaudeCodeSession with a pending notification produces an attach AttentionItem."""

    def test_pending_notification_produces_attach_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif = _make_notification(
            body="Should I refactor this?",
            created_at=datetime(2026, 1, 1, 12, 0, 0),
        )
        session = _make_claude_session(
            session_id="cc-1",
            ticket_id="TKT-2",
            notifications=[notif],
        )

        items = build_attention_queue([session], now)

        assert len(items) == 1
        item = items[0]
        assert item.action_type == "attach"
        assert item.session_id == "cc-1"
        assert item.ticket_id == "TKT-2"
        assert item.description == "Should I refactor this?"
        assert item.step_name is None
        assert item.finding_count == 0
        assert item.wait_seconds == 600
        assert item.card_key == "cc-1"

    def test_latest_unanswered_notification_used(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        older = _make_notification(
            body="Older question",
            created_at=datetime(2026, 1, 1, 12, 0, 0),
        )
        newer = _make_notification(
            body="Newer question",
            created_at=datetime(2026, 1, 1, 12, 5, 0),
        )
        session = _make_claude_session(notifications=[older, newer])

        items = build_attention_queue([session], now)

        assert len(items) == 1
        # pending_question returns the last unanswered notification
        assert items[0].description == "Newer question"
        assert items[0].wait_seconds == 300


class TestQueueSorting(unittest.TestCase):
    """Queue is sorted by wait_seconds descending."""

    def test_sorted_by_wait_descending(self) -> None:
        now = datetime(2026, 1, 1, 12, 30, 0)

        # Step created 20 minutes ago -> 1200s wait
        step_old = _make_step("build-audit", SessionState.READY, datetime(2026, 1, 1, 12, 10, 0))
        session_old = _make_zing_session(session_id="s-old", steps=[step_old])

        # Step created 5 minutes ago -> 300s wait
        step_new = _make_step("build-audit", SessionState.READY, datetime(2026, 1, 1, 12, 25, 0))
        session_new = _make_zing_session(session_id="s-new", steps=[step_new])

        # ClaudeCode notification created 10 minutes ago -> 600s wait
        notif = _make_notification(created_at=datetime(2026, 1, 1, 12, 20, 0))
        session_cc = _make_claude_session(session_id="cc-mid", notifications=[notif])

        items = build_attention_queue([session_new, session_cc, session_old], now)

        assert len(items) == 3
        assert items[0].session_id == "s-old"
        assert items[0].wait_seconds == 1200
        assert items[1].session_id == "cc-mid"
        assert items[1].wait_seconds == 600
        assert items[2].session_id == "s-new"
        assert items[2].wait_seconds == 300


class TestNoAttentionNeeded(unittest.TestCase):
    """Sessions with no pending action produce no items."""

    def test_pending_step_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("build-audit", SessionState.PENDING, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert items == []

    def test_started_step_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("build-audit", SessionState.STARTED, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert items == []

    def test_completed_step_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("build-audit", SessionState.COMPLETED, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert items == []

    def test_stopped_step_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("build-audit", SessionState.STOPPED, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])

        items = build_attention_queue([session], now)

        assert items == []

    def test_claude_session_with_all_answered_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif = _make_notification(
            answered_at=datetime(2026, 1, 1, 12, 5, 0),
        )
        session = _make_claude_session(notifications=[notif])

        items = build_attention_queue([session], now)

        assert items == []

    def test_claude_session_with_no_notifications_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        session = _make_claude_session()

        items = build_attention_queue([session], now)

        assert items == []

    def test_zing_session_with_no_steps_produces_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        session = _make_zing_session(steps=[])

        items = build_attention_queue([session], now)

        assert items == []


class TestCompletedSessions(unittest.TestCase):
    """Completed sessions produce no items."""

    def test_all_completed_steps_produce_no_item(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step1 = _make_step("build-audit", SessionState.COMPLETED, datetime(2026, 1, 1, 12, 0, 0))
        step2 = _make_step("plan", SessionState.COMPLETED, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step1, step2])

        items = build_attention_queue([session], now)

        assert items == []

    def test_empty_session_list_returns_empty(self) -> None:
        now = datetime(2026, 1, 1, 12, 0, 0)
        items = build_attention_queue([], now)
        assert items == []


class TestNewFields(unittest.TestCase):
    """New AttentionItem fields: auto_dismiss, step_id, created_at, is_new."""

    # ------------------------------------------------------------------
    # stopped session is excluded
    # ------------------------------------------------------------------

    def test_stopped_pinned_session_excluded(self) -> None:
        """ClaudeCodeSession with state=STOPPED is not surfaced in the queue."""
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif = _make_notification(
            body="Hello?",
            created_at=datetime(2026, 1, 1, 12, 0, 0),
        )
        # Create a session that will appear STOPPED:
        # terminal_session set, launched_at well outside grace window, pinned=True.
        session = ClaudeCodeSession(
            session_id="cc-stopped",
            title="Stopped Session",
            terminal_session="old-session",
            launched_at=datetime(2025, 1, 1, 0, 0, 0),  # very old
            pinned=True,
            notifications=[notif],
        )
        # _session_alive defaults to False; _ever_seen_alive defaults to False;
        # launched_at is far in the past → state == STOPPED.
        assert session.state == SessionState.STOPPED

        items = build_attention_queue([session], now)
        assert items == []

    # ------------------------------------------------------------------
    # auto_dismiss set correctly
    # ------------------------------------------------------------------

    def test_auto_dismiss_false_for_pinned_attach(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif = _make_notification(created_at=datetime(2026, 1, 1, 12, 0, 0))
        session = ClaudeCodeSession(
            session_id="cc-pinned",
            title="Pinned",
            pinned=True,
            notifications=[notif],
        )
        items = build_attention_queue([session], now)
        assert len(items) == 1
        assert items[0].auto_dismiss is False

    def test_auto_dismiss_true_for_unpinned_attach(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif = _make_notification(created_at=datetime(2026, 1, 1, 12, 0, 0))
        session = _make_claude_session(notifications=[notif])
        # pinned defaults to False
        items = build_attention_queue([session], now)
        assert len(items) == 1
        assert items[0].auto_dismiss is True

    def test_auto_dismiss_false_for_findings(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("build-audit", SessionState.READY, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])
        items = build_attention_queue([session], now)
        assert len(items) == 1
        assert items[0].auto_dismiss is False

    def test_auto_dismiss_false_for_questions(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("plan", SessionState.READY, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])
        items = build_attention_queue([session], now)
        assert len(items) == 1
        assert items[0].auto_dismiss is False

    # ------------------------------------------------------------------
    # step_id set for findings/questions, None for attach
    # ------------------------------------------------------------------

    def test_step_id_set_for_findings(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("build-audit", SessionState.READY, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])
        items = build_attention_queue([session], now)
        assert items[0].step_id == step.step_id

    def test_step_id_set_for_questions(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        step = _make_step("plan", SessionState.READY, datetime(2026, 1, 1, 12, 0, 0))
        session = _make_zing_session(steps=[step])
        items = build_attention_queue([session], now)
        assert items[0].step_id == step.step_id

    def test_step_id_none_for_attach(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif = _make_notification(created_at=datetime(2026, 1, 1, 12, 0, 0))
        session = _make_claude_session(notifications=[notif])
        items = build_attention_queue([session], now)
        assert items[0].step_id is None

    # ------------------------------------------------------------------
    # created_at populated
    # ------------------------------------------------------------------

    def test_created_at_set_for_findings(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        created = datetime(2026, 1, 1, 12, 0, 0)
        step = _make_step("build-audit", SessionState.READY, created)
        session = _make_zing_session(steps=[step])
        items = build_attention_queue([session], now)
        assert items[0].created_at is not None

    def test_created_at_set_for_attach(self) -> None:
        now = datetime(2026, 1, 1, 12, 10, 0)
        notif_time = datetime(2026, 1, 1, 12, 0, 0)
        notif = _make_notification(created_at=notif_time)
        session = _make_claude_session(notifications=[notif])
        items = build_attention_queue([session], now)
        assert items[0].created_at is not None

    # ------------------------------------------------------------------
    # is_new property
    # ------------------------------------------------------------------

    def test_is_new_true_for_recent_item(self) -> None:
        item = AttentionItem(
            action_type="findings",
            session_id="s",
            ticket_id=None,
            title="T",
            description="d",
            step_name="build-audit",
            finding_count=0,
            wait_seconds=0,
            card_key="s",
            created_at=datetime.now(UTC),  # just now
        )
        assert item.is_new is True

    def test_is_new_false_for_old_item(self) -> None:
        item = AttentionItem(
            action_type="findings",
            session_id="s",
            ticket_id=None,
            title="T",
            description="d",
            step_name="build-audit",
            finding_count=0,
            wait_seconds=0,
            card_key="s",
            created_at=datetime.now(UTC) - timedelta(seconds=10),
        )
        assert item.is_new is False


if __name__ == "__main__":
    unittest.main()
