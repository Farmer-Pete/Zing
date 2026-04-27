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

# Whitelist of characters allowed in id-bearing parameters. Constraining the
# format upfront eliminates the need for downstream HTML/CSS-selector escaping.
_ID_RE = re.compile(r"^[A-Za-z0-9_-]+$")
_BUTTON_ID_RE = _ID_RE  # historic alias

_KIND_VALUES = frozenset({"ok", "err", "info"})


def sse_toast(
    message: str,
    kind: Literal["ok", "err", "info"] = "info",
    *,
    toast_id: str | None = None,
) -> DatastarEvent:
    """Yield an APPEND patch that adds a toast to #cc-toast-container.

    Toast self-removes after 5s via data-init__delay.5000ms.

    Args:
        message: User-visible toast text — HTML-escaped before insertion.
        kind: One of ``"ok"`` / ``"err"`` / ``"info"``.
        toast_id: Optional stable id (for dedupe). Must match ``[A-Za-z0-9_-]+``.

    Raises:
        ValueError: If ``kind`` is not in the allowlist or ``toast_id`` is invalid.
    """
    if kind not in _KIND_VALUES:
        msg = f"Invalid kind {kind!r}: must be one of {sorted(_KIND_VALUES)}"
        raise ValueError(msg)
    if toast_id is not None and not _ID_RE.fullmatch(toast_id):
        msg = f"Invalid toast_id {toast_id!r}: must match [A-Za-z0-9_-]+"
        raise ValueError(msg)
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
    class_: str,
    attrs: str = "",
    kind: Literal["idle", "ok", "err"] = "idle",
    disabled: bool = False,
) -> DatastarEvent:
    """Yield an OUTER patch replacing the button.

    Used to surface a post-completion ok/err copy on the button itself ("✓ Launched!",
    "Failed"). Callers must pass the originating ``class_`` and any Datastar/aria
    ``attrs`` so the replacement preserves all bindings the page depends on for
    re-arming. The kind suffix (``btn-ok`` / ``btn-err``) is appended to ``class_``.

    Args:
        button_id: DOM id of the target button. Must match ``[A-Za-z0-9_-]+``.
        label: Visible label text — HTML-escaped before insertion.
        class_: The originating button's class attribute (preserved verbatim).
        attrs: Optional pre-rendered Datastar/aria attributes string (e.g.
            ``'data-on:click="…" data-indicator="…"'``). Caller is responsible
            for ensuring values are correctly escaped.
        kind: Visual variant — ``idle`` / ``ok`` / ``err``. Adds a class suffix.
        disabled: When True, renders the ``disabled`` attribute on the button.

    Raises:
        ValueError: If ``button_id`` contains characters outside the whitelist.
    """
    if not _ID_RE.fullmatch(button_id):
        msg = f"Invalid button_id {button_id!r}: must match [A-Za-z0-9_-]+"
        raise ValueError(msg)
    suffix = {"idle": "", "ok": " btn-ok", "err": " btn-err"}[kind]
    full_cls = f"{class_}{suffix}"
    dis = " disabled" if disabled else ""
    attrs_str = f" {attrs}" if attrs else ""
    html = (
        f'<button id="{button_id}" class="{full_cls}"{attrs_str}{dis}>{html_escape(label)}</button>'
    )
    return SSE.patch_elements(html, selector=f"#{button_id}", mode=ElementPatchMode.OUTER)
