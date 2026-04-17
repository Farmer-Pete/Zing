from __future__ import annotations

import asyncio
import contextlib
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

from tests.test_command_center.conftest import (
    make_issue as _make_issue,
)
from tests.test_command_center.conftest import (
    make_pr as _make_pr,
)
from zing_ai.config import CommandCenterConfig
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.external_poller import ExternalPoller
from zing_ai.server.github_client import GitHubAPIError
from zing_ai.server.linear_client import LinearAPIError


def _make_config(
    linear_api_key: str = "",
    github_token: str = "",
    github_excluded_repos: list[str] | None = None,
    linear_poll_seconds: int = 60,
    github_poll_seconds: int = 60,
) -> CommandCenterConfig:
    """Build a CommandCenterConfig bypassing pydantic validation for test values."""
    return CommandCenterConfig.model_construct(
        linear_api_key=linear_api_key,
        github_token=github_token,
        github_excluded_repos=github_excluded_repos or [],
        linear_poll_seconds=linear_poll_seconds,
        github_poll_seconds=github_poll_seconds,
    )


def _good_config() -> CommandCenterConfig:
    return _make_config(linear_api_key="lin_test_key", github_token="ghp_test_token")


class TestExternalPollerInit(unittest.TestCase):
    def test_init_missing_keys_sets_last_error(self) -> None:
        """No API keys in config → cache.last_error populated, no clients created."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _make_config()  # no api keys
        poller = ExternalPoller(cache=cache, queues=queues, config=config)
        assert cache.last_error is not None
        assert "Linear API key" in cache.last_error
        assert "GitHub token" in cache.last_error
        assert poller._linear is None
        assert poller._github is None


class TestPollOnce(unittest.IsolatedAsyncioTestCase):
    async def test_poll_once_writes_cache(self) -> None:
        """Mock clients, call _poll_once, assert cache fields populated."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _good_config()

        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=queues, config=config)

        issues = [_make_issue(identifier="BAK-1"), _make_issue(identifier="BAK-2")]
        prs = [_make_pr(number=42)]
        username = "octocat"

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(return_value=issues)
        mock_linear.fetch_completed_issues = AsyncMock(return_value=[])
        mock_github = MagicMock()
        mock_github.fetch_writable_repos = AsyncMock(return_value=["owner/repo"])
        mock_github.fetch_open_prs = AsyncMock(return_value=prs)
        mock_github.fetch_current_user = AsyncMock(return_value=username)
        mock_github.fetch_recent_prs = AsyncMock(return_value=[])

        poller._linear = mock_linear
        poller._github = mock_github

        await poller._poll_once()

        assert cache.issues == issues
        assert cache.prs == prs
        assert cache.github_repos == ["owner/repo"]
        assert cache.github_username == username
        assert cache.last_polled_at is not None
        assert cache.last_error is None
        # Slow poll ran (first iteration, _last_slow_poll was None).
        mock_github.fetch_recent_prs.assert_called_once()
        mock_linear.fetch_completed_issues.assert_called_once()

    async def test_poll_once_handles_linear_error(self) -> None:
        """Mock LinearClient to raise, assert cache.last_error set, poll_status dispatched."""
        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _good_config()

        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[q], config=config)

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(
            side_effect=LinearAPIError("Linear API error: 500")
        )
        mock_github = MagicMock()
        mock_github.fetch_writable_repos = AsyncMock(return_value=["owner/repo"])
        mock_github.fetch_open_prs = AsyncMock(return_value=[])
        mock_github.fetch_current_user = AsyncMock(return_value="user")

        poller._linear = mock_linear
        poller._github = mock_github

        with self.assertLogs("zing_ai.server.external_poller", level="WARNING") as log_cm:
            await poller._poll_once()

        assert cache.last_error is not None
        assert "Linear API error" in cache.last_error
        # poll_status must have been dispatched
        event = q.get_nowait()
        assert event == "poll_status"
        # The failure must be reported to stdout/journal, not just the browser.
        assert any("External poll failed" in msg for msg in log_cm.output)

    async def test_poll_once_skips_repo_on_pr_fetch_error(self) -> None:
        """A GitHubAPIError on one repo skips it but completes the poll normally."""
        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _good_config()

        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[q], config=config)

        good_prs = [_make_pr(number=1)]
        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(return_value=[])
        mock_linear.fetch_completed_issues = AsyncMock(return_value=[])
        mock_github = MagicMock()
        mock_github.fetch_writable_repos = AsyncMock(return_value=["owner/good", "owner/bad"])
        mock_github.fetch_open_prs = AsyncMock(
            side_effect=[good_prs, GitHubAPIError("HTTP 503", status_code=503)]
        )
        mock_github.fetch_current_user = AsyncMock(return_value="user")
        mock_github.fetch_recent_prs = AsyncMock(return_value=[])

        poller._linear = mock_linear
        poller._github = mock_github

        with self.assertLogs("zing_ai.server.external_poller", level="WARNING"):
            await poller._poll_once()

        # Poll succeeded — bad repo was skipped, good repo PRs came through.
        assert cache.last_error is None
        assert cache.prs == good_prs
        assert cache.last_polled_at is not None

    async def test_poll_once_handles_repo_list_error(self) -> None:
        """A GitHubAPIError on fetch_writable_repos surfaces as poll error."""
        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _good_config()

        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[q], config=config)

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(return_value=[])
        mock_github = MagicMock()
        mock_github.fetch_writable_repos = AsyncMock(
            side_effect=GitHubAPIError("HTTP 503", status_code=503)
        )
        mock_github.fetch_current_user = AsyncMock(return_value="user")

        poller._linear = mock_linear
        poller._github = mock_github

        await poller._poll_once()

        assert cache.last_error is not None
        assert "GitHub API error" in cache.last_error
        assert q.get_nowait() == "poll_status"

    async def test_poll_once_dispatches_poll_status_when_config_missing(self) -> None:
        """Regression: missing config must still notify connected SSE clients.

        Without this, clients that connect after startup see no error banner
        update even though cache.last_error is set in __init__.
        """
        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _make_config()  # no api keys -> _linear/_github stay None
        poller = ExternalPoller(cache=cache, queues=[q], config=config)

        assert poller._linear is None
        assert poller._github is None

        await poller._poll_once()

        assert q.get_nowait() == "poll_status"

    async def test_poll_once_handles_httpx_transport_error(self) -> None:
        """Transport errors (network down, timeout) must surface in cache.last_error."""
        import httpx

        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _good_config()

        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[q], config=config)

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(
            side_effect=httpx.ConnectError("Connection refused")
        )
        mock_github = MagicMock()
        mock_github.fetch_writable_repos = AsyncMock(return_value=["owner/repo"])
        mock_github.fetch_open_prs = AsyncMock(return_value=[])
        mock_github.fetch_current_user = AsyncMock(return_value="user")

        poller._linear = mock_linear
        poller._github = mock_github

        # Must not raise — transport errors are caught just like API errors.
        await poller._poll_once()

        assert cache.last_error is not None
        # Summary keeps last_error short (the full body is logged separately).
        assert "Network error" in cache.last_error
        event = q.get_nowait()
        assert event == "poll_status"


