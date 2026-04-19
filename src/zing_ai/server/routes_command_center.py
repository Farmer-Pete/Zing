"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import asyncio
import contextlib
import logging
from datetime import UTC, datetime

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.applications import FastAPI
from fastapi.responses import HTMLResponse

from zing_ai.server.command_center import aggregate
from zing_ai.server.models_external import KanbanView
from zing_ai.server.templates import render

logger = logging.getLogger(__name__)
router = APIRouter()


def _format_last_polled(dt: datetime | None) -> str:
    """Return a compact, glanceable label for *dt*.

    Examples: ``"just now"``, ``"42s ago"``, ``"5m ago"``, ``"2h ago"``,
    ``"yesterday"``. Returns an empty string when *dt* is ``None`` so the
    template's fallback ``"Waiting for first poll"`` shows instead.
    """
    if dt is None:
        return ""
    now = datetime.now(dt.tzinfo) if dt.tzinfo is not None else datetime.now(UTC)
    # dt may still be naive if someone bypassed the tz-aware write path.
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=UTC)
    seconds = int((now - dt).total_seconds())
    if seconds < 5:
        return "just now"
    if seconds < 60:
        return f"{seconds}s ago"
    if seconds < 3600:
        return f"{seconds // 60}m ago"
    if seconds < 86400:
        return f"{seconds // 3600}h ago"
    if seconds < 172800:
        return "yesterday"
    return f"{seconds // 86400}d ago"


def _view_fingerprint(cache, sessions: list) -> tuple:  # noqa: ANN001
    """Cheap fingerprint that captures what ``_build_view`` depends on.

    Uses ``cache.version`` (bumped by the poller on snapshot change) plus a
    per-session tuple of (id, ticket_id, step count, last-step state). Two
    SSE events queued within the same poll cycle typically share the same
    fingerprint, which lets ``_build_view`` skip re-aggregating the world.
    """
    sessions_sig = tuple(
        (
            s.session_id,
            s.ticket_id,
            len(s.steps),
            s.steps[-1].state.value if s.steps else "",
        )
        for s in sessions
    )
    return (cache.version, len(sessions), sessions_sig)


def _build_view(app: FastAPI) -> KanbanView:
    """Re-aggregate from cache + sessions, return a KanbanView.

    Results are memoised on ``app.state._cc_view_memo`` keyed on
    :func:`_view_fingerprint`. Back-to-back SSE events fired by one poll
    reuse the same aggregation rather than repeating the full pass per event.
    """
    cache = app.state.external_cache
    sessions = app.state.session_manager.list_sessions()
    fingerprint = _view_fingerprint(cache, sessions)
    memo = getattr(app.state, "_cc_view_memo", None)
    if memo is not None and memo[0] == fingerprint:
        return memo[1]

    view = aggregate(
        cache.issues,
        cache.prs,
        cache.recent_prs,
        cache.completed_issues,
        sessions,
        cache.github_username,
    )
    app.state._cc_view_memo = (fingerprint, view)
    return view  # type: ignore[return-value]


def render_board_fragment(app: FastAPI) -> str:
    """Render the full kanban board. Used by SSE board_changed events."""
    view = _build_view(app)
    cache = app.state.external_cache
    return render(
        "fragments/kanban_board.html",
        view=view,
        current_username=cache.github_username or "",
    )  # hub disappeared between events


@router.get("/command-center", response_class=HTMLResponse)
async def get_command_center(request: Request) -> HTMLResponse:
    """Return the Command Center HTML page."""
    view = _build_view(request.app)
    cache = request.app.state.external_cache
    return HTMLResponse(
        render(
            "command_center.html",
            view=view,
            current_path="/command-center",
            last_polled_at=cache.last_polled_at,
            last_polled_label=_format_last_polled(cache.last_polled_at),
            last_error=cache.last_error,
            body_class="command-center",
            current_username=cache.github_username or "",
        )
    )


@router.get("/command-center/events")
@datastar_response
async def command_center_events(request: Request):  # noqa: ANN201
    """SSE endpoint that pushes Command Center updates to the browser."""

    async def _generate():  # noqa: ANN202
        """Yield SSE events for board changes and poll status."""
        queue: asyncio.Queue[str] = asyncio.Queue(maxsize=100)
        request.app.state.cc_queues.append(queue)
        try:
            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=30.0)
                except TimeoutError:
                    yield SSE.patch_signals({"_heartbeat": True})
                    continue
                kind, _, _target = event.partition(":")
                if kind == "board_changed":
                    html = render_board_fragment(request.app)
                    yield SSE.patch_elements(
                        html,
                        selector="#kanban-board",
                        mode=ElementPatchMode.OUTER,
                    )
                elif kind == "poll_status":
                    cache = request.app.state.external_cache
                    yield SSE.patch_signals(
                        {
                            "lastPolledLabel": _format_last_polled(cache.last_polled_at),
                            "lastError": cache.last_error or "",
                        }
                    )
        finally:
            # Suppress ValueError in case the queue was already cleared (tests
            # or admin endpoints may reset cc_queues); letting it raise here
            # would mask the real cancellation reason in logs.
            with contextlib.suppress(ValueError):
                request.app.state.cc_queues.remove(queue)

    return _generate()
