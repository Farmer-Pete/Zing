"""FastAPI route handlers for the Zing batch review server."""

from __future__ import annotations

import asyncio
import html
import json
import logging
from collections import defaultdict
from typing import Any

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, JSONResponse

from zing_ai.server.models import Finding, ResponseAction, UserResponse
from zing_ai.server.templates import render

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


def _notify_dashboard_connections(event: str) -> None:
    """Push an event to all active dashboard SSE queues.

    Args:
        event: An event type string (e.g. "created", "completed", "cleaned_up").
    """
    for queue in _dashboard_queues:
        queue.put_nowait(event)


def finding_fragment(finding: Finding) -> str:
    """Render a single finding as an HTML fragment.

    Args:
        finding: The finding model to render.

    Returns:
        Rendered HTML string for the finding.
    """
    return render("fragments/finding.html", finding=finding)


@router.post("/{session_id}/save-response")
async def post_save_response(session_id: str, request: Request) -> JSONResponse:
    """Auto-save a single finding response on blur/change events.

    Accepts JSON with ``step_id``, ``finding_id``, and response fields
    (``action``, ``selected``, ``answer``, ``other_text``).
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
    finding_id = body.get("finding_id")
    if not step_id or not finding_id:
        return JSONResponse(
            status_code=400,
            content={
                "error": "missing_fields",
                "message": "step_id and finding_id are required",
            },
        )

    # Build a UserResponse from the remaining fields
    response = UserResponse(
        action=body.get("action"),
        selected=body.get("selected"),
        answer=body.get("answer"),
        other_text=body.get("other_text"),
    )

    try:
        manager.save_response(session_id, step_id, finding_id, response)
    except KeyError as exc:
        return JSONResponse(
            status_code=404,
            content={"error": "not_found", "message": str(exc)},
        )
    except ValueError as exc:
        return JSONResponse(
            status_code=400,
            content={"error": "invalid_request", "message": str(exc)},
        )

    return JSONResponse(status_code=200, content={"status": "ok"})


@router.post("/{session_id}/submit")
async def post_submit(session_id: str, request: Request) -> JSONResponse:
    """Accept user responses for all findings in a workflow step.

    Handles both JSON API calls (with ``responses`` array) and Datastar
    signal submissions (with ``responses.*`` keys in the body).
    """
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
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
            content={
                "error": "missing_step_id",
                "message": (
                    "step_id is required. Specify the workflow step to submit responses for."
                ),
            },
        )

    try:
        _session_from_step, step = manager.get_step_by_id(step_id)
    except KeyError as exc:
        return JSONResponse(
            status_code=404,
            content={"error": "step_not_found", "message": str(exc)},
        )

    if step.state.value != "ready":
        return JSONResponse(
            status_code=409,
            content={
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
        return JSONResponse(
            status_code=400,
            content={"error": "invalid_responses", "message": "responses must be a list or object"},
        )

    try:
        review = manager.submit_responses(session_id, step_id, responses)
    except ValueError as exc:
        return JSONResponse(
            status_code=400,
            content={"error": "response_count_mismatch", "message": str(exc)},
        )
    logger.info(
        "Session %s step '%s' (id=%s) submitted with %d responses",
        session_id,
        step.step_name,
        step_id,
        len(responses),
    )
    return JSONResponse(
        status_code=200,
        content={
            "status": "ok",
            "session_id": review.session_id,
            "step_name": review.step_name,
            "items_count": len(review.items),
        },
    )


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
        elif finding.type == "choice":
            selected = value if isinstance(value, str) else None
            other_text = None
            if selected == "__other__":
                other_key = f"{finding.id}_other"
                raw_other = signals.get(other_key)
                if isinstance(raw_other, str) and raw_other.strip():
                    other_text = raw_other.strip()
            responses.append(UserResponse(selected=selected, other_text=other_text))
        elif finding.type == "triage":
            action = None
            if isinstance(value, str) and value in {a.value for a in ResponseAction}:
                action = ResponseAction(value)
            responses.append(UserResponse(action=action))
        elif finding.type == "evaluation":
            responses.append(UserResponse())
        else:
            responses.append(UserResponse())
    return responses


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
    if not step_id and session.steps:
        step_id = session.steps[-1].step_id

    async def _generate():  # noqa: ANN202
        """Yield SSE events for existing and new findings."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _sse_queues[session_id].append(queue)
        try:
            current_session = manager.get_session(session_id)
            if current_session is None:
                return

            # Find the target step by step_id
            target_step = None
            if step_id:
                for s in current_session.steps:
                    if s.step_id == step_id:
                        target_step = s
                        break

            if target_step is None:
                return

            # Backfill existing findings
            seen = 0
            for finding in target_step.findings:
                yield SSE.patch_elements(
                    finding_fragment(finding),
                    selector="#findings-container",
                    mode="append",
                )
                seen += 1

            # If already ready/completed, show submit UI immediately
            if target_step.state.value in ("ready", "completed"):
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

            # Stream new findings as they arrive via queue notifications
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

                # Re-find the target step by step_id
                target_step = None
                if step_id:
                    for s in current_session.steps:
                        if s.step_id == step_id:
                            target_step = s
                            break
                if target_step is None:
                    return

                # Yield any new findings since last check
                for finding in target_step.findings[seen:]:
                    yield SSE.patch_elements(
                        finding_fragment(finding),
                        selector="#findings-container",
                        mode="append",
                    )
                    seen += 1

                # Check terminal states
                if event == "ready" or target_step.state.value in ("ready", "completed"):
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
    sessions = manager.list_sessions()
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
                    await asyncio.wait_for(queue.get(), timeout=30.0)
                except TimeoutError:
                    continue

                sessions = manager.list_sessions()
                html = render("dashboard.html", sessions=sessions)
                yield SSE.patch_elements(html, selector="body", mode="innerHTML")
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

    # Determine which step to display (by step_id)
    step_id = request.query_params.get("step")
    if not step_id and session.steps:
        step_id = session.steps[-1].step_id

    html = render("review.html", session=session, current_step=step_id)
    return HTMLResponse(content=html)
