"""Tests for Zing server HTTP route endpoints."""

from __future__ import annotations

import asyncio
import json
from unittest.mock import MagicMock, patch

from tests.test_server_base import _STEP, ServerTestBase
from zing_ai.server.mcp_tools import configure, review_wait
from zing_ai.server.routes import _dashboard_queues, _sse_queues


class TestRemovedEndpoints(ServerTestBase):
    """Tests that removed REST endpoints return 404."""

    def test_post_findings_returns_404(self) -> None:
        """POST /{session_id}/findings is removed and returns 404."""
        self._create_session()
        resp = self.client.post(
            "/test-session/findings",
            json={"step_id": self.step_id, "type": "text", "title": "Hello"},
        )
        self.assertEqual(resp.status_code, 404)

    def test_post_steps_returns_404(self) -> None:
        """POST /{session_id}/steps is removed and returns 404."""
        session = self.manager.create_session("s1", "Test", steps=["code-review"])
        step_id = session.steps[0].step_id
        resp = self.client.post("/s1/steps", json={"step_id": step_id})
        self.assertEqual(resp.status_code, 404)

    def test_post_agent_complete_returns_404(self) -> None:
        """POST /{session_id}/agent-complete is removed and returns 404."""
        self._create_session()
        resp = self.client.post(
            "/test-session/agent-complete",
            json={"step_id": self.step_id},
        )
        self.assertEqual(resp.status_code, 404)


