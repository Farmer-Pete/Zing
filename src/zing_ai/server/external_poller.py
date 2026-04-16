from __future__ import annotations

import asyncio
import logging
import os
from datetime import UTC, datetime

import httpx

from zing_ai.config import CommandCenterConfig
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
        # Validate config + env vars at init; surface in cache.last_error.
        problems: list[str] = []
        if not config.github_repo:
            problems.append(
                "GitHub repo not configured (set command_center.github_repo in /config)"
            )
        if not os.environ.get("LINEAR_API_KEY"):
            problems.append("LINEAR_API_KEY env var not set")
        if not os.environ.get("GITHUB_TOKEN"):
            problems.append("GITHUB_TOKEN env var not set")
        if problems:
            self._cache.last_error = " | ".join(problems)
        else:
            self._linear = LinearClient(api_key=os.environ["LINEAR_API_KEY"])
            self._github = GitHubClient(token=os.environ["GITHUB_TOKEN"])

    async def aclose(self) -> None:
        if self._linear:
            await self._linear.aclose()
        if self._github:
            await self._github.aclose()

    async def _poll_once(self) -> None:
        """One full poll cycle. Testable in isolation."""
        if self._linear is None or self._github is None:
            # Config/env missing; cache.last_error was set in __init__.
            # Still dispatch poll_status so SSE clients that connect AFTER
            # startup see the persistent configuration error (otherwise the
            # error banner only updates on initial page-render).
            self._dispatch("poll_status")
            return
        try:
            issues, prs, username = await asyncio.gather(
                self._linear.fetch_my_open_issues(),
                self._github.fetch_open_prs(self._config.github_repo),
                self._github.fetch_current_user(),
            )
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
        prev_issue_ids = {i.identifier for i in self._cache.issues}
        prev_pr_numbers = {p.number for p in self._cache.prs}
        self._cache.issues = issues
        self._cache.prs = prs
        self._cache.github_username = username
        # Store as tz-aware UTC so the ISO string carries +00:00 and browsers
        # don't parse it as local time.
        self._cache.last_polled_at = datetime.now(UTC)
        self._cache.last_error = None
        # Dispatch SSE events for what changed.
        new_issue_ids = {i.identifier for i in issues}
        new_pr_numbers = {p.number for p in prs}
        if prev_issue_ids != new_issue_ids or prev_pr_numbers != new_pr_numbers:
            self._dispatch("hub_added")  # full re-render of hub list
        # For now: just re-render inbox + emit poll_status; per-hub diffing is a v2 nicety.
        self._dispatch("inbox_changed")
        self._dispatch("poll_status")

    def _dispatch(self, event: str) -> None:
        # Snapshot so an SSE handler's disconnect-removal during iteration
        # can't skip queues or raise — the list is shared with app.state.cc_queues.
        for q in list(self._queues):
            try:
                q.put_nowait(event)
            except asyncio.QueueFull:
                logger.warning("SSE queue full; dropping event %s", event)

    async def run(self) -> None:
        """Forever loop. Survives single-iteration failures."""
        poll_seconds = min(self._config.linear_poll_seconds, self._config.github_poll_seconds)
        while True:
            try:
                await self._poll_once()
            except Exception:
                logger.exception("Poller iteration failed; will retry")
            await asyncio.sleep(poll_seconds)
