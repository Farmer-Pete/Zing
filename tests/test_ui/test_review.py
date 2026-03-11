"""Playwright tests for the Zing review page."""

from __future__ import annotations

import re

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui

_STEP = "review"


def _create_ready_session(
    server: _ServerInfo,
    session_id: str = "rev-test",
    title: str = "Review Test",
) -> str:
    """Create a session in READY state with sample findings. Returns step_id."""
    manager = server.manager
    session = manager.create_session(
        session_id=session_id, title=title, steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step(session_id, step_id)

    # Add one of each finding type
    manager.add_finding(session_id, step_id, {
        "type": "triage",
        "id": "triage-1",
        "title": "SQL Injection Risk",
        "body": "Potential SQL injection in query builder.",
        "category": "security",
        "severity": "high",
        "confidence": "high",
    })
    manager.add_finding(session_id, step_id, {
        "type": "text",
        "id": "text-1",
        "title": "Describe your testing strategy",
        "body": "How will you test this change?",
    })
    manager.add_finding(session_id, step_id, {
        "type": "choice",
        "id": "choice-1",
        "title": "Pick an approach",
        "options": [
            {"label": "Option A", "description": "First approach"},
            {"label": "Option B", "description": "Second approach"},
        ],
    })

    manager.mark_step_ready(session_id, step_id)
    return step_id


def _wait_for_datastar(page: Page) -> None:
    """Wait for Datastar JS to load and initialize."""
    page.wait_for_load_state("networkidle", timeout=3000)


def test_review_page_shows_findings(server: _ServerInfo, page: Page) -> None:
    """Ready review page pre-renders all findings."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")

    expect(page.locator("#finding-triage-1")).to_be_visible(timeout=3000)
    expect(page.locator("#finding-text-1")).to_be_visible(timeout=3000)
    expect(page.locator("#finding-choice-1")).to_be_visible(timeout=3000)
    expect(page.locator("text=SQL Injection Risk")).to_be_visible(timeout=3000)


def test_triage_button_toggles_selected_class(server: _ServerInfo, page: Page) -> None:
    """Clicking a triage action button toggles the 'selected' class via Datastar."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    expect(page.locator("#finding-triage-1")).to_be_visible(timeout=3000)
    _wait_for_datastar(page)

    # Click the "Accept" button — Datastar should add 'selected' class
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    accept_btn.click()
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Click "Drop" — Accept should lose 'selected', Drop should gain it
    drop_btn = page.locator("#finding-triage-1 .action-btn", has_text="Drop")
    drop_btn.click()
    expect(drop_btn).to_have_class(re.compile("selected"), timeout=3000)
    expect(accept_btn).not_to_have_class(re.compile("selected"), timeout=3000)


def test_text_finding_textarea_binding(server: _ServerInfo, page: Page) -> None:
    """Text finding textarea is bound to Datastar signals via data-bind."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    expect(page.locator("#finding-text-1")).to_be_visible(timeout=3000)
    _wait_for_datastar(page)

    textarea = page.locator("#finding-text-1 textarea")
    textarea.fill("My testing strategy is comprehensive.")
    expect(textarea).to_have_value("My testing strategy is comprehensive.", timeout=3000)


def test_choice_radio_selection(server: _ServerInfo, page: Page) -> None:
    """Selecting a radio button in a choice finding updates Datastar signals."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    expect(page.locator("#finding-choice-1")).to_be_visible(timeout=3000)
    _wait_for_datastar(page)

    radio = page.locator("#finding-choice-1 input[value='Option A']")
    radio.click()
    expect(radio).to_be_checked(timeout=3000)


def test_submit_review_completes_workflow(server: _ServerInfo, page: Page) -> None:
    """Clicking Submit Review button completes the workflow step."""
    _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    _wait_for_datastar(page)

    submit_btn = page.locator(".submit-btn")
    expect(submit_btn).to_be_visible(timeout=3000)
    expect(submit_btn).to_have_text("Submit Review", timeout=3000)

    with page.expect_response("**/submit", timeout=3000):
        submit_btn.click()

    # After submit, banner should show success
    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    # Button should be disabled
    expect(page.locator(".submit-btn")).to_be_disabled(timeout=3000)


def test_submit_captures_triage_response(server: _ServerInfo, page: Page) -> None:
    """Submit sends triage action selection to the server via Datastar signals."""
    step_id = _create_ready_session(server)

    page.goto(f"{server.base_url}/rev-test")
    _wait_for_datastar(page)

    # Select "Accept" on the triage finding
    page.locator("#finding-triage-1 .action-btn", has_text="Accept").click()

    # Select "Option A" on the choice finding
    page.locator("#finding-choice-1 input[value='Option A']").click()

    # Submit
    with page.expect_response("**/submit", timeout=3000):
        page.locator(".submit-btn").click()

    expect(page.locator("#review-status")).to_contain_text("Review submitted", timeout=3000)

    # Verify server-side state
    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses is not None
    assert len(step.responses) == 3

    # Triage finding: action should be "accept"
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses[triage_idx].action is not None
    assert step.responses[triage_idx].action.value == "accept"

    # Choice finding: selected should be "Option A"
    choice_idx = next(i for i, f in enumerate(step.findings) if f.id == "choice-1")
    assert step.responses[choice_idx].selected == "Option A"


def test_sse_streams_findings_into_dom(server: _ServerInfo, page: Page) -> None:
    """SSE streams new findings into the DOM as they arrive."""
    manager = server.manager
    session = manager.create_session(
        session_id="sse-stream", title="SSE Stream Test", steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step("sse-stream", step_id)

    # Navigate while step is in STARTED state (SSE will be active)
    page.goto(f"{server.base_url}/sse-stream")

    # Now add a finding server-side — it should appear via SSE
    manager.add_finding("sse-stream", step_id, {
        "type": "triage",
        "id": "sse-finding-1",
        "title": "SSE Streamed Finding",
        "category": "correctness",
        "severity": "medium",
        "confidence": "high",
    })

    expect(page.locator("#finding-sse-finding-1")).to_be_visible(timeout=3000)
    expect(page.locator("text=SSE Streamed Finding")).to_be_visible(timeout=3000)


def test_tab_navigation(server: _ServerInfo, page: Page) -> None:
    """Tab navigation works between workflow steps."""
    manager = server.manager
    manager.create_session(
        session_id="tab-test", title="Tab Test", steps=["audit", "review"]
    )

    page.goto(f"{server.base_url}/tab-test")

    # Both step tabs should be visible
    expect(page.locator(".step-link", has_text="audit")).to_be_visible(timeout=3000)
    expect(page.locator(".step-link", has_text="review")).to_be_visible(timeout=3000)

    # Click the audit tab
    page.locator(".step-link", has_text="audit").click()

    # URL should update with the step parameter
    page.wait_for_url("**/tab-test?step=*", timeout=3000)
