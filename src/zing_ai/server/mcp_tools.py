"""MCP tool handlers for the Zing batch review server."""

from __future__ import annotations

import webbrowser

from mcp.server.fastmcp import FastMCP

from zing_ai.server.routes import _notify_dashboard_connections
from zing_ai.server.sessions import SessionManager

mcp_server = FastMCP("Zing Review")

_session_manager: SessionManager | None = None
_port: int = 9876


def configure(session_manager: SessionManager, port: int = 9876) -> None:
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
    session_id: str, title: str, zing_file: str, expected_agents: int
) -> dict:
    """Creates a review session, opens browser, returns URL."""
    sm = _get_session_manager()
    sm.create_session(
        session_id=session_id,
        title=title,
        zing_file=zing_file,
        expected_agents=expected_agents,
    )
    url = f"http://localhost:{_port}/{session_id}"
    webbrowser.open(url)
    _notify_dashboard_connections("created")
    return {"status": "created", "url": url}


@mcp_server.tool()
async def wait_for_review(session_id: str) -> dict:
    """Blocks until user submits. Returns full findings + responses."""
    sm = _get_session_manager()
    review_response = await sm.wait_for_review(session_id)
    return review_response.model_dump()
