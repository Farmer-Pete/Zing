"""Shared pytest fixtures for zing-ai tests."""

from __future__ import annotations

from unittest.mock import patch

import pytest


@pytest.fixture()
def mock_state_file(tmp_path):
    """Provide a temporary state file path, patched into sim._STATE_FILE."""
    state_path = tmp_path / "sim-state.json"
    with patch("zing_ai.sim._STATE_FILE", state_path):
        yield state_path


@pytest.fixture()
def mock_staging_root(tmp_path):
    """Provide a temporary staging root, patched into sim._STAGING_ROOT.

    Keeps viz-attach/viz-teardown tests from touching the real
    ~/.zing-ai/sim-sessions/ directory.
    """
    staging_root = tmp_path / "sim-sessions"
    with patch("zing_ai.sim._STAGING_ROOT", staging_root):
        yield staging_root


@pytest.fixture()
def mock_mcp_call():
    """Patch _call_mcp to return configurable responses without a real MCP server."""
    with patch("zing_ai.sim._call_mcp") as mock:
        yield mock


@pytest.fixture()
def sample_state():
    """Return a dict matching the state file schema."""
    return {
        "session_id": "test-session-abc123",
        "steps": {
            "plan": "step-plan-id",
            "plan-audit": "step-audit-id",
            "build": "step-build-id",
            "build-audit": "step-build-audit-id",
        },
        "url": "http://localhost:9876/mcp",
    }
