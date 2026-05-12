"""Playwright UI tests for the plan-detail viewer.

Verifies the Convene-style viewer renders, click-to-focus mutates signals,
and release restores the default grid.
"""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path

import pytest
from playwright.sync_api import Page, expect

from tests.test_ui.conftest import _ServerInfo

pytestmark = pytest.mark.ui

FIXTURE = (
    Path(__file__).parent.parent
    / "test_viz"
    / "fixtures"
    / "BAK-1321"
    / "BAK-1321-direct-flatten.viz.json"
)


def _seed_plan_session(server: _ServerInfo, session_id: str = "plan-ui-1") -> Path:
    """Create a session backed by an on-disk markdown + viz JSON pair.

    Returns the path to the temporary work directory so the test can clean it
    up if needed. The session manager itself cleans up the session.
    """
    tmp = tempfile.mkdtemp(prefix="zing-plan-ui-")
    work = Path(tmp)
    md = work / "plan.md"
    viz = work / "plan.viz.json"
    md.write_text("# Plan\n\nbody.\n")
    shutil.copyfile(FIXTURE, viz)
    server.manager.create_session(
        session_id=session_id,
        title=session_id,
        zing_file=str(md),
        steps=["plan"],
    )
    return work


class TestPlanDetailViewer:
    """End-to-end browser tests for the plan-detail page."""

    def test_default_grid_renders_all_step_cards(self, server: _ServerInfo, page: Page) -> None:
        _seed_plan_session(server, session_id="plan-ui-default")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-default/plan",
            wait_until="domcontentloaded",
        )
        expect(page.locator("#viz-stage")).to_be_visible(timeout=5000)
        # All 13 step cards from the fixture should render.
        for n in range(1, 14):
            expect(page.locator(f"#card-{n}")).to_be_attached(timeout=2000)
        # Default-state class applied to every card.
        expect(page.locator(".viz-card--default")).to_have_count(13, timeout=2000)

    def test_click_focus_applies_pred_succ_faded_classes(
        self, server: _ServerInfo, page: Page
    ) -> None:
        _seed_plan_session(server, session_id="plan-ui-focus")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-focus/plan",
            wait_until="domcontentloaded",
        )
        expect(page.locator("#card-6")).to_be_attached(timeout=5000)

        # Click step 6 — has predecessors (step 4) and successors (steps 7, 8).
        # The default-grid stacks 13 cards vertically, so card 6 lands far
        # below the visible viewport. We dispatch a click event directly so
        # the SVG transform doesn't fight Playwright's viewport check.
        page.locator("#card-6").dispatch_event("click")

        # Card 6 picks up the focused class; 4 (pred) and 7/8 (succ) get their classes.
        expect(page.locator("#card-6.viz-card--focused")).to_be_attached(timeout=5000)
        expect(page.locator("#card-4.viz-card--pred")).to_be_attached(timeout=3000)
        expect(page.locator("#card-7.viz-card--succ")).to_be_attached(timeout=3000)
        expect(page.locator("#card-8.viz-card--succ")).to_be_attached(timeout=3000)
        # At least one unconnected card faded.
        expect(page.locator(".viz-card--faded").first).to_be_attached(timeout=3000)

    def test_release_button_restores_default_grid(self, server: _ServerInfo, page: Page) -> None:
        _seed_plan_session(server, session_id="plan-ui-release")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-release/plan",
            wait_until="domcontentloaded",
        )
        expect(page.locator("#card-6")).to_be_attached(timeout=5000)
        # The default-grid stacks 13 cards vertically, so card 6 lands far
        # below the visible viewport. We dispatch a click event directly so
        # the SVG transform doesn't fight Playwright's viewport check.
        page.locator("#card-6").dispatch_event("click")
        expect(page.locator("#card-6.viz-card--focused")).to_be_attached(timeout=5000)

        page.locator(".viz-hud__release").dispatch_event("click")
        expect(page.locator(".viz-card--default")).to_have_count(13, timeout=5000)
        # focusedStep cleared back to empty string.
        expect(page.locator(".viz-hud__status span")).to_contain_text("Default plan", timeout=2000)

    def test_escape_key_releases_focus(self, server: _ServerInfo, page: Page) -> None:
        _seed_plan_session(server, session_id="plan-ui-esc")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-esc/plan",
            wait_until="domcontentloaded",
        )
        expect(page.locator("#card-6")).to_be_attached(timeout=5000)
        # The default-grid stacks 13 cards vertically, so card 6 lands far
        # below the visible viewport. We dispatch a click event directly so
        # the SVG transform doesn't fight Playwright's viewport check.
        page.locator("#card-6").dispatch_event("click")
        expect(page.locator("#card-6.viz-card--focused")).to_be_attached(timeout=5000)

        page.keyboard.press("Escape")
        expect(page.locator(".viz-card--default")).to_have_count(13, timeout=5000)

    def test_no_console_errors_after_default_render(self, server: _ServerInfo, page: Page) -> None:
        _seed_plan_session(server, session_id="plan-ui-noerr")
        errors: list[str] = []
        page.on("pageerror", lambda exc: errors.append(str(exc)))
        page.on(
            "console",
            lambda msg: errors.append(msg.text) if msg.type == "error" else None,
        )
        page.goto(
            f"{server.base_url}/command-center/plan-ui-noerr/plan",
            wait_until="networkidle",
        )
        expect(page.locator("#viz-stage")).to_be_visible(timeout=5000)
        assert not errors, f"Console errors after render: {errors}"
