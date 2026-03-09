"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

import contextlib
from collections.abc import AsyncGenerator

from fastapi import FastAPI
from starlette.applications import Starlette
from starlette.routing import Mount

from zing_ai.server.mcp_tools import configure, mcp_server
from zing_ai.server.routes import router
from zing_ai.server.sessions import SessionManager


def create_app(
    session_manager: SessionManager | None = None,
    port: int = 9876,
) -> Starlette:
    """Create and configure the application.

    Returns a Starlette app that routes OAuth/MCP paths to the MCP sub-app
    and everything else to the FastAPI web UI.

    Args:
        session_manager: Optional SessionManager instance. Creates a default one if not provided.
        port: The port the server will listen on, used for MCP tool URL construction.
    """
    sm = session_manager or SessionManager()

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

    # Outer Starlette app: MCP/OAuth routes first, then FastAPI as fallback.
    # Middleware from the MCP app (BearerAuthBackend, AuthContextMiddleware)
    # is carried over so token validation works on the /mcp endpoint.
    # Non-auth routes like /register and /token are unaffected because
    # BearerAuthBackend returns None when no Bearer token is present.
    routes = [*mcp_starlette.routes, Mount("/", app=fastapi_app)]

    return Starlette(
        routes=routes,
        lifespan=lifespan,
        middleware=mcp_starlette.user_middleware,
    )
