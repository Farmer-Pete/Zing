"""
Zellij reverse proxy module.

Provides ``create_zellij_router()`` which returns a FastAPI ``APIRouter``
mounting all routes needed to proxy the Zellij web terminal into the Zing
server — HTTP pages, static assets (with JS patches), WebSocket terminals,
and Zellij's own command/session/info APIs.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from collections.abc import Callable
from dataclasses import dataclass

import httpx
from fastapi import APIRouter, WebSocket, WebSocketDisconnect
from starlette.requests import Request
from starlette.responses import Response

_log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

_DEFAULT_ZELLIJ_WEB_PORT = 8082


def _get_zellij_origin(request: Request) -> str:
    """Return the Zellij web server origin URL, reading the port from app.state."""
    port = getattr(request.app.state, "zellij_web_port", _DEFAULT_ZELLIJ_WEB_PORT)
    return f"http://127.0.0.1:{port}"


def _get_zellij_port(request: Request) -> int:
    """Return the Zellij web server port from app.state."""
    return getattr(request.app.state, "zellij_web_port", _DEFAULT_ZELLIJ_WEB_PORT)


# ---------------------------------------------------------------------------
# JS asset patches
# ---------------------------------------------------------------------------
#
# Zellij ships JS we cannot otherwise modify. Each behavioral fix lives as
# a `JsPatch` in the `JS_PATCHES` registry below. The asset proxy iterates
# the registry on every `/assets/<file>` request, applying any patches
# whose ``target_file`` matches.
#
# Failure mode is loud-but-soft: if a patch's anchor string disappears on a
# Zellij upgrade, ``_replace_once`` logs a one-time warning and serves the
# unpatched asset. The terminal stays usable; one feature stops working
# until the patch is re-anchored.
#
# When upgrading Zellij, watch stderr / Sentry for
# ``zellij_proxy: patch '<name>' anchor not found`` — that names the patch
# whose anchor needs to be regenerated against the new bundle. The
# ``prototypes/zellij-shift-click`` and ``prototypes/zellij-web`` harnesses
# dump every Zellij asset to disk on first request, which is the easiest
# way to grep for a new anchor.

# One-shot: each patch warns at most once per process so a persistent
# upstream change doesn't flood logs.
_patch_anchor_warned: set[str] = set()


@dataclass(frozen=True)
class JsPatch:
    """A single text transformation to apply to a Zellij JS asset.

    Patches are pure: ``apply(text) -> text``. They never raise; if an
    expected anchor is missing the helper logs a warning and returns the
    input unchanged so the unpatched asset is still served.
    """

    name: str
    """Short id used in logs and the registry. Format ``file/feature``."""

    target_file: str
    """The asset path under ``/assets/`` this patch applies to."""

    why: str
    """One-sentence reason this patch exists."""

    apply: Callable[[str], str]
    """Pure transform from raw asset text to patched asset text."""


def _replace_once(name: str, target: str, replacement: str) -> Callable[[str], str]:
    """Build an ``apply`` that runs a single anchored replace, logging a
    one-time warning when the anchor is missing."""

    def _apply(text: str) -> str:
        if target not in text:
            if name not in _patch_anchor_warned:
                _patch_anchor_warned.add(name)
                _log.warning(
                    "zellij_proxy: patch %r anchor not found in served asset; "
                    "Zellij upstream may have changed. Serving unpatched asset.",
                    name,
                )
            return text
        return text.replace(target, replacement, 1)

    return _apply


def _append(suffix: str) -> Callable[[str], str]:
    """Build an ``apply`` that appends ``suffix`` to the asset text. For
    additive patches that don't anchor on existing source text."""

    def _apply(text: str) -> str:
        return text + suffix

    return _apply


# --- input.js: Cmd+C / Cmd+A passthrough + drop modifier-only keypress -----
#
# Zellij excludes Shift/Alt/Ctrl by keyCode but misses Meta. Without the
# modifier-only branch, pressing Cmd alone sends ``\x1b[77;9u`` (because
# ``ev.key.charCodeAt(0)`` of "Meta" is 77 = 'M') and the shell renders an
# "m"/"M" in the terminal.
_CMD_PASSTHROUGH_TARGET = """\
            if (isMac() && ev.key == "v" && ev.metaKey) {
                // pass cmd-v onwards so that paste is interpreted by xterm.js
                return;
            }"""
