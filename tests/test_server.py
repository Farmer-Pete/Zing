"""Tests for the Zing batch review server HTTP endpoints."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fastapi.testclient import TestClient

from zing_ai.server.app import create_app
from zing_ai.server.mcp_tools import (
    configure,
    create_review,
    mark_agent_complete,
    start_step,
    submit_finding,
    wait_for_review,
)
from zing_ai.server.models import (
    ChoiceFinding,
    ChoiceOption,
    CriterionRating,
    EvaluationFinding,
    LitmusTest,
    Location,
    TextFinding,
    TriageFinding,
    WarningSign,
)
from zing_ai.server.routes import finding_fragment
from zing_ai.server.sessions import SessionManager

_STEP = "review"


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
        """Helper to create a session with a default workflow step for testing."""
        self.manager.create_session(
            session_id=session_id,
            title=title,
            zing_file=None,
        )
        step = self.manager.start_step(session_id, _STEP, expected_agents)
        self.step_id = step.step_id


class TestPostFindings(_ServerTestBase):
    """Tests for POST /{session_id}/findings."""

    def test_valid_triage_finding(self) -> None:
        """A valid triage finding is accepted and stored."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Unused import",
                "body": "The `os` module is imported but never used.",
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
        self.assertEqual(session.total_findings, 1)

    def test_valid_text_finding(self) -> None:
        """A valid text finding is accepted."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "What is the main goal?"},
        )
        self.assertEqual(resp.status_code, 200)

    def test_valid_choice_finding(self) -> None:
        """A valid choice finding is accepted."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "choice",
                "title": "Pick an approach",
                "options": [
                    {"label": "A", "description": "Option A"},
                    {"label": "B", "description": "Option B"},
                ],
            },
        )
        self.assertEqual(resp.status_code, 200)

    def test_missing_step_id_returns_400(self) -> None:
        """Posting a finding without step_id returns 400."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={"type": "text", "title": "No step id"},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "missing_step_id")

    def test_invalid_session_returns_404(self) -> None:
        """Posting a finding to a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/findings",
            json={"step_id": "nonexistent-uuid", "type": "text", "title": "Hello?"},
        )
        self.assertEqual(resp.status_code, 404)
        body = resp.json()
        self.assertEqual(body["error"], "session_not_found")
        self.assertIn("no-such-session", body["message"])

    def test_step_from_wrong_session_returns_409(self) -> None:
        """Posting a finding with a step_id from a different session returns 409."""
        self._create_session(session_id="test-session")
        self.manager.create_session("other-session", "Other", zing_file=None)
        resp = self.client.post(
            "/other-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "Wrong session"},
        )
        self.assertEqual(resp.status_code, 409)
        body = resp.json()
        self.assertEqual(body["error"], "invalid_state")
        self.assertIn("test-session", body["message"])

    def test_valid_evaluation_finding(self) -> None:
        """A valid evaluation finding is accepted and stored."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "evaluation",
                "title": "Pass 1: Design Fundamentals",
                "criteria": [
                    {"name": "Clarity", "rating": "strong", "justification": "Clear design"},
                    {"name": "YAGNI", "rating": "weak", "justification": "Extra abstractions"},
                ],
                "litmus_tests": [
                    {"name": "Simplest thing?", "result": "Could be simpler"},
                ],
                "warnings": [
                    {"name": "Future flexibility", "found": True, "details": "Plugin system"},
                ],
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        self.assertEqual(session.total_findings, 1)
        self.assertEqual(session.steps[0].findings[0].type, "evaluation")

    def test_evaluation_finding_rejects_invalid_rating(self) -> None:
        """An evaluation finding with an invalid rating value returns 400."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "evaluation",
                "title": "Pass 1",
                "criteria": [
                    {"name": "Clarity", "rating": "excellent", "justification": "Great"},
                ],
            },
        )
        self.assertEqual(resp.status_code, 400)

    def test_malformed_finding_returns_400(self) -> None:
        """Posting invalid finding data returns 400 with field-level errors."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={"step_id": self.step_id, "type": "triage"},  # missing required fields
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
        resp = self.client.post(
            "/test-session/agent-complete",
            json={"step_id": self.step_id},
        )
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["completed_agents"], 1)
        self.assertEqual(body["expected_agents"], 2)
        self.assertEqual(body["state"], "pending")

    def test_all_agents_complete_transitions_to_ready(self) -> None:
        """When all expected agents complete, step transitions to ready."""
        self._create_session(expected_agents=2)
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})
        resp = self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["completed_agents"], 2)
        self.assertEqual(body["state"], "ready")

    def test_missing_step_id_returns_400(self) -> None:
        """Agent-complete without step_id returns 400."""
        self._create_session()
        resp = self.client.post("/test-session/agent-complete", json={})
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "missing_step_id")

    def test_invalid_session_returns_404(self) -> None:
        """Signaling agent-complete on a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/agent-complete",
            json={"step_id": "nonexistent-uuid"},
        )
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")


