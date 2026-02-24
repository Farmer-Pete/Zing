"""Tests for ProgressScreen -- subprocess tracking with live log output."""

from __future__ import annotations

import pytest
from textual.app import App, ComposeResult
from textual.widgets import RichLog

from zing_ai.orchestrator.tui.results import ProgressResult
from zing_ai.orchestrator.tui.screens.progress import ProgressScreen
from zing_ai.orchestrator.tui.widgets.subprocess_list import SubprocessList


# ── Helper app that pushes ProgressScreen ──────────────────────────────────


class ProgressScreenApp(App[ProgressResult | None]):
    """Test harness that pushes a ProgressScreen and captures its result."""

    def __init__(self) -> None:
        super().__init__()
        self.screen_result: ProgressResult | None = None

    def on_mount(self) -> None:
        screen = ProgressScreen()
        self.push_screen(screen, callback=self._on_dismiss)

    def _on_dismiss(self, result: ProgressResult) -> None:
        self.screen_result = result
        self.exit()


# ── Tests ──────────────────────────────────────────────────────────────────


class TestProgressScreenRender:
    """ProgressScreen should compose with SubprocessList and RichLog."""

    @pytest.mark.asyncio
    async def test_renders_subprocess_list_and_rich_log(self):
        app = ProgressScreenApp()
        async with app.run_test():
            screen = app.screen
            sp_list = screen.query_one("#sp-list", SubprocessList)
            log_view = screen.query_one("#log-view", RichLog)
            assert sp_list is not None
            assert log_view is not None


class TestAddSubprocess:
    """add_subprocess should add entries to the SubprocessList."""

    @pytest.mark.asyncio
    async def test_add_subprocess_adds_entries(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Build frontend")
            screen.add_subprocess("proc-2", "Build backend")
            await pilot.pause()

            sp_list = screen.query_one("#sp-list", SubprocessList)
            children = list(sp_list.children)
            assert len(children) == 2

    @pytest.mark.asyncio
    async def test_add_subprocess_auto_selects_first(self):
        app = ProgressScreenApp()
        async with app.run_test():
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "First process")
            assert screen._selected_id == "proc-1"


class TestUpdateStatus:
    """update_status should change the subprocess status dot/badge."""

    @pytest.mark.asyncio
    async def test_update_status_changes_dot_color(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Compile")
            await pilot.pause()

            # Initially pending
            assert screen._statuses["proc-1"] == "pending"

            screen.update_status("proc-1", "running")
            await pilot.pause()

            assert screen._statuses["proc-1"] == "running"

            screen.update_status("proc-1", "success")
            await pilot.pause()

            assert screen._statuses["proc-1"] == "success"

    @pytest.mark.asyncio
    async def test_update_status_ignores_unknown_id(self):
        app = ProgressScreenApp()
        async with app.run_test():
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            # Should not raise
            screen.update_status("nonexistent", "running")


class TestAppendOutput:
    """append_output should write to the correct subprocess log buffer."""

    @pytest.mark.asyncio
    async def test_append_output_stores_in_buffer(self):
        app = ProgressScreenApp()
        async with app.run_test():
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Task A")

            screen.append_output("proc-1", "line 1")
            screen.append_output("proc-1", "line 2")

            assert screen._buffers["proc-1"] == ["line 1", "line 2"]

    @pytest.mark.asyncio
    async def test_append_output_writes_to_richlog_when_selected(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Task A")
            await pilot.pause()

            # proc-1 should be auto-selected
            assert screen._selected_id == "proc-1"

            screen.append_output("proc-1", "hello world")
            await pilot.pause()

            # The RichLog should have content written to it
            # (we verify the buffer is correct; RichLog internal state
            # is hard to inspect directly)
            assert "hello world" in screen._buffers["proc-1"]

    @pytest.mark.asyncio
    async def test_append_output_buffers_non_selected(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Task A")
            screen.add_subprocess("proc-2", "Task B")
            await pilot.pause()

            # proc-1 is auto-selected; writing to proc-2 should buffer only
            screen.append_output("proc-2", "background output")

            assert screen._buffers["proc-2"] == ["background output"]
            # proc-1 buffer still empty
            assert screen._buffers["proc-1"] == []


class TestSelectSubprocess:
    """Selecting a subprocess in the list should switch the RichLog content."""

    @pytest.mark.asyncio
    async def test_selecting_subprocess_switches_richlog(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Task A")
            screen.add_subprocess("proc-2", "Task B")
            await pilot.pause()

            # Write some output to both
            screen.append_output("proc-1", "output from A")
            screen.append_output("proc-2", "output from B")
            await pilot.pause()

            # Simulate selection change by calling the handler directly
            # (since Pilot key navigation through ListView can be flaky
            # in headless tests, we test the handler logic directly)
            from zing_ai.orchestrator.tui.widgets.subprocess_list import (
                SubprocessEntry,
            )

            event = SubprocessList.Selected(
                index=1, entry=SubprocessEntry(label="Task B", status="pending")
            )
            screen.on_subprocess_list_selected(event)
            await pilot.pause()

            assert screen._selected_id == "proc-2"

    @pytest.mark.asyncio
    async def test_selecting_same_subprocess_is_noop(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Task A")
            await pilot.pause()

            from zing_ai.orchestrator.tui.widgets.subprocess_list import (
                SubprocessEntry,
            )

            # Select proc-1 again (it's already selected)
            event = SubprocessList.Selected(
                index=0, entry=SubprocessEntry(label="Task A", status="pending")
            )
            screen.on_subprocess_list_selected(event)
            await pilot.pause()

            # Should remain proc-1, no crash
            assert screen._selected_id == "proc-1"


class TestMarkAllComplete:
    """mark_all_complete should dismiss the screen with a ProgressResult."""

    @pytest.mark.asyncio
    async def test_mark_all_complete_dismisses_with_result(self):
        app = ProgressScreenApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, ProgressScreen)

            screen.add_subprocess("proc-1", "Task A")
            screen.add_subprocess("proc-2", "Task B")
            await pilot.pause()

            screen.update_status("proc-1", "success")
            screen.update_status("proc-2", "success")
            screen.append_output("proc-1", "done A")
            screen.append_output("proc-2", "done B")
            await pilot.pause()

            screen.mark_all_complete()
            await pilot.pause()

            # The app should have captured the result
            result = app.screen_result
            assert result is not None
            assert isinstance(result, ProgressResult)
            assert result.statuses == {"proc-1": "success", "proc-2": "success"}
            assert result.outputs["proc-1"] == "done A"
            assert result.outputs["proc-2"] == "done B"
