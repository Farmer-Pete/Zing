"""Pure aggregation logic for the Command Center dashboard.

Takes typed snapshots from Linear, GitHub, and the in-memory SessionManager
and produces the inbox items + hubs rendered on ``/command-center``. No I/O
lives here — everything is a pure function of its inputs so it can be unit
tested with synthetic data.
"""

from __future__ import annotations

import re
from datetime import datetime

from zing_ai.server.models import Session, SessionState
from zing_ai.server.models_external import (
    GitHubPR,
    Hub,
    HubUrgency,
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

# Ticket ids look like ``BAK-1179`` / ``FRO-42``. Require two+ letter prefixes
# and word boundaries so noisy tokens like ``UTF-8``/``SHA-256``/``PR-1`` don't
# masquerade as tickets. Length-1 team keys aren't used by Linear in practice.
_TICKET_RE = re.compile(r"\b[A-Z]{2,}-\d+\b", re.IGNORECASE)


def _parse_ticket_id(pr: GitHubPR) -> str | None:
    """Return the first ticket identifier referenced by a PR, if any.

    Scans ``head_ref``, then ``title``, then ``body`` (in that order) for the
    first match of ``\\b[A-Z]{2,}-\\d+\\b`` and uppercases it. Branch names
    carry the canonical ticket id in practice, so first-match ordering picks
    the right hub even when the PR body mentions a second ticket.
    """
    text = " ".join(filter(None, [pr.head_ref, pr.title, pr.body]))
    match = _TICKET_RE.search(text)
    return match.group(0).upper() if match else None


def _compute_urgency(hub: Hub, current_username: str) -> HubUrgency:
    """Determine the urgency tier for a hub.

    - ``hot``: an audit step is READY with findings, or a PR requests review
      from the current user and is not yet approved.
    - ``active``: a session step is STARTED, or a PR's CI is running.
    - ``cool``: everything else.
    """
    # --- hot checks ---
    for audit_step in hub.audits:
        if audit_step.state == SessionState.READY and audit_step.findings:
            return "hot"

    for pr in hub.prs:
        if current_username in pr.requested_reviewers and pr.review_decision != "APPROVED":
            return "hot"

    # --- active checks ---
    for session in hub.sessions:
        for step in session.steps:
            if step.state == SessionState.STARTED:
                return "active"

    for pr in hub.prs:
        if pr.ci_status == "pending" or pr.mergeable_state == "checking":
            return "active"

    return "cool"


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
    # current_username is used for urgency computation and step 3.5 (inbox derivation)

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
        pr_ticket_id = _parse_ticket_id(pr)
        if pr_ticket_id is not None and pr_ticket_id in ticket_hubs:
            ticket_hubs[pr_ticket_id].prs.append(pr)
        else:
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

    # --- Compute urgency for all hubs ---
    for hub in hubs:
        hub.urgency = _compute_urgency(hub, current_username)

    inbox_items = _derive_inbox_items(hubs, current_username, sessions)
    return (inbox_items, hubs)


def _format_time_waiting(since: datetime) -> str:
    """Return a compact string like '5m', '2h', '3d' for time elapsed since *since*.

    Tolerates both naive and tz-aware *since* values: anchors on the same
    timezone basis as *since* so naive/aware mixing never raises.
    """
    now = datetime.now(since.tzinfo) if since.tzinfo is not None else datetime.now()
    delta = now - since
    total_seconds = int(delta.total_seconds())
    if total_seconds < 0:
        return "—"
    if total_seconds < 3600:
        minutes = max(total_seconds // 60, 1)
        return f"{minutes}m"
    if total_seconds < 86400:
        return f"{total_seconds // 3600}h"
    return f"{delta.days}d"


def _derive_inbox_items(
    hubs: list[Hub],
    current_username: str,
    sessions: list[Session],
) -> list[InboxItem]:
    """Build the prioritized action-item list for the Command Center inbox.

    Rules:
    - Each audit step in READY state with findings -> high-priority InboxItem.
    - Each PR with current_username in requested_reviewers and not APPROVED ->
      medium-priority InboxItem.
    Items are sorted: high priority first, then by time_waiting descending (longest wait first).
    """
    # Build a mapping from WorkflowStep.step_id -> session_id so we can
    # construct target_url for audit items.
    step_to_session_id: dict[str, str] = {}
    for session in sessions:
        for step in session.steps:
            step_to_session_id[step.step_id] = session.session_id

    items: list[tuple[int, datetime, InboxItem]] = []  # (priority_rank, since, item)

    for hub in hubs:
        hub_label = hub.id if hub.kind == "ticket" else "Standalone"

        # --- Audit findings ---
        ready_audit_steps = [s for s in hub.audits if s.state == SessionState.READY and s.findings]
        if ready_audit_steps:
            n = sum(len(s.findings) for s in ready_audit_steps)
            # Use the first ready step's session for the target URL
            first_step = ready_audit_steps[0]
            session_id = step_to_session_id.get(first_step.step_id, "")
            target_url = f"/{session_id}" if session_id else "/"
            since = first_step.created_at
            item = InboxItem(
                priority="high",
                action_text=f"Triage {n} audit finding{'s' if n != 1 else ''}",
                detail_text=first_step.step_name,
                hub_id=hub.id,
                hub_label=hub_label,
                time_waiting=_format_time_waiting(since),
                target_url=target_url,
            )
            items.append((0, since, item))

        # --- PR reviews ---
        for pr in hub.prs:
            if current_username in pr.requested_reviewers and pr.review_decision != "APPROVED":
                one_more_needed = pr.review_decision == "REVIEW_REQUIRED"
                action_text = f"Review PR #{pr.number}"
                if one_more_needed:
                    action_text += " (one more approval needed)"
                since = pr.updated_at
                detail_text = pr.title[:80] if pr.title else None
                item = InboxItem(
                    priority="medium",
                    action_text=action_text,
                    detail_text=detail_text,
                    hub_id=hub.id,
                    hub_label=hub_label,
                    time_waiting=_format_time_waiting(since),
                    target_url=pr.url,
                )
                items.append((1, since, item))

    # Sort: high priority first (rank 0 < 1), then longest wait first (descending since)
    items.sort(key=lambda x: (x[0], -x[1].timestamp()))
    return [item for _, _, item in items]


def _attach_session_to_hub(session: Session, hub: Hub) -> None:
    """Attach *session* to *hub*, surfacing its audit steps alongside the session.

    A typical Zing session has mixed steps (e.g. ``plan``, ``plan-audit``,
    ``build``, ``build-audit``). The session is always appended to
    ``hub.sessions`` so the Sessions spoke shows the parent session's overall
    progress and ``_compute_urgency`` can observe STARTED non-audit steps
    (STARTED build = ``active`` urgency). Additionally, any steps whose
    ``step_name`` is in :data:`AUDIT_STEP_NAMES` are also appended to
    ``hub.audits`` so the Audits spoke highlights audit-specific state (READY
    with findings = ``hot`` urgency).
    """
    hub.sessions.append(session)
    for step in session.steps:
        if step.step_name in AUDIT_STEP_NAMES:
            hub.audits.append(step)
