"""Shared fixtures + factory helpers for Command Center tests.

Every test module under ``tests/test_command_center/`` can ``from
tests.test_command_center.conftest import make_pr`` (or take the equivalent
pytest fixture), rather than redefining the same ``_make_*`` helpers inline.
The factories accept broad kwargs to cover the various cases exercised across
the suite (urgency tests set ``state`` on workflow steps, routes tests inject
``requested_reviewers`` on PRs, etc.).
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from zing_ai.server.models import Session, SessionState, WorkflowStep
from zing_ai.server.models_external import GitHubPR, LinearIssue


def make_issue(
    *,
    identifier: str = "BAK-1",
    title: str = "Fix bug",
    team: str | None = "Back End",
    assignee: str | None = "alice",
    state: str = "In Progress",
    state_type: str = "started",
    priority: int = 0,
    url: str | None = None,
    updated_at: datetime | None = None,
) -> LinearIssue:
    """Build a :class:`LinearIssue` with sensible defaults for tests."""
    return LinearIssue(
        id="uuid-" + identifier,
        identifier=identifier,
        title=title,
        state=state,
        state_type=state_type,
        priority=priority,
        assignee=assignee,
        team=team,
        url=url or f"https://linear.app/t/{identifier}",
        updated_at=updated_at or datetime(2026, 4, 16, 0, 0, 0),
    )


def make_pr(
    *,
    number: int = 1,
    title: str = "Title",
    head_ref: str = "feature",
    body: str | None = None,
    state: str = "open",
    draft: bool = False,
    base_ref: str = "main",
    author: str = "octocat",
    requested_reviewers: list[str] | None = None,
    review_decision: str | None = None,
    mergeable_state: str = "clean",
    ci_status: str | None = None,
    ci_checks: list[Any] | None = None,
    url: str | None = None,
    updated_at: datetime | None = None,
) -> GitHubPR:
    """Build a :class:`GitHubPR` with sensible defaults for tests."""
    return GitHubPR(
        number=number,
        title=title,
        state=state,  # type: ignore[arg-type]
        draft=draft,
        head_ref=head_ref,
        base_ref=base_ref,
        body=body,
        author=author,
        requested_reviewers=requested_reviewers or [],
        review_decision=review_decision,  # type: ignore[arg-type]
        mergeable_state=mergeable_state,
        ci_status=ci_status,
        ci_checks=ci_checks or [],
        url=url or f"https://github.com/o/r/pull/{number}",
        updated_at=updated_at or datetime(2026, 4, 16, 0, 0, 0, tzinfo=UTC),
    )


def make_session(
    *,
    session_id: str = "sess-1",
    title: str = "Session 1",
    ticket_id: str | None = None,
    steps: list[WorkflowStep] | None = None,
) -> Session:
    """Build a :class:`Session` with sensible defaults for tests."""
    return Session(
        session_id=session_id,
        title=title,
        ticket_id=ticket_id,
        steps=steps or [],
    )


def make_workflow_step(
    *,
    step_name: str,
    sequence: int = 0,
    state: SessionState = SessionState.PENDING,
    findings: list[Any] | None = None,
) -> WorkflowStep:
    """Build a :class:`WorkflowStep`.

    ``state`` and ``findings`` are set after construction because
    ``WorkflowStep`` initialises them internally.
    """
    step = WorkflowStep(step_name=step_name, sequence=sequence)
    step.state = state
    if findings is not None:
        step.findings = findings
    return step
