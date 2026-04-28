"""
Zellij Web Client Prototype
============================
Validates embedding Zellij's web terminal in a custom interface
with session management (create, attach, detach, kill).

Run:  uv run python prototypes/zellij-web/prototype.py
Then: open http://127.0.0.1:8090
"""

from __future__ import annotations

import asyncio
import shutil
import subprocess
import tempfile
from contextlib import asynccontextmanager
from pathlib import Path

import httpx
from fastapi import FastAPI, WebSocket, WebSocketDisconnect
from fastapi.responses import HTMLResponse, JSONResponse
from starlette.requests import Request
from starlette.responses import Response

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

ZELLIJ_WEB_PORT = 8082
ZELLIJ_ORIGIN = f"http://127.0.0.1:{ZELLIJ_WEB_PORT}"
APP_PORT = 8090
ZELLIJ_BIN = shutil.which("zellij") or "zellij"
SESSION_PREFIX = "zing--"

# Temp dir for zellij config/layouts (cleaned up on shutdown)
_tmpdir: Path | None = None
_auth_token: str | None = None
_session_cookie: str | None = None  # Cookie obtained from server-side login
_http_client: httpx.AsyncClient | None = None

# ---------------------------------------------------------------------------
# Zellij helpers
# ---------------------------------------------------------------------------


def _run(args: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, capture_output=True, text=True, check=check)


def _write_config(tmpdir: Path) -> Path:
    """Write a minimal Zellij config that disables all chrome."""
    config = tmpdir / "config.kdl"
    config.write_text(
        """\
// Minimal config for prototype — no UI chrome, web sharing on
keybinds clear-defaults=true {
}
theme "default"
default_layout "bare"
pane_frames false
scroll_buffer_size 50000
web_sharing "on"
simplified_ui true
"""
    )
    return config


def _write_bare_layout(tmpdir: Path) -> Path:
    """Write a bare layout: single pane, no plugins, no bars."""
    layout = tmpdir / "layouts" / "bare.kdl"
    layout.parent.mkdir(parents=True, exist_ok=True)
    layout.write_text(
        """\
layout {
    pane
}
"""
    )
    return layout


def _write_command_layout(tmpdir: Path, command: str, args: list[str] | None = None) -> Path:
    """Write a layout that runs a specific command."""
    layout = tmpdir / "layouts" / "command.kdl"
    layout.parent.mkdir(parents=True, exist_ok=True)

    args_str = ""
    if args:
        args_lines = "\n".join(f'        "{a}"' for a in args)
        args_str = f"""
    args {args_lines}"""

    layout.write_text(
        f"""\
layout {{
    pane command="{command}" {{{args_str}
    }}
}}
"""
    )
    return layout


def _zellij_base_args() -> list[str]:
    """Common args that point Zellij at our temp config/layout dir."""
    assert _tmpdir is not None
    return [ZELLIJ_BIN, "--config", str(_tmpdir / "config.kdl"), "--config-dir", str(_tmpdir)]


def _start_web_server() -> str | None:
    """Start the Zellij web server, return auth token."""
    # Stop any existing instance first
    _run([*_zellij_base_args(), "web", "--stop"], check=False)

    # Start daemonized
    result = _run(
        [*_zellij_base_args(), "web", "--start", "--daemonize", "--port", str(ZELLIJ_WEB_PORT)],
        check=False,
    )
    if result.returncode != 0:
        print(f"Warning: web server start returned {result.returncode}: {result.stderr}")
        print(f"  stdout: {result.stdout}")

    # Create auth token
    # Output format: "Created token successfully\n\ntoken_N: <uuid>"
    result = _run([*_zellij_base_args(), "web", "--create-token"], check=False)
    token = None
    if result.returncode == 0:
        for line in result.stdout.strip().splitlines():
            line = line.strip()
            # Look for "token_N: <uuid>" pattern
            if line.startswith("token_") and ":" in line:
                token = line.split(":", 1)[1].strip()
                break
        print(f"Auth token: {token}" if token else f"No token parsed from: {result.stdout}")
    else:
        print(f"Warning: token creation failed: {result.stderr}")

    return token


