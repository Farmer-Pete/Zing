"""Pure aggregation logic for the Command Center dashboard.

Takes typed snapshots from Linear, GitHub, and the in-memory SessionManager
and produces the inbox items + hubs rendered on ``/command-center``. No I/O
lives here — everything is a pure function of its inputs so it can be unit
tested with synthetic data.
"""

from __future__ import annotations

from zing_ai.server.models import Session, WorkflowStep
from zing_ai.server.models_external import (
    GitHubPR,
    Hub,
    InboxItem,
    LinearIssue,
)

AUDIT_STEP_NAMES: frozenset[str] = frozenset(
    {
        "plan-audit",
        "build-audit",
        "pr-audit",
        "custom-audit",
    }
)


def aggregate(
    issues: list[LinearIssue],
    prs: list[GitHubPR],
    sessions: list[Session],
    current_username: str,
) -> tuple[list[InboxItem], list[Hub]]:
    """Build the Command Center view from external + local snapshots.

    Returns a pair of (inbox_items, hubs). Later steps fill in the body; for
    now this is the entry point so downstream code can import it.
    """
    _ = (issues, prs, sessions, current_username, WorkflowStep)  # hush linters
    return ([], [])
