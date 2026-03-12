"""MCP tool handlers for the Zing batch review server."""

from __future__ import annotations

import re
import webbrowser
from uuid import uuid4

from mcp.server.fastmcp import FastMCP

from zing_ai.server.sessions import SessionManager

_DEFAULT_PORT = 9876

mcp_server = FastMCP("Zing Review")

_session_manager: SessionManager | None = None
_port: int = _DEFAULT_PORT

_DEFAULT_STEPS = ["plan", "plan-audit", "build", "build-audit"]


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


def _slugify(text: str) -> str:
    """Convert text to a URL-friendly slug using simple regex."""
    text = text.lower().strip()
    text = re.sub(r"[^\w\s-]", "", text)
    text = re.sub(r"[\s_]+", "-", text)
    return re.sub(r"-+", "-", text).strip("-")


@mcp_server.tool()
async def session_create(title: str, steps: list[str] | None = None) -> dict:
    """Create a new session with pre-defined steps. Returns session_id and step IDs."""
    sm = _get_session_manager()
    step_names = steps if steps is not None else _DEFAULT_STEPS
    slug = _slugify(title)
    session_id = f"{slug}-{uuid4().hex[:6]}"
    try:
        session = sm.create_session(
            session_id=session_id,
            title=title,
            steps=step_names,
        )
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    url = f"http://localhost:{_port}/{session_id}"
    step_map = {s.step_name: s.step_id for s in session.steps}
    return {"session_id": session_id, "steps": step_map, "url": url}


@mcp_server.tool()
async def session_update(
    session_id: str, zing_file: str | None = None, title: str | None = None,
) -> dict:
    """Update session title and/or zing file path."""
    sm = _get_session_manager()
    try:
        session = sm.update_session(session_id, zing_file=zing_file, title=title)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {"status": "updated", "session_id": session.session_id, "title": session.title}


@mcp_server.tool()
async def step_start(session_id: str, step_id: str) -> dict:
    """Mark a pre-defined step as started (PENDING -> STARTED)."""
    sm = _get_session_manager()
    try:
        step = sm.start_step(session_id, step_id)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {
        "status": "started",
        "step_id": step.step_id,
        "step_name": step.step_name,
        "sequence": step.sequence,
    }


@mcp_server.tool()
async def agent_start(
    session_id: str, step_id: str, name: str, description: str = "",
) -> dict:
    """Register a running agent for a step. Dashboard shows it with a spinner."""
    sm = _get_session_manager()
    try:
        agent = sm.start_agent(session_id, step_id, name, description)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {"status": "started", "agent_name": agent.name, "state": agent.state.value}


@mcp_server.tool()
async def agent_stop(session_id: str, step_id: str, name: str) -> dict:
    """Mark an agent as completed. If all agents done, step transitions to READY."""
    sm = _get_session_manager()
    try:
        step = sm.stop_agent(session_id, step_id, name)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {"status": "stopped", "step_state": step.state.value}


@mcp_server.tool()
async def finding_submit(session_id: str, step_id: str, finding: dict) -> dict:
    """Submit a deduplicated finding to a step."""
    sm = _get_session_manager()
    try:
        result = sm.add_finding(session_id, step_id, finding)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {"status": "ok", "finding_id": result.id}


@mcp_server.tool()
async def review_wait(session_id: str, step_id: str) -> dict:
    """Block until user submits review for a step. Returns findings + responses."""
    sm = _get_session_manager()
    url = f"http://localhost:{_port}/{session_id}"
    try:
        webbrowser.open(url)
    except Exception:
        pass  # Headless/CI environments may not have a browser
    try:
        review_response = await sm.wait_for_review(session_id, step_id)
    except KeyError as exc:
        return {"error": str(exc)}
    return review_response.model_dump()


@mcp_server.tool()
async def step_log(session_id: str, step_id: str, agent_name: str, message: str) -> dict:
    """Append a log entry to a step. Appears in dashboard under the step."""
    sm = _get_session_manager()
    try:
        entry = sm.add_log(session_id, step_id, agent_name, message)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {"status": "ok", "timestamp": entry.timestamp.isoformat()}