async def _login_to_zellij() -> str | None:
    """Server-side login to Zellij, returns session cookie."""
    if not _auth_token:
        return None
    async with httpx.AsyncClient() as client:
        resp = await client.post(
            f"{ZELLIJ_ORIGIN}/command/login",
            json={"auth_token": _auth_token, "remember_me": True},
        )
        if resp.status_code == 200:
            cookie = resp.cookies.get("session_token")
            if cookie:
                print("Server-side Zellij login successful, got session cookie")
                return cookie
            # Also check Set-Cookie header directly
            for header_val in resp.headers.get_list("set-cookie"):
                if "session_token=" in header_val:
                    cookie = header_val.split("session_token=")[1].split(";")[0]
                    print("Server-side Zellij login successful, got session cookie")
                    return cookie
        print(f"Server-side Zellij login failed: {resp.status_code} {resp.text}")
    return None


def _stop_web_server() -> None:
    _run([ZELLIJ_BIN, "web", "--stop"], check=False)
    # Also try with config-dir in case the server was started with it
    if _tmpdir:
        _run([*_zellij_base_args(), "web", "--stop"], check=False)


def create_session(name: str, command: str = "bash", args: list[str] | None = None) -> dict:
    """Create a named Zellij session running the given command."""
    assert _tmpdir is not None

    # Write a layout for this command
    layout = _write_command_layout(_tmpdir, command, args)

    # Use attach -b -c with -l to create a detached session with a custom layout.
    # --new-session-with-layout requires a TTY, so it can't be used headless.
    result = _run(
        [*_zellij_base_args(), "-l", str(layout), "attach", name, "-b", "-c"],
        check=False,
    )

    return {
        "name": name,
        "command": command,
        "success": result.returncode == 0,
        "stdout": result.stdout.strip(),
        "stderr": result.stderr.strip(),
    }


def list_sessions() -> list[dict]:
    """List active Zellij sessions created by this prototype (filtered by prefix)."""
    result = _run([ZELLIJ_BIN, "list-sessions", "-sn"], check=False)
    if result.returncode != 0 or not result.stdout.strip():
        return []
    return [
        {"name": name.strip()}
        for name in result.stdout.strip().splitlines()
        if name.strip().startswith(SESSION_PREFIX)
    ]


def kill_session(name: str) -> dict:
    """Kill a Zellij session by name."""
    result = _run([ZELLIJ_BIN, "kill-session", name], check=False)
    return {
        "name": name,
        "success": result.returncode == 0,
        "stderr": result.stderr.strip(),
    }


# ---------------------------------------------------------------------------
# FastAPI app
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(app: FastAPI):
    global _tmpdir, _auth_token, _session_cookie, _http_client

    _tmpdir = Path(tempfile.mkdtemp(prefix="zellij-proto-"))
    print(f"Temp dir: {_tmpdir}")

    _write_config(_tmpdir)
    _write_bare_layout(_tmpdir)
    _auth_token = _start_web_server()

    # Check status
    result = _run([*_zellij_base_args(), "web", "--status"], check=False)
    print(f"Web server status: {result.stdout.strip()}")

    # Server-side login to get session cookie for proxying
    _session_cookie = await _login_to_zellij()

    # Persistent HTTP client for proxying (cookies set on client for auth)
    _http_client = httpx.AsyncClient(
        timeout=30.0,
        cookies={"session_token": _session_cookie} if _session_cookie else {},
    )

    print(f"\n  Open http://127.0.0.1:{APP_PORT}\n")

    yield

    print("\nShutting down...")
    if _http_client:
        await _http_client.aclose()
    _stop_web_server()
    # Clean up temp dir
    if _tmpdir and _tmpdir.exists():
        shutil.rmtree(_tmpdir, ignore_errors=True)


app = FastAPI(lifespan=lifespan)


# ---------------------------------------------------------------------------
# Reverse proxy for Zellij web client
# ---------------------------------------------------------------------------


@app.api_route("/zellij/{path:path}", methods=["GET", "POST", "PUT", "DELETE"])
async def proxy_http(request: Request, path: str):
    """Proxy HTTP requests to the Zellij web server."""
    assert _http_client is not None

    url = f"{ZELLIJ_ORIGIN}/{path}"
    if request.query_params:
        url += f"?{request.query_params}"

    body = await request.body()

    resp = await _http_client.request(
        method=request.method,
        url=url,
        headers={
            k: v
            for k, v in request.headers.items()
            if k.lower() not in ("host", "connection", "transfer-encoding")
        },
        content=body if body else None,
    )

    # Forward response, stripping hop-by-hop headers
    excluded_headers = {"transfer-encoding", "connection", "content-encoding", "content-length"}
    headers = {k: v for k, v in resp.headers.items() if k.lower() not in excluded_headers}

    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=headers,
        media_type=resp.headers.get("content-type"),
    )


