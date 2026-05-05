"""Per-session ttyd subprocess registry.

Each entry in ``app.state.ttyd_procs`` records the ttyd process, its
loopback port, and a monotonic ``last_used_at`` timestamp consumed by the
idle reaper. Route handlers go through :func:`ensure_ttyd_for` — the
registry itself is never mutated directly outside this module.

ttyd is spawned with ``-i 127.0.0.1`` so the port is loopback-only. The
iframe URL returned to the client is ``http://127.0.0.1:<port>/`` — the
parent page (Zing) is on a different port, so the iframe is cross-origin
loopback. This is the same trust model the pre-Zellij implementation used.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import shutil
import socket
import subprocess
import time
from dataclasses import dataclass

from fastapi import FastAPI

logger = logging.getLogger(__name__)

IDLE_TIMEOUT_SECONDS = 300
SPAWN_TIMEOUT_SECONDS = 3.0
SPAWN_MAX_ATTEMPTS = 3


@dataclass
class _TtydProc:
    proc: subprocess.Popen
    port: int
    last_used_at: float


def _get_registry(app: FastAPI) -> tuple[dict[str, _TtydProc], asyncio.Lock]:
    procs = getattr(app.state, "ttyd_procs", None)
    if procs is None:
        procs = {}
        app.state.ttyd_procs = procs
    lock = getattr(app.state, "ttyd_lock", None)
    if lock is None:
        lock = asyncio.Lock()
        app.state.ttyd_lock = lock
    return procs, lock


def _find_free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


async def _await_listening(proc: subprocess.Popen, port: int, timeout: float) -> bool:
    loop = asyncio.get_event_loop()
    deadline = loop.time() + timeout
    while loop.time() < deadline:
        if proc.poll() is not None:
            return False
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                return True
        except OSError:
            await asyncio.sleep(0.05)
    return False


async def ensure_ttyd_for(app: FastAPI, tmux_session: str) -> str | None:
    """Spawn or reuse a ttyd process for *tmux_session*. Return iframe URL or None.

    Returns ``None`` when ttyd or tmux is missing from PATH, when the spawned
    ttyd never starts listening within the spawn timeout, or when every port
    allocation collided. Callers should surface a toast in those cases.
    """
    ttyd_path = shutil.which("ttyd")
    tmux_path = shutil.which("tmux")
    if not ttyd_path or not tmux_path:
        logger.warning("ttyd_unavailable ttyd=%s tmux=%s", bool(ttyd_path), bool(tmux_path))
        return None

    procs, lock = _get_registry(app)
    async with lock:
        existing = procs.get(tmux_session)
        if existing is not None and existing.proc.poll() is None:
            existing.last_used_at = time.monotonic()
            return f"http://127.0.0.1:{existing.port}/"
        if existing is not None:
            procs.pop(tmux_session, None)

        from zing_ai.server.tmux_config import ensure_tmux_config

        conf_path = ensure_tmux_config()

        for _ in range(SPAWN_MAX_ATTEMPTS):
            port = _find_free_port()
            try:
                proc = subprocess.Popen(
                    [
                        ttyd_path,
                        "--writable",
                        "--port",
                        str(port),
                        "-i",
                        "127.0.0.1",
                        tmux_path,
                        "-f",
                        str(conf_path),
                        "attach",
                        "-t",
                        tmux_session,
                    ],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.PIPE,
                    start_new_session=True,
                )
            except OSError:
                logger.exception("ttyd_spawn_failed session=%s", tmux_session)
                return None

            if await _await_listening(proc, port, SPAWN_TIMEOUT_SECONDS):
                procs[tmux_session] = _TtydProc(proc=proc, port=port, last_used_at=time.monotonic())
                logger.info("ttyd_spawned session=%s port=%s", tmux_session, port)
                return f"http://127.0.0.1:{port}/"

            with contextlib.suppress(Exception):
                proc.kill()
        return None


def kill_ttyd_for(app: FastAPI, tmux_session: str) -> None:
    """Terminate the ttyd for *tmux_session*, if any. No-op when not running."""
    procs = getattr(app.state, "ttyd_procs", None)
    if not procs:
        return
    entry = procs.pop(tmux_session, None)
    if entry is None:
        return
    with contextlib.suppress(Exception):
        entry.proc.terminate()


def kill_all_ttyd(app: FastAPI) -> None:
    """Terminate every live ttyd. Called from the app shutdown hook."""
    procs = getattr(app.state, "ttyd_procs", None)
    if not procs:
        return
    for name, entry in list(procs.items()):
        with contextlib.suppress(Exception):
            entry.proc.terminate()
        procs.pop(name, None)


async def reap_idle_ttyds(app: FastAPI, max_idle: float = IDLE_TIMEOUT_SECONDS) -> None:
    """One sweep: terminate ttyds idle longer than *max_idle* seconds.

    The caller schedules the periodic loop; this function returns after a
    single pass so it can be tested without timing wires.
    """
    procs = getattr(app.state, "ttyd_procs", None)
    if not procs:
        return
    now = time.monotonic()
    for name, entry in list(procs.items()):
        if entry.proc.poll() is not None:
            procs.pop(name, None)
            continue
        if now - entry.last_used_at > max_idle:
            logger.info(
                "ttyd_idle_reap session=%s idle=%ss",
                name,
                int(now - entry.last_used_at),
            )
            with contextlib.suppress(Exception):
                entry.proc.terminate()
            procs.pop(name, None)
