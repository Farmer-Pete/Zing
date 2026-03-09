"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

from fastapi import FastAPI

from zing_ai.server.routes import router
from zing_ai.server.sessions import SessionManager


def create_app(session_manager: SessionManager | None = None) -> FastAPI:
    """Create and configure a FastAPI application instance.

    Args:
        session_manager: Optional SessionManager instance. Creates a default one if not provided.
    """
    app = FastAPI(
        title="Zing Batch Review",
        description="Batch review UI for Zing AI development pipeline",
    )
    app.state.session_manager = session_manager or SessionManager()
    app.include_router(router)
    return app