async def _proxy_ws(ws: WebSocket, upstream_path: str):
    """Proxy a WebSocket connection to the Zellij web server."""
    import websockets

    await ws.accept()

    ws_url = f"ws://127.0.0.1:{ZELLIJ_WEB_PORT}{upstream_path}"
    if ws.query_params:
        ws_url += f"?{ws.query_params}"

    extra_headers = {"Cookie": f"session_token={_session_cookie}"} if _session_cookie else {}

    try:
        async with websockets.connect(ws_url, additional_headers=extra_headers) as upstream:

            async def client_to_upstream():
                try:
                    while True:
                        data = await ws.receive()
                        if "text" in data:
                            await upstream.send(data["text"])
                        elif "bytes" in data:
                            await upstream.send(data["bytes"])
                except WebSocketDisconnect:
                    pass

            async def upstream_to_client():
                try:
                    async for msg in upstream:
                        if isinstance(msg, str):
                            await ws.send_text(msg)
                        elif isinstance(msg, bytes):
                            await ws.send_bytes(msg)
                except WebSocketDisconnect:
                    pass

            await asyncio.gather(client_to_upstream(), upstream_to_client())

    except Exception as e:
        print(f"WebSocket proxy error: {e}")
    finally:
        try:
            await ws.close()
        except Exception:
            pass


@app.websocket("/ws/terminal/{path:path}")
async def proxy_ws_terminal(ws: WebSocket, path: str):
    """Proxy terminal WebSocket to Zellij."""
    await _proxy_ws(ws, f"/ws/terminal/{path}")


@app.websocket("/ws/control")
async def proxy_ws_control(ws: WebSocket):
    """Proxy control WebSocket to Zellij."""
    await _proxy_ws(ws, "/ws/control")


