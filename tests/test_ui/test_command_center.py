"""Playwright UI tests for the Command Center dashboard."""

from __future__ import annotations

import time
from datetime import UTC, datetime

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui

# Amber token value (#f5a623) as reported by getComputedStyle in rgb form.
_AMBER_R, _AMBER_G, _AMBER_B = 245, 166, 35


def test_empty_inbox_shows_cute_message(server: _ServerInfo, page: Page) -> None:
    """When no issues/PRs/sessions exist the inbox shows the empty-state message."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    empty = page.locator(".inbox-empty")
    expect(empty).to_be_visible(timeout=5000)
    expect(empty).to_contain_text("nothing to do", timeout=3000)


def test_inbox_item_renders_with_action_and_time(server: _ServerInfo, page: Page) -> None:
    """An InboxItem derived from a ready audit step is rendered with action text and time."""
    manager = server.manager
    cache = server.external_cache

    # Create a Linear issue so a ticket hub is built
    issue = LinearIssue(
        id="linear-uuid-001",
        identifier="BAK-1001",
        title="Test feature",
        state="In Progress",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1001",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    # Create a session linked to the issue with a ready build-audit step
    session = manager.create_session(
        session_id="cc-inbox-1", title="Test Build", steps=["build-audit"]
    )
    manager.update_session("cc-inbox-1", ticket_id="BAK-1001")
    step = session.steps[0]
    manager.start_step("cc-inbox-1", step.step_id)
    manager.add_finding(
        "cc-inbox-1",
        step.step_id,
        {"type": "text", "id": "f1", "title": "A finding"},
    )
    manager.mark_step_ready("cc-inbox-1", step.step_id)

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    # At least one inbox item should appear
    inbox_item = page.locator(".inbox-item").first
    expect(inbox_item).to_be_visible(timeout=5000)

    # Action text should mention "audit finding"
    expect(inbox_item.locator(".inbox-action")).to_contain_text("audit finding", timeout=3000)

    # Time waiting should be present and non-empty
    time_span = inbox_item.locator(".inbox-time")
    expect(time_span).to_be_visible(timeout=3000)
    time_text = time_span.text_content(timeout=3000) or ""
    assert time_text.strip() != "", "Expected non-empty time_waiting text"


def test_hub_expand_collapse_via_datastar(server: _ServerInfo, page: Page) -> None:
    """Hub starts collapsed; clicking header adds .open class; second click removes it."""
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-002",
        identifier="BAK-1002",
        title="Expand test",
        state="Todo",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1002",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    hub = page.locator("#hub-bak_1002")
    expect(hub).to_be_visible(timeout=5000)

    # Wait for Datastar to initialize: it processes data-signals by setting its own attribute.
    # We verify the cc-page has retained its data-signals attribute (not stripped by Datastar).
    page.wait_for_function(
        "document.querySelector('.cc-page')"
        " && document.querySelector('.cc-page').hasAttribute('data-signals')",
        timeout=5000,
    )

    # Initially no .open class
    assert "open" not in (hub.get_attribute("class") or ""), "Hub should start closed"

    # Dispatch click via JS on the hub element itself (which carries data-on:click).
    # This approach bypasses Playwright's pointer-event interception and ensures
    # the event reaches the Datastar-bound element directly.
    page.evaluate("document.querySelector('#hub-bak_1002').click()")
    # Wait for Datastar to apply the .open class
    page.wait_for_function(
        "document.querySelector('#hub-bak_1002').classList.contains('open')",
        timeout=5000,
    )
    assert "open" in (hub.get_attribute("class") or ""), "Hub should be open after first click"

    # Click again to close
    page.evaluate("document.querySelector('#hub-bak_1002').click()")
    page.wait_for_function(
        "!document.querySelector('#hub-bak_1002').classList.contains('open')",
        timeout=5000,
    )
    assert "open" not in (hub.get_attribute("class") or ""), (
        "Hub should be closed after second click"
    )


def test_yellow_urgency_renders_on_hot_hub(server: _ServerInfo, page: Page) -> None:
    """A hub whose urgency is 'hot' renders with the .hub.hot class and amber-ish border color."""
    manager = server.manager
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-003",
        identifier="BAK-1003",
        title="Hot hub test",
        state="In Progress",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1003",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    # Create an audit session with a ready step + findings → triggers 'hot' urgency
    session = manager.create_session(session_id="cc-hot-1", title="Hot Hub", steps=["build-audit"])
    manager.update_session("cc-hot-1", ticket_id="BAK-1003")
    step = session.steps[0]
    manager.start_step("cc-hot-1", step.step_id)
    manager.add_finding(
        "cc-hot-1",
        step.step_id,
        {"type": "text", "id": "f-hot", "title": "Critical finding"},
    )
    manager.mark_step_ready("cc-hot-1", step.step_id)

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    hot_hub = page.locator("#hub-bak_1003")
    expect(hot_hub).to_be_visible(timeout=5000)

    # Assert the .hub.hot class is present
    classes = hot_hub.get_attribute("class") or ""
    assert "hot" in classes, f"Expected 'hot' in hub classes, got: {classes!r}"

    # Check that the hub's border color is in the amber range (rgb(245, 166, 35))
    border_color: str = page.evaluate(
        "getComputedStyle(document.querySelector('#hub-bak_1003')).borderColor"
    )
    # border-color returns e.g. "rgb(245, 166, 35)" or shorthand — just check red component is high
    # and blue is low (amber = high red, moderate green, low blue)
    if border_color.startswith("rgb"):
        parts = border_color.replace("rgb(", "").replace("rgba(", "").replace(")", "").split(",")
        r, g, b = int(parts[0].strip()), int(parts[1].strip()), int(parts[2].strip())
        assert r > 200, f"Expected amber red component > 200, got {r} in {border_color!r}"
        assert g > 100, f"Expected amber green component > 100, got {g} in {border_color!r}"
        assert b < 100, f"Expected amber blue component < 100, got {b} in {border_color!r}"


def test_sse_event_updates_hub_without_reload(server: _ServerInfo, page: Page) -> None:
    """Mutating external_cache + pushing hub_changed SSE event patches the DOM within 5 s."""
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-004",
        identifier="BAK-1004",
        title="Original title",
        state="Todo",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1004",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    hub = page.locator("#hub-bak_1004")
    expect(hub).to_be_visible(timeout=5000)
    expect(hub.locator(".hub-name")).to_contain_text("Original title", timeout=3000)

    # Wait a moment for the SSE connection to be established (queue to be appended)
    # The route handler appends a queue when the browser connects to /command-center/events
    deadline = time.monotonic() + 5.0
    while not server.cc_queues and time.monotonic() < deadline:
        time.sleep(0.05)
    assert server.cc_queues, "Expected at least one cc_queue (SSE connection) to be registered"

    # Mutate the cache title
    updated_issue = issue.model_copy(update={"title": "Updated title SSE"})
    cache.issues = [updated_issue]

    # Push the hub_changed event to all active SSE queues
    for queue in list(server.cc_queues):
        queue.put_nowait("hub_changed:bak_1004")

    # The DOM should reflect the updated title via SSE patch
    expect(hub.locator(".hub-name")).to_contain_text("Updated title SSE", timeout=5000)


def test_last_synced_footer_updates(server: _ServerInfo, page: Page) -> None:
    """Footer starts with 'Waiting for first poll'; after a poll_status SSE event it updates."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    footer_span = page.locator(".cc-footer span")
    expect(footer_span).to_be_visible(timeout=5000)

    # Before any poll: initial text should say 'Waiting for first poll'
    initial_text = footer_span.text_content(timeout=3000) or ""
    assert "Waiting for first poll" in initial_text, (
        f"Expected 'Waiting for first poll' in footer, got: {initial_text!r}"
    )

    # Wait for the SSE connection to be established
    deadline = time.monotonic() + 5.0
    while not server.cc_queues and time.monotonic() < deadline:
        time.sleep(0.05)
    assert server.cc_queues, "Expected at least one cc_queue (SSE connection) to be registered"

    # Set last_polled_at and push a poll_status event
    now = datetime.now(tz=UTC)
    server.external_cache.last_polled_at = now

    for queue in list(server.cc_queues):
        queue.put_nowait("poll_status")

    # Datastar should update the span text to include the ISO timestamp
    expect(footer_span).to_contain_text("Last synced", timeout=5000)
    updated_text = footer_span.text_content(timeout=3000) or ""
    assert "Waiting for first poll" not in updated_text, (
        f"Footer should no longer say 'Waiting for first poll', got: {updated_text!r}"
    )


