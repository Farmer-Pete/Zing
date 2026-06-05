"""Tests for the zing-ai sim CLI subcommands."""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

from zing_ai.cli import cli

BAK_1321_VIZ = (
    Path(__file__).parent
    / "test_viz"
    / "fixtures"
    / "BAK-1321"
    / "BAK-1321-direct-flatten.viz.json"
)
BAK_1321_MD = Path(__file__).parent / "test_viz" / "fixtures" / "BAK-1321" / "plan.md"

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


def test_sim_update_with_zing_file(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "update", "--zing-file", "/tmp/spec.md"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "session_update",
        {"session_id": "test-session-abc123", "zing_file": "/tmp/spec.md"},
    )


def test_sim_update_no_state_file(mock_mcp_call, mock_state_file):
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "update", "--title", "New Title"])
    assert result.exit_code != 0
    assert "No active sim session" in result.output


def test_sim_update_corrupted_state_file(mock_mcp_call, mock_state_file):
    mock_state_file.write_text("not valid json{{{")
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "update", "--title", "New Title"])
    assert result.exit_code != 0
    assert "Corrupted state file" in result.output


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
        cli, ["sim", "agent-start", "plan", "Analyzer", "--description", "Scanning code"]
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


# -- sim finding text ---------------------------------------------------------


def test_sim_finding_text(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok", "finding_id": "f1"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "finding", "text", "plan"])
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "finding_submit",
        {
            "session_id": "test-session-abc123",
            "step_id": "step-plan-id",
            "finding": {"type": "text", "title": "Test finding", "body": "Test body"},
        },
    )


# -- sim finding triage -------------------------------------------------------


def test_sim_finding_triage_defaults(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok", "finding_id": "f2"}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "finding", "triage", "plan"])
    assert result.exit_code == 0, result.output
    finding = mock_mcp_call.call_args[0][2]["finding"]
    assert finding["category"] == "correctness"
    assert finding["severity"] == "medium"
    assert finding["confidence"] == "medium"
    assert "location" not in finding


def test_sim_finding_triage_with_location(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok", "finding_id": "f3"}
    runner = CliRunner()
    result = runner.invoke(
        cli,
        ["sim", "finding", "triage", "plan", "--file", "src/db.py", "--line", "42"],
    )
    assert result.exit_code == 0, result.output
    finding = mock_mcp_call.call_args[0][2]["finding"]
    assert finding["location"] == {"file": "src/db.py", "line": 42}


# -- sim finding triage-options ------------------------------------------------


def test_sim_finding_triage_options(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok", "finding_id": "f4"}
    runner = CliRunner()
    result = runner.invoke(
        cli,
        [
            "sim",
            "finding",
            "triage-options",
            "plan",
            "--option",
            "Yes:Accept",
            "--option",
            "No:Reject",
        ],
    )
    assert result.exit_code == 0, result.output
    finding = mock_mcp_call.call_args[0][2]["finding"]
    assert finding["type"] == "triage"
    assert finding["options"] == [
        {"label": "Yes", "description": "Accept"},
        {"label": "No", "description": "Reject"},
    ]


def test_sim_finding_triage_options_requires_two_options(
    mock_mcp_call,
    mock_state_file,
    sample_state,
):
    mock_state_file.write_text(json.dumps(sample_state))
    runner = CliRunner()
    result = runner.invoke(
        cli, ["sim", "finding", "triage-options", "plan", "--option", "Yes:Accept"]
    )
    assert result.exit_code != 0
    assert "At least 2" in result.output


# -- sim finding evaluation ---------------------------------------------------


def test_sim_finding_evaluation(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "ok", "finding_id": "f5"}
    runner = CliRunner()
    result = runner.invoke(
        cli,
        ["sim", "finding", "evaluation", "plan", "--criterion", "Clarity:strong:Well written"],
    )
    assert result.exit_code == 0, result.output
    finding = mock_mcp_call.call_args[0][2]["finding"]
    assert finding["criteria"] == [
        {"name": "Clarity", "rating": "strong", "justification": "Well written"},
    ]


# -- sim wait -----------------------------------------------------------------


def test_sim_wait(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {
        "session_id": "test-session-abc123",
        "step_name": "plan",
        "items": [{"title": "Q1", "answer": "A1"}],
    }
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "wait", "plan"])
    assert result.exit_code == 0, result.output
    assert '"step_name": "plan"' in result.output
    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "review_wait",
        {"session_id": "test-session-abc123", "step_id": "step-plan-id"},
        timeout=None,
    )


def test_sim_wait_prints_waiting_message(mock_mcp_call, mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"session_id": "test-session-abc123", "items": []}
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "wait", "plan"])
    assert result.exit_code == 0, result.output
    assert "Waiting for review" in result.stderr


