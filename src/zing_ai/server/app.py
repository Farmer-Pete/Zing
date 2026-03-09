"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

from fastapi import FastAPI


def create_app() -> FastAPI:
    """Create and configure a FastAPI application instance."""
    app = FastAPI(
        title="Zing Batch Review",
        description="Batch review UI for Zing AI development pipeline",
    )
    return app
