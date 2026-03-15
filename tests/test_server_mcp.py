"""Tests for Zing server MCP tool endpoints."""

from __future__ import annotations

import asyncio

from tests.test_server_base import _STEP, ServerTestBase
from zing_ai.server.mcp_tools import (
    agent_start,
    agent_stop,
    configure,
    finding_submit,
    notification_send,
    review_wait,
    session_create,
    session_update,
    step_log,
    step_start,
)
from zing_ai.server.models import ResponseAction, TriageFinding, UserResponse


class TestSessionCreate(ServerTestBase):
    """Tests for the session_create MCP tool."""

    def test_session_create_creates_session_and_returns_url(self) -> None:
        """session_create creates a session with default steps and returns a URL."""
        configure(self.manager, port=9876)
        result = asyncio.run(session_create(title="MCP Review"))
        self.assertIn("session_id", result)
        self.assertIn("steps", result)
        self.assertIn("url", result)
        # Default steps should be created
        self.assertEqual(
            list(result["steps"].keys()),
            ["plan", "plan-audit", "build", "build-audit"],
        )

        session = self.manager.get_session(result["session_id"])
        self.assertIsNotNone(session)
        assert session is not None
        self.assertEqual(session.title, "MCP Review")

    def test_session_create_custom_steps(self) -> None:
        """session_create with custom steps creates only those steps."""
        configure(self.manager, port=9876)
        result = asyncio.run(session_create(title="Custom Steps", steps=["code-review", "docs"]))
        self.assertEqual(list(result["steps"].keys()), ["code-review", "docs"])

    def test_session_create_generates_slugified_id(self) -> None:
        """session_create generates a slugified session_id from the title."""
        configure(self.manager, port=9876)
        result = asyncio.run(session_create(title="My Great Review"))
        self.assertTrue(result["session_id"].startswith("my-great-review-"))


class TestSessionUpdate(ServerTestBase):
    """Tests for the session_update MCP tool."""

    def test_session_update_title(self) -> None:
        """session_update can update the title."""
        configure(self.manager, port=9876)
        self.manager.create_session("upd-test", "Original Title")
        result = asyncio.run(session_update(session_id="upd-test", title="New Title"))
        self.assertEqual(result["status"], "updated")
        self.assertEqual(result["title"], "New Title")

    def test_session_update_unknown_session(self) -> None:
        """session_update with unknown session returns error."""
        configure(self.manager, port=9876)
        result = asyncio.run(session_update(session_id="nonexistent", title="Nope"))
        self.assertIn("error", result)


class TestStepStart(ServerTestBase):
    """Tests for the step_start MCP tool."""

    def test_step_start_transitions_step(self) -> None:
        """step_start MCP tool transitions a pre-created step to STARTED."""
        configure(self.manager, port=9876)
        session = self.manager.create_session(
            session_id="step-test",
            title="Step Test",
            steps=["code-review"],
        )
        step_id = session.steps[0].step_id
        result = asyncio.run(step_start(session_id="step-test", step_id=step_id))
        self.assertEqual(result["status"], "started")
        self.assertEqual(result["step_name"], "code-review")
        self.assertEqual(result["step_id"], step_id)


class TestAgentStartStop(ServerTestBase):
    """Tests for the agent_start and agent_stop MCP tools."""

    def test_agent_start_registers_agent(self) -> None:
        """agent_start registers a running agent."""
        configure(self.manager, port=9876)
        self._create_session(session_id="agent-test")
        result = asyncio.run(
            agent_start(
                session_id="agent-test",
                step_id=self.step_id,
                name="lint-agent",
                description="Runs linting",
            )
        )
        self.assertEqual(result["status"], "started")
        self.assertEqual(result["agent_name"], "lint-agent")
        self.assertEqual(result["state"], "running")

    def test_agent_stop_completes_agent(self) -> None:
        """agent_stop marks agent as completed."""
        configure(self.manager, port=9876)
        self._create_session(session_id="stop-test")
        asyncio.run(
            agent_start(
                session_id="stop-test",
                step_id=self.step_id,
                name="lint-agent",
            )
        )
        result = asyncio.run(
            agent_stop(session_id="stop-test", step_id=self.step_id, name="lint-agent")
        )
        self.assertEqual(result["status"], "stopped")

    def test_agent_stop_unknown_agent_returns_error(self) -> None:
        """agent_stop with unknown agent name returns error."""
        configure(self.manager, port=9876)
        self._create_session(session_id="unknown-agent")
        result = asyncio.run(
            agent_stop(
                session_id="unknown-agent",
                step_id=self.step_id,
                name="nonexistent",
            )
        )
        self.assertIn("error", result)


