"""Flow mode helpers: queue filtering and context building."""

from __future__ import annotations

import json
import logging
from pathlib import Path
from typing import Any

from zing_ai.server.attention import AttentionItem
from zing_ai.server.models import ClaudeCodeSession, ZingSession
from zing_ai.server.sessions import SessionManager

logger = logging.getLogger("zing_ai.server.flow")

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
    """Return the next or previous item in the queue, or ``None`` at the edge.

    Args:
        queue: Ordered list of AttentionItems from build_attention_queue.
        current_session_id: The session_id of the currently-displayed item.
        direction: ``"next"`` to advance forward, ``"prev"`` to go backward.

    Returns:
        The adjacent AttentionItem, ``queue[0]`` if ``current_session_id`` is
        not found in the queue, or ``None`` if the queue is empty *or* the
        cursor is already at the relevant edge (no next from the last item;
        no prev from the first). The endpoint translates ``None`` into a
        navigate-to-empty-state redirect.

    Note:
        The fallback when ``current_session_id`` is missing from the queue is
        intentionally asymmetric: both ``"next"`` and ``"prev"`` return
        ``queue[0]`` regardless of direction. This is defensible UX — the
        topmost item is the most-urgent, and the user may have arrived at a
        dead URL (the session was completed/dismissed since the page loaded),
        so dropping them at the top of the queue is the most useful default.

        Wrap-around was removed deliberately: with a single-item queue (a
        common case for an attached terminal), wrap-around made Next/Prev
        no-op back to the same item — the user-visible bug.
    """
    if not queue:
        return None
    for i, item in enumerate(queue):
        if item.session_id == current_session_id:
            if direction == "prev":
                return queue[i - 1] if i > 0 else None
            return queue[i + 1] if i + 1 < len(queue) else None
    # current_session_id not in queue — fall back to first item (see Note above).
    return queue[0]


# ---------------------------------------------------------------------------
# Fragment dispatch
# ---------------------------------------------------------------------------

_BODY_FRAGMENTS: dict[str, str] = {
    "findings": "fragments/flow_body_findings.html",
    "questions": "fragments/flow_body_question.html",
    "attach": "fragments/flow_body_attach.html",
    "viz_preview": "fragments/flow_body_viz_preview.html",
}


def _body_fragment_for(active: AttentionItem | None) -> str:
    """Return the template path for the active item's body fragment."""
    if active is None:
        return "fragments/flow_body_empty.html"
    return _BODY_FRAGMENTS[active.action_type]


# ---------------------------------------------------------------------------
# Template context builder
# ---------------------------------------------------------------------------


def _load_viz_preview_context(session: ZingSession) -> dict[str, Any]:
    """Read the pending viz+md off disk and lay them out for rendering.

    Returns the keys the ``flow_body_viz_preview.html`` template expects:
    ``rendered_markdown``, ``steps``, ``cross_flows``, ``kinds``, ``focused_step``,
    ``default_pan_y``, ``default_scale``, ``viz_preview`` (the VizPreview model).
    Returns an empty dict on any IO/parse error after logging — the body
    fragment renders a fallback when these keys are missing.
    """
    preview = session.pending_viz_preview
    if preview is None:
        return {}

    # Import locally to keep flow.py's module-load cheap (graphviz layout is heavy).
    from zing_ai.server import focus_layout as focus_layout_mod
    from zing_ai.server.routes_plans import _build_render_context, _laid_out_graph
    from zing_ai.server.templates import render_markdown

    viz_path = Path(preview.viz_path)
    md_path = Path(preview.md_path)
    try:
        graph = json.loads(viz_path.read_text(encoding="utf-8"))
        md_text = md_path.read_text(encoding="utf-8")
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning("Failed to load viz preview for %s: %s", session.session_id, exc)
        return {"viz_preview": preview}

    laid_out = _laid_out_graph(viz_path, graph)
    positions = focus_layout_mod.default_grid(laid_out)
    context = _build_render_context(laid_out, positions, focused_step=None)
    return {
        **context,
        "rendered_markdown": render_markdown(md_text),
        "default_pan_y": focus_layout_mod.DEFAULT_PAN_Y,
        "default_scale": focus_layout_mod.DEFAULT_SCALE,
        "viz_preview": preview,
        # _viewer.html and _card.html build their @post URLs from {{ session_id }}.
        # Without this, card clicks hit "/command-center//plan/focus" and silently
        # do nothing — symptom: clicking a step does not zoom in the preview drawer.
        "session_id": session.session_id,
    }


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
    viz_preview_context: dict[str, Any] = {}

    if active is not None:
        if active.action_type == "viz_preview":
            session = manager.get_session(active.session_id)
            if isinstance(session, ZingSession):
                viz_preview_context = _load_viz_preview_context(session)
        elif active.action_type == "attach":
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

    # Compute the ticket_id of the next item in the queue. Used by the
    # toolbar's "Next ▸ <ticket>" button. None when the active item is the
    # last in the queue (no wrap-around).
    next_ticket_id: str | None = None
    if active and queue:
        try:
            idx = queue.index(active)
        except ValueError:
            idx = -1
        if 0 <= idx < len(queue) - 1 and queue[idx + 1].ticket_id:
            next_ticket_id = queue[idx + 1].ticket_id

    return {
        "queue": queue,
        "active": active,
        "queue_count": queue_count,
        "active_position": active_position,
        "active_session": active_session,
        "active_findings": active_findings,
        "initial_responses": initial_responses,
        "next_ticket_id": next_ticket_id,
        **viz_preview_context,
    }
