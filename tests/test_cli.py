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
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_claude") as mock,
    ):
        result = runner.invoke(cli, ["install", "--claude"])
    assert result.exit_code == 0
    mock.assert_called_once()


def test_install_opencode():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_opencode") as mock,
    ):
        result = runner.invoke(cli, ["install", "--opencode"])
    assert result.exit_code == 0
    mock.assert_called_once()


def test_install_all():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
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
        patch("zing_ai.config.load_config"),
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
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_claude") as mock,
    ):
        result = runner.invoke(cli, ["install"], input="1\n")
    assert result.exit_code == 0
    mock.assert_called_once()


def test_interactive_choice_2_selects_opencode():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_opencode") as mock,
    ):
        result = runner.invoke(cli, ["install"], input="2\n")
    assert result.exit_code == 0
    mock.assert_called_once()


def test_interactive_choice_3_selects_all():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_claude") as mock_claude,
        patch("zing_ai.installer.install_opencode") as mock_opencode,
    ):
        result = runner.invoke(cli, ["install"], input="3\n")
    assert result.exit_code == 0
    mock_claude.assert_called_once()
    mock_opencode.assert_called_once()


def test_interactive_invalid_then_valid():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_claude") as mock,
    ):
        result = runner.invoke(cli, ["install"], input="x\n1\n")
    assert result.exit_code == 0
    mock.assert_called_once()


def test_interactive_eof_exits_130():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
        patch("click.prompt", side_effect=EOFError),
    ):
        result = runner.invoke(cli, ["install"])
    assert result.exit_code == 130


def test_reapply_patches_dispatches():
    runner = CliRunner()
    with patch("zing_ai.backup.reapply_patches") as mock:
        result = runner.invoke(cli, ["reapply-patches", "--opencode"])
    assert result.exit_code == 0
    mock.assert_called_once()


# -- install with config loading ---------------------------------------------


def test_install_loads_default_config():
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config") as mock_load_config,
        patch("zing_ai.installer.install_claude") as mock_install,
    ):
        result = runner.invoke(cli, ["install", "--claude"])
    assert result.exit_code == 0, result.output
    mock_install.assert_called_once()
    _, kwargs = mock_install.call_args
    assert "config" in kwargs
    assert kwargs["config"] is mock_load_config.return_value


def test_install_surfaces_config_error():
    from zing_ai.config import ConfigError

    runner = CliRunner()
    with patch("zing_ai.config.load_config", side_effect=ConfigError("bad toml")):
        result = runner.invoke(cli, ["install", "--claude"])
    assert result.exit_code == 1
    assert "bad toml" in result.output


def test_install_surfaces_install_error():
    from zing_ai.installer import InstallError

    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config"),
        patch("zing_ai.installer.install_claude", side_effect=InstallError("boom")),
    ):
        result = runner.invoke(cli, ["install", "--claude"])
    assert result.exit_code == 1
    assert "boom" in result.output


# -- MCP command --------------------------------------------------------------


def test_mcp_default_port():
    runner = CliRunner()
    with patch("uvicorn.run") as mock_run:
        result = runner.invoke(cli, ["mcp"])
    assert result.exit_code == 0
    mock_run.assert_called_once()
    _args, kwargs = mock_run.call_args
    assert kwargs["host"] == "127.0.0.1"
    assert kwargs["port"] == 9876


def test_mcp_custom_port():
    runner = CliRunner()
    with patch("uvicorn.run") as mock_run:
        result = runner.invoke(cli, ["mcp", "--port", "8080"])
    assert result.exit_code == 0
    mock_run.assert_called_once()
    _args, kwargs = mock_run.call_args
    assert kwargs["port"] == 8080


# -- launch subcommand: markdown target ----------------------------------------


def test_launch_unrecognized_target_error():
    """Unrecognized target prints an error listing all three target types."""
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config") as mock_cfg,
        patch("urllib.request.urlopen"),  # server check passes
    ):
        mock_cfg.return_value.git.workflow_mode = "worktree"
        mock_cfg.return_value.command_center.claude_flags = ""
        result = runner.invoke(cli, ["launch", "not-a-thing"])
    assert result.exit_code == 1
    assert "Unrecognized target" in result.output
    assert "ticket ID" in result.output
    assert "PR URL" in result.output
    assert "markdown" in result.output


def test_launch_markdown_nonexistent_file():
    """Launching a nonexistent .md file prints a clear error."""
    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config") as mock_cfg,
        patch("urllib.request.urlopen"),
    ):
        mock_cfg.return_value.git.workflow_mode = "worktree"
        mock_cfg.return_value.command_center.claude_flags = ""
        result = runner.invoke(cli, ["launch", "/nonexistent/plan.md"])
    assert result.exit_code == 1
    assert "not found" in result.output


def test_launch_markdown_setup_only(tmp_path):
    """--setup-only with a valid .md file creates the environment without launching Claude."""
    md_file = tmp_path / "my-plan.md"
    md_file.write_text("# Plan\n")

    runner = CliRunner()
    with (
        patch("zing_ai.config.load_config") as mock_cfg,
        patch("urllib.request.urlopen"),
        patch("zing_ai.launch.detect_action_by_title", return_value=("new", None, None)),
        patch("zing_ai.launch.resolve_repo_root", return_value=tmp_path),
        patch("zing_ai.launch.create_worktree", return_value=tmp_path / "worktree"),
        patch("zing_ai.launch.run_init_script"),
        patch("zing_ai.launch.create_session_on_server") as mock_create,
    ):
        mock_cfg.return_value.git.workflow_mode = "worktree"
        mock_cfg.return_value.git.worktree_root = "../{repo}-{branch}"
        mock_cfg.return_value.git.branch_prefix = "zing/"
        mock_cfg.return_value.git.zing_init_script = ""
        mock_cfg.return_value.command_center.claude_flags = ""
        result = runner.invoke(cli, ["launch", str(md_file), "--setup-only"])
    assert result.exit_code == 0, result.output
    assert "Environment ready" in result.output
    assert "Session ID" in result.output
    # Verify session was created with correct args
    mock_create.assert_called_once()
    call_kwargs = mock_create.call_args
    assert call_kwargs.kwargs.get("ticket_id") is None or call_kwargs[1].get("ticket_id") is None
    assert call_kwargs.kwargs.get("skill") == "build" or call_kwargs[1].get("skill") == "build"
