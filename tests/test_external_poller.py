from __future__ import annotations

import asyncio
import contextlib
import os
import unittest
from datetime import UTC, datetime
from unittest.mock import AsyncMock, MagicMock, patch

from zing_ai.config import CommandCenterConfig
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.external_poller import ExternalPoller
from zing_ai.server.linear_client import LinearAPIError
from zing_ai.server.models_external import GitHubPR, LinearIssue


def _make_config(
    github_repo: str = "owner/repo",
    linear_poll_seconds: int = 60,
    github_poll_seconds: int = 60,
) -> CommandCenterConfig:
    """Build a CommandCenterConfig bypassing pydantic validation for test values."""
    return CommandCenterConfig.model_construct(
        github_repo=github_repo,
        linear_poll_seconds=linear_poll_seconds,
        github_poll_seconds=github_poll_seconds,
    )


def _make_issue(identifier: str = "BAK-1") -> LinearIssue:
    return LinearIssue(
        id="issue-uuid-1",
        identifier=identifier,
        title="Test issue",
        state="In Progress",
        assignee="Alice",
        team="Backend",
        url=f"https://linear.app/issue/{identifier}",
        updated_at=datetime(2026, 1, 15, 10, 30, tzinfo=UTC),
    )


def _make_pr(number: int = 42) -> GitHubPR:
    return GitHubPR(
        number=number,
        title="Test PR",
        state="open",
        draft=False,
        head_ref="feature/x",
        base_ref="main",
        body=None,
        requested_reviewers=[],
        review_decision=None,
        mergeable_state="clean",
        ci_status=None,
        url=f"https://github.com/owner/repo/pull/{number}",
        updated_at=datetime(2026, 3, 10, 12, 0, tzinfo=UTC),
    )


_GOOD_ENV = {"LINEAR_API_KEY": "lin_test_key", "GITHUB_TOKEN": "ghp_test_token"}


class TestExternalPollerInit(unittest.TestCase):
    def test_init_missing_env_sets_last_error(self) -> None:
        """No env vars → cache.last_error populated, no clients created."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _make_config()
        with patch.dict(os.environ, {}, clear=True):
            # Ensure the keys are absent
            os.environ.pop("LINEAR_API_KEY", None)
            os.environ.pop("GITHUB_TOKEN", None)
            poller = ExternalPoller(cache=cache, queues=queues, config=config)
        assert cache.last_error is not None
        assert "LINEAR_API_KEY" in cache.last_error
        assert "GITHUB_TOKEN" in cache.last_error
        assert poller._linear is None
        assert poller._github is None

    def test_init_missing_github_repo_sets_last_error(self) -> None:
        """Env vars set but github_repo='' → cache.last_error contains repo message."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _make_config(github_repo="")
        with patch.dict(os.environ, _GOOD_ENV, clear=False):
            poller = ExternalPoller(cache=cache, queues=queues, config=config)
        assert cache.last_error is not None
        err_lower = cache.last_error.lower()
        assert "github_repo" in err_lower or "github repo" in err_lower
        assert poller._linear is None
        assert poller._github is None


class TestPollOnce(unittest.IsolatedAsyncioTestCase):
    async def test_poll_once_writes_cache(self) -> None:
        """Mock clients, call _poll_once, assert cache fields populated."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _make_config()

        with (
            patch.dict(os.environ, _GOOD_ENV, clear=False),
            # Patch the constructors so no real HTTP clients are created
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=queues, config=config)

        issues = [_make_issue("BAK-1"), _make_issue("BAK-2")]
        prs = [_make_pr(42)]
        username = "octocat"

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(return_value=issues)
        mock_github = MagicMock()
        mock_github.fetch_open_prs = AsyncMock(return_value=prs)
        mock_github.fetch_current_user = AsyncMock(return_value=username)

        poller._linear = mock_linear
        poller._github = mock_github

        await poller._poll_once()

        assert cache.issues == issues
        assert cache.prs == prs
        assert cache.github_username == username
        assert cache.last_polled_at is not None
        assert cache.last_error is None

    async def test_poll_once_handles_linear_error(self) -> None:
        """Mock LinearClient to raise, assert cache.last_error set, poll_status dispatched."""
        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _make_config()

        with (
            patch.dict(os.environ, _GOOD_ENV, clear=False),
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[q], config=config)

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(
            side_effect=LinearAPIError("Linear API error: 500")
        )
        mock_github = MagicMock()
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

    async def test_poll_once_handles_httpx_transport_error(self) -> None:
        """Transport errors (network down, timeout) must surface in cache.last_error."""
        import httpx

        cache = ExternalCache()
        q: asyncio.Queue[str] = asyncio.Queue()
        config = _make_config()

        with (
            patch.dict(os.environ, _GOOD_ENV, clear=False),
            patch("zing_ai.server.external_poller.LinearClient"),
            patch("zing_ai.server.external_poller.GitHubClient"),
        ):
            poller = ExternalPoller(cache=cache, queues=[q], config=config)

        mock_linear = MagicMock()
        mock_linear.fetch_my_open_issues = AsyncMock(
            side_effect=httpx.ConnectError("Connection refused")
        )
        mock_github = MagicMock()
        mock_github.fetch_open_prs = AsyncMock(return_value=[])
        mock_github.fetch_current_user = AsyncMock(return_value="user")

        poller._linear = mock_linear
        poller._github = mock_github

        # Must not raise — transport errors are caught just like API errors.
        await poller._poll_once()

        assert cache.last_error is not None
        assert "Connection refused" in cache.last_error
        event = q.get_nowait()
        assert event == "poll_status"


class TestRunLoop(unittest.IsolatedAsyncioTestCase):
    async def test_run_survives_exception(self) -> None:
        """_poll_once raises on first call, succeeds on second. run() must continue."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = []
        config = _make_config()

        with (
            patch.dict(os.environ, _GOOD_ENV, clear=False),
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


class TestDispatch(unittest.TestCase):
    def test_dispatch_to_multiple_queues(self) -> None:
        """_dispatch('inbox_changed') puts the event into all 3 queues."""
        cache = ExternalCache()
        queues: list[asyncio.Queue[str]] = [asyncio.Queue() for _ in range(3)]
        config = _make_config()

        with patch.dict(os.environ, {}, clear=True):
            os.environ.pop("LINEAR_API_KEY", None)
            os.environ.pop("GITHUB_TOKEN", None)
            poller = ExternalPoller(cache=cache, queues=queues, config=config)

        poller._dispatch("inbox_changed")

        for q in queues:
            event = q.get_nowait()
            assert event == "inbox_changed"


if __name__ == "__main__":
    unittest.main()
