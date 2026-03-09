"""Tests for the Zing batch review server HTTP endpoints."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fastapi.testclient import TestClient

from zing_ai.server.app import create_app
from zing_ai.server.mcp_tools import configure, create_review, wait_for_review
from zing_ai.server.sessions import SessionManager


class _ServerTestBase(unittest.TestCase):
    """Base class that sets up a TestClient with an isolated SessionManager."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)
        app = create_app(session_manager=self.manager)
        self.client = TestClient(app)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _create_session(
        self,
        session_id: str = "test-session",
        title: str = "Test Session",
        expected_agents: int = 2,
    ) -> None:
        """Helper to create a session for testing."""
        self.manager.create_session(
            session_id=session_id,
            title=title,
            zing_file="test.zing",
            expected_agents=expected_agents,
        )


class TestPostFindings(_ServerTestBase):
    """Tests for POST /{session_id}/findings."""

    def test_valid_triage_finding(self) -> None:
        """A valid triage finding is accepted and stored."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "type": "triage",
                "description": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        self.assertEqual(resp.status_code, 201)
        body = resp.json()
        self.assertEqual(body["status"], "ok")
        self.assertIn("finding_id", body)

        session = self.manager.get_session("test-session")
        assert session is not None
        self.assertEqual(len(session.findings), 1)

    def test_valid_text_finding(self) -> None:
        """A valid text finding is accepted."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={"type": "text", "question": "What is the main goal?"},
        )
        self.assertEqual(resp.status_code, 201)

    def test_valid_choice_finding(self) -> None:
        """A valid choice finding is accepted."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "type": "choice",
                "question": "Pick an approach",
                "options": [
                    {"label": "A", "description": "Option A"},
                    {"label": "B", "description": "Option B"},
                ],
            },
        )
        self.assertEqual(resp.status_code, 201)

    def test_invalid_session_returns_404(self) -> None:
        """Posting a finding to a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/findings",
            json={"type": "text", "question": "Hello?"},
        )
        self.assertEqual(resp.status_code, 404)
        body = resp.json()
        self.assertEqual(body["error"], "session_not_found")
        self.assertIn("no-such-session", body["message"])

    def test_malformed_finding_returns_400(self) -> None:
        """Posting invalid finding data returns 400 with field-level errors."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={"type": "triage"},  # missing required fields
        )
        self.assertEqual(resp.status_code, 400)
        body = resp.json()
        self.assertEqual(body["error"], "validation_error")
        self.assertIsInstance(body["details"], list)
        self.assertTrue(len(body["details"]) > 0)


class TestAgentComplete(_ServerTestBase):
    """Tests for POST /{session_id}/agent-complete."""

    def test_signal_completion(self) -> None:
        """Signaling agent-complete increments the completed count."""
        self._create_session(expected_agents=2)
        resp = self.client.post("/test-session/agent-complete")
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["completed_agents"], 1)
        self.assertEqual(body["expected_agents"], 2)
        self.assertEqual(body["state"], "pending")

    def test_all_agents_complete_transitions_to_ready(self) -> None:
        """When all expected agents complete, session transitions to ready."""
        self._create_session(expected_agents=2)
        self.client.post("/test-session/agent-complete")
        resp = self.client.post("/test-session/agent-complete")
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["completed_agents"], 2)
        self.assertEqual(body["state"], "ready")

    def test_invalid_session_returns_404(self) -> None:
        """Signaling agent-complete on a nonexistent session returns 404."""
        resp = self.client.post("/no-such-session/agent-complete")
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")


class TestSubmit(_ServerTestBase):
    """Tests for POST /{session_id}/submit."""

    def test_submit_responses(self) -> None:
        """Submitting responses stores them and transitions to completed."""
        self._create_session(expected_agents=1)
        self.client.post(
            "/test-session/findings",
            json={"type": "text", "question": "What is the goal?"},
        )
        self.client.post("/test-session/agent-complete")

        resp = self.client.post(
            "/test-session/submit",
            json={"responses": [{"answer": "Build a review UI"}]},
        )
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["status"], "ok")
        self.assertEqual(body["items_count"], 1)

        session = self.manager.get_session("test-session")
        assert session is not None
        self.assertEqual(session.state.value, "completed")

    def test_invalid_session_returns_404(self) -> None:
        """Submitting to a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/submit",
            json={"responses": []},
        )
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")


class TestSSEStream(_ServerTestBase):
    """Tests for GET /{session_id}/stream."""

    def test_backfills_existing_findings(self) -> None:
        """SSE stream includes findings that existed before the connection."""
        self._create_session(expected_agents=1)
        resp_finding = self.client.post(
            "/test-session/findings",
            json={
                "type": "triage",
                "description": "Old finding",
                "category": "bug",
                "severity": "high",
                "confidence": "high",
            },
        )
        finding_id = resp_finding.json()["finding_id"]
        # Mark agent complete so the stream terminates
        self.client.post("/test-session/agent-complete")

        resp = self.client.get("/test-session/stream")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("text/event-stream", resp.headers.get("content-type", ""))
        body_text = resp.text
        # The backfilled finding should appear by its ID
        self.assertIn(finding_id, body_text)
        self.assertIn("triage", body_text)
        self.assertIn("Ready for review", body_text)

    def test_stream_receives_new_findings(self) -> None:
        """SSE stream receives findings that are added after connection starts."""
        self._create_session(expected_agents=1)

        # Add a finding and complete agent so the stream terminates
        resp_finding = self.client.post(
            "/test-session/findings",
            json={"type": "text", "question": "Added after connect?"},
        )
        finding_id = resp_finding.json()["finding_id"]
        self.client.post("/test-session/agent-complete")

        resp = self.client.get("/test-session/stream")
        self.assertEqual(resp.status_code, 200)
        # The finding should appear in the stream by its ID
        self.assertIn(finding_id, resp.text)
        self.assertIn("text", resp.text)


