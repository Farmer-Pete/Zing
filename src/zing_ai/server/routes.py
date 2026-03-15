"""FastAPI route handlers for the Zing batch review server."""

from __future__ import annotations

import asyncio
import html
import json
import logging
import pathlib
from collections import defaultdict
from typing import Any

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, HTTPException, Request
from fastapi.responses import HTMLResponse, JSONResponse

from zing_ai.server.models import (
    Complexity,
    Finding,
    Notification,
    ResponseAction,
    UserResponse,
)
from zing_ai.server.templates import render, render_markdown

logger = logging.getLogger("zing_ai.server")

router = APIRouter()

# Per-session list of asyncio queues for active SSE connections.
# Each SSE connection registers its own queue to receive push notifications.
_sse_queues: dict[str, list[asyncio.Queue[str]]] = defaultdict(list)

# Queues for dashboard SSE connections — notified when any session changes state.
_dashboard_queues: list[asyncio.Queue[str]] = []


def _session_not_found(session_id: str) -> JSONResponse:
    """Return a 404 response for an unknown session."""
    return JSONResponse(
        status_code=404,
        content={
            "error": "session_not_found",
            "message": f"Session '{session_id}' does not exist",
        },
    )


def _notify_sse_connections(session_id: str, event: str) -> None:
    """Push an event string to all active SSE queues for a session.

    Args:
        session_id: The session whose SSE connections to notify.
        event: An event type string (e.g. "finding", "ready", "completed").
    """
    for queue in _sse_queues.get(session_id, []):
        queue.put_nowait(event)


def _notify_dashboard_connections(event: str, session_id: str | None = None) -> None:
    """Push an event to all active dashboard SSE queues.

    Args:
        event: An event type string (e.g. "created", "completed", "cleaned_up").
        session_id: Optional session identifier. When provided, the event is
            pushed as ``"{event}:{session_id}"`` so consumers can determine
            which session the event belongs to.
    """
    message = f"{event}:{session_id}" if session_id is not None else event
    for queue in _dashboard_queues:
        queue.put_nowait(message)


def _build_notification_script(notif: Notification, default_on_click_js: str) -> str:
    """Build a browser Notification JS snippet from a Notification model.

    Args:
        notif: The notification to render as a browser popup.
        default_on_click_js: JS expression for onclick when ``notif.url`` is not set.
    """
    title_js = json.dumps(notif.title)
    opts: dict[str, str] = {}
    if notif.body:
        opts["body"] = notif.body
    opts_js = json.dumps(opts)
    if notif.url:
        url_js = json.dumps(notif.url)
        on_click_js = f"window.location.href = {url_js}; n.close();"
    else:
        on_click_js = default_on_click_js
    return (
        f"if (Notification.permission === 'granted') {{"
        f"  const n = new Notification({title_js}, {opts_js});"
        f"  n.onclick = () => {{ {on_click_js} }};"
        f"}}"
    )


def finding_fragment(
    finding: Finding,
    session_id: str,
    saved_responses: dict[str, str] | None = None,
) -> str:
    """Render a single finding as an HTML fragment.

    Args:
        finding: The finding model to render.
        session_id: The session ID for auto-save POST URLs.
        saved_responses: Optional dict of saved response values for signal initialization.

    Returns:
        Rendered HTML string for the finding.
    """
    kwargs: dict[str, object] = {"finding": finding, "session_id": session_id}
    if saved_responses is not None:
        kwargs["saved_responses"] = saved_responses
    return render("fragments/finding.html", **kwargs)


