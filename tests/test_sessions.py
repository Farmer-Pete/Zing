"""Tests for the SessionManager class."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from zing_ai.server.models import SessionState, UserResponse
from zing_ai.server.sessions import SessionManager

_STEP = "review"


class TestSessionLifecycle(unittest.TestCase):
    """Test the full lifecycle: create, start step, add findings, complete, submit."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _create_session_with_step(
        self, session_id: str = "s1", title: str = "Test", expected_agents: int = 1
    ) -> None:
        self.manager.create_session(session_id, title, "test.zing")
        self.manager.start_step(session_id, _STEP, expected_agents)

    def test_create_session(self) -> None:
        """Creating a session sets initial state to PENDING with no steps."""
        session = self.manager.create_session("s1", "Test Session", "test.zing")
        assert session.session_id == "s1"
        assert session.state == SessionState.PENDING
        assert session.steps == []
        assert session.total_findings == 0

    def test_start_step(self) -> None:
        """Starting a step adds it to the session."""
        self.manager.create_session("s1", "Test", "test.zing")
        step = self.manager.start_step("s1", "review", 2)
        assert step.step_name == "review"
        assert step.sequence == 0
        assert step.expected_agents == 2
        assert step.state == SessionState.PENDING

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 1

    def test_add_finding_text(self) -> None:
        """Adding a text finding appends it to the step."""
        self._create_session_with_step()
        finding = self.manager.add_finding("s1", {
            "type": "text",
            "title": "What is the meaning of life?",
        }, step_name=_STEP)
        assert finding.type == "text"
        session = self.manager.get_session("s1")
        assert session is not None
        assert session.total_findings == 1
        assert len(session.steps[0].findings) == 1

    def test_add_finding_evaluation(self) -> None:
        """Adding an evaluation finding appends it to the step."""
        self._create_session_with_step()
        finding = self.manager.add_finding("s1", {
            "type": "evaluation",
            "title": "Pass 1: Design Fundamentals",
            "criteria": [
                {"name": "Clarity", "rating": "strong", "justification": "Clear"},
            ],
            "litmus_tests": [
                {"name": "Simplest thing?", "result": "Yes"},
            ],
            "warnings": [
                {"name": "Future flexibility", "found": False},
            ],
        }, step_name=_STEP)
        assert finding.type == "evaluation"
        session = self.manager.get_session("s1")
        assert session is not None
        assert session.total_findings == 1

    def test_add_finding_choice(self) -> None:
        """Adding a choice finding appends it to the step."""
        self._create_session_with_step()
        finding = self.manager.add_finding("s1", {
            "type": "choice",
            "title": "Pick one",
            "options": [
                {"label": "A", "description": "Option A"},
                {"label": "B", "description": "Option B"},
            ],
        }, step_name=_STEP)
        assert finding.type == "choice"

    def test_add_finding_triage(self) -> None:
        """Adding a triage finding appends it to the step."""
        self._create_session_with_step()
        finding = self.manager.add_finding("s1", {
            "type": "triage",
            "title": "Unused import",
            "category": "style",
            "severity": "low",
            "confidence": "high",
        }, step_name=_STEP)
        assert finding.type == "triage"

    def test_add_finding_auto_creates_step(self) -> None:
        """Adding a finding for a nonexistent step auto-creates it."""
        self.manager.create_session("s1", "Test", "test.zing")
        self.manager.add_finding("s1", {
            "type": "text",
            "title": "Auto step",
        }, step_name="auto-step")
        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 1
        assert session.steps[0].step_name == "auto-step"

    def test_full_lifecycle(self) -> None:
        """Full lifecycle: create → start step → add findings → agents complete → submit."""
        self._create_session_with_step(expected_agents=2)

        self.manager.add_finding("s1", {
            "type": "text",
            "title": "Is this correct?",
        }, step_name=_STEP)
        self.manager.add_finding("s1", {
            "type": "triage",
            "title": "Bug found",
            "category": "correctness",
            "severity": "high",
            "confidence": "medium",
        }, step_name=_STEP)

        # First agent completes — still pending
        step = self.manager.mark_agent_complete("s1", _STEP)
        assert step.state == SessionState.PENDING
        assert step.completed_agents == 1

        # Second agent completes — now ready
        step = self.manager.mark_agent_complete("s1", _STEP)
        assert step.state == SessionState.READY
        assert step.completed_agents == 2

        # Submit responses
        responses = [
            UserResponse(answer="Yes, it is correct"),
            UserResponse(action="accept"),
        ]
        review = self.manager.submit_responses("s1", _STEP, responses)
        assert review.session_id == "s1"
        assert review.step_name == _STEP
        assert len(review.items) == 2

        session = self.manager.get_session("s1")
        assert session is not None
        assert session.state == SessionState.COMPLETED


