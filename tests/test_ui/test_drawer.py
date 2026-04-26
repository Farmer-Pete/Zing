"""Playwright UI tests for the Command Center review drawer."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models import ResponseAction, UserResponse
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui


def _seed_session_with_findings(
    server: _ServerInfo,
    *,
    session_id: str = "drawer-1",
    ticket_id: str = "BAK-2001",
    title: str = "Drawer test",
    finding_id: str = "f-drawer",
) -> str:
    """Create a ZingSession with one ready build-audit step and a triage finding.

    Returns the workflow step_id so callers can target the submit endpoint.
    """
    cache = server.external_cache
    cache.issues = [
        LinearIssue(
            id="linear-uuid-drawer",
            identifier=ticket_id,
            title=title,
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url=f"https://linear.app/test/issue/{ticket_id}",
            updated_at=datetime.now(tz=UTC),
        )
    ]

    manager = server.manager
    session = manager.create_session(session_id=session_id, title=title, steps=["build-audit"])
    manager.update_session(session_id, ticket_id=ticket_id)
    step = session.steps[0]
    manager.start_step(session_id, step.step_id)
    manager.add_finding(
        session_id,
        step.step_id,
        {"type": "triage", "id": finding_id, "title": "Suspicious null check"},
    )
    manager.mark_step_ready(session_id, step.step_id)
    return step.step_id


def _open_drawer(server: _ServerInfo, page: Page) -> None:
    """Navigate to /command-center and click the first attention bar item."""
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    item = page.locator(".attn-item").first
    expect(item).to_be_visible(timeout=5000)
    item.click()

    drawer = page.locator("#review-drawer")
    expect(drawer).to_be_visible(timeout=5000)


def test_drawer_opens_from_attention_bar(server: _ServerInfo, page: Page) -> None:
    """Clicking an attention bar item fetches the drawer fragment and renders it."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    expect(page.locator("#review-drawer .dh-title")).to_contain_text("Drawer test", timeout=3000)
    expect(page.locator(".df-tb[data-action='accept']").first).to_be_visible(timeout=3000)


def test_drawer_close_button_dismisses_drawer(server: _ServerInfo, page: Page) -> None:
    """The header X (data-drawer-close) hides the drawer container."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    page.locator("#review-drawer .dh-close").click()
    expect(page.locator("#review-drawer-container")).to_be_hidden(timeout=3000)


def test_drawer_escape_key_dismisses_drawer(server: _ServerInfo, page: Page) -> None:
    """Pressing Escape closes the drawer (handled by the document keydown listener)."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    page.keyboard.press("Escape")
    expect(page.locator("#review-drawer-container")).to_be_hidden(timeout=3000)


def test_drawer_triage_button_toggles_state(server: _ServerInfo, page: Page) -> None:
    """Clicking Accept on a finding adds the .sa class and bumps the triage counter."""
    _seed_session_with_findings(server)
    _open_drawer(server, page)

    counter = page.locator("#drawer-triage-count")
    expect(counter).to_have_text("0", timeout=3000)

    accept = page.locator(".df-tb[data-action='accept']").first
    accept.click()

    expect(accept).to_have_class("df-tb sa", timeout=3000)
    expect(counter).to_have_text("1", timeout=3000)


