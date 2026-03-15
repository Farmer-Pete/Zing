"""Shared base class for Zing server tests."""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from fastapi.testclient import TestClient

from zing_ai.server.app import create_app
from zing_ai.server.sessions import SessionManager

_STEP = "review"


class ServerTestBase(unittest.TestCase):
    """Base class that sets up a TestClient with an isolated SessionManager."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)
        app = create_app(session_manager=self.manager)
        self.client = TestClient(app)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _create_session(
        self,
        session_id: str = "test-session",
        title: str = "Test Session",
    ) -> None:
        """Helper to create a session with a default workflow step for testing."""
        session = self.manager.create_session(
            session_id=session_id,
            title=title,
            zing_file=None,
            steps=[_STEP],
        )
        step = self.manager.start_step(session_id, session.steps[0].step_id)
        self.step_id = step.step_id
