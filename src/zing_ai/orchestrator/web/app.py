"""FastAPI application factory and server launcher."""

from __future__ import annotations

import logging
import threading
import webbrowser
from pathlib import Path
from typing import Any

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


def start_server_background(
    zing_file_path: Path | None,
    *,
    port: int,
    no_browser: bool,
    **app_state: Any,
) -> threading.Thread:
    """Start the FastAPI web server in a background daemon thread.

    Parameters
    ----------
    zing_file_path:
        Path to the zing XML file (stored on app state).
    port:
        Port to listen on.
    no_browser:
        If ``True``, do not open the browser.
    **app_state:
        Extra attributes to set on ``app.state`` (e.g. ``finding_groups``).

    Returns
    -------
    threading.Thread
        The daemon thread running the server.
    """
    app = create_app(zing_file=zing_file_path)

    for key, value in app_state.items():
        setattr(app.state, key, value)

    thread = threading.Thread(
        target=start_server,
        args=(app,),
        kwargs={"port": port, "no_browser": no_browser},
        daemon=True,
    )
    thread.start()
    return thread
