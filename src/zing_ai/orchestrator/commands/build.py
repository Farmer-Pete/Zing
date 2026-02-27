"""Orchestrator ``build`` command -- execute plan steps.

Iterates through all stages and steps in the plan, distilling files,
invoking Claude with the ``build_step.md.j2`` prompt, displaying progress
via Rich, and marking each step done upon completion.

After all steps are finished, delegates to :func:`run_build_audit`.
"""

from __future__ import annotations

import logging
from pathlib import Path

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.config import CallType, ZingConfig, resolve_aid_path
from zing_ai.orchestrator.distiller import distill_files
from zing_ai.orchestrator.models import ZingDocument
from zing_ai.orchestrator.ui.progress import run_with_progress
from zing_ai.orchestrator.xml_parser import parse_zing_file, write_zing_file
from zing_ai.prompts import render_prompt

logger = logging.getLogger(__name__)

# The MCP mandate included in every build-step prompt.
MCP_MANDATE = (
    "Use Serena for code exploration, aid for analysis, CodeGraphContext for "
    "architecture. Prefer Serena's search_for_pattern over Grep, "
    "find_referencing_symbols over Grep for references, "
    "get_symbols_overview/find_symbol over Read for code files, "
    "replace_symbol_body/insert_before_symbol/insert_after_symbol over Edit "
    "for symbol-level edits."
)


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def run_build(
    *,
    zing_file: str | None,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``build`` orchestrator command.

    Loads the zing document, iterates through all plan stages and steps
    in order, and for each incomplete step:

    1. Distills all referenced files (using cache).
    2. Renders the ``build_step.md.j2`` prompt.
    3. Invokes Claude via streaming.
    4. Marks the step as done and writes the updated zing document.

    A Rich Live spinner displays progress.  After all steps are
    complete, delegates to :func:`run_build_audit`.

    Parameters
    ----------
    zing_file:
        Optional zing file name to build from.
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all Claude
        calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.
    """
    # Resolve the aid binary path (fail fast if missing)
    aid_path = resolve_aid_path(config)

    # Resolve the zing file
    zing_path = project.resolve_zing_file(zing_file, project_root)
    logger.info("Building with zing file: %s", zing_path)

    # Load the zing document and update stage to "build"
    doc = parse_zing_file(zing_path)
    logger.debug("Loaded document at stage '%s'", doc.stage)
    doc.stage = "build"
    write_zing_file(zing_path, doc)

    # Load the plan
    plan = doc.plan
    if plan is None:
        logger.error("No plan found in zing file: %s", zing_path)
        return

    zing_overview = doc.content or ""

    total_steps = sum(len(stage.steps) for stage in plan.stages)
    logger.info("Plan has %d total steps across %d stages", total_steps, len(plan.stages))

    def _execute_step(stage_idx: int, step_idx: int) -> str:
        """Execute a single build step, returning its output."""
        stage = plan.stages[stage_idx]
        step = stage.steps[step_idx]

        # Skip completed steps
        if step.done:
            logger.info("Skipping completed step: %s", step.label)
            return ""

        logger.info("Executing step: %s", step.label)

        # 1. Distill all referenced files
        file_paths = [
            project_root / f
            for f in step.files
            if (project_root / f).is_file()
        ]
        logger.debug(
            "Step '%s': %d referenced files, %d exist on disk",
            step.label, len(step.files), len(file_paths),
        )

        distilled: dict[Path, str] = {}
        if file_paths:
            distilled = distill_files(file_paths, project_root=project_root, aid_path=aid_path)
            logger.info(
                "Distilled %d files for step '%s'",
                len(distilled),
                step.label,
            )

        # Convert Path keys to string keys for the template
        distilled_files: dict[str, str] = {
            str(fp.relative_to(project_root)): content
            for fp, content in distilled.items()
        }

        # 2. Render the build_step prompt
        prompt = render_prompt(
            "build_step.md.j2",
            zing_overview=zing_overview,
            step_label=step.label,
            step_instructions=step.instructions,
            distilled_files=distilled_files,
            mcp_mandate=MCP_MANDATE,
        )

        logger.debug("Prompt size: %d chars", len(prompt))

        # 3. Invoke Claude via streaming
        logger.debug("Invoking Claude for step '%s'", step.label)
        output_lines: list[str] = []
        with claude.invoke_claude(
            prompt,
            call_type=CallType.BUILD,
            config=config,
            skip_permissions=skip_permissions,
        ) as lines:
            for line in lines:
                output_lines.append(line)

        # 4. On completion, mark step as done
        step.done = True

        # Re-read the document to avoid overwriting concurrent
        # changes, then update just the step status and write back.
        logger.debug("Re-reading zing file to update step status")
        fresh_doc = parse_zing_file(zing_path)
        if fresh_doc.plan is not None:
            _update_step_done(fresh_doc, stage.label, step.label)
        write_zing_file(zing_path, fresh_doc)

        logger.info("Step '%s' completed successfully", step.label)
        return "".join(output_lines)

    # Run all steps with Rich progress display.
    run_with_progress("Building", plan.stages, _execute_step)

    logger.info("Build complete. Starting build audit.")

    # After all steps done, call run_build_audit()
    from zing_ai.orchestrator.commands.build_audit import run_build_audit

    run_build_audit(
        zing_file=zing_path.name,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


def _update_step_done(doc: ZingDocument, stage_label: str, step_label: str) -> None:
    """Find a step by stage and step labels and mark it as done.

    Parameters
    ----------
    doc:
        The zing document to update.
    stage_label:
        The label of the stage containing the step.
    step_label:
        The label of the step to mark as done.
    """
    logger.debug("Marking step done: stage=%s, step=%s", stage_label, step_label)
    if doc.plan is None:
        return
    for stage in doc.plan.stages:
        if stage.label == stage_label:
            for step in stage.steps:
                if step.label == step_label:
                    step.done = True
                    return
    logger.debug("Step not found for update: stage=%s, step=%s", stage_label, step_label)
