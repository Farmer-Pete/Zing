"""Tests for the orchestrator pipeline controller."""

from __future__ import annotations

import asyncio
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

from zing_ai.orchestrator.config import ZingConfig
from zing_ai.orchestrator.pipeline import STAGES, run_pipeline


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _run(coro):  # type: ignore[no-untyped-def]
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


def _make_kwargs(tmp_path: Path) -> dict:
    """Build the common keyword arguments for ``run_pipeline``."""
    return dict(
        zing_file="test.xml",
        no_browser=True,
        skip_permissions=True,
        config=ZingConfig(),
        project_root=tmp_path,
    )


# ---------------------------------------------------------------------------
# STAGES constant
# ---------------------------------------------------------------------------


class TestStages:
    """Tests for the STAGES constant."""

    def test_stages_contains_all_six(self) -> None:
        assert STAGES == ("new", "plan", "plan-audit", "plan-review", "build", "build-audit")

    def test_stages_length(self) -> None:
        assert len(STAGES) == 6


# ---------------------------------------------------------------------------
# Invalid stage
# ---------------------------------------------------------------------------


class TestInvalidStage:
    """Tests for invalid start_stage values."""

    def test_invalid_stage_raises_value_error(self, tmp_path: Path) -> None:
        with pytest.raises(ValueError, match="Invalid start_stage 'bogus'"):
            _run(run_pipeline("bogus", **_make_kwargs(tmp_path)))

    def test_empty_stage_raises_value_error(self, tmp_path: Path) -> None:
        with pytest.raises(ValueError, match="Invalid start_stage"):
            _run(run_pipeline("", **_make_kwargs(tmp_path)))

    def test_error_message_lists_valid_stages(self, tmp_path: Path) -> None:
        with pytest.raises(ValueError, match="new, plan, plan-audit"):
            _run(run_pipeline("invalid", **_make_kwargs(tmp_path)))


# ---------------------------------------------------------------------------
# Dispatch tests — one per stage
# ---------------------------------------------------------------------------


class TestDispatchNew:
    """Verify ``run_pipeline('new', ...)`` dispatches to ``run_new``."""

    @patch("zing_ai.orchestrator.commands.new.run_new", new_callable=AsyncMock)
    def test_dispatches_to_run_new(self, mock_run_new: AsyncMock, tmp_path: Path) -> None:
        _run(run_pipeline("new", **_make_kwargs(tmp_path)))

        mock_run_new.assert_awaited_once()
        call_kwargs = mock_run_new.call_args.kwargs
        assert call_kwargs["zing_file"] == "test.xml"
        assert call_kwargs["no_browser"] is True
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["project_root"] == tmp_path
        assert isinstance(call_kwargs["config"], ZingConfig)


class TestDispatchPlan:
    """Verify ``run_pipeline('plan', ...)`` dispatches to ``run_plan``."""

    @patch("zing_ai.orchestrator.commands.plan.run_plan", new_callable=AsyncMock)
    def test_dispatches_to_run_plan(self, mock_run_plan: AsyncMock, tmp_path: Path) -> None:
        _run(run_pipeline("plan", **_make_kwargs(tmp_path)))

        mock_run_plan.assert_awaited_once()
        call_kwargs = mock_run_plan.call_args.kwargs
        assert call_kwargs["zing_file"] == "test.xml"
        assert call_kwargs["no_browser"] is True
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["project_root"] == tmp_path


class TestDispatchPlanAudit:
    """Verify ``run_pipeline('plan-audit', ...)`` dispatches to ``run_plan_audit``."""

    @patch("zing_ai.orchestrator.commands.plan_audit.run_plan_audit", new_callable=AsyncMock)
    def test_dispatches_to_run_plan_audit(
        self, mock_run_plan_audit: AsyncMock, tmp_path: Path
    ) -> None:
        _run(run_pipeline("plan-audit", **_make_kwargs(tmp_path)))

        mock_run_plan_audit.assert_awaited_once()
        # zing_file is passed as a positional arg for plan_audit
        call_args = mock_run_plan_audit.call_args
        assert call_args.args[0] == "test.xml"
        assert call_args.kwargs["no_browser"] is True
        assert call_args.kwargs["skip_permissions"] is True
        assert call_args.kwargs["project_root"] == tmp_path

    @patch("zing_ai.orchestrator.commands.plan_audit.run_plan_audit", new_callable=AsyncMock)
    def test_plan_audit_with_none_zing_file(
        self, mock_run_plan_audit: AsyncMock, tmp_path: Path
    ) -> None:
        """When zing_file is None, plan-audit receives empty string."""
        kwargs = _make_kwargs(tmp_path)
        kwargs["zing_file"] = None
        _run(run_pipeline("plan-audit", **kwargs))

        mock_run_plan_audit.assert_awaited_once()
        call_args = mock_run_plan_audit.call_args
        assert call_args.args[0] == ""


