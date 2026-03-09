"""Tests for MCP server registration logic."""

from __future__ import annotations

import json
from pathlib import Path
from unittest.mock import patch

from zing_ai.installer import register_mcp_server

# ---------------------------------------------------------------------------
# Claude Code registration
# ---------------------------------------------------------------------------


def test_claude_calls_subprocess_with_correct_args() -> None:
    """register_mcp_server('claude') invokes the claude CLI with the right args."""
    with (
        patch("zing_ai.installer.shutil.which", return_value="/usr/local/bin/claude"),
        patch("zing_ai.installer.subprocess.run") as mock_run,
    ):
        register_mcp_server("claude")

    mock_run.assert_called_once_with(
        [
            "claude", "mcp", "add",
            "-s", "user",
            "-t", "http",
            "zing-ai",
            "http://127.0.0.1:9876/mcp",
        ],
        check=True,
        capture_output=True,
        text=True,
    )


def test_claude_warns_when_cli_not_on_path() -> None:
    """When the claude CLI is not found, warn and skip without error."""
    with (
        patch("zing_ai.installer.shutil.which", return_value=None),
        patch("zing_ai.installer.subprocess.run") as mock_run,
        patch("zing_ai.installer.logger") as mock_logger,
    ):
        register_mcp_server("claude")

    mock_run.assert_not_called()
    mock_logger.warning.assert_called()
    assert "not found" in mock_logger.warning.call_args[0][0].lower()


def test_claude_idempotent_re_registration() -> None:
    """Calling register_mcp_server('claude') twice runs the CLI each time (idempotent)."""
    with (
        patch("zing_ai.installer.shutil.which", return_value="/usr/local/bin/claude"),
        patch("zing_ai.installer.subprocess.run") as mock_run,
    ):
        register_mcp_server("claude")
        register_mcp_server("claude")

    assert mock_run.call_count == 2


# ---------------------------------------------------------------------------
# OpenCode registration
# ---------------------------------------------------------------------------


def test_opencode_creates_config_with_correct_structure(tmp_path: Path) -> None:
    """register_mcp_server('opencode') creates the config file with correct JSON."""
    with patch(
        "zing_ai.installer.Path.home",
        return_value=tmp_path / "fakehome",
    ):
        # Set up the expected path
        expected_dir = tmp_path / "fakehome" / ".config" / "opencode"
        expected_path = expected_dir / "opencode.json"

        register_mcp_server("opencode")

        assert expected_path.exists()
        config = json.loads(expected_path.read_text(encoding="utf-8"))
        assert "mcp" in config
        assert "zing-ai" in config["mcp"]
        assert config["mcp"]["zing-ai"] == {
            "type": "http",
            "url": "http://127.0.0.1:9876/mcp",
        }


def test_opencode_merges_into_existing_config(tmp_path: Path) -> None:
    """Existing config keys and MCP servers are preserved."""
    fake_home = tmp_path / "fakehome"
    config_dir = fake_home / ".config" / "opencode"
    config_dir.mkdir(parents=True)
    config_path = config_dir / "opencode.json"

    existing = {
        "theme": "dark",
        "mcp": {
            "other-server": {"type": "local", "command": ["other", "serve"]},
        },
    }
    config_path.write_text(json.dumps(existing), encoding="utf-8")

    with patch("zing_ai.installer.Path.home", return_value=fake_home):
        register_mcp_server("opencode")

    config = json.loads(config_path.read_text(encoding="utf-8"))
    # Original keys preserved
    assert config["theme"] == "dark"
    # Original MCP server preserved
    assert config["mcp"]["other-server"] == {"type": "local", "command": ["other", "serve"]}
    # Zing MCP server added
    assert config["mcp"]["zing-ai"] == {
        "type": "http",
        "url": "http://127.0.0.1:9876/mcp",
    }


def test_opencode_idempotent_re_registration(tmp_path: Path) -> None:
    """Calling register_mcp_server('opencode') twice produces identical config."""
    fake_home = tmp_path / "fakehome"

    with patch("zing_ai.installer.Path.home", return_value=fake_home):
        register_mcp_server("opencode")
        first = json.loads(
            (fake_home / ".config" / "opencode" / "opencode.json").read_text(encoding="utf-8")
        )

        register_mcp_server("opencode")
        second = json.loads(
            (fake_home / ".config" / "opencode" / "opencode.json").read_text(encoding="utf-8")
        )

    assert first == second
