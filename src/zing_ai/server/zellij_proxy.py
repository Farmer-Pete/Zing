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

import httpx
from fastapi import APIRouter, WebSocket, WebSocketDisconnect
from starlette.requests import Request
from starlette.responses import Response

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
# JS patch constants — extracted verbatim from the prototype
# ---------------------------------------------------------------------------

# Patch 1: Add Cmd+C and Cmd+A passthrough alongside the existing Cmd+V passthrough.
# Also drop modifier-only key presses (Meta/Cmd alone) before they reach the kitty
# encoder — Zellij excludes Shift/Alt/Ctrl by keyCode but misses Meta. Without this,
# pressing Cmd by itself sends `\\x1b[77;9u` (because ev.key.charCodeAt(0) of "Meta"
# is 77 = 'M') and the shell renders an "m"/"M" in the terminal.
_INPUT_JS_PATCH_TARGET = """\
            if (isMac() && ev.key == "v" && ev.metaKey) {
                // pass cmd-v onwards so that paste is interpreted by xterm.js
                return;
            }"""
_INPUT_JS_PATCH_REPLACEMENT = """\
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

# Patch 2: Add Shift+Enter → kitty-encoded \x1b[13;2u before the multi-modifier block.
_INPUT_JS_PATCH2_TARGET = """\
            if (
                (modifiers_count > 1 || ev.metaKey) &&"""
_INPUT_JS_PATCH2_REPLACEMENT = """\
            if (ev.key === "Enter" && ev.shiftKey && modifiers_count === 1) {
                ev.preventDefault();
                sendFunction("\\x1b[13;2u");
                return false;
            }
            if (
                (modifiers_count > 1 || ev.metaKey) &&"""

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
        """Proxy Zellij static assets, patching input.js."""
        if (err := _check_available(request)) is not None:
            return err

        client = _get_client(request)
        origin = _get_zellij_origin(request)
        resp = await client.get(f"{origin}/assets/{path}")
        headers = _response_headers(resp)

        content = resp.content
        if path == "input.js":
            text = content.decode()
            text = text.replace(_INPUT_JS_PATCH_TARGET, _INPUT_JS_PATCH_REPLACEMENT)
            text = text.replace(_INPUT_JS_PATCH2_TARGET, _INPUT_JS_PATCH2_REPLACEMENT)
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
