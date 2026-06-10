"""Tests for the viz preview gate: model, sessions plumbing, attention, flow."""

from __future__ import annotations

import asyncio
import json
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path

from zing_ai.server.attention import build_attention_queue
from zing_ai.server.flow import build_flow_context
from zing_ai.server.models import VizPreview, VizPreviewDecision, ZingSession
from zing_ai.server.sessions import SessionManager

_MINIMAL_GRAPH = {
    "title": "Test plan",
    "steps": [
        {
            "step": 1,
            "id": "step-1",
            "title": "First step",
            "side": "unchanged",
            "nodes": [
                {
                    "id": "node-1",
                    "shape": "rect",
                    "side": "unchanged",
                    "label": "Do thing",
                }
            ],
            "edges": [],
        }
    ],
}


class _VizPreviewBase(unittest.TestCase):
    """Base with a SessionManager + ZingSession + on-disk viz/md files."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)
        self.session_id = "s-viz-1"

        self.md_path = self.data_dir / "plan.md"
        self.md_path.write_text("# Plan\n\nSome body.\n", encoding="utf-8")
        self.viz_path = self.data_dir / "plan.viz.json"
        self.viz_path.write_text(json.dumps(_MINIMAL_GRAPH), encoding="utf-8")

        self.manager.create_session(
            self.session_id,
            "Viz preview session",
            zing_file=str(self.md_path),
            steps=[],
        )

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _session(self) -> ZingSession:
        sess = self.manager.get_session(self.session_id)
        assert isinstance(sess, ZingSession)
        return sess


class TestVizPreviewModel(_VizPreviewBase):
    def test_serialization_round_trip(self) -> None:
        preview = VizPreview(viz_path="/a.json", md_path="/a.md", gate_label="x")
        dumped = preview.model_dump_json()
        round_trip = VizPreview.model_validate_json(dumped)
        assert round_trip.viz_path == "/a.json"
        assert round_trip.gate_label == "x"
        assert round_trip.iteration == 1

    def test_session_persists_pending_preview(self) -> None:
        """pending_viz_preview survives a SessionManager reload."""
        self.manager.set_viz_preview(
            self.session_id,
            str(self.viz_path),
            str(self.md_path),
            "Topology review",
        )
        reloaded = SessionManager(data_dir=self.data_dir)
        sess = reloaded.get_session(self.session_id)
        assert isinstance(sess, ZingSession)
        assert sess.pending_viz_preview is not None
        assert sess.pending_viz_preview.gate_label == "Topology review"
        assert sess.pending_viz_preview.iteration == 1


class TestSetVizPreview(_VizPreviewBase):
    def test_sets_preview_first_time(self) -> None:
        preview = self.manager.set_viz_preview(
            self.session_id, str(self.viz_path), str(self.md_path), "Gate A"
        )
        assert preview.iteration == 1
        assert self._session().pending_viz_preview is not None

    def test_replace_increments_iteration(self) -> None:
        self.manager.set_viz_preview(
            self.session_id, str(self.viz_path), str(self.md_path), "Gate A"
        )
        second = self.manager.set_viz_preview(
            self.session_id, str(self.viz_path), str(self.md_path), "Gate A round 2"
        )
        assert second.iteration == 2

    def test_replace_resolves_prior_wait_as_reject(self) -> None:
        """A pending wait_for_viz_preview is unblocked with a reject when superseded."""

        async def _scenario() -> VizPreviewDecision:
            self.manager.set_viz_preview(
                self.session_id, str(self.viz_path), str(self.md_path), "Gate A"
            )
            wait_task = asyncio.create_task(self.manager.wait_for_viz_preview(self.session_id))
            await asyncio.sleep(0)  # let the wait subscribe to the event
            self.manager.set_viz_preview(
                self.session_id, str(self.viz_path), str(self.md_path), "Gate A round 2"
            )
            return await asyncio.wait_for(wait_task, timeout=1.0)

        decision = asyncio.run(_scenario())
        assert decision.decision == "reject"
        assert "superseded" in decision.comments

    def test_set_on_unknown_session_raises(self) -> None:
        with self.assertRaises(KeyError):
            self.manager.set_viz_preview("nope", str(self.viz_path), str(self.md_path), "x")


class TestResolveAndWait(_VizPreviewBase):
    def test_resolve_accept_clears_pending(self) -> None:
        self.manager.set_viz_preview(self.session_id, str(self.viz_path), str(self.md_path), "x")
        self.manager.resolve_viz_preview(self.session_id, "accept", "looks good")
        assert self._session().pending_viz_preview is None

    def test_wait_returns_decision(self) -> None:
        async def _scenario() -> VizPreviewDecision:
            self.manager.set_viz_preview(
                self.session_id, str(self.viz_path), str(self.md_path), "x"
            )
            wait_task = asyncio.create_task(self.manager.wait_for_viz_preview(self.session_id))
            await asyncio.sleep(0)
            self.manager.resolve_viz_preview(self.session_id, "reject", "rename node-1")
            return await asyncio.wait_for(wait_task, timeout=1.0)

        decision = asyncio.run(_scenario())
        assert decision.decision == "reject"
        assert decision.comments == "rename node-1"

    def test_wait_returns_immediately_when_pre_resolved(self) -> None:
        """Decisions resolved before wait_for_viz_preview is called are picked up."""

        async def _scenario() -> VizPreviewDecision:
            self.manager.set_viz_preview(
                self.session_id, str(self.viz_path), str(self.md_path), "x"
            )
            self.manager.resolve_viz_preview(self.session_id, "accept", "")
            return await asyncio.wait_for(
                self.manager.wait_for_viz_preview(self.session_id), timeout=1.0
            )

        decision = asyncio.run(_scenario())
        assert decision.decision == "accept"

    def test_resolve_without_pending_raises(self) -> None:
        with self.assertRaises(ValueError):
            self.manager.resolve_viz_preview(self.session_id, "accept", "")

    def test_cleanup_session_unblocks_wait(self) -> None:
        """cleanup_session sets the event so wait_for_viz_preview returns control."""

        async def _scenario() -> bool:
            self.manager.set_viz_preview(
                self.session_id, str(self.viz_path), str(self.md_path), "x"
            )
            wait_task = asyncio.create_task(self.manager.wait_for_viz_preview(self.session_id))
            await asyncio.sleep(0)
            self.manager.cleanup_session(self.session_id)
            try:
                await asyncio.wait_for(wait_task, timeout=1.0)
            except KeyError:
                return True
            return False

        assert asyncio.run(_scenario()) is True


class TestAttentionQueue(_VizPreviewBase):
    def test_pending_preview_emits_viz_preview_item(self) -> None:
        self.manager.set_viz_preview(
            self.session_id, str(self.viz_path), str(self.md_path), "Topology review"
        )
        queue = build_attention_queue(self.manager.list_sessions(), datetime.now(UTC))
        items = [it for it in queue if it.action_type == "viz_preview"]
        assert len(items) == 1
        assert items[0].session_id == self.session_id
        assert items[0].description == "Topology review"
        assert items[0].step_id == f"viz_preview:{self.session_id}"

    def test_no_pending_preview_emits_nothing(self) -> None:
        queue = build_attention_queue(self.manager.list_sessions(), datetime.now(UTC))
        assert not [it for it in queue if it.action_type == "viz_preview"]


class TestFlowContext(_VizPreviewBase):
    def test_viz_preview_active_loads_render_context(self) -> None:
        self.manager.set_viz_preview(
            self.session_id, str(self.viz_path), str(self.md_path), "Topology review"
        )
        queue = build_attention_queue(self.manager.list_sessions(), datetime.now(UTC))
        active = next(it for it in queue if it.action_type == "viz_preview")
        ctx = build_flow_context(self.manager, queue, active)
        assert ctx["viz_preview"].gate_label == "Topology review"
        assert ctx["steps"], "render context should include laid-out steps"
        assert "rendered_markdown" in ctx
        assert "default_pan_y" in ctx
        assert "default_scale" in ctx


if __name__ == "__main__":
    unittest.main()
