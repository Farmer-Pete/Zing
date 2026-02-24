"""Tests for AuditScreen -- finding triage grouped by severity."""

from __future__ import annotations

import pytest
from textual.app import App
from textual.widgets import Button

from zing_ai.orchestrator.commands.build_audit import Finding, FindingGroup
from zing_ai.orchestrator.tui.results import AuditResult
from zing_ai.orchestrator.tui.screens.audit import AuditScreen
from zing_ai.orchestrator.tui.widgets.finding_group import FindingGroupPanel


# ── Fixtures ──────────────────────────────────────────────────────────────


def _make_finding_groups() -> list[FindingGroup]:
    """Create sample FindingGroup data covering multiple severities."""
    return [
        FindingGroup(
            severity="high",
            findings=[
                Finding(
                    index=0,
                    category="security",
                    severity="high",
                    confidence="high",
                    location="src/auth.py:12",
                    title="SQL injection in login query",
                ),
                Finding(
                    index=1,
                    category="security",
                    severity="high",
                    confidence="medium",
                    location="src/auth.py:45",
                    title="Hardcoded secret key",
                ),
            ],
        ),
        FindingGroup(
            severity="medium",
            findings=[
                Finding(
                    index=2,
                    category="quality",
                    severity="medium",
                    confidence="high",
                    location="src/utils.py:88",
                    title="Unused import of os module",
                ),
            ],
        ),
        FindingGroup(
            severity="low",
            findings=[
                Finding(
                    index=3,
                    category="style",
                    severity="low",
                    confidence="low",
                    location="src/main.py:3",
                    title="Missing docstring on main()",
                ),
            ],
        ),
    ]


# ── Helper app ────────────────────────────────────────────────────────────


class AuditScreenApp(App[AuditResult | None]):
    """Test harness that pushes an AuditScreen and captures its result."""

    def __init__(
        self, finding_groups: list[FindingGroup] | None = None
    ) -> None:
        super().__init__()
        self._finding_groups = (
            _make_finding_groups() if finding_groups is None else finding_groups
        )
        self.screen_result: AuditResult | None = None

    def on_mount(self) -> None:
        screen = AuditScreen(self._finding_groups)
        self.push_screen(screen, callback=self._on_dismiss)

    def _on_dismiss(self, result: AuditResult) -> None:
        self.screen_result = result
        self.exit()


# ── Tests ─────────────────────────────────────────────────────────────────


