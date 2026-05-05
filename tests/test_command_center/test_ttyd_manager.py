"""Tests for the ttyd subprocess registry."""

from __future__ import annotations

import asyncio
import time
import unittest
from types import SimpleNamespace
from typing import cast
from unittest.mock import MagicMock, patch

from fastapi import FastAPI

from zing_ai.server.ttyd_manager import (
    _TtydProc,
    ensure_ttyd_for,
    kill_all_ttyd,
    kill_ttyd_for,
    reap_idle_ttyds,
)


def _fake_app() -> FastAPI:
    """Build a minimal app stand-in with a writable .state SimpleNamespace."""
    return cast(FastAPI, SimpleNamespace(state=SimpleNamespace()))


class TestKillTtydFor(unittest.TestCase):
    """kill_ttyd_for is the route-side hook on /command-center/kill-session."""

    def test_no_op_when_no_registry(self) -> None:
        app = _fake_app()
        kill_ttyd_for(app, "any-name")  # no exception

    def test_no_op_when_session_not_in_registry(self) -> None:
        app = _fake_app()
        app.state.ttyd_procs = {}
        kill_ttyd_for(app, "missing")
        self.assertEqual(app.state.ttyd_procs, {})

    def test_terminates_and_removes_entry(self) -> None:
        app = _fake_app()
        proc = MagicMock()
        proc.poll.return_value = None
        app.state.ttyd_procs = {
            "zing-x": _TtydProc(proc=proc, port=12345, last_used_at=time.monotonic()),
        }
        kill_ttyd_for(app, "zing-x")
        proc.terminate.assert_called_once()
        self.assertNotIn("zing-x", app.state.ttyd_procs)


class TestKillAllTtyd(unittest.TestCase):
    """kill_all_ttyd is invoked from the lifespan shutdown hook."""

    def test_terminates_every_entry_and_clears_registry(self) -> None:
        app = _fake_app()
        p1, p2 = MagicMock(), MagicMock()
        app.state.ttyd_procs = {
            "zing-a": _TtydProc(proc=p1, port=1, last_used_at=0.0),
            "zing-b": _TtydProc(proc=p2, port=2, last_used_at=0.0),
        }
        kill_all_ttyd(app)
        p1.terminate.assert_called_once()
        p2.terminate.assert_called_once()
        self.assertEqual(app.state.ttyd_procs, {})

    def test_no_registry_is_silent(self) -> None:
        app = _fake_app()
        kill_all_ttyd(app)  # no exception


class TestReapIdleTtyds(unittest.IsolatedAsyncioTestCase):
    """reap_idle_ttyds runs a single sweep; the lifespan loop schedules it."""

    async def test_terminates_idle_entry(self) -> None:
        app = _fake_app()
        proc = MagicMock()
        proc.poll.return_value = None
        # Backdate last_used_at so the entry is past the threshold.
        app.state.ttyd_procs = {
            "zing-x": _TtydProc(proc=proc, port=1, last_used_at=time.monotonic() - 9999),
        }
        await reap_idle_ttyds(app, max_idle=300)
        proc.terminate.assert_called_once()
        self.assertNotIn("zing-x", app.state.ttyd_procs)

    async def test_keeps_recent_entry(self) -> None:
        app = _fake_app()
        proc = MagicMock()
        proc.poll.return_value = None
        app.state.ttyd_procs = {
            "zing-x": _TtydProc(proc=proc, port=1, last_used_at=time.monotonic()),
        }
        await reap_idle_ttyds(app, max_idle=300)
        proc.terminate.assert_not_called()
        self.assertIn("zing-x", app.state.ttyd_procs)

    async def test_drops_already_dead_entry(self) -> None:
        app = _fake_app()
        proc = MagicMock()
        proc.poll.return_value = 0  # already exited
        app.state.ttyd_procs = {
            "zing-x": _TtydProc(proc=proc, port=1, last_used_at=time.monotonic()),
        }
        await reap_idle_ttyds(app, max_idle=300)
        # Dead entries get dropped without a redundant terminate().
        proc.terminate.assert_not_called()
        self.assertNotIn("zing-x", app.state.ttyd_procs)


class TestEnsureTtydFor(unittest.IsolatedAsyncioTestCase):
    """ensure_ttyd_for is the spawn-or-reuse entry point used by the routes."""

    async def test_returns_none_when_ttyd_missing(self) -> None:
        app = _fake_app()
        with patch(
            "zing_ai.server.ttyd_manager.shutil.which",
            side_effect=lambda b: None if b == "ttyd" else "/usr/bin/tmux",
        ):
            url = await ensure_ttyd_for(app, "zing-x")
        self.assertIsNone(url)

    async def test_returns_none_when_tmux_missing(self) -> None:
        app = _fake_app()
        with patch(
            "zing_ai.server.ttyd_manager.shutil.which",
            side_effect=lambda b: None if b == "tmux" else "/usr/bin/ttyd",
        ):
            url = await ensure_ttyd_for(app, "zing-x")
        self.assertIsNone(url)

    async def test_reuses_alive_proc_and_bumps_last_used_at(self) -> None:
        app = _fake_app()
        proc = MagicMock()
        proc.poll.return_value = None  # alive
        original_ts = time.monotonic() - 100
        app.state.ttyd_procs = {
            "zing-x": _TtydProc(proc=proc, port=54321, last_used_at=original_ts),
        }
        app.state.ttyd_lock = asyncio.Lock()

        with patch("zing_ai.server.ttyd_manager.shutil.which", return_value="/x"):
            url = await ensure_ttyd_for(app, "zing-x")

        self.assertEqual(url, "http://127.0.0.1:54321/")
        # last_used_at advanced past the original (so the reaper won't kill us).
        self.assertGreater(app.state.ttyd_procs["zing-x"].last_used_at, original_ts)


if __name__ == "__main__":
    unittest.main()
