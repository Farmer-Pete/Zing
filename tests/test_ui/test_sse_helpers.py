"""Playwright UI tests for SSE-helper-driven toast notifications."""

from __future__ import annotations

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui


class _NoOpPoller:
    """Minimal stand-in for ExternalPoller that succeeds without doing I/O."""

    async def _poll_once(self) -> None:  # noqa: ANN202
        return


def test_refresh_button_shows_ok_toast_then_removes(server: _ServerInfo, page: Page) -> None:
    """Clicking the Refresh button yields a cc-toast-ok containing 'Refreshed'.

    The toast should appear in #cc-toast-container and then be removed from the
    DOM after ~5 seconds via the data-init__delay.5000ms self-removal mechanism.
    No JS console errors should occur during the interaction.
    """
    # Install a no-op poller so the route doesn't return a 503-like err toast.
    server.fastapi_app.state.poller = _NoOpPoller()

    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    # Click the Refresh button and wait for the SSE POST to complete
    btn = page.locator("#btn-refresh")
    expect(btn).to_be_visible(timeout=5000)

    with page.expect_response(lambda r: "/command-center/refresh" in r.url, timeout=5000):
        btn.click()

    # Toast should appear inside #cc-toast-container with ok styling and correct text
    toast = page.locator("#cc-toast-container .cc-toast.cc-toast-ok")
    expect(toast).to_be_visible(timeout=5000)
    expect(toast).to_contain_text("Refreshed", timeout=3000)

    # After ~5.5 s the data-on-load__delay removes the element from the DOM.
    # Give a generous 12 s window — the 5 s delay starts from when the element
    # was inserted, which may precede this wait_for_selector call.
    page.wait_for_selector(
        "#cc-toast-container .cc-toast",
        state="detached",
        timeout=12000,
    )

    # Clean up the mock poller so it doesn't affect other tests
    del server.fastapi_app.state.poller

    assert errors == [], f"Unexpected JS console errors after refresh: {errors}"
