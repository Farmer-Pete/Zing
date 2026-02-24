"""Tests for the zing-ai CLI (click-based)."""

from __future__ import annotations

from unittest.mock import patch

import click
import pytest
from click.testing import CliRunner

from zing_ai.cli import _resolve_runtimes, cli

# -- CLI structure -----------------------------------------------------------


def test_no_args_prints_help():
    runner = CliRunner()
    result = runner.invoke(cli, [])
    assert result.exit_code == 0
    assert "Usage" in result.output


def test_version_flag():
    runner = CliRunner()
    result = runner.invoke(cli, ["--version"])
    assert result.exit_code == 0
    assert "zing-ai" in result.output


# -- install subcommand -----------------------------------------------------


def test_install_help():
    runner = CliRunner()
    result = runner.invoke(cli, ["install", "--help"])
    assert result.exit_code == 0
    assert "--claude" in result.output
    assert "--opencode" in result.output
    assert "--all" in result.output


def test_install_claude():
    runner = CliRunner()
    with patch("zing_ai.installer.install_claude") as mock:
        result = runner.invoke(cli, ["install", "--claude"])
    assert result.exit_code == 0
    mock.assert_called_once()


def test_install_opencode():
    runner = CliRunner()
    with patch("zing_ai.installer.install_opencode") as mock:
        result = runner.invoke(cli, ["install", "--opencode"])
    assert result.exit_code == 0
    mock.assert_called_once()


def test_install_all():
    runner = CliRunner()
    with (
        patch("zing_ai.installer.install_claude") as mock_claude,
        patch("zing_ai.installer.install_opencode") as mock_opencode,
    ):
        result = runner.invoke(cli, ["install", "--all"])
    assert result.exit_code == 0
    mock_claude.assert_called_once()
    mock_opencode.assert_called_once()


def test_install_claude_and_opencode():
    runner = CliRunner()
    with (
        patch("zing_ai.installer.install_claude") as mock_claude,
        patch("zing_ai.installer.install_opencode") as mock_opencode,
    ):
        result = runner.invoke(cli, ["install", "--claude", "--opencode"])
    assert result.exit_code == 0
    mock_claude.assert_called_once()
    mock_opencode.assert_called_once()


# -- reapply-patches subcommand ---------------------------------------------


def test_reapply_patches_help():
    runner = CliRunner()
    result = runner.invoke(cli, ["reapply-patches", "--help"])
    assert result.exit_code == 0
    assert "--claude" in result.output


def test_reapply_patches_claude():
    runner = CliRunner()
    with patch("zing_ai.backup.reapply_patches") as mock:
        result = runner.invoke(cli, ["reapply-patches", "--claude"])
    assert result.exit_code == 0
    mock.assert_called_once()


def test_reapply_patches_all():
    runner = CliRunner()
    with patch("zing_ai.backup.reapply_patches") as mock:
        result = runner.invoke(cli, ["reapply-patches", "--all"])
    assert result.exit_code == 0
    assert mock.call_count == 2


# -- resolve runtimes -------------------------------------------------------


def test_resolve_all_flag_returns_both():
    assert _resolve_runtimes(claude=False, opencode=False, all_runtimes=True) == [
        "claude",
        "opencode",
    ]


def test_resolve_claude_only():
    assert _resolve_runtimes(claude=True, opencode=False, all_runtimes=False) == [
        "claude",
    ]


def test_resolve_opencode_only():
    assert _resolve_runtimes(claude=False, opencode=True, all_runtimes=False) == [
        "opencode",
    ]


def test_resolve_both_explicit():
    assert _resolve_runtimes(claude=True, opencode=True, all_runtimes=False) == [
        "claude",
        "opencode",
    ]


def test_resolve_all_with_claude_is_error():
    with pytest.raises(click.UsageError):
        _resolve_runtimes(claude=True, opencode=False, all_runtimes=True)


def test_resolve_all_with_opencode_is_error():
    with pytest.raises(click.UsageError):
        _resolve_runtimes(claude=False, opencode=True, all_runtimes=True)


# -- interactive prompt ------------------------------------------------------


def test_interactive_choice_1_selects_claude():
    runner = CliRunner()
    with patch("zing_ai.installer.install_claude") as mock:
        result = runner.invoke(cli, ["install"], input="1\n")
    assert result.exit_code == 0
    mock.assert_called_once()


def test_interactive_choice_2_selects_opencode():
    runner = CliRunner()
    with patch("zing_ai.installer.install_opencode") as mock:
        result = runner.invoke(cli, ["install"], input="2\n")
    assert result.exit_code == 0
    mock.assert_called_once()


def test_interactive_choice_3_selects_all():
    runner = CliRunner()
    with (
        patch("zing_ai.installer.install_claude") as mock_claude,
        patch("zing_ai.installer.install_opencode") as mock_opencode,
    ):
        result = runner.invoke(cli, ["install"], input="3\n")
    assert result.exit_code == 0
    mock_claude.assert_called_once()
    mock_opencode.assert_called_once()


def test_interactive_invalid_then_valid():
    runner = CliRunner()
    with patch("zing_ai.installer.install_claude") as mock:
        result = runner.invoke(cli, ["install"], input="x\n1\n")
    assert result.exit_code == 0
    mock.assert_called_once()


