"""Parity test for the shared render_finding macro on the standalone review page.

The standalone review page (/<session_id>) renders findings via
fragments/_finding_macros.html. This test exercises a triage + complexity
flow and asserts the server ends up with the expected WorkflowStep.responses
shape — proving the UI is wired to the correct signal contract and per-action
save endpoint.
"""

from __future__ import annotations

import re
from datetime import UTC, datetime

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models import Complexity, ResponseAction
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui


def _seed_session(server: _ServerInfo, session_id: str, ticket_id: str) -> str:
    """Create a ZingSession with one ready triage finding. Returns step_id."""
    server.external_cache.issues = list(server.external_cache.issues) + [
        LinearIssue(
            id=f"uuid-{session_id}",
            identifier=ticket_id,
            title=f"Parity {session_id}",
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url=f"https://linear.app/test/issue/{ticket_id}",
            updated_at=datetime.now(tz=UTC),
        )
    ]
    manager = server.manager
    session = manager.create_session(
        session_id=session_id, title=f"Parity {session_id}", steps=["build-audit"]
    )
    manager.update_session(session_id, ticket_id=ticket_id)
    step = session.steps[0]
    manager.start_step(session_id, step.step_id)
    manager.add_finding(
        session_id,
        step.step_id,
        {
            "type": "triage",
            "id": "triage-1",
            "title": "Parity finding",
            "category": "correctness",
            "severity": "high",
            "confidence": "high",
        },
    )
    manager.mark_step_ready(session_id, step.step_id)
    return step.step_id


def test_triage_and_complexity_persist(server: _ServerInfo, page: Page) -> None:
    """Accept + Complex on a triage finding persists the expected UserResponse.

    Exercises the standard signal contract and per-action save endpoint on
    the standalone review page.
    """
    session_id = "parity-standalone"
    ticket_id = "BAK-9000"
    step_id = _seed_session(server, session_id=session_id, ticket_id=ticket_id)

    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/{session_id}")
    page.wait_for_load_state("networkidle", timeout=3000)
    finding_scope = page.locator("#finding-triage-1")

    expect(finding_scope).to_be_visible(timeout=5000)

    accept_btn = finding_scope.locator(".action-btn[data-action='accept']")
    complex_btn = finding_scope.locator(".complexity-btn[data-complexity='complex']")

    # Click Accept — posts to /<sid>/save-response per-action.
    with page.expect_response(re.compile(r"/save-response$"), timeout=5000):
        accept_btn.click()
    expect(accept_btn).to_have_class(re.compile(r"\bselected\b"), timeout=3000)

    # Click Complex — same per-action save.
    with page.expect_response(re.compile(r"/save-response$"), timeout=5000):
        complex_btn.click()
    expect(complex_btn).to_have_class(re.compile(r"\bselected\b"), timeout=3000)

    # Server-side: the WorkflowStep.responses shape must match expectations.
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage_idx].complexity == Complexity.COMPLEX

    assert console_errors == [], f"Unexpected JS console errors: {console_errors}"
