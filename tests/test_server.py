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
from zing_ai.server.models import (
    ChoiceFinding,
    ChoiceOption,
    TextFinding,
    TriageFinding,
)
from zing_ai.server.routes import finding_fragment
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
        self.assertEqual(resp.status_code, 200)
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
            json={"type": "text", "title": "What is the main goal?"},
        )
        self.assertEqual(resp.status_code, 200)

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
        self.assertEqual(resp.status_code, 200)

    def test_invalid_session_returns_404(self) -> None:
        """Posting a finding to a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/findings",
            json={"type": "text", "title": "Hello?"},
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
            json={"type": "text", "title": "What is the goal?"},
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

    def test_submit_datastar_signals(self) -> None:
        """Submitting Datastar signals maps finding IDs to responses."""
        self._create_session(expected_agents=1)
        # Add findings of each type
        r1 = self.client.post(
            "/test-session/findings",
            json={"type": "text", "title": "What do you think?"},
        )
        r2 = self.client.post(
            "/test-session/findings",
            json={
                "type": "triage",
                "description": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        r3 = self.client.post(
            "/test-session/findings",
            json={
                "type": "choice",
                "question": "Pick one",
                "options": [
                    {"label": "A", "description": "Option A"},
                    {"label": "B", "description": "Option B"},
                ],
            },
        )
        self.client.post("/test-session/agent-complete")

        text_id = r1.json()["finding_id"]
        triage_id = r2.json()["finding_id"]
        choice_id = r3.json()["finding_id"]

        # Submit as Datastar signals (responses is a dict)
        resp = self.client.post(
            "/test-session/submit",
            json={
                "responses": {
                    text_id: "Looks good",
                    triage_id: "accept",
                    choice_id: "A",
                }
            },
        )
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["items_count"], 3)

        session = self.manager.get_session("test-session")
        assert session is not None
        self.assertEqual(session.state.value, "completed")
        assert session.responses is not None
        self.assertEqual(session.responses[0].answer, "Looks good")
        self.assertEqual(session.responses[1].action, "accept")
        self.assertEqual(session.responses[2].selected, "A")

    def test_submit_returns_400_for_invalid_responses(self) -> None:
        """Submitting non-list non-dict responses returns 400."""
        self._create_session(expected_agents=1)
        self.client.post("/test-session/agent-complete")
        resp = self.client.post(
            "/test-session/submit",
            json={"responses": "invalid"},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "invalid_responses")

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
        self.assertIn("Old finding", body_text)
        self.assertIn("ready for review", body_text.lower())

    def test_stream_receives_new_findings(self) -> None:
        """SSE stream receives findings that are added after connection starts."""
        self._create_session(expected_agents=1)

        # Add a finding and complete agent so the stream terminates
        resp_finding = self.client.post(
            "/test-session/findings",
            json={"type": "text", "title": "Added after connect?"},
        )
        finding_id = resp_finding.json()["finding_id"]
        self.client.post("/test-session/agent-complete")

        resp = self.client.get("/test-session/stream")
        self.assertEqual(resp.status_code, 200)
        # The finding should appear in the stream by its ID
        self.assertIn(finding_id, resp.text)
        self.assertIn("Added after connect?", resp.text)  # title appears in stream

    def test_stream_shows_submit_button_when_ready(self) -> None:
        """SSE stream sends submit button HTML when all agents complete."""
        self._create_session(expected_agents=1)
        self.client.post(
            "/test-session/findings",
            json={"type": "text", "title": "Quick question"},
        )
        self.client.post("/test-session/agent-complete")

        resp = self.client.get("/test-session/stream")
        self.assertEqual(resp.status_code, 200)
        # Submit button should be present
        self.assertIn("Submit Review", resp.text)
        self.assertIn("@post(", resp.text)

    def test_submit_unblocks_wait_for_review(self) -> None:
        """Submit endpoint collects responses and unblocks wait_for_review."""
        configure(self.manager, port=9876)
        self._create_session(session_id="unblock-session", expected_agents=1)
        self.client.post(
            "/unblock-session/findings",
            json={
                "type": "triage",
                "description": "Test finding",
                "category": "bug",
                "severity": "high",
                "confidence": "high",
            },
        )
        self.client.post("/unblock-session/agent-complete")

        # Submit via Datastar signals
        finding_id = self.manager.get_session("unblock-session").findings[0].id  # type: ignore[union-attr]
        self.client.post(
            "/unblock-session/submit",
            json={"responses": {finding_id: "accept"}},
        )

        # wait_for_review should return immediately since session is completed
        result = asyncio.run(wait_for_review(session_id="unblock-session"))
        self.assertEqual(result["session_id"], "unblock-session")
        self.assertEqual(len(result["items"]), 1)
        self.assertEqual(result["items"][0]["finding"]["type"], "triage")
        self.assertEqual(result["items"][0]["response"]["action"], "accept")


class TestFindingFragment(unittest.TestCase):
    """Tests for the finding_fragment() template renderer."""

    def test_text_finding_renders_textarea(self) -> None:
        """Text finding renders with a textarea and data-bind."""
        finding = TextFinding(id="txt1", title="What do you think?", body="Some **markdown**")
        html = finding_fragment(finding)
        self.assertIn("finding-txt1", html)
        self.assertIn("<textarea", html)
        self.assertIn('data-bind="responses.txt1"', html)
        self.assertIn("What do you think?", html)

    def test_text_finding_with_context(self) -> None:
        """Text finding renders context when provided."""
        finding = TextFinding(id="txt2", title="Your thoughts?", context="Some context")
        html = finding_fragment(finding)
        self.assertIn("Some context", html)

    def test_text_finding_with_markdown_body(self) -> None:
        """Text finding with markdown body renders HTML in finding-body div."""
        finding = TextFinding(
            id="txt3",
            title="Architecture review",
            body="Using **bold** and `inline code` in the body.",
        )
        html = finding_fragment(finding)
        self.assertIn("finding-body", html)
        self.assertIn("<strong>bold</strong>", html)
        self.assertIn("<code>inline code</code>", html)

    def test_choice_finding_renders_radio_buttons(self) -> None:
        """Choice finding renders radio buttons with data-bind for each option."""
        finding = ChoiceFinding(
            id="ch1",
            question="Pick one",
            options=[
                ChoiceOption(label="A", description="Option A"),
                ChoiceOption(label="B", description="Option B"),
            ],
        )
        html = finding_fragment(finding)
        self.assertIn("finding-ch1", html)
        self.assertIn('type="radio"', html)
        self.assertIn('data-bind="responses.ch1"', html)
        self.assertIn("Option A", html)
        self.assertIn("Option B", html)
        # Should have a skip option
        self.assertIn('value="skip"', html)

    def test_triage_finding_renders_action_buttons(self) -> None:
        """Triage finding renders action buttons with data-bind."""
        finding = TriageFinding(
            id="tri1",
            description="Unused import os",
            category="style",
            severity="low",
            confidence="high",
            location="src/main.py:5",
        )
        html = finding_fragment(finding)
        self.assertIn("finding-tri1", html)
        self.assertIn("Unused import os", html)
        self.assertIn("src/main.py:5", html)
        self.assertIn("low", html)
        self.assertIn("high", html)
        # Action buttons
        self.assertIn("accept", html)
        self.assertIn("drop", html)
        self.assertIn("downgrade", html)
        self.assertIn("discuss", html)
        self.assertIn("data-class-selected=\"$responses.tri1 === 'accept'\"", html)


class TestMarkdownFilter(unittest.TestCase):
    """Tests for the _render_markdown Jinja2 filter."""

    def test_renders_basic_markdown(self) -> None:
        """Basic markdown with bold and italic renders to HTML tags."""
        from zing_ai.server.templates import _render_markdown

        result = _render_markdown("**bold** and *italic*")
        self.assertIn("<strong>", result)
        self.assertIn("<em>", result)

    def test_renders_code_blocks(self) -> None:
        """Fenced code blocks render with Pygments syntax highlighting."""
        from zing_ai.server.templates import _render_markdown

        md = "```python\ndef hello():\n    return 42\n```"
        result = _render_markdown(md)
        self.assertIn('<div class="highlight">', result)
        self.assertIn("<span", result)

    def test_empty_input(self) -> None:
        """Empty string input returns empty string without error."""
        from zing_ai.server.templates import _render_markdown

        result = _render_markdown("")
        self.assertEqual(result, "")

    def test_returns_markup(self) -> None:
        """Return type is markupsafe.Markup, not plain str."""
        import markupsafe

        from zing_ai.server.templates import _render_markdown

        result = _render_markdown("hello")
        self.assertIsInstance(result, markupsafe.Markup)

    def test_fallback_on_render_error(self) -> None:
        """If mistune raises, filter falls back to escaped text in <pre> tags."""
        from zing_ai.server.templates import _render_markdown

        with patch("zing_ai.server.templates._markdown", side_effect=Exception("boom")):
            result = _render_markdown("<script>alert('xss')</script>")
        self.assertIn("<pre>", result)
        self.assertIn("&lt;script&gt;", result)
        self.assertNotIn("<script>", result)


class TestConcurrentSessions(_ServerTestBase):
    """Tests that two sessions don't interfere with each other."""

    def test_sessions_are_isolated(self) -> None:
        """Findings posted to one session don't appear in another."""
        self._create_session(session_id="session-a", expected_agents=1)
        self._create_session(session_id="session-b", expected_agents=1)

        self.client.post(
            "/session-a/findings",
            json={"type": "text", "title": "Question for A"},
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
        """Dashboard returns HTML listing all sessions with correct status badges."""
        self._create_session(session_id="s1", title="First Session")
        self._create_session(session_id="s2", title="Second Session", expected_agents=1)
        # Complete the second session so it gets a different status badge
        self.client.post(
            "/s2/findings",
            json={"type": "text", "title": "How is it?"},
        )
        self.client.post("/s2/agent-complete")
        resp = self.client.get("/dashboard")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("First Session", resp.text)
        self.assertIn("Second Session", resp.text)
        self.assertIn("Zing Dashboard", resp.text)
        self.assertIn("status-pending", resp.text)
        self.assertIn("status-ready", resp.text)

    def test_session_page(self) -> None:
        """Session page returns HTML with session details."""
        self._create_session(session_id="s1", title="My Review")
        resp = self.client.get("/s1")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("My Review", resp.text)

    def test_review_page_has_datastar_script(self) -> None:
        """Review page includes Datastar CDN script tag."""
        self._create_session(session_id="s1", title="Review Test")
        resp = self.client.get("/s1")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("datastar", resp.text)
        self.assertIn("<script", resp.text)

    def test_review_page_has_sse_connection(self) -> None:
        """Review page has data-on-load with @get for SSE connection."""
        self._create_session(session_id="s1", title="SSE Test")
        resp = self.client.get("/s1")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("data-on-load", resp.text)
        self.assertIn("@get(", resp.text)
        self.assertIn("/s1/stream", resp.text)

    def test_session_page_not_found(self) -> None:
        """Requesting a nonexistent session page returns 404."""
        resp = self.client.get("/no-such-session")
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")


class TestCleanup(_ServerTestBase):
    """Tests for the POST /sessions/{session_id}/cleanup endpoint."""

    def test_cleanup_removes_session(self) -> None:
        """POST cleanup removes session from manager and returns 200."""
        self._create_session(session_id="s1", title="To Clean")
        resp = self.client.post("/sessions/s1/cleanup")
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.json()["status"], "ok")
        self.assertIsNone(self.manager.get_session("s1"))

    def test_cleanup_returns_404_for_invalid_session(self) -> None:
        """POST cleanup returns 404 for nonexistent session."""
        resp = self.client.post("/sessions/nonexistent/cleanup")
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")