def test_drawer_step_toggle_accordion(server: _ServerInfo, page: Page) -> None:
    """Clicking a past-step header expands and collapses the step-content via Datastar signals."""
    cache = server.external_cache
    cache.issues = [
        LinearIssue(
            id="linear-uuid-accordion",
            identifier="BAK-2099",
            title="Accordion test",
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url="https://linear.app/test/issue/BAK-2099",
            updated_at=datetime.now(tz=UTC),
        )
    ]

    manager = server.manager
    # Two steps: plan (completes when build-audit starts) + build-audit (current/ready).
    session = manager.create_session(
        session_id="accordion-1", title="Accordion test", steps=["plan", "build-audit"]
    )
    manager.update_session("accordion-1", ticket_id="BAK-2099")
    plan_step = session.steps[0]
    audit_step = session.steps[1]

    manager.start_step("accordion-1", plan_step.step_id)
    manager.mark_step_ready("accordion-1", plan_step.step_id)
    # Starting audit auto-completes plan.
    manager.start_step("accordion-1", audit_step.step_id)
    manager.add_finding(
        "accordion-1",
        audit_step.step_id,
        {"type": "triage", "id": "f-acc", "title": "Accordion finding"},
    )
    manager.mark_step_ready("accordion-1", audit_step.step_id)

    _open_drawer(server, page)

    # The plan step-section should be present (past step) and initially collapsed.
    past_section = page.locator(f"#step-section-{plan_step.step_id}")
    expect(past_section).to_be_visible(timeout=5000)
    # step-content is display:none when .open is absent; confirm collapsed.
    expect(past_section.locator(".step-content")).to_be_hidden(timeout=3000)

    # Click the toggle button to expand.
    past_section.locator("button[data-step-toggle]").click()
    expect(past_section.locator(".step-content")).to_be_visible(timeout=3000)

    # Click again to collapse.
    past_section.locator("button[data-step-toggle]").click()
    expect(past_section.locator(".step-content")).to_be_hidden(timeout=3000)


def test_drawer_submit_posts_and_advances(server: _ServerInfo, page: Page) -> None:
    """Submit & Next POSTs to /{session_id}/submit with the gathered responses
    and marks the step COMPLETED on the server."""
    step_id = _seed_session_with_findings(server)
    _open_drawer(server, page)

    # Triage one finding so the submit body has a populated responses object.
    page.locator(".df-tb[data-action='accept']").first.click()

    # Capture the submit POST so we can assert on its payload.
    with page.expect_request(lambda r: "/submit" in r.url and r.method == "POST") as info:
        page.locator("[data-drawer-submit]").click()
    request = info.value
    assert request.post_data_json is not None
    assert request.post_data_json["step_id"] == step_id
    assert request.post_data_json["responses"]["f-drawer"] == "accept"

    # Server-side effect: the step is now COMPLETED.
    page.wait_for_function(
        "() => document.getElementById('review-drawer-container').style.display === 'none'",
        timeout=5000,
    )
    _, step = server.manager.get_step_by_id(step_id)
    assert step.state.value == "completed"


def test_drawer_triage_state_rehydrates_on_reload(server: _ServerInfo, page: Page) -> None:
    """Pre-existing saved triage responses re-hydrate $triage on drawer open.

    Seeds a session with one 'accept' response already persisted server-side,
    then opens the drawer and asserts:
      - the Accept button carries the .sa class (Datastar applied it from
        the saved_triage_responses signal initialised in the template), and
      - the triage counter shows "1".
    """
    manager = server.manager
    step_id = _seed_session_with_findings(
        server,
        session_id="rehydrate-1",
        finding_id="f-rehydrate",
    )

    # Pre-save an "accept" response so the server has state before the page loads.
    manager.save_response(
        "rehydrate-1",
        step_id,
        "f-rehydrate",
        UserResponse(action=ResponseAction.ACCEPT),
    )

    # Navigate and open the drawer.
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    item = page.locator(".attn-item").first
    expect(item).to_be_visible(timeout=5000)
    item.click()

    drawer = page.locator("#review-drawer")
    expect(drawer).to_be_visible(timeout=5000)

    # The Accept button for "f-rehydrate" should carry .sa because
    # saved_triage_responses seeded $triage with {"f-rehydrate": "accept"}.
    accept_btn = page.locator(".df-tb[data-action='accept']").first
    expect(accept_btn).to_be_visible(timeout=3000)
    expect(accept_btn).to_have_class("df-tb sa", timeout=3000)

    # Counter should reflect the one pre-saved response.
    expect(page.locator("#drawer-triage-count")).to_have_text("1", timeout=3000)