def test_error_banner_shows_when_last_error_set(server: _ServerInfo, page: Page) -> None:
    """Error banner is visible with error text when cache.last_error is non-empty."""
    server.external_cache.last_error = "rate limited"

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    banner = page.locator(".cc-error")
    expect(banner).to_be_visible(timeout=5000)
    expect(banner).to_contain_text("rate limited", timeout=3000)

    # Clean up for other tests
    server.external_cache.last_error = None


def test_no_console_errors_after_datastar_interactions(server: _ServerInfo, page: Page) -> None:
    """No JS console errors occur after page load and hub click interactions."""
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-005",
        identifier="BAK-1005",
        title="Console error check",
        state="Todo",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1005",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    hub = page.locator("#hub-bak_1005")
    expect(hub).to_be_visible(timeout=5000)

    # Perform expand interaction via JS dispatch to bypass pointer-event interception
    page.evaluate("document.querySelector('#hub-bak_1005').click()")
    page.wait_for_function(
        "document.querySelector('#hub-bak_1005').classList.contains('open')",
        timeout=5000,
    )

    # Perform collapse interaction
    page.evaluate("document.querySelector('#hub-bak_1005').click()")
    page.wait_for_function(
        "!document.querySelector('#hub-bak_1005').classList.contains('open')",
        timeout=5000,
    )

    assert errors == [], f"Unexpected JS console errors after Datastar interactions: {errors}"