class TestStepLog(ServerTestBase):
    """Tests for the step_log MCP tool."""

    def test_step_log_appends_entry(self) -> None:
        """step_log appends a log entry to the step."""
        configure(self.manager, port=9876)
        self._create_session(session_id="log-test")
        result = asyncio.run(
            step_log(
                session_id="log-test",
                step_id=self.step_id,
                agent_name="build-agent",
                message="Starting build...",
            )
        )
        self.assertEqual(result["status"], "ok")
        self.assertIn("timestamp", result)

        session = self.manager.get_session("log-test")
        assert session is not None
        self.assertEqual(len(session.steps[0].logs), 1)
        self.assertEqual(session.steps[0].logs[0].message, "Starting build...")


class TestReviewWait(ServerTestBase):
    """Tests for the review_wait MCP tool."""

    def test_review_wait_returns_correct_json(self) -> None:
        """review_wait returns correct JSON with full finding data."""
        configure(self.manager, port=9876)
        self._create_session(session_id="wait-session")

        # Add a finding, complete agent lifecycle, then submit responses
        self.manager.add_finding(
            "wait-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Unused import",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        self.manager.start_agent("wait-session", self.step_id, "test-agent")
        self.manager.stop_agent("wait-session", self.step_id, "test-agent")

        self.manager.submit_responses(
            "wait-session",
            self.step_id,
            [UserResponse(action=ResponseAction.ACCEPT)],
        )

        # Now review_wait should return immediately since the event is already set
        result = asyncio.run(review_wait(session_id="wait-session", step_id=self.step_id))

        self.assertEqual(result["session_id"], "wait-session")
        self.assertEqual(result["step_name"], _STEP)
        self.assertIsInstance(result["items"], list)

    def test_review_wait_blocks_until_submission(self) -> None:
        """review_wait blocks until the step is submitted."""
        configure(self.manager, port=9876)
        self._create_session(session_id="block-session")

        self.manager.add_finding(
            "block-session", self.step_id, {"type": "text", "title": "What do you think?"}
        )
        self.manager.start_agent("block-session", self.step_id, "test-agent")
        self.manager.stop_agent("block-session", self.step_id, "test-agent")

        async def _test_blocking() -> None:
            wait_started = asyncio.Event()
            completed = False

            async def do_wait() -> dict:
                nonlocal completed
                wait_started.set()
                result = await review_wait(session_id="block-session", step_id=self.step_id)
                completed = True
                return result

            task = asyncio.create_task(do_wait())

            # Wait until the review_wait coroutine has started
            await wait_started.wait()
            # Yield once more to ensure it's blocked on the internal event
            await asyncio.sleep(0)
            self.assertFalse(completed, "review_wait should block until submission")

            # Submit responses to unblock
            self.manager.submit_responses(
                "block-session",
                self.step_id,
                [UserResponse(answer="Looks good")],
            )

            result = await task
            self.assertTrue(completed)
            self.assertEqual(result["session_id"], "block-session")

        asyncio.run(_test_blocking())


class TestMCPFindingSubmit(ServerTestBase):
    """Tests for the finding_submit MCP tool."""

    def setUp(self) -> None:
        super().setUp()
        configure(self.manager, port=9876)

    def test_submit_text_finding(self) -> None:
        """finding_submit with a text finding stores it in the session step."""
        self._create_session(session_id="sf-text")
        result = asyncio.run(
            finding_submit(
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

    def test_submit_triage_finding_without_metadata(self) -> None:
        """finding_submit with a triage finding (no metadata) preserves options."""
        self._create_session(session_id="sf-triage-no-meta")
        result = asyncio.run(
            finding_submit(
                session_id="sf-triage-no-meta",
                step_id=self.step_id,
                finding={
                    "type": "triage",
                    "title": "Pick an approach",
                    "options": [
                        {"label": "Option A", "description": "First approach"},
                        {"label": "Option B", "description": "Second approach"},
                    ],
                },
            )
        )
        self.assertEqual(result["status"], "ok")

        session = self.manager.get_session("sf-triage-no-meta")
        assert session is not None
        finding = session.steps[0].findings[0]
        assert isinstance(finding, TriageFinding)
        self.assertEqual(finding.type, "triage")
        assert finding.options is not None
        self.assertEqual(len(finding.options), 2)
        self.assertEqual(finding.options[0].label, "Option A")
        self.assertEqual(finding.options[1].label, "Option B")

    def test_submit_to_unknown_session_returns_error(self) -> None:
        """finding_submit with a bogus step_id returns error dict."""
        result = asyncio.run(
            finding_submit(
                session_id="nonexistent",
                step_id="00000000-0000-0000-0000-000000000000",
                finding={"type": "text", "title": "Nope"},
            )
        )
        self.assertIn("error", result)

    def test_submit_to_completed_step_returns_error(self) -> None:
        """finding_submit to a completed step returns error dict."""
        self._create_session(session_id="sf-completed")

        # Add a finding, start+stop agent to transition to READY, then submit responses
        self.manager.add_finding(
            "sf-completed", self.step_id, {"type": "text", "title": "A finding"}
        )
        self.manager.start_agent("sf-completed", self.step_id, "test-agent")
        self.manager.stop_agent("sf-completed", self.step_id, "test-agent")

        self.manager.submit_responses("sf-completed", self.step_id, [UserResponse(answer="ok")])
        # Step is now COMPLETED — submitting a new finding should return error
        result = asyncio.run(
            finding_submit(
                session_id="sf-completed",
                step_id=self.step_id,
                finding={"type": "text", "title": "Too late"},
            )
        )
        self.assertIn("error", result)


class TestNotificationSend(ServerTestBase):
    """Tests for the notification_send MCP tool."""

    def setUp(self) -> None:
        super().setUp()
        configure(self.manager, port=9876)

    def test_notification_send_valid_session(self) -> None:
        """notification_send with a valid session returns status sent and notification_id."""
        self._create_session(session_id="notif-valid")
        result = asyncio.run(notification_send(session_id="notif-valid", title="Build complete"))
        self.assertEqual(result["status"], "sent")
        self.assertIn("notification_id", result)

    def test_notification_send_invalid_session(self) -> None:
        """notification_send with an invalid session_id returns an error dict."""
        result = asyncio.run(notification_send(session_id="nonexistent", title="Oops"))
        self.assertIn("error", result)

    def test_notification_send_body_and_url_stored(self) -> None:
        """notification_send stores optional body and url on the created notification."""
        self._create_session(session_id="notif-opts")
        result = asyncio.run(
            notification_send(
                session_id="notif-opts",
                title="Deploy ready",
                body="Version 2.1 is staged",
                url="https://example.com/deploy",
            )
        )
        self.assertEqual(result["status"], "sent")

        session = self.manager.get_session("notif-opts")
        assert session is not None
        notification = session.notifications[-1]
        self.assertEqual(notification.title, "Deploy ready")
        self.assertEqual(notification.body, "Version 2.1 is staged")
        self.assertEqual(notification.url, "https://example.com/deploy")
