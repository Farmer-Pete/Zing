"""Playwright tests for the complexity selector UI on triage findings."""

from __future__ import annotations

import re

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models import Complexity, ResponseAction, UserResponse

pytestmark = pytest.mark.ui

_STEP = "review"


def _create_triage_session(
    server: _ServerInfo,
    session_id: str = "complexity-test",
    title: str = "Complexity Test",
    *,
    complexity: Complexity = Complexity.STANDARD,
) -> str:
    """Create a READY session with a single triage finding. Returns step_id."""
    manager = server.manager
    session = manager.create_session(
        session_id=session_id, title=title, steps=[_STEP]
    )
    step_id = session.steps[0].step_id
    manager.start_step(session_id, step_id)

    manager.add_finding(session_id, step_id, {
        "type": "triage",
        "id": "triage-1",
        "title": "Test Finding",
        "body": "A test triage finding.",
        "category": "security",
        "severity": "high",
        "confidence": "high",
        "complexity": complexity.value,
    })

    manager.mark_step_ready(session_id, step_id)
    return step_id


def _wait_for_datastar(page: Page) -> None:
    """Wait for Datastar JS to load and initialize."""
    page.wait_for_load_state("networkidle", timeout=3000)


def _assert_no_console_errors(errors: list[str]) -> None:
    """Assert that no JS console errors were collected."""
    assert errors == [], f"Unexpected JS console errors: {errors}"


# ---------------------------------------------------------------------------
# Test: Complexity selector renders with correct default
# ---------------------------------------------------------------------------


def test_complexity_selector_renders_with_default(server: _ServerInfo, page: Page) -> None:
    """Complexity selector renders three buttons with 'Standard' selected by default."""
    _create_triage_session(server, session_id="cx-render")

    page.goto(f"{server.base_url}/cx-render")
    _wait_for_datastar(page)

    selector = page.locator("#finding-triage-1 .complexity-selector")
    expect(selector).to_be_visible(timeout=3000)

    # All three buttons should be present
    buttons = page.locator("#finding-triage-1 .complexity-btn")
    expect(buttons).to_have_count(3, timeout=3000)

    # Verify button labels
    expect(buttons.nth(0)).to_have_text("Simple", timeout=3000)
    expect(buttons.nth(1)).to_have_text("Standard", timeout=3000)
    expect(buttons.nth(2)).to_have_text("Complex", timeout=3000)

    # "Standard" should have the 'selected' class (default)
    standard_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='standard']")
    expect(standard_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Other two should NOT have 'selected'
    simple_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='simple']")
    expect(simple_btn).not_to_have_class(re.compile("selected"), timeout=3000)

    complex_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='complex']")
    expect(complex_btn).not_to_have_class(re.compile("selected"), timeout=3000)


def test_complexity_selector_renders_non_default(server: _ServerInfo, page: Page) -> None:
    """Complexity selector highlights the finding's complexity when not 'standard'."""
    _create_triage_session(server, session_id="cx-simple", complexity=Complexity.SIMPLE)

    page.goto(f"{server.base_url}/cx-simple")
    _wait_for_datastar(page)

    # "Simple" should have the 'selected' class
    simple_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='simple']")
    expect(simple_btn).to_have_class(re.compile("selected"), timeout=3000)

    # "Standard" should NOT
    standard_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='standard']")
    expect(standard_btn).not_to_have_class(re.compile("selected"), timeout=3000)


# ---------------------------------------------------------------------------
# Test: Clicking a complexity button updates UI and persists via POST
# ---------------------------------------------------------------------------


def test_complexity_click_updates_ui_and_persists(server: _ServerInfo, page: Page) -> None:
    """Clicking a different complexity button toggles selection and saves to server."""
    step_id = _create_triage_session(server, session_id="cx-click")

    page.goto(f"{server.base_url}/cx-click")
    _wait_for_datastar(page)

    # Default: Standard is selected
    standard_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='standard']")
    expect(standard_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Click "Complex" — should POST to save-response
    complex_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='complex']")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        complex_btn.click()
    assert resp_info.value.status == 200

    # UI: Complex should now be selected, Standard should not
    expect(complex_btn).to_have_class(re.compile("selected"), timeout=3000)
    expect(standard_btn).not_to_have_class(re.compile("selected"), timeout=3000)

    # Server-side: complexity should be "complex"
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].complexity == Complexity.COMPLEX

    # Now click "Simple" — should switch again
    simple_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='simple']")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        simple_btn.click()
    assert resp_info.value.status == 200

    expect(simple_btn).to_have_class(re.compile("selected"), timeout=3000)
    expect(complex_btn).not_to_have_class(re.compile("selected"), timeout=3000)
    expect(standard_btn).not_to_have_class(re.compile("selected"), timeout=3000)

    # Verify server state updated
    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses[triage_idx].complexity == Complexity.SIMPLE


