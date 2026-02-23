"""SSE event helpers for Datastar integration.

Each helper returns a Datastar-compatible SSE event string that patches
DOM elements via the ``datastar-py`` SDK.
"""

from __future__ import annotations

import html
import json

from datastar_py import ServerSentEventGenerator as SSE


def progress_event(subprocess_id: str, status: str, message: str) -> object:
    """Return an SSE event that patches a subprocess progress indicator.

    Updates the status badge and message for the given subprocess card.
    """
    safe_id = html.escape(subprocess_id, quote=True)
    safe_status = html.escape(status, quote=True)
    safe_message = html.escape(message)

    badge = (
        f'<span id="badge-{safe_id}" '
        f'class="badge badge--{safe_status}">'
        f"{safe_status.capitalize()}</span>"
    )
    msg = f'<span id="msg-{safe_id}" class="process-message">{safe_message}</span>'
    return SSE.patch_elements(badge + msg)


def output_event(subprocess_id: str, line: str) -> object:
    """Return an SSE event that appends a line to a subprocess log panel."""
    safe_id = html.escape(subprocess_id, quote=True)
    safe_line = html.escape(line)

    fragment = f'<span id="line-{safe_id}">{safe_line}\n</span>'
    return SSE.patch_elements(
        fragment,
        selector=f"#log-{safe_id}",
        mode="append",
    )


def completion_event(subprocess_id: str, result: str) -> object:
    """Return an SSE event that marks a subprocess as complete."""
    safe_id = html.escape(subprocess_id, quote=True)
    safe_result = html.escape(result)

    fragment = (
        f'<span id="badge-{safe_id}" '
        f'class="badge badge--success">Done</span>'
    )
    result_fragment = (
        f'<div id="result-{safe_id}" class="process-result">'
        f"{safe_result}</div>"
    )
    return SSE.patch_elements(fragment + result_fragment)


def error_event(subprocess_id: str, error: str) -> object:
    """Return an SSE event that displays an error for a subprocess."""
    safe_id = html.escape(subprocess_id, quote=True)
    safe_error = html.escape(error)

    fragment = (
        f'<span id="badge-{safe_id}" '
        f'class="badge badge--failed">Failed</span>'
    )
    error_fragment = (
        f'<div id="error-{safe_id}" class="log-error">'
        f"{safe_error}</div>"
    )
    return SSE.patch_elements(fragment + error_fragment)


def notify_event(title: str, body: str) -> object:
    """Return an SSE event that triggers a browser desktop notification.

    Appends a script tag to the body that calls the global
    ``showNotification()`` function defined in ``base.html``.
    """
    # Use json.dumps for proper JS string escaping (handles </script>,
    # backslashes, newlines, quotes, and all other special characters).
    safe_title = json.dumps(title)
    safe_body = json.dumps(body)

    script = f"<script>showNotification({safe_title}, {safe_body})</script>"
    return SSE.patch_elements(script, selector="body", mode="append")
