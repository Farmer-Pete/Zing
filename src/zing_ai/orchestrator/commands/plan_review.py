"""Orchestrator ``plan-review`` command -- review and approve a plan.

Shows the plan's choices to the user in the web UI.  The user can approve
(no changes), modify choices (switch recommended option), or delete choice
sets.  Approval triggers the build; modifications trigger the re-plan ->
re-audit -> review loop.
"""

from __future__ import annotations

import asyncio
import logging
import threading
from dataclasses import dataclass, field
from pathlib import Path

from zing_ai.orchestrator import project
from zing_ai.orchestrator.config import ZingConfig
from zing_ai.orchestrator.models import ChoiceSet
from zing_ai.orchestrator.xml_parser import parse_zing_file, write_zing_file

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Review state shared between the command and routes
# ---------------------------------------------------------------------------


@dataclass
class ReviewState:
    """Mutable state shared between ``run_plan_review`` and the route handlers.

    Stored on ``app.state.review`` so route handlers can read/update
    choices and signal the review decision.
    """

    #: Current choice sets (mutable -- routes modify selections and deletions).
    choice_sets: list[ChoiceSet] = field(default_factory=list)

    #: Tracks user selections per choice-set index.
    #: Maps choice_set_index -> selected_choice_index (or ``None`` for deleted).
    user_selections: dict[int, int | None] = field(default_factory=dict)

    #: Set to ``True`` when the user clicks "Approve & Build".
    approved: bool = False

    #: Event signalled when the user has made their final decision
    #: (either approve or modify-and-submit).  Uses ``threading.Event``
    #: (not ``asyncio.Event``) because it is set from the uvicorn daemon
    #: thread and waited on from the main asyncio thread.
    decision_event: threading.Event = field(default_factory=threading.Event)

    @property
    def has_modifications(self) -> bool:
        """Return ``True`` if the user has changed any choices from recommended."""
        for cs_idx, sel_idx in self.user_selections.items():
            if sel_idx is None:
                # Deletion is always a modification
                return True
            if 0 <= cs_idx < len(self.choice_sets):
                cs = self.choice_sets[cs_idx]
                if 0 <= sel_idx < len(cs.choices) and not cs.choices[sel_idx].recommended:
                    return True
        return False


# ---------------------------------------------------------------------------
# Diff computation
# ---------------------------------------------------------------------------


def _compute_changes(
    original_choice_sets: list[ChoiceSet],
    user_selections: dict[int, int | None],
) -> list[dict]:
    """Compute a diff of user modifications against the original recommendations.

    Parameters
    ----------
    original_choice_sets:
        The choice sets as loaded from the zing document (before any user edits).
    user_selections:
        Maps ``choice_set_index`` -> ``selected_choice_index`` (or ``None``
        for deleted choice sets).

    Returns
    -------
    list[dict]
        Each dict has keys: ``choice_set_message``, ``original_recommended``,
        ``user_selected``, and optionally ``deleted: True``.  Matches the
        format expected by ``plan_replan.md.j2`` and ``plan_reaudit.md.j2``.
    """
    changes: list[dict] = []

    for cs_idx, sel_idx in sorted(user_selections.items()):
        if cs_idx < 0 or cs_idx >= len(original_choice_sets):
            continue

        cs = original_choice_sets[cs_idx]
        recommended = next((c for c in cs.choices if c.recommended), None)
        recommended_label = recommended.label if recommended else "N/A"

        if sel_idx is None:
            # User deleted this choice set
            changes.append({
                "choice_set_message": cs.message,
                "original_recommended": recommended_label,
                "user_selected": recommended_label,
                "deleted": True,
            })
        else:
            if 0 <= sel_idx < len(cs.choices):
                selected = cs.choices[sel_idx]
                if not selected.recommended:
                    # User switched from recommended to a different choice
                    changes.append({
                        "choice_set_message": cs.message,
                        "original_recommended": recommended_label,
                        "user_selected": selected.label,
                    })

    return changes


# ---------------------------------------------------------------------------
# Web server with review state
# ---------------------------------------------------------------------------


