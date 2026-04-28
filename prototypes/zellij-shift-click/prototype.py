"""
Zellij Shift+Click Prototype
============================
Minimal harness for investigating why Zellij's "Shift-Click: <url>" tooltip
appears on hover but Shift+Click does nothing.

What this gives you:
  - One auto-launched session that prints clickable URLs.
  - Full-page iframe pointed at the Zellij web client.
  - A debug overlay logging mouse events captured at the iframe boundary AND
    (when same-origin) inside the iframe document itself.
  - WebSocket hex-dump for any frame containing a mouse CSI sequence
    (\\x1b[<...M / m).
  - All Zellij /assets/* files dumped to ./_assets_dump/ on first proxy hit
    so they can be read directly with the Read tool.
  - Two clearly-marked patch zones (input.js + a generic asset patcher) where
    new theories can be wired in and reloaded with a browser refresh.

Run:  uv run python prototypes/zellij-shift-click/prototype.py
Then: open http://127.0.0.1:8091
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

# Zellij's web daemon is a per-user singleton (uses a fixed-path bus socket
# under /tmp). Multiple wrappers can't each spawn their own. This prototype
# probes for an existing daemon first and reuses it; only spawns its own if
# nothing is running.
ZELLIJ_PROBE_PORTS = (8082, 8083)  # default + our preferred fallback
ZELLIJ_FALLBACK_PORT = 8083
APP_PORT = 8091
ZELLIJ_BIN = shutil.which("zellij") or "zellij"
SESSION_NAME = "zing-shiftclick"

ASSET_DUMP_DIR = Path(__file__).parent / "_assets_dump"

_tmpdir: Path | None = None
_auth_token: str | None = None
_session_cookie: str | None = None
_http_client: httpx.AsyncClient | None = None
_zellij_port: int | None = None  # the daemon port we're talking to
_zellij_origin: str | None = None  # http://127.0.0.1:<_zellij_port>
_we_started_daemon: bool = False  # True iff this process spawned it

# ---------------------------------------------------------------------------
# Zellij helpers
# ---------------------------------------------------------------------------


def _run(args: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, capture_output=True, text=True, check=check)


def _write_config(tmpdir: Path) -> None:
    (tmpdir / "config.kdl").write_text(
        """\
