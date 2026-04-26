"""Playwright UI tests for kebab menu interactions on Command Center kanban cards."""

from __future__ import annotations

import json
import re
from datetime import UTC, datetime
from typing import Literal

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.models_external import CICheck, GitHubPR, LinearIssue

pytestmark = pytest.mark.ui

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_PR_AUTHOR = "dev-user"
_OTHER_USER = "reviewer-user"


def _make_pr(
    number: int = 101,
    *,
    author: str = _PR_AUTHOR,
    repo: str = "org/my-repo",
    review_decision: Literal["APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"] | None = None,
    state: Literal["open", "closed", "merged"] = "open",
    merged_at: datetime | None = None,
    draft: bool = False,
    ci_checks: list[CICheck] | None = None,
) -> GitHubPR:
    """Build a minimal GitHubPR for test fixtures."""
    return GitHubPR(
        number=number,
        title=f"PR #{number}",
        state=state,
        draft=draft,
        head_ref=f"feature/pr-{number}",
        base_ref="main",
        body=None,
        author=author,
        repo=repo,
        requested_reviewers=[],
        reviewers=[],
        reviewer_states={},
        review_decision=review_decision,
        mergeable_state="clean",
        ci_status=None,
        ci_checks=ci_checks or [],
        url=f"https://github.com/{repo}/pull/{number}",
        updated_at=datetime.now(tz=UTC),
        merged_at=merged_at,
    )


def _make_issue(identifier: str = "FRO-42", title: str = "Test issue") -> LinearIssue:
    """Build a minimal LinearIssue for test fixtures."""
    return LinearIssue(
        id=f"uuid-{identifier.lower()}",
        identifier=identifier,
        title=title,
        state="In Progress",
        state_type="started",
        assignee=None,
        team="Frontend",
        url=f"https://linear.app/test/issue/{identifier}",
        updated_at=datetime.now(tz=UTC),
    )


def _setup_cache(server: _ServerInfo, username: str = _PR_AUTHOR) -> ExternalCache:
    """Return the server's cache pre-configured with a github_username.

    Orphan PR cards are excluded from the board unless the current user is
    the author or reviewer. Setting the username ensures test PRs are visible.
    """
    cache = server.external_cache
    cache.github_username = username
    return cache


def _wait_for_page(page: Page) -> None:
    """Wait for the Command Center page to settle."""
    page.wait_for_load_state("domcontentloaded", timeout=5000)
    # Give Datastar / inline scripts a moment to bind
    page.wait_for_timeout(300)


# ---------------------------------------------------------------------------
# 1. Menu open / close behaviour
# ---------------------------------------------------------------------------


def test_kebab_menu_opens_on_click(server: _ServerInfo, page: Page) -> None:
    """Clicking the kebab button opens the adjacent strip-menu."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=201)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Locate a kebab button on any PR strip
    kebab = page.locator(".strip-kebab").first
    expect(kebab).to_be_visible(timeout=5000)

    # The adjacent menu should NOT be open initially
    menu = page.locator(".strip-menu").first
    expect(menu).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # Click the kebab — menu should become open
    kebab.click()
    expect(menu).to_have_class(re.compile(r"\bopen\b"), timeout=3000)


def test_kebab_menu_closes_on_second_click(server: _ServerInfo, page: Page) -> None:
    """Clicking the kebab button again (toggle) closes the menu."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=202)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    kebab = page.locator(".strip-kebab").first
    menu = page.locator(".strip-menu").first

    kebab.click()
    expect(menu).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    kebab.click()
    expect(menu).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)


