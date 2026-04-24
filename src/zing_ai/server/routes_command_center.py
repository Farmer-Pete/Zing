"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import shlex
import subprocess
import sys
import uuid
from datetime import UTC, datetime
from pathlib import Path

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.applications import FastAPI
from fastapi.responses import HTMLResponse, JSONResponse

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
from zing_ai.server.models import ClaudeCodeSession
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


class _WorktreeEntry:
    """Container for a ClaudeCodeSession worktree entry with orphan flag."""

    def __init__(self, session: ClaudeCodeSession, orphaned: bool) -> None:
        self.session = session
        self.orphaned = orphaned


def _build_tray_data(
    view: KanbanView,
    sessions: list,
    live_tmux_sessions: set[str],
) -> dict:
    """Build running_sessions, worktree_entries, running_count, orphan_count for the tray.

    Args:
        view: The current KanbanView (all columns).
        sessions: All sessions from the session manager.
        live_tmux_sessions: Set of currently alive tmux session names.

    Returns:
        A dict with keys: running_sessions, worktree_entries, running_count, orphan_count.
    """
    # Collect all card keys in the done column and all existing card keys.
    done_keys: set[str] = {card.key for card in view.done}
    all_card_keys: set[str] = {
        card.key
        for col in (view.todo, view.in_progress, view.needs_review, view.done)
        for card in col
    }

    running_sessions: list[ClaudeCodeSession] = []
    worktree_entries: list[_WorktreeEntry] = []
    seen_worktree_paths: set[str] = set()

    for session in sessions:
        if not isinstance(session, ClaudeCodeSession):
            continue

        # Track running sessions (alive in tmux).
        if session.tmux_session and session.tmux_session in live_tmux_sessions:
            running_sessions.append(session)

        # Build worktree entries for sessions with a worktree_path.
        # Deduplicate by path — multiple sessions can reference the same worktree.
        if session.worktree_path and session.worktree_path not in seen_worktree_paths:
            seen_worktree_paths.add(session.worktree_path)
            ticket_id = session.ticket_id
            # Orphaned: card is in done column, or no card exists at all.
            if ticket_id is None or ticket_id in done_keys or ticket_id not in all_card_keys:
                orphaned = True
            else:
                orphaned = False
            worktree_entries.append(_WorktreeEntry(session=session, orphaned=orphaned))

    orphan_count = sum(1 for e in worktree_entries if e.orphaned)

    return {
        "running_sessions": running_sessions,
        "worktree_entries": worktree_entries,
        "running_count": len(running_sessions),
        "orphan_count": orphan_count,
    }


def render_board_fragment(app: FastAPI) -> str:
    """Render the full kanban board. Used by SSE board_changed events."""
    view = _build_view(app)
    cache = app.state.external_cache
    live_tmux_sessions: set[str] = getattr(app.state, "live_tmux_sessions", set())
    config = load_config()
    return render(
        "fragments/kanban_board.html",
        view=view,
        current_username=cache.github_username or "",
        live_tmux_sessions=live_tmux_sessions,
        iterm2_integration=config.command_center.iterm2_integration,
    )


def _render_tray_fragment(app: FastAPI) -> str:
    """Render the management tray. Used by SSE board_changed events."""
    view = _build_view(app)
    live_tmux_sessions: set[str] = getattr(app.state, "live_tmux_sessions", set())
    sessions = app.state.session_manager.list_sessions()
    tray_data = _build_tray_data(view, sessions, live_tmux_sessions)
    return render("fragments/management_tray.html", **tray_data)