_CMD_PASSTHROUGH_REPLACEMENT = """\
            if (isMac() && ev.key == "v" && ev.metaKey) {
                // pass cmd-v onwards so that paste is interpreted by xterm.js
                return;
            }
            if (isMac() && ev.key == "c" && ev.metaKey) {
                // pass cmd-c onwards so that copy is interpreted by the browser
                return;
            }
            if (isMac() && ev.key == "a" && ev.metaKey) {
                // pass cmd-a onwards so that select all works
                return;
            }
            if (ev.key == "Meta" || ev.key == "Control" || ev.key == "Shift" || ev.key == "Alt") {
                // Modifier-only keypress: don't encode. Zellij's encode_kitty_key
                // does ev.key.charCodeAt(0) which for "Meta" is 77 ('M'), causing a
                // spurious "m"/"M" to appear in the terminal when Cmd is pressed alone.
                return;
            }"""


# --- input.js: Shift+Enter → kitty CSI \x1b[13;2u --------------------------
#
# xterm.js sends plain ``\r`` for Shift+Enter (1 modifier), so the shell
# can't distinguish it from plain Enter. The kitty protocol encodes it as
# ``\x1b[13;2u`` (key=13/CR, modifier=2/Shift), which shells like fish and
# zsh's vi-mode can hook.
_SHIFT_ENTER_TARGET = """\
            if (
                (modifiers_count > 1 || ev.metaKey) &&"""
_SHIFT_ENTER_REPLACEMENT = """\
            if (ev.key === "Enter" && ev.shiftKey && modifiers_count === 1) {
                ev.preventDefault();
                sendFunction("\\x1b[13;2u");
                return false;
            }
            if (
                (modifiers_count > 1 || ev.metaKey) &&"""


# --- terminal.js: Shift+Click on URL opens a new tab even in mouse mode ----
#
# xterm.js (Zellij's bundled version) does not honor the XTerm convention
# of "shift suppresses mouse-reporting and lets the click reach the link
# provider." When a program inside the pane enables xterm mouse mode
# (Claude Code, less, vim, …), shift+click is encoded as a wire-protocol
# mouse CSI and the link-provider's ``activate`` callback never fires —
# even though Zellij's ``links.js`` shows a "Shift-Click: <url>" hover hint
# that suggests it should work.
#
# Fix: capture-phase ``mousedown`` listener that runs *before* xterm.js's
# own mouse handler. On shift + left-click we look up the URL at the click
# cell, ``preventDefault + stopImmediatePropagation`` so xterm.js never
# encodes the click, and ``window.open()`` directly.
_SHIFT_CLICK_FIX = r"""
;(function(){
  // Same regex addon-web-links.js uses internally; copied to avoid
  // depending on its private export shape.
  const URL_RE = /(https?|HTTPS?):[/]{2}[^\s"'!*(){}|\\^<>`]*[^\s"':,.!?{}|\\^~\[\]`()<>]/;
  function findUrl(term, col, row) {
    const line = term.buffer.active.getLine(row);
    if (!line) return null;
    const text = line.translateToString(true);
    const re = new RegExp(URL_RE.source, "g");
    let m;
    while ((m = re.exec(text))) {
      if (col >= m.index && col < m.index + m[0].length) return m[0];
    }
    return null;
  }
  function install(term) {
    const screen = term.element && term.element.querySelector('.xterm-screen');
    const surface = screen || term.element;
    if (!surface || surface.__zingShiftClickInstalled) return;
    surface.__zingShiftClickInstalled = true;
    surface.addEventListener("mousedown", (ev) => {
      if (!ev.shiftKey || ev.button !== 0) return;
      const rect = surface.getBoundingClientRect();
      const cw = rect.width  / term.cols;
      const ch = rect.height / term.rows;
      const col = Math.floor((ev.clientX - rect.left) / cw);
      const visRow = Math.floor((ev.clientY - rect.top) / ch);
      const row = term.buffer.active.viewportY + visRow;
      const url = findUrl(term, col, row);
      if (!url) return;
      ev.preventDefault();
      ev.stopPropagation();
      ev.stopImmediatePropagation();
      const w = window.open(url, "_blank");
      if (w) { try { w.opener = null; } catch (e) {} }
    }, true /* capture phase */);
  }
  // window.term is a debug global set by Zellij's terminal.js after
  // initTerminal(). Poll briefly until it appears, then bail with a
  // warning if Zellij upgraded and removed the global.
  let attempts = 0;
  const handle = setInterval(() => {
    if (window.term && window.term.element) {
      install(window.term);
      clearInterval(handle);
    } else if (++attempts > 100) {
      clearInterval(handle);
      console.warn("[zing] shift-click patch: window.term never appeared");
    }
  }, 50);
})();
"""