class TestAuditScreenRender:
    """AuditScreen should compose FindingGroupPanels grouped by severity."""

    @pytest.mark.asyncio
    async def test_renders_finding_groups_by_severity(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            panels = list(screen.query(FindingGroupPanel))
            # Should have 3 panels: high, medium, low
            assert len(panels) == 3

            # Verify severity ordering matches input (high -> medium -> low)
            assert panels[0].severity == "high"
            assert panels[1].severity == "medium"
            assert panels[2].severity == "low"

    @pytest.mark.asyncio
    async def test_correct_finding_count_per_group(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            panels = list(screen.query(FindingGroupPanel))
            # High has 2 findings, medium has 1, low has 1
            assert len(panels[0].findings) == 2
            assert len(panels[1].findings) == 1
            assert len(panels[2].findings) == 1

    @pytest.mark.asyncio
    async def test_renders_submit_button(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            btn = screen.query_one("#btn-submit", Button)
            assert btn is not None
            assert str(btn.label) == "Submit Decisions"

    @pytest.mark.asyncio
    async def test_renders_footer_status(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # Check via the screen's internal status text helper
            text = screen._status_text()
            # All 4 findings default to "skip"
            assert "4 skip" in text
            assert "4 total" in text


class TestFixButton:
    """Clicking Fix should record a 'fix' action for the finding."""

    @pytest.mark.asyncio
    async def test_fix_button_records_fix_action(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # Simulate a FindingAction message for finding 0 with "fix"
            msg = FindingGroupPanel.FindingAction(
                severity="high",
                finding_index=0,
                action="fix",
            )
            screen.on_finding_group_panel_finding_action(msg)
            await pilot.pause()

            # The decision should be recorded
            assert screen.decisions[("high", 0)] == "fix"

    @pytest.mark.asyncio
    async def test_fix_updates_footer_status(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # Mark finding 0 as "fix"
            msg = FindingGroupPanel.FindingAction(
                severity="high",
                finding_index=0,
                action="fix",
            )
            screen.on_finding_group_panel_finding_action(msg)
            await pilot.pause()

            text = screen._status_text()
            assert "1 fix" in text


class TestSkipButton:
    """Skip button should record a 'skip' action for the finding."""

    @pytest.mark.asyncio
    async def test_skip_button_records_skip_action(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # First change to "fix", then back to "skip"
            msg_fix = FindingGroupPanel.FindingAction(
                severity="high",
                finding_index=0,
                action="fix",
            )
            screen.on_finding_group_panel_finding_action(msg_fix)
            await pilot.pause()

            assert screen.decisions[("high", 0)] == "fix"

            msg_skip = FindingGroupPanel.FindingAction(
                severity="high",
                finding_index=0,
                action="skip",
            )
            screen.on_finding_group_panel_finding_action(msg_skip)
            await pilot.pause()

            assert screen.decisions[("high", 0)] == "skip"

    @pytest.mark.asyncio
    async def test_default_action_is_skip(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # All findings should default to "skip"
            for key, action in screen.decisions.items():
                assert action == "skip"


class TestSubmitDismisses:
    """Submitting should dismiss with AuditResult containing all decisions."""

    @pytest.mark.asyncio
    async def test_submit_button_dismisses_with_audit_result(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # Mark some findings with different actions
            screen.on_finding_group_panel_finding_action(
                FindingGroupPanel.FindingAction(
                    severity="high", finding_index=0, action="fix"
                )
            )
            screen.on_finding_group_panel_finding_action(
                FindingGroupPanel.FindingAction(
                    severity="medium", finding_index=2, action="discuss"
                )
            )
            await pilot.pause()

            # Click submit
            btn = screen.query_one("#btn-submit", Button)
            await pilot.click(f"#{btn.id}")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, AuditResult)

            # Should have 4 decisions (one per finding)
            assert len(result.decisions) == 4

            # Check specific decisions by finding_index
            decisions_by_index = {
                d["finding_index"]: d for d in result.decisions
            }
            assert decisions_by_index[0]["action"] == "fix"
            assert decisions_by_index[0]["severity"] == "high"
            assert decisions_by_index[1]["action"] == "skip"
            assert decisions_by_index[1]["severity"] == "high"
            assert decisions_by_index[2]["action"] == "discuss"
            assert decisions_by_index[2]["severity"] == "medium"
            assert decisions_by_index[3]["action"] == "skip"
            assert decisions_by_index[3]["severity"] == "low"

    @pytest.mark.asyncio
    async def test_s_key_dismisses_with_audit_result(self):
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # Press 's' to submit (key binding)
            await pilot.press("s")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, AuditResult)
            # All 4 findings should have decisions (all defaulting to "skip")
            assert len(result.decisions) == 4
            for d in result.decisions:
                assert d["action"] == "skip"

    @pytest.mark.asyncio
    async def test_submit_with_all_fix(self):
        """When all findings are marked as fix, result reflects that."""
        app = AuditScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            # Mark all findings as "fix"
            for (severity, finding_index) in list(screen.decisions.keys()):
                screen.on_finding_group_panel_finding_action(
                    FindingGroupPanel.FindingAction(
                        severity=severity,
                        finding_index=finding_index,
                        action="fix",
                    )
                )
            await pilot.pause()

            await pilot.press("s")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert all(d["action"] == "fix" for d in result.decisions)

    @pytest.mark.asyncio
    async def test_empty_findings_still_works(self):
        """Screen with no findings should still be dismissable."""
        app = AuditScreenApp(finding_groups=[])
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, AuditScreen)
            await pilot.pause()

            panels = list(screen.query(FindingGroupPanel))
            assert len(panels) == 0

            # Submit via keybinding
            await pilot.press("s")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, AuditResult)
            assert result.decisions == []
