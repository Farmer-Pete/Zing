"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import logging
from collections import defaultdict

from fastapi import APIRouter, Request
from fastapi.applications import FastAPI
from fastapi.responses import HTMLResponse

from zing_ai.server.command_center import aggregate
from zing_ai.server.templates import render

logger = logging.getLogger(__name__)
router = APIRouter()


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
    return HTMLResponse(
        render(
            "command_center.html",
            inbox_items=inbox_items,
            groups=groups,
            current_path="/command-center",
            last_polled_at=request.app.state.external_cache.last_polled_at,
            last_error=request.app.state.external_cache.last_error,
        )
    )
