"""Orchestrator ``plan-review`` command -- review and approve a plan.

Shows the plan's choices to the user in the TUI.  The user can approve
(no changes), modify choices (switch recommended option), or delete choice
sets.  Approval triggers the build; modifications trigger the re-plan ->
re-audit -> review loop.
"""

from __future__ import annotations

import logging
from pathlib import Path

from zing_ai.orchestrator import project
from zing_ai.orchestrator.config import ZingConfig
from zing_ai.orchestrator.models import ChoiceSet
from zing_ai.orchestrator.tui.app import ZingApp
from zing_ai.orchestrator.tui.results import ReviewResult
from zing_ai.orchestrator.tui.screens.plan_review import PlanReviewScreen
from zing_ai.orchestrator.xml_parser import parse_zing_file, write_zing_file

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def run_plan_review(
    *,
    zing_file: str | None,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``plan-review`` orchestrator command.

    Loads choices from the zing document and presents them to the user
    via the TUI.  The user can either approve the plan or modify choices.

    **Approval (no changes):**
        Sets ``approved=True`` on the document and calls ``run_build()``.

    **Modifications:**
        Extracts the change list from the review result and calls
        ``run_plan(replan_changes=changes)`` to re-enter the
        plan -> audit -> review loop.

    Parameters
    ----------
    zing_file:
        Optional zing file name to review.
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
    logger.info("Reviewing plan: %s", zing_path)

    # Load the document and extract choices
    doc = parse_zing_file(zing_path)
    choice_sets: list[ChoiceSet] = []
    if doc.interactions is not None:
        choice_sets = list(doc.interactions.choice_sets)

    if not choice_sets:
        logger.warning("No choices found in zing document; skipping review")
        # Nothing to review -- approve and proceed to build
        doc.approved = True
        write_zing_file(zing_path, doc)
        _call_build(
            zing_path=zing_path,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
        )
        return

    # Launch the TUI review screen and block until the user decides
    logger.info(
        "Launching review TUI (%d choice sets to review)...",
        len(choice_sets),
    )
    screen = PlanReviewScreen(choice_sets)
    result: ReviewResult | None = ZingApp.run_with_screen(screen)

    if result is not None and result.action == "approve":
        # --- Approval path: no changes ---
        logger.info("Plan approved (no changes)")
        doc.approved = True
        write_zing_file(zing_path, doc)

        _call_build(
            zing_path=zing_path,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
        )
    elif result is not None and result.action == "replan":
        # --- Modification path: extract changes and re-plan ---
        changes = result.changes
        logger.info("Plan modifications detected (%d changes)", len(changes))
        for change in changes:
            if change.get("new_selection") is None:
                logger.debug(
                    "  Deleted: %s", change.get("choice_id", "unknown")
                )
            else:
                logger.debug(
                    "  Changed: %s -> selection %s",
                    change.get("choice_id", "unknown"),
                    change.get("new_selection"),
                )

        _call_replan(
            zing_path=zing_path,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
            replan_changes=changes,
        )
    else:
        # User closed the TUI without making a decision
        logger.warning("Review cancelled -- no action taken")


# ---------------------------------------------------------------------------
# Flow-control helpers (separate for testability)
# ---------------------------------------------------------------------------


def _call_build(
    *,
    zing_path: Path,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Call ``run_build`` to execute the approved plan."""
    from zing_ai.orchestrator.commands.build import run_build

    run_build(
        zing_file=zing_path.name,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


def _call_replan(
    *,
    zing_path: Path,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
    replan_changes: list[dict],
) -> None:
    """Call ``run_plan`` with ``replan_changes`` to re-enter the plan loop."""
    from zing_ai.orchestrator.commands.plan import run_plan

    run_plan(
        zing_file=zing_path.name,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
        replan_changes=replan_changes,
    )
