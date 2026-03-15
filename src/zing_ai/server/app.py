"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

import contextlib
import logging
import pathlib
import time
from collections.abc import AsyncGenerator

from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from starlette.applications import Starlette
from starlette.routing import Mount
from starlette.types import ASGIApp, Receive, Scope, Send

from zing_ai.server.mcp_tools import configure, mcp_server
from zing_ai.server.routes import _notify_dashboard_connections, _notify_sse_connections, router
from zing_ai.server.sessions import SessionManager

logger = logging.getLogger("zing_ai.server")

_STATIC_DIR = pathlib.Path(__file__).parent / "static"


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
            "notification_added": "notification",
        }
        dashboard_events = {
            "session_created": "created",
            "step_started": "step_started",
            "step_ready": "step_ready",
            "agents_done": "agents_done",
            "agent_started": "agent_started",
            "agent_stopped": "agent_stopped",
            "review_submitted": "review_submitted",
            "session_cleaned_up": "cleaned_up",
            "notification_added": "notification",
        }
        # Events that should include session_id context in dashboard notifications
        _dashboard_session_context_events = {"notification_added"}
        if event_type in sse_events:
            _notify_sse_connections(session_id, sse_events[event_type])
        if event_type in dashboard_events:
            kwargs = {}
            if event_type in _dashboard_session_context_events:
                kwargs["session_id"] = session_id
            _notify_dashboard_connections(dashboard_events[event_type], **kwargs)

    sm.add_listener(_on_session_event)

    @contextlib.asynccontextmanager
    async def lifespan(app: Starlette) -> AsyncGenerator[None]:
        async with mcp_server.session_manager.run():
            yield

    mcp_starlette = mcp_server.streamable_http_app()

    class MCPDebugMiddleware:
        """Log request/response details for /mcp requests."""

        def __init__(self, app: ASGIApp) -> None:
            self.app = app

        async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
            if scope["type"] != "http" or scope["path"] != "/mcp":
                await self.app(scope, receive, send)
                return

            method = scope.get("method", "?")
            headers = dict(scope.get("headers", []))
            # Decode header keys/values for logging
            header_strs = {
                k.decode("latin-1"): v.decode("latin-1")
                for k, v in scope.get("headers", [])
            }
            logger.info(
                "MCP >>> %s /mcp headers=%s",
                method,
                {k: v for k, v in header_strs.items() if k in (
                    "content-type", "accept", "mcp-session-id",
                    "mcp-protocol-version", "authorization",
                )},
            )

            # Capture request body
            body_parts: list[bytes] = []
            request_complete = False

            async def receive_wrapper() -> dict:
                nonlocal request_complete
                msg = await receive()
                if msg["type"] == "http.request":
                    body_parts.append(msg.get("body", b""))
                    if not msg.get("more_body", False):
                        request_complete = True
                        body = b"".join(body_parts)
                        body_preview = body[:500].decode("utf-8", errors="replace")
                        logger.info("MCP >>> body: %s", body_preview)
                return msg

            # Capture response status
            response_status = 0
            response_headers: dict[str, str] = {}

            async def send_wrapper(message: dict) -> None:
                nonlocal response_status, response_headers
                if message["type"] == "http.response.start":
                    response_status = message["status"]
                    response_headers = {
                        k.decode("latin-1"): v.decode("latin-1")
                        for k, v in message.get("headers", [])
                    }
                    logger.info(
                        "MCP <<< %d headers=%s",
                        response_status,
                        {k: v for k, v in response_headers.items()
                         if k in ("content-type", "mcp-session-id")},
                    )
                elif message["type"] == "http.response.body":
                    body = message.get("body", b"")
                    if body and response_status >= 400:
                        logger.info(
                            "MCP <<< body: %s",
                            body[:500].decode("utf-8", errors="replace"),
                        )
                await send(message)

            start = time.monotonic()
            await self.app(scope, receive_wrapper, send_wrapper)
            elapsed = time.monotonic() - start
            logger.info("MCP --- %s /mcp → %d (%.3fs)", method, response_status, elapsed)

    fastapi_app = FastAPI(
        title="Zing Batch Review",
        description="Batch review UI for Zing AI development pipeline",
    )
    fastapi_app.state.session_manager = sm
    configure(sm, port=port)
    fastapi_app.mount("/static", StaticFiles(directory=_STATIC_DIR), name="static")
    fastapi_app.include_router(router)

    routes = [*mcp_starlette.routes, Mount("/", app=fastapi_app)]

    starlette_app = Starlette(
        routes=routes,
        lifespan=lifespan,
    )

    return MCPDebugMiddleware(starlette_app)
