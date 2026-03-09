"""Tests for the SessionManager class."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from zing_ai.server.models import SessionState, UserResponse
from zing_ai.server.sessions import SessionManager


class TestSessionLifecycle(unittest.TestCase):
    """Test the full session lifecycle: create → add findings → agent complete → submit."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_create_session(self) -> None:
        """Creating a session sets initial state to PENDING."""
        session = self.manager.create_session(
            session_id="s1",
            title="Test Session",
            zing_file="test.zing",
            expected_agents=2,
        )
        assert session.session_id == "s1"
        assert session.state == SessionState.PENDING
        assert session.expected_agents == 2
        assert session.completed_agents == 0
        assert session.findings == []
        assert session.responses is None

    def test_add_finding_text(self) -> None:
        """Adding a text finding appends it to the session."""
        self.manager.create_session("s1", "Test", "test.zing", 1)
        finding = self.manager.add_finding("s1", {
            "type": "text",
            "title": "What is the meaning of life?",
        })
        assert finding.type == "text"
        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.findings) == 1

    def test_add_finding_evaluation(self) -> None:
        """Adding an evaluation finding appends it to the session."""
        self.manager.create_session("s1", "Test", "test.zing", 1)
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
        })
        assert finding.type == "evaluation"
        session = self.manager.get_session("s1")
        assert session is not None
        assert len(session.findings) == 1

    def test_add_finding_choice(self) -> None:
        """Adding a choice finding appends it to the session."""
        self.manager.create_session("s1", "Test", "test.zing", 1)
        finding = self.manager.add_finding("s1", {
            "type": "choice",
            "title": "Pick one",
            "options": [
                {"label": "A", "description": "Option A"},
                {"label": "B", "description": "Option B"},
            ],
        })
        assert finding.type == "choice"

    def test_add_finding_triage(self) -> None:
        """Adding a triage finding appends it to the session."""
        self.manager.create_session("s1", "Test", "test.zing", 1)
        finding = self.manager.add_finding("s1", {
            "type": "triage",
            "title": "Unused import",
            "category": "style",
            "severity": "low",
            "confidence": "high",
        })
        assert finding.type == "triage"

    def test_full_lifecycle(self) -> None:
        """Full lifecycle: create → add findings → agents complete → submit."""
        self.manager.create_session("s1", "Review", "test.zing", 2)

        self.manager.add_finding("s1", {
            "type": "text",
            "title": "Is this correct?",
        })
        self.manager.add_finding("s1", {
            "type": "triage",
            "title": "Bug found",
            "category": "correctness",
            "severity": "high",
            "confidence": "medium",
        })

        # First agent completes — still pending
        session = self.manager.mark_agent_complete("s1")
        assert session.state == SessionState.PENDING
        assert session.completed_agents == 1

        # Second agent completes — now ready
        session = self.manager.mark_agent_complete("s1")
        assert session.state == SessionState.READY
        assert session.completed_agents == 2

        # Submit responses
        responses = [
            UserResponse(answer="Yes, it is correct"),
            UserResponse(action="accept"),
        ]
        review = self.manager.submit_responses("s1", responses)
        assert review.session_id == "s1"
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
        self.manager.create_session("s1", "Session 1", "a.zing", 1)
        self.manager.create_session("s2", "Session 2", "b.zing", 1)

        self.manager.add_finding("s1", {
            "type": "text",
            "title": "Question for s1",
        })
        self.manager.add_finding("s2", {
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
        assert len(s1.findings) == 1
        assert len(s2.findings) == 1
        assert s1.findings[0].type == "text"  # type: ignore[union-attr]
        assert s2.findings[0].type == "triage"  # type: ignore[union-attr]

    def test_agent_completion_isolated(self) -> None:
        """Completing an agent in one session doesn't affect another."""
        self.manager.create_session("s1", "Session 1", "a.zing", 1)
        self.manager.create_session("s2", "Session 2", "b.zing", 2)

        self.manager.mark_agent_complete("s1")
        s1 = self.manager.get_session("s1")
        s2 = self.manager.get_session("s2")
        assert s1 is not None
        assert s2 is not None
        assert s1.state == SessionState.READY
        assert s2.state == SessionState.PENDING
        assert s2.completed_agents == 0


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
        self.manager.create_session("s1", "Test", "test.zing", 1)
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
        self.manager.create_session("s1", "Session 1", "a.zing", 1)
        self.manager.create_session("s2", "Session 2", "b.zing", 1)
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
        self.manager.create_session("s1", "Test", "test.zing", 3)

        session = self.manager.mark_agent_complete("s1")
        assert session.completed_agents == 1
        assert session.state == SessionState.PENDING

        session = self.manager.mark_agent_complete("s1")
        assert session.completed_agents == 2
        assert session.state == SessionState.PENDING

        session = self.manager.mark_agent_complete("s1")
        assert session.completed_agents == 3
        assert session.state == SessionState.READY

    def test_duplicate_agent_complete_beyond_expected(self) -> None:
        """Extra mark_agent_complete calls beyond expected still work (idempotent READY)."""
        self.manager.create_session("s1", "Test", "test.zing", 1)

        self.manager.mark_agent_complete("s1")
        session = self.manager.mark_agent_complete("s1")
        assert session.completed_agents == 2
        assert session.state == SessionState.READY

    def test_invalid_session_id_raises(self) -> None:
        """Operations on a nonexistent session raise KeyError."""
        with self.assertRaises(KeyError):
            self.manager.add_finding("nonexistent", {"type": "text", "title": "Q"})

        with self.assertRaises(KeyError):
            self.manager.mark_agent_complete("nonexistent")

        with self.assertRaises(KeyError):
            self.manager.submit_responses("nonexistent", [])


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
        mgr1.create_session("s1", "Persistent", "test.zing", 2)
        mgr1.add_finding("s1", {
            "type": "text",
            "title": "Will this persist?",
        })
        mgr1.mark_agent_complete("s1")

        # Create a new manager pointing at the same data dir
        mgr2 = SessionManager(data_dir=self.data_dir)
        session = mgr2.get_session("s1")
        assert session is not None
        assert session.title == "Persistent"
        assert len(session.findings) == 1
        assert session.completed_agents == 1
        assert session.state == SessionState.PENDING


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
            self.manager.create_session("s1", "Async Test", "test.zing", 1)
            self.manager.add_finding("s1", {
                "type": "text",
                "title": "Async question",
            })
            self.manager.mark_agent_complete("s1")

            async def _submit_later() -> None:
                await asyncio.sleep(0.05)
                self.manager.submit_responses("s1", [
                    UserResponse(answer="async answer"),
                ])

            submit_task = asyncio.create_task(_submit_later())
            review = await self.manager.wait_for_review("s1")
            await submit_task

            assert review.session_id == "s1"
            assert len(review.items) == 1
            assert review.items[0].response.answer == "async answer"

        asyncio.run(_run())


if __name__ == "__main__":
    unittest.main()
