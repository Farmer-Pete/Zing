"""Playwright UI tests for the plan-detail viewer.

Verifies the Convene-style viewer renders, click-to-focus mutates signals,
and release restores the default grid.
"""

from __future__ import annotations

import shutil
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
MIXED_AXIS_FIXTURE = (
    Path(__file__).parent.parent
    / "test_viz"
    / "fixtures"
    / "mixed-axis-activity-feed"
    / "activity-feed.viz.json"
)


def _seed_plan_session(server: _ServerInfo, tmp_path: Path, session_id: str = "plan-ui-1") -> Path:
    """Create a session backed by an on-disk markdown + viz JSON pair.

    ``tmp_path`` is pytest's auto-cleaned temporary directory fixture — pass
    it from the test to get free cleanup. The session manager cleans up the
    in-memory session separately.
    """
    work = tmp_path / session_id
    work.mkdir()
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


def _seed_mixed_axis_session(
    server: _ServerInfo, tmp_path: Path, session_id: str = "plan-ui-mixed"
) -> Path:
    """Create a session backed by the mixed-axis (logic + struct) fixture."""
    work = tmp_path / session_id
    work.mkdir()
    md = work / "plan.md"
    viz = work / "plan.viz.json"
    md.write_text("# Activity feed\n\nMixed-axis worked example.\n")
    shutil.copyfile(MIXED_AXIS_FIXTURE, viz)
    server.manager.create_session(
        session_id=session_id,
        title=session_id,
        zing_file=str(md),
        steps=["plan"],
    )
    return work


