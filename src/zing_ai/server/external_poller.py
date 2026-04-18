from __future__ import annotations

import asyncio
import logging
from datetime import UTC, datetime

import httpx

from zing_ai.config import CommandCenterConfig, load_config
from zing_ai.server.external_cache import ExternalCache
from zing_ai.server.github_client import GitHubAPIError, GitHubClient
from zing_ai.server.linear_client import LinearAPIError, LinearClient

logger = logging.getLogger(__name__)


def _summarise_poll_error(exc: Exception) -> str:
    """Return a short, Datastar-safe error summary for *exc*.

    Raw API response bodies are logged verbosely by the clients; the
    user-visible ``cache.last_error`` is kept terse so it survives round-
    tripping through the inline ``data-signals`` JSON envelope.
    """
    status_code = getattr(exc, "status_code", None)
    if isinstance(exc, LinearAPIError):
        return f"Linear API error (HTTP {status_code})" if status_code else "Linear API error"
    if isinstance(exc, GitHubAPIError):
        return f"GitHub API error (HTTP {status_code})" if status_code else "GitHub API error"
    if isinstance(exc, httpx.TransportError):
        return "Network error reaching Linear/GitHub"
    return "External poll failed"


class ExternalPoller:
    def __init__(
        self,
        cache: ExternalCache,
        queues: list[asyncio.Queue[str]],
        config: CommandCenterConfig,
    ) -> None:
        self._cache = cache
        self._queues = queues
        self._config = config
        self._linear: LinearClient | None = None
        self._github: GitHubClient | None = None
        self._last_slow_poll: datetime | None = None
        # Try creating clients at startup; if config/env is incomplete the
        # error surfaces in cache.last_error and _ensure_clients returns False.
        self._ensure_clients()

    def _ensure_clients(self) -> bool:
        """Create HTTP clients for whichever APIs have credentials configured.

        Returns True if at least one client is available. When the user updates
        credentials via ``/config``, the next call picks up the changes and
        instantiates the client(s) that were previously ``None``.
        """
        if self._linear is None and self._config.linear_api_key:
            self._linear = LinearClient(api_key=self._config.linear_api_key)
        if self._github is None and self._config.github_token:
            self._github = GitHubClient(token=self._config.github_token)

        if self._linear is None and self._github is None:
            self._cache.last_error = (
                "No API keys configured — set Linear API key and/or GitHub token in /config"
            )
            return False

        # Surface a non-blocking note if only one source is configured.
        problems: list[str] = []
        if self._linear is None:
            problems.append("Linear API key not configured (set in /config)")
        if self._github is None:
            problems.append("GitHub token not configured (set in /config)")
        self._cache.last_error = " | ".join(problems) if problems else None

        return True

    async def aclose(self) -> None:
        if self._linear:
            await self._linear.aclose()
        if self._github:
            await self._github.aclose()

    async def _fetch_prs_for_repos(self, repos: list[str]) -> list:
        """Fetch open PRs for each repo, skipping repos that error."""
        if self._github is None:
            return []
        results = await asyncio.gather(
            *(self._github.fetch_open_prs(repo) for repo in repos),
            return_exceptions=True,
        )
        prs = []
        for repo, result in zip(repos, results, strict=True):
            if isinstance(result, BaseException):
                logger.warning("Skipping %s: %s", repo, result)
            else:
                prs.extend(result)
        return prs

    async def _poll_once(self) -> None:
        """One full poll cycle. Testable in isolation."""
        if not self._ensure_clients():
            # No clients at all; _ensure_clients wrote cache.last_error.
            # Still dispatch poll_status so SSE clients see the error banner.
            self._dispatch("poll_status")
            return

        # --- Fast poll: issues + open PRs ---
        issues: list = []
        prs: list = []
        repos: list[str] = []
        username: str = ""
        active_repos: list[str] = []

        try:
            # Build parallel tasks for whichever APIs are available.
            tasks: dict[str, asyncio.Task] = {}
            if self._linear is not None:
                tasks["issues"] = asyncio.ensure_future(self._linear.fetch_my_open_issues())
            if self._github is not None:
                tasks["repos"] = asyncio.ensure_future(self._github.fetch_writable_repos())
                tasks["username"] = asyncio.ensure_future(self._github.fetch_current_user())

            results = await asyncio.gather(*tasks.values(), return_exceptions=True)
            result_map = dict(zip(tasks.keys(), results, strict=True))

            # Check for errors in any task.
            for _key, result in result_map.items():
                if isinstance(result, asyncio.CancelledError):
                    raise result
                if isinstance(result, (LinearAPIError, GitHubAPIError, httpx.TransportError)):
                    raise result

            if "issues" in result_map and not isinstance(result_map["issues"], BaseException):
                issues = result_map["issues"]
            if "repos" in result_map and not isinstance(result_map["repos"], BaseException):
                repos = result_map["repos"]
                excluded = set(self._config.github_excluded_repos)
                active_repos = [r for r in repos if r not in excluded]
                prs = await self._fetch_prs_for_repos(active_repos)
            if "username" in result_map and not isinstance(result_map["username"], BaseException):
                username = result_map["username"]

        except asyncio.CancelledError:
            raise
        except (LinearAPIError, GitHubAPIError, httpx.TransportError) as e:
            logger.warning("External poll failed: %s", e)
            summary = _summarise_poll_error(e)
            self._cache.last_error = summary
            self._dispatch("poll_status")
            return

        # Build new snapshot. Torn reads accepted (single user, 60s polls).
        self._cache.issues = issues
        self._cache.prs = prs
        self._cache.github_repos = repos
        self._cache.github_username = username
        self._cache.last_polled_at = datetime.now(UTC)
        # Bump the cache version so _build_view's memo invalidates.
        self._cache.version += 1
        self._dispatch("board_changed")
        self._dispatch("poll_status")

        # --- Slow poll (every 5 minutes) ---
        _SLOW_POLL_SECONDS = 300
        now = datetime.now(UTC)
        if (
            self._last_slow_poll is None
            or (now - self._last_slow_poll).total_seconds() >= _SLOW_POLL_SECONDS
        ):
            try:
                slow_tasks: dict[str, asyncio.Task] = {}
                if self._github is not None and active_repos and username:
                    slow_tasks["recent_prs"] = asyncio.ensure_future(
                        self._github.fetch_recent_prs(active_repos, username)
                    )
                if self._linear is not None:
                    slow_tasks["completed"] = asyncio.ensure_future(
                        self._linear.fetch_completed_issues()
                    )
                if slow_tasks:
                    slow_results = await asyncio.gather(
                        *slow_tasks.values(), return_exceptions=True
                    )
                    slow_map = dict(zip(slow_tasks.keys(), slow_results, strict=True))
                    for result in slow_map.values():
                        if isinstance(result, asyncio.CancelledError):
                            raise result
                    # Apply results that succeeded; log and skip failures.
                    for _key, result in slow_map.items():
                        if isinstance(result, BaseException):
                            logger.warning("Slow poll failed: %s", result)
                    if "recent_prs" in slow_map and not isinstance(
                        slow_map["recent_prs"], BaseException
                    ):
                        self._cache.recent_prs = slow_map["recent_prs"]
                    if "completed" in slow_map and not isinstance(
                        slow_map["completed"], BaseException
                    ):
                        self._cache.completed_issues = slow_map["completed"]
                    self._cache.version += 1
                    self._dispatch("board_changed")
                self._last_slow_poll = now
            except asyncio.CancelledError:
                raise
            except (LinearAPIError, GitHubAPIError, httpx.TransportError) as e:
                logger.warning("Slow poll failed: %s", e)

    def _dispatch(self, event: str) -> None:
        # Snapshot so an SSE handler's disconnect-removal during iteration
        # can't skip queues or raise — the list is shared with app.state.cc_queues.
        for q in list(self._queues):
            try:
                q.put_nowait(event)
            except asyncio.QueueFull:
                logger.warning("SSE queue full; dropping event %s", event)

    async def run(self) -> None:
        """Forever loop. Survives single-iteration failures.

        Re-reads the on-disk config at the top of every iteration so changes
        made via ``/config`` (poll intervals, excluded repos) take effect
        without a server restart. The TOML read is microseconds; this runs at
        most once per ``poll_seconds`` (~60s).
        """
        while True:
            # Reload config so /config UI changes apply without restart.
            try:
                new_cc = load_config().command_center
                if new_cc != self._config:
                    self._config = new_cc
                    # Credentials or repo may have changed; close old clients and re-create.
                    if self._linear:
                        await self._linear.aclose()
                    if self._github:
                        await self._github.aclose()
                    self._linear = None
                    self._github = None
            except Exception:
                logger.exception("Failed to reload config; using previous values")
            try:
                await self._poll_once()
            except Exception:
                logger.exception("Poller iteration failed; will retry")
            poll_seconds = self._config.poll_seconds
            await asyncio.sleep(poll_seconds)
