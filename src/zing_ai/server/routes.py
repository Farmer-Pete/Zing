"""FastAPI route handlers for the Zing batch review server."""

from __future__ import annotations

import asyncio
import logging
from collections import defaultdict
from typing import Any

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, JSONResponse
from pydantic import TypeAdapter, ValidationError

from zing_ai.server.models import Finding, ResponseAction, UserResponse
from zing_ai.server.templates import render

logger = logging.getLogger("zing_ai.server")

router = APIRouter()

# Per-session list of asyncio queues for active SSE connections.
# Each SSE connection registers its own queue to receive push notifications.
_sse_queues: dict[str, list[asyncio.Queue[str]]] = defaultdict(list)


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


def finding_fragment(finding: Finding) -> str:
    """Render a single finding as an HTML fragment.

    Args:
        finding: The finding model to render.

    Returns:
        Rendered HTML string for the finding.
    """
    return render("fragments/finding.html", finding=finding)


@router.post("/{session_id}/findings")
async def post_finding(session_id: str, request: Request) -> JSONResponse:
    """Accept a finding from a subagent and add it to the session."""
    manager = request.app.state.session_manager
    if manager.get_session(session_id) is None:
        return _session_not_found(session_id)

    body: dict[str, Any] = await request.json()
    adapter = TypeAdapter(Finding)
    try:
        adapter.validate_python(body)
    except ValidationError as exc:
        details = [
            {"field": e["loc"][-1] if e["loc"] else "unknown", "error": e["msg"]}
            for e in exc.errors()
        ]
        return JSONResponse(
            status_code=400,
            content={
                "error": "validation_error",
                "message": "Finding validation failed",
                "details": details,
            },
        )

    finding = manager.add_finding(session_id, body)
    _notify_sse_connections(session_id, "finding")
    logger.info("Finding added to session %s: %s", session_id, finding.type)
    return JSONResponse(
        status_code=201,
        content={"status": "ok", "finding_id": finding.id},
    )


@router.post("/{session_id}/agent-complete")
async def post_agent_complete(session_id: str, request: Request) -> JSONResponse:
    """Signal that one subagent has finished producing findings."""
    manager = request.app.state.session_manager
    if manager.get_session(session_id) is None:
        return _session_not_found(session_id)

    session = manager.mark_agent_complete(session_id)
    if session.state.value == "ready":
        _notify_sse_connections(session_id, "ready")
    logger.info(
        "Agent complete for session %s (%d/%d)",
        session_id,
        session.completed_agents,
        session.expected_agents,
    )
    return JSONResponse(
        status_code=200,
        content={
            "status": "ok",
            "completed_agents": session.completed_agents,
            "expected_agents": session.expected_agents,
            "state": session.state.value,
        },
    )


@router.post("/{session_id}/submit")
async def post_submit(session_id: str, request: Request) -> JSONResponse:
    """Accept user responses for all findings in a session.

    Handles both JSON API calls (with ``responses`` array) and Datastar
    signal submissions (with ``responses.*`` keys in the body).
    """
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return _session_not_found(session_id)

    body: dict[str, Any] = await request.json()

    # Check if this is a Datastar signal submission (has "responses" as a dict)
    # or a direct JSON API call (has "responses" as a list).
    raw_responses = body.get("responses", {})

    if isinstance(raw_responses, list):
        # Direct JSON API call — list of UserResponse dicts
        responses = [UserResponse.model_validate(r) for r in raw_responses]
    elif isinstance(raw_responses, dict):
        # Datastar signal submission — map finding IDs to responses
        responses = _map_signals_to_responses(session.findings, raw_responses)
    else:
        return JSONResponse(
            status_code=400,
            content={"error": "invalid_responses", "message": "responses must be a list or object"},
        )

    review = manager.submit_responses(session_id, responses)
    _notify_sse_connections(session_id, "completed")
    logger.info("Session %s submitted with %d responses", session_id, len(responses))
    return JSONResponse(
        status_code=200,
        content={
            "status": "ok",
            "session_id": review.session_id,
            "items_count": len(review.items),
        },
    )


def _map_signals_to_responses(
    findings: list[Finding],
    signals: dict[str, Any],
) -> list[UserResponse]:
    """Convert Datastar response signals to a list of UserResponse objects.

    Args:
        findings: The session's findings list (determines ordering).
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
            responses.append(UserResponse(selected=value if isinstance(value, str) else None))
        elif finding.type == "triage":
            action = None
            if isinstance(value, str) and value in {a.value for a in ResponseAction}:
                action = ResponseAction(value)
            responses.append(UserResponse(action=action))
        else:
            responses.append(UserResponse())
    return responses


@router.get("/{session_id}/stream")
@datastar_response
async def stream_findings(session_id: str, request: Request):  # noqa: ANN201
    """SSE endpoint that streams findings as they arrive."""
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return SSE.patch_elements(
            f'<div id="error">Session \'{session_id}\' not found</div>'
        )

    async def _generate():  # noqa: ANN202
        """Yield SSE events for existing and new findings."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _sse_queues[session_id].append(queue)
        try:
            current_session = manager.get_session(session_id)
            if current_session is None:
                return

            # Backfill existing findings
            seen = 0
            for finding in current_session.findings:
                yield SSE.patch_elements(
                    finding_fragment(finding),
                    selector="#findings-container",
                    mode="append",
                )
                seen += 1

            # If already ready/completed, show submit UI immediately
            if current_session.state.value in ("ready", "completed"):
                yield SSE.patch_elements(
                    '<div id="review-status" class="submit-banner">'
                    "All agents complete — ready for review</div>",
                )
                yield SSE.patch_elements(
                    '<div id="submit-section">'
                    f'<button class="submit-btn" data-on-click="@post(\'/{session_id}/submit\')">'
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

                # Yield any new findings since last check
                for finding in current_session.findings[seen:]:
                    yield SSE.patch_elements(
                        finding_fragment(finding),
                        selector="#findings-container",
                        mode="append",
                    )
                    seen += 1

                # Check terminal states
                if event == "ready" or current_session.state.value in ("ready", "completed"):
                    yield SSE.patch_elements(
                        '<div id="review-status" class="submit-banner">'
                        "All agents complete — ready for review</div>",
                    )
                    yield SSE.patch_elements(
                        '<div id="submit-section">'
                        f'<button class="submit-btn" '
                        f"data-on-click=\"@post('/{session_id}/submit')\">"
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


@router.get("/{session_id}", response_model=None)
async def get_session_page(session_id: str, request: Request) -> HTMLResponse | JSONResponse:
    """Return the review page HTML for a specific session."""
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return _session_not_found(session_id)

    html = render("review.html", session=session)
    return HTMLResponse(content=html)
