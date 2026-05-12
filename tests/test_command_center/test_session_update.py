"""Tests for the tightened session_update zing_file invariant."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from tests.test_server_base import ServerTestBase
from zing_ai.server.mcp_tools import configure, session_update
from zing_ai.server.models import ZingSession


class TestSessionUpdateZingFileInvariant(ServerTestBase):
    """session_update rejects non-absolute and non-existent zing_file paths."""

    def _setup_session(self, session_id: str = "upd-test") -> None:
        configure(self.manager, port=9876)
        self.manager.create_session(session_id, "Title")

    def test_relative_path_rejected_with_error(self) -> None:
        self._setup_session()
        result = asyncio.run(session_update(session_id="upd-test", zing_file="relative/path.md"))
        self.assertIn("error", result)
        self.assertIn("absolute", result["error"].lower())

    def test_nonexistent_path_rejected_with_error(self) -> None:
        self._setup_session()
        result = asyncio.run(
            session_update(session_id="upd-test", zing_file="/nonexistent/path/foo.md")
        )
        self.assertIn("error", result)
        self.assertIn("does not exist", result["error"])

    def test_absolute_existing_path_accepted(self) -> None:
        self._setup_session()
        with tempfile.NamedTemporaryFile(mode="w", suffix=".md", delete=False) as f:
            f.write("# title\n")
            tmp_path = f.name
        try:
            result = asyncio.run(session_update(session_id="upd-test", zing_file=tmp_path))
            self.assertEqual(result["status"], "updated")
            session = self.manager.get_session("upd-test")
            assert isinstance(session, ZingSession)
            self.assertEqual(session.zing_file, tmp_path)
        finally:
            Path(tmp_path).unlink()


if __name__ == "__main__":
    unittest.main()
