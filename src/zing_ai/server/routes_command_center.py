"""FastAPI route handlers for the Command Center dashboard."""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import re
import subprocess
import uuid
from collections.abc import AsyncIterator, Callable
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
    rollback_worktree,  # used by /cleanup-worktree (explicit user action)
)
from zing_ai.server.attention import build_attention_queue
from zing_ai.server.command_center import (
    aggregate,
    build_session_phases,
    generate_standup,
    infer_repo_for_ticket,
)
from zing_ai.server.flow import (
    _body_fragment_for,
    build_flow_context,
    resolve_active_item,
)
from zing_ai.server.models import (
    LAUNCH_GRACE_SECONDS,
    ClaudeCodeSession,
    QuestionData,
    QuestionOption,
)
from zing_ai.server.models_external import KanbanView
from zing_ai.server.signals import to_signal_key as _to_signal_key
from zing_ai.server.sse_helpers import sse_toast as _sse_toast
from zing_ai.server.templates import render, render_markdown

logger = logging.getLogger(__name__)
router = APIRouter()

# Guards against concurrent manual-refresh clicks (Decision #22).
_refresh_lock = asyncio.Lock()
# Guards against concurrent launch-background invocations for the same card.
_launch_lock = asyncio.Lock()


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


def _render_tray_fragment(app: FastAPI) -> str:
    """Render the management tray. Used by SSE board_changed events."""
    view = _build_view(app)
    live_sessions: set[str] = getattr(app.state, "live_sessions", set())
    sessions = app.state.session_manager.list_sessions()
    tray_data = _build_tray_data(view, sessions, live_sessions)
    return render("fragments/management_tray.html", **tray_data)


def _build_initial_signals(
    view: KanbanView,
    sessions: list,
    last_polled_label: str,
    last_error: str,
) -> dict[str, Any]:
    """Build the initial signal envelope rendered onto ``.cc-page``.

    Centralising the defaults here makes them diffable / commentable in Python
    rather than hidden inside a ~250-character minified JSON blob in the
    template. Per-card ``$busyButtons`` keys are pre-computed from the kanban
    view so every key the page may reference exists with the safe value
    ``False`` — Datastar v1 otherwise treats undefined indicator signals as
    truthy on first read, which would gate the launch / kill / attach /
    cleanup / resume / start buttons closed at page load.
    """
    busy_buttons: dict[str, bool] = {
        # Toolbar buttons.
        "refresh": False,
        "standup": False,
    }
    # Pre-init every per-card / per-session indicator key the page may dereference.
    for column in (view.todo, view.in_progress, view.needs_review, view.done):
        for card in column:
            sig = card.signal_key
            busy_buttons[f"launch_{sig}"] = False
            if card.ticket is not None:
                busy_buttons[f"start_{_to_signal_key(card.ticket.identifier)}"] = False
    for s in sessions:
        if isinstance(s, ClaudeCodeSession):
            sig = s.signal_key
            busy_buttons[f"attach_{sig}"] = False
            busy_buttons[f"kill_{sig}"] = False
            busy_buttons[f"resume_{sig}"] = False
            busy_buttons[f"cleanup_{sig}"] = False

    return {
        # Polling status (also patched via SSE).
        "lastPolledLabel": last_polled_label or "",
        "lastError": last_error or "",
        # Per-button busy/disabled flags (see helper above).
        "busyButtons": busy_buttons,
        # Modal open/closed flags. Five modals share this dict so a single
        # data-on-signal-patch-filter on .cc-page can react to any change.
        "modals": {
            "mgmt": False,
            "standup": False,
            "terminal": False,
            "repoChooser": False,
            "launchPopup": False,
        },
        # Currently-open kebab key (empty string = none open). Empty string
        # rather than null because Datastar deletes null'd keys from the proxy
        # silently, which breaks close-on-outside-click watchers.
        "openKebab": "",
        # Kebab search input (clears when a menu opens).
        "kebabQuery": "",
        # Standup modal state.
        "standupTab": "rendered",
        "standupMarkdown": "",
        # Terminal modal — URL signal patched by /flow/launch-popup-open.
        "terminalUrl": "",
        # Launch popup — session name, URL, and title patched by /flow/launch-popup-open.
        "launchPopupSession": "",
        "launchPopupUrl": "",
        "launchPopupTitle": "",
    }


