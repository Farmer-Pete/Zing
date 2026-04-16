"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import asyncio
import contextlib
import logging
from collections import defaultdict
from datetime import UTC, datetime

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.applications import FastAPI
from fastapi.responses import HTMLResponse

from zing_ai.server.command_center import aggregate
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


def _build_view(app: FastAPI) -> tuple[list, dict[str, list]]:
    """Re-aggregate from cache + sessions, group hubs by team."""
    cache = app.state.external_cache
    sessions = app.state.session_manager.list_sessions()
    inbox_items, hubs = aggregate(
        cache.issues,
        cache.prs,
        sessions,
        cache.github_username,
    )
    groups: dict[str, list] = defaultdict(list)
    for hub in hubs:
        groups[hub.team or "Standalone"].append(hub)
    return inbox_items, dict(groups)


def render_hub_fragment(app: FastAPI, hub_id: str) -> str:
    """Render a single hub card. Used by SSE patches."""
    _, groups = _build_view(app)
    for hubs in groups.values():
        for hub in hubs:
            if hub.signal_key == hub_id or hub.id == hub_id:
                return render("fragments/cc_hub.html", hub=hub)
    return ""  # hub disappeared between events


def render_inbox_fragment(app: FastAPI) -> str:
    """Render the full inbox. Used by SSE inbox_changed events."""
    inbox_items, _ = _build_view(app)
    return render("fragments/inbox_list.html", inbox_items=inbox_items)


@router.get("/command-center", response_class=HTMLResponse)
async def get_command_center(request: Request) -> HTMLResponse:
    """Return the Command Center HTML page."""
    inbox_items, groups = _build_view(request.app)  # type: ignore[arg-type]
    cache = request.app.state.external_cache  # type: ignore[attr-defined]
    return HTMLResponse(
        render(
            "command_center.html",
            inbox_items=inbox_items,
            groups=groups,
            current_path="/command-center",
            last_polled_at=cache.last_polled_at,
            last_polled_label=_format_last_polled(cache.last_polled_at),
            last_error=cache.last_error,
        )
    )


@router.get("/command-center/events")
@datastar_response
async def command_center_events(request: Request):  # noqa: ANN201
    """SSE endpoint that pushes Command Center updates to the browser."""

    async def _generate():  # noqa: ANN202
        """Yield SSE events for hub changes, inbox updates, and poll status."""
        queue: asyncio.Queue[str] = asyncio.Queue(maxsize=100)
        request.app.state.cc_queues.append(queue)  # type: ignore[attr-defined]
        try:
            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=30.0)
                except TimeoutError:
                    yield SSE.patch_signals({"_heartbeat": True})
                    continue
                kind, _, target = event.partition(":")
                if kind == "hub_changed":
                    html = render_hub_fragment(request.app, target)  # type: ignore[arg-type]
                    if html:
                        yield SSE.patch_elements(
                            html,
                            selector=f"#hub-{target}",
                            mode=ElementPatchMode.OUTER,
                        )
                elif kind == "inbox_changed":
                    html = render_inbox_fragment(request.app)  # type: ignore[arg-type]
                    yield SSE.patch_elements(
                        html,
                        selector="#inbox-list",
                        mode=ElementPatchMode.OUTER,
                    )
                elif kind in ("hub_added", "hub_removed"):
                    # Full hub-list re-render.
                    _, groups = _build_view(request.app)  # type: ignore[arg-type]
                    html = render("fragments/hubs_list.html", groups=groups)
                    yield SSE.patch_elements(
                        html,
                        selector="#hubs-list",
                        mode=ElementPatchMode.OUTER,
                    )
                elif kind == "poll_status":
                    cache = request.app.state.external_cache  # type: ignore[attr-defined]
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
                request.app.state.cc_queues.remove(queue)  # type: ignore[attr-defined]

    return _generate()
