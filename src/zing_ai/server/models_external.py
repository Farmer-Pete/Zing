from __future__ import annotations

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field

from zing_ai.server.models import Session, WorkflowStep
from zing_ai.server.signals import to_signal_key as _to_signal_key


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

    @property
    def signal_key(self) -> str:
        """Return :attr:`identifier` sanitised for use as a Datastar signal name.

        Same purpose as :attr:`KanbanCard.signal_key` — see that property's
        docstring for the rationale. Used by templates that build per-ticket
        signals like ``$busyButtons.start_<signal_key>``.
        """
        return _to_signal_key(self.identifier)


class CICheck(BaseModel):
    """A single CI check run or status context from GitHub."""

    name: str
    status: str  # queued, in_progress, completed
    conclusion: str | None = None  # success, failure, neutral, cancelled, skipped, timed_out
    url: str | None = None


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
    reviewers: list[str] = Field(default_factory=list)  # users who submitted reviews
    reviewer_states: dict[str, str] = Field(
        default_factory=dict
    )  # login -> APPROVED/CHANGES_REQUESTED/COMMENTED/etc.
    review_decision: Literal["APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"] | None
    mergeable_state: str
    ci_status: str | None
    ci_checks: list[CICheck] = Field(default_factory=list)
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
    review_group: str | None = None  # mine_passing, mine_failing, others (needs_review only)
    done_group: str | None = None  # ready_to_merge, completed (done only)
    in_progress_reason: str | None = None  # human-readable explanation (in_progress only)

    @property
    def signal_key(self) -> str:
        """Return :attr:`key` sanitised for use as a Datastar signal-property name.

        Datastar parses ``$busyButtons.launch_BAK-1234`` as subtraction. The
        signal_key replaces non-alphanumerics with ``_`` so templates can
        compose ``$busyButtons.launch_<signal_key>`` without quirks.
        """
        return _to_signal_key(self.key)


class KanbanView(BaseModel):
    """The full Kanban board view, split into four columns."""

    todo: list[KanbanCard] = Field(default_factory=list)
    in_progress: list[KanbanCard] = Field(default_factory=list)
    needs_review: list[KanbanCard] = Field(default_factory=list)
    done: list[KanbanCard] = Field(default_factory=list)
