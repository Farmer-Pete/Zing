"""Tests for PlanReviewScreen -- plan choice review and approval."""

from __future__ import annotations

import pytest
from textual.app import App, ComposeResult

from zing_ai.orchestrator.models import Choice, ChoiceSet
from zing_ai.orchestrator.tui.results import ReviewResult
from zing_ai.orchestrator.tui.screens.plan_review import PlanReviewScreen
from zing_ai.orchestrator.tui.widgets.choice_card import ChoiceCard


# ── Fixtures ──────────────────────────────────────────────────────────────


def _make_choice_sets() -> list[ChoiceSet]:
    """Create sample ChoiceSet data for testing."""
    return [
        ChoiceSet(
            message="Database engine",
            explanation="Choose the database backend.",
            choices=[
                Choice(label="PostgreSQL", description="Robust RDBMS", recommended=True),
                Choice(label="SQLite", description="Lightweight embedded DB", recommended=False),
            ],
        ),
        ChoiceSet(
            message="Auth provider",
            explanation="Choose the authentication method.",
            choices=[
                Choice(label="OAuth2", description="Industry standard", recommended=False),
                Choice(label="JWT", description="Simple token-based", recommended=True),
            ],
        ),
        ChoiceSet(
            message="Hosting",
            explanation="Choose the deployment target.",
            choices=[
                Choice(label="AWS", description="Amazon Web Services", recommended=True),
                Choice(label="GCP", description="Google Cloud Platform", recommended=False),
                Choice(label="Azure", description="Microsoft Azure", recommended=False),
            ],
        ),
    ]


# ── Helper app that pushes PlanReviewScreen ───────────────────────────────


class PlanReviewApp(App[ReviewResult | None]):
    """Test harness that pushes a PlanReviewScreen and captures its result."""

    def __init__(self, choice_sets: list[ChoiceSet] | None = None) -> None:
        super().__init__()
        self._choice_sets = choice_sets or _make_choice_sets()
        self.screen_result: ReviewResult | None = None

    def on_mount(self) -> None:
        screen = PlanReviewScreen(self._choice_sets)
        self.push_screen(screen, callback=self._on_dismiss)

    def _on_dismiss(self, result: ReviewResult) -> None:
        self.screen_result = result
        self.exit()


# ── Tests ──────────────────────────────────────────────────────────────────


class TestPlanReviewScreenRender:
    """PlanReviewScreen should compose choice cards from ChoiceSet data."""

    @pytest.mark.asyncio
    async def test_renders_choice_cards_from_choice_sets(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            cards = list(screen.query(ChoiceCard))
            assert len(cards) == 3

            # Verify card titles match the ChoiceSet messages
            assert cards[0]._title == "Database engine"
            assert cards[1]._title == "Auth provider"
            assert cards[2]._title == "Hosting"

    @pytest.mark.asyncio
    async def test_renders_footer_with_approve_button(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            from textual.widgets import Button

            btn = screen.query_one("#btn-approve", Button)
            assert btn is not None
            assert str(btn.label) == "Approve & Build"


class TestApproveKey:
    """Pressing 'a' should dismiss with ReviewResult(action='approve')."""

    @pytest.mark.asyncio
    async def test_a_key_dismisses_with_approve(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            await pilot.press("a")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, ReviewResult)
            assert result.action == "approve"
            assert result.changes == []


class TestModifyThenApprove:
    """Modifying a choice then pressing 'a' should produce action='replan'."""

    @pytest.mark.asyncio
    async def test_modify_choice_then_approve(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            # Simulate a card being modified by posting a Modified message
            # directly (since interacting with RadioSet in headless tests
            # is unreliable)
            cards = list(screen.query(ChoiceCard))
            first_card = cards[0]

            # Simulate modification: the user changed from recommended (0) to index 1
            msg = ChoiceCard.Modified(
                card_id=first_card.card_id,
                selected_index=1,
                is_modified=True,
            )
            screen.on_choice_card_modified(msg)
            await pilot.pause()

            # Now approve
            await pilot.press("a")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert isinstance(result, ReviewResult)
            assert result.action == "replan"
            assert len(result.changes) == 1
            assert result.changes[0]["choice_id"] == "choice-0"
            assert result.changes[0]["new_selection"] == 1


class TestDeleteCard:
    """Pressing 'd' should delete the focused card and mark as modified."""

    @pytest.mark.asyncio
    async def test_d_deletes_focused_card(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            # The first card should be focused on mount
            cards_before = list(screen.query(ChoiceCard))
            assert len(cards_before) == 3

            await pilot.press("d")
            await pilot.pause()

            cards_after = list(screen.query(ChoiceCard))
            assert len(cards_after) == 2

            # The deletion should be tracked
            assert "choice-0" in screen._deleted_ids
            assert screen.has_modifications is True

    @pytest.mark.asyncio
    async def test_delete_then_approve_produces_replan(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            # Delete first card
            await pilot.press("d")
            await pilot.pause()

            # Approve
            await pilot.press("a")
            await pilot.pause()

            result = app.screen_result
            assert result is not None
            assert result.action == "replan"
            # Should contain a change entry with new_selection=None for deleted
            deleted_changes = [
                c for c in result.changes if c["new_selection"] is None
            ]
            assert len(deleted_changes) == 1
            assert deleted_changes[0]["choice_id"] == "choice-0"


class TestArrowKeyNavigation:
    """Arrow keys should navigate focus between choice cards."""

    @pytest.mark.asyncio
    async def test_down_arrow_moves_to_next_card(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            # First card should be focused
            focused = screen.focused
            assert isinstance(focused, ChoiceCard)
            assert focused.card_id == "choice-0"

            # Press down arrow
            await pilot.press("down")
            await pilot.pause()

            focused = screen.focused
            assert isinstance(focused, ChoiceCard)
            assert focused.card_id == "choice-1"

    @pytest.mark.asyncio
    async def test_up_arrow_moves_to_previous_card(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            # Move down first, then up
            await pilot.press("down")
            await pilot.pause()

            focused = screen.focused
            assert isinstance(focused, ChoiceCard)
            assert focused.card_id == "choice-1"

            await pilot.press("up")
            await pilot.pause()

            focused = screen.focused
            assert isinstance(focused, ChoiceCard)
            assert focused.card_id == "choice-0"

    @pytest.mark.asyncio
    async def test_navigate_through_all_cards(self):
        app = PlanReviewApp()
        async with app.run_test() as pilot:
            screen = app.screen
            assert isinstance(screen, PlanReviewScreen)
            await pilot.pause()

            # Navigate down through all 3 cards
            for expected_id in ["choice-0", "choice-1", "choice-2"]:
                focused = screen.focused
                assert isinstance(focused, ChoiceCard)
                assert focused.card_id == expected_id
                if expected_id != "choice-2":
                    await pilot.press("down")
                    await pilot.pause()