@router.post("/{session_id}/save-response")
async def post_save_response(session_id: str, request: Request) -> JSONResponse:
    """Auto-save finding responses on blur/change events.

    Accepts the Datastar signal store containing ``step_id`` and a
    ``responses`` dict mapping finding IDs to response values.
    """
    manager = request.app.state.session_manager
    if manager.get_session(session_id) is None:
        return _session_not_found(session_id)

    try:
        body: dict[str, Any] = await request.json()
    except json.JSONDecodeError:
        return JSONResponse(
            status_code=400,
            content={"error": "invalid_json", "message": "Request body is not valid JSON"},
        )

    step_id = body.get("step_id")
    if not step_id:
        return JSONResponse(
            status_code=400,
            content={"error": "missing_fields", "message": "step_id is required"},
        )

    try:
        _, step = manager.get_step_by_id(step_id)
    except KeyError as exc:
        return JSONResponse(
            status_code=404,
            content={"error": "not_found", "message": str(exc)},
        )

    # Datastar sends the signal store: {step_id: "...", responses: {...}}
    raw_responses = body.get("responses", {})
    if not isinstance(raw_responses, dict):
        return JSONResponse(
            status_code=400,
            content={"error": "invalid_responses", "message": "responses must be an object"},
        )

    # Map the signal store to UserResponse objects using the same logic as submit
    mapped = _map_signals_to_responses(step.findings, raw_responses)

    # Save each non-empty response
    saved_count = 0
    for finding, response in zip(step.findings, mapped, strict=True):
        if response.action is not None or response.selected is not None or response.answer is not None or response.complexity is not None:
            # Merge with existing response to preserve fields from earlier saves
            if step.responses and finding.id:
                idx = next(
                    (i for i, f in enumerate(step.findings) if f.id == finding.id), None
                )
                if idx is not None and idx < len(step.responses):
                    existing = step.responses[idx]
                    response = response.merge_over(existing)
            try:
                manager.save_response(session_id, step_id, finding.id, response)
                saved_count += 1
            except ValueError as exc:
                return JSONResponse(
                    status_code=400,
                    content={"error": "invalid_request", "message": str(exc)},
                )

    return JSONResponse(status_code=200, content={"status": "ok", "saved": saved_count})


@router.post("/{session_id}/submit")
@datastar_response
async def post_submit(session_id: str, request: Request):  # noqa: ANN201
    """Accept user responses for all findings in a workflow step.

    Handles both JSON API calls (with ``responses`` array) and Datastar
    signal submissions (with ``responses.*`` keys in the body).
    """
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        raise HTTPException(
            status_code=404,
            detail={"error": "session_not_found", "message": f"Session '{session_id}' not found"},
        )

    try:
        body: dict[str, Any] = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(
            status_code=400,
            detail={"error": "invalid_json", "message": "Request body is not valid JSON"},
        )

    step_id = body.get("step_id")
    if not step_id:
        raise HTTPException(
            status_code=400,
            detail={
                "error": "missing_step_id",
                "message": (
                    "step_id is required. Specify the workflow step to submit responses for."
                ),
            },
        )

    try:
        session_from_step, step = manager.get_step_by_id(step_id)
    except KeyError as exc:
        raise HTTPException(
            status_code=404,
            detail={"error": "step_not_found", "message": str(exc)},
        ) from exc

    if session_from_step.session_id != session_id:
        raise HTTPException(
            status_code=400,
            detail={
                "error": "step_session_mismatch",
                "message": (
                    f"Step '{step_id}' belongs to session '{session_from_step.session_id}', "
                    f"not '{session_id}'"
                ),
            },
        )

    if step.state.value != "ready":
        raise HTTPException(
            status_code=409,
            detail={
                "error": "invalid_state",
                "message": (
                    f"Step '{step.step_name}' (id={step_id}) in session '{session_id}' is in "
                    f"state '{step.state.value}', expected 'ready'"
                ),
            },
        )

    # Check if this is a Datastar signal submission (has "responses" as a dict)
    # or a direct JSON API call (has "responses" as a list).
    raw_responses = body.get("responses", {})

    if isinstance(raw_responses, list):
        # Direct JSON API call — list of UserResponse dicts
        responses = [UserResponse.model_validate(r) for r in raw_responses]
    elif isinstance(raw_responses, dict):
        # Datastar signal submission — map finding IDs to responses
        responses = _map_signals_to_responses(step.findings, raw_responses)
    else:
        raise HTTPException(
            status_code=400,
            detail={
                "error": "invalid_responses",
                "message": "responses must be a list or object",
            },
        )

    # Merge with auto-saved responses to preserve fields not in the signal store
    if step.responses:
        responses = [
            resp.merge_over(existing)
            for resp, existing in zip(responses, step.responses, strict=False)
        ]

    try:
        review = manager.submit_responses(session_id, step_id, responses)
    except ValueError as exc:
        raise HTTPException(
            status_code=400,
            detail={"error": "response_count_mismatch", "message": str(exc)},
        ) from exc
    logger.info(
        "Session %s step '%s' (id=%s) submitted with %d responses",
        session_id,
        step.step_name,
        step_id,
        len(responses),
    )

    def _sse_patches():  # noqa: ANN202
        yield SSE.patch_elements(
            '<div id="review-status" class="submit-banner">'
            "Review submitted — thank you!</div>",
        )
        yield SSE.patch_elements(
            '<div id="submit-section">'
            '<button class="submit-btn submit-btn--done"'
            " disabled>Review submitted</button></div>",
        )

    return _sse_patches()


