"""Tests for the Rich-based inline progress display functions."""

from __future__ import annotations

import io
from unittest.mock import MagicMock, patch

from rich.console import Console

from zing_ai.orchestrator.models import Stage, Step
from zing_ai.orchestrator.ui.progress import (
    run_parallel_investigations,
    run_with_progress,
)
from zing_ai.orchestrator.ui.types import (
    BuildProgress,
    InvestigationEntry,
    InvestigationResult,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_stages() -> list[Stage]:
    """Create two stages with a total of three steps for testing."""
    return [
        Stage(
            label="Setup",
            steps=[
                Step(label="Install deps", instructions="npm install", files=["package.json"]),
                Step(label="Generate config", instructions="gen config", files=["config.json"]),
            ],
        ),
        Stage(
            label="Build",
            steps=[
                Step(label="Compile", instructions="tsc", files=["src/"]),
            ],
        ),
    ]


def _fake_console() -> Console:
    """Return a Console that writes to a StringIO buffer."""
    return Console(file=io.StringIO(), force_terminal=True)


# ---------------------------------------------------------------------------
# run_with_progress
# ---------------------------------------------------------------------------


class TestRunWithProgress:
    """Tests for :func:`run_with_progress`."""

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_all_steps_succeed(self, mock_console: Console) -> None:
        """All steps complete successfully and are recorded in order."""
        stages = _make_stages()
        call_order: list[tuple[int, int]] = []

        def execute_step(stage_idx: int, step_idx: int) -> str:
            call_order.append((stage_idx, step_idx))
            return f"output-{stage_idx}-{step_idx}"

        result = run_with_progress("Build", stages, execute_step)

        assert isinstance(result, BuildProgress)
        assert result.completed_steps == [(0, 0), (0, 1), (1, 0)]
        assert result.failed_step is None
        assert call_order == [(0, 0), (0, 1), (1, 0)]

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_step_failure_stops_execution(self, mock_console: Console) -> None:
        """When execute_step raises on step (1, 0), earlier steps are
        completed and the failed step is recorded."""
        stages = _make_stages()
        call_order: list[tuple[int, int]] = []

        def execute_step(stage_idx: int, step_idx: int) -> str:
            call_order.append((stage_idx, step_idx))
            if (stage_idx, step_idx) == (1, 0):
                raise RuntimeError("compile failed")
            return "ok"

        result = run_with_progress("Build", stages, execute_step)

        assert result.failed_step == (1, 0)
        assert result.completed_steps == [(0, 0), (0, 1)]
        # Step (1, 0) was called but no further steps after it.
        assert call_order == [(0, 0), (0, 1), (1, 0)]

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_keyboard_interrupt_records_failed_step(
        self, mock_console: Console
    ) -> None:
        """KeyboardInterrupt during execute_step records the current step as
        failed and returns partial results."""
        stages = _make_stages()

        def execute_step(stage_idx: int, step_idx: int) -> str:
            if (stage_idx, step_idx) == (0, 1):
                raise KeyboardInterrupt
            return "ok"

        result = run_with_progress("Build", stages, execute_step)

        assert result.failed_step == (0, 1)
        assert result.completed_steps == [(0, 0)]

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_empty_stages(self, mock_console: Console) -> None:
        """No stages means no steps to run; returns empty progress."""
        result = run_with_progress("Empty", [], lambda si, sti: "")

        assert result.completed_steps == []
        assert result.failed_step is None


# ---------------------------------------------------------------------------
# run_parallel_investigations
# ---------------------------------------------------------------------------


class TestRunParallelInvestigations:
    """Tests for :func:`run_parallel_investigations`."""

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_all_entries_succeed(self, mock_console: Console) -> None:
        """All investigation entries run and produce correct outputs."""
        entries: list[InvestigationEntry] = [
            InvestigationEntry(id="inv-1", label="Investigate A"),
            InvestigationEntry(id="inv-2", label="Investigate B"),
            InvestigationEntry(id="inv-3", label="Investigate C"),
        ]

        def run_fn(entry_id: str) -> str:
            return f"result-for-{entry_id}"

        result = run_parallel_investigations(entries, run_fn)

        assert isinstance(result, InvestigationResult)
        assert set(result.outputs.keys()) == {"inv-1", "inv-2", "inv-3"}
        assert result.outputs["inv-1"] == "result-for-inv-1"
        assert result.outputs["inv-2"] == "result-for-inv-2"
        assert result.outputs["inv-3"] == "result-for-inv-3"
        assert result.statuses["inv-1"] == "success"
        assert result.statuses["inv-2"] == "success"
        assert result.statuses["inv-3"] == "success"

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_worker_failure_records_error(self, mock_console: Console) -> None:
        """When a worker raises, that entry is marked failed with the error
        message, but other entries still succeed."""
        entries: list[InvestigationEntry] = [
            InvestigationEntry(id="good", label="Good"),
            InvestigationEntry(id="bad", label="Bad"),
        ]

        def run_fn(entry_id: str) -> str:
            if entry_id == "bad":
                raise RuntimeError("investigation failed")
            return f"result-{entry_id}"

        result = run_parallel_investigations(entries, run_fn)

        assert result.statuses["good"] == "success"
        assert result.outputs["good"] == "result-good"
        assert result.statuses["bad"] == "failed"
        assert "investigation failed" in result.outputs["bad"]

    @patch("zing_ai.orchestrator.ui.progress.console", new_callable=_fake_console)
    def test_empty_entries(self, mock_console: Console) -> None:
        """No entries means an empty result."""
        result = run_parallel_investigations([], lambda eid: "")

        assert result.outputs == {}
        assert result.statuses == {}