# Patch for Zellij's input.js — adds Shift+Enter handling.
# The original only sends kitty-encoded keys for 2+ modifiers or meta key,
# so Shift+Enter (1 modifier) falls through to xterm.js default (\r = same as Enter).
# This patch adds a check before the general modifier block to encode Shift+Enter
# as \x1b[13;2u (kitty protocol: key=13/CR, modifier=2/shift).
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
            }"""

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


# Proxy Zellij's static assets — the iframe HTML references /assets/* directly
@app.get("/assets/{path:path}")
async def proxy_assets(path: str):
    """Proxy static asset requests to Zellij, patching input.js for Shift+Enter."""
    assert _http_client is not None
    resp = await _http_client.get(f"{ZELLIJ_ORIGIN}/assets/{path}")
    excluded_headers = {"transfer-encoding", "connection", "content-encoding", "content-length"}
    headers = {k: v for k, v in resp.headers.items() if k.lower() not in excluded_headers}

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


# Proxy Zellij's /command/* and /session endpoints (used by its JS for auth and WebSocket setup)
# Proxy Zellij's /info/* endpoints (version check, etc.)
@app.get("/info/{path:path}")
async def proxy_info(path: str):
    """Proxy Zellij info endpoints."""
    assert _http_client is not None
    resp = await _http_client.get(f"{ZELLIJ_ORIGIN}/info/{path}")
    return Response(content=resp.content, status_code=resp.status_code)


@app.api_route("/command/{path:path}", methods=["GET", "POST"])
async def proxy_command(request: Request, path: str):
    """Proxy Zellij command endpoints."""
    assert _http_client is not None
    body = await request.body()
    resp = await _http_client.request(
        method=request.method,
        url=f"{ZELLIJ_ORIGIN}/command/{path}",
        headers={
            k: v
            for k, v in request.headers.items()
            if k.lower() not in ("host", "connection", "transfer-encoding")
        },
        content=body if body else None,
    )
    excluded_headers = {"transfer-encoding", "connection", "content-encoding", "content-length"}
    headers = {k: v for k, v in resp.headers.items() if k.lower() not in excluded_headers}
    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=headers,
        media_type=resp.headers.get("content-type"),
    )


@app.api_route("/session", methods=["GET", "POST"])
async def proxy_session(request: Request):
    """Proxy Zellij session endpoint."""
    assert _http_client is not None
    body = await request.body()
    resp = await _http_client.request(
        method=request.method,
        url=f"{ZELLIJ_ORIGIN}/session",
        headers={
            k: v
            for k, v in request.headers.items()
            if k.lower() not in ("host", "connection", "transfer-encoding")
        },
        content=body if body else None,
    )
    excluded_headers = {"transfer-encoding", "connection", "content-encoding", "content-length"}
    headers = {k: v for k, v in resp.headers.items() if k.lower() not in excluded_headers}
    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=headers,
        media_type=resp.headers.get("content-type"),
    )


# ---------------------------------------------------------------------------
# App routes
# ---------------------------------------------------------------------------


@app.get("/", response_class=HTMLResponse)
async def index():
    return HTML_PAGE


@app.post("/sessions/create")
async def api_create_session(body: dict):
    name = body.get("name", "").strip()
    command = body.get("command", "bash").strip()
    if not name:
        return JSONResponse({"error": "name is required"}, status_code=400)

    # Auto-prepend prefix if not already present
    if not name.startswith(SESSION_PREFIX):
        name = SESSION_PREFIX + name

    # Split command into binary + args
    parts = command.split()
    cmd = parts[0]
    args = parts[1:] if len(parts) > 1 else None

    result = create_session(name, cmd, args)
    return JSONResponse(result)


@app.get("/sessions")
async def api_list_sessions():
    return JSONResponse(list_sessions())


@app.post("/sessions/{name}/kill")
async def api_kill_session(name: str):
    return JSONResponse(kill_session(name))


# ---------------------------------------------------------------------------
# HTML UI
# ---------------------------------------------------------------------------

HTML_PAGE = """\
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Zellij Web Prototype</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "SF Mono", monospace;
         background: #0d1117; color: #c9d1d9; height: 100vh; display: flex; flex-direction: column; }

  /* Top bar */
  .topbar { display: flex; align-items: center; gap: 12px; padding: 8px 16px;
            background: #161b22; border-bottom: 1px solid #30363d; flex-shrink: 0; }
  .topbar h1 { font-size: 14px; font-weight: 600; color: #58a6ff; }
  .topbar button { padding: 4px 12px; border-radius: 4px; border: 1px solid #30363d;
                   background: #21262d; color: #c9d1d9; cursor: pointer; font-size: 12px; }
  .topbar button:hover { background: #30363d; }

  /* Main layout */
  .main { display: flex; flex: 1; min-height: 0; }

  /* Sidebar */
  .sidebar { width: 260px; background: #161b22; border-right: 1px solid #30363d;
             display: flex; flex-direction: column; flex-shrink: 0; }
  .sidebar-header { padding: 10px 12px; font-size: 11px; text-transform: uppercase;
                    letter-spacing: 0.5px; color: #8b949e; border-bottom: 1px solid #30363d; }
  .session-list { flex: 1; overflow-y: auto; padding: 4px 0; }
  .session-item { padding: 8px 12px; display: flex; align-items: center; justify-content: space-between;
                  border-bottom: 1px solid #21262d; cursor: pointer; }
  .session-item:hover { background: #1c2129; }
  .session-item.active { background: #1f3a5f; border-left: 2px solid #58a6ff; }
  .session-name { font-size: 13px; font-weight: 500; }
  .session-actions { display: flex; gap: 4px; }
  .session-actions button { padding: 2px 6px; font-size: 10px; border-radius: 3px;
                            border: 1px solid #30363d; background: #21262d; color: #c9d1d9;
                            cursor: pointer; }
  .session-actions button:hover { background: #30363d; }
  .session-actions button.kill { border-color: #f85149; color: #f85149; }
  .session-actions button.kill:hover { background: #f8514922; }
  .empty-state { padding: 20px 12px; color: #8b949e; font-size: 12px; text-align: center; }

  /* Terminal panel */
  .terminal-panel { flex: 1; display: flex; flex-direction: column; min-width: 0; }
  .terminal-header { padding: 6px 12px; font-size: 11px; color: #8b949e;
                     background: #0d1117; border-bottom: 1px solid #30363d; flex-shrink: 0;
                     display: flex; align-items: center; justify-content: space-between; }
  .terminal-frame { flex: 1; border: none; background: #000; }
  .placeholder { flex: 1; display: flex; align-items: center; justify-content: center;
                 color: #484f58; font-size: 14px; }

  /* Create modal */
  .modal-overlay { display: none; position: fixed; inset: 0; background: #00000088;
                   z-index: 100; align-items: center; justify-content: center; }
  .modal-overlay.open { display: flex; }
  .modal { background: #161b22; border: 1px solid #30363d; border-radius: 8px;
           padding: 20px; width: 380px; }
  .modal h2 { font-size: 14px; margin-bottom: 16px; color: #f0f6fc; }
  .modal label { display: block; font-size: 12px; color: #8b949e; margin-bottom: 4px; }
  .modal input { width: 100%; padding: 6px 10px; border-radius: 4px; border: 1px solid #30363d;
                 background: #0d1117; color: #c9d1d9; font-size: 13px; margin-bottom: 12px;
                 font-family: inherit; }
  .modal input:focus { outline: none; border-color: #58a6ff; }
  .modal .btn-row { display: flex; gap: 8px; justify-content: flex-end; }
  .modal button { padding: 6px 16px; border-radius: 4px; border: 1px solid #30363d;
                  background: #21262d; color: #c9d1d9; cursor: pointer; font-size: 12px; }
  .modal button.primary { background: #238636; border-color: #238636; color: #fff; }
  .modal button.primary:hover { background: #2ea043; }

  /* Log panel */
  .log-panel { height: 100px; background: #0d1117; border-top: 1px solid #30363d;
               overflow-y: auto; padding: 6px 12px; font-size: 11px; color: #8b949e;
               flex-shrink: 0; font-family: "SF Mono", monospace; }
  .log-panel .log-entry { padding: 1px 0; }
  .log-panel .log-error { color: #f85149; }
  .log-panel .log-success { color: #3fb950; }
</style>
</head>
<body>

<div class="topbar">
  <h1>Zellij Web Prototype</h1>
  <button onclick="openCreateModal()">+ New Session</button>
  <button onclick="refreshSessions()">Refresh</button>
</div>

<div class="main">
  <div class="sidebar">
    <div class="sidebar-header">Sessions</div>
    <div class="session-list" id="sessionList">
      <div class="empty-state">No sessions. Click "+ New Session" to start.</div>
    </div>
  </div>
  <div class="terminal-panel">
    <div class="terminal-header">
      <span id="terminalTitle">No session attached</span>
      <button onclick="detach()" style="font-size:10px; padding:2px 8px; border-radius:3px;
              border:1px solid #30363d; background:#21262d; color:#c9d1d9; cursor:pointer;">
        Detach
      </button>
    </div>
    <iframe id="terminalFrame" class="terminal-frame" style="display:none;"></iframe>
    <div id="placeholder" class="placeholder">Select a session to attach</div>
  </div>
</div>

<div class="log-panel" id="logPanel"></div>

<!-- Create session modal -->
<div class="modal-overlay" id="createModal">
  <div class="modal">
    <h2>Create Session</h2>
    <label for="sessionName">Session Name</label>
    <input type="text" id="sessionName" placeholder="my-session" autofocus>
    <label for="sessionCmd">Command</label>
    <input type="text" id="sessionCmd" placeholder="bash" value="bash">
    <div class="btn-row">
      <button onclick="closeCreateModal()">Cancel</button>
      <button class="primary" onclick="createSession()">Create</button>
    </div>
  </div>
</div>

<script>
let activeSession = null;
let sessionCounter = 0;

function log(msg, type = "") {
  const panel = document.getElementById("logPanel");
  const entry = document.createElement("div");
  entry.className = "log-entry" + (type ? " log-" + type : "");
  entry.textContent = new Date().toLocaleTimeString() + "  " + msg;
  panel.appendChild(entry);
  panel.scrollTop = panel.scrollHeight;
}

function openCreateModal() {
  sessionCounter++;
  document.getElementById("sessionName").value = "test-" + sessionCounter;
  document.getElementById("sessionCmd").value = "bash";
  document.getElementById("createModal").classList.add("open");
  document.getElementById("sessionName").focus();
}

function closeCreateModal() {
  document.getElementById("createModal").classList.remove("open");
}

async function createSession() {
  const name = document.getElementById("sessionName").value.trim();
  const command = document.getElementById("sessionCmd").value.trim() || "bash";
  if (!name) return;
  closeCreateModal();
  log("Creating session: " + name + " (" + command + ")...");
  try {
    const resp = await fetch("/sessions/create", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, command }),
    });
    const data = await resp.json();
    if (data.success) {
      log("Session created: " + name, "success");
    } else {
      log("Create failed: " + (data.stderr || data.error || "unknown"), "error");
    }
    // Small delay for Zellij to register the session
    setTimeout(refreshSessions, 500);
  } catch (e) {
    log("Error: " + e.message, "error");
  }
}

async function refreshSessions() {
  try {
    const resp = await fetch("/sessions");
    const sessions = await resp.json();
    const list = document.getElementById("sessionList");
    if (sessions.length === 0) {
      list.innerHTML = '<div class="empty-state">No sessions. Click "+ New Session" to start.</div>';
      return;
    }
    list.innerHTML = sessions.map(s => `
      <div class="session-item ${s.name === activeSession ? 'active' : ''}"
           onclick="attach('${s.name}')">
        <span class="session-name">${s.name}</span>
        <div class="session-actions">
          <button onclick="event.stopPropagation(); attach('${s.name}')">Attach</button>
          <button class="kill" onclick="event.stopPropagation(); killSession('${s.name}')">Kill</button>
        </div>
      </div>
    `).join("");
  } catch (e) {
    log("Refresh error: " + e.message, "error");
  }
}

function attach(name) {
  activeSession = name;
  const frame = document.getElementById("terminalFrame");
  const placeholder = document.getElementById("placeholder");
  const title = document.getElementById("terminalTitle");

  // Route through our reverse proxy — same origin, no CORS, auth handled server-side
  frame.src = "/zellij/" + encodeURIComponent(name);
  frame.style.display = "block";
  placeholder.style.display = "none";
  title.textContent = "Attached: " + name;
  log("Attached to: " + name);
  refreshSessions();
}

function detach() {
  const frame = document.getElementById("terminalFrame");
  const placeholder = document.getElementById("placeholder");
  const title = document.getElementById("terminalTitle");

  frame.src = "about:blank";
  frame.style.display = "none";
  placeholder.style.display = "flex";
  title.textContent = "No session attached";
  if (activeSession) {
    log("Detached from: " + activeSession);
  }
  activeSession = null;
  refreshSessions();
}

async function killSession(name) {
  log("Killing session: " + name + "...");
  try {
    const resp = await fetch("/sessions/" + encodeURIComponent(name) + "/kill", { method: "POST" });
    const data = await resp.json();
    if (data.success) {
      log("Killed: " + name, "success");
      if (activeSession === name) detach();
    } else {
      log("Kill failed: " + (data.stderr || "unknown"), "error");
    }
    setTimeout(refreshSessions, 500);
  } catch (e) {
    log("Error: " + e.message, "error");
  }
}

// Handle Enter key in modal
document.getElementById("sessionName").addEventListener("keydown", e => {
  if (e.key === "Enter") createSession();
  if (e.key === "Escape") closeCreateModal();
});
document.getElementById("sessionCmd").addEventListener("keydown", e => {
  if (e.key === "Enter") createSession();
  if (e.key === "Escape") closeCreateModal();
});

// Initial load
refreshSessions();
log("Prototype ready. Terminal proxied through same origin.");

// Auto-refresh every 5s
setInterval(refreshSessions, 5000);
</script>

</body>
</html>
"""

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    import uvicorn

    print(
        """
╔══════════════════════════════════════════════════════════════╗
║  Zellij Web Prototype                                       ║
║                                                              ║
║  Test checklist:                                             ║
║  1. Create 2-3 sessions with different commands              ║
║  2. Attach to each, type commands, verify output             ║
║  3. Generate scrollback: seq 1 500 (or similar)              ║
║  4. Detach and reattach — is scrollback preserved?           ║
║  5. Switch between sessions — do they stay alive?            ║
║  6. Test: shift-enter, mouse select, copy/paste, resize      ║
║  7. Kill this script (Ctrl-C), restart — sessions survive?   ║
║  8. Kill a session — does it disappear?                      ║
╚══════════════════════════════════════════════════════════════╝
"""
    )

    uvicorn.run(app, host="127.0.0.1", port=APP_PORT)
