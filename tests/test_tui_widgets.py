"""Tests for Zing TUI custom widgets using Textual Pilot."""

from __future__ import annotations

import pytest
from textual.app import App, ComposeResult
from textual.message import Message
from textual.widgets import Label, ListView, RadioSet

from zing_ai.orchestrator.tui.widgets import (
    ChoiceCard,
    FindingData,
    FindingGroupPanel,
    StepStatus,
    StepTracker,
    SubprocessEntry,
    SubprocessList,
    TrackerStep,
)


# ── Helper apps that mount individual widgets for testing ─────────────────────


class SubprocessListApp(App[None]):
    """Test harness for SubprocessList."""

    def __init__(self, entries: list[SubprocessEntry]) -> None:
        super().__init__()
        self._entries = entries

    def compose(self) -> ComposeResult:
        yield SubprocessList(self._entries, id="sp-list")


class StepTrackerApp(App[None]):
    """Test harness for StepTracker."""

    def __init__(self, steps: list[TrackerStep]) -> None:
        super().__init__()
        self._steps = steps

    def compose(self) -> ComposeResult:
        yield StepTracker(self._steps, id="tracker")


class ChoiceCardApp(App[None]):
    """Test harness for ChoiceCard."""

    def __init__(
        self,
        choices: list[tuple[str, str]],
        recommended_index: int = 0,
    ) -> None:
        super().__init__()
        self._choices = choices
        self._recommended_index = recommended_index
        self.collected_messages: list[Message] = []

    def compose(self) -> ComposeResult:
        yield ChoiceCard(
            title="Pick a framework",
            explanation="Choose the best option for your project.",
            choices=self._choices,
            recommended_index=self._recommended_index,
            card_id="test-card",
            id="card",
        )

    def on_choice_card_modified(self, event: ChoiceCard.Modified) -> None:
        self.collected_messages.append(event)

    def on_choice_card_deleted(self, event: ChoiceCard.Deleted) -> None:
        self.collected_messages.append(event)


class FindingGroupApp(App[None]):
    """Test harness for FindingGroupPanel."""

    def __init__(self, severity: str, findings: list[FindingData]) -> None:
        super().__init__()
        self._severity = severity
        self._findings = findings
        self.collected_actions: list[FindingGroupPanel.FindingAction] = []

    def compose(self) -> ComposeResult:
        yield FindingGroupPanel(
            severity=self._severity,
            findings=self._findings,
            id="fg-panel",
        )

    def on_finding_group_panel_finding_action(
        self, event: FindingGroupPanel.FindingAction
    ) -> None:
        self.collected_actions.append(event)


# ── SubprocessList tests ──────────────────────────────────────────────────────


class TestSubprocessList:
    """SubprocessList renders entries and emits Selected on navigation."""

    @pytest.mark.asyncio
    async def test_renders_entries(self):
        entries = [
            SubprocessEntry(label="Step 1", status="running"),
            SubprocessEntry(label="Step 2", status="pending"),
            SubprocessEntry(label="Step 3", status="success"),
        ]
        app = SubprocessListApp(entries)
        async with app.run_test():
            sp_list = app.query_one("#sp-list", SubprocessList)
            # ListView should contain 3 items
            children = list(sp_list.children)
            assert len(children) == 3

    @pytest.mark.asyncio
    async def test_emits_selected_on_key_navigation(self):
        entries = [
            SubprocessEntry(label="First", status="pending"),
            SubprocessEntry(label="Second", status="running"),
        ]
        app = SubprocessListApp(entries)
        selected_messages: list[SubprocessList.Selected] = []

        def hook(msg: Message) -> None:
            if isinstance(msg, SubprocessList.Selected):
                selected_messages.append(msg)

        async with app.run_test(message_hook=hook) as pilot:
            sp_list = app.query_one("#sp-list", SubprocessList)
            sp_list.focus()

            await pilot.press("down")
            await pilot.pause()

            # Verify the list has the correct entries
            assert sp_list._entries[0].label == "First"
            assert sp_list._entries[1].label == "Second"

            # At least one Selected message should have been emitted
            assert len(selected_messages) > 0
            assert any(m.entry.label == "Second" for m in selected_messages)


