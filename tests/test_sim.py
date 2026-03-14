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


# -- sim update ---------------------------------------------------------------


def test_sim_update(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "update", "--title", "New Title"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "session_update",
        {"session_id": "test-session-abc123", "title": "New Title"},
    )


def test_sim_update_no_state_file(mock_mcp_call, mock_state_file):
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "update", "--title", "New Title"])
    assert result.exit_code != 0
    assert "No active sim session" in result.output


# -- sim start ----------------------------------------------------------------


def test_sim_start(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok", "step_id": "step-plan-id", "step_name": "plan"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "start", "plan"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "step_start",
        {"session_id": "test-session-abc123", "step_id": "step-plan-id"},
    )


def test_sim_start_invalid_step(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "start", "nonexistent"])
    assert result.exit_code != 0
    assert "Unknown step 'nonexistent'" in result.output


# -- sim agent-start ----------------------------------------------------------


def test_sim_agent_start(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "started", "agent_name": "Analyzer"}
    runner = CliRunner()
    result = runner.invoke(
        cli, ["sim", "agent-start", "plan", "Analyzer", "--desc", "Scanning code"]
    )
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "agent_start",
        {
            "session_id": "test-session-abc123",
            "step_id": "step-plan-id",
            "name": "Analyzer",
            "description": "Scanning code",
        },
    )


# -- sim agent-stop -----------------------------------------------------------


def test_sim_agent_stop(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "stopped"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "agent-stop", "plan", "Analyzer"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "agent_stop",
        {
            "session_id": "test-session-abc123",
            "step_id": "step-plan-id",
            "name": "Analyzer",
        },
    )


# -- sim log ------------------------------------------------------------------


def test_sim_log(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "log", "plan", "Analyzer", "Found 3 issues"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "step_log",
        {
            "session_id": "test-session-abc123",
            "step_id": "step-plan-id",
            "agent_name": "Analyzer",
            "message": "Found 3 issues",
        },
    )
