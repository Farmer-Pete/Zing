"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

import contextlib
from collections.abc import AsyncGenerator

from fastapi import FastAPI

from zing_ai.server.mcp_tools import configure, mcp_server
from zing_ai.server.routes import router
from zing_ai.server.sessions import SessionManager


def create_app(
    session_manager: SessionManager | None = None,
    port: int = 9876,
) -> FastAPI:
    """Create and configure a FastAPI application instance.

    Args:
        session_manager: Optional SessionManager instance. Creates a default one if not provided.
        port: The port the server will listen on, used for MCP tool URL construction.
    """
    sm = session_manager or SessionManager()

    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncGenerator[None]:
        async with mcp_server.session_manager.run():
            yield

    app = FastAPI(
        title="Zing Batch Review",
        description="Batch review UI for Zing AI development pipeline",
        lifespan=lifespan,
    )
    app.state.session_manager = sm
    configure(sm, port=port)
    app.include_router(router)
    app.mount("/mcp", mcp_server.streamable_http_app())
    return app
