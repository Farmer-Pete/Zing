"""Pure HTML fragment helpers shared by route modules.

These functions return HTML strings used by SSE patches and route responses.
They have no dependency on FastAPI or any router state.
"""

from __future__ import annotations

import html
import json

from fastapi.responses import JSONResponse

from zing_ai.server.models import Notification


def submitted_status_html() -> str:
    """Return the 'Review submitted' banner HTML."""
    return '<div id="review-status" class="submit-banner">Review submitted — thank you!</div>'


def submitted_button_html() -> str:
    """Return the disabled 'Review submitted' button HTML."""
    return (
        '<div id="submit-section">'
        '<button class="submit-btn submit-btn--done"'
        " disabled>Review submitted</button></div>"
    )


def ready_status_html() -> str:
    """Return the 'ready for review' banner HTML."""
    return (
        '<div id="review-status" class="submit-banner">All agents complete — ready for review</div>'
    )


def ready_button_html(session_id: str) -> str:
    """Return the Submit Review button HTML."""
    return (
        '<div id="submit-section">'
        f'<button class="submit-btn" '
        f"data-on:click=\"@post('/{html.escape(session_id)}/submit')\">"
        "Submit Review</button></div>"
    )


def session_not_found(session_id: str) -> JSONResponse:
    """Return a 404 response for an unknown session."""
    return JSONResponse(
        status_code=404,
        content={
            "error": "session_not_found",
            "message": f"Session '{session_id}' does not exist",
        },
    )


def notification_dot_html(
    tab_id: str,
    href: str,
    label: str,
    badge_html: str = "",
) -> str:
    """Return a Datastar-compatible element that adds the notification-dot class to a tab.

    The returned ``<a>`` must include the full inner content (label, badge span)
    because ``ElementPatchMode.OUTER`` replaces the entire element.
    """
    return (
        f'<a id="{html.escape(tab_id)}" '
        f'href="{html.escape(href)}" '
        f'class="step-link notification-dot">'
        f"{html.escape(label)}"
        f"{badge_html}"
        f"</a>"
    )


def build_notification_script(notif: Notification, default_on_click_js: str) -> str:
    """Build a browser Notification JS snippet from a Notification model."""
    title_js = json.dumps(notif.title)
    opts: dict[str, str] = {}
    if notif.body:
        opts["body"] = notif.body
    opts_js = json.dumps(opts)
    if notif.url:
        url_js = json.dumps(notif.url)
        on_click_js = f"window.location.href = {url_js}; n.close();"
    else:
        on_click_js = default_on_click_js
    return (
        f"if (Notification.permission === 'granted') {{"
        f"  const n = new Notification({title_js}, {opts_js});"
        f"  n.onclick = () => {{ {on_click_js} }};"
        f"}}"
    )