def _start_review_server(
    zing_file_path: Path | None,
    review_state: ReviewState,
    *,
    port: int,
    no_browser: bool,
) -> threading.Thread:
    """Start the FastAPI web server with review state in a background thread.

    The review state is attached to ``app.state.review`` so the route
    handlers can access it.

    Parameters
    ----------
    zing_file_path:
        Path to the zing XML file.
    review_state:
        Shared mutable state for the review session.
    port:
        Port to listen on.
    no_browser:
        If ``True``, do not open the browser.

    Returns
    -------
    threading.Thread
        The daemon thread running the server.
    """
    from zing_ai.orchestrator.web.app import create_app, start_server

    app = create_app(zing_file=zing_file_path)
    app.state.review = review_state

    thread = threading.Thread(
        target=start_server,
        args=(app,),
        kwargs={"port": port, "no_browser": no_browser},
        daemon=True,
    )
    thread.start()
    return thread


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


async def run_plan_review(
    *,
    zing_file: str | None,
    no_browser: bool,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``plan-review`` orchestrator command.

    Starts the web UI, loads choices from the zing document, and waits
    for the user to either approve the plan or modify choices.

    **Approval (no changes):**
        Sets ``approved=True`` on the document and calls ``run_build()``.

    **Modifications:**
        Computes a diff of changed choices and calls
        ``run_plan(replan_changes=changes)`` to re-enter the
        plan -> audit -> review loop.

    Parameters
    ----------
    zing_file:
        Optional zing file name to review.
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
        await _call_build(
            zing_path=zing_path,
            no_browser=no_browser,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
        )
        return

    # Build shared review state
    review = ReviewState(choice_sets=choice_sets)

    # Start the web server with review state
    _start_review_server(
        zing_path,
        review,
        port=config.port,
        no_browser=no_browser,
    )

    logger.info(
        "Waiting for user decision (%d choice sets to review)...",
        len(choice_sets),
    )

    # Wait for the user to approve or modify.  ``decision_event`` is a
    # threading.Event (set from the uvicorn daemon thread), so we poll it
    # from the async loop to avoid blocking.
    while not review.decision_event.is_set():
        await asyncio.sleep(0.2)

    if review.approved and not review.has_modifications:
        # --- Approval path: no changes ---
        logger.info("Plan approved (no changes)")
        doc.approved = True
        write_zing_file(zing_path, doc)

        await _call_build(
            zing_path=zing_path,
            no_browser=no_browser,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
        )
    else:
        # --- Modification path: compute diff and re-plan ---
        changes = _compute_changes(choice_sets, review.user_selections)
        logger.info("Plan modifications detected (%d changes)", len(changes))
        for change in changes:
            if change.get("deleted"):
                logger.debug(
                    "  Deleted: %s", change["choice_set_message"]
                )
            else:
                logger.debug(
                    "  Changed: %s — %s -> %s",
                    change["choice_set_message"],
                    change["original_recommended"],
                    change["user_selected"],
                )

        await _call_replan(
            zing_path=zing_path,
            no_browser=no_browser,
            skip_permissions=skip_permissions,
            config=config,
            project_root=project_root,
            replan_changes=changes,
        )


# ---------------------------------------------------------------------------
# Flow-control helpers (separate for testability)
# ---------------------------------------------------------------------------


async def _call_build(
    *,
    zing_path: Path,
    no_browser: bool,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Call ``run_build`` to execute the approved plan."""
    from zing_ai.orchestrator.commands.build import run_build

    await run_build(
        zing_file=zing_path.name,
        no_browser=no_browser,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
    )


async def _call_replan(
    *,
    zing_path: Path,
    no_browser: bool,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
    replan_changes: list[dict],
) -> None:
    """Call ``run_plan`` with ``replan_changes`` to re-enter the plan loop."""
    from zing_ai.orchestrator.commands.plan import run_plan

    await run_plan(
        zing_file=zing_path.name,
        no_browser=no_browser,
        skip_permissions=skip_permissions,
        config=config,
        project_root=project_root,
        replan_changes=replan_changes,
    )
