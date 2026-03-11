"""Playwright tests for the Zing dashboard page."""

from __future__ import annotations

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui


def test_dashboard_loads_with_session_cards(server: _ServerInfo, page: Page) -> None:
    """Dashboard displays session cards for existing sessions."""
    manager = server.manager
    manager.create_session(session_id="dash-1", title="First Session", steps=["review"])
    manager.create_session(session_id="dash-2", title="Second Session", steps=["review"])

    page.goto(f"{server.base_url}/dashboard")

    expect(page.locator(".timeline-card")).to_have_count(2, timeout=3000)
    expect(page.locator("text=First Session")).to_be_visible(timeout=3000)
    expect(page.locator("text=Second Session")).to_be_visible(timeout=3000)


def test_dashboard_empty_state(server: _ServerInfo, page: Page) -> None:
    """Dashboard shows empty message when no sessions exist."""
    page.goto(f"{server.base_url}/dashboard")

    expect(page.locator("text=No sessions found.")).to_be_visible(timeout=3000)


def test_dashboard_reflects_new_session_on_reload(server: _ServerInfo, page: Page) -> None:
    """Dashboard shows newly created sessions after page reload."""
    page.goto(f"{server.base_url}/dashboard")
    expect(page.locator("text=No sessions found.")).to_be_visible(timeout=3000)

    # Create a session server-side
    manager = server.manager
    manager.create_session(session_id="reload-new", title="Reload Created", steps=["review"])

    # Reload to see the new session
    page.reload()
    expect(page.locator("text=Reload Created")).to_be_visible(timeout=3000)


def test_dashboard_delete_removes_session(server: _ServerInfo, page: Page) -> None:
    """Clicking Delete button removes the session via Datastar @post."""
    manager = server.manager
    manager.create_session(session_id="del-1", title="Delete Me", steps=["review"])

    page.goto(f"{server.base_url}/dashboard")
    expect(page.locator("text=Delete Me")).to_be_visible(timeout=3000)

    page.locator(".cleanup-btn").click()

    # After cleanup, the dashboard redirects and session should be gone
    page.wait_for_url("**/dashboard", timeout=3000)
    expect(page.locator("text=Delete Me")).not_to_be_visible(timeout=3000)


def test_dashboard_status_badges(server: _ServerInfo, page: Page) -> None:
    """Status badges reflect session state correctly."""
    manager = server.manager
    session = manager.create_session(
        session_id="badge-1", title="Badge Test", steps=["review"]
    )
    step = session.steps[0]
    manager.start_step("badge-1", step.step_id)

    page.goto(f"{server.base_url}/dashboard")

    badge = page.locator(".status-badge.status-started")
    expect(badge).to_be_visible(timeout=3000)
    expect(badge).to_have_text("started", timeout=3000)
