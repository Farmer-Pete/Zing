"""Playwright tests for the Zing dashboard page."""

from __future__ import annotations

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models import UserResponse

pytestmark = pytest.mark.ui


def test_dashboard_loads_with_session_cards(server: _ServerInfo, page: Page) -> None:
    """Dashboard displays session cards for existing sessions."""
    manager = server.manager
    manager.create_session(session_id="dash-1", title="First Session", steps=["review"])
    manager.create_session(session_id="dash-2", title="Second Session", steps=["review"])

    page.goto(f"{server.base_url}/dashboard")

    expect(page.locator(".timeline-card")).to_have_count(2, timeout=3000)
    expect(page.locator(".timeline-title", has_text="First Session")).to_be_visible(timeout=3000)
    expect(page.locator(".timeline-title", has_text="Second Session")).to_be_visible(timeout=3000)


def test_dashboard_empty_state(server: _ServerInfo, page: Page) -> None:
    """Dashboard shows empty message when no sessions exist."""
    page.goto(f"{server.base_url}/dashboard")

    expect(page.locator("text=No sessions yet")).to_be_visible(timeout=3000)


def test_dashboard_reflects_new_session_on_reload(server: _ServerInfo, page: Page) -> None:
    """Dashboard shows newly created sessions after page reload."""
    page.goto(f"{server.base_url}/dashboard")
    expect(page.locator("text=No sessions yet")).to_be_visible(timeout=3000)

    # Create a session server-side
    manager = server.manager
    manager.create_session(session_id="reload-new", title="Reload Created", steps=["review"])

    # Reload to see the new session
    page.reload()
    expect(page.locator(".timeline-title", has_text="Reload Created")).to_be_visible(timeout=3000)


def test_dashboard_delete_removes_session(server: _ServerInfo, page: Page) -> None:
    """Clicking Delete button removes the session via Datastar @post."""
    manager = server.manager
    manager.create_session(session_id="del-1", title="Delete Me", steps=["review"])

    page.goto(f"{server.base_url}/dashboard")
    expect(page.locator(".timeline-title", has_text="Delete Me")).to_be_visible(timeout=3000)

    # Auto-accept the confirm() dialog triggered by the cleanup button
    page.on("dialog", lambda dialog: dialog.accept())
    page.locator(".cleanup-btn").click()

    # After cleanup, the dashboard redirects and session should be gone
    page.wait_for_url("**/dashboard", timeout=5000)
    page.wait_for_load_state("domcontentloaded", timeout=5000)
    expect(page.locator(".timeline-title", has_text="Delete Me")).not_to_be_visible(timeout=5000)


def test_dashboard_status_badges(server: _ServerInfo, page: Page) -> None:
    """Status badges reflect session state correctly."""
    manager = server.manager
    session = manager.create_session(session_id="badge-1", title="Badge Test", steps=["review"])
    step = session.steps[0]
    manager.start_step("badge-1", step.step_id)

    page.goto(f"{server.base_url}/dashboard")

    badge = page.locator(".status-badge.status-started")
    expect(badge).to_be_visible(timeout=3000)
    expect(badge).to_have_text("started", timeout=3000)


def test_dashboard_session_state_on_reload(server: _ServerInfo, page: Page) -> None:
    """Dashboard badges update when session state changes between page loads."""
    manager = server.manager
    session = manager.create_session(
        session_id="state-reload", title="State Reload", steps=["review"]
    )
    step = session.steps[0]
    step_id = step.step_id
    manager.start_step("state-reload", step_id)

    # Add a finding so submit works
    manager.add_finding(
        "state-reload",
        step_id,
        {
            "type": "triage",
            "id": "t1",
            "title": "Finding",
            "category": "security",
            "severity": "high",
            "confidence": "high",
        },
    )
    manager.mark_step_ready("state-reload", step_id)

    page.goto(f"{server.base_url}/dashboard")
    expect(page.locator(".status-badge.status-ready")).to_be_visible(timeout=3000)

    # Submit responses server-side to transition to completed
    manager.submit_responses("state-reload", step_id, [UserResponse()])

    page.reload()
    expect(page.locator(".status-badge.status-completed")).to_be_visible(timeout=3000)


def test_404_for_nonexistent_session(server: _ServerInfo, page: Page) -> None:
    """Navigating to a nonexistent session returns 404."""
    response = page.goto(f"{server.base_url}/nonexistent-session-id")
    assert response is not None
    assert response.status == 404


def test_dashboard_live_status_update(server: _ServerInfo, page: Page) -> None:
    """Dashboard badges update in real time via SSE without page reload."""
    manager = server.manager
    session = manager.create_session(
        session_id="live-dash", title="Live Dashboard", steps=["review"]
    )
    step = session.steps[0]
    step_id = step.step_id
    manager.start_step("live-dash", step_id)

    page.goto(f"{server.base_url}/dashboard")
    # Badge should show "started" from the SSE connection
    badge = page.locator("#session-card-live-dash .status-badge")
    expect(badge).to_have_text("started", timeout=5000)

    # Mark step ready server-side — badge should update without reload
    manager.add_finding(
        "live-dash",
        step_id,
        {
            "type": "text",
            "id": "f1",
            "title": "Note",
        },
    )
    manager.start_agent("live-dash", step_id, "test-agent")
    manager.stop_agent("live-dash", step_id, "test-agent")
    manager.mark_step_ready("live-dash", step_id)
    expect(badge).to_have_text("ready", timeout=5000)

    # Submit responses server-side — badge should update to completed
    manager.submit_responses("live-dash", step_id, [UserResponse()])
    expect(badge).to_have_text("completed", timeout=5000)


