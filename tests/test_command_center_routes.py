"""Tests for Command Center route endpoints."""

from __future__ import annotations

import tempfile
import unittest
from datetime import datetime
from pathlib import Path

from fastapi import FastAPI
from fastapi.testclient import TestClient

from zing_ai.server.app import create_app
from zing_ai.server.models import Session, SessionState, WorkflowStep
from zing_ai.server.models_external import GitHubPR, LinearIssue
from zing_ai.server.sessions import SessionManager


def _make_issue(
    *,
    identifier: str = "BAK-1",
    title: str = "Fix bug",
    team: str = "Back End",
    assignee: str | None = "alice",
) -> LinearIssue:
    return LinearIssue(
        id="uuid-" + identifier,
        identifier=identifier,
        title=title,
        state="In Progress",
        assignee=assignee,
        team=team,
        url=f"https://linear.app/t/{identifier}",
        updated_at=datetime(2026, 4, 16, 0, 0, 0),
    )


def _make_pr(
    *,
    number: int = 1,
    title: str = "Title",
    head_ref: str = "feature",
    body: str | None = None,
) -> GitHubPR:
    return GitHubPR(
        number=number,
        title=title,
        state="open",
        draft=False,
        head_ref=head_ref,
        base_ref="main",
        body=body,
        requested_reviewers=[],
        review_decision=None,
        mergeable_state="clean",
        ci_status=None,
        url=f"https://github.com/o/r/pull/{number}",
        updated_at=datetime(2026, 4, 16, 0, 0, 0),
    )


def _make_workflow_step(*, step_name: str, sequence: int = 0) -> WorkflowStep:
    return WorkflowStep(step_name=step_name, sequence=sequence)


class CommandCenterTestBase(unittest.TestCase):
    """Base class that sets up a TestClient with an isolated SessionManager."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)
        self.app_instance = create_app(session_manager=self.manager)
        self.client = TestClient(self.app_instance)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _get_fastapi_app(self) -> FastAPI:
        """Unwrap the ASGI middleware stack to reach the FastAPI instance."""
        # MCPDebugMiddleware wraps a Starlette app; the last Mount route is fastapi_app.
        starlette_app = self.app_instance.app  # type: ignore[attr-defined]
        for route in starlette_app.routes:
            app = getattr(route, "app", None)
            if isinstance(app, FastAPI):
                return app
        raise RuntimeError("Could not locate FastAPI app inside ASGI stack")


class TestCommandCenterRoutes(CommandCenterTestBase):
    """Tests for the /command-center endpoint."""

    def test_get_command_center_returns_200_with_empty_cache(self) -> None:
        """GET /command-center returns 200 with an empty ExternalCache."""
        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("text/html", resp.headers["content-type"])

    def test_page_contains_command_center_in_nav(self) -> None:
        """The response includes the Command Center nav link."""
        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("Command Center", resp.text)

    def test_empty_inbox_shows_cute_message(self) -> None:
        """With no inbox items the inbox renders the empty-state message."""
        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("nothing to do", resp.text)

    def test_hub_renders_with_signal_key_attributes(self) -> None:
        """Injecting an issue produces a hub with correct Datastar signal key attributes."""
        fastapi_app = self._get_fastapi_app()
        issue = _make_issue(identifier="BAK-1179", title="Search broken")
        fastapi_app.state.external_cache.issues = [issue]

        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        # signal_key = "BAK-1179".lower().replace("-", "_") → "bak_1179"
        self.assertIn('data-signals:open.bak_1179="false"', resp.text)

    def test_yellow_urgency_renders_on_hot_hub(self) -> None:
        """A hub with a READY audit step and findings renders with class 'hot'."""
        fastapi_app = self._get_fastapi_app()
        issue = _make_issue(identifier="BAK-9000", title="Hot issue")

        # Build a session with an audit step in READY state with a finding.
        audit_step = _make_workflow_step(step_name="build-audit", sequence=0)
        audit_step.state = SessionState.READY
        # Add a minimal finding so _compute_urgency sees findings as truthy.
        from zing_ai.server.models import TextFinding

        audit_step.findings = [TextFinding(title="Found something")]  # type: ignore[list-item]

        session = Session(
            session_id="sess-hot",
            title="Hot Session",
            ticket_id="BAK-9000",
            steps=[audit_step],
        )
        self.manager._sessions["sess-hot"] = session  # type: ignore[attr-defined]

        fastapi_app.state.external_cache.issues = [issue]

        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("hot", resp.text)
