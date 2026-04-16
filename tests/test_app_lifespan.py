"""Tests for the FastAPI app lifespan: poller startup and state initialization."""

from __future__ import annotations

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


if __name__ == "__main__":
    unittest.main()
