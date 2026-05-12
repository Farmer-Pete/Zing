"""Tests for the step_stop MCP tool (validation gate)."""

from __future__ import annotations

import asyncio
import json
import shutil
import tempfile
import unittest
from pathlib import Path

from tests.test_server_base import ServerTestBase
from zing_ai.server import step_validators
from zing_ai.server.mcp_tools import configure, step_stop
from zing_ai.server.models import SessionState

FIXTURE = (
    Path(__file__).parent.parent
    / "test_viz"
    / "fixtures"
    / "BAK-1321"
    / "BAK-1321-direct-flatten.viz.json"
)


class TestStepStop(ServerTestBase):
    """step_stop runs validators, transitions on success, errors on failure."""

    def setUp(self) -> None:
        super().setUp()
        self._work = tempfile.TemporaryDirectory()
        self.work_dir = Path(self._work.name)
        self.md_path = self.work_dir / "BAK-1321-direct-flatten.md"
        self.viz_path = self.work_dir / "BAK-1321-direct-flatten.viz.json"
        self.md_path.write_text("# title\n")
        shutil.copyfile(FIXTURE, self.viz_path)

        configure(self.manager, port=9876)
        session = self.manager.create_session(
            session_id="ss-test",
            title="ss test",
            zing_file=str(self.md_path),
            steps=["plan", "build"],
        )
        self.session = session
        self.plan_step = session.steps[0]
        self.manager.start_step("ss-test", self.plan_step.step_id)

    def tearDown(self) -> None:
        super().tearDown()
        self._work.cleanup()

    def test_happy_path_transitions_to_ready(self) -> None:
        result = asyncio.run(step_stop(session_id="ss-test", step_id=self.plan_step.step_id))
        self.assertEqual(result["status"], "ready")
        self.assertEqual(result["step_name"], "plan")
        # Reload step from manager and confirm state
        sess, step = self.manager.get_step_by_id(self.plan_step.step_id)
        self.assertEqual(step.state, SessionState.READY)

    def test_bad_json_returns_error_dict(self) -> None:
        self.viz_path.write_text("{ this is not valid json")
        result = asyncio.run(step_stop(session_id="ss-test", step_id=self.plan_step.step_id))
        self.assertIn("error", result)
        # JSONDecodeError messages typically reference line/column
        msg = result["error"]
        self.assertTrue(
            "line" in msg.lower() or "char" in msg.lower(),
            f"expected JSON decode error message, got: {msg!r}",
        )

    def test_missing_viz_json_returns_actionable_error(self) -> None:
        self.viz_path.unlink()
        result = asyncio.run(step_stop(session_id="ss-test", step_id=self.plan_step.step_id))
        self.assertIn("error", result)
        self.assertIn("viz JSON not written", result["error"])
        self.assertIn(str(self.viz_path), result["error"])

    def test_validation_failure_returns_error_dict_with_pointer(self) -> None:
        graph = json.loads(self.viz_path.read_text())
        # Inject a typo into a cross_flow's from_node so xref validation fires
        graph["cross_flows"][0]["from_node"] = "no-such-node"
        self.viz_path.write_text(json.dumps(graph))
        result = asyncio.run(step_stop(session_id="ss-test", step_id=self.plan_step.step_id))
        self.assertIn("error", result)
        self.assertIn("/cross_flows/0/from_node", result["error"])

    def test_server_bug_in_validator_propagates_as_unhandled_exception(self) -> None:
        """A KeyError raised by a validator must NOT be converted to {error: ...}."""

        def buggy(sess, step):  # noqa: ANN001
            raise KeyError("internal server bug")

        original = step_validators.STEP_VALIDATORS["plan"]
        step_validators.STEP_VALIDATORS["plan"] = [buggy]
        try:
            with self.assertRaises(KeyError):
                asyncio.run(step_stop(session_id="ss-test", step_id=self.plan_step.step_id))
        finally:
            step_validators.STEP_VALIDATORS["plan"] = original


if __name__ == "__main__":
    unittest.main()
