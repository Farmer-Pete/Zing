"""FindingGroupPanel widget -- displays a group of audit findings by severity.

Shows a severity header with colored border, followed by a list of findings.
Each finding displays its title, ``file:line`` in monospace, and action
buttons (Fix / Skip / Discuss).

Named ``FindingGroupPanel`` to avoid collision with the existing
``FindingGroup`` dataclass in ``build_audit.py``.
"""

from __future__ import annotations

from dataclasses import dataclass

from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.message import Message
from textual.widget import Widget
from textual.widgets import Button, Label, Static


# ── Data types ───────────────────────────────────────────────────────────────


@dataclass
class FindingData:
    """A single finding to be displayed in the panel."""

    title: str
    location: str  # e.g. "src/main.py:42"
    severity: str  # critical | high | medium | low
    index: int = 0


# ── Severity colors ──────────────────────────────────────────────────────────

_SEVERITY_COLORS: dict[str, str] = {
    "critical": "#E74C3C",
    "high": "#E74C3C",
    "medium": "#F5B041",
    "low": "#6B7280",
}

_SEVERITY_BORDER_CLASS: dict[str, str] = {
    "critical": "status-dot--failed",
    "high": "status-dot--failed",
    "medium": "status-dot--warning",
    "low": "status-dot--pending",
}


# ── Internal sub-widgets ─────────────────────────────────────────────────────


class _FindingRow(Widget):
    """One finding row: title, location, and action buttons."""

    DEFAULT_CSS = """
    _FindingRow {
        height: auto;
        layout: vertical;
        padding: 1 0;
        border: solid #252B36;
        margin: 0 0 1 0;
    }
    _FindingRow .fr-header {
        layout: horizontal;
        height: 1;
    }
    _FindingRow .fr-title {
        width: 1fr;
        text-style: bold;
    }
    _FindingRow .fr-location {
        width: auto;
        color: #6B7280;
        text-style: italic;
    }
    _FindingRow .fr-actions {
        layout: horizontal;
        height: auto;
        padding: 1 0 0 0;
    }
    _FindingRow Button {
        margin: 0 1 0 0;
        min-width: 10;
    }
    """

    class Action(Message):
        """Posted when an action button is pressed on a finding."""

        def __init__(self, finding_index: int, action: str) -> None:
            self.finding_index = finding_index
            self.action = action
            super().__init__()

    def __init__(self, finding: FindingData) -> None:
        super().__init__()
        self._finding = finding

    def compose(self) -> ComposeResult:
        with Horizontal(classes="fr-header"):
            yield Label(self._finding.title, classes="fr-title")
            yield Label(self._finding.location, classes="fr-location")
        with Horizontal(classes="fr-actions"):
            yield Button(
                "Fix",
                classes="action-button action-button--primary",
                id=f"fix-{self._finding.index}",
            )
            yield Button(
                "Skip",
                classes="action-button action-button--secondary",
                id=f"skip-{self._finding.index}",
            )
            yield Button(
                "Discuss",
                classes="action-button action-button--secondary",
                id=f"discuss-{self._finding.index}",
            )

    def on_button_pressed(self, event: Button.Pressed) -> None:
        btn_id = event.button.id or ""
        for action in ("fix", "skip", "discuss"):
            if btn_id.startswith(f"{action}-"):
                self.post_message(
                    _FindingRow.Action(
                        finding_index=self._finding.index,
                        action=action,
                    )
                )
                event.stop()
                break


# ── Public widget ────────────────────────────────────────────────────────────


class FindingGroupPanel(Widget):
    """A panel grouping findings by severity.

    Displays a colored severity header and a list of finding rows,
    each with action buttons (Fix / Skip / Discuss).

    Emits :class:`FindingGroupPanel.FindingAction` when the user clicks
    an action button on any finding.
    """

    DEFAULT_CSS = """
    FindingGroupPanel {
        height: auto;
        width: 1fr;
        layout: vertical;
        padding: 1 2;
        margin: 1 0;
        background: $surface;
    }
    FindingGroupPanel .fgp-header {
        text-style: bold;
        padding: 0 0 1 0;
        height: 1;
    }
    """

    class FindingAction(Message):
        """Posted when a user clicks Fix/Skip/Discuss on a finding."""

        def __init__(self, severity: str, finding_index: int, action: str) -> None:
            self.severity = severity
            self.finding_index = finding_index
            self.action = action
            super().__init__()

    # ── Init ──────────────────────────────────────────────────────────

    def __init__(
        self,
        severity: str,
        findings: list[FindingData] | None = None,
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._severity = severity
        self._findings: list[FindingData] = list(findings or [])

        # Apply severity-colored border
        color = _SEVERITY_COLORS.get(severity.lower(), "#6B7280")
        self.styles.border = ("solid", color)

    # ── Compose ───────────────────────────────────────────────────────

    def compose(self) -> ComposeResult:
        color = _SEVERITY_COLORS.get(self._severity.lower(), "#6B7280")
        header = Label(
            f"{self._severity.upper()} ({len(self._findings)})",
            classes="fgp-header",
        )
        header.styles.color = color
        yield header

        for finding in self._findings:
            yield _FindingRow(finding)

    # ── Bubble up finding actions ─────────────────────────────────────

    def on__finding_row_action(self, event: _FindingRow.Action) -> None:
        """Translate internal _FindingRow.Action to public FindingAction."""
        self.post_message(
            self.FindingAction(
                severity=self._severity,
                finding_index=event.finding_index,
                action=event.action,
            )
        )
        event.stop()

    # ── Public API ────────────────────────────────────────────────────

    @property
    def severity(self) -> str:
        return self._severity

    @property
    def findings(self) -> list[FindingData]:
        return list(self._findings)
