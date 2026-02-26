"""Orchestrator ``plan`` command -- generate a development plan.

Implements the full planning pipeline:

1. **Identification** -- ask Claude to identify investigation areas.
2. **Distillation** -- distill referenced files with the ``aid`` CLI.
3. **Investigation** -- run parallel Claude calls per area via a TUI
   :class:`~zing_ai.orchestrator.tui.screens.progress.ProgressScreen`,
   each producing :class:`~zing_ai.orchestrator.models.Interaction`
   choice sets.
4. **Flesh out** -- collect recommended choices, produce a
   :class:`~zing_ai.orchestrator.models.Plan` with stages/steps.
5. **Assembly** -- write the zing XML file with plan, interactions,
   and the session ID for later resumption.

A **re-plan** path (when ``replan_changes`` is provided) resumes the
saved session and merges updated steps/interactions.
"""

from __future__ import annotations

import contextlib
import logging
import re
import threading
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

import jinja2

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.claude import print_line
from zing_ai.orchestrator.config import CallType, ZingConfig, resolve_aid_path
from zing_ai.orchestrator.distiller import distill_files
from zing_ai.orchestrator.models import Interaction, Plan, ZingDocument
from zing_ai.orchestrator.tui.app import ZingApp
from zing_ai.orchestrator.tui.results import ProgressResult
from zing_ai.orchestrator.tui.screens.progress import ProgressScreen
from zing_ai.orchestrator.xml_parser import (
    ValidationError,
    parse_interactions_response,
    parse_steps_response,
    parse_zing_file,
    write_zing_file,
)
from zing_ai.prompts import render_prompt

logger = logging.getLogger(__name__)

# Simple Jinja2 retry template used with invoke_claude_validated.
_RETRY_TEMPLATE = jinja2.Template(
    "Your previous response was invalid: {{ error }}. "
    "Please produce a corrected response following the original instructions."
)

# ---------------------------------------------------------------------------
# Identification phase -- parse structured text (not XML)
# ---------------------------------------------------------------------------


@dataclass
class InvestigationArea:
    """A single area identified for investigation."""

    name: str
    description: str
    files: list[str]


def _parse_identification_response(text: str) -> list[InvestigationArea]:
    """Parse the identification phase response into a list of areas.

    The expected format is::

        ### Area: {area name}
        {Description lines}
        Files:
        - {file or pattern}
        - {file or pattern}

    Parameters
    ----------
    text:
        The raw text response from Claude.

    Returns
    -------
    list[InvestigationArea]
        Parsed investigation areas (3-5 expected).

    Raises
    ------
    ValidationError
        If the response cannot be parsed or contains fewer than 1 area.
    """
    areas: list[InvestigationArea] = []

    # Split on "### Area:" headers
    pattern = re.compile(r"###\s+Area:\s*(.+)", re.IGNORECASE)
    sections = pattern.split(text)

    # sections[0] is text before the first header (discard).
    # After that, alternating: name, body, name, body, ...
    if len(sections) < 3:
        raise ValidationError(
            "Could not find any '### Area:' headers in the identification response. "
            f"Response starts with: {text[:200]!r}"
        )

    for i in range(1, len(sections), 2):
        area_name = sections[i].strip()
        body = sections[i + 1] if i + 1 < len(sections) else ""

        description_lines: list[str] = []
        files: list[str] = []
        in_files = False

        for line in body.splitlines():
            stripped = line.strip()

            # Detect the "Files:" marker
            if re.match(r"^files\s*:", stripped, re.IGNORECASE):
                in_files = True
                continue

            if in_files:
                # Parse file list items (lines starting with "- " or "* ")
                file_match = re.match(r"^[-*]\s+(.+)$", stripped)
                if file_match:
                    # Strip backticks if present
                    file_path = file_match.group(1).strip().strip("`")
                    if file_path:
                        files.append(file_path)
                elif stripped:
                    # Non-empty line that doesn't look like a list item
                    # could be a continuation or end of file list
                    # If we already have files, this is a new section
                    if files:
                        break
            else:
                if stripped:
                    description_lines.append(stripped)

        description = "\n".join(description_lines)
        areas.append(InvestigationArea(
            name=area_name,
            description=description,
            files=files,
        ))

    if not areas:
        raise ValidationError(
            "No investigation areas found in the identification response."
        )

    return areas


