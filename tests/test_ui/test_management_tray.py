"""Playwright UI tests for the management tray FAB and panel on the Command Center."""

from __future__ import annotations

import re
from datetime import UTC, datetime

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _goto_cc(server: _ServerInfo, page: Page) -> None:
    """Navigate to the Command Center and wait for Datastar to initialise."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)
    # Allow Datastar / inline scripts a moment to bind signals
    page.wait_for_timeout(300)


# ---------------------------------------------------------------------------
# 1. FAB toggles the panel open and closed
# ---------------------------------------------------------------------------


def test_mgmt_fab_toggles_open_and_close(server: _ServerInfo, page: Page) -> None:
    """Clicking the FAB opens the management panel; clicking again closes it.

    The FAB has data-on:click="$modals.mgmt = !$modals.mgmt".
    The panel has data-class:open="$modals.mgmt".
    The FAB itself has data-class:hidden="$modals.mgmt" (hides the FAB while open).
    """
    _goto_cc(server, page)

    fab = page.locator("#mgmt-fab")
    panel = page.locator("#mgmt-panel")

    # Initial state: FAB visible, panel does NOT carry .open.
    expect(fab).to_be_visible(timeout=5000)
    expect(panel).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # Click FAB — panel should open.
    fab.click()
    expect(panel).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # FAB is now hidden (data-class:hidden="$modals.mgmt").
    expect(fab).to_have_class(re.compile(r"\bhidden\b"), timeout=3000)

    # Close via the panel's own close button.
    close_btn = panel.locator(".mgmt-panel-close")
    expect(close_btn).to_be_visible(timeout=3000)
    close_btn.click()

    expect(panel).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)
    # FAB reappears.
    expect(fab).not_to_have_class(re.compile(r"\bhidden\b"), timeout=3000)


# ---------------------------------------------------------------------------
# 2. ESC key closes the panel
# ---------------------------------------------------------------------------


def test_mgmt_panel_closes_on_escape(server: _ServerInfo, page: Page) -> None:
    """Pressing Escape closes the management panel.

    The .cc-page element has:
      data-on:keydown__window__key.escape="... $modals.mgmt = false; ..."
    so the Datastar keydown listener should flip the signal and collapse the panel.
    """
    _goto_cc(server, page)

    fab = page.locator("#mgmt-fab")
    panel = page.locator("#mgmt-panel")

    # Open the panel via FAB.
    expect(fab).to_be_visible(timeout=5000)
    fab.click()
    expect(panel).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # Press Escape — the body-level keydown handler should close the panel.
    page.keyboard.press("Escape")

    expect(panel).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)


# ---------------------------------------------------------------------------
# 3. Kill session drives correct SSE (optional — requires seeded session)
# ---------------------------------------------------------------------------


def test_kill_session_button_removes_session(server: _ServerInfo, page: Page) -> None:
    """Clicking Kill on a running session POSTs kill-session and the session is removed.

    Seeds a running session (tmux_session set), opens the management tray,
    clicks Kill, and asserts that the session is gone from the manager.
    """
    manager = server.manager

    # Seed a Linear issue so the CC page renders.
    cache = server.external_cache
    cache.issues = [
        LinearIssue(
            id="uuid-kill-test",
            identifier="BAK-9999",
            title="Kill session test",
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url="https://linear.app/test/issue/BAK-9999",
            updated_at=datetime.now(tz=UTC),
        )
    ]

    # Create a ClaudeCodeSession with tmux_session populated (makes it appear in
    # the "Running Sessions" section of the management tray).
    # tmux_session is only settable at creation time via create_claude_code_session.
    manager.create_claude_code_session(
        session_id="kill-test-1",
        title="Kill Test Session",
        ticket_id="BAK-9999",
        tmux_session="fake-tmux-session",
    )

    # Seed the FastAPI app's live_sessions set so the route counts this session as
    # "running" (the tray only shows Kill buttons for sessions alive in live_sessions).
    server.fastapi_app.state.live_sessions = {"fake-tmux-session"}

    _goto_cc(server, page)

    fab = page.locator("#mgmt-fab")
    panel = page.locator("#mgmt-panel")

    expect(fab).to_be_visible(timeout=5000)
    fab.click()
    expect(panel).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # The Kill button should be present in the Running Sessions section.
    kill_btn = panel.locator(".mgmt-btn-danger", has_text="Kill").first
    expect(kill_btn).to_be_visible(timeout=3000)

    # Capture the POST request before clicking.
    with page.expect_request(
        lambda r: "/kill-session" in r.url and r.method == "POST", timeout=5000
    ) as req_info:
        kill_btn.click()

    request = req_info.value
    assert request.post_data is not None, "Kill button must POST to /kill-session"

    # Wait briefly for the SSE response to be processed.
    page.wait_for_timeout(800)

    # The session should now be gone from the manager.
    remaining_ids = {s.session_id for s in manager.list_sessions()}
    assert "kill-test-1" not in remaining_ids, (
        f"Expected session 'kill-test-1' to be removed after Kill, "
        f"but it is still in: {remaining_ids}"
    )
