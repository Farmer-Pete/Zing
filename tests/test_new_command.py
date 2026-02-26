"""Tests for the orchestrator ``new`` command."""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock, call, patch

import pytest

from zing_ai.orchestrator.commands.new import run_new
from zing_ai.orchestrator.config import ZingConfig


# ---------------------------------------------------------------------------
# run_new tests (with mocked subprocess and filesystem)
# ---------------------------------------------------------------------------


class TestRunNew:
    """Tests for run_new() with direct subprocess.run invocation."""

    def _make_config(self) -> ZingConfig:
        return ZingConfig()

    def test_subprocess_called_with_correct_args(self, tmp_path: Path) -> None:
        """subprocess.run is called with ['claude', '--system-prompt', <prompt>]."""
        config = self._make_config()
        # Pre-create a .xml file so the flow finds it after Claude exits
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        (zing_dir / "my-project.xml").write_text("<zing/>")

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ) as mock_run,
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="rendered system prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

            mock_run.assert_called_once_with(
                ["claude", "--system-prompt", "rendered system prompt", "Greet the user"],
                stdin=sys.stdin,
                stdout=sys.stdout,
                stderr=sys.stderr,
            )

    def test_inherited_stdio(self, tmp_path: Path) -> None:
        """subprocess.run receives stdin, stdout, stderr from sys."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        (zing_dir / "test.xml").write_text("<zing/>")

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ) as mock_run,
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

            call_kwargs = mock_run.call_args
            assert call_kwargs.kwargs["stdin"] is sys.stdin
            assert call_kwargs.kwargs["stdout"] is sys.stdout
            assert call_kwargs.kwargs["stderr"] is sys.stderr

    def test_system_prompt_rendered_from_template(self, tmp_path: Path) -> None:
        """The system prompt passed to Claude comes from render_prompt('new.md.j2')."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        (zing_dir / "project.xml").write_text("<zing/>")

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ) as mock_run,
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="my custom system prompt",
            ) as mock_render,
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

            mock_render.assert_called_once_with("new.md.j2")
            # The system prompt is the third element of the command list
            cmd_args = mock_run.call_args.args[0]
            assert cmd_args == ["claude", "--system-prompt", "my custom system prompt", "Greet the user"]

    def test_ensures_zing_dir_exists(self, tmp_path: Path) -> None:
        """run_new creates .zing/ if it doesn't exist."""
        config = self._make_config()
        assert not (tmp_path / ".zing").exists()

        # We need to create the xml file *after* .zing/ is created by run_new
        # but before the glob scan. We use a side_effect on subprocess.run
        # to create the file.
        def create_xml_on_call(*args: object, **kwargs: object) -> None:
            zing_dir = tmp_path / ".zing"
            (zing_dir / "new-project.xml").write_text("<zing/>")

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
                side_effect=create_xml_on_call,
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert (tmp_path / ".zing").is_dir()

    def test_finds_newest_xml_file(self, tmp_path: Path) -> None:
        """After Claude exits, the newest .xml file by mtime is selected."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        # Create two xml files; the newer one should be picked
        import time

        older = zing_dir / "old-project.xml"
        older.write_text("<zing/>")
        # Ensure mtime difference
        time.sleep(0.05)
        newer = zing_dir / "new-project.xml"
        newer.write_text("<zing/>")

        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        call_kwargs = mock_run_plan.call_args.kwargs
        assert call_kwargs["zing_file"] == "new-project.xml"

    def test_auto_chains_to_run_plan(self, tmp_path: Path) -> None:
        """After Claude exits and a .xml file is found, run_plan is called."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        (zing_dir / "my-feature.xml").write_text("<zing/>")

        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_run_plan.assert_called_once_with(
            zing_file="my-feature.xml",
            skip_permissions=False,
            config=config,
            project_root=tmp_path,
        )

    def test_run_plan_receives_skip_permissions(self, tmp_path: Path) -> None:
        """The skip_permissions flag is forwarded to run_plan."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        (zing_dir / "project.xml").write_text("<zing/>")

        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        call_kwargs = mock_run_plan.call_args.kwargs
        assert call_kwargs["skip_permissions"] is True

    def test_no_xml_file_prints_error(self, tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
        """If no .xml file exists after Claude exits, print an error message."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        # No .xml files in .zing/

        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # run_plan should NOT have been called
        mock_run_plan.assert_not_called()

        # Error message should be printed
        captured = capsys.readouterr()
        assert "No valid zing file was created" in captured.out
        assert "zing-ai new" in captured.out

    def test_no_xml_file_does_not_chain_to_plan(self, tmp_path: Path) -> None:
        """If no .xml file is found, run_plan is not called."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        mock_run_plan = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                mock_run_plan,
            ),
        ):
            run_new(
                zing_file=None,
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_run_plan.assert_not_called()

    def test_existing_zing_dir_is_reused(self, tmp_path: Path) -> None:
        """If .zing/ already exists, it is not recreated."""
        config = self._make_config()
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()
        # Place a marker file to verify the directory isn't replaced
        (zing_dir / "existing.txt").write_text("marker")
        (zing_dir / "project.xml").write_text("<zing/>")

        with (
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
            ),
            patch(
                "zing_ai.orchestrator.commands.new.render_prompt",
                return_value="prompt",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
                MagicMock(),
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
