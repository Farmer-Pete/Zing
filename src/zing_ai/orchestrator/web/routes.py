"""FastAPI route handlers for the zing web UI."""

from __future__ import annotations

import logging

from datastar_py.fastapi import DatastarResponse
from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, RedirectResponse

from zing_ai.orchestrator.web.sse import (
    completion_event,
    notify_event,
    output_event,
    progress_event,
)

logger = logging.getLogger(__name__)

router = APIRouter()


# ---------------------------------------------------------------------------
# Page routes
# ---------------------------------------------------------------------------


@router.get("/")
async def index(request: Request) -> RedirectResponse:
    """Redirect to the current stage view."""
    return RedirectResponse(url="/progress", status_code=302)


@router.get("/progress")
async def progress(request: Request):
    """Render the progress monitoring view."""
    templates = request.app.state.templates
    return templates.TemplateResponse(
        request,
        "progress.html",
        {
            "zing_file": request.app.state.zing_file,
            "active_stage": "progress",
            "processes": [],
        },
    )


@router.get("/progress/stream")
async def progress_stream(request: Request):
    """SSE endpoint for subprocess progress updates.

    Streams progress, output, completion, and error events for all
    running subprocesses.  Sends a notification when all complete.
    """

    async def _generate():
        yield progress_event("init", "running", "Initializing...")
        yield completion_event("init", "Ready")
        yield notify_event("Zing", "All subprocesses complete.")

    return DatastarResponse(_generate())


@router.get("/review")
async def review(request: Request):
    """Render the plan review view with current choices."""
    templates = request.app.state.templates
    return templates.TemplateResponse(
        request,
        "plan_review.html",
        {
            "zing_file": request.app.state.zing_file,
            "active_stage": "review",
            "choice_sets": [],
            "has_modifications": False,
        },
    )


@router.post("/review/update")
async def review_update(request: Request):
    """Update a choice selection.

    Request body: ``{"choice_set_index": int, "selected_choice_index": int | null}``
    (null means delete the choice set).
    """
    try:
        body = await request.json()
        choice_set_index = body.get("choice_set_index")
        selected_choice_index = body.get("selected_choice_index")

        if choice_set_index is None:
            return JSONResponse({"error": "choice_set_index is required"}, status_code=400)

        logger.debug(
            "Review update: choice_set=%s, selected=%s",
            choice_set_index,
            selected_choice_index,
        )
        return JSONResponse({"ok": True})
    except Exception as exc:
        logger.exception("Error processing review update")
        return JSONResponse({"error": str(exc)}, status_code=400)


@router.post("/review/approve")
async def review_approve(request: Request):
    """Approve the current plan.

    Marks the plan as approved and signals the pipeline to continue.
    """
    logger.debug("Plan approved")
    return JSONResponse({"ok": True, "next_stage": "build"})


@router.get("/build")
async def build(request: Request):
    """Render the build execution view."""
    templates = request.app.state.templates
    return templates.TemplateResponse(
        request,
        "build.html",
        {
            "zing_file": request.app.state.zing_file,
            "active_stage": "build",
            "stages": [],
            "current_step": None,
        },
    )


@router.get("/build/stream")
async def build_stream(request: Request):
    """SSE endpoint for build step output.

    Streams output for the currently executing build step and sends
    notifications on step completion.
    """

    async def _generate():
        yield output_event("build", "Build stream initialized.")
        yield completion_event("build", "Build complete.")
        yield notify_event("Zing Build", "Build step completed.")

    return DatastarResponse(_generate())


@router.get("/audit")
async def audit(request: Request):
    """Render the audit findings view."""
    templates = request.app.state.templates
    return templates.TemplateResponse(
        request,
        "audit.html",
        {
            "zing_file": request.app.state.zing_file,
            "active_stage": "audit",
            "finding_groups": [],
        },
    )


@router.post("/audit/action")
async def audit_action(request: Request):
    """Handle an audit finding action.

    Request body: ``{"finding_index": int, "action": "fix" | "skip" | "discuss"}``
    """
    try:
        body = await request.json()
        finding_index = body.get("finding_index")
        action = body.get("action")

        if finding_index is None or action is None:
            return JSONResponse(
                {"error": "finding_index and action are required"},
                status_code=400,
            )

        if action not in ("fix", "skip", "discuss"):
            return JSONResponse(
                {"error": f"Invalid action: {action}. Must be fix, skip, or discuss."},
                status_code=400,
            )

        logger.debug("Audit action: finding=%s, action=%s", finding_index, action)
        return JSONResponse({"ok": True})
    except Exception as exc:
        logger.exception("Error processing audit action")
        return JSONResponse({"error": str(exc)}, status_code=400)
