"""AuditScreen -- displays grouped audit findings for user triage.

Takes a list of :class:`FindingGroup` data (from
``src/zing_ai/orchestrator/commands/build_audit.py``) and presents them
in a scrollable layout grouped by severity (High -> Medium -> Low).

Each finding has Fix / Skip / Discuss buttons.  The screen dismisses
with an :class:`AuditResult` containing the user's action decision
for every finding.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import VerticalScroll
from textual.screen import Screen
from textual.widgets import Button, Footer, Static

from zing_ai.orchestrator.commands.build_audit import Finding, FindingGroup
from zing_ai.orchestrator.tui.results import AuditResult
from zing_ai.orchestrator.tui.widgets.finding_group import (
    FindingData,
    FindingGroupPanel,
)


class AuditScreen(Screen[AuditResult]):
    """Screen for triaging audit findings grouped by severity.

    Parameters
    ----------
    finding_groups:
        Ordered list of :class:`FindingGroup` instances, typically
        sorted from highest to lowest severity by
        :func:`group_findings_by_severity`.
    """

    DEFAULT_CSS = """
    AuditScreen {
        layout: vertical;
    }
    AuditScreen #audit-scroll {
        height: 1fr;
    }
    AuditScreen #audit-footer {
        dock: bottom;
        height: 3;
        layout: horizontal;
        align: center middle;
        padding: 0 1;
    }
    AuditScreen #audit-footer .footer-status {
        width: 1fr;
        content-align: left middle;
    }
    """

    BINDINGS = [
        Binding("s", "submit", "Submit Decisions"),
    ]

    # ── Init ──────────────────────────────────────────────────────────

    def __init__(
        self,
        finding_groups: list[FindingGroup],
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._finding_groups = finding_groups

        # Map (severity, finding_index) -> action string.
        # Pre-populate with "skip" as the default so every finding
        # always has a decision even if the user doesn't interact with it.
        self._decisions: dict[tuple[str, int], str] = {}
        for group in finding_groups:
            for finding in group.findings:
                self._decisions[(group.severity.lower(), finding.index)] = "skip"

    # ── Compose ───────────────────────────────────────────────────────

    def compose(self) -> ComposeResult:
        with VerticalScroll(id="audit-scroll"):
            for group in self._finding_groups:
                findings_data = [
                    FindingData(
                        title=f.title,
                        location=f.location,
                        severity=group.severity,
                        index=f.index,
                    )
                    for f in group.findings
                ]
                yield FindingGroupPanel(
                    severity=group.severity,
                    findings=findings_data,
                    id=f"group-{group.severity.lower()}",
                )
        with_footer_bar = len(self._finding_groups) > 0
        if with_footer_bar:
            from textual.containers import Horizontal

            with Horizontal(id="audit-footer"):
                yield Static(
                    self._status_text(),
                    classes="footer-status",
                    id="footer-status",
                )
                yield Button(
                    "Submit Decisions",
                    variant="success",
                    id="btn-submit",
                )
        yield Footer()

    # ── Message handlers ─────────────────────────────────────────────

    def on_finding_group_panel_finding_action(
        self, event: FindingGroupPanel.FindingAction
    ) -> None:
        """Record the user's action choice for a finding."""
        key = (event.severity.lower(), event.finding_index)
        self._decisions[key] = event.action
        self._update_footer_status()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        """Handle the submit button press."""
        if event.button.id == "btn-submit":
            self._do_submit()

    # ── Actions (key bindings) ───────────────────────────────────────

    def action_submit(self) -> None:
        """Submit decisions and dismiss the screen."""
        self._do_submit()

    # ── Private helpers ──────────────────────────────────────────────

    def _do_submit(self) -> None:
        """Build an AuditResult from recorded decisions and dismiss."""
        decisions: list[dict] = []
        for (severity, finding_index), action in sorted(
            self._decisions.items(), key=lambda kv: kv[0][1]
        ):
            decisions.append(
                {
                    "severity": severity,
                    "finding_index": finding_index,
                    "action": action,
                }
            )
        self.dismiss(AuditResult(decisions=decisions))

    def _status_text(self) -> str:
        """Build a summary of current decisions."""
        fix_count = sum(1 for a in self._decisions.values() if a == "fix")
        skip_count = sum(1 for a in self._decisions.values() if a == "skip")
        discuss_count = sum(
            1 for a in self._decisions.values() if a == "discuss"
        )
        total = len(self._decisions)
        parts: list[str] = []
        if fix_count:
            parts.append(f"{fix_count} fix")
        if skip_count:
            parts.append(f"{skip_count} skip")
        if discuss_count:
            parts.append(f"{discuss_count} discuss")
        return f"Decisions: {', '.join(parts)} ({total} total)"

    def _update_footer_status(self) -> None:
        """Refresh the footer status label."""
        try:
            status_label = self.query_one("#footer-status", Static)
            status_label.update(self._status_text())
        except Exception:
            pass

    # ── Public API ───────────────────────────────────────────────────

    @property
    def decisions(self) -> dict[tuple[str, int], str]:
        """Current decision map: ``(severity, finding_index) -> action``."""
        return dict(self._decisions)