class TestConcurrentSessions(unittest.TestCase):
    """Test that two sessions don't interfere with each other."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_sessions_are_isolated(self) -> None:
        """Findings added to one session don't appear in another."""
        self.manager.create_session("s1", "Session 1", "a.zing")
        self.manager.start_step("s1", _STEP, 1)
        self.manager.create_session("s2", "Session 2", "b.zing")
        self.manager.start_step("s2", _STEP, 1)

        self.manager.add_finding("s1", {
            "type": "text",
            "title": "Question for s1",
        }, step_name=_STEP)
        self.manager.add_finding("s2", {
            "type": "triage",
            "title": "Finding for s2",
            "category": "correctness",
            "severity": "high",
            "confidence": "high",
        }, step_name=_STEP)

        s1 = self.manager.get_session("s1")
        s2 = self.manager.get_session("s2")
        assert s1 is not None
        assert s2 is not None
        assert s1.total_findings == 1
        assert s2.total_findings == 1
        assert s1.steps[0].findings[0].type == "text"  # type: ignore[union-attr]
        assert s2.steps[0].findings[0].type == "triage"  # type: ignore[union-attr]

    def test_agent_completion_isolated(self) -> None:
        """Completing an agent in one session doesn't affect another."""
        self.manager.create_session("s1", "Session 1", "a.zing")
        self.manager.start_step("s1", _STEP, 1)
        self.manager.create_session("s2", "Session 2", "b.zing")
        self.manager.start_step("s2", _STEP, 2)

        self.manager.mark_agent_complete("s1", _STEP)
        s1 = self.manager.get_session("s1")
        s2 = self.manager.get_session("s2")
        assert s1 is not None
        assert s2 is not None
        assert s1.steps[0].state == SessionState.READY
        assert s2.steps[0].state == SessionState.PENDING
        assert s2.steps[0].completed_agents == 0


class TestCleanup(unittest.TestCase):
    """Test cleanup of sessions."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_cleanup_removes_session(self) -> None:
        """Cleanup removes a session from memory and disk."""
        self.manager.create_session("s1", "Test", "test.zing")
        assert self.manager.get_session("s1") is not None
        assert (self.data_dir / "s1.json").exists()

        self.manager.cleanup_session("s1")
        assert self.manager.get_session("s1") is None
        assert not (self.data_dir / "s1.json").exists()

    def test_cleanup_nonexistent_session(self) -> None:
        """Cleaning up a nonexistent session does not raise."""
        self.manager.cleanup_session("nonexistent")  # Should not raise

    def test_list_sessions_after_cleanup(self) -> None:
        """Cleaned up sessions don't appear in list_sessions."""
        self.manager.create_session("s1", "Session 1", "a.zing")
        self.manager.create_session("s2", "Session 2", "b.zing")
        assert len(self.manager.list_sessions()) == 2

        self.manager.cleanup_session("s1")
        sessions = self.manager.list_sessions()
        assert len(sessions) == 1
        assert sessions[0].session_id == "s2"


