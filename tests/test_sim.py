"""Tests for the zing-ai sim CLI subcommands."""

from __future__ import annotations

import json

from click.testing import CliRunner

from zing_ai.cli import cli


# -- sim create ---------------------------------------------------------------


def test_sim_create(mock_mcp_call, mock_state_file):
    mock_mcp_call.return_value = {
        "session_id": "test-123",
        "steps": {"plan": "p1"},
        "url": "http://localhost:9876",
    }
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "create", "My Title"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "session_create",
        {"title": "My Title", "steps": None},
    )
    # Verify state file was written
    state = json.loads(mock_state_file.read_text())
    assert state["session_id"] == "test-123"
    assert state["steps"] == {"plan": "p1"}
    assert state["url"] == "http://localhost:9876/mcp"


def test_sim_create_with_steps(mock_mcp_call, mock_state_file):
    mock_mcp_call.return_value = {
        "session_id": "test-456",
        "steps": {"plan": "p1", "build": "b1"},
        "url": "http://localhost:9876",
    }
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "create", "My Title", "--steps", "plan,build"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "session_create",
        {"title": "My Title", "steps": ["plan", "build"]},
    )
