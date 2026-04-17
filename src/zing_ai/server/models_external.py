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
    state_type: str = (
        "unstarted"  # state.type e.g. "started", "unstarted", "completed", "cancelled"
    )
    priority: int = 0  # 0=No priority, 1=Urgent, 2=High, 3=Medium, 4=Low
    assignee: str | None
    team: str | None  # team.name; null on triage / unassigned-team issues
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
    author: str = ""
    repo: str = ""
    requested_reviewers: list[str]
    review_decision: Literal["APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"] | None
    mergeable_state: str
    ci_status: str | None
    url: str
    updated_at: datetime
    merged_at: datetime | None = None


# ---------------------------------------------------------------------------
# Kanban models
# ---------------------------------------------------------------------------

KanbanColumn = Literal["todo", "in_progress", "needs_review", "done"]


class KanbanCard(BaseModel):
    """A card on the Kanban board, grouping a ticket and its related artifacts."""

    key: str  # ticket identifier or PR-only key
    ticket: LinearIssue | None = None  # None for orphan-PR cards
    prs: list[GitHubPR] = Field(default_factory=list)
    sessions: list[Session] = Field(default_factory=list)
    audit_steps: list[WorkflowStep] = Field(default_factory=list)


class KanbanView(BaseModel):
    """The full Kanban board view, split into four columns."""

    todo: list[KanbanCard] = Field(default_factory=list)
    in_progress: list[KanbanCard] = Field(default_factory=list)
    needs_review: list[KanbanCard] = Field(default_factory=list)
    done: list[KanbanCard] = Field(default_factory=list)
