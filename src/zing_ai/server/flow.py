"""Flow mode helpers: queue filtering and context building."""

from __future__ import annotations

from typing import Any

from zing_ai.server.attention import AttentionItem
from zing_ai.server.models import ClaudeCodeSession, ZingSession
from zing_ai.server.sessions import SessionManager

# ---------------------------------------------------------------------------
# Queue helpers
# ---------------------------------------------------------------------------


def resolve_active_item(
    queue: list[AttentionItem],
    session_id: str | None,
    step_id: str | None,
) -> AttentionItem | None:
    """Return the item matching session_id/step_id, or the topmost item.

    If the queue is empty, returns ``None``.
    If session_id is None, returns the topmost item.
    If session_id matches an item (and step_id matches when non-None),
    returns that item. Otherwise falls back to the topmost item.
    """
    if not queue:
        return None
    if session_id is None:
        return queue[0]
    for item in queue:
        if item.session_id == session_id and (step_id is None or item.step_id == step_id):
            return item
    # session_id is stale — fall back to topmost.
    return queue[0]


def next_in_queue(
    queue: list[AttentionItem],
    current_session_id: str,
    direction: str,
) -> AttentionItem | None:
    """Return the next or previous item in the queue with wrap-around.

    Args:
        queue: Ordered list of AttentionItems from build_attention_queue.
        current_session_id: The session_id of the currently-displayed item.
        direction: ``"next"`` to advance forward, ``"prev"`` to go backward.

    Returns:
        The adjacent AttentionItem (with wrap-around), ``queue[0]`` if
        ``current_session_id`` is not found in the queue, or ``None`` if the
        queue is empty.

    Note:
        The fallback when ``current_session_id`` is missing from the queue is
        intentionally asymmetric: both ``"next"`` and ``"prev"`` return
        ``queue[0]`` regardless of direction. This is defensible UX — the
        topmost item is the most-urgent, and the user may have arrived at a
        dead URL (the session was completed/dismissed since the page loaded),
        so dropping them at the top of the queue is the most useful default.
    """
    if not queue:
        return None
    for i, item in enumerate(queue):
        if item.session_id == current_session_id:
            if direction == "prev":
                return queue[(i - 1) % len(queue)]
            return queue[(i + 1) % len(queue)]
    # current_session_id not in queue — fall back to first item (see Note above).
    return queue[0]


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
