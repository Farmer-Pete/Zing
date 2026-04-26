"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import re
import subprocess
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

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
    build_session_name,
    checkout_pr_branch,
    create_session_on_server,
    create_worktree,
    derive_branch_name,
    exec_or_detach,
    find_repo_path,
    move_ticket_in_progress,
    rollback_worktree,
)
from zing_ai.server.attention import AttentionItem, build_attention_queue
from zing_ai.server.command_center import (
    aggregate,
    build_session_phases,
    generate_standup,
    infer_repo_for_ticket,
)
from zing_ai.server.models import ClaudeCodeSession, Session, ZingSession
from zing_ai.server.models_external import KanbanView
from zing_ai.server.sse_helpers import sse_btn_state as _sse_btn_state
from zing_ai.server.sse_helpers import sse_toast as _sse_toast
from zing_ai.server.templates import render, render_markdown

logger = logging.getLogger(__name__)
router = APIRouter()

# Guards against concurrent manual-refresh clicks (Decision #22).
_refresh_lock = asyncio.Lock()


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
    live_sessions: set[str],
) -> dict:
    """Build running_sessions, worktree_entries, running_count, orphan_count for the tray.

    Args:
        view: The current KanbanView (all columns).
        sessions: All sessions from the session manager.
        live_sessions: Set of currently alive zellij session names.

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

        # Track running sessions (alive in zellij).
        if session.terminal_session and session.terminal_session in live_sessions:
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
    live_sessions: set[str] = getattr(app.state, "live_sessions", set())
    sessions = app.state.session_manager.list_sessions()
    session_phases = {}
    for s in sessions:
        if hasattr(s, "steps"):
            session_phases[s.session_id] = build_session_phases(s)
    return render(
        "fragments/kanban_board.html",
        view=view,
        current_username=cache.github_username or "",
        live_sessions=live_sessions,
        session_phases=session_phases,
    )


def render_attention_bar_fragment(app: FastAPI, sessions: list[Session] | None = None) -> str:
    """Render the attention bar fragment. Used by SSE board_changed events."""
    resolved: list[Session] = (
        sessions if sessions is not None else app.state.session_manager.list_sessions()
    )
    attention_items = build_attention_queue(resolved, datetime.now(UTC))
    return render(
        "fragments/attention_bar.html",
        attention_items=attention_items,
    )


def _render_tray_fragment(app: FastAPI) -> str:
    """Render the management tray. Used by SSE board_changed events."""
    view = _build_view(app)
    live_sessions: set[str] = getattr(app.state, "live_sessions", set())
    sessions = app.state.session_manager.list_sessions()
    tray_data = _build_tray_data(view, sessions, live_sessions)
    return render("fragments/management_tray.html", **tray_data)


@router.get("/command-center", response_class=HTMLResponse)
async def get_command_center(request: Request) -> HTMLResponse:
    """Return the Command Center HTML page."""
    view = _build_view(request.app)
    cache = request.app.state.external_cache
    live_sessions: set[str] = getattr(request.app.state, "live_sessions", set())
    sessions = request.app.state.session_manager.list_sessions()
    tray_data = _build_tray_data(view, sessions, live_sessions)
    attention_items = build_attention_queue(sessions, datetime.now(UTC))
    session_phases = {}
    for s in sessions:
        if hasattr(s, "steps"):
            session_phases[s.session_id] = build_session_phases(s)
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
            live_sessions=live_sessions,
            attention_items=attention_items,
            session_phases=session_phases,
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
                    sessions = request.app.state.session_manager.list_sessions()
                    html = render_board_fragment(request.app)
                    yield SSE.patch_elements(
                        html,
                        selector="#kanban-board",
                        mode=ElementPatchMode.OUTER,
                    )
                    attn_html = render_attention_bar_fragment(request.app, sessions=sessions)
                    yield SSE.patch_elements(
                        attn_html,
                        selector="#attention-bar",
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
@datastar_response
async def refresh_command_center(request: Request):  # noqa: ANN201
    """Trigger an immediate poll of Linear/GitHub and refresh the board."""
    poller = getattr(request.app.state, "poller", None)

    async def _stream():  # noqa: ANN202
        if poller is None:
            yield _sse_toast("Poller not available", "err")
            return
        if _refresh_lock.locked():
            yield _sse_toast("Refresh already in progress", "info")
            return
        async with _refresh_lock:
            try:
                await poller._poll_once()  # noqa: SLF001
                yield _sse_toast("Refreshed", "ok")
            except Exception as e:  # noqa: BLE001
                yield _sse_toast(str(e), "err")

    return _stream()


@router.get("/command-center/standup")
async def get_standup(request: Request) -> JSONResponse:
    """Generate a standup message from the current board state."""
    view = _build_view(request.app)
    cache = request.app.state.external_cache
    username = cache.github_username or ""
    markdown = generate_standup(view, username)
    html = str(render_markdown(markdown))
    return JSONResponse({"markdown": markdown, "html": html})


# ---------------------------------------------------------------------------
# Zellij session attach (browser proxy)
# ---------------------------------------------------------------------------


@router.post("/command-center/attach-session")
async def attach_session(request: Request) -> JSONResponse:
    """Attach to a zellij session via the browser proxy."""
    if not getattr(request.app.state, "zellij_available", False):
        return JSONResponse({"error": "Zellij is not available"}, status_code=503)
    try:
        data = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)
    terminal_session = data.get("terminal_session")
    if not terminal_session:
        return JSONResponse({"error": "terminal_session is required"}, status_code=400)
    if not re.fullmatch(r"[a-zA-Z0-9_-]+", terminal_session):
        return JSONResponse({"error": "invalid session name"}, status_code=400)
    return JSONResponse({"url": f"/zellij/{terminal_session}"})


# ---------------------------------------------------------------------------
# Background session launch
# ---------------------------------------------------------------------------


@router.post("/command-center/launch-background")
@datastar_response
async def launch_background(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Launch a background Claude Code session for a Kanban card."""
    card_key = payload.get("card_key")
    repo_override = payload.get("repo")  # set when user picks from the chooser
    skill_override = payload.get("skill")  # e.g. "pr-respond" for respond buttons
    btn_id = payload.get("btn_id", f"btn-launch-{card_key}")

    # Snapshot original button HTML for sse_btn_state reset_html.
    original_button_html = render(
        "fragments/launch_button.html",
        ticket_id=card_key,
        btn_label="Launch",
        btn_skill=skill_override,
        btn_pr=payload.get("pr_number"),
    )

    async def _stream():  # noqa: ANN202
        if not card_key:
            yield _sse_toast("card_key is required", "err")
            return

        logger.debug("launch-background: card_key=%s", card_key)

        # In-flight dedup lock — initialise lazily if not set by create_app.
        launching_set: set[str] = getattr(request.app.state, "launching_set", None) or set()
        if not hasattr(request.app.state, "launching_set"):
            request.app.state.launching_set = launching_set

        if card_key in launching_set:
            logger.warning("launch-background: duplicate launch for card %s", card_key)
            yield _sse_toast("Launch already in progress for this card", "err")
            return

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
            yield _sse_toast("code_dir is not configured", "err")
            yield _sse_btn_state(
                btn_id,
                "Failed",
                kind="err",
                reset_html=original_button_html,
                reset_after_ms=2000,
            )
            return

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
                pr_number_override = payload.get("pr_number")
                if pr_number_override is not None:
                    try:
                        pr = next(
                            (p for p in card.prs if p.number == int(pr_number_override)),
                            card.prs[0],
                        )
                    except ValueError:
                        launching_set.discard(card_key)
                        yield _sse_toast(f"Invalid pr_number: {pr_number_override}", "err")
                        yield _sse_btn_state(
                            btn_id,
                            "Failed",
                            kind="err",
                            reset_html=original_button_html,
                            reset_after_ms=2000,
                        )
                        return
                else:
                    pr = card.prs[0]
                if not repo_override:
                    repo_name = pr.repo  # "owner/repo"
                branch_name = pr.head_ref
                pr_number = pr.number
                pr_repo = pr.repo
                is_pr_card = True
            if card.ticket is not None:
                ticket_id = card.ticket.identifier

        if repo_override:
            repo_name = repo_override

        # For ticket-only cards (no PRs), infer repo from same-team cards.
        if repo_name is None:
            team = card.ticket.team if (card is not None and card.ticket) else None
            candidates = infer_repo_for_ticket(kanban_view, team)
            if len(candidates) == 1:
                repo_name = candidates[0]
            elif candidates:
                launching_set.discard(card_key)
                candidates_dicts = [{"path": r, "label": r.split("/")[-1]} for r in candidates]
                yield SSE.patch_elements(
                    render(
                        "fragments/repo_chooser_modal.html",
                        card_key=card_key,
                        repos=candidates_dicts,
                    ),
                    selector="#repo-chooser-modal-container",
                    mode=ElementPatchMode.INNER,
                )
                yield SSE.patch_signals({"modals": {"repoChooser": True}})
                return
            else:
                launching_set.discard(card_key)
                logger.error(
                    "launch-background: cannot determine repo for card %s "
                    "(no PRs, no same-team cards)",
                    card_key,
                )
                error_msg = (
                    f"Cannot determine repository for card {card_key}. "
                    "Ticket has no linked PR and no same-team cards have PRs."
                )
                yield _sse_toast(error_msg, "err")
                yield _sse_btn_state(
                    btn_id,
                    "Failed",
                    kind="err",
                    reset_html=original_button_html,
                    reset_after_ms=2000,
                )
                return

        # Resolve local repo path.
        assert repo_name is not None  # guarded above
        repo_root = find_repo_path(code_dir, repo_name, repo_path_cache)
        if repo_root is None:
            launching_set.discard(card_key)
            logger.error("launch-background: repo %s not found under %s", repo_name, code_dir)
            yield _sse_toast(f"Repository {repo_name} not found under {code_dir}", "err")
            yield _sse_btn_state(
                btn_id,
                "Failed",
                kind="err",
                reset_html=original_button_html,
                reset_after_ms=2000,
            )
            return

        # Derive branch name for ticket-only cards.
        if branch_name is None and ticket_id is not None and config.command_center.linear_api_key:
            try:
                branch_name = derive_branch_name(ticket_id, config.command_center.linear_api_key)
            except LaunchError as exc:
                launching_set.discard(card_key)
                logger.error(
                    "launch-background: derive_branch_name failed for %s: %s", ticket_id, exc
                )
                yield _sse_toast(str(exc), "err")
                yield _sse_btn_state(
                    btn_id,
                    "Failed",
                    kind="err",
                    reset_html=original_button_html,
                    reset_after_ms=2000,
                )
                return

        if branch_name is None:
            launching_set.discard(card_key)
            logger.error(
                "launch-background: cannot determine branch for card %s "
                "(ticket_id=%s, has_linear_key=%s)",
                card_key,
                ticket_id,
                bool(config.command_center.linear_api_key),
            )
            yield _sse_toast(f"Cannot determine branch for card {card_key}", "err")
            yield _sse_btn_state(
                btn_id,
                "Failed",
                kind="err",
                reset_html=original_button_html,
                reset_after_ms=2000,
            )
            return

        title = card.ticket.title if (card is not None and card.ticket is not None) else card_key
        skill = skill_override or ("pr-audit" if is_pr_card else "new")
        target = card.prs[0].url if (is_pr_card and card is not None and card.prs) else ticket_id
        session_name = build_session_name(
            target=ticket_id or branch_name,
            pr_number=pr_number,
        )
        server_url = str(request.base_url).rstrip("/")

        logger.info("Launching background session for card %s (repo: %s)", card_key, repo_name)

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
                    terminal_session=session_name,
                )

                args = build_claude_args(
                    skill,
                    session_id,
                    title,
                    target,
                    claude_flags=config.command_center.claude_flags,
                )
                exec_or_detach(args, wt, terminal_session=session_name)

                return session_id, wt

            session_id, _wt = await asyncio.to_thread(_blocking)

            logger.info("Background session launched: %s (session: %s)", session_id, session_name)

            # Notify all SSE connections of board change.
            _push_board_changed(request.app)

            yield _sse_toast("Launched", "ok")
            yield _sse_btn_state(
                btn_id,
                "✓ Launched!",
                kind="ok",
                reset_html=original_button_html,
                reset_after_ms=2000,
            )

        except (LaunchError, subprocess.CalledProcessError) as exc:
            logger.error("Background launch failed for card %s: %s", card_key, exc)
            if worktree_path is not None:
                try:
                    rollback_worktree(worktree_path)
                except Exception as rollback_exc:  # noqa: BLE001
                    logger.warning("Rollback failed: %s", rollback_exc)
            yield _sse_toast(str(exc), "err")
            yield _sse_btn_state(
                btn_id,
                "Failed",
                kind="err",
                reset_html=original_button_html,
                reset_after_ms=2000,
            )
        finally:
            launching_set.discard(card_key)

    return _stream()


