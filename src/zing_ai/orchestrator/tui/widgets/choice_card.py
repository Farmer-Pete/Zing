"""ChoiceCard widget -- compound widget for displaying and selecting choices.

Shows a title, markdown explanation, RadioSet of choices with a recommended
pill on the recommended option, and a delete button.  Tracks whether the
user has modified the selection from the recommended default.

Emits :class:`ChoiceCard.Modified` and :class:`ChoiceCard.Deleted` messages.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.containers import Horizontal
from textual.message import Message
from textual.reactive import reactive
from textual.widget import Widget
from textual.widgets import Button, Label, Markdown, RadioButton, RadioSet


class ChoiceCard(Widget, can_focus=True):
    """A card displaying choices with explanation and selection tracking.

    Parameters
    ----------
    title:
        Card heading text (typically the ``ChoiceSet.message``).
    explanation:
        Markdown body (typically the ``ChoiceSet.explanation``).
    choices:
        List of ``(label, description)`` tuples.
    recommended_index:
        Zero-based index of the recommended choice.
    card_id:
        Optional identifier for this card, included in messages.
    """

    DEFAULT_CSS = """
    ChoiceCard {
        height: auto;
        width: 1fr;
        layout: vertical;
    }
    ChoiceCard .cc-title {
        text-style: bold;
        padding: 0 0 1 0;
    }
    ChoiceCard .cc-explanation {
        padding: 0 0 1 0;
    }
    ChoiceCard .cc-radio-row {
        height: auto;
        layout: horizontal;
    }
    ChoiceCard .cc-footer {
        height: auto;
        layout: horizontal;
        align: right middle;
        padding: 1 0 0 0;
    }
    """

    # ── Messages ──────────────────────────────────────────────────────

    class Modified(Message):
        """Posted when the user changes the selection from/to the recommended."""

        def __init__(self, card_id: str, selected_index: int, is_modified: bool) -> None:
            self.card_id = card_id
            self.selected_index = selected_index
            self.is_modified = is_modified
            super().__init__()

    class Deleted(Message):
        """Posted when the user presses the delete button."""

        def __init__(self, card_id: str) -> None:
            self.card_id = card_id
            super().__init__()

    # ── Reactive state ────────────────────────────────────────────────

    is_modified: reactive[bool] = reactive(False)

    # ── Init ──────────────────────────────────────────────────────────

    def __init__(
        self,
        title: str,
        explanation: str,
        choices: list[tuple[str, str]],
        recommended_index: int = 0,
        card_id: str = "",
        *,
        name: str | None = None,
        id: str | None = None,
        classes: str | None = None,
    ) -> None:
        super().__init__(name=name, id=id, classes=classes)
        self._title = title
        self._explanation = explanation
        self._choices = choices
        self._recommended_index = recommended_index
        self._card_id = card_id
        self.add_class("choice-card")

    # ── Compose ───────────────────────────────────────────────────────

    def compose(self) -> ComposeResult:
        yield Label(self._title, classes="cc-title")
        yield Markdown(self._explanation, classes="cc-explanation")

        radio_buttons: list[RadioButton] = []
        for i, (label, _description) in enumerate(self._choices):
            display_label = label
            if i == self._recommended_index:
                display_label = f"{label} [recommended]"
            radio_buttons.append(
                RadioButton(display_label, value=(i == self._recommended_index))
            )

        yield RadioSet(*radio_buttons, id=f"radioset-{self._card_id}")

        with Horizontal(classes="cc-footer"):
            yield Button(
                "Delete",
                variant="error",
                classes="action-button action-button--danger",
                id=f"delete-{self._card_id}",
            )

    # ── Event handlers ────────────────────────────────────────────────

    def on_radio_set_changed(self, event: RadioSet.Changed) -> None:
        """Track whether current selection differs from recommended."""
        selected_index = event.radio_set.pressed_index
        self.is_modified = selected_index != self._recommended_index

        if self.is_modified:
            self.add_class("choice-card--modified")
        else:
            self.remove_class("choice-card--modified")

        self.post_message(
            self.Modified(
                card_id=self._card_id,
                selected_index=selected_index,
                is_modified=self.is_modified,
            )
        )

    def on_button_pressed(self, event: Button.Pressed) -> None:
        """Handle delete button press."""
        if event.button.id == f"delete-{self._card_id}":
            self.post_message(self.Deleted(card_id=self._card_id))

    # ── Public API ────────────────────────────────────────────────────

    @property
    def selected_index(self) -> int:
        """Return the currently selected choice index."""
        try:
            radio_set = self.query_one(RadioSet)
            return radio_set.pressed_index
        except Exception:
            return self._recommended_index

    @property
    def card_id(self) -> str:
        return self._card_id

    @property
    def recommended_index(self) -> int:
        return self._recommended_index
