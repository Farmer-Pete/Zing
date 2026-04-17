"""Pure aggregation logic for the Command Center dashboard.

Takes typed snapshots from Linear, GitHub, and the in-memory SessionManager
and produces the Kanban view rendered on ``/command-center``. No I/O
lives here — everything is a pure function of its inputs so it can be unit
tested with synthetic data.
"""

from __future__ import annotations

import re
from datetime import UTC, datetime, timedelta

from zing_ai.server.models import Session, SessionState, WorkflowStep
from zing_ai.server.models_external import (
    GitHubPR,
    Hub,
    HubUrgency,
    InboxItem,
    KanbanCard,
    KanbanView,
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


def _actionable_findings(step: WorkflowStep) -> list:
    """Return the subset of *step*'s findings that require user action.

    Evaluation findings are informational (criteria + litmus tests + warnings)
    and don't drive user input. Excluding them from the "hot" urgency gate
    prevents a hub that's really just been audited for context from screaming
    for attention.
    """
    return [f for f in step.findings if getattr(f, "type", None) != "evaluation"]


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


# ---------------------------------------------------------------------------
# New Kanban aggregation (Step 5)
# ---------------------------------------------------------------------------

_DONE_WINDOW = timedelta(days=7)


def _card_most_recent_activity(card: KanbanCard) -> datetime:
    """Return the most recent activity timestamp for sorting purposes."""
    candidates: list[datetime] = []
    if card.ticket is not None:
        candidates.append(card.ticket.updated_at)
    for pr in card.prs:
        candidates.append(pr.updated_at)
        if pr.merged_at is not None:
            candidates.append(pr.merged_at)
    for session in card.sessions:
        for step in session.steps:
            candidates.append(step.created_at)
    if not candidates:
        return datetime.min.replace(tzinfo=UTC)
    # Normalise all to UTC-aware for comparison
    aware = []
    for dt in candidates:
        if dt.tzinfo is None:
            aware.append(dt.replace(tzinfo=UTC))
        else:
            aware.append(dt)
    return max(aware)


def _classify_card(
    card: KanbanCard,
    current_username: str,
    now: datetime,
) -> str:
    """Return the Kanban column name for *card* using the priority rule.

    Priority: in_progress > needs_review > done > todo
    """
    in_progress = False
    needs_review = False
    done = False

    cutoff = now - _DONE_WINDOW

    # --- In Progress ---
    # Any session has a step in STARTED state
    for session in card.sessions:
        for step in session.steps:
            if step.state == SessionState.STARTED:
                in_progress = True
                break
        if in_progress:
            break

    # Any linked PR is open with no pending reviews
    if not in_progress:
        for pr in card.prs:
            if pr.state == "open" and not pr.requested_reviewers:
                in_progress = True
                break

    if in_progress:
        return "in_progress"

    # --- Needs Review ---
    # Any linked PR has review_decision != APPROVED and has requested_reviewers
    # (either user is a reviewer, or user is author and others are reviewers)
    for pr in card.prs:
        if pr.review_decision != "APPROVED" and pr.requested_reviewers:
            in_review = (current_username in pr.requested_reviewers) or (
                pr.author == current_username and bool(pr.requested_reviewers)
            )
            if in_review:
                needs_review = True
                break

    if needs_review:
        return "needs_review"

    # --- Done ---
    # Ticket state_type == "completed" with updatedAt in last 7 days
    if card.ticket is not None:
        ticket_updated = card.ticket.updated_at
        if ticket_updated.tzinfo is None:
            ticket_updated = ticket_updated.replace(tzinfo=UTC)
        if card.ticket.state_type == "completed" and ticket_updated >= cutoff:
            done = True

    # Any linked PR is merged in last 7 days
    if not done:
        for pr in card.prs:
            if pr.state == "merged" and pr.merged_at is not None:
                merged_at = pr.merged_at
                if merged_at.tzinfo is None:
                    merged_at = merged_at.replace(tzinfo=UTC)
                if merged_at >= cutoff:
                    done = True
                    break

    # Any linked PR was reviewed by user in last 7 days — approximated by
    # review_decision == APPROVED and PR updated within window by someone
    if not done:
        for pr in card.prs:
            if pr.review_decision == "APPROVED":
                pr_updated = pr.updated_at
                if pr_updated.tzinfo is None:
                    pr_updated = pr_updated.replace(tzinfo=UTC)
                if pr_updated >= cutoff:
                    done = True
                    break

    if done:
        return "done"

    return "todo"


def _todo_sort_key(card: KanbanCard) -> tuple:
    """Sort key for todo column.

    State-type bucket: other=0 (TOP), backlog=1, triage=2 (BOTTOM).
    Within bucket: priority ascending (1=urgent first, 4=low last, 0=no priority last).
    """
    if card.ticket is None:
        return (0, 0)
    state_type = card.ticket.state_type
    if state_type == "triage":
        bucket = 2
    elif state_type == "backlog":
        bucket = 1
    else:
        bucket = 0
    priority = card.ticket.priority
    # 0 = no priority → sort last within bucket
    effective_priority = priority if priority != 0 else 5
    return (bucket, effective_priority)


def aggregate(  # type: ignore[return]
    issues: list[LinearIssue],
    prs: list[GitHubPR],
    recent_prs: list[GitHubPR] | list[Session] | None = None,
    completed_issues: list[LinearIssue] | str | None = None,
    sessions: list[Session] | None = None,
    current_username: str | None = None,
) -> KanbanView:
    """Build the Kanban board view from external + local snapshots.

    New 6-argument call: ``aggregate(issues, prs, recent_prs, completed_issues,
    sessions, current_username) -> KanbanView``

    Legacy 4-argument call (backward-compat for routes_command_center.py until
    Step 7 updates it): ``aggregate(issues, prs, sessions, current_username) ->
    tuple[list[InboxItem], list[Hub]]``

    Standalone sessions (no ``ticket_id``) are excluded.
    Cards without at least a ticket or a PR are excluded.
    """
    # Detect the legacy 4-arg call patterns:
    # - positional: aggregate(issues, prs, sessions_list, username_str)
    #   → completed_issues is a str, recent_prs is a list of Sessions
    # - keyword: aggregate(issues=..., prs=..., sessions=..., current_username=...)
    #   → recent_prs is None, completed_issues is None
    if isinstance(completed_issues, str):
        # Positional legacy call: aggregate(issues, prs, sessions, current_username)
        _sessions: list[Session] = recent_prs or []  # type: ignore[assignment]
        _username: str = completed_issues
        return _aggregate_hubs(issues, prs, _sessions, _username)  # type: ignore[return-value]

    if recent_prs is None and completed_issues is None and current_username is not None:
        # Keyword legacy call: aggregate(issues=..., prs=..., sessions=..., current_username=...)
        _sessions = sessions or []
        _username = current_username
        return _aggregate_hubs(issues, prs, _sessions, _username)  # type: ignore[return-value]

    # New 6-arg call
    _recent_prs: list[GitHubPR] = recent_prs or []  # type: ignore[assignment]
    _completed_issues: list[LinearIssue] = completed_issues or []  # type: ignore[assignment]
    _sessions = sessions or []
    _username = current_username or ""

    now = datetime.now(UTC)

    # -----------------------------------------------------------------------
    # 1. Build cards keyed by ticket identifier
    # -----------------------------------------------------------------------
    cards: dict[str, KanbanCard] = {}

    # Open issues
    for issue in issues:
        cards[issue.identifier] = KanbanCard(
            key=issue.identifier,
            ticket=issue,
        )

    # Completed issues (may overlap with issues; _completed_issues wins if newer)
    for issue in _completed_issues:
        if issue.identifier not in cards:
            cards[issue.identifier] = KanbanCard(
                key=issue.identifier,
                ticket=issue,
            )
        else:
            # Keep whichever has the more recent updated_at
            existing = cards[issue.identifier]
            if existing.ticket is None or issue.updated_at > existing.ticket.updated_at:
                cards[issue.identifier].ticket = issue

    # -----------------------------------------------------------------------
    # 2. Attach open PRs; collect orphan PRs
    # -----------------------------------------------------------------------
    orphan_prs: list[GitHubPR] = []
    for pr in prs:
        pr_ticket_id = _parse_ticket_id(pr)
        if pr_ticket_id is not None and pr_ticket_id in cards:
            cards[pr_ticket_id].prs.append(pr)
        else:
            orphan_prs.append(pr)

    # -----------------------------------------------------------------------
    # 3. Attach recent (merged) PRs; new orphan merged PRs
    # -----------------------------------------------------------------------
    for pr in _recent_prs:
        pr_ticket_id = _parse_ticket_id(pr)
        if pr_ticket_id is not None and pr_ticket_id in cards:
            # Avoid duplicates if same PR already attached
            existing_numbers = {p.number for p in cards[pr_ticket_id].prs}
            if pr.number not in existing_numbers:
                cards[pr_ticket_id].prs.append(pr)
        else:
            # Check if already tracked as orphan
            orphan_numbers = {p.number for p in orphan_prs}
            if pr.number not in orphan_numbers:
                orphan_prs.append(pr)

    # -----------------------------------------------------------------------
    # 4. Attach sessions; skip standalone sessions (no ticket_id)
    # -----------------------------------------------------------------------
    for session in _sessions:
        if not session.ticket_id:
            continue  # exclude standalone sessions
        if session.ticket_id in cards:
            cards[session.ticket_id].sessions.append(session)
            # Surface audit steps
            for step in session.steps:
                if step.step_name in AUDIT_STEP_NAMES:
                    cards[session.ticket_id].audit_steps.append(step)

    # -----------------------------------------------------------------------
    # 5. Orphan PR cards (no matching ticket)
    # -----------------------------------------------------------------------
    orphan_cards: list[KanbanCard] = []
    for pr in orphan_prs:
        orphan_cards.append(
            KanbanCard(
                key=f"pr-{pr.number}",
                ticket=None,
                prs=[pr],
            )
        )

    # -----------------------------------------------------------------------
    # 6. Collect all cards and filter empties
    # -----------------------------------------------------------------------
    all_cards: list[KanbanCard] = [c for c in cards.values() if c.ticket is not None or c.prs] + [
        c for c in orphan_cards if c.prs
    ]

    # -----------------------------------------------------------------------
    # 7. Classify and attach audit state
    # -----------------------------------------------------------------------
    view = KanbanView()
    for card in all_cards:
        # Attach audit steps that are READY with actionable findings
        ready_audit_steps = [
            step
            for step in card.audit_steps
            if step.state == SessionState.READY and _actionable_findings(step)
        ]
        card.audit_steps = ready_audit_steps

        column = _classify_card(card, _username, now)

        if column == "in_progress":
            view.in_progress.append(card)
        elif column == "needs_review":
            view.needs_review.append(card)
        elif column == "done":
            view.done.append(card)
        else:
            view.todo.append(card)

    # -----------------------------------------------------------------------
    # 8. Sort each column
    # -----------------------------------------------------------------------
    # To Do: state_type bucket asc, then priority asc (1=urgent first, 0=last)
    view.todo.sort(key=_todo_sort_key)

    # Other columns: most recent activity first (descending)
    for col in (view.in_progress, view.needs_review, view.done):
        col.sort(key=_card_most_recent_activity, reverse=True)

    return view


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


# ---------------------------------------------------------------------------
# Legacy helpers — kept for backward-compat during Step 5.
# Will be removed when Step 7 lands.
# ---------------------------------------------------------------------------


def _compute_urgency(hub: Hub, current_username: str) -> HubUrgency:
    """Determine the urgency tier for a hub.

    - ``hot``: an audit step is READY with findings, or a PR requests review
      from the current user and is not yet approved.
    - ``active``: a session step is STARTED, or a PR's CI is running.
    - ``cool``: everything else.
    """
    # --- hot checks ---
    for audit_step in hub.audits:
        if audit_step.state == SessionState.READY and _actionable_findings(audit_step):
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
        # Only count findings that require user action (skip informational
        # evaluation findings). See _actionable_findings.
        ready_audit_steps = [
            s for s in hub.audits if s.state == SessionState.READY and _actionable_findings(s)
        ]
        if ready_audit_steps:
            n = sum(len(_actionable_findings(s)) for s in ready_audit_steps)
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


def _partition_session_into_hub(session: Session, hub: Hub) -> None:
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


# ---------------------------------------------------------------------------
# Legacy aggregate shim — backward-compat for routes_command_center.py (Step 7
# will update that module to use the new Kanban API).
# ---------------------------------------------------------------------------


def _aggregate_hubs(
    issues: list[LinearIssue],
    prs: list[GitHubPR],
    sessions: list[Session],
    current_username: str,
) -> tuple[list[InboxItem], list[Hub]]:
    """Legacy Hub-based aggregation, kept for routes_command_center.py (Step 7).

    .. deprecated::
        Use :func:`aggregate` (Kanban) instead. Will be removed in Step 7.
    """
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
            _partition_session_into_hub(session, hub)
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
        _partition_session_into_hub(session, hub)
        hubs.append(hub)

    # --- Compute urgency for all hubs ---
    for hub in hubs:
        hub.urgency = _compute_urgency(hub, current_username)

    inbox_items = _derive_inbox_items(hubs, current_username, sessions)
    return (inbox_items, hubs)
