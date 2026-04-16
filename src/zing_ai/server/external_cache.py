"""Transient runtime container for external API snapshots.

A Pydantic model so the whole ``server`` module sticks to one style; the
cache is still a process-local snapshot (never serialized, never
round-tripped), but consistency with :mod:`models_external` is worth the
handful of microseconds per assignment.
"""

from __future__ import annotations

from datetime import datetime

from pydantic import BaseModel, ConfigDict, Field

from zing_ai.server.models_external import GitHubPR, LinearIssue


class ExternalCache(BaseModel):
    """In-memory snapshot of the latest Linear + GitHub poll results."""

    # Disable validate-assignment to keep mutation cheap; this object never
    # round-trips through JSON so we don't need strict validation on writes.
    model_config = ConfigDict(validate_assignment=False, arbitrary_types_allowed=True)

    issues: list[LinearIssue] = Field(default_factory=list)
    prs: list[GitHubPR] = Field(default_factory=list)
    github_username: str = ""
    last_polled_at: datetime | None = None
    last_error: str | None = None