class TestDashboardSSE(_ServerTestBase):
    """Tests for the dashboard SSE notification mechanism."""

    def test_dashboard_notified_on_agent_complete(self) -> None:
        """Dashboard SSE queues receive events when agent completes."""
        from zing_ai.server.routes import _dashboard_queues

        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(
                session_id="sse-dash", title="SSE Dashboard Test", expected_agents=1,
            )
            self.client.post(
                "/sse-dash/findings",
                json={"type": "text", "title": "Test?"},
            )
            self.client.post("/sse-dash/agent-complete")
            # The agent_complete notification should be in the queue
            event = queue.get_nowait()
            self.assertEqual(event, "agent_complete")
        finally:
            _dashboard_queues.remove(queue)

    def test_dashboard_notified_on_review_submitted(self) -> None:
        """Dashboard SSE queues receive events when review is submitted."""
        from zing_ai.server.routes import _dashboard_queues

        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(
                session_id="sse-sub", title="Submit Test", expected_agents=1,
            )
            self.client.post(
                "/sse-sub/findings",
                json={"type": "text", "title": "How?"},
            )
            self.client.post("/sse-sub/agent-complete")
            # Drain the agent_complete event
            queue.get_nowait()

            self.client.post(
                "/sse-sub/submit",
                json={"responses": [{"answer": "Fine"}]},
            )
            event = queue.get_nowait()
            self.assertEqual(event, "review_submitted")
        finally:
            _dashboard_queues.remove(queue)

    def test_dashboard_notified_on_cleanup(self) -> None:
        """Dashboard SSE queues receive events when session is cleaned up."""
        from zing_ai.server.routes import _dashboard_queues

        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(session_id="sse-clean", title="Cleanup Test")
            self.client.post("/sessions/sse-clean/cleanup")
            event = queue.get_nowait()
            self.assertEqual(event, "cleaned_up")
        finally:
            _dashboard_queues.remove(queue)


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
            json={"type": "text", "title": "What do you think?"},
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
