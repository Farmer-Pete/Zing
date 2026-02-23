"""FastAPI application factory and server launcher."""

from __future__ import annotations

import logging
import webbrowser
from pathlib import Path

import uvicorn
from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from zing_ai.orchestrator.web.routes import router

logger = logging.getLogger(__name__)

_WEB_DIR = Path(__file__).resolve().parent
_TEMPLATES_DIR = _WEB_DIR / "templates"
_STATIC_DIR = _WEB_DIR / "static"


def create_app(zing_file: Path | None = None) -> FastAPI:
    """Create and configure the FastAPI application.

    Parameters
    ----------
    zing_file:
        Optional path to the active zing XML file.  Stored on
        ``app.state.zing_file`` so route handlers can access it.
    """
    app = FastAPI(title="Zing Orchestrator", docs_url=None, redoc_url=None)

    # Jinja2 templates
    templates = Jinja2Templates(directory=str(_TEMPLATES_DIR))
    app.state.templates = templates

    # Static files
    app.mount("/static", StaticFiles(directory=str(_STATIC_DIR)), name="static")

    # Store zing file path for route access
    app.state.zing_file = zing_file

    # Include route handlers
    app.include_router(router)

    logger.debug("FastAPI app created (zing_file=%s)", zing_file)
    return app


def start_server(
    app: FastAPI,
    *,
    port: int = 8741,
    no_browser: bool = False,
) -> None:
    """Start the uvicorn server and optionally open a browser.

    Parameters
    ----------
    app:
        The FastAPI application instance.
    port:
        Port to bind to (default ``8741``).
    no_browser:
        If ``False`` (the default), open the URL in the default browser.
    """
    url = f"http://localhost:{port}"
    print(f"Zing UI: {url}")  # noqa: T201

    if not no_browser:
        webbrowser.open(url)

    uvicorn.run(app, host="127.0.0.1", port=port, log_level="warning")
