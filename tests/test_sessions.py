"""Tests for the SessionManager class."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from zing_ai.server.models import SessionState, UserResponse, WorkflowStep
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
    ) -> WorkflowStep:
        self.manager.create_session(session_id, title, "test.zing")
        return self.manager.start_step(session_id, _STEP, expected_agents)

    def test_create_session(self) -> None:
        """Creating a session sets initial state to PENDING with no steps."""
        session = self.manager.create_session("s1", "Test Session", "test.zing")
        assert session.session_id == "s1"
        assert session.state == SessionState.PENDING
        assert session.steps == []
        assert session.total_findings == 0

    def test_start_step(self) -> None:
        """Starting a step adds it to the session and returns a step_id."""
        self.manager.create_session("s1", "Test", "test.zing")
        step = self.manager.start_step("s1", "review", 2)
        assert step.step_name == "review"
        assert step.sequence == 0
        assert step.expected_agents == 2
        assert step.state == SessionState.PENDING
        assert len(step.step_id) == 32  # uuid4().hex

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 1

    def test_add_finding_text(self) -> None:
        """Adding a text finding appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "text",
            "title": "What is the meaning of life?",
        })
        assert finding.type == "text"
        session = self.manager.get_session("s1")
        assert session is not None
        assert session.total_findings == 1
        assert len(session.steps[0].findings) == 1

    def test_add_finding_evaluation(self) -> None:
        """Adding an evaluation finding appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding("s1", step.step_id, {
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
        })
        assert finding.type == "evaluation"
        session = self.manager.get_session("s1")
        assert session is not None
        assert session.total_findings == 1

    def test_add_finding_choice(self) -> None:
        """Adding a choice finding appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "choice",
            "title": "Pick one",
            "options": [
                {"label": "A", "description": "Option A"},
                {"label": "B", "description": "Option B"},
            ],
        })
        assert finding.type == "choice"

    def test_add_finding_triage(self) -> None:
        """Adding a triage finding appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "triage",
            "title": "Unused import",
            "category": "style",
            "severity": "low",
            "confidence": "high",
        })
        assert finding.type == "triage"

    def test_full_lifecycle(self) -> None:
        """Full lifecycle: create → start step → add findings → agents complete → submit."""
        step = self._create_session_with_step(expected_agents=2)

        self.manager.add_finding("s1", step.step_id, {
            "type": "text",
            "title": "Is this correct?",
        })
        self.manager.add_finding("s1", step.step_id, {
            "type": "triage",
            "title": "Bug found",
            "category": "correctness",
            "severity": "high",
            "confidence": "medium",
        })

        # First agent completes — still pending
        updated = self.manager.mark_agent_complete(step.step_id)
        assert updated.state == SessionState.PENDING
        assert updated.completed_agents == 1

        # Second agent completes — now ready
        updated = self.manager.mark_agent_complete(step.step_id)
        assert updated.state == SessionState.READY
        assert updated.completed_agents == 2

        # Submit responses
        responses = [
            UserResponse(answer="Yes, it is correct"),
            UserResponse(action="accept"),
        ]
        review = self.manager.submit_responses("s1", step.step_id, responses)
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
        step1 = self.manager.start_step("s1", _STEP, 1)
        self.manager.create_session("s2", "Session 2", "b.zing")
        step2 = self.manager.start_step("s2", _STEP, 1)

        self.manager.add_finding("s1", step1.step_id, {
            "type": "text",
            "title": "Question for s1",
        })
        self.manager.add_finding("s2", step2.step_id, {
            "type": "triage",
            "title": "Finding for s2",
            "category": "correctness",
            "severity": "high",
            "confidence": "high",
        })

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
        step1 = self.manager.start_step("s1", _STEP, 1)
        self.manager.create_session("s2", "Session 2", "b.zing")
        step2 = self.manager.start_step("s2", _STEP, 2)

        self.manager.mark_agent_complete(step1.step_id)
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

    def test_cleanup_removes_step_index(self) -> None:
        """Cleanup removes step_id entries from the index."""
        self.manager.create_session("s1", "Test", "test.zing")
        step = self.manager.start_step("s1", _STEP, 1)
        step_id = step.step_id

        self.manager.cleanup_session("s1")
        with self.assertRaises(KeyError):
            self.manager._get_step_by_id(step_id)

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
        step = self.manager.start_step("s1", _STEP, 3)

        updated = self.manager.mark_agent_complete(step.step_id)
        assert updated.completed_agents == 1
        assert updated.state == SessionState.PENDING

        updated = self.manager.mark_agent_complete(step.step_id)
        assert updated.completed_agents == 2
        assert updated.state == SessionState.PENDING

        updated = self.manager.mark_agent_complete(step.step_id)
        assert updated.completed_agents == 3
        assert updated.state == SessionState.READY

    def test_agent_complete_rejected_when_ready(self) -> None:
        """mark_agent_complete raises ValueError once step is READY."""
        self.manager.create_session("s1", "Test", "test.zing")
        step = self.manager.start_step("s1", _STEP, 1)

        self.manager.mark_agent_complete(step.step_id)
        with self.assertRaises(ValueError):
            self.manager.mark_agent_complete(step.step_id)

    def test_invalid_session_id_raises(self) -> None:
        """Operations on a nonexistent session raise KeyError."""
        with self.assertRaises(KeyError):
            self.manager.add_finding("s1", "nonexistent-uuid", {"type": "text", "title": "Q"})

        with self.assertRaises(KeyError):
            self.manager.mark_agent_complete("nonexistent-uuid")

        with self.assertRaises(KeyError):
            self.manager.submit_responses("nonexistent", "nonexistent-step-id", [])

    def test_add_finding_rejects_ready_step(self) -> None:
        """Adding a finding to a READY step raises ValueError."""
        self.manager.create_session("s1", "Test", "test.zing")
        step = self.manager.start_step("s1", _STEP, 1)
        self.manager.mark_agent_complete(step.step_id)

        with self.assertRaises(ValueError):
            self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Late"})

    def test_add_finding_rejects_completed_step(self) -> None:
        """Adding a finding to a COMPLETED step raises ValueError."""
        self.manager.create_session("s1", "Test", "test.zing")
        step = self.manager.start_step("s1", _STEP, 1)
        self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Q"})
        self.manager.mark_agent_complete(step.step_id)
        self.manager.submit_responses("s1", step.step_id, [UserResponse(answer="A")])

        with self.assertRaises(ValueError):
            self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Too late"})

    def test_add_finding_rejects_wrong_session(self) -> None:
        """Adding a finding with a step_id from a different session raises ValueError."""
        self.manager.create_session("s1", "Session 1", "test.zing")
        step = self.manager.start_step("s1", _STEP, 1)
        self.manager.create_session("s2", "Session 2", "test.zing")

        with self.assertRaises(ValueError, msg="Step .* belongs to session 's1', not 's2'"):
            self.manager.add_finding("s2", step.step_id, {"type": "text", "title": "Wrong session"})

    def test_step_id_unique(self) -> None:
        """Each start_step call generates a unique step_id."""
        self.manager.create_session("s1", "Test", "test.zing")
        step1 = self.manager.start_step("s1", "review", 1)
        step2 = self.manager.start_step("s1", "review", 1)
        assert step1.step_id != step2.step_id


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
        step = mgr1.start_step("s1", _STEP, 2)
        mgr1.add_finding("s1", step.step_id, {
            "type": "text",
            "title": "Will this persist?",
        })
        mgr1.mark_agent_complete(step.step_id)

        # Create a new manager pointing at the same data dir
        mgr2 = SessionManager(data_dir=self.data_dir)
        session = mgr2.get_session("s1")
        assert session is not None
        assert session.title == "Persistent"
        assert session.total_findings == 1
        assert session.steps[0].completed_agents == 1
        assert session.steps[0].state == SessionState.PENDING

    def test_step_id_survives_persistence(self) -> None:
        """step_id index is rebuilt on reload."""
        mgr1 = SessionManager(data_dir=self.data_dir)
        mgr1.create_session("s1", "Test", "test.zing")
        step = mgr1.start_step("s1", _STEP, 1)
        step_id = step.step_id

        mgr2 = SessionManager(data_dir=self.data_dir)
        session, reloaded_step = mgr2._get_step_by_id(step_id)
        assert reloaded_step.step_name == _STEP
        assert reloaded_step.step_id == step_id

    def test_old_format_migration(self) -> None:
        """Old flat-findings format is migrated to workflow steps on load."""
        import json

        old_session = {
            "session_id": "old1",
            "title": "Legacy",
            "zing_file": None,
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
        """Starting the same step name twice creates two steps with different IDs."""
        self.manager.create_session("s1", "Test", "test.zing")
        step1 = self.manager.start_step("s1", "review", 1)
        step2 = self.manager.start_step("s1", "review", 1)
        assert step1.sequence == 0
        assert step2.sequence == 1
        assert step1.step_id != step2.step_id

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 2

    def test_findings_go_to_correct_step_by_id(self) -> None:
        """Findings are routed to the correct step via step_id."""
        self.manager.create_session("s1", "Test", "test.zing")
        step1 = self.manager.start_step("s1", "review", 1)
        self.manager.add_finding("s1", step1.step_id, {"type": "text", "title": "First"})

        step2 = self.manager.start_step("s1", "review", 1)
        self.manager.add_finding("s1", step2.step_id, {"type": "text", "title": "Second"})

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
            step = self.manager.start_step("s1", _STEP, 1)
            self.manager.add_finding("s1", step.step_id, {
                "type": "text",
                "title": "Async question",
            })
            self.manager.mark_agent_complete(step.step_id)

            async def _submit_later() -> None:
                await asyncio.sleep(0.05)
                self.manager.submit_responses("s1", step.step_id, [
                    UserResponse(answer="async answer"),
                ])

            submit_task = asyncio.create_task(_submit_later())
            review = await self.manager.wait_for_review("s1", step.step_id)
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
            step = self.manager.start_step("s1", _STEP, 1)
            self.manager.add_finding("s1", step.step_id, {
                "type": "text",
                "title": "Q",
            })
            self.manager.mark_agent_complete(step.step_id)
            self.manager.submit_responses("s1", step.step_id, [UserResponse(answer="A")])

            # Should return immediately since step is completed
            review = await self.manager.wait_for_review("s1", step.step_id)
            assert review.step_name == _STEP
            assert len(review.items) == 1

        asyncio.run(_run())


if __name__ == "__main__":
    unittest.main()
