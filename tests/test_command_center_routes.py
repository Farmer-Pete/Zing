"""Tests for Command Center route endpoints."""

from __future__ import annotations

import asyncio
import tempfile
import threading
import time
import unittest
from collections.abc import Awaitable, Callable
from datetime import datetime
from pathlib import Path
from unittest.mock import patch

from fastapi import FastAPI
from fastapi.testclient import TestClient

from zing_ai.server.app import create_app
from zing_ai.server.models import Session, SessionState, WorkflowStep
from zing_ai.server.models_external import GitHubPR, LinearIssue
from zing_ai.server.sessions import SessionManager


def _make_issue(
    *,
    identifier: str = "BAK-1",
    title: str = "Fix bug",
    team: str = "Back End",
    assignee: str | None = "alice",
) -> LinearIssue:
    return LinearIssue(
        id="uuid-" + identifier,
        identifier=identifier,
        title=title,
        state="In Progress",
        assignee=assignee,
        team=team,
        url=f"https://linear.app/t/{identifier}",
        updated_at=datetime(2026, 4, 16, 0, 0, 0),
    )


def _make_pr(
    *,
    number: int = 1,
    title: str = "Title",
    head_ref: str = "feature",
    body: str | None = None,
) -> GitHubPR:
    return GitHubPR(
        number=number,
        title=title,
        state="open",
        draft=False,
        head_ref=head_ref,
        base_ref="main",
        body=body,
        requested_reviewers=[],
        review_decision=None,
        mergeable_state="clean",
        ci_status=None,
        url=f"https://github.com/o/r/pull/{number}",
        updated_at=datetime(2026, 4, 16, 0, 0, 0),
    )


def _make_workflow_step(*, step_name: str, sequence: int = 0) -> WorkflowStep:
    return WorkflowStep(step_name=step_name, sequence=sequence)