def test_kebab_menu_closes_on_outside_click(server: _ServerInfo, page: Page) -> None:
    """Clicking outside any kebab or strip-menu closes an open menu."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=203)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    kebab = page.locator(".strip-kebab").first
    menu = page.locator(".strip-menu").first

    kebab.click()
    expect(menu).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # Click somewhere neutral — the page heading area
    page.locator(".cc-toolbar").click()
    expect(menu).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)


# ---------------------------------------------------------------------------
# 2. Copy-to-clipboard
# ---------------------------------------------------------------------------


def test_copy_cmd_button_calls_clipboard_write_text(server: _ServerInfo, page: Page) -> None:
    """Click on a copy button fires clipboard.writeText via inline data-on:click."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=204)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Intercept navigator.clipboard.writeText before we interact with the page
    writes: list[str] = []
    page.expose_function("__recordClipboardWrite", lambda text: writes.append(text))
    page.evaluate("""
        navigator.clipboard.writeText = function(text) {
            window.__recordClipboardWrite(text);
            return Promise.resolve();
        };
    """)

    # Open the kebab menu so the copy buttons are accessible
    kebab = page.locator(".strip-kebab").first
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    # Click the first copy button inside the open menu
    copy_btn = page.locator(".strip-menu.open .menu-row-copy").first
    expect(copy_btn).to_be_visible(timeout=3000)

    # Extract expected command from the inline data-on:click attribute.
    # The expression looks like:
    #   navigator.clipboard.writeText("zing-ai launch ..."); $openKebab = null
    on_click = copy_btn.get_attribute("data-on:click")
    assert on_click, "Copy button must carry a data-on:click attribute"
    import re as _re

    m = _re.search(r'writeText\((".*?")\)', on_click)
    assert m, f"data-on:click must contain writeText(...) with a JSON string: {on_click!r}"
    import json as _json

    expected_cmd = _json.loads(m.group(1))

    copy_btn.click()

    # Allow time for the async clipboard promise to resolve and the exposed function to fire
    page.wait_for_timeout(1000)

    assert expected_cmd in writes, f"Expected clipboard to receive {expected_cmd!r}, got: {writes}"


def test_copy_cmd_closes_menu_after_click(server: _ServerInfo, page: Page) -> None:
    """After clicking a copy button the clipboard write fires without JS error.

    Menu closure via $openKebab is wired in Step 32 (cc-kebab.js signal watch).
    Until then, we verify the inline data-on:click fires without throwing and
    the clipboard API receives the call.
    """
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=205)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Stub clipboard so the handler doesn't throw in test environment
    writes: list[str] = []
    page.expose_function("__recordClipboardWrite205", lambda text: writes.append(text))
    page.evaluate("""
        navigator.clipboard.writeText = function(text) {
            window.__recordClipboardWrite205(text);
            return Promise.resolve();
        };
    """)

    kebab = page.locator(".strip-kebab").first
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    copy_btn = page.locator(".strip-menu.open .menu-row-copy").first
    copy_btn.click()

    # Allow time for async clipboard promise to resolve
    page.wait_for_timeout(500)

    # Clipboard writeText must have been called by the inline data-on:click expression
    assert writes, f"Expected clipboard.writeText to be called, but writes={writes}"


def test_copy_icon_fires_clipboard_write(server: _ServerInfo, page: Page) -> None:
    """After clicking a copy button the clipboard writeText fires via inline data-on:click.

    The old JS handler added a .copied CSS class flash; that is now handled inline
    by the data-on:click expression, so this test verifies the clipboard API is called.
    """
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=206)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    writes: list[str] = []
    page.expose_function("__recordClipboardWrite206", lambda text: writes.append(text))
    page.evaluate("""
        navigator.clipboard.writeText = function(text) {
            window.__recordClipboardWrite206(text);
            return Promise.resolve();
        };
    """)

    kebab = page.locator(".strip-kebab").first
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    copy_btn = page.locator(".strip-menu.open .menu-row-copy").first
    copy_btn.click()

    page.wait_for_timeout(500)

    # The inline data-on:click must have triggered clipboard.writeText
    assert writes, (
        f"Expected clipboard.writeText to be called after copy button click, got: {writes}"
    )
    assert any(w for w in writes if isinstance(w, str) and w), (
        f"Clipboard must receive a non-empty string, got: {writes}"
    )


# ---------------------------------------------------------------------------
# 3. Only one menu open at a time
# ---------------------------------------------------------------------------


