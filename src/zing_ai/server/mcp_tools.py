"""MCP tool handlers for the Zing batch review server."""

from __future__ import annotations

import json
import logging
import re
from pathlib import Path
from uuid import uuid4

import yaml
from mcp.server.fastmcp import FastMCP

from zing_ai.server.sessions import SessionManager
from zing_ai.server.step_validators import STEP_VALIDATORS, ValidationException

logger = logging.getLogger(__name__)

_DEFAULT_PORT = 9876

mcp_server = FastMCP("Zing Review", stateless_http=True)

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
async def session_create(
    title: str,
    steps: list[str] | None = None,
    pr_number: int | None = None,
    pr_repo: str | None = None,
) -> dict:
    """Create a new session with pre-defined steps. Returns session_id and step IDs.

    For PR review sessions, pass both ``pr_number`` and ``pr_repo`` (as
    ``"owner/repo"``) so the Command Center can attach the session to the
    correct PR card. Passing only ``pr_number`` is not enough — PR numbers
    are not unique across repositories.
    """
    sm = _get_session_manager()
    step_names = steps if steps is not None else _DEFAULT_STEPS
    slug = _slugify(title)
    session_id = f"{slug}-{uuid4().hex[:6]}"
    try:
        session = sm.create_session(
            session_id=session_id,
            title=title,
            steps=step_names,
            pr_number=pr_number,
            pr_repo=pr_repo,
        )
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    url = f"http://localhost:{_port}/{session_id}"
    step_map = {s.step_name: s.step_id for s in session.steps}
    return {"session_id": session_id, "steps": step_map, "url": url}


def _extract_ticket_id_from_frontmatter(file_path: str) -> str | None:
    """Extract ``ticket_id`` from YAML frontmatter of a zing file.

    The file is expected to start with ``---\n<yaml>\n---\n<body>``.
    Returns ``None`` if the file cannot be read, has no frontmatter, or
    ``ticket_id`` is not present.

    Narrowly catches the expected failure modes (file I/O, malformed YAML)
    and logs a warning so the user/operator can see why a ``ticket_id`` was
    silently dropped. Other exceptions (programming errors) propagate —
    they shouldn't be masked by an overly broad ``except Exception``.
    """
    try:
        with open(file_path, encoding="utf-8") as fh:
            content = fh.read()
    except OSError as exc:
        logger.warning("Failed to read %s for ticket_id extraction: %s", file_path, exc)
        return None
    if not content.startswith("---"):
        return None
    # Find the closing '---'
    end = content.find("\n---", 3)
    if end == -1:
        return None
    frontmatter_text = content[3:end].strip()
    try:
        data = yaml.safe_load(frontmatter_text)
    except yaml.YAMLError as exc:
        logger.warning("Malformed YAML frontmatter in %s: %s", file_path, exc)
        return None
    if isinstance(data, dict):
        value = data.get("ticket_id")
        return str(value) if value is not None else None
    return None


@mcp_server.tool()
async def session_update(
    session_id: str,
    zing_file: str | None = None,
    title: str | None = None,
    ticket_id: str | None = None,
) -> dict:
    """Update session title, zing file path, and/or ticket ID.

    Precedence for ticket_id:
    - If ``ticket_id`` is provided explicitly, it is used directly.
    - If ``ticket_id`` is None and ``zing_file`` is provided, the file's YAML
      frontmatter is parsed and ``ticket_id`` is extracted from there if present.
    - A malformed or unreadable zing_file does not raise; it is silently skipped.
    """
    sm = _get_session_manager()

    if zing_file is not None:
        p = Path(zing_file)
        if not p.is_absolute():
            return {
                "error": (
                    f"zing_file must be an absolute path, got {zing_file!r}. "
                    "Resolve via os.path.abspath() or Path.resolve() before submitting."
                )
            }
        if not p.exists():
            return {
                "error": (
                    f"zing_file does not exist: {zing_file}. "
                    "Write the file first, then call session_update."
                )
            }

    resolved_ticket_id = ticket_id
    if resolved_ticket_id is None and zing_file is not None:
        resolved_ticket_id = _extract_ticket_id_from_frontmatter(zing_file)

    try:
        session = sm.update_session(
            session_id, zing_file=zing_file, title=title, ticket_id=resolved_ticket_id
        )
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
async def step_stop(session_id: str, step_id: str) -> dict:
    """Complete a step (STARTED -> READY) after running its validators.

    Short-circuits on the first failing validator. Returns
    ``{"error": "<formatted errors>"}`` on validation failure so Claude can
    fix the underlying issue and retry. Only ``json.JSONDecodeError`` and
    ``ValidationException`` are caught — server bugs (KeyError, OSError,
    etc.) propagate as 500s so Claude doesn't retry-loop on something it
    can't fix.
    """
    sm = _get_session_manager()
    try:
        sess, step = sm.get_step_by_id(step_id)
        for validator in STEP_VALIDATORS.get(step.step_name, []):
            errors = validator(sess, step)
            if errors:
                return {"error": "\n\n".join(errors)}
        sm.mark_step_ready(session_id, step_id)
    except (json.JSONDecodeError, ValidationException) as exc:
        return {"error": str(exc)}
    return {"status": "ready", "step_id": step_id, "step_name": step.step_name}