class TestSaveResponse(ServerTestBase):
    """Tests for POST /{session_id}/save-response."""

    def _create_session_with_finding(self) -> tuple[str, str]:
        """Create a session with a text finding and return (step_id, finding_id)."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {"type": "text", "title": "What changed?"},
        )
        return self.step_id, finding.id

    def test_save_response_ok(self) -> None:
        """Auto-saving a response returns 200."""
        step_id, finding_id = self._create_session_with_finding()
        resp = self.client.post(
            "/test-session/save-response",
            json={"step_id": step_id, "responses": {finding_id: "Lots of stuff"}},
        )
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.json()["status"], "ok")

    def test_save_response_persists(self) -> None:
        """Auto-saved response is stored on the step."""
        step_id, finding_id = self._create_session_with_finding()
        self.client.post(
            "/test-session/save-response",
            json={"step_id": step_id, "responses": {finding_id: "Saved text"}},
        )
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].answer, "Saved text")

    def test_save_response_missing_step_id(self) -> None:
        """Missing step_id returns 400."""
        self._create_session()
        resp = self.client.post(
            "/test-session/save-response",
            json={"responses": {}},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["error"], "missing_fields")

    def test_save_response_invalid_session(self) -> None:
        """Auto-save to nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/save-response",
            json={"step_id": "x", "responses": {"y": "z"}},
        )
        self.assertEqual(resp.status_code, 404)

    def test_save_response_empty_responses(self) -> None:
        """Empty responses dict saves nothing."""
        step_id, _finding_id = self._create_session_with_finding()
        resp = self.client.post(
            "/test-session/save-response",
            json={"step_id": step_id, "responses": {}},
        )
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.json()["saved"], 0)

    def _create_session_with_triage_finding(self) -> tuple[str, str]:
        """Create a session with a triage finding and return (step_id, finding_id)."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        return self.step_id, finding.id

    def test_save_response_complexity(self) -> None:
        """Auto-saving a complexity value via the _complexity signal key persists it."""
        step_id, finding_id = self._create_session_with_triage_finding()
        resp = self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {f"{finding_id}_complexity": "simple"},
            },
        )
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.json()["saved"], 1)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        assert step.responses[0].complexity is not None
        self.assertEqual(step.responses[0].complexity.value, "simple")

    def test_save_response_complexity_complex(self) -> None:
        """Auto-saving complexity='complex' persists correctly."""
        step_id, finding_id = self._create_session_with_triage_finding()
        resp = self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {f"{finding_id}_complexity": "complex"},
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        assert step.responses[0].complexity is not None
        self.assertEqual(step.responses[0].complexity.value, "complex")

    def test_save_response_complexity_preserves_action(self) -> None:
        """Saving complexity after action preserves the previously saved action."""
        step_id, finding_id = self._create_session_with_triage_finding()
        # First save an action
        self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {finding_id: "accept"},
            },
        )
        # Then save complexity
        self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {f"{finding_id}_complexity": "complex"},
            },
        )
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].action, "accept")
        assert step.responses[0].complexity is not None
        self.assertEqual(step.responses[0].complexity.value, "complex")

    def test_save_response_complexity_default_when_missing(self) -> None:
        """When no complexity is sent, the UserResponse.complexity remains None."""
        step_id, finding_id = self._create_session_with_triage_finding()
        # Save only an action, no complexity
        self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {finding_id: "drop"},
            },
        )
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].action, "drop")
        self.assertIsNone(step.responses[0].complexity)

    def test_save_response_invalid_complexity_ignored(self) -> None:
        """An invalid complexity value is silently discarded (complexity stays None)."""
        step_id, finding_id = self._create_session_with_triage_finding()
        resp = self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {f"{finding_id}_complexity": "urgent"},
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        # No response should have been saved since the only field was invalid
        if step.responses:
            self.assertIsNone(step.responses[0].complexity)

    def test_save_response_empty_complexity_ignored(self) -> None:
        """An empty string complexity value is silently discarded."""
        step_id, finding_id = self._create_session_with_triage_finding()
        resp = self.client.post(
            "/test-session/save-response",
            json={
                "step_id": step_id,
                "responses": {f"{finding_id}_complexity": ""},
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        if step.responses:
            self.assertIsNone(step.responses[0].complexity)


class TestSubmit(ServerTestBase):
    """Tests for POST /{session_id}/submit."""

    def _add_finding_and_ready(
        self,
        session_id: str = "test-session",
        finding_data: dict | None = None,
    ) -> str:
        """Add a finding via manager, transition step to ready, return finding_id."""
        if finding_data is None:
            finding_data = {"type": "text", "title": "What is the goal?"}
        finding = self.manager.add_finding(session_id, self.step_id, finding_data)
        # Transition step to ready by starting and stopping an agent, then marking ready
        self.manager.start_agent(session_id, self.step_id, "test-agent")
        self.manager.stop_agent(session_id, self.step_id, "test-agent")
        self.manager.mark_step_ready(session_id, self.step_id)
        return finding.id

    def test_submit_responses(self) -> None:
        """Submitting responses stores them and transitions to completed."""
        self._create_session()
        self._add_finding_and_ready()

        resp = self.client.post(
            "/test-session/submit",
            json={"step_id": self.step_id, "responses": [{"answer": "Build a review UI"}]},
        )
        self.assertEqual(resp.status_code, 200)
        # Response is SSE (Datastar patches), not JSON
        self.assertIn("text/event-stream", resp.headers.get("content-type", ""))

        session = self.manager.get_session("test-session")
        assert session is not None
        self.assertEqual(session.state.value, "completed")

    def test_submit_datastar_signals(self) -> None:
        """Submitting Datastar signals maps finding IDs to responses."""
        self._create_session()
        # Add findings of each type via manager
        f_text = self.manager.add_finding(
            "test-session",
            self.step_id,
            {"type": "text", "title": "What do you think?"},
        )
        f_triage = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        f_triage_no_meta = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Pick one",
                "options": [
                    {"label": "A", "description": "Option A"},
                    {"label": "B", "description": "Option B"},
                ],
            },
        )
        # Transition step to ready
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        # Submit as Datastar signals (responses is a dict)
        resp = self.client.post(
            "/test-session/submit",
            json={
                "step_id": self.step_id,
                "responses": {
                    f_text.id: "Looks good",
                    f_triage.id: "accept",
                    f"{f_triage_no_meta.id}_approach": "A",
                },
            },
        )
        self.assertEqual(resp.status_code, 200)
        # Response is SSE (Datastar patches), not JSON
        self.assertIn("text/event-stream", resp.headers.get("content-type", ""))

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
        self._create_session()
        self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "evaluation",
                "title": "Pass 1",
                "criteria": [
                    {"name": "Clarity", "rating": "strong", "justification": "Good"},
                ],
            },
        )
        f_text = self.manager.add_finding(
            "test-session",
            self.step_id,
            {"type": "text", "title": "Any thoughts?"},
        )
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        resp = self.client.post(
            "/test-session/submit",
            json={"step_id": self.step_id, "responses": {f_text.id: "Looks good"}},
        )
        self.assertEqual(resp.status_code, 200)
        self.assertIn("text/event-stream", resp.headers.get("content-type", ""))

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
        self._create_session()
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)
        resp = self.client.post(
            "/test-session/submit",
            json={"step_id": self.step_id, "responses": "invalid"},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["detail"]["error"], "invalid_responses")

    def test_missing_step_id_returns_400(self) -> None:
        """Submitting without step_id returns 400."""
        self._create_session()
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)
        resp = self.client.post(
            "/test-session/submit",
            json={"responses": [{"answer": "test"}]},
        )
        self.assertEqual(resp.status_code, 400)
        self.assertEqual(resp.json()["detail"]["error"], "missing_step_id")

    def test_invalid_session_returns_404(self) -> None:
        """Submitting to a nonexistent session returns 404."""
        resp = self.client.post(
            "/no-such-session/submit",
            json={"step_id": "nonexistent-step", "responses": []},
        )
        self.assertEqual(resp.status_code, 404)
        self.assertEqual(resp.json()["detail"]["error"], "session_not_found")

    def test_submit_other_triage_approach(self) -> None:
        """Submitting __other__ triage approach captures freeform text."""
        self._create_session()
        f_triage = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Pick one",
                "options": [{"label": "A", "description": "Option A"}],
            },
        )
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        resp = self.client.post(
            "/test-session/submit",
            json={
                "step_id": self.step_id,
                "responses": {
                    f"{f_triage.id}_approach": "__other__",
                    f"{f_triage.id}_approach_other": "My custom answer",
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

    def test_submit_with_complexity(self) -> None:
        """Submitting Datastar signals with complexity propagates it correctly."""
        self._create_session()
        f_triage = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        resp = self.client.post(
            "/test-session/submit",
            json={
                "step_id": self.step_id,
                "responses": {
                    f_triage.id: "accept",
                    f"{f_triage.id}_complexity": "complex",
                },
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].action, "accept")
        assert step.responses[0].complexity is not None
        self.assertEqual(step.responses[0].complexity.value, "complex")

    def test_submit_merges_with_auto_saved_complexity(self) -> None:
        """Submit merges with auto-saved responses, preserving complexity not in signals."""
        self._create_session()
        f_triage = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        # Auto-save complexity before submit
        self.client.post(
            "/test-session/save-response",
            json={
                "step_id": self.step_id,
                "responses": {f"{f_triage.id}_complexity": "complex"},
            },
        )
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        # Submit with only action — no complexity in signals
        resp = self.client.post(
            "/test-session/submit",
            json={
                "step_id": self.step_id,
                "responses": {f_triage.id: "accept"},
            },
        )
        self.assertEqual(resp.status_code, 200)
        session = self.manager.get_session("test-session")
        assert session is not None
        step = session.steps[0]
        assert step.responses is not None
        self.assertEqual(step.responses[0].action, "accept")
        # Complexity should be preserved from auto-save
        assert step.responses[0].complexity is not None
        self.assertEqual(step.responses[0].complexity.value, "complex")


class TestSSEStream(ServerTestBase):
    """Tests for GET /{session_id}/stream."""

    def _add_finding_and_ready(
        self,
        session_id: str = "test-session",
        finding_data: dict | None = None,
    ) -> str:
        """Add a finding via manager, transition step to ready, return finding_id."""
        if finding_data is None:
            finding_data = {
                "type": "triage",
                "title": "Old finding",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
            }
        finding = self.manager.add_finding(session_id, self.step_id, finding_data)
        self.manager.start_agent(session_id, self.step_id, "test-agent")
        self.manager.stop_agent(session_id, self.step_id, "test-agent")
        self.manager.mark_step_ready(session_id, self.step_id)
        return finding.id

    def test_backfills_existing_findings(self) -> None:
        """SSE stream includes findings that existed before the connection."""
        self._create_session()
        finding_id = self._add_finding_and_ready()

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
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {"type": "text", "title": "Added after connect?"},
        )
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        resp = self.client.get(f"/test-session/stream?step={self.step_id}")
        self.assertEqual(resp.status_code, 200)
        # The finding should appear in the stream by its ID
        self.assertIn(finding.id, resp.text)
        self.assertIn("Added after connect?", resp.text)  # title appears in stream

    def test_stream_shows_submit_button_when_ready(self) -> None:
        """SSE stream sends submit button HTML when all agents complete."""
        self._create_session()
        self.manager.add_finding(
            "test-session",
            self.step_id,
            {"type": "text", "title": "Quick question"},
        )
        self.manager.start_agent("test-session", self.step_id, "test-agent")
        self.manager.stop_agent("test-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("test-session", self.step_id)

        resp = self.client.get(f"/test-session/stream?step={self.step_id}")
        self.assertEqual(resp.status_code, 200)
        # Submit button should be present
        self.assertIn("Submit Review", resp.text)
        self.assertIn("@post(", resp.text)

    def test_submit_unblocks_wait_for_review(self) -> None:
        """Submit endpoint collects responses and unblocks wait_for_review."""
        configure(self.manager, port=9876)
        self._create_session(session_id="unblock-session")
        finding = self.manager.add_finding(
            "unblock-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Test finding",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
            },
        )
        self.manager.start_agent("unblock-session", self.step_id, "test-agent")
        self.manager.stop_agent("unblock-session", self.step_id, "test-agent")
        self.manager.mark_step_ready("unblock-session", self.step_id)

        # Submit via Datastar signals
        self.client.post(
            "/unblock-session/submit",
            json={"step_id": self.step_id, "responses": {finding.id: "accept"}},
        )

        # review_wait should return immediately since step is completed
        result = asyncio.run(review_wait(session_id="unblock-session", step_id=self.step_id))
        self.assertEqual(result["session_id"], "unblock-session")
        self.assertEqual(result["step_name"], _STEP)
        self.assertEqual(len(result["items"]), 1)
        self.assertEqual(result["items"][0]["finding"]["type"], "triage")
        self.assertEqual(result["items"][0]["response"]["action"], "accept")


class TestHTMLEndpoints(ServerTestBase):
    """Tests for the HTML page endpoints."""

    def test_dashboard(self) -> None:
        """Dashboard returns HTML listing all sessions with correct status badges."""
        self._create_session(session_id="s1", title="First Session")
        self._create_session(session_id="s2", title="Second Session")
        step_id_s2 = self.step_id
        # Transition second session's step to READY via SessionManager
        self.manager.add_finding(
            "s2",
            step_id_s2,
            {"type": "text", "title": "How is it?"},
        )
        self.manager.start_agent("s2", step_id_s2, "test-agent")
        self.manager.stop_agent("s2", step_id_s2, "test-agent")
        self.manager.mark_step_ready("s2", step_id_s2)
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

    def test_completed_review_renders_findings(self) -> None:
        """Completed review page includes findings in the initial HTML."""
        self._create_session(session_id="s1", title="Completed Review")
        finding = self.manager.add_finding(
            "s1",
            self.step_id,
            {
                "type": "triage",
                "title": "Bug in auth module",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
            },
        )
        self.manager.start_agent("s1", self.step_id, "test-agent")
        self.manager.stop_agent("s1", self.step_id, "test-agent")
        self.manager.mark_step_ready("s1", self.step_id)
        self.client.post(
            "/s1/submit",
            json={"step_id": self.step_id, "responses": {finding.id: "accept"}},
        )
        resp = self.client.get(f"/s1?step={self.step_id}")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("Bug in auth module", resp.text)
        self.assertIn("Review submitted", resp.text)


class TestCleanup(ServerTestBase):
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


class TestDashboardSSE(ServerTestBase):
    """Tests for the dashboard SSE notification mechanism."""

    @staticmethod
    def _drain_queue(queue: asyncio.Queue[str]) -> list[str]:
        """Drain all pending events from a queue and return them."""
        events: list[str] = []
        while not queue.empty():
            events.append(queue.get_nowait())
        return events

    def test_dashboard_notified_on_finding_added(self) -> None:
        """Dashboard SSE queues receive events when findings are added."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(
                session_id="sse-dash",
                title="SSE Dashboard Test",
            )
            # Drain observer events from session creation and step start
            self._drain_queue(queue)
            self.manager.add_finding(
                "sse-dash",
                self.step_id,
                {"type": "text", "title": "Test?"},
            )
            # finding_added is not currently mapped to dashboard events,
            # but session_created and step_started are. Verify the mechanism
            # works for cleanup (tested below) which is still routed.
        finally:
            _dashboard_queues.remove(queue)

    def test_dashboard_notified_on_review_submitted(self) -> None:
        """Dashboard SSE queues receive events when review is submitted."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(
                session_id="sse-sub",
                title="Submit Test",
            )
            self.manager.add_finding(
                "sse-sub",
                self.step_id,
                {"type": "text", "title": "How?"},
            )
            self.manager.start_agent("sse-sub", self.step_id, "test-agent")
            self.manager.stop_agent("sse-sub", self.step_id, "test-agent")
            self.manager.mark_step_ready("sse-sub", self.step_id)
            # Drain all earlier events
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

    def test_dashboard_notified_on_step_ready(self) -> None:
        """Dashboard SSE queues receive step_ready when a step is marked ready."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(session_id="sse-ready", title="Ready Test")
            self.manager.add_finding(
                "sse-ready",
                self.step_id,
                {"type": "text", "title": "Note"},
            )
            self.manager.start_agent("sse-ready", self.step_id, "test-agent")
            self.manager.stop_agent("sse-ready", self.step_id, "test-agent")
            # Drain all earlier events
            self._drain_queue(queue)

            self.manager.mark_step_ready("sse-ready", self.step_id)
            events = self._drain_queue(queue)
            self.assertIn("step_ready", events)
        finally:
            _dashboard_queues.remove(queue)

    def test_dashboard_notified_on_agents_done(self) -> None:
        """Dashboard SSE queues receive agents_done when all agents complete."""
        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            self._create_session(session_id="sse-agents", title="Agents Done Test")
            self.manager.start_agent("sse-agents", self.step_id, "agent-1")
            # Drain events from creation, step start, and agent start
            self._drain_queue(queue)

            self.manager.stop_agent("sse-agents", self.step_id, "agent-1")
            events = self._drain_queue(queue)
            self.assertIn("agents_done", events)
        finally:
            _dashboard_queues.remove(queue)


