"""Tests for the orchestrator ``plan-review`` command."""

from __future__ import annotations

import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from zing_ai.orchestrator.commands.plan_review import run_plan_review
from zing_ai.orchestrator.config import ZingConfig
from zing_ai.orchestrator.models import (
    Choice,
    ChoiceSet,
    Interaction,
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.tui.results import ReviewResult
from zing_ai.orchestrator.xml_parser import write_zing_file

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_choice_sets() -> list[ChoiceSet]:
    """Create a list of sample choice sets for testing."""
    return [
        ChoiceSet(
            message="Which database?",
            explanation="We need to choose a database.",
            choices=[
                Choice(label="PostgreSQL", description="Relational DB", recommended=True),
                Choice(label="MongoDB", description="Document DB", recommended=False),
                Choice(label="SQLite", description="Embedded DB", recommended=False),
            ],
        ),
        ChoiceSet(
            message="Which framework?",
            explanation="We need to choose a framework.",
            choices=[
                Choice(label="FastAPI", description="Modern async", recommended=True),
                Choice(label="Flask", description="Classic", recommended=False),
            ],
        ),
    ]


def _make_zing_file_with_choices(
    tmp_path: Path,
    *,
    choice_sets: list[ChoiceSet] | None = None,
    plan_session: str = "sess-plan-001",
) -> Path:
    """Create a zing file with plan and interactions."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"

    plan = Plan(stages=[
        Stage(label="stage-1", steps=[
            Step(label="step-1", instructions="Do something", files=["src/main.py"], done=False),
        ]),
    ])

    if choice_sets is None:
        choice_sets = _make_choice_sets()

    interaction = Interaction(choice_sets=choice_sets)

    doc = ZingDocument(
        stage="plan",
        content="# Test Project\n\nA test project.",
        plan=plan,
        interactions=interaction,
        audit=True,
        approved=False,
        plan_session=plan_session,
        audit_session="sess-audit-001",
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_zing_file_no_choices(tmp_path: Path) -> Path:
    """Create a zing file without any interactions."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"

    plan = Plan(stages=[
        Stage(label="stage-1", steps=[
            Step(label="step-1", instructions="Do something", files=["src/main.py"], done=False),
        ]),
    ])

    doc = ZingDocument(
        stage="plan",
        content="# Test Project\n\nA test project.",
        plan=plan,
        interactions=None,
        audit=True,
        approved=False,
    )
    write_zing_file(zing_path, doc)
    return zing_path


# ---------------------------------------------------------------------------
# ReviewState removed
# ---------------------------------------------------------------------------


class TestReviewStateRemoved:
    """Verify that the legacy ReviewState class has been removed."""

    def test_review_state_not_importable(self) -> None:
        """ReviewState should no longer be exported from plan_review."""
        import zing_ai.orchestrator.commands.plan_review as mod

        assert not hasattr(mod, "ReviewState"), (
            "ReviewState should be removed from plan_review module"
        )


# ---------------------------------------------------------------------------
# run_plan_review approval tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewApproval:
    """Tests for the approval flow (no modifications)."""

    def test_approval_sets_approved_true(self, tmp_path: Path) -> None:
        """Approve with no changes -> writes approved=True to zing file."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_build = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="approve", changes=[]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
                mock_build,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Verify approved=True in the zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "true"

        # Verify run_build was called
        mock_build.assert_called_once()

    def test_approval_calls_build_with_correct_args(self, tmp_path: Path) -> None:
        """Approved plan calls _call_build with correct arguments."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_build = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="approve", changes=[]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
                mock_build,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        call_kwargs = mock_build.call_args.kwargs
        assert call_kwargs["zing_path"] == zing_path
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["config"] is config
        assert call_kwargs["project_root"] == tmp_path

    def test_run_with_screen_called_with_plan_review_screen(self, tmp_path: Path) -> None:
        """ZingApp.run_with_screen should be called with a PlanReviewScreen."""
        _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()

        mock_run = MagicMock(
            return_value=ReviewResult(action="approve", changes=[]),
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                mock_run,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
                MagicMock(),
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_run.assert_called_once()
        from zing_ai.orchestrator.tui.screens.plan_review import PlanReviewScreen

        screen_arg = mock_run.call_args.args[0]
        assert isinstance(screen_arg, PlanReviewScreen)


# ---------------------------------------------------------------------------
# run_plan_review replan tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewReplan:
    """Tests for the replan flow (user modifies choices)."""

    def test_replan_calls_replan_with_changes(self, tmp_path: Path) -> None:
        """Replan action -> calls _call_replan with the changes from ReviewResult."""
        _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = MagicMock()

        changes = [
            {"choice_id": "card-0", "new_selection": 1},
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="replan", changes=changes),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
                mock_replan,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_replan.assert_called_once()
        call_kwargs = mock_replan.call_args.kwargs
        assert call_kwargs["replan_changes"] == changes

    def test_replan_with_deletion(self, tmp_path: Path) -> None:
        """Replan with a deletion -> calls _call_replan with deleted change."""
        _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = MagicMock()

        changes = [
            {"choice_id": "card-1", "new_selection": None},
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="replan", changes=changes),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
                mock_replan,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_replan.assert_called_once()
        call_kwargs = mock_replan.call_args.kwargs
        changes_result = call_kwargs["replan_changes"]
        assert len(changes_result) == 1
        assert changes_result[0]["new_selection"] is None

    def test_replan_does_not_set_approved(self, tmp_path: Path) -> None:
        """Replan should NOT set approved=True in the zing file."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()

        changes = [
            {"choice_id": "card-0", "new_selection": 1},
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="replan", changes=changes),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
                MagicMock(),
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # The zing file should NOT have approved=True (modifications go through replan)
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "false"

    def test_replan_called_with_correct_args(self, tmp_path: Path) -> None:
        """Verify _call_replan receives all expected arguments."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = MagicMock()

        changes = [
            {"choice_id": "card-0", "new_selection": 2},
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="replan", changes=changes),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
                mock_replan,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        call_kwargs = mock_replan.call_args.kwargs
        assert call_kwargs["zing_path"] == zing_path
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["config"] is config
        assert call_kwargs["project_root"] == tmp_path
        assert len(call_kwargs["replan_changes"]) == 1

    def test_replan_with_multiple_changes(self, tmp_path: Path) -> None:
        """Multiple changes in ReviewResult are all passed through."""
        _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = MagicMock()

        changes = [
            {"choice_id": "card-0", "new_selection": 1},
            {"choice_id": "card-1", "new_selection": None},
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=ReviewResult(action="replan", changes=changes),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
                mock_replan,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        call_kwargs = mock_replan.call_args.kwargs
        assert len(call_kwargs["replan_changes"]) == 2


# ---------------------------------------------------------------------------
# run_plan_review no-choices tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewNoChoices:
    """Tests for when the zing document has no choices."""

    def test_no_choices_auto_approves(self, tmp_path: Path) -> None:
        """When no choices exist, the plan is auto-approved."""
        zing_path = _make_zing_file_no_choices(tmp_path)
        config = ZingConfig()
        mock_build = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
                mock_build,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Verify approved=True in the zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "true"

        # Verify build was called
        mock_build.assert_called_once()

    def test_no_choices_does_not_launch_tui(self, tmp_path: Path) -> None:
        """When no choices exist, the TUI should not be launched."""
        _make_zing_file_no_choices(tmp_path)
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                side_effect=AssertionError("TUI should not be launched"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
                MagicMock(),
            ),
        ):
            # Should not raise -- TUI should never be launched
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )


# ---------------------------------------------------------------------------
# run_plan_review cancellation tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewCancellation:
    """Tests for when the user cancels (closes the TUI without deciding)."""

    def test_none_result_no_action(self, tmp_path: Path) -> None:
        """When run_with_screen returns None, no build or replan is called."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_build = MagicMock()
        mock_replan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.ZingApp.run_with_screen",
                return_value=None,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
                mock_build,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
                mock_replan,
            ),
        ):
            run_plan_review(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_build.assert_not_called()
        mock_replan.assert_not_called()

        # Verify zing file was NOT modified
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "false"