# ── StepTracker tests ─────────────────────────────────────────────────────────


class TestStepTracker:
    """StepTracker renders steps and updates status icons."""

    @pytest.mark.asyncio
    async def test_renders_steps(self):
        steps = [
            TrackerStep(label="Init", status=StepStatus.COMPLETE),
            TrackerStep(label="Build", status=StepStatus.ACTIVE),
            TrackerStep(label="Test", status=StepStatus.PENDING),
        ]
        app = StepTrackerApp(steps)
        async with app.run_test():
            tracker = app.query_one("#tracker", StepTracker)
            assert len(tracker.steps) == 3
            assert tracker.steps[0].status == StepStatus.COMPLETE
            assert tracker.steps[1].status == StepStatus.ACTIVE
            assert tracker.steps[2].status == StepStatus.PENDING

    @pytest.mark.asyncio
    async def test_status_icon_updates(self):
        steps = [
            TrackerStep(label="Step A", status=StepStatus.PENDING),
            TrackerStep(label="Step B", status=StepStatus.PENDING),
        ]
        app = StepTrackerApp(steps)
        async with app.run_test():
            tracker = app.query_one("#tracker", StepTracker)

            # Update step 0 to ACTIVE
            tracker.update_step(0, StepStatus.ACTIVE)
            assert tracker.steps[0].status == StepStatus.ACTIVE
            assert tracker.steps[1].status == StepStatus.PENDING

            # Update step 0 to COMPLETE
            tracker.update_step(0, StepStatus.COMPLETE)
            assert tracker.steps[0].status == StepStatus.COMPLETE

    @pytest.mark.asyncio
    async def test_advance(self):
        steps = [
            TrackerStep(label="Step A", status=StepStatus.ACTIVE),
            TrackerStep(label="Step B", status=StepStatus.PENDING),
            TrackerStep(label="Step C", status=StepStatus.PENDING),
        ]
        app = StepTrackerApp(steps)
        async with app.run_test():
            tracker = app.query_one("#tracker", StepTracker)

            tracker.advance()
            assert tracker.steps[0].status == StepStatus.COMPLETE
            assert tracker.steps[1].status == StepStatus.ACTIVE
            assert tracker.steps[2].status == StepStatus.PENDING

    @pytest.mark.asyncio
    async def test_failed_status(self):
        steps = [
            TrackerStep(label="Step A", status=StepStatus.ACTIVE),
        ]
        app = StepTrackerApp(steps)
        async with app.run_test():
            tracker = app.query_one("#tracker", StepTracker)
            tracker.update_step(0, StepStatus.FAILED)
            assert tracker.steps[0].status == StepStatus.FAILED


# ── ChoiceCard tests ──────────────────────────────────────────────────────────


