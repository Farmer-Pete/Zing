"""Shared fixtures for Playwright-based UI tests."""

from __future__ import annotations

import asyncio
import tempfile
import threading
from collections.abc import Generator
from pathlib import Path

import pytest
import uvicorn
from fastapi import FastAPI

from zing_ai.server.app import create_app
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.sessions import SessionManager


class _ServerInfo:
    """Holds the base URL, session manager, external cache, SSE queues, and FastAPI app."""

    def __init__(
        self,
        base_url: str,
        manager: SessionManager,
        external_cache: ExternalCache,
        cc_queues: list[asyncio.Queue[str]],
        fastapi_app: FastAPI,
    ) -> None:
        self.base_url = base_url
        self.manager = manager
        self.external_cache = external_cache
        self.cc_queues = cc_queues
        self.fastapi_app = fastapi_app


@pytest.fixture(scope="session")
def ui_server() -> Generator[_ServerInfo]:
    """Start a uvicorn server in a background thread on a random port.

    Session-scoped so the server is shared across all UI tests.
    """
    tmp = tempfile.TemporaryDirectory()
    data_dir = Path(tmp.name)
    manager = SessionManager(data_dir=data_dir)
    external_cache = ExternalCache()
    cc_queues: list[asyncio.Queue[str]] = []
    app = create_app(
        session_manager=manager,
        external_cache=external_cache,
        cc_queues=cc_queues,
        disable_polling=True,
    )

    # Navigate the ASGI middleware chain to the inner FastAPI app so tests can
    # inspect or mutate app.state (e.g. inject a mock poller).
    # Chain: MCPDebugMiddleware -> Starlette -> Mount("/", app=FastAPI)
    _starlette = app.app  # type: ignore[attr-defined]
    _fastapi_app: FastAPI = _starlette.routes[-1].app  # type: ignore[attr-defined]

    # Use port 0 to let the OS assign a free port
    config = uvicorn.Config(app, host="127.0.0.1", port=0, log_level="warning")
    server = uvicorn.Server(config)

    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()

    # Wait for server to be ready
    while not server.started:
        pass

    # Extract the actual port from the server socket
    sockets = server.servers[0].sockets  # type: ignore[union-attr]
    port = sockets[0].getsockname()[1]

    yield _ServerInfo(
        base_url=f"http://127.0.0.1:{port}",
        manager=manager,
        external_cache=external_cache,
        cc_queues=cc_queues,
        fastapi_app=_fastapi_app,
    )

    server.should_exit = True
    thread.join(timeout=5)
    tmp.cleanup()


@pytest.fixture
def server(ui_server: _ServerInfo) -> Generator[_ServerInfo]:
    """Per-test fixture that cleans up sessions after each test.

    This lets each test start with a fresh session state while reusing
    the same server process for speed.
    """
    yield ui_server

    # Clean up all sessions created during the test
    for session in list(ui_server.manager.list_sessions()):
        ui_server.manager.cleanup_session(session.session_id)

    # Reset external cache so tests don't bleed state into each other. The
    # ``version`` bump invalidates the ``_build_view`` memo (otherwise the
    # next test would potentially get a cached view rendered against the
    # previous test's ``cache.issues``).
    ui_server.external_cache.issues = []
    ui_server.external_cache.prs = []
    ui_server.external_cache.github_username = ""
    ui_server.external_cache.last_polled_at = None
    ui_server.external_cache.last_error = None
    ui_server.external_cache.version += 1
