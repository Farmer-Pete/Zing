"""FastAPI application factory for the Zing batch review server."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import pathlib
import socket
import subprocess
import time
from collections.abc import AsyncGenerator

import httpx
from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from starlette.applications import Starlette
from starlette.routing import Mount
from starlette.types import ASGIApp, Message, Receive, Scope, Send

from zing_ai.config import load_config
from zing_ai.server.command_center import get_live_sessions
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.external_poller import ExternalPoller
from zing_ai.server.mcp_tools import configure, mcp_server
from zing_ai.server.models import ClaudeCodeSession
from zing_ai.server.routes import (
    _dashboard_queues,
    _notify_dashboard_connections,
    _notify_sse_connections,
    _sse_queues,
    router,
)
from zing_ai.server.routes_command_center import router as command_center_router
from zing_ai.server.routes_config import router as config_router
from zing_ai.server.routes_install import router as install_router
from zing_ai.server.sessions import SessionManager
from zing_ai.server.zellij_config import ensure_zellij_config
from zing_ai.server.zellij_proxy import create_zellij_router

logger = logging.getLogger("zing_ai.server")

_STATIC_DIR = pathlib.Path(__file__).parent / "static"


class MCPDebugMiddleware:
    """Log request/response details for /mcp requests."""

    def __init__(self, app: ASGIApp) -> None:
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http" or scope["path"] != "/mcp":
            await self.app(scope, receive, send)
            return

        method = scope.get("method", "?")
        # Decode header keys/values for logging
        header_strs = {
            k.decode("latin-1"): v.decode("latin-1") for k, v in scope.get("headers", [])
        }
        # Mask authorization header value to avoid leaking tokens
        logged_headers = {}
        for k, v in header_strs.items():
            if k in ("content-type", "accept", "mcp-session-id", "mcp-protocol-version"):
                logged_headers[k] = v
            elif k == "authorization":
                logged_headers[k] = "<redacted>"
        logger.info("MCP >>> %s /mcp headers=%s", method, logged_headers)

        # Capture request body
        body_parts: list[bytes] = []
        request_complete = False

        async def receive_wrapper() -> Message:
            nonlocal request_complete
            msg = await receive()
            if msg["type"] == "http.request":
                body_parts.append(msg.get("body", b""))
                if not msg.get("more_body", False):
                    request_complete = True
                    body = b"".join(body_parts)
                    body_preview = body[:500].decode("utf-8", errors="replace")
                    logger.info("MCP >>> body: %s", body_preview)
            return msg

        # Capture response status
        response_status = 0
        response_headers: dict[str, str] = {}

        async def send_wrapper(message: Message) -> None:
            nonlocal response_status, response_headers
            if message["type"] == "http.response.start":
                response_status = message["status"]
                response_headers = {
                    k.decode("latin-1"): v.decode("latin-1") for k, v in message.get("headers", [])
                }
                logger.info(
                    "MCP <<< %d headers=%s",
                    response_status,
                    {
                        k: v
                        for k, v in response_headers.items()
                        if k in ("content-type", "mcp-session-id")
                    },
                )
            elif message["type"] == "http.response.body":
                body = message.get("body", b"")
                if body and response_status >= 400:
                    logger.info(
                        "MCP <<< body: %s",
                        body[:500].decode("utf-8", errors="replace"),
                    )
            await send(message)

        start = time.monotonic()
        await self.app(scope, receive_wrapper, send_wrapper)
        elapsed = time.monotonic() - start
        logger.info("MCP --- %s /mcp → %d (%.3fs)", method, response_status, elapsed)


async def _start_zellij(app: FastAPI, port: int) -> None:
    """Start the Zellij web server and authenticate, storing state on app.state.

    Sets app.state.zellij_available, app.state.zellij_http_client, and
    app.state.zellij_session_cookie. Uses early returns on each failure
    rather than deep nesting.
    """
    # 1. Start Zellij web server (daemonized)
    try:
        result = subprocess.run(
            ["zellij", "web", "--start", "--daemonize", "--port", str(port)],
            capture_output=True,
            text=True,
            check=False,
        )
    except FileNotFoundError:
        logger.warning("zellij binary not found — terminal sessions unavailable")
        return
    except Exception:
        logger.exception("Zellij startup failed — terminal sessions unavailable")
        return

    if result.returncode != 0:
        logger.warning("Zellij web server failed to start: %s", result.stderr)
        return

    # 2. Create auth token
    try:
        token_result = subprocess.run(
            ["zellij", "web", "--create-token"],
            capture_output=True,
            text=True,
            check=False,
        )
    except Exception:
        logger.exception("Zellij startup failed — terminal sessions unavailable")
        return

    auth_token = None
    if token_result.returncode == 0:
        for line in token_result.stdout.strip().splitlines():
            line = line.strip()
            if line.startswith("token_") and ":" in line:
                auth_token = line.split(":", 1)[1].strip()
                break

    if not auth_token:
        logger.warning("Zellij token creation failed — terminal sessions unavailable")
        return

    # 3. Readiness probe — poll the port before login
    for _ in range(30):
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                break
        except OSError:
            await asyncio.sleep(0.1)

    # 4. Server-side login to get session cookie
    try:
        async with httpx.AsyncClient() as login_client:
            resp = await login_client.post(
                f"http://127.0.0.1:{port}/command/login",
                json={"auth_token": auth_token, "remember_me": True},
            )
            session_cookie = None
            if resp.status_code == 200:
                session_cookie = resp.cookies.get("session_token")
                if not session_cookie:
                    for header_val in resp.headers.get_list("set-cookie"):
                        if "session_token=" in header_val:
                            session_cookie = header_val.split("session_token=")[1].split(";")[0]
                            break
    except Exception:
        logger.exception("Zellij startup failed — terminal sessions unavailable")
        return

    if not session_cookie:
        logger.warning("Zellij login failed — terminal sessions unavailable")
        return

    app.state.zellij_http_client = httpx.AsyncClient(
        timeout=30.0,
        cookies={"session_token": session_cookie},
    )
    app.state.zellij_available = True
    app.state.zellij_session_cookie = session_cookie
    logger.info("Zellij web server ready")


def create_app(
    session_manager: SessionManager | None = None,
    port: int = 9876,
    external_cache: ExternalCache | None = None,
    cc_queues: list[asyncio.Queue[str]] | None = None,
    disable_polling: bool = False,
    disable_zellij: bool = False,
) -> ASGIApp:
    """Create and configure the application.

    Returns an ASGI app that routes MCP paths to the MCP sub-app
    and everything else to the FastAPI web UI.

    Args:
        session_manager: Optional SessionManager instance. Creates a default one if not provided.
        port: The port the server will listen on, used for MCP tool URL construction.
        external_cache: Optional ExternalCache instance for testing (injects pre-populated state).
        cc_queues: Optional list of asyncio queues for SSE command-center events (for testing).
        disable_zellij: If True, skip Zellij web server startup (useful in tests).
    """
    sm = session_manager or SessionManager()

    # Initialise cc_queues up-front so the session-event listener can close
    # over it. We also assign it to fastapi_app.state below for the SSE route
    # + poller; both see the same list object.
    cc_queues_list: list[asyncio.Queue[str]] = cc_queues if cc_queues is not None else []

    # SessionManager event types that should trigger a board_changed SSE refresh.
    _CC_BOARD_EVENTS: frozenset[str] = frozenset(
        {
            "session_created",
            "session_cleaned_up",
            "session_updated",
            "step_started",
            "step_ready",
            "review_submitted",
            "finding_added",
            "agents_done",
        }
    )

    def _notify_cc_connections(event: str) -> None:
        """Push an SSE event onto every connected Command Center queue."""
        for q in list(cc_queues_list):
            try:
                q.put_nowait(event)
            except asyncio.QueueFull:
                logger.warning("cc_queues queue full; dropping event %s", event)

    # Map SessionManager events to the existing SSE/dashboard notification functions
    def _on_session_event(event_type: str, session_id: str) -> None:
        sse_events = {
            "finding_added": "finding",
            "step_started": "step_started",
            "agent_started": "agent_started",
            "agent_stopped": "agent_stopped",
            "agents_done": "agents_done",
            "step_ready": "ready",
            "review_submitted": "completed",
            "log_added": "log_added",
            "session_updated": "session_updated",
        }
        dashboard_events = {
            "session_created": "created",
            "step_started": "step_started",
            "step_ready": "step_ready",
            "agents_done": "agents_done",
            "agent_started": "agent_started",
            "agent_stopped": "agent_stopped",
            "review_submitted": "review_submitted",
            "session_cleaned_up": "cleaned_up",
        }
        if event_type.startswith("notification_added:"):
            notif_id = event_type.split(":", 1)[1]
            _notify_sse_connections(session_id, f"notification:{notif_id}")
            _notify_dashboard_connections(f"notification:{notif_id}", session_id=session_id)
            _notify_cc_connections("board_changed")
            return
        if event_type in sse_events:
            _notify_sse_connections(session_id, sse_events[event_type])
        if event_type in dashboard_events:
            _notify_dashboard_connections(dashboard_events[event_type])
        # Command Center bridge: emit board_changed so the /command-center page
        # reflects local session-manager state without waiting for the next
        # external poll cycle.
        if event_type in _CC_BOARD_EVENTS:
            _notify_cc_connections("board_changed")

    sm.add_listener(_on_session_event)

    @contextlib.asynccontextmanager
    async def lifespan(app: Starlette) -> AsyncGenerator[None]:
        # Note: state is set on fastapi_app.state, NOT starlette_app.state (different objects).
        poller_task: asyncio.Task[None] | None = None
        session_poll_task: asyncio.Task[None] | None = None

        # ------------------------------------------------------------------
        # Zellij web server startup (soft — never crashes the server)
        # ------------------------------------------------------------------
        config = load_config()

        fastapi_app.state.zellij_available = False
        fastapi_app.state.zellij_http_client = None
        fastapi_app.state.zellij_session_cookie = None
        fastapi_app.state.zellij_web_port = config.command_center.zellij_web_port

        if not disable_zellij:
            ensure_zellij_config()
            await _start_zellij(fastapi_app, config.command_center.zellij_web_port)

        if not disable_polling:
            poller = ExternalPoller(
                cache=fastapi_app.state.external_cache,
                queues=fastapi_app.state.cc_queues,
                config=load_config().command_center,
            )
            fastapi_app.state.poller = poller
            poller_task = asyncio.create_task(poller.run())

            def _sync_session_alive(live: set[str]) -> None:
                """Set _session_alive on each ClaudeCodeSession based on live session names."""
                for session in sm.list_sessions():
                    if isinstance(session, ClaudeCodeSession) and session.terminal_session:
                        session._session_alive = session.terminal_session in live

            async def _poll_sessions() -> None:
                while True:
                    try:
                        live = await asyncio.to_thread(get_live_sessions)
                        fastapi_app.state.live_sessions = live
                        _sync_session_alive(live)
                    except Exception:
                        logger.exception("Session polling failed, keeping stale state")
                    await asyncio.sleep(0.5)

            session_poll_task = asyncio.create_task(_poll_sessions())
        else:
            fastapi_app.state.live_sessions = set()

        try:
            async with mcp_server.session_manager.run():
                yield
        finally:
            if poller_task is not None:
                poller_task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await poller_task
                await poller.aclose()  # type: ignore[possibly-undefined]
            if session_poll_task is not None:
                session_poll_task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await session_poll_task
            zellij_client = getattr(fastapi_app.state, "zellij_http_client", None)
            if zellij_client is not None:
                await zellij_client.aclose()
            if not disable_zellij:
                with contextlib.suppress(FileNotFoundError, OSError):
                    subprocess.run(["zellij", "web", "--stop"], check=False)

    mcp_starlette = mcp_server.streamable_http_app()

    external_cache = external_cache or ExternalCache()

    fastapi_app = FastAPI(
        title="Zing Batch Review",
        description="Batch review UI for Zing AI development pipeline",
    )
    fastapi_app.state.session_manager = sm
    fastapi_app.state.external_cache = external_cache
    # Reuse the same list object the session-event listener closed over; both
    # the SSE route and the listener mutate it as clients connect/disconnect.
    fastapi_app.state.cc_queues = cc_queues_list
    # Expose the legacy module-level SSE/dashboard queue stores via app.state
    # so new code can DI-read them (matching the cc_queues pattern). Same
    # list/dict object — transitional step toward a full migration off the
    # module globals; see the TODO(consistency) note in routes.py.
    fastapi_app.state.sse_queues = _sse_queues
    fastapi_app.state.dashboard_queues = _dashboard_queues
    fastapi_app.state.live_sessions = set()
    configure(sm, port=port)
    fastapi_app.mount("/static", StaticFiles(directory=_STATIC_DIR), name="static")
    # Specific routers must come before the main router because the latter has
    # a catch-all `/{session_id}` route that would otherwise swallow /config etc.
    fastapi_app.include_router(config_router)
    fastapi_app.include_router(install_router)
    fastapi_app.include_router(command_center_router)
    fastapi_app.include_router(create_zellij_router())
    fastapi_app.include_router(router)

    routes = [*mcp_starlette.routes, Mount("/", app=fastapi_app)]

    starlette_app = Starlette(
        routes=routes,
        lifespan=lifespan,
    )

    return MCPDebugMiddleware(starlette_app)
