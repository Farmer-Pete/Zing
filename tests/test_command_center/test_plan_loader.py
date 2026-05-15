"""Tests for plan_loader.load_plan_for_session."""

from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from fastapi import HTTPException

from zing_ai.server.plan_loader import load_plan_for_session
from zing_ai.server.sessions import SessionManager

FIXTURE = (
    Path(__file__).parent.parent
    / "test_viz"
    / "fixtures"
    / "BAK-1321"
    / "BAK-1321-direct-flatten.viz.json"
)


class TestPlanLoader(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._work = tempfile.TemporaryDirectory()
        self.work = Path(self._work.name)
        self.manager = SessionManager(data_dir=Path(self._tmp.name))
        self.md_path = self.work / "smoke-plan.md"
        self.viz_path = self.work / "smoke-plan.viz.json"
        self.md_path.write_text("# smoke-plan\n\nBody.\n")
        shutil.copyfile(FIXTURE, self.viz_path)

    def tearDown(self) -> None:
        self._tmp.cleanup()
        self._work.cleanup()

    def _create_session(self, session_id: str = "happy") -> None:
        self.manager.create_session(
            session_id=session_id,
            title="happy",
            zing_file=str(self.md_path),
            steps=["plan"],
        )

    def test_happy_path_returns_markdown_and_viz(self) -> None:
        self._create_session()
        md_text, graph, viz_path = load_plan_for_session("happy", self.manager)
        self.assertEqual(viz_path, self.viz_path)
        self.assertIn("smoke-plan", md_text)
        self.assertEqual(graph["title"], "BAK-1321 · DirectFlatten pipeline")
        self.assertIn("steps", graph)

    def test_missing_session_raises_404(self) -> None:
        with self.assertRaises(HTTPException) as ctx:
            load_plan_for_session("does-not-exist", self.manager)
        self.assertEqual(ctx.exception.status_code, 404)

    def test_session_without_zing_file_raises_404(self) -> None:
        self.manager.create_session(
            session_id="noplan", title="noplan", zing_file=None, steps=["plan"]
        )
        with self.assertRaises(HTTPException) as ctx:
            load_plan_for_session("noplan", self.manager)
        self.assertEqual(ctx.exception.status_code, 404)

    def test_missing_markdown_raises_404(self) -> None:
        self._create_session()
        self.md_path.unlink()
        with self.assertRaises(HTTPException) as ctx:
            load_plan_for_session("happy", self.manager)
        self.assertEqual(ctx.exception.status_code, 404)
        self.assertIn("markdown missing", ctx.exception.detail)

    def test_missing_viz_json_raises_404(self) -> None:
        self._create_session()
        self.viz_path.unlink()
        with self.assertRaises(HTTPException) as ctx:
            load_plan_for_session("happy", self.manager)
        self.assertEqual(ctx.exception.status_code, 404)
        self.assertIn("viz JSON missing", ctx.exception.detail)


if __name__ == "__main__":
    unittest.main()
