"""Shared pytest fixtures for zing-ai tests."""

from __future__ import annotations

from unittest.mock import patch

import pytest

from zing_ai.config import load_config as _real_load_config


@pytest.fixture(autouse=True)
def _disable_zellij_in_config():
    """Force ``command_center.zellij_support`` off for the test suite.

    Prevents the app lifespan from invoking ``zellij web --start`` /
    ``zellij web --stop`` on the host (which would disrupt the developer's
    real zellij sessions). Tests that need to exercise the zellij startup
    path should pass ``zellij_support=True`` to ``create_app`` explicitly
    (with ``subprocess.run`` mocked).
    """

    def _patched_load_config():
        cfg = _real_load_config()
        cfg.command_center.zellij_support = False
        return cfg

    with patch("zing_ai.server.app.load_config", _patched_load_config):
        yield


@pytest.fixture()
def mock_state_file(tmp_path):
    """Provide a temporary state file path, patched into sim._STATE_FILE."""
    state_path = tmp_path / "sim-state.json"
    with patch("zing_ai.sim._STATE_FILE", state_path):
        yield state_path


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