class CommandCenterTestBase(unittest.TestCase):
    """Base class that sets up a TestClient with an isolated SessionManager."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.data_dir = Path(self._tmp.name)
        self.manager = SessionManager(data_dir=self.data_dir)
        self.app_instance = create_app(session_manager=self.manager)
        self.client = TestClient(self.app_instance)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _get_fastapi_app(self) -> FastAPI:
        """Unwrap the ASGI middleware stack to reach the FastAPI instance."""
        # MCPDebugMiddleware wraps a Starlette app; the last Mount route is fastapi_app.
        starlette_app = self.app_instance.app  # type: ignore[attr-defined]
        for route in starlette_app.routes:
            app = getattr(route, "app", None)
            if isinstance(app, FastAPI):
                return app
        raise RuntimeError("Could not locate FastAPI app inside ASGI stack")


class TestCommandCenterRoutes(CommandCenterTestBase):
    """Tests for the /command-center endpoint."""

    def test_get_command_center_returns_200_with_empty_cache(self) -> None:
        """GET /command-center returns 200 with an empty ExternalCache."""
        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("text/html", resp.headers["content-type"])

    def test_page_contains_command_center_in_nav(self) -> None:
        """The response includes the Command Center nav link."""
        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("Command Center", resp.text)

    def test_empty_inbox_shows_cute_message(self) -> None:
        """With no inbox items the inbox renders the empty-state message."""
        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("nothing to do", resp.text)

    def test_hub_renders_with_signal_key_attributes(self) -> None:
        """Injecting an issue produces a hub with correct Datastar signal key attributes."""
        fastapi_app = self._get_fastapi_app()
        issue = _make_issue(identifier="BAK-1179", title="Search broken")
        fastapi_app.state.external_cache.issues = [issue]

        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        # signal_key = "BAK-1179".lower().replace("-", "_") → "bak_1179"
        self.assertIn('data-signals:open.bak_1179="false"', resp.text)

    def test_yellow_urgency_renders_on_hot_hub(self) -> None:
        """A hub with a READY audit step and findings renders with class 'hot'."""
        fastapi_app = self._get_fastapi_app()
        issue = _make_issue(identifier="BAK-9000", title="Hot issue")

        # Build a session with an audit step in READY state with a finding.
        audit_step = _make_workflow_step(step_name="build-audit", sequence=0)
        audit_step.state = SessionState.READY
        # Add a minimal finding so _compute_urgency sees findings as truthy.
        from zing_ai.server.models import TextFinding

        audit_step.findings = [TextFinding(title="Found something")]  # type: ignore[list-item]

        session = Session(
            session_id="sess-hot",
            title="Hot Session",
            ticket_id="BAK-9000",
            steps=[audit_step],
        )
        self.manager._sessions["sess-hot"] = session  # type: ignore[attr-defined]

        fastapi_app.state.external_cache.issues = [issue]

        resp = self.client.get("/command-center")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("hot", resp.text)


class _SSEAsyncHelpers:
    """Shared helpers for the async-driven SSE tests.

    Kept separate so ``TestCommandCenterSSEAsync`` can inherit from
    ``IsolatedAsyncioTestCase`` without inheriting the sync ``setUp`` chain
    that ``CommandCenterTestBase`` provides.
    """


class TestCommandCenterSSE(CommandCenterTestBase):
    """Tests for the /command-center/events SSE endpoint (synchronous path)."""

    def _get_cc_queues(self) -> list:
        """Return the cc_queues list from the FastAPI app state."""
        fastapi_app = self._get_fastapi_app()
        return fastapi_app.state.cc_queues  # type: ignore[attr-defined]

    # ------------------------------------------------------------------
    # Helper: call the SSE handler directly and collect output chunks.
    # ------------------------------------------------------------------

    @staticmethod
    async def _collect_cc_events(
        app_instance,  # noqa: ANN001
        events: list[str],
    ) -> str:
        """Call command_center_events directly, push events via queue, return SSE text."""
        from unittest.mock import MagicMock

        from zing_ai.server.routes_command_center import command_center_events

        # Find the FastAPI app inside the ASGI stack.
        starlette_app = app_instance.app  # type: ignore[attr-defined]
        fastapi_app = None
        for route in starlette_app.routes:
            candidate = getattr(route, "app", None)
            if isinstance(candidate, FastAPI):
                fastapi_app = candidate
                break
        assert fastapi_app is not None

        request = MagicMock()
        request.app = fastapi_app

        # Pre-populate a queue with our test events.
        queue: asyncio.Queue[str] = asyncio.Queue(maxsize=100)
        for ev in events:
            queue.put_nowait(ev)

        real_wait_for = asyncio.wait_for
        delivery_count = 0

        async def _fast_wait_for(coro, *, timeout=None):  # noqa: ANN001,ANN201
            nonlocal delivery_count
            delivery_count += 1
            if delivery_count <= len(events):
                return await real_wait_for(coro, timeout=0.5)
            coro.close()
            raise asyncio.CancelledError

        chunks: list[str] = []
        with (
            patch(
                "zing_ai.server.routes_command_center.asyncio.Queue",
                return_value=queue,
            ),
            patch(
                "zing_ai.server.routes_command_center.asyncio.wait_for",
                _fast_wait_for,
            ),
        ):
            try:
                response = await command_center_events(request)
                async for chunk in response.body_iterator:
                    if isinstance(chunk, bytes):
                        chunk = chunk.decode()
                    chunks.append(chunk)  # type: ignore[arg-type]
            except (asyncio.CancelledError, StopAsyncIteration):
                pass

        # Clean up queue if still registered.
        cc_queues = fastapi_app.state.cc_queues  # type: ignore[attr-defined]
        if queue in cc_queues:
            cc_queues.remove(queue)

        return "".join(chunks)

    # ------------------------------------------------------------------
    # Tests
    # ------------------------------------------------------------------

    def test_sse_connection_appends_queue(self) -> None:
        """Opening the SSE connection appends a queue to cc_queues."""
        cc_queues = self._get_cc_queues()
        initial_len = len(cc_queues)

        # Use TestClient streaming; the connection stays open until we stop iterating.
        stop_event = threading.Event()

        def _stream() -> None:
            with self.client.stream("GET", "/command-center/events") as resp:
                assert resp.status_code == 200
                # Signal the main thread that the connection is open.
                stop_event.set()
                # Block in iteration so the server keeps the queue registered.
                for _ in resp.iter_lines():
                    if stop_event.is_set() and len(cc_queues) > initial_len:
                        break

        t = threading.Thread(target=_stream, daemon=True)
        t.start()
        stop_event.wait(timeout=5.0)
        # Give the server a moment to append the queue.
        time.sleep(0.1)
        self.assertGreater(len(cc_queues), initial_len)


def _run_async_in_thread[T](coro_factory: Callable[[], Awaitable[T]]) -> T:
    """Run an async coroutine in a worker thread with its own event loop.

    Necessary because pytest-playwright fixtures install a long-lived asyncio
    loop on the main thread — ``asyncio.run`` and ``IsolatedAsyncioTestCase``
    both refuse to start another loop when one is already running in the same
    thread. A fresh thread has no running loop.
    """
    value: list[T] = []
    error: list[BaseException] = []

    def _worker() -> None:
        loop = asyncio.new_event_loop()
        try:
            value.append(loop.run_until_complete(coro_factory()))
        except BaseException as exc:  # noqa: BLE001
            error.append(exc)
        finally:
            loop.close()

    t = threading.Thread(target=_worker)
    t.start()
    t.join()
    if error:
        raise error[0]
    return value[0]


class TestCommandCenterSSEAsync(CommandCenterTestBase):
    """Async SSE tests that tolerate a co-resident asyncio loop.

    Runs the async test bodies in a worker thread so they always own a fresh
    event loop, even when pytest-playwright's fixtures have a main-thread loop
    already running. This keeps the tests green regardless of test ordering.
    """

    def test_sse_dispatches_inbox_changed(self) -> None:
        """An inbox_changed event causes a patch to #inbox-list."""
        app_instance = self.app_instance

        async def _coro() -> str:
            return await TestCommandCenterSSE._collect_cc_events(app_instance, ["inbox_changed"])

        body = _run_async_in_thread(_coro)
        self.assertIn("#inbox-list", body)

    def test_sse_disconnect_removes_queue(self) -> None:
        """After the SSE connection closes, the queue is removed from cc_queues."""
        fastapi_app = self._get_fastapi_app()

        async def _coro() -> int:
            from unittest.mock import MagicMock

            from zing_ai.server.routes_command_center import command_center_events

            request = MagicMock()
            request.app = fastapi_app

            queue: asyncio.Queue[str] = asyncio.Queue(maxsize=100)

            async def _immediate_cancel(coro, *, timeout=None):  # noqa: ANN001,ANN201
                coro.close()
                raise asyncio.CancelledError

            with (
                patch(
                    "zing_ai.server.routes_command_center.asyncio.Queue",
                    return_value=queue,
                ),
                patch(
                    "zing_ai.server.routes_command_center.asyncio.wait_for",
                    _immediate_cancel,
                ),
            ):
                try:
                    response = await command_center_events(request)
                    async for _ in response.body_iterator:
                        pass
                except (asyncio.CancelledError, StopAsyncIteration):
                    pass

            return len(fastapi_app.state.cc_queues)  # type: ignore[attr-defined]

        remaining = _run_async_in_thread(_coro)
        self.assertEqual(remaining, 0)
