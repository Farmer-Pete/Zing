"""Flow mode helpers: cursor management, queue filtering, and context building."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Literal

from zing_ai.server.attention import AttentionItem
from zing_ai.server.models import ClaudeCodeSession, ZingSession
from zing_ai.server.sessions import SessionManager


@dataclass(frozen=True)
class FlowCursor:
    """Immutable cursor pointing at the active Flow item.

    Stored on ``app.state.flow_cursor``. Resets on server restart.
    """

    session_id: str | None = None
    step_id: str | None = None


# ---------------------------------------------------------------------------
# Queue helpers
# ---------------------------------------------------------------------------


def resolve_active_item(queue: list[AttentionItem], cursor: FlowCursor) -> AttentionItem | None:
    """Return the item the cursor points at, or the topmost item.

    If the queue is empty, returns ``None``.
    If the cursor is unset (session_id is None), returns the topmost item.
    If cursor.session_id matches an item (and cursor.step_id matches when
    non-None), returns that item. Otherwise falls back to the topmost item.
    """
    if not queue:
        return None
    if cursor.session_id is None:
        return queue[0]
    for item in queue:
        if item.session_id == cursor.session_id and (
            cursor.step_id is None or item.step_id == cursor.step_id
        ):
            return item
    # Cursor is stale — fall back to topmost.
    return queue[0]


def advance_cursor(
    queue: list[AttentionItem],
    cursor: FlowCursor,
    direction: Literal["next", "prev"],
) -> FlowCursor:
    """Return a new FlowCursor pointing at the next or previous item.

    Wraps at the ends of the queue. If the cursor doesn't match any item,
    treats the topmost item as current.
    """
    if not queue:
        return FlowCursor()
    current = resolve_active_item(queue, cursor)
    try:
        idx = queue.index(current)  # type: ignore[arg-type]
    except ValueError:
        idx = 0
    new_idx = (idx + 1) % len(queue) if direction == "next" else (idx - 1) % len(queue)
    new_item = queue[new_idx]
    return FlowCursor(session_id=new_item.session_id, step_id=new_item.step_id)


def auto_dismiss_unpinned(
    queue: list[AttentionItem], leaving_item: AttentionItem
) -> list[AttentionItem]:
    """Return the queue with ``leaving_item`` removed iff it is auto-dismissible.

    Findings/questions items always have ``auto_dismiss=False`` by construction
    so they are never dropped. Only unpinned attach items are removed.

    This is a **pure function**: it does not call
    ``mark_pending_question_answered`` or any other persistence path.
    """
    if leaving_item.auto_dismiss:
        return [item for item in queue if item is not leaving_item]
    return list(queue)


# ---------------------------------------------------------------------------
# Fragment dispatch
# ---------------------------------------------------------------------------

_BODY_FRAGMENTS: dict[str, str] = {
    "findings": "fragments/flow_body_findings.html",
    "questions": "fragments/flow_body_question.html",
    "attach": "fragments/flow_body_attach.html",
}


def _body_fragment_for(active: AttentionItem | None) -> str:
    """Return the template path for the active item's body fragment."""
    if active is None:
        return "fragments/flow_body_empty.html"
    return _BODY_FRAGMENTS[active.action_type]


# ---------------------------------------------------------------------------
# Template context builder
# ---------------------------------------------------------------------------


def build_flow_context(
    manager: SessionManager,
    queue: list[AttentionItem],
    active: AttentionItem | None,
) -> dict[str, Any]:
    """Build template context for the Flow page.

    Mirrors the structure of ``build_drawer_context`` so fragment templates
    can use the same field names.

    Returns a dict containing:
    - ``queue``, ``active``, ``queue_count``, ``active_position``
    - ``active_session`` — resolved ClaudeCodeSession for attach mode, else None
    - ``active_findings`` — findings list for the active step (findings/questions
      action types only; empty list otherwise)
    - ``initial_responses`` — ``{finding_id: str}`` dict mirroring
      ``build_drawer_context``'s ``saved_responses`` shape
    """
    queue_count = len(queue)
    active_position = (queue.index(active) + 1) if active and active in queue else 0

    active_session: ClaudeCodeSession | None = None
    active_findings: list[Any] = []
    initial_responses: dict[str, str] = {}

    if active is not None:
        if active.action_type == "attach":
            session = manager.get_session(active.session_id)
            if isinstance(session, ClaudeCodeSession):
                active_session = session
        elif active.action_type in ("findings", "questions"):
            session = manager.get_session(active.session_id)
            if isinstance(session, ZingSession):
                # Locate the step by step_id when present, otherwise use last READY step.
                step = None
                if active.step_id is not None:
                    step = next(
                        (s for s in session.steps if s.step_id == active.step_id),
                        None,
                    )
                if step is None:
                    from zing_ai.server.models import SessionState

                    step = next(
                        (s for s in reversed(session.steps) if s.state == SessionState.READY),
                        None,
                    )
                if step is not None:
                    active_findings = list(step.findings)
                    # Build initial_responses mirroring build_drawer_context's
                    # saved_responses logic (finding_id-keyed, str values).
                    responses = step.responses or []
                    same_length = len(responses) == len(step.findings)
                    for idx, finding in enumerate(step.findings):
                        if not (same_length and idx < len(responses)):
                            continue
                        resp = responses[idx]
                        if finding.type == "triage":
                            if resp.action is not None:
                                initial_responses[finding.id] = resp.action.value
                            if resp.selected is not None:
                                initial_responses[f"{finding.id}_approach"] = resp.selected
                                if resp.other_text is not None:
                                    initial_responses[f"{finding.id}_approach_other"] = (
                                        resp.other_text
                                    )
                            if resp.complexity is not None:
                                initial_responses[f"{finding.id}_complexity"] = (
                                    resp.complexity.value
                                )
                        elif finding.type == "text" and resp.answer is not None:
                            initial_responses[finding.id] = resp.answer

    # Compute the ticket_id of the next item in the queue (wraps around).
    # Used by the toolbar's "Next ▸ <ticket>" button.
    next_ticket_id: str | None = None
    if active and queue:
        try:
            idx = queue.index(active)
        except ValueError:
            idx = -1
        if idx >= 0:
            next_idx = (idx + 1) % len(queue)
            if next_idx != idx and queue[next_idx].ticket_id:
                next_ticket_id = queue[next_idx].ticket_id

    return {
        "queue": queue,
        "active": active,
        "queue_count": queue_count,
        "active_position": active_position,
        "active_session": active_session,
        "active_findings": active_findings,
        "initial_responses": initial_responses,
        "next_ticket_id": next_ticket_id,
    }
