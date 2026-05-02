"""Tests for flow.py: queue helpers and context builder."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime
from unittest.mock import MagicMock

from zing_ai.server.attention import AttentionItem
from zing_ai.server.flow import (
    _body_fragment_for,
    build_flow_context,
    resolve_active_item,
)
from zing_ai.server.models import (
    ClaudeCodeSession,
    SessionState,
    TextFinding,
    WorkflowStep,
    ZingSession,
)


def _make_item(
    action_type: str = "findings",
    session_id: str = "sess-1",
    step_id: str | None = "step-1",
    created_at: datetime | None = None,
) -> AttentionItem:
    return AttentionItem(
        action_type=action_type,  # type: ignore[arg-type]
        session_id=session_id,
        ticket_id=None,
        title="Test",
        description="desc",
        step_name="build-audit",
        finding_count=1,
        wait_seconds=60,
        card_key=session_id,
        step_id=step_id,
        created_at=created_at or datetime.now(UTC),
    )


# ---------------------------------------------------------------------------
# resolve_active_item
# ---------------------------------------------------------------------------


class TestResolveActiveItem(unittest.TestCase):
    """resolve_active_item returns the correct item."""

    def test_empty_queue_returns_none(self) -> None:
        result = resolve_active_item([], session_id=None, step_id=None)
        self.assertIsNone(result)

    def test_unset_cursor_returns_topmost(self) -> None:
        items = [_make_item(session_id="a"), _make_item(session_id="b")]
        result = resolve_active_item(items, session_id=None, step_id=None)
        self.assertIs(result, items[0])

    def test_cursor_matches_returns_that_item(self) -> None:
        items = [
            _make_item(session_id="a", step_id="st-a"),
            _make_item(session_id="b", step_id="st-b"),
        ]
        result = resolve_active_item(items, session_id="b", step_id="st-b")
        self.assertIs(result, items[1])

    def test_cursor_matches_session_only(self) -> None:
        """When step_id is None, match on session_id alone."""
        items = [
            _make_item(session_id="a", step_id="st-a"),
            _make_item(session_id="b", step_id="st-b"),
        ]
        result = resolve_active_item(items, session_id="b", step_id=None)
        self.assertIs(result, items[1])

    def test_stale_cursor_returns_topmost(self) -> None:
        items = [_make_item(session_id="a"), _make_item(session_id="b")]
        result = resolve_active_item(items, session_id="gone", step_id=None)
        self.assertIs(result, items[0])


# ---------------------------------------------------------------------------
# _body_fragment_for
# ---------------------------------------------------------------------------


class TestBodyFragmentFor(unittest.TestCase):
    """_body_fragment_for dispatches to the correct fragment path."""

    def test_findings_fragment(self) -> None:
        item = _make_item(action_type="findings")
        self.assertEqual(_body_fragment_for(item), "fragments/flow_body_findings.html")

    def test_questions_fragment(self) -> None:
        item = _make_item(action_type="questions")
        self.assertEqual(_body_fragment_for(item), "fragments/flow_body_question.html")

    def test_attach_fragment(self) -> None:
        item = _make_item(action_type="attach")
        self.assertEqual(_body_fragment_for(item), "fragments/flow_body_attach.html")

    def test_none_returns_empty_fragment(self) -> None:
        self.assertEqual(_body_fragment_for(None), "fragments/flow_body_empty.html")


# ---------------------------------------------------------------------------
# build_flow_context
# ---------------------------------------------------------------------------


def _make_manager_for_session(session):  # type: ignore[no-untyped-def]
    """Return a minimal mock SessionManager that resolves to the given session."""
    from zing_ai.server.sessions import SessionManager

    mgr = MagicMock(spec=SessionManager)
    mgr.get_session.return_value = session
    return mgr


class TestBuildFlowContext(unittest.TestCase):
    """build_flow_context returns correct shape."""

    def test_empty_queue(self) -> None:
        from zing_ai.server.sessions import SessionManager

        mgr = MagicMock(spec=SessionManager)
        ctx = build_flow_context(mgr, [], None)
        self.assertEqual(ctx["queue_count"], 0)
        self.assertEqual(ctx["active_position"], 0)
        self.assertEqual(ctx["active_findings"], [])
        self.assertEqual(ctx["initial_responses"], {})
        self.assertIsNone(ctx["active_session"])

    def test_findings_active_populates_findings_and_no_session(self) -> None:
        finding = TextFinding(title="Fix this")
        step = WorkflowStep(
            step_id="st-1",
            step_name="build-audit",
            sequence=0,
            state=SessionState.READY,
            findings=[finding],
        )
        zing_session = ZingSession(
            session_id="sess-1",
            title="My session",
            steps=[step],
        )
        item = _make_item(
            action_type="findings",
            session_id="sess-1",
            step_id="st-1",
        )
        mgr = _make_manager_for_session(zing_session)
        ctx = build_flow_context(mgr, [item], item)
        self.assertEqual(len(ctx["active_findings"]), 1)
        self.assertEqual(ctx["active_findings"][0].id, finding.id)
        self.assertIsNone(ctx["active_session"])
        self.assertEqual(ctx["active_position"], 1)
        self.assertEqual(ctx["queue_count"], 1)

    def test_attach_active_populates_active_session(self) -> None:
        cc_session = ClaudeCodeSession(
            session_id="cc-1",
            title="Terminal",
            terminal_session="zellij-abc",
        )
        item = _make_item(
            action_type="attach",
            session_id="cc-1",
            step_id=None,
        )
        mgr = _make_manager_for_session(cc_session)
        ctx = build_flow_context(mgr, [item], item)
        self.assertIs(ctx["active_session"], cc_session)
        self.assertEqual(ctx["active_findings"], [])
        self.assertEqual(ctx["initial_responses"], {})

    def test_initial_responses_populated_from_text_findings(self) -> None:
        from zing_ai.server.models import UserResponse

        finding = TextFinding(title="Question?")
        resp = UserResponse(answer="my answer")
        step = WorkflowStep(
            step_id="st-2",
            step_name="plan",
            sequence=0,
            state=SessionState.READY,
            findings=[finding],
            responses=[resp],
        )
        zing_session = ZingSession(
            session_id="sess-2",
            title="Plan session",
            steps=[step],
        )
        item = _make_item(
            action_type="questions",
            session_id="sess-2",
            step_id="st-2",
        )
        mgr = _make_manager_for_session(zing_session)
        ctx = build_flow_context(mgr, [item], item)
        self.assertEqual(ctx["initial_responses"], {finding.id: "my answer"})

    def test_next_ticket_id_resolved_from_queue(self) -> None:
        """next_ticket_id is the ticket_id of the item after active (wraps)."""
        from zing_ai.server.sessions import SessionManager

        mgr = MagicMock(spec=SessionManager)
        mgr.get_session.return_value = None

        # Build two items; second has a ticket_id.
        item_a = AttentionItem(
            action_type="findings",
            session_id="sess-a",
            ticket_id="BAK-111",
            title="First",
            description="",
            step_name="build-audit",
            finding_count=1,
            wait_seconds=0,
            card_key="sess-a",
            step_id="st-a",
            created_at=datetime.now(UTC),
        )
        item_b = AttentionItem(
            action_type="findings",
            session_id="sess-b",
            ticket_id="BAK-222",
            title="Second",
            description="",
            step_name="build-audit",
            finding_count=1,
            wait_seconds=0,
            card_key="sess-b",
            step_id="st-b",
            created_at=datetime.now(UTC),
        )
        ctx = build_flow_context(mgr, [item_a, item_b], item_a)
        self.assertEqual(ctx["next_ticket_id"], "BAK-222")

    def test_next_ticket_id_none_when_single_item(self) -> None:
        """next_ticket_id is None when there is only one item in the queue."""
        from zing_ai.server.sessions import SessionManager

        mgr = MagicMock(spec=SessionManager)
        mgr.get_session.return_value = None

        item = AttentionItem(
            action_type="findings",
            session_id="sess-x",
            ticket_id="BAK-999",
            title="Only",
            description="",
            step_name="build-audit",
            finding_count=1,
            wait_seconds=0,
            card_key="sess-x",
            step_id="st-x",
            created_at=datetime.now(UTC),
        )
        ctx = build_flow_context(mgr, [item], item)
        self.assertIsNone(ctx["next_ticket_id"])

    def test_next_ticket_id_none_when_next_has_no_ticket(self) -> None:
        """next_ticket_id is None when the next queue item has no ticket_id."""
        from zing_ai.server.sessions import SessionManager

        mgr = MagicMock(spec=SessionManager)
        mgr.get_session.return_value = None

        item_a = AttentionItem(
            action_type="findings",
            session_id="sess-a2",
            ticket_id="BAK-100",
            title="First",
            description="",
            step_name="build-audit",
            finding_count=1,
            wait_seconds=0,
            card_key="sess-a2",
            step_id="st-a2",
            created_at=datetime.now(UTC),
        )
        item_b = AttentionItem(
            action_type="findings",
            session_id="sess-b2",
            ticket_id=None,
            title="Second no ticket",
            description="",
            step_name="build-audit",
            finding_count=1,
            wait_seconds=0,
            card_key="sess-b2",
            step_id="st-b2",
            created_at=datetime.now(UTC),
        )
        ctx = build_flow_context(mgr, [item_a, item_b], item_a)
        self.assertIsNone(ctx["next_ticket_id"])
