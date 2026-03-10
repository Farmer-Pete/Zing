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
        self, session_id: str = "s1", title: str = "Test",
    ) -> WorkflowStep:
        session = self.manager.create_session(session_id, title, steps=[_STEP])
        return self.manager.start_step(session_id, session.steps[0].step_id)

    def test_create_session(self) -> None:
        """Creating a session sets initial state to PENDING with no steps."""
        session = self.manager.create_session("s1", "Test Session")
        assert session.session_id == "s1"
        assert session.state == SessionState.PENDING
        assert session.steps == []
        assert session.total_findings == 0

    def test_create_session_zing_file_none(self) -> None:
        """Creating a session with zing_file=None succeeds."""
        session = self.manager.create_session("s1", "Test", zing_file=None)
        assert session.zing_file is None

    def test_create_session_zing_file_absolute(self) -> None:
        """Creating a session with a valid absolute zing_file path succeeds."""
        session = self.manager.create_session("s1", "Test", zing_file=__file__)
        assert session.zing_file == __file__

    def test_create_session_zing_file_relative_rejected(self) -> None:
        """Creating a session with a relative zing_file path raises ValueError."""
        with self.assertRaises(ValueError, msg="zing_file must be an absolute path"):
            self.manager.create_session("s1", "Test", zing_file="relative/path.md")

    def test_create_session_zing_file_nonexistent_rejected(self) -> None:
        """Creating a session with a non-existent absolute path raises ValueError."""
        with self.assertRaises(ValueError, msg="zing_file path does not exist"):
            self.manager.create_session("s1", "Test", zing_file="/nonexistent/path.md")

    def test_update_session(self) -> None:
        """update_session changes zing_file and title on an existing session."""
        session = self.manager.create_session("s1", "Original Title")
        assert session.title == "Original Title"
        assert session.zing_file is None

        updated = self.manager.update_session(
            "s1", zing_file=__file__, title="New Title",
        )
        assert updated.title == "New Title"
        assert updated.zing_file == __file__

        # Verify changes persisted by reloading
        reloaded = SessionManager(data_dir=self.data_dir)
        s = reloaded.get_session("s1")
        assert s is not None
        assert s.title == "New Title"
        assert s.zing_file == __file__

    def test_update_session_partial(self) -> None:
        """update_session with None parameters skips those fields."""
        self.manager.create_session("s1", "Title", zing_file=__file__)

        # Update only title
        updated = self.manager.update_session("s1", title="New Title")
        assert updated.title == "New Title"
        assert updated.zing_file == __file__

        # Update only zing_file (using setUp.py as a different valid file)
        updated2 = self.manager.update_session("s1", zing_file=__file__)
        assert updated2.title == "New Title"
        assert updated2.zing_file == __file__

    def test_update_session_validates_zing_file(self) -> None:
        """update_session rejects relative and nonexistent zing_file paths."""
        self.manager.create_session("s1", "Title")

        with self.assertRaises(ValueError):
            self.manager.update_session("s1", zing_file="relative/path.md")

        with self.assertRaises(ValueError):
            self.manager.update_session("s1", zing_file="/nonexistent/path.md")

    def test_create_session_with_steps(self) -> None:
        """Creating a session with steps pre-creates all WorkflowStep objects."""
        step_names = ["plan", "plan-audit", "build", "build-audit"]
        session = self.manager.create_session("s1", "Title", steps=step_names)
        assert len(session.steps) == 4
        # Each step has the correct name, sequence, and PENDING state
        for i, name in enumerate(step_names):
            step = session.steps[i]
            assert step.step_name == name
            assert step.sequence == i
            assert step.state == SessionState.PENDING
        # All step_ids are unique
        step_ids = [s.step_id for s in session.steps]
        assert len(set(step_ids)) == 4
        # get_step_by_id works for all pre-created steps
        for step in session.steps:
            found_session, found_step = self.manager.get_step_by_id(step.step_id)
            assert found_session.session_id == "s1"
            assert found_step.step_id == step.step_id

    def test_start_step(self) -> None:
        """Starting a step transitions it from PENDING to STARTED."""
        session = self.manager.create_session("s1", "Test", steps=["review"])
        step_id = session.steps[0].step_id
        step = self.manager.start_step("s1", step_id)
        assert step.step_name == "review"
        assert step.sequence == 0
        assert step.state == SessionState.STARTED
        assert step.step_id == step_id

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 1
        assert session.steps[0].state == SessionState.STARTED

    def test_start_step_already_started(self) -> None:
        """Starting an already-started step raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=["review"])
        step_id = session.steps[0].step_id
        self.manager.start_step("s1", step_id)
        with self.assertRaises(ValueError):
            self.manager.start_step("s1", step_id)

    def test_start_step_nonexistent(self) -> None:
        """Starting a nonexistent step raises KeyError."""
        self.manager.create_session("s1", "Test", steps=["review"])
        with self.assertRaises(KeyError):
            self.manager.start_step("s1", "nonexistent-step-id")

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
        step = self._create_session_with_step()

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
        updated = self.manager.mark_agent_complete("s1", step.step_id)
        assert updated.state == SessionState.PENDING
        assert updated.completed_agents == 1

        # Second agent completes — now ready
        updated = self.manager.mark_agent_complete("s1", step.step_id)
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
        s1 = self.manager.create_session("s1", "Session 1", steps=[_STEP])
        step1 = self.manager.start_step("s1", s1.steps[0].step_id)
        s2 = self.manager.create_session("s2", "Session 2", steps=[_STEP])
        step2 = self.manager.start_step("s2", s2.steps[0].step_id)

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
        s1 = self.manager.create_session("s1", "Session 1", steps=[_STEP])
        step1 = self.manager.start_step("s1", s1.steps[0].step_id)
        s2 = self.manager.create_session("s2", "Session 2", steps=[_STEP])
        step2 = self.manager.start_step("s2", s2.steps[0].step_id)

        self.manager.mark_agent_complete("s1", step1.step_id)
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
        self.manager.create_session("s1", "Test")
        assert self.manager.get_session("s1") is not None
        assert (self.data_dir / "s1.json").exists()

        self.manager.cleanup_session("s1")
        assert self.manager.get_session("s1") is None
        assert not (self.data_dir / "s1.json").exists()

    def test_cleanup_removes_step_index(self) -> None:
        """Cleanup removes step_id entries from the index."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        step_id = step.step_id

        self.manager.cleanup_session("s1")
        with self.assertRaises(KeyError):
            self.manager.get_step_by_id(step_id)

    def test_cleanup_nonexistent_session(self) -> None:
        """Cleaning up a nonexistent session does not raise."""
        self.manager.cleanup_session("nonexistent")  # Should not raise

    def test_list_sessions_after_cleanup(self) -> None:
        """Cleaned up sessions don't appear in list_sessions."""
        self.manager.create_session("s1", "Session 1")
        self.manager.create_session("s2", "Session 2")
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
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        updated = self.manager.mark_agent_complete("s1", step.step_id)
        assert updated.completed_agents == 1
        assert updated.state == SessionState.PENDING

        updated = self.manager.mark_agent_complete("s1", step.step_id)
        assert updated.completed_agents == 2
        assert updated.state == SessionState.PENDING

        updated = self.manager.mark_agent_complete("s1", step.step_id)
        assert updated.completed_agents == 3
        assert updated.state == SessionState.READY

    def test_agent_complete_rejected_when_ready(self) -> None:
        """mark_agent_complete raises ValueError once step is READY."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        self.manager.mark_agent_complete("s1", step.step_id)
        with self.assertRaises(ValueError):
            self.manager.mark_agent_complete("s1", step.step_id)

    def test_invalid_session_id_raises(self) -> None:
        """Operations on a nonexistent session raise KeyError."""
        with self.assertRaises(KeyError):
            self.manager.add_finding("s1", "nonexistent-uuid", {"type": "text", "title": "Q"})

        with self.assertRaises(KeyError):
            self.manager.mark_agent_complete("s1", "nonexistent-uuid")

        with self.assertRaises(KeyError):
            self.manager.submit_responses("nonexistent", "nonexistent-step-id", [])

    def test_add_finding_rejects_ready_step(self) -> None:
        """Adding a finding to a READY step raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.mark_agent_complete("s1", step.step_id)

        with self.assertRaises(ValueError):
            self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Late"})

    def test_add_finding_rejects_completed_step(self) -> None:
        """Adding a finding to a COMPLETED step raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Q"})
        self.manager.mark_agent_complete("s1", step.step_id)
        self.manager.submit_responses("s1", step.step_id, [UserResponse(answer="A")])

        with self.assertRaises(ValueError):
            self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Too late"})

    def test_add_finding_rejects_wrong_session(self) -> None:
        """Adding a finding with a step_id from a different session raises ValueError."""
        session = self.manager.create_session("s1", "Session 1", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.create_session("s2", "Session 2")

        with self.assertRaises(ValueError, msg="Step .* belongs to session 's1', not 's2'"):
            self.manager.add_finding("s2", step.step_id, {"type": "text", "title": "Wrong session"})

    def test_step_id_unique(self) -> None:
        """Pre-created steps each have a unique step_id."""
        session = self.manager.create_session("s1", "Test", steps=["review", "review"])
        assert session.steps[0].step_id != session.steps[1].step_id


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
        session = mgr1.create_session("s1", "Persistent", steps=[_STEP])
        step = mgr1.start_step("s1", session.steps[0].step_id)
        mgr1.add_finding("s1", step.step_id, {
            "type": "text",
            "title": "Will this persist?",
        })
        mgr1.mark_agent_complete("s1", step.step_id)

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
        session = mgr1.create_session("s1", "Test", steps=[_STEP])
        step = mgr1.start_step("s1", session.steps[0].step_id)
        step_id = step.step_id

        mgr2 = SessionManager(data_dir=self.data_dir)
        session, reloaded_step = mgr2.get_step_by_id(step_id)
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
        """Pre-creating the same step name twice creates two steps with different IDs."""
        session = self.manager.create_session("s1", "Test", steps=["review", "review"])
        step1 = self.manager.start_step("s1", session.steps[0].step_id)
        step2 = self.manager.start_step("s1", session.steps[1].step_id)
        assert step1.sequence == 0
        assert step2.sequence == 1
        assert step1.step_id != step2.step_id

        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.steps) == 2

    def test_findings_go_to_correct_step_by_id(self) -> None:
        """Findings are routed to the correct step via step_id."""
        session = self.manager.create_session("s1", "Test", steps=["review", "review"])
        step1 = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.add_finding("s1", step1.step_id, {"type": "text", "title": "First"})

        step2 = self.manager.start_step("s1", session.steps[1].step_id)
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
            session = self.manager.create_session("s1", "Async Test", steps=[_STEP])
            step = self.manager.start_step("s1", session.steps[0].step_id)
            self.manager.add_finding("s1", step.step_id, {
                "type": "text",
                "title": "Async question",
            })
            self.manager.mark_agent_complete("s1", step.step_id)

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
            session = self.manager.create_session("s1", "Test", steps=[_STEP])
            step = self.manager.start_step("s1", session.steps[0].step_id)
            self.manager.add_finding("s1", step.step_id, {
                "type": "text",
                "title": "Q",
            })
            self.manager.mark_agent_complete("s1", step.step_id)
            self.manager.submit_responses("s1", step.step_id, [UserResponse(answer="A")])

            # Should return immediately since step is completed
            review = await self.manager.wait_for_review("s1", step.step_id)
            assert review.step_name == _STEP
            assert len(review.items) == 1

        asyncio.run(_run())


class TestAgentLifecycle(unittest.TestCase):
    """Test start_agent and stop_agent methods."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_start_agent_creates_running_agent(self) -> None:
        """start_agent creates an Agent in RUNNING state."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        agent = self.manager.start_agent("s1", step.step_id, "agent-1", "First agent")
        assert agent.name == "agent-1"
        assert agent.description == "First agent"
        assert agent.state.value == "running"
        assert agent.completed_at is None

    def test_stop_agent_transitions_to_completed(self) -> None:
        """stop_agent transitions agent to COMPLETED state."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.start_agent("s1", step.step_id, "agent-2")

        updated = self.manager.stop_agent("s1", step.step_id, "agent-1")
        stopped = [a for a in updated.agents if a.name == "agent-1"][0]
        assert stopped.state.value == "completed"
        assert stopped.completed_at is not None

    def test_three_agents_step_transitions(self) -> None:
        """Start 3 agents, stop 2 -> step still STARTED, stop 3rd -> step READY."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "a1")
        self.manager.start_agent("s1", step.step_id, "a2")
        self.manager.start_agent("s1", step.step_id, "a3")

        updated = self.manager.stop_agent("s1", step.step_id, "a1")
        assert updated.state == SessionState.STARTED

        updated = self.manager.stop_agent("s1", step.step_id, "a2")
        assert updated.state == SessionState.STARTED

        updated = self.manager.stop_agent("s1", step.step_id, "a3")
        assert updated.state == SessionState.READY

    def test_stop_agent_unknown_name_raises(self) -> None:
        """stop_agent raises KeyError if agent name not found."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "agent-1")

        with self.assertRaises(KeyError):
            self.manager.stop_agent("s1", step.step_id, "nonexistent")

    def test_stop_agent_twice_raises(self) -> None:
        """Stopping the same agent twice raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "agent-1")

        self.manager.stop_agent("s1", step.step_id, "agent-1")
        with self.assertRaises(ValueError):
            self.manager.stop_agent("s1", step.step_id, "agent-1")


class TestSaveResponse(unittest.TestCase):
    """Test save_response method for incremental auto-save."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_save_response_stores_at_correct_index(self) -> None:
        """Add 3 findings, save response for finding 2, verify responses[1] is set."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        f1 = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Finding 1",
        })
        f2 = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Finding 2",
        })
        f3 = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Finding 3",
        })

        response = UserResponse(answer="my answer")
        self.manager.save_response("s1", step.step_id, f2.id, response)

        _, updated_step = self.manager.get_step_by_id(step.step_id)
        assert updated_step.responses is not None
        assert len(updated_step.responses) == 3
        # Finding 2 is at index 1
        assert updated_step.responses[1].answer == "my answer"
        # Others should be empty UserResponse objects
        assert updated_step.responses[0].action is None
        assert updated_step.responses[0].answer is None
        assert updated_step.responses[2].action is None
        assert updated_step.responses[2].answer is None

    def test_save_response_overwrites_previous(self) -> None:
        """Multiple auto-saves overwrite previous values for the same finding."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Q1",
        })

        self.manager.save_response(
            "s1", step.step_id, finding.id, UserResponse(answer="first"),
        )
        self.manager.save_response(
            "s1", step.step_id, finding.id, UserResponse(answer="second"),
        )

        _, updated_step = self.manager.get_step_by_id(step.step_id)
        assert updated_step.responses is not None
        assert updated_step.responses[0].answer == "second"

    def test_save_response_lazy_init(self) -> None:
        """step.responses is initialized lazily on first save."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Q1",
        })

        # Before save, responses should be None
        _, pre_step = self.manager.get_step_by_id(step.step_id)
        assert pre_step.responses is None

        self.manager.save_response(
            "s1", step.step_id, finding.id, UserResponse(answer="hello"),
        )

        _, post_step = self.manager.get_step_by_id(step.step_id)
        assert post_step.responses is not None

    def test_save_response_does_not_change_state(self) -> None:
        """Auto-save does not change step state or set events."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Q1",
        })

        self.manager.save_response(
            "s1", step.step_id, finding.id, UserResponse(answer="test"),
        )

        _, updated_step = self.manager.get_step_by_id(step.step_id)
        assert updated_step.state == SessionState.STARTED

    def test_save_response_invalid_finding_id(self) -> None:
        """Saving a response for a nonexistent finding raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Q1",
        })

        with self.assertRaises(ValueError):
            self.manager.save_response(
                "s1", step.step_id, "nonexistent", UserResponse(answer="x"),
            )

    def test_save_response_wrong_session_id(self) -> None:
        """Saving a response with wrong session ID raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding("s1", step.step_id, {
            "type": "text", "title": "Q1",
        })

        with self.assertRaises(ValueError):
            self.manager.save_response(
                "wrong-session", step.step_id, finding.id, UserResponse(answer="x"),
            )


if __name__ == "__main__":
    unittest.main()
