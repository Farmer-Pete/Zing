"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import subprocess
import uuid
from datetime import UTC, datetime
from pathlib import Path

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.applications import FastAPI
from fastapi.responses import HTMLResponse, JSONResponse
from pydantic import BaseModel

from zing_ai.config import load_config
from zing_ai.launch import (
    LaunchError,
    build_claude_args,
    build_tmux_session_name,
    checkout_pr_branch,
    create_session_on_server,
    create_worktree,
    derive_branch_name,
    exec_or_detach,
    find_repo_path,
    move_ticket_in_progress,
    rollback_worktree,
)
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
            len(steps) if (steps := getattr(s, "steps", None)) else 0,
            steps[-1].state.value if steps else "",
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
    live_tmux_sessions: set[str] = getattr(app.state, "live_tmux_sessions", set())
    return render(
        "fragments/kanban_board.html",
        view=view,
        current_username=cache.github_username or "",
        live_tmux_sessions=live_tmux_sessions,
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


# ---------------------------------------------------------------------------
# Background session launch
# ---------------------------------------------------------------------------


class _LaunchBackgroundBody(BaseModel):
    card_key: str


@router.post("/command-center/launch-background")
async def launch_background(request: Request, body: _LaunchBackgroundBody) -> JSONResponse:
    """Launch a background Claude Code session for a Kanban card."""
    card_key = body.card_key

    # In-flight dedup lock — initialise lazily if not set by create_app.
    launching_set: set[str] = getattr(request.app.state, "launching_set", None) or set()
    if not hasattr(request.app.state, "launching_set"):
        request.app.state.launching_set = launching_set

    if card_key in launching_set:
        return JSONResponse({"error": "Launch already in progress for this card"}, status_code=409)

    launching_set.add(card_key)

    # Lazy-init repo path cache.
    repo_path_cache: dict[str, Path] = getattr(request.app.state, "repo_path_cache", None) or {}
    if not hasattr(request.app.state, "repo_path_cache"):
        request.app.state.repo_path_cache = repo_path_cache

    config = load_config()
    code_dir = config.git.code_dir
    if not code_dir:
        launching_set.discard(card_key)
        return JSONResponse({"error": "code_dir is not configured"}, status_code=422)

    # Locate the card in the current kanban view (search all columns).
    kanban_view: KanbanView = _build_view(request.app)
    card = None
    for column in (
        kanban_view.todo,
        kanban_view.in_progress,
        kanban_view.needs_review,
        kanban_view.done,
    ):
        for c in column:
            if c.key == card_key:
                card = c
                break
        if card is not None:
            break

    # Derive repo name and branch/PR info from the card.
    repo_name: str | None = None
    branch_name: str | None = None
    ticket_id: str | None = None
    pr_number: int | None = None
    pr_repo: str | None = None
    is_pr_card = False

    if card is not None:
        if card.prs:
            pr = card.prs[0]
            repo_name = pr.repo  # "owner/repo"
            branch_name = pr.head_ref
            pr_number = pr.number
            pr_repo = pr.repo
            is_pr_card = True
        if card.ticket is not None:
            ticket_id = card.ticket.identifier
            if repo_name is None and card.ticket.team:
                # Fallback: try derive branch from Linear — repo still unknown
                pass

    if repo_name is None:
        launching_set.discard(card_key)
        return JSONResponse(
            {"error": f"Cannot determine repository for card {card_key}"},
            status_code=422,
        )

    # Resolve local repo path.
    repo_root = find_repo_path(code_dir, repo_name, repo_path_cache)
    if repo_root is None:
        launching_set.discard(card_key)
        return JSONResponse(
            {"error": f"Repository {repo_name} not found under {code_dir}"},
            status_code=404,
        )

    # Derive branch name for ticket-only cards.
    if branch_name is None and ticket_id is not None and config.command_center.linear_api_key:
        try:
            branch_name = derive_branch_name(ticket_id, config.command_center.linear_api_key)
        except LaunchError as exc:
            launching_set.discard(card_key)
            return JSONResponse({"error": str(exc)}, status_code=500)

    if branch_name is None:
        launching_set.discard(card_key)
        return JSONResponse(
            {"error": f"Cannot determine branch for card {card_key}"},
            status_code=422,
        )

    title = card.ticket.title if (card is not None and card.ticket is not None) else card_key
    skill = "pr-audit" if is_pr_card else "new"
    target = card.prs[0].url if (is_pr_card and card is not None and card.prs) else ticket_id
    tmux_name = build_tmux_session_name(
        target=ticket_id or branch_name,
        pr_number=pr_number,
    )
    server_url = str(request.base_url).rstrip("/")

    logger.info("Launching background session for card %s (repo: %s)", card_key, repo_name)

    async def _run_launch() -> JSONResponse:
        worktree_path: Path | None = None
        try:

            def _blocking() -> tuple[str, Path]:
                nonlocal worktree_path
                # Create worktree
                if is_pr_card:
                    wt = checkout_pr_branch(
                        repo_root,
                        branch_name,  # type: ignore[arg-type]
                        config.git.worktree_root,
                    )
                else:
                    wt = create_worktree(
                        repo_root,
                        branch_name,  # type: ignore[arg-type]
                        config.git.worktree_root,
                        config.git.branch_prefix,
                    )
                worktree_path = wt

                # Move Linear ticket to in-progress for ticket-based launches.
                if ticket_id and config.command_center.linear_api_key:
                    move_ticket_in_progress(ticket_id, config.command_center.linear_api_key)

                session_id = str(uuid.uuid4())
                create_session_on_server(
                    server_url,
                    session_id,
                    title,
                    ticket_id,
                    str(wt),
                    skill,
                    pr_number=pr_number,
                    pr_repo=pr_repo,
                    tmux_session=tmux_name,
                )

                args = build_claude_args(skill, session_id, title, target)
                exec_or_detach(args, wt, tmux_session=tmux_name)

                return session_id, wt

            session_id, wt = await asyncio.to_thread(_blocking)

            logger.info("Background session launched: %s (tmux: %s)", session_id, tmux_name)

            # Notify all SSE connections of board change.
            for q in request.app.state.cc_queues:
                q.put_nowait("board_changed")

            return JSONResponse(
                {"status": "launched", "session_id": session_id, "tmux_session": tmux_name}
            )

        except (LaunchError, subprocess.CalledProcessError) as exc:
            logger.error("Background launch failed for card %s: %s", card_key, exc)
            if worktree_path is not None:
                try:
                    rollback_worktree(worktree_path)
                except Exception as rollback_exc:  # noqa: BLE001
                    logger.warning("Rollback failed: %s", rollback_exc)
            return JSONResponse({"error": str(exc)}, status_code=500)
        finally:
            launching_set.discard(card_key)

    return await _run_launch()