def test_submit_to_completed_step_is_disabled(server: _ServerInfo, page: Page) -> None:
    """Submit button is disabled after completion; forced POST returns 409."""
    manager = server.manager
    session = manager.create_session(
        session_id="completed-submit", title="Completed Submit", steps=["review"]
    )
    step = session.steps[0]
    step_id = step.step_id
    manager.start_step("completed-submit", step_id)

    manager.add_finding(
        "completed-submit",
        step_id,
        {
            "type": "triage",
            "id": "t1",
            "title": "Finding",
            "category": "security",
            "severity": "high",
            "confidence": "high",
        },
    )
    manager.mark_step_ready("completed-submit", step_id)

    page.goto(f"{server.base_url}/completed-submit")
    page.wait_for_load_state("networkidle", timeout=3000)

    # Submit via UI
    with page.expect_response("**/submit", timeout=3000):
        page.locator(".submit-btn").click()
    expect(page.locator(".submit-btn")).to_be_disabled(timeout=3000)

    # Force a POST to the submit endpoint — should get 409
    api_response = page.request.post(
        f"{server.base_url}/completed-submit/submit",
        data={"step_id": step_id, "responses": []},
    )
    assert api_response.status == 409


def test_notif_opt_in_visible_when_permission_default(server: _ServerInfo, page: Page) -> None:
    """Notification opt-in button is visible when permission is 'default'."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    page.add_init_script("""
        window.Notification = {
            permission: 'default',
            requestPermission: () => Promise.resolve('default'),
        };
    """)
    page.goto(f"{server.base_url}/dashboard")
    page.wait_for_load_state("domcontentloaded", timeout=3000)

    btn = page.locator("#notif-opt-in")
    expect(btn).to_be_visible(timeout=3000)
    assert errors == [], f"JS console errors: {errors}"


def test_notif_opt_in_hidden_when_permission_granted(server: _ServerInfo, page: Page) -> None:
    """Notification opt-in button is hidden when permission is already 'granted'."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    page.add_init_script("""
        window.Notification = {
            permission: 'granted',
            requestPermission: () => Promise.resolve('granted'),
        };
    """)
    page.goto(f"{server.base_url}/dashboard")
    page.wait_for_load_state("domcontentloaded", timeout=3000)

    btn = page.locator("#notif-opt-in")
    expect(btn).not_to_be_visible(timeout=3000)
    assert errors == [], f"JS console errors: {errors}"


def test_notif_opt_in_click_hides_button(server: _ServerInfo, page: Page) -> None:
    """Clicking the opt-in button calls requestPermission and hides the button on 'granted'."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    page.add_init_script("""
        window.Notification = {
            permission: 'default',
            requestPermission: () => Promise.resolve('granted'),
        };
    """)
    page.goto(f"{server.base_url}/dashboard")
    page.wait_for_load_state("domcontentloaded", timeout=3000)

    btn = page.locator("#notif-opt-in")
    expect(btn).to_be_visible(timeout=3000)
    btn.click()
    expect(btn).not_to_be_visible(timeout=3000)
    assert errors == [], f"JS console errors: {errors}"


def test_notification_timeline_appears_with_notifications(server: _ServerInfo, page: Page) -> None:
    """Notification timeline appears under session card when notifications exist."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    manager = server.manager
    manager.create_session(session_id="timeline-1", title="Timeline Test", steps=["review"])
    # create_session auto-adds a notification, so timeline should have entries
    page.goto(f"{server.base_url}/dashboard")

    timeline = page.locator("#notifications-timeline-1")
    expect(timeline).to_be_visible(timeout=3000)
    expect(page.locator("#notifications-timeline-1 .notification-entry")).to_have_count(
        1, timeout=3000
    )
    assert errors == [], f"JS console errors: {errors}"


def test_notification_timeline_live_update(server: _ServerInfo, page: Page) -> None:
    """Timeline updates live when new notification arrives via SSE."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    manager = server.manager
    manager.create_session(session_id="live-tl", title="Live Timeline", steps=["review"])

    page.goto(f"{server.base_url}/dashboard")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    # Should have 1 notification entry from auto-notification on create
    timeline = page.locator("#notifications-live-tl")
    expect(timeline).to_be_visible(timeout=3000)
    entries = page.locator("#notifications-live-tl .notification-entry")
    expect(entries).to_have_count(1, timeout=3000)

    # Add a notification server-side — timeline should update via SSE
    manager.add_notification("live-tl", title="Agent finished", body="Review is ready")
    expect(entries).to_have_count(2, timeout=5000)
    notif_title = page.locator(
        "#notifications-live-tl .notification-title",
        has_text="Agent finished",
    )
    expect(notif_title).to_be_visible(timeout=3000)
    assert errors == [], f"JS console errors: {errors}"
