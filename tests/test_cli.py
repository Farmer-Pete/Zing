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
