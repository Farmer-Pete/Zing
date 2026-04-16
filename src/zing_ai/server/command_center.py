"""Pure aggregation logic for the Command Center dashboard.

Takes typed snapshots from Linear, GitHub, and the in-memory SessionManager
and produces the inbox items + hubs rendered on ``/command-center``. No I/O
lives here — everything is a pure function of its inputs so it can be unit
tested with synthetic data.
"""

from __future__ import annotations

import re

from zing_ai.server.models import Session
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

_TICKET_RE = re.compile(r"[A-Z]+-\d+", re.IGNORECASE)


def _parse_ticket_ids(pr: GitHubPR) -> set[str]:
    """Return the set of ticket identifiers referenced by a PR.

    Scans the PR's head ref, title, and body for matches of ``[A-Z]+-\\d+``
    (case-insensitive), uppercasing each match so lowercase branch names like
    ``bak-1179/feature`` normalise to ``BAK-1179``.
    """
    text = " ".join(filter(None, [pr.head_ref, pr.title, pr.body]))
    return {m.upper() for m in _TICKET_RE.findall(text)}


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
    _ = current_username  # used by step 3.5 (inbox derivation)

    # --- Build ticket hubs keyed by identifier ---
    ticket_hubs: dict[str, Hub] = {}
    for issue in issues:
        ticket_hubs[issue.identifier] = Hub(
            id=issue.identifier,
            kind="ticket",
            title=issue.title,
            team=issue.team,
            assignee=issue.assignee,
            urgency="cool",
            linear_issue=issue,
        )

    # --- Attach PRs to ticket hubs; collect orphan PRs ---
    orphan_prs: list[GitHubPR] = []
    for pr in prs:
        pr_ticket_ids = _parse_ticket_ids(pr)
        matched = False
        for ticket_id in pr_ticket_ids:
            if ticket_id in ticket_hubs:
                ticket_hubs[ticket_id].prs.append(pr)
                matched = True
        if not matched:
            orphan_prs.append(pr)

    # --- Attach sessions to ticket hubs; collect orphan sessions ---
    orphan_sessions: list[Session] = []
    for session in sessions:
        if session.ticket_id and session.ticket_id in ticket_hubs:
            hub = ticket_hubs[session.ticket_id]
            _attach_session_to_hub(session, hub)
        else:
            orphan_sessions.append(session)

    hubs: list[Hub] = list(ticket_hubs.values())

    # --- Orphan PR hubs ---
    for pr in orphan_prs:
        hub = Hub(
            id=f"pr-{pr.number}",
            kind="pr",
            title=pr.title,
            team=None,
            assignee=None,
            urgency="cool",
        )
        hub.prs.append(pr)
        hubs.append(hub)

    # --- Orphan session hubs ---
    for session in orphan_sessions:
        hub = Hub(
            id=f"session-{session.session_id}",
            kind="session",
            title=session.title,
            team=None,
            assignee=None,
            urgency="cool",
        )
        _attach_session_to_hub(session, hub)
        hubs.append(hub)

    return ([], hubs)


def _attach_session_to_hub(session: Session, hub: Hub) -> None:
    """Partition session into hub.audits (WorkflowStep) or hub.sessions (Session).

    A session is classified as an audit if any of its steps has a step_name in
    AUDIT_STEP_NAMES.  In that case, the matching WorkflowStep objects are
    appended to hub.audits.  Otherwise the whole Session is appended to
    hub.sessions.
    """
    audit_steps = [s for s in session.steps if s.step_name in AUDIT_STEP_NAMES]
    if audit_steps:
        hub.audits.extend(audit_steps)
    else:
        hub.sessions.append(session)
