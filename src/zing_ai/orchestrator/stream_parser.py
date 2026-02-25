"""Parse JSONL events from ``claude --output-format stream-json --verbose``.

Provides helpers for parsing individual JSONL lines, formatting events for
human-readable display, and extracting metadata (session IDs, result text)
from the event stream.
"""

from __future__ import annotations

import json
import logging

logger = logging.getLogger(__name__)

# Well-known tool input keys worth displaying, ordered by priority.
# For any tool_use block, the first matching key becomes the detail text.
_TOOL_DETAIL_KEYS: tuple[str, ...] = (
    "command",           # Bash
    "file_path",         # Read/Write/Edit
    "relative_path",     # Serena tools
    "pattern",           # Glob/Grep/search_for_pattern
    "description",       # Task
    "name_path_pattern", # Serena find_symbol
    "memory_name",       # Serena read_memory
    "query",             # Query-style tools
)

_MAX_DETAIL_LEN = 80


def parse_event(line: str) -> dict | None:
    """Parse a single JSONL line into an event dict.

    Parameters
    ----------
    line:
        A raw line from Claude's stdout (may include trailing newline).

    Returns
    -------
    dict | None
        The parsed JSON object, or ``None`` for empty/invalid lines.
    """
    stripped = line.strip()
    if not stripped:
        return None
    try:
        return json.loads(stripped)
    except json.JSONDecodeError:
        logger.debug("Skipping non-JSON line: %s", stripped[:200])
        return None


def format_event(event: dict) -> str | None:
    """Format a parsed JSONL event for human-readable display.

    Parameters
    ----------
    event:
        A parsed event dict from :func:`parse_event`.

    Returns
    -------
    str | None
        A formatted string for display, or ``None`` if the event should
        be silently skipped.

    Display rules:

    - ``system/init``: Skip (silent)
    - ``assistant`` with ``text`` content block: Output the text
    - ``assistant`` with ``thinking`` content block: Skip
    - ``assistant`` with ``tool_use`` content block: Output ``"Tool: {name}\\n"``
    - ``user`` (tool results): Skip
    - ``result/success``: Output cost/duration summary
    - ``rate_limit_event``: Skip
    - ``system/task_started``: Output ``"Task: {description}\\n"``
    """
    event_type = event.get("type")

    if event_type == "system":
        subtype = event.get("subtype")
        if subtype == "init":
            return None
        if subtype == "task_started":
            description = event.get("description", "")
            return f"Task: {description}\n"
        return None

    if event_type == "assistant":
        return _format_assistant_event(event)

    if event_type == "user":
        return None

    if event_type == "result":
        subtype = event.get("subtype")
        if subtype == "success":
            return _format_result_success(event)
        return None

    if event_type == "rate_limit_event":
        return None

    # Unknown event types are silently skipped
    return None


def _format_tool_use(block: dict) -> str:
    """Format a tool_use content block for concise terminal display.

    Scans :data:`_TOOL_DETAIL_KEYS` for the first matching input key and
    appends its value in parentheses.  Long values are truncated and
    newlines are collapsed to spaces.
    """
    name = block.get("name", "unknown")
    tool_input = block.get("input") or {}

    for key in _TOOL_DETAIL_KEYS:
        value = tool_input.get(key)
        if value is not None and isinstance(value, str) and value:
            text = value.replace("\n", " ").strip()
            if len(text) > _MAX_DETAIL_LEN:
                text = text[:_MAX_DETAIL_LEN] + "..."
            return f"Tool: {name} ({text})\n"

    return f"Tool: {name}\n"


def _format_assistant_event(event: dict) -> str | None:
    """Format an assistant-type event."""
    message = event.get("message", {})
    content_blocks = message.get("content", [])

    parts: list[str] = []
    for block in content_blocks:
        block_type = block.get("type")
        if block_type == "text":
            text = block.get("text", "")
            if text:
                parts.append(text)
        elif block_type == "tool_use":
            parts.append(_format_tool_use(block))
        # thinking blocks are silently skipped

    if not parts:
        return None
    return "".join(parts)


def _format_result_success(event: dict) -> str | None:
    """Format a result/success event with cost and duration."""
    duration = event.get("duration_seconds")
    cost = event.get("total_cost_usd")

    parts: list[str] = []
    if duration is not None:
        parts.append(f"Completed in {duration:.1f}s")
    if cost is not None:
        parts.append(f"Cost: ${cost:.4f}")

    if parts:
        return " | ".join(parts) + "\n"
    return None


def extract_session_id(event: dict) -> str | None:
    """Extract the session ID from a ``system/init`` event.

    Parameters
    ----------
    event:
        A parsed event dict.

    Returns
    -------
    str | None
        The session ID string, or ``None`` if this is not an init event
        or has no session ID.
    """
    if event.get("type") == "system" and event.get("subtype") == "init":
        session_id = event.get("session_id")
        if session_id:
            return str(session_id)
    return None


def extract_result_text(event: dict) -> str | None:
    """Extract the result text from a ``result/success`` event.

    Parameters
    ----------
    event:
        A parsed event dict.

    Returns
    -------
    str | None
        The result text, or ``None`` if this is not a result event
        or has no result text.
    """
    if event.get("type") == "result" and event.get("subtype") == "success":
        result = event.get("result")
        if result:
            return str(result)
    return None


def collect_assistant_text(event: dict) -> str | None:
    """Extract text content from an assistant event for output collection.

    Unlike :func:`format_event`, this only returns raw text content blocks
    (not tool use formatting), suitable for collecting the structured
    response when no temp file is used.

    Parameters
    ----------
    event:
        A parsed event dict.

    Returns
    -------
    str | None
        The text content, or ``None`` if the event has no text.
    """
    if event.get("type") != "assistant":
        return None

    message = event.get("message", {})
    content_blocks = message.get("content", [])

    parts: list[str] = []
    for block in content_blocks:
        if block.get("type") == "text":
            text = block.get("text", "")
            if text:
                parts.append(text)

    if not parts:
        return None
    return "".join(parts)
