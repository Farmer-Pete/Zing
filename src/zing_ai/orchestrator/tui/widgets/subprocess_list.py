"""SubprocessList widget -- a navigable list of subprocess entries.

Each entry displays a colored status dot, a label, and a status badge.
Arrow keys navigate items; selection changes emit :class:`SubprocessList.Selected`.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

from textual.app import ComposeResult
from textual.message import Message
from textual.widgets import Label, ListItem, ListView

logger = logging.getLogger(__name__)

# ── Subprocess entry data ────────────────────────────────────────────────────


@dataclass
class SubprocessEntry:
    """Data for a single subprocess displayed in the list."""

    label: str
    status: str = "pending"  # pending | running | success | warning | failed


# ── Internal sub-widgets ─────────────────────────────────────────────────────

_STATUS_DOTS: dict[str, str] = {
    "pending": "\u25cb",   # ○
    "running": "\u25cf",   # ●
    "success": "\u2713",   # ✓
    "warning": "\u25cf",   # ●
    "failed": "\u2717",    # ✗
}


class _SubprocessItem(ListItem):
    """A single row inside the subprocess list."""

    DEFAULT_CSS = """
    _SubprocessItem {
        height: 1;
        layout: horizontal;
    }
    _SubprocessItem .sp-dot {
        width: 3;
        content-align: center middle;
    }
    _SubprocessItem .sp-label {
        width: 1fr;
    }
    _SubprocessItem .sp-badge {
        width: auto;
        padding: 0 1;
    }
    """

    def __init__(self, entry: SubprocessEntry) -> None:
        super().__init__()
        self.entry = entry

    def compose(self) -> ComposeResult:
        dot_char = _STATUS_DOTS.get(self.entry.status, "\u25cb")
        yield Label(
            dot_char,
            classes=f"sp-dot status-dot status-dot--{self.entry.status}",
        )
        yield Label(self.entry.label, classes="sp-label")
        yield Label(
            self.entry.status,
            classes=f"sp-badge status-dot--{self.entry.status}",
        )


# ── Public widget ────────────────────────────────────────────────────────────


class SubprocessList(ListView):
    """Navigable list of subprocess entries with status indicators.

    Emits :class:`SubprocessList.Selected` when the highlighted entry changes.
    """

    class Selected(Message):
        """Posted when the highlighted subprocess changes."""

        def __init__(self, index: int, entry: SubprocessEntry) -> None:
            self.index = index
            self.entry = entry
            super().__init__()

    # ------------------------------------------------------------------

    def __init__(
        self,
        entries: list[SubprocessEntry] | None = None,
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._entries: list[SubprocessEntry] = list(entries or [])

    def compose(self) -> ComposeResult:
        for entry in self._entries:
            yield _SubprocessItem(entry)

    # ------------------------------------------------------------------

    def on_list_view_highlighted(self, event: ListView.Highlighted) -> None:
        """Re-emit as SubprocessList.Selected with entry data."""
        if event.item is not None and isinstance(event.item, _SubprocessItem):
            idx = self.index
            if idx is not None and 0 <= idx < len(self._entries):
                self.post_message(self.Selected(idx, event.item.entry))

    # ------------------------------------------------------------------

    def set_entries(self, entries: list[SubprocessEntry]) -> None:
        """Replace all entries and re-render."""
        logger.debug("Setting %d subprocess entries", len(entries))
        self._entries = list(entries)
        self.clear()
        for entry in self._entries:
            self.append(_SubprocessItem(entry))

    def update_entry(self, index: int, status: str) -> None:
        """Update the status of an existing entry by index."""
        logger.debug("Updating entry %d: status=%s", index, status)
        if 0 <= index < len(self._entries):
            self._entries[index].status = status
            # Re-mount the item
            item = _SubprocessItem(self._entries[index])
            children = list(self.children)
            if index < len(children):
                children[index].remove()
                self.mount(item, before=index)
