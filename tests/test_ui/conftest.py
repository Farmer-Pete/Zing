"""Shared fixtures for Playwright-based UI tests."""

from __future__ import annotations

import tempfile
import threading
from collections.abc import Generator
from pathlib import Path

import pytest
import uvicorn

from zing_ai.server.app import create_app
from zing_ai.server.sessions import SessionManager


class _ServerInfo:
    """Holds the base URL and session manager for a running test server."""

    def __init__(self, base_url: str, manager: SessionManager) -> None:
        self.base_url = base_url
        self.manager = manager


@pytest.fixture(scope="session")
def ui_server() -> Generator[_ServerInfo]:
    """Start a uvicorn server in a background thread on a random port.

    Session-scoped so the server is shared across all UI tests.
    """
    tmp = tempfile.TemporaryDirectory()
    data_dir = Path(tmp.name)
    manager = SessionManager(data_dir=data_dir)
    app = create_app(session_manager=manager)

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

    yield _ServerInfo(base_url=f"http://127.0.0.1:{port}", manager=manager)

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