def _map_signals_to_responses(
    findings: list[Finding],
    signals: dict[str, Any],
) -> list[UserResponse]:
    """Convert Datastar response signals to a list of UserResponse objects.

    Args:
        findings: The step's findings list (determines ordering).
        signals: Dict mapping finding IDs to response values from Datastar signals.

    Returns:
        Ordered list of UserResponse objects matching the findings list.
    """
    responses: list[UserResponse] = []
    for finding in findings:
        value = signals.get(finding.id)
        if finding.type == "text":
            responses.append(UserResponse(answer=value if isinstance(value, str) else None))
        elif finding.type == "triage":
            action = None
            if isinstance(value, str) and value in {a.value for a in ResponseAction}:
                action = ResponseAction(value)
            # Extract selected approach (if any)
            approach_key = f"{finding.id}_approach"
            selected = signals.get(approach_key)
            selected = selected if isinstance(selected, str) else None
            other_text = None
            if selected == "__other__":
                other_key = f"{finding.id}_approach_other"
                raw_other = signals.get(other_key)
                if isinstance(raw_other, str) and raw_other.strip():
                    other_text = raw_other.strip()
            # Extract complexity override (if any)
            complexity_key = f"{finding.id}_complexity"
            raw_complexity = signals.get(complexity_key)
            complexity = None
            if isinstance(raw_complexity, str) and raw_complexity in {c.value for c in Complexity}:
                complexity = Complexity(raw_complexity)
            responses.append(
                UserResponse(action=action, selected=selected, other_text=other_text, complexity=complexity)
            )
        elif finding.type == "evaluation":
            responses.append(UserResponse())
        else:
            responses.append(UserResponse())
    return responses


def _notification_dot_html(
    tab_id: str,
    href: str,
    label: str,
    badge_html: str = "",
) -> str:
    """Return a Datastar-compatible element that adds the notification-dot class to a tab.

    The returned ``<a>`` must include the full inner content (label, badge span)
    because ``ElementPatchMode.OUTER`` replaces the entire element.

    Args:
        tab_id: The DOM id of the tab link element (e.g. "step-tab-<step_id>").
        href: The link target for the tab.
        label: The visible text label for the tab.
        badge_html: Optional pre-built HTML for the status badge ``<span>``.

    Returns:
        An HTML snippet that patches the tab element via SSE.
    """
    return (
        f'<a id="{html.escape(tab_id)}" '
        f'href="{html.escape(href)}" '
        f'class="step-link notification-dot">'
        f"{html.escape(label)}"
        f"{badge_html}"
        f"</a>"
    )


def _default_step_id(steps: list[Any]) -> str | None:
    """Pick the best default step: last started/ready step, else last step."""
    for step in reversed(steps):
        if step.state.value in ("started", "ready"):
            return step.step_id
    return steps[-1].step_id if steps else None