@router.get("/command-center", response_class=HTMLResponse)
async def get_command_center(request: Request) -> HTMLResponse:
    """Return the Command Center HTML page."""
    config = load_config()
    view = _build_view(request.app)
    cache = request.app.state.external_cache
    live_tmux_sessions: set[str] = getattr(request.app.state, "live_tmux_sessions", set())
    sessions = request.app.state.session_manager.list_sessions()
    tray_data = _build_tray_data(view, sessions, live_tmux_sessions)
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
            live_tmux_sessions=live_tmux_sessions,
            iterm2_integration=config.command_center.iterm2_integration,
            **tray_data,
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
                    tray_html = _render_tray_fragment(request.app)
                    yield SSE.patch_elements(
                        tray_html,
                        selector="#mgmt-tray",
                        mode=ElementPatchMode.INNER,
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


@router.post("/command-center/refresh")
async def refresh_command_center(request: Request) -> JSONResponse:
    """Trigger an immediate poll of Linear/GitHub and refresh the board."""
    poller = getattr(request.app.state, "poller", None)
    if poller is None:
        return JSONResponse({"error": "Poller not available"}, status_code=503)
    try:
        await poller._poll_once()  # noqa: SLF001
    except Exception as exc:  # noqa: BLE001
        logger.error("Manual refresh failed: %s", exc)
        return JSONResponse({"error": str(exc)}, status_code=500)
    return JSONResponse({"status": "refreshed"})


# ---------------------------------------------------------------------------
# iTerm2 tmux attach
# ---------------------------------------------------------------------------


@router.post("/command-center/attach-session")
async def attach_session(request: Request) -> JSONResponse:
    """Open a tmux session in iTerm2 using control mode (tmux -CC)."""
    if sys.platform != "darwin":
        return JSONResponse(
            {"error": "iTerm2 integration is only available on macOS"}, status_code=422
        )

    try:
        body = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)

    tmux_session = body.get("tmux_session")
    if not tmux_session:
        return JSONResponse({"error": "tmux_session is required"}, status_code=400)

    import shutil

    tmux_path = shutil.which("tmux")
    if not tmux_path:
        return JSONResponse({"error": "tmux not found on PATH"}, status_code=500)

    # Validate tmux session name to prevent injection
    safe_name = shlex.quote(tmux_session)

    applescript = (
        'tell application "iTerm2"\n'
        "  create window with default profile "
        f'command "{tmux_path} -CC attach -t {safe_name}"\n'
        "end tell"
    )

    try:
        subprocess.Popen(
            ["osascript", "-e", applescript],
            start_new_session=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except FileNotFoundError:
        return JSONResponse({"error": "osascript not found"}, status_code=500)

    return JSONResponse({"status": "attached", "tmux_session": tmux_session})


# ---------------------------------------------------------------------------
# Background session launch
# ---------------------------------------------------------------------------


@router.post("/command-center/launch-background")
async def launch_background(request: Request) -> JSONResponse:
    """Launch a background Claude Code session for a Kanban card."""
    try:
        body = await request.json()
    except json.JSONDecodeError:
        raw = await request.body()
        logger.error("launch-background: invalid JSON body: %s", raw[:500])
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)

    # Datastar sends payload under a "payload" key in the signal store.
    payload = body.get("payload", body)
    card_key = payload.get("card_key")
    if not card_key:
        logger.error("launch-background: missing card_key in body: %s", body)
        return JSONResponse({"error": "card_key is required"}, status_code=400)

    logger.debug("launch-background: card_key=%s, body keys=%s", card_key, list(body.keys()))

    # In-flight dedup lock — initialise lazily if not set by create_app.
    launching_set: set[str] = getattr(request.app.state, "launching_set", None) or set()
    if not hasattr(request.app.state, "launching_set"):
        request.app.state.launching_set = launching_set

    if card_key in launching_set:
        logger.warning("launch-background: duplicate launch for card %s", card_key)
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
        logger.error("launch-background: code_dir is not configured")
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
        logger.error(
            "launch-background: cannot determine repo for card %s "
            "(has_ticket=%s, has_prs=%s, card_found=%s)",
            card_key,
            card is not None and card.ticket is not None,
            card is not None and bool(card.prs),
            card is not None,
        )
        return JSONResponse(
            {"error": f"Cannot determine repository for card {card_key}"},
            status_code=422,
        )

    # Resolve local repo path.
    repo_root = find_repo_path(code_dir, repo_name, repo_path_cache)
    if repo_root is None:
        launching_set.discard(card_key)
        logger.error("launch-background: repo %s not found under %s", repo_name, code_dir)
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
            logger.error("launch-background: derive_branch_name failed for %s: %s", ticket_id, exc)
            return JSONResponse({"error": str(exc)}, status_code=500)

    if branch_name is None:
        launching_set.discard(card_key)
        logger.error(
            "launch-background: cannot determine branch for card %s "
            "(ticket_id=%s, has_linear_key=%s)",
            card_key,
            ticket_id,
            bool(config.command_center.linear_api_key),
        )
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

                args = build_claude_args(
                    skill,
                    session_id,
                    title,
                    target,
                    claude_flags=config.command_center.claude_flags,
                )
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


# ---------------------------------------------------------------------------
# Session management actions
# ---------------------------------------------------------------------------


def _push_board_changed(app: FastAPI) -> None:
    """Push a board_changed event to all SSE queues."""
    for q in app.state.cc_queues:
        q.put_nowait("board_changed")


@router.post("/command-center/kill-session")
async def kill_session(request: Request) -> JSONResponse:
    """Kill a running tmux session and remove the session record."""
    try:
        body = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)
    payload = body.get("payload", body)
    session_id = payload.get("session_id")
    if not session_id:
        return JSONResponse({"error": "session_id is required"}, status_code=400)
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)

    if session is None or not isinstance(session, ClaudeCodeSession) or not session.tmux_session:
        return JSONResponse({"error": "session not found"}, status_code=404)

    subprocess.run(
        ["tmux", "kill-session", "-t", session.tmux_session],
        capture_output=True,
    )

    manager.cleanup_session(session_id)
    _push_board_changed(request.app)
    return JSONResponse({"status": "killed"})


@router.post("/command-center/cleanup-worktree")
async def cleanup_worktree(request: Request) -> JSONResponse:
    """Roll back a worktree and remove the session record."""
    try:
        body = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)
    payload = body.get("payload", body)
    session_id = payload.get("session_id")
    if not session_id:
        return JSONResponse({"error": "session_id is required"}, status_code=400)
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)

    if session is None or not isinstance(session, ClaudeCodeSession) or not session.worktree_path:
        return JSONResponse({"error": "session not found"}, status_code=404)

    live_tmux_sessions: set[str] = getattr(request.app.state, "live_tmux_sessions", set())
    if session.tmux_session and session.tmux_session in live_tmux_sessions:
        return JSONResponse(
            {"error": "Cannot clean up worktree while session is running"},
            status_code=409,
        )

    try:
        rollback_worktree(Path(session.worktree_path))
    except Exception as exc:  # noqa: BLE001
        logger.error("Worktree rollback failed for session %s: %s", session_id, exc)
        return JSONResponse({"error": str(exc)}, status_code=500)

    manager.cleanup_session(session_id)
    _push_board_changed(request.app)
    return JSONResponse({"status": "cleaned_up"})
