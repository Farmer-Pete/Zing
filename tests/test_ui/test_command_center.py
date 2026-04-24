"""Playwright UI tests for the Command Center dashboard."""

from __future__ import annotations

from datetime import UTC, datetime

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui


def test_empty_board_shows_nothing_here(server: _ServerInfo, page: Page) -> None:
    """When no issues/PRs/sessions exist each column shows the empty-state message."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    empty = page.locator(".column-empty").first
    expect(empty).to_be_visible(timeout=5000)
    expect(empty).to_contain_text("Nothing here", timeout=3000)


def test_card_renders_with_ticket_and_title(server: _ServerInfo, page: Page) -> None:
    """A card derived from a Linear issue renders its identifier and title."""
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-001",
        identifier="BAK-1001",
        title="Test feature",
        state="In Progress",
        state_type="started",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1001",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    card = page.locator("#card-bak-1001")
    expect(card).to_be_visible(timeout=5000)

    # Ticket identifier should be a clickable link
    ticket_link = card.locator(".card-ticket-id")
    expect(ticket_link).to_be_visible(timeout=3000)
    expect(ticket_link).to_contain_text("BAK-1001", timeout=3000)

    # Title should be present
    expect(card.locator(".card-title")).to_contain_text("Test feature", timeout=3000)


def test_card_with_audit_findings_shows_badge(server: _ServerInfo, page: Page) -> None:
    """A card with audit findings shows the audit badge in the footer."""
    manager = server.manager
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-003",
        identifier="BAK-1003",
        title="Audit badge test",
        state="In Progress",
        state_type="started",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1003",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    # Create a session with a ready build-audit step + findings
    session = manager.create_session(session_id="cc-audit-1", title="Audit", steps=["build-audit"])
    manager.update_session("cc-audit-1", ticket_id="BAK-1003")
    step = session.steps[0]
    manager.start_step("cc-audit-1", step.step_id)
    manager.add_finding(
        "cc-audit-1",
        step.step_id,
        {"type": "triage", "id": "f-audit", "title": "Critical finding"},
    )
    manager.mark_step_ready("cc-audit-1", step.step_id)

    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    card = page.locator("#card-bak-1003")
    expect(card).to_be_visible(timeout=5000)

    # Findings count in the strip should be visible
    findings = card.locator(".strip-findings")
    expect(findings).to_be_visible(timeout=3000)
    expect(findings).to_contain_text("finding", timeout=3000)


def test_sse_event_updates_board_without_reload(server: _ServerInfo, page: Page) -> None:
    """Mutating external_cache + pushing board_changed SSE event patches the DOM within 5 s."""
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-004",
        identifier="BAK-1004",
        title="Original title",
        state="In Progress",
        state_type="started",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-1004",
        updated_at=datetime.now(tz=UTC),
    )
    cache.issues = [issue]

    # Wait for the SSE response to start streaming before mutating state.
    with page.expect_response(lambda r: "/command-center/events" in r.url, timeout=5000):
        page.goto(f"{server.base_url}/command-center")

    page.wait_for_load_state("domcontentloaded", timeout=5000)

    card = page.locator("#card-bak-1004")
    expect(card).to_be_visible(timeout=5000)
    expect(card.locator(".card-title")).to_contain_text("Original title", timeout=3000)

    assert server.cc_queues, "Expected SSE queue to be registered after response started"

    # Mutate the cache title and bump version so the memo invalidates.
    updated_issue = issue.model_copy(update={"title": "Updated title SSE"})
    cache.issues = [updated_issue]
    cache.version += 1

    # Push the board_changed event to all active SSE queues
    for queue in list(server.cc_queues):
        queue.put_nowait("board_changed")

    # The DOM should reflect the updated title via SSE patch
    expect(card.locator(".card-title")).to_contain_text("Updated title SSE", timeout=5000)


def test_last_synced_footer_updates(server: _ServerInfo, page: Page) -> None:
    """Toolbar starts with 'Waiting for first poll'; after a poll_status SSE event it updates."""
    with page.expect_response(lambda r: "/command-center/events" in r.url, timeout=5000):
        page.goto(f"{server.base_url}/command-center")

    page.wait_for_load_state("domcontentloaded", timeout=5000)

    toolbar_span = page.locator(".cc-toolbar span")
    expect(toolbar_span).to_be_visible(timeout=5000)

    initial_text = toolbar_span.text_content(timeout=3000) or ""
    assert "Waiting for first poll" in initial_text, (
        f"Expected 'Waiting for first poll' in footer, got: {initial_text!r}"
    )

    assert server.cc_queues, "Expected SSE queue to be registered after response started"

    # Set last_polled_at and push a poll_status event
    now = datetime.now(tz=UTC)
    server.external_cache.last_polled_at = now

    for queue in list(server.cc_queues):
        queue.put_nowait("poll_status")

    expect(toolbar_span).to_contain_text("Last synced", timeout=5000)
    updated_text = toolbar_span.text_content(timeout=3000) or ""
    assert "Waiting for first poll" not in updated_text, (
        f"Toolbar should no longer say 'Waiting for first poll', got: {updated_text!r}"
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


def test_no_console_errors_after_page_load(server: _ServerInfo, page: Page) -> None:
    """No JS console errors occur after page load with data on the board."""
    cache = server.external_cache

    issue = LinearIssue(
        id="linear-uuid-005",
        identifier="BAK-1005",
        title="Console error check",
        state="In Progress",
        state_type="started",
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

    card = page.locator("#card-bak-1005")
    expect(card).to_be_visible(timeout=5000)

    # Allow a brief moment for any async JS errors to surface
    page.wait_for_timeout(1000)

    assert errors == [], f"Unexpected JS console errors: {errors}"