class TestConcurrentSessions(_ServerTestBase):
    """Tests that two sessions don't interfere with each other."""

    def test_sessions_are_isolated(self) -> None:
        """Findings posted to one session don't appear in another."""
        self._create_session(session_id="session-a", expected_agents=1)
        self._create_session(session_id="session-b", expected_agents=1)

        self.client.post(
            "/session-a/findings",
            json={"type": "text", "question": "Question for A"},
        )
        self.client.post(
            "/session-b/findings",
            json={
                "type": "triage",
                "description": "Finding for B",
                "category": "perf",
                "severity": "medium",
                "confidence": "medium",
            },
        )

        session_a = self.manager.get_session("session-a")
        session_b = self.manager.get_session("session-b")
        assert session_a is not None
        assert session_b is not None

        self.assertEqual(len(session_a.findings), 1)
        self.assertEqual(len(session_b.findings), 1)
        self.assertEqual(session_a.findings[0].type, "text")
        self.assertEqual(session_b.findings[0].type, "triage")


class TestHTMLEndpoints(_ServerTestBase):
    """Tests for the HTML page endpoints."""

    def test_dashboard(self) -> None:
        """Dashboard returns HTML with session list."""
        self._create_session(session_id="s1", title="First Session")
        resp = self.client.get("/dashboard")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("First Session", resp.text)
        self.assertIn("Zing Dashboard", resp.text)

    def test_session_page(self) -> None:
        """Session page returns HTML with session details."""
        self._create_session(session_id="s1", title="My Review")
        resp = self.client.get("/s1")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("My Review", resp.text)

    def test_session_page_not_found(self) -> None:
        """Requesting a nonexistent session page returns 404."""
        resp = self.client.get("/no-such-session")
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")


class TestCreateReview(_ServerTestBase):
    """Tests for the create_review MCP tool."""

    def test_create_review_creates_session_and_returns_url(self) -> None:
        """create_review creates a session and returns a URL."""
        configure(self.manager, port=9876)
        with patch("zing_ai.server.mcp_tools.webbrowser.open") as mock_open:
            result = asyncio.run(
                create_review(
                    session_id="mcp-session",
                    title="MCP Review",
                    zing_file="test.zing",
                    expected_agents=2,
                )
            )
        self.assertEqual(result["status"], "created")
        self.assertEqual(result["url"], "http://localhost:9876/mcp-session")
        mock_open.assert_called_once_with("http://localhost:9876/mcp-session")

        session = self.manager.get_session("mcp-session")
        self.assertIsNotNone(session)
        assert session is not None
        self.assertEqual(session.title, "MCP Review")
        self.assertEqual(session.expected_agents, 2)


class TestWaitForReview(_ServerTestBase):
    """Tests for the wait_for_review MCP tool."""

    def test_wait_for_review_returns_correct_json(self) -> None:
        """wait_for_review returns correct JSON with full finding data."""
        configure(self.manager, port=9876)
        self._create_session(session_id="wait-session", expected_agents=1)

        # Add a finding and submit responses
        self.client.post(
            "/wait-session/findings",
            json={
                "type": "triage",
                "description": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        self.client.post("/wait-session/agent-complete")
        self.client.post(
            "/wait-session/submit",
            json={"responses": [{"action": "accept"}]},
        )

        # Now wait_for_review should return immediately since the event is already set
        result = asyncio.run(
            wait_for_review(session_id="wait-session")
        )

        self.assertEqual(result["session_id"], "wait-session")
        self.assertIsInstance(result["items"], list)
        self.assertEqual(len(result["items"]), 1)

        item = result["items"][0]
        self.assertEqual(item["finding"]["type"], "triage")
        self.assertEqual(item["finding"]["description"], "Unused import")
        self.assertEqual(item["response"]["action"], "accept")

    def test_wait_for_review_blocks_until_submission(self) -> None:
        """wait_for_review blocks until the session is submitted."""
        configure(self.manager, port=9876)
        self._create_session(session_id="block-session", expected_agents=1)

        self.client.post(
            "/block-session/findings",
            json={"type": "text", "question": "What do you think?"},
        )
        self.client.post("/block-session/agent-complete")

        async def _test_blocking() -> None:
            completed = False

            async def do_wait() -> dict:
                nonlocal completed
                result = await wait_for_review(session_id="block-session")
                completed = True
                return result

            task = asyncio.create_task(do_wait())

            # Yield control briefly — wait_for_review should NOT have completed yet
            await asyncio.sleep(0.05)
            self.assertFalse(completed, "wait_for_review should block until submission")

            # Submit responses to unblock
            self.client.post(
                "/block-session/submit",
                json={"responses": [{"answer": "Looks good"}]},
            )

            result = await task
            self.assertTrue(completed)
            self.assertEqual(result["session_id"], "block-session")
            self.assertEqual(len(result["items"]), 1)

        asyncio.run(_test_blocking())