class TestSubmit(_ServerTestBase):
    """Tests for POST /{session_id}/submit."""

    def test_submit_responses(self) -> None:
        """Submitting responses stores them and transitions to completed."""
        self._create_session(expected_agents=1)
        self.client.post(
            "/test-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "What is the goal?"},
        )
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        resp = self.client.post(
            "/test-session/submit",
            json={"step_id": self.step_id, "responses": [{"answer": "Build a review UI"}]},
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
            json={"step_id": self.step_id, "type": "text", "title": "What do you think?"},
        )
        r2 = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        r3 = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "choice",
                "title": "Pick one",
                "options": [
                    {"label": "A", "description": "Option A"},
                    {"label": "B", "description": "Option B"},
                ],
            },
        )
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        text_id = r1.json()["finding_id"]
        triage_id = r2.json()["finding_id"]
        choice_id = r3.json()["finding_id"]

        # Submit as Datastar signals (responses is a dict)
        resp = self.client.post(
            "/test-session/submit",
            json={
                "step_id": self.step_id,
                "responses": {
                    text_id: "Looks good",
                    triage_id: "accept",
                    choice_id: "A",
                },
            },
        )
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["items_count"], 3)

        session = self.manager.get_session("test-session")
        assert session is not None
        self.assertEqual(session.state.value, "completed")
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].answer, "Looks good")
        self.assertEqual(step.responses[1].action, "accept")
        self.assertEqual(step.responses[2].selected, "A")

    def test_submit_with_evaluation_finding(self) -> None:
        """Evaluation findings are auto-acknowledged with empty UserResponse."""
        self._create_session(expected_agents=1)
        self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "evaluation",
                "title": "Pass 1",
                "criteria": [
                    {"name": "Clarity", "rating": "strong", "justification": "Good"},
                ],
            },
        )
        r2 = self.client.post(
            "/test-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "Any thoughts?"},
        )
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        text_id = r2.json()["finding_id"]

        resp = self.client.post(
            "/test-session/submit",
            json={"step_id": self.step_id, "responses": {text_id: "Looks good"}},
        )
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.json()["items_count"], 2)

        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        # Evaluation finding gets empty response (auto-acknowledged)
        self.assertIsNone(step.responses[0].answer)
        self.assertIsNone(step.responses[0].action)
        self.assertIsNone(step.responses[0].selected)
        # Text finding gets user's answer
        self.assertEqual(step.responses[1].answer, "Looks good")

    def test_submit_returns_400_for_invalid_responses(self) -> None:
        """Submitting non-list non-dict responses returns 400."""
        self._create_session(expected_agents=1)
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})
        resp = self.client.post(
            "/test-session/submit",
            json={"step_id": self.step_id, "responses": "invalid"},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "invalid_responses")

    def test_missing_step_id_returns_400(self) -> None:
        """Submitting without step_id returns 400."""
        self._create_session(expected_agents=1)
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})
        resp = self.client.post(
            "/test-session/submit",
            json={"responses": [{"answer": "test"}]},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "missing_step_id")

    def test_invalid_session_returns_404(self) -> None:
        """Submitting to a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/submit",
            json={"step_id": "nonexistent-step", "responses": []},
        )
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["error"], "session_not_found")

    def test_submit_other_choice(self) -> None:
        """Submitting __other__ choice captures freeform text."""
        self._create_session(expected_agents=1)
        r1 = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "choice",
                "title": "Pick one",
                "options": [{"label": "A", "description": "Option A"}],
            },
        )
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        choice_id = r1.json()["finding_id"]
        resp = self.client.post(
            "/test-session/submit",
            json={
                "step_id": self.step_id,
                "responses": {
                    choice_id: "__other__",
                    f"{choice_id}_other": "My custom answer",
                },
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].selected, "__other__")
        self.assertEqual(step.responses[0].other_text, "My custom answer")


class TestSSEStream(_ServerTestBase):
    """Tests for GET /{session_id}/stream."""

    def test_backfills_existing_findings(self) -> None:
        """SSE stream includes findings that existed before the connection."""
        self._create_session(expected_agents=1)
        resp_finding = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Old finding",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
            },
        )
        finding_id = resp_finding.json()["finding_id"]
        # Mark agent complete so the stream terminates
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        resp = self.client.get(f"/test-session/stream?step={self.step_id}")
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
            json={"step_id": self.step_id, "type": "text", "title": "Added after connect?"},
        )
        finding_id = resp_finding.json()["finding_id"]
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        resp = self.client.get(f"/test-session/stream?step={self.step_id}")
        self.assertEqual(resp.status_code, 200)
        # The finding should appear in the stream by its ID
        self.assertIn(finding_id, resp.text)
        self.assertIn("Added after connect?", resp.text)  # title appears in stream

    def test_stream_shows_submit_button_when_ready(self) -> None:
        """SSE stream sends submit button HTML when all agents complete."""
        self._create_session(expected_agents=1)
        self.client.post(
            "/test-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "Quick question"},
        )
        self.client.post("/test-session/agent-complete", json={"step_id": self.step_id})

        resp = self.client.get(f"/test-session/stream?step={self.step_id}")
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
                "step_id": self.step_id,
                "type": "triage",
                "title": "Test finding",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
            },
        )
        self.client.post("/unblock-session/agent-complete", json={"step_id": self.step_id})

        # Submit via Datastar signals
        session = self.manager.get_session("unblock-session")
        assert session is not None
        finding_id = session.steps[0].findings[0].id
        self.client.post(
            "/unblock-session/submit",
            json={"step_id": self.step_id, "responses": {finding_id: "accept"}},
        )

        # wait_for_review should return immediately since step is completed
        result = asyncio.run(wait_for_review(session_id="unblock-session", step_name=_STEP))
        self.assertEqual(result["session_id"], "unblock-session")
        self.assertEqual(result["step_name"], _STEP)
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
            title="Pick one",
            body="Some context about the choice",
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
        # Should have skip and other options
        self.assertIn('value="skip"', html)
        self.assertIn('value="__other__"', html)

    def test_choice_finding_has_other_textarea(self) -> None:
        """Choice finding renders an 'Other' option with conditional textarea."""
        finding = ChoiceFinding(
            id="ch2",
            title="Pick one",
            options=[ChoiceOption(label="A", description="Option A")],
        )
        html = finding_fragment(finding)
        self.assertIn("__other__", html)
        self.assertIn("data-show", html)
        self.assertIn("ch2_other", html)

    def test_triage_finding_renders_action_buttons(self) -> None:
        """Triage finding renders action buttons with data-bind."""
        finding = TriageFinding(
            id="tri1",
            title="Unused import os",
            body="The `os` module is imported but **never used**.",
            category="style",
            severity="low",
            confidence="high",
            location=Location(file="src/main.py", line=5),
        )
        html = finding_fragment(finding)
        self.assertIn("finding-tri1", html)
        self.assertIn("Unused import os", html)
        self.assertIn("src/main.py:5", html)
        self.assertIn("low", html)
        self.assertIn("high", html)
        # Body renders as HTML (markdown processed)
        self.assertIn("<strong>never used</strong>", html)
        # Action buttons
        self.assertIn("accept", html)
        self.assertIn("drop", html)
        self.assertIn("downgrade", html)
        self.assertIn("discuss", html)
        self.assertIn("data-class:selected=\"$responses.tri1 === 'accept'\"", html)


    def test_evaluation_finding_renders_tables(self) -> None:
        """Evaluation finding renders structured tables with badges."""
        finding = EvaluationFinding(
            id="eval1",
            title="Pass 1: Design Fundamentals",
            criteria=[
                CriterionRating(name="Clarity", rating="strong", justification="Very clear"),
                CriterionRating(name="YAGNI", rating="weak", justification="Over-engineered"),
            ],
            litmus_tests=[
                LitmusTest(name="Simplest thing?", result="Could be simpler"),
            ],
            warnings=[
                WarningSign(name="Future flexibility", found=True, details="Plugin system"),
                WarningSign(name="Only one approach", found=False),
            ],
        )
        html = finding_fragment(finding)
        self.assertIn("finding-eval1", html)
        self.assertIn("Pass 1: Design Fundamentals", html)
        self.assertIn("eval-table", html)
        # Criteria
        self.assertIn("Clarity", html)
        self.assertIn("badge-strong", html)
        self.assertIn("Very clear", html)
        self.assertIn("YAGNI", html)
        self.assertIn("badge-weak", html)
        # Litmus tests
        self.assertIn("Simplest thing?", html)
        self.assertIn("Could be simpler", html)
        # Warnings
        self.assertIn("Future flexibility", html)
        self.assertIn("badge-warn-yes", html)
        self.assertIn("Plugin system", html)
        # Informational meta
        self.assertIn("Informational", html)
        # No input controls
        self.assertNotIn("<textarea", html)
        self.assertNotIn('type="radio"', html)

    def test_evaluation_finding_without_optional_sections(self) -> None:
        """Evaluation finding with only criteria renders no litmus/warning tables."""
        finding = EvaluationFinding(
            id="eval2",
            title="Pass 4: Code Quality",
            criteria=[
                CriterionRating(name="Code Quality", rating="adequate", justification="Decent"),
            ],
        )
        html = finding_fragment(finding)
        self.assertIn("Code Quality", html)
        self.assertIn("badge-adequate", html)
        # Should not render litmus or warning tables
        self.assertNotIn("Litmus Test", html)
        self.assertNotIn("Warning Sign", html)


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
        step_id_a = self.step_id
        self._create_session(session_id="session-b", expected_agents=1)
        step_id_b = self.step_id

        self.client.post(
            "/session-a/findings",
            json={"step_id": step_id_a, "type": "text", "title": "Question for A"},
        )
        self.client.post(
            "/session-b/findings",
            json={
                "step_id": step_id_b,
                "type": "triage",
                "title": "Finding for B",
                "category": "performance",
                "severity": "medium",
                "confidence": "medium",
            },
        )

        session_a = self.manager.get_session("session-a")
        session_b = self.manager.get_session("session-b")
        assert session_a is not None
        assert session_b is not None

        self.assertEqual(session_a.total_findings, 1)
        self.assertEqual(session_b.total_findings, 1)
        self.assertEqual(session_a.steps[0].findings[0].type, "text")
        self.assertEqual(session_b.steps[0].findings[0].type, "triage")


class TestHTMLEndpoints(_ServerTestBase):
    """Tests for the HTML page endpoints."""

    def test_dashboard(self) -> None:
        """Dashboard returns HTML listing all sessions with correct status badges."""
        self._create_session(session_id="s1", title="First Session")
        self._create_session(session_id="s2", title="Second Session", expected_agents=1)
        step_id_s2 = self.step_id
        # Complete the second session's step so it gets a different status badge
        self.client.post(
            "/s2/findings",
            json={"step_id": step_id_s2, "type": "text", "title": "How is it?"},
        )
        self.client.post("/s2/agent-complete", json={"step_id": step_id_s2})
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
        """Review page has data-init with @get for SSE connection."""
        self._create_session(session_id="s1", title="SSE Test")
        resp = self.client.get("/s1")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("data-init", resp.text)
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
        """POST cleanup removes session from manager."""
        self._create_session(session_id="s1", title="To Clean")
        resp = self.client.post("/sessions/s1/cleanup")
        self.assertEqual(resp.status_code, 200)
        self.assertIsNone(self.manager.get_session("s1"))

    def test_cleanup_returns_404_for_invalid_session(self) -> None:
        """POST cleanup for nonexistent session returns 204 (no-op)."""
        resp = self.client.post("/sessions/nonexistent/cleanup")
        self.assertEqual(resp.status_code, 204)


class TestDashboardSSE(_ServerTestBase):
    """Tests for the dashboard SSE notification mechanism."""

    @staticmethod
    def _drain_queue(queue: asyncio.Queue[str]) -> list[str]:
        """Drain all pending events from a queue and return them."""
        events: list[str] = []
        while not queue.empty():
            events.append(queue.get_nowait())
        return events

    def test_dashboard_notified_on_agent_complete(self) -> None:
        """Dashboard SSE queues receive events when agent completes."""
        from zing_ai.server.routes import _dashboard_queues

        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(
                session_id="sse-dash", title="SSE Dashboard Test", expected_agents=1,
            )
            # Drain observer events from session creation and step start
            self._drain_queue(queue)
            self.client.post(
                "/sse-dash/findings",
                json={"step_id": self.step_id, "type": "text", "title": "Test?"},
            )
            self.client.post("/sse-dash/agent-complete", json={"step_id": self.step_id})
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
                json={"step_id": self.step_id, "type": "text", "title": "How?"},
            )
            self.client.post("/sse-sub/agent-complete", json={"step_id": self.step_id})
            # Drain all earlier events (created, step_started, agent_complete)
            self._drain_queue(queue)

            self.client.post(
                "/sse-sub/submit",
                json={"step_id": self.step_id, "responses": [{"answer": "Fine"}]},
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
            # Drain observer events from session creation and step start
            self._drain_queue(queue)
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
                    zing_file=None,
                )
            )
        self.assertEqual(result["status"], "created")
        self.assertEqual(result["url"], "http://localhost:9876/mcp-session")
        mock_open.assert_called_once_with("http://localhost:9876/mcp-session")

        session = self.manager.get_session("mcp-session")
        self.assertIsNotNone(session)
        assert session is not None
        self.assertEqual(session.title, "MCP Review")

    def test_start_step_creates_step(self) -> None:
        """start_step MCP tool creates a workflow step."""
        configure(self.manager, port=9876)
        asyncio.run(
            create_review(session_id="step-test", title="Step Test", zing_file=None)
        )
        result = asyncio.run(
            start_step(session_id="step-test", step_name="code-review", expected_agents=6)
        )
        self.assertEqual(result["status"], "started")
        self.assertEqual(result["step_name"], "code-review")
        self.assertIn("step_id", result)

        session = self.manager.get_session("step-test")
        assert session is not None
        self.assertEqual(len(session.steps), 1)
        self.assertEqual(session.steps[0].expected_agents, 6)


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
                "step_id": self.step_id,
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        self.client.post("/wait-session/agent-complete", json={"step_id": self.step_id})
        self.client.post(
            "/wait-session/submit",
            json={"step_id": self.step_id, "responses": [{"action": "accept"}]},
        )

        # Now wait_for_review should return immediately since the event is already set
        result = asyncio.run(
            wait_for_review(session_id="wait-session", step_name=_STEP)
        )

        self.assertEqual(result["session_id"], "wait-session")
        self.assertEqual(result["step_name"], _STEP)
        self.assertIsInstance(result["items"], list)
        self.assertEqual(len(result["items"]), 1)

        item = result["items"][0]
        self.assertEqual(item["finding"]["type"], "triage")
        self.assertEqual(item["finding"]["title"], "Unused import")
        self.assertEqual(item["response"]["action"], "accept")

    def test_wait_for_review_blocks_until_submission(self) -> None:
        """wait_for_review blocks until the step is submitted."""
        configure(self.manager, port=9876)
        self._create_session(session_id="block-session", expected_agents=1)

        self.client.post(
            "/block-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "What do you think?"},
        )
        self.client.post("/block-session/agent-complete", json={"step_id": self.step_id})

        async def _test_blocking() -> None:
            completed = False

            async def do_wait() -> dict:
                nonlocal completed
                result = await wait_for_review(session_id="block-session", step_name=_STEP)
                completed = True
                return result

            task = asyncio.create_task(do_wait())

            # Yield control briefly — wait_for_review should NOT have completed yet
            await asyncio.sleep(0.05)
            self.assertFalse(completed, "wait_for_review should block until submission")

            # Submit responses to unblock
            self.client.post(
                "/block-session/submit",
                json={"step_id": self.step_id, "responses": [{"answer": "Looks good"}]},
            )

            result = await task
            self.assertTrue(completed)
            self.assertEqual(result["session_id"], "block-session")
            self.assertEqual(len(result["items"]), 1)

        asyncio.run(_test_blocking())


class TestTriageEnumValidation(_ServerTestBase):
    """Tests for StrEnum validation on triage findings."""

    def test_enum_validation_rejects_invalid_severity(self) -> None:
        """POST triage with invalid severity returns 400."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Some finding",
                "category": "style",
                "severity": "invalid",
                "confidence": "high",
            },
        )
        self.assertEqual(resp.status_code, 400)

    def test_enum_validation_rejects_invalid_confidence(self) -> None:
        """POST triage with invalid confidence returns 400."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Some finding",
                "category": "style",
                "severity": "low",
                "confidence": "invalid",
            },
        )
        self.assertEqual(resp.status_code, 400)

    def test_enum_validation_rejects_invalid_category(self) -> None:
        """POST triage with invalid category returns 400."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Some finding",
                "category": "invalid",
                "severity": "low",
                "confidence": "high",
            },
        )
        self.assertEqual(resp.status_code, 400)

    def test_structured_location(self) -> None:
        """POST triage with structured location stores and renders correctly."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Missing null check",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
                "location": {"file": "src/main.py", "line": 42},
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        finding = session.steps[0].findings[0]
        assert finding.location is not None
        self.assertEqual(finding.location.file, "src/main.py")
        self.assertEqual(finding.location.line, 42)
        html = finding_fragment(finding)
        self.assertIn("src/main.py:42", html)

    def test_location_without_line(self) -> None:
        """POST triage with location without line works correctly."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "File-level issue",
                "category": "architecture",
                "severity": "medium",
                "confidence": "medium",
                "location": {"file": "src/main.py"},
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        finding = session.steps[0].findings[0]
        html = finding_fragment(finding)
        self.assertIn("src/main.py", html)
        self.assertNotIn(":null", html)
        self.assertNotIn(":None", html)

    def test_triage_with_options(self) -> None:
        """POST triage with options renders both action buttons and options text."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Consider refactoring",
                "category": "readability",
                "severity": "medium",
                "confidence": "medium",
                "options": [
                    {"label": "Extract method", "description": "Pull the loop into a helper"},
                    {"label": "Inline comments", "description": "Add comments to clarify intent"},
                ],
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        finding = session.steps[0].findings[0]
        html = finding_fragment(finding)
        # Action buttons still present
        self.assertIn("accept", html)
        self.assertIn("drop", html)
        # Options rendered
        self.assertIn("triage-options", html)
        self.assertIn("Suggested approaches:", html)
        self.assertIn("Extract method", html)
        self.assertIn("Pull the loop into a helper", html)
        self.assertIn("Inline comments", html)

    def test_triage_without_options(self) -> None:
        """POST triage without options renders only action buttons, no options div."""
        self._create_session()
        self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Simple finding",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        session = self.manager.get_session("test-session")
        assert session is not None
        finding = session.steps[0].findings[0]
        html = finding_fragment(finding)
        self.assertIn("accept", html)
        self.assertNotIn("triage-options", html)

    def test_info_severity_badge(self) -> None:
        """POST triage with severity info renders badge-info class."""
        self._create_session()
        self.client.post(
            "/test-session/findings",
            json={
                "step_id": self.step_id,
                "type": "triage",
                "title": "Informational note",
                "category": "architecture",
                "severity": "info",
                "confidence": "low",
            },
        )
        session = self.manager.get_session("test-session")
        assert session is not None
        finding = session.steps[0].findings[0]
        html = finding_fragment(finding)
        self.assertIn("badge-info", html)


class TestStartStep(_ServerTestBase):
    """Tests for POST /{session_id}/steps."""

    def test_start_step(self) -> None:
        """Starting a step returns correct response."""
        self.manager.create_session("s1", "Test")
        resp = self.client.post(
            "/s1/steps",
            json={"step_name": "code-review", "expected_agents": 6},
        )
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertEqual(body["status"], "started")
        self.assertEqual(body["step_name"], "code-review")
        self.assertIn("step_id", body)
        self.assertEqual(body["sequence"], 0)

    def test_missing_step_name_returns_400(self) -> None:
        """Starting a step without step_name returns 400."""
        self.manager.create_session("s1", "Test")
        resp = self.client.post("/s1/steps", json={})
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "missing_step_name")


class TestMCPSubmitFinding(_ServerTestBase):
    """Tests for the submit_finding and mark_agent_complete MCP tools."""

    def setUp(self) -> None:
        super().setUp()
        configure(self.manager, port=9876)

    def test_submit_text_finding(self) -> None:
        """submit_finding with a text finding stores it in the session step."""
        self._create_session(session_id="sf-text", expected_agents=1)
        result = asyncio.run(
            submit_finding(
                session_id="sf-text",
                step_id=self.step_id,
                finding={"type": "text", "title": "What do you think?"},
            )
        )
        self.assertEqual(result["status"], "ok")
        self.assertIn("finding_id", result)

        session = self.manager.get_session("sf-text")
        assert session is not None
        self.assertEqual(len(session.steps[0].findings), 1)
        self.assertEqual(session.steps[0].findings[0].type, "text")
        self.assertEqual(session.steps[0].findings[0].title, "What do you think?")

    def test_submit_choice_finding(self) -> None:
        """submit_finding with a choice finding preserves options."""
        self._create_session(session_id="sf-choice", expected_agents=1)
        result = asyncio.run(
            submit_finding(
                session_id="sf-choice",
                step_id=self.step_id,
                finding={
                    "type": "choice",
                    "title": "Pick an approach",
                    "options": [
                        {"label": "Option A", "description": "First approach"},
                        {"label": "Option B", "description": "Second approach"},
                    ],
                },
            )
        )
        self.assertEqual(result["status"], "ok")

        session = self.manager.get_session("sf-choice")
        assert session is not None
        finding = session.steps[0].findings[0]
        self.assertEqual(finding.type, "choice")
        self.assertEqual(len(finding.options), 2)
        self.assertEqual(finding.options[0].label, "Option A")
        self.assertEqual(finding.options[1].label, "Option B")

    def test_submit_invalid_finding(self) -> None:
        """submit_finding with malformed dict raises a validation error."""
        self._create_session(session_id="sf-invalid", expected_agents=1)
        with self.assertRaises(Exception) as ctx:
            asyncio.run(
                submit_finding(
                    session_id="sf-invalid",
                    step_id=self.step_id,
                    finding={"not_a_type": "garbage"},
                )
            )
        # Pydantic ValidationError is raised for missing 'type' discriminator
        self.assertIn("ValidationError", type(ctx.exception).__name__)

    def test_submit_to_unknown_session(self) -> None:
        """submit_finding with a bogus step_id raises KeyError."""
        with self.assertRaises(KeyError):
            asyncio.run(
                submit_finding(
                    session_id="nonexistent",
                    step_id="00000000-0000-0000-0000-000000000000",
                    finding={"type": "text", "title": "Nope"},
                )
            )

    def test_submit_to_completed_step(self) -> None:
        """submit_finding to a completed step raises ValueError."""
        self._create_session(session_id="sf-completed", expected_agents=1)

        # Add a finding, mark agent complete, then submit responses to complete the step
        self.manager.add_finding(
            "sf-completed", self.step_id, {"type": "text", "title": "A finding"}
        )
        self.manager.mark_agent_complete("sf-completed", self.step_id)

        from zing_ai.server.models import UserResponse

        self.manager.submit_responses(
            "sf-completed", self.step_id, [UserResponse(answer="ok")]
        )
        # Step is now COMPLETED — submitting a new finding should fail
        with self.assertRaises(ValueError):
            asyncio.run(
                submit_finding(
                    session_id="sf-completed",
                    step_id=self.step_id,
                    finding={"type": "text", "title": "Too late"},
                )
            )

    def test_mark_agent_complete(self) -> None:
        """mark_agent_complete increments completed_agents."""
        self._create_session(session_id="mac-basic", expected_agents=3)
        result = asyncio.run(
            mark_agent_complete(session_id="mac-basic", step_id=self.step_id)
        )
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["step_state"], "pending")

        session = self.manager.get_session("mac-basic")
        assert session is not None
        self.assertEqual(session.steps[0].completed_agents, 1)

    def test_mark_all_agents_complete_transitions_to_ready(self) -> None:
        """Step state changes to READY when all agents are done."""
        self._create_session(session_id="mac-ready", expected_agents=2)

        result1 = asyncio.run(
            mark_agent_complete(session_id="mac-ready", step_id=self.step_id)
        )
        self.assertEqual(result1["step_state"], "pending")

        result2 = asyncio.run(
            mark_agent_complete(session_id="mac-ready", step_id=self.step_id)
        )
        self.assertEqual(result2["step_state"], "ready")

        session = self.manager.get_session("mac-ready")
        assert session is not None
        self.assertEqual(session.steps[0].completed_agents, 2)
        self.assertEqual(session.steps[0].state.value, "ready")