class TestDispatchPlanReview:
    """Verify ``run_pipeline('plan-review', ...)`` dispatches to ``run_plan_review``."""

    @patch("zing_ai.orchestrator.commands.plan_review.run_plan_review", new_callable=AsyncMock)
    def test_dispatches_to_run_plan_review(
        self, mock_run_plan_review: AsyncMock, tmp_path: Path
    ) -> None:
        _run(run_pipeline("plan-review", **_make_kwargs(tmp_path)))

        mock_run_plan_review.assert_awaited_once()
        call_kwargs = mock_run_plan_review.call_args.kwargs
        assert call_kwargs["zing_file"] == "test.xml"
        assert call_kwargs["no_browser"] is True


class TestDispatchBuild:
    """Verify ``run_pipeline('build', ...)`` dispatches to ``run_build``."""

    @patch("zing_ai.orchestrator.commands.build.run_build", new_callable=AsyncMock)
    def test_dispatches_to_run_build(self, mock_run_build: AsyncMock, tmp_path: Path) -> None:
        _run(run_pipeline("build", **_make_kwargs(tmp_path)))

        mock_run_build.assert_awaited_once()
        call_kwargs = mock_run_build.call_args.kwargs
        assert call_kwargs["zing_file"] == "test.xml"
        assert call_kwargs["no_browser"] is True
        assert call_kwargs["skip_permissions"] is True


class TestDispatchBuildAudit:
    """Verify ``run_pipeline('build-audit', ...)`` dispatches to ``run_build_audit``."""

    @patch("zing_ai.orchestrator.commands.build_audit.run_build_audit", new_callable=AsyncMock)
    def test_dispatches_to_run_build_audit(
        self, mock_run_build_audit: AsyncMock, tmp_path: Path
    ) -> None:
        _run(run_pipeline("build-audit", **_make_kwargs(tmp_path)))

        mock_run_build_audit.assert_awaited_once()
        call_kwargs = mock_run_build_audit.call_args.kwargs
        assert call_kwargs["zing_file"] == "test.xml"
        assert call_kwargs["no_browser"] is True
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["project_root"] == tmp_path


# ---------------------------------------------------------------------------
# Edge cases
# ---------------------------------------------------------------------------


class TestEdgeCases:
    """Miscellaneous edge-case tests."""

    @patch("zing_ai.orchestrator.commands.new.run_new", new_callable=AsyncMock)
    def test_zing_file_none_for_new(self, mock_run_new: AsyncMock, tmp_path: Path) -> None:
        """``new`` stage should accept zing_file=None."""
        kwargs = _make_kwargs(tmp_path)
        kwargs["zing_file"] = None
        _run(run_pipeline("new", **kwargs))

        mock_run_new.assert_awaited_once()
        assert mock_run_new.call_args.kwargs["zing_file"] is None

    @patch("zing_ai.orchestrator.commands.build.run_build", new_callable=AsyncMock)
    def test_config_is_forwarded(self, mock_run_build: AsyncMock, tmp_path: Path) -> None:
        """Verify the config object is passed through to the command."""
        custom_config = ZingConfig(port=9999)
        _run(
            run_pipeline(
                "build",
                zing_file="test.xml",
                no_browser=False,
                skip_permissions=False,
                config=custom_config,
                project_root=tmp_path,
            )
        )

        mock_run_build.assert_awaited_once()
        assert mock_run_build.call_args.kwargs["config"] is custom_config
        assert mock_run_build.call_args.kwargs["config"].port == 9999