class TestChoiceCard:
    """ChoiceCard renders, tracks modifications, and emits messages."""

    @pytest.mark.asyncio
    async def test_renders_card(self):
        choices = [("React", "A JS library"), ("Vue", "Progressive framework")]
        app = ChoiceCardApp(choices, recommended_index=0)
        async with app.run_test():
            card = app.query_one("#card", ChoiceCard)
            assert card.card_id == "test-card"
            assert card.recommended_index == 0
            assert not card.is_modified

    @pytest.mark.asyncio
    async def test_tracks_modification(self):
        choices = [("React", "A JS library"), ("Vue", "Progressive framework")]
        app = ChoiceCardApp(choices, recommended_index=0)

        async with app.run_test() as pilot:
            card = app.query_one("#card", ChoiceCard)

            # Initially not modified
            assert not card.is_modified

            # Focus the radio set and use keys to change selection.
            # RadioSet bindings: down = next, space/enter = toggle.
            radio_set = card.query_one(RadioSet)
            radio_set.focus()
            await pilot.pause()
            # Move to next radio button and press space to select it
            await pilot.press("down")
            await pilot.pause()
            await pilot.press("enter")
            await pilot.pause()

            # Card should now be modified (selection moved away from recommended)
            assert card.is_modified
            assert card.has_class("choice-card--modified")

            # A Modified message should have been collected by the app
            modified_msgs = [
                m for m in app.collected_messages if isinstance(m, ChoiceCard.Modified)
            ]
            assert len(modified_msgs) > 0
            assert modified_msgs[-1].is_modified is True
            assert modified_msgs[-1].card_id == "test-card"

    @pytest.mark.asyncio
    async def test_emits_deleted(self):
        choices = [("React", "A JS library"), ("Vue", "Progressive framework")]
        app = ChoiceCardApp(choices, recommended_index=0)

        async with app.run_test() as pilot:
            # Click the delete button
            await pilot.click("#delete-test-card")
            await pilot.pause()

            deleted_msgs = [
                m for m in app.collected_messages if isinstance(m, ChoiceCard.Deleted)
            ]
            assert len(deleted_msgs) == 1
            assert deleted_msgs[0].card_id == "test-card"

    @pytest.mark.asyncio
    async def test_has_choice_card_class(self):
        choices = [("A", "desc")]
        app = ChoiceCardApp(choices)
        async with app.run_test():
            card = app.query_one("#card", ChoiceCard)
            assert card.has_class("choice-card")


# ── FindingGroupPanel tests ───────────────────────────────────────────────────


class TestFindingGroupPanel:
    """FindingGroupPanel renders severity groups with findings."""

    @pytest.mark.asyncio
    async def test_renders_severity_header(self):
        findings = [
            FindingData(title="SQL Injection", location="api.py:10", severity="high", index=0),
        ]
        app = FindingGroupApp("high", findings)
        async with app.run_test():
            panel = app.query_one("#fg-panel", FindingGroupPanel)
            assert panel.severity == "high"
            assert len(panel.findings) == 1

            # The header label should contain the severity text
            header = panel.query_one(".fgp-header", Label)
            # Label stores text via update(); check the DOM node text content
            header_text = str(header.render())
            assert "HIGH" in header_text

    @pytest.mark.asyncio
    async def test_renders_multiple_findings(self):
        findings = [
            FindingData(title="XSS Attack", location="render.py:5", severity="critical", index=0),
            FindingData(title="CSRF Issue", location="forms.py:20", severity="critical", index=1),
            FindingData(title="Open Redirect", location="urls.py:15", severity="critical", index=2),
        ]
        app = FindingGroupApp("critical", findings)
        async with app.run_test():
            panel = app.query_one("#fg-panel", FindingGroupPanel)
            assert len(panel.findings) == 3

            # Check all finding rows rendered
            rows = panel.query("_FindingRow")
            assert len(rows) == 3

    @pytest.mark.asyncio
    async def test_renders_action_buttons(self):
        findings = [
            FindingData(title="Bug", location="main.py:1", severity="medium", index=0),
        ]
        app = FindingGroupApp("medium", findings)
        async with app.run_test():
            panel = app.query_one("#fg-panel", FindingGroupPanel)
            # Each finding row should have Fix, Skip, Discuss buttons
            buttons = panel.query("Button")
            assert len(buttons) == 3  # Fix, Skip, Discuss

    @pytest.mark.asyncio
    async def test_emits_finding_action(self):
        findings = [
            FindingData(title="Bug", location="main.py:1", severity="high", index=0),
        ]
        app = FindingGroupApp("high", findings)

        async with app.run_test() as pilot:
            # Click the "Fix" button
            await pilot.click("#fix-0")
            await pilot.pause()

            assert len(app.collected_actions) == 1
            assert app.collected_actions[0].action == "fix"
            assert app.collected_actions[0].severity == "high"
            assert app.collected_actions[0].finding_index == 0

    @pytest.mark.asyncio
    async def test_empty_findings(self):
        app = FindingGroupApp("low", [])
        async with app.run_test():
            panel = app.query_one("#fg-panel", FindingGroupPanel)
            assert len(panel.findings) == 0
            rows = panel.query("_FindingRow")
            assert len(rows) == 0