@router.get("/command-center", response_class=HTMLResponse)
async def get_command_center(request: Request) -> HTMLResponse:
    """Return the Command Center HTML page."""
    view = _build_view(request.app)
    cache = request.app.state.external_cache
    live_sessions: set[str] = getattr(request.app.state, "live_sessions", set())
    sessions = request.app.state.session_manager.list_sessions()
    tray_data = _build_tray_data(view, sessions, live_sessions)
    attention_items = build_attention_queue(sessions, datetime.now(UTC))
    queue_count = len(attention_items)
    session_phases = {}
    for s in sessions:
        if hasattr(s, "steps"):
            session_phases[s.session_id] = build_session_phases(s)
    initial_signals = _build_initial_signals(
        view,
        sessions,
        last_polled_label=_format_last_polled(cache.last_polled_at),
        last_error=cache.last_error or "",
    )
    return HTMLResponse(
        render(
            "command_center.html",
            view=view,
            current_path="/command-center",
            current_view="board",
            queue_count=queue_count,
            last_polled_at=cache.last_polled_at,
            last_polled_label=_format_last_polled(cache.last_polled_at),
            last_error=cache.last_error,
            body_class="command-center",
            current_username=cache.github_username or "",
            live_sessions=live_sessions,
            attention_items=attention_items,
            session_phases=session_phases,
            initial_signals=initial_signals,
            **tray_data,
        )
    )


@router.get("/command-center/flow", response_class=HTMLResponse)
async def get_flow(  # noqa: ANN201
    request: Request,
    session_id: str | None = None,
    step_id: str | None = None,
):
    """Return the Flow Mode HTML page.

    Query params ``session_id`` and ``step_id`` select the active item
    directly — the URL is the only cursor.
    """
    manager = request.app.state.session_manager
    sessions = manager.list_sessions()
    queue = build_attention_queue(sessions, datetime.now(UTC))
    active = resolve_active_item(queue, session_id, step_id)
    ctx = build_flow_context(manager, queue, active)
    return HTMLResponse(
        render("flow.html", current_path="/command-center/flow", current_view="flow", **ctx)
    )


async def _cc_events_stream(
    request: Request,
    on_board_changed: Callable[[Request], AsyncIterator[Any]],
) -> AsyncIterator[Any]:
    """Shared connection lifecycle for Command Center SSE handlers.

    Owns: cc_queues registration, 30s heartbeat, event dispatch via callback,
    contextlib.suppress(ValueError) cleanup in finally.

    The queue is registered synchronously (before the generator body runs) so
    the response headers flush before the first iteration, avoiding a race where
    cc_queues is empty even though the SSE stream appears open.
    """
    q: asyncio.Queue[str] = asyncio.Queue(maxsize=100)
    request.app.state.cc_queues.append(q)
    try:
        while True:
            try:
                event = await asyncio.wait_for(q.get(), timeout=30.0)
            except TimeoutError:
                yield SSE.patch_signals({"_heartbeat": True})
                continue
            kind, _, _target = event.partition(":")
            if kind == "board_changed":
                async for ev in on_board_changed(request):
                    yield ev
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
            request.app.state.cc_queues.remove(q)


@router.get("/command-center/events")
@datastar_response
async def command_center_events(request: Request):  # noqa: ANN201
    """SSE endpoint that pushes Command Center updates to the browser."""

    async def _on_board_changed(req: Request):  # noqa: ANN202
        sessions = req.app.state.session_manager.list_sessions()
        queue_count = len(build_attention_queue(sessions, datetime.now(UTC)))
        html = render_board_fragment(req.app)
        yield SSE.patch_elements(
            html,
            selector="#kanban-board",
            mode=ElementPatchMode.OUTER,
        )
        tray_html = _render_tray_fragment(req.app)
        yield SSE.patch_elements(
            tray_html,
            selector="#mgmt-tray",
            mode=ElementPatchMode.INNER,
        )
        # Update the Flow toggle badge (visible on the Board page too).
        yield SSE.patch_elements(
            f'<span class="toggle-badge" id="flow-toggle-badge">{queue_count}</span>',
            selector="#flow-toggle-badge",
            mode=ElementPatchMode.OUTER,
        )

    async def _stream():  # noqa: ANN202
        async for ev in _cc_events_stream(request, _on_board_changed):
            yield ev

    return _stream()