# ---------------------------------------------------------------------------
# Flesh-out helper (needs session ID back)
# ---------------------------------------------------------------------------


def _invoke_flesh_out_with_session(
    prompt: str,
    *,
    call_type: CallType,
    config: ZingConfig,
    skip_permissions: bool,
    on_output: Callable[[str], None] | None = None,
    max_retries: int = 3,
    zing_dir: Path | None = None,
) -> tuple[Plan, str]:
    """Invoke Claude for the flesh-out phase and return ``(plan, session_id)``.

    Unlike :func:`invoke_claude_validated`, this helper also captures and
    returns the session ID so it can be persisted in the zing document for
    later session resumption.

    Parameters
    ----------
    prompt:
        The rendered flesh-out prompt.
    call_type:
        The Claude call type.
    config:
        Zing configuration.
    skip_permissions:
        Whether to skip permission checks.
    on_output:
        Optional callback for streaming output lines.
    max_retries:
        Maximum validation retry attempts.

    Returns
    -------
    tuple[Plan, str]
        ``(parsed_plan, session_id)``

    Raises
    ------
    ValidationError
        If validation fails after all retries.
    """
    output, session_id = claude.invoke_claude_full(
        prompt,
        on_output=on_output,
        zing_dir=zing_dir,
        call_type=call_type,
        config=config,
        skip_permissions=skip_permissions,
    )

    for attempt in range(1, max_retries + 1):
        try:
            plan = parse_steps_response(output)
            return plan, session_id
        except ValidationError as exc:
            logger.warning(
                "Flesh-out validation failed (attempt %d/%d): %s",
                attempt,
                max_retries,
                exc,
            )
            if attempt >= max_retries:
                raise

            retry_prompt = _RETRY_TEMPLATE.render(error=str(exc))
            output, session_id = claude.invoke_claude_full(
                retry_prompt,
                on_output=on_output,
                zing_dir=zing_dir,
                call_type=call_type,
                config=config,
                skip_permissions=skip_permissions,
                resume_session=session_id,
            )

    # Should not be reached
    return parse_steps_response(output), session_id  # pragma: no cover


# ---------------------------------------------------------------------------
# Re-plan helper
# ---------------------------------------------------------------------------


