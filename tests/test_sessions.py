"""Tests for the SessionManager class."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from zing_ai.server.models import (
    ResponseAction,
    SessionState,
    UserResponse,
    WorkflowStep,
    ZingSession,
)
from zing_ai.server.sessions import SessionManager

_STEP = "review"


class TestSessionLifecycle(unittest.TestCase):
    """Test the full lifecycle: create, start step, add findings, complete, submit."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)
        self.zing_file = str(self.data_dir / "plan.md")
        Path(self.zing_file).write_text("# Plan\n", encoding="utf-8")

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _create_session_with_step(
        self,
        session_id: str = "s1",
        title: str = "Test",
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
        session = self.manager.create_session("s1", "Test", zing_file=self.zing_file)
        assert session.zing_file == self.zing_file

    def test_create_session_zing_file_relative_rejected(self) -> None:
        """Creating a session with a relative zing_file path raises ValueError."""
        with self.assertRaises(ValueError, msg="zing_file must be an absolute path"):
            self.manager.create_session("s1", "Test", zing_file="relative/path.md")

    def test_create_session_zing_file_nonexistent_rejected(self) -> None:
        """Creating a session with a non-existent absolute path raises ValueError."""
        with self.assertRaises(ValueError, msg="zing_file path does not exist"):
            self.manager.create_session("s1", "Test", zing_file="/nonexistent/path.md")

    def test_create_session_zing_file_non_markdown_rejected(self) -> None:
        """Creating a session with a non-.md zing_file raises ValueError."""
        non_md = str(self.data_dir / "plan.txt")
        Path(non_md).write_text("not markdown", encoding="utf-8")
        with self.assertRaises(ValueError, msg="zing_file must be a markdown file"):
            self.manager.create_session("s1", "Test", zing_file=non_md)

    def test_update_session(self) -> None:
        """update_session changes zing_file and title on an existing session."""
        session = self.manager.create_session("s1", "Original Title")
        assert session.title == "Original Title"
        assert session.zing_file is None

        updated = self.manager.update_session(
            "s1",
            zing_file=self.zing_file,
            title="New Title",
        )
        assert isinstance(updated, ZingSession)
        assert updated.title == "New Title"
        assert updated.zing_file == self.zing_file

        # Verify changes persisted by reloading
        reloaded = SessionManager(data_dir=self.data_dir)
        s = reloaded.get_session("s1")
        assert s is not None
        assert isinstance(s, ZingSession)
        assert s.title == "New Title"
        assert s.zing_file == self.zing_file

    def test_update_session_partial(self) -> None:
        """update_session with None parameters skips those fields."""
        self.manager.create_session("s1", "Title", zing_file=self.zing_file)

        # Update only title
        updated = self.manager.update_session("s1", title="New Title")
        assert isinstance(updated, ZingSession)
        assert updated.title == "New Title"
        assert updated.zing_file == self.zing_file

        # Update only zing_file (using setUp.py as a different valid file)
        updated2 = self.manager.update_session("s1", zing_file=self.zing_file)
        assert isinstance(updated2, ZingSession)
        assert updated2.title == "New Title"
        assert updated2.zing_file == self.zing_file

    def test_update_session_validates_zing_file(self) -> None:
        """update_session rejects relative and nonexistent zing_file paths."""
        self.manager.create_session("s1", "Title")

        with self.assertRaises(ValueError):
            self.manager.update_session("s1", zing_file="relative/path.md")

        with self.assertRaises(ValueError):
            self.manager.update_session("s1", zing_file="/nonexistent/path.md")

    def test_update_session_sets_ticket_id(self) -> None:
        """update_session persists ticket_id when provided."""
        self.manager.create_session("s1", "Title")

        updated = self.manager.update_session("s1", ticket_id="ABC-1")
        assert updated.ticket_id == "ABC-1"

        # Verify persisted by reloading from disk
        reloaded_mgr = SessionManager(data_dir=self.data_dir)
        s = reloaded_mgr.get_session("s1")
        assert s is not None
        assert s.ticket_id == "ABC-1"

    def test_update_session_ticket_id_none_preserves(self) -> None:
        """update_session with no ticket_id leaves existing value unchanged."""
        self.manager.create_session("s1", "Title")
        self.manager.update_session("s1", ticket_id="ABC-1")

        # Call update_session without ticket_id — should not clear the existing value
        self.manager.update_session("s1", title="New Title")

        reloaded_mgr = SessionManager(data_dir=self.data_dir)
        s = reloaded_mgr.get_session("s1")
        assert s is not None
        assert s.ticket_id == "ABC-1"
        assert s.title == "New Title"

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
        assert isinstance(session, ZingSession)
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
        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "What is the meaning of life?",
            },
        )
        assert finding.type == "text"
        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert session.total_findings == 1
        assert len(session.steps[0].findings) == 1

    def test_add_finding_evaluation(self) -> None:
        """Adding an evaluation finding appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
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
            },
        )
        assert finding.type == "evaluation"
        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert session.total_findings == 1

    def test_add_finding_triage_without_metadata(self) -> None:
        """Adding a triage finding without metadata appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "triage",
                "title": "Pick one",
                "options": [
                    {"label": "A", "description": "Option A"},
                    {"label": "B", "description": "Option B"},
                ],
            },
        )
        assert finding.type == "triage"

    def test_add_finding_triage(self) -> None:
        """Adding a triage finding appends it to the step."""
        step = self._create_session_with_step()
        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        assert finding.type == "triage"

    def test_duplicate_findings_are_deduplicated(self) -> None:
        """Submitting two findings with the same type and title stores only one."""
        step = self._create_session_with_step()
        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "triage",
                "title": "Improve error handling",
                "options": [
                    {"label": "Try/except", "description": "Wrap in try/except"},
                    {"label": "Result type", "description": "Use a Result type"},
                ],
            },
        )
        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "triage",
                "title": "Improve error handling",
                "options": [
                    {"label": "Try/except", "description": "Wrap in try/except"},
                    {"label": "Result type", "description": "Use a Result type"},
                ],
            },
        )
        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert len(session.steps[0].findings) == 1, (
            f"Expected 1 finding after dedup, got {len(session.steps[0].findings)}"
        )

    def test_full_lifecycle(self) -> None:
        """Full lifecycle: create → start step → add findings → agents complete → submit."""
        step = self._create_session_with_step()

        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Is this correct?",
            },
        )
        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "triage",
                "title": "Bug found",
                "category": "correctness",
                "severity": "high",
                "confidence": "medium",
            },
        )

        # Start two agents
        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.start_agent("s1", step.step_id, "agent-2")

        # First agent completes — still started (not all done)
        updated = self.manager.stop_agent("s1", step.step_id, "agent-1")
        assert updated.state == SessionState.STARTED

        # Second agent completes — step stays STARTED until explicitly marked ready
        updated = self.manager.stop_agent("s1", step.step_id, "agent-2")
        assert updated.state == SessionState.STARTED

        # Mark step ready (parent does this after submitting findings)
        self.manager.mark_step_ready("s1", step.step_id)

        # Submit responses
        responses = [
            UserResponse(answer="Yes, it is correct"),
            UserResponse(action=ResponseAction.ACCEPT),
        ]
        review = self.manager.submit_responses("s1", step.step_id, responses)
        assert review.session_id == "s1"
        assert review.step_name == _STEP
        assert len(review.items) == 2

        session = self.manager.get_session("s1")
        assert session is not None
        assert session.state == SessionState.COMPLETED

    def test_session_not_completed_until_all_steps_done(self) -> None:
        """Session state is COMPLETED only when all steps are COMPLETED."""
        session = self.manager.create_session("s1", "Multi-step", steps=["build", "audit"])
        step1 = self.manager.start_step("s1", session.steps[0].step_id)

        # Start and complete first step's agent
        self.manager.start_agent("s1", step1.step_id, "a1")
        self.manager.stop_agent("s1", step1.step_id, "a1")
        self.manager.mark_step_ready("s1", step1.step_id)
        self.manager.submit_responses("s1", step1.step_id, [])

        # Session should NOT be completed — second step is still pending
        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert session.state != SessionState.COMPLETED

        # Complete second step
        step2 = self.manager.start_step("s1", session.steps[1].step_id)
        self.manager.start_agent("s1", step2.step_id, "a1")
        self.manager.stop_agent("s1", step2.step_id, "a1")
        self.manager.mark_step_ready("s1", step2.step_id)
        self.manager.submit_responses("s1", step2.step_id, [])

        # Now session should be completed
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

        self.manager.add_finding(
            "s1",
            step1.step_id,
            {
                "type": "text",
                "title": "Question for s1",
            },
        )
        self.manager.add_finding(
            "s2",
            step2.step_id,
            {
                "type": "triage",
                "title": "Finding for s2",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
            },
        )

        s1 = self.manager.get_session("s1")
        s2 = self.manager.get_session("s2")
        assert s1 is not None
        assert s2 is not None
        assert isinstance(s1, ZingSession)
        assert isinstance(s2, ZingSession)
        assert s1.total_findings == 1
        assert s2.total_findings == 1
        assert s1.steps[0].findings[0].type == "text"
        assert s2.steps[0].findings[0].type == "triage"

    def test_agent_completion_isolated(self) -> None:
        """Completing an agent in one session doesn't affect another."""
        s1 = self.manager.create_session("s1", "Session 1", steps=[_STEP])
        step1 = self.manager.start_step("s1", s1.steps[0].step_id)
        s2 = self.manager.create_session("s2", "Session 2", steps=[_STEP])
        self.manager.start_step("s2", s2.steps[0].step_id)

        self.manager.start_agent("s1", step1.step_id, "agent-1")
        self.manager.stop_agent("s1", step1.step_id, "agent-1")
        s1 = self.manager.get_session("s1")
        s2 = self.manager.get_session("s2")
        assert s1 is not None
        assert s2 is not None
        assert isinstance(s1, ZingSession)
        assert isinstance(s2, ZingSession)
        assert s1.steps[0].state == SessionState.STARTED
        assert s2.steps[0].state == SessionState.STARTED
        assert len(s2.steps[0].agents) == 0


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
    """Test agent lifecycle tracking and edge cases."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_stop_agent_keeps_step_started(self) -> None:
        """Stopping the last agent keeps step in STARTED (parent submits findings first)."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.start_agent("s1", step.step_id, "agent-2")

        updated = self.manager.stop_agent("s1", step.step_id, "agent-1")
        assert updated.state == SessionState.STARTED

        updated = self.manager.stop_agent("s1", step.step_id, "agent-2")
        assert updated.state == SessionState.STARTED

    def test_stop_agent_rejected_when_already_completed(self) -> None:
        """stop_agent raises ValueError if agent is already completed."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.stop_agent("s1", step.step_id, "agent-1")
        with self.assertRaises(ValueError):
            self.manager.stop_agent("s1", step.step_id, "agent-1")

    def test_invalid_session_id_raises(self) -> None:
        """Operations on a nonexistent session raise KeyError."""
        with self.assertRaises(KeyError):
            self.manager.add_finding("s1", "nonexistent-uuid", {"type": "text", "title": "Q"})

        with self.assertRaises(KeyError):
            self.manager.submit_responses("nonexistent", "nonexistent-step-id", [])

    def test_add_finding_rejects_ready_step(self) -> None:
        """Adding a finding to a READY step raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.stop_agent("s1", step.step_id, "agent-1")
        self.manager.mark_step_ready("s1", step.step_id)

        with self.assertRaises(ValueError):
            self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Late"})

    def test_add_finding_rejects_completed_step(self) -> None:
        """Adding a finding to a COMPLETED step raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Q"})
        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.stop_agent("s1", step.step_id, "agent-1")
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

    def test_submit_responses_with_autosave(self) -> None:
        """Auto-save 2 of 3 responses, then submit all 3 → verify COMPLETED."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        # Add 3 findings
        f1 = self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Q1"})
        f2 = self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Q2"})
        self.manager.add_finding("s1", step.step_id, {"type": "text", "title": "Q3"})

        # Auto-save 2 of 3 responses (by finding ID)
        self.manager.save_response("s1", step.step_id, f1.id, UserResponse(answer="A1"))
        self.manager.save_response("s1", step.step_id, f2.id, UserResponse(answer="A2"))

        # Verify partial auto-save state
        _, partial_step = self.manager.get_step_by_id(step.step_id)
        assert partial_step.responses is not None
        assert len(partial_step.responses) == 3  # save_response pads the list
        assert partial_step.state != SessionState.COMPLETED

        # Submit all 3 final responses (overwrites auto-saved)
        final_responses = [
            UserResponse(answer="Final A1"),
            UserResponse(answer="Final A2"),
            UserResponse(answer="Final A3"),
        ]
        review = self.manager.submit_responses("s1", step.step_id, final_responses)

        assert review.session_id == "s1"
        assert len(review.items) == 3
        assert review.items[0].response.answer == "Final A1"
        assert review.items[1].response.answer == "Final A2"
        assert review.items[2].response.answer == "Final A3"

        # Verify step is COMPLETED
        _, completed_step = self.manager.get_step_by_id(step.step_id)
        assert completed_step.state == SessionState.COMPLETED

        session = self.manager.get_session("s1")
        assert session is not None
        assert session.state == SessionState.COMPLETED

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
        mgr1.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Will this persist?",
            },
        )
        mgr1.start_agent("s1", step.step_id, "agent-1")

        # Create a new manager pointing at the same data dir
        mgr2 = SessionManager(data_dir=self.data_dir)
        session = mgr2.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert session.title == "Persistent"
        assert session.total_findings == 1
        assert len(session.steps[0].agents) == 1
        assert session.steps[0].state == SessionState.STARTED
        assert session.state == SessionState.STARTED

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
        assert isinstance(session, ZingSession)
        assert len(session.steps) == 1
        assert session.steps[0].step_name == "review"
        assert len(session.steps[0].findings) == 1
        # Legacy counter fields are stripped by migration; agents list is empty
        assert session.steps[0].agents == []
        assert session.state == SessionState.PENDING

    def test_ticket_id_survives_persistence(self) -> None:
        """ticket_id is preserved after persist/reload and defaults to None."""
        # Default is None when not set
        mgr1 = SessionManager(data_dir=self.data_dir)
        session = mgr1.create_session("s2", "No Ticket", steps=[_STEP])
        assert session.ticket_id is None

        # Set ticket_id, persist, reload
        mgr2 = SessionManager(data_dir=self.data_dir)
        session2 = mgr2.create_session("s3", "With Ticket", steps=[_STEP])
        session2.ticket_id = "TEST-1"
        mgr2._persist(session2)

        mgr3 = SessionManager(data_dir=self.data_dir)
        reloaded = mgr3.get_session("s3")
        assert reloaded is not None
        assert reloaded.ticket_id == "TEST-1"


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
        assert isinstance(session, ZingSession)
        assert len(session.steps) == 2

    def test_start_step_auto_completes_prior_steps(self) -> None:
        """Starting a new step auto-completes any prior in-progress steps."""
        session = self.manager.create_session("s1", "Test", steps=["build", "audit"])
        self.manager.start_step("s1", session.steps[0].step_id)

        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert session.steps[0].state.value == "started"

        self.manager.start_step("s1", session.steps[1].step_id)

        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert session.steps[0].state.value == "completed"
        assert session.steps[1].state.value == "started"

    def test_findings_go_to_correct_step_by_id(self) -> None:
        """Findings are routed to the correct step via step_id."""
        session = self.manager.create_session("s1", "Test", steps=["review", "review"])
        step1 = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.add_finding("s1", step1.step_id, {"type": "text", "title": "First"})

        step2 = self.manager.start_step("s1", session.steps[1].step_id)
        self.manager.add_finding("s1", step2.step_id, {"type": "text", "title": "Second"})

        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        assert len(session.steps[0].findings) == 1
        assert len(session.steps[1].findings) == 1
        assert session.steps[0].findings[0].title == "First"
        assert session.steps[1].findings[0].title == "Second"


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
            self.manager.add_finding(
                "s1",
                step.step_id,
                {
                    "type": "text",
                    "title": "Async question",
                },
            )
            self.manager.start_agent("s1", step.step_id, "agent-1")
            self.manager.stop_agent("s1", step.step_id, "agent-1")

            async def _submit_later() -> None:
                await asyncio.sleep(0.05)
                self.manager.submit_responses(
                    "s1",
                    step.step_id,
                    [
                        UserResponse(answer="async answer"),
                    ],
                )

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
            self.manager.add_finding(
                "s1",
                step.step_id,
                {
                    "type": "text",
                    "title": "Q",
                },
            )
            self.manager.start_agent("s1", step.step_id, "agent-1")
            self.manager.stop_agent("s1", step.step_id, "agent-1")
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

    def test_three_agents_step_stays_started(self) -> None:
        """Start 3 agents, stop all 3 -> step still STARTED until mark_step_ready."""
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
        assert updated.state == SessionState.STARTED

        updated = self.manager.mark_step_ready("s1", step.step_id)
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

        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Finding 1",
            },
        )
        f2 = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Finding 2",
            },
        )
        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Finding 3",
            },
        )

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

        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Q1",
            },
        )

        self.manager.save_response(
            "s1",
            step.step_id,
            finding.id,
            UserResponse(answer="first"),
        )
        self.manager.save_response(
            "s1",
            step.step_id,
            finding.id,
            UserResponse(answer="second"),
        )

        _, updated_step = self.manager.get_step_by_id(step.step_id)
        assert updated_step.responses is not None
        assert updated_step.responses[0].answer == "second"

    def test_save_response_lazy_init(self) -> None:
        """step.responses is initialized lazily on first save."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Q1",
            },
        )

        # Before save, responses should be None
        _, pre_step = self.manager.get_step_by_id(step.step_id)
        assert pre_step.responses is None

        self.manager.save_response(
            "s1",
            step.step_id,
            finding.id,
            UserResponse(answer="hello"),
        )

        _, post_step = self.manager.get_step_by_id(step.step_id)
        assert post_step.responses is not None

    def test_save_response_does_not_change_state(self) -> None:
        """Auto-save does not change step state or set events."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Q1",
            },
        )

        self.manager.save_response(
            "s1",
            step.step_id,
            finding.id,
            UserResponse(answer="test"),
        )

        _, updated_step = self.manager.get_step_by_id(step.step_id)
        assert updated_step.state == SessionState.STARTED

    def test_save_response_invalid_finding_id(self) -> None:
        """Saving a response for a nonexistent finding raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Q1",
            },
        )

        with self.assertRaises(ValueError):
            self.manager.save_response(
                "s1",
                step.step_id,
                "nonexistent",
                UserResponse(answer="x"),
            )

    def test_save_response_wrong_session_id(self) -> None:
        """Saving a response with wrong session ID raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        finding = self.manager.add_finding(
            "s1",
            step.step_id,
            {
                "type": "text",
                "title": "Q1",
            },
        )

        with self.assertRaises(ValueError):
            self.manager.save_response(
                "wrong-session",
                step.step_id,
                finding.id,
                UserResponse(answer="x"),
            )