@router.get("/command-center/flow/events")
@datastar_response
async def flow_events(request: Request):  # noqa: ANN201
    """SSE endpoint that pushes Flow Mode updates to the browser."""

    async def _on_board_changed(req: Request):  # noqa: ANN202
        manager = req.app.state.session_manager
        sessions = manager.list_sessions()
        queue = build_attention_queue(sessions, datetime.now(UTC))
        # No cursor in SSE context — Step 10 will add body-patch removal.
        active = resolve_active_item(queue, session_id=None, step_id=None)
        ctx = build_flow_context(manager, queue, active)
        ctx["current_view"] = "flow"
        yield SSE.patch_elements(
            render("fragments/flow_progress_strip.html", **ctx),
            selector="#flow-strip",
            mode=ElementPatchMode.OUTER,
        )
        yield SSE.patch_elements(
            render(_body_fragment_for(active), **ctx),
            selector="#flow-body",
            mode=ElementPatchMode.INNER,
        )
        # Update the Flow toggle badge count.
        yield SSE.patch_elements(
            f'<span class="toggle-badge" id="flow-toggle-badge">{len(queue)}</span>',
            selector="#flow-toggle-badge",
            mode=ElementPatchMode.OUTER,
        )

    async def _stream():  # noqa: ANN202
        async for ev in _cc_events_stream(request, _on_board_changed):
            yield ev

    return _stream()


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
            except Exception:  # noqa: BLE001
                logger.exception("cc-refresh failed", extra={"event": "cc_refresh_failed"})
                yield _sse_toast("Refresh failed — see server logs", "err")

    return _stream()


