"""StepTracker widget -- vertical list of steps connected by a line.

Each step shows a status icon and label, with the active step highlighted.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

from textual.app import ComposeResult
from textual.reactive import reactive
from textual.widget import Widget
from textual.widgets import Label, Static


class StepStatus(Enum):
    """Possible status values for a step."""

    PENDING = "pending"
    ACTIVE = "active"
    COMPLETE = "complete"
    FAILED = "failed"


_STATUS_ICONS: dict[StepStatus, str] = {
    StepStatus.PENDING: "\u25cb",   # ○
    StepStatus.ACTIVE: "\u25b6",    # ▶
    StepStatus.COMPLETE: "\u2713",  # ✓
    StepStatus.FAILED: "\u2717",    # ✗
}

_STATUS_CSS_CLASS: dict[StepStatus, str] = {
    StepStatus.PENDING: "step-tracker__item--pending",
    StepStatus.ACTIVE: "step-tracker__item--active",
    StepStatus.COMPLETE: "step-tracker__item--complete",
    StepStatus.FAILED: "step-tracker__item--failed",
}


@dataclass
class TrackerStep:
    """Data for a single step displayed in the tracker."""

    label: str
    status: StepStatus = StepStatus.PENDING


class _StepRow(Static):
    """One row in the step tracker showing connector + icon + label."""

    DEFAULT_CSS = """
    _StepRow {
        height: 1;
        width: 1fr;
    }
    """

    def __init__(self, step: TrackerStep, is_last: bool = False) -> None:
        self._step = step
        self._is_last = is_last
        super().__init__()

    def render(self) -> str:
        icon = _STATUS_ICONS.get(self._step.status, "\u25cb")
        connector = "  " if self._is_last else "\u2502 "
        return f" {icon} {self._step.label}"

    @property
    def step(self) -> TrackerStep:
        return self._step


class StepTracker(Widget):
    """Vertical step-by-step progress tracker.

    Renders steps with status icons (pending/active/complete/failed)
    and highlights the currently active step.
    """

    DEFAULT_CSS = """
    StepTracker {
        height: auto;
        width: 1fr;
        layout: vertical;
    }
    """

    def __init__(
        self,
        steps: list[TrackerStep] | None = None,
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._steps: list[TrackerStep] = list(steps or [])

    def compose(self) -> ComposeResult:
        for i, step in enumerate(self._steps):
            is_last = i == len(self._steps) - 1
            css_class = _STATUS_CSS_CLASS.get(step.status, "step-tracker__item--pending")
            row = _StepRow(step, is_last=is_last)
            row.add_class("step-tracker__item")
            row.add_class(css_class)
            yield row

    # ── Public API ────────────────────────────────────────────────────

    @property
    def steps(self) -> list[TrackerStep]:
        return list(self._steps)

    def update_step(self, index: int, status: StepStatus) -> None:
        """Update the status of a step and re-render."""
        if 0 <= index < len(self._steps):
            self._steps[index].status = status
            self._rebuild()

    def advance(self) -> None:
        """Mark the current active step complete and activate the next pending one."""
        for i, step in enumerate(self._steps):
            if step.status == StepStatus.ACTIVE:
                self._steps[i].status = StepStatus.COMPLETE
                # Activate the next pending step
                if i + 1 < len(self._steps):
                    self._steps[i + 1].status = StepStatus.ACTIVE
                self._rebuild()
                return

    def set_steps(self, steps: list[TrackerStep]) -> None:
        """Replace all steps and re-render."""
        self._steps = list(steps)
        self._rebuild()

    def _rebuild(self) -> None:
        """Remove existing children and recompose."""
        self.query("_StepRow").remove()
        for i, step in enumerate(self._steps):
            is_last = i == len(self._steps) - 1
            css_class = _STATUS_CSS_CLASS.get(step.status, "step-tracker__item--pending")
            row = _StepRow(step, is_last=is_last)
            row.add_class("step-tracker__item")
            row.add_class(css_class)
            self.mount(row)
