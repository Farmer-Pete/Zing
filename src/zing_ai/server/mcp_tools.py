"""MCP tool handlers for the Zing batch review server."""

from __future__ import annotations

import webbrowser

from mcp.server.fastmcp import FastMCP

from zing_ai.server.sessions import SessionManager

_DEFAULT_PORT = 9876

mcp_server = FastMCP("Zing Review")

_session_manager: SessionManager | None = None
_port: int = _DEFAULT_PORT


def configure(session_manager: SessionManager, port: int = _DEFAULT_PORT) -> None:
    """Set the session manager and port used by MCP tool handlers.

    Args:
        session_manager: The SessionManager instance to use.
        port: The port the server is running on.
    """
    global _session_manager, _port  # noqa: PLW0603
    _session_manager = session_manager
    _port = port


def _get_session_manager() -> SessionManager:
    """Return the configured session manager or raise."""
    if _session_manager is None:
        msg = "MCP tools not configured — call configure() first"
        raise RuntimeError(msg)
    return _session_manager


@mcp_server.tool()
async def create_review(
    session_id: str, title: str, zing_file: str | None = None
) -> dict:
    """Creates a review session, opens browser, returns URL."""
    sm = _get_session_manager()
    sm.create_session(
        session_id=session_id,
        title=title,
        zing_file=zing_file,
    )
    url = f"http://localhost:{_port}/{session_id}"
    webbrowser.open(url)
    return {"status": "created", "url": url}


@mcp_server.tool()
async def start_step(
    session_id: str, step_name: str, expected_agents: int
) -> dict:
    """Starts a new workflow step within a session.

    Must be called before agents submit findings for this step.
    The same step_name can be used multiple times (for loops).
    """
    sm = _get_session_manager()
    step = sm.start_step(session_id, step_name, expected_agents)
    return {
        "status": "started",
        "step_id": step.step_id,
        "step_name": step.step_name,
        "sequence": step.sequence,
    }


@mcp_server.tool()
async def wait_for_review(session_id: str, step_name: str) -> dict:
    """Blocks until user submits feedback for a specific workflow step.

    Returns the full findings + responses for that step.
    Both session_id and step_name are required.
    """
    sm = _get_session_manager()
    step = sm.get_latest_step(session_id, step_name)
    review_response = await sm.wait_for_review(session_id, step.step_id)
    return review_response.model_dump()