class TestAgentTracking(unittest.TestCase):
    """Test expected_agents tracking and edge cases."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_increments_correctly(self) -> None:
        """Each mark_agent_complete increments the count by one."""
        self.manager.create_session("s1", "Test", "test.zing")
        self.manager.start_step("s1", _STEP, 3)

        step = self.manager.mark_agent_complete("s1", _STEP)
        assert step.completed_agents == 1
        assert step.state == SessionState.PENDING

        step = self.manager.mark_agent_complete("s1", _STEP)
        assert step.completed_agents == 2
        assert step.state == SessionState.PENDING

        step = self.manager.mark_agent_complete("s1", _STEP)
        assert step.completed_agents == 3
        assert step.state == SessionState.READY

    def test_duplicate_agent_complete_beyond_expected(self) -> None:
        """Extra mark_agent_complete calls beyond expected still work (idempotent READY)."""
        self.manager.create_session("s1", "Test", "test.zing")
        self.manager.start_step("s1", _STEP, 1)

        self.manager.mark_agent_complete("s1", _STEP)
        step = self.manager.mark_agent_complete("s1", _STEP)
        assert step.completed_agents == 2
        assert step.state == SessionState.READY

    def test_invalid_session_id_raises(self) -> None:
        """Operations on a nonexistent session raise KeyError."""
        with self.assertRaises(KeyError):
            self.manager.add_finding("nonexistent", {"type": "text", "title": "Q"}, step_name=_STEP)

        with self.assertRaises(KeyError):
            self.manager.mark_agent_complete("nonexistent", _STEP)

        with self.assertRaises(KeyError):
            self.manager.submit_responses("nonexistent", _STEP, [])


class TestPersistence(unittest.TestCase):
    """Test that sessions persist across SessionManager restarts."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_sessions_survive_restart(self) -> None:
        """Sessions created by one manager are loaded by a new one."""
        mgr1 = SessionManager(data_dir=self.data_dir)
        mgr1.create_session("s1", "Persistent", "test.zing")
        mgr1.start_step("s1", _STEP, 2)
        mgr1.add_finding("s1", {
            "type": "text",
            "title": "Will this persist?",
        }, step_name=_STEP)
        mgr1.mark_agent_complete("s1", _STEP)

        # Create a new manager pointing at the same data dir
        mgr2 = SessionManager(data_dir=self.data_dir)
        session = mgr2.get_session("s1")
        assert session is not None
        assert session.title == "Persistent"
        assert session.total_findings == 1
        assert session.steps[0].completed_agents == 1
        assert session.steps[0].state == SessionState.PENDING

    def test_old_format_migration(self) -> None:
        """Old flat-findings format is migrated to workflow steps on load."""
        import json

        old_session = {
            "session_id": "old1",
            "title": "Legacy",
            "zing_file": "test.zing",
            "expected_agents": 2,
            "completed_agents": 1,
            "state": "pending",
            "findings": [
                {"type": "text", "id": "f1", "title": "Old finding"},
            ],
            "responses": None,
            "created_at": "2025-01-01T00:00:00",
        }
        (self.data_dir / "old1.json").write_text(json.dumps(old_session))

        mgr = SessionManager(data_dir=self.data_dir)
        session = mgr.get_session("old1")
        assert session is not None
        assert len(session.steps) == 1
        assert session.steps[0].step_name == "review"
        assert len(session.steps[0].findings) == 1
        assert session.steps[0].expected_agents == 2
        assert session.steps[0].completed_agents == 1


class TestWorkflowStepLooping(unittest.TestCase):
    """Test that the same step name can be used multiple times."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_duplicate_step_names(self) -> None:
        """Starting the same step name twice creates two steps."""
        self.manager.create_session("s1", "Test", "test.zing")
        step1 = self.manager.start_step("s1", "review", 1)
        step2 = self.manager.start_step("s1", "review", 1)
        assert step1.sequence == 0
        assert step2.sequence == 1

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 2

    def test_findings_go_to_latest_step(self) -> None:
        """Findings are added to the most recent step with the given name."""
        self.manager.create_session("s1", "Test", "test.zing")
        self.manager.start_step("s1", "review", 1)
        self.manager.add_finding("s1", {"type": "text", "title": "First"}, step_name="review")

        self.manager.start_step("s1", "review", 1)
        self.manager.add_finding("s1", {"type": "text", "title": "Second"}, step_name="review")

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps[0].findings) == 1
        assert len(session.steps[1].findings) == 1
        assert session.steps[0].findings[0].title == "First"  # type: ignore[union-attr]
        assert session.steps[1].findings[0].title == "Second"  # type: ignore[union-attr]


class TestWaitForReview(unittest.TestCase):
    """Test the async wait_for_review method."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_wait_for_review_resolves_on_submit(self) -> None:
        """wait_for_review unblocks when submit_responses is called."""

        async def _run() -> None:
            self.manager.create_session("s1", "Async Test", "test.zing")
            self.manager.start_step("s1", _STEP, 1)
            self.manager.add_finding("s1", {
                "type": "text",
                "title": "Async question",
            }, step_name=_STEP)
            self.manager.mark_agent_complete("s1", _STEP)

            async def _submit_later() -> None:
                await asyncio.sleep(0.05)
                self.manager.submit_responses("s1", _STEP, [
                    UserResponse(answer="async answer"),
                ])

            submit_task = asyncio.create_task(_submit_later())
            review = await self.manager.wait_for_review("s1", _STEP)
            await submit_task

            assert review.session_id == "s1"
            assert review.step_name == _STEP
            assert len(review.items) == 1
            assert review.items[0].response.answer == "async answer"

        asyncio.run(_run())

    def test_wait_returns_immediately_if_completed(self) -> None:
        """wait_for_review returns immediately if step is already completed."""

        async def _run() -> None:
            self.manager.create_session("s1", "Test", "test.zing")
            self.manager.start_step("s1", _STEP, 1)
            self.manager.add_finding("s1", {
                "type": "text",
                "title": "Q",
            }, step_name=_STEP)
            self.manager.mark_agent_complete("s1", _STEP)
            self.manager.submit_responses("s1", _STEP, [UserResponse(answer="A")])

            # Should return immediately since step is completed
            review = await self.manager.wait_for_review("s1", _STEP)
            assert review.step_name == _STEP
            assert len(review.items) == 1

        asyncio.run(_run())


if __name__ == "__main__":
    unittest.main()