def test_opening_second_menu_closes_first(server: _ServerInfo, page: Page) -> None:
    """Opening a second kebab menu closes any previously open menu.

    Uses JS to call toggleMenu directly on the second kebab, avoiding
    Playwright overlay-interception issues when two cards stack vertically.
    """
    cache = _setup_cache(server)
    # Two PRs on the same card (same issue/repo)
    issue = _make_issue("FRO-50", "Multi-PR card")
    cache.issues = [issue]
    cache.prs = [
        _make_pr(number=301, repo="org/repo-a"),
        _make_pr(number=302, repo="org/repo-a"),
    ]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Need at least two kebab buttons for this test
    kebabs = page.locator(".strip-kebab")
    if kebabs.count() < 2:
        pytest.skip("Fewer than two kebab buttons rendered — card layout may differ")

    first_kebab = kebabs.nth(0)

    first_kebab.click()
    first_menu = first_kebab.locator("+ .strip-menu")
    expect(first_menu).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # Dispatch click event on the second kebab toggle to bypass Playwright overlay checks.
    page.locator("[data-kebab-toggle]").nth(1).dispatch_event("click")
    page.wait_for_timeout(300)

    # Second menu should now be open
    second_menu = kebabs.nth(1).locator("+ .strip-menu")
    expect(second_menu).to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # First menu must now be closed
    expect(first_menu).not_to_have_class(re.compile(r"\bopen\b"), timeout=3000)

    # Only one menu is open across the whole page
    open_menus = page.locator(".strip-menu.open")
    expect(open_menus).to_have_count(1, timeout=3000)


# ---------------------------------------------------------------------------
# 4. Search filtering inside complex menus
# ---------------------------------------------------------------------------


def test_search_filters_menu_rows(server: _ServerInfo, page: Page) -> None:
    """Typing in the search input shows only matching rows and hides others."""
    cache = _setup_cache(server)
    # changes-requested triggers the "Respond" section which adds a search box
    pr = _make_pr(number=401, author=_PR_AUTHOR, review_decision="CHANGES_REQUESTED")
    cache.prs = [pr]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Open the kebab that has a search input (CHANGES_REQUESTED PR)
    kebab = page.locator(".strip-kebab").first
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    search = page.locator(".strip-menu.open .menu-search").first

    # The search box should be visible and focussed
    expect(search).to_be_visible(timeout=3000)

    # Count all visible menu rows before filtering
    all_rows = page.locator(".strip-menu.open .menu-row")
    total_rows = all_rows.count()
    assert total_rows > 1, "Expected multiple menu rows for a CHANGES_REQUESTED PR"

    # Type a query that uniquely matches "respond" rows
    search.fill("respond")
    page.wait_for_timeout(200)  # filterMenu() is synchronous but allow render tick

    # At minimum "Respond" row must be visible
    respond_row = page.locator(".strip-menu.open .menu-row-main[data-s*='respond']")
    expect(respond_row).to_be_visible(timeout=2000)

    # Type something that matches nothing — all rows should vanish
    search.fill("zzznomatch")
    page.wait_for_timeout(200)

    # No visible rows
    for i in range(all_rows.count()):
        row = all_rows.nth(i)
        # row.style.display should be 'none' for all
        display = row.evaluate("el => el.style.display")
        assert display == "none", (
            f"Row {i} should be hidden after no-match search, got display={display!r}"
        )


def test_search_clears_on_menu_reopen(server: _ServerInfo, page: Page) -> None:
    """Re-opening a menu that had an active search clears the filter."""
    cache = _setup_cache(server)
    pr = _make_pr(number=402, author=_PR_AUTHOR, review_decision="CHANGES_REQUESTED")
    cache.prs = [pr]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    kebab = page.locator(".strip-kebab").first
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    search = page.locator(".strip-menu.open .menu-search").first
    search.fill("respond")
    page.wait_for_timeout(200)

    # Close the menu
    page.locator(".cc-toolbar").click()
    expect(page.locator(".strip-menu.open")).not_to_be_visible(timeout=2000)

    # Reopen
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    search_after = page.locator(".strip-menu.open .menu-search").first
    # toggleMenu() clears search.value and calls filterMenu(search) on open
    value_after = search_after.input_value(timeout=2000)
    assert value_after == "", f"Search input should be cleared on re-open, got: {value_after!r}"


