"""BuildScreen -- displays build execution with step tracking and live log output.

Layout: :class:`StepTracker` docked left (width 30) + :class:`RichLog`
filling the remaining space.  As each step runs, its output streams into
the ``RichLog`` and the ``StepTracker`` highlights the active step.
"""

from __future__ import annotations

import logging

from textual.app import ComposeResult
from textual.screen import Screen
from textual.widgets import RichLog

from zing_ai.orchestrator.models import Stage
from zing_ai.orchestrator.tui.notifications import notify
from zing_ai.orchestrator.tui.results import BuildResult
from zing_ai.orchestrator.tui.widgets.step_tracker import (
    StepStatus,
    StepTracker,
    TrackerStep,
)

logger = logging.getLogger(__name__)


class BuildScreen(Screen[BuildResult]):
    """Screen that tracks build steps with a sidebar tracker and log viewer.

    Takes a list of :class:`Stage` objects and flattens their steps into
    a :class:`StepTracker` docked on the left.  A :class:`RichLog` fills
    the remaining space to display the output of the currently-running step.
    """

    DEFAULT_CSS = """
    BuildScreen {
        layout: horizontal;
    }
    BuildScreen StepTracker {
        dock: left;
        width: 30;
        height: 100%;
    }
    BuildScreen RichLog {
        width: 1fr;
        height: 100%;
    }
    """

    def __init__(
        self,
        stages: list[Stage],
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._stages = stages
        total_steps = sum(len(s.steps) for s in stages)
        logger.debug(
            "BuildScreen created: %d stage(s), %d total step(s)",
            len(stages), total_steps,
        )
        # Build a flat list of tracker steps and a mapping from
        # (stage_idx, step_idx) -> flat index.
        self._tracker_steps: list[TrackerStep] = []
        self._flat_index: dict[tuple[int, int], int] = {}
        for stage_idx, stage in enumerate(stages):
            for step_idx, step in enumerate(stage.steps):
                flat_idx = len(self._tracker_steps)
                self._flat_index[(stage_idx, step_idx)] = flat_idx
                self._tracker_steps.append(
                    TrackerStep(label=step.label, status=StepStatus.PENDING)
                )
        # Track completed and failed steps for the final result.
        self._completed: list[tuple[int, int]] = []
        self._failed: tuple[int, int] | None = None
        # Buffer of output lines for the current step (cleared on start_step).
        self._log_lines: list[str] = []

    def compose(self) -> ComposeResult:
        yield StepTracker(self._tracker_steps, id="step-tracker")
        yield RichLog(highlight=True, markup=False, id="log-view")

    # ── Public API ───────────────────────────────────────────────────────

    def start_step(self, stage_idx: int, step_idx: int) -> None:
        """Highlight the given step in the tracker and clear the log.

        Marks the step as *active* in the :class:`StepTracker` and clears
        the :class:`RichLog` so only the new step's output is visible.

        Args:
            stage_idx: Zero-based index of the stage.
            step_idx: Zero-based index of the step within the stage.
        """
        logger.debug("Starting step: stage_idx=%d, step_idx=%d", stage_idx, step_idx)
        flat_idx = self._flat_index.get((stage_idx, step_idx))
        if flat_idx is None:
            return
        self._log_lines.clear()
        if not self.is_mounted:
            return
        tracker = self.query_one("#step-tracker", StepTracker)
        tracker.update_step(flat_idx, StepStatus.ACTIVE)
        log_view = self.query_one("#log-view", RichLog)
        log_view.clear()

    def append_output(self, line: str) -> None:
        """Append a line of output to the log viewer.

        Args:
            line: The text line to write to the :class:`RichLog`.
        """
        self._log_lines.append(line)
        if not self.is_mounted:
            return
        log_view = self.query_one("#log-view", RichLog)
        log_view.write(line)

    def complete_step(
        self, stage_idx: int, step_idx: int, success: bool
    ) -> None:
        """Update the step icon and trigger a notification.

        Sets the step status to ``COMPLETE`` (with a check-mark icon) on
        success, or ``FAILED`` (with an x-mark icon) on failure.

        Args:
            stage_idx: Zero-based index of the stage.
            step_idx: Zero-based index of the step within the stage.
            success: Whether the step completed successfully.
        """
        logger.debug(
            "Completing step: stage_idx=%d, step_idx=%d, success=%s",
            stage_idx, step_idx, success,
        )
        flat_idx = self._flat_index.get((stage_idx, step_idx))
        if flat_idx is None:
            return
        if success:
            self._completed.append((stage_idx, step_idx))
        else:
            self._failed = (stage_idx, step_idx)
        if not self.is_mounted:
            return
        tracker = self.query_one("#step-tracker", StepTracker)
        if success:
            tracker.update_step(flat_idx, StepStatus.COMPLETE)
            notify("Zing", f"Step completed: {self._tracker_steps[flat_idx].label}")
        else:
            tracker.update_step(flat_idx, StepStatus.FAILED)
            notify("Zing", f"Step failed: {self._tracker_steps[flat_idx].label}")

    def finish(self) -> None:
        """Dismiss the screen with a :class:`BuildResult`.

        Called by the command module when all steps are complete (or a
        step has failed).
        """
        logger.debug(
            "Build finished: %d completed, failed=%s",
            len(self._completed), self._failed is not None,
        )
        result = BuildResult(
            completed_steps=list(self._completed),
            failed_step=self._failed,
        )
        self.dismiss(result)
