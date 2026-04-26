"""Playwright UI tests for the Command Center review drawer."""

from __future__ import annotations

from datetime import UTC, datetime

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui


def _seed_session_with_findings(
    server: _ServerInfo,
    *,
    session_id: str = "drawer-1",
    ticket_id: str = "BAK-2001",
    title: str = "Drawer test",
    finding_id: str = "f-drawer",
) -> str:
    """Create a ZingSession with one ready build-audit step and a triage finding.

    Returns the workflow step_id so callers can target the submit endpoint.
    """
    cache = server.external_cache
    cache.issues = [
        LinearIssue(
            id="linear-uuid-drawer",
            identifier=ticket_id,
            title=title,
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url=f"https://linear.app/test/issue/{ticket_id}",
            updated_at=datetime.now(tz=UTC),
        )
    ]

    manager = server.manager
    session = manager.create_session(session_id=session_id, title=title, steps=["build-audit"])
    manager.update_session(session_id, ticket_id=ticket_id)
    step = session.steps[0]
    manager.start_step(session_id, step.step_id)
    manager.add_finding(
        session_id,
        step.step_id,
        {"type": "triage", "id": finding_id, "title": "Suspicious null check"},
    )
    manager.mark_step_ready(session_id, step.step_id)
    return step.step_id


def _open_drawer(server: _ServerInfo, page: Page) -> None:
    """Navigate to /command-center and click the first attention bar item."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    item = page.locator(".attn-item").first
    expect(item).to_be_visible(timeout=5000)
    item.click()

    drawer = page.locator("#review-drawer")
    expect(drawer).to_be_visible(timeout=5000)


def test_drawer_opens_from_attention_bar(server: _ServerInfo, page: Page) -> None:
    """Clicking an attention bar item fetches the drawer fragment and renders it."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    expect(page.locator("#review-drawer .dh-title")).to_contain_text("Drawer test", timeout=3000)
    expect(page.locator(".df-tb[data-action='accept']").first).to_be_visible(timeout=3000)


def test_drawer_close_button_dismisses_drawer(server: _ServerInfo, page: Page) -> None:
    """The header X (data-drawer-close) hides the drawer container."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    page.locator("#review-drawer .dh-close").click()
    expect(page.locator("#review-drawer-container")).to_be_hidden(timeout=3000)


def test_drawer_escape_key_dismisses_drawer(server: _ServerInfo, page: Page) -> None:
    """Pressing Escape closes the drawer (handled by the document keydown listener)."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    page.keyboard.press("Escape")
    expect(page.locator("#review-drawer-container")).to_be_hidden(timeout=3000)


def test_drawer_triage_button_toggles_state(server: _ServerInfo, page: Page) -> None:
    """Clicking Accept on a finding adds the .sa class and bumps the triage counter."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    counter = page.locator("#drawer-triage-count")
    expect(counter).to_have_text("0", timeout=3000)

    accept = page.locator(".df-tb[data-action='accept']").first
    accept.click()

    expect(accept).to_have_class("df-tb sa", timeout=3000)
    expect(counter).to_have_text("1", timeout=3000)


def test_drawer_step_toggle_accordion(server: _ServerInfo, page: Page) -> None:
    """Clicking a past-step header expands and collapses the step-content via Datastar signals."""
    cache = server.external_cache
    cache.issues = [
        LinearIssue(
            id="linear-uuid-accordion",
            identifier="BAK-2099",
            title="Accordion test",
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url="https://linear.app/test/issue/BAK-2099",
            updated_at=datetime.now(tz=UTC),
        )
    ]

    manager = server.manager
    # Two steps: plan (completes when build-audit starts) + build-audit (current/ready).
    session = manager.create_session(
        session_id="accordion-1", title="Accordion test", steps=["plan", "build-audit"]
    )
    manager.update_session("accordion-1", ticket_id="BAK-2099")
    plan_step = session.steps[0]
    audit_step = session.steps[1]

    manager.start_step("accordion-1", plan_step.step_id)
    manager.mark_step_ready("accordion-1", plan_step.step_id)
    # Starting audit auto-completes plan.
    manager.start_step("accordion-1", audit_step.step_id)
    manager.add_finding(
        "accordion-1",
        audit_step.step_id,
        {"type": "triage", "id": "f-acc", "title": "Accordion finding"},
    )
    manager.mark_step_ready("accordion-1", audit_step.step_id)

    _open_drawer(server, page)

    # The plan step-section should be present (past step) and initially collapsed.
    past_section = page.locator(f"#step-section-{plan_step.step_id}")
    expect(past_section).to_be_visible(timeout=5000)
    # step-content is display:none when .open is absent; confirm collapsed.
    expect(past_section.locator(".step-content")).to_be_hidden(timeout=3000)

    # Click the toggle button to expand.
    past_section.locator("button[data-step-toggle]").click()
    expect(past_section.locator(".step-content")).to_be_visible(timeout=3000)

    # Click again to collapse.
    past_section.locator("button[data-step-toggle]").click()
    expect(past_section.locator(".step-content")).to_be_hidden(timeout=3000)


def test_drawer_submit_posts_and_advances(server: _ServerInfo, page: Page) -> None:
    """Submit & Next POSTs to /{session_id}/submit with the gathered responses
    and marks the step COMPLETED on the server."""
    step_id = _seed_session_with_findings(server)
    _open_drawer(server, page)

    # Triage one finding so the submit body has a populated responses object.
    page.locator(".df-tb[data-action='accept']").first.click()

    # Capture the submit POST so we can assert on its payload.
    with page.expect_request(lambda r: "/submit" in r.url and r.method == "POST") as info:
        page.locator("[data-drawer-submit]").click()
    request = info.value
    assert request.post_data_json is not None
    assert request.post_data_json["step_id"] == step_id
    assert request.post_data_json["responses"]["f-drawer"] == "accept"

    # Server-side effect: the step is now COMPLETED.
    page.wait_for_function(
        "() => document.getElementById('review-drawer-container').style.display === 'none'",
        timeout=5000,
    )
    _, step = server.manager.get_step_by_id(step_id)
    assert step.state.value == "completed"
