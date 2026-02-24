"""PlanReviewScreen -- presents plan choices for user review and approval.

Layout: :class:`VerticalScroll` container with :class:`ChoiceCard` widgets
stacked vertically.  Footer bar with action button ("Approve & Build" /
"Re-plan & Continue").

Key bindings:
  ``a`` -- approve the plan
  ``d`` -- delete the focused card
  ``up`` / ``down`` -- navigate between cards
  ``enter`` -- cycle through radio options on the focused card
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, VerticalScroll
from textual.screen import Screen
from textual.widgets import Button, Footer, Static

from zing_ai.orchestrator.models import ChoiceSet
from zing_ai.orchestrator.tui.results import ReviewResult
from zing_ai.orchestrator.tui.widgets.choice_card import ChoiceCard


class PlanReviewScreen(Screen[ReviewResult]):
    """Screen for reviewing and optionally modifying plan choices.

    Parameters
    ----------
    choice_sets:
        The list of :class:`ChoiceSet` objects to display as cards.
    """

    DEFAULT_CSS = """
    PlanReviewScreen {
        layout: vertical;
    }
    PlanReviewScreen #review-scroll {
        height: 1fr;
    }
    PlanReviewScreen #review-scroll ChoiceCard {
        margin: 0 0 1 0;
    }
    PlanReviewScreen #review-footer {
        dock: bottom;
        height: 3;
        layout: horizontal;
        align: center middle;
        padding: 0 1;
    }
    PlanReviewScreen #review-footer .footer-status {
        width: 1fr;
        content-align: left middle;
    }
    PlanReviewScreen ChoiceCard:focus {
        border: tall $accent;
    }
    """

    BINDINGS = [
        Binding("a", "approve", "Approve & Build"),
        Binding("d", "delete_card", "Delete Card"),
        Binding("up", "focus_previous_card", "Previous Card", show=False, priority=True),
        Binding("down", "focus_next_card", "Next Card", show=False, priority=True),
        Binding("enter", "cycle_radio", "Cycle Option", show=False),
    ]

    def __init__(
        self,
        choice_sets: list[ChoiceSet],
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._choice_sets = choice_sets
        # Track which cards have been modified or deleted
        # Maps card_id -> new selected index (int) or None if deleted
        self._modifications: dict[str, int | None] = {}
        # Ordered list of card ids (mirrors compose order, entries removed on delete)
        self._card_ids: list[str] = []
        # Set of deleted card ids
        self._deleted_ids: set[str] = set()

    def compose(self) -> ComposeResult:
        with VerticalScroll(id="review-scroll"):
            for i, cs in enumerate(self._choice_sets):
                card_id = f"choice-{i}"
                self._card_ids.append(card_id)
                choices = [(c.label, c.description) for c in cs.choices]
                recommended = next(
                    (j for j, c in enumerate(cs.choices) if c.recommended), 0
                )
                yield ChoiceCard(
                    title=cs.message,
                    explanation=cs.explanation,
                    choices=choices,
                    recommended_index=recommended,
                    card_id=card_id,
                    id=f"card-{card_id}",
                    classes="focusable-card",
                )
        with Horizontal(id="review-footer"):
            yield Static("", classes="footer-status", id="footer-status")
            yield Button(
                "Approve & Build",
                variant="success",
                id="btn-approve",
            )
        yield Footer()

    def on_mount(self) -> None:
        """Focus the first card after mount."""
        self._update_footer_status()
        cards = self.query("ChoiceCard")
        if cards:
            cards.first().focus()

    # ── Message handlers ─────────────────────────────────────────────

    def on_choice_card_modified(self, event: ChoiceCard.Modified) -> None:
        """Track when a card's selection changes from recommended."""
        if event.is_modified:
            self._modifications[event.card_id] = event.selected_index
        else:
            self._modifications.pop(event.card_id, None)
        self._update_footer_status()

    def on_choice_card_deleted(self, event: ChoiceCard.Deleted) -> None:
        """Remove a card from the display and mark as deleted."""
        self._perform_delete(event.card_id)

    def on_button_pressed(self, event: Button.Pressed) -> None:
        """Handle the approve button press."""
        if event.button.id == "btn-approve":
            self._do_approve()

    # ── Actions (key bindings) ───────────────────────────────────────

    def action_approve(self) -> None:
        """Approve the current plan (possibly with modifications)."""
        self._do_approve()

    def action_delete_card(self) -> None:
        """Delete the currently focused card."""
        focused = self.focused
        if isinstance(focused, ChoiceCard):
            self._perform_delete(focused.card_id)

    def action_focus_next_card(self) -> None:
        """Move focus to the next ChoiceCard."""
        self.focus_next("ChoiceCard")

    def action_focus_previous_card(self) -> None:
        """Move focus to the previous ChoiceCard."""
        self.focus_previous("ChoiceCard")

    def action_cycle_radio(self) -> None:
        """Cycle to the next radio option on the focused card."""
        focused = self.focused
        if isinstance(focused, ChoiceCard):
            from textual.widgets import RadioSet

            try:
                radio_set = focused.query_one(RadioSet)
                current = radio_set.pressed_index
                count = len(focused._choices)
                if count > 0:
                    next_index = (current + 1) % count
                    radio_set.pressed_index = next_index
            except Exception:
                pass

    # ── Private helpers ──────────────────────────────────────────────

    def _do_approve(self) -> None:
        """Build the ReviewResult and dismiss."""
        has_changes = bool(self._modifications) or bool(self._deleted_ids)

        if not has_changes:
            self.dismiss(ReviewResult(action="approve", changes=[]))
        else:
            changes: list[dict] = []
            # Modified selections
            for card_id, new_idx in self._modifications.items():
                if card_id not in self._deleted_ids:
                    changes.append(
                        {"choice_id": card_id, "new_selection": new_idx}
                    )
            # Deleted cards
            for card_id in self._deleted_ids:
                changes.append({"choice_id": card_id, "new_selection": None})
            self.dismiss(ReviewResult(action="replan", changes=changes))

    def _perform_delete(self, card_id: str) -> None:
        """Remove a card widget and track the deletion."""
        self._deleted_ids.add(card_id)
        # Also remove from modifications if present (deletion overrides)
        self._modifications.pop(card_id, None)

        # Find the card widget and determine focus movement
        try:
            card = self.query_one(f"#card-{card_id}", ChoiceCard)
        except Exception:
            return

        # Determine which card to focus next
        visible_cards = list(self.query("ChoiceCard"))
        card_index = None
        for i, c in enumerate(visible_cards):
            if c.card_id == card_id:
                card_index = i
                break

        card.remove()

        if card_id in self._card_ids:
            self._card_ids.remove(card_id)

        # Focus the next (or previous) card after removal
        remaining = list(self.query("ChoiceCard"))
        if remaining and card_index is not None:
            focus_idx = min(card_index, len(remaining) - 1)
            remaining[focus_idx].focus()

        self._update_footer_status()

    def _update_footer_status(self) -> None:
        """Update the footer status text based on modification state."""
        has_changes = bool(self._modifications) or bool(self._deleted_ids)
        try:
            status_label = self.query_one("#footer-status", Static)
            btn = self.query_one("#btn-approve", Button)
        except Exception:
            return

        if has_changes:
            mod_count = len(self._modifications)
            del_count = len(self._deleted_ids)
            parts = []
            if mod_count:
                parts.append(f"{mod_count} modified")
            if del_count:
                parts.append(f"{del_count} deleted")
            status_label.update(f"Changes: {', '.join(parts)}")
            btn.label = "Re-plan & Continue"
        else:
            status_label.update("No changes")
            btn.label = "Approve & Build"

    # ── Public API ───────────────────────────────────────────────────

    @property
    def has_modifications(self) -> bool:
        """Return True if any cards have been modified or deleted."""
        return bool(self._modifications) or bool(self._deleted_ids)
