"""Transient runtime container for external API snapshots.

Intentional one-off departure from Pydantic-everywhere: this is a
process-local snapshot that is never validated, never serialized, never
round-tripped through JSON. ``@dataclass(slots=True)`` is lighter.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime

from zing_ai.server.models_external import GitHubPR, LinearIssue


@dataclass(slots=True)
class ExternalCache:
    """In-memory snapshot of the latest Linear + GitHub poll results."""

    issues: list[LinearIssue] = field(default_factory=list)
    prs: list[GitHubPR] = field(default_factory=list)
    github_username: str = ""
    last_polled_at: datetime | None = None
    last_error: str | None = None