JS_PATCHES: tuple[JsPatch, ...] = (
    JsPatch(
        name="input-js/cmd-passthrough",
        target_file="input.js",
        why=(
            "Pass Cmd+C/Cmd+A through to the browser; drop bare-modifier "
            "keypresses so Cmd alone doesn't render as 'M' in the terminal."
        ),
        apply=_replace_once(
            "input-js/cmd-passthrough",
            _CMD_PASSTHROUGH_TARGET,
            _CMD_PASSTHROUGH_REPLACEMENT,
        ),
    ),
    JsPatch(
        name="input-js/shift-enter",
        target_file="input.js",
        why=(
            "Encode Shift+Enter as the kitty CSI \\x1b[13;2u so shells can "
            "distinguish it from plain Enter."
        ),
        apply=_replace_once(
            "input-js/shift-enter",
            _SHIFT_ENTER_TARGET,
            _SHIFT_ENTER_REPLACEMENT,
        ),
    ),
    JsPatch(
        name="terminal-js/shift-click",
        target_file="terminal.js",
        why=(
            "Open URLs on Shift+Click even when an inner program has "
            "enabled xterm mouse-reporting (xterm.js doesn't honor the "
            "XTerm shift-suppresses-mouse convention)."
        ),
        apply=_append(_SHIFT_CLICK_FIX),
    ),
)

# Headers that must not be forwarded between proxy hops.
_HOP_BY_HOP = frozenset({"transfer-encoding", "connection", "content-encoding", "content-length"})
_REQUEST_EXCLUDE = frozenset({"host", "connection", "transfer-encoding"})


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _forward_headers(request: Request) -> dict[str, str]:
    """Strip hop-by-hop headers from an incoming request before forwarding."""
    return {k: v for k, v in request.headers.items() if k.lower() not in _REQUEST_EXCLUDE}


def _response_headers(resp: httpx.Response) -> dict[str, str]:
    """Strip hop-by-hop headers from an upstream response before returning."""
    return {k: v for k, v in resp.headers.items() if k.lower() not in _HOP_BY_HOP}


def _check_available(request: Request) -> Response | None:
    """Return a 503 Response if Zellij is unavailable, else None."""
    if not getattr(request.app.state, "zellij_available", False):
        return Response(content="Zellij unavailable", status_code=503)
    return None


def _get_client(request: Request) -> httpx.AsyncClient:
    return request.app.state.zellij_http_client


async def _proxy_ws(
    ws: WebSocket, upstream_path: str, session_cookie: str | None, port: int
) -> None:
    """Proxy a WebSocket connection to the Zellij web server."""
    import websockets  # type: ignore[import-untyped]

    await ws.accept()

    ws_url = f"ws://127.0.0.1:{port}{upstream_path}"
    if ws.query_params:
        ws_url += f"?{ws.query_params}"

    extra_headers = {"Cookie": f"session_token={session_cookie}"} if session_cookie else {}

    try:
        async with websockets.connect(ws_url, additional_headers=extra_headers) as upstream:

            async def client_to_upstream() -> None:
                try:
                    while True:
                        data = await ws.receive()
                        if "text" in data:
                            await upstream.send(data["text"])
                        elif "bytes" in data:
                            await upstream.send(data["bytes"])
                except WebSocketDisconnect:
                    pass

            async def upstream_to_client() -> None:
                try:
                    async for msg in upstream:
                        if isinstance(msg, str):
                            await ws.send_text(msg)
                        elif isinstance(msg, bytes):
                            await ws.send_bytes(msg)
                except WebSocketDisconnect:
                    pass

            await asyncio.gather(client_to_upstream(), upstream_to_client())

    except Exception as exc:  # noqa: BLE001
        # Swallow connection errors — the client already disconnected or the
        # upstream is gone; nothing actionable to do here.
        _ = exc
    finally:
        with contextlib.suppress(Exception):
            await ws.close()


# ---------------------------------------------------------------------------
# Router factory
# ---------------------------------------------------------------------------


