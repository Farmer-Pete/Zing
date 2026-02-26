"""Rich-based inline menu functions for user interaction.

Provides numbered-prompt menus for plan review, audit triage, and
general option selection.  All functions use ``rich.prompt.Prompt``
for input and ``rich.panel.Panel`` / ``rich.table.Table`` for display.
"""

from __future__ import annotations

from typing import Literal

from rich.panel import Panel
from rich.prompt import Prompt
from rich.table import Table

from zing_ai.orchestrator.commands.build_audit import Finding, FindingGroup
from zing_ai.orchestrator.models import ChoiceSet
from zing_ai.orchestrator.ui import console
from zing_ai.orchestrator.ui.types import AuditDecision, MenuOption, ReviewChange


def numbered_menu(
    title: str,
    options: list[MenuOption],
    recommended: int | None = None,
) -> int:
    """Display a numbered menu and return the selected zero-based index.

    Parameters
    ----------
    title:
        Heading displayed in a Rich Panel above the options.
    options:
        Items to display.  Each must have ``label`` and ``description``.
    recommended:
        If set, the zero-based index whose label gets an
        ``(recommended)`` suffix.

    Returns
    -------
    int
        The zero-based index chosen by the user.

    Raises
    ------
    SystemExit
        ``SystemExit(130)`` on ``KeyboardInterrupt`` so the CLI exits
        cleanly.
    """
    rows: list[str] = []
    for idx, opt in enumerate(options):
        label = opt["label"]
        if recommended is not None and idx == recommended:
            label = f"{label} (recommended)"
        rows.append(f"  [bold cyan]{idx + 1}[/bold cyan]. {label} -- {opt['description']}")

    body = "\n".join(rows)
    console.print(Panel(body, title=title, expand=False))

    while True:
        try:
            raw = Prompt.ask(
                f"Select an option [1-{len(options)}]",
                console=console,
            )
        except KeyboardInterrupt:
            raise SystemExit(130)

        try:
            choice = int(raw)
        except ValueError:
            console.print("[red]Please enter a valid number.[/red]")
            continue

        if 1 <= choice <= len(options):
            return choice - 1

        console.print(f"[red]Choice must be between 1 and {len(options)}.[/red]")


def plan_review_menu(
    choice_sets: list[ChoiceSet],
) -> tuple[Literal["approve", "replan"], list[ReviewChange]]:
    """Walk through each *ChoiceSet* and collect user selections.

    After all choices are made a summary table is shown and the user
    picks "Approve & Build" or "Request Replan".

    Parameters
    ----------
    choice_sets:
        The choice sets parsed from the zing document.

    Returns
    -------
    tuple
        ``(action, changes)`` where *action* is ``"approve"`` or
        ``"replan"`` and *changes* is a list of :class:`ReviewChange`
        dicts recording each selection.
    """
    changes: list[ReviewChange] = []

    for cs_idx, cs in enumerate(choice_sets):
        # Find the recommended index
        rec_idx: int | None = None
        for i, ch in enumerate(cs.choices):
            if ch.recommended:
                rec_idx = i
                break

        options: list[MenuOption] = [
            MenuOption(label=ch.label, description=ch.description)
            for ch in cs.choices
        ]

        title = f"Choice {cs_idx + 1}/{len(choice_sets)}: {cs.message}"
        console.print(f"\n[dim]{cs.explanation}[/dim]")
        selected = numbered_menu(title, options, recommended=rec_idx)

        changes.append(
            ReviewChange(choice_set_id=cs.message, selected_index=selected)
        )

    # --- Summary table ---
    table = Table(title="Review Summary")
    table.add_column("#", style="bold")
    table.add_column("Choice Set")
    table.add_column("Selected")

    for i, (cs, change) in enumerate(zip(choice_sets, changes)):
        sel_label = cs.choices[change["selected_index"]].label
        table.add_row(str(i + 1), cs.message, sel_label)

    console.print(table)

    # --- Approve / Replan ---
    action_idx = numbered_menu(
        "What would you like to do?",
        [
            MenuOption(label="Approve & Build", description="Proceed with these selections"),
            MenuOption(label="Request Replan", description="Send changes back for re-planning"),
        ],
    )

    action: Literal["approve", "replan"] = "approve" if action_idx == 0 else "replan"
    return action, changes


def audit_triage_menu(
    finding_groups: list[FindingGroup],
) -> list[AuditDecision]:
    """Present audit findings grouped by severity and collect triage decisions.

    Each finding is displayed in a Rich Panel and the user selects
    Fix / Skip / Discuss via :func:`numbered_menu`.

    Parameters
    ----------
    finding_groups:
        Groups of findings, each with a severity level and list of
        :class:`Finding` objects.

    Returns
    -------
    list[AuditDecision]
        One decision per finding in presentation order.
    """
    decisions: list[AuditDecision] = []
    action_options: list[MenuOption] = [
        MenuOption(label="Fix", description="Address this finding"),
        MenuOption(label="Skip", description="Ignore this finding"),
        MenuOption(label="Discuss", description="Flag for further discussion"),
    ]
    action_map: dict[int, Literal["fix", "skip", "discuss"]] = {
        0: "fix",
        1: "skip",
        2: "discuss",
    }

    for group in finding_groups:
        console.print(f"\n[bold underline]Severity: {group.severity}[/bold underline]")

        for finding in group.findings:
            body = (
                f"[bold]{finding.title}[/bold]\n"
                f"Category: {finding.category}\n"
                f"Location: {finding.location}\n"
                f"Confidence: {finding.confidence}"
            )
            console.print(Panel(body, title=f"Finding #{finding.index}", expand=False))

            selected = numbered_menu(
                f"Action for finding #{finding.index}",
                action_options,
            )

            decisions.append(
                AuditDecision(
                    finding_index=finding.index,
                    category=finding.category,
                    severity=group.severity,
                    title=finding.title,
                    action=action_map[selected],
                )
            )

    return decisions
