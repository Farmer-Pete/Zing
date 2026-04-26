"""SSE helper functions for common Datastar patch-elements patterns.

These functions return ``DatastarEvent`` objects (string subclass) produced by
``SSE.patch_elements``, ready to be yielded inside an SSE route generator.
"""

from __future__ import annotations

import re
from html import escape as html_escape
from typing import Literal
from uuid import uuid4

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.sse import DatastarEvent

# Whitelist of characters allowed in button_id. Every legitimate caller already
# produces ids of the form ``btn-<verb>-<key>`` with safe characters; constraining
# the format upfront eliminates the need for downstream HTML/CSS-selector escaping
# (Decision: code review #2).
_BUTTON_ID_RE = re.compile(r"^[A-Za-z0-9_-]+$")


def sse_toast(
    message: str,
    kind: Literal["ok", "err", "info"] = "info",
    *,
    toast_id: str | None = None,
) -> DatastarEvent:
    """Yield an APPEND patch that adds a toast to #cc-toast-container.

    Toast self-removes after 5s via data-init__delay.5000ms (Decision #2).
    """
    tid = toast_id or f"toast-{uuid4().hex[:8]}"
    html = (
        f'<div id="{tid}" class="cc-toast cc-toast-{kind}" '
        f'data-init__delay.5000ms="el.remove()">'
        f"{html_escape(message)}</div>"
    )
    return SSE.patch_elements(html, selector="#cc-toast-container", mode=ElementPatchMode.APPEND)


def sse_btn_state(
    button_id: str,
    label: str,
    *,
    kind: Literal["idle", "ok", "err"] = "idle",
    disabled: bool = False,
) -> DatastarEvent:
    """Yield an OUTER patch replacing the button.

    Used to surface a post-completion ok/err copy on the button itself ("\u2713 Launched!",
    "Failed"). Callers that want the button to return to its original state should
    yield a ``patch_signals`` flipping the relevant ``$busyButtons`` flag back to
    ``false`` (the per-card pre-init in ``kanban_card.html`` keeps the button live).

    Args:
        button_id: DOM id of the target button. Must match ``[A-Za-z0-9_-]+``.
        label: Visible label text — HTML-escaped before insertion.
        kind: Visual variant — ``idle`` / ``ok`` / ``err``.
        disabled: When True, renders the ``disabled`` attribute on the button.

    Raises:
        ValueError: If ``button_id`` contains characters outside the whitelist.
    """
    if not _BUTTON_ID_RE.fullmatch(button_id):
        msg = f"Invalid button_id {button_id!r}: must match [A-Za-z0-9_-]+"
        raise ValueError(msg)
    cls = {"idle": "btn", "ok": "btn btn-ok", "err": "btn btn-err"}[kind]
    dis = " disabled" if disabled else ""
    html = f'<button id="{button_id}" class="{cls}"{dis}>{html_escape(label)}</button>'
    return SSE.patch_elements(html, selector=f"#{button_id}", mode=ElementPatchMode.OUTER)
