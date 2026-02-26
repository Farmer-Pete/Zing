"""Rich-based inline progress displays for build execution and parallel investigations."""

from __future__ import annotations

import threading
from collections.abc import Callable
from concurrent.futures import Future, ThreadPoolExecutor

from rich.live import Live
from rich.spinner import Spinner
from rich.table import Table

from . import console
from .types import BuildProgress, InvestigationEntry, InvestigationResult

# Re-import Stage here so callers can see the type in signatures.
from ..models import Stage


def _build_step_table(
    stages: list[Stage],
    statuses: dict[tuple[int, int], str],
) -> Table:
    """Build a Rich Table showing one row per step with a spinner/icon."""
    table = Table(show_header=False, show_edge=False, pad_edge=False, box=None)
    table.add_column("status", width=3)
    table.add_column("label")

    for stage_idx, stage in enumerate(stages):
        for step_idx, step in enumerate(stage.steps):
            key = (stage_idx, step_idx)
            status = statuses.get(key, "pending")
            if status == "pending":
                icon = "[dim]...[/dim]"
            elif status == "running":
                icon = Spinner("dots", style="cyan")
            elif status == "done":
                icon = "[green]OK[/green]"
            else:  # failed
                icon = "[red]XX[/red]"
            table.add_row(icon, step.label)

    return table


def run_with_progress(
    label: str,
    stages: list[Stage],
    execute_step: Callable[[int, int], str],
) -> BuildProgress:
    """Run build steps sequentially with a Rich Live spinner display.

    Parameters
    ----------
    label:
        A heading label for the progress display.
    stages:
        The list of build stages, each containing steps.
    execute_step:
        Callable that takes ``(stage_idx, step_idx)`` and returns the
        step's output as a string.  Called sequentially for each step.

    Returns
    -------
    BuildProgress
        Contains ``completed_steps`` and ``failed_step``.
    """
    completed_steps: list[tuple[int, int]] = []
    failed_step: tuple[int, int] | None = None
    statuses: dict[tuple[int, int], str] = {}

    # Initialise all steps as pending.
    for stage_idx, stage in enumerate(stages):
        for step_idx in range(len(stage.steps)):
            statuses[(stage_idx, step_idx)] = "pending"

    console.print(f"\n[bold]{label}[/bold]")

    try:
        with Live(
            _build_step_table(stages, statuses),
            console=console,
            refresh_per_second=8,
        ) as live:
            for stage_idx, stage in enumerate(stages):
                for step_idx, step in enumerate(stage.steps):
                    key = (stage_idx, step_idx)
                    statuses[key] = "running"
                    live.update(_build_step_table(stages, statuses))

                    try:
                        output = execute_step(stage_idx, step_idx)
                    except KeyboardInterrupt:
                        statuses[key] = "failed"
                        live.update(_build_step_table(stages, statuses))
                        failed_step = key
                        return BuildProgress(
                            completed_steps=completed_steps,
                            failed_step=failed_step,
                        )
                    except Exception:
                        statuses[key] = "failed"
                        live.update(_build_step_table(stages, statuses))
                        failed_step = key
                        return BuildProgress(
                            completed_steps=completed_steps,
                            failed_step=failed_step,
                        )

                    statuses[key] = "done"
                    live.update(_build_step_table(stages, statuses))
                    completed_steps.append(key)

                    # Print streaming output below the Live display.
                    if output.strip():
                        for line in output.splitlines():
                            console.print(f"[dim]{line}[/dim]")
    except KeyboardInterrupt:
        # KeyboardInterrupt raised outside of execute_step (e.g. during
        # Live rendering).  Record whatever step was in progress.
        for key, status in statuses.items():
            if status == "running":
                failed_step = key
                break
        return BuildProgress(
            completed_steps=completed_steps,
            failed_step=failed_step,
        )

    return BuildProgress(completed_steps=completed_steps, failed_step=failed_step)


def _build_investigation_table(
    entries: list[InvestigationEntry],
    statuses: dict[str, str],
) -> Table:
    """Build a Rich Table showing one row per investigation entry."""
    table = Table(show_header=False, show_edge=False, pad_edge=False, box=None)
    table.add_column("status", width=3)
    table.add_column("label")

    for entry in entries:
        eid = entry["id"]
        status = statuses.get(eid, "pending")
        if status == "pending":
            icon = "[dim]...[/dim]"
        elif status == "running":
            icon = Spinner("dots", style="cyan")
        elif status == "success":
            icon = "[green]OK[/green]"
        else:  # failed
            icon = "[red]XX[/red]"
        table.add_row(icon, entry["label"])

    return table


def run_parallel_investigations(
    entries: list[InvestigationEntry],
    run_fn: Callable[[str], str],
) -> InvestigationResult:
    """Run parallel Claude investigations with a Rich Live spinner display.

    Parameters
    ----------
    entries:
        List of investigation entries, each with ``id`` and ``label``.
    run_fn:
        Callable that takes an entry ``id`` and returns the
        investigation output as a string.  Called concurrently for each
        entry via a thread pool.

    Returns
    -------
    InvestigationResult
        Contains ``outputs`` and ``statuses`` dicts keyed by entry id.
    """
    outputs: dict[str, str] = {}
    statuses: dict[str, str] = {e["id"]: "pending" for e in entries}
    print_lock = threading.Lock()

    def _worker(entry: InvestigationEntry) -> tuple[str, str]:
        eid = entry["id"]
        statuses[eid] = "running"
        try:
            result = run_fn(eid)
            # Print interleaved output under the lock.
            with print_lock:
                for line in result.splitlines():
                    console.print(f"[dim][{eid}][/dim] {line}")
            return eid, result
        except Exception as exc:
            with print_lock:
                console.print(f"[dim][{eid}][/dim] [red]Error: {exc}[/red]")
            raise

    futures: dict[Future[tuple[str, str]], InvestigationEntry] = {}

    try:
        with Live(
            _build_investigation_table(entries, statuses),
            console=console,
            refresh_per_second=8,
        ) as live:
            with ThreadPoolExecutor(max_workers=len(entries) or 1) as executor:
                for entry in entries:
                    future = executor.submit(_worker, entry)
                    futures[future] = entry

                # Poll futures until all are done.
                remaining = set(futures.keys())
                while remaining:
                    live.update(_build_investigation_table(entries, statuses))
                    done_this_round: set[Future[tuple[str, str]]] = set()
                    for future in remaining:
                        if future.done():
                            done_this_round.add(future)
                            entry = futures[future]
                            eid = entry["id"]
                            try:
                                eid_result, output = future.result()
                                outputs[eid] = output
                                statuses[eid] = "success"
                            except Exception as exc:
                                outputs[eid] = str(exc)
                                statuses[eid] = "failed"
                    remaining -= done_this_round

                    if remaining:
                        # Brief sleep to avoid busy-waiting; import here
                        # to keep it local.
                        import time
                        time.sleep(0.05)

                live.update(_build_investigation_table(entries, statuses))

    except KeyboardInterrupt:
        # Cancel any pending futures.
        for future in futures:
            future.cancel()
        # Mark any still-running entries.
        for entry in entries:
            eid = entry["id"]
            if statuses[eid] == "running":
                statuses[eid] = "failed"
            if eid not in outputs:
                outputs[eid] = ""

    return InvestigationResult(outputs=outputs, statuses=statuses)
