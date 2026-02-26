"""Tests for the Rich-based inline menu functions."""

from __future__ import annotations

from unittest.mock import patch

import pytest

from zing_ai.orchestrator.commands.build_audit import Finding, FindingGroup
from zing_ai.orchestrator.models import Choice, ChoiceSet
from zing_ai.orchestrator.ui.menus import (
    audit_triage_menu,
    numbered_menu,
    plan_review_menu,
)
from zing_ai.orchestrator.ui.types import MenuOption


# ---------------------------------------------------------------------------
# numbered_menu
# ---------------------------------------------------------------------------


class TestNumberedMenu:
    """Tests for :func:`numbered_menu`."""

    def _options(self) -> list[MenuOption]:
        return [
            MenuOption(label="Option A", description="First option"),
            MenuOption(label="Option B", description="Second option"),
            MenuOption(label="Option C", description="Third option"),
        ]

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask", return_value="1")
    def test_valid_selection_first(self, mock_ask: object) -> None:
        result = numbered_menu("Pick one", self._options())
        assert result == 0

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask", return_value="3")
    def test_valid_selection_last(self, mock_ask: object) -> None:
        result = numbered_menu("Pick one", self._options())
        assert result == 2

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask", side_effect=["bad", "0", "2"])
    def test_invalid_input_triggers_reprompt(self, mock_ask: object) -> None:
        """Non-numeric and out-of-range values should re-prompt until valid."""
        result = numbered_menu("Pick one", self._options())
        assert result == 1  # "2" -> index 1

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask", return_value="2")
    def test_recommended_option_display(
        self, mock_ask: object, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """When *recommended* is set, '(recommended)' should appear in output."""
        with patch("zing_ai.orchestrator.ui.menus.console") as mock_console:
            numbered_menu("Pick one", self._options(), recommended=1)
            # Check that the Panel printed contains "(recommended)"
            printed_calls = mock_console.print.call_args_list
            # The first call is the Panel
            panel_call = printed_calls[0]
            panel_arg = panel_call[0][0]
            # The panel renderable is a Panel; extract its renderable text
            assert "(recommended)" in panel_arg.renderable

    @patch(
        "zing_ai.orchestrator.ui.menus.Prompt.ask",
        side_effect=["5", "-1", "abc", "1"],
    )
    def test_out_of_range_reprompts(self, mock_ask: object) -> None:
        """Out-of-range numbers and non-numeric input should re-prompt."""
        result = numbered_menu("Pick one", self._options())
        assert result == 0  # "1" -> index 0


# ---------------------------------------------------------------------------
# plan_review_menu
# ---------------------------------------------------------------------------


class TestPlanReviewMenu:
    """Tests for :func:`plan_review_menu`."""

    def _choice_sets(self) -> list[ChoiceSet]:
        return [
            ChoiceSet(
                message="Architecture",
                explanation="Choose an architecture pattern",
                choices=[
                    Choice(label="Monolith", description="Single deployment", recommended=True),
                    Choice(label="Microservices", description="Distributed", recommended=False),
                ],
            ),
            ChoiceSet(
                message="Database",
                explanation="Choose a database",
                choices=[
                    Choice(label="PostgreSQL", description="Relational", recommended=False),
                    Choice(label="MongoDB", description="Document store", recommended=True),
                ],
            ),
        ]

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask")
    def test_approve_flow(self, mock_ask: object) -> None:
        """Selecting recommended options then 'Approve' returns approve action."""
        # For choice_set 1: select "1" (Monolith, recommended)
        # For choice_set 2: select "2" (MongoDB, recommended)
        # For summary: select "1" (Approve & Build)
        with patch(
            "zing_ai.orchestrator.ui.menus.Prompt.ask",
            side_effect=["1", "2", "1"],
        ):
            action, changes = plan_review_menu(self._choice_sets())

        assert action == "approve"
        assert len(changes) == 2
        assert changes[0]["choice_set_id"] == "Architecture"
        assert changes[0]["selected_index"] == 0
        assert changes[1]["choice_set_id"] == "Database"
        assert changes[1]["selected_index"] == 1

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask")
    def test_replan_flow(self, mock_ask: object) -> None:
        """Selecting different options then 'Request Replan' returns replan action."""
        # For choice_set 1: select "2" (Microservices)
        # For choice_set 2: select "1" (PostgreSQL)
        # For summary: select "2" (Request Replan)
        with patch(
            "zing_ai.orchestrator.ui.menus.Prompt.ask",
            side_effect=["2", "1", "2"],
        ):
            action, changes = plan_review_menu(self._choice_sets())

        assert action == "replan"
        assert len(changes) == 2
        assert changes[0]["selected_index"] == 1  # Microservices
        assert changes[1]["selected_index"] == 0  # PostgreSQL


# ---------------------------------------------------------------------------
# audit_triage_menu
# ---------------------------------------------------------------------------


class TestAuditTriageMenu:
    """Tests for :func:`audit_triage_menu`."""

    def _finding_groups(self) -> list[FindingGroup]:
        return [
            FindingGroup(
                severity="high",
                findings=[
                    Finding(
                        index=0,
                        category="security",
                        severity="high",
                        confidence="high",
                        location="src/app.py:10",
                        title="SQL Injection",
                    ),
                    Finding(
                        index=1,
                        category="security",
                        severity="high",
                        confidence="medium",
                        location="src/auth.py:25",
                        title="Weak Password Hash",
                    ),
                ],
            ),
            FindingGroup(
                severity="low",
                findings=[
                    Finding(
                        index=2,
                        category="style",
                        severity="low",
                        confidence="high",
                        location="src/utils.py:5",
                        title="Unused Import",
                    ),
                ],
            ),
        ]

    @patch("zing_ai.orchestrator.ui.menus.Prompt.ask")
    def test_returns_matching_decisions(self, mock_ask: object) -> None:
        """Each finding gets the action the user selected."""
        # Finding 0: Fix (select "1")
        # Finding 1: Skip (select "2")
        # Finding 2: Discuss (select "3")
        with patch(
            "zing_ai.orchestrator.ui.menus.Prompt.ask",
            side_effect=["1", "2", "3"],
        ):
            decisions = audit_triage_menu(self._finding_groups())

        assert len(decisions) == 3

        assert decisions[0]["finding_index"] == 0
        assert decisions[0]["action"] == "fix"
        assert decisions[0]["severity"] == "high"
        assert decisions[0]["category"] == "security"
        assert decisions[0]["title"] == "SQL Injection"

        assert decisions[1]["finding_index"] == 1
        assert decisions[1]["action"] == "skip"

        assert decisions[2]["finding_index"] == 2
        assert decisions[2]["action"] == "discuss"
        assert decisions[2]["severity"] == "low"


# ---------------------------------------------------------------------------
# KeyboardInterrupt handling
# ---------------------------------------------------------------------------


class TestKeyboardInterrupt:
    """Test that KeyboardInterrupt during prompts raises SystemExit(130)."""

    @patch(
        "zing_ai.orchestrator.ui.menus.Prompt.ask",
        side_effect=KeyboardInterrupt,
    )
    def test_numbered_menu_keyboard_interrupt(self, mock_ask: object) -> None:
        with pytest.raises(SystemExit) as exc_info:
            numbered_menu(
                "Pick one",
                [MenuOption(label="A", description="a")],
            )
        assert exc_info.value.code == 130

    @patch(
        "zing_ai.orchestrator.ui.menus.Prompt.ask",
        side_effect=KeyboardInterrupt,
    )
    def test_plan_review_keyboard_interrupt(self, mock_ask: object) -> None:
        cs = ChoiceSet(
            message="Test",
            explanation="Test explanation",
            choices=[
                Choice(label="Only", description="Only choice", recommended=True),
            ],
        )
        with pytest.raises(SystemExit) as exc_info:
            plan_review_menu([cs])
        assert exc_info.value.code == 130

    @patch(
        "zing_ai.orchestrator.ui.menus.Prompt.ask",
        side_effect=KeyboardInterrupt,
    )
    def test_audit_triage_keyboard_interrupt(self, mock_ask: object) -> None:
        groups = [
            FindingGroup(
                severity="high",
                findings=[
                    Finding(
                        index=0,
                        category="security",
                        severity="high",
                        confidence="high",
                        location="src/app.py:1",
                        title="Issue",
                    ),
                ],
            ),
        ]
        with pytest.raises(SystemExit) as exc_info:
            audit_triage_menu(groups)
        assert exc_info.value.code == 130