# -- sim viz-attach -----------------------------------------------------------


def test_sim_viz_attach_happy_path(mock_mcp_call, mock_state_file, mock_staging_root, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "updated", "session_id": "test-session-abc123"}
    runner = CliRunner()
    result = runner.invoke(
        cli,
        ["sim", "viz-attach", str(BAK_1321_VIZ), "--md", str(BAK_1321_MD)],
    )
    assert result.exit_code == 0, result.output

    sid = sample_state["session_id"]
    expected_md = mock_staging_root / sid / f"{sid}.md"
    expected_viz = mock_staging_root / sid / f"{sid}.viz.json"
    assert expected_md.exists()
    assert expected_viz.exists()

    mock_mcp_call.assert_called_once_with(
        "http://localhost:9876/mcp",
        "session_update",
        {"session_id": sid, "zing_file": str(expected_md)},
    )

    # State file is updated with staging_dir + zing_file
    state = json.loads(mock_state_file.read_text())
    assert state["staging_dir"] == str(mock_staging_root / sid)
    assert state["zing_file"] == str(expected_md)

    # Summary JSON is printed to stdout
    summary = json.loads(result.output)
    assert summary["session_id"] == sid
    assert summary["plan_url"].endswith(f"/command-center/{sid}/plan")
    assert summary["steps"] > 0


def test_sim_viz_attach_invalid_viz_no_mcp_call(
    tmp_path, mock_mcp_call, mock_state_file, mock_staging_root, sample_state
):
    """Malformed viz aborts before any MCP call (no half-attach)."""
    mock_state_file.write_text(json.dumps(sample_state))
    bad_viz = tmp_path / "bad.viz.json"
    bad_viz.write_text(
        json.dumps(
            {
                "title": "bad",
                "steps": [
                    {
                        "step": 1,
                        "id": "a",
                        "title": "a",
                        "nodes": [{"id": "n", "shape": "rect", "label": "x"}],
                        "edges": [],
                    },
                    {
                        "step": 1,  # duplicate step number — uniqueness violation
                        "id": "b",
                        "title": "b",
                        "nodes": [{"id": "n", "shape": "rect", "label": "x"}],
                        "edges": [],
                    },
                ],
            }
        )
    )

    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "viz-attach", str(bad_viz)])
    assert result.exit_code != 0
    assert "validation" in result.output.lower()
    mock_mcp_call.assert_not_called()


def test_sim_viz_attach_reattach_guard(
    mock_mcp_call, mock_state_file, mock_staging_root, sample_state
):
    """Second viz-attach without --force errors; with --force succeeds."""
    mock_state_file.write_text(json.dumps(sample_state))
    mock_mcp_call.return_value = {"status": "updated"}

    # Pre-create the staging dir to simulate a prior attachment
    sid = sample_state["session_id"]
    stage = mock_staging_root / sid
    stage.mkdir(parents=True)
    (stage / f"{sid}.md").write_text("# prior\n")
    (stage / f"{sid}.viz.json").write_text("{}")

    runner = CliRunner()
    result = runner.invoke(
        cli,
        ["sim", "viz-attach", str(BAK_1321_VIZ), "--md", str(BAK_1321_MD)],
    )
    assert result.exit_code != 0
    assert "already has a viz attached" in result.output
    assert "--force" in result.output
    mock_mcp_call.assert_not_called()

    # With --force, succeeds
    result = runner.invoke(
        cli,
        ["sim", "viz-attach", str(BAK_1321_VIZ), "--md", str(BAK_1321_MD), "--force"],
    )
    assert result.exit_code == 0, result.output
    mock_mcp_call.assert_called_once()