class TestPlanDetailViewer:
    """End-to-end browser tests for the plan-detail page."""

    def test_default_grid_renders_all_step_cards(
        self, server: _ServerInfo, page: Page, tmp_path: Path
    ) -> None:
        _seed_plan_session(server, tmp_path, session_id="plan-ui-default")
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
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        _seed_plan_session(server, tmp_path, session_id="plan-ui-focus")
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

    def test_focus_patched_cards_stay_in_svg_namespace(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        """Regression: cards patched via SSE must stay in the SVG namespace.

        Datastar parses ``patch_elements`` markup as HTML by default. Without
        ``namespace=svg`` on the SSE event, the new ``<g>`` lands in the
        XHTML namespace, attaches to the DOM with the right class, but
        renders at zero size — the focus interaction silently wipes the
        canvas. Existing tests passed because ``to_be_attached`` only
        checks DOM presence; this test asserts both the namespace and a
        nonzero bounding rect so future regressions in routes_plans.py or
        the datastar-py SDK get caught.
        """
        _seed_plan_session(server, tmp_path, session_id="plan-ui-ns")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-ns/plan",
            wait_until="domcontentloaded",
        )
        expect(page.locator("#card-6")).to_be_attached(timeout=5000)
        page.locator("#card-6").dispatch_event("click")
        expect(page.locator("#card-6.viz-card--focused")).to_be_attached(timeout=5000)

        snapshot = page.evaluate(
            """
            () => {
              const c = document.getElementById('card-6');
              const r = c.getBoundingClientRect();
              return {
                ns: c.namespaceURI,
                w: Math.round(r.width),
                h: Math.round(r.height),
              };
            }
            """
        )
        assert snapshot["ns"] == "http://www.w3.org/2000/svg", (
            f"focused card landed in {snapshot['ns']!r} — namespace=svg missing from "
            "patch_elements? (see routes_plans._patch_svg)"
        )
        assert snapshot["w"] > 0 and snapshot["h"] > 0, (
            f"focused card has zero size {snapshot['w']}x{snapshot['h']} — "
            "Datastar likely parsed the SVG fragment as HTML"
        )

    def test_release_button_restores_default_grid(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        _seed_plan_session(server, tmp_path, session_id="plan-ui-release")
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

    def test_escape_key_releases_focus(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        _seed_plan_session(server, tmp_path, session_id="plan-ui-esc")
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

    def test_no_console_errors_after_default_render(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        _seed_plan_session(server, tmp_path, session_id="plan-ui-noerr")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-noerr/plan",
            wait_until="networkidle",
        )
        expect(page.locator("#viz-stage")).to_be_visible(timeout=5000)
        # Listener-based check happens in the console_errors fixture teardown.

    def test_mixed_axis_renders_logic_and_struct_in_one_scene(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        """A viz with both logic primitives (rect, diamond) and struct primitives
        (struct/union/collections) must render both axes correctly in the same
        scene. The struct rendering does not erase the existing logic affordances.
        """
        _seed_mixed_axis_session(server, tmp_path, session_id="plan-ui-mixed")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-mixed/plan",
            wait_until="domcontentloaded",
        )
        # Logic-axis nodes still attach.
        expect(page.locator(".viz-shape--rect").first).to_be_attached(timeout=5000)
        expect(page.locator(".viz-shape--diamond").first).to_be_attached(timeout=2000)
        # Struct-axis nodes attach with the wrapper class and the kind discriminator.
        expect(page.locator(".viz-shape--struct").first).to_be_attached(timeout=2000)
        expect(page.locator(".viz-struct--union").first).to_be_attached(timeout=2000)
        expect(page.locator(".viz-struct--collections").first).to_be_attached(timeout=2000)
        # Per-row side coding lands as class names on the row rects.
        expect(page.locator(".viz-struct__row--added").first).to_be_attached(timeout=2000)
        expect(page.locator(".viz-struct__row--removed").first).to_be_attached(timeout=2000)
        expect(page.locator(".viz-struct__row--diverged").first).to_be_attached(timeout=2000)
        expect(page.locator(".viz-struct__row--unchanged").first).to_be_attached(timeout=2000)
        # Field text actually renders inside struct rows (the EventPayload struct
        # has a diverged actor_id with today=int proposed=UUID).
        page_text = page.locator("#viz-stage").inner_text()
        assert "EventPayload" in page_text, "struct type name should render"
        assert "actor_id" in page_text, "struct field name should render"
        assert "int" in page_text and "UUID" in page_text, "diverged today/proposed should render"
        # Click an existing rect — the logic-side focus affordance still works.
        page.locator("#card-1").dispatch_event("click")
        expect(page.locator("#card-1.viz-card--focused")).to_be_attached(timeout=5000)

    def test_legend_collapses_and_expands(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        """The legend starts collapsed (only a button), expands on click,
        and includes all four sections: shapes, sides, cross-flow lines,
        focus states."""
        _seed_plan_session(server, tmp_path, session_id="plan-ui-legend")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-legend/plan",
            wait_until="domcontentloaded",
        )
        toggle = page.locator(".viz-legend__toggle")
        panel = page.locator("#viz-legend-panel")
        expect(toggle).to_be_visible(timeout=5000)
        expect(panel).to_be_hidden()

        toggle.click()
        expect(panel).to_be_visible(timeout=2000)
        expect(page.locator(".viz-legend__heading", has_text="Shapes")).to_be_visible()
        expect(page.locator(".viz-legend__heading", has_text="Change markers")).to_be_visible()
        expect(page.locator(".viz-legend__heading", has_text="Cross-flow lines")).to_be_visible()
        expect(page.locator(".viz-legend__heading", has_text="Focus states")).to_be_visible()

        toggle.click()
        expect(panel).to_be_hidden(timeout=2000)

    def test_tabs_toggle_viz_and_markdown_panes(
        self,
        server: _ServerInfo,
        page: Page,
        console_errors: list[str],
        tmp_path: Path,
    ) -> None:
        """Viz is default; clicking Markdown swaps panes; clicking Viz again
        restores it and the gesture handler still works (card click focuses)."""
        _seed_plan_session(server, tmp_path, session_id="plan-ui-tabs")
        page.goto(
            f"{server.base_url}/command-center/plan-ui-tabs/plan",
            wait_until="domcontentloaded",
        )
        viz = page.locator(".plan-viz")
        md = page.locator(".plan-md")
        expect(viz).to_be_visible(timeout=5000)
        expect(md).to_be_hidden()

        page.locator(".plan-tab", has_text="Markdown").click()
        expect(md).to_be_visible(timeout=2000)
        expect(viz).to_be_hidden()

        page.locator(".plan-tab", has_text="Visualization").click()
        expect(viz).to_be_visible(timeout=2000)
        expect(md).to_be_hidden()

        # After round-tripping tabs, the viz is still interactive.
        page.locator("#card-6").dispatch_event("click")
        expect(page.locator("#card-6.viz-card--focused")).to_be_attached(timeout=5000)