keybinds clear-defaults=true {
}
theme "default"
default_layout "urls"
pane_frames false
scroll_buffer_size 50000
web_sharing "on"
simplified_ui true
mouse_mode true
copy_on_select false
"""
    )


def _write_urls_layout(tmpdir: Path) -> Path:
    """Layout that runs a small Python program which:

    1. Enables SGR mouse-reporting (\x1b[?1006h\x1b[?1000h) so the pane
       behaves like a typical interactive program (Claude Code, less,
       vim, …) — i.e. xterm.js will encode every click as a wire-protocol
       CSI sequence instead of letting the link-provider's activate
       callback fire. This is the *wild* repro case: the bug only shows
       up when the inner program has put the terminal into mouse mode.
    2. Prints clickable URLs.
    3. Reads-and-discards stdin forever so the process stays alive and
       the mouse mode stays on. (The mouse-protocol bytes the browser
       sends back arrive on this stdin; we just drop them.)
    4. Restores the terminal on SIGINT/SIGTERM.
    """
    layout_dir = tmpdir / "layouts"
    layout_dir.mkdir(parents=True, exist_ok=True)
    script = tmpdir / "url_pane.py"
    script.write_text(
        '''\
#!/usr/bin/env python3
"""Mock interactive program: enables mouse-reporting, prints URLs, idles.

The pty must be put into non-canonical, no-echo mode before the loop;
otherwise the kernel buffers mouse-protocol bytes line-by-line AND echoes
each byte back to stdout, splattering '^[[<0;23;2m' all over the screen.
We keep ISIG enabled so Ctrl+C still works.
"""
import os, select, signal, sys, termios

ENABLE  = "\\x1b[?1006h\\x1b[?1000h"   # SGR + basic mouse tracking
DISABLE = "\\x1b[?1006l\\x1b[?1000l"

fd = sys.stdin.fileno()
try:
    saved_termios = termios.tcgetattr(fd)
except termios.error:
    saved_termios = None

def restore(*_):
    sys.stdout.write(DISABLE); sys.stdout.flush()
    if saved_termios is not None:
        try: termios.tcsetattr(fd, termios.TCSADRAIN, saved_termios)
        except termios.error: pass
    sys.exit(0)

if saved_termios is not None:
    new_attrs = termios.tcgetattr(fd)
    # lflag: turn off ICANON + ECHO, keep ISIG for Ctrl+C handling.
    new_attrs[3] &= ~(termios.ICANON | termios.ECHO)
    # cc: read returns as soon as 1 byte is available.
    new_attrs[6][termios.VMIN]  = 1
    new_attrs[6][termios.VTIME] = 0
    termios.tcsetattr(fd, termios.TCSANOW, new_attrs)

sys.stdout.write("\\x1b[2J\\x1b[H")
sys.stdout.write(ENABLE)
sys.stdout.write("""
  URL link test  --  mouse-reporting ON (like Claude Code)
  ---------------------------------------------------------

  Plain URL:    https://example.com
  GitHub PR:    https://github.com/turngate/backend-v1/pull/1885
  With path:    https://github.com/anthropics/claude-code/issues/123

  Hover any URL: tooltip should read 'Shift-Click: <url>'.
  Shift+Click should open it in a new browser tab.

  This pane has SGR mouse-reporting enabled in non-canonical, no-echo
  mode. Mouse bytes received on stdin are silently discarded; nothing
  should appear on this screen. Ctrl+C to exit.

""")
sys.stdout.flush()

signal.signal(signal.SIGINT,  restore)
signal.signal(signal.SIGTERM, restore)

try:
    while True:
        r, _, _ = select.select([fd], [], [], 1.0)
        if r:
            try: os.read(fd, 4096)
            except OSError: break
finally:
    restore()
'''
    )
    script.chmod(0o755)

    layout = layout_dir / "urls.kdl"
    layout.write_text(
        f"""\
layout {{
    pane command="{script}"
}}
"""
    )
    return layout


def _zellij_base_args() -> list[str]:
    assert _tmpdir is not None
    return [ZELLIJ_BIN, "--config", str(_tmpdir / "config.kdl"), "--config-dir", str(_tmpdir)]


async def _probe_port(port: int) -> bool:
    """Return True if a Zellij web daemon answers on this port."""
    try:
        async with httpx.AsyncClient(timeout=1.0) as client:
            resp = await client.get(f"http://127.0.0.1:{port}/info/version")
            return resp.status_code == 200
    except Exception:
        return False


async def _find_or_start_daemon() -> tuple[int, bool]:
    """Probe known ports for a running daemon; spawn one if none responds.

    Returns (port, did_we_spawn_it).
    """
    for port in ZELLIJ_PROBE_PORTS:
        if await _probe_port(port):
            print(f"reusing existing zellij web daemon on :{port}")
            return port, False

    print(f"no existing daemon found, starting one on :{ZELLIJ_FALLBACK_PORT}")
    result = _run(
        [
            *_zellij_base_args(),
            "web",
            "--start",
            "--daemonize",
            "--port",
            str(ZELLIJ_FALLBACK_PORT),
        ],
        check=False,
    )
    if result.returncode != 0:
        print(f"web --start rc={result.returncode} stderr={result.stderr.strip()}")
    return ZELLIJ_FALLBACK_PORT, True


def _create_token() -> str | None:
    """Ask the daemon for a fresh auth token. Tokens are stored daemon-side,
    so this works whether or not we started the daemon — as long as our
    --config-dir points at the daemon's data location. Zellij stores tokens
    in a fixed-path data dir keyed by user, not by --config-dir, so this is
    safe across wrappers."""
    result = _run([*_zellij_base_args(), "web", "--create-token"], check=False)
    for line in result.stdout.strip().splitlines():
        line = line.strip()
        if line.startswith("token_") and ":" in line:
            return line.split(":", 1)[1].strip()
    print(f"create-token failed rc={result.returncode} stderr={result.stderr.strip()}")
    return None


async def _login_to_zellij() -> str | None:
    if not _auth_token or not _zellij_origin:
        return None
    async with httpx.AsyncClient() as client:
        resp = await client.post(
            f"{_zellij_origin}/command/login",
            json={"auth_token": _auth_token, "remember_me": True},
        )
        if resp.status_code == 200:
            cookie = resp.cookies.get("session_token")
            if cookie:
                return cookie
            for header_val in resp.headers.get_list("set-cookie"):
                if "session_token=" in header_val:
                    return header_val.split("session_token=")[1].split(";")[0]
        print(f"login failed status={resp.status_code} body={resp.text[:200]}")
    return None


def _create_session() -> None:
    """Create the prototype's session. We have to fully *delete* any existing
    one first — kill-session merely marks it EXITED, and a subsequent
    `attach -c -l <layout>` will resurrect the old session with its OLD
    layout instead of creating a new one with ours."""
    assert _tmpdir is not None
    layout = _write_urls_layout(_tmpdir)
    _run([ZELLIJ_BIN, "kill-session", SESSION_NAME], check=False)
    _run([ZELLIJ_BIN, "delete-session", SESSION_NAME, "--force"], check=False)
    result = _run(
        [*_zellij_base_args(), "-l", str(layout), "attach", SESSION_NAME, "-b", "-c"],
        check=False,
    )
    print(f"create session rc={result.returncode} stderr={result.stderr.strip()}")


# ---------------------------------------------------------------------------
# === PATCH ZONE ============================================================
# Add / swap theories here. Each function gets the raw asset text and returns
# patched text. The _assets_dump/ directory holds the *original* files so you
# can read them with the Read tool to find new patch targets.
# ---------------------------------------------------------------------------


def _patch_input_js(text: str) -> str:
    """Theories about keyboard / Shift+Enter sit here. Currently a no-op."""
    return text


_SHIFT_CLICK_FIX = r"""
// === Shift+Click URL fix (added by zellij-shift-click prototype) ===
// xterm.js (this version) does NOT honor the XTerm convention of
// "shift suppresses mouse-reporting and lets click-to-link run." When the
// inner program enables mouse mode, shift+click is just encoded as
// \x1b[<4;col;row;M and the link provider's `activate` callback never
// fires — even though Zellij's links.js shows a 'Shift-Click: <url>'
// hover hint suggesting it should work.
//
// Fix: capture-phase mousedown listener that runs *before* xterm.js's
// own mouse handler. On shift + left-click, look up the URL at the
// click cell and call window.open() directly; preventDefault +
// stopImmediatePropagation so xterm.js never encodes the click.
(function(){
  // Same regex addon-web-links.js uses internally, copied here so we
  // don't depend on internals.
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
    if (!surface || surface.__shiftClickInstalled) return;
    surface.__shiftClickInstalled = true;
    surface.addEventListener("mousedown", (ev) => {
      if (!ev.shiftKey || ev.button !== 0) return;
      const rect = surface.getBoundingClientRect();
      const cw = rect.width  / term.cols;
      const ch = rect.height / term.rows;
      const col = Math.floor((ev.clientX - rect.left) / cw);
      const visRow = Math.floor((ev.clientY - rect.top) / ch);
      const row = term.buffer.active.viewportY + visRow;
      const url = findUrl(term, col, row);
      console.log("[shift-click] col=" + col + " row=" + row + " url=" + url);
      if (!url) return;
      ev.preventDefault();
      ev.stopPropagation();
      ev.stopImmediatePropagation();
      const w = window.open(url, "_blank");
      if (w) { try { w.opener = null; } catch (e) {} }
    }, true /* capture */);
    console.log("[shift-click-fix] installed on " + (screen ? ".xterm-screen" : "term.element"));
  }
  // window.term is set by terminal.js's initTerminal(). Poll until it's
  // ready (terminal init can lag the script load).
  let attempts = 0;
  const handle = setInterval(() => {
    if (window.term && window.term.element) {
      install(window.term);
      clearInterval(handle);
    } else if (++attempts > 100) {
      clearInterval(handle);
      console.warn("[shift-click-fix] window.term never appeared");
    }
  }, 50);
})();
"""


def _patch_mouse_or_link_js(filename: str, text: str) -> str:
    """Mouse / link-provider patches.

    Currently active: append a shift+click→window.open handler to
    terminal.js. terminal.js sets window.term, so we use that as our hook
    point and avoid touching the minified xterm.js bundle.
    """
    if filename == "terminal.js":
        return text + _SHIFT_CLICK_FIX
    return text


# ---------------------------------------------------------------------------
# Asset dump + proxy
# ---------------------------------------------------------------------------


def _dump_asset(path: str, content: bytes) -> None:
    """Save unpatched asset to disk for inspection."""
    out = ASSET_DUMP_DIR / path
    out.parent.mkdir(parents=True, exist_ok=True)
    if not out.exists():
        out.write_bytes(content)


# ---------------------------------------------------------------------------
# WebSocket logger
# ---------------------------------------------------------------------------

# Mouse CSI sequences: ESC [ < Cb ; Cx ; Cy (M|m)
_MOUSE_PREFIX = b"\x1b[<"


def _format_ws_frame(direction: str, data: bytes | str) -> str | None:
    """Return a short log line if the frame *looks* mouse-related, else None."""
    raw = data.encode() if isinstance(data, str) else data
    if _MOUSE_PREFIX not in raw:
        return None
    # Pull the CSI segment out of the surrounding noise for readability.
    idx = raw.find(_MOUSE_PREFIX)
    snippet = raw[idx : idx + 32]
    return f"WS {direction}  {snippet!r}"


_ws_log: list[str] = []


def _ws_log_push(line: str) -> None:
    _ws_log.append(line)
    if len(_ws_log) > 200:
        del _ws_log[:100]
    print(line)


# ---------------------------------------------------------------------------
# FastAPI
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(app: FastAPI):
    global _tmpdir, _auth_token, _session_cookie, _http_client
    global _zellij_port, _zellij_origin, _we_started_daemon

    _tmpdir = Path(tempfile.mkdtemp(prefix="zellij-shiftclick-"))
    print(f"tmpdir = {_tmpdir}")
    print(f"asset dump = {ASSET_DUMP_DIR}")

    _write_config(_tmpdir)
    _write_urls_layout(_tmpdir)

    _zellij_port, _we_started_daemon = await _find_or_start_daemon()
    _zellij_origin = f"http://127.0.0.1:{_zellij_port}"
    print(f"zellij origin = {_zellij_origin} (we_started={_we_started_daemon})")

    _auth_token = _create_token()
    print(f"auth token = {_auth_token}")

    _session_cookie = await _login_to_zellij()
    if not _session_cookie:
        print(
            "\n  WARNING: could not log in to the zellij daemon. The daemon was\n"
            "  probably started by a different wrapper with its own data dir,\n"
            "  so our token isn't recognised. Stop the other wrapper(s) and\n"
            "  try again, or kill the daemon manually with `zellij web --stop`.\n"
        )

    _http_client = httpx.AsyncClient(
        timeout=30.0,
        cookies={"session_token": _session_cookie} if _session_cookie else {},
    )

    _create_session()

    print(f"\n  open http://127.0.0.1:{APP_PORT}\n")
    yield

    print("\nshutting down...")
    if _http_client:
        await _http_client.aclose()
    _run([ZELLIJ_BIN, "kill-session", SESSION_NAME], check=False)
    if _we_started_daemon:
        # Only stop the daemon if we were the ones who started it; otherwise
        # we'd kill the web server out from under main Zing or the other
        # prototype.
        _run([*_zellij_base_args(), "web", "--stop"], check=False)
    if _tmpdir and _tmpdir.exists():
        shutil.rmtree(_tmpdir, ignore_errors=True)


app = FastAPI(lifespan=lifespan)


def _strip_hop_headers(headers: dict[str, str]) -> dict[str, str]:
    excluded = {"transfer-encoding", "connection", "content-encoding", "content-length"}
    return {k: v for k, v in headers.items() if k.lower() not in excluded}


@app.get("/assets/{path:path}")
async def proxy_assets(path: str):
    """Serve patched Zellij assets and dump unpatched copy to disk."""
    assert _http_client is not None
    resp = await _http_client.get(f"{_zellij_origin}/assets/{path}")
    headers = _strip_hop_headers(dict(resp.headers))
    content = resp.content

    _dump_asset(path, content)

    media = resp.headers.get("content-type", "")
    if "javascript" in media or path.endswith(".js"):
        text = content.decode(errors="replace")
        if path == "input.js":
            text = _patch_input_js(text)
        text = _patch_mouse_or_link_js(path, text)
        content = text.encode()

    return Response(
        content=content,
        status_code=resp.status_code,
        headers=headers,
        media_type=resp.headers.get("content-type"),
    )


@app.api_route("/zellij/{path:path}", methods=["GET", "POST", "PUT", "DELETE"])
async def proxy_zellij(request: Request, path: str):
    assert _http_client is not None
    url = f"{_zellij_origin}/{path}"
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
    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=_strip_hop_headers(dict(resp.headers)),
        media_type=resp.headers.get("content-type"),
    )


@app.api_route("/command/{path:path}", methods=["GET", "POST"])
async def proxy_command(request: Request, path: str):
    assert _http_client is not None
    body = await request.body()
    resp = await _http_client.request(
        method=request.method,
        url=f"{_zellij_origin}/command/{path}",
        headers={
            k: v
            for k, v in request.headers.items()
            if k.lower() not in ("host", "connection", "transfer-encoding")
        },
        content=body if body else None,
    )
    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=_strip_hop_headers(dict(resp.headers)),
        media_type=resp.headers.get("content-type"),
    )


@app.get("/info/{path:path}")
async def proxy_info(path: str):
    assert _http_client is not None
    resp = await _http_client.get(f"{_zellij_origin}/info/{path}")
    return Response(content=resp.content, status_code=resp.status_code)


@app.api_route("/session", methods=["GET", "POST"])
async def proxy_session(request: Request):
    assert _http_client is not None
    body = await request.body()
    resp = await _http_client.request(
        method=request.method,
        url=f"{_zellij_origin}/session",
        headers={
            k: v
            for k, v in request.headers.items()
            if k.lower() not in ("host", "connection", "transfer-encoding")
        },
        content=body if body else None,
    )
    return Response(
        content=resp.content,
        status_code=resp.status_code,
        headers=_strip_hop_headers(dict(resp.headers)),
        media_type=resp.headers.get("content-type"),
    )


async def _proxy_ws(ws: WebSocket, upstream_path: str):
    import websockets

    await ws.accept()
    ws_url = f"ws://127.0.0.1:{_zellij_port}{upstream_path}"
    if ws.query_params:
        ws_url += f"?{ws.query_params}"
    extra_headers = {"Cookie": f"session_token={_session_cookie}"} if _session_cookie else {}

    try:
        async with websockets.connect(ws_url, additional_headers=extra_headers) as upstream:

            async def c_to_u():
                try:
                    while True:
                        data = await ws.receive()
                        if "text" in data:
                            line = _format_ws_frame("→up  ", data["text"])
                            if line:
                                _ws_log_push(line)
                            await upstream.send(data["text"])
                        elif "bytes" in data:
                            line = _format_ws_frame("→up  ", data["bytes"])
                            if line:
                                _ws_log_push(line)
                            await upstream.send(data["bytes"])
                except WebSocketDisconnect:
                    pass

            async def u_to_c():
                try:
                    async for msg in upstream:
                        if isinstance(msg, str):
                            line = _format_ws_frame("←down", msg)
                            if line:
                                _ws_log_push(line)
                            await ws.send_text(msg)
                        else:
                            line = _format_ws_frame("←down", msg)
                            if line:
                                _ws_log_push(line)
                            await ws.send_bytes(msg)
                except WebSocketDisconnect:
                    pass

            await asyncio.gather(c_to_u(), u_to_c())
    except Exception as e:
        print(f"ws error: {e}")
    finally:
        try:
            await ws.close()
        except Exception:
            pass


@app.websocket("/ws/terminal/{path:path}")
async def proxy_ws_terminal(ws: WebSocket, path: str):
    await _proxy_ws(ws, f"/ws/terminal/{path}")


@app.websocket("/ws/control")
async def proxy_ws_control(ws: WebSocket):
    await _proxy_ws(ws, "/ws/control")


# ---------------------------------------------------------------------------
# Debug helpers
# ---------------------------------------------------------------------------


@app.get("/debug/ws-log")
async def debug_ws_log():
    return JSONResponse(_ws_log[-100:])


@app.get("/debug/asset-list")
async def debug_asset_list():
    if not ASSET_DUMP_DIR.exists():
        return JSONResponse([])
    return JSONResponse(
        sorted(
            p.relative_to(ASSET_DUMP_DIR).as_posix()
            for p in ASSET_DUMP_DIR.rglob("*")
            if p.is_file()
        )
    )


# ---------------------------------------------------------------------------
# Page
# ---------------------------------------------------------------------------


@app.get("/", response_class=HTMLResponse)
async def index():
    return HTML_PAGE.replace("__SESSION__", SESSION_NAME)


HTML_PAGE = """\
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Zellij Shift+Click Prototype</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { height: 100%; }
  body { font-family: -apple-system, "SF Mono", monospace; background: #0d1117; color: #c9d1d9;
         display: grid; grid-template-rows: 32px 1fr 220px; }
  .topbar { display: flex; align-items: center; gap: 12px; padding: 0 12px;
            background: #161b22; border-bottom: 1px solid #30363d; font-size: 12px; }
  .topbar b { color: #58a6ff; }
  .topbar button { font-size: 11px; padding: 2px 8px; background: #21262d; color: #c9d1d9;
                   border: 1px solid #30363d; border-radius: 3px; cursor: pointer; }
  .topbar button:hover { background: #30363d; }

  #frameWrap { position: relative; background: #000; }
  iframe { width: 100%; height: 100%; border: none; background: #000; }

  .log { background: #0d1117; border-top: 1px solid #30363d; overflow: hidden;
         display: grid; grid-template-columns: 1fr 1fr; }
  .col { overflow-y: auto; padding: 6px 10px; font-size: 11px; line-height: 1.4;
         font-family: "SF Mono", monospace; }
  .col + .col { border-left: 1px solid #30363d; }
  .col h3 { font-size: 10px; text-transform: uppercase; color: #8b949e; margin-bottom: 4px;
            letter-spacing: 0.5px; position: sticky; top: 0; background: #0d1117;
            padding-bottom: 2px; }
  .ev { white-space: pre; color: #c9d1d9; }
  .ev .mod { color: #f0883e; }
  .ev .ws { color: #3fb950; }
  .ev .err { color: #f85149; }
</style>
</head>
<body>

<div class="topbar">
  <b>Zellij Shift+Click Prototype</b>
  <span>session: __SESSION__</span>
  <button onclick="reloadFrame()">reload iframe</button>
  <button onclick="clearLogs()">clear logs</button>
  <button onclick="dumpAssets()">list dumped assets</button>
  <span id="status" style="color:#8b949e;"></span>
</div>

<div id="frameWrap">
  <iframe id="zellij" src="/zellij/__SESSION__"></iframe>
</div>

<div class="log">
  <div class="col" id="mouseCol">
    <h3>mouse / key events (iframe)</h3>
  </div>
  <div class="col" id="wsCol">
    <h3>websocket mouse frames (server)</h3>
  </div>
</div>

<script>
const mouseCol = document.getElementById("mouseCol");
const wsCol    = document.getElementById("wsCol");
const status_  = document.getElementById("status");
const iframe   = document.getElementById("zellij");

function logEv(col, html) {
  const div = document.createElement("div");
  div.className = "ev";
  div.innerHTML = html;
  col.appendChild(div);
  col.scrollTop = col.scrollHeight;
}

function fmtMods(e) {
  const m = [];
  if (e.shiftKey) m.push("SHIFT");
  if (e.ctrlKey)  m.push("CTRL");
  if (e.altKey)   m.push("ALT");
  if (e.metaKey)  m.push("META");
  return m.length ? `<span class="mod">${m.join("+")}</span>` : "—";
}

function attach(target, label) {
  ["mousedown", "mouseup", "click", "auxclick"].forEach(type => {
    target.addEventListener(type, e => {
      logEv(mouseCol,
        `[${label}] ${type.padEnd(9)} btn=${e.button} mods=${fmtMods(e)} ` +
        `target=${e.target?.tagName ?? "?"}`);
    }, true);
  });
}

// Outer wrapper — captures events that bubble to the iframe element
attach(iframe, "outer");

// Try to reach into the iframe's document (same-origin via proxy)
function tryAttachInner() {
  try {
    const doc = iframe.contentDocument;
    if (!doc) throw new Error("no contentDocument");
    attach(doc, "inner");
    // Also watch what xterm.js does at the canvas / textarea level
    const handler = e => {
      logEv(mouseCol,
        `[inner] ${e.type.padEnd(9)} btn=${e.button} mods=${fmtMods(e)} ` +
        `target=${e.target?.tagName ?? "?"} ` +
        `defaultPrevented=${e.defaultPrevented}`);
    };
    ["mousedown", "mouseup", "click", "auxclick", "contextmenu"]
      .forEach(t => doc.addEventListener(t, handler, true));
    status_.textContent = "inner-doc listeners attached";
  } catch (e) {
    status_.textContent = "could not attach inner: " + e.message;
  }
}
iframe.addEventListener("load", () => setTimeout(tryAttachInner, 200));

// Poll the server for WS mouse-frame log
async function pollWs() {
  try {
    const r = await fetch("/debug/ws-log");
    const lines = await r.json();
    wsCol.innerHTML = '<h3>websocket mouse frames (server)</h3>' +
      lines.map(l => `<div class="ev"><span class="ws">${escapeHtml(l)}</span></div>`).join("");
    wsCol.scrollTop = wsCol.scrollHeight;
  } catch (e) { /* ignore */ }
}
setInterval(pollWs, 1000);

function escapeHtml(s) {
  return s.replace(/[&<>]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;"}[c]));
}

function reloadFrame() { iframe.src = iframe.src; }
function clearLogs()   {
  mouseCol.innerHTML = '<h3>mouse / key events (iframe)</h3>';
  wsCol.innerHTML    = '<h3>websocket mouse frames (server)</h3>';
}
async function dumpAssets() {
  const r = await fetch("/debug/asset-list");
  const list = await r.json();
  alert("Assets dumped to _assets_dump/:\\n\\n" + list.join("\\n"));
}
</script>

</body>
</html>
"""

if __name__ == "__main__":
    import uvicorn

    print(
        """
╔══════════════════════════════════════════════════════════════╗
║  Zellij Shift+Click prototype                                ║
║                                                              ║
║  - Session auto-attaches with three URLs visible.            ║
║  - Hover one: tooltip should appear.                         ║
║  - Shift+Click: should open a new tab (currently broken).    ║
║  - Watch both log columns for what the browser saw and       ║
║    what the WebSocket actually shipped to Zellij.            ║
║  - Edit _patch_input_js / _patch_mouse_or_link_js to test    ║
║    a theory, then click "reload iframe".                     ║
║  - Original asset bodies land in ./_assets_dump/ for the     ║
║    Read tool.                                                ║
╚══════════════════════════════════════════════════════════════╝
"""
    )
    uvicorn.run(app, host="127.0.0.1", port=APP_PORT)