@router.get("/{session_id}/stream")
@datastar_response
async def stream_findings(session_id: str, request: Request):  # noqa: ANN201
    """SSE endpoint that streams findings as they arrive for a workflow step."""
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return SSE.patch_elements(
            f'<div id="error">Session \'{html.escape(session_id)}\' not found</div>'
        )

    # Determine which step to stream (by step_id)
    step_id = request.query_params.get("step")
    if not step_id:
        step_id = _default_step_id(session.steps)

    # Track which tab is active so we can send notification dots for other tabs
    active_tab = request.query_params.get("active_tab", "step")

    async def _generate():  # noqa: ANN202
        """Yield SSE events for existing and new findings."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _sse_queues[session_id].append(queue)
        try:
            current_session = manager.get_session(session_id)
            if current_session is None:
                return

            # Track finding counts for all steps to detect changes in non-active tabs
            step_finding_counts: dict[str, int] = {
                s.step_id: len(s.findings) for s in current_session.steps
            }

            # Track step states for badge updates
            step_states: dict[str, str] = {
                s.step_id: s.state.value for s in current_session.steps
            }
            session_state: str = current_session.state.value

            # Count of findings already sent for the active step
            seen = 0

            # If viewing a step tab, stream its findings
            if active_tab == "step":
                # Find the target step by step_id
                target_step = None
                if step_id:
                    for s in current_session.steps:
                        if s.step_id == step_id:
                            target_step = s
                            break

                if target_step is None:
                    return

                # Backfill existing findings as a single morph (safe on reconnect)
                if target_step.findings:
                    container_html = '<div id="findings-container">' + "".join(
                        finding_fragment(f, session_id) for f in target_step.findings
                    ) + "</div>"
                    yield SSE.patch_elements(container_html)
                    seen = len(target_step.findings)

                # Backfill agent status and logs
                if target_step.agents:
                    yield SSE.patch_elements(
                        render("fragments/agent_status.html", step=target_step),
                    )
                if target_step.logs:
                    yield SSE.patch_elements(
                        render("fragments/log_viewer.html", step=target_step),
                    )

                # Track log count for incremental updates
                seen_logs = len(target_step.logs)

                # If already ready/completed, show submit UI immediately
                if target_step.state.value in ("ready", "completed"):
                    if target_step.state.value == "completed":
                        yield SSE.patch_elements(
                            '<div id="review-status" class="submit-banner">'
                            "Review submitted — thank you!</div>",
                        )
                        yield SSE.patch_elements(
                            '<div id="submit-section">'
                            '<button class="submit-btn"'
                            ' style="background: #059669; cursor: default;"'
                            " disabled>Review submitted</button></div>",
                        )
                    else:
                        yield SSE.patch_elements(
                            '<div id="review-status" class="submit-banner">'
                            "All agents complete — ready for review</div>",
                        )
                        yield SSE.patch_elements(
                            '<div id="submit-section">'
                            f'<button class="submit-btn" '
                            f"data-on:click=\"@post('/{html.escape(session_id)}/submit')\">"
                            "Submit Review</button></div>",
                        )
                    return

            # Stream events (findings for active step + notification dots for other tabs)
            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=0.5)
                except TimeoutError:
                    # Check if session still exists
                    current_session = manager.get_session(session_id)
                    if current_session is None:
                        return
                    continue

                current_session = manager.get_session(session_id)
                if current_session is None:
                    return

                if event.startswith("notification:"):
                    notif_id = event.split(":", 1)[1]
                    if current_session and current_session.notifications:
                        notif = next(
                            (n for n in current_session.notifications if n.id == notif_id),
                            None,
                        )
                        if notif is not None:
                            script = _build_notification_script(
                                notif, "window.focus(); n.close();"
                            )
                            yield SSE.execute_script(script)
                        # Re-render the notification timeline on the session page
                        timeline_html = render(
                            "fragments/notification_timeline.html", s=current_session
                        )
                        yield SSE.patch_elements(
                            timeline_html,
                            selector=f"#notifications-{session_id}",
                            mode=ElementPatchMode.OUTER,
                        )
                    continue

                # Check for notification dots on non-active step tabs
                for s in current_session.steps:
                    old_count = step_finding_counts.get(s.step_id, 0)
                    new_count = len(s.findings)
                    if new_count > old_count:
                        step_finding_counts[s.step_id] = new_count
                        # Only show dot if this is NOT the currently viewed step
                        if s.step_id != step_id or active_tab == "plan":
                            yield SSE.patch_elements(
                                _notification_dot_html(
                                    tab_id=f"step-tab-{s.step_id}",
                                    href=f"/{session_id}?step={s.step_id}",
                                    label=s.step_name,
                                    badge_html=(
                                        f'<span id="step-badge-{s.step_id}"'
                                        f' class="status-badge status-{s.state.value}">'
                                        f"{s.state.value}</span>"
                                    ),
                                ),
                                mode=ElementPatchMode.OUTER,
                            )

                # If zing_file was updated, show dot on the Plan tab (unless viewing it)
                if (
                    event == "session_updated"
                    and active_tab != "plan"
                    and current_session.zing_file
                ):
                    yield SSE.patch_elements(
                        _notification_dot_html(
                            tab_id="step-tab-plan",
                            href=f"/{session_id}?tab=plan",
                            label="Plan",
                        ),
                        mode=ElementPatchMode.OUTER,
                    )

                # Update step tab badges and session header when state changes
                states_changed = False
                for s in current_session.steps:
                    if step_states.get(s.step_id) != s.state.value:
                        step_states[s.step_id] = s.state.value
                        states_changed = True
                        yield SSE.patch_elements(
                            f'<span id="step-badge-{html.escape(s.step_id)}" '
                            f'class="status-badge status-{html.escape(s.state.value)}">'
                            f"{html.escape(s.state.value)}</span>",
                        )
                if states_changed or session_state != current_session.state.value:
                    session_state = current_session.state.value
                    yield SSE.patch_elements(
                        f'<span id="session-status-badge" '
                        f'class="status-badge status-'
                        f'{html.escape(current_session.state.value)}">'
                        f"{html.escape(current_session.state.value)}</span>",
                    )

                # If viewing a step tab, also stream findings for the active step
                if active_tab == "step" and step_id:
                    target_step = None
                    for s in current_session.steps:
                        if s.step_id == step_id:
                            target_step = s
                            break
                    if target_step is None:
                        return

                    # Yield any new findings since last check
                    for finding in target_step.findings[seen:]:
                        yield SSE.patch_elements(
                            finding_fragment(finding, session_id),
                            selector="#findings-container",
                            mode=ElementPatchMode.APPEND,
                        )
                        seen += 1

                    # Stream agent status changes
                    if event in ("agent_started", "agent_stopped"):
                        yield SSE.patch_elements(
                            render("fragments/agent_status.html", step=target_step),
                        )

                    # Stream new log entries
                    if event == "log_added" and len(target_step.logs) > seen_logs:
                        yield SSE.patch_elements(
                            render("fragments/log_viewer.html", step=target_step),
                        )
                        seen_logs = len(target_step.logs)

                    # Show intermediate status when agents finish
                    if event == "agents_done":
                        yield SSE.patch_elements(
                            '<div id="review-status" class="submit-banner">'
                            "All agents complete</div>",
                        )

                    # Check terminal states
                    if target_step.state.value == "completed":
                        yield SSE.patch_elements(
                            '<div id="review-status" class="submit-banner">'
                            "Review submitted — thank you!</div>",
                        )
                        yield SSE.patch_elements(
                            '<div id="submit-section">'
                            '<button class="submit-btn"'
                            ' style="background: #059669; cursor: default;"'
                            " disabled>Review submitted</button></div>",
                        )
                        return
                    if event == "ready" or target_step.state.value == "ready":
                        yield SSE.patch_elements(
                            '<div id="review-status" class="submit-banner">'
                            "All agents complete — ready for review</div>",
                        )
                        yield SSE.patch_elements(
                            '<div id="submit-section">'
                            f'<button class="submit-btn" '
                            f"data-on:click=\"@post('/{html.escape(session_id)}/submit')\">"
                            "Submit Review</button></div>",
                        )
                        return

                    if event == "completed":
                        yield SSE.patch_elements(
                            '<div id="review-status" class="submit-banner">'
                            "Review submitted — thank you!</div>",
                        )
                        yield SSE.patch_elements(
                            '<div id="submit-section">'
                            '<button class="submit-btn"'
                            ' style="background: #059669; cursor: default;"'
                            " disabled>Review submitted</button></div>",
                        )
                        return
        finally:
            # Remove this connection's queue
            queues = _sse_queues.get(session_id, [])
            if queue in queues:
                queues.remove(queue)
            if not queues:
                _sse_queues.pop(session_id, None)

    return _generate()


@router.get("/dashboard")
async def get_dashboard(request: Request) -> HTMLResponse:
    """Return the dashboard HTML page."""
    manager = request.app.state.session_manager
    sessions = sorted(manager.list_sessions(), key=lambda s: s.created_at, reverse=True)
    html = render("dashboard.html", sessions=sessions)
    return HTMLResponse(content=html)


@router.get("/dashboard/events")
@datastar_response
async def dashboard_events(request: Request):  # noqa: ANN201
    """SSE endpoint that pushes re-rendered session list when session state changes."""
    manager = request.app.state.session_manager

    async def _generate():  # noqa: ANN202
        """Yield SSE events with updated dashboard session list."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=30.0)
                except TimeoutError:
                    continue

                if event.startswith("notification:"):
                    # Format: "notification:{notif_id}:{session_id}"
                    parts = event.split(":", 2)
                    notif_id = parts[1] if len(parts) > 1 else ""
                    notif_session_id = parts[2] if len(parts) > 2 else ""
                    session = manager.get_session(notif_session_id)
                    if session and session.notifications:
                        notif = next(
                            (n for n in session.notifications if n.id == notif_id),
                            None,
                        )
                        if notif is not None:
                            session_url = json.dumps(f"/{notif_session_id}")
                            script = _build_notification_script(
                                notif,
                                f"window.location.href = {session_url}; n.close();",
                            )
                            yield SSE.execute_script(script)
                        # Re-render the notification timeline for the affected session card
                        timeline_html = render("fragments/notification_timeline.html", s=session)
                        yield SSE.patch_elements(
                            timeline_html,
                            selector=f"#notifications-{notif_session_id}",
                            mode=ElementPatchMode.OUTER,
                        )
                    continue

                sessions = sorted(
                    manager.list_sessions(), key=lambda s: s.created_at, reverse=True,
                )

                if event in ("created", "cleaned_up"):
                    # Structural change — full re-render
                    page_html = render("dashboard.html", sessions=sessions)
                    yield SSE.patch_elements(
                        page_html, selector="body", mode=ElementPatchMode.INNER,
                    )
                else:
                    # State-only change — targeted per-card patches
                    for s in sessions:
                        card_html = render("fragments/session_card.html", s=s)
                        yield SSE.patch_elements(
                            card_html,
                            selector=f"#session-card-{s.session_id}",
                            mode=ElementPatchMode.OUTER,
                        )
        finally:
            if queue in _dashboard_queues:
                _dashboard_queues.remove(queue)

    return _generate()


