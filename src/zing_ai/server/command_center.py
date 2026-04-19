"""Pure aggregation logic for the Command Center dashboard.

Takes typed snapshots from Linear, GitHub, and the in-memory SessionManager
and produces the Kanban view rendered on ``/command-center``. No I/O
lives here — everything is a pure function of its inputs so it can be unit
tested with synthetic data.
"""

from __future__ import annotations

import re
from datetime import UTC, datetime, timedelta

from zing_ai.server.models import Session, SessionState, WorkflowStep, ZingSession
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


def _ensure_utc(dt: datetime) -> datetime:
    """Return *dt* as a UTC-aware datetime."""
    return dt.replace(tzinfo=UTC) if dt.tzinfo is None else dt


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
        if isinstance(session, ZingSession):
            for step in session.steps:
                candidates.append(step.created_at)
    if not candidates:
        return datetime.min.replace(tzinfo=UTC)
    return max(_ensure_utc(dt) for dt in candidates)


# -- Signal helpers ----------------------------------------------------------


def _has_active_session(card: KanbanCard) -> bool:
    """Any session has a step in STARTED state."""
    return any(
        step.state == SessionState.STARTED
        for session in card.sessions
        if isinstance(session, ZingSession)
        for step in session.steps
    )


def _has_open_pr_in_progress(card: KanbanCard, current_username: str) -> bool:
    """Any linked PR is open with no pending reviews.

    For orphan-PR cards (no ticket), require the PR to be authored by
    the user so other people's un-reviewed PRs don't clutter the board.
    """
    return any(
        pr.state == "open"
        and not pr.requested_reviewers
        and (card.ticket is not None or pr.author == current_username)
        for pr in card.prs
    )


def _has_pr_needing_review(card: KanbanCard, current_username: str) -> bool:
    """Any linked PR has reviewers requested and the user is involved."""
    for pr in card.prs:
        if (
            pr.state == "open"
            and pr.review_decision != "APPROVED"
            and pr.requested_reviewers
            and (current_username in pr.requested_reviewers or pr.author == current_username)
        ):
            return True
    return False


def _is_recently_done(card: KanbanCard, cutoff: datetime) -> bool:
    """Ticket completed or PR merged/approved within the cutoff window."""
    if (
        card.ticket is not None
        and card.ticket.state_type == "completed"
        and _ensure_utc(card.ticket.updated_at) >= cutoff
    ):
        return True

    for pr in card.prs:
        if (
            pr.state == "merged"
            and pr.merged_at is not None
            and _ensure_utc(pr.merged_at) >= cutoff
        ):
            return True
        if pr.review_decision == "APPROVED" and _ensure_utc(pr.updated_at) >= cutoff:
            return True

    return False


# -- Filter ------------------------------------------------------------------


def _should_include_card(card: KanbanCard, current_username: str, cutoff: datetime) -> bool:
    """Return False when the card should be excluded from the board entirely."""
    # Orphan-PR cards where the user is neither author nor reviewer.
    if card.ticket is None and card.prs:
        user_involved = any(
            pr.author == current_username or current_username in pr.requested_reviewers
            for pr in card.prs
        )
        if not user_involved:
            return False

    # Stale approved PRs: all PRs approved, none updated within the done window,
    # and no open ticket driving them.
    if card.prs and all(pr.review_decision == "APPROVED" for pr in card.prs):
        any_recent = any(_ensure_utc(pr.updated_at) >= cutoff for pr in card.prs)
        if not any_recent:
            has_open_ticket = card.ticket is not None and card.ticket.state_type not in (
                "completed",
                "cancelled",
            )
            if not has_open_ticket:
                return False

    return True


# -- Review grouping ---------------------------------------------------------


def _assign_review_group(card: KanbanCard, current_username: str) -> str:
    """Return the review sub-group for a needs_review card.

    - ``mine_passing``: user is a requested reviewer and CI is passing
    - ``mine_failing``: user is a requested reviewer and CI is not passing
    - ``others``: user is the author waiting on other reviewers
    """
    user_is_reviewer = any(current_username in pr.requested_reviewers for pr in card.prs)
    if user_is_reviewer:
        ci_passing = all(pr.ci_status == "success" for pr in card.prs if pr.ci_status is not None)
        return "mine_passing" if ci_passing else "mine_failing"
    return "others"


# -- Classification ----------------------------------------------------------


def _classify_card(
    card: KanbanCard,
    current_username: str,
    now: datetime,
) -> str | None:
    """Return the Kanban column name for *card*.

    Returns ``None`` when the card should be excluded from the board.

    Priority: in_progress > needs_review > done > ticket-started > todo
    """
    cutoff = now - _DONE_WINDOW

    if not _should_include_card(card, current_username, cutoff):
        return None

    if _has_active_session(card) or _has_open_pr_in_progress(card, current_username):
        return "in_progress"

    if _has_pr_needing_review(card, current_username):
        return "needs_review"

    if _is_recently_done(card, cutoff):
        return "done"

    # Ticket is actively being worked on but no PR signal drove it elsewhere.
    if card.ticket is not None and card.ticket.state_type == "started":
        return "in_progress"

    return "todo"


def _user_involved_in_done_card(card: KanbanCard, current_username: str) -> bool:
    """Return True if the user authored or reviewed any PR on this card.

    For ticket-only cards (no PRs), always include them (they're the user's
    assigned tickets).
    """
    if not card.prs:
        return True  # ticket-only card, no PR filter needed
    return any(
        pr.author == current_username or current_username in pr.requested_reviewers
        for pr in card.prs
    )


def _assign_done_group(card: KanbanCard) -> str:
    """Return the done sub-group for a done card.

    - ``ready_to_merge``: has an open PR that is approved but not yet merged
    - ``completed``: merged PRs or completed tickets
    """
    for pr in card.prs:
        if pr.state == "open" and pr.review_decision == "APPROVED":
            return "ready_to_merge"
    return "completed"


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
            # Surface audit steps (ZingSession only — ClaudeCodeSession has no steps)
            if isinstance(session, ZingSession):
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

        if column is None:
            continue
        elif column == "in_progress":
            view.in_progress.append(card)
        elif column == "needs_review":
            view.needs_review.append(card)
        elif column == "done":
            if _user_involved_in_done_card(card, _username):
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

    # -----------------------------------------------------------------------
    # 9. Assign review groups for the needs_review column
    # -----------------------------------------------------------------------
    for card in view.needs_review:
        card.review_group = _assign_review_group(card, _username)

    # Sort needs_review by group order: mine_passing, mine_failing, others
    _GROUP_ORDER = {"mine_passing": 0, "mine_failing": 1, "others": 2}
    view.needs_review.sort(key=lambda c: _GROUP_ORDER.get(c.review_group or "", 3))

    # -----------------------------------------------------------------------
    # 10. Assign done groups and sort: ready_to_merge first, then completed
    # -----------------------------------------------------------------------
    for card in view.done:
        card.done_group = _assign_done_group(card)

    _DONE_GROUP_ORDER = {"ready_to_merge": 0, "completed": 1}
    view.done.sort(key=lambda c: _DONE_GROUP_ORDER.get(c.done_group or "", 2))

    return view