@router.post("/command-center/standup")
@datastar_response
async def get_standup(request: Request):  # noqa: ANN201
    """Generate a standup message from the current board state.

    POST + SSE keeps verb semantics consistent with the rest of the file
    (refresh, attach, kill, cleanup, drawer all use POST + SSE) and avoids
    the cacheability concern of SSE-on-GET.
    """

    async def _stream():  # noqa: ANN202
        view = _build_view(request.app)
        cache = request.app.state.external_cache
        username = cache.github_username or ""
        markdown = generate_standup(view, username)
        html = str(render_markdown(markdown))
        yield SSE.patch_elements(
            html,
            selector="#standup-modal-body",
            mode=ElementPatchMode.INNER,
        )
        # standupHtml signal intentionally NOT patched — the rendered HTML
        # already lives in #standup-modal-body and the Copy button reads
        # it from the DOM (see dispatchCopyStandup in cc-modals.js).
        yield SSE.patch_signals(
            {
                "modals": {"standup": True},
                "standupMarkdown": markdown,
            }
        )

    return _stream()


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
    # Reset $busyButtons.launch_<sig> on every exit so the button returns to its
    # interactive state. The card_key may legitimately be missing here (early
    # error path); callers handle that case explicitly with a fallback.
    sig_key = _to_signal_key(card_key) if card_key else ""
    reset_busy = (
        SSE.patch_signals({"busyButtons": {f"launch_{sig_key}": False}}) if sig_key else None
    )

    async def _stream():  # noqa: ANN202
        if not card_key:
            logger.warning(
                "launch-background: missing card_key",
                extra={"event": "cc_launch_invalid"},
            )
            yield _sse_toast("card_key is required", "err")
            return

        logger.debug("launch-background: card_key=%s", card_key)

        launching_set: set[str] = request.app.state.launching_set

        # Atomic check-and-add — without the lock, two concurrent SSE connections
        # (second tab, rapid double-click) can both pass the membership check
        # before either has called add(), creating two worktrees for the same card.
        async with _launch_lock:
            if card_key in launching_set:
                logger.warning(
                    "launch-background: duplicate launch for card %s",
                    card_key,
                    extra={"event": "cc_launch_duplicate", "card_key": card_key},
                )
                yield _sse_toast("Launch already in progress for this card", "err")
                if reset_busy is not None:
                    yield reset_busy
                return
            launching_set.add(card_key)

        repo_path_cache: dict[str, Path] = request.app.state.repo_path_cache

        config = load_config()
        code_dir = config.git.code_dir
        if not code_dir:
            launching_set.discard(card_key)
            logger.error("launch-background: code_dir is not configured")
            yield _sse_toast("code_dir is not configured", "err")
            if reset_busy is not None:
                yield reset_busy
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
                        if reset_busy is not None:
                            yield reset_busy
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
                # Symmetric with every other early-return branch — reset busy so
                # the launch indicator clears and the chooser button picks up the
                # in-flight state via its own data-indicator binding.
                if reset_busy is not None:
                    yield reset_busy
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
                if reset_busy is not None:
                    yield reset_busy
                return

        # Resolve local repo path.
        assert repo_name is not None  # guarded above
        repo_root = find_repo_path(code_dir, repo_name, repo_path_cache)
        if repo_root is None:
            launching_set.discard(card_key)
            logger.error("launch-background: repo %s not found under %s", repo_name, code_dir)
            yield _sse_toast(f"Repository {repo_name} not found under {code_dir}", "err")
            if reset_busy is not None:
                yield reset_busy
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
                if reset_busy is not None:
                    yield reset_busy
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
            if reset_busy is not None:
                yield reset_busy
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

        # Reconcile against any existing Zellij session with the same name before
        # we hand off to exec_or_detach (which would fail with "Session already
        # exists"). Two cases:
        #   - Live + tracked by an in-app session → redirect user to attach.
        #   - Live + orphaned (no record claims it) → kill the stale Zellij
        #     session and proceed.
        live_sessions: set[str] = getattr(request.app.state, "live_sessions", set())
        if session_name in live_sessions:
            session_manager = request.app.state.session_manager
            tracked = next(
                (
                    s
                    for s in session_manager.list_sessions()
                    if isinstance(s, ClaudeCodeSession) and s.terminal_session == session_name
                ),
                None,
            )
            if tracked is not None:
                logger.info(
                    "launch-background: %s already running for session %s — attaching",
                    session_name,
                    tracked.session_id,
                )
                launching_set.discard(card_key)
                yield _sse_toast(f"Session {session_name} already running — attaching", "info")
                yield SSE.patch_signals(
                    {
                        "terminalUrl": f"/zellij/{session_name}",
                        "modals": {"terminal": True},
                    }
                )
                if reset_busy is not None:
                    yield reset_busy
                return
            logger.warning(
                "launch-background: pruning orphaned Zellij session %s "
                "(no in-app record claims it)",
                session_name,
            )
            # `kill-session` only signals the session — the name lingers in
            # `list-sessions` as EXITED until the record is removed, which
            # would still trip exec_or_detach's collision check. `delete-session
            # --force` kills (if alive) and removes the record in one step.
            await asyncio.to_thread(
                subprocess.run,
                ["zellij", "delete-session", "--force", session_name],
                capture_output=True,
                check=False,
            )
            # Drop from cache so the 0.5s poll lag doesn't re-trip the check.
            live_sessions.discard(session_name)

        try:

            def _blocking() -> tuple[str, Path]:
                # Create worktree (or reuse if it already exists — see create_worktree
                # / checkout_pr_branch). On downstream failure we deliberately leave
                # the worktree on disk so the next launch attempt can reuse it.
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
            request.app.state.notify_cc("board_changed")

            # Schedule a follow-up board_changed when the launch grace window
            # ends. The 0.5s liveness poll already pushes board_changed when
            # zellij comes up, so the happy path flips STARTING→STARTED quickly.
            # This timer covers the unhappy path (zellij never starts): without
            # it the strip would sit on "Starting…" until the next external
            # poll (~60s) finally re-rendered with the expired grace.
            async def _grace_expiry_push() -> None:
                await asyncio.sleep(LAUNCH_GRACE_SECONDS + 1)
                request.app.state.notify_cc("board_changed")

            asyncio.create_task(_grace_expiry_push())

            yield _sse_toast("Launched", "ok")
            if reset_busy is not None:
                yield reset_busy

        except (LaunchError, subprocess.CalledProcessError) as exc:
            logger.error("Background launch failed for card %s: %s", card_key, exc)
            yield _sse_toast(str(exc), "err")
            if reset_busy is not None:
                yield reset_busy
        finally:
            launching_set.discard(card_key)

    return _stream()


