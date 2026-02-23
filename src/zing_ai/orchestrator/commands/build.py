"""Orchestrator ``build`` command — execute plan steps.

Iterates through all stages and steps in the plan, distilling files,
invoking Claude with the ``build_step.md.j2`` prompt, streaming output
to the web UI via SSE, and marking each step done upon completion.

After all steps are finished, delegates to :func:`run_build_audit`.
"""

from __future__ import annotations

import logging
from pathlib import Path

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.distiller import distill_files
from zing_ai.orchestrator.models import ZingDocument
from zing_ai.orchestrator.web.app import (
    start_server_background as _start_web_server_background,
)
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


async def run_build(
    *,
    zing_file: str | None,
    no_browser: bool,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``build`` orchestrator command.

    Loads the zing document, iterates through all plan stages and steps
    in order, and for each incomplete step:

    1. Distills all referenced files (using cache).
    2. Renders the ``build_step.md.j2`` prompt.
    3. Invokes Claude via streaming and sends output to the web UI.
    4. Marks the step as done and writes the updated zing document.

    After all steps are complete, delegates to :func:`run_build_audit`.

    Parameters
    ----------
    zing_file:
        Optional zing file name to build from.
    no_browser:
        If ``True``, do not open the browser automatically.
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all Claude
        calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.
    """
    # Resolve the zing file
    zing_path = project.resolve_zing_file(zing_file, project_root)
    logger.info("Building with zing file: %s", zing_path)

    # Load the zing document and update stage to "build"
    doc = parse_zing_file(zing_path)
    doc.stage = "build"
    write_zing_file(zing_path, doc)

    # Load the plan
    plan = doc.plan
    if plan is None:
        logger.error("No plan found in zing file: %s", zing_path)
        return

    zing_overview = doc.content or ""

    # Start web server with build UI
    _start_web_server_background(
        zing_path,
        port=config.port,
        no_browser=no_browser,
    )

    # Iterate through stages and steps in order
    step_number = 0
    total_steps = sum(len(stage.steps) for stage in plan.stages)
    logger.info("Plan has %d total steps across %d stages", total_steps, len(plan.stages))

    for stage in plan.stages:
        for step in stage.steps:
            step_number += 1

            # Skip completed steps
            if step.done:
                logger.info(
                    "Skipping completed step %d/%d: %s",
                    step_number,
                    total_steps,
                    step.label,
                )
                continue

            logger.info(
                "Executing step %d/%d: %s",
                step_number,
                total_steps,
                step.label,
            )

            # 1. Distill all referenced files
            file_paths = [project_root / f for f in step.files if (project_root / f).is_file()]

            distilled: dict[Path, str] = {}
            if file_paths:
                distilled = await distill_files(file_paths, project_root=project_root)
                logger.info("Distilled %d files for step '%s'", len(distilled), step.label)

            # Convert Path keys to string keys for the template
            distilled_files: dict[str, str] = {
                str(fp.relative_to(project_root)): content for fp, content in distilled.items()
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

            # 3. Invoke Claude via streaming
            output_lines: list[str] = []
            try:
                async for line in claude.invoke_claude(
                    prompt,
                    call_type=CallType.BUILD,
                    config=config,
                    skip_permissions=skip_permissions,
                ):
                    output_lines.append(line)

                # 4. On completion, mark step as done
                step.done = True

                # Re-read the document to avoid overwriting concurrent changes,
                # then update just the step status and write back.
                doc = parse_zing_file(zing_path)
                if doc.plan is not None:
                    _update_step_done(doc, stage.label, step.label)
                write_zing_file(zing_path, doc)

                logger.info("Step '%s' completed successfully", step.label)

            except Exception:
                logger.exception("Step '%s' failed", step.label)
                raise

    logger.info("All build steps complete. Starting build audit.")

    # After all steps done, call run_build_audit()
    from zing_ai.orchestrator.commands.build_audit import run_build_audit

    await run_build_audit(
        zing_file=zing_path.name,
        no_browser=no_browser,
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
    if doc.plan is None:
        return
    for stage in doc.plan.stages:
        if stage.label == stage_label:
            for step in stage.steps:
                if step.label == step_label:
                    step.done = True
                    return