def test_sim_viz_attach_server_restart_recovery(
    mock_mcp_call, mock_state_file, mock_staging_root, sample_state
):
    """KeyError-shaped error from session_update surfaces the recovery hint."""
    mock_state_file.write_text(json.dumps(sample_state))
    sid = sample_state["session_id"]
    # KeyError from SessionManager surfaces as repr of the key
    mock_mcp_call.return_value = {"error": f"'{sid}'"}

    runner = CliRunner()
    result = runner.invoke(
        cli,
        ["sim", "viz-attach", str(BAK_1321_VIZ), "--md", str(BAK_1321_MD)],
    )
    assert result.exit_code != 0
    assert "not found on server" in result.output
    assert "may have been restarted" in result.output
    assert "viz-teardown" in result.output


# -- sim url ------------------------------------------------------------------


def test_sim_url_dashboard(mock_state_file, sample_state):
    mock_state_file.write_text(json.dumps(sample_state))
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "url"])
    assert result.exit_code == 0, result.output
    assert result.output.strip() == f"http://localhost:9876/{sample_state['session_id']}"


def test_sim_url_plan_gate_before_attach(mock_state_file, sample_state):
    """--plan refuses to print before viz-attach has run."""
    mock_state_file.write_text(json.dumps(sample_state))  # no zing_file key
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "url", "--plan"])
    assert result.exit_code != 0
    assert "no plan attached" in result.output
    assert "viz-attach" in result.output


def test_sim_url_plan_after_attach(mock_state_file, sample_state):
    """--plan prints the plan-detail URL when zing_file is set in state."""
    state = {**sample_state, "zing_file": "/tmp/some.md", "staging_dir": "/tmp"}
    mock_state_file.write_text(json.dumps(state))
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "url", "--plan"])
    assert result.exit_code == 0, result.output
    assert result.output.strip() == (
        f"http://localhost:9876/command-center/{state['session_id']}/plan"
    )


# -- sim viz-teardown ---------------------------------------------------------


def test_sim_viz_teardown_removes_state_and_staging(
    mock_state_file, mock_staging_root, sample_state
):
    sid = sample_state["session_id"]
    stage = mock_staging_root / sid
    stage.mkdir(parents=True)
    (stage / f"{sid}.md").write_text("# stub\n")
    (stage / f"{sid}.viz.json").write_text("{}")

    state = {**sample_state, "staging_dir": str(stage), "zing_file": str(stage / f"{sid}.md")}
    mock_state_file.write_text(json.dumps(state))

    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "viz-teardown"])
    assert result.exit_code == 0, result.output
    assert not mock_state_file.exists()
    assert not stage.exists()


def test_sim_viz_teardown_keep_staging(mock_state_file, mock_staging_root, sample_state):
    sid = sample_state["session_id"]
    stage = mock_staging_root / sid
    stage.mkdir(parents=True)
    (stage / f"{sid}.md").write_text("# stub\n")

    state = {**sample_state, "staging_dir": str(stage), "zing_file": str(stage / f"{sid}.md")}
    mock_state_file.write_text(json.dumps(state))

    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "viz-teardown", "--keep-staging"])
    assert result.exit_code == 0, result.output
    assert not mock_state_file.exists()
    assert stage.exists()


def test_sim_viz_teardown_no_state_file(mock_state_file):
    runner = CliRunner()
    result = runner.invoke(cli, ["sim", "viz-teardown"])
    assert result.exit_code == 0, result.output
    assert "No sim state to remove" in result.output