@router.post("/sessions/{session_id}/cleanup", response_model=None)
@datastar_response
async def post_cleanup(
    session_id: str, request: Request,
):  # noqa: ANN201
    """Remove a session from the manager.

    Args:
        session_id: The session to clean up.
        request: The incoming request.
    """
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return

    manager.cleanup_session(session_id)
    logger.info("Session %s cleaned up via dashboard", session_id)
    return SSE.redirect("/dashboard")



@router.get("/{session_id}", response_model=None)
async def get_session_page(session_id: str, request: Request) -> HTMLResponse | JSONResponse:
    """Return the review page HTML for a specific session."""
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return _session_not_found(session_id)

    tab = request.query_params.get("tab")
    plan_html = None
    current_tab = None

    if tab == "plan" and session.zing_file:
        current_tab = "plan"
        zing_path = pathlib.Path(session.zing_file)
        if zing_path.is_file():
            plan_html = render_markdown(zing_path.read_text(encoding="utf-8"))

    # Determine which step to display (by step_id)
    step_id = request.query_params.get("step")
    if not step_id:
        step_id = _default_step_id(session.steps)

    # Build saved responses dict for Datastar signal initialization
    saved_responses: dict[str, str] = {}
    if step_id:
        for s in session.steps:
            if s.step_id == step_id and s.responses:
                for i, finding in enumerate(s.findings):
                    if i < len(s.responses):
                        resp = s.responses[i]
                        if resp.action is not None:
                            saved_responses[finding.id] = resp.action.value
                        if resp.selected is not None:
                            saved_responses[f"{finding.id}_approach"] = resp.selected
                            if resp.other_text is not None:
                                saved_responses[f"{finding.id}_approach_other"] = (
                                    resp.other_text
                                )
                        elif resp.answer is not None:
                            saved_responses[finding.id] = resp.answer
                        if resp.complexity is not None:
                            saved_responses[f"{finding.id}_complexity"] = (
                                resp.complexity.value
                            )
                break

    # Resolve the active step object for agent/log display in templates
    active_step = None
    if step_id:
        for s in session.steps:
            if s.step_id == step_id:
                active_step = s
                break

    # For completed/ready steps, pre-render findings and submit UI server-side
    # instead of relying on SSE backfill (which may close before Datastar processes events)
    rendered_findings: list[str] = []
    review_status_html = ""
    submit_html = ""
    if active_step and active_step.state.value in ("ready", "completed"):
        rendered_findings = [
            finding_fragment(f, session_id, saved_responses=saved_responses)
            for f in active_step.findings
        ]
        if active_step.state.value == "completed":
            review_status_html = (
                '<div id="review-status" class="submit-banner">'
                "Review submitted — thank you!</div>"
            )
            submit_html = (
                '<div id="submit-section">'
                '<button class="submit-btn"'
                ' style="background: #059669; cursor: default;"'
                " disabled>Review submitted</button></div>"
            )
        else:
            review_status_html = (
                '<div id="review-status" class="submit-banner">'
                "All agents complete — ready for review</div>"
            )
            submit_html = (
                '<div id="submit-section">'
                '<button class="submit-btn" '
                f"data-on:click=\"@post('/{html.escape(session_id)}/submit')\">"
                "Submit Review</button></div>"
            )

    page_html = render(
        "review.html",
        session=session,
        current_step=step_id,
        current_tab=current_tab,
        plan_html=plan_html,
        saved_responses=saved_responses,
        step=active_step,
        rendered_findings=rendered_findings,
        review_status_html=review_status_html,
        submit_html=submit_html,
    )
    return HTMLResponse(content=page_html)