def create_zellij_router() -> APIRouter:
    """Return an APIRouter with all Zellij reverse-proxy routes."""

    router = APIRouter()

    # ------------------------------------------------------------------
    # HTTP proxy: Zellij session pages
    # ------------------------------------------------------------------

    @router.api_route("/zellij/{path:path}", methods=["GET", "POST", "PUT", "DELETE"])
    async def proxy_http(request: Request, path: str) -> Response:
        """Proxy HTTP requests to the Zellij web server."""
        if (err := _check_available(request)) is not None:
            return err

        client = _get_client(request)
        origin = _get_zellij_origin(request)
        url = f"{origin}/{path}"
        if request.query_params:
            url += f"?{request.query_params}"

        body = await request.body()
        resp = await client.request(
            method=request.method,
            url=url,
            headers=_forward_headers(request),
            content=body if body else None,
        )
        return Response(
            content=resp.content,
            status_code=resp.status_code,
            headers=_response_headers(resp),
            media_type=resp.headers.get("content-type"),
        )

    # ------------------------------------------------------------------
    # HTTP proxy: static assets (with JS patching for input.js)
    # ------------------------------------------------------------------

    @router.get("/assets/{path:path}")
    async def proxy_assets(request: Request, path: str) -> Response:
        """Proxy Zellij static assets, applying any registered JS patches."""
        if (err := _check_available(request)) is not None:
            return err

        client = _get_client(request)
        origin = _get_zellij_origin(request)
        resp = await client.get(f"{origin}/assets/{path}")
        headers = _response_headers(resp)

        content = resp.content
        patches = [p for p in JS_PATCHES if p.target_file == path]
        if patches:
            text = content.decode()
            for patch in patches:
                text = patch.apply(text)
            content = text.encode()

        return Response(
            content=content,
            status_code=resp.status_code,
            headers=headers,
            media_type=resp.headers.get("content-type"),
        )

    # ------------------------------------------------------------------
    # WebSocket proxy: terminal data channel
    # ------------------------------------------------------------------

    @router.websocket("/ws/terminal/{path:path}")
    async def proxy_ws_terminal(ws: WebSocket, path: str) -> None:
        """Proxy terminal WebSocket to Zellij."""
        cookie = getattr(ws.app.state, "zellij_session_cookie", None)
        port = getattr(ws.app.state, "zellij_web_port", _DEFAULT_ZELLIJ_WEB_PORT)
        await _proxy_ws(ws, f"/ws/terminal/{path}", cookie, port)

    # ------------------------------------------------------------------
    # WebSocket proxy: control channel
    # ------------------------------------------------------------------

    @router.websocket("/ws/control")
    async def proxy_ws_control(ws: WebSocket) -> None:
        """Proxy Zellij control WebSocket."""
        cookie = getattr(ws.app.state, "zellij_session_cookie", None)
        port = getattr(ws.app.state, "zellij_web_port", _DEFAULT_ZELLIJ_WEB_PORT)
        await _proxy_ws(ws, "/ws/control", cookie, port)

    # ------------------------------------------------------------------
    # HTTP proxy: command API
    # ------------------------------------------------------------------

    @router.api_route("/command/{path:path}", methods=["GET", "POST", "PUT", "DELETE"])
    async def proxy_command(request: Request, path: str) -> Response:
        """Proxy Zellij command endpoints."""
        if (err := _check_available(request)) is not None:
            return err

        client = _get_client(request)
        origin = _get_zellij_origin(request)
        body = await request.body()
        resp = await client.request(
            method=request.method,
            url=f"{origin}/command/{path}",
            headers=_forward_headers(request),
            content=body if body else None,
        )
        headers = _response_headers(resp)
        return Response(
            content=resp.content,
            status_code=resp.status_code,
            headers=headers,
            media_type=resp.headers.get("content-type"),
        )

    # ------------------------------------------------------------------
    # HTTP proxy: session API
    # ------------------------------------------------------------------

    @router.api_route("/session", methods=["GET", "POST", "PUT", "DELETE"])
    async def proxy_session(request: Request) -> Response:
        """Proxy Zellij session endpoint."""
        if (err := _check_available(request)) is not None:
            return err

        client = _get_client(request)
        origin = _get_zellij_origin(request)
        body = await request.body()
        resp = await client.request(
            method=request.method,
            url=f"{origin}/session",
            headers=_forward_headers(request),
            content=body if body else None,
        )
        headers = _response_headers(resp)
        return Response(
            content=resp.content,
            status_code=resp.status_code,
            headers=headers,
            media_type=resp.headers.get("content-type"),
        )

    # ------------------------------------------------------------------
    # HTTP proxy: info API
    # ------------------------------------------------------------------

    @router.get("/info/{path:path}")
    async def proxy_info(request: Request, path: str) -> Response:
        """Proxy Zellij info endpoints."""
        if (err := _check_available(request)) is not None:
            return err

        client = _get_client(request)
        origin = _get_zellij_origin(request)
        resp = await client.get(f"{origin}/info/{path}")
        return Response(content=resp.content, status_code=resp.status_code)

    return router
