"""Tests for /command-center/<session_id>/plan routes."""

from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from fastapi.testclient import TestClient
from mcp.server.fastmcp import FastMCP

FIXTURE = (
    Path(__file__).parent.parent
    / "test_viz"
    / "fixtures"
    / "BAK-1321"
    / "BAK-1321-direct-flatten.viz.json"
)


class TestRoutesPlans(unittest.TestCase):
    def setUp(self) -> None:
        from zing_ai.server.app import create_app
        from zing_ai.server.sessions import SessionManager

        self._data = tempfile.TemporaryDirectory()
        self._work = tempfile.TemporaryDirectory()
        self.work = Path(self._work.name)
        self.md = self.work / "plan.md"
        self.viz = self.work / "plan.viz.json"
        self.md.write_text("# Plan\n\nbody.\n")
        shutil.copyfile(FIXTURE, self.viz)

        self.manager = SessionManager(data_dir=Path(self._data.name))
        self.manager.create_session(
            session_id="sm-1",
            title="sm-1",
            zing_file=str(self.md),
            steps=["plan"],
        )
        mcp_for_test = FastMCP("Zing Plans Test", stateless_http=True)
        app = create_app(
            session_manager=self.manager,
            disable_polling=True,
            mcp_server_instance=mcp_for_test,
        )
        self.app = app
        self.client = TestClient(app)
        self.client.__enter__()

    def tearDown(self) -> None:
        self.client.__exit__(None, None, None)
        self._data.cleanup()
        self._work.cleanup()

    def test_get_renders_viewer_with_default_grid(self) -> None:
        r = self.client.get("/command-center/sm-1/plan")
        self.assertEqual(r.status_code, 200)
        self.assertIn('id="viz-stage"', r.text)
        self.assertIn('id="card-1"', r.text)
        self.assertIn('id="xflow-layer"', r.text)
        # All 13 steps render
        for n in range(1, 14):
            self.assertIn(f'id="card-{n}"', r.text)
        # Default class state
        self.assertIn("viz-card--default", r.text)

    def test_get_for_unknown_session_returns_404(self) -> None:
        r = self.client.get("/command-center/does-not-exist/plan")
        self.assertEqual(r.status_code, 404)

    def test_post_focus_returns_sse_events_with_signals_and_card_patches(self) -> None:
        r = self.client.post(
            "/command-center/sm-1/plan/focus",
            json={"step": 6, "viewport": {"w": 1600, "h": 900}},
        )
        self.assertEqual(r.status_code, 200)
        body = r.text
        # Datastar SSE format: event: ... \n data: ...
        self.assertIn("event: datastar-patch-signals", body)
        self.assertIn("focusedStep", body)
        self.assertIn("event: datastar-patch-elements", body)
        # Card 6 patched as focused; card 4 as pred; card 7/8 as succ
        self.assertIn("viz-card--focused", body)
        self.assertIn("viz-card--pred", body)
        self.assertIn("viz-card--succ", body)
        # xflow-layer is re-rendered
        self.assertIn('id="xflow-layer"', body)

    def test_post_release_restores_default_grid_and_clears_focus(self) -> None:
        # First focus, then release.
        self.client.post(
            "/command-center/sm-1/plan/focus",
            json={"step": 6, "viewport": {"w": 1600, "h": 900}},
        )
        r = self.client.post("/command-center/sm-1/plan/release", json={})
        self.assertEqual(r.status_code, 200)
        body = r.text
        self.assertIn("event: datastar-patch-signals", body)
        # focusedStep cleared (empty-string per Q "don't use null")
        self.assertIn('"focusedStep":""', body.replace(" ", ""))
        # All cards re-rendered as default
        self.assertIn("viz-card--default", body)


if __name__ == "__main__":
    unittest.main()
