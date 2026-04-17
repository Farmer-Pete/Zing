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
        """Return True if HTTP clients are available. Try to create them if missing.

        When the user updates credentials via ``/config``, the next call to
        ``_ensure_clients`` picks up the changes and instantiates the client(s)
        that were previously ``None``.
        """
        if self._linear is not None and self._github is not None:
            return True
        problems: list[str] = []
        if not self._config.linear_api_key:
            problems.append("Linear API key not configured (set in /config)")
        if not self._config.github_token:
            problems.append("GitHub token not configured (set in /config)")
        if problems:
            self._cache.last_error = " | ".join(problems)
            return False
        self._linear = LinearClient(api_key=self._config.linear_api_key)
        self._github = GitHubClient(token=self._config.github_token)
        return True

    async def aclose(self) -> None:
        if self._linear:
            await self._linear.aclose()
        if self._github:
            await self._github.aclose()

    async def _fetch_prs_for_repos(self, repos: list[str]) -> list:
        """Fetch open PRs for each repo, skipping repos that error."""
        assert self._github is not None
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
            # Config/env missing; _ensure_clients wrote cache.last_error.
            # Still dispatch poll_status so SSE clients that connect AFTER
            # startup see the persistent configuration error (otherwise the
            # error banner only updates on initial page-render).
            self._dispatch("poll_status")
            return
        # _ensure_clients guarantees both are set when it returns True.
        assert self._linear is not None  # pyright narrow
        assert self._github is not None  # pyright narrow
        try:
            issues, repos, username = await asyncio.gather(
                self._linear.fetch_my_open_issues(),
                self._github.fetch_writable_repos(),
                self._github.fetch_current_user(),
            )
            # Filter out excluded repos, then fetch PRs for each.
            excluded = set(self._config.github_excluded_repos)
            active_repos = [r for r in repos if r not in excluded]
            prs = await self._fetch_prs_for_repos(active_repos)
        except asyncio.CancelledError:
            # Lifespan shutdown: propagate so run()'s cancellation path fires.
            raise
        except (LinearAPIError, GitHubAPIError, httpx.TransportError) as e:
            logger.warning("External poll failed: %s", e)
            # Keep the user-visible error terse — upstream API response bodies
            # (which we already log verbosely) can contain control characters
            # and quotes that break the Datastar-signal JSON envelope.
            summary = _summarise_poll_error(e)
            self._cache.last_error = summary
            self._dispatch("poll_status")
            return
        # Build new snapshot. Torn reads accepted (single user, 60s polls).
        self._cache.issues = issues
        self._cache.prs = prs
        self._cache.github_repos = repos
        self._cache.github_username = username
        # Store as tz-aware UTC so the ISO string carries +00:00 and browsers
        # don't parse it as local time.
        self._cache.last_polled_at = datetime.now(UTC)
        self._cache.last_error = None
        # Bump the cache version so _build_view's memo invalidates.
        self._cache.version += 1
        # Dispatch SSE events for what changed.
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
                recent_prs, completed_issues = await asyncio.gather(
                    self._github.fetch_recent_prs(active_repos, username),
                    self._linear.fetch_completed_issues(),
                )
                self._cache.recent_prs = recent_prs
                self._cache.completed_issues = completed_issues
                self._cache.version += 1
                self._last_slow_poll = now
                self._dispatch("board_changed")
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
                logger.warning("Failed to reload config; using previous values")
            try:
                await self._poll_once()
            except Exception:
                logger.exception("Poller iteration failed; will retry")
            poll_seconds = min(
                self._config.linear_poll_seconds,
                self._config.github_poll_seconds,
            )
            await asyncio.sleep(poll_seconds)
