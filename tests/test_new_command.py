"""Tests for the orchestrator ``new`` command."""

from __future__ import annotations

import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from zing_ai.orchestrator.commands.new import (
    _extract_project_name,
    _to_kebab_case,
    run_new,
)
from zing_ai.orchestrator.config import CallType, ZingConfig


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


SAMPLE_MARKDOWN = """\
# Recipe App

## Overview
A web application for managing recipes.

## Features
- Add recipes
- Search recipes
- Share recipes with friends
"""

SAMPLE_MARKDOWN_NO_HEADING = """\
This is a project about weather forecasting.

It should track temperature and humidity.
"""

SAMPLE_MARKDOWN_H2_ONLY = """\
Some intro text.

## My Dashboard Project

Details here.
"""


# ---------------------------------------------------------------------------
# _to_kebab_case tests
# ---------------------------------------------------------------------------


class TestToKebabCase:
    """Tests for the kebab-case conversion helper."""

    def test_simple_two_words(self) -> None:
        assert _to_kebab_case("Recipe App") == "recipe-app"

    def test_multiple_words(self) -> None:
        assert _to_kebab_case("My Cool Project") == "my-cool-project"

    def test_strips_special_characters(self) -> None:
        assert _to_kebab_case("My Cool Project!!!") == "my-cool-project"

    def test_collapses_whitespace(self) -> None:
        assert _to_kebab_case("  hello   world  ") == "hello-world"

    def test_preserves_existing_hyphens(self) -> None:
        assert _to_kebab_case("already-kebab") == "already-kebab"

    def test_collapses_multiple_hyphens(self) -> None:
        assert _to_kebab_case("foo---bar") == "foo-bar"

    def test_strips_leading_trailing_hyphens(self) -> None:
        assert _to_kebab_case("-hello-") == "hello"

    def test_mixed_case(self) -> None:
        assert _to_kebab_case("FooBar BAZ") == "foobar-baz"

    def test_numbers_preserved(self) -> None:
        assert _to_kebab_case("Project 42") == "project-42"

    def test_all_special_chars(self) -> None:
        assert _to_kebab_case("@#$%^&") == ""

    def test_single_word(self) -> None:
        assert _to_kebab_case("hello") == "hello"


# ---------------------------------------------------------------------------
# _extract_project_name tests
# ---------------------------------------------------------------------------


class TestExtractProjectName:
    """Tests for extracting the project name from markdown."""

    def test_extracts_from_h1_heading(self) -> None:
        assert _extract_project_name("# Recipe App\n\nSome text.") == "recipe-app"

    def test_extracts_from_h2_heading(self) -> None:
        assert _extract_project_name("## My Dashboard\n\nDetails.") == "my-dashboard"

    def test_extracts_from_h3_heading(self) -> None:
        assert _extract_project_name("### Tiny Tool\n") == "tiny-tool"

    def test_prefers_first_heading(self) -> None:
        md = "# First Project\n\n## Second Project\n"
        assert _extract_project_name(md) == "first-project"

    def test_skips_empty_lines_before_heading(self) -> None:
        md = "\n\n\n# Delayed Heading\n"
        assert _extract_project_name(md) == "delayed-heading"

    def test_fallback_to_first_line_when_no_heading(self) -> None:
        md = "Weather Forecasting Tool\n\nMore details.\n"
        assert _extract_project_name(md) == "weather-forecasting-tool"

    def test_fallback_skips_empty_lines(self) -> None:
        md = "\n\n\nActual Content\n"
        assert _extract_project_name(md) == "actual-content"

    def test_raises_on_empty_string(self) -> None:
        with pytest.raises(ValueError, match="empty markdown"):
            _extract_project_name("")

    def test_raises_on_whitespace_only(self) -> None:
        with pytest.raises(ValueError, match="empty markdown"):
            _extract_project_name("   \n\n   \n")

    def test_sample_markdown(self) -> None:
        assert _extract_project_name(SAMPLE_MARKDOWN) == "recipe-app"

    def test_sample_no_heading(self) -> None:
        assert (
            _extract_project_name(SAMPLE_MARKDOWN_NO_HEADING)
            == "this-is-a-project-about-weather-forecasting"
        )

    def test_h2_only_with_preceding_text(self) -> None:
        """H2 is found before the fallback first line."""
        assert _extract_project_name(SAMPLE_MARKDOWN_H2_ONLY) == "my-dashboard-project"

    def test_heading_with_special_chars(self) -> None:
        md = "# Recipe App (v2.0)!\n"
        assert _extract_project_name(md) == "recipe-app-v2-0"