# ---------------------------------------------------------------------------
# Test: Complexity works with pre-existing saved responses (reload scenario)
# ---------------------------------------------------------------------------


def test_complexity_default_shows_after_reload_with_saved_responses(
    server: _ServerInfo, page: Page
) -> None:
    """After saving a triage action and reloading, complexity still shows the finding default.

    The complexity signal is initialized from ``finding.complexity.value`` via
    ``data-signals`` on the finding element. Saved complexity overrides are not
    currently restored into the signal store on page load, so the selector
    reflects the finding's default after a reload.
    """
    step_id = _create_triage_session(server, session_id="cx-reload")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    # Save a triage action server-side (the common pre-existing state scenario)
    server.manager.save_response(
        "cx-reload", step_id, "triage-1",
        UserResponse(action=ResponseAction.ACCEPT),
    )

    # Navigate to page with pre-existing state
    page.goto(f"{server.base_url}/cx-reload")
    _wait_for_datastar(page)

    # Triage action should be restored
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    expect(accept_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Complexity selector should render with the finding's default (standard)
    standard_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='standard']")
    expect(standard_btn).to_have_class(re.compile("selected"), timeout=3000)

    # Should be able to change complexity and POST succeeds
    complex_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='complex']")
    with page.expect_response("**/save-response", timeout=3000) as resp_info:
        complex_btn.click()
    assert resp_info.value.status == 200

    expect(complex_btn).to_have_class(re.compile("selected"), timeout=3000)

    _assert_no_console_errors(console_errors)


# ---------------------------------------------------------------------------
# Test: No JS console errors during complexity interactions
# ---------------------------------------------------------------------------


def test_complexity_interaction_no_console_errors(server: _ServerInfo, page: Page) -> None:
    """Clicking all three complexity buttons produces no JS console errors."""
    _create_triage_session(server, session_id="cx-errors")
    console_errors: list[str] = []
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/cx-errors")
    _wait_for_datastar(page)

    # Click each complexity button in sequence
    for complexity in ("simple", "complex", "standard"):
        btn = page.locator(f"#finding-triage-1 .complexity-btn[data-complexity='{complexity}']")
        with page.expect_response("**/save-response", timeout=3000):
            btn.click()
        expect(btn).to_have_class(re.compile("selected"), timeout=3000)

    # Allow any async errors to surface
    page.wait_for_timeout(500)

    _assert_no_console_errors(console_errors)


# ---------------------------------------------------------------------------
# Test: Complexity persists alongside triage action (merged save)
# ---------------------------------------------------------------------------


def test_complexity_persists_alongside_action(server: _ServerInfo, page: Page) -> None:
    """Saving complexity does not clobber a previously saved triage action, and vice versa."""
    step_id = _create_triage_session(server, session_id="cx-merge")

    page.goto(f"{server.base_url}/cx-merge")
    _wait_for_datastar(page)

    # First: set complexity to "complex"
    complex_btn = page.locator("#finding-triage-1 .complexity-btn[data-complexity='complex']")
    with page.expect_response("**/save-response", timeout=3000):
        complex_btn.click()

    # Verify only complexity is saved (no action yet)
    _, step = server.manager.get_step_by_id(step_id)
    triage_idx = next(i for i, f in enumerate(step.findings) if f.id == "triage-1")
    assert step.responses is not None
    assert step.responses[triage_idx].complexity == Complexity.COMPLEX
    # action may or may not be set depending on signal state — what matters
    # is that after we set the action, complexity is preserved

    # Second: click Accept action
    accept_btn = page.locator("#finding-triage-1 .action-btn", has_text="Accept")
    with page.expect_response("**/save-response", timeout=3000):
        accept_btn.click()

    # Both should be preserved via merge
    _, step = server.manager.get_step_by_id(step_id)
    assert step.responses[triage_idx].action == ResponseAction.ACCEPT
    assert step.responses[triage_idx].complexity == Complexity.COMPLEX
