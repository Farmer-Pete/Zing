"""Cross-surface parity test for the shared render_finding macro.

The standalone review page (/<session_id>) and the command-center drawer
both render findings via fragments/_finding_macros.html. This test
parametrises a triage + complexity flow over both surfaces and asserts the
server ends up with the same WorkflowStep.responses shape — proving the two
UIs are wired to the same signal contract and per-action save endpoint.

Without this test the two surfaces can drift: prior to the unification, the
drawer was silently dropping every triage finding's complexity field and
losing free-text answers entirely on submit.
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


def _open_via_attention_bar(server: _ServerInfo, page: Page) -> None:
    """Open the drawer by clicking the first attention-bar item."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)
    item = page.locator(".attn-item").first
    expect(item).to_be_visible(timeout=5000)
    item.click()
    expect(page.locator("#review-drawer")).to_be_visible(timeout=5000)


@pytest.mark.parametrize("surface", ["standalone", "drawer"])
def test_triage_and_complexity_persist_identically(
    server: _ServerInfo, page: Page, surface: str
) -> None:
    """Accept + Complex on a triage finding persists the same UserResponse on both surfaces.

    This is the regression net for the drawer-vs-standalone-page divergence:
    if either surface stops emitting the macro's standard signal contract,
    one of the two parametrised cases will fail.
    """
    session_id = f"parity-{surface}"
    ticket_id = f"BAK-9{0 if surface == 'standalone' else 1}00"
    step_id = _seed_session(server, session_id=session_id, ticket_id=ticket_id)

    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    if surface == "standalone":
        page.goto(f"{server.base_url}/{session_id}")
        page.wait_for_load_state("networkidle", timeout=3000)
        finding_scope = page.locator("#finding-triage-1")
    else:
        _open_via_attention_bar(server, page)
        # Drawer scopes everything inside #review-drawer; multiple findings
        # would all match #finding-triage-1 if seeded across sessions, so
        # constrain to the drawer subtree.
        finding_scope = page.locator("#review-drawer #finding-triage-1")

    expect(finding_scope).to_be_visible(timeout=5000)

    accept_btn = finding_scope.locator(".action-btn[data-action='accept']")
    complex_btn = finding_scope.locator(".complexity-btn[data-complexity='complex']")

    # Click Accept — both surfaces post to /<sid>/save-response per-action.
    with page.expect_response(re.compile(r"/save-response$"), timeout=5000):
        accept_btn.click()
    expect(accept_btn).to_have_class(re.compile(r"\bselected\b"), timeout=3000)

    # Click Complex — same per-action save.
    with page.expect_response(re.compile(r"/save-response$"), timeout=5000):
        complex_btn.click()
    expect(complex_btn).to_have_class(re.compile(r"\bselected\b"), timeout=3000)

    # Server-side: the WorkflowStep.responses shape is identical regardless
    # of which surface drove the interaction.
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage_idx].complexity == Complexity.COMPLEX

    assert console_errors == [], f"Unexpected JS console errors on {surface}: {console_errors}"