# ---------------------------------------------------------------------------
# run_new integration tests (with mocked Claude subprocess)
# ---------------------------------------------------------------------------


class TestRunNew:
    """Tests for the full run_new() flow with mocked dependencies."""

    def test_full_flow_creates_zing_file_and_calls_plan(self, tmp_path: Path) -> None:
        """The happy path: Claude returns markdown, zing file is created, plan is called."""
        config = ZingConfig()
        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=(SAMPLE_MARKDOWN, "sess-001"),
            ) as mock_claude,
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="rendered prompt",
            ) as mock_render,
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

            # Verify prompt was rendered
            mock_render.assert_called_once_with("new.md.j2")

            # Verify Claude was invoked with the rendered prompt
            mock_claude.assert_called_once_with(
                "rendered prompt",
                call_type=CallType.INVESTIGATE,
                config=config,
                skip_permissions=False,
            )

        # Verify .zing directory was created
        assert (tmp_path / ".zing").is_dir()

        # Verify the zing file was created with correct name
        zing_file_path = tmp_path / ".zing" / "recipe-app.xml"
        assert zing_file_path.is_file()

        # Verify the zing file content
        tree = ET.parse(zing_file_path)
        root = tree.getroot()
        assert root.tag == "zing"
        assert root.get("stage") == "new"
        assert root.get("audit") == "false"
        assert root.get("approved") == "false"
        content_elem = root.find("content")
        assert content_elem is not None
        assert content_elem.text is not None
        assert "Recipe App" in content_elem.text

        # Verify run_plan was called with the correct arguments
        mock_run_plan.assert_called_once_with(
            zing_file="recipe-app.xml",
            skip_permissions=False,
            config=config,
            project_root=tmp_path,
        )

    def test_ensures_zing_dir_exists(self, tmp_path: Path) -> None:
        """run_new creates .zing/ if it doesn't exist."""
        config = ZingConfig()

        assert not (tmp_path / ".zing").exists()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=(SAMPLE_MARKDOWN, "sess-001"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert (tmp_path / ".zing").is_dir()

    def test_skip_permissions_passed_to_claude(self, tmp_path: Path) -> None:
        """The skip_permissions flag is forwarded to Claude."""
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=(SAMPLE_MARKDOWN, "sess-001"),
            ) as mock_claude,
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

            mock_claude.assert_called_once_with(
                "prompt",
                call_type=CallType.INVESTIGATE,
                config=config,
                skip_permissions=True,
            )

    def test_markdown_without_heading_uses_first_line(self, tmp_path: Path) -> None:
        """When Claude returns markdown without a heading, use the first line for the filename."""
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=(SAMPLE_MARKDOWN_NO_HEADING, "sess-002"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        expected_name = "this-is-a-project-about-weather-forecasting.xml"
        assert (tmp_path / ".zing" / expected_name).is_file()

    def test_zing_file_has_correct_stage(self, tmp_path: Path) -> None:
        """The created zing file should have stage='new'."""
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=("# Test Project\n\nContent here.", "sess-003"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        zing_path = tmp_path / ".zing" / "test-project.xml"
        assert zing_path.is_file()
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("stage") == "new"

    def test_plan_receives_zing_filename(self, tmp_path: Path) -> None:
        """run_plan is called with just the filename (not full path)."""
        config = ZingConfig()
        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=("# My Feature\n\nDetails.", "sess-004"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        call_kwargs = mock_run_plan.call_args.kwargs
        assert call_kwargs["zing_file"] == "my-feature.xml"

    def test_existing_zing_dir_is_reused(self, tmp_path: Path) -> None:
        """If .zing/ already exists, it is not recreated."""
        config = ZingConfig()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        # Place a marker file to verify the directory isn't replaced
        (zing_dir / "existing.txt").write_text("marker")

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=("# Existing Dir Test\n", "sess-005"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Marker file should still exist
        assert (zing_dir / "existing.txt").read_text() == "marker"
        # New zing file should also exist
        assert (zing_dir / "existing-dir-test.xml").is_file()

    def test_uses_investigate_call_type(self, tmp_path: Path) -> None:
        """The new command should use CallType.INVESTIGATE for the Claude call."""
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.claude.invoke_claude_full",
                return_value=("# Test\n", "sess-006"),
            ) as mock_claude,
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

            call_kwargs = mock_claude.call_args.kwargs
            assert call_kwargs["call_type"] == CallType.INVESTIGATE
