"""FastAPI route handlers for the Zing batch review server."""

from __future__ import annotations

import asyncio
import logging
from typing import Any

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.fastapi import datastar_response
from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, JSONResponse
from pydantic import TypeAdapter, ValidationError

from zing_ai.server.models import Finding, UserResponse

logger = logging.getLogger("zing_ai.server")

router = APIRouter()


def _session_not_found(session_id: str) -> JSONResponse:
    """Return a 404 response for an unknown session."""
    return JSONResponse(
        status_code=404,
        content={
            "error": "session_not_found",
            "message": f"Session '{session_id}' does not exist",
        },
    )


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
    """Accept user responses for all findings in a session."""
    manager = request.app.state.session_manager
    if manager.get_session(session_id) is None:
        return _session_not_found(session_id)

    body: dict[str, Any] = await request.json()
    raw_responses = body.get("responses", [])
    responses = [UserResponse.model_validate(r) for r in raw_responses]
    review = manager.submit_responses(session_id, responses)
    logger.info("Session %s submitted with %d responses", session_id, len(responses))
    return JSONResponse(
        status_code=200,
        content={
            "status": "ok",
            "session_id": review.session_id,
            "items_count": len(review.items),
        },
    )


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
        current_session = manager.get_session(session_id)
        if current_session is None:
            return

        # Backfill existing findings
        seen = 0
        for finding in current_session.findings:
            yield SSE.patch_elements(
                f'<div id="finding-{finding.id}" class="finding">'
                f"{finding.type}: {finding.id}</div>"
            )
            seen += 1

        # Stream new findings as they arrive
        while True:
            current_session = manager.get_session(session_id)
            if current_session is None:
                return

            # Yield any new findings since last check
            for finding in current_session.findings[seen:]:
                yield SSE.patch_elements(
                    f'<div id="finding-{finding.id}" class="finding">'
                    f"{finding.type}: {finding.id}</div>"
                )
                seen += 1

            # Check if all agents are done
            if current_session.state.value in ("ready", "completed"):
                yield SSE.patch_elements(
                    '<div id="review-status" class="ready">Ready for review</div>'
                )
                return

            await asyncio.sleep(0.1)

    return _generate()


@router.get("/dashboard")
async def get_dashboard(request: Request) -> HTMLResponse:
    """Return the dashboard HTML page."""
    manager = request.app.state.session_manager
    sessions = manager.list_sessions()
    items = "".join(
        f'<li><a href="/{s.session_id}">{s.title}</a> — {s.state.value}</li>'
        for s in sessions
    )
    html = f"<html><body><h1>Zing Dashboard</h1><ul>{items}</ul></body></html>"
    return HTMLResponse(content=html)


@router.get("/{session_id}", response_model=None)
async def get_session_page(session_id: str, request: Request) -> HTMLResponse | JSONResponse:
    """Return the review page HTML for a specific session."""
    manager = request.app.state.session_manager
    session = manager.get_session(session_id)
    if session is None:
        return _session_not_found(session_id)

    findings_html = "".join(
        f'<div id="finding-{f.id}" class="finding">{f.type}: {f.id}</div>'
        for f in session.findings
    )
    html = (
        f"<html><body>"
        f"<h1>{session.title}</h1>"
        f"<div id='findings'>{findings_html}</div>"
        f"<div id='review-status'>{session.state.value}</div>"
        f"</body></html>"
    )
    return HTMLResponse(content=html)