def test_interactive_eof_exits_130():
    runner = CliRunner()
    with patch("click.prompt", side_effect=EOFError):
        result = runner.invoke(cli, ["install"])
    assert result.exit_code == 130


def test_reapply_patches_dispatches():
    runner = CliRunner()
    with patch("zing_ai.backup.reapply_patches") as mock:
        result = runner.invoke(cli, ["reapply-patches", "--opencode"])
    assert result.exit_code == 0
    mock.assert_called_once()


# -- orchestrator subcommands -----------------------------------------------

ORCHESTRATOR_COMMANDS = ["new", "plan", "plan-audit", "plan-review", "build", "build-audit"]


class TestOrchestratorCommandsRegistered:
    """Verify all 6 orchestrator commands are registered on the CLI group."""

    def test_all_orchestrator_commands_registered(self):
        for cmd_name in ORCHESTRATOR_COMMANDS:
            assert cmd_name in cli.commands, f"{cmd_name!r} not registered"

    def test_existing_commands_still_registered(self):
        assert "install" in cli.commands
        assert "reapply-patches" in cli.commands


class TestOrchestratorCommandHelp:
    """Verify each orchestrator command shows --help with expected options."""

    @pytest.mark.parametrize("cmd_name", ORCHESTRATOR_COMMANDS)
    def test_help_shows_zing_file_argument(self, cmd_name):
        runner = CliRunner()
        result = runner.invoke(cli, [cmd_name, "--help"])
        assert result.exit_code == 0
        assert "ZING_FILE" in result.output or "zing_file" in result.output.lower()

    @pytest.mark.parametrize("cmd_name", ORCHESTRATOR_COMMANDS)
    def test_help_shows_skip_permissions_flag(self, cmd_name):
        runner = CliRunner()
        result = runner.invoke(cli, [cmd_name, "--help"])
        assert result.exit_code == 0
        assert "--skip-permissions" in result.output


class TestOrchestratorCommandDelegation:
    """Verify each orchestrator command delegates to its corresponding module."""

    _COMMAND_MODULES = {
        "new": "zing_ai.orchestrator.commands.new.run_new",
        "plan": "zing_ai.orchestrator.commands.plan.run_plan",
        "plan-audit": "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
        "plan-review": "zing_ai.orchestrator.commands.plan_review.run_plan_review",
        "build": "zing_ai.orchestrator.commands.build.run_build",
        "build-audit": "zing_ai.orchestrator.commands.build_audit.run_build_audit",
    }

    @pytest.mark.parametrize(
        ("cmd_name", "run_func_path"),
        list(_COMMAND_MODULES.items()),
    )
    def test_command_delegates_to_module(self, cmd_name, run_func_path, tmp_path):
        """Each command loads config, finds project root, and calls the run function."""
        # Create a fake .git dir so find_project_root() works
        (tmp_path / ".git").mkdir()

        runner = CliRunner()
        with (
            patch(run_func_path, side_effect=NotImplementedError("stub")) as mock_run,
            patch(
                "zing_ai.orchestrator.project.find_project_root",
                return_value=tmp_path,
            ),
            patch(
                "zing_ai.orchestrator.config.load_config",
            ) as mock_config,
        ):
            result = runner.invoke(cli, [cmd_name])

        # The stub raises NotImplementedError which propagates
        assert result.exit_code == 1
        mock_config.assert_called_once_with(tmp_path)
        mock_run.assert_called_once()

        # Verify keyword arguments passed to the run function
        call_kwargs = mock_run.call_args.kwargs
        assert call_kwargs["zing_file"] is None
        assert call_kwargs["skip_permissions"] is False
        assert call_kwargs["config"] == mock_config.return_value
        assert call_kwargs["project_root"] == tmp_path

    @pytest.mark.parametrize("cmd_name", ORCHESTRATOR_COMMANDS)
    def test_command_passes_zing_file_argument(self, cmd_name, tmp_path):
        (tmp_path / ".git").mkdir()
        run_func_path = self._COMMAND_MODULES[cmd_name]

        runner = CliRunner()
        with (
            patch(run_func_path, side_effect=NotImplementedError("stub")) as mock_run,
            patch(
                "zing_ai.orchestrator.project.find_project_root",
                return_value=tmp_path,
            ),
            patch("zing_ai.orchestrator.config.load_config"),
        ):
            result = runner.invoke(cli, [cmd_name, "my-feature.xml"])

        assert result.exit_code == 1
        assert mock_run.call_args.kwargs["zing_file"] == "my-feature.xml"

    @pytest.mark.parametrize("cmd_name", ORCHESTRATOR_COMMANDS)
    def test_command_passes_skip_permissions_flag(self, cmd_name, tmp_path):
        (tmp_path / ".git").mkdir()
        run_func_path = self._COMMAND_MODULES[cmd_name]

        runner = CliRunner()
        with (
            patch(run_func_path, side_effect=NotImplementedError("stub")) as mock_run,
            patch(
                "zing_ai.orchestrator.project.find_project_root",
                return_value=tmp_path,
            ),
            patch("zing_ai.orchestrator.config.load_config"),
        ):
            result = runner.invoke(cli, [cmd_name, "--skip-permissions"])

        assert result.exit_code == 1
        assert mock_run.call_args.kwargs["skip_permissions"] is True