def _seed_session_for_queue(
    server: _ServerInfo,
    *,
    session_id: str,
    ticket_id: str,
    title: str,
    created_at: datetime | None = None,
) -> str:
    """Create a minimal ready ZingSession for attention-queue tests.

    The optional ``created_at`` override seeds the session and its initial
    step with a deterministic timestamp so attention-queue ordering tests
    don't need ``time.sleep`` to disambiguate wait_seconds.

    Returns the step_id.
    """
    cache = server.external_cache
    # Add the Linear issue if not already present.
    existing_ids = {i.identifier for i in cache.issues}
    if ticket_id not in existing_ids:
        cache.issues = list(cache.issues) + [
            LinearIssue(
                id=f"uuid-{session_id}",
                identifier=ticket_id,
                title=title,
                state="In Progress",
                state_type="started",
                assignee=None,
                team="Backend",
                url=f"https://linear.app/test/issue/{ticket_id}",
                updated_at=datetime.now(tz=UTC),
            )
        ]

    manager = server.manager
    session = manager.create_session(
        session_id=session_id, title=title, steps=["build-audit"], created_at=created_at
    )
    manager.update_session(session_id, ticket_id=ticket_id)
    step = session.steps[0]
    manager.start_step(session_id, step.step_id)
    manager.add_finding(
        session_id,
        step.step_id,
        {"type": "triage", "id": f"f-{session_id}", "title": f"Finding in {title}"},
    )
    manager.mark_step_ready(session_id, step.step_id)
    return step.step_id


def test_drawer_prev_next_navigation(server: _ServerInfo, page: Page) -> None:
    """Clicking Prev/Next re-renders the drawer for the adjacent session.

    Seeds three sessions into the attention queue, opens the middle one, then:
      - clicks Next  → drawer title changes to the third session's title, and
      - clicks Prev  → drawer title returns to the middle session's title.

    The prev/next buttons are only shown when $prevSessionId/$nextSessionId are
    non-empty (data-show), so their visibility is itself a signal assertion.
    """
    # Use explicit timestamps with 1-second gaps so the attention queue
    # sort (wait_seconds desc) is deterministic regardless of CI host load.
    # Past timestamps so wait_seconds is meaningful: s1 = oldest, s3 = newest.
    base = datetime.now(tz=UTC).replace(microsecond=0)
    _seed_session_for_queue(
        server,
        session_id="nav-s1",
        ticket_id="BAK-3001",
        title="Nav Session 1",
        created_at=base - timedelta(seconds=30),
    )
    _seed_session_for_queue(
        server,
        session_id="nav-s2",
        ticket_id="BAK-3002",
        title="Nav Session 2",
        created_at=base - timedelta(seconds=20),
    )
    _seed_session_for_queue(
        server,
        session_id="nav-s3",
        ticket_id="BAK-3003",
        title="Nav Session 3",
        created_at=base - timedelta(seconds=10),
    )

    # Attention queue order (wait_seconds desc): nav-s1, nav-s2, nav-s3.
    # Open nav-s2 directly via its attention-bar item so it has both prev and next.
    page.goto(f"{server.base_url}/command-center")
    page.wait_for_load_state("domcontentloaded", timeout=5000)

    # Click the attention item that opens nav-s2.
    s2_item = page.locator(".attn-item[data-on\\:click*='nav-s2']")
    expect(s2_item).to_be_visible(timeout=5000)
    s2_item.click()

    drawer = page.locator("#review-drawer")
    expect(drawer).to_be_visible(timeout=5000)
    expect(page.locator("#review-drawer .dh-title")).to_contain_text("Nav Session 2", timeout=3000)

    # Both Prev and Next buttons should be visible (nav-s2 is in the middle).
    prev_btn = page.locator("#review-drawer .dh-nav-btn[title='Previous']")
    next_btn = page.locator("#review-drawer .dh-nav-btn[title='Next']")
    expect(prev_btn).to_be_visible(timeout=3000)
    expect(next_btn).to_be_visible(timeout=3000)

    # Click Next → should load nav-s3.
    next_btn.click()
    expect(page.locator("#review-drawer .dh-title")).to_contain_text("Nav Session 3", timeout=5000)

    # Now at nav-s3 which is the last item; prev should be visible, next hidden.
    expect(page.locator("#review-drawer .dh-nav-btn[title='Previous']")).to_be_visible(timeout=3000)

    # Click Prev → should return to nav-s2.
    page.locator("#review-drawer .dh-nav-btn[title='Previous']").click()
    expect(page.locator("#review-drawer .dh-title")).to_contain_text("Nav Session 2", timeout=5000)
