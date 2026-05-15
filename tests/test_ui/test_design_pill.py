"""Playwright UI tests for the Design pill on kanban cards."""

from __future__ import annotations

import shutil
from datetime import UTC, datetime
from pathlib import Path

import pytest
from playwright.sync_api import Page, expect

from tests.test_command_center.conftest import (
    make_session,
    make_workflow_step,
)
from tests.test_ui.conftest import _ServerInfo
from zing_ai.server.models import SessionState
from zing_ai.server.models_external import LinearIssue

pytestmark = pytest.mark.ui

FIXTURE = (
    Path(__file__).parent.parent
    / "test_viz"
    / "fixtures"
    / "BAK-1321"
    / "BAK-1321-direct-flatten.viz.json"
)


def _seed_card_with_plan(server: _ServerInfo, tmp_path: Path) -> tuple[Path, str]:
    """Seed an issue, a ZingSession with a real on-disk viz.json sibling.

    Returns ``(work_dir, session_id)``. ``tmp_path`` is pytest's auto-cleaned
    temp directory fixture.
    """
    work = tmp_path / "design-pill-seed"
    work.mkdir()
    md = work / "BAK-2001-some-plan.md"
    viz = work / "BAK-2001-some-plan.viz.json"
    md.write_text("# Plan\n")
    shutil.copyfile(FIXTURE, viz)

    issue = LinearIssue(
        id="uuid-BAK-2001",
        identifier="BAK-2001",
        title="Has a viz plan",
        state="In Progress",
        state_type="started",
        assignee=None,
        team="Backend",
        url="https://linear.app/test/issue/BAK-2001",
        updated_at=datetime.now(tz=UTC),
    )
    server.external_cache.issues = [issue]
    server.external_cache.version += 1

    session = make_session(
        session_id="zs-design",
        ticket_id="BAK-2001",
        steps=[make_workflow_step(step_name="plan", state=SessionState.STARTED)],
        zing_file=str(md),
    )
    server.manager._sessions[session.session_id] = session
    return work, session.session_id


class TestDesignPill:
    def test_design_pill_links_to_plan_route(
        self, server: _ServerInfo, page: Page, tmp_path: Path
    ) -> None:
        _, session_id = _seed_card_with_plan(server, tmp_path)

        page.goto(f"{server.base_url}/command-center", wait_until="domcontentloaded")
        card = page.locator("#card-bak-2001")
        expect(card).to_be_visible(timeout=5000)

        pill = card.locator(".design-pill")
        expect(pill).to_be_visible(timeout=3000)
        href = pill.get_attribute("href")
        assert href == f"/command-center/{session_id}/plan"

    def test_end_to_end_click_through_to_plan_detail_and_focus(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        """Click Design pill on kanban → land on plan-detail → focus a step."""
        _, session_id = _seed_card_with_plan(server, tmp_path)

        page.goto(f"{server.base_url}/command-center", wait_until="domcontentloaded")
        card = page.locator("#card-bak-2001")
        expect(card).to_be_visible(timeout=5000)
        pill = card.locator(".design-pill")
        expect(pill).to_be_visible(timeout=3000)

        # Click the pill — should navigate to /command-center/<sid>/plan
        pill.click()
        page.wait_for_url(
            f"{server.base_url}/command-center/{session_id}/plan",
            timeout=5000,
        )
        expect(page.locator("#viz-stage")).to_be_visible(timeout=5000)
        # Default-grid renders all 13 fixture cards.
        expect(page.locator(".viz-card--default")).to_have_count(13, timeout=3000)

        # Focus interaction still works end-to-end.
        page.locator("#card-6").dispatch_event("click")
        expect(page.locator("#card-6.viz-card--focused")).to_be_attached(timeout=5000)

    def test_no_design_pill_when_no_viz_json_on_disk(
        self, server: _ServerInfo, page: Page, tmp_path: Path
    ) -> None:
        # Seed an issue + session but NO viz.json sibling.
        work = tmp_path / "noviz-seed"
        work.mkdir()
        md = work / "BAK-2002-noviz.md"
        md.write_text("# Plan\n")

        issue = LinearIssue(
            id="uuid-BAK-2002",
            identifier="BAK-2002",
            title="No viz here",
            state="In Progress",
            state_type="started",
            assignee=None,
            team="Backend",
            url="https://linear.app/test/issue/BAK-2002",
            updated_at=datetime.now(tz=UTC),
        )
        server.external_cache.issues = [issue]
        server.external_cache.version += 1

        session = make_session(
            session_id="zs-noviz",
            ticket_id="BAK-2002",
            steps=[make_workflow_step(step_name="plan", state=SessionState.STARTED)],
            zing_file=str(md),
        )
        server.manager._sessions[session.session_id] = session

        page.goto(f"{server.base_url}/command-center", wait_until="domcontentloaded")
        card = page.locator("#card-bak-2002")
        expect(card).to_be_visible(timeout=5000)
        expect(card.locator(".design-pill")).to_have_count(0, timeout=2000)