def _invoke_replan_with_session(
    prompt: str,
    *,
    call_type: CallType,
    config: ZingConfig,
    skip_permissions: bool,
    resume_session: str,
    on_output: Callable[[str], None] | None = None,
    max_retries: int = 3,
    zing_dir: Path | None = None,
) -> tuple[Plan, Interaction | None, str]:
    """Invoke Claude for the re-plan phase (session resumption).

    The response may contain updated ``<zing:steps>`` and optionally new
    ``<zing:interactions>``.

    Returns
    -------
    tuple[Plan, Interaction | None, str]
        ``(updated_plan, new_interactions_or_none, session_id)``
    """
    output, session_id = claude.invoke_claude_full(
        prompt,
        on_output=on_output,
        zing_dir=zing_dir,
        call_type=call_type,
        config=config,
        skip_permissions=skip_permissions,
        resume_session=resume_session,
    )

    for attempt in range(1, max_retries + 1):
        try:
            plan = parse_steps_response(output)

            # Optionally parse new interactions (not required)
            new_interactions: Interaction | None = None
            with contextlib.suppress(ValidationError):
                new_interactions = parse_interactions_response(output)

            return plan, new_interactions, session_id
        except ValidationError as exc:
            logger.warning(
                "Re-plan validation failed (attempt %d/%d): %s",
                attempt,
                max_retries,
                exc,
            )
            if attempt >= max_retries:
                raise

            retry_prompt = _RETRY_TEMPLATE.render(error=str(exc))
            output, session_id = claude.invoke_claude_full(
                retry_prompt,
                on_output=on_output,
                zing_dir=zing_dir,
                call_type=call_type,
                config=config,
                skip_permissions=skip_permissions,
                resume_session=session_id,
            )

    # Should not be reached
    return parse_steps_response(output), None, session_id  # pragma: no cover


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def run_plan(
    *,
    zing_file: str | None,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
    replan_changes: list[dict] | None = None,
) -> None:
    """Run the ``plan`` orchestrator command.

    Parameters
    ----------
    zing_file:
        Zing file name (inside ``.zing/``).
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all Claude
        calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.
    replan_changes:
        ``None`` on first run. On re-plan, a list of dicts describing
        changed choices (each with ``choice_set_message``,
        ``original_recommended``, ``user_selected``, and optionally
        ``deleted``).
    """
    # Resolve the zing file
    zing_path = project.resolve_zing_file(zing_file, project_root)
    logger.info("Planning with zing file: %s", zing_path)

    if replan_changes is not None:
        _run_replan(
            zing_path=zing_path,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
            replan_changes=replan_changes,
        )
    else:
        _run_first_plan(
            zing_path=zing_path,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
        )

    # Flow into plan audit
    from zing_ai.orchestrator.commands.plan_audit import run_plan_audit

    run_plan_audit(
        zing_file=zing_path.name,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


# ---------------------------------------------------------------------------
# Investigation TUI helper
# ---------------------------------------------------------------------------


def _run_investigation_tui(
    *,
    area_prompts: list[tuple[InvestigationArea, str]],
    config: ZingConfig,
    skip_permissions: bool,
) -> tuple[ProgressResult, list[Interaction]]:
    """Run parallel investigation Claude calls inside a ProgressScreen.

    Each investigation area gets its own subprocess entry in the TUI.
    Output is streamed line-by-line to the screen.  When all areas
    complete, the screen is dismissed and the parsed results are
    returned.

    Parameters
    ----------
    area_prompts:
        A list of ``(InvestigationArea, rendered_prompt)`` tuples.
    config:
        Zing configuration.
    skip_permissions:
        Whether to skip permission checks.

    Returns
    -------
    tuple[ProgressResult, list[Interaction]]
        The TUI progress result and a list of parsed interactions
        (one per area, in the same order as *area_prompts*).
    """
    screen = ProgressScreen()

    # Shared state guarded by a lock so workers can safely record their
    # results without races.
    results_lock = threading.Lock()
    investigation_results: dict[str, Interaction] = {}

    def _investigate_worker(
        area: InvestigationArea,
        prompt: str,
        area_id: str,
    ) -> None:
        """Worker function executed in a thread by each investigation."""
        screen.update_status(area_id, "running")

        try:
            interaction = claude.invoke_claude_validated(
                prompt,
                validator=parse_interactions_response,
                retry_prompt_template=_RETRY_TEMPLATE,
                on_output=lambda line, _id=area_id: screen.append_output(_id, line),
                call_type=CallType.INVESTIGATE,
                config=config,
                skip_permissions=skip_permissions,
            )

            screen.append_output(area_id, f"Produced {len(interaction.choice_sets)} choice set(s)")
            logger.info(
                "Area '%s' produced %d choice sets",
                area.name,
                len(interaction.choice_sets),
            )

            with results_lock:
                investigation_results[area_id] = interaction

            screen.update_status(area_id, "success")
        except Exception as exc:
            logger.error("Investigation failed for area '%s': %s", area.name, exc)
            screen.append_output(area_id, f"ERROR: {exc}")
            screen.update_status(area_id, "failed")

            # Store an empty interaction so the pipeline can continue
            with results_lock:
                investigation_results[area_id] = Interaction(choice_sets=[])

    # Register subprocess entries and build area ID mapping
    area_ids: list[str] = []
    for i, (area, _prompt) in enumerate(area_prompts):
        area_id = f"investigate-{i}"
        area_ids.append(area_id)
        screen.add_subprocess(area_id, f"Investigate: {area.name}")

    # Launch all workers as daemon threads that interact with the screen.
    # We track them so we can call mark_all_complete when they're done.
    threads: list[threading.Thread] = []
    for (area, prompt), area_id in zip(area_prompts, area_ids, strict=True):
        t = threading.Thread(
            target=_investigate_worker,
            args=(area, prompt, area_id),
            daemon=True,
        )
        threads.append(t)

    # A coordinator thread that waits for all workers and then
    # dismisses the screen.
    def _coordinator() -> None:
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        screen.mark_all_complete()

    coordinator = threading.Thread(target=_coordinator, daemon=True)
    coordinator.start()

    # Block until the screen is dismissed.
    progress_result: ProgressResult = ZingApp.run_with_screen(screen)

    # Gather results in original area order.
    ordered_results: list[Interaction] = []
    for area_id in area_ids:
        ordered_results.append(
            investigation_results.get(area_id, Interaction(choice_sets=[]))
        )

    return progress_result, ordered_results


# ---------------------------------------------------------------------------
# First-run flow
# ---------------------------------------------------------------------------


def _run_first_plan(
    *,
    zing_path: Path,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Execute the full first-run planning pipeline.

    Phases: identification -> distillation -> investigation -> flesh out -> assembly.
    """
    # Resolve the aid binary path (fail fast if missing)
    aid_path = resolve_aid_path(config)

    # Read the zing document for the project specification content
    doc = parse_zing_file(zing_path)
    zing_content = doc.content or ""

    # --- Phase 1: Identification ---
    logger.info("Phase 1: Identification")
    identify_prompt = render_prompt("plan_identify.md.j2", zing_content=zing_content)

    zing_dir = project.ensure_zing_dir(project_root)

    identify_output, _identify_session = claude.invoke_claude_full(
        identify_prompt,
        on_output=print_line,
        zing_dir=zing_dir,
        call_type=CallType.INVESTIGATE,
        config=config,
        skip_permissions=skip_permissions,
    )

    areas = _parse_identification_response(identify_output)
    logger.info("Identified %d investigation areas", len(areas))
    for area in areas:
        logger.debug("  Area: %s (%d files)", area.name, len(area.files))

    # --- Phase 2: Distillation ---
    logger.info("Phase 2: Distillation")
    all_files: set[str] = set()
    for area in areas:
        all_files.update(area.files)

    # Resolve file paths relative to project root and filter to existing files
    file_paths: list[Path] = []
    for f in sorted(all_files):
        fp = project_root / f
        if fp.is_file():
            file_paths.append(fp)
        else:
            logger.debug("Skipping non-existent file: %s", fp)

    distilled: dict[Path, str] = {}
    if file_paths:
        distilled = distill_files(file_paths, project_root=project_root, aid_path=aid_path)
        logger.info("Distilled %d files", len(distilled))

    # --- Phase 3: Investigation (parallel, via TUI) ---
    logger.info("Phase 3: Investigation (parallel)")

    # Build the investigation prompts ahead of time so the TUI workers
    # only need to invoke Claude and stream output.
    area_prompts: list[tuple[InvestigationArea, str]] = []
    for area in areas:
        area_distilled: dict[str, str] = {}
        for f in area.files:
            fp = project_root / f
            if fp in distilled:
                area_distilled[f] = distilled[fp]

        investigate_prompt = render_prompt(
            "plan_investigate.md.j2",
            zing_content=zing_content,
            area_name=area.name,
            area_description=area.description,
            files=area.files,
            distilled_code=area_distilled,
            mcp_mandate="Use the MCP tools available to you to investigate the codebase.",
        )
        area_prompts.append((area, investigate_prompt))

    # Run all investigations in a ProgressScreen via the TUI.
    # _run_investigation_tui returns the ProgressResult and a mapping
    # of area names to their parsed Interaction objects.
    progress_result, investigation_results = _run_investigation_tui(
        area_prompts=area_prompts,
        config=config,
        skip_permissions=skip_permissions,
    )

    # Merge all interactions into one
    all_choice_sets = []
    for interaction in investigation_results:
        all_choice_sets.extend(interaction.choice_sets)
    merged_interactions = Interaction(choice_sets=all_choice_sets)
    logger.info("Total choice sets from investigation: %d", len(all_choice_sets))

    # --- Phase 4: Flesh out ---
    logger.info("Phase 4: Flesh out")

    # Build recommended choices for the flesh-out template
    recommended_choices = []
    for cs in merged_interactions.choice_sets:
        selected = next((c for c in cs.choices if c.recommended), None)
        recommended_choices.append({
            "message": cs.message,
            "selected_label": selected.label if selected else "N/A",
            "explanation": cs.explanation,
        })

    flesh_out_prompt = render_prompt(
        "plan_flesh_out.md.j2",
        zing_content=zing_content,
        recommended_choices=recommended_choices,
    )

    plan, session_id = _invoke_flesh_out_with_session(
        flesh_out_prompt,
        call_type=CallType.PLAN,
        config=config,
        skip_permissions=skip_permissions,
        on_output=print_line,
        zing_dir=zing_dir,
    )
    logger.info(
        "Flesh out produced plan with %d stages, session_id=%s",
        len(plan.stages),
        session_id,
    )

    # --- Phase 5: Assembly ---
    logger.info("Phase 5: Assembly")

    result_doc = ZingDocument(
        stage="plan",
        content=zing_content,
        plan=plan,
        interactions=merged_interactions,
        audit=False,
        approved=False,
        plan_session=session_id or None,
    )

    write_zing_file(zing_path, result_doc)
    logger.info("Wrote zing file: %s", zing_path)


# ---------------------------------------------------------------------------
# Re-plan flow
# ---------------------------------------------------------------------------


def _run_replan(
    *,
    zing_path: Path,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
    replan_changes: list[dict],
) -> None:
    """Execute the re-plan flow with session resumption.

    Reads the saved ``plan-session`` ID from the zing file, renders the
    re-plan template with the changed choices, resumes the Claude session,
    and merges updated steps and any new interactions.
    """
    doc = parse_zing_file(zing_path)
    zing_content = doc.content or ""

    plan_session = doc.plan_session
    if not plan_session:
        logger.warning("No plan-session found in zing file; running re-plan without resume")

    zing_dir = project.ensure_zing_dir(project_root)

    # Render the re-plan prompt
    replan_prompt = render_prompt(
        "plan_replan.md.j2",
        zing_content=zing_content,
        changes=replan_changes,
    )

    # Invoke Claude with session resumption
    updated_plan, new_interactions, session_id = _invoke_replan_with_session(
        replan_prompt,
        call_type=CallType.PLAN,
        config=config,
        skip_permissions=skip_permissions,
        resume_session=plan_session or "",
        on_output=print_line,
        zing_dir=zing_dir,
    )

    logger.info(
        "Re-plan produced plan with %d stages",
        len(updated_plan.stages),
    )

    # Merge new interactions with existing ones
    existing_interactions = doc.interactions
    if new_interactions is not None and new_interactions.choice_sets:
        if existing_interactions is not None:
            merged_choice_sets = (
                existing_interactions.choice_sets + new_interactions.choice_sets
            )
            merged_interactions = Interaction(choice_sets=merged_choice_sets)
        else:
            merged_interactions = new_interactions
        logger.info(
            "Merged %d new choice sets into interactions",
            len(new_interactions.choice_sets),
        )
    else:
        merged_interactions = existing_interactions

    # Write updated zing file, preserving plan-session ID
    result_doc = ZingDocument(
        stage="plan",
        content=zing_content,
        plan=updated_plan,
        interactions=merged_interactions,
        audit=False,
        approved=False,
        plan_session=session_id or plan_session or None,
    )

    write_zing_file(zing_path, result_doc)
    logger.info("Wrote updated zing file: %s", zing_path)
