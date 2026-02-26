"""Tests for BuildScreen -- step tracking with live log output."""

from __future__ import annotations

import pytest
from textual.app import App
from textual.widgets import RichLog

from zing_ai.orchestrator.models import Stage, Step
from zing_ai.orchestrator.tui.results import BuildResult
from zing_ai.orchestrator.tui.screens.build import BuildScreen
from zing_ai.orchestrator.tui.widgets.step_tracker import (
    StepStatus,
    StepTracker,
)


# ── Helper fixtures ─────────────────────────────────────────────────────


def _make_stages() -> list[Stage]:
    """Return a small list of stages for testing."""
    return [
        Stage(
            label="Setup",
            steps=[
                Step(label="Install deps", instructions="npm install", files=[]),
                Step(label="Lint", instructions="npm run lint", files=[]),
            ],
        ),
        Stage(
            label="Build",
            steps=[
                Step(label="Compile", instructions="npm run build", files=[]),
            ],
        ),
    ]


class BuildScreenApp(App[BuildResult | None]):
    """Test harness that pushes a BuildScreen and captures its result."""

    def __init__(self, stages: list[Stage]) -> None:
        super().__init__()
        self._stages = stages
        self.screen_result: BuildResult | None = None

    def on_mount(self) -> None:
        screen = BuildScreen(self._stages)
        self.push_screen(screen, callback=self._on_dismiss)

    def _on_dismiss(self, result: BuildResult) -> None:
        self.screen_result = result
        self.exit()


# ── Tests ───────────────────────────────────────────────────────────────


class TestBuildScreenBeforeMount:
    """Methods called on an unmounted BuildScreen must not crash."""

    def test_start_step_before_mount_no_crash(self):
        screen = BuildScreen(_make_stages())
        screen.start_step(0, 0)

    def test_append_output_before_mount_no_crash(self):
        screen = BuildScreen(_make_stages())
        screen.append_output("hello")
        assert screen._log_lines == ["hello"]

    def test_complete_step_before_mount_no_crash(self):
        screen = BuildScreen(_make_stages())
        screen.complete_step(0, 0, success=True)
        assert (0, 0) in screen._completed

    def test_complete_step_failure_before_mount_no_crash(self):
        screen = BuildScreen(_make_stages())
        screen.complete_step(0, 0, success=False)
        assert screen._failed == (0, 0)


class TestBuildScreenRender:
    """BuildScreen should compose with StepTracker and RichLog."""

    @pytest.mark.asyncio
    async def test_renders_step_tracker_and_rich_log(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test():
            screen = app.screen
            tracker = screen.query_one("#step-tracker", StepTracker)
            log_view = screen.query_one("#log-view", RichLog)
            assert tracker is not None
            assert log_view is not None

    @pytest.mark.asyncio
    async def test_tracker_has_correct_step_count(self):
        stages = _make_stages()
        app = BuildScreenApp(stages)
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            tracker = screen.query_one("#step-tracker", StepTracker)
            # 2 steps in stage 0, 1 step in stage 1 = 3 total
            assert len(tracker.steps) == 3

    @pytest.mark.asyncio
    async def test_all_steps_initially_pending(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            tracker = screen.query_one("#step-tracker", StepTracker)
            for step in tracker.steps:
                assert step.status == StepStatus.PENDING


class TestStartStep:
    """start_step should highlight the correct step and clear the log."""

    @pytest.mark.asyncio
    async def test_start_step_highlights_correct_step(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.start_step(0, 0)
            await pilot.pause()

            tracker = screen.query_one("#step-tracker", StepTracker)
            steps = tracker.steps
            assert steps[0].status == StepStatus.ACTIVE
            assert steps[1].status == StepStatus.PENDING
            assert steps[2].status == StepStatus.PENDING

    @pytest.mark.asyncio
    async def test_start_step_clears_log(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            # Write some output first
            screen.append_output("some previous output")
            await pilot.pause()

            # Starting a new step should clear the log
            screen.start_step(0, 1)
            await pilot.pause()

            # The internal log buffer should be cleared
            assert screen._log_lines == []

    @pytest.mark.asyncio
    async def test_start_step_second_stage(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            # Start a step in the second stage (stage_idx=1, step_idx=0)
            screen.start_step(1, 0)
            await pilot.pause()

            tracker = screen.query_one("#step-tracker", StepTracker)
            steps = tracker.steps
            # Flat index 2 (third step) should be active
            assert steps[2].status == StepStatus.ACTIVE
            assert steps[0].status == StepStatus.PENDING
            assert steps[1].status == StepStatus.PENDING


class TestAppendOutput:
    """append_output should write to the RichLog."""

    @pytest.mark.asyncio
    async def test_append_output_writes_to_richlog(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.append_output("hello world")
            await pilot.pause()

            # The line should be captured in the internal buffer
            assert screen._log_lines == ["hello world"]

    @pytest.mark.asyncio
    async def test_append_output_multiple_lines(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.append_output("line 1")
            screen.append_output("line 2")
            screen.append_output("line 3")
            await pilot.pause()

            assert screen._log_lines == ["line 1", "line 2", "line 3"]


class TestCompleteStep:
    """complete_step should update the step icon based on success/failure."""

    @pytest.mark.asyncio
    async def test_complete_step_success_shows_check(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.start_step(0, 0)
            await pilot.pause()

            screen.complete_step(0, 0, success=True)
            await pilot.pause()

            tracker = screen.query_one("#step-tracker", StepTracker)
            steps = tracker.steps
            assert steps[0].status == StepStatus.COMPLETE

    @pytest.mark.asyncio
    async def test_complete_step_failure_shows_x(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.start_step(0, 0)
            await pilot.pause()

            screen.complete_step(0, 0, success=False)
            await pilot.pause()

            tracker = screen.query_one("#step-tracker", StepTracker)
            steps = tracker.steps
            assert steps[0].status == StepStatus.FAILED

    @pytest.mark.asyncio
    async def test_complete_step_tracks_completed_steps(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.complete_step(0, 0, success=True)
            screen.complete_step(0, 1, success=True)
            await pilot.pause()

            assert (0, 0) in screen._completed
            assert (0, 1) in screen._completed

    @pytest.mark.asyncio
    async def test_complete_step_tracks_failed_step(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.complete_step(0, 0, success=False)
            await pilot.pause()

            assert screen._failed == (0, 0)


class TestFinish:
    """finish should dismiss the screen with a BuildResult."""

    @pytest.mark.asyncio
    async def test_finish_dismisses_with_result(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.complete_step(0, 0, success=True)
            screen.complete_step(0, 1, success=True)
            screen.complete_step(1, 0, success=True)
            await pilot.pause()

            screen.finish()
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, BuildResult)
            assert len(result.completed_steps) == 3
            assert result.failed_step is None

    @pytest.mark.asyncio
    async def test_finish_with_failure(self):
        app = BuildScreenApp(_make_stages())
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, BuildScreen)
            await pilot.pause()

            screen.complete_step(0, 0, success=True)
            screen.complete_step(0, 1, success=False)
            await pilot.pause()

            screen.finish()
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, BuildResult)
            assert len(result.completed_steps) == 1
            assert result.failed_step == (0, 1)
