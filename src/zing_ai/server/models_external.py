from __future__ import annotations

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field

from zing_ai.server.models import Session, WorkflowStep


class LinearIssue(BaseModel):
    """A Linear issue fetched from the Linear API."""

    id: str  # Linear's internal UUID
    identifier: str  # e.g. "BAK-1179"
    title: str
    state: str  # state.name
    assignee: str | None
    team: str  # team.name
    url: str
    updated_at: datetime


class GitHubPR(BaseModel):
    """A GitHub pull request fetched from the GitHub API."""

    number: int
    title: str
    state: Literal["open", "closed", "merged"]
    draft: bool
    head_ref: str
    base_ref: str
    body: str | None
    requested_reviewers: list[str]
    review_decision: Literal["APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"] | None
    mergeable_state: str
    ci_status: str | None
    url: str
    updated_at: datetime


HubUrgency = Literal["hot", "active", "cool"]
HubKind = Literal["ticket", "pr", "session"]
InboxPriority = Literal["high", "medium"]


class Hub(BaseModel):
    """Aggregation model grouping a ticket, PR, or session with related artifacts."""

    id: str  # ticket identifier, "pr-150", or "session-{id}"
    kind: HubKind
    title: str
    team: str | None
    assignee: str | None
    urgency: HubUrgency
    prs: list[GitHubPR] = Field(default_factory=list)
    sessions: list[Session] = Field(default_factory=list)
    audits: list[WorkflowStep] = Field(default_factory=list)
    linear_issue: LinearIssue | None = None

    @property
    def signal_key(self) -> str:
        """JS-safe identifier for use in Datastar signal paths."""
        return self.id.lower().replace("-", "_").replace(" ", "_")


class InboxItem(BaseModel):
    """A single actionable item surfaced in the Command Center inbox."""

    priority: InboxPriority
    action_text: str
    detail_text: str | None
    hub_id: str
    hub_label: str  # e.g. "BAK-1179" or "Standalone"
    time_waiting: str
    target_url: str
