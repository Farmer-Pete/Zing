"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

import contextlib
from collections.abc import AsyncGenerator

from fastapi import FastAPI
from starlette.applications import Starlette
from starlette.routing import Mount

from zing_ai.server.mcp_tools import configure, mcp_server
from zing_ai.server.routes import _notify_dashboard_connections, _notify_sse_connections, router
from zing_ai.server.sessions import SessionManager


def create_app(
    session_manager: SessionManager | None = None,
    port: int = 9876,
) -> Starlette:
    """Create and configure the application.

    Returns a Starlette app that routes MCP paths to the MCP sub-app
    and everything else to the FastAPI web UI.

    Args:
        session_manager: Optional SessionManager instance. Creates a default one if not provided.
        port: The port the server will listen on, used for MCP tool URL construction.
    """
    sm = session_manager or SessionManager()

    # Map SessionManager events to the existing SSE/dashboard notification functions
    def _on_session_event(event_type: str, session_id: str) -> None:
        sse_events = {
            "finding_added": "finding",
            "step_started": "step_started",
            "agent_started": "agent_started",
            "agent_stopped": "agent_stopped",
            "agents_done": "agents_done",
            "step_ready": "ready",
            "review_submitted": "completed",
            "log_added": "log_added",
            "session_updated": "session_updated",
        }
        dashboard_events = {
            "session_created": "created",
            "step_started": "step_started",
            "agent_started": "agent_started",
            "agent_stopped": "agent_stopped",
            "review_submitted": "review_submitted",
            "session_cleaned_up": "cleaned_up",
        }
        if event_type in sse_events:
            _notify_sse_connections(session_id, sse_events[event_type])
        if event_type in dashboard_events:
            _notify_dashboard_connections(dashboard_events[event_type])

    sm.add_listener(_on_session_event)

    @contextlib.asynccontextmanager
    async def lifespan(app: Starlette) -> AsyncGenerator[None]:
        async with mcp_server.session_manager.run():
            yield

    mcp_starlette = mcp_server.streamable_http_app()

    fastapi_app = FastAPI(
        title="Zing Batch Review",
        description="Batch review UI for Zing AI development pipeline",
    )
    fastapi_app.state.session_manager = sm
    configure(sm, port=port)
    fastapi_app.include_router(router)

    routes = [*mcp_starlette.routes, Mount("/", app=fastapi_app)]

    return Starlette(
        routes=routes,
        lifespan=lifespan,
    )
