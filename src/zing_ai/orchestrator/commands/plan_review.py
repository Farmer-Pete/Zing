"""Orchestrator ``plan-review`` command -- review and approve a plan.

Shows the plan's choices to the user via Rich inline menus.  The user can
approve (no changes) or modify choices.  Approval triggers the build;
modifications trigger the re-plan -> re-audit -> review loop.
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Literal

from zing_ai.orchestrator import project
from zing_ai.orchestrator.config import ZingConfig
from zing_ai.orchestrator.models import ChoiceSet
from zing_ai.orchestrator.ui.menus import plan_review_menu
from zing_ai.orchestrator.ui.types import ReviewChange
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
    via Rich inline menus.  The user can either approve the plan or
    modify choices.

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

    # Present choices via Rich inline menu
    logger.info(
        "Launching review menu (%d choice sets to review)...",
        len(choice_sets),
    )
    action: Literal["approve", "replan"]
    changes: list[ReviewChange]
    action, changes = plan_review_menu(choice_sets)

    if action == "approve":
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
    elif action == "replan":
        # --- Modification path: extract changes and re-plan ---
        logger.info("Plan modifications detected (%d changes)", len(changes))
        for change in changes:
            logger.debug(
                "  Changed: %s -> selection %s",
                change.get("choice_set_id", "unknown"),
                change.get("selected_index"),
            )

        _call_replan(
            zing_path=zing_path,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
            replan_changes=changes,
        )


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
    replan_changes: list[ReviewChange],
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
