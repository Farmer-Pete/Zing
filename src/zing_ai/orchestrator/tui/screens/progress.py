"""ProgressScreen -- displays subprocess execution with live log output.

Layout: :class:`SubprocessList` docked left (width 30) + :class:`RichLog`
filling the remaining space.  Selecting a subprocess in the list switches
the ``RichLog`` to show that subprocess's captured output.
"""

from __future__ import annotations

import logging

from textual.app import ComposeResult
from textual.screen import Screen
from textual.widgets import RichLog

from zing_ai.orchestrator.tui.notifications import notify
from zing_ai.orchestrator.tui.results import ProgressResult
from zing_ai.orchestrator.tui.widgets.subprocess_list import (
    SubprocessEntry,
    SubprocessList,
)

logger = logging.getLogger(__name__)


class ProgressScreen(Screen[ProgressResult]):
    """Screen that tracks running subprocesses and their log output.

    Provides a navigable subprocess list on the left and a log viewer
    on the right that displays the output of the currently-selected
    subprocess.
    """

    DEFAULT_CSS = """
    ProgressScreen {
        layout: horizontal;
    }
    ProgressScreen SubprocessList {
        dock: left;
        width: 30;
        height: 100%;
    }
    ProgressScreen RichLog {
        width: 1fr;
        height: 100%;
    }
    """

    def __init__(
        self,
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        # Maps subprocess id -> list of output lines
        self._buffers: dict[str, list[str]] = {}
        # Maps subprocess id -> final status string
        self._statuses: dict[str, str] = {}
        # Maps subprocess id -> index in the SubprocessList
        self._id_to_index: dict[str, int] = {}
        # Maps subprocess id -> label
        self._id_to_label: dict[str, str] = {}
        # Currently selected subprocess id (or None)
        self._selected_id: str | None = None

    def compose(self) -> ComposeResult:
        yield SubprocessList(id="sp-list")
        yield RichLog(highlight=True, markup=False, id="log-view")

    # ── Public API ───────────────────────────────────────────────────────

    def on_mount(self) -> None:
        """Flush any subprocess entries added before the screen was mounted."""
        if self._id_to_index:
            self._sync_subprocess_list()

    def _sync_subprocess_list(self) -> None:
        """Rebuild the SubprocessList widget from internal state."""
        sp_list = self.query_one("#sp-list", SubprocessList)
        entries = [
            SubprocessEntry(label=self._id_to_label[sid], status=self._statuses[sid])
            for sid in self._id_to_index
        ]
        sp_list.set_entries(entries)

    def add_subprocess(self, id: str, label: str) -> None:
        """Add a new subprocess entry to the list.

        Args:
            id: Unique identifier for the subprocess.
            label: Human-readable label displayed in the sidebar.
        """
        logger.debug("Adding subprocess: id=%s, label=%s", id, label)
        index = len(self._id_to_index)
        self._id_to_index[id] = index
        self._id_to_label[id] = label
        self._buffers[id] = []
        self._statuses[id] = "pending"

        if self.is_mounted:
            self._sync_subprocess_list()

        # Auto-select the first subprocess added
        if self._selected_id is None:
            self._selected_id = id

    def update_status(self, id: str, status: str) -> None:
        """Update the status dot/badge of a subprocess.

        Args:
            id: The subprocess identifier.
            status: New status string (``"pending"``, ``"running"``,
                    ``"success"``, ``"warning"``, or ``"failed"``).
        """
        logger.debug("Updating subprocess status: id=%s, status=%s", id, status)
        if id not in self._id_to_index:
            return
        self._statuses[id] = status
        if not self.is_mounted:
            return
        index = self._id_to_index[id]
        sp_list = self.query_one("#sp-list", SubprocessList)
        sp_list.update_entry(index, status)

    def append_output(self, id: str, line: str) -> None:
        """Append a line of output to a subprocess's log buffer.

        If the subprocess is currently selected, the line is also
        written to the visible ``RichLog`` immediately.

        Args:
            id: The subprocess identifier.
            line: The text line to append.
        """
        if id not in self._buffers:
            return
        self._buffers[id].append(line)
        if id == self._selected_id and self.is_mounted:
            log_view = self.query_one("#log-view", RichLog)
            log_view.write(line)

    def mark_all_complete(self) -> None:
        """Signal that all subprocesses have finished.

        Sends a desktop notification and dismisses the screen with a
        :class:`ProgressResult` containing all captured output and
        final statuses.
        """
        logger.debug("Marking all %d subprocess(es) complete", len(self._id_to_index))
        notify("Zing", "All subprocesses complete.")
        result = ProgressResult(
            outputs={sid: "\n".join(lines) for sid, lines in self._buffers.items()},
            statuses=dict(self._statuses),
        )
        self.dismiss(result)

    # ── Event handlers ───────────────────────────────────────────────────

    def on_subprocess_list_selected(self, event: SubprocessList.Selected) -> None:
        """Switch the RichLog to display the selected subprocess's output."""
        # Determine which subprocess id was selected by index
        selected_id: str | None = None
        for sid, idx in self._id_to_index.items():
            if idx == event.index:
                selected_id = sid
                break

        if selected_id is None or selected_id == self._selected_id:
            return

        self._selected_id = selected_id
        log_view = self.query_one("#log-view", RichLog)
        log_view.clear()
        for line in self._buffers.get(selected_id, []):
            log_view.write(line)
