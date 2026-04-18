"""Tests for the FastAPI app lifespan: poller startup and state initialization."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from fastapi.testclient import TestClient

from zing_ai.server.app import create_app
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.sessions import SessionManager


class TestAppLifespan(unittest.TestCase):
    """Tests for app lifespan and state initialization."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_lifespan_starts_and_stops_poller_cleanly(self) -> None:
        """Entering and exiting the lifespan context must not raise."""
        app = create_app(session_manager=self.manager)
        with TestClient(app):
            # Lifespan has entered: poller is running as a background task.
            pass
        # Lifespan has exited: poller task was cancelled and aclose() was called.
        # No exception means the poller started and stopped cleanly.

    def test_app_state_has_external_cache(self) -> None:
        """fastapi_app.state.external_cache must be set after app creation."""
        from zing_ai.server.app import create_app as _create_app

        # We need access to the inner fastapi_app, not the Starlette wrapper.
        # TestClient entering the lifespan is not needed for this check because
        # state is set in create_app() body, before the lifespan runs.
        app = _create_app(session_manager=self.manager)

        # Unwrap MCPDebugMiddleware → Starlette → find FastAPI mount
        starlette_app = app.app  # type: ignore[attr-defined]
        # The FastAPI app is mounted under Mount("/") as the last route.
        fastapi_app = starlette_app.routes[-1].app  # type: ignore[attr-defined]

        assert fastapi_app.state.external_cache is not None
        assert isinstance(fastapi_app.state.external_cache, ExternalCache)
        assert isinstance(fastapi_app.state.cc_queues, list)

    def test_app_state_aliases_module_level_sse_and_dashboard_queues(self) -> None:
        """fastapi_app.state exposes the legacy _sse_queues / _dashboard_queues.

        Transitional: new code can DI-read these via app.state instead of
        importing the module globals directly. The same dict/list objects
        back both the module-level names and app.state for now.
        """
        from zing_ai.server.routes import _dashboard_queues, _sse_queues

        app = create_app(session_manager=self.manager)
        starlette_app = app.app  # type: ignore[attr-defined]
        fastapi_app = starlette_app.routes[-1].app  # type: ignore[attr-defined]

        # Identity check — not merely equal, but the same object.
        assert fastapi_app.state.sse_queues is _sse_queues
        assert fastapi_app.state.dashboard_queues is _dashboard_queues

    def test_session_events_dispatch_to_cc_queues(self) -> None:
        """SessionManager events must push board_changed to cc_queues.

        The session-event listener bridges local state changes to connected
        Command Center SSE clients via a single board_changed event.
        """
        cc_queues: list[asyncio.Queue[str]] = []
        queue: asyncio.Queue[str] = asyncio.Queue(maxsize=100)
        cc_queues.append(queue)

        app = create_app(session_manager=self.manager, cc_queues=cc_queues)
        assert app is not None  # smoke check; listener attached in create_app

        # Trigger a session creation — fires session_created.
        session = self.manager.create_session(
            session_id="bak-1-test-session",
            title="Test session",
        )
        assert session is not None
        # Bind to a ticket to fire session_updated as well.
        self.manager.update_session(session.session_id, ticket_id="BAK-1")

        drained: list[str] = []
        while not queue.empty():
            drained.append(queue.get_nowait())

        # Both session_created and session_updated dispatch board_changed.
        assert "board_changed" in drained


if __name__ == "__main__":
    unittest.main()