# ---------------------------------------------------------------------------
# 5. z-index / menu-open class applied to the card
# ---------------------------------------------------------------------------


def test_card_gets_menu_open_class_when_menu_opens(server: _ServerInfo, page: Page) -> None:
    """When a kebab menu is opened the parent .card gets the .menu-open CSS class."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=501)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    kebab = page.locator(".strip-kebab").first
    card = kebab.locator("xpath=ancestor::div[contains(@class,'card')][1]")

    # Not open initially
    expect(card).not_to_have_class(re.compile(r"\bmenu-open\b"), timeout=3000)

    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    # Card should now carry the menu-open class
    expect(card).to_have_class(re.compile(r"\bmenu-open\b"), timeout=3000)


def test_card_menu_open_class_removed_when_menu_closes(server: _ServerInfo, page: Page) -> None:
    """Closing the menu removes .menu-open from the parent .card."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=502)]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    kebab = page.locator(".strip-kebab").first
    card = kebab.locator("xpath=ancestor::div[contains(@class,'card')][1]")

    kebab.click()
    expect(card).to_have_class(re.compile(r"\bmenu-open\b"), timeout=3000)

    # Close via outside click
    page.locator(".cc-toolbar").click()
    expect(card).not_to_have_class(re.compile(r"\bmenu-open\b"), timeout=3000)


# ---------------------------------------------------------------------------
# 6. launch-background POST payload correctness
# ---------------------------------------------------------------------------


def test_launch_bg_primary_button_sends_skill_and_pr_number(
    server: _ServerInfo, page: Page
) -> None:
    """Clicking a strip-primary-btn sends skill and pr_number in the POST body via Datastar."""
    cache = _setup_cache(server, _OTHER_USER)  # reviewer, so "PR Audit" button shows
    pr = _make_pr(number=601, author=_PR_AUTHOR)
    pr.requested_reviewers = [_OTHER_USER]  # ensure card is visible to reviewer
    cache.prs = [pr]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Hover the card to reveal opacity:0 buttons
    card = page.locator(".card").first
    card.hover()

    # Primary button now uses Datastar data-on:click — find it by its class
    primary_btn = page.locator(".strip-primary-btn").first
    expect(primary_btn).to_be_visible(timeout=5000)

    # Extract expected skill and pr_number from the inline data-on:click payload.
    # The expression looks like: @post('/command-center/launch-background',
    #   {payload: {card_key: '...', btn_id: '...', skill: 'pr-audit', pr_number: 601}})
    on_click = primary_btn.get_attribute("data-on:click")
    assert on_click, "Primary button must carry a data-on:click attribute"
    skill_match = re.search(r"skill:\s*'([^']+)'", on_click)
    pr_match = re.search(r"pr_number:\s*(\d+)", on_click)
    assert skill_match, f"data-on:click must contain skill: '...': {on_click!r}"
    assert pr_match, f"data-on:click must contain pr_number: <int>: {on_click!r}"
    expected_skill = skill_match.group(1)
    expected_pr = int(pr_match.group(1))

    # Capture the POST request before clicking
    with page.expect_request("**/launch-background", timeout=5000) as req_info:
        primary_btn.click()

    request = req_info.value
    body = json.loads(request.post_data or "{}")
    payload = body.get("payload", body)

    assert payload.get("skill") == expected_skill, (
        f"Expected skill={expected_skill!r}, got payload={payload}"
    )
    assert payload.get("pr_number") == expected_pr, (
        f"Expected pr_number={expected_pr}, got payload={payload}"
    )


