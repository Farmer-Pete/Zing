"""Playwright UI tests for the standup modal on the Command Center."""

from __future__ import annotations

import re
from datetime import UTC, datetime

import pytest
from playwright.sync_api import BrowserContext, Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _seed_cache(server: _ServerInfo) -> None:
    """Seed a minimal LinearIssue so the board is non-empty and the standup
    endpoint has something to generate a message from."""
    cache = server.external_cache
    if not cache.issues:
        cache.github_username = "dev-user"
        cache.issues = [
            LinearIssue(
                id="uuid-standup-seed",
                identifier="BAK-9001",
                title="Standup seed issue",
                state="In Progress",
                state_type="started",
                assignee=None,
                team="Backend",
                url="https://linear.app/test/issue/BAK-9001",
                updated_at=datetime.now(tz=UTC),
            )
        ]


def _goto_cc(server: _ServerInfo, page: Page) -> None:
    """Navigate to the Command Center and wait for Datastar to initialise."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)
    # Allow Datastar / inline scripts a moment to bind signals
    page.wait_for_timeout(400)


def _open_standup(server: _ServerInfo, page: Page) -> None:
    """Click the standup button and wait for the modal to open."""
    _goto_cc(server, page)

    btn = page.locator("#cc-standup-btn")
    expect(btn).to_be_visible(timeout=5000)
    btn.click()

    # Modal acquires .open class via data-class:open="$modals.standup" once the
    # SSE response patches the signal.
    modal = page.locator("#standup-modal")
    expect(modal).to_have_class(re.compile(r"\bopen\b"), timeout=8000)


# ---------------------------------------------------------------------------
# 1. Open modal + SSE content renders
# ---------------------------------------------------------------------------


def test_standup_button_opens_modal_and_renders_content(server: _ServerInfo, page: Page) -> None:
    """Clicking the standup button opens the modal and SSE-patches content into the body."""
    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    _seed_cache(server)
    _open_standup(server, page)

    modal = page.locator("#standup-modal")
    expect(modal).to_have_class(re.compile(r"\bopen\b"), timeout=8000)

    # The SSE endpoint patches rendered HTML into #standup-modal-body via
    # SSE.patch_elements(..., mode=INNER).  Wait for the element to become
    # non-empty (innerHTML != '').
    body = page.locator("#standup-modal-body")
    expect(body).to_be_visible(timeout=5000)

    # Wait until the body has some non-empty inner HTML (SSE patch has arrived).
    page.wait_for_function(
        "() => document.getElementById('standup-modal-body')?.innerHTML?.trim()?.length > 0",
        timeout=8000,
    )

    assert errors == [], f"Unexpected JS console errors after standup open: {errors}"


# ---------------------------------------------------------------------------
# 2. Tab switching toggles content panes
# ---------------------------------------------------------------------------


def test_standup_modal_tab_switch_toggles_content(server: _ServerInfo, page: Page) -> None:
    """Clicking Markdown / Rich Text tabs shows and hides the correct pane.

    The rendered pane has data-show="$standupTab === 'rendered'".
    The markdown pane has data-show="$standupTab === 'markdown'".
    Datastar resolves these once the signal changes.
    """
    _seed_cache(server)
    _open_standup(server, page)

    # Wait for SSE content to arrive before testing tab switching.
    page.wait_for_function(
        "() => document.getElementById('standup-modal-body')?.innerHTML?.trim()?.length > 0",
        timeout=8000,
    )

    rendered_pane = page.locator(".standup-rendered")
    markdown_pane = page.locator(".standup-markdown")

    # Default tab is 'rendered' — rendered pane should be visible.
    expect(rendered_pane).to_be_visible(timeout=3000)

    # Click the Markdown tab
    md_tab = page.locator(".standup-tabs .standup-tab", has_text="Markdown")
    expect(md_tab).to_be_visible(timeout=3000)
    md_tab.click()

    # After switching: markdown pane visible, rendered pane hidden.
    expect(markdown_pane).to_be_visible(timeout=3000)
    expect(rendered_pane).to_be_hidden(timeout=3000)

    # Switch back to Rich Text
    rt_tab = page.locator(".standup-tabs .standup-tab", has_text="Rich Text")
    rt_tab.click()

    expect(rendered_pane).to_be_visible(timeout=3000)
    expect(markdown_pane).to_be_hidden(timeout=3000)


# ---------------------------------------------------------------------------
# 3. Copy button writes markdown to clipboard
# ---------------------------------------------------------------------------


@pytest.mark.xfail(
    reason=(
        "ClipboardItem / clipboard.write() requires a secure context or browser "
        "permission grant that may not be available in CI headless environments."
    ),
    strict=False,
)
def test_standup_copy_button_writes_to_clipboard(
    server: _ServerInfo, context: BrowserContext, page: Page
) -> None:
    """Clicking Copy calls navigator.clipboard.write() with the standup text.

    Uses context.grant_permissions to allow clipboard-read/write so the async
    clipboard API resolves inside the Playwright test.

    The copy handler in cc-modals.js dispatches 'copy-standup' which calls
    navigator.clipboard.write([ClipboardItem({text/html, text/plain})]).
    Playwright can only read back text/plain reliably via readText().
    """
    context.grant_permissions(["clipboard-read", "clipboard-write"])

    _seed_cache(server)

    # Navigate inside the granted context.
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)
    page.wait_for_timeout(400)

    btn = page.locator("#cc-standup-btn")
    expect(btn).to_be_visible(timeout=5000)
    btn.click()

    modal = page.locator("#standup-modal")
    expect(modal).to_have_class(re.compile(r"\bopen\b"), timeout=8000)

    # Wait for SSE content to arrive — standupMarkdown signal is non-empty once
    # the SSE endpoint has fired its patch_signals event.
    page.wait_for_function(
        "() => document.getElementById('standup-modal-body')?.innerHTML?.trim()?.length > 0",
        timeout=8000,
    )

    copy_btn = page.locator("#standup-copy-btn")
    expect(copy_btn).to_be_visible(timeout=3000)
    copy_btn.click()

    # Allow the async clipboard.write() Promise to resolve.
    page.wait_for_timeout(800)

    # Read back the plain-text portion of the clipboard.
    clipboard_text: str = page.evaluate("navigator.clipboard.readText()")

    assert isinstance(clipboard_text, str), (
        f"Expected clipboard to contain a string, got: {clipboard_text!r}"
    )
    assert len(clipboard_text.strip()) > 0, "Expected clipboard to contain non-empty standup text"