# ---------------------------------------------------------------------------
# Session management actions
# ---------------------------------------------------------------------------


@router.post("/command-center/start-ticket")
@datastar_response
async def start_ticket(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Move a Linear ticket to 'In Progress'."""
    ticket_id = payload.get("ticket_id")

    async def _stream():  # noqa: ANN202
        if not ticket_id:
            logger.warning(
                "start-ticket: missing ticket_id",
                extra={"event": "cc_start_invalid"},
            )
            yield _sse_toast("ticket_id is required", "err")
            return

        config = load_config()
        api_key = config.command_center.linear_api_key
        if not api_key:
            logger.warning(
                "start-ticket: linear api key missing",
                extra={"event": "cc_start_unconfigured", "ticket_id": ticket_id},
            )
            yield _sse_toast("Linear API key not configured", "err")
            return

        try:
            await asyncio.to_thread(move_ticket_in_progress, ticket_id, api_key)
        except LaunchError as exc:
            logger.error(
                "start-ticket failed for %s: %s",
                ticket_id,
                exc,
                extra={"event": "cc_start_failed", "ticket_id": ticket_id},
            )
            yield _sse_toast(str(exc), "err")
            return

        # Trigger a poll in the background to pick up the state change.
        poller = getattr(request.app.state, "poller", None)
        if poller is not None:

            async def _bg_poll() -> None:
                with contextlib.suppress(Exception):
                    await poller._poll_once()  # noqa: SLF001
                request.app.state.notify_cc("board_changed")

            asyncio.create_task(_bg_poll())
        else:
            request.app.state.notify_cc("board_changed")

        yield _sse_toast("Ticket started", "ok")

    return _stream()


@router.post("/command-center/kill-session")
@datastar_response
async def kill_session(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Kill a running zellij session and remove the session record."""
    session_id = payload.get("session_id")

    async def _stream():  # noqa: ANN202
        if not session_id:
            logger.warning(
                "kill-session: missing session_id",
                extra={"event": "cc_kill_invalid"},
            )
            yield _sse_toast("session_id is required", "err")
            return
        manager = request.app.state.session_manager
        session = manager.get_session(session_id)
        if (
            session is None
            or not isinstance(session, ClaudeCodeSession)
            or not session.terminal_session
        ):
            logger.warning(
                "kill-session: session not found %s",
                session_id,
                extra={"event": "cc_kill_not_found", "session_id": session_id},
            )
            yield _sse_toast("Session not found", "err")
            return
        # zellij may legitimately be absent (CI runners, dev machines without
        # the binary). Surface a soft warning but still clean up the session
        # record so the UI reflects the user's intent.
        try:
            subprocess.run(
                ["zellij", "kill-session", session.terminal_session],
                capture_output=True,
                check=False,
            )
        except FileNotFoundError:
            logger.warning(
                "kill-session: zellij binary not found; cleaning up session record only",
                extra={"event": "cc_kill_no_zellij", "session_id": session_id},
            )
        manager.cleanup_session(session_id)
        request.app.state.notify_cc("board_changed")
        yield _sse_toast("Session killed", "ok")

    return _stream()


@router.post("/command-center/cleanup-worktree")
@datastar_response
async def cleanup_worktree(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Roll back a worktree and remove the session record."""
    session_id = payload.get("session_id")

    async def _stream():  # noqa: ANN202
        if not session_id:
            logger.warning(
                "cleanup-worktree: missing session_id",
                extra={"event": "cc_cleanup_invalid"},
            )
            yield _sse_toast("session_id is required", "err")
            return
        manager = request.app.state.session_manager
        session = manager.get_session(session_id)
        if (
            session is None
            or not isinstance(session, ClaudeCodeSession)
            or not session.worktree_path
        ):
            logger.warning(
                "cleanup-worktree: session not found %s",
                session_id,
                extra={"event": "cc_cleanup_not_found", "session_id": session_id},
            )
            yield _sse_toast("Session not found", "err")
            return
        live_sessions: set[str] = getattr(request.app.state, "live_sessions", set())
        if session.terminal_session and session.terminal_session in live_sessions:
            logger.warning(
                "cleanup-worktree: session %s still running",
                session_id,
                extra={"event": "cc_cleanup_running", "session_id": session_id},
            )
            yield _sse_toast("Cannot clean up worktree while session is running", "err")
            return
        try:
            rollback_worktree(Path(session.worktree_path))
        except (LaunchError, OSError, subprocess.CalledProcessError) as exc:
            logger.error(
                "Worktree rollback failed for session %s: %s",
                session_id,
                exc,
                extra={"event": "cc_cleanup_failed", "session_id": session_id},
            )
            yield _sse_toast(str(exc), "err")
            return
        manager.cleanup_session(session_id)
        request.app.state.notify_cc("board_changed")
        yield _sse_toast("Worktree cleaned up", "ok")

    return _stream()


@router.post("/command-center/session-question")
async def session_question(request: Request) -> JSONResponse:
    """Receive a question from a Claude Code hook and add it as a notification.

    The ``question`` field may be either a plain string (legacy form) or a
    structured object ``{question, header, multiSelect, options: [{label,
    description}]}`` from the AskUserQuestion tool. Structured payloads are
    stored as ``QuestionData`` so the drawer can render labelled choices
    instead of dumping the raw JSON.
    """
    try:
        body = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)
    session_id = body.get("session_id")
    raw_question = body.get("question")
    if not session_id or not raw_question:
        return JSONResponse({"status": "ignored"})

    question_text, question_data = _parse_question_payload(raw_question)
    if not question_text:
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
    manager.add_notification(
        session_id, title="Input needed", body=question_text, question=question_data
    )
    return JSONResponse({"status": "ok"})


@router.post("/command-center/session-idle")
async def session_idle(request: Request) -> JSONResponse:
    """Receive an idle / attention-needed event from a Claude Code hook.

    Triggered by Claude Code's ``Notification`` hook when the prompt has been
    idle for ~60s waiting for input or a permission decision is pending. The
    payload mirrors ``session-question`` minus the structured question — just
    a ``title`` and ``body`` string. We append it as a plain notification so
    the existing dot / drawer-list / browser-toast pipeline lights up without
    auto-opening the drawer (no ``question`` payload).
    """
    try:
        body_payload = await request.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "Invalid JSON"}, status_code=400)
    session_id = body_payload.get("session_id")
    title = body_payload.get("title") or "Claude is waiting"
    body = body_payload.get("body") or ""
    if not session_id:
        return JSONResponse({"status": "ignored"})

    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is not None and not isinstance(session, ClaudeCodeSession):
        session = None
    if session is None:
        for s in manager.list_sessions():
            if isinstance(s, ClaudeCodeSession) and s.terminal_session == session_id:
                session = s
                session_id = s.session_id
                break
    if session is None:
        logger.debug("session_idle ignored: session_id=%s not found", session_id)
        return JSONResponse({"status": "ignored"})
    manager.add_notification(session_id, title=title, body=body)
    return JSONResponse({"status": "ok"})


def _parse_question_payload(raw: object) -> tuple[str, QuestionData | None]:
    """Normalise the hook payload into (display_text, structured_data)."""
    if isinstance(raw, str):
        return raw, None
    if not isinstance(raw, dict):
        return "", None
    text = raw.get("question")
    if not isinstance(text, str) or not text:
        return "", None
    raw_options = raw.get("options") or []
    options: list[QuestionOption] = []
    if isinstance(raw_options, list):
        for opt in raw_options:
            if not isinstance(opt, dict):
                continue
            label = opt.get("label")
            if not isinstance(label, str) or not label:
                continue
            description = opt.get("description")
            options.append(
                QuestionOption(
                    label=label,
                    description=description if isinstance(description, str) else "",
                )
            )
    header = raw.get("header")
    return text, QuestionData(
        question=text,
        header=header if isinstance(header, str) else "",
        multi_select=bool(raw.get("multiSelect")),
        options=options,
    )


@router.post("/command-center/flow/pin")
@datastar_response
async def post_flow_pin(payload: dict[str, Any], request: Request):  # noqa: ANN201
    async def _stream():  # noqa: ANN202
        manager = request.app.state.session_manager
        session_id = str(payload.get("session_id") or "").strip()
        if not session_id:
            yield _sse_toast("Missing session_id", "err")
            return
        session = manager.get_session(session_id)
        if not isinstance(session, ClaudeCodeSession):
            yield _sse_toast("Pin only available for terminal sessions", "err")
            return
        new_pinned = not session.pinned
        try:
            manager.set_pinned(session_id, new_pinned)
        except (KeyError, ValueError) as exc:
            yield _sse_toast(str(exc), "err")
            return
        # Re-fetch queue to reflect new pin state
        queue = build_attention_queue(manager.list_sessions(), datetime.now(UTC))
        active = resolve_active_item(queue, session_id=session_id, step_id=None)
        ctx = build_flow_context(manager, queue, active)
        yield SSE.patch_elements(
            render("fragments/flow_progress_strip.html", **ctx),
            selector="#flow-strip",
            mode=ElementPatchMode.OUTER,
        )
        yield _sse_toast("Pinned" if new_pinned else "Unpinned", "ok")

    return _stream()


@router.post("/command-center/flow/launch-popup-open")
@datastar_response
async def post_flow_launch_popup_open(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Open the launch popup for a terminal session."""

    async def _stream():  # noqa: ANN202
        if not getattr(request.app.state, "zellij_available", False):
            yield _sse_toast("Zellij is not available", "err")
            return
        terminal_session = str(payload.get("terminal_session") or "").strip()
        if not terminal_session:
            yield _sse_toast("terminal_session is required", "err")
            return
        if not re.fullmatch(r"[a-zA-Z0-9_-]+", terminal_session):
            yield _sse_toast("invalid session name", "err")
            return
        url = f"/zellij/{terminal_session}"
        # Derive a human-readable title from the matching ClaudeCodeSession, if found.
        manager = request.app.state.session_manager
        title = terminal_session
        for s in manager.list_sessions():
            if isinstance(s, ClaudeCodeSession) and s.terminal_session == terminal_session:
                title = s.title or terminal_session
                break
        yield SSE.patch_signals(
            {
                "launchPopupSession": terminal_session,
                "launchPopupUrl": url,
                "launchPopupTitle": title,
                "modals": {"launchPopup": True},
            }
        )

    return _stream()


@router.post("/command-center/flow/launch-popup-send")
@datastar_response
async def post_flow_launch_popup_send(payload: dict[str, Any], request: Request):  # noqa: ANN201
    """Pin the session and redirect to Flow mode."""

    async def _stream():  # noqa: ANN202
        manager = request.app.state.session_manager
        terminal_session = str(payload.get("terminal_session") or "").strip()
        if not terminal_session:
            yield _sse_toast("Missing terminal session", "err")
            return
        sessions = manager.list_sessions()
        match: ClaudeCodeSession | None = None
        for session in sessions:
            if isinstance(session, ClaudeCodeSession) and (
                session.terminal_session == terminal_session
            ):
                match = session
                break
        if match is None:
            yield _sse_toast("Session not found", "err")
            return
        manager.set_pinned(match.session_id, True)
        yield SSE.patch_signals({"modals": {"launchPopup": False}})
        sid = match.session_id
        yield SSE.execute_script(
            f"window.location = '/command-center/flow?session_id={sid}&step_id={sid}'"
        )

    return _stream()
