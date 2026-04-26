"""SSE helper functions for common Datastar patch-elements patterns.

These functions return ``DatastarEvent`` objects (string subclass) produced by
``SSE.patch_elements``, ready to be yielded inside an SSE route generator.
"""

from __future__ import annotations

import json
from html import escape as html_escape
from typing import Literal
from uuid import uuid4

from datastar_py import ServerSentEventGenerator as SSE
from datastar_py.consts import ElementPatchMode
from datastar_py.sse import DatastarEvent


def sse_toast(
    message: str,
    kind: Literal["ok", "err", "info"] = "info",
    *,
    toast_id: str | None = None,
) -> DatastarEvent:
    """Yield an APPEND patch that adds a toast to #cc-toast-container.

    Toast self-removes after 5s via data-on-load__delay.5000ms (Decision #2).
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
    reset_html: str | None = None,
    reset_after_ms: int = 2000,
) -> DatastarEvent:
    """Yield an OUTER patch replacing the button.

    For post-completion ok/err copy. If reset_html is provided, the patched
    button auto-restores the original interactive markup via
    data-init__delay.{reset_after_ms}ms="el.outerHTML = ..."
    so the button doesn't get stuck non-interactive.
    """
    cls = {"idle": "btn", "ok": "btn btn-ok", "err": "btn btn-err"}[kind]
    dis = " disabled" if disabled else ""
    if reset_html is not None:
        reset_attr = (
            f'data-init__delay.{reset_after_ms}ms="el.outerHTML = {json.dumps(reset_html)}"'
        )
    else:
        reset_attr = ""
    html = f'<button id="{button_id}" class="{cls}"{dis} {reset_attr}>{html_escape(label)}</button>'
    return SSE.patch_elements(html, selector=f"#{button_id}", mode=ElementPatchMode.OUTER)