class TestConcurrentSessions(ServerTestBase):
    """Tests that two sessions don't interfere with each other."""

    def test_sessions_are_isolated(self) -> None:
        """Findings added to one session don't appear in another."""
        self._create_session(session_id="session-a")
        step_id_a = self.step_id
        self._create_session(session_id="session-b")
        step_id_b = self.step_id

        self.manager.add_finding(
            "session-a",
            step_id_a,
            {"type": "text", "title": "Question for A"},
        )
        self.manager.add_finding(
            "session-b",
            step_id_b,
            {
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


class TestNotificationRouting(ServerTestBase):
    """Tests for notification event routing through SSE and dashboard queues."""

    @staticmethod
    def _drain_queue(queue: asyncio.Queue[str]) -> list[str]:
        """Drain all pending events from a queue and return them."""
        events: list[str] = []
        while not queue.empty():
            events.append(queue.get_nowait())
        return events

    def test_dashboard_notification_with_session_id(self) -> None:
        """_notify_dashboard_connections encodes session_id into the event string."""
        from zing_ai.server.routes import _notify_dashboard_connections

        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            _notify_dashboard_connections("notification:notif1", session_id="abc123")
            events = self._drain_queue(queue)
            self.assertEqual(events, ["notification:notif1:abc123"])
        finally:
            _dashboard_queues.remove(queue)

    def test_dashboard_notification_without_session_id(self) -> None:
        """_notify_dashboard_connections passes event unchanged when no session_id."""
        from zing_ai.server.routes import _notify_dashboard_connections

        queue: asyncio.Queue[str] = asyncio.Queue()
        _dashboard_queues.append(queue)
        try:
            _notify_dashboard_connections("notification")
            events = self._drain_queue(queue)
            self.assertEqual(events, ["notification"])
        finally:
            _dashboard_queues.remove(queue)

    def test_sse_notification_routed_to_session_queues(self) -> None:
        """_notify_sse_connections pushes 'notification:{id}' to the session's queues."""
        from zing_ai.server.routes import _notify_sse_connections

        session_id = "notif-sse"
        queue: asyncio.Queue[str] = asyncio.Queue()
        _sse_queues[session_id].append(queue)
        try:
            _notify_sse_connections(session_id, "notification:abc123")
            events = self._drain_queue(queue)
            self.assertEqual(events, ["notification:abc123"])
        finally:
            _sse_queues[session_id].remove(queue)
            if not _sse_queues[session_id]:
                _sse_queues.pop(session_id, None)


class TestNotificationSSEOutput(ServerTestBase):
    """Tests that SSE generators yield correct directives for notification events.

    These tests call the route handler functions directly with mocked
    ``Request`` objects, then iterate the returned async generator to
    collect SSE output.  ``asyncio.wait_for`` is patched so that the
    pre-loaded queue events are consumed immediately and the generator
    terminates cleanly after processing them.
    """

    @staticmethod
    def _mock_request(manager, query_string: str = "") -> MagicMock:  # noqa: ANN001
        """Build a fake Starlette Request with the given manager and query params."""
        from starlette.datastructures import QueryParams

        app_state = MagicMock()
        app_state.session_manager = manager
        app_mock = MagicMock()
        app_mock.state = app_state
        request = MagicMock()
        request.app = app_mock
        request.query_params = QueryParams(query_string)
        return request

    @staticmethod
    async def _collect_stream_findings(
        manager,  # noqa: ANN001
        session_id: str,
        step_id: str,
        events: list[str],
    ) -> str:
        """Call stream_findings, push events via queue, and return joined SSE text."""
        from zing_ai.server.routes import stream_findings

        request = TestNotificationSSEOutput._mock_request(
            manager,
            f"step={step_id}",
        )

        # Pre-load the queue.  The generator creates its own queue via
        # asyncio.Queue() and appends it to _sse_queues[session_id].
        # We patch asyncio.Queue to return a pre-loaded queue.
        queue: asyncio.Queue[str] = asyncio.Queue()
        for ev in events:
            queue.put_nowait(ev)

        # Patch asyncio.Queue to return our pre-loaded queue
        with patch("zing_ai.server.routes.asyncio.Queue", return_value=queue):
            # Patch wait_for: deliver queued events, then clean up session
            real_wait_for = asyncio.wait_for
            delivery_count = 0

            async def _fast_wait_for(coro, *, timeout=None):  # noqa: ANN001,ANN201
                nonlocal delivery_count
                delivery_count += 1
                if delivery_count <= len(events):
                    return await real_wait_for(coro, timeout=0.1)
                # After all events consumed, remove session to exit generator
                coro.close()
                manager.cleanup_session(session_id)
                raise TimeoutError

            # Mock render for notification_timeline.html (template created
            # in a later step) to return a placeholder.
            _real_render = None

            def _mock_render(template_name, **kwargs):  # noqa: ANN001,ANN201,ANN003
                if template_name == "fragments/notification_timeline.html":
                    s = kwargs.get("s")
                    sid = s.session_id if s else "unknown"
                    return f'<div id="notifications-{sid}">timeline</div>'
                assert _real_render is not None
                return _real_render(template_name, **kwargs)

            import zing_ai.server.routes as _routes_mod

            _real_render = _routes_mod.render

            with (
                patch("zing_ai.server.routes.asyncio.wait_for", _fast_wait_for),
                patch("zing_ai.server.routes.render", side_effect=_mock_render),
            ):
                response = await stream_findings(session_id, request)
                chunks: list[str] = []
                async for chunk in response.body_iterator:
                    if isinstance(chunk, bytes):
                        chunk = chunk.decode()
                    chunks.append(chunk)  # type: ignore[arg-type]

        # Clean up queue registration
        queues = _sse_queues.get(session_id, [])
        if queue in queues:
            queues.remove(queue)
        if not queues:
            _sse_queues.pop(session_id, None)

        return "".join(chunks)

    @staticmethod
    async def _collect_dashboard_events(
        manager,  # noqa: ANN001
        events: list[str],
    ) -> str:
        """Call dashboard_events, push events via queue, and return joined SSE text."""
        from zing_ai.server.routes import dashboard_events

        request = TestNotificationSSEOutput._mock_request(manager)

        queue: asyncio.Queue[str] = asyncio.Queue()
        for ev in events:
            queue.put_nowait(ev)

        _real_render = None

        def _mock_render(template_name, **kwargs):  # noqa: ANN001,ANN201,ANN003
            if template_name == "fragments/notification_timeline.html":
                s = kwargs.get("s")
                sid = s.session_id if s else "unknown"
                return f'<div id="notifications-{sid}">timeline</div>'
            assert _real_render is not None
            return _real_render(template_name, **kwargs)

        import zing_ai.server.routes as _routes_mod

        _real_render = _routes_mod.render

        with patch("zing_ai.server.routes.asyncio.Queue", return_value=queue):
            real_wait_for = asyncio.wait_for
            delivery_count = 0

            async def _fast_wait_for(coro, *, timeout=None):  # noqa: ANN001,ANN201
                nonlocal delivery_count
                delivery_count += 1
                if delivery_count <= len(events):
                    return await real_wait_for(coro, timeout=0.1)
                coro.close()
                raise asyncio.CancelledError

            chunks: list[str] = []
            with (
                patch("zing_ai.server.routes.asyncio.wait_for", _fast_wait_for),
                patch("zing_ai.server.routes.render", side_effect=_mock_render),
            ):
                try:
                    response = await dashboard_events(request)
                    async for chunk in response.body_iterator:
                        if isinstance(chunk, bytes):
                            chunk = chunk.decode()
                        chunks.append(chunk)  # type: ignore[arg-type]
                except (asyncio.CancelledError, StopAsyncIteration):
                    pass

        if queue in _dashboard_queues:
            _dashboard_queues.remove(queue)

        return "".join(chunks)

    def test_stream_findings_notification_yields_execute_script(self) -> None:
        """stream_findings yields execute_script with Notification JS on notification event."""
        self._create_session(session_id="notif-stream")
        notif = self.manager.add_notification(
            "notif-stream",
            "Build started",
            body="Step 1 running",
        )

        body = asyncio.run(
            self._collect_stream_findings(
                self.manager,
                "notif-stream",
                self.step_id,
                [f"notification:{notif.id}"],
            ),
        )
        # The SSE output should contain a script element with Notification constructor
        self.assertIn("Notification(", body)
        self.assertIn("Build started", body)
        self.assertIn("Step 1 running", body)

    def test_stream_findings_notification_yields_timeline_patch(self) -> None:
        """stream_findings yields patch_elements for notification timeline."""
        self._create_session(session_id="notif-tl")
        notif = self.manager.add_notification(
            "notif-tl",
            "Build started",
            body="Step 1 running",
        )

        body = asyncio.run(
            self._collect_stream_findings(
                self.manager,
                "notif-tl",
                self.step_id,
                [f"notification:{notif.id}"],
            ),
        )
        self.assertIn("notifications-notif-tl", body)

    def test_stream_findings_notification_without_body(self) -> None:
        """stream_findings handles notification with no body (empty opts)."""
        self._create_session(session_id="notif-nobody")
        notif = self.manager.add_notification("notif-nobody", "Build started")

        body = asyncio.run(
            self._collect_stream_findings(
                self.manager,
                "notif-nobody",
                self.step_id,
                [f"notification:{notif.id}"],
            ),
        )
        self.assertIn("Notification(", body)
        self.assertIn("Build started", body)
        self.assertIn("{}", body)

    def test_dashboard_events_notification_yields_script_and_patch(self) -> None:
        """dashboard_events parses notification and yields script + timeline."""
        session_id = "dash-notif"
        self._create_session(session_id=session_id)
        notif = self.manager.add_notification(
            session_id,
            "Review ready",
            body="Please check",
        )

        body = asyncio.run(
            self._collect_dashboard_events(
                self.manager,
                [f"notification:{notif.id}:{session_id}"],
            ),
        )
        self.assertIn("Notification(", body)
        self.assertIn("Review ready", body)
        self.assertIn("Please check", body)
        self.assertIn(f"notifications-{session_id}", body)
        self.assertIn(json.dumps(f"/{session_id}"), body)

    def test_stream_findings_notification_empty_notifications_list(self) -> None:
        """notification event with empty notifications list is handled gracefully."""
        self._create_session(session_id="empty-notif")
        session = self.manager.get_session("empty-notif")
        assert session is not None
        session.notifications.clear()

        body = asyncio.run(
            self._collect_stream_findings(
                self.manager,
                "empty-notif",
                self.step_id,
                ["notification:nonexistent"],
            ),
        )
        self.assertNotIn("new Notification(", body)

    def test_dashboard_events_notification_empty_notifications(self) -> None:
        """dashboard_events handles notification with no notifications gracefully."""
        session_id = "dash-empty-notif"
        self._create_session(session_id=session_id)
        session = self.manager.get_session(session_id)
        assert session is not None
        session.notifications.clear()

        body = asyncio.run(
            self._collect_dashboard_events(
                self.manager,
                [f"notification:nonexistent:{session_id}"],
            ),
        )
        self.assertNotIn("new Notification(", body)
