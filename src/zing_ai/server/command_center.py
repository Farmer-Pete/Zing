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


def aggregate(
    issues: list[LinearIssue],
    prs: list[GitHubPR],
    recent_prs: list[GitHubPR] | None = None,
    completed_issues: list[LinearIssue] | None = None,
    sessions: list[Session] | None = None,
    current_username: str | None = None,
) -> KanbanView:
    """Build the Kanban board view from external + local snapshots.

    Standalone sessions (no ``ticket_id``) are excluded.
    Cards without at least a ticket or a PR are excluded.
    """
    _recent_prs: list[GitHubPR] = recent_prs or []
    _completed_issues: list[LinearIssue] = completed_issues or []
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