class TestAddLog(unittest.TestCase):
    """Tests for SessionManager.add_log."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_add_three_log_entries(self) -> None:
        """Add 3 log entries and verify step.logs has 3 entries with correct data."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)

        self.manager.add_log("s1", step.step_id, "agent-a", "Starting analysis")
        self.manager.add_log("s1", step.step_id, "agent-b", "Found 3 issues")
        entry3 = self.manager.add_log("s1", step.step_id, "agent-a", "Analysis complete")

        updated_session = self.manager.get_session("s1")
        assert updated_session is not None
        assert isinstance(updated_session, ZingSession)
        updated_step = updated_session.steps[0]
        assert len(updated_step.logs) == 3

        assert updated_step.logs[0].agent_name == "agent-a"
        assert updated_step.logs[0].message == "Starting analysis"
        assert updated_step.logs[0].timestamp is not None

        assert updated_step.logs[1].agent_name == "agent-b"
        assert updated_step.logs[1].message == "Found 3 issues"
        assert updated_step.logs[1].timestamp is not None

        assert updated_step.logs[2].agent_name == "agent-a"
        assert updated_step.logs[2].message == "Analysis complete"

        # Verify the return value matches
        assert entry3.agent_name == "agent-a"
        assert entry3.message == "Analysis complete"

    def test_add_log_wrong_session_raises(self) -> None:
        """Adding a log with wrong session_id raises ValueError."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        with self.assertRaises(ValueError):
            self.manager.add_log("wrong-session", step.step_id, "agent", "msg")

    def test_add_log_unknown_step_raises(self) -> None:
        """Adding a log with unknown step_id raises KeyError."""
        self.manager.create_session("s1", "Test", steps=[_STEP])
        with self.assertRaises(KeyError):
            self.manager.add_log("s1", "nonexistent-step-id", "agent", "msg")


class TestNotifications(unittest.TestCase):
    """Tests for notification storage, auto-generation, persistence, and events."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_add_notification_stores_on_session(self) -> None:
        """add_notification() appends a Notification to session.notifications."""
        self.manager.create_session("s1", "Test")
        notif = self.manager.add_notification("s1", "Alert", body="Details", url="/s1")
        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        # create_session adds one auto-notification, add_notification adds another
        assert len(session.notifications) == 2
        assert session.notifications[-1].title == "Alert"
        assert session.notifications[-1].body == "Details"
        assert session.notifications[-1].url == "/s1"
        assert session.notifications[-1].id == notif.id

    def test_add_notification_persists_to_disk(self) -> None:
        """Notifications added via add_notification() survive a manager restart."""
        self.manager.create_session("s1", "Test")
        self.manager.add_notification("s1", "Persisted alert")

        mgr2 = SessionManager(data_dir=self.data_dir)
        session = mgr2.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        titles = [n.title for n in session.notifications]
        assert "Persisted alert" in titles

    def test_create_session_auto_notification(self) -> None:
        """create_session() auto-creates a 'New session: {title}' notification."""
        session = self.manager.create_session("s1", "My Review")
        assert len(session.notifications) == 1
        assert session.notifications[0].title == "New session: My Review"

    def test_mark_step_ready_auto_notification(self) -> None:
        """mark_step_ready() auto-creates a 'Review ready: {step_name}' notification."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.stop_agent("s1", step.step_id, "agent-1")
        self.manager.mark_step_ready("s1", step.step_id)

        session = self.manager.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        titles = [n.title for n in session.notifications]
        assert f"Review ready: {_STEP}" in titles

    def test_notifications_survive_persistence_roundtrip(self) -> None:
        """Notifications from both auto-generation and add_notification survive save/reload."""
        session = self.manager.create_session("s1", "Test", steps=[_STEP])
        step = self.manager.start_step("s1", session.steps[0].step_id)
        self.manager.start_agent("s1", step.step_id, "agent-1")
        self.manager.stop_agent("s1", step.step_id, "agent-1")
        self.manager.mark_step_ready("s1", step.step_id)
        self.manager.add_notification("s1", "Custom alert")

        mgr2 = SessionManager(data_dir=self.data_dir)
        session = mgr2.get_session("s1")
        assert session is not None
        assert isinstance(session, ZingSession)
        titles = [n.title for n in session.notifications]
        assert "New session: Test" in titles
        assert f"Review ready: {_STEP}" in titles
        assert "Custom alert" in titles
        assert len(session.notifications) == 3

    def test_notification_added_event_emitted(self) -> None:
        """add_notification() emits a 'notification_added:{id}' event to listeners."""
        self.manager.create_session("s1", "Test")
        events: list[tuple[str, str]] = []
        self.manager.add_listener(lambda et, sid: events.append((et, sid)))

        notif = self.manager.add_notification("s1", "Alert")
        matching = [
            (et, sid)
            for et, sid in events
            if et == f"notification_added:{notif.id}" and sid == "s1"
        ]
        assert len(matching) == 1


class TestClaudeCodeSession(unittest.TestCase):
    """Tests for ClaudeCodeSession creation and persistence."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_create_claude_code_session(self) -> None:
        """Creating a ClaudeCodeSession returns correct fields."""
        from zing_ai.server.models import ClaudeCodeSession

        session = self.manager.create_claude_code_session(
            session_id="cc-1",
            title="PR #42 Review",
            ticket_id="BAK-123",
            worktree_path="/tmp/worktree",
            skill="pr-audit",
            pr_number=42,
            pr_repo="acme/repo",
        )
        assert isinstance(session, ClaudeCodeSession)
        assert session.session_id == "cc-1"
        assert session.title == "PR #42 Review"
        assert session.ticket_id == "BAK-123"
        assert session.worktree_path == "/tmp/worktree"
        assert session.skill == "pr-audit"
        assert session.pr_number == 42
        assert session.pr_repo == "acme/repo"
        assert session.state == SessionState.STARTED

    def test_create_duplicate_raises(self) -> None:
        """Duplicate session_id raises ValueError."""
        self.manager.create_claude_code_session(session_id="cc-dup", title="First")
        with self.assertRaises(ValueError, msg="Session already exists"):
            self.manager.create_claude_code_session(session_id="cc-dup", title="Second")

    def test_invalid_session_id_raises(self) -> None:
        """Invalid session_id is rejected."""
        with self.assertRaises(ValueError):
            self.manager.create_claude_code_session(session_id="../bad", title="Bad")

    def test_persisted_and_reloaded(self) -> None:
        """ClaudeCodeSession survives round-trip through disk persistence."""
        from zing_ai.server.models import ClaudeCodeSession

        self.manager.create_claude_code_session(
            session_id="cc-persist",
            title="Persist Test",
            ticket_id="FRO-99",
            pr_number=99,
            pr_repo="acme/frontend",
        )
        # Create a new manager that loads from the same directory
        manager2 = SessionManager(data_dir=self.data_dir)
        sessions = manager2.list_sessions()
        matching = [s for s in sessions if s.session_id == "cc-persist"]
        assert len(matching) == 1
        loaded = matching[0]
        assert isinstance(loaded, ClaudeCodeSession)
        assert loaded.title == "Persist Test"
        assert loaded.ticket_id == "FRO-99"
        assert loaded.pr_number == 99
        assert loaded.pr_repo == "acme/frontend"

    def test_listed_in_list_sessions(self) -> None:
        """ClaudeCodeSession appears in list_sessions()."""
        self.manager.create_claude_code_session(session_id="cc-list", title="List Test")
        sessions = self.manager.list_sessions()
        ids = [s.session_id for s in sessions]
        assert "cc-list" in ids


if __name__ == "__main__":
    unittest.main()