class TestRunLoop(unittest.IsolatedAsyncioTestCase):
    async def test_run_survives_exception(self) -> None:
        """_poll_once raises on first call, succeeds on second. run() must continue."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _good_config()

        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=queues, config=config)

        call_count = 0

        async def fake_poll_once() -> None:
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                raise RuntimeError("Simulated poller crash")

        poller._poll_once = fake_poll_once  # type: ignore[method-assign]

        # Patch asyncio.sleep to a no-op so the loop spins fast.
        # Use the real asyncio.sleep captured before patching to avoid recursion.
        _real_sleep = asyncio.sleep

        async def fast_sleep(_seconds: float) -> None:
            # Yield control without waiting
            await _real_sleep(0)

        with (
            patch("zing_ai.server.external_poller.asyncio.sleep", side_effect=fast_sleep),
            contextlib.suppress(TimeoutError),
        ):
            await asyncio.wait_for(poller.run(), timeout=0.5)

        assert call_count >= 2, f"Expected at least 2 iterations, got {call_count}"


class TestSlowPoll(unittest.IsolatedAsyncioTestCase):
    """Verify dual-cadence slow poll (5-minute gate)."""

    def _make_poller(self) -> tuple[ExternalPoller, ExternalCache, MagicMock, MagicMock]:
        cache = ExternalCache()
        config = _good_config()
        with (
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[], config=config)
        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(return_value=[])
        mock_linear.fetch_completed_issues = AsyncMock(return_value=[])
        mock_github = MagicMock()
        mock_github.fetch_writable_repos = AsyncMock(return_value=["owner/repo"])
        mock_github.fetch_open_prs = AsyncMock(return_value=[])
        mock_github.fetch_current_user = AsyncMock(return_value="user")
        mock_github.fetch_recent_prs = AsyncMock(return_value=[])
        poller._linear = mock_linear
        poller._github = mock_github
        return poller, cache, mock_linear, mock_github

    async def test_slow_poll_runs_on_first_call(self) -> None:
        """Slow poll fires immediately on the first _poll_once (no prior timestamp)."""
        poller, cache, mock_linear, mock_github = self._make_poller()
        assert poller._last_slow_poll is None

        await poller._poll_once()

        mock_github.fetch_recent_prs.assert_called_once()
        mock_linear.fetch_completed_issues.assert_called_once()
        assert poller._last_slow_poll is not None

    async def test_slow_poll_skipped_when_recent(self) -> None:
        """Slow poll is skipped when < 300 seconds have elapsed since last run."""
        from datetime import UTC, datetime, timedelta

        poller, cache, mock_linear, mock_github = self._make_poller()
        # Simulate a slow poll that ran 10 seconds ago.
        poller._last_slow_poll = datetime.now(UTC) - timedelta(seconds=10)

        await poller._poll_once()

        mock_github.fetch_recent_prs.assert_not_called()
        mock_linear.fetch_completed_issues.assert_not_called()

    async def test_slow_poll_runs_after_interval_elapsed(self) -> None:
        """Slow poll fires again once >= 300 seconds have elapsed."""
        from datetime import UTC, datetime, timedelta

        poller, cache, mock_linear, mock_github = self._make_poller()
        # Simulate a slow poll that ran more than 5 minutes ago.
        poller._last_slow_poll = datetime.now(UTC) - timedelta(seconds=301)

        await poller._poll_once()

        mock_github.fetch_recent_prs.assert_called_once()
        mock_linear.fetch_completed_issues.assert_called_once()

    async def test_slow_poll_updates_cache(self) -> None:
        """Slow poll stores results in cache.recent_prs and cache.completed_issues."""
        from tests.test_command_center.conftest import make_issue as _make_issue
        from tests.test_command_center.conftest import make_pr as _make_pr

        poller, cache, mock_linear, mock_github = self._make_poller()
        recent_pr = _make_pr(number=99)
        completed = _make_issue(identifier="BAK-99")
        mock_github.fetch_recent_prs.return_value = [recent_pr]
        mock_linear.fetch_completed_issues.return_value = [completed]

        await poller._poll_once()

        assert cache.recent_prs == [recent_pr]
        assert cache.completed_issues == [completed]

    async def test_slow_poll_error_is_logged_not_raised(self) -> None:
        """A GitHubAPIError in the slow poll is logged but does not fail the poll."""
        poller, cache, mock_linear, mock_github = self._make_poller()
        mock_github.fetch_recent_prs.side_effect = GitHubAPIError("rate limited")

        # Must not raise; poll completes normally.
        with self.assertLogs("zing_ai.server.external_poller", level="WARNING") as log_cm:
            await poller._poll_once()

        assert cache.last_error is None  # fast poll still succeeded
        assert any("Slow poll failed" in m for m in log_cm.output)


class TestDispatch(unittest.TestCase):
    def test_dispatch_to_multiple_queues(self) -> None:
        """_dispatch('inbox_changed') puts the event into all 3 queues."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = [asyncio.Queue() for _ in range(3)]
        config = _make_config()  # no api keys — clients stay None, but dispatch still works
        poller = ExternalPoller(cache=cache, queues=queues, config=config)

        poller._dispatch("inbox_changed")

        for q in queues:
            event = q.get_nowait()
            assert event == "inbox_changed"

    def test_dispatch_skips_full_queue_without_affecting_others(self) -> None:
        """A queue at maxsize drops the event + logs a warning; other queues still receive it."""
        import logging as _logging

        cache = ExternalCache()
        full_q: asyncio.Queue[str] = asyncio.Queue(maxsize=1)
        full_q.put_nowait("seed")  # already at capacity
        normal_q: asyncio.Queue[str] = asyncio.Queue()
        config = _make_config()
        poller = ExternalPoller(cache=cache, queues=[full_q, normal_q], config=config)

        with self.assertLogs("zing_ai.server.external_poller", level=_logging.WARNING) as log_cm:
            poller._dispatch("inbox_changed")

        # Full queue still has just its seed (new event dropped).
        self.assertEqual(full_q.get_nowait(), "seed")
        self.assertTrue(full_q.empty())
        # Other queue received the event normally.
        self.assertEqual(normal_q.get_nowait(), "inbox_changed")
        # Drop was logged.
        self.assertTrue(any("queue full" in m for m in log_cm.output))


if __name__ == "__main__":
    unittest.main()