@mcp_server.tool()
async def agent_start(
    session_id: str,
    step_id: str,
    name: str,
    description: str = "",
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
        review_response = await sm.wait_for_review(session_id, step_id)
    except KeyError as exc:
        return {"error": str(exc)}
    result = review_response.model_dump()
    result["review_url"] = url
    return result


@mcp_server.tool()
async def step_log(session_id: str, step_id: str, agent_name: str, message: str) -> dict:
    """Append a log entry to a step. Appears in dashboard under the step."""
    sm = _get_session_manager()
    try:
        entry = sm.add_log(session_id, step_id, agent_name, message)
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
    return {"status": "ok", "timestamp": entry.timestamp.isoformat()}


@mcp_server.tool()
async def viz_preview_request(
    session_id: str,
    viz_path: str,
    md_path: str,
    gate_label: str,
) -> dict:
    """Surface a viz+markdown pair in the Command Center Flow for human review.

    Validates the viz JSON against the schema before queueing. If validation
    fails, the preview is NOT queued — the caller must fix the file and retry.

    ``gate_label`` is shown in the Flow card and the drawer header (e.g.
    "Topology review (round 1)" or "Final audit review"). Replacing an
    existing pending preview resolves its waiter as a reject so the prior
    skill cycle can recover instead of hanging.

    After this returns ``{"status": "requested"}``, call ``viz_preview_wait``
    to block until the user clicks Accept or Reject.
    """
    from zing_ai.viz.validate import validate as viz_validate

    sm = _get_session_manager()
    viz = Path(viz_path)
    md = Path(md_path)
    if not viz.exists():
        return {"error": f"viz file not found: {viz_path}"}
    if not md.exists():
        return {"error": f"markdown file not found: {md_path}"}
    try:
        graph = json.loads(viz.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return {"error": f"failed to read viz JSON: {exc}"}

    issues = viz_validate(graph)
    if issues:
        return {
            "error": "viz validation failed",
            "issues": [
                {
                    "pointer": i.pointer,
                    "message": i.message,
                    "available": i.available,
                    "suggestion": i.suggestion,
                }
                for i in issues
            ],
        }

    try:
        preview = sm.set_viz_preview(session_id, str(viz), str(md), gate_label)
    except (KeyError, ValueError) as exc:
        return {"error": str(exc)}

    sm.add_notification(
        session_id,
        title="Viz preview ready",
        body=f"{gate_label} — review viz + markdown before submitting.",
        url=f"http://localhost:{_port}/command-center?session={session_id}",
    )

    return {
        "status": "requested",
        "iteration": preview.iteration,
        "review_url": f"http://localhost:{_port}/command-center?session={session_id}",
    }


@mcp_server.tool()
async def viz_preview_wait(session_id: str) -> dict:
    """Block until the user accepts or rejects the pending viz preview.

    Returns ``{"decision": "accept"|"reject", "comments": str}``. Comments may
    be empty. On reject, read the comments and discuss with the user in the
    Claude Code session before editing the artifact and (optionally) calling
    ``viz_preview_request`` again for another review round.
    """
    sm = _get_session_manager()
    try:
        decision = await sm.wait_for_viz_preview(session_id)
    except KeyError as exc:
        return {"error": str(exc)}
    return {"decision": decision.decision, "comments": decision.comments}


@mcp_server.tool()
async def notification_send(
    session_id: str,
    title: str,
    body: str = "",
    url: str | None = None,
) -> dict:
    """Send a browser notification tied to a Zing session."""
    if len(title) > 200:
        return {"error": "title must be 200 characters or fewer"}
    if len(body) > 1000:
        return {"error": "body must be 1000 characters or fewer"}
    if url is not None and not url.startswith(("http://", "https://")):
        return {"error": "url must use http:// or https:// scheme"}
    sm = _get_session_manager()
    try:
        notification = sm.add_notification(session_id, title, body, url)
        return {"status": "sent", "notification_id": notification.id}
    except (ValueError, KeyError) as exc:
        return {"error": str(exc)}
