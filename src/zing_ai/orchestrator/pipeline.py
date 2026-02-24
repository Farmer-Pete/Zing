"""Pipeline controller — dispatches to the correct orchestrator stage.

Provides a single entry point, :func:`run_pipeline`, that routes execution to
the appropriate command function based on ``start_stage``.  Each command
already wires its own progression to the next stage (e.g. ``run_plan()``
finishes then calls ``run_plan_audit()``), so the pipeline controller only
needs to dispatch the *starting* point.
"""

from __future__ import annotations

import logging
from pathlib import Path

from zing_ai.orchestrator.config import ZingConfig

logger = logging.getLogger(__name__)

#: Valid stage names in pipeline order.
STAGES = ("new", "plan", "plan-audit", "plan-review", "build", "build-audit")


async def run_pipeline(
    start_stage: str,
    zing_file: str | None,
    *,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Dispatch to the correct command function based on *start_stage*.

    Each command function handles its own forward progression (e.g.
    ``run_plan`` calls ``run_plan_audit`` upon completion), so the
    pipeline controller only needs to enter the flow at the right
    starting point.

    Parameters
    ----------
    start_stage:
        One of ``"new"``, ``"plan"``, ``"plan-audit"``, ``"plan-review"``,
        ``"build"``, or ``"build-audit"``.
    zing_file:
        Optional zing file name (inside ``.zing/``).  Required for all
        stages except ``"new"``.
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all
        Claude calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.

    Raises
    ------
    ValueError
        If *start_stage* is not a recognised stage name.
    """
    if start_stage not in STAGES:
        raise ValueError(
            f"Invalid start_stage {start_stage!r}. "
            f"Must be one of: {', '.join(STAGES)}"
        )

    logger.info("Pipeline starting at stage: %s", start_stage)

    # Keyword arguments shared by every command.
    common_kwargs = dict(
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )

    if start_stage == "new":
        from zing_ai.orchestrator.commands.new import run_new

        await run_new(zing_file=zing_file, **common_kwargs)

    elif start_stage == "plan":
        from zing_ai.orchestrator.commands.plan import run_plan

        await run_plan(zing_file=zing_file, **common_kwargs)

    elif start_stage == "plan-audit":
        from zing_ai.orchestrator.commands.plan_audit import run_plan_audit

        # run_plan_audit takes zing_file as a positional str parameter.
        await run_plan_audit(zing_file or "", **common_kwargs)

    elif start_stage == "plan-review":
        from zing_ai.orchestrator.commands.plan_review import run_plan_review

        await run_plan_review(zing_file=zing_file, **common_kwargs)

    elif start_stage == "build":
        from zing_ai.orchestrator.commands.build import run_build

        await run_build(zing_file=zing_file, **common_kwargs)

    elif start_stage == "build-audit":
        from zing_ai.orchestrator.commands.build_audit import run_build_audit

        await run_build_audit(zing_file=zing_file, **common_kwargs)