# ---------------------------------------------------------------------------
# Session management actions
# ---------------------------------------------------------------------------


def _push_board_changed(app: FastAPI) -> None:
    """Push a board_changed event to all SSE queues."""
    for q in app.state.cc_queues:
        q.put_nowait("board_changed")


@router.post("/command-center/start-ticket")
@datastar_response
async def start_ticket(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Move a Linear ticket to 'In Progress'."""
    ticket_id = payload.get("ticket_id")

    async def _stream():  # noqa: ANN202
        if not ticket_id:
            yield _sse_toast("ticket_id is required", "err")
            return

        config = load_config()
        api_key = config.command_center.linear_api_key
        if not api_key:
            yield _sse_toast("Linear API key not configured", "err")
            return

        try:
            await asyncio.to_thread(move_ticket_in_progress, ticket_id, api_key)
        except LaunchError as exc:
            logger.error("start-ticket failed for %s: %s", ticket_id, exc)
            yield _sse_toast(str(exc), "err")
            return

        # Trigger a poll in the background to pick up the state change.
        poller = getattr(request.app.state, "poller", None)
        if poller is not None:

            async def _bg_poll() -> None:
                with contextlib.suppress(Exception):
                    await poller._poll_once()  # noqa: SLF001
                _push_board_changed(request.app)

            asyncio.create_task(_bg_poll())
        else:
            _push_board_changed(request.app)

        yield _sse_toast("Ticket started", "ok")

    return _stream()


@router.post("/command-center/kill-session")
@datastar_response
async def kill_session(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Kill a running zellij session and remove the session record."""
    session_id = payload.get("session_id")

    async def _stream():  # noqa: ANN202
        if not session_id:
            yield _sse_toast("session_id is required", "err")
            return
        manager = request.app.state.session_manager
        session = manager.get_session(session_id)
        if (
            session is None
            or not isinstance(session, ClaudeCodeSession)
            or not session.terminal_session
        ):
            yield _sse_toast("Session not found", "err")
            return
        subprocess.run(
            ["zellij", "kill-session", session.terminal_session],
            capture_output=True,
        )
        manager.cleanup_session(session_id)
        _push_board_changed(request.app)
        yield _sse_toast("Session killed", "ok")

    return _stream()


@router.post("/command-center/cleanup-worktree")
@datastar_response
async def cleanup_worktree(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Roll back a worktree and remove the session record."""
    session_id = payload.get("session_id")

    async def _stream():  # noqa: ANN202
        if not session_id:
            yield _sse_toast("session_id is required", "err")
            return
        manager = request.app.state.session_manager
        session = manager.get_session(session_id)
        if (
            session is None
            or not isinstance(session, ClaudeCodeSession)
            or not session.worktree_path
        ):
            yield _sse_toast("Session not found", "err")
            return
        live_sessions: set[str] = getattr(request.app.state, "live_sessions", set())
        if session.terminal_session and session.terminal_session in live_sessions:
            yield _sse_toast("Cannot clean up worktree while session is running", "err")
            return
        try:
            rollback_worktree(Path(session.worktree_path))
        except Exception as exc:  # noqa: BLE001
            logger.error("Worktree rollback failed for session %s: %s", session_id, exc)
            yield _sse_toast(str(exc), "err")
            return
        manager.cleanup_session(session_id)
        _push_board_changed(request.app)
        yield _sse_toast("Worktree cleaned up", "ok")

    return _stream()


@router.post("/command-center/session-question")
async def session_question(request: Request) -> JSONResponse:
    """Receive a question from a Claude Code hook and add it as a notification."""
    try:
        body = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)
    session_id = body.get("session_id")
    question = body.get("question")
    if not session_id or not question:
        return JSONResponse({"status": "ignored"})
    manager = request.app.state.session_manager
    # Direct lookup first.
    session = manager.get_session(session_id)
    if session is not None and not isinstance(session, ClaudeCodeSession):
        session = None  # Only accept ClaudeCodeSession for hook questions
    if session is None:
        # Fallback: scan for a ClaudeCodeSession whose terminal_session matches.
        for s in manager.list_sessions():
            if isinstance(s, ClaudeCodeSession) and s.terminal_session == session_id:
                session = s
                session_id = s.session_id
                break
    if session is None:
        logger.debug("session_question ignored: session_id=%s not found", session_id)
        return JSONResponse({"status": "ignored"})
    manager.add_notification(session_id, title="Input needed", body=question)
    return JSONResponse({"status": "ok"})


# ---------------------------------------------------------------------------
# Review drawer
# ---------------------------------------------------------------------------


def _format_wait_label(seconds: int) -> str:
    """Return a compact wait-time string: '42s', '5m', '2h'."""
    if seconds < 60:
        return f"{seconds}s"
    if seconds < 3600:
        return f"{seconds // 60}m"
    return f"{seconds // 3600}h"


def build_drawer_context(
    session_id: str,
    manager: object,
    attention_queue: list[AttentionItem],
) -> dict:
    """Build template context for the review drawer.

    Args:
        session_id: The session to open in the drawer.
        manager: The session manager (app.state.session_manager).
        attention_queue: Current attention queue from build_attention_queue().

    Returns:
        A dict with keys: session, steps, current_step, mode, queue_position,
        queue_total, queue_items, next_session_id, prev_session_id,
        waiting_label, notification_body, notification.
    """
    session = manager.get_session(session_id)  # type: ignore[union-attr]
    if session is None:
        return {}

    # Find this session in the attention queue to determine position.
    queue_index = next(
        (i for i, item in enumerate(attention_queue) if item.session_id == session_id),
        None,
    )

    queue_total = len(attention_queue)
    queue_position = (queue_index + 1) if queue_index is not None else 1

    # Items after this one in the queue.
    queue_items = attention_queue[queue_index + 1 :] if queue_index is not None else []

    next_session_id: str | None = queue_items[0].session_id if queue_items else None
    prev_session_id: str | None = (
        attention_queue[queue_index - 1].session_id
        if (queue_index is not None and queue_index > 0)
        else None
    )

    # Determine mode.
    current_attention = attention_queue[queue_index] if queue_index is not None else None
    mode = current_attention.action_type if current_attention else "findings"

    # Normalise: "questions" maps to "findings" for template mode.
    if mode == "questions":
        mode = "findings"

    # Wait label.
    waiting_label = ""
    if current_attention:
        waiting_label = _format_wait_label(current_attention.wait_seconds)

    # Steps and current step for ZingSession.
    steps: list = []
    current_step = None
    notification = None
    notification_body = ""

    phase_segments: list[dict] = []
    if isinstance(session, ZingSession):
        steps = list(session.steps)
        # Current step = last READY step.
        current_step = next((s for s in reversed(steps) if s.state.value == "ready"), None)
        phase_segments = build_session_phases(session)
    elif isinstance(session, ClaudeCodeSession):
        notification = session.pending_question
        notification_body = notification.body if notification else ""

    return {
        "session": session,
        "steps": steps,
        "current_step": current_step,
        "phase_segments": phase_segments,
        "mode": mode,
        "queue_position": queue_position,
        "queue_total": max(queue_total, 1),
        "queue_items": queue_items,
        "next_session_id": next_session_id,
        "prev_session_id": prev_session_id,
        "waiting_label": waiting_label,
        "notification": notification,
        "notification_body": notification_body,
    }


@router.get("/command-center/drawer/{session_id}", response_class=HTMLResponse)
async def get_drawer(session_id: str, request: Request) -> HTMLResponse:
    """Return the drawer HTML fragment for a session.

    The fragment includes backdrop + panel.  The JS injects it into
    #review-drawer-container and sets display:block.
    """
    manager = request.app.state.session_manager
    sessions = manager.list_sessions()
    attention_queue = build_attention_queue(sessions, datetime.now(UTC))

    ctx = build_drawer_context(session_id, manager, attention_queue)
    if not ctx:
        logger.warning("Drawer requested for unknown session: %s", session_id)
        return HTMLResponse("<div>Session not found</div>", status_code=404)

    session = ctx["session"]

    if isinstance(session, ClaudeCodeSession):
        # Attach mode — use the simpler attach template.
        html = render("fragments/drawer_attach.html", **ctx)
    else:
        # Findings/questions mode.
        html = render("fragments/review_drawer.html", **ctx)

    return HTMLResponse(html)


@router.get(
    "/command-center/drawer/{session_id}/step/{step_id}",
    response_class=HTMLResponse,
)
async def get_drawer_step(
    session_id: str,
    step_id: str,
    request: Request,
) -> HTMLResponse:
    """Return a single step-history fragment for the drawer.

    The JS can inject individual step sections when expanding collapsed history.
    """
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None or not isinstance(session, ZingSession):
        logger.warning("Drawer step not found: session=%s step=%s", session_id, step_id)
        return HTMLResponse("<div>Session not found</div>", status_code=404)

    step = next((s for s in session.steps if s.step_id == step_id), None)
    if step is None:
        logger.warning("Drawer step not found: session=%s step=%s", session_id, step_id)
        return HTMLResponse("<div>Step not found</div>", status_code=404)

    html = render("fragments/drawer_step_history.html", step=step, session=session)
    return HTMLResponse(html)
