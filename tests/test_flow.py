"""Tests for flow.py: FlowCursor, cursor helpers, queue filters, context builder."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime
from unittest.mock import MagicMock, patch

from zing_ai.server.attention import AttentionItem
from zing_ai.server.flow import (
    FlowCursor,
    _body_fragment_for,
    advance_cursor,
    auto_dismiss_unpinned,
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
    auto_dismiss: bool = False,
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
        auto_dismiss=auto_dismiss,
        step_id=step_id,
        created_at=created_at or datetime.now(UTC),
    )


# ---------------------------------------------------------------------------
# FlowCursor
# ---------------------------------------------------------------------------


class TestFlowCursor(unittest.TestCase):
    """FlowCursor is a frozen dataclass."""

    def test_default_empty_cursor(self) -> None:
        cursor = FlowCursor()
        self.assertIsNone(cursor.session_id)
        self.assertIsNone(cursor.step_id)

    def test_equality_same_values(self) -> None:
        a = FlowCursor(session_id="s1", step_id="st1")
        b = FlowCursor(session_id="s1", step_id="st1")
        self.assertEqual(a, b)

    def test_inequality_different_step_id(self) -> None:
        a = FlowCursor(session_id="s1", step_id="st1")
        b = FlowCursor(session_id="s1", step_id="st2")
        self.assertNotEqual(a, b)

    def test_immutability_raises_typeerror(self) -> None:
        cursor = FlowCursor(session_id="s1")
        with self.assertRaises((TypeError, AttributeError)):
            cursor.session_id = "s2"  # type: ignore[misc]


# ---------------------------------------------------------------------------
# resolve_active_item
# ---------------------------------------------------------------------------


class TestResolveActiveItem(unittest.TestCase):
    """resolve_active_item returns the correct item."""

    def test_empty_queue_returns_none(self) -> None:
        result = resolve_active_item([], FlowCursor())
        self.assertIsNone(result)

    def test_unset_cursor_returns_topmost(self) -> None:
        items = [_make_item(session_id="a"), _make_item(session_id="b")]
        result = resolve_active_item(items, FlowCursor())
        self.assertIs(result, items[0])

    def test_cursor_matches_returns_that_item(self) -> None:
        items = [
            _make_item(session_id="a", step_id="st-a"),
            _make_item(session_id="b", step_id="st-b"),
        ]
        cursor = FlowCursor(session_id="b", step_id="st-b")
        result = resolve_active_item(items, cursor)
        self.assertIs(result, items[1])

    def test_cursor_matches_session_only(self) -> None:
        """When cursor.step_id is None, match on session_id alone."""
        items = [
            _make_item(session_id="a", step_id="st-a"),
            _make_item(session_id="b", step_id="st-b"),
        ]
        cursor = FlowCursor(session_id="b", step_id=None)
        result = resolve_active_item(items, cursor)
        self.assertIs(result, items[1])

    def test_stale_cursor_returns_topmost(self) -> None:
        items = [_make_item(session_id="a"), _make_item(session_id="b")]
        cursor = FlowCursor(session_id="gone")
        result = resolve_active_item(items, cursor)
        self.assertIs(result, items[0])


# ---------------------------------------------------------------------------
# advance_cursor
# ---------------------------------------------------------------------------


class TestAdvanceCursor(unittest.TestCase):
    """advance_cursor wraps correctly in both directions."""

    def _queue(self) -> list[AttentionItem]:
        return [
            _make_item(session_id="a", step_id="s-a"),
            _make_item(session_id="b", step_id="s-b"),
            _make_item(session_id="c", step_id="s-c"),
        ]

    def test_forward(self) -> None:
        q = self._queue()
        cursor = FlowCursor(session_id="a", step_id="s-a")
        result = advance_cursor(q, cursor, "next")
        self.assertEqual(result, FlowCursor(session_id="b", step_id="s-b"))

    def test_backward(self) -> None:
        q = self._queue()
        cursor = FlowCursor(session_id="c", step_id="s-c")
        result = advance_cursor(q, cursor, "prev")
        self.assertEqual(result, FlowCursor(session_id="b", step_id="s-b"))

    def test_wrap_at_end(self) -> None:
        q = self._queue()
        cursor = FlowCursor(session_id="c", step_id="s-c")
        result = advance_cursor(q, cursor, "next")
        self.assertEqual(result, FlowCursor(session_id="a", step_id="s-a"))

    def test_wrap_at_start(self) -> None:
        q = self._queue()
        cursor = FlowCursor(session_id="a", step_id="s-a")
        result = advance_cursor(q, cursor, "prev")
        self.assertEqual(result, FlowCursor(session_id="c", step_id="s-c"))

    def test_empty_queue_returns_empty_cursor(self) -> None:
        result = advance_cursor([], FlowCursor(), "next")
        self.assertEqual(result, FlowCursor())


# ---------------------------------------------------------------------------
# auto_dismiss_unpinned
# ---------------------------------------------------------------------------


class TestAutoDismissUnpinned(unittest.TestCase):
    """auto_dismiss_unpinned filters correctly and never calls answered."""

    def test_unpinned_attach_is_dropped(self) -> None:
        attach = _make_item(action_type="attach", session_id="cc-1", auto_dismiss=True)
        other = _make_item(action_type="findings", session_id="sess-2")
        result = auto_dismiss_unpinned([attach, other], attach)
        self.assertNotIn(attach, result)
        self.assertIn(other, result)

    def test_pinned_attach_stays(self) -> None:
        attach = _make_item(action_type="attach", session_id="cc-1", auto_dismiss=False)
        result = auto_dismiss_unpinned([attach], attach)
        self.assertIn(attach, result)

    def test_findings_never_dropped(self) -> None:
        item = _make_item(action_type="findings", auto_dismiss=False)
        result = auto_dismiss_unpinned([item], item)
        self.assertIn(item, result)

    def test_questions_never_dropped(self) -> None:
        item = _make_item(action_type="questions", auto_dismiss=False)
        result = auto_dismiss_unpinned([item], item)
        self.assertIn(item, result)

    def test_does_not_call_mark_pending_question_answered(self) -> None:
        """auto_dismiss_unpinned must not touch any persistence path."""
        attach = _make_item(action_type="attach", auto_dismiss=True)
        with patch("zing_ai.server.flow.SessionManager", create=True) as mock_cls:
            # Importing flow should not trigger any SessionManager method.
            auto_dismiss_unpinned([attach], attach)
            mock_cls.assert_not_called()


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