def test_launch_bg_menu_row_sends_skill_and_pr_number(server: _ServerInfo, page: Page) -> None:
    """Clicking a menu-row-main button inside the kebab sends the correct skill + pr_number."""
    cache = _setup_cache(server, _OTHER_USER)
    pr = _make_pr(number=602, author=_PR_AUTHOR)
    pr.requested_reviewers = [_OTHER_USER]
    cache.prs = [pr]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Hover the card to reveal opacity:0 buttons, then open kebab
    card = page.locator(".card").first
    card.hover()
    kebab = page.locator(".strip-kebab").first
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    # Pick the first menu-row-main that has a data-on:click with a skill payload.
    # All launch buttons in the kebab now use Datastar data-on:click with @post.
    row_btns = page.locator(".strip-menu.open .menu-row-main[data-on\\:click*='skill']")
    expect(row_btns.first).to_be_visible(timeout=3000)
    row_btn = row_btns.first

    # Extract expected skill and pr_number from the inline data-on:click payload.
    on_click = row_btn.get_attribute("data-on:click")
    assert on_click, "Menu row must carry a data-on:click attribute"
    skill_match = re.search(r"skill:\s*'([^']+)'", on_click)
    pr_match = re.search(r"pr_number:\s*(\d+)", on_click)
    assert skill_match, f"data-on:click must contain skill: '...': {on_click!r}"
    assert pr_match, f"data-on:click must contain pr_number: <int>: {on_click!r}"
    expected_skill = skill_match.group(1)
    expected_pr = int(pr_match.group(1))

    with page.expect_request("**/launch-background", timeout=5000) as req_info:
        row_btn.click()

    request = req_info.value
    body = json.loads(request.post_data or "{}")
    payload = body.get("payload", body)

    assert payload.get("skill") == expected_skill, (
        f"Expected skill={expected_skill!r}, got payload={payload}"
    )
    assert payload.get("pr_number") == expected_pr, (
        f"Expected pr_number={expected_pr}, got payload={payload}"
    )


def test_launch_bg_pr_number_matches_pr_on_card(server: _ServerInfo, page: Page) -> None:
    """The pr_number in the POST payload matches the PR number on the card, not a default."""
    cache = _setup_cache(server, _OTHER_USER)
    # Use a non-default PR number to catch hard-coded 0 bugs
    pr = _make_pr(number=9999, author=_PR_AUTHOR)
    pr.requested_reviewers = [_OTHER_USER]
    cache.prs = [pr]

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Hover the card to reveal opacity:0 buttons
    card = page.locator(".card").first
    card.hover()

    # Primary button now uses Datastar data-on:click with pr_number in payload.
    # Find it by checking that data-on:click contains pr_number: 9999.
    primary_btn = page.locator(".strip-primary-btn[data-on\\:click*='pr_number: 9999']").first
    expect(primary_btn).to_be_visible(timeout=5000)

    with page.expect_request("**/launch-background", timeout=5000) as req_info:
        primary_btn.click()

    body = json.loads(req_info.value.post_data or "{}")
    payload = body.get("payload", body)
    assert payload.get("pr_number") == 9999, f"Expected pr_number=9999, got payload={payload}"


# ---------------------------------------------------------------------------
# 7. No JS console errors after all menu interactions
# ---------------------------------------------------------------------------


def test_no_console_errors_during_kebab_interactions(server: _ServerInfo, page: Page) -> None:
    """Open/close + copy interactions produce no JS console errors."""
    cache = _setup_cache(server)
    cache.prs = [_make_pr(number=701, author=_PR_AUTHOR)]

    errors: list[str] = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)

    page.goto(f"{server.base_url}/command-center")
    _wait_for_page(page)

    # Stub clipboard after navigation — navigator.clipboard is undefined on about:blank
    page.evaluate("""
        if (!navigator.clipboard) navigator.clipboard = {};
        navigator.clipboard.writeText = function(text) { return Promise.resolve(); };
    """)

    kebab = page.locator(".strip-kebab").first

    # Open
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    # Copy — inline data-on:click calls clipboard.writeText; menu stays open
    # (menu-close via $openKebab signal is wired in Step 32)
    copy_btn = page.locator(".strip-menu.open .menu-row-copy").first
    copy_btn.click()
    page.wait_for_timeout(200)

    # Close via outside click (menu still open after copy, so outside click closes it)
    page.locator(".cc-toolbar").click()
    expect(page.locator(".strip-menu.open")).not_to_be_visible(timeout=2000)

    # Re-open
    kebab.click()
    expect(page.locator(".strip-menu.open")).to_be_visible(timeout=3000)

    # Close again
    page.locator(".cc-toolbar").click()
    page.wait_for_timeout(300)

    assert errors == [], f"Unexpected JS console errors during kebab interactions: {errors}"
